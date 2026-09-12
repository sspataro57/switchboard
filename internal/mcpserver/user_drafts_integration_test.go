//go:build integration

package mcpserver_test

// SWT-44 review fix 2 against a real database: through a ProfileUser server
// over the PRODUCTION executor wiring (policy.NewMatrix in front of the static
// allow-list — the queueMatrixExecutor pattern; a static-only executor would
// hide a humanOnly denial), update_delivery edits only drafts the calling
// actor created. The user profile pins require_own_draft:"true"; the handler
// compares deliveries.created_by with the actor, under the row lock. The full
// profile has no pin, so the same edit from this repo's session still works —
// the positive control that the refusal is the pin, not policy.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -run UserDrafts ./internal/mcpserver/
//
// Actors are test-owned (manual:itest-mcp-drafts — a HUMAN shape, so policy
// allows update_delivery exactly as it does for manual:salvo), and MCP audit
// rows carry a NULL task_id, so cleanup deletes them by actor.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/mcpserver"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	udSlug       = "itest-mcp-drafts-proj"
	udClient     = "itest-mcp-drafts-client"
	udAcct       = "itest-mcp-drafts-a@example.com"
	udThreadKey  = "gmail:itest-mcp-drafts-a@example.com:gt-1"
	udInboundMID = "<itest-mcp-drafts-in-1@example.com>"
	udWorker     = "manual:itest-mcp-drafts"
	udActor      = "mcp:" + udWorker
	// A worker-shaped id on the user binary (OPS_WORKER_ID not manual:*).
	udBadWorker = "itest-mcp-drafts-acme"
)

func udPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use the compose db on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	udCleanup(t, ctx, pool)
	t.Cleanup(func() {
		udCleanup(t, ctx, pool)
		pool.Close()
	})
	return pool
}

func udCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, st := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM policy_decisions WHERE audit_event_id IN
			(SELECT id FROM audit_events WHERE actor LIKE 'mcp:%itest-mcp-drafts%')`, nil},
		{`DELETE FROM audit_events WHERE actor LIKE 'mcp:%itest-mcp-drafts%'`, nil},
		{`DELETE FROM deliveries WHERE task_id IN
			(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{udSlug}},
		{`DELETE FROM task_events WHERE task_id IN
			(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{udSlug}},
		{`DELETE FROM normalized_messages WHERE external_message_id=$1`, []any{udInboundMID}},
		{`DELETE FROM normalized_threads WHERE thread_key=$1`, []any{udThreadKey}},
		{`DELETE FROM raw_source_items WHERE source_account_id IN
			(SELECT id FROM source_accounts WHERE account_email=$1)`, []any{udAcct}},
		{`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, []any{udSlug}},
		{`DELETE FROM projects WHERE slug=$1`, []any{udSlug}},
		{`DELETE FROM source_accounts WHERE account_email=$1`, []any{udAcct}},
	} {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

// udExecutor is the production wiring (queueMatrixExecutor's shape).
func udExecutor(pool *pgxpool.Pool) *executor.Executor {
	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	return executor.New(reg, checker, audit.NewPGStore(pool))
}

type udFixture struct{ taskID, threadID int64 }

func udSeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool) udFixture {
	t.Helper()
	var fx udFixture
	var acctID, projID, rawID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email) VALUES ('google',$1) RETURNING id`, udAcct).Scan(&acctID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,$2,'manual','dashboard','/tmp/itest','any') RETURNING id`, udSlug, udClient).Scan(&projID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status) VALUES ($1,'itest drafts task','human','ready')
		 RETURNING id`, projID).Scan(&fx.taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1,'itest-mcp-drafts-raw-1','{}','itest-mcp-drafts-hash-1') RETURNING id`, acctID).Scan(&rawID); err != nil {
		t.Fatalf("seed raw: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO normalized_threads (thread_key, subject) VALUES ($1,'quote request') RETURNING id`,
		udThreadKey).Scan(&fx.threadID); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1,$2,'inbound',$3,now(),'can you quote this?','quote request','client@itest-mcp-drafts.example','gmail')`,
		rawID, fx.threadID, udInboundMID); err != nil {
		t.Fatalf("seed inbound: %v", err)
	}
	return fx
}

func udBody(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var b string
	if err := pool.QueryRow(ctx, `SELECT body FROM deliveries WHERE id=$1`, id).Scan(&b); err != nil {
		t.Fatalf("read delivery %d: %v", id, err)
	}
	return b
}

func TestUserDrafts_Integration_UpdateOnlyOwnDrafts(t *testing.T) {
	ctx := context.Background()
	pool := udPool(t, ctx)
	fx := udSeed(t, ctx, pool)
	ex := udExecutor(pool)
	user := mcpserver.NewWithProfile(ex, udWorker, mcpserver.ProfileUser)

	// 1. A user-profile draft records the SAME actor string the update compares.
	out, err := user.CallTool(ctx, "draft_delivery", json.RawMessage(fmt.Sprintf(
		`{"task_id":%d,"channel":"gmail","subject":"Re: quote request","body":"first words","thread_id":%d}`,
		fx.taskID, fx.threadID)))
	if err != nil {
		t.Fatalf("user-profile draft_delivery: %v", err)
	}
	var d struct {
		DeliveryID int64 `json:"delivery_id"`
	}
	if err := json.Unmarshal(out, &d); err != nil || d.DeliveryID == 0 {
		t.Fatalf("draft_delivery output %s: %v", out, err)
	}
	var createdBy string
	if err := pool.QueryRow(ctx, `SELECT created_by FROM deliveries WHERE id=$1`, d.DeliveryID).Scan(&createdBy); err != nil {
		t.Fatalf("read created_by: %v", err)
	}
	if createdBy != udActor {
		t.Fatalf("created_by = %q, want the MCP actor %q — the own-draft check compares exactly this", createdBy, udActor)
	}

	// 2. Its own draft is editable.
	if _, err := user.CallTool(ctx, "update_delivery",
		json.RawMessage(fmt.Sprintf(`{"delivery_id":%d,"body":"better words"}`, d.DeliveryID))); err != nil {
		t.Fatalf("user-profile update of its own draft: %v", err)
	}
	if b := udBody(t, ctx, pool, d.DeliveryID); b != "better words" {
		t.Errorf("own draft body = %q, want the edit", b)
	}

	// 3. Anyone else's draft is refused, and untouched.
	for _, other := range []string{"drafts:gpt", "dashboard:salvo", "mcp:manual:itest-mcp-drafts-other", "mcp:itest-mcp-drafts-acme"} {
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO deliveries (task_id, channel, body, subject, status, thread_id, created_by)
			 VALUES ($1,'gmail','their words','Re: quote request','drafted',$2,$3) RETURNING id`,
			fx.taskID, fx.threadID, other).Scan(&id); err != nil {
			t.Fatalf("seed %s draft: %v", other, err)
		}
		_, err := user.CallTool(ctx, "update_delivery",
			json.RawMessage(fmt.Sprintf(`{"delivery_id":%d,"body":"planted words"}`, id)))
		if err == nil {
			t.Errorf("the user profile edited a draft created by %q; a session may edit only its own drafts", other)
		} else if !strings.Contains(err.Error(), "only its own drafts") {
			t.Errorf("refusal of %q's draft = %q, want it to say a session edits only its own drafts", other, err)
		}
		if b := udBody(t, ctx, pool, id); b != "their words" {
			t.Errorf("%q's draft body = %q after a refused edit, want it untouched", other, b)
		}

		// Positive control: the full profile (no pin), same human actor, edits it.
		if other == "drafts:gpt" {
			full := mcpserver.New(ex, udWorker)
			if _, err := full.CallTool(ctx, "update_delivery",
				json.RawMessage(fmt.Sprintf(`{"delivery_id":%d,"body":"fixed on this repo's session"}`, id))); err != nil {
				t.Errorf("POSITIVE CONTROL: full-profile update of a drafts:gpt draft: %v — the refusal above must be "+
					"the user profile's pin, not policy", err)
			}
		}
	}

	// 4. The actor path: the user binary with a worker-shaped OPS_WORKER_ID
	// still drafts, but update_delivery is humanOnly, so policy refuses the
	// edit even of its own draft.
	bad := mcpserver.NewWithProfile(ex, udBadWorker, mcpserver.ProfileUser)
	out, err = bad.CallTool(ctx, "draft_delivery", json.RawMessage(fmt.Sprintf(
		`{"task_id":%d,"channel":"gmail","body":"worker words","thread_id":%d}`, fx.taskID, fx.threadID)))
	if err != nil {
		t.Fatalf("draft_delivery as %s: %v", udBadWorker, err)
	}
	var bd struct {
		DeliveryID int64 `json:"delivery_id"`
	}
	_ = json.Unmarshal(out, &bd)
	_, err = bad.CallTool(ctx, "update_delivery", json.RawMessage(fmt.Sprintf(`{"delivery_id":%d,"body":"x"}`, bd.DeliveryID)))
	if err == nil || !strings.Contains(err.Error(), "denied by policy (human_only)") {
		t.Errorf("update_delivery as mcp:%s = %v, want a human_only denial (OPS_WORKER_ID must be manual:*)", udBadWorker, err)
	}

	// 5. Fix 3 end to end: a user-profile slack_reply draft writes no row.
	var before int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE task_id=$1`, fx.taskID).Scan(&before)
	if _, err := user.CallTool(ctx, "draft_delivery", json.RawMessage(fmt.Sprintf(
		`{"task_id":%d,"channel":"slack_reply","body":"b","target_ref":"https://app.slack.com/client/TITEST/CITEST/p1750000000000000"}`,
		fx.taskID))); err == nil {
		t.Error("the user profile drafted a slack_reply; it drafts gmail only")
	}
	var after int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE task_id=$1`, fx.taskID).Scan(&after)
	if after != before {
		t.Errorf("a refused slack_reply draft changed the delivery count %d → %d", before, after)
	}
}
