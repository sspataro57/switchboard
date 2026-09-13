package promote_test

// SWT-40 Part C, the PURE half (docs/tickets/inquiry-promote_SPEC.md, C-D3,
// C-D5, C-D6, C-D7, C-D8, C-D10, C-D12; criteria C3, C7, C14): the inquiry
// gate, Decide's inquiry-lane branches and the dismissal-outcome readout are
// functions of their inputs with ZERO I/O (invariant 7's discipline, the
// promote_test.go shape). This file imports testing, time and the package —
// inquiry_structure_test.go parses its import block and fails on anything else.
//
// ---- IMPOSED SURFACE (internal/promote/inquiry.go, outcomes.go) --------------
//
//	type Lane string
//	const LanePersonal Lane = "personal"  // Config.Lane's zero value means personal (C1)
//	const LaneInquiry  Lane = "inquiry"
//	const InquiryActor = "promote:inquiry"       // every inquiry-lane executor call
//	const InquiryMaxAge = 72 * time.Hour         // C-D6's second fence
//	const InquiryGrace  = time.Hour              // C-D6's grace
//
//	// Verdict gains Lane; on the inquiry lane Kind carries ask_kind.
//	type Verdict struct { Lane Lane; Kind string; ... }
//	// Decide: attach-open -> attach+reopen dismissed -> create. On the inquiry
//	// lane create uses inquiryCreateStatus ("holding" under O7 -> action
//	// "review"; "ready" after the flip -> action "task"); the personal whitelist
//	// never applies to it.
//
//	// InquiryCandidate is what the gate reads: stored verdict facts plus the two
//	// replyfold columns and the current thread.
//	type InquiryCandidate struct {
//	    AskKind         string        // fields.ask_kind
//	    Channel         string        // fields.channel
//	    ThreadKey       string        // fields.thread_key (stored verbatim)
//	    ThreadScope     string        // fields.thread_scope
//	    StoredThreadID  int64         // fields.thread_id (0 = none)
//	    CurrentThreadID int64         // normalized_messages.thread_id now (0 = none)
//	    SentAt          time.Time     // normalized_messages.sent_at
//	    RepliedSince    bool          // replyfold.RepliedSinceCol
//	    PriorPost       bool          // replyfold.PriorParticipationCol
//	    MaxAge          time.Duration // 0 = InquiryMaxAge; only a --dry-run may widen it
//	}
//	// InquiryGate returns "" when the verdict may promote, else the FIRST
//	// failing reason in C3's order: rethreaded, kind, stale, pending, answered,
//	// not_addressed.
//	func InquiryGate(c InquiryCandidate, now time.Time) string
//
//	type Dismissal struct { ReasonCode, ReopenedBy string } // the task's FIRST dismissal
//	// InquiryOutcome: "false_positive" | "true_positive" | "mis_click" | "excluded".
//	func InquiryOutcome(status string, first *Dismissal) string
//	type OutcomeCounts struct { FalsePositive, TruePositive, MisClick, Excluded int }
//	func (c OutcomeCounts) Decided() int // FalsePositive + TruePositive
//
// The reason and outcome VOCABULARY is spelled with literals, as promote_test.go
// spells actions: they are what stats, the dry-run and the readout print.
//
// GREENFIELD NOTE — EXPECTED RED: none of the above exists, so package
// promote_test compile-FAILS ("undefined: promote.InquiryCandidate", ...).

import (
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/promote"
)

var iqNow = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

// iqBase passes every clause: a gmail question on a thread, two hours old,
// unanswered, on the thread it was classified on.
func iqBase() promote.InquiryCandidate {
	return promote.InquiryCandidate{
		AskKind: "question", Channel: "gmail",
		ThreadKey: "gmail:salvador@handsonconnect.org:thread-1", ThreadScope: "thread",
		StoredThreadID: 7, CurrentThreadID: 7,
		SentAt: iqNow.Add(-2 * time.Hour),
	}
}

