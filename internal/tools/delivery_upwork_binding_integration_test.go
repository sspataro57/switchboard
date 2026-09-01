//go:build integration

package tools_test

// SWT-20 acceptance criteria 9, 10, 11, 12 and 13: `draft_delivery` reopens for
// `upwork_chat`, bound SERVER-SIDE to the task's recorded source conversation.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run UpworkBinding ./internal/tools/
//
// THE SHAPE OF THE GATE, because it is easy to read as the thing the IK entry on
// actor prefixes forbids, and it is not:
//
//	The CLIENT BINDING is unconditional. Every actor — dashboard, opsctl, an
//	agent over MCP, an interactive session over MCP, `drafts:gpt`, a bare
//	`worker:` — may only draft into the conversation partner the task's
//	provenance names. That restriction is NOT keyed on the caller, which is
//	exactly what the IK entry demands ("if a restriction exists to stop an
//	untrusted or automated caller, do not key it on the actor prefix").
//
//	The ONE actor-keyed decision is who may pick a DIFFERENT ROOM inside that
//	already-bound client (criterion 12) — finding 3's "explicit human choice".
//	It restricts nothing that the binding has not already restricted; it only
//	decides who may exercise a choice among conversations that are all provably
//	the same partner. It uses `policy.HumanActor` (the gate `drafts:gpt`
//	correctly fails) rather than `executor.ViaMCP` (which `drafts:gpt`
//	correctly passes, which is why ViaMCP is useless here).
//
// Q1 was answered (a) on 2026-08-31: the reopening applies to EVERY actor from
// day one, `drafts:gpt` included. Nothing sends without a human either way —
// `send_delivery` stays `channel_assisted`-denied and approve/mark are
// human-only — so day-one exposure is approval-queue rows plus model spend, and
// the earned-autonomy rule in the policy matrix gates SENDING tiers, not draft
// creation.
//
// GREENFIELD NOTE: red today, in three layers. (1) `tasks.source_thread_id`,
// `deliveries.target_client_ref` and `deliveries_upwork_identity_check` do not
// exist until migration 0019 is applied to the compose db. (2) `draft_delivery`
// still refuses `upwork_chat` outright for every actor
// (delivery.go:279-281) — the pass-four closure this ticket lifts. (3) Nothing
// writes `target_client_ref` or binds a target to a client.
//
// Mutual-cleanup pact: this suite owns projects slugged `itest-upb-%`, the two
// client uuids below and their thread keys, and every actor ending
// `:itest-upb`. Cleaned in FK order before and after. No global count
// assertions.

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
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	upbSlug = "itest-upb-proj"
	// Two DIFFERENT conversation partners. Criterion 11 is the whole reason the
	// second one exists, so the fixture guard below asserts they differ.
	upbClientA = "aaaa2020-0000-0000-0000-0000000upb1"
	upbClientB = "bbbb2020-0000-0000-0000-0000000upb2"
	upbRoomOne = "room_upb1a2b3c4d"
	upbRoomTwo = "room_upb9f8e7d6c"

	upbBody = "Deployed to staging this morning; the reconciliation numbers follow tomorrow."
)

func upbRoomedKey(client, room string) string { return "upwork_crm:" + client + ":room:" + room }
func upbLegacyKey(client string) string       { return "upwork_crm:" + client + ":upwork" }

// The six actor shapes the SWT-19 closure test pinned, reused verbatim
// (criterion 10: "reusing the actor list already pinned by SWT-19's closure
// test"). One of them is usually the hole; `drafts:gpt` was.
var upbAllActors = []string{
	"dashboard:salvo@itest-upb",
	"opsctl:itest-upb",
	"mcp:worker:itest-upb",
	"mcp:manual:itest-upb",
	"drafts:gpt",
	"worker:itest-upb",
}

