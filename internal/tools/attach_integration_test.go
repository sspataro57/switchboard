//go:build integration

package tools_test

// task_attach against a real database — comms-inbox (SWT-74,
// docs/tickets/comms-inbox_SPEC.md) D7 and criteria 30, 31, 33, 34 and 47:
// route a comm onto a task and close it, in ONE audited transaction.
//
// Called through the executor with the REAL policy matrix (deliveryExecutor) as
// dashboard: / opsctl: / mcp:manual: — humanOnly passes those three — and as
// mcp:treetop, which must be DENIED with a policy_decisions row and no write.
// Reuses revive_integration_test.go's rvSuite wholesale (its cleanup pact, its
// task/message fixtures) and activity_integration_test.go's amRequire0039 /
// maTaskRow.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_comms?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run Attach ./internal/tools/
//
// "TEST THE COLUMN, NOT THE FIXTURE" (IK): criterion 34's assertion is a
// MUTATION — the target is marked with a real inbound message through
// task_mark_activity BEFORE the attach, and both stamps are re-read after it.
// A version that surfaced the target would move activity_at; a version that
// stamped reviewed_at would move that. Neither value is in the fixture.
//
// GREENFIELD NOTE — EXPECTED RED: task_attach is not registered, so every call
// fails with "unknown tool" and each test fails on its own sentence.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - task_attach writes `UPDATE tasks SET status='closed'` itself -> ClosesTheSourceThroughCloseTransition
//     (an active-work source would no longer roll the whole call back).
//   - it puts the note on the target -> TheTargetGainsOneIdsOnlyLogLine.
//   - it marks activity on the target -> TheTargetIsNotSurfaced.
//   - drop the (source,target) dedup -> IsIdempotentOnThePair.
//   - remove task_attach from humanOnly -> AWorkerConsoleIsDenied.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const (
	atDash   = "dashboard:itest-revive" // rvDismisser: the board's Attach form
	atOpsctl = "opsctl:itest-revive"    // rvCloser: the other human shape
	atWorker = "mcp:treetop"            // a worker console — humanOnly refuses it
)