func TestInquiryConstants(t *testing.T) {
	if promote.InquiryMaxAge != 72*time.Hour {
		t.Errorf("InquiryMaxAge = %v, want 72h (C-D6)", promote.InquiryMaxAge)
	}
	if promote.InquiryGrace != time.Hour {
		t.Errorf("InquiryGrace = %v, want 1h (C-D6: the replied-since fold fires first)", promote.InquiryGrace)
	}
	if promote.InquiryActor != "promote:inquiry" {
		t.Errorf("InquiryActor = %q, want promote:inquiry (C-D1)", promote.InquiryActor)
	}
	if promote.Actor != "promote:classify" {
		t.Errorf("Actor = %q; the personal lane's actor must not move (C1: byte-identical)", promote.Actor)
	}
	if promote.LanePersonal != "personal" || promote.LaneInquiry != "inquiry" {
		t.Errorf("lanes = %q/%q, want personal/inquiry (the --lane spellings, E5)", promote.LanePersonal, promote.LaneInquiry)
	}
	var zero promote.Config
	if zero.Lane != "" && zero.Lane != promote.LanePersonal {
		t.Errorf("Config's zero Lane = %q; the zero value must mean personal so every existing caller "+
			"(the classify-promote CronJob, the SWT-30/36 suites) is unchanged (C1)", zero.Lane)
	}
}

// ---- C3: one fixture per reason ---------------------------------------------

