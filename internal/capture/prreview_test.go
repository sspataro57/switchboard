package capture

// Unit tests for SWT-54 (docs/tickets/treetop-pr-review-tasks_SPEC.md): the
// pure half of the PR-review rule. ZERO I/O — no database, no network, no LLM.
//
//   - criterion 5: the SEEDED pattern and key_regex strings, evaluated against
//     real thread-key shapes and carried through github.ParsePRRef / PRKey /
//     PRURL, the driver's canonicalization (D2 point 3);
//   - D3 / criterion 12: priority 91 between rule 10 (90) and the LHH rules (100);
//   - criterion 7: decidePRAuthor implements D1's table row by row — case-folded
//     logins, `*`-suffix entries, `author` outranking everything, `undetermined`
//     when nothing is known;
//   - OQ-1 = (b): the seeded rule's exclude list is EMPTY, so a dependabot[bot]
//     PR is a colleague's PR (row 4), and the exclusion mechanism is still pinned
//     for a rule that does list `*[bot]` (row 3);
//   - criterion 11: prReviewTitle's pinned cases.
//
// GREENFIELD NOTE — EXPECTED RED: prreview.go (decidePRAuthor, prReviewTitle,
// matchExcludedAuthor, the verdict consts) and internal/connector/github's
// prref.go do not exist, so this file compile-FAILS internal/capture's tests.
//
// IMPOSED SURFACE (the SPEC names decidePRAuthor(facts, excludeList) and
// prReviewTitle(ref, subject, body); the Go shapes are chosen here):
//
//	const prAuthorOwn, prAuthorExcluded, prAuthorOther, prAuthorUndetermined = "own", "excluded", "other", "undetermined"
//	type prAuthorFacts struct {
//	    reasons          []string // X-GitHub-Reason of every stored inbound mail read for the PR ("" = header absent)
//	    openingFound     bool     // the message whose external_message_id is <{owner}/{repo}/pull/{N}@github.com>
//	    openingSender    string   // its X-GitHub-Sender: the PR author's login
//	    openingRecipient string   // its X-GitHub-Recipient: his login as GitHub addressed that mail
//	}
//	type prAuthorVerdict struct { verdict, author, evidence string }
//	func decidePRAuthor(f prAuthorFacts, exclude []string) prAuthorVerdict
//	func matchExcludedAuthor(login string, exclude []string) (entry string, ok bool)
//	func prReviewTitle(ref github.PRRef, subject, body string) string
//
// Readings chosen here, each flagged:
//  1. Two EMPTY logins never read as "his" (row 2): the `key != ""` precedent
//     (ruleTaskTitle) — two absent values are not a match.
//  2. A `*` entry's suffix match is case-folded like the equality: GitHub logins
//     are case-insensitive. A bare `*` (an empty suffix) matches NOTHING: the
//     tool refuses it, and a stored one must not exclude every PR.
//  3. prReviewTitle's body fallback is the body's first line with
//     ruleFirstLine's semantics.

import (
	"fmt"
	"net/mail"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sspataro57/switchboard/internal/connector/github"
	"github.com/sspataro57/switchboard/internal/textmatch"
)

// The seeded rule, verbatim from the SPEC's D2 block and Verification step 3.
// capture_rules cannot be edited or re-added with the same pattern (IK F8), so
// these exact strings are proven here before anyone types them into prod.
const (
	seededPRPattern  = `<treetopllc/`
	seededPRKeyRegex = `<(treetopllc/[A-Za-z0-9._-]+/pull/[0-9]+)@github\.com>$`
	seededPRPriority = 91
	// Step 0b: his login, the ONLY X-GitHub-Recipient seen in 90 days.
	prHisLogin = "sspataro57"
)

func seededPRRule(id int64) Rule {
	system := "github"
	re := seededPRKeyRegex
	return Rule{ID: id, Project: "collaboratory", Kind: KindThreadKeyContains, Pattern: seededPRPattern,
		Source: &system, ExternalKeyRegex: &re, Priority: seededPRPriority, Enabled: true}
}

// ---- criterion 5: the seeded strings on real thread keys -------------------------

