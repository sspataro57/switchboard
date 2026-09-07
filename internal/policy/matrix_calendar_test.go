package policy_test

// Unit tests for the calendar AUTO tier in the delivery policy matrix
// (SWT-28 / docs/tickets/calendar-booking_SPEC.md, acceptance criteria 14-17;
// Q1 = (b) in docs/tickets/calendar-booking_OPEN_QUESTIONS.md). Pure functions
// of (Request, Snapshot), ZERO I/O — invariant 7's reason for existing, and the
// reason these can be the FIRST thing that goes green.
//
// WHY THIS FILE IS THE LOAD-BEARING ONE. Every other delivery channel in this
// repo is gated by policy.humanOnly, so a mistake in the matrix still leaves a
// person between an agent and a client. `book_calendar_block` is deliberately
// NOT human-only (Q1's answer, taken with the prompt-injection exposure read
// and accepted), so the matrix IS the gate: the channel guard, the kill switch
// and the hourly limit are what remain, plus the LoadBusy refusal in the
// handler. If one of these cases is wrong, an injected worker call reaches a
// real Google calendar.
//
// GREENFIELD NOTE: internal/policy/matrix.go has no `case "calendar":`, no
// `channel_mismatch` rule, and does not know the tool name, so every test here
// FAILS today — calendar falls into the `channel_not_live` default
// (matrix.go:159-162) and `book_calendar_block`, not being sendShaped, returns
// allow/matrix-human before any channel is consulted (matrix.go:108). That
// second failure is exactly the hole criterion 15 closes.
//
// IMPLEMENTER NOTE, and it is a REGRESSION, not a new test: matrix_test.go's
// TestDecide_NotLiveChannels_DeniedNotLive still lists "calendar" among the
// not-live channels. Criterion 14 graduates it, so that list must lose
// "calendar" (leaving only "github_review") in the same change — the jira
// graduation did the same thing in SWT-9. This file deliberately does not edit
// it; the contradiction is the point of a failing test.

import (
	"testing"

	"github.com/sspataro57/switchboard/internal/policy"
)

// calendarSnap is the calendar channel's snapshot: rate-limited like gmail and
// jira (criterion 14).
func calendarSnap(sentThisHour int, frozen bool) policy.Snapshot {
	return policy.Snapshot{
		SendingFrozen: frozen,
		SentLastHour:  map[string]int{"calendar": sentThisHour},
		Channel:       "calendar",
		HourlyLimit:   10,
	}
}

// otherChannels is the FULL set of legal deliveries.channel values other than
// calendar (migration 0001:193 as re-stated by 0009:7-8), plus the empty
// string — the snapshot a delivery_id that resolved to nothing produces
// (pgloader.go:33-38 returns a zero Snapshot rather than erroring).
//
// The whole list is driven on purpose. Premise 14 of the SPEC: once the channel
// switch is reached, every branch allows any sendShaped tool it does not deny
// by name. A guard written for gmail alone would leave jira, slack and upwork
// open, and a guard written as "not calendar" but placed INSIDE the switch
// would leave the default branch's channels open. Enumerate, don't sample.
var otherChannels = []string{"gmail", "jira_comment", "upwork_chat", "slack_reply", "github_review", ""}

// ---------------------------------------------------------------------------
// Criterion 14: the calendar branch. Both verbs allowed within limit, both
// rate-limited, both stopped by the kill switch.
// ---------------------------------------------------------------------------

