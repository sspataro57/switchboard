//go:build integration

package ticketstatus_test

// SWT-32 against a real database: the candidate query, the column-fed gate, the
// refusals, the executor path, the candidate-driven lookup, the
// write-then-read-back proof, the dry run and idempotence
// (docs/tickets/jira-status-sync_SPEC.md criteria 10, 11, 12, 16, 17, 18, 19,
// 22-33, 40, 41, 42, 43, 44).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run TicketStatus ./internal/ticketstatus/
//
// Build-tagged `integration` AND env-gated on DATABASE_URL, with a FATAL guard
// on 192.168.50.49 — this suite closes tasks and deletes rows.
//
// WHY EVERY ONE OF THESE IS HERE AND NOT IN decide_test.go. Decide is pure and
// tested there, exhaustively. Everything in this file turns on a value POSTGRES
// produces: projects.ticket_assignee_gate, source_accounts.sync_cursor->>
// 'own_account_id', the raw_source_items row the lookup just wrote and its
// ingested_at, tasks.status, the task_dismissals row, and the count of
// audit_events. A fake store would supply the very values the query is supposed
// to compute — SWT-21's 6th landmine, whose standing rule is: for any predicate
// whose input comes from a COLUMN, the regression test belongs in the
// integration suite and must fail when the column is dropped from the SELECT.
// The mutation that must turn each assertion red is named inline; criterion 33
// is that test for ticket_assignee_gate.
//
// NO LLM, NO LIVE JIRA. The status half makes no network call at all (its
// snapshots are already in raw_source_items — that is the whole point of the
// polled/lookup split). The lookup half talks to an httptest server defined at
// the bottom of this file, injected exactly the way cmd/connectors/jira/main.go
// injects its token-decrypting factory.
//
// GREENFIELD NOTE — EXPECTED RED. internal/ticketstatus does not exist and
// migration 0023 has not been applied to the compose db, so this file
// compile-FAILs the integration build and then fails at seed time on the missing
// ticket_status_syncs relation (tsRequireStateTable turns that into one sentence
// instead of an error from inside cleanup).
//
// ---- IMPOSED surface (store.go), beyond decide_test.go's --------------------
//
//	// Run is the driver: advisory lock, candidate query, per-ref decision,
//	// executor calls, state UPSERT. Shaped on capture.EvaluateRules and
//	// promote.Run, which the SPEC names as the siblings to copy.
//	func Run(ctx context.Context, pool *pgxpool.Pool, ex *executor.Executor, cfg Config) (Stats, error)
//
//	type Config struct {
//	    DryRun bool
//	    Force  bool          // bypass the TTL (D20)
//	    Limit  int           // bound one run
//	    TTL    time.Duration // zero => LookupTTL()
//	    // Lookup is the injected client factory (the SPEC's "injected client
//	    // factory with a decrypted token"). NIL means no credential: the status
//	    // half still reconciles and every lookup ref counts unpolled, LOUDLY
//	    // (D21). internal/ticketstatus therefore never handles a token.
//	    Lookup jira.ClientFactory
//	}
//
//	// Stats is criterion 43's counter vocabulary, one field per printed name.
//	type Stats struct {
//	    Considered, ClosedTicketDone, ClosedNotAssigned, Reopened int
//	    RefusedActive, SuppressedDismissed, Converged             int
//	    Unpolled, Ambiguous, Unreadable                           int
//	    Fetched, FetchSkippedTTL, FetchFailed                     int
//	}
//
// COUNTER ASSERTIONS ARE DELIBERATELY ONE-SIDED except for Fetched. The
// candidate set is every `system='jira'` external_ref in the database, which is
// GLOBAL like capture's and promote's inboxes; another suite's leftover ref
// would inflate Considered or Unpolled. So the weight is carried by per-row
// assertions against THIS suite's tasks, and the counters are asserted with >=
// (Fetched is exact because no other suite has a jira_lookup account).
//
// CROSS-POLLUTION PACT (IK, "integration suites cross-pollute"; `make
// integration` runs -p 1 for this reason). This suite owns, and clears at start
// AND at end:
//   - projects         itest-tstatus-%
//   - source_accounts  account_email LIKE 'itest-tstatus-%' (provider stays the
//     REAL 'jira' / 'jira_lookup' — the queries filter on it, so a private
//     provider value would test a fixture instead of the code)
//   - raw_source_items / sync_runs under those accounts
//   - tasks / external_refs / task_events / task_dismissals /
//     ticket_status_syncs of those projects
//   - audit_events + policy_decisions for actor ticketstatus:% (as well as by
//     task): the audit row for a call is written BEFORE the handler runs, and
//     criterion 44 counts them.
//
// ANTI-DATE-ROT: every timestamp is now() or now() - interval '...'. There is no
// literal date anywhere in this file.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

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
	// The actor every executor call this pass makes must carry (criterion 40),
	// in the capture:{connector} / promote:{lane} shape.
	tsActor = "ticketstatus:jira"

	tsPlainProject = "itest-tstatus-plain" // gate OFF — the collaboratory shape
	tsGatedProject = "itest-tstatus-gated" // gate ON  — the reengine shape

	tsPolledAcct = "itest-tstatus-polled@example.com" // provider jira, own id known
	tsSecondAcct = "itest-tstatus-second@example.com" // provider jira, own id known (the ambiguity)
	tsNoOwnAcct  = "itest-tstatus-noown@example.com"  // provider jira, own id EMPTY (D12's fail-safe)
	tsLookupAcct = "itest-tstatus-lookup@example.com" // provider jira_lookup

	tsOwnID    = "acc-itest-tstatus-own"
	tsOtherID  = "acc-itest-tstatus-other"
	tsLookupID = "acc-itest-tstatus-lookup-own" // what the fake /myself returns
)

// ---- fixtures ----------------------------------------------------------------

// tsIssue is one stored (or fetchable) snapshot. assigneeKey=false means the
// `assignee` key is ABSENT from fields — evidence missing, which D14 keeps
// distinct from `"assignee": null`.
type tsIssue struct {
	key         string
	category    string // "" => no fields.status at all
	assignee    string
	assigneeKey bool
}

// tsIssueJSON renders the issue the way the poller stores it: the GET
// /rest/api/2/issue/{key} document with fields.comment already split off.
func tsIssueJSON(iss tsIssue) string {
	doc := `{"id":"1","key":"` + iss.key + `","fields":{"summary":"itest ` + iss.key + `",` +
		`"description":"body","created":"2026-08-01T10:00:00.000+0000",` +
		`"updated":"2026-09-01T10:00:00.000+0000","reporter":{"accountId":"` + tsOtherID + `"}`
	if iss.category != "" {
		doc += `,"status":{"name":"a workflow name","id":"3","statusCategory":{"id":3,"key":"` +
			iss.category + `","colorName":"blue-gray","name":"a category name"}}`
	}
	if iss.assigneeKey {
		if iss.assignee == "" {
			doc += `,"assignee":null`
		} else {
			doc += `,"assignee":{"accountId":"` + iss.assignee + `"}`
		}
	}
	return doc + `}}`
}

type tsSuite struct {
	pool *pgxpool.Pool
	ex   *executor.Executor
	fake *tsFakeJira

	plain, gated int64
	accounts     map[string]int64 // account_email -> id
	tasks        map[string]int64 // ticket key -> task id
	refs         map[string]int64 // ticket key -> external_refs id
}

func newTSSuite(t *testing.T, ctx context.Context) *tsSuite {
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
	tsRequireStateTable(t, ctx, pool)
	tsCleanup(t, ctx, pool)
	t.Cleanup(func() { tsCleanup(t, ctx, pool) })

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))

	s := &tsSuite{
		pool: pool, ex: executor.New(reg, checker, audit.NewPGStore(pool)),
		fake:     newTSFakeJira(),
		accounts: map[string]int64{}, tasks: map[string]int64{}, refs: map[string]int64{},
	}
	t.Cleanup(s.fake.close)
	s.seed(t, ctx)
	return s
}

