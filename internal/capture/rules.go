package capture

// The capture-rules evaluator (SWT-17, docs/tickets/capture-rules_SPEC.md §1-§3).
//
// This file is the PURE half of the engine and the boundary is the point:
// acceptance criterion 2 requires that `Evaluate` live in a file with no
// database, no network, no provider adapter, no LLM and no environment read, so
// that "which project does this message belong to" stays answerable in a unit
// test forever. rules_structure_test.go enforces that mechanically by scanning
// this source for the tokens that would betray I/O — so keep new code here
// free of them, and put anything that needs a connection in rules_store.go.
//
// It is invariant 7 ("orchestrator purity") applied to the routing engine: the
// decision is a function of (message, rules), the driver decides what to DO with
// it, and the two are testable apart.
//
// Determinism is the other half of the contract. The same message and the same
// rule set must resolve the same way on every run and in every process, because
// the outcome is written to `capture_decisions` and, in live mode, becomes a task
// on a client's board. So evaluation depends on the rules' declared ordering
// (`priority DESC, id ASC`) and NOT on the order the caller happened to load them
// in — see Evaluate.

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// The six criteria types, spelled exactly as the `capture_rules.criteria_type`
// CHECK constraint spells them (SPEC §1). An unknown kind matches nothing —
// evaluation never fails a connector run over a row it does not understand.
const (
	KindBodyRegex            = "body_regex"
	KindSender               = "sender"
	KindThreadKeyPrefix      = "thread_key_prefix"
	KindThreadKeyContains    = "thread_key_contains"
	KindSourceSlackWorkspace = "source_slack_workspace"
	KindPerson               = "person"
)

// ValidKind reports whether kind is one of the six criteria types. Offered so the
// `capture_rule_add` tool's validation and this switch cannot drift apart: a kind
// the tool accepts but the evaluator ignores is a rule that is stored, listed,
// and silently matches nothing — the failure mode this repo has paid for three
// times (a constant discriminator, an inert time floor, one of two room columns).
func ValidKind(kind string) bool {
	switch kind {
	case KindBodyRegex, KindSender, KindThreadKeyPrefix,
		KindThreadKeyContains, KindSourceSlackWorkspace, KindPerson:
		return true
	}
	return false
}

// slackWebAccountSuffix is the synthetic-identity domain the slack_web connector
// gives a workspace: `strings.ToLower(workspace.ID) + "@slack-web.local"`
// (slackweb/sink.go, and reconcile.go and tools/delivery.go agree).
//
// It is re-spelled here rather than imported because criterion 2 forbids this
// file from importing a connector package. The comparison below is therefore
// case-FOLDED rather than exact: the connector lowercases the workspace id, while
// a rule author writes it as Slack does (`T0360B84U`, per the SPEC's fixture
// table). An exact comparison would match nothing, forever, with no error
// anywhere — SWT-13's canonicalization landmine in this ticket's costume.
const slackWebAccountSuffix = "@slack-web.local"

// Rule is one row of `capture_rules`, reduced to what evaluation needs.
type Rule struct {
	// ID is `capture_rules.id`. It is the tie-break at equal priority, so it is
	// load-bearing, not just an identifier.
	ID int64
	// Project is the project SLUG the rule attributes to (the store resolves
	// `project_id` to it). A rule with no project is inert: it cannot attribute.
	Project string
	// Kind is `criteria_type` — one of the Kind* constants above.
	Kind string
	// Pattern is `pattern`: a Go regexp for body_regex, a literal otherwise.
	Pattern string
	// Source carries the rule's `external_system` for the driver's benefit
	// (it becomes `external_refs.system`, and a NULL there means attribution
	// only, no task — SPEC §3).
	//
	// Evaluate does NOT read it, deliberately. Gating key derivation on it would
	// make external-key extraction untestable from this file, and the unit
	// fixtures leave it nil for every criteria type; whether a match becomes a
	// task or a bare attribution is the driver's decision, taken from this field
	// plus Match.ExternalKey.
	Source *string
	// ExternalKeyRegex is `key_regex`: the dedup key extractor. Nil means "derive
	// the key the default way for this kind" — see externalKey.
	ExternalKeyRegex *string
	// Priority orders rules; higher wins.
	Priority int
	// Enabled is `enabled`. The store filters on it, and Evaluate honours it
	// again: "disabled" must not depend on one WHERE clause being remembered by
	// every future caller.
	Enabled bool
}

