//go:build integration

package tools_test

// Integration test for the storage half of SWT-19 criterion 7: draft_delivery
// persists the CANONICAL spelling of an upwork_chat target_ref, not the
// caller's.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run UpworkTarget ./internal/tools/
//
// This is the SWT-13 fix's shape, transplanted. There, slackweb.ParseTargetURL
// accepted a trailing slash while confirmDelivery matched target_ref by EXACT
// string, so a delivery drafted with the caller's spelling could never be
// confirmed — silently, with no error anywhere. draft_delivery now stores
// Target.CanonicalURL() (delivery.go), and the same rule must hold here: since
// SWT-18 the upwork matcher compares target_ref exactly, and since SWT-19 it
// parses it, so a stored spelling that differs from the normalizer's is a
// delivery that never closes its loop.
//
// GREENFIELD NOTE: fails until draftDelivery writes upworkcrm.ThreadKey(parsed)
// instead of the raw argument. Expected red state.
//
// Cross-suite discipline: everything is scoped to the itest-utg-% slug and
// cleaned in FK order, before and after, so the suite is rerunnable against the
// persistent compose db.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	utgActor  = "dashboard:itest-utg@example.com"
	utgSlug   = "itest-utg-proj"
	utgClient = "e2ef9b65-9813-4d79-ac10-0e1813f788ff" // a real client uuid shape
	utgRoom   = "room_1a2b3c4d5e"
)

func cleanupUpworkTarget(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	stmts := []string{
		// policy_decisions before audit_events: the FK points that way
		// (lifecycle_integration_test.go:80-82's order).
		`DELETE FROM policy_decisions WHERE audit_event_id IN
			(SELECT id FROM audit_events WHERE actor='` + utgActor + `')`,
		`DELETE FROM audit_events WHERE actor='` + utgActor + `'`,
		`DELETE FROM task_events WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + utgSlug + `'))`,
		`DELETE FROM deliveries WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + utgSlug + `'))`,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + utgSlug + `')`,
		`DELETE FROM projects WHERE slug='` + utgSlug + `'`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
}

func TestUpworkTarget_Integration_DraftStoresCanonicalSpelling(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	defer pool.Close()

	cleanupUpworkTarget(t, ctx, pool)
	defer cleanupUpworkTarget(t, ctx, pool)

	projID := seedProject(t, ctx, pool, utgSlug, "itest-utg-client")
	var taskID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'itest-utg work','claude','done_locally') RETURNING id`, projID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	ex := newExecutor(pool)

	roomed := "upwork_crm:" + utgClient + ":room:" + utgRoom
	unroomed := "upwork_crm:" + utgClient + ":upwork"

	cases := []struct {
		name, given, want string
	}{
		{"roomed, already canonical", roomed, roomed},
		{"unroomed legacy, already canonical", unroomed, unroomed},
		{"surrounding whitespace is trimmed", "  " + roomed + "\n", roomed},
		{"a tab-padded caller spelling", "\t" + unroomed + " ", unroomed},
	}

	// Fixture guard: at least one case must actually differ between what the
	// caller sends and what must be stored, or "stores the canonical spelling"
	// is proved by cases where the two are the same string — the failure mode IK
	// names for this exact matcher.
	differs := 0
	for _, tc := range cases {
		if tc.given != tc.want {
			differs++
		}
	}
	if differs == 0 {
		t.Fatalf("fixture invalid: every case sends exactly what it expects stored, so canonicalisation is never exercised")
	}

	// SWT-19 (Codex adversarial finding): draft_delivery requires an upwork
	// target_ref to name a thread we have actually ingested, because a
	// parseable-but-unknown key is a client-wide confirmation wildcard under the
	// unknown-room tolerance. Seed the threads these cases target — every case
	// asserts CANONICALISATION, so the target must be real for the assertion to
	// be reachable at all.
	for _, tc := range cases {
		if _, err := pool.Exec(ctx,
			`INSERT INTO normalized_threads (thread_key, participants) VALUES ($1,'[]')
			 ON CONFLICT (thread_key) WHERE thread_key IS NOT NULL DO NOTHING`, tc.want); err != nil {
			t.Fatalf("seed upwork thread %q: %v", tc.want, err)
		}
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := callOK(t, ctx, ex, utgActor, "draft_delivery",
				`{"task_id":`+itoa(taskID)+`,"channel":"upwork_chat","body":"thanks, will do","target_ref":`+
					quoteJSONIT(tc.given)+`}`)
			var r struct {
				DeliveryID int64 `json:"delivery_id"`
			}
			mustUnmarshal(t, out, &r)
			if r.DeliveryID == 0 {
				t.Fatalf("draft_delivery returned delivery_id 0")
			}
			var stored string
			if err := pool.QueryRow(ctx, `SELECT target_ref FROM deliveries WHERE id=$1`, r.DeliveryID).Scan(&stored); err != nil {
				t.Fatalf("read stored target_ref: %v", err)
			}
			if stored != tc.want {
				t.Errorf("stored target_ref = %q, want %q. The matcher compares target_ref EXACTLY against the "+
					"normalizer's thread_key, so any spelling the normalizer would not produce is a delivery that "+
					"can never be confirmed — the SWT-13 failure, which errors nowhere", stored, tc.want)
			}
		})
	}
}

