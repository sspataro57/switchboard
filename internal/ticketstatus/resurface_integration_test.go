//go:build integration

package ticketstatus_test

// SWT-45 (docs/tickets/jira-activity-revive_SPEC.md) against a real database,
// the RECONCILER side: criteria 5 (0030's CHECK admits 'resurfaced'), 33 (the
// hold is fed by tasks.surfaced_at through loadCandidates), 34 (upsertState
// records surfaced_seen_at only when RecordSeen, exactly), 35 (the counter),
// 36 (the dry-run line) and 39 (S12: a hand reopen sticks, and so does a hand
// close after it). Reuses store_integration_test.go's suite wholesale (its
// fixtures, cleanup pact over itest-tstatus-%, fake Jira, FATAL guard).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_iso45?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run 'Resurfac|HandReopen|LastActionAdmits' ./internal/ticketstatus/
//
// "TEST THE COLUMN, NOT THE FIXTURE": surfaced_at is written into the tasks ROW
// and the pass must READ it (MUTATION: select NULL for t.surfaced_at in
// loadCandidates -> the held task closes and the first test goes red). The
// surfacing in S12 is written by the real task_reopen handler as a human.
//
// RED TODAY: 0030 is not applied (rsRequire0030 fails every test with one
// sentence); the file also names Stats.Resurfaced, which does not compile.

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/ticketstatus"
)

const rsHuman = "opsctl:itest-tstatus"

func rsRequire0030(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE (table_name='tasks' AND column_name IN ('closed_at','closed_from_status','surfaced_at','surfaced_by_message_id'))
		     OR (table_name='ticket_status_syncs' AND column_name='surfaced_seen_at')`).Scan(&n); err != nil {
		t.Fatalf("probe 0030's columns: %v", err)
	}
	if n != 5 {
		t.Fatalf("found %d of the 5 columns migration 0030 adds (tasks.closed_at, closed_from_status, surfaced_at, "+
			"surfaced_by_message_id; ticket_status_syncs.surfaced_seen_at). Apply "+
			"migrations/0030_jira_activity_revive.sql to the compose db (`make migrate LOCAL_DB_URL=...`). Merging a "+
			"migration is not applying it", n)
	}
}

// addKey seeds one more jira-keyed task under the plain (gate-off) project with
// a stored snapshot, so a test can own its ticket.
func (s *tsSuite) addKey(t *testing.T, ctx context.Context, key, category, taskStatus string) int64 {
	t.Helper()
	s.storeRaw(t, ctx, tsPolledAcct, tsIssue{key, category, tsOwnID, true}, 0)
	id := s.insID(t, ctx, `INSERT INTO tasks (project_id, title, status) VALUES ($1,$2,$3) RETURNING id`,
		s.plain, "itest-tstatus "+key, taskStatus)
	s.tasks[key] = id
	s.refs[key] = s.insID(t, ctx,
		`INSERT INTO external_refs (task_id, system, external_key) VALUES ($1,'jira',$2) RETURNING id`, id, key)
	return id
}

func (s *tsSuite) surfacedAt(t *testing.T, ctx context.Context, key string) (*time.Time, *int64) {
	t.Helper()
	var at *time.Time
	var by *int64
	if err := s.pool.QueryRow(ctx, `SELECT surfaced_at, surfaced_by_message_id FROM tasks WHERE id=$1`,
		s.tasks[key]).Scan(&at, &by); err != nil {
		t.Fatalf("read surfacing of %s: %v", key, err)
	}
	return at, by
}

func (s *tsSuite) seen(t *testing.T, ctx context.Context, key string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT surfaced_seen_at FROM ticket_status_syncs WHERE external_ref_id=$1`,
		s.refs[key]).Scan(&at); err != nil {
		t.Fatalf("read surfaced_seen_at of %s: %v", key, err)
	}
	return at
}