// tsRequireStateTable turns "relation ticket_status_syncs does not exist" —
// which otherwise surfaces from inside CLEANUP and reads like a broken pact —
// into the one sentence that is actually true before this ticket lands.
func tsRequireStateTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var present bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('ticket_status_syncs') IS NOT NULL`).Scan(&present); err != nil {
		t.Fatalf("probe for ticket_status_syncs: %v", err)
	}
	if !present {
		t.Fatalf("ticket_status_syncs does not exist in this database. Criterion 1: migration " +
			"0023_ticket_status_sync.sql creates it, and `make integration` applies migrations to the " +
			"compose db on :5433 before running. Merging a migration is not applying it")
	}
	var gate bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		                 WHERE table_name='projects' AND column_name='ticket_assignee_gate')`).Scan(&gate); err != nil {
		t.Fatalf("probe for projects.ticket_assignee_gate: %v", err)
	}
	if !gate {
		t.Fatalf("projects.ticket_assignee_gate does not exist. Criterion 2: the gate is a typed " +
			"COLUMN, default false, armed by a hand-run UPDATE per project (D11)")
	}
}

func tsCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE account_email LIKE 'itest-tstatus-%')`
	const projs = `(SELECT id FROM projects WHERE slug LIKE 'itest-tstatus-%')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`

	for _, q := range []string{
		`DELETE FROM ticket_status_syncs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM external_refs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM deliveries WHERE task_id IN ` + tasksOf,
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor LIKE 'ticketstatus:%')`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor LIKE 'ticketstatus:%'`,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM projects WHERE slug LIKE 'itest-tstatus-%'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN
		   (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'jira:itest-tstatus%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE account_email LIKE 'itest-tstatus-%'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func (s *tsSuite) insID(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", q, err)
	}
	return id
}

func (s *tsSuite) exec(t *testing.T, ctx context.Context, q string, args ...any) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func (s *tsSuite) seed(t *testing.T, ctx context.Context) {
	t.Helper()

	// ticket_assignee_gate is named EXPLICITLY on both projects. 0023 defaults
	// it to false, so a fixture that omitted it would get the plain shape twice
	// and the gate half would exercise nothing.
	// ai_locality='any' is named explicitly (the internal/provider structure
	// guard insists, and it is right to: 0016 defaults the column to
	// 'local_only', which would make a client-project fixture behave like a
	// personal one). It is irrelevant to this pass — nothing here reaches a
	// model — but a fixture that leans on a default is a fixture nobody can read.
	s.plain = s.insID(t, ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, ai_locality, ticket_assignee_gate)
		 VALUES ($1,$1,'itest-tstatus-client','manual','dashboard','any', false) RETURNING id`, tsPlainProject)
	s.gated = s.insID(t, ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, ai_locality, ticket_assignee_gate)
		 VALUES ($1,$1,'itest-tstatus-client','manual','dashboard','any', true) RETURNING id`, tsGatedProject)

	account := func(email, provider, site, cursor string, scopes []string) int64 {
		id := s.insID(t, ctx,
			`INSERT INTO source_accounts (provider, account_email, domain_default, scopes, send_enabled, sync_cursor)
			 VALUES ($1,$2,$3,$4,false,$5::jsonb) RETURNING id`,
			provider, email, site, scopes, cursor)
		s.accounts[email] = id
		return id
	}
	// The polled sites: their snapshots are ALREADY in raw_source_items, which
	// is why the status half needs no network at all.
	account(tsPolledAcct, "jira", "https://itest-tstatus.example.com",
		`{"own_account_id":"`+tsOwnID+`","jira_updated_at":"2026-09-01T10:00:00.000+0000"}`, []string{"ITS"})
	account(tsSecondAcct, "jira", "https://itest-tstatus-2.example.com",
		`{"own_account_id":"`+tsOwnID+`"}`, []string{"ITS"})
	// D12's fail-safe: an account that never cached its own accountId.
	account(tsNoOwnAcct, "jira", "https://itest-tstatus-3.example.com", `{}`, []string{"ITS"})
	// The lookup account (D17). Its cursor carries an unrelated key that the
	// merge-write must not clobber (criterion 16, the SWT-24 whole-blob
	// landmine), and NO own_account_id — the pass's /myself call fills it.
	account(tsLookupAcct, "jira_lookup", s.fake.url(), `{"itest_keepme":"survives"}`, []string{"ILK"})

	// Stored snapshots for the polled half.
	for _, seed := range []struct {
		acct string
		iss  tsIssue
	}{
		{tsPolledAcct, tsIssue{"ITS-DONE", "done", tsOwnID, true}},
		{tsPolledAcct, tsIssue{"ITS-OPEN", "indeterminate", tsOwnID, true}},
		{tsPolledAcct, tsIssue{"ITS-NOTMINE", "indeterminate", tsOtherID, true}},
		{tsPolledAcct, tsIssue{"ITS-ACTIVE", "done", tsOwnID, true}},
		{tsPolledAcct, tsIssue{"ITS-DISMISSED", "indeterminate", tsOwnID, true}},
		{tsPolledAcct, tsIssue{"ITS-HUMANCLOSED", "indeterminate", tsOwnID, true}},
		{tsPolledAcct, tsIssue{"ITS-REOPEN", "indeterminate", tsOwnID, true}},
		{tsPolledAcct, tsIssue{"ITS-GMINE", "indeterminate", tsOwnID, true}},
		{tsPolledAcct, tsIssue{"ITS-GNOTMINE", "indeterminate", tsOtherID, true}},
		{tsPolledAcct, tsIssue{"ITS-GUNASSIGNED", "indeterminate", "", true}}, // D14: null assignee
		{tsPolledAcct, tsIssue{"ITS-GNOSTATUS", "", tsOwnID, true}},
		{tsPolledAcct, tsIssue{"ITS-GNOASSIGNEEKEY", "indeterminate", "", false}},
		// The same key stored under TWO accounts: criterion 31's ambiguity.
		{tsPolledAcct, tsIssue{"ITS-AMBIG", "done", tsOwnID, true}},
		{tsSecondAcct, tsIssue{"ITS-AMBIG", "indeterminate", tsOwnID, true}},
		// Stored by an account with no own_account_id: D12's fail-safe.
		{tsNoOwnAcct, tsIssue{"ITS-GNOOWN", "indeterminate", tsOtherID, true}},
		// A lookup snapshot that is already FRESH: the TTL must skip it and the
		// verdict must come from these bytes (criteria 11 and 17).
		{tsLookupAcct, tsIssue{"ILK-FRESH", "done", tsLookupID, true}},
	} {
		s.storeRaw(t, ctx, seed.acct, seed.iss, 0)
	}

	// Tasks + their external_refs. The ref is what makes a ticket a CANDIDATE
	// (D16): the key set can never exceed the number of jira-keyed tasks.
	type taskSpec struct {
		key     string
		project int64
		status  string
		url     string
	}
	for _, spec := range []taskSpec{
		{"ITS-DONE", s.plain, "ready", ""},
		{"ITS-OPEN", s.plain, "ready", ""},
		{"ITS-NOTMINE", s.plain, "ready", ""},
		{"ITS-ACTIVE", s.plain, "in_progress", ""},
		{"ITS-DISMISSED", s.plain, "closed", ""},
		{"ITS-HUMANCLOSED", s.plain, "closed", ""},
		{"ITS-REOPEN", s.plain, "closed", ""},
		{"ITS-AMBIG", s.plain, "ready", ""},
		{"WEBX-9", s.plain, "ready", ""}, // no snapshot, no claiming lookup account
		{"ITS-GMINE", s.gated, "ready", ""},
		{"ITS-GNOTMINE", s.gated, "ready", ""},
		{"ITS-GUNASSIGNED", s.gated, "ready", ""},
		{"ITS-GNOSTATUS", s.gated, "ready", ""},
		{"ITS-GNOASSIGNEEKEY", s.gated, "ready", ""},
		{"ITS-GNOOWN", s.gated, "ready", ""},
		{"ILK-MINE", s.gated, "ready", ""},
		{"ILK-NOTMINE", s.gated, "ready", ""},
		{"ILK-DONE", s.gated, "ready", ""},
		// D18's SSRF argument, made concrete: external_url names a host nobody
		// authorised. Routing is by PREFIX against scopes and must ignore it.
		{"ILK-FRESH", s.gated, "ready", "https://evil.example.org/browse/ILK-FRESH"},
	} {
		taskID := s.insID(t, ctx,
			`INSERT INTO tasks (project_id, title, status) VALUES ($1,$2,$3) RETURNING id`,
			spec.project, "itest-tstatus "+spec.key, spec.status)
		s.tasks[spec.key] = taskID
		s.refs[spec.key] = s.insID(t, ctx,
			`INSERT INTO external_refs (task_id, system, external_key, external_url)
			 VALUES ($1,'jira',$2,NULLIF($3,'')) RETURNING id`, taskID, spec.key, spec.url)
	}

	// A human dismissal on an already-closed task, plus a state row saying THIS
	// pass closed it: D4's non-redundancy case, which is the only shape where
	// the dismissal clause does any work.
	s.exec(t, ctx,
		`INSERT INTO task_dismissals (task_id, reason_code, note, dismissed_by)
		 VALUES ($1,'not_actionable','itest','dashboard:itest-tstatus')`, s.tasks["ITS-DISMISSED"])
	s.seedState(t, ctx, "ITS-DISMISSED", "closed", "ready", "indeterminate", tsOwnID)
	// The return path's precondition: a task this pass closed, from `delivered`.
	s.seedState(t, ctx, "ITS-REOPEN", "closed", "delivered", "done", tsOwnID)
	// ITS-HUMANCLOSED gets NO state row on purpose: closed by somebody else.
}

// storeRaw writes one snapshot under an account, ageing it by `age` so the TTL
// has something to be true about.
func (s *tsSuite) storeRaw(t *testing.T, ctx context.Context, email string, iss tsIssue, age time.Duration) {
	t.Helper()
	raw := tsIssueJSON(iss)
	s.exec(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, ingested_at)
		 VALUES ($1,$2,$3::jsonb,$4, now() - make_interval(secs => $5))
		 ON CONFLICT (source_account_id, external_id) DO UPDATE
		   SET raw_json = EXCLUDED.raw_json, content_hash = EXCLUDED.content_hash,
		       ingested_at = EXCLUDED.ingested_at`,
		s.accounts[email], jira.IssueRawID(iss.key), raw,
		fmt.Sprintf("itest-tstatus-%s-%s-%s", iss.key, iss.category, iss.assignee), age.Seconds())
}