func TestEvaluate_SeededPRRuleDerivesOneCanonicalKeyPerPR(t *testing.T) {
	// Step 0f: TWO receiving accounts. The account is part of the thread key, so
	// the key_regex must not depend on it.
	const acctA, acctB = "sspataro@gmail.com", "second-account@example.test"
	gh := func(account, repo, kind string, n int) string {
		return fmt.Sprintf("gmail:%s:<%s/%s/%d@github.com>", account, repo, kind, n)
	}
	cases := []struct {
		name      string
		threadKey string
		matched   bool
		wantKey   string // "" with matched = attribution only, no key
		wantURL   string
	}{
		{"collaboratory-www pull (the recorded production sample)", msgGitHubMailKey, true,
			"treetopllc/collaboratory-www#3179", "https://github.com/treetopllc/collaboratory-www/pull/3179"},
		{"gonoble pull", gh(acctA, "treetopllc/gonoble", "pull", 12), true,
			"treetopllc/gonoble#12", "https://github.com/treetopllc/gonoble/pull/12"},
		{"another treetopllc repo, second account (0b's noble-go-sdk#486)", gh(acctB, "treetopllc/noble-go-sdk", "pull", 486), true,
			"treetopllc/noble-go-sdk#486", "https://github.com/treetopllc/noble-go-sdk/pull/486"},
		{"dots and underscores in a repo name", gh(acctA, "treetopllc/web.app_v2", "pull", 7), true,
			"treetopllc/web.app_v2#7", "https://github.com/treetopllc/web.app_v2/pull/7"},
		{"a www issue thread matches but derives NO key", gh(acctA, "treetopllc/collaboratory-www", "issues", 214), true, "", ""},
		{"a gonoble issue thread (the recorded sample)", msgGitHubGonobleKey, true, "", ""},
		{"commit mail on a treetopllc repo", "gmail:" + acctA + ":<treetopllc/collaboratory-www/commit/0a1b2c3d@github.com>", true, "", ""},
		{"Foundry-Underwriting is his own: not matched", gh(acctA, "Foundry-Underwriting/foundry-rave", "pull", 31), false, "", ""},
		{"tower987124 is his own: not matched", gh(acctA, "tower987124/switchboard", "pull", 5), false, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := Evaluate(Message{ID: 1, Source: acctA, ThreadKey: c.threadKey,
				Sender: "joseg-avviato <notifications@github.com>"}, []Rule{seededPRRule(64)})
			if !c.matched {
				assertUnmatched(t, m, "the pattern <treetopllc/ must not reach repos he controls")
				return
			}
			assertMatched(t, m, 64, "collaboratory")
			if c.wantKey == "" {
				if m.ExternalKey != "" {
					t.Errorf("ExternalKey = %q, want \"\": only a /pull/ root derives a key (attribution only otherwise)", m.ExternalKey)
				}
				return
			}
			ref, ok := github.ParsePRRef(m.ExternalKey)
			if !ok {
				t.Fatalf("the seeded key_regex captured %q, which github.ParsePRRef refuses; the driver would give "+
					"attribution only for every PR", m.ExternalKey)
			}
			if got := github.PRKey(ref); got != c.wantKey {
				t.Errorf("PRKey(ParsePRRef(%q)) = %q, want %q (criterion 5: the canonical key)", m.ExternalKey, got, c.wantKey)
			}
			if got := github.PRURL(ref); got != c.wantURL {
				t.Errorf("PRURL = %q, want %q", got, c.wantURL)
			}
		})
	}
}

// ---- D3 / criterion 12: where 91 sits --------------------------------------------

func TestEvaluate_SeededPRRuleOutranksRule10AndYieldsToTheLHHRules(t *testing.T) {
	jira := "jira"
	rules := append(fixtureRules(), seededPRRule(64),
		// rule 10 as the runbook spells it: the prefix-keyed Treetop body_regex at 90.
		Rule{ID: 10, Project: "collaboratory", Kind: KindBodyRegex, Pattern: `(WEB|API|OPS)-[0-9]+`,
			Source: &jira, Priority: 90, Enabled: true})

	web := Message{ID: 1, ThreadKey: msgGitHubMailKey,
		Subject: "Re: [treetopllc/collaboratory-www] WEB-1234 ranking widget (PR #3179)"}
	assertMatched(t, Evaluate(web, rules), 64, "collaboratory") // 91 beats rule 10's 90

	lhh := Message{ID: 2, ThreadKey: msgGitHubMailKey,
		Subject: "Re: [treetopllc/collaboratory-www] LHH-23637 recommendation fix (PR #3179)"}
	assertMatched(t, Evaluate(lhh, rules), 1, "reengine") // rule 1 at 100 keeps reengine work

	plain := Message{ID: 3, ThreadKey: msgGitHubMailKey,
		Subject: "Re: [treetopllc/collaboratory-www] Ranking widget (PR #3179)"}
	assertMatched(t, Evaluate(plain, rules), 64, "collaboratory") // 91 beats rule 6's 50
}