func quoteJSONIT(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

// A parseable upwork target_ref that names no ingested thread is REFUSED
// (SWT-19, from the Codex adversarial pass).
//
// The hole this closes was opened by SWT-19 itself. SameConversation treats an
// unroomed key as compatible with any room of the same client — the legacy
// tolerance that keeps 576 pre-2026-07-21 outbound rows confirmable. Paired with
// a validator that checked only SHAPE, that turned any typo of the form
// upwork_crm:{real-client}:typo into a CLIENT-WIDE CONFIRMATION WILDCARD: mark it
// sent, and the next outbound message from any room of that client confirms it —
// burning that message's external id on a delivery naming a conversation which
// does not exist, and locking the real delivery out of that id permanently under
// deliveries_sent_external_idx. That is the invariant-4 damage SWT-18 existed to
// prevent, arriving through a different door.
//
// Parseable is not real. Requiring the target to name an ingested thread costs
// the tolerance nothing, because every legacy key IS one.
func TestUpworkTarget_Integration_UnknownThreadIsRefused(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	defer pool.Close()

	cleanupUpworkTarget(t, ctx, pool)
	defer cleanupUpworkTarget(t, ctx, pool)

	projID := seedProject(t, ctx, pool, utgSlug, "itest-utg-client")
	var taskID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'itest-utg unknown','claude','done_locally') RETURNING id`, projID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	ex := newExecutor(pool)

	real := "upwork_crm:" + utgClient + ":upwork"
	if _, err := pool.Exec(ctx,
		`INSERT INTO normalized_threads (thread_key, participants) VALUES ($1,'[]')
		 ON CONFLICT (thread_key) WHERE thread_key IS NOT NULL DO NOTHING`, real); err != nil {
		t.Fatalf("seed the one real thread: %v", err)
	}

	for _, tc := range []struct{ name, target, why string }{
		{
			"typo in the channel segment of a real client", "upwork_crm:" + utgClient + ":typo",
			"parses as a well-formed unroomed key, so only an existence check catches it — and under the " +
				"unknown-room tolerance it would then match any roomed message of this client",
		},
		{
			"a room this client does not have", "upwork_crm:" + utgClient + ":room:room_deadbeefdeadbeef",
			"a roomed key is narrower, but a room never ingested still names no conversation",
		},
		{
			"a client that does not exist", "upwork_crm:99999999-0000-0000-0000-000000000000:upwork",
			"shape alone cannot tell a real client uuid from an invented one",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ex.Execute(ctx, executor.Call{Tool: "draft_delivery", Actor: utgActor,
				Args: []byte(`{"task_id":` + itoa(taskID) + `,"channel":"upwork_chat","body":"thanks, will do",` +
					`"target_ref":"` + tc.target + `"}`)})
			if err == nil {
				t.Fatalf("draft_delivery accepted target_ref %q, which names no ingested thread — %s", tc.target, tc.why)
			}
			if !strings.Contains(err.Error(), "names no ingested thread") {
				t.Errorf("refused %q with %q; the error should say the target names no ingested thread, so a "+
					"reader is not left hunting a format problem that does not exist", tc.target, err.Error())
			}
		})
	}

	// The control. Without it this test would pass against a validator that
	// refused EVERY upwork target — and the legacy corpus is 2,009 of 2,441
	// messages, so that regression would be expensive and silent.
	t.Run("the real ingested thread is still accepted", func(t *testing.T) {
		if _, err := ex.Execute(ctx, executor.Call{Tool: "draft_delivery", Actor: utgActor,
			Args: []byte(`{"task_id":` + itoa(taskID) + `,"channel":"upwork_chat","body":"thanks, will do",` +
				`"target_ref":"` + real + `"}`)}); err != nil {
			t.Fatalf("draft_delivery refused the real ingested thread %q: %v", real, err)
		}
	})
}

// The SWT-20 go-live gate is ENFORCED, not just documented (SWT-19, third
// adversarial pass).
//
// The existence check above proves a target was ingested, not that it belongs to
// this task's client — and binding it needs a task-to-client relation that does
// not exist yet (the only candidate, projects.client_person_id, is dropped by
// SWT-17). draft_delivery is MCP-listed and this session ingests client mail and
// chat, so an injected call could otherwise name a real thread belonging to a
// DIFFERENT client and a human could approve and send it later.
//
// Refusing the agent surface makes the gate impossible to cross by forgetting
// about it. A human at the dashboard or opsctl is choosing deliberately and is
// not the exposure, so that path stays open.
func TestUpworkTarget_Integration_AgentDraftsAreGatedUntilProvenance(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	defer pool.Close()

	cleanupUpworkTarget(t, ctx, pool)
	defer cleanupUpworkTarget(t, ctx, pool)

	projID := seedProject(t, ctx, pool, utgSlug, "itest-utg-client")
	var taskID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'itest-utg gate','claude','done_locally') RETURNING id`, projID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	ex := newExecutor(pool)

	target := "upwork_crm:" + utgClient + ":upwork"
	if _, err := pool.Exec(ctx,
		`INSERT INTO normalized_threads (thread_key, participants) VALUES ($1,'[]')
		 ON CONFLICT (thread_key) WHERE thread_key IS NOT NULL DO NOTHING`, target); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	args := []byte(`{"task_id":` + itoa(taskID) + `,"channel":"upwork_chat","body":"thanks, will do","target_ref":"` + target + `"}`)

	// Over MCP: refused, even though the target is real and ingested.
	// ViaMCP reads the actor prefix that the executor puts into context, so the
	// mcp: actor is the whole mechanism — no separate transport flag.
	if _, err := ex.Execute(ctx, executor.Call{
		Tool: "draft_delivery", Actor: "mcp:worker:itest-utg", Args: args}); err == nil {
		t.Fatal("draft_delivery accepted an upwork_chat draft over MCP. The target cannot yet be bound to the " +
			"task's client, so an injected call could name another client's thread and a human could approve " +
			"and send it. The gate must hold until SWT-20's provenance exists")
	}

	// The same call from a human surface still works — the gate must not become
	// a blanket refusal of the channel.
	if _, err := ex.Execute(ctx, executor.Call{Tool: "draft_delivery", Actor: utgActor, Args: args}); err != nil {
		t.Fatalf("draft_delivery refused a dashboard-actor upwork draft: %v — a human choosing the target "+
			"deliberately is not the exposure, and refusing them disables the assisted tier entirely", err)
	}
}
