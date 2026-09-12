//go:build integration

package pipeline_test

// SWT-40 Part E, criterion E2, integration half (docs/tickets/inquiry-promote_SPEC.md).
// Build-tagged `integration` AND gated on BOTH MQTT_BROKER and DATABASE_URL, as
// internal/fleet's integration test is. Run with:
//
//	MQTT_BROKER=tcp://localhost:1884 \
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run Captured ./internal/pipeline/
//
// It runs the REAL capture pass (capture.EvaluateRules, shadow mode, no rules
// needed: an inbound message with no rule is decided `unmatched`, which is a
// committed decision) against compose Postgres, then the connector seam
// pipeline.AnnounceCaptured against compose Mosquitto, and asserts:
//   - one `captured` wake per pass that committed ≥1 decision, and the stage
//     that wakes on it finds the decision row already in its inbox;
//   - none on an empty pass;
//   - nothing left retained on ops/pipeline/captured;
//   - with the broker down, the pass still succeeds and its decision rows stay.
//
// GREENFIELD NOTE: compile-FAILs until internal/pipeline exists (and fleet's
// *Client grows Publish/Subscribe). Surface imposed: see contract_test.go.
//
// Cross-suite discipline: capture's pending filter is GLOBAL, so this pass also
// decides other suites' leftover inbound messages. Like
// internal/capture/rules_integration_test.go, this suite deletes
// capture_decisions WHOLESALE, and so refuses the production DSN. It owns
// source account (google, itest-pipeline@pipeline.example.test) and thread
// keys gmail:itest-pipeline:%. Never run against the production broker either.

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/fleet"
	"github.com/sspataro57/switchboard/internal/pipeline"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	itPipeAccount   = "itest-pipeline@pipeline.example.test"
	itPipeThreadKey = "gmail:itest-pipeline:thread-1"
	itPipeConnector = "itest-pipeline"
	// A port nothing listens on: connection refused at once, no hang.
	itDeadBroker = "tcp://127.0.0.1:1"
)

func requirePipelineEnv(t *testing.T) string {
	t.Helper()
	broker := os.Getenv("MQTT_BROKER")
	if broker == "" || os.Getenv("DATABASE_URL") == "" {
		t.Skip("MQTT_BROKER and/or DATABASE_URL not set; skipping pipeline integration test")
	}
	if strings.Contains(broker, "192.168.50.45") {
		t.Fatal("pipeline integration tests must NEVER use the production broker; use tcp://localhost:1884")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("pipeline integration tests must NEVER run against the real ops db (cleanup deletes " +
			"capture_decisions wholesale); use the compose db on :5433")
	}
	return broker
}