func TestInquiryGate_EachReasonAlone(t *testing.T) {
	type mod func(*promote.InquiryCandidate)
	cases := []struct {
		name string
		m    mod
		want string
	}{
		{"the base candidate passes", func(*promote.InquiryCandidate) {}, ""},

		// C-D10: the stored thread no longer matches the message's current one.
		{"rethreaded: moved to another thread", func(c *promote.InquiryCandidate) { c.CurrentThreadID = 8 }, "rethreaded"},
		{"rethreaded: thread removed", func(c *promote.InquiryCandidate) { c.CurrentThreadID = 0 }, "rethreaded"},
		{"rethreaded: thread appeared", func(c *promote.InquiryCandidate) { c.StoredThreadID = 0 }, "rethreaded"},

		// C-D5: the whitelist {question, request, decision, scheduling}.
		{"kind: fyi asks nothing", func(c *promote.InquiryCandidate) { c.AskKind = "fyi" }, "kind"},
		{"kind: empty", func(c *promote.InquiryCandidate) { c.AskKind = "" }, "kind"},
		{"kind: outside the enum", func(c *promote.InquiryCandidate) { c.AskKind = "payment_due" }, "kind"},
		{"kind: request passes", func(c *promote.InquiryCandidate) { c.AskKind = "request" }, ""},
		{"kind: decision passes", func(c *promote.InquiryCandidate) { c.AskKind = "decision" }, ""},
		{"kind: scheduling passes", func(c *promote.InquiryCandidate) { c.AskKind = "scheduling" }, ""},

		// C-D6: max age 72h on sent_at.
		{"stale: 73h old", func(c *promote.InquiryCandidate) { c.SentAt = iqNow.Add(-73 * time.Hour) }, "stale"},
		{"fresh: 71h old", func(c *promote.InquiryCandidate) { c.SentAt = iqNow.Add(-71 * time.Hour) }, ""},
		{"--max-age widens the fence (dry-run only)", func(c *promote.InquiryCandidate) {
			c.MaxAge = 720 * time.Hour
			c.SentAt = iqNow.Add(-100 * time.Hour)
		}, ""},
		{"--max-age is still a fence", func(c *promote.InquiryCandidate) {
			c.MaxAge = 720 * time.Hour
			c.SentAt = iqNow.Add(-721 * time.Hour)
		}, "stale"},

		// C-D6: grace 1h, so the replied-since fold can fire first.
		{"pending: 59m old", func(c *promote.InquiryCandidate) { c.SentAt = iqNow.Add(-59 * time.Minute) }, "pending"},
		{"released: 61m old", func(c *promote.InquiryCandidate) { c.SentAt = iqNow.Add(-61 * time.Minute) }, ""},
		{"pending: sent in the future (clock skew)", func(c *promote.InquiryCandidate) { c.SentAt = iqNow.Add(time.Minute) }, "pending"},

		// C-D7: ANY replied-since state blocks promotion.
		{"answered in thread", func(c *promote.InquiryCandidate) { c.RepliedSince = true }, "answered"},
		{"spoke in the DM since", func(c *promote.InquiryCandidate) {
			c.Channel, c.ThreadKey, c.ThreadScope = "slack", "slack:T0360B84U:D01EJRX6P45", "conversation"
			c.RepliedSince = true
		}, "answered"},

		// C-D3: addressed = gmail OR a 1:1 DM OR (thread scope AND he posted before).
		{"gmail is addressed without a prior post", func(c *promote.InquiryCandidate) { c.PriorPost = false }, ""},
		{"slack DM is addressed", func(c *promote.InquiryCandidate) {
			c.Channel, c.ThreadKey, c.ThreadScope = "slack", "slack:T0360B84U:D01EJRX6P45", "conversation"
		}, ""},
		{"slack thread inside a DM is addressed", func(c *promote.InquiryCandidate) {
			c.Channel, c.ThreadKey, c.ThreadScope = "slack", "slack:T0360B84U:D01EJRX6P45:p1757000000000100", "thread"
		}, ""},
		{"top-level channel message is not addressed", func(c *promote.InquiryCandidate) {
			c.Channel, c.ThreadKey, c.ThreadScope = "slack", "slack:T0HPR78RX:C07ABCDEF", "conversation"
		}, "not_addressed"},
		{"top-level channel message is not addressed EVEN after he spoke there (conversation scope)",
			func(c *promote.InquiryCandidate) {
				c.Channel, c.ThreadKey, c.ThreadScope = "slack", "slack:T0HPR78RX:C07ABCDEF", "conversation"
				c.PriorPost = true
			}, "not_addressed"},
		{"channel thread he never posted in is not addressed", func(c *promote.InquiryCandidate) {
			c.Channel, c.ThreadKey, c.ThreadScope = "slack", "slack:T0HPR78RX:C07ABCDEF:p1757000000000100", "thread"
		}, "not_addressed"},
		{"channel thread he posted in before is addressed", func(c *promote.InquiryCandidate) {
			c.Channel, c.ThreadKey, c.ThreadScope = "slack", "slack:T0HPR78RX:C07ABCDEF:p1757000000000100", "thread"
			c.PriorPost = true
		}, ""},
		{"group DM is not addressed (C-D4 excludes it)", func(c *promote.InquiryCandidate) {
			c.Channel, c.ThreadKey, c.ThreadScope = "slack", "slack:T0HPR78RX:G01GROUPDM", "conversation"
		}, "not_addressed"},
		{"jira thread without a prior post is not addressed", func(c *promote.InquiryCandidate) {
			c.Channel, c.ThreadKey, c.ThreadScope = "jira", "jira:avviato.atlassian.net:LHH-1", "thread"
		}, "not_addressed"},
		{"jira thread with a prior post is addressed", func(c *promote.InquiryCandidate) {
			c.Channel, c.ThreadKey, c.ThreadScope = "jira", "jira:avviato.atlassian.net:LHH-1", "thread"
			c.PriorPost = true
		}, ""},

		// C-D13: never attach to or reopen a non-human thread task.
		{"claude_task: the thread's open task is claude's", func(c *promote.InquiryCandidate) {
			c.ThreadTask = &promote.ExistingTask{ID: 5, Status: "ready", AssigneeType: "claude"}
		}, "claude_task"},
		{"claude_task: the thread's dismissed task is claude's", func(c *promote.InquiryCandidate) {
			c.ThreadTask = &promote.ExistingTask{ID: 5, Status: "closed", DismissalID: 9, AssigneeType: "claude"}
		}, "claude_task"},
		{"claude_task: an empty assignee reads as not human (fail closed)", func(c *promote.InquiryCandidate) {
			c.ThreadTask = &promote.ExistingTask{ID: 5, Status: "ready"}
		}, "claude_task"},
		{"a human thread task passes", func(c *promote.InquiryCandidate) {
			c.ThreadTask = &promote.ExistingTask{ID: 5, Status: "ready", AssigneeType: "human"}
		}, ""},
	}
	for _, tc := range cases {
		c := iqBase()
		tc.m(&c)
		if got := promote.InquiryGate(c, iqNow); got != tc.want {
			t.Errorf("%s: InquiryGate(%+v) = %q, want %q", tc.name, c, got, tc.want)
		}
	}
}

