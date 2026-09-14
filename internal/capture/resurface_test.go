package capture

// chat-on-closed-task (SWT-53, docs/tickets/chat-on-closed-task_SPEC.md), the
// PURE half of the capture side: criterion 3 (resurfaces' full truth table,
// a distinct reason per disqualifier) and criterion 4 (notifierSender is an
// EQUALITY match, never a substring one; senderAddress has its own table).
// ZERO I/O.
//
// ---- IMPOSED SURFACE ---------------------------------------------------------
//
//	internal/capture/resurface.go (new, pure: no context, no pgx, no connector,
//	no net/mail; the revive.go / overrides precedent):
//
//	  type resurfaceInput struct {
//	      status        string // the linked task's tasks.status, from taskForExternalRef
//	      activity      bool   // decideMessage's `activity` (SWT-45 J1): revive, own_action
//	                           // and blind outcomes stay SWT-45's
//	      dismissed     bool   // the task has an OPEN dismissal: SWT-36's guarded reopen owns it
//	      connectorCopy bool   // pm.channel == jira.Channel, computed in rules_store.go and passed
//	                           // in so this file imports no connector (SWT-45 J3)
//	      notifier      bool   // notifierSender(pm.msg.Sender, winner.notifiers) (CC4)
//	  }
//
//	  // resurfaces is true iff status == "closed" AND NOT activity AND NOT
//	  // dismissed AND NOT connectorCopy AND NOT notifier. The string is the
//	  // reason fragment the decision row records; every false outcome names
//	  // its own cause.
//	  func resurfaces(in resurfaceInput) (bool, string)
//
//	  // notifierSender reports whether an entry of list, case-folded and
//	  // trimmed, EQUALS either the whole stored sender (a Slack display name)
//	  // or senderAddress(sender) (a mail address). Empty / whitespace entries
//	  // match nothing.
//	  func notifierSender(sender string, list []string) bool
//
//	internal/capture/senderdomain.go:
//
//	  // senderAddress returns the addr-spec of the From header, "" when the
//	  // sender carries no address (a Slack / Upwork display name). The one
//	  // net/mail spelling; senderDomain is refactored onto it and
//	  // senderdomain_test.go passes UNMODIFIED.
//	  func senderAddress(sender string) string
//
// resurfaces is called on decideMessage's `found` branch, where the action is
// already task_log, so the input carries no action (the migration's CHECK is
// what pins resurface to task_log in the data).
//
// RED TODAY: resurface.go, resurfaceInput, resurfaces, notifierSender and
// senderAddress do not exist, so this file does not compile (and with it the
// package's unit test binary).

import (
	"fmt"
	"strings"
	"testing"
)

// ---- criterion 3: resurfaces' truth table -------------------------------------

// Every combination of {closed, ready, delivered, in_progress} x activity x
// dismissed x connectorCopy x notifier: 64 rows, true in exactly four of them
// (closed and nothing else set), and never true for a non-closed status.
func TestResurfaces_TruthTable(t *testing.T) {
	statuses := []string{"closed", "ready", "delivered", "in_progress"}
	bools := []bool{false, true}
	trues := 0
	for _, status := range statuses {
		for _, activity := range bools {
			for _, dismissed := range bools {
				for _, conn := range bools {
					for _, notifier := range bools {
						in := resurfaceInput{status: status, activity: activity, dismissed: dismissed,
							connectorCopy: conn, notifier: notifier}
						want := status == "closed" && !activity && !dismissed && !conn && !notifier
						got, reason := resurfaces(in)
						if got != want {
							t.Errorf("resurfaces(%+v) = %v, want %v. CC3: true iff the linked task is CLOSED, "+
								"the winner is not an activity match (SWT-45), the task has no open dismissal "+
								"(SWT-36), the message is not the Jira connector's own copy (J3), and the sender "+
								"is not on the project's notifier list (CC4)", in, got, want)
						}
						if got {
							trues++
						}
						if !got && strings.TrimSpace(reason) == "" {
							t.Errorf("resurfaces(%+v) = false with an EMPTY reason. Criterion 3: each false row "+
								"returns a reason fragment, because the decision row is the only place a "+
								"'why did this chat not resurface' question can be answered", in)
						}
					}
				}
			}
		}
	}
	// With four statuses and no other input, only (closed, all false) is true.
	if trues != 1 {
		t.Errorf("resurfaces was true in %d of 64 rows, want exactly 1 (closed, no activity, no dismissal, "+
			"not the connector copy, not a notifier)", trues)
	}
}