func TestDecide_Calendar_AutoTierAllowsBothVerbs(t *testing.T) {
	for _, tool := range []string{"send_delivery", "book_calendar_block"} {
		tool := tool
		t.Run(tool+"/human under limit -> allow", func(t *testing.T) {
			d := policy.Decide(policy.Request{Tool: tool, Actor: humanActor}, calendarSnap(2, false))
			if d.Decision != "allow" {
				t.Fatalf("%s on calendar (human, 2/10, not frozen) = %q/%q/%q, want allow. Criterion 14 "+
					"graduates calendar out of channel_not_live; CLAUDE.md's matrix has said auto for own "+
					"blocks since day one", tool, d.Decision, d.Rule, d.Reason)
			}
			if d.Rule == "channel_not_live" {
				t.Fatalf("%s on calendar still carries channel_not_live; the branch did not land", tool)
			}
			if d.Rule == "" {
				t.Errorf("%s allow must record which rule allowed it (empty Rule)", tool)
			}
		})
		t.Run(tool+"/at limit -> rate_limit", func(t *testing.T) {
			assertDeny(t, policy.Decide(policy.Request{Tool: tool, Actor: humanActor}, calendarSnap(10, false)), "rate_limit")
		})
		t.Run(tool+"/over limit -> rate_limit", func(t *testing.T) {
			assertDeny(t, policy.Decide(policy.Request{Tool: tool, Actor: humanActor}, calendarSnap(11, false)), "rate_limit")
		})
		t.Run(tool+"/frozen -> kill_switch", func(t *testing.T) {
			// The operator's stop button for an unattended booker. Criterion 17
			// puts book_calendar_block in freezeGated for exactly this: with no
			// human gate, set_sending_frozen is the only thing that can halt a
			// worker that has decided to book.
			assertDeny(t, policy.Decide(policy.Request{Tool: tool, Actor: humanActor}, calendarSnap(0, true)), "kill_switch")
		})
	}
}

// Criterion 14, the limit's default: HourlyLimit 0 means "unset", and the
// matrix falls back to 10 rather than to "no limit". A zero that read as
// unlimited would remove the auto tier's only volume brake.
func TestDecide_Calendar_UnsetHourlyLimitFallsBackToTen(t *testing.T) {
	snap := policy.Snapshot{SentLastHour: map[string]int{"calendar": 10}, Channel: "calendar", HourlyLimit: 0}
	assertDeny(t, policy.Decide(policy.Request{Tool: "book_calendar_block", Actor: humanActor}, snap), "rate_limit")

	snap.SentLastHour = map[string]int{"calendar": 9}
	if d := policy.Decide(policy.Request{Tool: "book_calendar_block", Actor: humanActor}, snap); d.Decision != "allow" {
		t.Errorf("9 bookings this hour with HourlyLimit unset = %q/%q, want allow (default 10)", d.Decision, d.Rule)
	}
}

// ---------------------------------------------------------------------------
// Criterion 15: book_calendar_block is DENIED BY NAME on every other channel,
// with its own rule string, BEFORE the channel switch.
//
// THIS IS THE HOLE THE SPEC NAMES (premise 14). policy.Decide's channel switch
// allows any sendShaped tool once a live branch is reached, so the moment
// book_calendar_block becomes sendShaped it is allowed on a GMAIL row — and the
// handler's own channel check would be the only thing between an agent and an
// unapproved client email. Two gates; this is the outer, pure, unit-testable one.
// ---------------------------------------------------------------------------

func TestDecide_BookCalendarBlock_DeniedOnEveryOtherChannel(t *testing.T) {
	for _, ch := range otherChannels {
		ch := ch
		name := ch
		if name == "" {
			name = "(empty channel: a delivery_id that resolved to nothing)"
		}
		t.Run(name, func(t *testing.T) {
			snap := policy.Snapshot{SentLastHour: map[string]int{}, Channel: ch, HourlyLimit: 10}
			d := policy.Decide(policy.Request{Tool: "book_calendar_block", Actor: humanActor}, snap)
			if d.Decision == "allow" {
				t.Fatalf("book_calendar_block on channel %q = allow/%q. Criterion 15: it is denied by NAME on "+
					"every channel but calendar. Once the verb is sendShaped, the %q branch allows any "+
					"sendShaped tool it does not deny explicitly — and this verb approves AND sends in one "+
					"call, so an allow here is an agent sending an unapproved client message", ch, d.Rule, ch)
			}
			if d.Rule != "channel_mismatch" {
				t.Errorf("book_calendar_block on channel %q denied with rule %q, want \"channel_mismatch\". "+
					"The rule string is the contract: it is what audit_events/policy_decisions record and "+
					"what an operator greps for, and it must be distinguishable from channel_assisted "+
					"(upwork's tier) and channel_not_live (github's)", ch, d.Rule)
			}
			if d.Reason == "" {
				t.Errorf("channel_mismatch deny for %q must carry a non-empty reason", ch)
			}
		})
	}
}

