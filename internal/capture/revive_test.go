package capture

// SWT-45 (docs/tickets/jira-activity-revive_SPEC.md), the PURE half of the
// capture side: criterion 20 (overrides' full truth table) and the three rule
// SHAPES this ticket seeds as data — J2 (Treetop notification mail keyed from
// the SUBJECT line only, outranking rule 10), J4 (reengine's addressed-to-me
// rule) and the rule-10 successor the owner's 2026-09-12 answer arms (a Slack
// or GitHub mention of a key IS Jira activity). ZERO I/O.
//
// ---- IMPOSED SURFACE ---------------------------------------------------------
//
//	internal/capture/revive.go (new, pure — no context, no pgx):
//	  func overrides(revive, addressed, gateOn bool) bool
//	  // = revive AND (NOT gateOn OR addressed)   (J1)
//
// RED TODAY: revive.go and overrides do not exist, so this file does not
// compile (and with it the package's unit test binary).
//
// THE EVALUATOR PINS ARE GREEN BY DESIGN once the file compiles. F2 says no new
// criterion type is needed: `sender` + a subject-anchored key_regex, and a
// `body_regex` anchored the same way, already key on the subject with TODAY's
// Evaluate. These tests pin the exact regexes the runbook seeds (F8: a rule's
// key_regex cannot be edited after the INSERT, so it must be right the first
// time) against the evaluator that will run them. They were verified against a
// throwaway stub of overrides in the authoring session.

import "testing"

// ---- criterion 20: overrides, all 8 rows --------------------------------------

func TestOverrides_TruthTable(t *testing.T) {
	for _, tc := range []struct {
		revive, addressed, gateOn, want bool
		why                             string
	}{
		{false, false, false, false, "not activity: today's behaviour"},
		{false, false, true, false, "not activity: today's behaviour (Part D's gate once merged)"},
		{false, true, false, false, "addressed without revive is refused by the CHECK; if it existed it overrides nothing"},
		{false, true, true, false, "same"},
		{true, false, false, true, "decision 1: on a gate-off project (collaboratory) ANY activity revives or creates"},
		{true, false, true, false, "decision 3: a revive-only rule on a GATED project still goes through Part D — " +
			"an operator cannot bypass the gate by forgetting which flag means what"},
		{true, true, false, true, "addressed implies revive; gate off"},
		{true, true, true, true, "decision 3: activity ADDRESSED to him overrides the assignee check"},
	} {
		if got := overrides(tc.revive, tc.addressed, tc.gateOn); got != tc.want {
			t.Errorf("overrides(revive=%v, addressed=%v, gateOn=%v) = %v, want %v — %s",
				tc.revive, tc.addressed, tc.gateOn, got, tc.want, tc.why)
		}
	}
}

// ---- J2: the Treetop notification rule keys from the SUBJECT line only --------

func rvStr(s string) *string { return &s }

// The two collaboratory rules as they will stand after seeding: J2 at 92, rule
// 10 at 90 (priority DESC, id ASC). IDs mirror production's for readability.
func rvTreetopRules() []Rule {
	jira := "jira"
	return []Rule{
		{ID: 10, Project: "collaboratory", Kind: KindBodyRegex, Pattern: `(WEB|API|OPS)-[0-9]+`,
			Source: &jira, Priority: 90, Enabled: true},
		{ID: 92, Project: "collaboratory", Kind: KindSender, Pattern: "jira@treetopllc.jira.com",
			Source: &jira, ExternalKeyRegex: rvStr(`^[^\n]*?\b((?:WEB|API|OPS)-[0-9]+)\b`), Priority: 92, Enabled: true},
	}
}

const rvTreetopSender = "Katie Evans (JIRA) <jira@treetopllc.jira.com>"