// "Each false row returns a DISTINCT reason fragment": one disqualifier at a
// time, so a smoke read of capture_decisions.reason can tell the five apart
// (Verification step 4 keys on "the reason naming the list").
func TestResurfaces_EachDisqualifierNamesItsOwnCause(t *testing.T) {
	cases := []struct {
		cause string
		in    resurfaceInput
	}{
		{"not closed", resurfaceInput{status: "ready"}},
		{"activity (SWT-45's)", resurfaceInput{status: "closed", activity: true}},
		{"open dismissal (SWT-36's)", resurfaceInput{status: "closed", dismissed: true}},
		{"the Jira connector's own copy (J3)", resurfaceInput{status: "closed", connectorCopy: true}},
		{"a notifier (CC4)", resurfaceInput{status: "closed", notifier: true}},
	}
	seen := map[string]string{}
	for _, tc := range cases {
		got, reason := resurfaces(tc.in)
		if got {
			t.Errorf("%s: resurfaces(%+v) = true, want false", tc.cause, tc.in)
			continue
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s: resurfaces(%+v) returned no reason", tc.cause, tc.in)
			continue
		}
		if prev, dup := seen[reason]; dup {
			t.Errorf("%s and %s share the reason %q. Criterion 3: each false row returns a DISTINCT fragment",
				prev, tc.cause, reason)
		}
		seen[reason] = tc.cause
	}
	// The notifier fragment names the list, so the decision reason does (criterion 5).
	if _, reason := resurfaces(resurfaceInput{status: "closed", notifier: true}); !strings.Contains(strings.ToLower(reason), "notifier") {
		t.Errorf("the notifier reason %q does not name the notifier list; criterion 5 asserts the decision "+
			"reason names it, and Verification 4 reads it to tell bot traffic from a human", reason)
	}
	// A non-closed status: delivered is log-only (J14 precedent), as is every open status.
	for _, status := range []string{"delivered", "in_progress", "ready", "holding", "blocked"} {
		if got, _ := resurfaces(resurfaceInput{status: status}); got {
			t.Errorf("resurfaces(status=%q) = true; only a CLOSED task's log line disappears from the board, "+
				"so only a closed task resurfaces (T6, T11)", status)
		}
	}
}

// ---- criterion 4: notifierSender is EQUALITY -------------------------------------

func TestNotifierSender_IsEqualityNeverSubstring(t *testing.T) {
	const katie = `"Katie Evans (JIRA)" <jira@treetopllc.jira.com>`
	cases := []struct {
		list   []string
		sender string
		want   bool
		why    string
	}{
		// A Slack display name: the WHOLE stored sender, case-folded and trimmed.
		{[]string{"Jira"}, "Jira", true, "the Jira Slack app's display name, exactly"},
		{[]string{"Jira"}, " jira ", true, "trimmed and case-folded"},
		{[]string{"Jira"}, "JIRA", true, "case-folded"},
		{[]string{"Jira"}, "Jiraiya Tanaka", false,
			"EQUALITY, never substring: a human whose name merely contains the entry is the exact message this " +
				"ticket exists to stop swallowing (the `sender` capture criterion is a substring match; reusing it " +
				"here is the named mutation)"},
		{[]string{"Jira"}, "Jira Software", false, "a longer display name is a different sender"},
		{[]string{"Jira"}, katie, false,
			"a mail From whose display name CONTAINS 'JIRA' is not the entry `Jira`: neither the whole sender nor " +
				"its address equals it"},
		// A mail address: the address parsed from the From, case-folded.
		{[]string{"jira@treetopllc.jira.com"}, katie, true, "the address inside the From header"},
		{[]string{"jira@treetopllc.jira.com"}, "JIRA@TreetopLLC.jira.com", true, "a bare address, case-folded"},
		{[]string{" JIRA@TreetopLLC.jira.com "}, katie, true, "the ENTRY is trimmed and case-folded too"},
		{[]string{"jira@treetopllc.jira.com"}, `"Katie Evans (JIRA)" <jira@treetopllc.jira.com.evil.example>`, false,
			"an address that merely starts with the entry is a different address"},
		{[]string{"jira@treetopllc.jira.com"}, "Katie Evans", false, "a display name with no address"},
		{[]string{"treetopllc.jira.com"}, katie, false, "a DOMAIN is not an address entry: equality, not a suffix"},
		// Empty and whitespace entries match nothing, even an empty sender.
		{[]string{""}, "", false, "an empty entry must never match (it would equal an address-less sender's \"\")"},
		{[]string{""}, "Jira", false, "an empty entry matches nothing"},
		{[]string{"   "}, "", false, "a whitespace entry trims to empty and matches nothing"},
		{[]string{"   "}, "   ", false, "same, against a whitespace sender"},
		{[]string{""}, "Katie Evans", false, "an empty entry vs a sender with no address (senderAddress = \"\")"},
		{nil, "Jira", false, "a nil list matches nothing (DEFAULT '{}' is today's behaviour)"},
		{[]string{}, "Jira", false, "an empty list matches nothing"},
		{[]string{"", "Jira"}, "Jira", true, "an empty entry is skipped, not poison: the next entry still matches"},
	}
	for _, tc := range cases {
		if got := notifierSender(tc.sender, tc.list); got != tc.want {
			t.Errorf("notifierSender(%q, %q) = %v, want %v — %s", tc.sender, tc.list, got, tc.want, tc.why)
		}
	}
}

