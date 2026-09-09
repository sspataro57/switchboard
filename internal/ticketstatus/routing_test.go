package ticketstatus_test

// Unit tests for SWT-32 (docs/tickets/jira-status-sync_SPEC.md) criteria 14, 15
// and 17: which lookup account, if any, may fetch a given ticket key — and the
// defensive freshness reader that decides whether it fetches at all. ZERO I/O
// (RouteLookup is pure; LookupTTL reads one environment variable through
// t.Setenv and touches nothing else).
//
// Kept out of decide_test.go on purpose: that file is the purity proof and
// structure_test.go parses its import block, so anything importing
// internal/connector/jira or reading the environment belongs here instead.
//
// GREENFIELD NOTE — EXPECTED RED. internal/ticketstatus does not exist:
// "undefined: ticketstatus.RouteLookup", "undefined: ticketstatus.LookupTTL".
//
// ---- IMPOSED surface ---------------------------------------------------------
//
//	// RouteLookup picks the lookup account whose declared scopes claim key's
//	// project prefix (D18). outcome is one of "routed" | "unpolled" |
//	// "ambiguous" — the counter names criterion 43 prints. It takes the KEY and
//	// the accounts and nothing else: external_refs.external_url is not a
//	// parameter, which is criterion 15's guarantee made structural.
//	func RouteLookup(key string, accounts []jira.Account) (acct jira.Account, outcome string)
//
//	// LookupTTL reads TICKET_LOOKUP_TTL (a Go duration), defaulting to
//	// DefaultLookupTTL. Anything unparseable or non-positive falls back —
//	// capture's ObserveHorizon/RulesHorizon shape.
//	const DefaultLookupTTL = time.Hour
//	func LookupTTL() time.Duration
//
// jira.Account is reused rather than mirrored: D17 changes the provider VALUE,
// not the row shape, so a lookup account is an ordinary Account whose `scopes`
// mean "prefixes I may fetch by key" instead of "projects I poll".

import (
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/jira"
	"github.com/sspataro57/switchboard/internal/ticketstatus"
)

func lookupAcct(id int64, projects ...string) jira.Account {
	return jira.Account{
		ID: id, Email: "lookup@example.com",
		SiteBaseURL: "https://avviato.example.net", Projects: projects,
	}
}

// ---- criterion 14: routing by key prefix against `scopes` --------------------

// "LHH-123 -> the account declaring LHH; WEB-1 with no claiming account ->
// unpolled, no HTTP call; two accounts declaring LHH -> ambiguous, no HTTP call,
// nothing acted on; a key whose prefix is not a legal Jira project key is
// unpolled, never a fetch."
//
// The prefix is the routing key for two reasons worth restating where the test
// lives: it preserves 09-jira-github-connectors' mandatory-scoping property
// verbatim (an account can only ever fetch keys whose prefix it declares), and
// the alternative — routing on external_refs.external_url's host — would trust a
// column `link_external_ref` writes as AGENT-FACING FREE TEXT, letting a crafted
// ref aim our token at a host of the caller's choosing.
func TestRouteLookup_ByDeclaredPrefix(t *testing.T) {
	avviato := lookupAcct(1, "LHH")
	other := lookupAcct(2, "ABC", "DEF")

	for _, tc := range []struct {
		name, key   string
		accounts    []jira.Account
		wantOutcome string
		wantAcct    int64
		why         string
	}{
		{
			name: "the declaring account wins", key: "LHH-123",
			accounts: []jira.Account{other, avviato}, wantOutcome: "routed", wantAcct: 1,
			why: "the ONE case that must work: reengine's tickets reach the Avviato token",
		},
		{
			name: "a second declared prefix on the same account", key: "DEF-9",
			accounts: []jira.Account{other, avviato}, wantOutcome: "routed", wantAcct: 2,
			why: "scopes is a set; an account may legitimately claim several projects",
		},
		{
			name: "no claimant", key: "WEB-1",
			accounts: []jira.Account{other, avviato}, wantOutcome: "unpolled",
			why: "exactly today's behaviour for collaboratory-shaped keys with no lookup account: " +
				"counted, and NOTHING happens to the ref (criterion 30)",
		},
		{
			name: "two claimants", key: "LHH-5",
			accounts: []jira.Account{avviato, lookupAcct(3, "LHH")}, wantOutcome: "ambiguous",
			why: "the multi-match refusal precedent: refusing is reversible, while guessing spends a " +
				"token against a site nobody chose and can close a task from the wrong snapshot",
		},
		{
			name: "no accounts at all", key: "LHH-5",
			accounts: nil, wantOutcome: "unpolled",
			why: "before Salvador stores the Avviato token, every reengine ref is unpolled and the " +
				"status half still runs (D21)",
		},
		{
			name: "prefix must match WHOLE, not as a prefix-of-a-prefix", key: "LHHX-1",
			accounts: []jira.Account{avviato}, wantOutcome: "unpolled",
			why: "`LHHX` is a different project; a HasPrefix match on the raw key would send another " +
				"project's issues to this token",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			acct, outcome := ticketstatus.RouteLookup(tc.key, tc.accounts)
			if outcome != tc.wantOutcome {
				t.Fatalf("RouteLookup(%q) outcome = %q, want %q — %s", tc.key, outcome, tc.wantOutcome, tc.why)
			}
			if tc.wantOutcome == "routed" && acct.ID != tc.wantAcct {
				t.Errorf("RouteLookup(%q) chose account %d, want %d", tc.key, acct.ID, tc.wantAcct)
			}
			if tc.wantOutcome != "routed" && acct.ID != 0 {
				t.Errorf("RouteLookup(%q) returned account %d alongside outcome %q; a refusal must "+
					"hand back nothing to fetch with — %s", tc.key, acct.ID, outcome, tc.why)
			}
		})
	}
}

