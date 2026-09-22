package promote_test

// activity-resurfaces (SWT-72, docs/tickets/activity-resurfaces_SPEC.md) D11
// and criteria 13, 15, 16, 37 — the OWNER DECISION of 2026-09-22:
//
//	"and those messages to create it's own tasks even is they require action on
//	 other tasks that would be for claude to figure out"
//	"that was the intent int the incomning. fast incoming event to tell people
//	 I'll check"
//
// An inquiry-lane verdict that needs a reply ALWAYS becomes its own task,
// whatever else is open on that thread. José's four asks and Katie's two were
// `attached` onto #452 / #464 and nobody saw them.
//
// PURE: this file imports nothing but testing and the package under test, the
// promote_test.go rule that structure_test.go PARSES and enforces — no pgx, no
// net, no provider (invariant 7).
//
// ---- IMPOSED SURFACE (D11; greenfield, so the SPEC's contract defines it) -----
//
//	type Decision struct { Action, Status string; TaskID, ReopenDismissalID int64
//	                      RelatedTaskID int64 } // set on the INQUIRY CREATE only
//
//	Decide, for LaneInquiry:
//	  inquiry, existing OPEN      -> {Action: actionForStatus(inquiryCreateStatus),
//	                                  Status: inquiryCreateStatus,
//	                                  RelatedTaskID: existing.ID}   // CHANGED
//	  inquiry, dismissed          -> attached + reopen  (rule 2, UNCHANGED)
//	  inquiry, nothing / finished -> create             (UNCHANGED)
//	  PERSONAL lane: byte-unchanged, RelatedTaskID always 0.
//
//	InquiryGate's C-D13 NARROWS: refuse claude_task only when the thread task is
//	not a human's AND Decide would ATTACH to it (rule 2's dismissed path) —
//	spelled as one call to the pure Decide, so "would attach" has one spelling.
//	Every other C3 reason is untouched, GateAnswered included.
//
// GREENFIELD NOTE — EXPECTED RED: Decision has no RelatedTaskID field, so this
// file compile-FAILS package promote_test; once it compiles, Decide still
// returns `attached` for an inquiry verdict with an open thread task and the
// tables fire on their own assertions.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - Decide returns `attached` for an inquiry verdict with an open thread task
//     -> InquiryOpenTaskBecomesItsOwnTask, DecideTableByLaneAndThreadTask.
//   - RelatedTaskID set on a personal attach, or on an inquiry create with no
//     thread task -> RelatedTaskIDIsSetOnTheInquiryCreateOnly.
//   - keep C-D13 gating an OPEN claude thread task -> ClaudeGateNarrowsToTheAttachPath.
//   - loosen GateAnswered -> TheOtherSixGatesAreUnchanged.

import (
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/promote"
)

var inquiryAskKindsUnderTest = []string{"question", "request", "decision", "scheduling"}

// ---- criterion 13: Decide's table -------------------------------------------------

// The trigger's own shape: an inquiry verdict on a thread whose OPEN task is a
// ticket task. Today it attaches a log line nobody sees; D11 makes it a task.
func TestDecide_InquiryOpenTaskBecomesItsOwnTask(t *testing.T) {
	for _, k := range inquiryAskKindsUnderTest {
		for _, st := range []string{"ready", "holding", "blocked", "in_progress", "needs_feedback", "done_locally"} {
			v := promote.Verdict{Lane: promote.LaneInquiry, Kind: k}
			got := promote.Decide(v, &promote.ExistingTask{ID: 452, Status: st, AssigneeType: "human"})
			if got.Action == "attached" || got.TaskID != 0 {
				t.Errorf("Decide(inquiry %s, open %s task 452) = %+v; D11: an ask is ALWAYS its own task — "+
					"\"those messages to create it's own tasks\" (owner, 2026-09-22)", k, st, got)
				continue
			}
			if got.RelatedTaskID != 452 {
				t.Errorf("Decide(inquiry %s, open %s task 452) = %+v, want RelatedTaskID 452: the light pointer "+
					"both ways (D11)", k, st, got)
			}
			if got.ReopenDismissalID != 0 {
				t.Errorf("Decide(inquiry %s, open %s task) = %+v, want no reopen: nothing was dismissed", k, st, got)
			}
			// The create itself is the EXISTING one: inquiryCreateStatus and its
			// action, never the personal whitelist.
			plain := promote.Decide(v, nil)
			if got.Action != plain.Action || got.Status != plain.Status {
				t.Errorf("Decide(inquiry %s, open task) = {%s %s}, want the SAME create as with no task at all "+
					"({%s %s}); D11 changes WHAT a passing verdict becomes, not the create's status (criterion 13)",
					k, got.Action, got.Status, plain.Action, plain.Status)
			}
		}
	}
}

