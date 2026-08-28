package capture

// Unit tests for the capture-rules evaluator (SWT-17,
// docs/tickets/capture-rules_SPEC.md §1-§3; acceptance criteria 2, 3, 4, 19).
// ZERO I/O: no database, no network, no LLM, no provider — criterion 2 makes
// that structural ("no pgxpool, no context, no provider import in its file"),
// and purity_test.go enforces it mechanically. This file only exercises it.
//
// GREENFIELD NOTE: internal/capture/rules.go does not exist yet, so every symbol
// below is a compile FAILURE today — the expected red state. Nothing here is
// stubbed; the SPEC's contract plus the implementer's declared signatures are the
// surface.
//
// IMPOSED surface (declared by the implementer, matching SPEC §2):
//
//	type Rule struct {
//	    ID int64; Project string; Kind string; Pattern string
//	    Source *string; ExternalKeyRegex *string; Priority int; Enabled bool
//	}
//	type Message struct {
//	    ID int64; Source string; ThreadKey string; Sender string
//	    Subject string; BodyText string; ExternalMessageID string
//	}
//	type Match struct { Rule *Rule; Project string; ExternalKey string }
//	func Evaluate(msg Message, rules []Rule) Match
//
// Three readings the SPEC does not spell out in Go, chosen here and flagged so
// they are decided rather than discovered:
//
//  1. `Message.Source` is the message's source account identity —
//     `source_accounts.account_email` — because that is what SPEC §1 says the
//     `source_slack_workspace` criterion resolves through ("deliberately ...
//     rather than by prefix-matching slack:{ws}: on the thread_key"). See
//     TestEvaluate_SlackWorkspaceMatchesTheLowercasedSyntheticAccount, which is
//     load-bearing: the connector lowercases the workspace id into that email
//     and the SPEC's fixture table spells the pattern uppercase.
//  2. `Rule.Source` is the rule's `external_system` (jira|github|upwork_crm|
//     slack|gmail), carried for the DRIVER's benefit — it decides `action`
//     ('task' vs 'attributed') and fills `capture_decisions.external_system`.
//     `Evaluate` does not consult it: a keyless rule still derives a key, and the
//     driver is what declines to act on it. The fixture set below sets it exactly
//     as the SPEC's acceptance table does, so the two cannot drift.
//  3. Evaluate must not depend on the CALLER's slice order — see
//     TestEvaluate_FirstMatchWinsByPriorityThenID. SPEC §2 says rules arrive
//     pre-sorted `priority DESC, id ASC`; a guarantee that holds only while every
//     caller remembers to sort is the shape this repo has been bitten by
//     repeatedly, and routing silently inverting is worse than one sort.
//
// The `person` criteria type is NOT here: `Message` as declared carries no
// participants, so the kind cannot be evaluated at all. See rules_person_test.go,
// which is deliberately its own file so it can be deleted in one move if `person`
// is being deferred.

import (
	"fmt"
	"strings"
	"testing"
)

// ---- fixture data: the SPEC's acceptance rule set, and message shapes the
// connectors actually emit -------------------------------------------------------

func rulePattern(s string) *string { return &s }

// Kinds, spelled exactly as the `capture_rules.criteria_type` CHECK (SPEC §1).
const (
	kindBodyRegex  = "body_regex"
	kindSender     = "sender"
	kindKeyPrefix  = "thread_key_prefix"
	kindKeyContain = "thread_key_contains"
	kindSlackWS    = "source_slack_workspace"
)

// fixtureRules is the SPEC's "Fixture rules (the acceptance data)" table,
// verbatim, already in the store's order (`priority DESC, id ASC`). Rule ids are
// the table's row order, so "rule 1" in a failure message is the first row.
func fixtureRules() []Rule {
	jira := rulePattern("jira") // external_system; NULL on the attribution-only rows
	return []Rule{
		{ID: 1, Project: "reengine", Kind: kindBodyRegex, Pattern: `LHH-[0-9]+`, Source: jira, Priority: 100, Enabled: true},
		{ID: 2, Project: "reengine", Kind: kindSender, Pattern: `jira@avviato.atlassian.net`, Source: jira,
			ExternalKeyRegex: rulePattern(`[A-Z]+-[0-9]+`), Priority: 100, Enabled: true},
		{ID: 3, Project: "collaboratory", Kind: kindKeyPrefix, Pattern: `jira:treetopllc.jira.com:WEB-`, Source: jira,
			ExternalKeyRegex: rulePattern(`[A-Z]+-[0-9]+$`), Priority: 50, Enabled: true},
		{ID: 4, Project: "collaboratory", Kind: kindKeyPrefix, Pattern: `jira:treetopllc.jira.com:API-`, Source: jira,
			ExternalKeyRegex: rulePattern(`[A-Z]+-[0-9]+$`), Priority: 50, Enabled: true},
		{ID: 5, Project: "collaboratory", Kind: kindKeyPrefix, Pattern: `jira:treetopllc.jira.com:OPS-`, Source: jira,
			ExternalKeyRegex: rulePattern(`[A-Z]+-[0-9]+$`), Priority: 50, Enabled: true},
		{ID: 6, Project: "collaboratory", Kind: kindKeyContain, Pattern: `treetopllc/collaboratory-www`, Priority: 50, Enabled: true},
		{ID: 7, Project: "collaboratory", Kind: kindKeyContain, Pattern: `treetopllc/gonoble`, Priority: 50, Enabled: true},
		{ID: 8, Project: "collaboratory", Kind: kindSlackWS, Pattern: `T0HPR78RX`, Priority: 10, Enabled: true},
		{ID: 9, Project: "collaboratory", Kind: kindSlackWS, Pattern: `T0360B84U`, Priority: 1, Enabled: true},
	}
}

