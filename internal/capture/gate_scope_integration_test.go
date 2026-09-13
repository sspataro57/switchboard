//go:build integration

package capture_test

// SWT-40 Part D, second review round, against a real database on
// gate_integration_test.go's harness (cgSuite, the fake Jira, the compose-db
// guard and the wholesale cleanup).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run CaptureGate ./internal/capture/
//
//   - Fix 1: a task_log resolution is claimed with task_id NULL, exactly like a
//     task one, and completed only after task_append_log (and the guarded
//     reopen) succeed. A pass that dies in between leaves a row the report's
//     "claimed with no task" line counts, instead of a spent claim that looks
//     complete.
//   - Fix 2: a stored snapshot decides a hold only if it came from the account
//     ticketstatus routes the key to (RouteLookup, by prefix scope); for a key no
//     lookup account claims, from the one poller account that stores it.
//
// MUTATIONS (review round 2):
//   - claim task_log with task_id set → the failing-append test finds task_id
//     set and no WARNING line: red;
//   - drop the account filter (ScopeToRoute) → the two-snapshot test goes
//     pending (Count 2) and the lone-foreign test decides a task from a snapshot
//     no route owns: red.

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/connector/jira"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

// failingAppendExecutor is the real executor with task_append_log replaced by a
// handler that always errors: the append fails AFTER the gate's claim.
func (s *cgSuite) failingAppendExecutor() *executor.Executor {
	reg := executor.NewRegistry()
	tools.Register(reg, s.pool)
	reg.Register(executor.Tool{
		Name:     "task_append_log",
		Validate: func([]byte) error { return nil },
		Handle: func(context.Context, []byte) ([]byte, error) {
			return nil, errors.New("itest-capgate: forced task_append_log failure")
		},
	})
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(s.pool), policy.NewStatic(reg.Names()...))
	return executor.New(reg, checker, audit.NewPGStore(s.pool))
}

// ---- fix 1: task_log is claimed with task_id NULL ------------------------------------

func TestCaptureGate_Integration_TaskLogWhoseAppendFailsStaysVisibleToTheReport(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	task := s.taskWithRef(t, ctx, "GTE-910")
	s.fake.put(cgIssue{"GTE-910", "indeterminate", "In Progress", cgOwnID})
	m := s.mention(t, ctx, "applog-fail", "GTE-910", time.Minute)
	s.live(t, ctx)
	if d, _ := s.decision(t, ctx, m, "live"); d.action != "held" {
		t.Fatalf("setup: live decision = %q, want held (a would-be task_log on a gated project)", d.action)
	}

	_, err := capture.RunGate(ctx, s.pool, s.failingAppendExecutor(),
		capture.GateConfig{Limit: 200, Lookup: s.fake.factory()})
	if err == nil {
		t.Fatalf("RunGate with a failing task_append_log returned no error")
	}

	g, ok := s.decision(t, ctx, m, "gate")
	if !ok || g.action != "task_log" {
		t.Fatalf("gate row = %+v (found %v), want the task_log claim (written before the executor call)", g, ok)
	}
	if g.taskID != nil {
		t.Errorf("the gate task_log row carries task_id %d although task_append_log FAILED: it is claimed with "+
			"task_id NULL and completed only after the append (and the guarded reopen) succeed, so the report's "+
			"crash line can see it", *g.taskID)
	}
	if got := s.n(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='log'`, task); got != 0 {
		t.Errorf("log events on the task = %d, want 0 (the append failed)", got)
	}
	report, err := capture.Report(ctx, s.pool, time.Time{}, "")
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if !regexp.MustCompile(`WARNING: 1 \S.*claimed with no task`).MatchString(report) {
		t.Errorf("the report's crash-artifact line does not count the gate task_log row with no task_id; "+
			"report:\n%s", report)
	}
}

func TestCaptureGate_Integration_SuccessfulTaskLogEndsWithItsTask(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	task := s.taskWithRef(t, ctx, "GTE-911")
	s.fake.put(cgIssue{"GTE-911", "indeterminate", "In Progress", cgOwnID})
	m := s.mention(t, ctx, "applog-ok", "GTE-911", time.Minute)
	s.live(t, ctx)

	st := s.gate(t, ctx)

	g, ok := s.decision(t, ctx, m, "gate")
	if !ok || g.action != "task_log" || g.taskID == nil || *g.taskID != task {
		t.Errorf("gate row = %+v (found %v), want task_log completed with task %d", g, ok, task)
	}
	if st.Appended != 1 {
		t.Errorf("GateStats = %+v, want Appended 1", st)
	}
	report, err := capture.Report(ctx, s.pool, time.Time{}, "")
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if regexp.MustCompile(`WARNING: \d+ \S.*claimed with no task`).MatchString(report) {
		t.Errorf("the report warns about a completed task_log; report:\n%s", report)
	}
}

// ---- fix 2: snapshots are scoped to the routed account --------------------------------

// pollerAccount seeds a provider='jira' (polled) account on a DIFFERENT site,
// whose /myself identity is his: its stored rows would read "assigned to him".
func (s *cgSuite) pollerAccount(t *testing.T, ctx context.Context, label string) int64 {
	t.Helper()
	return s.insID(t, ctx,
		`INSERT INTO source_accounts (provider, account_email, domain_default, scopes, send_enabled, sync_cursor)
		 VALUES ('jira',$1,$2,'{GTE,PLR}',false,jsonb_build_object('own_account_id',$3::text)) RETURNING id`,
		"itest-capgate-poller-"+label+"@example.test", "https://itest-capgate-"+label+".example.test", cgOwnID)
}

// storeIssue writes a stored snapshot of iss under acct NOW (after any message
// seeded before it, so freshness never excuses it).
func (s *cgSuite) storeIssue(t *testing.T, ctx context.Context, acct int64, iss cgIssue) {
	t.Helper()
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1,$2,$3::jsonb,$4)`,
		acct, jira.IssueRawID(iss.key), cgIssueJSON(iss), "itest-capgate-foreign-"+iss.key); err != nil {
		t.Fatalf("store %s under account %d: %v", iss.key, acct, err)
	}
}

