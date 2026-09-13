//go:build integration

package capture_test

// SWT-40 Part D review fixes against a real database, on gate_integration_test.go's
// harness (cgSuite, the fake Jira, the compose-db guard and the wholesale cleanup).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run CaptureGate ./internal/capture/
//
//   - Fix 1, freshness: a stored snapshot decides a hold only if it was verified
//     at or after the message's first-seen time (normalized_messages.created_at,
//     which no upsert rewrites). A key whose snapshot predates a held message is
//     fetched whatever the TTL; a failed fetch leaves the hold pending. The
//     verified time is the stored row's ingested_at, OR the start of this pass's
//     successful GET when the content came back unchanged — upsertRaw leaves
//     ingested_at alone on an equal hash, so ingested_at alone would starve every
//     mention of an unchanged ticket until it expired.
//   - Fix 2, DryRunGate: stored snapshots only, no writes, one line per hold.
//   - Fix 3, no capture lock across Jira HTTP.
//   - Fix 5, the report's crash-artifact line counts gate rows too.
//   - Fix 6, the inbox re-checks direction = 'inbound'.
//
// MUTATIONS (V3, review round):
//   - drop the freshness forcing (MinFresh) → the D-D6 test decides from the stale
//     "unassigned" snapshot: attributed, no task — red;
//   - drop the freshness READ check (VerifiedAt vs first-seen) → Jira-down decides
//     from the older "assigned" snapshot: a task — red.

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/ticketstatus"
)

// seedSnapshot stores a fresh snapshot of key NOW, through the shared fetch path
// (the reconciler's EnsureSnapshots with Force), exactly as a reconciler pass would.
func (s *cgSuite) seedSnapshot(t *testing.T, ctx context.Context, key string) {
	t.Helper()
	if _, _, err := ticketstatus.EnsureSnapshots(ctx, s.pool, []string{key},
		ticketstatus.Config{Force: true, Lookup: s.fake.factory()}); err != nil {
		t.Fatalf("seed snapshot for %s: %v", key, err)
	}
}

// ---- fix 1: freshness -------------------------------------------------------------

// D-D6, the case the review found: the gate fetched a ticket (unassigned →
// attributed); the ticket is then assigned to him, and the assignment mail
// arrives INSIDE the 1h TTL. The stored snapshot is older than that mail, so it
// may not decide it: the gate refetches and creates the task.
func TestCaptureGate_Integration_LaterAssignmentInsideTheTTLRefetches(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	s.fake.put(cgIssue{"GTE-701", "indeterminate", "In Progress", ""})
	m1 := s.mention(t, ctx, "dd6-mention", "GTE-701", 5*time.Minute)
	s.live(t, ctx)
	s.gate(t, ctx)
	if g, ok := s.decision(t, ctx, m1, "gate"); !ok || g.action != "attributed" {
		t.Fatalf("setup: the unassigned mention resolved %+v (found %v), want attributed", g, ok)
	}
	if got := s.fake.getsFor("GTE-701"); got != 1 {
		t.Fatalf("setup: GETs for GTE-701 = %d, want 1", got)
	}

	// Reassigned to him; Jira mails the assignment (rule 2's sender).
	s.fake.put(cgIssue{"GTE-701", "indeterminate", "In Progress", cgOwnID})
	m2 := s.msg(t, ctx, "dd6-assigned", cgThreadPrefix+"dd6-assigned", "Jira <"+cgJiraSender+">",
		"[JIRA] GTE-701 assigned to you", "The issue was assigned to you.", time.Second)
	s.live(t, ctx)
	if d, _ := s.decision(t, ctx, m2, "live"); d.action != "held" {
		t.Fatalf("setup: the assignment mail's live decision = %q, want held", d.action)
	}

	st := s.gate(t, ctx)

	if got := s.fake.getsFor("GTE-701"); got != 2 {
		t.Errorf("GETs for GTE-701 = %d, want 2: the stored snapshot predates the assignment mail, so the gate "+
			"must refetch inside the TTL (a snapshot older than the message never decides it)", got)
	}
	g, ok := s.decision(t, ctx, m2, "gate")
	if !ok || g.action != "task" {
		t.Errorf("the assignment mail resolved %+v (found %v), want task — D-D6: the next mention of a ticket "+
			"assigned to him later gets its task", g, ok)
	}
	if _, ok := s.refTask(t, ctx, "GTE-701"); !ok {
		t.Errorf("no external_refs row for GTE-701 after the assignment mail")
	}
	if st.TasksCreated != 1 {
		t.Errorf("GateStats = %+v, want TasksCreated 1", st)
	}
}