func attachArgs(source, target int64, note string) string {
	m := map[string]any{"task_id": source, "target_task_id": target}
	if note != "" {
		m["note"] = note
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// atLogs returns the target's `log` event messages, oldest first.
func atLogs(t *testing.T, ctx context.Context, s *rvSuite, task int64) []string {
	t.Helper()
	rows, err := s.pool.Query(ctx,
		`SELECT COALESCE(payload->>'message','') FROM task_events
		  WHERE task_id=$1 AND event_type='log' ORDER BY id`, task)
	if err != nil {
		t.Fatalf("read log events of task %d: %v", task, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			t.Fatalf("scan log event: %v", err)
		}
		out = append(out, m)
	}
	return out
}

func atCount(t *testing.T, ctx context.Context, s *rvSuite, q string, args ...any) int {
	t.Helper()
	return s.n(t, ctx, q, args...)
}

// atComm seeds the pair this verb exists for: a COMM task (the thing he is
// looking at in INCOMING) and the TICKET task it belongs with.
func atComm(t *testing.T, ctx context.Context, s *rvSuite, label string) (comm, target, message int64) {
	t.Helper()
	comm, commThread := s.task(t, ctx, "comm-"+label, "ready")
	target, _ = s.task(t, ctx, "ticket-"+label, "ready")
	now := s.dbNow(t, ctx)
	message = s.message(t, ctx, "comm-"+label, commThread, "inbound", now, now)
	// The comm reached INCOMING the way capture puts it there (invariant 3).
	s.call(t, ctx, "task_mark_activity", maActor, comm, activityArgs(comm, message))
	return comm, target, message
}

// ---- criteria 30, 31 and 47: the happy path ------------------------------------------

func TestAttach_TheTargetGainsOneIdsOnlyLogLineAndTheSourceCloses(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)
	amRequire0039(t, ctx, s.pool)

	comm, target, message := atComm(t, ctx, s, "happy")
	logsBefore := len(atLogs(t, ctx, s, target))

	out := s.call(t, ctx, attachTool, atDash, comm, attachArgs(comm, target, "answered on the thread"))
	if out["attached"] != true {
		t.Errorf("task_attach returned %v, want attached:true (D7's result shape: {task_id, target_task_id, "+
			"attached, closed, skipped?})", out)
	}
	if out["closed"] != true {
		t.Errorf("task_attach returned %v, want closed:true — routing a comm is the end of it (D7 step 5)", out)
	}

	// D7 step 4: exactly ONE log row on the target, IDS ONLY.
	logs := atLogs(t, ctx, s, target)
	if len(logs) != logsBefore+1 {
		t.Fatalf("the target gained %d log event(s), want exactly 1 (criterion 47): %v", len(logs)-logsBefore, logs)
	}
	line := logs[len(logs)-1]
	want := "attached: task #" + itoa(comm) + " (message " + itoa(message) + ")"
	if line != want {
		t.Errorf("the target's log line is %q, want EXACTLY %q. Criterion 31 / D7 (unilateral): ids only — the "+
			"NOTE never reaches the target, because task_append_log is pinned to HUMAN tasks on the user "+
			"profile (SWT-38 C4) precisely because a claude task's log feeds a worker prompt, and a note is "+
			"words a session may have composed from untrusted input", line, want)
	}
	if strings.Contains(line, "answered on the thread") {
		t.Errorf("the target's log line carries the NOTE (%q); it rides the SOURCE's close reason and the "+
			"source's own event", line)
	}

	// D7 step 5: the source is closed through closeTransition, so every stamp
	// that verb owns lands.
	var status string
	var closedFrom *string
	var closedAt, reviewedAt *time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT status, closed_from_status, closed_at, reviewed_at FROM tasks WHERE id=$1`, comm).
		Scan(&status, &closedFrom, &closedAt, &reviewedAt); err != nil {
		t.Fatalf("read the comm task: %v", err)
	}
	if status != "closed" {
		t.Errorf("the comm task is %q after being routed, want closed", status)
	}
	if closedFrom == nil || *closedFrom != "ready" {
		t.Errorf("closed_from_status = %v, want \"ready\" — criterion 32: the close goes through "+
			"closeTransition, so closed_at, closed_from_status, the session-state clear and SWT-72's "+
			"reviewed_at stamp all come for FREE", closedFrom)
	}
	if closedAt == nil {
		t.Errorf("closed_at is NULL; closeTransition writes it (criterion 32)")
	}
	if reviewedAt == nil {
		t.Errorf("reviewed_at is NULL on the routed comm. Criterion 32: a close stamps the review (SWT-72 D8), " +
			"which is what takes the row off INCOMING for good")
	}

	// D7 step 6, and the ONE status_changed every close in the system emits (D9:
	// the orchestrator's R1/R2/R8 already handle it).
	if n := atCount(t, ctx, s, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='attached'`, comm); n != 1 {
		t.Errorf("the source carries %d `attached` event(s), want exactly 1 (D7 step 6, payload "+
			"{target_task_id, note})", n)
	}
	var payload []byte
	if err := s.pool.QueryRow(ctx,
		`SELECT payload FROM task_events WHERE task_id=$1 AND event_type='attached' ORDER BY id DESC LIMIT 1`,
		comm).Scan(&payload); err == nil {
		var p map[string]any
		_ = json.Unmarshal(payload, &p)
		if p["target_task_id"] == nil {
			t.Errorf("the `attached` payload %s does not name target_task_id (D7 step 6)", payload)
		}
		if p["note"] != "answered on the thread" {
			t.Errorf("the `attached` payload %s does not carry the note; that is WHERE the note belongs "+
				"(D7: it rides the source's own event and close reason)", payload)
		}
	}
	if n := atCount(t, ctx, s,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='status_changed'`, comm); n != 1 {
		t.Errorf("the source carries %d status_changed event(s), want exactly 1 — the same event every close "+
			"already emits (D9: no new lifecycle traffic for the orchestrator)", n)
	}

	// Invariant 3: one audited call, and a policy_decisions row for it.
	if n := atCount(t, ctx, s,
		`SELECT count(*) FROM audit_events WHERE task_id=$1 AND tool=$2 AND actor=$3 AND status='ok'`,
		comm, attachTool, atDash); n != 1 {
		t.Errorf("audit_events for %s on task %d = %d, want 1 (invariant 3)", attachTool, comm, n)
	}
}

// ---- criterion 34: the target is NOT surfaced ------------------------------------------

func TestAttach_TheTargetIsNotSurfaced(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)
	amRequire0039(t, ctx, s.pool)

	comm, target, _ := atComm(t, ctx, s, "nosurface")
	// The MUTATION the IK demands: give the TARGET a real activity mark and a
	// review stamp first, so "unchanged" is a comparison of values Postgres
	// produced rather than of two NULLs.
	targetThread := s.id(t, ctx,
		`INSERT INTO normalized_threads (thread_key, subject, participants)
		 VALUES ('itest-revive:attach-target','itest-revive','[]') RETURNING id`)
	now := s.dbNow(t, ctx)
	old := s.message(t, ctx, "attach-target", targetThread, "inbound", now, now)
	s.call(t, ctx, "task_mark_activity", maActor, target, activityArgs(target, old))
	s.call(t, ctx, "task_requeue", atOpsctl, target, `{"task_id":`+itoa(target)+`}`)
	before := maTaskRow(t, ctx, s, target)
	if before.activityAt == nil || before.reviewedAt == nil {
		t.Fatalf("CONTROL: the target does not carry both stamps before the attach (%+v); the comparison below "+
			"would be two NULLs", before)
	}
	time.Sleep(5 * time.Millisecond)

	s.call(t, ctx, attachTool, atDash, comm, attachArgs(comm, target, "route it"))

	after := maTaskRow(t, ctx, s, target)
	if !after.activityAt.Equal(*before.activityAt) {
		t.Errorf("the target's activity_at moved %v -> %v. Criterion 34 / D7: the target is NOT surfaced — "+
			"\"he just looked at the comm and decided where it belongs; re-raising the destination is the "+
			"double-row D11 refused\". It also keeps task_mark_activity's contract intact: that tool takes an "+
			"inbound MESSAGE, and an attach is a human act", before.activityAt, after.activityAt)
	}
	if !after.reviewedAt.Equal(*before.reviewedAt) {
		t.Errorf("the target's reviewed_at moved %v -> %v; an attach reviews nothing on the TARGET",
			before.reviewedAt, after.reviewedAt)
	}
	if after.activityBy == nil || *after.activityBy != old {
		t.Errorf("the target's activity_by_message_id = %v, want the earlier message %d unchanged", after.activityBy, old)
	}
	if after.status != before.status {
		t.Errorf("the target's status moved %q -> %q; only the SOURCE closes", before.status, after.status)
	}
}

// ---- criterion 33: idempotence on the (source, target) PAIR ------------------------------

func TestAttach_IsIdempotentOnThePair(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)
	amRequire0039(t, ctx, s.pool)

	comm, target, _ := atComm(t, ctx, s, "dedup")
	second, _ := s.task(t, ctx, "ticket-dedup-2", "ready")

	s.call(t, ctx, attachTool, atDash, comm, attachArgs(comm, target, "first"))
	logs := len(atLogs(t, ctx, s, target))
	attachEvents := atCount(t, ctx, s, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='attached'`, comm)

	// The SAME pair again: a success that changes nothing.
	out := s.call(t, ctx, attachTool, atDash, comm, attachArgs(comm, target, "first again"))
	if out["attached"] != false || out["skipped"] != "already_attached" {
		t.Errorf("a repeated (source,target) attach returned %v, want {attached:false, skipped:\"already_attached\"} "+
			"— a SUCCESS (D7 step 3). The board's Attach form is one tap and a double-tap must be a no-op", out)
	}
	if n := len(atLogs(t, ctx, s, target)); n != logs {
		t.Errorf("the repeat wrote %d further log event(s) on the target, want 0 (D7 step 3)", n-logs)
	}
	if n := atCount(t, ctx, s, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='attached'`, comm); n != attachEvents {
		t.Errorf("the repeat wrote %d further `attached` event(s), want 0", n-attachEvents)
	}

	// A SECOND, DIFFERENT target is legal — he can route one comm onto two
	// tasks — and only the CLOSE is skipped the second time.
	out = s.call(t, ctx, attachTool, atDash, comm, attachArgs(comm, second, "and this one too"))
	if out["attached"] != true {
		t.Errorf("attaching the same comm to a DIFFERENT target returned %v, want attached:true. D7 step 3: the "+
			"dedup is keyed on the PAIR, \"which also makes a second, different target legal\"", out)
	}
	if out["closed"] != false {
		t.Errorf("the second attach returned %v, want closed:false — the source is already closed, and "+
			"closeTransition's idempotent no-op is what reports it", out)
	}
	if n := len(atLogs(t, ctx, s, second)); n != 1 {
		t.Errorf("the second target has %d log event(s), want exactly 1 pointer", n)
	}
	if n := atCount(t, ctx, s, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='attached'`, comm); n != attachEvents+1 {
		t.Errorf("the second, different target recorded %d further `attached` event(s), want 1", n-attachEvents)
	}
}