func upbOpen(t *testing.T, ctx context.Context) (*pgxpool.Pool, *executor.Executor) {
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
	t.Cleanup(pool.Close)
	upbCleanup(t, ctx, pool)
	t.Cleanup(func() { upbCleanup(t, ctx, pool) })

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	// The real matrix: draft_delivery is not humanOnly and not send-shaped, so
	// nothing here is decided by policy — every refusal below must come from the
	// HANDLER's binding, after validate and after the policy check, so that an
	// attempted cross-client target is AUDITED (SPEC "API / MCP tool changes").
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	return pool, executor.New(reg, checker, audit.NewPGStore(pool))
}

func upbCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const projs = `(SELECT id FROM projects WHERE slug LIKE 'itest-upb-%')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	threads := []string{
		upbRoomedKey(upbClientA, upbRoomOne), upbRoomedKey(upbClientA, upbRoomTwo),
		upbLegacyKey(upbClientA), upbRoomedKey(upbClientB, upbRoomOne), upbLegacyKey(upbClientB),
	}
	stmts := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM policy_decisions WHERE audit_event_id IN
		    (SELECT id FROM audit_events WHERE actor LIKE '%itest-upb%' OR actor='drafts:gpt'
		       OR task_id IN ` + tasksOf + `)`, nil},
		{`DELETE FROM audit_events WHERE actor LIKE '%itest-upb%' OR actor='drafts:gpt'
		    OR task_id IN ` + tasksOf, nil},
		{`DELETE FROM task_events WHERE task_id IN ` + tasksOf, nil},
		{`DELETE FROM deliveries WHERE task_id IN ` + tasksOf, nil},
		{`DELETE FROM external_refs WHERE task_id IN ` + tasksOf, nil},
		{`DELETE FROM tasks WHERE parent_id IS NOT NULL AND project_id IN ` + projs, nil},
		{`DELETE FROM tasks WHERE project_id IN ` + projs, nil},
		{`DELETE FROM projects WHERE slug LIKE 'itest-upb-%'`, nil},
		{`DELETE FROM normalized_threads WHERE thread_key = ANY($1)`, []any{threads}},
	}
	for _, st := range stmts {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

type upbFixture struct {
	pool *pgxpool.Pool

	provenanceThread int64 // client A, room one — what the task records
	provenanceKey    string
	otherRoomThread  int64 // client A, room two — same partner, different room
	otherRoomKey     string
	otherClientKey   string // client B, room one — a DIFFERENT partner

	task     int64 // carries provenance
	bareTask int64 // records nothing, and its parent records nothing either
}

func upbSeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool) upbFixture {
	t.Helper()
	if upbClientA == upbClientB {
		t.Fatalf("fixture invalid: the two client ids are the same string")
	}
	ins := func(q string, args ...any) int64 {
		var id int64
		if err := pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
		return id
	}
	thread := func(key string) int64 {
		return ins(`INSERT INTO normalized_threads (thread_key, subject, participants)
		            VALUES ($1,'itest-upb','[]') RETURNING id`, key)
	}
	f := upbFixture{pool: pool}
	f.provenanceKey = upbRoomedKey(upbClientA, upbRoomOne)
	f.otherRoomKey = upbRoomedKey(upbClientA, upbRoomTwo)
	f.otherClientKey = upbRoomedKey(upbClientB, upbRoomOne)
	f.provenanceThread = thread(f.provenanceKey)
	f.otherRoomThread = thread(f.otherRoomKey)
	thread(f.otherClientKey)
	// Legacy threads for both clients: real corpus shape, and they must never
	// become a target by accident.
	thread(upbLegacyKey(upbClientA))
	thread(upbLegacyKey(upbClientB))

	proj := ins(`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
	             VALUES ($1,$1,'itest-upb client','manual','dashboard','/tmp/itest-upb','any') RETURNING id`, upbSlug)
	f.task = ins(`INSERT INTO tasks (project_id, title, assignee_type, status, source_thread_id)
	              VALUES ($1,'itest-upb work','claude','done_locally',$2) RETURNING id`, proj, f.provenanceThread)
	bareParent := ins(`INSERT INTO tasks (project_id, title, assignee_type, status)
	                   VALUES ($1,'itest-upb unprovenanced work','claude','done_locally') RETURNING id`, proj)
	f.bareTask = ins(`INSERT INTO tasks (project_id, parent_id, title, assignee_type, status)
	                  VALUES ($1,$2,'Deliver #itest-upb bare','claude','ready') RETURNING id`, proj, bareParent)
	return f
}