// Jira down, and the stored snapshot (older than the message) says assigned: the
// forced refetch fails, and the old snapshot may NOT decide. No task, pending.
func TestCaptureGate_Integration_JiraDownWithAnOlderAssignedSnapshotStaysPending(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	s.fake.put(cgIssue{"GTE-702", "indeterminate", "In Progress", cgOwnID})
	s.seedSnapshot(t, ctx, "GTE-702")
	m := s.mention(t, ctx, "down-old", "GTE-702", time.Second)
	s.live(t, ctx)
	s.fake.errAll(true)
	reqsBefore := s.fake.requests()

	st := s.gate(t, ctx)

	if s.fake.requests() == reqsBefore {
		t.Errorf("the gate made no Jira request: a snapshot older than the held message must force a fetch " +
			"whatever the TTL")
	}
	if g, ok := s.decision(t, ctx, m, "gate"); ok {
		t.Errorf("gate row %+v written from a snapshot OLDER than the message while the refetch failed; the "+
			"hold must stay pending (a verdict never comes from a snapshot older than the message)", g)
	}
	if got := s.n(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, s.gated); got != 0 {
		t.Errorf("tasks = %d, want 0", got)
	}
	if st.PendingLookup != 1 || st.Resolved != 0 {
		t.Errorf("GateStats = %+v, want PendingLookup 1, Resolved 0", st)
	}
}

// A snapshot stored AFTER the message was first seen decides it with no GET.
func TestCaptureGate_Integration_SnapshotNewerThanTheMessageCostsNoGet(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	s.fake.put(cgIssue{"GTE-703", "indeterminate", "In Progress", cgOwnID})
	m := s.mention(t, ctx, "fresh", "GTE-703", time.Minute)
	s.seedSnapshot(t, ctx, "GTE-703")
	s.live(t, ctx)

	st := s.gate(t, ctx)

	if got := s.fake.getsFor("GTE-703"); got != 1 {
		t.Errorf("GETs for GTE-703 = %d, want 1 (the seed only): a snapshot newer than the message is fresh "+
			"enough, and the TTL skips it", got)
	}
	if g, ok := s.decision(t, ctx, m, "gate"); !ok || g.action != "task" {
		t.Errorf("gate decision = %+v (found %v), want task from the stored snapshot", g, ok)
	}
	if st.TasksCreated != 1 {
		t.Errorf("GateStats = %+v, want TasksCreated 1", st)
	}
}