func (s *tsSuite) seedState(t *testing.T, ctx context.Context, key, lastAction, closedFrom, category, assignee string) {
	t.Helper()
	s.exec(t, ctx,
		`INSERT INTO ticket_status_syncs
		   (external_ref_id, task_id, status_category, status_name, assignee_account_id,
		    assigned_to_self, last_action, closed_from_status, reason, observed_at, acted_at)
		 VALUES ($1,$2,$3,'a workflow name',$4,$5,$6,NULLIF($7,''),'itest seed', now(), now())`,
		s.refs[key], s.tasks[key], category, assignee, assignee == tsOwnID, lastAction, closedFrom)
}

// ---- reads --------------------------------------------------------------------

func (s *tsSuite) status(t *testing.T, ctx context.Context, key string) string {
	t.Helper()
	var st string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, s.tasks[key]).Scan(&st); err != nil {
		t.Fatalf("read status of %s: %v", key, err)
	}
	return st
}

type tsState struct {
	found                                        bool
	lastAction, dropReason, closedFrom, category string
	assignee                                     string
	assignedToSelf                               *bool
}

func (s *tsSuite) state(t *testing.T, ctx context.Context, key string) tsState {
	t.Helper()
	var st tsState
	var drop, closedFrom, assignee *string
	err := s.pool.QueryRow(ctx,
		`SELECT last_action, drop_reason, closed_from_status, status_category, assignee_account_id, assigned_to_self
		   FROM ticket_status_syncs WHERE external_ref_id=$1`, s.refs[key]).
		Scan(&st.lastAction, &drop, &closedFrom, &st.category, &assignee, &st.assignedToSelf)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return tsState{}
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
	if assignee != nil {
		st.assignee = *assignee
	}
	return st
}

func (s *tsSuite) count(t *testing.T, ctx context.Context, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n
}

func (s *tsSuite) auditCount(t *testing.T, ctx context.Context) int {
	t.Helper()
	return s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE actor=$1`, tsActor)
}

func (s *tsSuite) logEvents(t *testing.T, ctx context.Context, key string) int {
	t.Helper()
	return s.count(t, ctx,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='log'`, s.tasks[key])
}