// ---- criterion 30: the refusals, each by name --------------------------------------------

func TestAttach_RefusesAClosedTargetByNameAndAMissingTask(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)
	amRequire0039(t, ctx, s.pool)

	comm, target, _ := atComm(t, ctx, s, "refusals")
	s.close(t, ctx, target)

	_, err := s.run(ctx, attachTool, atDash, comm, attachArgs(comm, target, ""))
	if err == nil {
		t.Fatalf("task_attach onto a CLOSED target succeeded; D7 step 2: routing live work onto a closed task " +
			"is a mistake and nothing would ever read it")
	}
	if !strings.Contains(err.Error(), "task_reopen") {
		t.Errorf("attaching onto a closed target = %q, want a refusal NAMING task_reopen (D7 step 2)", err)
	}
	if got := s.row(t, ctx, comm).status; got == "closed" {
		t.Errorf("the SOURCE closed although the call was refused (%q); the whole call is one transaction", got)
	}

	// A missing task is lockTask's spelling, on either side.
	for _, args := range []string{
		attachArgs(comm, 99999999, ""),
		attachArgs(99999999, comm, ""),
	} {
		_, err := s.run(ctx, attachTool, atDash, comm, args)
		if err == nil {
			t.Errorf("task_attach(%s) succeeded with a missing task", args)
			continue
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("task_attach(%s) = %q, want lockTask's \"task N not found\" spelling (criterion 30)", args, err)
		}
	}
}