// Criterion 13's full table: {lane} x {no task, open task, dismissed task,
// finished task} x {whitelisted kind, other}. The PERSONAL lane is
// byte-unchanged — D11 touches the inquiry lane only.
func TestDecide_DecideTableByLaneAndThreadTask(t *testing.T) {
	const openID, dismissedID, finishedID = 452, 464, 470
	openTask := func() *promote.ExistingTask {
		return &promote.ExistingTask{ID: openID, Status: "ready", AssigneeType: "human"}
	}
	dismissedTask := func() *promote.ExistingTask {
		return &promote.ExistingTask{ID: dismissedID, Status: "closed", DismissalID: 9,
			DismissalCode: "not_actionable", AssigneeType: "human"}
	}
	finishedTask := func() *promote.ExistingTask {
		return &promote.ExistingTask{ID: finishedID, Status: "delivered", AssigneeType: "human"}
	}

	for _, lane := range []promote.Lane{promote.LanePersonal, promote.LaneInquiry} {
		for _, kind := range []string{"payment_due", "question", "newsletter"} {
			lane, kind := lane, kind
			v := promote.Verdict{Lane: lane, Kind: kind}
			inquiry := lane == promote.LaneInquiry
			create := promote.Decide(promote.Verdict{Lane: lane, Kind: kind}, nil) // the lane's own create

			t.Run(string(lane)+"/"+kind+"/no task", func(t *testing.T) {
				if create.TaskID != 0 || create.RelatedTaskID != 0 || create.ReopenDismissalID != 0 {
					t.Errorf("Decide(%s %s, nil) = %+v, want a plain create with no pointers", lane, kind, create)
				}
			})

			t.Run(string(lane)+"/"+kind+"/open task", func(t *testing.T) {
				got := promote.Decide(v, openTask())
				if !inquiry {
					// Q3, byte-unchanged for the personal lane.
					if got.Action != "attached" || got.TaskID != openID || got.Status != "" || got.RelatedTaskID != 0 {
						t.Errorf("Decide(personal %s, open task) = %+v, want a plain attach to %d with no "+
							"RelatedTaskID — D11 keeps rules 1-4 byte-identical for the personal lane", kind, got, openID)
					}
					return
				}
				want := promote.Decision{Action: create.Action, Status: create.Status, RelatedTaskID: openID}
				if got != want {
					t.Errorf("Decide(inquiry %s, open task) = %+v, want %+v (criterion 13)", kind, got, want)
				}
			})

			t.Run(string(lane)+"/"+kind+"/dismissed task", func(t *testing.T) {
				got := promote.Decide(v, dismissedTask())
				want := promote.Decision{Action: "attached", TaskID: dismissedID, ReopenDismissalID: 9}
				if got != want {
					t.Errorf("Decide(%s %s, dismissed task) = %+v, want %+v. D11 deliberately does NOT touch "+
						"rule 2: a dismissed task coming back is VISIBLE (SWT-36's reopen, the board's "+
						"`reopened after dismissal` marker) — it is not a silent pile-on", lane, kind, got, want)
				}
			})

			t.Run(string(lane)+"/"+kind+"/finished task", func(t *testing.T) {
				got := promote.Decide(v, finishedTask())
				if got != create {
					t.Errorf("Decide(%s %s, finished task) = %+v, want the lane's plain create %+v (Q3, unchanged)",
						lane, kind, got, create)
				}
				if got.RelatedTaskID != 0 {
					t.Errorf("Decide(%s %s, finished task) set RelatedTaskID %d; the pointer names the thread's "+
						"OPEN task, and there is none", lane, kind, got.RelatedTaskID)
				}
			})
		}
	}
}

