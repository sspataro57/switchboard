package tools_test

// task_attach's no-database half — comms-inbox (SWT-74,
// docs/tickets/comms-inbox_SPEC.md) D7 and criteria 29, 31 and 32: the routing
// VERB is registered, validates its arguments before any handler runs, closes
// the source through closeTransition (never an UPDATE of its own), and puts
// IDS ONLY on the target.
//
// Driven through executor.Execute with a NIL pool (requeue_test.go's shape):
// every refusal stops at Validate, so nothing dereferences the pool, and an
// "unknown tool" error is explicitly rejected as NOT a validation failure.
//
// ---- IMPOSED SURFACE (SPEC D7 and the "API / MCP tool changes" table) ---------
//
//	task_attach {task_id, target_task_id, note?}
//	  -> {task_id, target_task_id, attached, closed, skipped?}
//	  registered in tools.Register (internal/tools/createtask.go's table) with
//	  validateAttach / attachTask, implemented in internal/tools/attach.go.
//	  humanOnly in policy; listed in BOTH MCP profiles, beside task_requeue.
//	  Validation refuses task_id <= 0, target_task_id <= 0, the two being EQUAL,
//	  and a note over 500 characters.
//	  ONE transaction, locking the LOWER task id first:
//	    1. refuse task_id == target_task_id;
//	    2. refuse a CLOSED target by name, naming task_reopen;
//	    3. dedup on the (source, target) pair -> {attached:false, skipped:"already_attached"};
//	    4. on the TARGET: one `log` task_events row, IDS ONLY —
//	         attached: task #<source> (message <M>)
//	       (the message omitted when the source has none);
//	    5. on the SOURCE: closeTransition(tx, source, "closed",
//	         "routed to task #<target>: <note>");
//	    6. one `attached` task_events row on the SOURCE, payload {target_task_id, note}.
//
// GREENFIELD NOTE, EXPECTED RED: task_attach is not registered and attach.go
// does not exist, so every test here fails on its own assertion.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - task_attach writes `UPDATE tasks SET status='closed'` itself -> ClosesThroughCloseTransition.
//   - it puts the note or the source's title on the target -> TheTargetLogIsIdsOnly.
//   - it marks activity on the target -> the same test.
//   - drop the self-attach or note-length refusal -> RefusesTheBadArgumentShapes.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

const attachTool = "task_attach"

func attachExecutor() *executor.Executor {
	reg := executor.NewRegistry()
	tools.Register(reg, nil)
	// The STATIC allow-list, not the matrix: this file tests validation, and the
	// humanOnly gate is internal/policy's matrix_attach_test.go (criterion 35).
	return executor.New(reg, policy.NewStatic(reg.Names()...), audit.NewMemStore())
}

// Criterion 29: registered like every other tool.
func TestRegister_TaskAttachIsRegistered(t *testing.T) {
	reg := executor.NewRegistry()
	tools.Register(reg, nil)
	for _, n := range reg.Names() {
		if n == attachTool {
			return
		}
	}
	t.Errorf("tool %q is not registered by tools.Register. D7: \"Route this task onto that one and close it\" "+
		"is the second half of \"I'll route to the tasks or dismiss\", and every verb is one executor call "+
		"(invariant 3)", attachTool)
}

// Criterion 29: the four refusals, each by name.
func TestValidate_TaskAttach_RefusesTheBadArgumentShapes(t *testing.T) {
	ex := attachExecutor()
	ctx := context.Background()
	long := strings.Repeat("x", 501)
	for _, tc := range []struct {
		args, names, why string
	}{
		{`{}`, "task_id", "no source"},
		{`{"task_id":0,"target_task_id":452}`, "task_id", "a zero source"},
		{`{"task_id":-3,"target_task_id":452}`, "task_id", "a negative source"},
		{`{"task_id":481}`, "target_task_id", "no target"},
		{`{"task_id":481,"target_task_id":0}`, "target_task_id", "a zero target"},
		{`{"task_id":481,"target_task_id":-1}`, "target_task_id", "a negative target"},
		{`{"task_id":481,"target_task_id":481}`, "", "D7 step 1: a task cannot be routed onto itself — that " +
			"would close the row and point it at itself"},
		{`{"task_id":481,"target_task_id":452,"note":"` + long + `"}`, "note", "a note over 500 characters: the " +
			"note rides the source's close reason, and a close reason is a line, not a document"},
	} {
		tc := tc
		t.Run(tc.why, func(t *testing.T) {
			_, err := ex.Execute(ctx, executor.Call{Tool: attachTool, Actor: "mcp:manual:salvo",
				Args: json.RawMessage(tc.args)})
			if err == nil {
				t.Fatalf("%s(%s) succeeded; want a validation failure — %s", attachTool, tc.args, tc.why)
			}
			if strings.Contains(err.Error(), "unknown tool") {
				t.Fatalf("%s is not registered (%v); that is not a validation failure", attachTool, err)
			}
			if tc.names != "" && !strings.Contains(err.Error(), tc.names) {
				t.Errorf("%s(%s) = %q, want a refusal naming %s", attachTool, tc.args, err, tc.names)
			}
		})
	}
}

