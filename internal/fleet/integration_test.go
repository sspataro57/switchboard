//go:build integration

package fleet_test

// Integration test for the MQTT fleet mirror (SPEC 03-mqtt-heartbeats,
// acceptance criterion 7 + verification protocol step 2). Build-tagged
// `integration` AND gated on BOTH MQTT_BROKER and DATABASE_URL: excluded from
// the default zero-network `go test ./...`, and skips cleanly if EITHER env is
// unset. Run with:
//
//   MQTT_BROKER=tcp://localhost:1884 \
//   DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//     go test -tags integration ./internal/fleet/
//
// It exercises the mirror against compose Mosquitto (host port 1884) + compose
// Postgres: raw paho publishers, the fleet mirror subscriber, and SQL assertions
// on worker_heartbeats.
//
// GREENFIELD NOTE: under `-tags integration` this compile-FAILs until
// internal/fleet exists AND github.com/eclipse/paho.mqtt.golang is added to
// go.mod (the implementer's job per the SPEC) — the expected failure mode.
// Exported surface imposed here (SPEC's client.go + mirror.go pg store):
//
//   func NewPGStore(pool *pgxpool.Pool) Store               // Existing + Upsert over worker_heartbeats
//   func NewMirror(store Store) *Mirror
//   func NewMirrorClient(ctx context.Context, brokerURL string) (*Client, error) // no LWT
//   func (c *Client) SubscribeStatus(handler func(topic string, payload []byte)) error // subscribes ops/workers/+/status
//   func (c *Client) Disconnect()
//
// Rerunnable against the persistent compose broker + db: every worker id uses
// the test-owned prefix "itest-fleet-", cleanup deletes its rows AND clears its
// retained messages (zero-length retained publish) BEFORE the run.

import (
	"context"
	"net"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/fleet"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	itWorkerBasic = "itest-fleet-basic"
	itWorkerLWT   = "itest-fleet-lwt"
	itProjectSlug = "itest-fleet"
)

func requireEnv(t *testing.T) string {
	t.Helper()
	broker := os.Getenv("MQTT_BROKER")
	if broker == "" || os.Getenv("DATABASE_URL") == "" {
		t.Skip("MQTT_BROKER and/or DATABASE_URL not set; skipping fleet integration test")
	}
	return broker
}

// rawPublisher connects a plain paho client (optionally will-carrying) that the
// test drives directly. When willTopic != "" it registers the retained dead LWT
// and installs a custom connection opener so the test can slam the raw TCP conn
// shut (the trick that makes the broker fire the will; a clean Disconnect would
// suppress it). The captured *net.Conn is returned for that purpose.
func rawPublisher(t *testing.T, broker, clientID, willTopic string) (mqtt.Client, *net.Conn) {
	t.Helper()
	var held net.Conn
	opts := mqtt.NewClientOptions().
		AddBroker(broker).
		SetClientID(clientID).
		SetKeepAlive(2 * time.Second).
		SetConnectTimeout(5 * time.Second).
		SetAutoReconnect(false)
	if willTopic != "" {
		opts.SetBinaryWill(willTopic, fleet.LWTPayload(), 1, true)
		opts.SetCustomOpenConnectionFn(func(uri *url.URL, _ mqtt.ClientOptions) (net.Conn, error) {
			c, err := net.DialTimeout("tcp", uri.Host, 5*time.Second)
			if err != nil {
				return nil, err
			}
			held = c
			return c, nil
		})
	}
	c := mqtt.NewClient(opts)
	tok := c.Connect()
	if !tok.WaitTimeout(10 * time.Second) {
		t.Fatalf("paho connect %s: timed out", clientID)
	}
	if tok.Error() != nil {
		t.Fatalf("paho connect %s: %v", clientID, tok.Error())
	}
	return c, &held
}

func publishRetained(t *testing.T, c mqtt.Client, topic string, payload []byte) {
	t.Helper()
	tok := c.Publish(topic, 1, true, payload)
	if !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
		t.Fatalf("publish %s: %v", topic, tok.Error())
	}
}

// clearRetained publishes a zero-length retained message to wipe the broker's
// retained slot for a status topic (MQTT clear-retained convention).
func clearRetained(t *testing.T, c mqtt.Client, worker string) {
	t.Helper()
	publishRetained(t, c, fleet.StatusTopic(worker), []byte{})
}

func cleanupFleet(t *testing.T, ctx context.Context, pool *pgxpool.Pool, c mqtt.Client) {
	t.Helper()
	// Clear retained messages first so a fresh subscribe does not re-deliver them.
	clearRetained(t, c, itWorkerBasic)
	clearRetained(t, c, itWorkerLWT)
	// FK order: heartbeats reference tasks; tasks reference projects.
	stmts := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM worker_heartbeats WHERE worker_id LIKE 'itest-fleet-%'`, nil},
		{`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, []any{itProjectSlug}},
		{`DELETE FROM projects WHERE slug=$1`, []any{itProjectSlug}},
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("cleanup %q: %v", s.sql, err)
		}
	}
}

// seedTask creates a project + task so the LWT worker's task_id satisfies the
// tasks FK (otherwise the mirror would rewrite it to NULL and there would be no
// task to preserve on the dead transition).
func seedTask(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var projectID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery)
		 VALUES ($1,$2,$3,'manual','dashboard') RETURNING id`,
		"Fleet Integ", itProjectSlug, "FleetClient").Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	var taskID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, status, title) VALUES ($1,'ready',$2) RETURNING id`,
		projectID, "itest-fleet task").Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return taskID
}