// D11: RelatedTaskID is set on the inquiry CREATE and nowhere else — a pointer
// on an attach or on a personal create would put `related_task: N` into a body
// contract that never asked for it.
func TestDecide_RelatedTaskIDIsSetOnTheInquiryCreateOnly(t *testing.T) {
	cases := []struct {
		name     string
		v        promote.Verdict
		existing *promote.ExistingTask
		want     int64
	}{
		{"inquiry + open task", promote.Verdict{Lane: promote.LaneInquiry, Kind: "question"},
			&promote.ExistingTask{ID: 452, Status: "ready", AssigneeType: "human"}, 452},
		{"inquiry + open CLAUDE task", promote.Verdict{Lane: promote.LaneInquiry, Kind: "question"},
			&promote.ExistingTask{ID: 452, Status: "ready", AssigneeType: "claude"}, 452},
		{"inquiry + nothing", promote.Verdict{Lane: promote.LaneInquiry, Kind: "question"}, nil, 0},
		{"inquiry + dismissed", promote.Verdict{Lane: promote.LaneInquiry, Kind: "question"},
			&promote.ExistingTask{ID: 464, Status: "closed", DismissalID: 3, AssigneeType: "human"}, 0},
		{"personal + open task", promote.Verdict{Kind: "payment_due"},
			&promote.ExistingTask{ID: 452, Status: "ready", AssigneeType: "human"}, 0},
		{"personal + nothing", promote.Verdict{Kind: "payment_due"}, nil, 0},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := promote.Decide(tc.v, tc.existing).RelatedTaskID; got != tc.want {
				t.Errorf("Decide(%s).RelatedTaskID = %d, want %d (D11)", tc.name, got, tc.want)
			}
		})
	}
}

// Decide stays PURE and TOTAL: the same inputs give the same answer, and no
// input mutates the caller's ExistingTask (invariant 7).
func TestDecide_StaysPure(t *testing.T) {
	existing := promote.ExistingTask{ID: 452, Status: "ready", AssigneeType: "human"}
	before := existing
	v := promote.Verdict{Lane: promote.LaneInquiry, Kind: "question"}
	first := promote.Decide(v, &existing)
	second := promote.Decide(v, &existing)
	if first != second {
		t.Errorf("Decide is not deterministic: %+v then %+v", first, second)
	}
	if existing != before {
		t.Errorf("Decide mutated its ExistingTask argument: %+v -> %+v", before, existing)
	}
}

// ---- criterion 16: C-D13 narrows, it does not disappear ---------------------------

// C-D13 existed solely because an attach would log untrusted text onto a claude
// task, and "creating a second task instead would break Q3". Q3 no longer
// applies to asks, so gating an ask because the thread's task belongs to a
// worker would keep the black hole open for exactly the messages this ticket is
// about. The gate now refuses only when the thread task is not a human's AND
// Decide would ATTACH to it — rule 2's dismissed path.
func TestInquiryGate_ClaudeGateNarrowsToTheAttachPath(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	base := func(task *promote.ExistingTask) promote.InquiryCandidate {
		return promote.InquiryCandidate{
			AskKind: "question", Channel: "gmail", ThreadKey: "gmail:itest", ThreadScope: "thread",
			StoredThreadID: 7, CurrentThreadID: 7, SentAt: now.Add(-2 * time.Hour), ThreadTask: task,
		}
	}
	// CONTROL: with no thread task the verdict passes every gate, so a refusal
	// below is C-D13's doing and not the fixture's.
	if got := promote.InquiryGate(base(nil), now); got != "" {
		t.Fatalf("CONTROL: a verdict with no thread task is gated %q; this fixture cannot isolate C-D13", got)
	}

	for _, tc := range []struct {
		name string
		task *promote.ExistingTask
		want string
	}{
		{"open CLAUDE task: NOT gated any more",
			&promote.ExistingTask{ID: 452, Status: "ready", AssigneeType: "claude"}, ""},
		{"open task with an EMPTY assignee: NOT gated any more",
			&promote.ExistingTask{ID: 452, Status: "ready", AssigneeType: ""}, ""},
		{"open human task",
			&promote.ExistingTask{ID: 452, Status: "ready", AssigneeType: "human"}, ""},
		{"DISMISSED claude task: still gated (Decide would attach + reopen)",
			&promote.ExistingTask{ID: 464, Status: "closed", DismissalID: 5, AssigneeType: "claude"},
			promote.GateClaudeTask},
		{"DISMISSED task with an empty assignee: still gated, fail closed",
			&promote.ExistingTask{ID: 464, Status: "closed", DismissalID: 5, AssigneeType: ""},
			promote.GateClaudeTask},
		{"DISMISSED human task: not gated",
			&promote.ExistingTask{ID: 464, Status: "closed", DismissalID: 5, AssigneeType: "human"}, ""},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := promote.InquiryGate(base(tc.task), now)
			if got != tc.want {
				t.Errorf("InquiryGate(thread task %+v) = %q, want %q. Criterion 16: the gate refuses only when "+
					"the task is not a human's AND Decide would ATTACH to it (rule 2's dismissed path) — an OPEN "+
					"claude task now yields the ask its OWN human task", tc.task, got, tc.want)
			}
		})
	}
}