// Message shapes as the connectors emit them (SPEC facts 9 and 11):
//   - jira thread_key      `jira:{site_host}:{KEY}`      (jira/normalize.go:64)
//   - slack thread_key     `slack:{ws}:{conv}`           (slackweb/normalize.go:71)
//   - slack account_email  `{lower(ws)}@slack-web.local` (slackweb/sink.go:28)
//   - gmail sender         the RAW From header, `Name <addr>` (google/normalize.go:111)
const (
	msgJiraWEBKey   = "jira:treetopllc.jira.com:WEB-1204"
	msgJiraAPIKey   = "jira:treetopllc.jira.com:API-77"
	msgSlackTreetop = "slack:T0360B84U:D01EJRX6P45"
	msgSlackCollab  = "slack:T0HPR78RX:C04LLAMA1"

	// A GitHub notification mail, which is how a REPO PATH gets inside a
	// thread_key — see TestEvaluate_RepoRulesMatchGitHubNotificationMail. The
	// shape is `gmail:{account}:{Message-ID}` and GitHub's Message-ID is
	// `<{owner}/{repo}/{kind}/{number}@github.com>`; production carries 199 such
	// keys (measured 2026-08-28, all gmail).
	msgGitHubMailKey    = "gmail:sspataro@gmail.com:<treetopllc/collaboratory-www/pull/3179@github.com>"
	msgGitHubGonobleKey = "gmail:sspataro@gmail.com:<treetopllc/gonoble/issues/214@github.com>"

	// The synthetic slack identity, spelled as sink.go writes it: LOWERCASED.
	acctTreetopSlack = "t0360b84u@slack-web.local"
	acctCollabSlack  = "t0hpr78rx@slack-web.local"
	acctJira         = "sspataro@gmail.com"

	// A raw From header, not a bare address (fact 9: sender matching is substring).
	senderAvviatoJira = `Jira <jira@avviato.atlassian.net>`
)

func assertMatched(t *testing.T, got Match, wantRuleID int64, wantProject string) {
	t.Helper()
	if got.Rule == nil {
		t.Fatalf("Evaluate returned the zero Match (unmatched); want rule %d -> %q", wantRuleID, wantProject)
	}
	if got.Rule.ID != wantRuleID {
		t.Errorf("matched rule id = %d, want %d", got.Rule.ID, wantRuleID)
	}
	if got.Project != wantProject {
		t.Errorf("Match.Project = %q, want %q", got.Project, wantProject)
	}
	// The winner and the reported project must be the SAME rule's project, or a
	// decision row would name a project no rule chose.
	if got.Rule.Project != got.Project {
		t.Errorf("Match.Project = %q but the matched rule's project is %q — they must be one value",
			got.Project, got.Rule.Project)
	}
}

func assertUnmatched(t *testing.T, got Match, why string) {
	t.Helper()
	if got.Rule != nil || got.Project != "" || got.ExternalKey != "" {
		t.Fatalf("Evaluate matched (rule=%v project=%q key=%q); want the zero Match: %s",
			got.Rule, got.Project, got.ExternalKey, why)
	}
}

// ---- criterion 2: unmatched is the zero Match ----------------------------------

func TestEvaluate_UnmatchedIsTheZeroMatch(t *testing.T) {
	msg := Message{
		ID: 1, Source: acctJira, ThreadKey: "jira:sspataro.atlassian.net:SWT-17",
		Sender: "Salvador <salvador@example.test>", Subject: "lunch",
		BodyText: "nothing routable in here", ExternalMessageID: "jira:sspataro.atlassian.net:comment:9",
	}
	assertUnmatched(t, Evaluate(msg, fixtureRules()), "no fixture rule covers this message")

	// An empty rule set is the day-one state (rules are seeded by runbook, not by
	// the migration — SPEC "Decisions made unilaterally" 5). It must be inert, not
	// a panic inside a CronJob.
	assertUnmatched(t, Evaluate(msg, nil), "no rules are configured at all")
	assertUnmatched(t, Evaluate(msg, []Rule{}), "the rule set is empty")
}

// ---- criterion 2 / SPEC §1 table: every criteria type, with its near miss -------

