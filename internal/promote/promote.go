// Package promote turns stored personal-lane classify verdicts into tasks
// (SWT-30, docs/tickets/classify-promotion_SPEC.md). It is the deliberate,
// one-lane exit from shadow that CLAUDE.md build-order step 6 describes in the
// abstract, applied to the local classify lane.
//
// The package is SEPARATE from internal/classify on purpose (D3): classify
// imports internal/provider by construction, and "the promoter never calls an
// LLM" is only a structural fact if this package cannot reach a provider at
// all — structure_test.go walks the import graph to keep it that way. The only
// tables written directly here are classify_promotions (its own append-only
// decision log, the capture_decisions precedent) — everything in tasks,
// task_events and provenance goes through the executor with actor
// promote:classify (invariant 3).
package promote

import "time"

// Actor is the identity every executor call carries (criterion 13), in the
// capture:{connector} shape.
const Actor = "promote:classify"

// whitelist is the set of verdict kinds that auto-create a LIVE task (D2). A
// Go constant, not configuration: the whitelist IS the autonomy argument this
// ticket makes, and as a DB row a typo would widen auto-creation with no
// review. Widening it later is a one-line diff plus a test. Everything else —
// including kinds outside the closed enum in internal/classify/prompt.go —
// lands in the review lane, never a live task (criterion 8, fail-closed).
var whitelist = map[string]bool{
	"payment_due": true,
	"deadline":    true,
}

// Verdict is one stored classify verdict as the driver reads it back from
// ai_extractions. Decide consumes only Kind; the rest is what the executor
// calls copy into the task (criterion 15: copied, never generated).
type Verdict struct {
	// Lane is which classify lane produced the verdict. The zero value is the
	// personal lane (SWT-30), so every existing caller is unchanged (C1). On the
	// inquiry lane (SWT-40 Part C) Kind carries ask_kind.
	Lane Lane
	Kind string

	MessageID    int64
	ThreadID     *int64
	ExtractionID int64
	RawItemID    *int64

	// ProjectID / ProjectSlug are the message's CURRENT attribution — the
	// latest capture_decisions row's project (criterion 16). StoredProjectID is
	// what fields->>'project_id' said when the verdict was recorded; when the
	// two differ the promotion row's reason records both.
	ProjectID       int64
	ProjectSlug     string
	StoredProjectID int64

	Title   string
	Reason  string
	Sender  string
	Subject string
	LinkURL string
	SentAt  *time.Time // normalized_messages.sent_at; nil when unset

	// RunAt is when the verdict was recorded (ai_runs.created_at) — the clock
	// the cutover compares against (Q2: the verdict clock ONLY).
	RunAt time.Time

	// The inquiry lane's stored facts (SWT-40 C-D9), copied into the task body
	// verbatim; empty on the personal lane. StoredThreadID and ThreadKey are
	// the thread identity the verdict recorded, never re-derived.
	Asker             string
	Channel           string
	ThreadKey         string
	ThreadScope       string
	StoredThreadID    int64
	ExternalMessageID string
}

// ExistingTask is the thread's oldest task that is NOT closed/delivered, or —
// for the Q3 fall-through — the closed/delivered one that was found instead.
// Decide takes the STATUS as an input and stays pure (Q3 answer, 2026-09-09).
//
// DismissalID is SWT-36's input, the ticketstatus.Observation.Dismissed
// pattern: the task's OPEN task_dismissals row (status='closed' AND
// reopened_at IS NULL, D3), 0 = none. DismissalCode is its reason code, for
// the promotion row's prose only — Decide never branches on it.
//
// AssigneeType is tasks.assignee_type, read for the inquiry lane's C-D13
// gate (a non-human thread task is never attached to or reopened). Decide
// never branches on it, and the personal lane never reads it.
type ExistingTask struct {
	ID            int64
	Status        string
	DismissalID   int64
	DismissalCode string
	AssigneeType  string
}

// Decision is what one verdict becomes. Action is classify_promotions.action
// ('task' | 'review' | 'attached'); Status is create_task's status argument
// ('ready' | 'holding', empty when attaching); TaskID is the attach target.
// ReopenDismissalID, when non-zero, means attach AND request a guarded
// task_reopen against that dismissal (SWT-36 D4) — whether the dismissal is
// overtaken is the HANDLER's call, under the row lock, never this package's.
type Decision struct {
	Action            string
	Status            string
	TaskID            int64
	ReopenDismissalID int64
}

// open reports whether a task can still absorb a follow-up. Q3's answer is
// spelled as NOT closed/delivered — never as a list of open statuses — so a
// promoted task sitting in blocked or needs_feedback attaches instead of
// spawning a duplicate.
func open(status string) bool {
	return status != "closed" && status != "delivered"
}

// Decide is the whole rule set, in order (criterion 6):
//
//  1. an OPEN task on the message's thread  -> attach (Q3)
//  2. a DISMISSED task on the thread (SWT-36) -> attach + request a reopen
//     against its open dismissal, whatever the kind (D8: never a duplicate of
//     what the human just dismissed)
//  3. a whitelisted kind (payment_due|deadline) -> ready task
//  4. anything else -> holding, the review lane
//
// On the INQUIRY lane (SWT-40 C-D8) rules 1 and 2 are unchanged, and a create
// uses inquiryCreateStatus ("holding" under O7, action review) instead of rules
// 3-4: the personal whitelist is the personal lane's autonomy argument and
// never applies to an inquiry verdict, whatever its kind string says.
//
// Pure: a function of (verdict, existing task) with zero I/O, unit-tested with
// no pgx, no net and no provider (invariant 7). The attach rule is evaluated
// FIRST — a re-classified message or a follow-up on the thread must never
// create a second task, however whitelisted its kind.
func Decide(v Verdict, existing *ExistingTask) Decision {
	if existing != nil && open(existing.Status) {
		return Decision{Action: "attached", TaskID: existing.ID}
	}
	if existing != nil && existing.DismissalID != 0 {
		return Decision{Action: "attached", TaskID: existing.ID, ReopenDismissalID: existing.DismissalID}
	}
	if v.Lane == LaneInquiry {
		return Decision{Action: actionForStatus(inquiryCreateStatus), Status: inquiryCreateStatus}
	}
	if whitelist[v.Kind] {
		return Decision{Action: "task", Status: "ready"}
	}
	return Decision{Action: "review", Status: "holding"}
}
