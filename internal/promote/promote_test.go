package promote_test

// THE purity proof for the promoter (SWT-30,
// docs/tickets/classify-promotion_SPEC.md criteria 6 and 9): the promotion
// decision is a pure function of (verdict, existing task) with ZERO I/O.
//
// This file imports NOTHING but testing and the package under test — no pgx, no
// net, no internal/provider — which is criterion 6's literal demand ("its unit
// test imports no pgx, no net and no provider") and the shape
// internal/orchestrator/rules_test.go established. The ban is not left to
// review: structure_test.go PARSES this file's import block and fails on any of
// those three, so a future "just one lookup" cannot be added here quietly.
//
// GREENFIELD NOTE — EXPECTED RED. internal/promote does not exist, so this
// directory currently holds only _test.go files and the package does not build:
// `go test ./...` reports "build constraints exclude all Go files in
// .../internal/promote" and `go vet -tags integration ./...` reports "no
// non-test Go files". That IS the red state for a spec-first test. Verified in
// the authoring session against a throwaway stub declaring the surface below and
// returning zero values: every assertion here then fires on its own merits
// (Action="" want "task", and so on) rather than on the missing package.
//
// ---- IMPOSED exported surface (promote.go) ------------------------------------
//
// Deliberately MINIMAL. The SPEC fixes the signature and this file only pins
// what it asserts; every other field of Verdict (the ids, title, sender,
// subject, sent_at, link_url that criterion 15 copies into the task body, and
// the stored-vs-current project ids of criterion 16) is the driver's business
// and is asserted against the DATABASE in store_integration_test.go, where the
// values come from columns instead of from the test.
//
//	// Decide is the whole rule set, in order (criterion 6):
//	//   1. an OPEN task on the message's thread  -> attach       (Q3)
//	//   2. a whitelisted kind (payment_due|deadline) -> ready task
//	//   3. anything else                          -> holding, the review lane
//	func Decide(v Verdict, existing *ExistingTask) Decision
//
//	type Verdict struct { Kind string; ... }
//
//	// ExistingTask is the thread's oldest task that is NOT closed/delivered, or
//	// — for the Q3 fall-through — the closed/delivered one that was found
//	// instead. Decide takes the STATUS as an input and stays pure (Q3 answer).
//	type ExistingTask struct { ID int64; Status string }
//
//	type Decision struct {
//	    Action string // "task" | "review" | "attached" (classify_promotions.action)
//	    Status string // create_task's status: "ready" | "holding"; "" when attaching
//	    TaskID int64  // the attach target; 0 otherwise
//	}
//
// The action and status VOCABULARY is spelled with string literals below rather
// than through package constants, on purpose: 'task'/'review'/'attached' is what
// migration 0021's CHECK constraint stores and 'ready'/'holding' is what
// tasks.status stores, so the test asserts the values the database will see. A
// constant that drifted from the CHECK would satisfy a test written against the
// constant.

import (
	"testing"

	"github.com/sspataro57/switchboard/internal/promote"
)

// classifyKinds is the CLOSED enum from internal/classify/prompt.go:42-53, in
// the order the schema lists it, plus one kind that is not in it at all.
//
// The last case is not padding. Criterion 8 says "any kind not in the
// whitelist", and a model that emits a kind outside the schema (or a schema
// widened later) must land in review, never in a live task — the fail-closed
// side. A table over only the five known kinds would leave the default branch
// untested.
var classifyKinds = []struct {
	kind        string
	whitelisted bool
}{
	{"payment_due", true},
	{"deadline", true},
	{"appointment", false},
	{"action_required", false},
	{"informational", false},
	{"newsletter", false}, // not in the enum at all
}

// ---- criterion 7 + 8: the whitelist, and everything else --------------------