func upbDraft(ctx context.Context, ex *executor.Executor, actor string, taskID int64, target string) (int64, error) {
	args, err := json.Marshal(map[string]any{
		"task_id": taskID, "channel": "upwork_chat", "body": upbBody, "target_ref": target,
	})
	if err != nil {
		return 0, err
	}
	res, err := ex.Execute(ctx, executor.Call{
		Tool: "draft_delivery", Actor: actor, Args: args, TaskID: &taskID,
	})
	if err != nil {
		return 0, err
	}
	var out struct {
		DeliveryID int64 `json:"delivery_id"`
	}
	if uerr := json.Unmarshal(res.Output, &out); uerr != nil {
		return 0, fmt.Errorf("parse draft_delivery result %s: %w", res.Output, uerr)
	}
	return out.DeliveryID, nil
}

// ---- criterion 9: the channel reopens, and the row carries its identity --------

// The automated path: after §3, the drafts worker produces EXACTLY the
// provenance key as its target_ref, so this is the call the worker will make on
// its first run after deploy.
//
// The row is asserted whole, because every column here has a downstream reader
// that fails silently without it: `target_ref` is what the matcher parses,
// `target_client_ref` is what the shortlist selects on (and what the CHECK
// forces to exist), and `thread_id`'s FK is what SUBSUMES the old
// `EXISTS(thread_key=...)` probe (§4 step 5, D5).
func TestUpworkBinding_Integration_ProvenanceTargetIsAccepted(t *testing.T) {
	ctx := context.Background()
	pool, ex := upbOpen(t, ctx)
	f := upbSeed(t, ctx, pool)

	id, err := upbDraft(ctx, ex, "drafts:gpt", f.task, f.provenanceKey)
	if err != nil {
		t.Fatalf("draft_delivery on the task's OWN recorded conversation was refused: %v.\n"+
			"Criterion 9 lifts the pass-four closure, and Q1 was answered (a): the reopening applies to every "+
			"actor including drafts:gpt, because the trust boundary is the server-side binding, not the caller", err)
	}
	if id == 0 {
		t.Fatalf("draft_delivery returned no delivery_id")
	}

	var (
		channel, targetRef, status string
		threadID                   *int64
	)
	if err := pool.QueryRow(ctx,
		`SELECT channel, COALESCE(target_ref,''), status, thread_id
		   FROM deliveries WHERE id=$1`, id).Scan(&channel, &targetRef, &status, &threadID); err != nil {
		t.Fatalf("read delivery %d: %v", id, err)
	}
	if channel != "upwork_chat" {
		t.Errorf("channel = %q, want upwork_chat", channel)
	}
	if targetRef != f.provenanceKey {
		t.Errorf("target_ref = %q, want the canonical provenance key %q. The matcher parses this string; a "+
			"non-canonical spelling is permanently unconfirmable with no error anywhere (SWT-13)",
			targetRef, f.provenanceKey)
	}
	if status != "drafted" {
		t.Errorf("status = %q, want drafted", status)
	}
	if threadID == nil || *threadID != f.provenanceThread {
		t.Errorf("thread_id = %v, want the provenance thread %d. Its FK is what proves the target names an "+
			"ingested thread — it replaces the ad-hoc EXISTS(thread_key) probe, and it is what a future "+
			"outbound_observed for this channel will key on (D5)", threadID, f.provenanceThread)
	}
}