func TestEvaluate_TreetopNotificationKeysFromTheSubjectOnly(t *testing.T) {
	for _, tc := range []struct {
		name, subject, body string
		wantRule            int64
		wantKey             string
		why                 string
	}{
		{
			name: "mentioned you on", subject: "Katie Evans mentioned you on API-4104",
			body:     "Katie Evans mentioned you on a comment.\nSee also WEB-10355 and OPS-12.",
			wantRule: 92, wantKey: "API-4104",
			why: "the key is the SUBJECT's; the body names two other tickets and must not win",
		},
		{
			name: "assigned to you", subject: "Katie Evans assigned OPS-77 to you",
			body: "API-1 is related", wantRule: 92, wantKey: "OPS-77",
		},
		{
			name: "status update shape", subject: "(API-4103) Fix the volunteer export",
			body: "Katie Evans changed the status to TT-Closed", wantRule: 92, wantKey: "API-4103",
		},
		{
			name: "a digest with no key in the subject", subject: "Your Jira daily digest",
			body:     "WEB-1 was updated\nAPI-2 was commented on",
			wantRule: 92, wantKey: "",
			why: "J2: a subject with no key derives NO key and becomes attribution only — never a bucket log " +
				"(which is what rule 10 would do with its prefix key)",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			m := Evaluate(Message{ID: 1, Sender: rvTreetopSender, Subject: tc.subject, BodyText: tc.body}, rvTreetopRules())
			if m.Rule == nil || m.Rule.ID != tc.wantRule {
				t.Fatalf("Evaluate(%q) winner = %v, want rule %d. J2 at priority 92 outranks rule 10 (90), so "+
					"Jira mail leaves the prefix buckets", tc.subject, m.Rule, tc.wantRule)
			}
			if m.ExternalKey != tc.wantKey {
				t.Errorf("Evaluate(%q).ExternalKey = %q, want %q. F2: keyText is subject+\"\\n\"+body and Go's ^ "+
					"without (?m) is START OF TEXT, so ^[^\\n]*? reads the subject line only. %s",
					tc.subject, m.ExternalKey, tc.wantKey, tc.why)
			}
		})
	}
}

// F1, as the control that makes the pin above mean something: rule 10 ALONE on
// the same mail keys by PREFIX. That is the landmine J2 routes the mail away
// from, and the reason J1 forbids ever flagging rule 10.
func TestEvaluate_Rule10AloneKeysByPrefix(t *testing.T) {
	rules := rvTreetopRules()[:1]
	m := Evaluate(Message{ID: 1, Sender: rvTreetopSender, Subject: "Katie Evans mentioned you on API-4104"}, rules)
	if m.Rule == nil || m.ExternalKey != "API" {
		t.Errorf("rule 10 alone keyed the mention as %q (rule %v), want \"API\" — F1: extractKey returns the "+
			"FIRST group, (WEB|API|OPS). If this ever changes, re-read J1's refusal before relaxing it",
			m.ExternalKey, m.Rule)
	}
}

// A Slack message has no subject, so its key text starts with "\n" and a
// subject-anchored key regex cannot match (J1's own argument). J2's SENDER test
// already excludes it; this pins that the anchor would too.
func TestEvaluate_TheSubjectAnchorNeverMatchesASubjectlessMessage(t *testing.T) {
	r := rvTreetopRules()[1]
	r.Kind = KindBodyRegex
	r.Pattern = `API-[0-9]+`
	if got := externalKey(Message{BodyText: "can you look at API-4104?"}, &r); got != "" {
		t.Errorf("a subject-anchored key regex derived %q from a subjectless (Slack-shaped) message, want \"\"", got)
	}
}

// ---- J4: reengine's addressed-to-me rule ----------------------------------------

const (
	rvJ4Pattern  = `\A[^\n]*(?:\bmentioned you on LHH-[0-9]+|\bassigned LHH-[0-9]+ to you)`
	rvJ4KeyRegex = `\A[^\n]*?\b(LHH-[0-9]+)\b`
)

// Rule 1 (reengine's LHH catch-all) and rule 2 (the Avviato Jira mail sender
// rule, Part D D-D6) stay untouched; J4 outranks both and stays below 95.
func rvReengineRules() []Rule {
	jira := "jira"
	return []Rule{
		{ID: 1, Project: "reengine", Kind: KindBodyRegex, Pattern: `LHH-[0-9]+`, Source: &jira, Priority: 90, Enabled: true},
		{ID: 2, Project: "reengine", Kind: KindSender, Pattern: "jira@avviato.atlassian.net",
			Source: &jira, ExternalKeyRegex: rvStr(`LHH-[0-9]+`), Priority: 90, Enabled: true},
		{ID: 404, Project: "reengine", Kind: KindBodyRegex, Pattern: rvJ4Pattern, Source: &jira,
			ExternalKeyRegex: rvStr(rvJ4KeyRegex), Priority: 91, Enabled: true},
	}
}

