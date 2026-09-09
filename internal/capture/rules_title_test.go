package capture

// Unit tests for SWT-31 (docs/tickets/board-dismissals_SPEC.md) Part 1 —
// acceptance criteria 1, 2, 4 and 6: the task TITLE a capture rule composes, and
// the task BODY it must NOT change. ZERO I/O: `ruleTaskTitle` and `ruleTaskBody`
// are pure string functions over values the driver already holds, which is the
// whole reason the branch can be unit-tested at all (invariant 7's habit applied
// to a non-orchestrator function).
//
// GREENFIELD NOTE — EXPECTED RED. `ruleTaskTitle` takes (key, subject, body)
// today and `storedRule` has no project NAME, so this file compile-FAILS
// internal/capture until the SPEC's shape lands. That is the intended initial
// state; criterion 6's characterization below is green under the current
// implementation and is here to STAY green.
//
// IMPOSED SURFACE (the SPEC fixes the behaviour, not the Go spelling; this is
// the smallest growth of the existing call that carries every input criterion 4
// names, and it passes values the driver already has in hand at the call site
// `createRuleTask`):
//
//	func ruleTaskTitle(key string, msg Message, winner storedRule) string
//	type storedRule struct { ...; projectName string }   // criterion 3's new column
//
// The DISCRIMINATOR is D1's, and nothing else: `key == msg.ThreadKey && key != ""`.
// Not `external_system == "upwork_crm"`, not a provider constant, not a parse of
// the key — see rules_structure_test.go's scan for the enforced half of that.

import (
	"strings"
	"testing"
	"time"
)

// ---- fixtures ------------------------------------------------------------------

// A live-shaped upwork rule: `thread_key_prefix` with NO key_regex, which is the
// exact configuration that makes capture.externalKey return the thread_key
// VERBATIM (rules.go) — the cause of the 128-character board titles this ticket
// fixes. Rules 57/58 on `saka` are its production instances.
func titleUpworkRule() storedRule {
	return storedRule{
		rule: Rule{
			ID: 57, Project: "saka", Kind: "thread_key_prefix",
			Pattern: "upwork_crm:1234:room:", Enabled: true,
		},
		projectID:   9001,
		extSystem:   "upwork_crm",
		projectName: "Saka",
	}
}

// A jira rule, whose derived key is NEVER the thread key: `WEB-1204` vs
// `jira:caprules.jira.com:WEB-1204`. This is the "discriminates on real data
// today" claim in D1, and every case below that uses it is a characterization of
// behaviour that must not move a byte.
func titleJiraRule() storedRule {
	return storedRule{
		rule: Rule{
			ID: 12, Project: "collaboratory", Kind: "thread_key_prefix",
			Pattern: "jira:caprules.jira.com:WEB-", Enabled: true,
		},
		projectID:   9002,
		extSystem:   "jira",
		projectName: "Collaboratory",
	}
}

const titleUpworkThreadKey = "upwork_crm:1234:room:5678"

// ---- criterion 1: the non-thread-key title is byte-identical to today ----------

