//go:build integration

package capture_test

// Integration test for SWT-31 (docs/tickets/board-dismissals_SPEC.md) criterion
// 3: BOTH inputs to the thread-keyed title label come from COLUMNS, and both
// reach the created task's title.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run CaptureTitle ./internal/capture/
//
// WHY THIS IS AN INTEGRATION TEST AND NOT A UNIT TEST (institutional landmine 6,
// stated in criterion 3 itself): "for any predicate whose input comes from a
// column, the regression test belongs in the integration suite, and it must fail
// when the column is dropped from the SELECT." The unit tests in
// rules_title_test.go SUPPLY sender and project name themselves, so they cannot
// tell a wired-up column from a fixture — that is exactly how internal/drafts'
// locality guard passed for a week while being inert. The two mutations this
// test exists to catch:
//
//   - replace `COALESCE(m.sender,'')` with a literal in pendingMessages' SELECT
//     -> the Mario Cruz case goes red;
//   - drop `p.name` from loadRules' SELECT (or scan it into the wrong field)
//     -> the empty-sender case goes red.
//
// Run either mutation by hand before trusting this file.
//
// GREENFIELD NOTE — EXPECTED RED. ruleTaskTitle still renders `{key} — {head}`
// for a thread-keyed task, so both title assertions fail with the 128-character
// thread key in place of the label. Nothing here needs a new migration.
//
// CROSS-SUITE DISCIPLINE. Joins the capture suite's cleanup pact verbatim:
// EvaluateRules' pending filter is GLOBAL, so a run here writes a
// capture_decisions row for every other suite's leftover inbound message, and
// capture_decisions.task_id has NO cascade — a leftover row blocks another
// suite's `DELETE FROM tasks` with an FK violation that reads like the
// cross-pollution pact breaking. So capture_decisions is deleted WHOLESALE
// (compose db only; the guard refuses the production DSN), and this suite owns:
//   - project itest-captitle-saka
//   - source_account (upwork_crm, itest-captitle@upwork.example.test)
//   - thread_key prefix upwork_crm:9100:room:%

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	// The slug and the NAME differ, and neither is a substring of the other.
	// That is the fixture's whole job: a title carrying "Saka Test Client" can
	// only have come from projects.name, and one carrying the slug can only have
	// come from the column this package already had.
	ctSlug = "itest-captitle-saka"
	ctName = "Saka Test Client"

	ctAccount = "itest-captitle@upwork.example.test"

	// Real upwork_crm thread-key shape (internal/connector/upworkcrm/threadkey.go
	// owns the format; nothing in internal/capture may parse it — criterion 5).
	// A `thread_key_prefix` rule with NO key_regex derives the external key from
	// this string VERBATIM, which is the entire defect this ticket fixes.
	ctPrefix  = "upwork_crm:9100:room:"
	ctThreadA = "upwork_crm:9100:room:8801"
	ctThreadB = "upwork_crm:9100:room:8802"

	ctSender = "Mario Cruz"
	ctBody   = "Hi Salvador,\nI wanted to check in about the invoice"
)

type ctSuite struct {
	pool    *pgxpool.Pool
	ex      *executor.Executor
	project int64
	ruleID  int64
}

func newCTSuite(t *testing.T, ctx context.Context) *ctSuite {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db (cleanup deletes capture_decisions " +
			"wholesale); use the compose db on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	cleanupCaptureTitle(t, ctx, pool)
	t.Cleanup(func() { cleanupCaptureTitle(t, ctx, pool) })

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))

	s := &ctSuite{pool: pool, ex: executor.New(reg, checker, audit.NewPGStore(pool))}
	s.seed(t, ctx)
	return s
}

func cleanupCaptureTitle(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const acct = `(SELECT id FROM source_accounts WHERE provider='upwork_crm' AND account_email='` + ctAccount + `')`
	const proj = `(SELECT id FROM projects WHERE slug = '` + ctSlug + `')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + proj + `)`
	for _, q := range []string{
		`DELETE FROM capture_decisions`,
		`DELETE FROM external_refs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM deliveries WHERE task_id IN ` + tasksOf,
		// audit_events.task_id FKs tasks and the create_task audit row predates
		// the task, so it is swept by actor namespace too.
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor LIKE 'capture:%')`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor LIKE 'capture:%'`,
		`DELETE FROM tasks WHERE project_id IN ` + proj,
		`DELETE FROM capture_rules WHERE project_id IN ` + proj,
		`DELETE FROM projects WHERE slug = '` + ctSlug + `'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN
		   (SELECT id FROM raw_source_items WHERE source_account_id IN ` + acct + `)`,
		`DELETE FROM normalized_threads WHERE thread_key IN ('` + ctThreadA + `','` + ctThreadB + `')`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + acct,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + acct,
		`DELETE FROM source_accounts WHERE provider='upwork_crm' AND account_email='` + ctAccount + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func (s *ctSuite) insID(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", q, err)
	}
	return id
}

