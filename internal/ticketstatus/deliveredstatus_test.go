package ticketstatus_test

// The fold and the membership test (SWT-34,
// docs/tickets/qa-delivered-drop_SPEC.md criteria 9, 10 and 11): the ONE place
// a configured status name is compared to an observed one.
//
// Zero I/O, and the file under test imports only `strings` — criterion 9. This
// table is the whole behavioural contract of E2/E4/E5; the structural half
// (never re-spelled in SQL, criterion 12) lives in
// delivered_structure_test.go.
//
// GREENFIELD NOTE — EXPECTED RED. internal/ticketstatus/deliveredstatus.go does
// not exist, so `go build ./internal/ticketstatus/` fails with "undefined:
// NormalizeStatusName" / "undefined: IsDeliveredStatus" and the whole package's
// test binary refuses to link. That IS the red state for a spec-first test.
// Verified in the authoring session against a throwaway stub declaring both
// functions and returning zero values: every assertion below then fires on its
// own merits (`NormalizeStatusName("TT-In QA") = "" want "tt-in qa"`,
// `IsDeliveredStatus(...) = false want true`) rather than on the missing file.
//
// ---- IMPOSED surface (deliveredstatus.go) -----------------------------------
//
// Both signatures are fixed by the SPEC (criterion 9) and neither may grow a
// third argument: a per-project case-sensitivity or regex knob is an untyped
// predicate by another name (Out of scope).
//
//	// NormalizeStatusName folds a human-typed workflow label: lowercase, and
//	// every run of UNICODE whitespace collapsed to one space, ends trimmed.
//	// strings.ToLower(strings.Join(strings.Fields(s), " ")) — strings.Fields
//	// splits on unicode.IsSpace, which is what makes the NBSP case work, and is
//	// the same reasoning internal/textmatch records.
//	func NormalizeStatusName(s string) string
//
//	// IsDeliveredStatus reports whether an observed status name is a member of
//	// a project's configured delivered set. EXACT on the normalized form —
//	// never substring, never prefix (E2).
//	func IsDeliveredStatus(name string, configured []string) bool
//
// WHY THE TEST DATA IS THE REAL TREETOP NAMES. Criterion 30 bans "TT-In QA",
// "TT-In Review" and "TT-Verified" from internal/ticketstatus' and the jira
// readers' NON-TEST sources — the names live in the database or they do not
// exist. Test files are out of that scan's scope on purpose, so the fixtures
// here are the exact strings the runbook's arming UPDATE seeds.

import (
	"testing"

	"github.com/sspataro57/switchboard/internal/ticketstatus"
)

// The one entry the runbook arms collaboratory with (E11: TT-In Review is NOT
// guessed into the set), and the NBSP variant a browser copy/paste produces.
const (
	qdQA     = "TT-In QA"
	qdQANBSP = "TT-In\u00a0QA" // U+00A0, the realistic typo E2 exists for
)

// ---- criterion 10: the fold ---------------------------------------------------

// "TT-In QA" -> "tt-in qa"; "  TT-In QA " -> same; NBSP -> same; "tt-in  qa" ->
// same; "" -> ""; "   " -> "".
//
// Why normalize at all, in one sentence: these are human-typed labels on BOTH
// sides — Jira serialises whatever an admin typed into the workflow column, and
// the configured entry is pasted into a psql UPDATE by hand. A trailing space or
// an NBSP would make an armed set match nothing, and an armed feature that
// matches nothing looks exactly like an unarmed one.
func TestNormalizeStatusName_FoldsCaseAndUnicodeWhitespace(t *testing.T) {
	for _, tc := range []struct{ in, want, why string }{
		{qdQA, "tt-in qa", "the base case: lowercase, single spaces preserved"},
		{"  TT-In QA ", "tt-in qa", "leading and trailing whitespace is trimmed"},
		{qdQANBSP, "tt-in qa", "an NBSP is UNICODE whitespace: strings.Fields splits on unicode.IsSpace, " +
			"which a POSIX \\s in SQL does not — that difference is exactly why the comparison never goes into SQL"},
		{"tt-in  qa", "tt-in qa", "a run of whitespace collapses to ONE space"},
		{"TT-In\tQA", "tt-in qa", "a tab is whitespace too; the fold is over runs, not over the space character"},
		{"", "", "an empty name folds to empty — E5's input, and it must not fold to something matchable"},
		{"   ", "", "whitespace only folds to empty — E4's configured entry, and it must not become a wildcard"},
	} {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			if got := ticketstatus.NormalizeStatusName(tc.in); got != tc.want {
				t.Errorf("NormalizeStatusName(%q) = %q, want %q — %s", tc.in, got, tc.want, tc.why)
			}
		})
	}
}

// Idempotence, because the fold is applied to a value that may already have been
// folded (the report re-folds the stored status_name to print delivered=yes|no,
// criterion 27) and a fold that is not idempotent makes the second application
// disagree with the first.
func TestNormalizeStatusName_IsIdempotent(t *testing.T) {
	for _, in := range []string{qdQA, qdQANBSP, "  TT-In  Review  ", "", "   "} {
		once := ticketstatus.NormalizeStatusName(in)
		if twice := ticketstatus.NormalizeStatusName(once); twice != once {
			t.Errorf("NormalizeStatusName is not idempotent: %q -> %q -> %q. The report folds a stored "+
				"name a second time to compute delivered=yes|no with the SAME function the decision "+
				"used (criterion 27); a non-idempotent fold makes those two answers disagree",
				in, once, twice)
		}
	}
}

// ---- criterion 11: membership is EXACT on the normalized form ----------------