// "ruleTaskTitle renders {key} — {head} byte-identically to today for every task
// whose derived external key is NOT the message's thread key ... including the
// 120-rune textmatch.NormalizedPrefix truncation and the subject →
// first-body-line fallback."
//
// A characterization test: every `want` here is what the CURRENT implementation
// produces. If the new branch changes any of them, the five live jira-shaped
// capture tasks silently change shape too.
func TestRuleTaskTitle_NonThreadKeyedIsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		msg  Message
		want string
	}{
		{
			// Jira-shaped key WITH a subject. The message has a thread key and it
			// is not equal to the derived key, so the new branch must not fire.
			name: "jira key with a subject",
			key:  "WEB-1204",
			msg: Message{
				ID: 1, ThreadKey: "jira:caprules.jira.com:WEB-1204",
				Sender: "Jira <jira@caprules.jira.com>", Subject: "Rate limiting is live",
				BodyText: "the change is deployed\nsecond line ignored",
			},
			want: "WEB-1204 — Rate limiting is live",
		},
		{
			// Jira-shaped key with NO subject: the head falls back to the FIRST
			// line of the body, trimmed. The rest of the body is dropped.
			name: "jira key falls back to the first body line",
			key:  "LHH-23637",
			msg: Message{
				ID: 2, ThreadKey: "slack:TCAPRULES:C0CAPRULES", Sender: "Jira APP",
				BodyText: "  Salvador commented on LHH-23637: pass 1 of the ranking fix  \nand a second line",
			},
			want: "LHH-23637 — Salvador commented on LHH-23637: pass 1 of the ranking fix",
		},
		{
			// Whitespace is COLLAPSED, not preserved — NormalizedPrefix runs
			// strings.Fields over the whole composed title. Pinned because a
			// board row rendered with a tab in it is how this surfaces.
			name: "internal whitespace collapses",
			key:  "WEB-1204",
			msg: Message{
				ID: 3, ThreadKey: "jira:caprules.jira.com:WEB-1204",
				Subject: "  Rate   limiting\tis\n live  ",
			},
			want: "WEB-1204 — Rate limiting is live",
		},
		{
			// No subject and no body: the title is the key alone, with no
			// dangling separator. create_task rejects an empty title, so this is
			// the floor for the plain branch (criterion 4's other half).
			name: "no head at all yields the bare key",
			key:  "WEB-9",
			msg:  Message{ID: 4, ThreadKey: "jira:caprules.jira.com:WEB-9"},
			want: "WEB-9",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := ruleTaskTitle(tc.key, tc.msg, titleJiraRule())
			if got != tc.want {
				t.Errorf("ruleTaskTitle(%q, ...) = %q, want %q.\nCriterion 1: for a key that is NOT the "+
					"message's thread key the title must be byte-identical to what shipped — the branch "+
					"D1 adds is a NEW case, not a rewrite of the existing one", tc.key, got, tc.want)
			}
		})
	}
}

// The 120-rune truncation, in RUNES and not bytes. The subject is multibyte on
// purpose: a byte-slicing truncation would both produce the wrong length and cut
// a rune in half, and the whole point of routing this through
// textmatch.NormalizedPrefix is that there is ONE spelling of rune-safe
// truncation in the repo.
func TestRuleTaskTitle_TruncatesTo120Runes(t *testing.T) {
	subject := strings.Repeat("é", 200)
	got := ruleTaskTitle("WEB-1204", Message{
		ID: 5, ThreadKey: "jira:caprules.jira.com:WEB-1204", Subject: subject,
	}, titleJiraRule())

	if n := len([]rune(got)); n != 120 {
		t.Fatalf("ruleTaskTitle produced %d runes, want exactly 120 (rulesTitleLen via "+
			"textmatch.NormalizedPrefix).\ngot: %q", n, got)
	}
	if len(got) == len([]rune(got)) {
		t.Errorf("the truncated title has one byte per rune (%d bytes), so the multibyte subject was "+
			"lost or the truncation is byte-based; 120 RUNES is the contract", len(got))
	}
	if !strings.HasPrefix(got, "WEB-1204 — é") {
		t.Errorf("truncated title = %q, want it to still begin `WEB-1204 — é` (truncation is a prefix, "+
			"not a summary)", got)
	}
	if strings.ContainsRune(got, '�') {
		t.Errorf("truncated title contains a replacement rune (%q): a rune was cut in half", got)
	}
}

// ---- criterion 2: the thread-keyed branch is the SENDER ------------------------

// Q1's answer, verbatim from the SPEC: "a thread_key_prefix rule on
// upwork_crm:{client}:room:{room} with sender `Mario Cruz`, an empty subject and
// a body starting `Hi Salvador,` yields `Mario Cruz — Hi Salvador,`".
//
// The sender is the one fact the board row does not already show — the project is
// its own column — which is exactly why the answer is not the project name.
func TestRuleTaskTitle_ThreadKeyedUsesTheSender(t *testing.T) {
	got := ruleTaskTitle(titleUpworkThreadKey, Message{
		ID: 6, ThreadKey: titleUpworkThreadKey, Sender: "Mario Cruz", Subject: "",
		BodyText: "Hi Salvador,\nI wanted to check in about the invoice",
	}, titleUpworkRule())

	const want = "Mario Cruz — Hi Salvador,"
	if got != want {
		t.Errorf("ruleTaskTitle for a thread-keyed task = %q, want %q.\nCriterion 2 (Q1 = message sender): "+
			"when the derived external key IS the message's thread key, the 128-character key is not a "+
			"title — it is the dedup key, and it already lives in external_refs and the task body", got, want)
	}
	if strings.Contains(got, "upwork_crm:") {
		t.Errorf("the title still contains the raw thread key (%q). That IS the defect: "+
			"`upwork_crm:1234:room:5678 — Hi Salvador,` is what the board renders today", got)
	}
}

