//go:build integration

package tools_test

// slack-watch-sweep (SWT-75) criteria 1 and 2 against a real database: the
// applied shape of migrations/0041_slack_watch.sql — the two id CHECKs asserted
// as POSTGRES enforces them, "not a Go validator" (criterion 1's own words) —
// and the three tools driven through the executor with the REAL policy matrix,
// so a humanOnly slip or a missing audit row shows up here.
//
//	psql '...' -c "CREATE DATABASE ops_slackwatch"
//	make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_slackwatch?sslmode=disable'
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_slackwatch?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run SlackWatch ./internal/tools/
//
// Build-tagged AND env-gated; newToolsPool skips without DATABASE_URL. NEVER
// the prod db and never the shared compose `ops` (SPEC Verification step 3).
//
// This suite owns the workspaces 'TWATCH75%' and the actor 'opsctl:swt75', and
// cleans its OWN rows in FK order, rerunnably, at start and end.
//
// RED TODAY: migration 0041 is not applied (slack_watch does not exist) and the
// three tools are not registered.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	swWorkspace = "TWATCH75"
	swConvA     = "DWATCH75A"
	swConvB     = "CWATCH75B"
	swActor     = "opsctl:swt75"
)

type swSuite struct {
	pool *pgxpool.Pool
	ex   *executor.Executor
}

func newSWSuite(t *testing.T, ctx context.Context) *swSuite {
	t.Helper()
	pool := newToolsPool(t, ctx)
	t.Cleanup(pool.Close)
	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	s := &swSuite{pool: pool, ex: executor.New(reg, checker, audit.NewPGStore(pool))}
	s.cleanup(t, ctx)
	t.Cleanup(func() { s.cleanup(t, context.Background()) })
	return s
}