// The shapes that MUST pass validation and reach the handler (a nil pool panics
// or errors — either proves the ARGUMENTS were accepted).
func TestValidate_TaskAttach_AcceptsAPairAndAnOptionalNote(t *testing.T) {
	ex := attachExecutor()
	for _, args := range []string{
		`{"task_id":481,"target_task_id":452}`,
		`{"task_id":481,"target_task_id":452,"note":"answered on the thread"}`,
		`{"task_id":481,"target_task_id":452,"note":""}`,
	} {
		args := args
		t.Run(args, func(t *testing.T) {
			var err error
			func() {
				defer func() { _ = recover() }() // a nil pool panics: that is past validation
				_, err = ex.Execute(context.Background(),
					executor.Call{Tool: attachTool, Actor: "mcp:manual:salvo", Args: json.RawMessage(args)})
			}()
			if err == nil {
				return
			}
			if strings.Contains(err.Error(), "unknown tool") {
				t.Fatalf("%s is not registered (%v)", attachTool, err)
			}
			if strings.Contains(err.Error(), "validate "+attachTool) {
				t.Errorf("%s(%s) was refused by the VALIDATOR: %v. D7: note is OPTIONAL", attachTool, args, err)
			}
		})
	}
}

// ---- criterion 32: the close has ONE writer ----------------------------------------

func attachSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("attach.go")
	if err != nil {
		t.Fatalf("read internal/tools/attach.go: %v — criterion 29: task_attach lives in its own file", err)
	}
	return string(b)
}

func TestTaskAttach_ClosesThroughCloseTransition(t *testing.T) {
	src := attachSource(t)
	if !strings.Contains(src, "closeTransition(") {
		t.Errorf("attach.go does not call closeTransition. Criterion 32 / D7 step 5: it is the ONE writer of " +
			"status='closed', so the active-work refusal, the in-flight-send fence, closed_at, " +
			"closed_from_status, the session-state clear and SWT-72's reviewed_at stamp all come for free")
	}
	for _, banned := range []struct{ tok, why string }{
		{"SET status", "criterion 32: a second UPDATE of tasks.status is how the close rules drift — " +
			"activeWorkRefusal, the send fence and the stamps would all be skipped"},
		{"status='closed'", "criterion 32: closeTransition owns the literal"},
		{`status = 'closed'`, "criterion 32: closeTransition owns the literal"},
		{"task_dismissals", "D7: NOT a dismissal row. A routed comm was CORRECT — it just belongs with other " +
			"work — and writing handled_elsewhere here would mint MISLABELLED training data for the inquiry " +
			"lane (IK SWT-68: the reason codes are labels)"},
		{"activity_at", "criterion 34 / D7: the TARGET is not surfaced — he just looked at the comm and decided " +
			"where it belongs; re-raising the destination is the double-row D11 refused"},
		{"task_mark_activity", "criterion 34: an attach is a HUMAN act, and task_mark_activity's contract takes " +
			"an inbound MESSAGE"},
	} {
		if strings.Contains(src, banned.tok) {
			t.Errorf("internal/tools/attach.go contains %q — %s", banned.tok, banned.why)
		}
	}
	// One transaction, and the lower id is locked FIRST: two concurrent attaches
	// in opposite directions must not deadlock.
	if !strings.Contains(src, "inTx(") {
		t.Errorf("attach.go does not run in one transaction (D7: the log, the close and the event are one act)")
	}
	if !strings.Contains(src, "lockTask(") {
		t.Errorf("attach.go takes no row lock (D7: lock the LOWER task id first, then the higher)")
	}
	if !strings.Contains(src, "task_reopen") {
		t.Errorf("attach.go never names task_reopen. D7 step 2: a CLOSED target is refused BY NAME — " +
			"\"task_reopen is the verb for that\" — because routing live work onto a closed task is a mistake " +
			"and nothing would ever read it")
	}
	if !strings.Contains(src, "already_attached") {
		t.Errorf("attach.go has no already_attached skip. D7 step 3: the same (source, target) pair twice is a " +
			"SUCCESS with attached:false — and the pair-keyed dedup is also what makes a SECOND, different " +
			"target legal (he can route one comm onto two tasks)")
	}
}

// ---- criterion 31: the target's log is IDS ONLY --------------------------------------

func TestTaskAttach_TheTargetLogIsIdsOnly(t *testing.T) {
	src := attachSource(t)
	if !strings.Contains(src, "attached: task #") {
		t.Errorf("attach.go does not carry D7 step 4's text `attached: task #<source> (message <M>)`. " +
			"Criterion 31: ids only — no note, no title, no text from the source task")
	}
	for _, banned := range []struct{ tok, why string }{
		{"a.Note", "criterion 31 / D7 (unilateral): THE NOTE NEVER REACHES THE TARGET. It rides the source's " +
			"close reason and the source's own event. task_append_log is pinned to HUMAN tasks on the user " +
			"profile (SWT-38 C4) precisely because a claude task's log feeds a worker prompt, and a note is " +
			"words a session may have composed from untrusted input"},
		{"title", "criterion 31: ids only — the source task's title is untrusted text"},
		{"body", "criterion 31: ids only"},
		{"assignee", "D7: ids-only on the target makes the verb unconditionally safe on a claude task, with NO " +
			"assignee branch — and an assignee branch would be one more predicate that is false for almost " +
			"every row (the inert-predicate landmine)"},
	} {
		// Scope the scan to the statement that writes the target's log row.
		i := strings.Index(src, "attached: task #")
		if i < 0 {
			continue
		}
		start := i - 600
		if start < 0 {
			start = 0
		}
		window := src[start : i+400]
		if strings.Contains(window, banned.tok) {
			t.Errorf("the target's log statement mentions %q — %s\n\n%s", banned.tok, banned.why, window)
		}
	}
	// The SOURCE's own event carries the note; that is where it belongs.
	if !strings.Contains(src, `"attached"`) {
		t.Errorf("attach.go writes no `attached` task_events row on the SOURCE (D7 step 6, payload " +
			"{target_task_id, note})")
	}
	if !strings.Contains(src, "routed to task #") {
		t.Errorf("attach.go does not close the source with D7 step 5's reason `routed to task #<target>: " +
			"<note>`; a close with a reason is the honest record of what happened")
	}
}
