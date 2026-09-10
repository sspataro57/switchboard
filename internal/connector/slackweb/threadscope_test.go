package slackweb_test

// SWT-33 criterion 14 — ONE spelling of "is this Slack thread key ROOTED", and
// the structural scan that keeps it one. Copies the shape of
// internal/connector/upworkcrm/keyspelling_test.go, retargeted at the slack key.
//
// Plain unit test on purpose: no build tag, no database, no browser, so it runs
// under `go test ./...` and on every pass.
//
// THE RULE. `channelThreadKey` BUILDS the key (`slack:{ws}:{conv}`) and
// NormalizeMessage appends `:{thread_root}` when the message is threaded
// (normalize.go:71-76). Exactly one exported helper READS that difference back
// out, and it lives beside the builder. No SQL anywhere may `split_part`, `LIKE`
// or `||` a slack thread key, and internal/classify must not re-spell the rule
// (its half of the scan is in internal/classify/inquiry_structure_test.go).
//
// WHY IT MATTERS HERE more than anywhere: the rooted/unrooted distinction is
// LOAD-BEARING for what a verdict CLAIMS. `thread` means a later outbound row is
// genuinely a reply in this thread; `conversation` means only that Salvador has
// spoken in the channel since — measured 2026-09-10 at 113 thread-exact keys vs
// 83 conversation-level, with ONE conversation-level key holding 9,704 messages.
// Collapse the two and the report hides real open inquiries behind "answered".
//
// ---- IMPOSED SURFACE ---------------------------------------------------------
//
//	// The SPEC fixes "ONE exported helper in internal/connector/slackweb
//	// (beside channelThreadKey, which builds it)"; the name is this file's.
//	func IsRootedThreadKey(threadKey string) bool
//
// GREENFIELD NOTE — EXPECTED RED. slackweb.IsRootedThreadKey does not exist, so
// this file compile-FAILS internal/connector/slackweb with "undefined:
// slackweb.IsRootedThreadKey". The repo-wide SQL scan at the bottom is GREEN
// today and is a guard, not a discovery — its job is to STAY green once the
// inquiry lane starts needing the answer.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
)

// ---- the helper ---------------------------------------------------------------

func TestIsRootedThreadKey(t *testing.T) {
	cases := []struct {
		key    string
		rooted bool
		why    string
	}{
		{"slack:T0HPR78RX:C07ABCDEF:p1757000000000100", true,
			"a threaded message: NormalizeMessage appends ThreadRootID, so the key is thread-EXACT and a " +
				"later outbound on it is genuinely a reply in this thread"},
		{"slack:T0HPR78RX:C07ABCDEF", false,
			"an unthreaded channel message: the key is the whole conversation. A later outbound here only " +
				"means Salvador has spoken in the channel since — weaker, and counted separately"},
		{"slack:T0HPR78RX:D07PRIVATE", false,
			"a DM with no thread root. Nearly a reply in practice, but the KEY cannot say so, and the fold " +
				"must not upgrade a claim the data does not carry"},
		{"", false, "the empty key is not rooted; a message with no thread records thread_scope='none'"},
		{"jira:sspataro.atlassian.net:WEB-1204", false,
			"a non-slack key. The helper answers about SLACK keys; anything else is not rooted BY THIS " +
				"RULE, and the caller decides what a jira thread is (criterion 14: non-slack channels are " +
				"'thread')"},
	}
	for _, tc := range cases {
		if got := slackweb.IsRootedThreadKey(tc.key); got != tc.rooted {
			t.Errorf("IsRootedThreadKey(%q) = %v, want %v — %s", tc.key, got, tc.rooted, tc.why)
		}
	}
}

// The helper and the BUILDER must agree, checked by round trip rather than by
// eye. This is the assertion a second spelling breaks: someone changes the
// separator or the segment count in normalize.go, and a hand-written parse
// somewhere else keeps answering about the old shape with no error anywhere.
func TestIsRootedThreadKey_AgreesWithTheKeyTheNormalizerBuilds(t *testing.T) {
	rooted, unrooted := swThreadKeysFromNormalizer(t)

	if !slackweb.IsRootedThreadKey(rooted) {
		t.Errorf("the normalizer built %q for a message WITH a thread root and IsRootedThreadKey says it "+
			"is not rooted. The helper and the builder are the same fact read in two directions; when "+
			"they disagree the verdict's thread_scope describes a key that does not exist", rooted)
	}
	if slackweb.IsRootedThreadKey(unrooted) {
		t.Errorf("the normalizer built %q for a message with NO thread root and IsRootedThreadKey says it "+
			"IS rooted. Every such verdict would then claim `thread` scope, and a later outbound anywhere "+
			"in the channel would be rendered as 'answered in thread' — hiding real open inquiries",
			unrooted)
	}
	if rooted == unrooted {
		t.Fatalf("POSITIVE CONTROL FAILED: the normalizer produced the same key (%q) for a threaded and an "+
			"unthreaded message; the fixtures below are not exercising the branch", rooted)
	}
}