// One reason per gated verdict, the FIRST in C3's listed order, so the stats
// and the dry-run count every gated verdict exactly once. Walked as a ladder:
// start with every clause failing, fix one at a time, and the next reason in
// the order must surface.
func TestInquiryGate_ReportsTheFirstReasonInCThreeOrder(t *testing.T) {
	c := promote.InquiryCandidate{
		AskKind: "fyi", Channel: "slack", ThreadKey: "slack:T0HPR78RX:C07ABCDEF", ThreadScope: "conversation",
		StoredThreadID: 7, CurrentThreadID: 8, SentAt: iqNow.Add(-73 * time.Hour), RepliedSince: true,
		ThreadTask: &promote.ExistingTask{ID: 5, Status: "ready", AssigneeType: "claude"},
	}
	steps := []struct {
		want string
		fix  func(*promote.InquiryCandidate)
	}{
		{"rethreaded", func(c *promote.InquiryCandidate) { c.CurrentThreadID = 7 }},
		{"kind", func(c *promote.InquiryCandidate) { c.AskKind = "question" }},
		{"stale", func(c *promote.InquiryCandidate) { c.SentAt = iqNow.Add(-10 * time.Minute) }},
		{"pending", func(c *promote.InquiryCandidate) { c.SentAt = iqNow.Add(-2 * time.Hour) }},
		{"answered", func(c *promote.InquiryCandidate) { c.RepliedSince = false }},
		{"not_addressed", func(c *promote.InquiryCandidate) { c.ThreadKey = "slack:T0HPR78RX:D07PRIVATE" }},
		{"claude_task", func(c *promote.InquiryCandidate) { c.ThreadTask.AssigneeType = "human" }},
		{"", nil},
	}
	for _, s := range steps {
		if got := promote.InquiryGate(c, iqNow); got != s.want {
			t.Fatalf("InquiryGate(%+v) = %q, want %q (C3 order: rethreaded, kind, stale, pending, answered, "+
				"not_addressed, then C-D13's claude_task)", c, got, s.want)
		}
		if s.fix != nil {
			s.fix(&c)
		}
	}
}

// ---- C7's pure half: Decide on the inquiry lane ------------------------------

var inquiryKinds = []string{"question", "request", "decision", "scheduling"}

// O7: a promoted inquiry lands in the Holding column (action 'review'). The
// flip to "ready" (action 'task') is ONE line, inquiryCreateStatus, and it
// edits this assertion and inquiry_internal_test.go's pin in the same diff.
func TestDecide_InquiryCreatesAHoldingReviewTask(t *testing.T) {
	for _, k := range inquiryKinds {
		got := promote.Decide(promote.Verdict{Lane: promote.LaneInquiry, Kind: k}, nil)
		if got.Action != "review" || got.Status != "holding" || got.TaskID != 0 || got.ReopenDismissalID != 0 {
			t.Errorf("Decide(inquiry, kind=%q, nil) = %+v, want {Action:review Status:holding} — O7: Holding first", k, got)
		}
	}
}

// The personal whitelist is the PERSONAL lane's autonomy argument (SWT-30 D2).
// It must not leak into the inquiry lane: an inquiry verdict whose kind string
// happens to be whitelisted still follows inquiryCreateStatus.
func TestDecide_ThePersonalWhitelistNeverAppliesToTheInquiryLane(t *testing.T) {
	for _, k := range []string{"payment_due", "deadline"} {
		got := promote.Decide(promote.Verdict{Lane: promote.LaneInquiry, Kind: k}, nil)
		if got.Action == "task" || got.Status == "ready" {
			t.Errorf("Decide(inquiry, kind=%q) = %+v; the personal whitelist leaked into the inquiry lane", k, got)
		}
	}
	// Control: the zero Lane is still the personal lane, whitelist and all.
	if got := promote.Decide(promote.Verdict{Kind: "payment_due"}, nil); got.Action != "task" || got.Status != "ready" {
		t.Errorf("Decide(personal zero-lane, payment_due) = %+v, want {task ready}: C1 — the personal lane is unchanged", got)
	}
}