func TestIsDeliveredStatus_ExactOnTheNormalizedForm(t *testing.T) {
	for _, tc := range []struct {
		name       string
		observed   string
		configured []string
		want       bool
		why        string
	}{
		{
			name: "plain member", observed: qdQA, configured: []string{qdQA}, want: true,
			why: "the base case — without it every negative below is scanning a predicate that says no to everything",
		},
		{
			name: "case differs on the OBSERVED side", observed: "tt-in qa", configured: []string{qdQA}, want: true,
			why: "Jira serialises whatever an admin typed; the fold is applied to both sides, not just to ours",
		},
		{
			name: "case and spacing differ on the CONFIGURED side", observed: qdQA,
			configured: []string{"  tt-in   QA "}, want: true,
			why: "the configured entry is pasted into a psql UPDATE by hand — this is the half criterion 24 " +
				"then proves again THROUGH POSTGRES, because a Go table test cannot show the value survived a column",
		},
		{
			name: "NBSP on the configured side", observed: qdQA, configured: []string{qdQANBSP}, want: true,
			why: "an NBSP copied out of a browser must not make an armed set match nothing — an armed feature " +
				"that matches nothing is indistinguishable from an unarmed one",
		},
		{
			name: "a member later in the set", observed: "TT-Verified",
			configured: []string{qdQA, "TT-In Review", "TT-Verified"}, want: true,
			why: "membership is over the whole set, not over its first element",
		},
		{
			name: "not in the set", observed: "TT-Work In Progress", configured: []string{qdQA}, want: false,
			why: "TT-Work In Progress and TT-In QA are BOTH indeterminate — separating them is the entire " +
				"reason this configured set exists (the design tension)",
		},
		{
			name: "a PREFIX of a member is not a member", observed: "TT-In", configured: []string{qdQA}, want: false,
			why: "E2: exact, never prefix",
		},
		{
			name: "TT-QA against a set containing TT-In QA", observed: "TT-QA", configured: []string{qdQA}, want: false,
			why: "criterion 11's named case: strings.Contains(name, \"QA\") would match this",
		},
		{
			name: "a member is not a substring match either", observed: "TT-In QA Blocked", configured: []string{qdQA},
			want: false,
			why: "criterion 11's other named case — TT-In QA Blocked and TT-Needs QA Rework mean the ball IS " +
				"in his court, and a partial predicate over a human label would drop them silently",
		},
		{
			name: "TT-QA Blocked is not a member", observed: "TT-QA Blocked", configured: []string{qdQA}, want: false,
			why: "the substring-rejection case stated the other way round: neither side contains the other, " +
				"and both share the token 'QA'",
		},
		{
			name: "an empty configured set matches nothing", observed: qdQA, configured: nil, want: false,
			why: "E1: '{}' is today's behaviour EXACTLY — every unarmed project, which is all but one",
		},
		{
			name: "an empty configured SLICE matches nothing", observed: qdQA, configured: []string{}, want: false,
			why: "nil and empty must behave identically; the driver hands over whatever pgx scanned from TEXT[]",
		},
		{
			name: "an empty entry is IGNORED, not a wildcard", observed: qdQA, configured: []string{""}, want: false,
			why: "E4: ARRAY[''] or a stray trailing comma must not become 'matches every status', which would " +
				"drop a client's whole board with no error",
		},
		{
			name: "a whitespace-only entry is IGNORED too", observed: qdQA, configured: []string{"  ", " "},
			want: false,
			why:  "E4 again, after the fold: ' ' folds to empty and must not match",
		},
		{
			name: "an empty entry does not even match an empty NAME", observed: "", configured: []string{""},
			want: false,
			why: "E4/E5 together — the one pair where a naive `folded(name) == folded(entry)` says yes and " +
				"drops every ticket whose status name we could not read",
		},
		{
			name: "an empty observed name against a real set", observed: "", configured: []string{qdQA}, want: false,
			why: "E5: reaching the decision at all means the CATEGORY was readable; only the name is missing. " +
				"'Not a member' keeps the task on the board (the fail-safe direction) and keeps the reopen path alive",
		},
		{
			name: "a whitespace-only observed name", observed: "   ", configured: []string{qdQA}, want: false,
			why: "the same, after the fold",
		},
		{
			name: "an empty entry beside a real one still matches the real one", observed: qdQA,
			configured: []string{"", qdQA}, want: true,
			why: "E4 ignores the empty entry — it must not poison the rest of the set, or a stray trailing " +
				"comma would silently disarm a project instead of loudly wildcarding it",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := ticketstatus.IsDeliveredStatus(tc.observed, tc.configured); got != tc.want {
				t.Errorf("IsDeliveredStatus(%q, %q) = %v, want %v — %s",
					tc.observed, tc.configured, got, tc.want, tc.why)
			}
		})
	}
}

// The membership test does not MUTATE the configured slice. The driver hands
// the same []string to every candidate of a project (one row read once), so an
// in-place normalization would fold the set on the first ref and compare a
// twice-folded set on the rest — which happens to be harmless for THIS fold and
// would not be for the next one. Cheap to pin, invisible to catch later.
func TestIsDeliveredStatus_DoesNotMutateTheConfiguredSet(t *testing.T) {
	configured := []string{"  TT-In QA  ", "TT-Verified"}
	before := append([]string(nil), configured...)

	if !ticketstatus.IsDeliveredStatus(qdQA, configured) {
		t.Fatalf("IsDeliveredStatus(%q, %q) = false, want true — the positive control for the "+
			"no-mutation assertion below", qdQA, configured)
	}
	for i := range before {
		if configured[i] != before[i] {
			t.Errorf("IsDeliveredStatus rewrote its configured argument: %q -> %q. The driver reads the "+
				"projects column ONCE and passes the same slice to every candidate of that project",
				before, configured)
			break
		}
	}
}