// swThreadKeysFromNormalizer runs the REAL normalizer over two raw observations
// that differ only in thread_root_id, so the keys under test are the ones
// production writes rather than strings this file made up. SWT-18's lesson:
// "verify a claimed data path against the DATA, not only the writes".
func swThreadKeysFromNormalizer(t *testing.T) (rooted, unrooted string) {
	t.Helper()
	const base = `{"kind":"message",
	  "workspace":{"id":"T0HPR78RX","own_user_id":"U0OWNSELF"},
	  "conversation":{"id":"C07ABCDEF","name":"llamasite-eng","url":"https://app.slack.com/client/T0HPR78RX/C07ABCDEF"},
	  "message":{"id":"p1757000000000900","timestamp":"2026-09-09T12:00:00Z","author":"Dana Ruiz",
	             "author_id":"U0DANARUIZ","text":"can you confirm the rotation date?"%s}}`

	for _, tc := range []struct {
		extra string
		dst   *string
	}{
		{`,"thread_root_id":"p1757000000000100"`, &rooted},
		{``, &unrooted},
	} {
		_, msg, err := slackweb.NormalizeMessage([]byte(strings.Replace(base, "%s", tc.extra, 1)))
		if err != nil {
			t.Fatalf("NormalizeMessage(extra=%q): %v", tc.extra, err)
		}
		*tc.dst = msg.ThreadKey
	}
	return rooted, unrooted
}

// ---- criterion 14: no SQL anywhere spells the slack thread key ---------------

// The markers that turn "a string mentioning slack" into "SQL (or Go) doing
// something with the key FORMAT". Deliberately narrow — `slack:` inside a
// string that ALSO contains one of these — so it flags key surgery rather than
// any mention of the provider.
var swKeySurgery = regexp.MustCompile(`(?i)\bLIKE\b|split_part|\|\|`)

// swLineSurgery is the SAME rule minus `||`, and it applies to single LINES of
// Go rather than to raw SQL blocks. In a raw string literal `||` is SQL
// concatenation; on a line of Go it is logical OR, and scanning for it there
// flags `if a != "slack:…" || b == nil` — a comparison, not key surgery. The
// upworkcrm scanner shares this weakness and happens not to trip on it; this key
// appears in enough Go conditionals that it does.
var swLineSurgery = regexp.MustCompile(`(?i)\bLIKE\b|split_part`)

var swRawStringLit = regexp.MustCompile("(?s)`[^`]*`")

// Exempt files, by EXACT PATH and with their existence asserted, so a rename
// drops the exemption loudly instead of silently widening it.
var swKeySpellingExempt = []string{
	// The scanner itself: it necessarily contains the patterns it looks for.
	// Its own correctness is guarded by the positive control instead.
	"connector/slackweb/threadscope_test.go",
	// Fixture cleanup and counting by PREFIX. Legitimate and test-only: a
	// cleanup must match BOTH key shapes (rooted and conversation-level) and
	// `slack:{ws}:%` is the only spelling that does, and nothing these files do
	// can reach production data. Listed by EXACT PATH, never by a pattern that
	// could also exempt production code by accident.
	"classify/inquiry_integration_test.go",
	"connector/slackweb/integration_test.go",
	"triage/integration_test.go",
}