func TestDecide_InquiryAttachOrderIsTheExistingOne(t *testing.T) {
	v := promote.Verdict{Lane: promote.LaneInquiry, Kind: "question"}
	// attach-open
	if got := promote.Decide(v, &promote.ExistingTask{ID: 11, Status: "holding"}); got.Action != "attached" ||
		got.TaskID != 11 || got.Status != "" || got.ReopenDismissalID != 0 {
		t.Errorf("inquiry verdict on a thread with an OPEN task: %+v, want a plain attach to 11", got)
	}
	// attach + reopen a dismissed task (SWT-36)
	if got := promote.Decide(v, &promote.ExistingTask{ID: 12, Status: "closed", DismissalID: 5}); got.Action != "attached" ||
		got.TaskID != 12 || got.ReopenDismissalID != 5 || got.Status != "" {
		t.Errorf("inquiry verdict on a DISMISSED task's thread: %+v, want attach to 12 + reopen against dismissal 5", got)
	}
	// Q3: a closed, non-dismissed task -> a new holding task
	for _, st := range []string{"closed", "delivered"} {
		if got := promote.Decide(v, &promote.ExistingTask{ID: 13, Status: st}); got.Action != "review" || got.Status != "holding" {
			t.Errorf("inquiry verdict past a %s task: %+v, want a NEW {review holding} task (Q3)", st, got)
		}
	}
}

// ---- C14: the dismissal-outcome readout --------------------------------------

func TestInquiryOutcome(t *testing.T) {
	d := func(code, by string) *promote.Dismissal { return &promote.Dismissal{ReasonCode: code, ReopenedBy: by} }
	cases := []struct {
		status string
		first  *promote.Dismissal
		want   string
		why    string
	}{
		{"closed", nil, "true_positive", "closed with no dismissal: he worked it"},
		{"delivered", nil, "true_positive", "delivered"},
		{"holding", nil, "excluded", "still open: undecided"},
		{"ready", nil, "excluded", "still open"},
		{"in_progress", nil, "excluded", "still open"},
		{"closed", d("not_actionable", ""), "false_positive", "first dismissal not_actionable"},
		{"closed", d("wrong_kind", ""), "false_positive", "first dismissal wrong_kind"},
		{"closed", d("handled_elsewhere", ""), "true_positive", "a real ask, answered elsewhere"},
		{"closed", d("duplicate", ""), "excluded", "duplicate says nothing about the ask"},
		{"closed", d("some_future_code", ""), "excluded", "an unknown code is never a label"},
		{"holding", d("not_actionable", "promote:inquiry"), "false_positive",
			"an ACTIVITY reopen (non-human actor) does not undo the label"},
		{"holding", d("not_actionable", "capture:slackweb"), "false_positive", "activity reopen via capture"},
		{"delivered", d("wrong_kind", "promote:inquiry"), "false_positive", "the FIRST dismissal decides even if it was later delivered"},
		{"holding", d("not_actionable", "dashboard:salvo"), "mis_click", "plain-reopened by a HUMAN (policy.HumanActor)"},
		{"holding", d("handled_elsewhere", "opsctl:salvo"), "mis_click", "a human undid the dismissal"},
		{"holding", d("wrong_kind", "mcp:manual:salvo"), "mis_click", "HumanActor strips the mcp: transport prefix"},
	}
	for _, tc := range cases {
		if got := promote.InquiryOutcome(tc.status, tc.first); got != tc.want {
			t.Errorf("InquiryOutcome(%q, %+v) = %q, want %q — %s", tc.status, tc.first, got, tc.want, tc.why)
		}
	}
}

func TestOutcomeCounts_DecidedIsFalsePlusTruePositives(t *testing.T) {
	c := promote.OutcomeCounts{FalsePositive: 3, TruePositive: 4, MisClick: 5, Excluded: 6}
	if got := c.Decided(); got != 7 {
		t.Errorf("Decided() = %d, want 7: mis-clicks and exclusions are not labels (C-D12)", got)
	}
}