func TestEvaluate_CriteriaKinds(t *testing.T) {
	cases := []struct {
		name    string
		rule    Rule
		msg     Message
		matched bool
		why     string
	}{
		{
			name:    "body_regex matches the BODY",
			rule:    Rule{ID: 1, Project: "reengine", Kind: kindBodyRegex, Pattern: `LHH-[0-9]+`, Priority: 100, Enabled: true},
			msg:     Message{ThreadKey: msgSlackTreetop, Subject: "", BodyText: "Salvador commented on LHH-23637: pushed the fix"},
			matched: true,
		},
		{
			name: "body_regex matches the SUBJECT (subject and body are one text)",
			rule: Rule{ID: 1, Project: "reengine", Kind: kindBodyRegex, Pattern: `LHH-[0-9]+`, Priority: 100, Enabled: true},
			// The SPEC matches `subject+"\n"+body_text`. A rule that only ever saw
			// body_text would miss every Jira notification mail, whose ticket key
			// lives in the subject line.
			msg:     Message{ThreadKey: msgSlackTreetop, Subject: "[Avviato] (LHH-23637) Recommendation ranking is stale", BodyText: "see the ticket"},
			matched: true,
		},
		{
			name:    "body_regex is CASE SENSITIVE unless the author writes (?i)",
			rule:    Rule{ID: 1, Project: "reengine", Kind: kindBodyRegex, Pattern: `LHH-[0-9]+`, Priority: 100, Enabled: true},
			msg:     Message{ThreadKey: msgSlackTreetop, BodyText: "lhh-23637 is fixed"},
			matched: false,
			why: "SPEC §1: 'Case-insensitivity is spelled by the rule author as (?i)'. An implementation that " +
				"lower-cases the corpus makes (?i) meaningless and silently widens every rule",
		},
		{
			name:    "body_regex honours (?i) when the author asks for it",
			rule:    Rule{ID: 10, Project: "reengine", Kind: kindBodyRegex, Pattern: `(?i)recommendation engine`, Priority: 90, Enabled: true},
			msg:     Message{ThreadKey: msgSlackTreetop, BodyText: "any update on the Recommendation Engine rollout?"},
			matched: true,
		},
		{
			name: "sender matches a SUBSTRING of the raw From header",
			rule: Rule{ID: 2, Project: "reengine", Kind: kindSender, Pattern: `jira@avviato.atlassian.net`, Priority: 100, Enabled: true},
			// Fact 9: normalized_messages.sender is the raw From header for gmail,
			// `Name <addr>` — equality would match nothing.
			msg:     Message{ThreadKey: "gmail:salvador@example.test:18f2c", Sender: senderAvviatoJira, BodyText: "LHH-23637 updated"},
			matched: true,
		},
		{
			name:    "sender is CASE INSENSITIVE",
			rule:    Rule{ID: 2, Project: "reengine", Kind: kindSender, Pattern: `jira@avviato.atlassian.net`, Priority: 100, Enabled: true},
			msg:     Message{ThreadKey: "gmail:salvador@example.test:18f2d", Sender: `JIRA <Jira@Avviato.Atlassian.NET>`, BodyText: "ticket updated"},
			matched: true,
			why:     "SPEC §1: 'case-insensitive substring of the raw From/author string' — mail addresses arrive in any case",
		},
		{
			name:    "sender does not match a different address",
			rule:    Rule{ID: 2, Project: "reengine", Kind: kindSender, Pattern: `jira@avviato.atlassian.net`, Priority: 100, Enabled: true},
			msg:     Message{ThreadKey: "gmail:salvador@example.test:18f2e", Sender: `Jira <jira@treetopllc.atlassian.net>`, BodyText: "ticket updated"},
			matched: false,
		},
		{
			name:    "thread_key_prefix matches at the START of the key",
			rule:    Rule{ID: 3, Project: "collaboratory", Kind: kindKeyPrefix, Pattern: `jira:treetopllc.jira.com:WEB-`, Priority: 50, Enabled: true},
			msg:     Message{ThreadKey: msgJiraWEBKey, BodyText: "deploy blocked"},
			matched: true,
		},
		{
			name:    "thread_key_prefix does not match a sibling project's key",
			rule:    Rule{ID: 3, Project: "collaboratory", Kind: kindKeyPrefix, Pattern: `jira:treetopllc.jira.com:WEB-`, Priority: 50, Enabled: true},
			msg:     Message{ThreadKey: msgJiraAPIKey, BodyText: "deploy blocked"},
			matched: false,
		},
		{
			name: "thread_key_prefix is a PREFIX, not a contains",
			rule: Rule{ID: 3, Project: "collaboratory", Kind: kindKeyPrefix, Pattern: `jira:treetopllc.jira.com:WEB-`, Priority: 50, Enabled: true},
			// The two kinds exist as separate enum values; if prefix were
			// implemented as Contains the enum would be one value with two names
			// and every prefix rule would silently widen.
			msg:     Message{ThreadKey: "mirror:" + msgJiraWEBKey, BodyText: "deploy blocked"},
			matched: false,
			why:     "thread_key_prefix implemented as strings.Contains makes the two criteria types indistinguishable",
		},
		{
			name:    "thread_key_contains matches mid-key",
			rule:    Rule{ID: 6, Project: "collaboratory", Kind: kindKeyContain, Pattern: `treetopllc.jira.com`, Priority: 50, Enabled: true},
			msg:     Message{ThreadKey: msgJiraWEBKey, BodyText: "deploy blocked"},
			matched: true,
		},
		{
			name:    "thread_key_contains does not match another site's key",
			rule:    Rule{ID: 6, Project: "collaboratory", Kind: kindKeyContain, Pattern: `treetopllc.jira.com`, Priority: 50, Enabled: true},
			msg:     Message{ThreadKey: "jira:sspataro.atlassian.net:SWT-17", BodyText: "deploy blocked"},
			matched: false,
		},
		{
			name:    "source_slack_workspace matches the workspace's synthetic account",
			rule:    Rule{ID: 8, Project: "collaboratory", Kind: kindSlackWS, Pattern: `T0HPR78RX`, Priority: 10, Enabled: true},
			msg:     Message{Source: acctCollabSlack, ThreadKey: msgSlackCollab, BodyText: "standup notes"},
			matched: true,
		},
		{
			name:    "source_slack_workspace does not match a different workspace",
			rule:    Rule{ID: 8, Project: "collaboratory", Kind: kindSlackWS, Pattern: `T0HPR78RX`, Priority: 10, Enabled: true},
			msg:     Message{Source: acctTreetopSlack, ThreadKey: msgSlackTreetop, BodyText: "standup notes"},
			matched: false,
		},
		{
			name: "source_slack_workspace does not match a non-slack account that merely contains the id",
			rule: Rule{ID: 8, Project: "collaboratory", Kind: kindSlackWS, Pattern: `T0HPR78RX`, Priority: 10, Enabled: true},
			// SPEC §1 binds this criterion to provider='slack_web' AND
			// account_email = pattern+'@slack-web.local'. A bare
			// strings.Contains(Source, pattern) would route a mailbox named after
			// the workspace into the project.
			msg:     Message{Source: "t0hpr78rx@gmail.example.test", ThreadKey: "gmail:t0hpr78rx@gmail.example.test:18f2f", BodyText: "standup notes"},
			matched: false,
			why:     "the criterion is the slack_web synthetic identity `{ws}@slack-web.local`, not a substring of any account name",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(tc.msg, []Rule{tc.rule})
			if tc.matched {
				assertMatched(t, got, tc.rule.ID, tc.rule.Project)
				return
			}
			why := tc.why
			if why == "" {
				why = "the rule does not cover this message"
			}
			assertUnmatched(t, got, why)
		})
	}
}