// The empty-sender half of criterion 2, and the head of criterion 4's chain:
// "the SAME rule with an empty sender yields `Saka — Hi Salvador,` (the
// project-name fallback)".
//
// Empty senders are real: the CRM's sender column is whatever the provider
// stored, and the open-questions note records that it is empty on some sources.
// That is the entire reason the answer needed a fallback chain at all.
func TestRuleTaskTitle_ThreadKeyedFallsBackToProjectName(t *testing.T) {
	got := ruleTaskTitle(titleUpworkThreadKey, Message{
		ID: 7, ThreadKey: titleUpworkThreadKey, Sender: "", Subject: "",
		BodyText: "Hi Salvador,\nI wanted to check in about the invoice",
	}, titleUpworkRule())

	const want = "Saka — Hi Salvador,"
	if got != want {
		t.Errorf("ruleTaskTitle with an empty sender = %q, want %q (projects.name — criterion 3's new "+
			"column). A blank prefix would render ` — Hi Salvador,` on the board", got, want)
	}
}

// ---- criterion 4: the whole chain, in order ------------------------------------

// "Fallbacks, in order: sender → project name → project slug → the key itself."
// Each step is exercised by REMOVING the one above it, so a chain that skipped a
// link (say, straight from sender to slug) fails on the middle case rather than
// passing by coincidence.
func TestRuleTaskTitle_FallbackChainInOrder(t *testing.T) {
	body := "Hi Salvador,\nI wanted to check in about the invoice"
	for _, tc := range []struct {
		name   string
		sender string
		rule   storedRule
		want   string
	}{
		{
			name: "sender wins over everything",
			// The project name and slug are both present and both different, so a
			// green result cannot come from the wrong link of the chain.
			sender: "Mario Cruz", rule: titleUpworkRule(),
			want: "Mario Cruz — Hi Salvador,",
		},
		{
			name:   "no sender falls to projects.name",
			sender: "", rule: titleUpworkRule(),
			want: "Saka — Hi Salvador,",
		},
		{
			name: "no sender and no name falls to the slug",
			// A project whose `name` column is empty. `projects.name` is NOT NULL
			// in 0001 but nothing forbids '', and the slug is the value this
			// package has always carried (storedRule.rule.Project).
			sender: "", rule: func() storedRule { r := titleUpworkRule(); r.projectName = ""; return r }(),
			want: "saka — Hi Salvador,",
		},
		{
			name:   "nothing but the key",
			sender: "", rule: func() storedRule {
				r := titleUpworkRule()
				r.projectName, r.rule.Project = "", ""
				return r
			}(),
			// The last link is the key itself — today's behaviour, kept as the
			// floor so the title is never empty and never a bare separator.
			want: titleUpworkThreadKey + " — Hi Salvador,",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := ruleTaskTitle(titleUpworkThreadKey, Message{
				ID: 8, ThreadKey: titleUpworkThreadKey, Sender: tc.sender, BodyText: body,
			}, tc.rule)
			if got != tc.want {
				t.Errorf("ruleTaskTitle = %q, want %q (criterion 4: sender → project name → project slug "+
					"→ the key)", got, tc.want)
			}
		})
	}
}

// "The title is never empty (create_task rejects an empty title —
// validateCreateTask), and the head-less case still yields a title."
//
// The assertion is deliberately the CONTRACT and not a chosen string: what the
// label-only title reads as is the implementer's call, but an empty title fails
// validateCreateTask and a dangling ` — ` is a rendering bug on the board.
func TestRuleTaskTitle_IsNeverEmptyAndNeverADanglingSeparator(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  Message
		rule storedRule
	}{
		{
			name: "thread-keyed with a sender and no head",
			msg:  Message{ID: 9, ThreadKey: titleUpworkThreadKey, Sender: "Mario Cruz"},
			rule: titleUpworkRule(),
		},
		{
			name: "thread-keyed with no sender and no head",
			msg:  Message{ID: 10, ThreadKey: titleUpworkThreadKey},
			rule: titleUpworkRule(),
		},
		{
			name: "thread-keyed with nothing but the key",
			msg:  Message{ID: 11, ThreadKey: titleUpworkThreadKey},
			rule: func() storedRule {
				r := titleUpworkRule()
				r.projectName, r.rule.Project = "", ""
				return r
			}(),
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := ruleTaskTitle(titleUpworkThreadKey, tc.msg, tc.rule)
			if strings.TrimSpace(got) == "" {
				t.Fatalf("ruleTaskTitle = %q — create_task refuses an empty title, so a headless "+
					"message would abort the capture pass mid-run (criterion 4)", got)
			}
			if strings.HasSuffix(strings.TrimSpace(got), "—") {
				t.Errorf("ruleTaskTitle = %q ends with a dangling separator; the head is optional and "+
					"the separator goes with it", got)
			}
		})
	}
}

