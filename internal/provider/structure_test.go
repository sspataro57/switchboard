package provider_test

// Structural tests for SWT-21 — the criteria this SPEC asks to be enforced
// mechanically rather than by review: 7-purity (invariant 7 generalised),
// 9 (the migration's statement ORDER), 10 (the test-fixture consequence),
// 12 (rules are configuration, not schema), 21 (the env contract) and
// 26 (the runbook's two prose rules).
//
// ZERO I/O beyond reading this repo's own source, SQL and docs. Same shape as
// internal/capture/rules_structure_test.go, and the same rule applies: every
// assertion first REQUIRES its subject to exist, because a source scan that
// passes because there is nothing to scan is the "fixture that proves nothing"
// landmine wearing a lab coat.

import (
	"bytes"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoFile reads a path relative to the repo root (this package sits at
// internal/provider, two levels down).
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// repoGoCode reads a Go file and returns it with COMMENTS REMOVED.
//
// Not a nicety: these files explain themselves, and a banned-token scan over raw
// text fails on the doc comment that says "no pgx, no net/http, no os.Getenv".
// A scan that trips on its own subject's prose is a scan nobody will keep. What
// remains after this is code — including string literals, which is where a
// smuggled reference would actually live.
func repoGoCode(t *testing.T, rel string) string {
	t.Helper()
	path := filepath.Join("..", "..", rel)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0) // no ParseComments: comments are dropped
	if err != nil {
		t.Errorf("parse %s: %v", rel, err)
		return ""
	}
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, f); err != nil {
		t.Errorf("print %s: %v", rel, err)
		return ""
	}
	return buf.String()
}