// run drives one live pass with the fake lookup wired in, and returns the
// counters and everything the pass printed.
func (s *tsSuite) run(t *testing.T, ctx context.Context, cfg ticketstatus.Config) (ticketstatus.Stats, string) {
	t.Helper()
	if cfg.Lookup == nil {
		cfg.Lookup = s.fake.factory()
	}
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

// ---- criteria 22, 25, 40: the status half, with no network at all -----------

// The SPEC's "usable alone": one pass makes the collaboratory tasks whose
// tickets are already Done leave the board, and leaves every other task
// untouched. The board hides `closed` by default, so `status='closed'` IS the
// "drops from switchboard" Salvador asked for.
func TestTicketStatus_ClosesTasksWhoseTicketIsDone(t *testing.T) {
	ctx := context.Background()
	s := newTSSuite(t, ctx)

	stats, _ := s.run(t, ctx, ticketstatus.Config{})

	if got := s.status(t, ctx, "ITS-DONE"); got != "closed" {
		t.Errorf("ITS-DONE's task is %q, want \"closed\". Its stored snapshot has "+
			"fields.status.statusCategory.key = 'done' — the ONE discriminator (D2). MUTATION THAT "+
			"MUST TURN THIS RED: reading fields.status.name instead of the category", got)
	}
	st := s.state(t, ctx, "ITS-DONE")
	if !st.found {
		t.Fatalf("no ticket_status_syncs row for ITS-DONE. D3: the pass records what it did, and " +
			"'closed' is the only value that authorises a later reopen")
	}
	if st.lastAction != "closed" || st.dropReason != "ticket_done" || st.closedFrom != "ready" {
		t.Errorf("ITS-DONE state = %+v, want last_action=closed drop_reason=ticket_done "+
			"closed_from_status=ready (criterion 22)", st)
	}
	if st.category != "done" {
		t.Errorf("ITS-DONE state.status_category = %q, want \"done\"", st.category)
	}

	// Invariant 3: the close went through the executor with the task attached.
	n := s.count(t, ctx,
		`SELECT count(*) FROM audit_events WHERE actor=$1 AND tool='task_close' AND task_id=$2`,
		tsActor, s.tasks["ITS-DONE"])
	if n != 1 {
		t.Errorf("audit_events(actor=%s, tool=task_close, task=ITS-DONE) = %d, want 1. Criterion 40: "+
			"every write to tasks/task_events goes through the executor with Call.TaskID set, so "+
			"audit_events.task_id is non-NULL and 'why did this leave the board' is answerable",
			tsActor, n)
	}
	if s.count(t, ctx,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='status_changed'`,
		s.tasks["ITS-DONE"]) != 1 {
		t.Errorf("ITS-DONE has no single status_changed event; the board's history is the trail")
	}

	// Every other task is untouched — the other half of the promise.
	for _, key := range []string{"ITS-OPEN", "ITS-NOTMINE"} {
		if got := s.status(t, ctx, key); got != "ready" {
			t.Errorf("%s's task is %q, want \"ready\": its ticket is open work and (with the gate OFF) "+
				"the assignee is not consulted at all", key, got)
		}
	}
	if stats.ClosedTicketDone < 1 {
		t.Errorf("Stats.ClosedTicketDone = %d, want >= 1 (this suite closed ITS-DONE)", stats.ClosedTicketDone)
	}
}

// ---- criterion 33: the gate is READ FROM THE COLUMN --------------------------

// "ticket_assignee_gate is read from the COLUMN and the regression test lives in
// the integration suite: mutating the projects SELECT to a literal false must
// turn a test red (institutional landmine 6). A unit test cannot catch this, by
// construction; it is the thing supplying the value."
//
// Both directions are asserted, and that is deliberate:
//   - forcing the gate FALSE leaves ITS-GNOTMINE on the board -> red here;
//   - forcing it TRUE closes ITS-NOTMINE in the ungated project -> red here.
//
// Either mutation alone would survive a one-sided test, and the second one is
// the dangerous direction: a gate armed by accident silently empties a client's
// board.
func TestTicketStatus_TheAssigneeGateComesFromTheProjectsColumn(t *testing.T) {
	ctx := context.Background()
	s := newTSSuite(t, ctx)

	s.run(t, ctx, ticketstatus.Config{})

	if got := s.status(t, ctx, "ITS-GNOTMINE"); got != "closed" {
		t.Errorf("ITS-GNOTMINE (gated project, ticket assigned to someone else) is %q, want "+
			"\"closed\". MUTATION THAT MUST TURN THIS RED: dropping p.ticket_assignee_gate from the "+
			"candidate SELECT and passing a literal false — the exact defect SWT-21 shipped twice", got)
	}
	if st := s.state(t, ctx, "ITS-GNOTMINE"); st.dropReason != "not_assigned" {
		t.Errorf("ITS-GNOTMINE drop_reason = %q, want \"not_assigned\" — D15 records WHICH fact "+
			"dropped it, and this is the counter Salvador will read to decide whether the reengine "+
			"capture rule is too broad", st.dropReason)
	}
	if got := s.status(t, ctx, "ITS-GUNASSIGNED"); got != "closed" {
		t.Errorf("ITS-GUNASSIGNED (gated, \"assignee\": null) is %q, want \"closed\". D14: Jira "+
			"serialises an unassigned issue with the key PRESENT and null, which is a positive "+
			"statement of unassignment — and unassigned counts as not-mine", got)
	}
	if got := s.status(t, ctx, "ITS-GMINE"); got != "ready" {
		t.Errorf("ITS-GMINE (gated, assigned to the polling account) is %q, want \"ready\" — D12's "+
			"comparison is fields.assignee.accountId == sync_cursor->>'own_account_id' for the account "+
			"that STORED the snapshot", got)
	}
	if got := s.status(t, ctx, "ITS-NOTMINE"); got != "ready" {
		t.Errorf("ITS-NOTMINE (UNGATED project, ticket assigned to someone else) is %q, want "+
			"\"ready\". The gate is PER PROJECT (D11: Salvador asked for it on reengine and said "+
			"nothing about collaboratory, where he IS the person the tickets are raised for). "+
			"MUTATION THAT MUST TURN THIS RED: arming the gate globally", got)
	}
}

// ---- criterion 24: active work is refused, once ------------------------------

func TestTicketStatus_RefusesToCloseActiveWork(t *testing.T) {
	ctx := context.Background()
	s := newTSSuite(t, ctx)

	s.run(t, ctx, ticketstatus.Config{})

	if got := s.status(t, ctx, "ITS-ACTIVE"); got != "in_progress" {
		t.Errorf("ITS-ACTIVE is %q, want \"in_progress\": fact 11 — task_close refuses claimed / "+
			"in_progress / needs_feedback, and the pass must HANDLE that refusal rather than treat it "+
			"as an error that aborts the run", got)
	}
	if n := s.logEvents(t, ctx, "ITS-ACTIVE"); n != 1 {
		t.Fatalf("ITS-ACTIVE has %d log events, want exactly 1: one task_append_log naming the ticket, "+
			"its status and its assignee, so the holder can see why the board thinks the work is "+
			"finished", n)
	}
	st := s.state(t, ctx, "ITS-ACTIVE")
	if st.lastAction != "refused_active" {
		t.Errorf("ITS-ACTIVE state.last_action = %q, want \"refused_active\"", st.lastAction)
	}

	// The second pass over the same unchanged observation: zero executor calls.
	before := s.auditCount(t, ctx)
	s.run(t, ctx, ticketstatus.Config{})
	if after := s.auditCount(t, ctx); after != before {
		t.Errorf("a second pass wrote %d more audit rows for actor %s. Criterion 24: a task nobody has "+
			"touched must not collect a log line every 15 minutes — 96 a day, forever",
			after-before, tsActor)
	}
	if n := s.logEvents(t, ctx, "ITS-ACTIVE"); n != 1 {
		t.Errorf("ITS-ACTIVE has %d log events after two passes, want 1", n)
	}
}

// ---- criterion 32: every evidence gap is a counted no-op --------------------

func TestTicketStatus_EvidenceGapsDoNothingInEitherDirection(t *testing.T) {
	ctx := context.Background()
	s := newTSSuite(t, ctx)

	stats, _ := s.run(t, ctx, ticketstatus.Config{})

	for _, tc := range []struct{ key, why string }{
		{"ITS-GNOSTATUS", "the stored snapshot has no fields.status at all: StatusKnown=false"},
		{"ITS-GNOASSIGNEEKEY", "the gate is ON and `fields` carries no assignee KEY — evidence " +
			"missing, which D14 keeps distinct from an explicit null"},
		{"ITS-GNOOWN", "the gate is ON and the account that stored the snapshot has an empty " +
			"own_account_id: D12's fail-safe, because a task must not vanish because we could not " +
			"identify ourselves"},
	} {
		if got := s.status(t, ctx, tc.key); got != "ready" {
			t.Errorf("%s is %q, want \"ready\" — %s. Criterion 32: unreadable means NO action in "+
				"either direction", tc.key, got, tc.why)
		}
		if st := s.state(t, ctx, tc.key); st.found {
			t.Errorf("%s got a ticket_status_syncs row (%+v). status_category is NOT NULL with a "+
				"three-value CHECK, so an unreadable ref cannot be recorded — it is COUNTED "+
				"(Stats.Unreadable) and left alone", tc.key, st)
		}
	}
	if stats.Unreadable < 3 {
		t.Errorf("Stats.Unreadable = %d, want >= 3 (this suite seeds three distinct evidence gaps). "+
			"Each is counted; an unreadable ref that showed up only as 'nothing happened' would be "+
			"indistinguishable from a healthy pass", stats.Unreadable)
	}
}

// ---- criteria 30 + 31: unpolled and ambiguous -------------------------------

func TestTicketStatus_UnpolledAndAmbiguousRefsAreRefused(t *testing.T) {
	ctx := context.Background()
	s := newTSSuite(t, ctx)

	stats, _ := s.run(t, ctx, ticketstatus.Config{})

	// WEBX-9: no stored snapshot, and no lookup account claiming the WEBX
	// prefix. Exactly today's behaviour, made visible.
	if got := s.status(t, ctx, "WEBX-9"); got != "ready" {
		t.Errorf("WEBX-9 is %q, want \"ready\": a ref with no snapshot and no claiming lookup account "+
			"is counted `unpolled` and NOTHING happens to it (criterion 30)", got)
	}
	if st := s.state(t, ctx, "WEBX-9"); st.found {
		t.Errorf("WEBX-9 got a state row (%+v); there is no observation to record", st)
	}
	if stats.Unpolled < 1 {
		t.Errorf("Stats.Unpolled = %d, want >= 1", stats.Unpolled)
	}

	// ITS-AMBIG: the same key stored under two accounts. Refusing is reversible;
	// a wrong close is a task that vanishes off the board with no explanation.
	if got := s.status(t, ctx, "ITS-AMBIG"); got != "ready" {
		t.Errorf("ITS-AMBIG is %q, want \"ready\". Two stored snapshots for one key (two accounts, "+
			"one saying done and one saying indeterminate) is a REFUSAL, counted — never a guess, and "+
			"never newest-wins", got)
	}
	if st := s.state(t, ctx, "ITS-AMBIG"); st.found {
		t.Errorf("ITS-AMBIG got a state row (%+v); nothing was observed and nothing was acted on", st)
	}
	if stats.Ambiguous < 1 {
		t.Errorf("Stats.Ambiguous = %d, want >= 1. The refusal must be VISIBLE: an invisible refusal "+
			"is the recorded cost of the multi-match precedent (google and jira still have no "+
			"reconciler, so their refusals are silent)", stats.Ambiguous)
	}
}

// ---- criteria 26, 27, 28: the return path and the human who outranks it -----

func TestTicketStatus_ReopensOnlyWhatItClosedAndKeepsDismissalsDown(t *testing.T) {
	ctx := context.Background()
	s := newTSSuite(t, ctx)

	s.run(t, ctx, ticketstatus.Config{})

	// (27) A task this pass closed, whose ticket is open again, comes back — to
	// the status it HELD, not flattened to ready.
	if got := s.status(t, ctx, "ITS-REOPEN"); got != "delivered" {
		t.Errorf("ITS-REOPEN is %q, want \"delivered\" (its closed_from_status). D6: restoring the "+
			"recorded status is more honest than flattening a delivered task to ready", got)
	}
	if st := s.state(t, ctx, "ITS-REOPEN"); st.lastAction != "reopened" {
		t.Errorf("ITS-REOPEN state.last_action = %q, want \"reopened\"", st.lastAction)
	}
	if s.count(t, ctx,
		`SELECT count(*) FROM audit_events WHERE actor=$1 AND tool='task_reopen' AND task_id=$2`,
		tsActor, s.tasks["ITS-REOPEN"]) != 1 {
		t.Errorf("the reopen did not go through the executor as task_reopen with the task attached " +
			"(criterion 40)")
	}

	// (26) A task closed by somebody else — a human's task_dismiss, R8's
	// delivery lifecycle, a hand-run task_close — is NEVER reopened, and the
	// first observation records that fact so the next pass cannot change its
	// mind.
	if got := s.status(t, ctx, "ITS-HUMANCLOSED"); got != "closed" {
		t.Errorf("ITS-HUMANCLOSED is %q, want \"closed\": this pass never observed it before, so it "+
			"cannot claim the close and must not undo it (criterion 23/26)", got)
	}
	if st := s.state(t, ctx, "ITS-HUMANCLOSED"); !st.found || st.lastAction != "none" {
		t.Errorf("ITS-HUMANCLOSED state = %+v, want a row with last_action=none. That row is what "+
			"stops a LATER pass reopening it: 'the pass must never claim a close it did not make'", st)
	}

	// (28) A dismissed task stays down, with exactly one log line saying why.
	if got := s.status(t, ctx, "ITS-DISMISSED"); got != "closed" {
		t.Errorf("ITS-DISMISSED is %q, want \"closed\". D4: a human dismissal outranks a reconciler "+
			"echo — resurrecting it would destroy the meaning of the label as training data", got)
	}
	if n := s.logEvents(t, ctx, "ITS-DISMISSED"); n != 1 {
		t.Fatalf("ITS-DISMISSED has %d log events, want exactly 1 (the suppression, recorded once)", n)
	}
	if st := s.state(t, ctx, "ITS-DISMISSED"); st.lastAction != "suppressed_dismissed" {
		t.Errorf("ITS-DISMISSED state.last_action = %q, want \"suppressed_dismissed\"", st.lastAction)
	}

	// ...and never repeated.
	s.run(t, ctx, ticketstatus.Config{})
	if n := s.logEvents(t, ctx, "ITS-DISMISSED"); n != 1 {
		t.Errorf("ITS-DISMISSED has %d log events after two passes, want 1. 'Recorded once and never "+
			"repeated' (D4) — every 15 minutes forever is not a record, it is a leak", n)
	}
	if got := s.status(t, ctx, "ITS-DISMISSED"); got != "closed" {
		t.Errorf("ITS-DISMISSED came back on the second pass (%q)", got)
	}
}

// ---- criteria 10, 11, 16, 17, 19: the candidate-driven lookup ---------------

// The heart of Q1's answer, and the one place invariant 1 is MECHANICAL rather
// than a formality (D19): the fetched issue is written to raw_source_items
// through the existing upsertRaw, and the decision then reads the STORED row —
// so every close is reproducible from raw bytes with the network unplugged.
func TestTicketStatus_LookupFetchesOnlyCandidatesAndDecidesFromTheStoredRow(t *testing.T) {
	ctx := context.Background()
	s := newTSSuite(t, ctx)

	s.fake.put(tsIssue{"ILK-MINE", "indeterminate", tsLookupID, true})
	s.fake.put(tsIssue{"ILK-NOTMINE", "indeterminate", tsOtherID, true})
	s.fake.put(tsIssue{"ILK-DONE", "done", tsLookupID, true})
	s.fake.put(tsIssue{"ILK-FRESH", "indeterminate", tsLookupID, true}) // must NOT be fetched

	// Two passes: the first caches the lookup site's identity from /myself
	// (criterion 16), the second is the steady state. Both are what the CronJob
	// does anyway. AMENDED 2026-09-09 (implementation session): the second pass
	// originally carried Force:true, which contradicts three of this test's own
	// assertions — a forced second pass re-fetches ILK-FRESH and overwrites the
	// stored-done snapshot the D19 divergence proof depends on, and adds four
	// GETs the fetch-order assertion forbids. The steady state the comment
	// describes is a PLAIN pass; --force has its own test below.
	stats, _ := s.run(t, ctx, ticketstatus.Config{})
	s.run(t, ctx, ticketstatus.Config{})

	// (10) exactly the candidates with no fresh snapshot, in key order, and no
	// others. The request volume is bounded by the number of jira-keyed TASKS,
	// which is the whole reason this shape beat a project-wide poll.
	if got := strings.Join(s.fake.gets(), ","); got != "ILK-DONE,ILK-MINE,ILK-NOTMINE" {
		t.Errorf("first pass fetched [%s], want [ILK-DONE,ILK-MINE,ILK-NOTMINE]: exactly the "+
			"candidates with no fresh stored snapshot, in key order. ILK-FRESH already has one "+
			"(criterion 17), and nothing outside external_refs is a candidate at all (D16)", got)
	}
	if stats.Fetched != 3 {
		t.Errorf("Stats.Fetched = %d, want 3", stats.Fetched)
	}
	if stats.FetchSkippedTTL < 1 {
		t.Errorf("Stats.FetchSkippedTTL = %d, want >= 1 (ILK-FRESH's snapshot is inside the TTL)",
			stats.FetchSkippedTTL)
	}

	// (11) raw-first: the snapshot exists under the LOOKUP account, with a
	// content hash, under the same external_id the poller would have used.
	for _, key := range []string{"ILK-MINE", "ILK-NOTMINE", "ILK-DONE"} {
		n := s.count(t, ctx,
			`SELECT count(*) FROM raw_source_items
			  WHERE source_account_id=$1 AND external_id=$2 AND content_hash <> '' AND raw_json ? 'fields'`,
			s.accounts[tsLookupAcct], jira.IssueRawID(key))
		if n != 1 {
			t.Errorf("no stored raw row for %s under the jira_lookup account. Invariant 1 is "+
				"MECHANICAL here: the write happens BEFORE any field is read, through the same "+
				"upsertRaw the poller uses", key)
		}
	}

	// the verdicts, from the stored bytes.
	if got := s.status(t, ctx, "ILK-MINE"); got != "ready" {
		t.Errorf("ILK-MINE is %q, want \"ready\": open, and assigned to the lookup site's own "+
			"accountId (which /myself supplied and the cursor now holds)", got)
	}
	if got := s.status(t, ctx, "ILK-NOTMINE"); got != "closed" {
		t.Errorf("ILK-NOTMINE is %q, want \"closed\" — the reengine complaint, in one row", got)
	}
	if st := s.state(t, ctx, "ILK-NOTMINE"); st.dropReason != "not_assigned" {
		t.Errorf("ILK-NOTMINE drop_reason = %q, want \"not_assigned\"", st.dropReason)
	}
	if got := s.status(t, ctx, "ILK-DONE"); got != "closed" {
		t.Errorf("ILK-DONE is %q, want \"closed\"", got)
	}
	if st := s.state(t, ctx, "ILK-DONE"); st.dropReason != "ticket_done" {
		t.Errorf("ILK-DONE drop_reason = %q, want \"ticket_done\" — status precedence holds on the "+
			"lookup half too (D15)", st.dropReason)
	}
	// ILK-FRESH was decided from the STORED row, which says `done` while the
	// server says `indeterminate`. That divergence is the proof: a pass that
	// decided from the HTTP response would have left this task open.
	if got := s.status(t, ctx, "ILK-FRESH"); got != "closed" {
		t.Errorf("ILK-FRESH is %q, want \"closed\". Its STORED snapshot says done while the fake "+
			"server says indeterminate — so this assertion fails exactly when the decision is made "+
			"from the response instead of from raw_source_items (D19)", got)
	}

	// (16) /myself, merged into the cursor — and the unrelated key survives.
	var own, keep *string
	if err := s.pool.QueryRow(ctx,
		`SELECT sync_cursor->>'own_account_id', sync_cursor->>'itest_keepme'
		   FROM source_accounts WHERE id=$1`, s.accounts[tsLookupAcct]).Scan(&own, &keep); err != nil {
		t.Fatalf("read the lookup account's cursor: %v", err)
	}
	if own == nil || *own != tsLookupID {
		t.Errorf("the lookup account's sync_cursor->>'own_account_id' = %v, want %q. D12: 'me' is per "+
			"source account, and until it is stored the assignee gate is unevaluable — every reengine "+
			"ref would count unreadable and nothing would ever drop", derefTS(own), tsLookupID)
	}
	if keep == nil || *keep != "survives" {
		t.Errorf("the unrelated cursor key itest_keepme = %v, want \"survives\". The write is "+
			"`sync_cursor || $2::jsonb` — a whole-blob overwrite here is the SWT-24 landmine, and it "+
			"would silently drop a polled account's jira_updated_at watermark", derefTS(keep))
	}

	// (19) one sync_runs row per lookup account per pass.
	if n := s.count(t, ctx,
		`SELECT count(*) FROM sync_runs WHERE source_account_id=$1 AND status='ok'`,
		s.accounts[tsLookupAcct]); n < 1 {
		t.Errorf("the lookup wrote %d ok sync_runs rows, want one per pass (criterion 19)", n)
	}

	// (11) THE WRITE-THEN-READ-BACK PROOF. Re-run with a client that errors on
	// every call: the verdicts must be unchanged AND no HTTP request may happen
	// at all, because every candidate is now inside the TTL.
	before := s.fake.requests()
	s.fake.errAll(true)
	s.run(t, ctx, ticketstatus.Config{})
	if after := s.fake.requests(); after != before {
		t.Errorf("the re-run made %d HTTP request(s) with every snapshot inside the TTL. Criterion 11: "+
			"'the same verdict is reached from the stored row with ZERO HTTP calls' — that is what "+
			"makes every close reproducible from raw_source_items alone", after-before)
	}
	for key, want := range map[string]string{
		"ILK-MINE": "ready", "ILK-NOTMINE": "closed", "ILK-DONE": "closed", "ILK-FRESH": "closed",
	} {
		if got := s.status(t, ctx, key); got != want {
			t.Errorf("after the offline re-run %s is %q, want %q — the same bytes must give the same "+
				"verdict", key, got, want)
		}
	}
}

// ---- criterion 15: external_url never routes a fetch ------------------------

// ILK-FRESH's ref carries external_url = https://evil.example.org/... , written
// the way `link_external_ref` writes it: AGENT-FACING FREE TEXT. The fetch (when
// one is forced) must still go to the account chosen by PREFIX.
func TestTicketStatus_ExternalURLNeverAimsTheFetch(t *testing.T) {
	ctx := context.Background()
	s := newTSSuite(t, ctx)
	s.fake.put(tsIssue{"ILK-FRESH", "indeterminate", tsLookupID, true})
	s.fake.put(tsIssue{"ILK-MINE", "indeterminate", tsLookupID, true})
	s.fake.put(tsIssue{"ILK-NOTMINE", "indeterminate", tsLookupID, true})
	s.fake.put(tsIssue{"ILK-DONE", "indeterminate", tsLookupID, true})

	// --force bypasses the TTL, so ILK-FRESH IS fetched this time.
	s.run(t, ctx, ticketstatus.Config{Force: true})

	found := false
	for _, k := range s.fake.gets() {
		if k == "ILK-FRESH" {
			found = true
		}
	}
	if !found {
		t.Errorf("--force did not re-fetch ILK-FRESH (gets = %v). Criterion 17: --force bypasses the "+
			"TTL, which is what the smoke uses", s.fake.gets())
	}
	// The fake server IS the account's domain_default; a router that followed
	// external_url would have gone to evil.example.org and fetched nothing here.
	if s.fake.requests() == 0 {
		t.Errorf("no request reached the account chosen by prefix. D18: routing is by ticket-key " +
			"prefix against the account's mandatory scopes, and external_refs.external_url — which an " +
			"agent can write — is never read")
	}
}

// ---- criterion 18: no credential is a LOUD skip -----------------------------

// "Missing OPS_TOKEN_KEY: the pass still reconciles from stored raw, prints a
// line naming each lookup account it skipped, counts their refs unpolled, and
// exits 0. Test asserts the line is printed — a skip that is only visible as a
// zero counter is not enough."
//
// Modelled as a nil Lookup factory, because that is what cmd/connectors/jira
// hands over when it cannot build one: internal/ticketstatus never handles a
// token itself.
func TestTicketStatus_MissingCredentialSkipsTheLookupLoudly(t *testing.T) {
	ctx := context.Background()
	s := newTSSuite(t, ctx)

	// AMENDED 2026-09-09 (implementation session): the original call went
	// through s.run, whose nil-injection convenience REPLACED tsNoLookup (a nil
	// ClientFactory) with the fake factory — the one substitution this test
	// exists to avoid. Run is called directly so nil actually reaches it.
	var stats ticketstatus.Stats
	var runErr error
	out := tsCaptureOutput(t, func() {
		stats, runErr = ticketstatus.Run(ctx, s.pool, s.ex, ticketstatus.Config{Lookup: tsNoLookup})
	})
	if runErr != nil {
		t.Fatalf("ticketstatus.Run: %v\n%s", runErr, out)
	}

	if !strings.Contains(out, tsLookupAcct) {
		t.Errorf("the pass printed nothing naming the lookup account it skipped.\n--- output ---\n%s\n"+
			"--- end ---\nD21: an empty result and a disabled credential must never look the same in "+
			"a log", out)
	}
	if !strings.Contains(strings.ToLower(out), "skip") {
		t.Errorf("the output never says the lookup was SKIPPED:\n%s", out)
	}
	// The status half still does its work — that is the point of the split.
	if got := s.status(t, ctx, "ITS-DONE"); got != "closed" {
		t.Errorf("ITS-DONE is %q, want \"closed\": the status half needs no token at all (Treetop's "+
			"raw is already stored), so a missing credential must not stop it", got)
	}
	if stats.Unpolled < 4 {
		t.Errorf("Stats.Unpolled = %d, want >= 4 (the four ILK refs plus WEBX-9). Their refs are "+
			"counted unpolled, and nothing happens to them", stats.Unpolled)
	}
	for _, key := range []string{"ILK-MINE", "ILK-NOTMINE", "ILK-DONE"} {
		if got := s.status(t, ctx, key); got != "ready" {
			t.Errorf("%s is %q, want \"ready\": with no credential there is no observation, and a ref "+
				"with no observation is never acted on", key, got)
		}
	}
	if s.fake.requests() != 0 {
		t.Errorf("the pass made %d HTTP request(s) with a nil lookup factory", s.fake.requests())
	}
}

// ---- criterion 42: --dry-run writes nothing and fetches nothing -------------

func TestTicketStatus_DryRunTouchesNothing(t *testing.T) {
	ctx := context.Background()
	s := newTSSuite(t, ctx)
	s.fake.put(tsIssue{"ILK-MINE", "indeterminate", tsLookupID, true})
	s.fake.put(tsIssue{"ILK-NOTMINE", "indeterminate", tsOtherID, true})
	s.fake.put(tsIssue{"ILK-DONE", "done", tsLookupID, true})

	rawBefore := s.count(t, ctx, `SELECT count(*) FROM raw_source_items WHERE source_account_id=$1`,
		s.accounts[tsLookupAcct])
	// A BASELINE, not a zero: the fixture seeds two state rows on purpose
	// (ITS-REOPEN's and ITS-DISMISSED's preconditions). Asserting "0 rows" here
	// would have been a test that could only pass by accident.
	stateBefore := s.count(t, ctx, `SELECT count(*) FROM ticket_status_syncs WHERE task_id IN
		(SELECT id FROM tasks WHERE project_id IN ($1,$2))`, s.plain, s.gated)

	stats, out := s.run(t, ctx, ticketstatus.Config{DryRun: true, Force: true})

	if got := s.status(t, ctx, "ITS-DONE"); got != "ready" {
		t.Errorf("a DRY RUN closed ITS-DONE (%q)", got)
	}
	if s.auditCount(t, ctx) != 0 {
		t.Errorf("a dry run wrote %d audit rows for actor %s; it performs NO writes of any kind",
			s.auditCount(t, ctx), tsActor)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM ticket_status_syncs WHERE task_id IN
		(SELECT id FROM tasks WHERE project_id IN ($1,$2))`, s.plain, s.gated); n != stateBefore {
		t.Errorf("a dry run changed the ticket_status_syncs row count by %d, want 0 — a dry run "+
			"writes NOTHING, not even a state row (criterion 42)", n-stateBefore)
	}
	if s.fake.requests() != 0 || stats.Fetched != 0 {
		t.Errorf("a dry run made %d HTTP request(s) and reported Fetched=%d, want 0/0 even with "+
			"--force. 'A dry run cannot even mutate raw_source_items' — the review IS the dry run, so "+
			"it must be safe to run against production at any time", s.fake.requests(), stats.Fetched)
	}
	if after := s.count(t, ctx, `SELECT count(*) FROM raw_source_items WHERE source_account_id=$1`,
		s.accounts[tsLookupAcct]); after != rawBefore {
		t.Errorf("a dry run added %d raw_source_items rows", after-rawBefore)
	}
	// The plan is the review (D9: no shadow mode — this is what replaces it).
	for _, want := range []string{"ITS-DONE", "warranted", "would_fetch"} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry-run plan never mentions %q.\n--- output ---\n%s\n--- end ---\n"+
				"Criterion 42: one line per decision including warranted, drop_reason, the assignee "+
				"comparison, and would_fetch for candidates whose snapshot is missing or stale",
				want, out)
		}
	}
}