// Criterion 9's identity half, read separately so a failure names the column.
func TestUpworkBinding_Integration_TargetClientRefIsPersisted(t *testing.T) {
	ctx := context.Background()
	pool, ex := upbOpen(t, ctx)
	f := upbSeed(t, ctx, pool)

	id, err := upbDraft(ctx, ex, "opsctl:itest-upb", f.task, f.provenanceKey)
	if err != nil {
		t.Fatalf("draft_delivery: %v", err)
	}
	var clientRef *string
	if err := pool.QueryRow(ctx, `SELECT target_client_ref FROM deliveries WHERE id=$1`, id).Scan(&clientRef); err != nil {
		t.Fatalf("read target_client_ref: %v", err)
	}
	if clientRef == nil || *clientRef != upbClientA {
		t.Errorf("target_client_ref = %v, want %q — extracted in GO by the SAME ParseThreadKey call that "+
			"produced the stored target_ref (§5). No SQL may build or dissect the key, so this column is the "+
			"only thing the shortlist can select on, and a row that lacks it silently drops out of the "+
			"candidate set", clientRef, upbClientA)
	}
}

// ---- criterion 10: no provenance -> refused, for all six actor shapes ----------

// The binding is NOT actor-keyed. This is the same enumeration SWT-19's closure
// test used, for the same reason: the previous gate keyed on `executor.ViaMCP`
// and therefore did nothing about `drafts:gpt`, the one component that would
// create upwork drafts automatically.
//
// The task here is a Deliver child whose PARENT also records nothing, so the
// two-level walk in `store.TaskSourceThread` is genuinely exhausted rather than
// merely not attempted.
func TestUpworkBinding_Integration_NoProvenanceRefusesEveryActor(t *testing.T) {
	ctx := context.Background()
	pool, ex := upbOpen(t, ctx)
	f := upbSeed(t, ctx, pool)

	for _, actor := range upbAllActors {
		actor := actor
		t.Run(actor, func(t *testing.T) {
			_, err := upbDraft(ctx, ex, actor, f.bareTask, f.provenanceKey)
			if err == nil {
				t.Fatalf("draft_delivery accepted an upwork_chat draft for a task that records NO source "+
					"conversation (actor %q). Without provenance there is nothing to bind the target to, and "+
					"the supplied target_ref could name any client's thread — the exposure the pass-four "+
					"closure was written for", actor)
			}
			msg := err.Error()
			if strings.Contains(msg, "denied by policy") {
				t.Fatalf("actor %q was refused by POLICY (%q). The refusal must come from the handler, after "+
					"validate and after the policy check, so an attempted mis-aimed target is AUDITED — and "+
					"draft_delivery is deliberately not humanOnly", actor, msg)
			}
			if !strings.Contains(msg, "task_set_source_thread") {
				t.Errorf("refusal for %q = %q; it must name `task_set_source_thread`. The reader is a human or a "+
					"worker log staring at a task that cannot be delivered, and the remedy — record which "+
					"conversation raised it — is not guessable from 'refused'", actor, msg)
			}
		})
	}
}

// ---- criterion 11: a DIFFERENT client is refused, humans included ---------------