func (s *tsSuite) taskAudits(t *testing.T, ctx context.Context, key string) int {
	t.Helper()
	return s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE actor=$1 AND task_id=$2`, tsActor, s.tasks[key])
}

// ---- criterion 5: the CHECK admits 'resurfaced' and nothing invented ---------

// MUTATION: remove 0030's ADD CONSTRAINT -> 'bogus' inserts and this goes red.
func TestTicketStatus_LastActionAdmitsResurfaced(t *testing.T) {
	ctx := context.Background()
	s := newTSSuite(t, ctx)
	rsRequire0030(t, ctx, s.pool)

	if _, err := s.pool.Exec(ctx,
		`INSERT INTO ticket_status_syncs (external_ref_id, task_id, status_category, last_action, drop_reason)
		 VALUES ($1,$2,'done','resurfaced','ticket_done')`, s.refs["ITS-OPEN"], s.tasks["ITS-OPEN"]); err != nil {
		t.Errorf("INSERT last_action='resurfaced' (with drop_reason) failed: %v. Criterion 4/5: 0030 swaps the CHECK "+
			"to admit it, and a resurfaced row records the drop fact it holds off", err)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO ticket_status_syncs (external_ref_id, task_id, status_category, last_action)
		 VALUES ($1,$2,'done','bogus')`, s.refs["ITS-NOTMINE"], s.tasks["ITS-NOTMINE"]); err == nil {
		t.Errorf("INSERT last_action='bogus' succeeded: ticket_status_syncs.last_action is UNCONSTRAINED — the ADD " +
			"half of 0030's drop/add is missing")
	}
}

// ---- criteria 33, 34, 35: the hold, fed by the column ------------------------

func TestTicketStatus_ASurfacedDoneTicketIsHeldOpenNotClosed(t *testing.T) {
	ctx := context.Background()
	s := newTSSuite(t, ctx)
	rsRequire0030(t, ctx, s.pool)

	s.addKey(t, ctx, "ITS-SURF", "done", "ready")
	s.exec(t, ctx, `UPDATE tasks SET surfaced_at = now() WHERE id=$1`, s.tasks["ITS-SURF"])

	stats, _ := s.run(t, ctx, ticketstatus.Config{})

	if got := s.status(t, ctx, "ITS-SURF"); got != "ready" {
		t.Errorf("ITS-SURF (ticket done, task surfaced) is %q after Run, want ready. J11: the pass holds a surfaced "+
			"task open. MUTATION THAT MUST TURN THIS RED: select NULL for t.surfaced_at in loadCandidates", got)
	}
	if got := s.status(t, ctx, "ITS-DONE"); got != "closed" {
		t.Errorf("ITS-DONE (same facts, surfaced_at NULL) is %q, want closed — the control: without a surfacing the "+
			"reconciler behaves exactly as SWT-32 did", got)
	}
	if n := s.logEvents(t, ctx, "ITS-SURF"); n != 1 {
		t.Errorf("ITS-SURF has %d log events, want exactly 1 (\"ticket is done, activity surfaced it, leaving it open\")", n)
	}
	st := s.state(t, ctx, "ITS-SURF")
	if st.lastAction != "resurfaced" || st.dropReason != "ticket_done" {
		t.Errorf("ITS-SURF state = %+v, want last_action=resurfaced drop_reason=ticket_done (criterion 34)", st)
	}
	// Criterion 34: the round trip is EXACT. Compared with Equal after a re-read of both.
	at, _ := s.surfacedAt(t, ctx, "ITS-SURF")
	seen := s.seen(t, ctx, "ITS-SURF")
	if at == nil || seen == nil || !seen.Equal(*at) {
		t.Errorf("surfaced_seen_at = %v, tasks.surfaced_at = %v; want equal instants. The next pass compares them "+
			"with Equal — an inexact round trip reads as a NEW surfacing every pass, one log line per tick forever", seen, at)
	}
	if stats.Resurfaced < 1 {
		t.Errorf("Stats.Resurfaced = %d, want >= 1 (criterion 35)", stats.Resurfaced)
	}

	// The second pass: converged, ZERO executor calls.
	before := s.taskAudits(t, ctx, "ITS-SURF")
	stats2, _ := s.run(t, ctx, ticketstatus.Config{})
	if after := s.taskAudits(t, ctx, "ITS-SURF"); after != before {
		t.Errorf("a second Run wrote %d audit rows for the held task, want 0 (criterion 33)", after-before)
	}
	if got := s.status(t, ctx, "ITS-SURF"); got != "ready" {
		t.Errorf("the held task closed on the second pass (%q)", got)
	}
	if stats2.Resurfaced != 0 || stats2.Converged < 1 {
		t.Errorf("second pass stats resurfaced=%d converged=%d, want 0 / >= 1", stats2.Resurfaced, stats2.Converged)
	}

	// S10 through real rows: a facts change ends the hold (assignee moved; gate
	// off, so the ticket is still done and still not warranted).
	s.storeRaw(t, ctx, tsPolledAcct, tsIssue{"ITS-SURF", "done", tsOtherID, true}, 0)
	s.run(t, ctx, ticketstatus.Config{})
	if got := s.status(t, ctx, "ITS-SURF"); got != "closed" {
		t.Errorf("after the ticket's assignee changed, the held task is %q, want closed — J11: a facts change closes", got)
	}
}