// ---- criterion 44: idempotence ----------------------------------------------

func TestTicketStatus_SecondRunOverAnUnchangedWorldDoesNothing(t *testing.T) {
	ctx := context.Background()
	s := newTSSuite(t, ctx)
	s.fake.put(tsIssue{"ILK-MINE", "indeterminate", tsLookupID, true})
	s.fake.put(tsIssue{"ILK-NOTMINE", "indeterminate", tsOtherID, true})
	s.fake.put(tsIssue{"ILK-DONE", "done", tsLookupID, true})

	s.run(t, ctx, ticketstatus.Config{})
	s.run(t, ctx, ticketstatus.Config{}) // let the world converge

	audits := s.auditCount(t, ctx)
	fetches := s.fake.requests()

	stats, _ := s.run(t, ctx, ticketstatus.Config{})

	if got := s.auditCount(t, ctx); got != audits {
		t.Errorf("a third pass over an unchanged world wrote %d more audit rows for %s, want 0. "+
			"Criterion 44 — and the reason it matters is that this pass is a hitchhiker on a */15 "+
			"CronJob: a pass that re-acts every tick fills task_events with noise nobody can read "+
			"past", got-audits, tsActor)
	}
	if got := s.fake.requests(); got != fetches {
		t.Errorf("a third pass made %d more HTTP request(s) inside the TTL, want 0 (D20)", got-fetches)
	}
	if stats.ClosedTicketDone != 0 || stats.ClosedNotAssigned != 0 || stats.Reopened != 0 ||
		stats.RefusedActive != 0 || stats.SuppressedDismissed != 0 {
		t.Errorf("a third pass reported action counters %+v, want all zero — 'converged' is the "+
			"counter for a pass that found nothing to do", stats)
	}
	if stats.Converged < 1 {
		t.Errorf("Stats.Converged = %d, want >= 1: a converged ref is a decision, and printing it is "+
			"how an operator tells a healthy pass from a pass that never ran", stats.Converged)
	}
}