// sqlCode strips `--` comments. Same reason as repoGoCode: 0016 documents its own
// statement order in a comment block that names every statement in it, so a scan
// over raw text finds the INSERT before the UPDATE in prose and reports a defect
// that is not there.
func sqlCode(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// ---- criterion 7 / invariant 7: locality.go is pure ---------------------------

// "LocalityOf, ClassOf, MostRestrictive and Decide are pure functions in a file
// with no db, no network and no env read, unit-testable with zero
// infrastructure." The FILE is the boundary, exactly as internal/capture/rules.go
// made it: purity that lives in a reviewer's memory rots into "pure except for
// one lookup".
//
// Note what is NOT banned here: the "context" import. locality.go also declares
// Prober, whose whole job is I/O, and the SPEC's words are "no context ON THE
// DECISION FUNCTIONS" — so the ban is applied to their signatures below rather
// than to the file's imports, which would fail for the wrong reason.
func TestLocalityGo_IsPure(t *testing.T) {
	src := repoGoCode(t, "internal/provider/locality.go")
	if src == "" {
		t.Fatalf("internal/provider/locality.go did not parse; the four decision functions live in THIS file")
	}

	banned := []struct{ token, why string }{
		{"pgx", "no database: the boundary must be decidable with zero infrastructure"},
		{"database/sql", "same"},
		{"net/http", "no network: a decision that needs a round trip is a decision that can be broken by a network"},
		{"os.Getenv", "configuration is passed in, never read inside the decision"},
		{"internal/", "no repo-internal imports: this file must not learn about tasks, capture or triage"},
		{"log/slog", "a pure function does not report; its caller does"},
	}
	for _, b := range banned {
		if strings.Contains(src, b.token) {
			t.Errorf("internal/provider/locality.go mentions %q — %s", b.token, b.why)
		}
	}

	// The four decision functions live in THIS file (pinning them here is what
	// makes the purity checkable) and none of them takes a context.
	for _, fn := range []string{"func LocalityOf(", "func ClassOf(", "func MostRestrictive(", "func Decide("} {
		i := strings.Index(src, fn)
		if i < 0 {
			t.Errorf("internal/provider/locality.go does not declare %s — the SPEC pins these four to this file", fn)
			continue
		}
		sig := src[i:]
		if j := strings.Index(sig, "\n"); j > 0 {
			sig = sig[:j]
		}
		if strings.Contains(sig, "context.Context") {
			t.Errorf("%s takes a context.Context: %s\nA context parameter is the first thing an I/O call needs; "+
				"its absence is what keeps the boundary decidable offline", fn, strings.TrimSpace(sig))
		}
	}
}

// ---- criterion 9: the migration's statement order is load-bearing -------------

// "Statement order is load-bearing and must be exactly: ALTER (existing rows take
// the local_only default) → UPDATE projects SET ai_locality='any' (existing rows
// named explicitly) → INSERT the personal row with local_only. Reversing the last
// two silently makes personal general, and nothing would fail."
//
// This one is a source test rather than a database test on purpose. On a fresh
// test database the UPDATE touches zero rows, so no amount of querying after the
// fact can tell a correct migration from a reversed one — the evidence only
// exists in production, where discovering it costs a leak. The file text is the
// only place the order is checkable before it runs.
func TestMigration0016_StatementOrderIsLoadBearing(t *testing.T) {
	src := sqlCode(repoFile(t, "migrations/0016_provider_locality.sql"))
	lower := strings.ToLower(src)

	alter := regexp.MustCompile(`alter\s+table\s+projects\s+add\s+column\s+ai_locality`).FindStringIndex(lower)
	update := regexp.MustCompile(`update\s+projects\s+set\s+ai_locality`).FindStringIndex(lower)
	insert := regexp.MustCompile(`insert\s+into\s+projects\b`).FindStringIndex(lower)

	if alter == nil {
		t.Fatalf("0016 does not ALTER TABLE projects ADD COLUMN ai_locality")
	}
	if update == nil {
		t.Fatalf("0016 does not UPDATE projects SET ai_locality — existing rows must be named EXPLICITLY; " +
			"leaving them on the default would silently restrict every client project")
	}
	if insert == nil {
		t.Fatalf("0016 does not INSERT the personal project")
	}
	if !(alter[0] < update[0]) {
		t.Errorf("the UPDATE precedes the ALTER; it would run against a column that does not exist yet")
	}
	if !(update[0] < insert[0]) {
		t.Errorf("the personal INSERT precedes the UPDATE. That order silently makes `personal` ai_locality='any' " +
			"— the exact leak this ticket exists to prevent — and NOTHING would fail: the migration succeeds, " +
			"every test passes, and personal mail goes to a hosted API")
	}

	// The UPDATE means "every project that existed before the boundary". A WHERE
	// naming slugs would rot the first time a project is renamed.
	stmt := lower[update[0]:]
	if i := strings.Index(stmt, ";"); i > 0 {
		stmt = stmt[:i]
	}
	if strings.Contains(stmt, "where") {
		t.Errorf("the UPDATE carries a WHERE clause (%q). It is deliberately unqualified: it means "+
			"'every project that existed before the boundary', and the personal INSERT that follows is the "+
			"only row that keeps the restrictive default", strings.TrimSpace(stmt))
	}

	// Fail closed on new rows (criterion 9) and constrain the vocabulary.
	if !strings.Contains(lower, "default 'local_only'") {
		t.Errorf("ai_locality does not DEFAULT 'local_only'. A project created later without thinking about " +
			"this column must STALL (one UPDATE, visible in the skipped lane), not LEAK (irreversible)")
	}
	if !strings.Contains(lower, "not null") {
		t.Errorf("ai_locality is not NOT NULL; absent is the exact state this ticket must treat as unsafe")
	}
	if !regexp.MustCompile(`check\s*\(\s*ai_locality\s+in\s*\(\s*'local_only'\s*,\s*'any'\s*\)`).MatchString(lower) {
		t.Errorf("ai_locality has no CHECK (ai_locality IN ('local_only','any')); a typo'd value would " +
			"otherwise be neither and the boundary would have to guess")
	}
	if !strings.Contains(lower, "'personal'") {
		t.Errorf("0016 does not seed the personal project slug")
	}
	if !regexp.MustCompile(`on\s+conflict\s*\(\s*slug\s*\)\s+do\s+nothing`).MatchString(lower) {
		t.Errorf("the personal INSERT is not `ON CONFLICT (slug) DO NOTHING` (criterion 11, verbatim). " +
			"A DO UPDATE variant re-asserts local_only on every migrate, which sounds safer and is a different " +
			"contract: it silently overwrites a deliberate later change to the row and makes the migration a " +
			"writer of live configuration rather than a seeder. If that is wanted, it belongs in the SPEC")
	}
}

// Criterion 12: the capture rules are NOT seeded by the migration. SWT-17's
// precedent, unchanged — routing is configuration with an enabled flag and an
// audit trail, and seeding it puts production routing into every test database
// while making a rule edit a new migration. Decision B (unmatched is restricted)
// is what makes this safe.
func TestMigration0016_DoesNotSeedCaptureRules(t *testing.T) {
	lower := strings.ToLower(sqlCode(repoFile(t, "migrations/0016_provider_locality.sql")))
	if regexp.MustCompile(`insert\s+into\s+capture_rules`).MatchString(lower) {
		t.Errorf("0016 seeds capture_rules. Criterion 12 keeps SWT-17's precedent: the personal rules go in " +
			"with `opsctl capture-rules add` and are recorded in the runbook, so they carry an enabled flag " +
			"and an audit row and can be edited without a migration")
	}
	if regexp.MustCompile(`insert\s+into\s+(tasks|external_refs|capture_decisions)\b`).MatchString(lower) {
		t.Errorf("0016 writes a tool-action table; the migration seeds the PROJECT ONLY (criterion 11)")
	}
}

// ---- criterion 21: the env contract the runbook documents ---------------------

// The SPEC names the two variables: OPS_LOCAL_PROVIDER_URL and OPS_LOCAL_MODEL.
// The names matter more than usual here, because criterion 26 puts them in a
// runbook and criterion 6 of the verification protocol has an operator TYPE one
// of them into a shell to prove the boundary refuses a lie. A variable the
// runbook names and the code does not read is a smoke test that passes while
// testing nothing — it would set an unread variable, get "no local provider",
// and read as success.
func TestCmdTriage_LocalLaneEnvContract(t *testing.T) {
	src := repoFile(t, "cmd/triage/main.go")

	for _, env := range []string{"OPS_LOCAL_PROVIDER_URL", "OPS_LOCAL_MODEL"} {
		if !strings.Contains(src, env) {
			t.Errorf("cmd/triage/main.go never reads %s; criterion 21 names it, and the runbook (criterion 26) "+
				"documents it as the way to point triage at a local model", env)
		}
	}
	// Criterion 21: a non-local OPS_LOCAL_PROVIDER_URL is refused AT STARTUP,
	// with a log line. Startup is the only moment an operator is watching.
	if !strings.Contains(src, "LocalityOf") {
		t.Errorf("cmd/triage/main.go does not call provider.LocalityOf. Criterion 21: when the configured " +
			"local endpoint is not local, the process logs the refusal at startup rather than leaving the " +
			"operator to infer it from a skipped-lane count hours later")
	}
}

// ---- criterion 10: integration fixtures declare their locality ---------------

// "Every integration test that inserts a project and expects the general lane
// must set ai_locality='any' explicitly."
//
// This scan exists because of the failure mode, not the rule: with the column
// defaulting to local_only, a fixture that forgets it produces a SKIP, not a
// failure. The suite stays green while its subject stops being exercised — worse
// than a red suite, and invisible in a diff. Mechanising it is the only way it
// does not rot back, and it is the internal/textmatch/callsites_test.go shape
// this repo already relies on.
func TestIntegrationFixtures_DeclareAILocality(t *testing.T) {
	root := filepath.Join("..", "..", "internal")
	insert := regexp.MustCompile(`(?is)INSERT\s+INTO\s+projects\b`)
	// Anchored at line start so this file's own prose about the tag does not
	// enrol it in its own scan.
	buildTagIntegration := regexp.MustCompile(`(?m)^//go:build[^\n]*\bintegration\b`)

	var scanned, sites int
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(b)
		// Integration suites only: they are the ones with a database, and
		// restricting the walk also keeps this scanner from matching its own
		// error messages.
		if !buildTagIntegration.MatchString(src) {
			return nil
		}
		if !insert.MatchString(src) {
			return nil
		}
		scanned++
		for _, loc := range insert.FindAllStringIndex(src, -1) {
			sites++
			// The SQL literal runs to the closing backtick of the raw string.
			end := strings.Index(src[loc[0]:], "`")
			if end < 0 {
				end = len(src) - loc[0]
			}
			stmt := src[loc[0] : loc[0]+end]
			if strings.Contains(stmt, "ai_locality") {
				continue
			}
			if strings.Contains(stmt, "locality-default-is-deliberate") {
				continue // opt-out for a fixture that is TESTING the restrictive default
			}
			line := 1 + strings.Count(src[:loc[0]], "\n")
			t.Errorf("%s:%d inserts a project without ai_locality. Migration 0016 defaults the column to "+
				"'local_only', so this fixture will make its suite SKIP rather than fail — a passing test that "+
				"exercises nothing. Add ai_locality='any', or the marker comment "+
				"locality-default-is-deliberate if the default is the point",
				strings.TrimPrefix(path, "../../"), line)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
	if scanned == 0 || sites == 0 {
		t.Fatalf("found no INSERT INTO projects in any internal/**/*_test.go (scanned %d files, %d sites); "+
			"a scan with nothing to scan proves nothing", scanned, sites)
	}
}

// ---- criterion 26: the runbook says the two things code cannot ---------------

// A test on prose, and it earns it the same way TestRunbook_DocumentsCaptureBeforeTriage
// does. Two rules in this ticket are only ever enforced by a reader's judgement:
//
//   - "a fallback to a hosted provider is never correct" — the well-intentioned
//     fix a contributor reaches for when the local box is slow.
//   - "an all-skipped report is the SUCCESS state until SWT-22 lands" — without
//     the sentence, the first person to read a report where every message was
//     skipped opens an incident, and the fix they will propose is the fallback
//     above.
func TestRunbook_ProviderLocality(t *testing.T) {
	doc := repoFile(t, "docs/runbooks/provider-locality.md")
	lower := strings.ToLower(doc)

	noFallback := regexp.MustCompile(`(?s)(never|not)[^.\n]{0,80}fall\s?back|fall\s?back[^.\n]{0,80}(never|not correct|is wrong)`)
	if !noFallback.MatchString(lower) {
		t.Errorf("docs/runbooks/provider-locality.md does not state that a fallback to a hosted provider is " +
			"NEVER correct. That sentence is the whole mitigation: nothing in the code can stop the next " +
			"contributor 'fixing' a skip into a fallback, because the fallback looks like an availability " +
			"improvement in a diff")
	}

	allSkipped := regexp.MustCompile(`(?s)(all[- ]skipped|every message[^.\n]{0,40}skipped|skips? everything)[^.]{0,200}(success|expected|by design|not an outage|normal)`)
	if !allSkipped.MatchString(lower) {
		t.Errorf("docs/runbooks/provider-locality.md does not say that an all-skipped triage report is the " +
			"SUCCESS state (criterion 26). Triage skips its entire inbox until a local adapter exists, so the " +
			"first report anyone reads shows zero processed messages — an operator without this sentence " +
			"opens an incident against a working boundary")
	}
	if !strings.Contains(lower, "swt-22") {
		t.Errorf("the runbook never names SWT-22. Criterion 26 requires the gating to be written down: triage " +
			"is idle BY DESIGN until the local classifier ships, and the dependency runs the opposite way " +
			"from what a reader assumes")
	}
	for _, want := range []string{"ops_local_provider_url", "ops_local_model", "triage report"} {
		if !strings.Contains(lower, want) {
			t.Errorf("the runbook never mentions %q; criterion 26 requires the env contract and what a skip "+
				"looks like in the report", want)
		}
	}
	if !strings.Contains(lower, "capture-rules add") {
		t.Errorf("the runbook does not record the exact `opsctl capture-rules add` commands used for the " +
			"personal sender list (criteria 12-13, 26). The rules are NOT in the migration, so the runbook is " +
			"the only record of what the boundary's input actually is")
	}
}
