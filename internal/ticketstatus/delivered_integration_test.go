//go:build integration

package ticketstatus_test

// SWT-34 against a real database (docs/tickets/qa-delivered-drop_SPEC.md
// criteria 5, 19, 20, 23, 24, 28, 29): the delivered set as a COLUMN-FED
// predicate, the fold as a property of the SYSTEM rather than of a Go table, the
// widened CHECK as the database sees it, E6's second log line, the convergent
// reason change, the dry run and idempotence.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run TicketStatus_Delivered ./internal/ticketstatus/
//
// WHY THESE ARE HERE AND NOT IN decide_delivered_test.go. Institutional landmine
// 6, verbatim: "for any predicate whose input comes from a COLUMN, the
// regression test belongs in the integration suite and must fail when the column
// is dropped from the SELECT. Mutate the SELECT to a literal and watch it go
// red; if it stays green you have tested your fixture." A unit test cannot catch
// a `loadCandidates` that never selects ticket_delivered_statuses, BY
// CONSTRUCTION — the unit test is the thing supplying the value. That is the
// defect internal/drafts shipped with `ai_locality`, and SWT-32's
// TestTicketStatus_TheAssigneeGateComesFromTheProjectsColumn is the same test for
// the sibling column. The mutation that must turn each assertion red is named
// inline.
//
// NO LLM, NO LIVE JIRA, NO NETWORK OF ANY KIND. This ticket makes no HTTP call:
// the status NAME it reads was already stored by the poller, and every snapshot
// below is a raw_source_items row this suite writes itself. Config.Lookup stays
// nil — there is no jira_lookup account in this suite's fixtures, so the pass's
// lookup half has nothing to route.
//
// GREENFIELD NOTE — EXPECTED RED. migration 0025 has not been applied to the
// compose db and internal/ticketstatus does not know the column, so this file
// fails at the schema probe with one sentence (qdiRequireDeliveredColumn) rather
// than from inside cleanup, and the package's test binary does not link at all
// until Observation.DeliveredStatuses exists.
//
// ---- IMPOSED surface, beyond decide_delivered_test.go's ---------------------
//
//	// loadCandidates SELECTs p.ticket_delivered_statuses and s.status_name; the
//	// driver copies the first into Observation.DeliveredStatuses and the second
//	// into State.StatusName (criteria 20 and 22). Nothing else about the query
//	// moves, and no membership test is spelled in it (criterion 12).
//	//
//	// Stats gains ClosedTicketDelivered (criterion 25).
//
// CROSS-POLLUTION PACT (IK, "integration suites cross-pollute"; `make
// integration` runs -p 1 for this reason). This suite owns, and clears at start
// AND at end:
//   - projects         itest-qadel-%
//   - source_accounts  account_email LIKE 'itest-qadel-%'
//   - raw_source_items / sync_runs under that account
//   - tasks / external_refs / task_events / task_dismissals /
//     ticket_status_syncs of those projects
//   - audit_events + policy_decisions BY TASK (never by actor: the sibling
//     suite in this package owns the actor-wide sweep, and two suites deleting
//     each other's audit rows mid-assertion is how an idempotence check starts
//     lying)
//
// COUNTER ASSERTIONS ARE ONE-SIDED. `Run`'s candidate set is every
// `system='jira'` external_ref in the database — global, like capture's and
// promote's inboxes — so another suite's leftover ref would inflate Considered.
// The weight is carried by per-row assertions against THIS suite's tasks.
//
// ANTI-DATE-ROT: every timestamp is now() or now() - interval '...'.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/connector/jira"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/ticketstatus"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	qdiArmedProject = "itest-qadel-armed" // ticket_delivered_statuses = the two names below
	qdiPlainProject = "itest-qadel-plain" // '{}' — the inert side, which is every project but one
	qdiFoldProject  = "itest-qadel-fold"  // the SAME name, stored mis-typed: case and spacing

	qdiAcct  = "itest-qadel-polled@example.com"
	qdiOwnID = "acc-itest-qadel-own"

	// The suite's own status names. NOT the Treetop strings: this fixture must
	// prove the COLUMN drives the decision, and a fixture that reused the real
	// seeded name would still pass if someone hardcoded it — the magic-literal
	// defect proving itself. These are names no code could know.
	qdiQAName    = "ITS-QA-NAME"    // "delivered" for the armed project
	qdiQA2Name   = "ITS-QA2-NAME"   // a SECOND delivered status, for E6's move
	qdiBuildName = "ITS-BUILD-NAME" // same statusCategory, ball still in his court
	qdiDoneName  = "ITS-DONE-NAME"  // category `done`, for the convergent reason change

	// The mis-typed configured entry: lowercase, leading and trailing spaces.
	// Stored in a TEXT[] and compared against the byte-exact name above — which
	// is the half a Go table test cannot show, because it never crosses a column.
	qdiFoldEntry = "  its-qa-name "
)