// ---- criterion 12: the lookup account is invisible to the funnel ------------

// D17's whole safety argument, proven against a real schema rather than by
// reading three queries: a jira_lookup account is invisible to ListAccounts (so
// it is never JQL-polled), to pendingRaw (so its rows are never normalized) and
// to accountMeta (so it is never given a site identity by the normalizer) — by
// CONSTRUCTION, because every one of them filters provider='jira'.
func TestTicketStatus_LookupAccountIsInvisibleToTheJiraFunnel(t *testing.T) {
	ctx := context.Background()
	s := newTSSuite(t, ctx)
	s.fake.put(tsIssue{"ILK-MINE", "indeterminate", tsLookupID, true})
	s.fake.put(tsIssue{"ILK-NOTMINE", "indeterminate", tsOtherID, true})
	s.fake.put(tsIssue{"ILK-DONE", "done", tsLookupID, true})

	s.run(t, ctx, ticketstatus.Config{})

	sink := jira.NewSink(s.pool)
	accounts, err := sink.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("jira.ListAccounts: %v", err)
	}
	for _, a := range accounts {
		if a.Email == tsLookupAcct {
			t.Errorf("jira.ListAccounts returned the jira_lookup account (%d/%s). D17: the JQL poller's "+
				"account list filters provider='jira', which is what makes a lookup account "+
				"structurally unable to trigger a project-wide poll", a.ID, a.Email)
		}
	}

	// A FULL Normalize run, exactly as the connector main does it. The polled
	// accounts' fixtures DO normalize (they are ordinary provider='jira' rows,
	// and the funnel counts below would be meaningless if this pass could not
	// see them); the lookup account's must not, and the assertions are scoped to
	// its rows for that reason.
	if _, err := jira.Normalize(ctx, sink, jira.Config{}); err != nil {
		t.Fatalf("jira.Normalize: %v", err)
	}

	if n := s.count(t, ctx,
		`SELECT count(*) FROM raw_source_items WHERE source_account_id=$1 AND normalized_at IS NOT NULL`,
		s.accounts[tsLookupAcct]); n != 0 {
		t.Errorf("%d lookup snapshots were normalized. Those rows exist to be read by this pass and "+
			"by nothing else: normalizing them would create a thread and a message per LHH ticket, "+
			"and the priority-100 LHH capture rule would turn each into a task — the board flood Q1 "+
			"rejected", n)
	}
	// The control that makes the line above mean something: pendingRaw DID have
	// work to do on this database, over the provider='jira' fixtures. A zero here
	// would mean Normalize skipped everything and the isolation proof is vacuous.
	if n := s.count(t, ctx,
		`SELECT count(*) FROM raw_source_items
		  WHERE source_account_id IN (SELECT id FROM source_accounts
		                               WHERE account_email LIKE 'itest-tstatus-%' AND provider='jira')
		    AND normalized_at IS NOT NULL`); n == 0 {
		t.Fatalf("Normalize normalized none of this suite's provider='jira' rows either, so the " +
			"jira_lookup assertion above proves nothing — a scan with nothing to scan")
	}
	if n := s.count(t, ctx,
		`SELECT count(*) FROM normalized_messages m
		   JOIN raw_source_items r ON r.id = m.raw_source_item_id
		  WHERE r.source_account_id = $1`, s.accounts[tsLookupAcct]); n != 0 {
		t.Errorf("%d normalized_messages rows came from lookup snapshots, want 0 (criterion 12: no "+
			"funnel change AT ALL — no thread, no message, no capture decision, no triage inbox row)", n)
	}
	if n := s.count(t, ctx,
		`SELECT count(*) FROM normalized_threads WHERE thread_key LIKE '%:ILK-%'`); n != 0 {
		t.Errorf("%d normalized_threads rows exist for lookup issue keys, want 0. A thread is what a "+
			"capture rule matches on; the LHH rule is priority 100 and would turn every fetched "+
			"issue into a task", n)
	}
	if n := s.count(t, ctx,
		`SELECT count(*) FROM capture_decisions cd
		   JOIN normalized_messages m ON m.id = cd.message_id
		   JOIN raw_source_items r ON r.id = m.raw_source_item_id
		  WHERE r.source_account_id = $1`, s.accounts[tsLookupAcct]); n != 0 {
		t.Errorf("%d capture_decisions rows came from lookup snapshots, want 0 — no message, so no "+
			"decision, so no task", n)
	}
}