// A burst of mentions after one fetch: they post-date the stored snapshot, so the
// next pass refetches ONCE for all of them — and the content comes back
// unchanged (ingested_at does not move), which must still verify them.
func TestCaptureGate_Integration_UnchangedRefetchVerifiesABurstWithOneGet(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	s.fake.put(cgIssue{"GTE-704", "indeterminate", "In Progress", cgOwnID})
	s.mention(t, ctx, "burst-0", "GTE-704", 10*time.Minute)
	s.live(t, ctx)
	s.gate(t, ctx)
	task, ok := s.refTask(t, ctx, "GTE-704")
	if !ok {
		t.Fatalf("setup: the first mention created no task")
	}
	var ingestedBefore time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT ingested_at FROM raw_source_items WHERE source_account_id=$1`, s.lookupAcct).Scan(&ingestedBefore); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}

	burst := []int64{
		s.mention(t, ctx, "burst-1", "GTE-704", 3*time.Second),
		s.mention(t, ctx, "burst-2", "GTE-704", 2*time.Second),
		s.mention(t, ctx, "burst-3", "GTE-704", time.Second),
	}
	s.live(t, ctx)

	st := s.gate(t, ctx)

	if got := s.fake.getsFor("GTE-704"); got != 2 {
		t.Errorf("GETs for GTE-704 = %d, want 2: one fetch for the first mention, ONE refetch for the burst", got)
	}
	var ingestedAfter time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT ingested_at FROM raw_source_items WHERE source_account_id=$1`, s.lookupAcct).Scan(&ingestedAfter); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if !ingestedAfter.Equal(ingestedBefore) {
		t.Logf("note: ingested_at moved on an unchanged refetch (%v -> %v); this test's premise is that it does not",
			ingestedBefore, ingestedAfter)
	}
	for _, m := range burst {
		g, ok := s.decision(t, ctx, m, "gate")
		if !ok || g.action != "task_log" || g.taskID == nil || *g.taskID != task {
			t.Errorf("burst message %d resolved %+v (found %v), want task_log on %d — an unchanged refetch "+
				"verifies the ticket as of the GET, even though ingested_at does not move", m, g, ok, task)
		}
	}
	if st.Appended != 3 || st.PendingLookup != 0 {
		t.Errorf("GateStats = %+v, want Appended 3, PendingLookup 0", st)
	}
}

// ---- fix 3: no capture lock across Jira HTTP -------------------------------------