// Criterion 17 / D11: "This ticket widens what a passing verdict BECOMES; it
// does not widen what passes." GateAnswered — the replied-since fold that keeps
// an ask he answered in Slack within minutes from becoming a task at all — and
// the other five C3 clauses are untouched, and they all still come BEFORE
// claude_task in C3's order.
func TestInquiryGate_TheOtherSixGatesAreUnchanged(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	ok := promote.InquiryCandidate{
		AskKind: "question", Channel: "gmail", ThreadKey: "gmail:itest", ThreadScope: "thread",
		StoredThreadID: 7, CurrentThreadID: 7, SentAt: now.Add(-2 * time.Hour),
	}
	if got := promote.InquiryGate(ok, now); got != "" {
		t.Fatalf("CONTROL: the passing candidate is gated %q", got)
	}
	// Each clause alone, with an OPEN CLAUDE thread task attached — the shape
	// criterion 16 stopped gating. The FIRST failing reason must still be the
	// C3 one, never claude_task and never "".
	claude := &promote.ExistingTask{ID: 452, Status: "ready", AssigneeType: "claude"}
	for _, tc := range []struct {
		name string
		fix  func(c *promote.InquiryCandidate)
		want string
	}{
		{"rethreaded", func(c *promote.InquiryCandidate) { c.CurrentThreadID = 8 }, promote.GateRethreaded},
		{"kind", func(c *promote.InquiryCandidate) { c.AskKind = "fyi" }, promote.GateKind},
		{"stale", func(c *promote.InquiryCandidate) { c.SentAt = now.Add(-100 * time.Hour) }, promote.GateStale},
		{"pending (clock skew)", func(c *promote.InquiryCandidate) { c.SentAt = now.Add(time.Hour) }, promote.GatePending},
		{"answered", func(c *promote.InquiryCandidate) { c.RepliedSince = true }, promote.GateAnswered},
		{"not_addressed", func(c *promote.InquiryCandidate) {
			c.Channel = "slack"
			c.ThreadKey = "slack:T0:C0"
			c.ThreadScope = "conversation"
			c.PriorPost = false
		}, promote.GateNotAddressed},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c := ok
			c.ThreadTask = claude
			tc.fix(&c)
			if got := promote.InquiryGate(c, now); got != tc.want {
				t.Errorf("InquiryGate(%s, open claude thread task) = %q, want %q — criterion 17: the six C3 "+
					"clauses and their ORDER are unchanged, and claude_task stays LAST", tc.name, got, tc.want)
			}
		})
	}
	// C3's order is still the declared one, claude_task last.
	want := []string{promote.GateRethreaded, promote.GateKind, promote.GateStale, promote.GatePending,
		promote.GateAnswered, promote.GateNotAddressed, promote.GateClaudeTask}
	got := promote.InquiryGateReasons()
	if len(got) != len(want) {
		t.Fatalf("InquiryGateReasons() = %v, want %v (criterion 16: the reason set is unchanged)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("InquiryGateReasons() = %v, want %v", got, want)
		}
	}
}