// This is pass two's deferred finding, and it gets its own test because it is
// the one the old code could not make: `draft_delivery` proved the target named
// SOME ingested thread, never that it belonged to this task's conversation
// (SWT-19 SPEC lines 1049-1060).
//
// The target here is ingested, canonical and parseable — the most favourable
// wrong answer there is. Only the CLIENT differs. And it is refused for
// `dashboard:`/`opsctl:`/`manual:` too: D8 is explicit that the client binding
// is unconditional and that the actor test decides only who may pick among rooms
// INSIDE the bound client. A human may choose a room; nobody may choose a
// different conversation partner.
func TestUpworkBinding_Integration_DifferentClientRefusedForEveryActor(t *testing.T) {
	ctx := context.Background()
	pool, ex := upbOpen(t, ctx)
	f := upbSeed(t, ctx, pool)

	// Fixture guard: the wrong target must genuinely be a real, ingested thread,
	// or this test proves only that unknown targets are refused — which the
	// pre-SWT-20 EXISTS check already did.
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM normalized_threads WHERE thread_key=$1)`, f.otherClientKey).Scan(&exists); err != nil {
		t.Fatalf("check other-client thread: %v", err)
	}
	if !exists {
		t.Fatalf("fixture invalid: %q is not an ingested thread, so a refusal proves nothing new", f.otherClientKey)
	}

	for _, actor := range upbAllActors {
		actor := actor
		t.Run(actor, func(t *testing.T) {
			_, err := upbDraft(ctx, ex, actor, f.task, f.otherClientKey)
			if err == nil {
				t.Fatalf("actor %q drafted into %q — a thread of client %s — for a task whose recorded "+
					"conversation belongs to client %s. The client binding is UNCONDITIONAL (D8): the actor "+
					"test decides only who may pick among rooms of the ALREADY-BOUND client, never whether "+
					"the binding applies", actor, f.otherClientKey, upbClientB, upbClientA)
			}
			if strings.Contains(err.Error(), "names no ingested thread") {
				t.Errorf("actor %q was refused because the thread is not ingested (%q). It IS ingested; the "+
					"refusal must come from the CLIENT binding, or this test is re-proving the pre-SWT-20 "+
					"existence check", actor, err.Error())
			}
			// And it must not be the blanket pass-four closure. While
			// delivery.go:279-281 stands, EVERY upwork draft is refused and this
			// test passes for a reason that has nothing to do with the client —
			// which would make it green today and silent on the day the closure
			// lifts. Criterion 9 is what proves the channel is open; this guard
			// is what ties the two together.
			if strings.Contains(err.Error(), "upwork_chat drafts are disabled") {
				t.Errorf("actor %q hit the blanket channel closure, not the client binding (%q). Criterion 9 "+
					"lifts that closure; until it does, this test cannot distinguish a bound refusal from a "+
					"shut door", actor, err.Error())
			}
		})
	}
}

// ---- criterion 12: a different ROOM of the SAME client is human-gated ----------

// THE ONE ACTOR-KEYED DECISION IN THIS TICKET, and the comment the SPEC asks for:
//
//	The client binding above is unconditional precisely because keying a
//	RESTRICTION on the caller is a transport label doing policy work (IK). This
//	check restricts nothing further — the candidate room is already proven to
//	belong to the same conversation partner. It decides who may exercise a
//	CHOICE inside that binding: a human moving the reply into another room of
//	the same partner is finding 3's "explicit human choice", while an automated
//	caller has no basis to make it (the drafts worker's own §3 resolution
//	always produces the provenance key, so it never needs to).
//
//	The gate is `policy.HumanActor`, not `executor.ViaMCP`. `drafts:gpt`
//	correctly FAILS HumanActor and correctly PASSES ViaMCP — which is why a
//	ViaMCP gate here would be exactly the defect SWT-19 shipped and had to
//	revert.
//
// NOTE ON CRITERION 12'S WORDING, and a judgement call this file makes
// deliberately: the criterion says "refused for drafts:gpt and for mcp:*". Taken
// literally that would put `mcp:manual:salvo` on the refusal side, which
// contradicts criterion 18 and §4 — `policy.HumanActor` strips ONE transport
// prefix, so an interactive session over MCP IS a human (the SWT-11 decision;
// the audit row keeps the full actor either way). This file reads `mcp:*` as the
// AGENT transport, `mcp:worker:`, and puts `mcp:manual:` with the humans. If the
// implementer disagrees, the fix is a one-line move here plus a note in the
// SPEC — but it must not be resolved by making the handler restate the prefixes
// instead of calling HumanActor.
func TestUpworkBinding_Integration_OtherRoomOfTheSameClientIsHumanGated(t *testing.T) {
	ctx := context.Background()
	pool, ex := upbOpen(t, ctx)
	f := upbSeed(t, ctx, pool)

	if f.otherRoomKey == f.provenanceKey {
		t.Fatalf("fixture invalid: the two rooms are the same key (%q)", f.provenanceKey)
	}

	t.Run("refused for automated callers", func(t *testing.T) {
		for _, actor := range []string{"drafts:gpt", "mcp:worker:itest-upb", "worker:itest-upb"} {
			actor := actor
			t.Run(actor, func(t *testing.T) {
				_, err := upbDraft(ctx, ex, actor, f.task, f.otherRoomKey)
				if err == nil {
					t.Errorf("actor %q drafted into room %q while the task records room %q. Choosing among a "+
						"client's rooms is an explicit human decision (finding 3); an automated caller has no "+
						"basis for it, and §3 makes the drafts worker produce the provenance key anyway",
						actor, f.otherRoomKey, f.provenanceKey)
					return
				}
				// Same guard as criterion 11's: while the blanket closure stands,
				// every actor is refused and this assertion means nothing.
				if strings.Contains(err.Error(), "upwork_chat drafts are disabled") {
					t.Errorf("actor %q hit the blanket channel closure, not the room gate (%q)", actor, err.Error())
				}
			})
		}
	})

	t.Run("accepted for human actors", func(t *testing.T) {
		for _, actor := range []string{
			"dashboard:salvo@itest-upb",
			"opsctl:itest-upb",
			"manual:itest-upb",
			// One transport prefix is stripped: an interactive session is a human.
			"mcp:manual:itest-upb",
		} {
			actor := actor
			t.Run(actor, func(t *testing.T) {
				id, err := upbDraft(ctx, ex, actor, f.task, f.otherRoomKey)
				if err != nil {
					t.Fatalf("human actor %q was refused a room of the SAME bound client (%q -> %q): %v. "+
						"The human path for moving a reply into another room of the same conversation partner "+
						"is `opsctl call draft_delivery` — there is no dashboard UI for it (out of scope)",
						actor, f.provenanceKey, f.otherRoomKey, err)
				}
				var target string
				var clientRef *string
				var threadID *int64
				if err := pool.QueryRow(ctx,
					`SELECT COALESCE(target_ref,''), target_client_ref, thread_id FROM deliveries WHERE id=$1`,
					id).Scan(&target, &clientRef, &threadID); err != nil {
					t.Fatalf("read delivery %d: %v", id, err)
				}
				if target != f.otherRoomKey {
					t.Errorf("target_ref = %q, want the room the human chose (%q)", target, f.otherRoomKey)
				}
				if clientRef == nil || *clientRef != upbClientA {
					t.Errorf("target_client_ref = %v, want %q — the bound client, whichever room was chosen",
						clientRef, upbClientA)
				}
				if threadID == nil || *threadID != f.otherRoomThread {
					t.Errorf("thread_id = %v, want the CHOSEN room's thread %d, not the provenance thread %d — "+
						"the delivery records where it is actually aimed", threadID, f.otherRoomThread, f.provenanceThread)
				}
				// One draft per actor: leave the row so the next actor's insert is
				// independent, and let cleanup remove them all.
			})
		}
	})
}

// ---- the canonicalization survives the reopened door (SWT-13's landmine) -------

// The deleted SWT-19 storage test proved draft_delivery persists the CANONICAL
// spelling; with the channel reopened the same rule must hold on the live
// path, because the matcher parses target_ref and a stored non-canonical
// spelling is permanently unconfirmable with no error anywhere.
func TestUpworkBinding_Integration_NonCanonicalSpellingIsStoredCanonical(t *testing.T) {
	ctx := context.Background()
	pool, ex := upbOpen(t, ctx)
	f := upbSeed(t, ctx, pool)

	// Whitespace padding is the non-canonical variant that still parses; the
	// handler must canonicalize BEFORE comparing against the provenance key,
	// or the drafts worker's exact key would be refused whenever a human's
	// copy-paste carried a stray space.
	id, err := upbDraft(ctx, ex, "opsctl:itest-upb", f.task, "  "+f.provenanceKey+" ")
	if err != nil {
		t.Fatalf("draft_delivery refused a whitespace-padded spelling of the provenance key: %v — "+
			"canonicalization must happen before the binding comparison", err)
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(target_ref,'') FROM deliveries WHERE id=$1`, id).Scan(&stored); err != nil {
		t.Fatalf("read delivery %d: %v", id, err)
	}
	if stored != f.provenanceKey {
		t.Errorf("stored target_ref = %q, want the canonical %q — the matcher parses this string, and a "+
			"non-canonical spelling never confirms (SWT-13)", stored, f.provenanceKey)
	}
}