func TestCaptureGate_Integration_CaptureLockIsNotHeldAcrossJiraHTTP(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	s.fake.put(cgIssue{"GTE-705", "indeterminate", "In Progress", cgOwnID})
	m := s.mention(t, ctx, "nolock", "GTE-705", time.Minute)
	s.live(t, ctx)

	probe, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer probe.Release()
	var mu sync.Mutex
	var free []bool
	s.fake.setOnIssueGet(func() {
		var taken bool
		if err := probe.QueryRow(context.Background(), `SELECT pg_try_advisory_lock($1)`, cgLockKey).Scan(&taken); err != nil {
			t.Errorf("probe lock: %v", err)
			return
		}
		if taken {
			var ok bool
			_ = probe.QueryRow(context.Background(), `SELECT pg_advisory_unlock($1)`, cgLockKey).Scan(&ok)
		}
		mu.Lock()
		free = append(free, taken)
		mu.Unlock()
	})

	s.gate(t, ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(free) == 0 {
		t.Fatalf("no issue GET happened; the probe proves nothing")
	}
	for i, f := range free {
		if !f {
			t.Errorf("issue GET %d ran while capture's lock 0x%X was held: every connector's capture pass "+
				"would wait on Jira. Fetch first (idempotent ingestion), then lock and decide", i, cgLockKey)
		}
	}
	if g, ok := s.decision(t, ctx, m, "gate"); !ok || g.action != "task" {
		t.Errorf("gate decision = %+v (found %v), want task", g, ok)
	}
}

// ---- fix 6: the inbox re-checks direction ------------------------------------------

// A held message whose direction a later re-normalization rewrote to outbound
// (the normalized_messages upserts rewrite direction) is not the gate's to act on.
func TestCaptureGate_Integration_OutboundHeldMessageIsNotResolved(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	s.fake.put(cgIssue{"GTE-706", "indeterminate", "In Progress", cgOwnID})
	m := s.mention(t, ctx, "outbound", "GTE-706", time.Minute)
	s.live(t, ctx)
	if d, _ := s.decision(t, ctx, m, "live"); d.action != "held" {
		t.Fatalf("setup: live decision = %q, want held", d.action)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE normalized_messages SET direction='outbound' WHERE id=$1`, m); err != nil {
		t.Fatalf("flip direction: %v", err)
	}

	st := s.gate(t, ctx)

	if g, ok := s.decision(t, ctx, m, "gate"); ok {
		t.Errorf("an OUTBOUND held message was resolved (%+v); the gate inbox re-checks direction = 'inbound'", g)
	}
	if got := s.n(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, s.gated); got != 0 {
		t.Errorf("tasks = %d, want 0", got)
	}
	if st.PendingLookup != 0 || st.Resolved != 0 || s.fake.requests() != 0 {
		t.Errorf("GateStats = %+v, Jira requests %d; want nothing — the message is not in the inbox", st, s.fake.requests())
	}
}

// ---- fix 5: the report's crash-artifact line ----------------------------------------

// A gate `task` row is claimed BEFORE create_task runs; a crash in between leaves
// a gate task row with no task_id, and the claim is permanent. It is the latest
// decision for its message, so the same WARNING line that counts the live shape
// must count it.
func TestCaptureGate_Integration_ReportCountsAGateTaskWithNoTask(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	m := s.mention(t, ctx, "crash", "GTE-707", time.Minute)
	s.live(t, ctx)
	if d, _ := s.decision(t, ctx, m, "live"); d.action != "held" {
		t.Fatalf("setup: live decision = %q, want held", d.action)
	}
	// The crash artifact: the gate's claim, never completed.
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO capture_decisions (message_id, mode, matched_rule_id, project_id, action, external_system, external_key, reason)
		 VALUES ($1,'gate',$2,$3,'task','jira','GTE-707','warranted: itest crash artifact')`, m, s.ruleKey, s.gated); err != nil {
		t.Fatalf("seed crash artifact: %v", err)
	}

	report, err := capture.Report(ctx, s.pool, time.Time{}, "")
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if !regexp.MustCompile(`WARNING: 1 \S.*no task`).MatchString(report) {
		t.Errorf("the report does not count the gate task row with no task_id on its crash-artifact WARNING "+
			"line (it counts mode IN ('live','gate')); report:\n%s", report)
	}
}

// ---- fix 2: the dry run ---------------------------------------------------------------

var dryLine = regexp.MustCompile(`message=(\d+) key=(\S+) outcome=(\S+)`)

func dryOutcomes(t *testing.T, out string) map[int64]string {
	t.Helper()
	got := map[int64]string{}
	for _, line := range strings.Split(out, "\n") {
		if m := dryLine.FindStringSubmatch(line); m != nil {
			var id int64
			fmt.Sscan(m[1], &id)
			got[id] = m[2] + " " + m[3]
		}
	}
	return got
}

func (s *cgSuite) worldCounts(t *testing.T, ctx context.Context) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, tbl := range []string{"capture_decisions", "tasks", "external_refs", "task_events", "audit_events",
		"raw_source_items", "sync_runs", "task_dismissals"} {
		out[tbl] = s.n(t, ctx, `SELECT count(*) FROM `+tbl)
	}
	var cursor string
	if err := s.pool.QueryRow(ctx, `SELECT sync_cursor::text FROM source_accounts WHERE id=$1`, s.lookupAcct).Scan(&cursor); err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	out["cursor:"+cursor] = 1
	return out
}