// The guard must not be reachable-around by a non-human actor either: this verb
// exists precisely so a worker can call it, so the mismatch deny is the ONLY
// thing standing in that path.
func TestDecide_BookCalendarBlock_ChannelGuardHoldsForAutomatedActors(t *testing.T) {
	for _, actor := range []string{botActor, workerMCPActor, "capture:google", "worker:acme"} {
		actor := actor
		t.Run(actor, func(t *testing.T) {
			snap := policy.Snapshot{SentLastHour: map[string]int{}, Channel: "gmail", HourlyLimit: 10}
			d := policy.Decide(policy.Request{Tool: "book_calendar_block", Actor: actor}, snap)
			if d.Decision == "allow" {
				t.Fatalf("book_calendar_block on a gmail row by %q = allow/%q — a worker sending an "+
					"unapproved client email through the calendar verb", actor, d.Rule)
			}
			if d.Rule != "channel_mismatch" {
				t.Errorf("rule = %q for actor %q, want channel_mismatch (NOT human_only: criterion 16 keeps "+
					"this verb out of humanOnly, so an actor-keyed denial here would be the right answer for "+
					"the wrong reason and would evaporate the moment the caller changes)", d.Rule, actor)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Criterion 16: book_calendar_block is NOT in policy.humanOnly, and humanOnly
// is not otherwise changed.
//
// EIGHT actor shapes, not one. IK, "An actor-prefix check is a transport label,
// not a trust boundary": the recorded defect in this repo was a gate that keyed
// on the caller, shipped with a test that pinned a single shape. The mirror
// image is what is being asserted here — the verb must work for EVERY caller,
// including the automated ones, because "a Claude Code worker can book its own
// focus time with no human in the loop" is the ticket's central claim.
// ---------------------------------------------------------------------------

// bookActorShapes is every actor shape this repo actually produces (IK names
// six; capture:{connector} and drafts:gpt are the two non-transport callers
// that reach the executor directly).
var bookActorShapes = []string{
	"dashboard:salvo@example.com",
	"opsctl:salvo",
	"manual:salvo",
	"mcp:manual:salvo",
	"mcp:worker:acme",
	"worker:acme",
	"drafts:gpt",
	"capture:google",
}

func TestDecide_BookCalendarBlock_IsNotHumanOnly(t *testing.T) {
	for _, actor := range bookActorShapes {
		actor := actor
		t.Run(actor, func(t *testing.T) {
			d := policy.Decide(policy.Request{Tool: "book_calendar_block", Actor: actor}, calendarSnap(0, false))
			if d.Rule == "human_only" {
				t.Fatalf("book_calendar_block by %q = deny/human_only. Q1 answered (b): the auto tier ships "+
					"NOW, and widening policy.humanOnly was explicitly off the table — a new verb outside it "+
					"is the whole shape of the answer. A human_only deny here means the verb was added to "+
					"humanOnly instead", actor)
			}
			if d.Decision != "allow" {
				t.Fatalf("book_calendar_block by %q on a clean calendar snapshot = %q/%q/%q, want allow. "+
					"The gates on this verb are the channel, the kill switch, the rate limit, "+
					"calendar_write_enabled and the LoadBusy refusal — none of them the actor",
					actor, d.Decision, d.Rule, d.Reason)
			}
			// ...and it reached the CALENDAR BRANCH to get there. "matrix-human"
			// is Decide's not-sendShaped fallthrough (matrix.go:108,
			// Reason "human delivery action"), which allows a verb BEFORE the
			// channel switch — i.e. with no rate limit, no kill switch and no
			// channel test at all. An allow with that rule is the state of the
			// repo today and is the opposite of criterion 17.
			if d.Rule == "matrix-human" {
				t.Fatalf("book_calendar_block by %q = allow/matrix-human — the not-sendShaped fallthrough. "+
					"Criterion 17 makes the verb sendShaped AND freezeGated, so it must reach the channel "+
					"branch: allowed here means allowed with no hourly limit and no kill switch, which is "+
					"the auto tier with both of its brakes missing", actor)
			}
		})
	}
}

// The other half of criterion 16: humanOnly is NOT otherwise changed. The
// shared human gate protects gmail, jira and slack; a widening done to make
// this ticket's verb work would be invisible here unless it is pinned.
func TestDecide_Calendar_HumanOnlyGateUnchangedForEveryOtherVerb(t *testing.T) {
	for _, tool := range []string{
		"update_delivery", "approve_delivery", "send_delivery",
		"mark_delivery_sent", "mark_delivery_failed", "prefill_delivery", "set_sending_frozen",
	} {
		tool := tool
		t.Run(tool+"/bot on a calendar row still denied", func(t *testing.T) {
			assertDeny(t, policy.Decide(policy.Request{Tool: tool, Actor: botActor}, calendarSnap(0, false)), "human_only")
		})
		t.Run(tool+"/worker over MCP on a calendar row still denied", func(t *testing.T) {
			assertDeny(t, policy.Decide(policy.Request{Tool: tool, Actor: workerMCPActor}, calendarSnap(0, false)), "human_only")
		})
	}
}

// ---------------------------------------------------------------------------
// Criterion 17: send_delivery on a calendar row stays available AND stays
// human-only — the explicit two-step for a human who wants to look first.
// mark_delivery_sent stays refused: this channel has a real send path and a
// reservable id, so it has no assisted tier and no click-may-have-landed window.
// ---------------------------------------------------------------------------

func TestDecide_Calendar_SendDeliveryStaysTheHumanTwoStep(t *testing.T) {
	t.Run("human -> allow", func(t *testing.T) {
		if d := policy.Decide(policy.Request{Tool: "send_delivery", Actor: humanActor}, calendarSnap(0, false)); d.Decision != "allow" {
			t.Fatalf("send_delivery on calendar by a human = %q/%q, want allow (criterion 17: both verbs "+
				"route to the same send half)", d.Decision, d.Rule)
		}
	})
	t.Run("worker over MCP -> deny/human_only", func(t *testing.T) {
		assertDeny(t, policy.Decide(policy.Request{Tool: "send_delivery", Actor: workerMCPActor}, calendarSnap(0, false)), "human_only")
	})
	t.Run("drafts worker -> deny/human_only", func(t *testing.T) {
		assertDeny(t, policy.Decide(policy.Request{Tool: "send_delivery", Actor: botActor}, calendarSnap(0, false)), "human_only")
	})
}

// mark_delivery_sent must not become a second, ungated way to declare a
// calendar row sent: it is not freeze-gated (SWT-12 Q4), so allowing it on this
// channel would hand an operator a path that skips the kill switch on a channel
// that has a real send. The RULE STRING is deliberately not pinned — criterion
// 17 says "refused", not which rule refuses.
func TestDecide_Calendar_MarkDeliverySentStaysRefused(t *testing.T) {
	d := policy.Decide(policy.Request{Tool: "mark_delivery_sent", Actor: humanActor}, calendarSnap(0, false))
	if d.Decision != "deny" {
		t.Errorf("mark_delivery_sent on calendar = %q/%q, want deny. Criterion 17: calendar has a real send "+
			"path and a reservable id, so it has neither an assisted tier nor a click-may-have-landed "+
			"window — and mark_delivery_sent is NOT freeze-gated, so allowing it here would be a route "+
			"around the auto tier's stop button", d.Decision, d.Rule)
	}
}