// "a key whose prefix is not a legal Jira project key is unpolled, never a
// fetch."
//
// The ref's external_key is written by `link_external_ref`, which IS on the MCP
// agent surface with a free-text external_key (the SWT-20 argument against
// external_refs as a provenance store). So the shapes below are not typos to be
// tolerated — they are the input an untrusted caller controls, and every one of
// them must fail CLOSED, before any HTTP request exists to be aimed.
func TestRouteLookup_RefusesKeysThatAreNotIssueKeys(t *testing.T) {
	accounts := []jira.Account{lookupAcct(1, "LHH"), lookupAcct(2, "lhh")}
	for _, key := range []string{
		"",             // nothing at all
		"LHH",          // no issue number
		"LHH-",         // no issue number, with the separator
		"lhh-123",      // lowercase: Jira project keys are uppercase
		"1LH-123",      // a key may not start with a digit
		"LHH 123",      // a space
		"../LHH-123",   // a path escape aimed at GetIssue's URL
		"LHH-123/../x", // the same, on the other side
		"LHH-abc",      // no issue number
	} {
		key := key
		t.Run("key="+key, func(t *testing.T) {
			acct, outcome := ticketstatus.RouteLookup(key, accounts)
			if outcome != "unpolled" {
				t.Errorf("RouteLookup(%q) = account %d / %q, want unpolled. external_key is agent-facing "+
					"free text; a key the router cannot READ must never become a request, and "+
					"`unpolled` is the counted, reversible refusal", key, acct.ID, outcome)
			}
		})
	}
}

// ---- criterion 17: the defensive freshness reader ---------------------------

// "Unit tests for the defensive duration reader: unset -> default, \"3600\" ->
// default, \"0s\" -> default, \"5m\" -> 5m."
//
// The same shape capture's horizon uses, and for the same recorded reason: "3600"
// is the realistic typo and it is NOT a Go duration — read as 3600ns it would
// make every candidate stale forever, so a */15 CronJob would re-GET every
// candidate four times an hour, which is exactly what D20 exists to prevent.
func TestLookupTTL_DefendsAgainstEveryUnusableValue(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		set       bool
		want      time.Duration
	}{
		{name: "unset", set: false, want: ticketstatus.DefaultLookupTTL},
		{name: "empty", env: "", set: true, want: ticketstatus.DefaultLookupTTL},
		{name: "bare number (the realistic typo)", env: "3600", set: true, want: ticketstatus.DefaultLookupTTL},
		{name: "zero", env: "0s", set: true, want: ticketstatus.DefaultLookupTTL},
		{name: "negative", env: "-5m", set: true, want: ticketstatus.DefaultLookupTTL},
		{name: "not a duration at all", env: "an hour", set: true, want: ticketstatus.DefaultLookupTTL},
		{name: "a real duration", env: "5m", set: true, want: 5 * time.Minute},
		{name: "a longer one", env: "6h", set: true, want: 6 * time.Hour},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("TICKET_LOOKUP_TTL", tc.env)
			}
			if got := ticketstatus.LookupTTL(); got != tc.want {
				t.Errorf("LookupTTL() with TICKET_LOOKUP_TTL=%q (set=%v) = %v, want %v. Anything "+
					"unparseable or non-positive falls back to the default — a misconfigured TTL must "+
					"cost nothing worse than today's behaviour", tc.env, tc.set, got, tc.want)
			}
		})
	}
}

// The default itself, named rather than assumed: D20 says one hour, and the
// value is the difference between 36 GETs a day and 3,456.
func TestDefaultLookupTTL_IsOneHour(t *testing.T) {
	if ticketstatus.DefaultLookupTTL != time.Hour {
		t.Errorf("DefaultLookupTTL = %v, want 1h (D20). Fine at 36 tasks, careless at 500",
			ticketstatus.DefaultLookupTTL)
	}
}