func TestNoSQLSpellsTheSlackThreadKey(t *testing.T) {
	// Positive control: a scanner that matches nothing passes everything. The
	// probes are assembled from pieces so this file does not trip its own scan.
	prov := "sla" + "ck:"
	for _, probe := range []string{
		"WHERE thread_key LIKE '" + prov + "'||$1||':%'",
		"SELECT split_part(thread_key, ':', 4) FROM normalized_threads WHERE thread_key LIKE '" + prov + "%'",
	} {
		if !(strings.Contains(probe, prov) && swKeySurgery.MatchString(probe)) {
			t.Fatalf("the scanner does not flag its own probe %q; the patterns have stopped matching", probe)
		}
		if !swLineSurgery.MatchString(probe) {
			t.Fatalf("the LINE scanner does not flag its own probe %q; narrowing it to drop `||` has "+
				"narrowed it past the two markers that matter", probe)
		}
	}
	// And the NEGATIVE control for the narrowing: a Go conditional that merely
	// COMPARES a key is not key surgery, and a scanner that flags it teaches the
	// next reader to add exemptions until it flags nothing.
	if swLineSurgery.MatchString(`if id != "` + prov + `T:C:p1" || other == nil {`) {
		t.Fatalf("the LINE scanner flags a plain Go comparison containing `||`. In a raw SQL literal `||` " +
			"is concatenation; on a line of Go it is logical OR, and conflating them makes the scan cry " +
			"wolf on every `if key != … || x` in the repo")
	}

	root := filepath.Join("..", "..") // internal/
	exempt := map[string]bool{}
	for _, rel := range swKeySpellingExempt {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if _, err := os.Stat(p); err != nil {
			t.Errorf("exempt path %s does not exist (%v): an exemption that names a moved file silently "+
				"stops exempting anything — and, worse, hides that the scan's coverage changed", rel, err)
		}
		exempt[filepath.ToSlash(rel)] = true
	}

	scanned := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if exempt[filepath.ToSlash(rel)] {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		scanned++
		body := string(src)

		report := func(where, snippet string) {
			t.Errorf("internal/%s %s spells the slack thread key in SQL:\n\t%s\n"+
				"Criterion 14: the format has ONE spelling — channelThreadKey builds it, "+
				"slackweb.IsRootedThreadKey reads the rooted/unrooted difference back out. A LIKE, a "+
				"split_part or a || that builds or picks apart the key is a SECOND spelling in the "+
				"database, and the two drift with no error anywhere. Worse here than for upwork: the "+
				"difference this key carries is whether a verdict may CLAIM 'answered in thread', and "+
				"one conversation-level key holds 9,704 messages.", filepath.ToSlash(rel), where, snippet)
		}

		for _, lit := range swRawStringLit.FindAllString(body, -1) {
			if strings.Contains(lit, "slack:") && swKeySurgery.MatchString(lit) {
				report("(raw string literal)", swFirstLines(lit, 3))
			}
		}
		// Single lines, for interpreted string literals. Whole-line `//`
		// comments are skipped deliberately, so prose may quote a spelling to
		// explain why it is gone (upworkcrm/keyspelling_test.go records the
		// same allowance and what it does and does not see).
		for i, line := range strings.Split(body, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
				continue
			}
			if strings.Contains(line, "`") {
				continue // already covered by the raw-literal pass
			}
			if strings.Contains(line, "slack:") && swLineSurgery.MatchString(line) {
				report("line "+swItoa(i+1), trimmed)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
	// Without this the scan passes silently if the walk root moves or the suffix
	// filter stops matching — the failure mode of every source-scanning test.
	if scanned < 50 {
		t.Errorf("scanned only %d .go files under internal/; the walk has probably stopped finding them",
			scanned)
	}
}

// ONE helper, not two. The scan above proves nothing spells the rule in SQL;
// this proves nothing spells it a second time in GO either — the upwork key has
// exactly one parse function and this one must too.
func TestThreadScopeRuleHasOneExportedSpelling(t *testing.T) {
	src, err := os.ReadFile("normalize.go")
	if err != nil {
		t.Fatalf("read normalize.go: %v", err)
	}
	if !strings.Contains(string(src), "func channelThreadKey(") {
		t.Fatalf("POSITIVE CONTROL FAILED: normalize.go no longer declares channelThreadKey; the helper " +
			"under test is defined as living BESIDE it, and this scan has stopped checking anything")
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/connector/slackweb: %v", err)
	}
	found := map[string]string{}
	decl := regexp.MustCompile(`func\s+(IsRooted\w*|ThreadScope\w*|RootedThreadKey\w*|ParseThreadKey\w*)\s*\(`)
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		for _, m := range decl.FindAllStringSubmatch(string(b), -1) {
			found[m[1]] = n
		}
	}
	if len(found) == 0 {
		t.Fatalf("internal/connector/slackweb declares no exported thread-key scope helper. Criterion 14: " +
			"the rooted/unrooted decision lives in ONE exported function in this package, beside the " +
			"channelThreadKey that builds the key")
	}
	if len(found) > 1 {
		t.Errorf("internal/connector/slackweb declares %d thread-key helpers (%v). ONE spelling: two "+
			"functions answering the same question is how the builder and the reader come to disagree "+
			"about a format neither of them owns", len(found), found)
	}
}

func swFirstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = append(lines[:n], "...")
	}
	return strings.Join(lines, "\n\t")
}

func swItoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