func cleanupPipelineCapture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const acct = `(SELECT id FROM source_accounts WHERE provider='google' AND account_email='` + itPipeAccount + `')`
	stmts := []string{
		`DELETE FROM capture_decisions`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + acct + `)`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'gmail:itest-pipeline:%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + acct,
		`DELETE FROM source_accounts WHERE provider='google' AND account_email='` + itPipeAccount + `'`,
	}
	for _, q := range stmts {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

type pipeFixture struct {
	pool     *pgxpool.Pool
	account  int64
	threadID int64
}

func (f *pipeFixture) scalar(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var v int64
	if err := f.pool.QueryRow(ctx, q, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return v
}

// inbound seeds one inbound message the capture pass has never decided.
func (f *pipeFixture) inbound(t *testing.T, ctx context.Context, label string) int64 {
	t.Helper()
	raw := f.scalar(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,'{}',$3, now()) RETURNING id`,
		f.account, "itest-pipeline-"+label, "itest-pipeline-hash-"+label)
	return f.scalar(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1,$2,'inbound',$3, now() - interval '5 minutes', 'itest pipeline body', 'itest pipeline',
		         'Someone <someone@pipeline.example.test>', 'gmail') RETURNING id`,
		raw, f.threadID, "<itest-pipeline-"+label+"@pipeline.example.test>")
}

func (f *pipeFixture) decisionsFor(t *testing.T, ctx context.Context, messageID int64) int64 {
	t.Helper()
	return f.scalar(t, ctx, `SELECT count(*) FROM capture_decisions WHERE message_id=$1`, messageID)
}

func shadowPass(t *testing.T, ctx context.Context, pool *pgxpool.Pool) capture.RulesStats {
	t.Helper()
	stats, err := capture.EvaluateRules(ctx, pool, nil, capture.RulesConfig{
		Mode:  capture.RulesModeShadow,
		Actor: "capture:" + itPipeConnector,
	})
	if err != nil {
		t.Fatalf("capture.EvaluateRules: %v", err)
	}
	return stats
}

// rawSub is a plain paho subscriber the test reads from directly.
type rawSub struct {
	c    mqtt.Client
	mu   sync.Mutex
	msgs []mqtt.Message
	got  chan struct{}
}

func subscribeRaw(t *testing.T, broker, clientID, topic string) *rawSub {
	t.Helper()
	s := &rawSub{got: make(chan struct{}, 64)}
	opts := mqtt.NewClientOptions().AddBroker(broker).SetClientID(clientID).
		SetConnectTimeout(5 * time.Second).SetAutoReconnect(false).SetCleanSession(true)
	s.c = mqtt.NewClient(opts)
	if tok := s.c.Connect(); !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
		t.Fatalf("paho connect %s: %v", clientID, tok.Error())
	}
	tok := s.c.Subscribe(topic, 1, func(_ mqtt.Client, m mqtt.Message) {
		s.mu.Lock()
		s.msgs = append(s.msgs, m)
		s.mu.Unlock()
		s.got <- struct{}{}
	})
	if !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
		t.Fatalf("subscribe %s: %v", topic, tok.Error())
	}
	t.Cleanup(func() { s.c.Disconnect(250) })
	return s
}

func (s *rawSub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.msgs)
}

func (s *rawSub) waitCount(t *testing.T, n int, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for s.count() < n {
		select {
		case <-s.got:
		case <-deadline:
			t.Fatalf("received %d messages, want %d within %v", s.count(), n, within)
		}
	}
}

func (s *rawSub) quiet(t *testing.T, n int, window time.Duration, why string) {
	t.Helper()
	time.Sleep(window)
	if got := s.count(); got != n {
		t.Fatalf("received %d messages, want %d: %s", got, n, why)
	}
}