// ---- the workspace-id case landmine (SWT-13's costume in this ticket) ----------

// The rule author writes the workspace id as Slack spells it — `T0360B84U`, which
// is what the SPEC's fixture table and criterion 19 both say. The connector writes
// the account as `strings.ToLower(workspace.ID) + "@slack-web.local"`
// (slackweb/sink.go:28, reconcile.go:106, tools/delivery.go:1018 all agree). So a
// criterion spelled `account_email = pattern || '@slack-web.local'` compares
// `t0360b84u@...` against `T0360B84U@...` and matches NOTHING, forever, with no
// error anywhere — and the 40,452-message workspace the whole Q2 answer is about
// simply never routes.
//
// Both cases are asserted because the two strings must DIFFER for this test to
// mean anything: the fixture guard below fails if the pattern and the account's
// workspace segment are already the same spelling.
func TestEvaluate_SlackWorkspaceMatchesTheLowercasedSyntheticAccount(t *testing.T) {
	const workspace = "T0360B84U"
	account := strings.ToLower(workspace) + "@slack-web.local"
	if strings.HasPrefix(account, workspace) {
		t.Fatalf("fixture invalid: the account %q already starts with the pattern %q, so a case-blind "+
			"implementation would pass this test", account, workspace)
	}

	rule := Rule{ID: 9, Project: "collaboratory", Kind: kindSlackWS, Pattern: workspace, Priority: 1, Enabled: true}
	msg := Message{ID: 1, Source: account, ThreadKey: msgSlackTreetop, BodyText: "any thoughts on the sprint?"}

	got := Evaluate(msg, []Rule{rule})
	if got.Rule == nil {
		t.Fatalf("workspace rule %q did not match account %q. The connector LOWERCASES the workspace id into "+
			"account_email (slackweb/sink.go:28) while the runbook spells the rule as Slack does. Case-fold the "+
			"comparison, or the T0360B84U catch-all — 59%% of the corpus — never fires",
			workspace, account)
	}
	assertMatched(t, got, 9, "collaboratory")

	// The mirror: a rule written in the connector's lowercase spelling must work
	// too, so neither side of the pair is privileged.
	lower := Rule{ID: 9, Project: "collaboratory", Kind: kindSlackWS, Pattern: strings.ToLower(workspace), Priority: 1, Enabled: true}
	assertMatched(t, Evaluate(msg, []Rule{lower}), 9, "collaboratory")
}

// ---- criterion 4: first match wins, ordered priority DESC then id ASC ----------