// Criterion 34's other half: surfaced_seen_at is written only when RecordSeen
// and PRESERVED otherwise (COALESCE(EXCLUDED..., existing)). Active work
// records nothing.
func TestTicketStatus_SurfacedSeenIsPreservedWhenNotRecorded(t *testing.T) {
	ctx := context.Background()
	s := newTSSuite(t, ctx)
	rsRequire0030(t, ctx, s.pool)

	s.addKey(t, ctx, "ITS-SEEN", "done", "ready")
	s.exec(t, ctx, `UPDATE tasks SET surfaced_at = now() - interval '5 minutes' WHERE id=$1`, s.tasks["ITS-SEEN"])
	s.run(t, ctx, ticketstatus.Config{})
	first := s.seen(t, ctx, "ITS-SEEN")
	if first == nil {
		t.Fatalf("the resurfaced pass recorded no surfaced_seen_at")
	}

	s.exec(t, ctx, `UPDATE tasks SET status='in_progress' WHERE id=$1`, s.tasks["ITS-SEEN"])
	s.run(t, ctx, ticketstatus.Config{})
	if st := s.state(t, ctx, "ITS-SEEN"); st.lastAction != "refused_active" {
		t.Fatalf("setup: the in_progress pass recorded %q, want refused_active", st.lastAction)
	}
	if after := s.seen(t, ctx, "ITS-SEEN"); after == nil || !after.Equal(*first) {
		t.Errorf("surfaced_seen_at after an active-work pass = %v, want the preserved %v. MUTATION: drop the COALESCE "+
			"in upsertState -> it goes NULL, and when the worker releases the task the old surfacing reads as new "+
			"and holds it again", after, first)
	}
}

// ---- criterion 36: --dry-run prints the action with no new field -------------

func TestTicketStatus_DryRunPlansResurfaced(t *testing.T) {
	ctx := context.Background()
	s := newTSSuite(t, ctx)
	rsRequire0030(t, ctx, s.pool)

	s.addKey(t, ctx, "ITS-SURFDRY", "done", "ready")
	s.exec(t, ctx, `UPDATE tasks SET surfaced_at = now() WHERE id=$1`, s.tasks["ITS-SURFDRY"])

	_, out := s.run(t, ctx, ticketstatus.Config{DryRun: true})
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "ITS-SURFDRY") && strings.Contains(l, "dry-run") {
			line = l
		}
	}
	if !strings.Contains(line, "action=resurfaced") {
		t.Errorf("the dry-run line for the surfaced task is %q, want it to carry action=resurfaced (Verification step "+
			"7: `opsctl ticket-status sync --dry-run | grep API-4103`)", line)
	}
	if got := s.status(t, ctx, "ITS-SURFDRY"); got != "ready" {
		t.Errorf("a DRY RUN changed the task (%q)", got)
	}
	if st := s.state(t, ctx, "ITS-SURFDRY"); st.found {
		t.Errorf("a dry run wrote a state row (%+v)", st)
	}
}