func (s *ctSuite) seed(t *testing.T, ctx context.Context) {
	t.Helper()

	// name != slug. Asserted, not assumed: if a future edit makes them equal,
	// the project-name case below would pass on the slug and prove nothing.
	s.project = s.insID(t, ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$2,'','manual','dashboard','/tmp/itest-captitle','any') RETURNING id`, ctName, ctSlug)
	if ctName == ctSlug || strings.Contains(ctSlug, ctName) {
		t.Fatalf("fixture invalid: projects.name (%q) and projects.slug (%q) must be distinct strings, or "+
			"the fallback assertion cannot tell which column fed the title", ctName, ctSlug)
	}

	// A thread_key_prefix rule with NO key_regex: capture.externalKey returns the
	// thread_key verbatim, which is the shape D1 branches on. external_system is
	// set, so a live pass creates a task (a NULL system is attribution-only).
	s.ruleID = s.insID(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex,
		                            url_template, priority, enabled, note)
		 VALUES ($1,'thread_key_prefix',$2,'upwork_crm',NULL,NULL,200,true,'itest-captitle upwork rooms')
		 RETURNING id`, s.project, ctPrefix)

	acct := s.insID(t, ctx,
		`INSERT INTO source_accounts (provider, account_email, scopes, send_enabled, calendar_in_availability)
		 VALUES ('upwork_crm',$1,'{}',false,false)
		 ON CONFLICT (provider, account_email) DO UPDATE SET account_email=EXCLUDED.account_email
		 RETURNING id`, ctAccount)

	threadA := s.insID(t, ctx,
		`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,'','[]') RETURNING id`, ctThreadA)
	threadB := s.insID(t, ctx,
		`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,'','[]') RETURNING id`, ctThreadB)

	// The CRM stored a display name for this one.
	s.message(t, ctx, acct, threadA, "a", ctSender)
	// And none for this one — the case Q1's fallback chain exists for.
	s.message(t, ctx, acct, threadB, "b", "")
}

func (s *ctSuite) message(t *testing.T, ctx context.Context, acct, thread int64, label, sender string) {
	t.Helper()
	raw := s.insID(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,'{}',$3, now()) RETURNING id`,
		acct, "itest-captitle-"+label, "itest-captitle-hash-"+label)
	s.insID(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		    body_text, subject, sender, channel)
		 VALUES ($1,$2,'inbound',$3, now() - interval '10 minutes', $4, '', $5, 'upwork_chat') RETURNING id`,
		raw, thread, "itest-captitle-msg-"+label, ctBody, sender)
}

// titleFor reads the title of the task the live pass created for one thread key,
// through external_refs — the dedup key, and the only link that does not depend
// on the title itself.
func (s *ctSuite) titleFor(t *testing.T, ctx context.Context, key string) string {
	t.Helper()
	var title string
	if err := s.pool.QueryRow(ctx,
		`SELECT t.title FROM tasks t JOIN external_refs er ON er.task_id = t.id
		  WHERE er.system='upwork_crm' AND er.external_key=$1`, key).Scan(&title); err != nil {
		t.Fatalf("no task linked to %s after a live capture pass: %v", key, err)
	}
	return title
}

// ---- criterion 3: the sender column feeds the title ---------------------------

func TestCaptureTitle_Integration_SenderAndProjectNameComeFromColumns(t *testing.T) {
	ctx := context.Background()
	s := newCTSuite(t, ctx)

	if _, err := capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{Mode: "live"}); err != nil {
		t.Fatalf("EvaluateRules(live): %v", err)
	}

	// The sender half. `Mario Cruz` exists nowhere in this test's rules,
	// projects or thread keys — only in normalized_messages.sender — so the
	// string can only have arrived through pendingMessages' SELECT.
	gotA := s.titleFor(t, ctx, ctThreadA)
	if want := ctSender + " — Hi Salvador,"; gotA != want {
		t.Errorf("task title for the message WITH a sender = %q, want %q.\nCriterion 3: the label is "+
			"`COALESCE(m.sender,'')` from pendingMessages' SELECT. Replace that expression with a "+
			"literal and this assertion must go red — if it stays green, the title is being fed by "+
			"the fixture rather than by the column (institutional landmine 6)", gotA, want)
	}

	// The project-name half. `Saka Test Client` exists ONLY in projects.name —
	// not in the slug, not in the rule, not in the message — so it can only have
	// arrived through loadRules' SELECT gaining p.name.
	gotB := s.titleFor(t, ctx, ctThreadB)
	if want := ctName + " — Hi Salvador,"; gotB != want {
		t.Errorf("task title for the message with NO sender = %q, want %q.\nCriterion 3: the fallback is "+
			"projects.name, which means loadRules' SELECT gains p.name and storedRule gains the field. "+
			"Drop the column and this must go red; a title reading %q would mean the code fell through "+
			"to the slug it already had", gotB, want, ctSlug+" — Hi Salvador,")
	}

	// And the defect itself, stated as its own assertion so a partial fix is
	// legible: neither title may still be the thread key.
	for key, got := range map[string]string{ctThreadA: gotA, ctThreadB: gotB} {
		if strings.Contains(got, ctPrefix) {
			t.Errorf("the title for %s still contains the raw thread key: %q. That is the 128-character "+
				"board row this ticket exists to fix; the key stays in external_refs and in the task "+
				"body (criterion 6)", key, got)
		}
	}

	// The body still carries what the title dropped (criterion 6), read from the
	// database rather than from the composer — the title change is only safe
	// because the identity survives on /tasks/{id}.
	var body string
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(t.body,'') FROM tasks t JOIN external_refs er ON er.task_id = t.id
		  WHERE er.system='upwork_crm' AND er.external_key=$1`, ctThreadA).Scan(&body); err != nil {
		t.Fatalf("read task body: %v", err)
	}
	for _, want := range []string{ctThreadA, ctSender, "message_id:"} {
		if !strings.Contains(body, want) {
			t.Errorf("the created task's body does not contain %q:\n%s", want, body)
		}
	}
}