func TestCaptureGate_Integration_DryRunWritesNothingAndPrintsOutcomes(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	s.fake.put(cgIssue{"GTE-801", "indeterminate", "In Progress", cgOwnID})
	s.fake.put(cgIssue{"GTE-802", "indeterminate", "In Progress", cgOtherID})
	s.fake.put(cgIssue{"GTE-804", "indeterminate", "In Progress", cgOwnID})
	a1 := s.mention(t, ctx, "dry-a1", "GTE-801", 5*time.Minute)
	a2 := s.mention(t, ctx, "dry-a2", "GTE-801", 4*time.Minute)
	b := s.mention(t, ctx, "dry-b", "GTE-802", 3*time.Minute)
	c := s.mention(t, ctx, "dry-c", "GTE-803", 2*time.Minute) // never stored: pending
	s.seedSnapshot(t, ctx, "GTE-801")
	s.seedSnapshot(t, ctx, "GTE-802")
	s.seedSnapshot(t, ctx, "GTE-804")
	d := s.mention(t, ctx, "dry-d", "GTE-804", time.Second) // newer than its snapshot: pending, no fetch
	s.live(t, ctx)
	before := s.worldCounts(t, ctx)
	reqs := s.fake.requests()

	var buf bytes.Buffer
	st, err := capture.DryRunGate(ctx, s.pool, capture.GateDryRunConfig{Out: &buf})
	if err != nil {
		t.Fatalf("DryRunGate: %v", err)
	}
	t.Logf("dry run:\n%s", buf.String())

	if after := s.worldCounts(t, ctx); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("the dry run changed the world: %v -> %v", before, after)
	}
	if got := s.fake.requests(); got != reqs {
		t.Errorf("the dry run made %d Jira request(s); it reads stored snapshots only", got-reqs)
	}
	got := dryOutcomes(t, buf.String())
	want := map[int64]string{
		a1: "GTE-801 task",
		a2: "GTE-801 task_log", // the task a1 would have created
		b:  "GTE-802 attributed:not_assigned",
		c:  "GTE-803 pending_lookup",
		d:  "GTE-804 pending_lookup",
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("dry-run line for message %d = %q, want %q", id, got[id], w)
		}
	}
	if st.TasksCreated != 1 || st.Appended != 1 || st.Attributed != 1 || st.PendingLookup != 2 {
		t.Errorf("dry-run GateStats = %+v, want TasksCreated 1, Appended 1, Attributed 1, PendingLookup 2", st)
	}
}

func TestCaptureGate_Integration_DryRunShadowReadsTheLatestShadowHolds(t *testing.T) {
	ctx := context.Background()
	s := newCGSuite(t, ctx)
	s.fake.put(cgIssue{"GTE-811", "indeterminate", "In Progress", cgOwnID})
	m := s.mention(t, ctx, "dry-shadow", "GTE-811", time.Minute)
	s.seedSnapshot(t, ctx, "GTE-811")
	s.shadow(t, ctx, false)
	s.shadow(t, ctx, true) // two shadow rows: the LATEST is read, once
	if d, ok := s.decision(t, ctx, m, "shadow"); !ok || d.action != "held" {
		t.Fatalf("setup: shadow decision = %+v (found %v), want held", d, ok)
	}
	before := s.worldCounts(t, ctx)

	var live bytes.Buffer
	if _, err := capture.DryRunGate(ctx, s.pool, capture.GateDryRunConfig{Out: &live}); err != nil {
		t.Fatalf("DryRunGate (live): %v", err)
	}
	if got := dryOutcomes(t, live.String()); len(got) != 0 {
		t.Errorf("the live dry run printed %v; there is no live hold (only shadow ones)", got)
	}

	var shadow bytes.Buffer
	st, err := capture.DryRunGate(ctx, s.pool, capture.GateDryRunConfig{Shadow: true, Out: &shadow})
	if err != nil {
		t.Fatalf("DryRunGate (shadow): %v", err)
	}
	t.Logf("shadow dry run:\n%s", shadow.String())
	got := dryOutcomes(t, shadow.String())
	if got[m] != "GTE-811 task" || len(got) != 1 {
		t.Errorf("shadow dry-run lines = %v, want exactly {%d: GTE-811 task}", got, m)
	}
	if st.TasksCreated != 1 {
		t.Errorf("shadow dry-run GateStats = %+v, want TasksCreated 1", st)
	}
	if after := s.worldCounts(t, ctx); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("the shadow dry run changed the world: %v -> %v", before, after)
	}
}