// ---- criterion 7: D1's table ------------------------------------------------------

func TestPRAuthorVerdicts_AreSpelledAsTheSPEC(t *testing.T) {
	for got, want := range map[string]string{
		prAuthorOwn: "own", prAuthorExcluded: "excluded", prAuthorOther: "other", prAuthorUndetermined: "undetermined",
	} {
		if got != want {
			t.Errorf("verdict const = %q, want %q (the dry run and the decision reasons print these words)", got, want)
		}
	}
}

func TestDecidePRAuthor_D1TableRowByRow(t *testing.T) {
	const anyAuthor = "\x00" // do not check the author field
	opening := func(sender, recipient string, reasons ...string) prAuthorFacts {
		return prAuthorFacts{reasons: reasons, openingFound: true, openingSender: sender, openingRecipient: recipient}
	}
	cases := []struct {
		name     string
		facts    prAuthorFacts
		exclude  []string
		verdict  string
		author   string
		evidence []string
	}{
		// Row 1: reason author on ANY mail — GitHub's own statement that he authored it.
		{"row 1: any mail carries reason author", prAuthorFacts{reasons: []string{"comment", "author"}}, nil,
			prAuthorOwn, anyAuthor, []string{"author"}},
		{"row 1 outranks a colleague-named opening", opening("joseg-avviato", prHisLogin, "author"), nil,
			prAuthorOwn, anyAuthor, []string{"author"}},
		{"row 1 outranks an excluded opening sender", opening("dependabot[bot]", prHisLogin, "subscribed", "author"),
			[]string{"*[bot]"}, prAuthorOwn, anyAuthor, []string{"author"}},
		{"row 1 outranks review_requested", prAuthorFacts{reasons: []string{"review_requested", "author"}}, nil,
			prAuthorOwn, anyAuthor, []string{"author"}},

		// Row 2: the opening's sender IS the recipient. Reason deliberately not
		// author, so only row 2 can call it his.
		{"row 2: opening sender equals its recipient", opening(prHisLogin, prHisLogin, "subscribed"), nil,
			prAuthorOwn, anyAuthor, []string{prHisLogin}},
		{"row 2 is case-folded", opening("SSpataro57", "sspataro57", "subscribed"), nil,
			prAuthorOwn, anyAuthor, nil},
		{"row 2 outranks row 3 (his login also listed)", opening(prHisLogin, prHisLogin), []string{prHisLogin},
			prAuthorOwn, anyAuthor, nil},

		// Row 3: exclude_pr_authors (his other logins, and bots when a rule lists them).
		{"row 3: an exact entry, case-folded", opening("SSpataro-Alt", prHisLogin, "subscribed"), []string{"sspataro-alt"},
			prAuthorExcluded, "SSpataro-Alt", []string{"sspataro-alt"}},
		{"row 3: *[bot] covers dependabot[bot]", opening("dependabot[bot]", prHisLogin, "subscribed"), []string{"*[bot]"},
			prAuthorExcluded, "dependabot[bot]", []string{"*[bot]", "dependabot[bot]"}},
		{"row 3: *[bot] covers every GitHub App bot", opening("renovate[bot]", prHisLogin), []string{"sspataro-alt", "*[bot]"},
			prAuthorExcluded, "renovate[bot]", []string{"*[bot]"}},
		{"row 3 outranks review_requested", opening("dependabot[bot]", prHisLogin, "review_requested"), []string{"*[bot]"},
			prAuthorExcluded, "dependabot[bot]", nil},

		// Row 4: a named colleague. Step 0b's opening senders.
		{"row 4: joseg-avviato", opening("joseg-avviato", prHisLogin, "subscribed"), nil,
			prAuthorOther, "joseg-avviato", []string{"joseg-avviato"}},
		{"row 4: ananthsekar007", opening("ananthsekar007", prHisLogin, "review_requested"), nil,
			prAuthorOther, "ananthsekar007", []string{"ananthsekar007"}},
		{"row 4 under the SEEDED rule (OQ-1 = b): dependabot[bot] with an EMPTY exclude list is a colleague",
			opening("dependabot[bot]", prHisLogin, "subscribed"), nil, prAuthorOther, "dependabot[bot]", []string{"dependabot[bot]"}},
		{"row 4: an exact entry is not a prefix match", opening("joseg-avviato", prHisLogin), []string{"joseg"},
			prAuthorOther, "joseg-avviato", nil},
		{"row 4: *[bot] is a SUFFIX match, not a substring", opening("bot-maker", prHisLogin), []string{"*[bot]"},
			prAuthorOther, "bot-maker", nil},
		{"row 4: an empty recipient cannot make row 2 fire", opening("joseg-avviato", ""), nil,
			prAuthorOther, "joseg-avviato", nil},

		// Row 5: review_requested proves another author without naming one.
		{"row 5: review_requested, no opening", prAuthorFacts{reasons: []string{"subscribed", "review_requested"}}, nil,
			prAuthorOther, "", []string{"review_requested"}},
		{"row 5: an opening that names no sender", opening("", prHisLogin, "review_requested"), nil,
			prAuthorOther, "", []string{"review_requested"}},

		// Row 6: nothing — fail-open.
		{"row 6: no evidence at all", prAuthorFacts{}, nil, prAuthorUndetermined, "", nil},
		{"row 6: 0b's noble-go-sdk#486 (state_change only)", prAuthorFacts{reasons: []string{"state_change"}}, nil,
			prAuthorUndetermined, "", nil},
		{"row 6: 0b's collaboratory-www#3186 (subscribed, no opening)", prAuthorFacts{reasons: []string{"subscribed"}}, nil,
			prAuthorUndetermined, "", nil},
		{"row 6: commit/push mail contributes empty reasons", prAuthorFacts{reasons: []string{"", ""}}, nil,
			prAuthorUndetermined, "", nil},
		// ACCEPTED BY DESIGN (SPEC D1 residual, 2026-09-14): his OWN PR whose first
		// mail he receives says reason `mention` (or whose `author` mail went to the
		// other account) is undetermined, so a task IS created. Fail-open: a wrong
		// task costs one Done; a missed review is invisible.
		{"row 6: a mention-only first mail on HIS OWN PR is undetermined (a task, by design)",
			prAuthorFacts{reasons: []string{"mention"}}, nil, prAuthorUndetermined, "", nil},
		{"two EMPTY logins are not his", opening("", ""), nil, prAuthorUndetermined, "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decidePRAuthor(c.facts, c.exclude)
			if got.verdict != c.verdict {
				t.Fatalf("decidePRAuthor(%+v, %v).verdict = %q, want %q", c.facts, c.exclude, got.verdict, c.verdict)
			}
			if c.author != anyAuthor && got.author != c.author {
				t.Errorf("author = %q, want %q (criterion 10: `other` names the author login when known)", got.author, c.author)
			}
			if c.verdict != prAuthorUndetermined && strings.TrimSpace(got.evidence) == "" {
				t.Errorf("evidence is empty for verdict %q; the decision reason carries it (invariant 7: every "+
					"decision says why)", got.verdict)
			}
			for _, frag := range c.evidence {
				if !strings.Contains(got.evidence, frag) {
					t.Errorf("evidence %q does not mention %q", got.evidence, frag)
				}
			}
		})
	}
}