// With no existing task on the thread, the kind alone decides: payment_due and
// deadline become a live `ready` task on the personal board; every other flagged
// kind lands in the review lane as `holding` and NEVER as `ready`.
//
// D2's argument restated, because it is what this table defends: the whitelist
// is a Go constant, so widening it is a diff plus a test rather than an UPDATE
// nobody reviews.
func TestDecide_WhitelistDecidesWhenTheThreadHasNoTask(t *testing.T) {
	for _, tc := range classifyKinds {
		t.Run(tc.kind, func(t *testing.T) {
			got := promote.Decide(promote.Verdict{Kind: tc.kind}, nil)

			wantAction, wantStatus := "review", "holding"
			if tc.whitelisted {
				wantAction, wantStatus = "task", "ready"
			}
			if got.Action != wantAction {
				t.Errorf("Decide(kind=%q, existing=nil).Action = %q, want %q "+
					"(criteria 7 and 8: exactly payment_due and deadline auto-create; "+
					"every other flagged kind is the human-review lane)", tc.kind, got.Action, wantAction)
			}
			if got.Status != wantStatus {
				t.Errorf("Decide(kind=%q, existing=nil).Status = %q, want %q — this is create_task's "+
					"status argument, and 'holding' is what makes /tasks?project=personal&status=holding "+
					"the review lane (Q1 = holding tasks)", tc.kind, got.Status, wantStatus)
			}
			if got.TaskID != 0 {
				t.Errorf("Decide(kind=%q, existing=nil).TaskID = %d, want 0: there is no task to attach to, "+
					"and a non-zero id here would make the promoter append a log to a task it invented",
					tc.kind, got.TaskID)
			}
		})
	}
}

// The review lane is a lane, not a soft `ready`. Stated as its own assertion
// because it is the ticket's autonomy argument in one line: a non-whitelisted
// flag must never reach the board as live work, no matter what else changes.
func TestDecide_NonWhitelistedKindIsNeverReady(t *testing.T) {
	for _, tc := range classifyKinds {
		if tc.whitelisted {
			continue
		}
		got := promote.Decide(promote.Verdict{Kind: tc.kind}, nil)
		if got.Status == "ready" || got.Action == "task" {
			t.Errorf("Decide(kind=%q, existing=nil) = {Action:%q Status:%q}; a kind outside the whitelist "+
				"must NEVER become a live task (criterion 8). Widening the whitelist is a one-line diff "+
				"plus a test — not a branch that leaks", tc.kind, got.Action, got.Status)
		}
	}
}

// ---- criterion 9 (Q3): attach to an OPEN task, create past a finished one ----

// taskStatuses is the FULL tasks.status enum (migrations/0001_initial.sql:123-126),
// split the way Q3 splits it: `closed` and `delivered` are finished, everything
// else is open.
//
// Enumerating all twelve is the point. Q3's answer is "not closed/delivered",
// and an implementation that instead listed the open statuses it happened to
// think of would silently create a duplicate task the first time a promoted task
// sat in `blocked` or `needs_feedback`.
var taskStatuses = []struct {
	status string
	open   bool
}{
	{"holding", true},
	{"ready", true},
	{"claimed", true},
	{"in_progress", true},
	{"needs_feedback", true},
	{"pr_open", true},
	{"awaiting_ci", true},
	{"awaiting_merge", true},
	{"done_locally", true},
	{"blocked", true},
	{"delivered", false},
	{"closed", false},
}

// Rule ORDER, pinned: attach-to-an-open-task wins over the whitelist. The kind
// is payment_due — the most whitelisted thing there is — so the only way this
// can pass is if the attach rule is evaluated first.
func TestDecide_AttachToOpenTaskWinsOverTheWhitelist(t *testing.T) {
	const existingID = int64(4242)
	for _, ts := range taskStatuses {
		if !ts.open {
			continue
		}
		t.Run(ts.status, func(t *testing.T) {
			got := promote.Decide(
				promote.Verdict{Kind: "payment_due"},
				&promote.ExistingTask{ID: existingID, Status: ts.status},
			)
			if got.Action != "attached" {
				t.Fatalf("Decide(kind=payment_due, existing={id:%d status:%q}).Action = %q, want %q. "+
					"Criterion 6 pins the ORDER: attach-before-create wins over the whitelist, or a "+
					"re-classified message and every follow-up on the thread create a second task",
					existingID, ts.status, got.Action, "attached")
			}
			if got.TaskID != existingID {
				t.Errorf("Decide(...).TaskID = %d, want %d — the promotion row carries the task the log "+
					"was appended to; a zero here is a row that remembers nothing",
					got.TaskID, existingID)
			}
			if got.Status != "" {
				t.Errorf("Decide(...).Status = %q, want \"\": an attach appends ONE task_append_log event "+
					"and changes nothing else (criterion 9, 'no second task, no status change'). Status "+
					"is create_task's argument, and attaching makes no create_task call", got.Status)
			}
		})
	}
}