func TestEvaluate_FirstMatchWinsByPriorityThenID(t *testing.T) {
	// Two rules that BOTH match the same message and name DIFFERENT projects —
	// that is the only shape in which "which one wins" means anything.
	high := Rule{ID: 100, Project: "reengine", Kind: kindBodyRegex, Pattern: `LHH-[0-9]+`, Priority: 100, Enabled: true}
	low := Rule{ID: 7, Project: "collaboratory", Kind: kindKeyPrefix, Pattern: `jira:treetopllc.jira.com:WEB-`, Priority: 50, Enabled: true}
	if high.Project == low.Project {
		t.Fatalf("fixture invalid: both rules name project %q, so the winner is unobservable", high.Project)
	}
	msg := Message{
		ID: 1, Source: acctJira, ThreadKey: msgJiraWEBKey,
		Subject: "WEB-1204 blocked", BodyText: "waiting on LHH-23637 before I can land this",
	}

	t.Run("higher priority wins even though its id is higher", func(t *testing.T) {
		// Note the id ordering is DELIBERATELY hostile: 100 > 7, so an
		// implementation that ordered by id alone would pick collaboratory.
		assertMatched(t, Evaluate(msg, []Rule{high, low}), high.ID, "reengine")
	})

	t.Run("the caller's slice order does not decide", func(t *testing.T) {
		// SPEC §2 says rules arrive pre-sorted. This asserts the guarantee does
		// not DEPEND on that: shuffled input must give the same answer. A routing
		// engine whose answer changes with the order of a slice is one refactor
		// away from silently sending ReEngine tickets to Collaboratory, and the
		// cost of sorting a few hundred configuration rows is nil.
		assertMatched(t, Evaluate(msg, []Rule{low, high}), high.ID, "reengine")
	})

	t.Run("equal priority: the LOWER id wins, whichever order they arrive in", func(t *testing.T) {
		// Criterion 4 verbatim: "a test pins that reordering two equal-priority
		// rules by id changes the winner".
		a := Rule{ID: 11, Project: "reengine", Kind: kindBodyRegex, Pattern: `LHH-[0-9]+`, Priority: 50, Enabled: true}
		b := Rule{ID: 12, Project: "collaboratory", Kind: kindKeyPrefix, Pattern: `jira:treetopllc.jira.com:WEB-`, Priority: 50, Enabled: true}
		if a.Priority != b.Priority {
			t.Fatalf("fixture invalid: priorities differ (%d vs %d), so this case does not test id ordering", a.Priority, b.Priority)
		}
		if a.ID == b.ID {
			t.Fatalf("fixture invalid: both rules have id %d", a.ID)
		}
		assertMatched(t, Evaluate(msg, []Rule{a, b}), a.ID, "reengine")
		assertMatched(t, Evaluate(msg, []Rule{b, a}), a.ID, "reengine")

		// Swap the ids between the two projects: the winner must swap with them.
		a.ID, b.ID = 12, 11
		assertMatched(t, Evaluate(msg, []Rule{a, b}), b.ID, "collaboratory")
		assertMatched(t, Evaluate(msg, []Rule{b, a}), b.ID, "collaboratory")
	})
}

// ---- disabled rules ------------------------------------------------------------

// `capture_rule_set_enabled` exists so a misfiring rule can be turned off without
// deleting it (SPEC "API / MCP tool changes"). The store filters on `WHERE
// enabled`, but the flag is on the struct, so a disabled rule that reached
// Evaluate must be inert — otherwise "disabled" depends entirely on one WHERE
// clause, and a --all report path or an opsctl list that forgets it re-enables
// production routing silently.
func TestEvaluate_DisabledRulesNeverMatch(t *testing.T) {
	disabled := Rule{ID: 1, Project: "reengine", Kind: kindBodyRegex, Pattern: `LHH-[0-9]+`, Priority: 100, Enabled: false}
	enabled := Rule{ID: 3, Project: "collaboratory", Kind: kindKeyPrefix, Pattern: `jira:treetopllc.jira.com:WEB-`, Priority: 50, Enabled: true}
	msg := Message{ID: 1, ThreadKey: msgJiraWEBKey, BodyText: "blocked on LHH-23637"}

	assertUnmatched(t, Evaluate(msg, []Rule{disabled}), "the only rule is disabled")

	// And a disabled high-priority rule must not shadow an enabled lower one.
	assertMatched(t, Evaluate(msg, []Rule{disabled, enabled}), enabled.ID, "collaboratory")
}

// ---- SPEC §3: the same match yields the dedup key ------------------------------