// D1, asserted directly: the branch keys on the EQUALITY, so a message with an
// empty thread key can never take it — `key != ""` is the second half of the
// condition and without it every keyless rule on a thread-less message would
// render the sender with no key anywhere.
func TestRuleTaskTitle_EmptyThreadKeyNeverTakesTheSenderBranch(t *testing.T) {
	// key == msg.ThreadKey == "" — equal, but empty. The plain branch must run.
	got := ruleTaskTitle("", Message{ID: 12, ThreadKey: "", Sender: "Mario Cruz",
		Subject: "a subject"}, titleUpworkRule())
	if strings.HasPrefix(got, "Mario Cruz") {
		t.Errorf("ruleTaskTitle with an EMPTY key and thread key = %q — the discriminator is "+
			"`key == msg.ThreadKey && key != \"\"` (D1). Dropping the second half makes two absent "+
			"values look like a match", got)
	}
}

// ---- criterion 6: ruleTaskBody is unchanged ------------------------------------

// "ruleTaskBody is unchanged — thread_key, sender, message_id and the rule id
// stay in the body, so the identity dropped from the title is still one click
// away on /tasks/{id}, and external_refs still carries the key."
//
// GREEN TODAY AND MUST STAY GREEN. This is the counterweight to the title
// change: the title stops being the identity, so the body has to keep being it.
// A full-string characterization rather than four Contains checks, because the
// argument is "unchanged", not "still mentions".
func TestRuleTaskBody_Characterization(t *testing.T) {
	pm := pendingMessage{
		msg: Message{
			ID: 4242, ThreadKey: titleUpworkThreadKey, Sender: "Mario Cruz", Subject: "",
			BodyText:          "Hi Salvador,\nI wanted to check in about the invoice",
			ExternalMessageID: "upwork:msg:99",
		},
		channel: "upwork",
		sentAt:  time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC),
	}
	want := "Captured deterministically by capture rule 57 (thread_key_prefix \"upwork_crm:1234:room:\").\n\n" +
		"external: upwork_crm " + titleUpworkThreadKey + "\n" +
		"channel: upwork\n" +
		"thread_key: " + titleUpworkThreadKey + "\n" +
		"sender: Mario Cruz\n" +
		"sent_at: 2026-09-09T12:00:00Z\n" +
		"message_id: 4242\n" +
		"external_message_id: upwork:msg:99\n" +
		"\nHi Salvador, I wanted to check in about the invoice"

	got := ruleTaskBody(pm, titleUpworkRule(), "upwork_crm", titleUpworkThreadKey)
	if got != want {
		t.Errorf("ruleTaskBody changed.\n got: %q\nwant: %q\nCriterion 6: the title change moves the "+
			"identity OUT of the title, so the body is now the only place a human can recover it — "+
			"thread_key, sender, message_id and the rule id all stay", got, want)
	}
}

// The same body, for a message whose sender is empty: `(none)` rather than a
// blank line. Pinned because the title's fallback chain now depends on the same
// empty-sender case, and a "helpful" cleanup of one is a silent change to the
// other.
func TestRuleTaskBody_EmptySenderStillReadsAsNone(t *testing.T) {
	pm := pendingMessage{
		msg: Message{
			ID: 4243, ThreadKey: titleUpworkThreadKey, Sender: "",
			BodyText: "Hi Salvador,",
		},
		channel: "",
		sentAt:  time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC),
	}
	got := ruleTaskBody(pm, titleUpworkRule(), "upwork_crm", titleUpworkThreadKey)
	if !strings.Contains(got, "sender: (none)\n") {
		t.Errorf("ruleTaskBody with an empty sender does not write `sender: (none)`:\n%s", got)
	}
	if !strings.Contains(got, "channel: (none)\n") {
		t.Errorf("ruleTaskBody with an empty channel does not write `channel: (none)`:\n%s", got)
	}
}
