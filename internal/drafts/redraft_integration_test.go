//go:build integration

package drafts_test

// SWT-43 (docs/tickets/delivery-deny_SPEC.md) criteria 19-21, 24 and 26,
// against a real database (IK landmine 6: a guard fed by a column needs a test
// that makes POSTGRES produce the value).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run DraftsRedraft ./internal/drafts/
//
// IMPOSED SURFACE (SPEC criteria 19, 20):
//
//	-- DeliverTasks' blocking clause
//	NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.task_id = t.parent_id
//	            AND NOT (d.status = 'rejected' AND d.redraft_requested_at IS NOT NULL))
//	-- plus a LEFT JOIN LATERAL on the parent's newest redraft-requested rejected
//	-- row (ORDER BY d.id DESC LIMIT 1) feeding
//	DeliverTask.RedraftOf int64, .RejectedBody string, .RejectionNote string
//
// MUTATIONS that must turn this file red (SPEC criterion 21):
//   - drop the new AND NOT (...)          -> (b) goes unlisted
//   - replace it with d.status <> 'rejected' -> (a) goes listed
//   - replace d.rejection_note with ''    -> (b) goes red
//   - drop the LATERAL's ORDER BY         -> (d) carries the OLDER note
//
// GREENFIELD NOTE — EXPECTED RED: the DeliverTask fields do not exist (the
// package's tests compile-FAIL), and before 0028 no fixture can seed a
// 'rejected' row.
//
// Cross-suite discipline: store_integration_test.go's dsFixture (itest-dstore-%
// projects, its thread list now including dsRedraftThread, FK-ordered cleanup
// at start and end). The end-to-end Run is SCOPED to this test's Deliver tasks
// by rdScopedStore — the queue read is the real PGStore, but drafts.Run with a
// real executor must never draft into another suite's leftover tasks. Its
// ai_runs rows are tagged by model name and deleted. Never 192.168.50.49.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/drafts"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/provider"
	"github.com/sspataro57/switchboard/internal/tools"
)

const rdModel = "itest-redraft-model"