func TestMatchExcludedAuthor_EqualityOrStarSuffix(t *testing.T) {
	cases := []struct {
		login   string
		exclude []string
		entry   string
		ok      bool
	}{
		{"dependabot[bot]", []string{"*[bot]"}, "*[bot]", true},
		{"Dependabot[BOT]", []string{"*[bot]"}, "*[bot]", true},
		{"joseg-avviato", []string{"*avviato"}, "*avviato", true},
		{"sspataro-alt", []string{"SSpataro-Alt"}, "SSpataro-Alt", true},
		{"dependabot[bot]", []string{"joseg-avviato", "*[bot]"}, "*[bot]", true},
		{"joseg-avviato", []string{"joseg"}, "", false},
		{"joseg", []string{"joseg-avviato"}, "", false},
		{"bot-maker", []string{"*[bot]"}, "", false},
		{"anyone", []string{"*"}, "", false},
		{"", []string{"*[bot]"}, "", false},
		{"", nil, "", false},
		{"joseg-avviato", []string{}, "", false},
	}
	for _, c := range cases {
		entry, ok := matchExcludedAuthor(c.login, c.exclude)
		if ok != c.ok || entry != c.entry {
			t.Errorf("matchExcludedAuthor(%q, %q) = (%q, %v), want (%q, %v)", c.login, c.exclude, entry, ok, c.entry, c.ok)
		}
	}
}