// Message is one normalized message, reduced to what evaluation needs. Every
// field is copied from stored data; nothing here is derived or invented.
type Message struct {
	// ID is `normalized_messages.id`.
	ID int64
	// Source is the message's source account identity —
	// `source_accounts.account_email`. For slack_web that is the synthetic
	// `{lower(workspace)}@slack-web.local`, which is what the
	// source_slack_workspace criterion resolves through (SPEC §1: deliberately
	// the account link rather than a `slack:{ws}:` thread_key prefix, because the
	// account survives a thread_key format change).
	Source string
	// ThreadKey is `normalized_threads.thread_key`.
	ThreadKey string
	// Sender is `normalized_messages.sender` — for gmail the RAW From header
	// (`Name <addr>`), which is why sender matching is substring, not equality.
	Sender string
	// Subject is `normalized_messages.subject`.
	Subject string
	// BodyText is `normalized_messages.body_text`.
	BodyText string
	// ExternalMessageID is `normalized_messages.external_message_id`. Carried for
	// the driver's decision row; no criterion matches on it.
	ExternalMessageID string
	// Participants is `normalized_threads.participants` as people ids — the input
	// to the person criterion. It is empty for every thread but upwork's today
	// (the other three sinks hardcode '[]'), so person rules match nothing until
	// that is fixed; the criterion is implemented so no code change is needed
	// when it is.
	Participants []int64
}

// Match is the outcome of one evaluation. The zero Match — Rule nil, empty
// Project, empty key — means UNMATCHED, which is a first-class outcome: it is
// what triage consumes as its inbox (SPEC §8b).
type Match struct {
	// Rule points at the winning element of the rules slice passed to Evaluate,
	// so the caller can reach the fields evaluation does not use
	// (`external_system`, and the row id for `matched_rule_id`).
	Rule *Rule
	// Project is the winning rule's project slug, repeated here so a caller that
	// only needs the answer does not have to dereference.
	Project string
	// ExternalKey is the dedup key derived from the SAME match (SPEC §3). Empty
	// means no key could be derived — the driver must then NOT write an
	// `external_refs` row, because `external_key=''` would collide every keyless
	// message of that system onto one task, forever.
	ExternalKey string
}

// Evaluate returns the first matching rule's attribution, where "first" is by
// `priority DESC, id ASC` — the ordering the store's load query already applies.
//
// It re-derives that ordering here instead of trusting the caller's slice order.
// SPEC §2 says rules arrive pre-sorted, and they do; but a guarantee that holds
// only while every future caller remembers an ORDER BY is one refactor away from
// silently sending ReEngine tickets to Collaboratory, and scanning a few hundred
// configuration rows costs nothing. Ambiguity (two matched rules naming different
// projects) is recorded by the driver, never used to change this outcome — total
// and reproducible beats clever.
//
// Rules that are disabled, that name no project, or whose pattern is empty are
// skipped: each of those is a row that cannot honestly attribute anything, and an
// empty pattern would otherwise make a prefix or contains rule a catch-all.
// An uncompilable regex makes its own rule inert and nothing else — a bad row
// must not take a connector run down, and must not shadow the rules after it.
func Evaluate(msg Message, rules []Rule) Match {
	winner := -1
	for i := range rules {
		r := &rules[i]
		if !r.Enabled || r.Project == "" || r.Pattern == "" {
			continue
		}
		if !matchesRule(msg, r) {
			continue
		}
		if winner == -1 || outranks(r, &rules[winner]) {
			winner = i
		}
	}
	if winner == -1 {
		return Match{}
	}
	won := &rules[winner]
	return Match{Rule: won, Project: won.Project, ExternalKey: externalKey(msg, won)}
}

func outranks(a, b *Rule) bool {
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	return a.ID < b.ID
}