// Key derivation is asserted independently of `Source` (external_system): §3
// derives the key from the criterion and the regexes, and the DRIVER decides
// whether a key is acted on. Keeping the two apart is what lets an
// attribution-only rule be promoted to a task rule by setting one column, with no
// change to how its key is computed — and §9 site B's external_refs join depends
// on that key being the thread_key verbatim in the no-regex case.
func TestEvaluate_ExternalKeyDerivation(t *testing.T) {
	cases := []struct {
		name    string
		rule    Rule
		msg     Message
		wantKey string
		why     string
	}{
		{
			name:    "key_regex NULL on a body_regex rule reuses the pattern as the key regex",
			rule:    Rule{ID: 1, Project: "reengine", Kind: kindBodyRegex, Pattern: `LHH-[0-9]+`, Priority: 100, Enabled: true},
			msg:     Message{ThreadKey: msgSlackTreetop, BodyText: "Jira: LHH-23637 moved to In Progress"},
			wantKey: "LHH-23637",
			why:     "SPEC §3: 'the common case: LHH-[0-9]+ is both the selector and the key'",
		},
		{
			name: "key_regex applies to subject+body for a sender rule",
			rule: Rule{ID: 2, Project: "reengine", Kind: kindSender, Pattern: `jira@avviato.atlassian.net`,
				ExternalKeyRegex: rulePattern(`[A-Z]+-[0-9]+`), Priority: 100, Enabled: true},
			msg: Message{ThreadKey: "gmail:salvador@example.test:18f2c", Sender: senderAvviatoJira,
				Subject: "[JIRA] (LHH-23637) Recommendation ranking is stale", BodyText: "Salvador updated the issue"},
			wantKey: "LHH-23637",
		},
		{
			name: "key_regex applies to the THREAD KEY for thread-key criteria",
			rule: Rule{ID: 3, Project: "collaboratory", Kind: kindKeyPrefix, Pattern: `jira:treetopllc.jira.com:WEB-`,
				ExternalKeyRegex: rulePattern(`[A-Z]+-[0-9]+$`), Priority: 50, Enabled: true},
			// The body deliberately carries a DIFFERENT ticket key, so a
			// implementation that ran key_regex over the body instead of the
			// thread_key produces API-77 and this case catches it.
			msg:     Message{ThreadKey: msgJiraWEBKey, Subject: "", BodyText: "duplicate of API-77"},
			wantKey: "WEB-1204",
			why:     "SPEC §3: key_regex runs on 'the thread_key for the thread_key/workspace/person types'",
		},
		{
			name: "the first CAPTURE GROUP wins when the regex has one",
			rule: Rule{ID: 20, Project: "reengine", Kind: kindBodyRegex, Pattern: `avviato\.atlassian\.net/browse/`,
				ExternalKeyRegex: rulePattern(`browse/([A-Z]+-[0-9]+)`), Priority: 100, Enabled: true},
			msg:     Message{ThreadKey: msgSlackTreetop, BodyText: "see https://avviato.atlassian.net/browse/LHH-23637 for detail"},
			wantKey: "LHH-23637",
			why:     "SPEC §3: 'the first capture group, or the whole match if the regex has no group'",
		},
		{
			name:    "key_regex NULL on a non-body rule yields the thread_key verbatim",
			rule:    Rule{ID: 30, Project: "collaboratory", Kind: kindKeyContain, Pattern: `treetopllc.jira.com`, Priority: 50, Enabled: true},
			msg:     Message{ThreadKey: msgJiraWEBKey, BodyText: "no ticket key in this body"},
			wantKey: msgJiraWEBKey,
			why: "SPEC §3: the thread_key IS the dedup key for these — which is also what §9 site B's " +
				"external_refs join depends on",
		},
		{
			name: "a key_regex that matches nothing yields no key rather than a wrong one",
			rule: Rule{ID: 31, Project: "reengine", Kind: kindSender, Pattern: `jira@avviato.atlassian.net`,
				ExternalKeyRegex: rulePattern(`[A-Z]+-[0-9]+`), Priority: 100, Enabled: true},
			msg: Message{ThreadKey: "gmail:salvador@example.test:18f30", Sender: senderAvviatoJira,
				Subject: "Weekly digest", BodyText: "3 issues were updated this week"},
			wantKey: "",
			why: "an empty key must not become an external_refs row: `external_key=''` would collide every " +
				"keyless message of that system onto ONE task, forever",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(tc.msg, []Rule{tc.rule})
			assertMatched(t, got, tc.rule.ID, tc.rule.Project)
			if got.ExternalKey != tc.wantKey {
				why := tc.why
				if why == "" {
					why = "SPEC §3"
				}
				t.Errorf("Match.ExternalKey = %q, want %q — %s", got.ExternalKey, tc.wantKey, why)
			}
		})
	}
}

// ---- the acceptance rule set end to end (criteria 3 and 19, winner side) -------