// ---- criterion 41: lock contention is log-and-skip --------------------------

// "Contention is log-and-skip returning zero stats and no error (capture's
// policy — this is a hitchhiker on a connector run), and the skip prints a line
// so a silent no-op and a real empty pass are never the same log."
//
// The key is READ OUT of internal/ticketstatus/store.go rather than restated
// here: the repo-wide collision scan fails on a key that appears twice, so a
// test that spelled it would break the guard it depends on.
func TestTicketStatus_LockContentionIsLoggedAndSkipped(t *testing.T) {
	ctx := context.Background()
	s := newTSSuite(t, ctx)

	key := tsLockKeyFromSource(t)

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	var held bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&held); err != nil {
		t.Fatalf("pg_try_advisory_lock: %v", err)
	}
	if !held {
		t.Fatalf("could not take the pass's advisory lock in the test itself; something else holds it")
	}
	defer func() {
		if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key); err != nil {
			t.Fatalf("unlock: %v", err)
		}
	}()

	stats, out := s.run(t, ctx, ticketstatus.Config{})

	if stats.Considered != 0 {
		t.Errorf("a contended pass considered %d refs, want 0 — capture's policy: log and skip, "+
			"returning ZERO stats and no error, because this pass rides on a connector run and four "+
			"CronJobs racing is expected", stats.Considered)
	}
	if got := s.status(t, ctx, "ITS-DONE"); got != "ready" {
		t.Errorf("a contended pass still acted (ITS-DONE is %q)", got)
	}
	if strings.TrimSpace(out) == "" {
		t.Errorf("a contended pass printed nothing. The skip must print a line, or a silent no-op and " +
			"a real empty pass are the same log entry — which is how a permanently wedged lock stays " +
			"invisible for weeks")
	}
}

