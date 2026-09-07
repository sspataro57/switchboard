package tools_test

// Unit tests for draft_delivery's `calendar` channel validation (SWT-28 /
// docs/tickets/calendar-booking_SPEC.md, acceptance criterion 10, and the
// verification protocol's step-1 list). ZERO network, ZERO Postgres: every
// assertion here stops at the executor's VALIDATE stage, which runs before any
// handler dereferences the pool — the tools_unit_test.go /
// delivery_upwork_target_test.go idiom, with a nil *pgxpool.Pool.
//
// WHY VALIDATE AND NOT THE HANDLER. A calendar delivery's interval IS its
// identity: migration 0020's deliveries_calendar_identity_check refuses a
// calendar row without starts_at/ends_at/target_ref/from_account_id, and the
// send path re-derives the busy set over exactly [starts_at, ends_at). A draft
// that gets past validate with a garbled interval either fails at INSERT with a
// constraint name (the good case) or books the wrong span (the bad one). The
// 12-hour cap is a fat-finger guard with a specific victim: a typo'd end DATE
// would blanket the calendar and make propose_slots refuse everything
// downstream, for everyone, until someone noticed.
//
// GREENFIELD NOTE: validateDraftDelivery's channel switch is
// gmail/upwork_chat/jira_comment/slack_reply today (delivery.go:98-101), so
// EVERY case here currently fails with `channel "calendar": must be gmail,
// upwork_chat, jira_comment, or slack_reply` — including the ones that are
// meant to be refused, which is why each case also asserts the refusal is
// about the thing it is testing and not about the channel enum. Imposed arg
// shape (criterion 10):
//
//	type draftDeliveryArgs struct {
//	    ...
//	    Start string `json:"start,omitempty"` // RFC3339
//	    End   string `json:"end,omitempty"`   // RFC3339
//	}

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

func calValidateExecutor(t *testing.T) *executor.Executor {
	t.Helper()
	reg := executor.NewRegistry()
	tools.Register(reg, nil)
	return executor.New(reg, policy.NewStatic(reg.Names()...), audit.NewMemStore())
}

// calBlock returns a 15-minute block tomorrow. RELATIVE to now, never a frozen
// date: a literal 2026-09-08 would drift out of the connector's
// [now-30d, now+90d] horizon and start failing for a reason unrelated to the
// code under test.
func calBlock() (start, end time.Time) {
	start = time.Now().UTC().Add(24 * time.Hour).Truncate(time.Hour)
	return start, start.Add(15 * time.Minute)
}

// calDraftArgs builds a draft_delivery arg blob for the calendar channel.
func calDraftArgs(target, subject, body, start, end string) string {
	return `{"task_id":1,"channel":"calendar","target_ref":` + quoteJSON(target) +
		`,"subject":` + quoteJSON(subject) + `,"body":` + quoteJSON(body) +
		`,"start":` + quoteJSON(start) + `,"end":` + quoteJSON(end) + `}`
}

// assertCalendarValidateRefusal runs the call and asserts it was refused at the
// VALIDATE stage for the reason under test — never because the channel enum
// still rejects "calendar", which would make every case here pass for the wrong
// reason once the enum is the only thing missing.
func assertCalendarValidateRefusal(t *testing.T, args string, wantAnyOf []string) {
	t.Helper()
	_, err := calValidateExecutor(t).Execute(context.Background(), executor.Call{
		Tool: "draft_delivery", Actor: "opsctl:unit", Args: []byte(args),
	})
	if err == nil {
		t.Fatalf("draft_delivery ACCEPTED %s. A calendar row's interval is its identity: migration 0020's "+
			"deliveries_calendar_identity_check refuses the row and the send path books exactly "+
			"[starts_at, ends_at)", args)
	}
	msg := err.Error()
	if strings.Contains(msg, "unknown tool") {
		t.Fatalf("draft_delivery is not registered: %q", msg)
	}
	if strings.Contains(msg, "denied by policy") {
		t.Fatalf("got a policy denial %q; a malformed draft is a VALIDATE failure, not a permissions "+
			"question — draft_delivery is agent-facing and must stay outside the human gate", msg)
	}
	if !strings.Contains(msg, "validate") {
		t.Errorf("error = %q, want a validate-stage failure", msg)
	}
	if strings.Contains(msg, "must be gmail") {
		t.Fatalf("error = %q — the channel enum still rejects \"calendar\". Criterion 10 adds it to "+
			"validateDraftDelivery's switch; until it does, every case in this file fails for the wrong "+
			"reason and none of them is actually testing its own rule", msg)
	}
	for _, want := range wantAnyOf {
		if strings.Contains(strings.ToLower(msg), strings.ToLower(want)) {
			return
		}
	}
	t.Errorf("error = %q; it must name what is wrong (any of %v). draft_delivery is the agent's only route "+
		"to client-visible words and to the calendar, so its refusals are read by a model that has to "+
		"correct itself from the message alone", msg, wantAnyOf)
}