type qdiSuite struct {
	pool *pgxpool.Pool
	ex   *executor.Executor

	projects map[string]int64
	acct     int64
	tasks    map[string]int64
	refs     map[string]int64
}

func newQDISuite(t *testing.T, ctx context.Context) *qdiSuite {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db (this suite CLOSES TASKS " +
			"and deletes fixtures); use the compose db on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	qdiRequireDeliveredColumn(t, ctx, pool)
	qdiCleanup(t, ctx, pool)
	t.Cleanup(func() { qdiCleanup(t, ctx, pool) })

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))

	s := &qdiSuite{
		pool: pool, ex: executor.New(reg, checker, audit.NewPGStore(pool)),
		projects: map[string]int64{}, tasks: map[string]int64{}, refs: map[string]int64{},
	}
	s.seed(t, ctx)
	return s
}

// qdiRequireDeliveredColumn turns "column projects.ticket_delivered_statuses
// does not exist" — which otherwise surfaces from inside a seed INSERT and reads
// like a broken fixture — into the one sentence that is actually true before
// this ticket lands.
func qdiRequireDeliveredColumn(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var present bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		                 WHERE table_name='projects' AND column_name='ticket_delivered_statuses')`).
		Scan(&present); err != nil {
		t.Fatalf("probe for projects.ticket_delivered_statuses: %v", err)
	}
	if !present {
		t.Fatalf("projects.ticket_delivered_statuses does not exist in this database. Criterion 1: " +
			"migration 0025_ticket_delivered_statuses.sql adds it, and `make integration` applies " +
			"migrations to the compose db on :5433 before running. Merging a migration is not " +
			"applying it")
	}

	// The CHECK, as the DATABASE holds it — probed here so the whole suite fails
	// with ONE sentence rather than with an opaque 23514 from inside a seed
	// INSERT. Criterion 5's test below still does the real work (both
	// directions, including the rejection); this only names the cause.
	var widened bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_constraint
		                 WHERE conrelid = 'ticket_status_syncs'::regclass AND contype = 'c'
		                   AND pg_get_constraintdef(oid) LIKE '%ticket_delivered%')`).Scan(&widened); err != nil {
		t.Fatalf("probe for the widened drop_reason CHECK: %v", err)
	}
	if !widened {
		t.Fatalf("ticket_status_syncs' drop_reason CHECK does not allow 'ticket_delivered'. Criteria " +
			"3 and 4: migration 0025 swaps the constraint by 0009's drop/add precedent and then " +
			"VERIFIES the swap in a DO $$ block — a DROP CONSTRAINT against a name Postgres did not " +
			"generate is a silent no-op, and the first real QA drop then fails at runtime, every " +
			"tick, on the same ref")
	}
}

func qdiCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE account_email LIKE 'itest-qadel-%')`
	const projs = `(SELECT id FROM projects WHERE slug LIKE 'itest-qadel-%')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`

	for _, q := range []string{
		`DELETE FROM ticket_status_syncs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM external_refs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM deliveries WHERE task_id IN ` + tasksOf,
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + `)`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM projects WHERE slug LIKE 'itest-qadel-%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE account_email LIKE 'itest-qadel-%'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

// qdiIssueJSON renders one stored snapshot the way the poller stores it: the GET
// /rest/api/2/issue/{key} document with fields.comment already split off. The
// status NAME is a parameter here — that is the whole difference from the
// sibling suite's fixture, and the reason this file has its own.
func qdiIssueJSON(key, category, name, assignee string) string {
	return `{"id":"1","key":"` + key + `","fields":{"summary":"itest ` + key + `",` +
		`"description":"body","created":"2026-08-01T10:00:00.000+0000",` +
		`"updated":"2026-09-01T10:00:00.000+0000","reporter":{"accountId":"` + qdiOwnID + `"},` +
		`"status":{"name":"` + name + `","id":"3","statusCategory":{"id":3,"key":"` + category +
		`","colorName":"blue-gray","name":"a category name"}},` +
		`"assignee":{"accountId":"` + assignee + `"}}}`
}

func (s *qdiSuite) insID(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", q, err)
	}
	return id
}

func (s *qdiSuite) exec(t *testing.T, ctx context.Context, q string, args ...any) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func (s *qdiSuite) seed(t *testing.T, ctx context.Context) {
	t.Helper()

	// ticket_delivered_statuses is named EXPLICITLY on all three projects. 0025
	// defaults it to '{}', so a fixture that omitted it on the armed project
	// would exercise nothing and pass — 0016's ai_locality trap, in the one
	// direction that is silent here (the default is the INERT side, so the
	// suite would not fail, it would simply stop testing).
	// ai_locality='any' and ticket_assignee_gate=false are named for the same
	// reason: nothing below may lean on a default.
	project := func(slug string, delivered []string) int64 {
		id := s.insID(t, ctx,
			`INSERT INTO projects (name, slug, client, execution, delivery, ai_locality,
			                       ticket_assignee_gate, ticket_delivered_statuses)
			 VALUES ($1,$1,'itest-qadel-client','manual','dashboard','any', false, $2) RETURNING id`,
			slug, delivered)
		s.projects[slug] = id
		return id
	}
	project(qdiArmedProject, []string{qdiQAName, qdiQA2Name})
	project(qdiPlainProject, []string{})
	project(qdiFoldProject, []string{qdiFoldEntry})

	s.acct = s.insID(t, ctx,
		`INSERT INTO source_accounts (provider, account_email, domain_default, scopes, send_enabled, sync_cursor)
		 VALUES ('jira',$1,'https://itest-qadel.example.com',$2,false,$3::jsonb) RETURNING id`,
		qdiAcct, []string{"QAD"}, `{"own_account_id":"`+qdiOwnID+`"}`)

	type spec struct {
		key      string
		project  string
		status   string
		category string
		name     string
	}
	for _, sp := range []spec{
		// The ask: an armed project's ticket sitting in a delivered status.
		{"QAD-ARMED", qdiArmedProject, "ready", "indeterminate", qdiQAName},
		// The same project's OTHER tickets, which must not move: same category,
		// different name. This is the row that proves the clause discriminates
		// on the NAME — statusCategory provably cannot separate these two.
		{"QAD-OTHER", qdiArmedProject, "ready", "indeterminate", qdiBuildName},
		// The identical ticket under an UNARMED project.
		{"QAD-PLAIN", qdiPlainProject, "ready", "indeterminate", qdiQAName},
		// The fold, through Postgres: configured mis-typed, observed exact.
		{"QAD-FOLD", qdiFoldProject, "ready", "indeterminate", qdiQAName},
		// A claimed task: refused, not closed — and E6's subject.
		{"QAD-ACTIVE", qdiArmedProject, "in_progress", "indeterminate", qdiQAName},
		// Closed by THIS pass while delivered; its ticket has since left the
		// set, so the reopen path must bring it back to `delivered`.
		{"QAD-REOPEN", qdiArmedProject, "closed", "indeterminate", qdiBuildName},
	} {
		s.exec(t, ctx,
			`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, ingested_at)
			 VALUES ($1,$2,$3::jsonb,$4, now())
			 ON CONFLICT (source_account_id, external_id) DO UPDATE
			   SET raw_json = EXCLUDED.raw_json, content_hash = EXCLUDED.content_hash`,
			s.acct, jira.IssueRawID(sp.key), qdiIssueJSON(sp.key, sp.category, sp.name, qdiOwnID),
			fmt.Sprintf("itest-qadel-%s-%s", sp.key, sp.name))

		taskID := s.insID(t, ctx,
			`INSERT INTO tasks (project_id, title, status) VALUES ($1,$2,$3) RETURNING id`,
			s.projects[sp.project], "itest-qadel "+sp.key, sp.status)
		s.tasks[sp.key] = taskID
		s.refs[sp.key] = s.insID(t, ctx,
			`INSERT INTO external_refs (task_id, system, external_key) VALUES ($1,'jira',$2) RETURNING id`,
			taskID, sp.key)
	}

	// The return path's precondition: a task this pass closed as DELIVERED, from
	// `delivered`. status_name carries the name it was closed under — the column
	// 0023 already has, which E6 now also reads back into State.
	s.exec(t, ctx,
		`INSERT INTO ticket_status_syncs
		   (external_ref_id, task_id, status_category, status_name, assignee_account_id,
		    assigned_to_self, last_action, drop_reason, closed_from_status, reason, observed_at, acted_at)
		 VALUES ($1,$2,'indeterminate',$3,$4,true,'closed','ticket_delivered','delivered','itest seed', now(), now())`,
		s.refs["QAD-REOPEN"], s.tasks["QAD-REOPEN"], qdiQAName, qdiOwnID)
}

// ---- reads --------------------------------------------------------------------

func (s *qdiSuite) status(t *testing.T, ctx context.Context, key string) string {
	t.Helper()
	var st string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, s.tasks[key]).Scan(&st); err != nil {
		t.Fatalf("read status of %s: %v", key, err)
	}
	return st
}

type qdiState struct {
	found                                                    bool
	lastAction, dropReason, closedFrom, category, statusName string
}

func (s *qdiSuite) state(t *testing.T, ctx context.Context, key string) qdiState {
	t.Helper()
	var st qdiState
	var drop, closedFrom, name *string
	err := s.pool.QueryRow(ctx,
		`SELECT last_action, drop_reason, closed_from_status, status_category, status_name
		   FROM ticket_status_syncs WHERE external_ref_id=$1`, s.refs[key]).
		Scan(&st.lastAction, &drop, &closedFrom, &st.category, &name)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return qdiState{}
		}
		t.Fatalf("read ticket_status_syncs for %s: %v", key, err)
	}
	st.found = true
	if drop != nil {
		st.dropReason = *drop
	}
	if closedFrom != nil {
		st.closedFrom = *closedFrom
	}
	if name != nil {
		st.statusName = *name
	}
	return st
}