// ---- D1 amendment 2026-09-14: the origin check -----------------------------------

// prGenuineAuth is the shape Gmail prepends to every GitHub notification on prod
// (121 of 121 Treetop PR-thread mails, both receiving accounts, 2026-09-14).
const prGenuineAuth = "Authentication-Results: mx.google.com;\r\n" +
	"       dkim=pass header.i=@github.com header.s=pf2023 header.b=AbCdEf12;\r\n" +
	"       spf=pass (google.com: domain of notifications@github.com designates 192.30.252.201 as permitted sender) smtp.mailfrom=notifications@github.com;\r\n" +
	"       dmarc=pass (p=REJECT sp=REJECT dis=NONE) header.from=github.com\r\n"

// prEvilAuth is what Gmail genuinely prepends to an attacker's own mail: a real
// mx.google.com header, dkim=pass — for the attacker's domain.
const prEvilAuth = "Authentication-Results: mx.google.com;\r\n" +
	"       dkim=pass header.i=@evil.example header.s=s1 header.b=Zz;\r\n" +
	"       spf=pass (google.com: domain of bounce@evil.example designates 203.0.113.9 as permitted sender) smtp.mailfrom=bounce@evil.example\r\n"

// prForgedGitHubAuth is an attacker-supplied header claiming GitHub's pass. It
// travels INSIDE the message, so it always sits below the receiving MX's.
const prForgedGitHubAuth = "Authentication-Results: mx.google.com; dkim=pass header.d=github.com header.i=@github.com\r\n"

// prHeaders parses a real header block (net/mail keeps the order of repeated
// headers, which is the property under test), so no map is built by hand.
func prHeaders(t *testing.T, block string) mail.Header {
	t.Helper()
	m, err := mail.ReadMessage(strings.NewReader(block +
		"From: GitHub <notifications@github.com>\r\nX-GitHub-Reason: author\r\n\r\nbody\r\n"))
	if err != nil {
		t.Fatalf("parse header block: %v", err)
	}
	return m.Header
}