func (s *swSuite) cleanup(t *testing.T, ctx context.Context) {
	t.Helper()
	for _, q := range []string{
		`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE actor = '` + swActor + `')`,
		`DELETE FROM audit_events WHERE actor = '` + swActor + `'`,
		`DELETE FROM slack_watch WHERE workspace_id LIKE 'TWATCH%'`,
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			// slack_watch missing is the expected red before 0041; say so once
			// rather than failing every test with a bare SQL error.
			if strings.Contains(err.Error(), "slack_watch") {
				t.Fatalf("cleanup %q: %v — criterion 1: apply migrations/0041_slack_watch.sql "+
					"(`make migrate LOCAL_DB_URL=...`); merging a migration is not applying it", q, err)
			}
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func (s *swSuite) call(t *testing.T, ctx context.Context, tool string, args map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(args)
	res, err := s.ex.Execute(ctx, executor.Call{Tool: tool, Actor: swActor, Args: raw})
	if err != nil {
		t.Fatalf("%s(%s) = %v", tool, raw, err)
	}
	var got map[string]any
	if err := json.Unmarshal(res.Output, &got); err != nil {
		t.Fatalf("%s result is not a JSON object (%v): %s", tool, err, res.Output)
	}
	return got
}

func (s *swSuite) n(t *testing.T, ctx context.Context, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n
}

// Criterion 1, the whole of it: the table exists with the unique key, and
// Postgres — not a Go validator — REFUSES a lowercase workspace id and a
// conversation id that is not [CDG] + 5 more. These INSERTs go straight at the
// table on purpose: D2's argument is that "the constraint that makes a dropped
// watch row impossible belongs in the database, where the write fails loudly"
// (BuildExportRequest silently DROPS a malformed id — export_request.go:101-104).
//
// MUTATION: drop either CHECK from 0041 and this test goes red.
func TestMigration0041_Integration_SlackWatchShape(t *testing.T) {
	ctx := context.Background()
	s := newSWSuite(t, ctx)

	const ins = `INSERT INTO slack_watch (workspace_id, conversation_id) VALUES ($1,$2)`
	for _, bad := range []struct{ ws, conv, why string }{
		{"t0360b84u", swConvA, "the SPEC's own example: a lowercased workspace id, which the leaf's " +
			"/^T[A-Z0-9]{5,}$/ drops silently"},
		{"T0360", swConvA, "too short for the leaf's rule"},
		{"0360B84U", swConvA, "no leading T"},
		{swWorkspace, "XYZ", "the SPEC's own example: not [CDG] + at least five"},
		{swWorkspace, "d04f7lxrb8b", "a lowercased conversation id"},
		{swWorkspace, "U04F7LXRB8B", "U is a user, not a conversation — the leaf reads C, D and G"},
	} {
		bad := bad
		t.Run(bad.ws+"/"+bad.conv, func(t *testing.T) {
			if _, err := s.pool.Exec(ctx, ins, bad.ws, bad.conv); err == nil {
				_, _ = s.pool.Exec(ctx, `DELETE FROM slack_watch WHERE workspace_id=$1 AND conversation_id=$2`,
					bad.ws, bad.conv)
				t.Errorf("INSERT slack_watch(%q,%q) SUCCEEDED — %s. Criterion 1: the id CHECKs are the leaf's "+
					"own rules restated where a bad value cannot be entered", bad.ws, bad.conv, bad.why)
			}
		})
	}

	// The good row, and the defaults the tools rely on.
	if _, err := s.pool.Exec(ctx, ins, swWorkspace, swConvA); err != nil {
		t.Fatalf("INSERT slack_watch(%q,%q) = %v, want accepted", swWorkspace, swConvA, err)
	}
	var label string
	var enabled bool
	if err := s.pool.QueryRow(ctx,
		`SELECT label, enabled FROM slack_watch WHERE workspace_id=$1 AND conversation_id=$2`,
		swWorkspace, swConvA).Scan(&label, &enabled); err != nil {
		t.Fatalf("read back the row: %v", err)
	}
	if label != "" || !enabled {
		t.Errorf("defaults are label=%q enabled=%v, want \"\" / true (criterion 1: a new watch row is live "+
			"and unlabelled)", label, enabled)
	}

	// The unique key slack_watch_add upserts on.
	if _, err := s.pool.Exec(ctx, ins, swWorkspace, swConvA); err == nil {
		t.Errorf("a second INSERT of (%q,%q) succeeded; criterion 1 wants UNIQUE (workspace_id, conversation_id) "+
			"— without it slack_watch_add's upsert has nothing to conflict on and a re-seed duplicates the "+
			"conversation in every targeted request", swWorkspace, swConvA)
	}
}

// Criterion 2: the three tools through the executor — audited, and the add is
// IDEMPOTENT on the unique key: a second add of the same pair updates the label
// and RE-ENABLES rather than erroring (memory: the owner works the board
// concurrently; a re-run seed command must succeed without changing anything
// else).
func TestSlackWatchTools_AddIsIdempotentAndAudited(t *testing.T) {
	ctx := context.Background()
	s := newSWSuite(t, ctx)

	first := s.call(t, ctx, "slack_watch_add", map[string]any{
		"workspace_id": swWorkspace, "conversation_id": swConvA, "label": "José",
	})
	if first["enabled"] != true || first["workspace_id"] != swWorkspace || first["conversation_id"] != swConvA {
		t.Fatalf("slack_watch_add = %v, want {id, workspace_id, conversation_id, label, enabled:true}", first)
	}
	id, ok := first["id"].(float64)
	if !ok || id <= 0 {
		t.Fatalf("slack_watch_add returned no id: %v", first)
	}

	// Disable it, then add the same pair again with a new label.
	off := s.call(t, ctx, "slack_watch_set_enabled", map[string]any{"id": int64(id), "enabled": false})
	if off["enabled"] != false {
		t.Errorf("slack_watch_set_enabled(false) = %v, want {id, enabled:false}", off)
	}
	// Idempotent no-op success: the same call again must NOT error.
	s.call(t, ctx, "slack_watch_set_enabled", map[string]any{"id": int64(id), "enabled": false})

	again := s.call(t, ctx, "slack_watch_add", map[string]any{
		"workspace_id": swWorkspace, "conversation_id": swConvA, "label": "José (DM)",
	})
	if again["id"] != first["id"] {
		t.Errorf("a repeated slack_watch_add made a NEW row (%v then %v); criterion 2 wants an upsert on the "+
			"unique key", first["id"], again["id"])
	}
	if again["enabled"] != true {
		t.Errorf("a repeated slack_watch_add left enabled=%v; criterion 2: adding a disabled pair RE-ENABLES it "+
			"— that is how a paused watch is resumed with the same command that created it", again["enabled"])
	}
	if again["label"] != "José (DM)" {
		t.Errorf("a repeated slack_watch_add left label=%v, want the new label", again["label"])
	}
	if n := s.n(t, ctx, `SELECT count(*) FROM slack_watch WHERE workspace_id=$1 AND conversation_id=$2`,
		swWorkspace, swConvA); n != 1 {
		t.Errorf("slack_watch holds %d rows for one conversation, want 1", n)
	}

	// A second conversation, then the list.
	s.call(t, ctx, "slack_watch_add", map[string]any{
		"workspace_id": swWorkspace, "conversation_id": swConvB, "label": "Katie",
	})
	listed := s.call(t, ctx, "slack_watch_list", map[string]any{})
	rows, ok := listed["rows"].([]any)
	if !ok {
		t.Fatalf("slack_watch_list = %v, want {rows:[...]}", listed)
	}
	seen := map[string]map[string]any{}
	for _, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("slack_watch_list row is not an object: %v", r)
		}
		if m["workspace_id"] == swWorkspace {
			seen[m["conversation_id"].(string)] = m
		}
	}
	if len(seen) != 2 {
		t.Fatalf("slack_watch_list returned %d of this suite's 2 rows: %v", len(seen), rows)
	}
	for _, field := range []string{"id", "workspace_id", "conversation_id", "label", "enabled"} {
		if _, ok := seen[swConvB][field]; !ok {
			t.Errorf("slack_watch_list row is missing %q; criterion 2 fixes the shape "+
				"{id, workspace_id, conversation_id, label, enabled, last_read_at}", field)
		}
	}
	if _, ok := seen[swConvB]["last_read_at"]; !ok {
		t.Errorf("slack_watch_list row carries no last_read_at; it is what /sources shows as \"when each was " +
			"last read\" (criterion 28) and it comes from sync_runs, not from slack_watch")
	}

	// Invariant 3: every call left an audit row that COMPLETED, plus its policy
	// decision. A tool that wrote the table without them would be a side door.
	for _, tool := range []string{"slack_watch_add", "slack_watch_set_enabled", "slack_watch_list"} {
		if n := s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE actor=$1 AND tool=$2 AND status='ok'
		                      AND completed_at IS NOT NULL`, swActor, tool); n == 0 {
			t.Errorf("%s wrote no completed audit_events row for %s. Criterion 2 / invariant 3: validate -> "+
				"policy check -> audit start -> handler -> audit complete, with no side doors", tool, swActor)
		}
		if n := s.n(t, ctx, `SELECT count(*) FROM policy_decisions p JOIN audit_events a ON a.id=p.audit_event_id
		                      WHERE a.actor=$1 AND p.tool=$2 AND p.decision='allow'`, swActor, tool); n == 0 {
			t.Errorf("%s recorded no allow policy_decision for the human actor %s; the humanOnly rule must "+
				"still ALLOW opsctl, or the seeding commands in \"Usable alone\" cannot run", tool, swActor)
		}
	}
}

// Criterion 3, the runtime half of the matrix test: with the REAL policy matrix
// in front of the executor, an agent-shaped actor is DENIED and writes nothing.
// The unit test pins the decision; this pins that the decision is actually in
// the path a caller takes.
func TestSlackWatchTools_RefuseAnAgentShapedActor(t *testing.T) {
	ctx := context.Background()
	s := newSWSuite(t, ctx)

	args, _ := json.Marshal(map[string]any{
		"workspace_id": swWorkspace, "conversation_id": swConvA, "label": "an agent's choice",
	})
	for _, actor := range []string{"mcp:collaboratory", "drafts:gpt", "worker:collab", "capture:slackweb"} {
		_, err := s.ex.Execute(ctx, executor.Call{Tool: "slack_watch_add", Actor: actor, Args: args})
		if err == nil {
			t.Errorf("slack_watch_add by %q SUCCEEDED. Criterion 3: browser time is a scarce shared resource "+
				"and an agent must not be able to point it at a conversation of its choosing", actor)
		}
	}
	if n := s.n(t, ctx, `SELECT count(*) FROM slack_watch WHERE workspace_id=$1`, swWorkspace); n != 0 {
		t.Errorf("a denied slack_watch_add left %d rows in slack_watch, want 0", n)
	}
}