// The ambiguity bookkeeping criteria 3 and 19 also demand (`matched_rule_ids`,
// `ambiguous`) is asserted where the SPEC puts it — on the capture_decisions row,
// in rules_integration_test.go. Match as declared carries only the winner, so
// this file pins the winner.
func TestEvaluate_FixtureRuleSet(t *testing.T) {
	cases := []struct {
		name        string
		msg         Message
		wantRuleID  int64
		wantProject string
		wantKey     string
		ignoreKey   bool // the rule's external_system is NULL: attribution only, no dedup key
		why         string
	}{
		{
			name: "criterion 19: an LHH ticket inside the Treetop workspace goes to ReEngine",
			msg: Message{ID: 1, Source: acctTreetopSlack, ThreadKey: msgSlackTreetop,
				Sender: "Jira APP", BodyText: "Salvador moved LHH-23637 to In Review"},
			wantRuleID: 1, wantProject: "reengine", wantKey: "LHH-23637",
			why: "one workspace feeds two projects and NOTHING but priority DESC separates them: rule 1 " +
				"(priority 100) must claim it before rule 9's priority-1 catch-all",
		},
		{
			name: "criterion 19: anything else in the Treetop workspace attributes to Collaboratory",
			msg: Message{ID: 2, Source: acctTreetopSlack, ThreadKey: msgSlackTreetop,
				Sender: "Marco", BodyText: "can you look at the staging deploy?"},
			wantRuleID: 9, wantProject: "collaboratory", ignoreKey: true,
			why: "the catch-all only ever sees what the specific rules did not claim. external_system is NULL " +
				"on this rule, so §3 says attribution only — no task, and the key is not asserted here",
		},
		{
			name: "criterion 3: an LHH mention inside a Collaboratory Jira thread still goes to ReEngine",
			msg: Message{ID: 3, Source: acctJira, ThreadKey: msgJiraWEBKey,
				Sender: "Jira <jira@treetopllc.atlassian.net>", Subject: "WEB-1204 comment",
				BodyText: "blocked until LHH-23637 ships"},
			wantRuleID: 1, wantProject: "reengine", wantKey: "LHH-23637",
			why: "SPEC §2's first 'priority is load-bearing' case, on a thread_key the connectors actually emit",
		},
		{
			name: "criterion 3 verbatim: an LHH mention in a collaboratory-www thread",
			msg: Message{ID: 4, Source: "salvador@example.test", ThreadKey: msgGitHubMailKey,
				Sender: "Marco <notifications@github.com>", Subject: "Re: [treetopllc/collaboratory-www] Ranking widget (PR #3179)",
				BodyText: "needs LHH-23637 first"},
			wantRuleID: 1, wantProject: "reengine", wantKey: "LHH-23637",
			why: "criterion 3 on the key shape production actually carries: a GitHub notification MAIL, whose " +
				"Message-ID embeds the repo path (see TestEvaluate_RepoRulesMatchGitHubNotificationMail). Rule 6 " +
				"claims that thread at priority 50; rule 1 takes it at 100",
		},
		{
			name: "a Collaboratory Jira thread with no LHH mention stays Collaboratory",
			msg: Message{ID: 5, Source: acctJira, ThreadKey: msgJiraAPIKey,
				Sender: "Jira <jira@treetopllc.atlassian.net>", Subject: "API-77", BodyText: "rate limiting is live"},
			wantRuleID: 4, wantProject: "collaboratory", wantKey: "API-77",
		},
		{
			name: "an Avviato Jira notification mail: rule 1 wins on id at equal priority",
			msg: Message{ID: 6, Source: "salvador@example.test", ThreadKey: "gmail:salvador@example.test:18f31",
				Sender: senderAvviatoJira, Subject: "[JIRA] (LHH-24001) Ranking job failed",
				BodyText: "View issue: https://avviato.atlassian.net/browse/LHH-24001"},
			wantRuleID: 1, wantProject: "reengine", wantKey: "LHH-24001",
			why: "rule 1 and rule 2 share priority 100, so the lower id decides — both name reengine, " +
				"so only the recorded rule id differs",
		},
		{
			name: "a digest from the Avviato Jira address routes on the SENDER alone",
			msg: Message{ID: 8, Source: "salvador@example.test", ThreadKey: "gmail:salvador@example.test:18f32",
				Sender: senderAvviatoJira, Subject: "Your Jira digest", BodyText: "3 issues were updated this week"},
			wantRuleID: 2, wantProject: "reengine", wantKey: "",
			why: "no ticket key anywhere in the text, so ONLY rule 2 can match — which is what makes this " +
				"case discriminating: it fails if the sender criterion was never implemented",
		},
		{
			name: "the Collaboratory Slack workspace attributes its own messages",
			msg: Message{ID: 7, Source: acctCollabSlack, ThreadKey: msgSlackCollab,
				Sender: "Intern", BodyText: "pushed the onboarding doc"},
			wantRuleID: 8, wantProject: "collaboratory", ignoreKey: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(tc.msg, fixtureRules())
			assertMatched(t, got, tc.wantRuleID, tc.wantProject)
			if !tc.ignoreKey && got.ExternalKey != tc.wantKey {
				t.Errorf("Match.ExternalKey = %q, want %q", got.ExternalKey, tc.wantKey)
			}
			if tc.why != "" && t.Failed() {
				t.Logf("why this case exists: %s", tc.why)
			}
		})
	}
}

// ---- a bad regex must not take the pass down --------------------------------

