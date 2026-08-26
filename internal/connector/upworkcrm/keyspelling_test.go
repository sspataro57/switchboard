package upworkcrm_test

// Structural enforcement of the ONE-SPELLING rule for the Upwork thread key
// (SWT-19 acceptance criterion 20), copying the shape of
// internal/textmatch/callsites_test.go.
//
// Plain unit test on purpose: no build tag, no database, so it runs under
// `go test ./...` and on every pass.
//
// THE RULE: no SQL anywhere constructs, parses, or pattern-matches an upwork
// thread key. The format lives in exactly one place — threadkey.go — and the
// matcher's client/room scoping happens in Go for this reason and not for style
// (SPEC §4): "any roomed key of this client" is not an equality, and writing it
// as a LIKE or a split_part would put a SECOND spelling of the format in the
// database, where it can drift from the Go one with no error anywhere.
//
// This repo has paid for a second spelling of one canonicalisation four times:
// SWT-13 (a trailing slash in a target_ref), SWT-16 (left(body,120) in SQL vs
// strings.Fields in Go — POSIX \s does not cover the unicode spaces Go splits
// on), SWT-18 (a discriminating column that is a constant in production), and
// the two-room-column near miss this very ticket was re-specced around. Each was
// silent. A twenty-line source scan is cheap next to that.
//
// The check is deliberately narrow — `upwork_crm:` inside a string that ALSO
// contains LIKE, split_part or || — so it flags key surgery rather than any
// mention of the provider. Fixture cleanups that delete by prefix are exempted
// BY EXPLICIT PATH below, never by a pattern that could also exempt production
// code by accident.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The markers that turn "a string mentioning the provider" into "SQL doing
// something with the key format".
var keySurgery = regexp.MustCompile(`(?i)\bLIKE\b|split_part|\|\|`)

// Raw (backtick) string literals are scanned as whole units so a multi-line SQL
// block whose LIKE and whose 'upwork_crm:' sit on different lines is still seen.
var rawStringLit = regexp.MustCompile("(?s)`[^`]*`")

// Exempt files: test fixtures that clean or count rows by key PREFIX. Listed by
// exact path, and their existence is asserted, so a rename drops the exemption
// loudly instead of silently widening it.
//
// Prefix cleanup is legitimate — it must match BOTH key shapes
// (`upwork_crm:{client}:{channel}` and `upwork_crm:{client}:room:{room}`) and
// `upwork_crm:%` is the only spelling that does. It is also test-only: nothing
// it does can reach production data.
var keySpellingExempt = []string{
	"connector/upworkcrm/integration_test.go",
	"triage/integration_test.go",
	// The scanner itself: it necessarily contains the patterns it looks for.
	// Its own correctness is guarded by the positive control instead.
	"connector/upworkcrm/keyspelling_test.go",
}