// Two stored snapshots of one key under two accounts: the routed lookup
// account's says someone else's; the other site's says his. Only the routed one
// may decide: attributed, no task.
func TestCaptureGate_Integration_OnlyTheRoutedAccountsSnapshotDecides(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	poller := s.pollerAccount(t, ctx, "other-site")
	s.fake.put(cgIssue{"GTE-920", "indeterminate", "In Progress", cgOtherID})
	m := s.mention(t, ctx, "scope-two", "GTE-920", time.Minute)
	s.live(t, ctx)
	s.storeIssue(t, ctx, poller, cgIssue{"GTE-920", "indeterminate", "In Progress", cgOwnID})

	st := s.gate(t, ctx)

	if got := s.fake.getsFor("GTE-920"); got != 1 {
		t.Errorf("GETs for GTE-920 = %d, want 1: the routed account had no snapshot, so it fetches", got)
	}
	g, ok := s.decision(t, ctx, m, "gate")
	if !ok || g.action != "attributed" {
		t.Fatalf("gate row = %+v (found %v), want attributed from the ROUTED account's snapshot (not his); a "+
			"snapshot from another site's account names another tenant's ticket", g, ok)
	}
	if got := s.n(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, s.gated); got != 0 {
		t.Errorf("tasks = %d, want 0", got)
	}
	if st.Attributed != 1 || st.PendingLookup != 0 {
		t.Errorf("GateStats = %+v, want Attributed 1, PendingLookup 0", st)
	}
}

// A lone snapshot from an account the key is NOT routed to (the lookup account
// claims GTE, and its fetch 404s) is unreadable: pending, no task.
func TestCaptureGate_Integration_ALoneSnapshotFromANonRoutedAccountStaysPending(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	poller := s.pollerAccount(t, ctx, "lone")
	m := s.mention(t, ctx, "scope-lone", "GTE-921", time.Minute)
	s.live(t, ctx)
	s.storeIssue(t, ctx, poller, cgIssue{"GTE-921", "indeterminate", "In Progress", cgOwnID})

	st := s.gate(t, ctx)

	if g, ok := s.decision(t, ctx, m, "gate"); ok {
		t.Errorf("gate row %+v written from a snapshot stored by an account GTE is not routed to; it must stay "+
			"pending (and expire fail-closed if the routed account never stores one)", g)
	}
	if got := s.n(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, s.gated); got != 0 {
		t.Errorf("tasks = %d, want 0", got)
	}
	if st.PendingLookup != 1 || st.Resolved != 0 {
		t.Errorf("GateStats = %+v, want PendingLookup 1, Resolved 0", st)
	}
}

// A key no lookup account claims falls back to the poller account storing it —
// when exactly one does. Two storing pollers: unreadable, pending.
func TestCaptureGate_Integration_UnroutedKeyFallsBackToItsOnePollerAccount(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	s.insID(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, url_template, priority, enabled, note)
		 VALUES ($1,'body_regex','PLR-[0-9]+','jira',NULL,'https://itest-capgate.example.test/browse/{key}',100,true,'itest-capgate plr')
		 RETURNING id`, s.gated)
	one := s.pollerAccount(t, ctx, "plr-one")
	two := s.pollerAccount(t, ctx, "plr-two")
	mOne := s.mention(t, ctx, "scope-plr-one", "PLR-5", 2*time.Minute)
	mTwo := s.mention(t, ctx, "scope-plr-two", "PLR-6", time.Minute)
	s.live(t, ctx)
	s.storeIssue(t, ctx, one, cgIssue{"PLR-5", "indeterminate", "In Progress", cgOwnID})
	s.storeIssue(t, ctx, one, cgIssue{"PLR-6", "indeterminate", "In Progress", cgOwnID})
	s.storeIssue(t, ctx, two, cgIssue{"PLR-6", "indeterminate", "In Progress", cgOwnID})

	st := s.gate(t, ctx)

	if s.fake.requests() != 0 {
		t.Errorf("Jira requests = %d, want 0: PLR is claimed by no lookup account, so nothing is fetched", s.fake.requests())
	}
	if g, ok := s.decision(t, ctx, mOne, "gate"); !ok || g.action != "task" {
		t.Errorf("PLR-5 (stored by exactly one poller account) resolved %+v (found %v), want task", g, ok)
	}
	if g, ok := s.decision(t, ctx, mTwo, "gate"); ok {
		t.Errorf("PLR-6 (stored by TWO poller accounts, no lookup route) resolved %+v; it must stay pending", g)
	}
	if st.TasksCreated != 1 || st.PendingLookup != 1 {
		t.Errorf("GateStats = %+v, want TasksCreated 1, PendingLookup 1", st)
	}
}