// Criterion 5 refuses an uncompilable pattern at INSERT time, which is the real
// guard. This is the second line: a row that predates the tool, or one written by
// psql, must make its own rule inert — not panic inside a CronJob and not stop the
// rules after it from matching. "A bad regex discovered inside a CronJob is a
// silent routing outage" (SPEC §1); a panic is a LOUD outage of every connector.
func TestEvaluate_UncompilablePatternIsInertNotFatal(t *testing.T) {
	bad := Rule{ID: 1, Project: "reengine", Kind: kindBodyRegex, Pattern: `LHH-[0-9`, Priority: 100, Enabled: true}
	good := Rule{ID: 2, Project: "collaboratory", Kind: kindKeyPrefix, Pattern: `jira:treetopllc.jira.com:WEB-`, Priority: 50, Enabled: true}
	msg := Message{ID: 1, ThreadKey: msgJiraWEBKey, BodyText: "LHH-23637"}

	assertUnmatched(t, Evaluate(msg, []Rule{bad}), "an uncompilable pattern matches nothing")
	assertMatched(t, Evaluate(msg, []Rule{bad, good}), good.ID, "collaboratory")

	// Same for a bad key_regex: the rule still routes, it just yields no key.
	badKey := Rule{ID: 3, Project: "reengine", Kind: kindBodyRegex, Pattern: `LHH-[0-9]+`,
		ExternalKeyRegex: rulePattern(`(LHH-[0-9]+`), Priority: 100, Enabled: true}
	got := Evaluate(msg, []Rule{badKey})
	assertMatched(t, got, badKey.ID, "reengine")
	if got.ExternalKey != "" {
		t.Errorf("Match.ExternalKey = %q from an uncompilable key_regex; want empty (attribution survives, "+
			"the dedup key does not)", got.ExternalKey)
	}
}

// ---- the repo rules fire through GitHub notification MAIL ----------------------

// How a repo path ends up inside a thread_key, which is not obvious and is worth
// writing down where the rule lives:
//
//	GitHub sets Message-ID `<{owner}/{repo}/{kind}/{number}@github.com>` on
//	notification mail; google/normalize.go builds `gmail:{account}:{Message-ID}`;
//	so the repo path is INSIDE the key, and `thread_key_contains
//	treetopllc/collaboratory-www` matches it.
//
// Measured against production on 2026-08-28: 199 thread_keys contain
// `treetopllc/`, all gmail — 182 collaboratory-www, 11 gonoble. Those counts are
// prose, not assertions: the corpus is live, and a frozen literal cries wolf every
// day a notification arrives.
//
// FRAGILITY, stated out loud because the rules depend on something we do not
// control: the match survives only while GitHub keeps that Message-ID format. If
// it changes, the two repo rules stop matching SILENTLY — a rule that matches
// nothing looks exactly like a repo nobody mentioned. The fixture guard below is
// where the next reader finds out why, and the shadow report's unmatched
// thread-key leaderboard (criterion 14) is where it would show up in operation.
func TestEvaluate_RepoRulesMatchGitHubNotificationMail(t *testing.T) {
	const account = "sspataro@gmail.com"

	// The mechanism, built rather than asserted as one opaque literal: the key is
	// the account plus GitHub's Message-ID, and the repo path is only in the key
	// because that Message-ID puts it there.
	githubMessageID := func(repo, kind string, number int) string {
		return fmt.Sprintf("<%s/%s/%d@github.com>", repo, kind, number)
	}
	gmailThreadKey := func(messageID string) string { return "gmail:" + account + ":" + messageID }

	wwwKey := gmailThreadKey(githubMessageID("treetopllc/collaboratory-www", "pull", 3179))
	if wwwKey != msgGitHubMailKey {
		t.Fatalf("fixture drift: built %q, but the recorded production sample is %q", wwwKey, msgGitHubMailKey)
	}
	if !strings.Contains(wwwKey, "treetopllc/collaboratory-www") {
		t.Fatalf("fixture invalid: the repo path is not in the thread_key %q. That is the WHOLE mechanism — "+
			"if GitHub changes its Message-ID format the two repo rules match nothing, silently", wwwKey)
	}

	t.Run("collaboratory-www notification attributes to Collaboratory", func(t *testing.T) {
		msg := Message{
			ID: 1, Source: account, ThreadKey: wwwKey,
			Sender:   "Marco <notifications@github.com>",
			Subject:  "Re: [treetopllc/collaboratory-www] Ranking widget (PR #3179)",
			BodyText: "review requested",
		}
		assertMatched(t, Evaluate(msg, fixtureRules()), 6, "collaboratory")
	})

	t.Run("gonoble notification attributes to Collaboratory", func(t *testing.T) {
		// gonoble is Collaboratory's API — a second repo, so the two rules are
		// separate rows and both must work.
		msg := Message{
			ID: 2, Source: account, ThreadKey: msgGitHubGonobleKey,
			Sender: "Marco <notifications@github.com>", BodyText: "opened an issue",
		}
		assertMatched(t, Evaluate(msg, fixtureRules()), 7, "collaboratory")
	})

	t.Run("a notification for another repo is not claimed", func(t *testing.T) {
		// Substring matching on a key is only safe while the substring is
		// specific: `treetopllc/` alone would sweep in every repo of the org.
		msg := Message{
			ID: 3, Source: account,
			ThreadKey: gmailThreadKey(githubMessageID("treetopllc/unrelated-tooling", "issues", 12)),
			Sender:    "Marco <notifications@github.com>", BodyText: "opened an issue",
		}
		assertUnmatched(t, Evaluate(msg, fixtureRules()), "no rule covers treetopllc/unrelated-tooling")
	})
}