// ---- criterion 13: the CHECK constraint bites ----------------------------------

// D4, and the reason this constraint ships at zero rows: a derived column that
// some path forgets to write is this repo's most expensive recurring bug (IK:
// "a guard whose column no query selected"; "test the column, not the fixture").
//
// Without the CHECK, an `upwork_chat` row missing `target_client_ref` does not
// error — it silently drops out of the shortlist, so the delivery can never
// confirm, and the only thing that surfaces it is the reconciler ~45 minutes
// later. With the CHECK, Postgres refuses the INSERT, loudly, in every fixture
// and every future write path. It cannot be added later without a backfill;
// production has zero `upwork_chat` deliveries and never had one.
//
// Asserted with a RAW INSERT on purpose: the point is that the guard holds for
// writers that never go near `draftDelivery` — the 23 test fixtures, a psql
// session, a future tool.
func TestUpworkBinding_Integration_IdentityCheckRefusesAnIncompleteRow(t *testing.T) {
	ctx := context.Background()
	pool, ex := upbOpen(t, ctx)
	_ = ex
	f := upbSeed(t, ctx, pool)

	cases := []struct {
		name string
		sql  string
		args []any
	}{
		{
			"neither target_client_ref nor thread_id",
			`INSERT INTO deliveries (task_id, channel, target_ref, body, status)
			 VALUES ($1,'upwork_chat',$2,$3,'drafted')`,
			[]any{f.task, f.provenanceKey, upbBody},
		},
		{
			"thread_id but no target_client_ref",
			`INSERT INTO deliveries (task_id, channel, target_ref, body, status, thread_id)
			 VALUES ($1,'upwork_chat',$2,$3,'drafted',$4)`,
			[]any{f.task, f.provenanceKey, upbBody, f.provenanceThread},
		},
		{
			"target_client_ref but no thread_id",
			`INSERT INTO deliveries (task_id, channel, target_ref, body, status, target_client_ref)
			 VALUES ($1,'upwork_chat',$2,$3,'drafted',$4)`,
			[]any{f.task, f.provenanceKey, upbBody, upbClientA},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, tc.sql, tc.args...)
			if err == nil {
				t.Fatalf("Postgres accepted an upwork_chat delivery with %s. Criterion 13: every upwork_chat row "+
					"is STRUCTURALLY forced to carry its identity, because a row that lacks it does not error — "+
					"it silently shrinks the shortlist and the delivery can never confirm", tc.name)
			}
			if !strings.Contains(err.Error(), "deliveries_upwork_identity_check") {
				t.Errorf("refused with %v, want a violation of deliveries_upwork_identity_check. If the refusal "+
					"came from a NOT NULL or a FK instead, the guard is somewhere else and does not say what it "+
					"means", err)
			}
		})
	}

	// The other side of the constraint: it must not touch any other channel.
	// `channel <> 'upwork_chat' OR (...)` — a gmail or slack row with neither
	// column is normal and must still insert.
	t.Run("other channels are untouched", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`INSERT INTO deliveries (task_id, channel, target_ref, body, status)
			 VALUES ($1,'slack_reply','https://app.slack.com/client/TUPB/CUPB',$2,'drafted')`,
			f.task, upbBody); err != nil {
			t.Errorf("the identity CHECK rejected a slack_reply row: %v. It is scoped to upwork_chat; widening "+
				"it would break every other channel's fixtures and every real gmail draft", err)
		}
	})
}