// rdSeedRejected writes a rejected gmail row on the parent. A rejection_note
// on a non-rejected row, or any row at status 'rejected', needs 0028.
func rdSeedRejected(t *testing.T, ctx context.Context, f *dsFixture, parentID int64, body, note string, redraft bool) int64 {
	t.Helper()
	var n any
	if note != "" {
		n = note
	}
	var id int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, body, status, rejection_note, redraft_requested_at, created_by)
		 VALUES ($1,'gmail',$2,'rejected',$3, CASE WHEN $4::boolean THEN now() END, 'drafts:gpt') RETURNING id`,
		parentID, body, n, redraft).Scan(&id); err != nil {
		t.Fatalf("seed rejected delivery on task %d: %v (needs migration 0028)", parentID, err)
	}
	return id
}

func rdQueue(t *testing.T, ctx context.Context, f *dsFixture) map[int64][]drafts.DeliverTask {
	t.Helper()
	tasks, err := drafts.NewStore(f.pool).DeliverTasks(ctx, drafts.Config{})
	if err != nil {
		t.Fatalf("DeliverTasks: %v", err)
	}
	out := map[int64][]drafts.DeliverTask{}
	for _, dt := range tasks {
		out[dt.DeliverTaskID] = append(out[dt.DeliverTaskID], dt)
	}
	return out
}

func TestDraftsRedraft_Integration_QueueHonoursTheRedraftColumn(t *testing.T) {
	ctx := context.Background()
	f := newDSFixture(t, ctx)

	control, _, _ := f.project(t, ctx, projectSpec{name: "rd-control", client: "RD Control"})
	aDeliver, aParent, _ := f.project(t, ctx, projectSpec{name: "rd-plain", client: "RD Plain"})
	bDeliver, bParent, _ := f.project(t, ctx, projectSpec{name: "rd-redraft", client: "RD Redraft"})
	cDeliver, cParent, _ := f.project(t, ctx, projectSpec{name: "rd-redrafted", client: "RD Redrafted"})
	dDeliver, dParent, _ := f.project(t, ctx, projectSpec{name: "rd-twice", client: "RD Twice"})

	// (a) plain Deny: keeps blocking exactly as today.
	rdSeedRejected(t, ctx, f, aParent, "a rejected body", "not needed", false)
	// (b) Redo: stops blocking, and carries its body and note.
	bID := rdSeedRejected(t, ctx, f, bParent, "Hi, apologies for the delay on the staging fix.", "shorter, no apology", true)
	// (c) Redo, then the new draft was written: the new row blocks again (the loop bound).
	rdSeedRejected(t, ctx, f, cParent, "c rejected body", "c reason", true)
	f.ins(t, ctx, `INSERT INTO deliveries (task_id, channel, body, status, created_by)
	               VALUES ($1,'gmail','the new draft','drafted','drafts:gpt') RETURNING id`, cParent)
	// (d) two Redos: listed once, with the NEWER row's note.
	rdSeedRejected(t, ctx, f, dParent, "first rejected body", "first reason", true)
	dNewer := rdSeedRejected(t, ctx, f, dParent, "second rejected body", "second reason", true)

	q := rdQueue(t, ctx, f)

	// Control first: a parent with no delivery reaches the queue with the three
	// fields zero. Without it, "not listed" below could mean "never qualified".
	if got := q[control]; len(got) != 1 {
		t.Fatalf("control Deliver task %d listed %d times, want 1 — the fixture does not reach the queue", control, len(got))
	} else if got[0].RedraftOf != 0 || got[0].RejectedBody != "" || got[0].RejectionNote != "" {
		t.Errorf("control carries redraft fields %+v; with no rejected row they are zero", got[0])
	}

	if n := len(q[aDeliver]); n != 0 {
		t.Errorf("(a) a PLAIN-rejected row's Deliver task is listed (%d); a Deny must keep blocking — "+
			"'no delivery for the parent other than a rejected row with a redraft requested'", n)
	}

	if got := q[bDeliver]; len(got) != 1 {
		t.Errorf("(b) the redraft-requested Deliver task is listed %d times, want 1. Redo must make exactly that "+
			"row stop blocking (D3)", len(got))
	} else {
		if got[0].RedraftOf != bID {
			t.Errorf("(b) RedraftOf = %d, want the rejected row %d", got[0].RedraftOf, bID)
		}
		if got[0].RejectionNote != "shorter, no apology" {
			t.Errorf("(b) RejectionNote = %q, want the stored note — read from the column, not a fixture", got[0].RejectionNote)
		}
		if got[0].RejectedBody != "Hi, apologies for the delay on the staging fix." {
			t.Errorf("(b) RejectedBody = %q, want the stored body", got[0].RejectedBody)
		}
	}

	if n := len(q[cDeliver]); n != 0 {
		t.Errorf("(c) listed %d time(s) although a newer drafted row exists; the new draft blocks again, which "+
			"is the loop bound (one human click per re-draft)", n)
	}

	if got := q[dDeliver]; len(got) != 1 {
		t.Errorf("(d) two redraft-rejected rows: listed %d times, want exactly 1 (the LATERAL is LIMIT 1)", len(got))
	} else {
		if got[0].RedraftOf != dNewer || got[0].RejectionNote != "second reason" || got[0].RejectedBody != "second rejected body" {
			t.Errorf("(d) carries RedraftOf=%d note=%q body=%q, want the NEWER row %d (second reason / second "+
				"rejected body): the LATERAL orders by d.id DESC", got[0].RedraftOf, got[0].RejectionNote,
				got[0].RejectedBody, dNewer)
		}
	}
}

// ---- criteria 24 and 26, end to end through drafts.Run --------------------------

// rdScopedStore reads the REAL queue and keeps only this test's tasks.
type rdScopedStore struct {
	inner *drafts.PGStore
	keep  map[int64]bool
}

func (s rdScopedStore) DeliverTasks(ctx context.Context, cfg drafts.Config) ([]drafts.DeliverTask, error) {
	all, err := s.inner.DeliverTasks(ctx, cfg)
	if err != nil {
		return nil, err
	}
	var out []drafts.DeliverTask
	for _, dt := range all {
		if s.keep[dt.DeliverTaskID] {
			out = append(out, dt)
		}
	}
	return out, nil
}

func (s rdScopedStore) RecordRun(ctx context.Context, run drafts.AIRun) (int64, error) {
	return s.inner.RecordRun(ctx, run)
}

// rdClient is the fake hosted lane: a canned schema-valid draft, and every
// user prompt it was shown.
type rdClient struct {
	calls int
	users []string
}

func (c *rdClient) Describe() provider.Descriptor {
	return provider.Descriptor{Name: "itest-redraft-hosted", Endpoint: "https://api.example.test/v1"}
}

func (c *rdClient) Complete(_ context.Context, req provider.Request) (provider.Response, error) {
	c.calls++
	c.users = append(c.users, req.User)
	return provider.Response{Raw: json.RawMessage(`{"subject":"Re: itest-dstore","body":"Fix is live. Please retest."}`)}, nil
}

func rdCleanupRuns(t *testing.T, ctx context.Context, f *dsFixture) {
	t.Helper()
	if _, err := f.pool.Exec(ctx, `DELETE FROM ai_runs WHERE model=$1`, rdModel); err != nil {
		t.Fatalf("cleanup ai_runs: %v", err)
	}
}

func TestDraftsRedraft_Integration_RunRedraftsOnceThenStops(t *testing.T) {
	ctx := context.Background()
	f := newDSFixture(t, ctx)
	rdCleanupRuns(t, ctx, f)
	t.Cleanup(func() { rdCleanupRuns(t, ctx, f) })

	// The Redo task: a gmail thread in dsAccount's mailbox (so draft_delivery
	// can resolve From), attributed to its 'any' project (so the fold is general
	// for the right reason — see the locality tests' header).
	threadID := f.thread(t, ctx, dsRedraftThread, 30)
	redoDeliver, redoParent, redoProject := f.project(t, ctx, projectSpec{
		name: "rd-e2e", client: "RD E2E", refSystem: "gmail", refKey: dsRedraftThread,
	})
	f.attributeThread(t, ctx, threadID, redoProject)
	const rejectedBody = "Hi, apologies for the long delay, we sincerely regret the inconvenience."
	const note = "shorter, no apology"
	rejectedID := rdSeedRejected(t, ctx, f, redoParent, rejectedBody, note, true)

	// The Deny task: plain-rejected, nothing may be drafted for it.
	denyDeliver, denyParent, _ := f.project(t, ctx, projectSpec{name: "rd-e2e-deny", client: "RD Deny"})
	rdSeedRejected(t, ctx, f, denyParent, "an unwanted draft", "not needed", false)

	reg := executor.NewRegistry()
	tools.Register(reg, f.pool)
	ex := executor.New(reg, policy.NewStatic(reg.Names()...), audit.NewMemStore())
	client := &rdClient{}
	store := rdScopedStore{inner: drafts.NewStore(f.pool), keep: map[int64]bool{redoDeliver: true, denyDeliver: true}}
	router := provider.NewRouter(client, nil, time.Minute)
	cfg := drafts.Config{Model: rdModel, MaxTokens: 256}

	stats, err := drafts.Run(ctx, store, router, ex, cfg)
	if err != nil {
		t.Fatalf("first Run: %v (stats %+v)", err, stats)
	}
	if stats.Drafted != 1 || client.calls != 1 {
		t.Fatalf("first Run drafted %d with %d model call(s), want exactly 1 and 1: the Redo task drafts, the "+
			"Deny task is never listed", stats.Drafted, client.calls)
	}
	user := client.users[0]
	if !strings.Contains(user, rejectedBody) || !strings.Contains(user, note) {
		t.Errorf("the redraft prompt lacks the rejected draft or the note (criterion 22):\n%s", user)
	}

	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", q, err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM deliveries WHERE task_id=$1 AND status='drafted' AND created_by='drafts:gpt'`, redoParent); n != 1 {
		t.Errorf("drafted rows on the Redo parent = %d, want 1 — a NEW row, through draft_delivery (criterion 24)", n)
	}
	if n := count(`SELECT count(*) FROM deliveries WHERE task_id=$1`, denyParent); n != 1 {
		t.Errorf("deliveries on the Deny parent = %d, want only the rejected row: after a Deny, no drafts run "+
			"writes anything for that task", n)
	}
	if n := count(`SELECT count(*) FROM task_events WHERE task_id=$1`, denyDeliver); n != 0 {
		t.Errorf("the Deny task's Deliver task got %d event(s) (a draft_skip means it was LISTED, not blocked)", n)
	}

	var input map[string]any
	var raw []byte
	if err := f.pool.QueryRow(ctx,
		`SELECT input::text FROM ai_runs WHERE model=$1 ORDER BY id DESC LIMIT 1`, rdModel).Scan(&raw); err != nil {
		t.Fatalf("read the redraft's ai_runs row: %v", err)
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		t.Fatalf("ai_runs.input: %v", err)
	}
	if got, _ := input["redraft_of_delivery_id"].(float64); int64(got) != rejectedID {
		t.Errorf("ai_runs.input redraft_of_delivery_id = %v, want the rejected row %d — it is the only link "+
			"between the redraft and the row it replaces (criterion 24)", input["redraft_of_delivery_id"], rejectedID)
	}
	if input["prompt_version"] != "drafts-v2" {
		t.Errorf("ai_runs.input prompt_version = %v, want drafts-v2", input["prompt_version"])
	}

	// Criterion 26: the new drafted row blocks. A second pass drafts nothing.
	stats, err = drafts.Run(ctx, store, router, ex, cfg)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if stats.Drafted != 0 || client.calls != 1 {
		t.Errorf("second Run drafted %d with %d total model call(s), want 0 and still 1. One human click per "+
			"re-draft is the loop bound (D3)", stats.Drafted, client.calls)
	}
	if n := count(`SELECT count(*) FROM deliveries WHERE task_id=$1 AND status='drafted'`, redoParent); n != 1 {
		t.Errorf("drafted rows on the Redo parent after the second Run = %d, want still 1", n)
	}
}