// ---- criterion 43: the counter vocabulary ------------------------------------

// Every counter the SPEC names, referenced once so the VOCABULARY is a
// compile-time fact rather than a promise. The printed line itself is the
// connector main's / opsctl's job (criterion 45's guard checks that it prints
// before returning the error), and the smoke reads it.
func TestTicketStatus_CountersCoverTheWholeVocabulary(t *testing.T) {
	ctx := context.Background()
	s := newTSSuite(t, ctx)
	// AMENDED 2026-09-09 (implementation session): the fake originally served no
	// ILK issues here, so the three lookup candidates 404ed — which the per-key
	// FetchFailed counter (go-reviewer F2) now correctly reports, contradicting
	// this test's own "healthy pass" premise. Seeded like every sibling test.
	s.fake.put(tsIssue{"ILK-MINE", "indeterminate", tsLookupID, true})
	s.fake.put(tsIssue{"ILK-NOTMINE", "indeterminate", tsOtherID, true})
	s.fake.put(tsIssue{"ILK-DONE", "done", tsLookupID, true})
	st, _ := s.run(t, ctx, ticketstatus.Config{})

	// AMENDED by SWT-34 criterion 25: the vocabulary gained
	// closed_ticket_delivered, and this line is the compile-time fact that says
	// so. The MECHANICAL half — every Stats field appears in both printed
	// counter lines, derived by reflection rather than from a list a test
	// supplies — is
	// delivered_structure_test.go's TestTicketStatus_EveryStatsFieldIsPrintedIn
	// BothCounterLines; this one keeps the human-readable line beside the
	// assertions that read it.
	line := fmt.Sprintf(
		"considered=%d closed_ticket_done=%d closed_ticket_delivered=%d closed_not_assigned=%d "+
			"reopened=%d refused_active=%d "+
			"suppressed_dismissed=%d converged=%d unpolled=%d ambiguous=%d unreadable=%d "+
			"fetched=%d fetch_skipped_ttl=%d fetch_failed=%d",
		st.Considered, st.ClosedTicketDone, st.ClosedTicketDelivered, st.ClosedNotAssigned,
		st.Reopened, st.RefusedActive,
		st.SuppressedDismissed, st.Converged, st.Unpolled, st.Ambiguous, st.Unreadable,
		st.Fetched, st.FetchSkippedTTL, st.FetchFailed)

	if st.Considered < len(s.tasks) {
		t.Errorf("Stats.Considered = %d, want at least this suite's %d jira refs. The candidate set is "+
			"`external_refs WHERE system='jira'` joined to tasks and projects (criterion 30); a "+
			"smaller number means the join is dropping refs — a task that is never considered is a "+
			"task that can never leave the board\n%s", st.Considered, len(s.tasks), line)
	}
	if st.FetchFailed != 0 {
		t.Errorf("Stats.FetchFailed = %d on a healthy pass\n%s", st.FetchFailed, line)
	}
}

// ---- the fake Jira site --------------------------------------------------------

// Two routes, which is all the lookup uses: GET /rest/api/2/myself and
// GET /rest/api/2/issue/{key}. Anything else is a 404 that will show up as a
// fetch failure — including /search/jql, so a project-wide poll sneaking into
// this path fails loudly here (criterion 9).
type tsFakeJira struct {
	mu       sync.Mutex
	srv      *httptest.Server
	issues   map[string]tsIssue
	issueGet []string
	reqs     int
	fail     bool
}

func newTSFakeJira() *tsFakeJira {
	f := &tsFakeJira{issues: map[string]tsIssue{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *tsFakeJira) close()      { f.srv.Close() }
func (f *tsFakeJira) url() string { return f.srv.URL }

func (f *tsFakeJira) put(iss tsIssue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues[iss.key] = iss
}

func (f *tsFakeJira) errAll(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = on
}

func (f *tsFakeJira) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs
}

func (f *tsFakeJira) gets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.issueGet...)
}

// factory is the injected dependency, shaped exactly like
// cmd/connectors/jira/main.go's token-decrypting closure — and pointed at
// acct.SiteBaseURL, so a router that followed external_refs.external_url instead
// would reach a host this server does not serve.
func (f *tsFakeJira) factory() jira.ClientFactory {
	return func(_ context.Context, acct jira.Account) (*jira.Client, error) {
		return jira.NewClient(http.DefaultClient, acct.SiteBaseURL, acct.Email, "itest-token"), nil
	}
}

func (f *tsFakeJira) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs++
	if f.fail {
		http.Error(w, `{"errorMessages":["itest forced failure"]}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/rest/api/2/myself":
		_ = json.NewEncoder(w).Encode(map[string]any{"accountId": tsLookupID})
	case strings.HasPrefix(r.URL.Path, "/rest/api/2/issue/"):
		key := strings.TrimPrefix(r.URL.Path, "/rest/api/2/issue/")
		iss, ok := f.issues[key]
		if !ok {
			http.Error(w, `{"errorMessages":["no issue"]}`, http.StatusNotFound)
			return
		}
		f.issueGet = append(f.issueGet, key)
		_, _ = io.WriteString(w, tsIssueJSON(iss))
	default:
		http.Error(w, `{"errorMessages":["no route: `+r.URL.Path+`"]}`, http.StatusNotFound)
	}
}

// tsNoLookup is a factory that must never be called: criterion 18 models a
// missing credential, and cmd hands over nil in that case.
var tsNoLookup jira.ClientFactory

// ---- output capture ------------------------------------------------------------

// tsCaptureOutput collects both stdout and the default slog handler for the
// duration of fn. Criteria 18 and 41 both demand a PRINTED line — "a skip that
// is only visible as a zero counter is not enough" — and neither says which of
// the two the implementation should use, so the test accepts either.
func tsCaptureOutput(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	stdout := os.Stdout
	logger := slog.Default()
	os.Stdout = w
	slog.SetDefault(slog.New(slog.NewTextHandler(w, nil)))

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	fn()

	os.Stdout = stdout
	slog.SetDefault(logger)
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

func derefTS(v *string) string {
	if v == nil {
		return "NULL"
	}
	return *v
}