// Criterion 32's behavioural half: the source's close is closeTransition's, so
// an ACTIVE-WORK source errors with activeWorkRefusal and the whole call rolls
// back — the target keeps no pointer for a routing that did not happen.
func TestAttach_ClosesTheSourceThroughCloseTransition(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)
	amRequire0039(t, ctx, s.pool)

	comm, target, _ := atComm(t, ctx, s, "activework")
	s.exec(t, ctx, `UPDATE tasks SET status='in_progress' WHERE id=$1`, comm)
	s.exec(t, ctx,
		`INSERT INTO task_claims (task_id, worker_id, claimed_at, expires_at)
		 VALUES ($1,'itest-revive-worker', now(), now() + interval '1 hour')`, comm)

	logsBefore := len(atLogs(t, ctx, s, target))
	_, err := s.run(ctx, attachTool, atDash, comm, attachArgs(comm, target, "route it"))
	if err == nil {
		t.Fatalf("task_attach on a source with active work succeeded; criterion 32: the close is " +
			"closeTransition's, and its active-work refusal applies to every caller")
	}
	if !strings.Contains(err.Error(), "active work") {
		t.Errorf("the refusal is %q, want closeTransition's activeWorkRefusal (criterion 32)", err)
	}
	if n := len(atLogs(t, ctx, s, target)); n != logsBefore {
		t.Errorf("the target gained %d log event(s) although the call failed, want 0. Criterion 32: ONE "+
			"transaction — a failed close rolls the pointer back", n-logsBefore)
	}
	if n := atCount(t, ctx, s, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='attached'`, comm); n != 0 {
		t.Errorf("the source carries %d `attached` event(s) although the call failed, want 0", n)
	}
}

// ---- criteria 35 and 47: humanOnly, end to end --------------------------------------------

func TestAttach_AWorkerConsoleIsDeniedWithAPolicyRowAndNoWrite(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)
	amRequire0039(t, ctx, s.pool)

	comm, target, _ := atComm(t, ctx, s, "humanonly")
	logsBefore := len(atLogs(t, ctx, s, target))

	_, err := s.run(ctx, attachTool, atWorker, comm, attachArgs(comm, target, "not mine to route"))
	if err == nil {
		t.Fatalf("task_attach as %s succeeded. Criterion 35 / D7: the verb is humanOnly — a worker console "+
			"never chooses its own work, and this one CLOSES a task", atWorker)
	}
	if got := s.row(t, ctx, comm).status; got == "closed" {
		t.Errorf("the denied call closed the comm anyway (%q)", got)
	}
	if n := len(atLogs(t, ctx, s, target)); n != logsBefore {
		t.Errorf("the denied call wrote %d log event(s) on the target, want 0", n-logsBefore)
	}
	// The denial is RECORDED: audit start + a policy_decisions row with the rule.
	var decision, rule string
	if err := s.pool.QueryRow(ctx,
		`SELECT pd.decision, pd.rule FROM policy_decisions pd
		   JOIN audit_events a ON a.id = pd.audit_event_id
		  WHERE a.tool=$1 AND a.actor=$2 ORDER BY pd.id DESC LIMIT 1`, attachTool, atWorker).
		Scan(&decision, &rule); err != nil {
		t.Fatalf("no policy_decisions row for the denied %s as %s: %v (invariant 3: every decision writes one)",
			attachTool, atWorker, err)
	}
	if decision != "deny" || rule != "human_only" {
		t.Errorf("the denial is %s/%s, want deny/human_only (criterion 35)", decision, rule)
	}

	// And the three human shapes pass.
	for _, actor := range []string{atDash, atOpsctl, "mcp:manual:salvo"} {
		actor := actor
		t.Run(actor, func(t *testing.T) {
			c, tg, _ := atComm(t, ctx, s, "human-"+strings.NewReplacer(":", "-", ".", "-").Replace(actor))
			if _, err := s.run(ctx, attachTool, actor, c, attachArgs(c, tg, "")); err != nil {
				t.Errorf("task_attach as %s was refused: %v. D7: interactive mcp:manual:salvo, dashboard: and "+
					"opsctl: all pass", actor, err)
			}
		})
	}
}