// MUTATIONS this table is built to catch:
//   - reading ANY Authentication-Results instead of the topmost: the two
//     "forged below" rows turn true;
//   - dropping the authserv-id comparison: the "wrong authserv-id" rows turn true.
func TestTrustedGitHubNotification_TopmostGmailDKIMPassForGitHubOnly(t *testing.T) {
	cases := []struct {
		name  string
		block string
		want  bool
	}{
		{"genuine: Gmail's header, dkim=pass header.i=@github.com", prGenuineAuth, true},
		{"genuine: header.d=github.com", "Authentication-Results: mx.google.com; dkim=pass header.d=github.com header.s=pf2023\r\n", true},
		{"genuine with a harmless forged one below", prGenuineAuth + "Authentication-Results: mx.google.com; dkim=fail header.d=github.com\r\n", true},
		{"authserv-id with a version", "Authentication-Results: mx.google.com 1; dkim=pass header.d=github.com\r\n", true},
		{"method, result and domain are case-insensitive", "Authentication-Results: mx.google.com; DKIM=Pass header.d=GitHub.COM\r\n", true},
		{"CFWS around '='", "Authentication-Results: mx.google.com; dkim = pass header.d = github.com\r\n", true},
		{"a second dkim result for github passes", "Authentication-Results: mx.google.com; dkim=pass header.i=@amazonses.com; dkim=pass header.i=@github.com\r\n", true},

		{"forged github pass BELOW Gmail's genuine header for the attacker's domain", prEvilAuth + prForgedGitHubAuth, false},
		{"forged github pass BELOW a genuine dkim=fail", "Authentication-Results: mx.google.com; dkim=fail header.i=@github.com\r\n" + prForgedGitHubAuth, false},
		{"forged topmost with a wrong authserv-id", "Authentication-Results: mx.evil.example; dkim=pass header.d=github.com\r\n", false},
		{"forged topmost with a wrong authserv-id above Gmail's", "Authentication-Results: mx.evil.example; dkim=pass header.d=github.com\r\n" + prEvilAuth, false},
		{"authserv-id that only starts with mx.google.com", "Authentication-Results: mx.google.com.evil.example; dkim=pass header.d=github.com\r\n", false},
		{"dkim=fail for github", "Authentication-Results: mx.google.com; dkim=fail header.i=@github.com header.d=github.com\r\n", false},
		{"dkim=neutral for github", "Authentication-Results: mx.google.com; dkim=neutral header.d=github.com\r\n", false},
		{"dkim=pass for another domain", prEvilAuth, false},
		{"header.d=github.com.evil.example", "Authentication-Results: mx.google.com; dkim=pass header.d=github.com.evil.example\r\n", false},
		{"header.i=@github.com.evil.example", "Authentication-Results: mx.google.com; dkim=pass header.i=@github.com.evil.example\r\n", false},
		{"header.i=@notgithub.com", "Authentication-Results: mx.google.com; dkim=pass header.i=@notgithub.com\r\n", false},
		// go-reviewer delta 2026-09-14: a quoted header.i smuggling a header.d token.
		// MUTATION: dropping the any-quote-in-a-dkim-result rule turns these true.
		{"a quoted header.i smuggling header.d=github.com",
			`Authentication-Results: mx.google.com; dkim=pass header.i="x header.d=github.com "@evil.example header.s=s1` + "\r\n", false},
		{"any quote in a dkim result, even beside a real github pass",
			`Authentication-Results: mx.google.com; dkim=pass header.d=github.com header.s="pf2023"` + "\r\n", false},
		{"a pass only inside a comment", "Authentication-Results: mx.google.com; dkim=fail (dkim=pass header.d=github.com) header.d=evil.example\r\n", false},
		{"spf/dmarc pass for github but no dkim", "Authentication-Results: mx.google.com; spf=pass smtp.mailfrom=notifications@github.com; dmarc=pass header.from=github.com\r\n", false},
		{"dkim=pass naming no domain", "Authentication-Results: mx.google.com; dkim=pass\r\n", false},
		{"no results at all", "Authentication-Results: mx.google.com; none\r\n", false},
		{"ARC-Authentication-Results is not Authentication-Results", "ARC-Authentication-Results: i=1; mx.google.com; dkim=pass header.i=@github.com\r\n", false},
		{"missing header", "", false},
	}
	for _, c := range cases {
		if got := trustedGitHubNotification(prHeaders(t, c.block)); got != c.want {
			t.Errorf("%s: trustedGitHubNotification = %v, want %v\nblock:\n%s", c.name, got, c.want, c.block)
		}
	}
	// Prepended duplicates of action-driving headers (Codex re-review 2026-09-14).
	// prHeaders' base already carries ONE X-GitHub-Reason. MUTATION: disabling
	// duplicatedGitHubHeader turns every "duplicated" row true.
	single := prGenuineAuth +
		"Message-ID: <treetopllc/collaboratory-www/pull/3179/c1@github.com>\r\n" +
		"References: <treetopllc/collaboratory-www/pull/3179@github.com>\r\n" +
		"In-Reply-To: <treetopllc/collaboratory-www/pull/3179@github.com>\r\n" +
		"X-GitHub-Sender: joseg-avviato\r\nX-GitHub-Recipient: sspataro57\r\n"
	for _, c := range []struct {
		name, extra, dup string
	}{
		{"a single instance of each stays trusted", "", ""},
		{"duplicated Message-ID", "Message-ID: <treetopllc/collaboratory-www/pull/9999@github.com>\r\n", "Message-ID"},
		{"duplicated Message-ID, other case", "message-id: <treetopllc/collaboratory-www/pull/9999@github.com>\r\n", "Message-ID"},
		{"duplicated References", "References: <treetopllc/collaboratory-www/pull/9999@github.com>\r\n", "References"},
		{"duplicated In-Reply-To", "In-Reply-To: <treetopllc/collaboratory-www/pull/9999@github.com>\r\n", "In-Reply-To"},
		{"duplicated X-GitHub-Reason", "X-GitHub-Reason: author\r\n", "X-GitHub-Reason"},
		{"duplicated X-GitHub-Sender", "X-GitHub-Sender: sspataro57\r\n", "X-GitHub-Sender"},
		{"duplicated X-GitHub-Recipient", "X-GitHub-Recipient: joseg-avviato\r\n", "X-GitHub-Recipient"},
	} {
		// The attacker's copies sit BELOW Gmail's header and ABOVE the signed originals.
		block := prGenuineAuth + c.extra + strings.TrimPrefix(single, prGenuineAuth)
		h := prHeaders(t, block)
		if got := duplicatedGitHubHeader(h); got != c.dup {
			t.Errorf("%s: duplicatedGitHubHeader = %q, want %q", c.name, got, c.dup)
		}
		if got, want := trustedGitHubNotification(h), c.dup == ""; got != want {
			t.Errorf("%s: trustedGitHubNotification = %v, want %v", c.name, got, want)
		}
	}

	if prTrustedAuthServID != "mx.google.com" || prTrustedDKIMDomain != "github.com" {
		t.Errorf("trust constants = %q / %q, want mx.google.com / github.com (prod evidence, 2026-09-14)",
			prTrustedAuthServID, prTrustedDKIMDomain)
	}
}