// startMirror wires a fresh mirror subscriber (new paho session, retained
// replay). Returns the client so the caller can Disconnect to simulate a restart.
func startMirror(t *testing.T, ctx context.Context, broker string, pool *pgxpool.Pool) *fleet.Client {
	t.Helper()
	m := fleet.NewMirror(fleet.NewPGStore(pool))
	client, err := fleet.NewMirrorClient(ctx, broker)
	if err != nil {
		t.Fatalf("NewMirrorClient: %v", err)
	}
	if err := client.SubscribeStatus(func(topic string, payload []byte) {
		if err := m.Handle(ctx, topic, payload); err != nil {
			t.Logf("mirror.Handle(%s): %v", topic, err)
		}
	}); err != nil {
		t.Fatalf("SubscribeStatus: %v", err)
	}
	return client
}

// pollRow waits (deadline) for a worker_heartbeats row satisfying pred, returning
// the observed (state, task_id).
func pollRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, worker string, deadline time.Duration, pred func(state string, taskID *int64) bool) (string, *int64) {
	t.Helper()
	end := time.Now().Add(deadline)
	for {
		var state string
		var taskID *int64
		err := pool.QueryRow(ctx,
			`SELECT state, task_id FROM worker_heartbeats WHERE worker_id=$1`, worker).Scan(&state, &taskID)
		if err == nil && pred(state, taskID) {
			return state, taskID
		}
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for worker_heartbeats row for %q (last err=%v state=%q task=%v)", worker, err, state, taskID)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestFleet_Integration_MirrorAndLWT(t *testing.T) {
	broker := requireEnv(t)
	ctx := context.Background()

	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	defer pool.Close()

	// Rerunnable cleanup up front (rows + retained topics), then seed the FK task.
	adminPub, _ := rawPublisher(t, broker, "itest-fleet-admin", "")
	defer adminPub.Disconnect(250)
	cleanupFleet(t, ctx, pool, adminPub)
	taskID := seedTask(t, ctx, pool)

	// ---- Mirror is up; publish retained status -> row appears --------------
	mirror := startMirror(t, ctx, broker, pool)

	pub, _ := rawPublisher(t, broker, "itest-fleet-pub", "")
	defer pub.Disconnect(250)
	publishRetained(t, pub, fleet.StatusTopic(itWorkerBasic), []byte(`{"state":"idle"}`))

	state, _ := pollRow(t, ctx, pool, itWorkerBasic, 10*time.Second, func(s string, _ *int64) bool {
		return s == fleet.StateIdle
	})
	if state != fleet.StateIdle {
		t.Fatalf("initial state = %q, want idle", state)
	}

	// ---- Publish a changed state for the same worker -> row updated, count same
	var before int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM worker_heartbeats WHERE worker_id LIKE 'itest-fleet-%'`).Scan(&before); err != nil {
		t.Fatalf("count before update: %v", err)
	}
	publishRetained(t, pub, fleet.StatusTopic(itWorkerBasic), []byte(`{"state":"working"}`))
	pollRow(t, ctx, pool, itWorkerBasic, 10*time.Second, func(s string, _ *int64) bool {
		return s == fleet.StateWorking
	})
	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM worker_heartbeats WHERE worker_id LIKE 'itest-fleet-%'`).Scan(&after); err != nil {
		t.Fatalf("count after update: %v", err)
	}
	if after != before {
		t.Errorf("heartbeat row count changed on update: before=%d after=%d (upsert, not insert)", before, after)
	}

	// ---- LWT: a will-carrying client publishes a live status with the seeded
	// task_id, then its TCP conn is slammed -> broker fires {"state":"dead"};
	// the row flips to dead with task_id preserved.
	willClient, conn := rawPublisher(t, broker, "itest-fleet-will", fleet.StatusTopic(itWorkerLWT))
	publishRetained(t, willClient, fleet.StatusTopic(itWorkerLWT),
		[]byte(`{"state":"working","task_id":`+strconv.FormatInt(taskID, 10)+`}`))
	pollRow(t, ctx, pool, itWorkerLWT, 10*time.Second, func(s string, task *int64) bool {
		return s == fleet.StateWorking && task != nil && *task == taskID
	})

	// Abrupt drop (no clean Disconnect) so the broker fires the retained will.
	if conn == nil || *conn == nil {
		t.Fatalf("custom connection was not captured; cannot force an abrupt drop")
	}
	if err := (*conn).Close(); err != nil {
		t.Fatalf("slam will conn: %v", err)
	}

	deadState, deadTask := pollRow(t, ctx, pool, itWorkerLWT, 30*time.Second, func(s string, _ *int64) bool {
		return s == fleet.StateDead
	})
	if deadState != fleet.StateDead {
		t.Fatalf("LWT state = %q, want dead", deadState)
	}
	if deadTask == nil || *deadTask != taskID {
		t.Errorf("dead row task_id = %v, want %d preserved", deadTask, taskID)
	}

	// ---- Retained recovery: restart the mirror (new instance) -> retained
	// status is re-delivered and the basic worker row is rebuilt from the broker.
	mirror.Disconnect()
	if _, err := pool.Exec(ctx, `DELETE FROM worker_heartbeats WHERE worker_id=$1`, itWorkerBasic); err != nil {
		t.Fatalf("delete row before recovery: %v", err)
	}
	mirror2 := startMirror(t, ctx, broker, pool)
	defer mirror2.Disconnect()
	pollRow(t, ctx, pool, itWorkerBasic, 10*time.Second, func(s string, _ *int64) bool {
		return s == fleet.StateWorking // last retained value for the basic worker
	})

	// ---- Clean up our retained messages so the broker goes quiet for next run.
	clearRetained(t, adminPub, itWorkerBasic)
	clearRetained(t, adminPub, itWorkerLWT)
	willClient.Disconnect(250)
}