func TestEvaluate_ReengineAddressedRuleSelectsOnTheSubjectShape(t *testing.T) {
	const avviato = "Ana Rossi (Jira) <jira@avviato.atlassian.net>"
	for _, tc := range []struct {
		name, sender, subject, body string
		wantRule                    int64
		wantKey                     string
		why                         string
	}{
		{"mentioned you on", avviato, "Ana Rossi mentioned you on LHH-23637", "see LHH-1", 404, "LHH-23637",
			"decision 3: 'X mentioned you on K' is activity addressed to him and overrides the gate"},
		{"assigned to you", avviato, "Ana Rossi assigned LHH-5 to you", "", 404, "LHH-5",
			"decision 3: 'X assigned K to you' likewise"},
		{"a status update is NOT addressed", avviato, "(LHH-9) moved to Done", "", 0, "LHH-9",
			"decision 3: all other activity on a gated ticket follows the SWT-40 Part D gate — rule 2, unchanged"},
		{"the phrase in the BODY only is not addressed", avviato, "(LHH-9) updated",
			"Ana Rossi mentioned you on LHH-9", 0, "LHH-9",
			"J4 selects on the SUBJECT shape (\\A[^\\n]*...); a quoted body line must not lift a message over the gate"},
		{"a Slack message carrying the phrase", "Ana Rossi", "", "Ana mentioned you on LHH-3", 0, "LHH-3",
			"a Slack message has no subject: its text starts with \\n and the \\A anchor cannot match (J1)"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			m := Evaluate(Message{ID: 1, Sender: tc.sender, Subject: tc.subject, BodyText: tc.body}, rvReengineRules())
			// wantRule 0 means "NOT J4 (rule 404)": rules 1 and 2 are untouched data
			// whose relative order is production's business, not this ticket's.
			switch {
			case m.Rule == nil:
				t.Fatalf("Evaluate(%q / %q) matched nothing — %s", tc.subject, tc.body, tc.why)
			case tc.wantRule == 0 && m.Rule.ID == 404:
				t.Fatalf("Evaluate(%q / %q) chose J4 (rule 404), want rule 1 or 2 — %s", tc.subject, tc.body, tc.why)
			case tc.wantRule != 0 && m.Rule.ID != tc.wantRule:
				t.Fatalf("Evaluate(%q / %q) winner = rule %d, want rule %d — %s", tc.subject, tc.body, m.Rule.ID, tc.wantRule, tc.why)
			}
			if m.ExternalKey != tc.wantKey {
				t.Errorf("Evaluate(%q).ExternalKey = %q, want %q", tc.subject, m.ExternalKey, tc.wantKey)
			}
		})
	}
	// The flags J4 is seeded with make it override on reengine's ARMED gate.
	if !overrides(true, true, true) {
		t.Errorf("J4's flags (--revive --addressed) do not override on a gated project; decision 3 is unreachable")
	}
}

// ---- the owner's answer (2026-09-12): a mention of a key IS Jira activity -----

// "A Slack or GitHub message that names a ticket key counts as Jira activity:
// it revives the ticket's closed task or creates one." The rule that carries it
// is capture-rule-ticket-keys' successor to rule 10, which this ticket does not
// add — but J1 decides what that rule must look like to be LEGAL with
// --revive: an explicit key_regex capturing the WHOLE key. Pinned here so that
// ticket's rule, seeded with --revive, keys API-4104 and not API.
func TestEvaluate_TheMentionSuccessorKeysTheWholeTicket(t *testing.T) {
	jira := "jira"
	mention := Rule{ID: 11, Project: "collaboratory", Kind: KindBodyRegex, Pattern: `\b(?:WEB|API|OPS)-[0-9]+\b`,
		Source: &jira, ExternalKeyRegex: rvStr(`\b((?:WEB|API|OPS)-[0-9]+)\b`), Priority: 90, Enabled: true}
	rules := append(rvTreetopRules()[1:], mention) // J2 + the successor (rule 10 disabled/replaced)

	for _, tc := range []struct {
		name string
		msg  Message
		rule int64
		key  string
	}{
		{"a Treetop Slack message", Message{ID: 1, Sender: "Katie Evans", BodyText: "can you look at API-4104?"}, 11, "API-4104"},
		{"a GitHub notification mail", Message{ID: 2, Sender: "GitHub <notifications@github.com>",
			Subject: "[treetop/web] Fix export (#88)", BodyText: "Closes WEB-10442."}, 11, "WEB-10442"},
		{"Jira mail still goes to J2", Message{ID: 3, Sender: rvTreetopSender,
			Subject: "Katie Evans mentioned you on API-4104", BodyText: "see WEB-1"}, 92, "API-4104"},
	} {
		m := Evaluate(tc.msg, rules)
		if m.Rule == nil || m.Rule.ID != tc.rule || m.ExternalKey != tc.key {
			t.Errorf("%s: Evaluate = rule %v key %q, want rule %d key %q", tc.name, m.Rule, m.ExternalKey, tc.rule, tc.key)
		}
	}
	// collaboratory's gate is off: a reviving mention rule overrides (decision 1).
	if !overrides(true, false, false) {
		t.Errorf("a --revive mention rule on collaboratory (gate off) does not override; the owner's answer " +
			"(mentions revive or create) is unreachable")
	}
}