// ---- 2026-09-14: the PR identity is bound to SIGNED headers ------------------------

// prProdEncodedSubject is the one prod Subject (of 123, 90 days) that does not
// end "(PR #N)" until RFC 2047-decoded — verbatim, collaboratory-www#3218.
const prProdEncodedSubject = "=?UTF-8?Q?[treetopllc/collaboratory-www]_WEB-8680_Remove_unused?= " +
	"=?UTF-8?Q?_OrganizationsUsers/GroupsUsers/OpportunitiesUs=E2=80=A6_=28PR?= =?UTF-8?Q?_#3218=29?="

// MUTATIONS: making either binding always pass turns its failure rows green-as-"".
func TestPRMailBinding_ListIDAndSubjectNameThePR(t *testing.T) {
	ref := github.PRRef{Repo: "treetopllc/collaboratory-www", PR: 3218}
	const goodList = "List-ID: treetopllc/collaboratory-www <collaboratory-www.treetopllc.github.com>\r\n"
	const goodSubj = "Subject: Re: [treetopllc/collaboratory-www] Ranking widget (PR #3218)\r\n"
	cases := []struct {
		name, block, want string // want: "" = bound, else a fragment of the failure
	}{
		{"List-ID and Subject name the PR (prod form)", goodList + goodSubj, ""},
		{"an opening's subject (no Re:)", goodList + "Subject: [treetopllc/collaboratory-www] Ranking widget (PR #3218)\r\n", ""},
		{"the prod RFC 2047-encoded subject decodes and binds", goodList + "Subject: " + prProdEncodedSubject + "\r\n", ""},
		{"List-ID compared case-insensitively", "List-ID: TreetopLLC/Collaboratory-WWW <Collaboratory-WWW.TreetopLLC.github.com>\r\n" + goodSubj, ""},

		{"a List-ID for another repo", "List-ID: treetopllc/gonoble <gonoble.treetopllc.github.com>\r\n" + goodSubj, "List-ID binding failed"},
		{"a List-ID for another owner", "List-ID: attacker-org/collaboratory-www <collaboratory-www.attacker-org.github.com>\r\n" + goodSubj, "List-ID binding failed"},
		{"a List-ID with a suffix", "List-ID: x <collaboratory-www.treetopllc.github.com.evil.example>\r\n" + goodSubj, "List-ID binding failed"},
		{"the right name only in the display part", "List-ID: treetopllc/collaboratory-www <evil-repo.attacker-org.github.com>\r\n" + goodSubj, "List-ID binding failed"},
		{"a missing List-ID", goodSubj, "no List-ID"},
		{"a Subject with a different PR number", goodList + "Subject: [treetopllc/collaboratory-www] Other (PR #3219)\r\n", "Subject binding failed"},
		{"a Subject naming the PR only mid-title", goodList + "Subject: [treetopllc/collaboratory-www] Revert (PR #3218) (PR #7)\r\n", "Subject binding failed"},
		{"a Subject whose number only starts with N", goodList + "Subject: [treetopllc/collaboratory-www] Other (PR #32180)\r\n", "Subject binding failed"},
		{"a missing Subject", goodList, "Subject binding failed"},
	}
	for _, c := range cases {
		h := prHeaders(t, prGenuineAuth+c.block)
		got := prMailBindingFailure(h, ref)
		if (c.want == "" && got != "") || (c.want != "" && !strings.Contains(got, c.want)) {
			t.Errorf("%s: prMailBindingFailure = %q, want %q", c.name, got, c.want)
		}
		if full := prUntrustedReason(h, ref); full != got {
			t.Errorf("%s: prUntrustedReason = %q, want the binding verdict %q (auth and headers are otherwise genuine)", c.name, full, got)
		}
	}
	for _, dup := range []string{"List-ID", "Subject"} {
		extra := goodList
		if dup == "Subject" {
			extra = goodSubj
		}
		h := prHeaders(t, prGenuineAuth+extra+goodList+goodSubj)
		if got := prUntrustedReason(h, ref); !strings.Contains(got, "header "+dup+" appears more than once") {
			t.Errorf("duplicated %s: prUntrustedReason = %q, want the duplicate named", dup, got)
		}
	}
}