// ---- criterion 4: senderAddress's own table ---------------------------------------

// The address comparison is case-insensitive in notifierSender, so this table
// compares case-insensitively and does not pin senderAddress's casing.
func TestSenderAddress_ParsesTheFromHeaderInGo(t *testing.T) {
	cases := []struct {
		sender, want, why string
	}{
		{"LinkedIn <messages-noreply@linkedin.com>", "messages-noreply@linkedin.com", "display name and angle-addr"},
		{"messages-noreply@linkedin.com", "messages-noreply@linkedin.com", "a bare address"},
		{`"Vazquez, Gil" <gil@sspataro.com>`, "gil@sspataro.com", "a quoted display name containing a comma"},
		{`"Katie Evans (JIRA)" <jira@treetopllc.jira.com>`, "jira@treetopllc.jira.com",
			"Jira's own From shape: the address, never the display name"},
		{"=?UTF-8?Q?Caf=C3=A9_Nextdoor?= <digest@nextdoor.com>", "digest@nextdoor.com", "an RFC 2047 display name"},
		{"JIRA@TreetopLLC.jira.com", "jira@treetopllc.jira.com", "case is not significant to the match"},
		{"Jira", "", "a Slack display name carries no address"},
		{"gil vazquez", "", "an Upwork / Slack display name carries no address"},
		{"", "", "empty"},
		{"   ", "", "whitespace"},
	}
	for _, tc := range cases {
		got := senderAddress(tc.sender)
		if !strings.EqualFold(got, tc.want) {
			t.Errorf("senderAddress(%q) = %q, want %q — %s", tc.sender, got, tc.want, tc.why)
		}
		if strings.ContainsAny(got, "<> ") {
			t.Errorf("senderAddress(%q) = %q carries an angle bracket or space: the split_part trap "+
				"senderdomain.go exists to avoid", tc.sender, got)
		}
	}
}

// "senderDomain is refactored onto it": for every addressed sender the two
// answers agree, so the one net/mail spelling cannot drift from itself.
func TestSenderDomain_IsTheHostOfSenderAddress(t *testing.T) {
	for _, sender := range []string{
		"LinkedIn <messages-noreply@linkedin.com>",
		`"Vazquez, Gil" <gil@sspataro.com>`,
		`"Katie Evans (JIRA)" <jira@treetopllc.jira.com>`,
		"Motorola <NEWS@Motorola.COM>",
		"news@medium.com",
	} {
		addr := senderAddress(sender)
		at := strings.LastIndexByte(addr, '@')
		if at < 0 {
			t.Errorf("senderAddress(%q) = %q has no '@'", sender, addr)
			continue
		}
		if want := strings.ToLower(addr[at+1:]); senderDomain(sender) != want {
			t.Errorf("senderDomain(%q) = %q, but the host of senderAddress is %q: %s", sender,
				senderDomain(sender), want, fmt.Sprintf("CC4 refactors senderDomain onto senderAddress (one net/mail spelling)"))
		}
	}
}