// ---- criterion 39 / S12 / J8: a hand reopen sticks; a hand close after it too -

func TestTicketStatus_AHandReopenOfADoneTicketSticksAndSoDoesAHandClose(t *testing.T) {
	ctx := context.Background()
	s := newTSSuite(t, ctx)
	rsRequire0030(t, ctx, s.pool)

	s.run(t, ctx, ticketstatus.Config{})
	if got := s.status(t, ctx, "ITS-DONE"); got != "closed" {
		t.Fatalf("setup: the reconciler did not close ITS-DONE (%q)", got)
	}
	// J8: the reconciler's OWN reopen never surfaces (ITS-REOPEN came back this pass).
	if at, _ := s.surfacedAt(t, ctx, "ITS-REOPEN"); at != nil {
		t.Errorf("ITS-REOPEN, reopened by ticketstatus:jira, has surfaced_at=%v. J8: the reconciler's own reopen "+
			"never surfaces — it would hold the task against the very pass that reopened it", at)
	}

	// Verification step 7's smoke, through the executor as a human.
	task := s.tasks["ITS-DONE"]
	if _, err := s.ex.Execute(ctx, executor.Call{Tool: "task_reopen", Actor: rsHuman, TaskID: &task,
		Args: json.RawMessage(`{"task_id":` + strconv.FormatInt(task, 10) + `,"reason":"SWT-45 smoke: comment missed"}`)}); err != nil {
		t.Fatalf("human task_reopen: %v", err)
	}
	at, by := s.surfacedAt(t, ctx, "ITS-DONE")
	if at == nil || by != nil {
		t.Fatalf("after a human's plain task_reopen, surfaced_at=%v surfaced_by_message_id=%v, want set / NULL (J8)", at, by)
	}

	logs := s.logEvents(t, ctx, "ITS-DONE")
	s.run(t, ctx, ticketstatus.Config{})
	if got := s.status(t, ctx, "ITS-DONE"); got != "ready" {
		t.Errorf("the hand-reopened task of a done ticket is %q after the next Run, want ready. F4's pre-existing "+
			"bounce: before SWT-45 the reconciler re-closed a hand reopen within 15 minutes", got)
	}
	if n := s.logEvents(t, ctx, "ITS-DONE"); n != logs+1 {
		t.Errorf("the hold wrote %d log lines, want exactly 1", n-logs)
	}
	before := s.taskAudits(t, ctx, "ITS-DONE")
	s.run(t, ctx, ticketstatus.Config{})
	if after := s.taskAudits(t, ctx, "ITS-DONE"); after != before {
		t.Errorf("a second Run over the held task wrote %d audit rows, want 0", after-before)
	}

	// ...and a hand close afterwards sticks.
	if _, err := s.ex.Execute(ctx, executor.Call{Tool: "task_close", Actor: rsHuman, TaskID: &task,
		Args: json.RawMessage(`{"task_id":` + strconv.FormatInt(task, 10) + `,"reason":"done with it"}`)}); err != nil {
		t.Fatalf("human task_close: %v", err)
	}
	before = s.taskAudits(t, ctx, "ITS-DONE")
	s.run(t, ctx, ticketstatus.Config{})
	if got := s.status(t, ctx, "ITS-DONE"); got != "closed" {
		t.Errorf("a hand close of a held task did not stick (%q). J10: 'one click closes it for good'", got)
	}
	if after := s.taskAudits(t, ctx, "ITS-DONE"); after != before {
		t.Errorf("the pass acted %d time(s) on a hand-closed held task, want 0 — it must never claim that close", after-before)
	}
}
