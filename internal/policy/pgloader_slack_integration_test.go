//go:build integration

package policy_test

// slack-auto-tier (SWT-77) criterion 13 / D5: pgSnapshotLoader.Load pins
// Snapshot.Channel = "slack_reply" for send_slack_reply BY TOOL NAME, and the
// pin WINS over a delivery_id in the args.
//
// Build-tagged `integration` AND env-gated on DATABASE_URL. Run it in a private
// database (IK "the compose Postgres is SHARED"):
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_slackauto?sslmode=disable' \
//	  go test -tags integration -count=1 -run SlackAuto ./internal/policy/
//
// WHY. The verb creates its own row, so at policy time there is no delivery_id
// and today's loader leaves Channel "" — which Decide sends to channel_not_live
// (or, once D5a lands, channel_mismatch): the verb would be unusable. And the
// pin must win, because otherwise a caller chooses the channel it is judged on
// by adding a stray delivery_id — a gmail row's id would put the call under
// gmail's rate count and, once D5a lands, a channel_mismatch deny that says
// nothing true.
//
// GREENFIELD NOTE — EXPECTED RED: pgloader.go has no tool->channel pin, so the
// no-delivery_id case loads Channel "" and the gmail-id case loads "gmail".
//
// Cross-suite discipline: owns the slug 'itest-slackauto-pol', cleans its own
// corpus in FK order at start and end.

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
)

const slackAutoPolSlug = "itest-slackauto-pol"

func slackAutoPolPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func cleanupSlackAutoPol(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const ourTasks = `(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + slackAutoPolSlug + `'))`
	for _, q := range []string{
		`DELETE FROM task_events WHERE task_id IN ` + ourTasks,
		`DELETE FROM deliveries WHERE task_id IN ` + ourTasks,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + slackAutoPolSlug + `')`,
		`DELETE FROM projects WHERE slug='` + slackAutoPolSlug + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

// seedGmailDelivery inserts one drafted gmail delivery and returns its id.
func seedGmailDelivery(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var projID, taskID, delID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1, $1, 'itest-slackauto-pol-client', 'manual', 'dashboard', '/tmp/itest', 'any') RETURNING id`,
		slackAutoPolSlug).Scan(&projID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1, 'itest slackauto pol', 'human', 'in_progress') RETURNING id`, projID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, body, status, created_by)
		 VALUES ($1, 'gmail', 'a gmail draft', 'drafted', 'opsctl:itest-slackauto-pol') RETURNING id`,
		taskID).Scan(&delID); err != nil {
		t.Fatalf("seed gmail delivery: %v", err)
	}
	return delID
}

func TestPGLoader_SlackAuto_PinsChannelByToolName(t *testing.T) {
	ctx := context.Background()
	pool := slackAutoPolPool(t, ctx)
	cleanupSlackAutoPol(t, ctx, pool)
	t.Cleanup(func() { cleanupSlackAutoPol(t, context.Background(), pool) })
	gmailID := seedGmailDelivery(t, ctx, pool)
	loader := policy.NewPGSnapshotLoader(pool)

	const args = `{"task_id":1,"target_ref":"https://app.slack.com/client/T0360B84U/DSA806DHA","text":"hi"}`

	t.Run("no delivery_id -> slack_reply", func(t *testing.T) {
		snap, err := loader.Load(ctx, policy.Request{Tool: "send_slack_reply", Actor: "mcp:manual:salvo", Args: []byte(args)})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if snap.Channel != "slack_reply" {
			t.Fatalf("Load(send_slack_reply) Channel = %q, want slack_reply. D5: the verb creates its own row, so "+
				"there is no delivery_id at policy time; without the by-name pin the call is judged on channel "+
				"%q — unusable, and rate-limited against nothing", snap.Channel, snap.Channel)
		}
	})

	t.Run("a gmail delivery_id in the args does NOT change the channel", func(t *testing.T) {
		withID := `{"task_id":1,"delivery_id":` + strconv.FormatInt(gmailID, 10) +
			`,"target_ref":"https://app.slack.com/client/T0360B84U/DSA806DHA","text":"hi"}`
		snap, err := loader.Load(ctx, policy.Request{Tool: "send_slack_reply", Actor: "mcp:manual:salvo", Args: []byte(withID)})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if snap.Channel != "slack_reply" {
			t.Fatalf("Load(send_slack_reply, delivery_id=%d gmail) Channel = %q, want slack_reply. Criterion 13: "+
				"the pin WINS over a delivery_id — otherwise a caller chooses the channel it is judged on",
				gmailID, snap.Channel)
		}
		// And through the whole matrix: the gmail id must not turn the call into
		// a channel_mismatch (or a gmail-rate decision).
		d, err := policy.NewMatrix(loader, policy.NewStatic("send_slack_reply")).Check(ctx,
			policy.Request{Tool: "send_slack_reply", Actor: "mcp:manual:salvo", Args: []byte(withID)})
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if d.Rule == "channel_mismatch" {
			t.Errorf("send_slack_reply with a stray gmail delivery_id was judged on gmail (channel_mismatch)")
		}
	})

	t.Run("control: send_delivery still resolves the row's channel", func(t *testing.T) {
		snap, err := loader.Load(ctx, policy.Request{Tool: "send_delivery", Actor: "dashboard:x",
			Args: []byte(`{"delivery_id":` + strconv.FormatInt(gmailID, 10) + `}`)})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if snap.Channel != "gmail" {
			t.Errorf("Load(send_delivery, gmail row) Channel = %q, want gmail: the pin is for send_slack_reply BY "+
				"NAME and must not disturb every other tool's delivery_id resolution", snap.Channel)
		}
	})
}