func TestNoSQLSpellsTheUpworkThreadKey(t *testing.T) {
	// Positive control: a scanner that matches nothing passes everything. The
	// probes are assembled from pieces so this file does not trip its own scan —
	// it is exempted by path below as well, and the two together are why the
	// control matters: nothing else proves the patterns still match.
	provider := "upwork" + "_crm:"
	for _, probe := range []string{
		"WHERE thread_key LIKE '" + provider + "'||$1||':%'",
		"SELECT split_part(thread_key, ':', 3) FROM normalized_threads WHERE thread_key LIKE '" + provider + "%'",
	} {
		if !(strings.Contains(probe, provider) && keySurgery.MatchString(probe)) {
			t.Fatalf("the scanner does not flag its own probe %q; the patterns have stopped matching", probe)
		}
	}

	root := filepath.Join("..", "..") // internal/
	exempt := map[string]bool{}
	for _, rel := range keySpellingExempt {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if _, err := os.Stat(p); err != nil {
			t.Errorf("exempt path %s does not exist (%v): an exemption that names a moved file silently stops "+
				"exempting anything — and, worse, hides that the scan's coverage changed", rel, err)
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
			t.Errorf("internal/%s %s spells the upwork thread key in SQL:\n\t%s\n"+
				"The format has ONE spelling (upworkcrm.ThreadKey / ParseThreadKey). A LIKE, a split_part or a "+
				"|| that builds or picks apart the key is a second spelling in the database, and the two drift "+
				"with no error anywhere — the failure mode this repo has already paid for in SWT-13, SWT-16 and "+
				"SWT-18. Scope in Go instead (SPEC §4); if this is fixture cleanup by prefix, add the file to "+
				"keySpellingExempt by path.", filepath.ToSlash(rel), where, snippet)
		}

		// Whole raw-string literals (multi-line SQL).
		for _, lit := range rawStringLit.FindAllString(body, -1) {
			if strings.Contains(lit, "upwork_crm:") && keySurgery.MatchString(lit) {
				report("(raw string literal)", firstLines(lit, 3))
			}
		}
		// Single lines, for interpreted string literals. Comments are skipped:
		// sink.go's block comment quotes the OLD client-wide LIKE precisely to
		// explain why it is gone, and a scan that cannot tell code from prose
		// would demand the history be deleted.
		for i, line := range strings.Split(body, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
				continue
			}
			if strings.Contains(line, "`") {
				continue // already covered by the raw-literal pass
			}
			if strings.Contains(line, "upwork_crm:") && keySurgery.MatchString(line) {
				report("line "+itoaKS(i+1), trimmed)
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
		t.Errorf("scanned only %d .go files under internal/; the walk has probably stopped finding them", scanned)
	}
}

// Criterion 18: the connector binary calls the reconciler after Normalize and
// prints its count UNCONDITIONALLY, so a pass that flagged nothing and a pass
// that never ran look different in the CronJob log.
//
// That distinction is the whole product here. The reconciler exists because the
// failure it detects is silent; a detector whose own output is silent when it
// finds nothing reproduces the problem one level up — "no reconcile line" would
// mean either "clean" or "never wired", and nobody could tell which.
func TestUpworkConnectorMainRunsTheReconciler(t *testing.T) {
	path := filepath.Join("..", "..", "..", "cmd", "connectors", "upworkcrm", "main.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	body := string(src)

	if !strings.Contains(body, "ReconcileUnconfirmed") {
		t.Errorf("cmd/connectors/upworkcrm/main.go never calls upworkcrm.ReconcileUnconfirmed: the detector ships " +
			"with this ticket precisely so a matcher refusal, an unparseable target_ref or the one-shot gap " +
			"stops being invisible. Unwired, it detects nothing")
	}
	if !strings.Contains(body, "UnconfirmedFlagPasses()") {
		t.Errorf("main.go does not pass upworkcrm.UnconfirmedFlagPasses(): the env override exists so the " +
			"threshold can be retuned without a rebuild; a hardcoded literal here makes it dead code")
	}
	if strings.Contains(body, "if flagged > 0") {
		t.Errorf("main.go prints the reconcile count only when flagged > 0. Criterion 18 wants it unconditional " +
			"(the shape cmd/connectors/slackweb/main.go uses for its capture line): a silent pass and a pass " +
			"that did not run must not look identical in the log")
	}
	normalizeAt := strings.Index(body, "upworkcrm.Normalize(")
	reconcileAt := strings.Index(body, "ReconcileUnconfirmed")
	if normalizeAt >= 0 && reconcileAt >= 0 && reconcileAt < normalizeAt {
		t.Errorf("main.go reconciles BEFORE it normalizes: an in-flight message has not had its chance to " +
			"confirm the delivery yet, so every run would flag rows the very next statement would have closed")
	}
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = append(lines[:n], "...")
	}
	return strings.Join(lines, "\n\t")
}

func itoaKS(n int) string {
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

// Criterion 4's second clause, mechanically: no code path in Normalize reads the
// SOURCE database.
//
// Invariant 1's promise is that reprocessing is always possible, and this whole
// ticket cashes it in — the re-key is a re-normalize from stored raw with a
// changed pure function, not a re-scrape. The moment normalize.go can reach the
// source, that promise is gone and nobody finds out until the source is
// unreachable and a re-normalize fails.
func TestNormalizeReadsRawOnly(t *testing.T) {
	src, err := os.ReadFile("normalize.go")
	if err != nil {
		t.Fatalf("read normalize.go: %v", err)
	}
	body := string(src)
	for _, banned := range []string{"SourceReader", "PGSource", "ListCommunications", "ListClients", "NewSource"} {
		if strings.Contains(body, banned) {
			t.Errorf("normalize.go references %q: Normalize must read raw_source_items ONLY. Re-keying the corpus "+
				"is a re-normalize from stored raw (invariant 1), and a normalize that can reach the source is a "+
				"normalize that stops working the day the source does", banned)
		}
	}
	// Guard the guard.
	if !strings.Contains(body, "func Normalize(") {
		t.Fatalf("normalize.go no longer declares Normalize; this scan has stopped checking anything")
	}
}