func (s *qdiSuite) count(t *testing.T, ctx context.Context, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n
}

// auditCount is scoped BY TASK, not by actor: the sibling suite in this package
// sweeps `actor LIKE 'ticketstatus:%'` and the two must not count each other's
// rows.
func (s *qdiSuite) auditCount(t *testing.T, ctx context.Context) int {
	t.Helper()
	return s.count(t, ctx,
		`SELECT count(*) FROM audit_events WHERE actor='ticketstatus:jira' AND task_id IN
		   (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug LIKE 'itest-qadel-%'))`)
}

func (s *qdiSuite) logEvents(t *testing.T, ctx context.Context, key string) int {
	t.Helper()
	return s.count(t, ctx,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='log'`, s.tasks[key])
}

// setStatus rewrites one stored snapshot — the ticket moved in Jira and the
// poller stored the new document. No network: D19's write-then-read-back
// property means every close this pass makes is reproducible from
// raw_source_items alone.
func (s *qdiSuite) setStatus(t *testing.T, ctx context.Context, key, category, name string) {
	t.Helper()
	s.exec(t, ctx,
		`UPDATE raw_source_items SET raw_json = $2::jsonb, content_hash = $3, ingested_at = now()
		  WHERE source_account_id = $1 AND external_id = $4`,
		s.acct, qdiIssueJSON(key, category, name, qdiOwnID),
		fmt.Sprintf("itest-qadel-%s-%s-moved", key, name), jira.IssueRawID(key))
}

func (s *qdiSuite) run(t *testing.T, ctx context.Context, cfg ticketstatus.Config) (ticketstatus.Stats, string) {
	t.Helper()
	var stats ticketstatus.Stats
	var err error
	out := tsCaptureOutput(t, func() {
		stats, err = ticketstatus.Run(ctx, s.pool, s.ex, cfg)
	})
	if err != nil {
		t.Fatalf("ticketstatus.Run: %v\n%s", err, out)
	}
	return stats, out
}

// ---- criterion 23: the delivered set is READ FROM THE COLUMN ----------------

// The sibling of TestTicketStatus_TheAssigneeGateComesFromTheProjectsColumn, for
// the second column, including its two-direction structure. Either mutation
// alone survives a one-sided test, and the second one is the dangerous
// direction: a set armed by accident silently empties a client's board.
func TestTicketStatus_DeliveredSetComesFromTheProjectsColumn(t *testing.T) {
	ctx := context.Background()
	s := newQDISuite(t, ctx)

	stats, _ := s.run(t, ctx, ticketstatus.Config{})

	if got := s.status(t, ctx, "QAD-ARMED"); got != "closed" {
		t.Errorf("QAD-ARMED (armed project, ticket in a configured delivered status) is %q, want "+
			"\"closed\". MUTATION THAT MUST TURN THIS RED: dropping p.ticket_delivered_statuses from "+
			"the candidate SELECT and passing a literal nil/[]string{} — the exact defect SWT-21 "+
			"shipped twice, and the reason this test cannot live in decide_delivered_test.go", got)
	}
	if st := s.state(t, ctx, "QAD-ARMED"); st.dropReason != "ticket_delivered" {
		t.Errorf("QAD-ARMED drop_reason = %q, want \"ticket_delivered\" — E7 records WHICH fact "+
			"dropped it, and this is the counter that answers 'how much of this board was work I had "+
			"already handed back'", st.dropReason)
	}
	if st := s.state(t, ctx, "QAD-ARMED"); st.closedFrom != "ready" {
		t.Errorf("QAD-ARMED closed_from_status = %q, want \"ready\" — D6: the reopen restores the "+
			"status the task HELD, and a close that does not record it flattens the task to `ready` "+
			"the day the client reopens the ticket", st.closedFrom)
	}

	if got := s.status(t, ctx, "QAD-PLAIN"); got != "ready" {
		t.Errorf("QAD-PLAIN (UNARMED project, byte-identical ticket status) is %q, want \"ready\". "+
			"MUTATION THAT MUST TURN THIS RED: passing a literal non-empty set, i.e. arming it "+
			"globally. E1: '{}' is today's behaviour exactly, and reengine / saka / foundry / town-ai "+
			"/ homelab / personal must not move because collaboratory was armed", got)
	}
	if s.state(t, ctx, "QAD-PLAIN").dropReason != "" {
		t.Errorf("QAD-PLAIN recorded a drop_reason (%q) — an unarmed project's ticket was not dropped "+
			"by anything", s.state(t, ctx, "QAD-PLAIN").dropReason)
	}

	if got := s.status(t, ctx, "QAD-OTHER"); got != "ready" {
		t.Errorf("QAD-OTHER (armed project, SAME statusCategory, name NOT in the set) is %q, want "+
			"\"ready\". This is the row the design tension is about: TT-In QA and TT-Work In Progress "+
			"are both `indeterminate`, so a clause that keyed on the category would empty the armed "+
			"project's whole board", got)
	}

	// The return path, for free (E8/fact 4): the ticket LEFT the set, warranted
	// is true again, and the existing reopen branch restores what it held.
	if got := s.status(t, ctx, "QAD-REOPEN"); got != "delivered" {
		t.Errorf("QAD-REOPEN (closed as ticket_delivered, ticket has since left the set) is %q, want "+
			"\"delivered\". Salvador's 'unless reopened' is not a second mechanism: a ticket leaving "+
			"the delivered set makes `warranted` true and Decide's existing branch reopens it to "+
			"closed_from_status", got)
	}

	if stats.ClosedTicketDelivered < 1 {
		t.Errorf("Stats.ClosedTicketDelivered = %d, want at least 1 (this suite closes QAD-ARMED and "+
			"QAD-FOLD). Criterion 26: count() routes a close by its drop_reason with a SWITCH — the "+
			"old `else` counted everything that was not not_assigned as ticket_done, which would make "+
			"the smoke's central check unfalsifiable", stats.ClosedTicketDelivered)
	}
}

// ---- criterion 24: the fold is a property of the SYSTEM ---------------------

// The configured entry is stored with a trailing space and the wrong case; the
// observed name is byte-exact. E2's normalization is only worth anything if it
// survives a TEXT[] column, a pgx scan and a Go comparison — a Go table test
// shows the function folds, never that the value reaching it was folded.
//
// MUTATION THAT MUST TURN THIS RED: comparing the raw strings (or spelling the
// comparison in SQL as `lower(btrim(...)) = ANY(...)`, which criterion 12's
// structural scan refuses for the unicode-space reason and which this row would
// not catch — the two guards cover different halves on purpose).
func TestTicketStatus_DeliveredFoldSurvivesThePostgresRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newQDISuite(t, ctx)

	s.run(t, ctx, ticketstatus.Config{})

	if got := s.status(t, ctx, "QAD-FOLD"); got != "closed" {
		t.Errorf("QAD-FOLD is %q, want \"closed\": the project's configured entry is %q and the "+
			"ticket's status name is %q. E2: an entry pasted into a psql UPDATE with a trailing space "+
			"or a stray capital must still arm the project — otherwise an armed set that matches "+
			"nothing looks exactly like an unarmed one, which is the failure E12's report column "+
			"exists to make visible", got, qdiFoldEntry, qdiQAName)
	}
	if st := s.state(t, ctx, "QAD-FOLD"); st.dropReason != "ticket_delivered" {
		t.Errorf("QAD-FOLD drop_reason = %q, want \"ticket_delivered\"", st.dropReason)
	}
}

// ---- criterion 5: the widened CHECK, as the DATABASE sees it ----------------

// A structural scan of the SQL text is not enough: the constraint that matters
// is the one in the database, and a DROP CONSTRAINT against a name Postgres did
// not generate is a SILENT no-op (criterion 4's self-check exists for exactly
// this). Both directions, because "the insert succeeded" proves nothing about a
// column that lost its CHECK entirely.
//
// MUTATION THAT MUST TURN THIS RED: removing the ADD CONSTRAINT from 0025 (the
// first half goes green-then-red only if the DROP also ran; the second half is
// what catches an unconstrained column).
func TestTicketStatus_DropReasonTicketDeliveredIsAValueTheDatabaseAccepts(t *testing.T) {
	ctx := context.Background()
	s := newQDISuite(t, ctx)

	insert := func(reason string) error {
		_, err := s.pool.Exec(ctx,
			`INSERT INTO ticket_status_syncs
			   (external_ref_id, task_id, status_category, last_action, drop_reason, reason)
			 VALUES ($1,$2,'indeterminate','closed',$3,'itest check probe')
			 ON CONFLICT (external_ref_id) DO UPDATE SET drop_reason = EXCLUDED.drop_reason`,
			s.refs["QAD-PLAIN"], s.tasks["QAD-PLAIN"], reason)
		return err
	}

	for _, reason := range []string{"ticket_done", "not_assigned", "ticket_delivered"} {
		if err := insert(reason); err != nil {
			t.Errorf("INSERT ticket_status_syncs with drop_reason=%q failed: %v. Criterion 3: the CHECK "+
				"is widened to all three values in migration 0025, by 0009's drop/add precedent — "+
				"without it the first real QA drop fails at runtime, every tick, on the same ref, "+
				"months after the code merged", reason, err)
		}
	}

	if err := insert("bogus"); err == nil {
		t.Errorf("INSERT ticket_status_syncs with drop_reason='bogus' SUCCEEDED. The swap must leave " +
			"the column constrained: a DROP CONSTRAINT whose name Postgres never generated is a " +
			"silent no-op, and an ADD that never ran leaves the vocabulary open — which is strictly " +
			"worse than the two-value CHECK it replaced, and invisible until a typo becomes a stored " +
			"value")
	} else if !strings.Contains(strings.ToLower(err.Error()), "check") {
		t.Errorf("INSERT with drop_reason='bogus' failed with %v, which does not look like a CHECK "+
			"violation — the test may be passing for an unrelated reason (a NOT NULL, an FK), which "+
			"would leave the constraint itself unproven", err)
	}
}

// ---- criterion 20 / E6: the drop-triggering NAME joins the dedup key --------

// The column-fed half of E6, and the reason it is here and not only in
// decide_delivered_test.go: `State.StatusName` has to come from
// `ticket_status_syncs.status_name` through `loadCandidates`. A unit test cannot
// show the driver selects it — it is the thing supplying the value.
//
// MUTATION THAT MUST TURN THIS RED: dropping s.status_name from the candidate
// SELECT (or scanning it and not copying it into State). The task then gets ONE
// log line naming a status it has left, and nothing anywhere says otherwise.
func TestTicketStatus_AClaimedTaskIsLoggedAgainWhenItMovesBetweenDeliveredStatuses(t *testing.T) {
	ctx := context.Background()
	s := newQDISuite(t, ctx)

	s.run(t, ctx, ticketstatus.Config{})

	if got := s.status(t, ctx, "QAD-ACTIVE"); got != "in_progress" {
		t.Fatalf("QAD-ACTIVE is %q, want \"in_progress\": task_close refuses claimed / in_progress / "+
			"needs_feedback, and the pass HANDLES that refusal — closing here would take the work "+
			"away from a running worker mid-turn", got)
	}
	if n := s.logEvents(t, ctx, "QAD-ACTIVE"); n != 1 {
		t.Fatalf("QAD-ACTIVE has %d log events after the first pass, want exactly 1 — the control for "+
			"everything below", n)
	}
	if st := s.state(t, ctx, "QAD-ACTIVE"); st.lastAction != "refused_active" || st.statusName != qdiQAName {
		t.Fatalf("QAD-ACTIVE state = %+v, want last_action=refused_active status_name=%q. The state row "+
			"is what makes 'the same unchanged observation' decidable next pass", st, qdiQAName)
	}

	// A second pass over the unchanged world: still one line (criterion 24 —
	// widening the key must not turn it into 'log every pass').
	s.run(t, ctx, ticketstatus.Config{})
	if n := s.logEvents(t, ctx, "QAD-ACTIVE"); n != 1 {
		t.Fatalf("QAD-ACTIVE has %d log events after an unchanged second pass, want 1: this runs every "+
			"15 minutes, and a task nobody has touched would otherwise collect 96 identical lines a "+
			"day", n)
	}

	// Now the ticket moves BETWEEN two configured delivered statuses: same
	// statusCategory, same assignee, different name. Under SWT-32's two-field
	// key nothing changed and no second line is written — while the line the
	// task already has names a status the ticket has left.
	s.setStatus(t, ctx, "QAD-ACTIVE", "indeterminate", qdiQA2Name)
	s.run(t, ctx, ticketstatus.Config{})

	if n := s.logEvents(t, ctx, "QAD-ACTIVE"); n != 2 {
		t.Errorf("QAD-ACTIVE has %d log events after its ticket moved %s -> %s (same category, same "+
			"assignee), want 2. E6: the drop-triggering fact is now the NAME, so the name must join "+
			"the 'unchanged observation' key — otherwise the holder's only explanation of why the "+
			"board thinks his work is finished names a status the ticket is no longer in",
			n, qdiQAName, qdiQA2Name)
	}
	if st := s.state(t, ctx, "QAD-ACTIVE"); st.statusName != qdiQA2Name {
		t.Errorf("QAD-ACTIVE state.status_name = %q after the move, want %q — the recorded observation "+
			"must track the ticket, or the next pass compares against a stale name and the dedup key "+
			"fires on the wrong transition", st.statusName, qdiQA2Name)
	}
}

// ---- criterion 19: a convergent re-observation that changes only the reason --

// A task already closed with drop_reason='ticket_delivered' whose ticket then
// moves to a `done` status: the state row's reason becomes ticket_done, its
// closed_from_status is PRESERVED, and zero executor calls are made.
//
// The preservation is the load-bearing half: closed_from_status is the only
// record of what the task held, and a convergent re-record that dropped it would
// flatten the task to `ready` on the day it eventually comes back — invisible
// until then.
func TestTicketStatus_AConvergentReasonChangeRewritesNothingButTheReason(t *testing.T) {
	ctx := context.Background()
	s := newQDISuite(t, ctx)

	s.run(t, ctx, ticketstatus.Config{})
	before := s.state(t, ctx, "QAD-ARMED")
	if before.dropReason != "ticket_delivered" || before.closedFrom != "ready" {
		t.Fatalf("QAD-ARMED state after the drop = %+v, want drop_reason=ticket_delivered "+
			"closed_from_status=ready — the control for the convergence below", before)
	}
	audits := s.auditCount(t, ctx)

	// The client verifies the ticket: it is now `done`, and the strongest true
	// statement about it changes.
	s.setStatus(t, ctx, "QAD-ARMED", "done", qdiDoneName)
	stats, _ := s.run(t, ctx, ticketstatus.Config{})

	after := s.state(t, ctx, "QAD-ARMED")
	if after.lastAction != "closed" {
		t.Errorf("QAD-ARMED state.last_action = %q after convergence, want \"closed\". D3: writing "+
			"'none' here erases the only fact that authorises a later reopen, invisibly, until the "+
			"day the ticket reopens and the task never comes back", after.lastAction)
	}
	if after.dropReason != "ticket_done" {
		t.Errorf("QAD-ARMED state.drop_reason = %q, want \"ticket_done\" — E7's precedence is "+
			"recomputed from the observation every pass, never read back from the row, so the record "+
			"converges on the strongest true statement", after.dropReason)
	}
	if after.closedFrom != "ready" {
		t.Errorf("QAD-ARMED state.closed_from_status = %q, want \"ready\" preserved. The task is "+
			"already closed, so THIS pass has no status to record — dropping the stored one would "+
			"silently flatten the task to `ready` the day it is reopened", after.closedFrom)
	}
	if got := s.auditCount(t, ctx); got != audits {
		t.Errorf("the convergent pass wrote %d audit rows for this suite's tasks, want 0. Criterion "+
			"19: a re-observation that changes only the RECORDED REASON makes zero executor calls — "+
			"task_close is idempotent, but calling it writes an audit row every 15 minutes forever",
			got-audits)
	}
	if stats.Converged < 1 {
		t.Errorf("Stats.Converged = %d, want at least 1: a converged ref is a decision, and printing "+
			"it is how an operator tells a healthy pass from a pass that never ran", stats.Converged)
	}
}

// ---- criterion 28: --dry-run over an ARMED project --------------------------

// "Unchanged in shape and needs no new field: its line already carries action,
// warranted, drop_reason and status=%s/%s." The dry run IS the review (D9 — this
// is what replaces a shadow mode), so an operator arming a client's board must
// be able to read exactly which tasks would go, by ticket key and by reason,
// before anything moves.
func TestTicketStatus_DryRunOverAnArmedProjectPlansTheDropAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	s := newQDISuite(t, ctx)

	stateBefore := s.count(t, ctx, `SELECT count(*) FROM ticket_status_syncs WHERE task_id IN
		(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug LIKE 'itest-qadel-%'))`)

	stats, out := s.run(t, ctx, ticketstatus.Config{DryRun: true, Force: true})

	if got := s.status(t, ctx, "QAD-ARMED"); got != "ready" {
		t.Errorf("a DRY RUN closed QAD-ARMED (%q). The review is the dry run, so it must be safe to "+
			"run against production at any time", got)
	}
	if got := s.auditCount(t, ctx); got != 0 {
		t.Errorf("a dry run wrote %d audit rows for this suite's tasks; it performs NO writes of any "+
			"kind", got)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM ticket_status_syncs WHERE task_id IN
		(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug LIKE 'itest-qadel-%'))`); n != stateBefore {
		t.Errorf("a dry run changed this suite's ticket_status_syncs row count by %d, want 0 — not "+
			"even a state row", n-stateBefore)
	}
	if stats.ClosedTicketDelivered < 1 {
		t.Errorf("the dry run reported ClosedTicketDelivered=%d, want at least 1: the plan and the "+
			"live counters have to agree, or the smoke's 'read the plan line by line FIRST' step "+
			"cannot be checked against what the pass then did", stats.ClosedTicketDelivered)
	}
	for _, want := range []string{"QAD-ARMED", "drop_reason=ticket_delivered", qdiQAName} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry-run plan never mentions %q.\n--- output ---\n%s\n--- end ---\n"+
				"Criterion 28: the line already carries action, warranted, drop_reason and "+
				"status=category/name — and the NAME is the fact that dropped the task now, so a plan "+
				"without it cannot be reviewed", want, out)
		}
	}
	// A plan LINE for QAD-PLAIN is expected — every candidate prints one. What
	// must never appear on it is a delivered drop.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "QAD-PLAIN") && strings.Contains(line, "ticket_delivered") {
			t.Errorf("the dry-run plan drops the UNARMED project's task:\n\t%s\nE1: '{}' is today's "+
				"behaviour exactly, and a dry run that plans a drop there means the live pass would "+
				"make it", strings.TrimSpace(line))
		}
	}
}