// The other half of Q3, and the half that flipped: when the thread's task is
// already `closed` or `delivered`, the verdict falls THROUGH to the whitelist
// rules and creates a NEW task. A second dunning notice on a settled thread is a
// live obligation, and burying it as a log line inside a closed task is how it
// stops being visible on the board.
func TestDecide_FinishedTaskFallsThroughToTheWhitelist(t *testing.T) {
	const finishedID = int64(77)
	for _, ts := range taskStatuses {
		if ts.open {
			continue
		}
		for _, k := range classifyKinds {
			t.Run(ts.status+"/"+k.kind, func(t *testing.T) {
				got := promote.Decide(
					promote.Verdict{Kind: k.kind},
					&promote.ExistingTask{ID: finishedID, Status: ts.status},
				)

				wantAction, wantStatus := "review", "holding"
				if k.whitelisted {
					wantAction, wantStatus = "task", "ready"
				}
				if got.Action != wantAction || got.Status != wantStatus {
					t.Errorf("Decide(kind=%q, existing={id:%d status:%q}) = {Action:%q Status:%q}, "+
						"want {Action:%q Status:%q}. Q3's answer (b): a thread yields at most one OPEN "+
						"task, so a %s task does not absorb a new obligation — the verdict takes the "+
						"ordinary whitelist path",
						k.kind, finishedID, ts.status, got.Action, got.Status, wantAction, wantStatus, ts.status)
				}
				if got.Action == "attached" {
					t.Errorf("Decide(kind=%q, existing={status:%q}) attached to a finished task. That is "+
						"answer (a), which Salvador rejected on 2026-09-09: the follow-up would show only "+
						"as a log event on a closed task's detail page", k.kind, ts.status)
				}
			})
		}
	}
}

// ---- SWT-36 criteria 8 + 9: a dismissed task on the thread (D3, D4, D8) -----
//
// docs/tickets/dismiss-reopen-on-activity_SPEC.md. Same purity rules as the
// rest of this file (criterion 6's imports: testing + the package, nothing
// else — structure_test.go parses this file's import block).
//
// IMPOSED SURFACE (criterion 8 names both fields):
//
//	type ExistingTask struct {
//	    ID          int64
//	    Status      string
//	    DismissalID int64 // the task's OPEN task_dismissals row (status='closed' AND
//	                      // reopened_at IS NULL, D3); 0 = none
//	    // ...plus the dismissal's reason code, for decisionReason's prose. The
//	    // field name is the implementer's; Decide must not branch on it and no
//	    // test here reads it.
//	}
//	type Decision struct {
//	    Action, Status    string
//	    TaskID            int64
//	    ReopenDismissalID int64 // non-zero = attach AND request task_reopen against this dismissal
//	}
//
// Rule order (criterion 8): an open task -> attach; else DismissalID != 0 ->
// {Action:"attached", TaskID, ReopenDismissalID} whatever the kind; else the
// existing whitelist/holding rules.
//
// GREENFIELD NOTE — EXPECTED RED: neither field exists, so this test package
// compile-FAILs until promote.go declares them.

// "open beats dismissed". threadTask looks for an open task FIRST, so an open
// task never carries a DismissalID in production — which is exactly why the
// rule ORDER is pinned here rather than assumed: a Decide that checked the
// dismissal first would ask the handler to reopen a task that is already open
// (harmless, not_closed) while the log line still attached correctly, and the
// mistake would hide until the rule order mattered for something else.
func TestDecide_AnOpenTaskBeatsADismissal(t *testing.T) {
	const existingID, dismissalID = int64(4243), int64(31)
	for _, ts := range taskStatuses {
		if !ts.open {
			continue
		}
		t.Run(ts.status, func(t *testing.T) {
			got := promote.Decide(promote.Verdict{Kind: "payment_due"},
				&promote.ExistingTask{ID: existingID, Status: ts.status, DismissalID: dismissalID})
			if got.Action != "attached" || got.TaskID != existingID {
				t.Fatalf("Decide(open %s task) = %+v, want an attach to %d", ts.status, got, existingID)
			}
			if got.ReopenDismissalID != 0 {
				t.Errorf("Decide(open %s task carrying DismissalID %d).ReopenDismissalID = %d, want 0. Criterion "+
					"8's rule order: an OPEN task gives a plain attach; the reopen request is only for a task "+
					"that is closed with an open dismissal (D3)", ts.status, dismissalID, got.ReopenDismissalID)
			}
		})
	}
}