// matchesRule is the criteria table of SPEC §1, one case per criteria_type.
func matchesRule(msg Message, rule *Rule) bool {
	switch rule.Kind {
	case KindBodyRegex:
		re := compiled(rule.Pattern)
		return re != nil && re.MatchString(matchText(msg))

	case KindSender:
		// Case-insensitive SUBSTRING of the raw From/author string: the stored
		// sender is `Name <addr>` for gmail, and addresses arrive in any case.
		return strings.Contains(strings.ToLower(msg.Sender), strings.ToLower(rule.Pattern))

	case KindThreadKeyPrefix:
		return strings.HasPrefix(msg.ThreadKey, rule.Pattern)

	case KindThreadKeyContains:
		return strings.Contains(msg.ThreadKey, rule.Pattern)

	case KindSourceSlackWorkspace:
		// The whole synthetic identity, not a substring of it: a substring test
		// would route a mailbox merely named after the workspace into the project.
		return strings.EqualFold(msg.Source, rule.Pattern+slackWebAccountSuffix)

	case KindPerson:
		// The pattern is a people.id as text, so the comparison is on the ID —
		// not on the rendered array, where "4242" is a substring of "42420".
		id, err := strconv.ParseInt(strings.TrimSpace(rule.Pattern), 10, 64)
		if err != nil {
			return false
		}
		for _, p := range msg.Participants {
			if p == id {
				return true
			}
		}
		return false
	}
	// An unknown criteria_type matches nothing.
	return false
}

// matchText is the text a body_regex criterion is evaluated against: subject and
// body as ONE text, because a Jira notification mail carries its ticket key in
// the subject line and a Slack message has no subject at all.
//
// Case is NOT folded here. SPEC §1: "Case-insensitivity is spelled by the rule
// author as (?i)" — lower-casing the corpus would make (?i) meaningless and
// silently widen every rule.
func matchText(msg Message) string {
	return msg.Subject + "\n" + msg.BodyText
}

// keyText is the text a key_regex runs against: the same text the criterion
// matched (SPEC §3) — subject+body for the content criteria, the thread_key for
// the criteria that selected on the thread or its source.
func keyText(msg Message, kind string) string {
	switch kind {
	case KindBodyRegex, KindSender:
		return matchText(msg)
	default:
		return msg.ThreadKey
	}
}

// externalKey derives the dedup key from the SAME match that chose the project
// (SPEC §3), in three cases:
//
//   - an explicit key_regex, applied to the criterion's own text;
//   - no key_regex on a body_regex rule: the pattern is reused as the key regex
//     (the common case — `LHH-[0-9]+` is both the selector and the key);
//   - no key_regex otherwise: the thread_key verbatim, which is what makes a
//     thread-keyed `external_refs` row joinable against `normalized_threads`
//     (§9 site B depends on exactly that).
//
// A regex that does not match yields NO key rather than a wrong one, and so does
// an uncompilable one — attribution survives, the dedup key does not. The driver
// must treat an empty key as "attribute, do not create a task": an empty
// `external_key` written to `external_refs` would collide every keyless message
// of that system onto one task forever, and nothing would ever notice.
func externalKey(msg Message, rule *Rule) string {
	if rule.ExternalKeyRegex != nil && *rule.ExternalKeyRegex != "" {
		return extractKey(*rule.ExternalKeyRegex, keyText(msg, rule.Kind))
	}
	if rule.Kind == KindBodyRegex {
		return extractKey(rule.Pattern, matchText(msg))
	}
	return msg.ThreadKey
}

// extractKey returns the first capture group, or the whole match when the regex
// has no group, or "" when it does not match or does not compile.
func extractKey(pattern, text string) string {
	re := compiled(pattern)
	if re == nil {
		return ""
	}
	m := re.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	if len(m) > 1 {
		return m[1]
	}
	return m[0]
}

// patternCache memoizes compilation. Patterns are configuration rows — a bounded
// set, reloaded per pass — while a pass evaluates tens of thousands of messages,
// so compiling per message would dominate the run.
//
// A nil value is a REMEMBERED failure: an uncompilable pattern is inert, not an
// error, so it must not be re-compiled on every message either. Caching keeps
// Evaluate a pure function of its arguments (compilation is deterministic) and
// safe to call concurrently.
var patternCache sync.Map // pattern string -> *regexp.Regexp (nil = uncompilable)

// compiled returns the compiled pattern, or nil if it does not compile.
//
// Patterns are Go regexp (RE2): no backtracking, so a hostile rule cannot hang a
// connector run. Criterion 5 refuses an uncompilable pattern at INSERT time —
// this is the second line of defence, for a row written before that tool existed
// or written by hand in psql.
func compiled(pattern string) *regexp.Regexp {
	if v, ok := patternCache.Load(pattern); ok {
		re, _ := v.(*regexp.Regexp)
		return re
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		re = nil
	}
	patternCache.Store(pattern, re)
	return re
}