func TestAnnounceCaptured_Integration_OnePerCommittedPassNoneOnEmptyBrokerDownSurvives(t *testing.T) {
	broker := requirePipelineEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	// t.Cleanup, not defer: cleanups run AFTER deferred calls, so a deferred
	// Close would hand the cleanup a closed pool. LIFO: the sweep runs first.
	t.Cleanup(pool.Close)
	cleanupPipelineCapture(t, ctx, pool)
	t.Cleanup(func() { cleanupPipelineCapture(t, context.Background(), pool) })

	topic := pipeline.Topic(pipeline.EventCaptured)

	// Precondition: nothing is retained on the wake topic. This test will not
	// clear it itself, because a retained publish to ops/pipeline/* is exactly
	// what the structure test bans everywhere, tests included.
	pre := subscribeRaw(t, broker, "itest-pipeline-pre", topic)
	pre.quiet(t, 0, 500*time.Millisecond, "a retained message sits on "+topic+" on this broker; clear it with "+
		"`mosquitto_pub -h localhost -p 1884 -r -n -t "+topic+"` and rerun")

	f := &pipeFixture{pool: pool}
	f.account = f.scalar(t, ctx,
		`INSERT INTO source_accounts (provider, account_email, scopes, send_enabled, calendar_in_availability)
		 VALUES ('google',$1,'{}',false,false) RETURNING id`, itPipeAccount)
	f.threadID = f.scalar(t, ctx,
		`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,'itest pipeline','[]') RETURNING id`,
		itPipeThreadKey)

	// The consumer side, as pipelined wires it: a gate stage's spine client
	// subscribed to its upstream wakes. On a wake it reads its inbox; the
	// decision row MUST already be there (V3 mutation: publish before commit).
	stageClient, err := fleet.NewSpineClient(ctx, broker, "itest-pipeline-stage")
	if err != nil {
		t.Fatalf("NewSpineClient: %v", err)
	}
	defer stageClient.Disconnect()

	var watchMsg int64
	var watchMu sync.Mutex
	seenAtWake := make(chan int64, 8)
	if err := pipeline.SubscribeWakes(stageClient, pipeline.StageGate, func(w pipeline.Wake) {
		if w.Event != pipeline.EventCaptured || w.Source != itPipeConnector {
			return
		}
		watchMu.Lock()
		id := watchMsg
		watchMu.Unlock()
		var n int64
		_ = pool.QueryRow(context.Background(),
			`SELECT count(*) FROM capture_decisions WHERE message_id=$1`, id).Scan(&n)
		seenAtWake <- n
	}); err != nil {
		t.Fatalf("SubscribeWakes: %v", err)
	}

	live := subscribeRaw(t, broker, "itest-pipeline-live", topic)

	// ---- 1. A pass that commits a decision publishes exactly one wake. -------
	m1 := f.inbound(t, ctx, "m1")
	watchMu.Lock()
	watchMsg = m1
	watchMu.Unlock()

	st := shadowPass(t, ctx, pool)
	if st.Considered < 1 || f.decisionsFor(t, ctx, m1) != 1 {
		t.Fatalf("fixture: the capture pass did not decide the seeded message (stats %+v)", st)
	}
	if !pipeline.AnnounceCaptured(ctx, broker, itPipeConnector, st) {
		t.Fatalf("AnnounceCaptured after a committing pass reported no wake published")
	}
	live.waitCount(t, 1, 5*time.Second)
	live.quiet(t, 1, 500*time.Millisecond, "one committing pass is one wake")

	live.mu.Lock()
	msg := live.msgs[0]
	live.mu.Unlock()
	w, err := pipeline.ParseWake(msg.Payload())
	if err != nil {
		t.Fatalf("ParseWake(%s): %v", msg.Payload(), err)
	}
	if w.Event != pipeline.EventCaptured || w.Source != itPipeConnector {
		t.Errorf("wake = %+v, want event captured, source %q", w, itPipeConnector)
	}
	if msg.Qos() != 1 {
		t.Errorf("wake delivered at QoS %d, want 1", msg.Qos())
	}

	select {
	case n := <-seenAtWake:
		if n != 1 {
			t.Errorf("the stage woken by `captured` found %d decision rows for the message, want 1: the wake "+
				"went out before the decisions committed", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the gate stage's SubscribeWakes handler never saw the captured wake")
	}

	// ---- 2. An empty pass publishes nothing. -----------------------------------
	empty := shadowPass(t, ctx, pool)
	if empty.Considered != 0 {
		t.Fatalf("fixture: second pass considered %d messages, want 0 (another writer is racing this test?)", empty.Considered)
	}
	if pipeline.AnnounceCaptured(ctx, broker, itPipeConnector, empty) {
		t.Errorf("AnnounceCaptured after an empty pass reported a published wake")
	}
	live.quiet(t, 1, time.Second, "an empty pass must not publish")

	// ---- 3. Nothing is retained: a fresh subscriber gets nothing. -------------
	post := subscribeRaw(t, broker, "itest-pipeline-post", topic)
	post.quiet(t, 0, time.Second, "the wake was stored RETAINED on the broker; wake-ups are never retained")

	// ---- 4. Broker down: the pass still succeeds, decisions intact. ----------
	m2 := f.inbound(t, ctx, "m2")
	down := shadowPass(t, ctx, pool)
	if down.Considered < 1 {
		t.Fatalf("fixture: broker-down pass decided nothing (stats %+v)", down)
	}
	start := time.Now()
	if pipeline.AnnounceCaptured(ctx, itDeadBroker, itPipeConnector, down) {
		t.Errorf("AnnounceCaptured against a dead broker reported a published wake")
	}
	if el := time.Since(start); el > 15*time.Second {
		t.Errorf("AnnounceCaptured against a dead broker took %v; a connector must not hang on the broker", el)
	}
	if got := f.decisionsFor(t, ctx, m2); got != 1 {
		t.Errorf("after a broker-down announce, message has %d decision rows, want 1 (the pass's work must stand)", got)
	}
	if got := f.decisionsFor(t, ctx, m1); got != 1 {
		t.Errorf("earlier decision rows changed: m1 has %d, want 1", got)
	}
	live.quiet(t, 1, 500*time.Millisecond, "a dead-broker announce cannot reach the live broker")
}