func TestValidateDraftDelivery_CalendarRequiresTargetSubjectBodyAndInterval(t *testing.T) {
	start, end := calBlock()
	s, e := start.Format(time.RFC3339), end.Format(time.RFC3339)

	cases := []struct {
		name      string
		args      string
		wantAnyOf []string
	}{
		{
			// target_ref IS the calendar id (the account email), and criterion
			// 11 resolves the account from it server-side. Without it there is
			// no calendar to book onto and no from_account_id to store.
			name:      "missing target_ref",
			args:      calDraftArgs("", "Focus block", "reserved for review", s, e),
			wantAnyOf: []string{"target_ref"},
		},
		{
			// subject becomes the event SUMMARY — the only thing visible on the
			// calendar. An untitled block is indistinguishable from a glitch.
			name:      "missing subject",
			args:      calDraftArgs("sspataro@example.com", "", "reserved for review", s, e),
			wantAnyOf: []string{"subject", "summary"},
		},
		{
			// body becomes the DESCRIPTION: the recorded reason the block
			// exists. Under the auto tier it is the only explanation a human
			// will find for an event they did not create.
			name:      "missing body",
			args:      calDraftArgs("sspataro@example.com", "Focus block", "", s, e),
			wantAnyOf: []string{"body", "description"},
		},
		{
			name:      "missing start",
			args:      calDraftArgs("sspataro@example.com", "Focus block", "reserved", "", e),
			wantAnyOf: []string{"start"},
		},
		{
			name:      "missing end",
			args:      calDraftArgs("sspataro@example.com", "Focus block", "reserved", s, ""),
			wantAnyOf: []string{"end"},
		},
		{
			name:      "start is not RFC3339",
			args:      calDraftArgs("sspataro@example.com", "Focus block", "reserved", "tomorrow at 3", e),
			wantAnyOf: []string{"start"},
		},
		{
			// A date with no time is the classic near-miss: it parses as a date
			// in some libraries and not in RFC3339, and if it slipped through it
			// would become a midnight-to-midnight block.
			name:      "end is a bare date, not RFC3339",
			args:      calDraftArgs("sspataro@example.com", "Focus block", "reserved", s, end.Format("2006-01-02")),
			wantAnyOf: []string{"end"},
		},
		{
			name: "end before start",
			args: calDraftArgs("sspataro@example.com", "Focus block", "reserved",
				e, s),
			wantAnyOf: []string{"end", "after", "before"},
		},
		{
			name:      "end equal to start (a zero-length block)",
			args:      calDraftArgs("sspataro@example.com", "Focus block", "reserved", s, s),
			wantAnyOf: []string{"end", "after"},
		},
		{
			// The fat-finger guard. 0020's CHECK only demands ends_at > starts_at,
			// so a typo'd end DATE (a year out) satisfies the database and
			// blankets the calendar; propose_slots then refuses every slot for
			// everyone, with no error to point at.
			name: "longer than 12 hours",
			args: calDraftArgs("sspataro@example.com", "Focus block", "reserved",
				s, start.Add(12*time.Hour+time.Minute).Format(time.RFC3339)),
			wantAnyOf: []string{"12", "hour", "duration", "long"},
		},
		{
			name: "a typo'd end DATE a year out",
			args: calDraftArgs("sspataro@example.com", "Focus block", "reserved",
				s, start.AddDate(1, 0, 0).Format(time.RFC3339)),
			wantAnyOf: []string{"12", "hour", "duration", "long"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertCalendarValidateRefusal(t, tc.args, tc.wantAnyOf)
		})
	}
}

// The 12-hour cap's boundary, asserted WITHOUT running the handler (a valid
// draft would reach it and dereference the nil pool): a block of exactly 12
// hours is inside the cap, so a draft that is 12h long and ALSO missing its
// subject must be refused for the SUBJECT. If the refusal names the duration,
// the comparison is >= where criterion 10 says <=.
func TestValidateDraftDelivery_CalendarTwelveHoursExactlyIsInsideTheCap(t *testing.T) {
	start, _ := calBlock()
	args := calDraftArgs("sspataro@example.com", "", "reserved",
		start.Format(time.RFC3339), start.Add(12*time.Hour).Format(time.RFC3339))

	_, err := calValidateExecutor(t).Execute(context.Background(), executor.Call{
		Tool: "draft_delivery", Actor: "opsctl:unit", Args: []byte(args),
	})
	if err == nil {
		t.Fatalf("draft_delivery accepted a subject-less calendar draft; the missing-subject rule is what "+
			"this case leans on: %s", args)
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "must be gmail") {
		t.Fatalf("the channel enum still rejects \"calendar\": %v", err)
	}
	if strings.Contains(msg, "12") || strings.Contains(msg, "duration") {
		t.Errorf("a 12h00m block was refused for its DURATION (%v). Criterion 10 says end - start <= 12h, "+
			"so exactly 12 hours is legal; a >= comparison silently forbids the boundary the SPEC allows", err)
	}
	if !strings.Contains(msg, "subject") && !strings.Contains(msg, "summary") {
		t.Errorf("error = %v, want the missing subject named — that is the only defect in this draft", err)
	}
}

// The channels this ticket does NOT touch keep their own rules: start/end are
// calendar-only fields, so a jira_comment draft carrying them must be refused
// for its OWN defect (no target_ref) and never for the new fields. Kept at the
// validate stage on purpose — a VALID draft would reach the handler and
// dereference the nil pool.
func TestValidateDraftDelivery_StartAndEndAddNoRulesToOtherChannels(t *testing.T) {
	start, end := calBlock()
	args := `{"task_id":1,"channel":"jira_comment","body":"pushed the fix",` +
		`"start":` + quoteJSON(start.Format(time.RFC3339)) + `,"end":` + quoteJSON(end.Format(time.RFC3339)) + `}`

	_, err := calValidateExecutor(t).Execute(context.Background(), executor.Call{
		Tool: "draft_delivery", Actor: "opsctl:unit", Args: []byte(args),
	})
	if err == nil {
		t.Fatalf("a jira_comment draft with no target_ref was accepted: %s", args)
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "target_ref") {
		t.Errorf("error = %v, want the missing target_ref named — that is this draft's only defect, and the "+
			"new start/end fields must add no rule to a channel this ticket does not touch", err)
	}
	if strings.Contains(msg, "start") || strings.Contains(msg, "\"end\"") {
		t.Errorf("error = %v mentions the calendar-only interval fields on a jira_comment draft", err)
	}
}