// ---- criterion 11: the review title -----------------------------------------------

func TestPRReviewTitle_PinnedCases(t *testing.T) {
	www := github.PRRef{Repo: "treetopllc/collaboratory-www", PR: 3179}
	gonoble := github.PRRef{Repo: "treetopllc/gonoble", PR: 12}
	cases := []struct {
		name    string
		ref     github.PRRef
		subject string
		body    string
		want    string
	}{
		{"the SPEC's pinned case", www, "Re: [treetopllc/collaboratory-www] Ranking widget (PR #3179)", "",
			"Review PR #3179 — collaboratory-www: Ranking widget"},
		{"no Re:", www, "[treetopllc/collaboratory-www] Ranking widget (PR #3179)", "",
			"Review PR #3179 — collaboratory-www: Ranking widget"},
		{"repeated Re:, any case", www, "RE: re: Re: [treetopllc/collaboratory-www] Ranking widget (PR #3179)", "",
			"Review PR #3179 — collaboratory-www: Ranking widget"},
		{"a subject without the bracket", www, "Ranking widget (PR #3179)", "",
			"Review PR #3179 — collaboratory-www: Ranking widget"},
		{"a bare subject", www, "Ranking widget", "",
			"Review PR #3179 — collaboratory-www: Ranking widget"},
		{"a (PR #N) whose N differs is left in place", www, "[treetopllc/collaboratory-www] Revert ranking (PR #3100)", "",
			"Review PR #3179 — collaboratory-www: Revert ranking (PR #3100)"},
		{"a Jira key stays visible (D3)", www, "Re: [treetopllc/collaboratory-www] WEB-1234 ranking widget (PR #3179)", "",
			"Review PR #3179 — collaboratory-www: WEB-1234 ranking widget"},
		{"gonoble", gonoble, "[treetopllc/gonoble] Rate limits (PR #12)", "",
			"Review PR #12 — gonoble: Rate limits"},
		{"an empty subject falls back to the body's first line", www, "",
			"Bump sanitize-html from 2.11.0 to 2.12.1\n\nBumps sanitize-html from 2.11.0 to 2.12.1.",
			"Review PR #3179 — collaboratory-www: Bump sanitize-html from 2.11.0 to 2.12.1"},
		{"empty subject and body: no dangling separator", www, "", "", "Review PR #3179 — collaboratory-www"},
		{"a whitespace-only subject is empty", www, "   ", "", "Review PR #3179 — collaboratory-www"},
	}
	for _, c := range cases {
		if got := prReviewTitle(c.ref, c.subject, c.body); got != c.want {
			t.Errorf("%s: prReviewTitle(%+v, %q, %q) = %q, want %q", c.name, c.ref, c.subject, c.body, got, c.want)
		}
	}
}

func TestPRReviewTitle_TruncatesWithTheOneSpelling(t *testing.T) {
	www := github.PRRef{Repo: "treetopllc/collaboratory-www", PR: 3179}
	long := strings.Repeat("überlange Überschrift ", 20) // multi-byte runes: truncation must be rune-safe
	got := prReviewTitle(www, "[treetopllc/collaboratory-www] "+long+"(PR #3179)", "")
	want := textmatch.NormalizedPrefix("Review PR #3179 — collaboratory-www: "+long, 120)
	if got != want {
		t.Errorf("prReviewTitle(long) = %q\nwant %q (textmatch.NormalizedPrefix, 120 runes — the ONE spelling)", got, want)
	}
	if n := utf8.RuneCountInString(got); n > 120 || !utf8.ValidString(got) {
		t.Errorf("title is %d runes (valid UTF-8: %v); want <= 120 and valid", n, utf8.ValidString(got))
	}
}