// ---- criterion 29: idempotence, with a set armed ----------------------------

func TestTicketStatus_ASecondPassOverAnUnchangedArmedWorldDoesNothing(t *testing.T) {
	ctx := context.Background()
	s := newQDISuite(t, ctx)

	s.run(t, ctx, ticketstatus.Config{})
	s.run(t, ctx, ticketstatus.Config{}) // let the world converge

	audits := s.auditCount(t, ctx)
	stats, _ := s.run(t, ctx, ticketstatus.Config{})

	if got := s.auditCount(t, ctx); got != audits {
		t.Errorf("a third pass over an unchanged armed world wrote %d more audit rows for this suite's "+
			"tasks, want 0. Criterion 29 — and the reason it matters is that this pass is a "+
			"hitchhiker on a */15 CronJob: a pass that re-acts every tick fills task_events with noise "+
			"nobody can read past", got-audits)
	}
	if stats.ClosedTicketDelivered != 0 || stats.Reopened != 0 || stats.RefusedActive != 0 {
		t.Errorf("a third pass reported closed_ticket_delivered=%d reopened=%d refused_active=%d, want "+
			"all zero — `converged` is the counter for a pass that found nothing to do",
			stats.ClosedTicketDelivered, stats.Reopened, stats.RefusedActive)
	}
}