// "dismissed plus a whitelisted kind attaches and never creates a task" —
// and the same for every other kind. D8: today's Q3 fall-through would create a
// DUPLICATE of the task Salvador just dismissed and split the thread's history;
// whether the handler then reopens (ingested after the dismissal) or only logs
// (ingested before it) is the handler's call, under the row lock (D4), never
// this pure function's.
func TestDecide_ADismissedTaskAttachesAndRequestsAReopenWhateverTheKind(t *testing.T) {
	const dismissedID, dismissalID = int64(88), int64(55)
	for _, k := range classifyKinds {
		k := k
		t.Run(k.kind, func(t *testing.T) {
			got := promote.Decide(promote.Verdict{Kind: k.kind},
				&promote.ExistingTask{ID: dismissedID, Status: "closed", DismissalID: dismissalID})
			if got.Action != "attached" {
				t.Fatalf("Decide(kind=%q, dismissed closed task).Action = %q, want \"attached\". D8: a message on a "+
					"dismissed thread returns to the SAME task (invariant 2's intent); creating a task here "+
					"re-creates what the human just threw away", k.kind, got.Action)
			}
			if got.TaskID != dismissedID {
				t.Errorf("Decide(kind=%q, dismissed).TaskID = %d, want %d (D7: classify_promotions.task_id is "+
					"the dismissed task)", k.kind, got.TaskID, dismissedID)
			}
			if got.ReopenDismissalID != dismissalID {
				t.Errorf("Decide(kind=%q, dismissed).ReopenDismissalID = %d, want %d. The driver passes it to "+
					"task_reopen as dismissal_id; the HANDLER decides whether the dismissal is overtaken (D4)",
					k.kind, got.ReopenDismissalID, dismissalID)
			}
			if got.Status != "" {
				t.Errorf("Decide(kind=%q, dismissed).Status = %q, want \"\". Status is create_task's argument, "+
					"and the restore status is the dismissal's closed_from_status, chosen in the handler (D5) "+
					"— a whitelisted kind must not lift a review-lane task to ready", k.kind, got.Status)
			}
		})
	}
}

// "a non-dismissed closed or delivered task keeps the Q3 new-task behaviour".
// D3's scope: plain task_close, R8's Deliver close and reconciler closes are
// NOT dismissals, and a follow-up past them is a new obligation (SWT-30's Q3).
func TestDecide_AClosedTaskWithoutADismissalKeepsQ3(t *testing.T) {
	for _, status := range []string{"closed", "delivered"} {
		for _, k := range classifyKinds {
			got := promote.Decide(promote.Verdict{Kind: k.kind},
				&promote.ExistingTask{ID: 77, Status: status, DismissalID: 0})
			wantAction, wantStatus := "review", "holding"
			if k.whitelisted {
				wantAction, wantStatus = "task", "ready"
			}
			if got.Action != wantAction || got.Status != wantStatus || got.ReopenDismissalID != 0 {
				t.Errorf("Decide(kind=%q, %s task, no dismissal) = %+v, want {Action:%q Status:%q "+
					"ReopenDismissalID:0}. D3: only a DISMISSAL reopens on activity; everything else keeps "+
					"today's fall-through", k.kind, status, got, wantAction, wantStatus)
			}
		}
	}
}

// "nil existing is unchanged."
func TestDecide_NoTaskOnTheThreadNeverRequestsAReopen(t *testing.T) {
	for _, k := range classifyKinds {
		if got := promote.Decide(promote.Verdict{Kind: k.kind}, nil); got.ReopenDismissalID != 0 || got.TaskID != 0 {
			t.Errorf("Decide(kind=%q, nil) = %+v, want no task and no reopen request", k.kind, got)
		}
	}
}
