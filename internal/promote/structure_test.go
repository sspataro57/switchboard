package promote_test

// Structural tests for SWT-30 (docs/tickets/classify-promotion_SPEC.md) — the
// criteria the SPEC asks to be enforced mechanically rather than by review:
// 6 (this package's unit test stays offline), 13 (tasks/task_events/deliveries/
// external_refs only through the executor), 14 (the promoter never calls a
// model), 18 (the funnel's promotion line stays a WINDOW), 19 (nothing
// outbound), and the data-model guard on migration 0021.
//
// ZERO I/O beyond reading this repo's own source. Same shape as
// internal/classify/structure_test.go (go/parser import scan, migration guard)
// and internal/capture/rules_structure_test.go:72 (the direct-write ban),
// retargeted at internal/promote.
//
// Every assertion REQUIRES its subject to exist first. A source scan that passes
// because there was nothing to scan is this repo's "fixture that proves nothing"
// landmine wearing a lab coat, and D3's whole argument is that the ban is
// STRUCTURAL — an inert scan would make it decorative instead.
//
// GREENFIELD NOTE — EXPECTED RED. internal/promote has no non-test source and
// migrations/0021_classify_promotion.sql does not exist, so this file's package
// does not build (no non-test Go files) and, once promote.go lands, the
// migration and funnel guards below fail until 0021 and the /funnel section do.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const prModule = "github.com/sspataro57/switchboard"

// ---- helpers -----------------------------------------------------------------

// prRepoFile reads a path relative to the repo root (this package sits at
// internal/promote, two levels down).
func prRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// prSources lists a package's NON-test .go files, relative to the repo root, and
// FAILS when there are none.
func prSources(t *testing.T, pkg string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("..", "..", pkg))
	if err != nil {
		t.Fatalf("read %s: %v", pkg, err)
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(pkg, n))
	}
	if len(out) == 0 {
		t.Fatalf("no non-test .go files in %s; a scan with nothing to scan proves nothing "+
			"(D3: the separate package is what makes 'the promoter never calls an LLM' structural)", pkg)
	}
	return out
}

// prFileImports returns the import PATHS of one .go file, parsed rather than
// grepped. Parsing is what lets this file name "github.com/jackc/pgx/v5" in its
// own banned list without banning itself: a string literal in an argument is not
// an import spec.
func prFileImports(t *testing.T, rel string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join("..", "..", rel), nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	var out []string
	for _, spec := range f.Imports {
		out = append(out, strings.Trim(spec.Path.Value, `"`))
	}
	return out
}

// prPackageImports returns the deduplicated import paths of a package's non-test
// files.
func prPackageImports(t *testing.T, pkg string) []string {
	t.Helper()
	seen := map[string]bool{}
	var out []string
	for _, rel := range prSources(t, pkg) {
		for _, p := range prFileImports(t, rel) {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// ---- criterion 6: the pure test stays offline --------------------------------

// "its unit test imports no pgx, no net and no provider (the
// internal/orchestrator/rules_test.go shape)". Enforced rather than reviewed:
// the day someone needs "just one lookup" in promote_test.go is the day Decide
// stops being a pure function, and the import block is where that shows first.
func TestPromoteTest_ImportsNothingThatCouldDoIO(t *testing.T) {
	const rel = "internal/promote/promote_test.go"
	if _, err := os.Stat(filepath.Join("..", "..", rel)); err != nil {
		t.Fatalf("%s is missing: %v — criterion 6's proof is the file itself", rel, err)
	}
	imports := prFileImports(t, rel)
	if len(imports) == 0 {
		t.Fatalf("%s imports nothing at all; it cannot be exercising promote.Decide", rel)
	}

	banned := []struct{ frag, why string }{
		{"pgx", "no database: Decide must be testable with zero network (invariant 7)"},
		{"net", "no network"},
		{prModule + "/internal/provider", "no model: the promoter reads stored verdicts only"},
		{prModule + "/internal/store", "no pool constructor — that is the integration suite's job"},
		{prModule + "/internal/executor", "no executor: the pure test asserts decisions, not tool calls"},
	}
	subject := false
	for _, imp := range imports {
		if imp == prModule+"/internal/promote" {
			subject = true
		}
		for _, b := range banned {
			if imp == b.frag || strings.Contains(imp, b.frag+"/") || strings.HasSuffix(imp, "/"+b.frag) {
				t.Errorf("%s imports %q — criterion 6 forbids it: %s", rel, imp, b.why)
			}
		}
	}
	if !subject {
		t.Errorf("%s does not import %s/internal/promote; whatever it tests, it is not the promoter",
			rel, prModule)
	}
}

// ---- criterion 14: the promoter never calls a model ---------------------------

// "internal/promote imports neither internal/provider nor any vendor SDK".
//
// TRANSITIVE, deliberately. A direct-import scan is satisfied by importing
// internal/classify — which imports internal/provider by construction, which is
// precisely why D3 put the promoter in its own package instead. So this walks
// the repo-internal import graph and fails if internal/provider is REACHABLE at
// all, and separately allows only stdlib, this module and pgx as direct imports:
// a vendor SDK is exactly a third-party module that is not the database driver.
func TestPromote_CannotReachAModelProvider(t *testing.T) {
	const pkg = "internal/promote"

	// Direct imports first: the allow-list is the honest spelling of "no vendor
	// SDK", because enumerating the SDKs that do not exist yet would certify a
	// no-op (this repo has no LLM SDK in go.mod — the local adapter is plain
	// net/http inside internal/provider).
	for _, imp := range prPackageImports(t, pkg) {
		switch {
		case !strings.Contains(strings.SplitN(imp, "/", 2)[0], "."):
			// stdlib: the first path element has no dot.
		case strings.HasPrefix(imp, prModule+"/"):
		case strings.HasPrefix(imp, "github.com/jackc/pgx/"):
		default:
			t.Errorf("%s imports third-party package %q. Criterion 14: this package imports no vendor SDK — "+
				"the only external dependency a deterministic promoter needs is the database driver", pkg, imp)
		}
	}

	// Transitive reachability of internal/provider.
	target := prModule + "/internal/provider"
	seen := map[string]bool{}
	var path []string
	var walk func(p string, trail []string) bool
	walk = func(p string, trail []string) bool {
		if seen[p] {
			return false
		}
		seen[p] = true
		for _, imp := range prPackageImports(t, strings.TrimPrefix(p, prModule+"/")) {
			if !strings.HasPrefix(imp, prModule+"/") {
				continue
			}
			if imp == target {
				path = append(append([]string{}, trail...), p, imp)
				return true
			}
			if walk(imp, append(trail, p)) {
				return true
			}
		}
		return false
	}
	if walk(prModule+"/"+pkg, nil) {
		t.Errorf("%s can reach internal/provider: %s. D3's whole argument is that a SEPARATE package makes "+
			"'the promoter never calls an LLM' structural — reaching provider through internal/classify "+
			"honours the letter and drops the guarantee", pkg, strings.Join(path, " -> "))
	}
}

// ---- criteria 13 + 19: no direct writes to tool-action tables ------------------

// The internal/capture/rules_structure_test.go:72 test, retargeted. Invariant 3
// for this package, and criterion 19 in the same scan: banning
// `INSERT INTO deliveries` here is what makes "nothing outbound" true by
// construction rather than by inspection.
//
// The two writes this package IS allowed are `INSERT INTO classify_promotions`
// and its `UPDATE ... SET task_id` — its own append-only decision log, the
// capture_decisions precedent — so the ban names four tables, not "any INSERT".
func TestPromote_NeverWritesToolActionTablesDirectly(t *testing.T) {
	files := prSources(t, "internal/promote")
	banned := regexp.MustCompile(`(?is)insert\s+into\s+(tasks|task_events|deliveries|external_refs)\b`)
	updateBanned := regexp.MustCompile(`(?is)update\s+(tasks|task_events|deliveries|external_refs)\b`)
	for _, rel := range files {
		src := prRepoFile(t, rel)
		if m := banned.FindString(src); m != "" {
			t.Errorf("%s contains %q — invariant 3: tasks, task_events, deliveries and external_refs are "+
				"reached ONLY through create_task, task_append_log and task_set_source_thread on the "+
				"executor, with actor promote:classify (criterion 13)", rel, m)
		}
		if m := updateBanned.FindString(src); m != "" {
			t.Errorf("%s contains %q — same rule: a direct mutation skips validate -> policy -> audit and "+
				"leaves no answer to 'who did this' (criteria 13, 19)", rel, m)
		}
	}

	// The control. The scan above is only meaningful if this package really does
	// write its OWN log directly; if it does not, the four bans are passing over
	// a package that writes nothing at all.
	var writesItsOwnLog bool
	own := regexp.MustCompile(`(?is)insert\s+into\s+classify_promotions\b`)
	for _, rel := range files {
		if own.MatchString(prRepoFile(t, rel)) {
			writesItsOwnLog = true
		}
	}
	if !writesItsOwnLog {
		t.Errorf("no file in internal/promote contains `INSERT INTO classify_promotions`. D4: the promotion " +
			"log is written directly by its own package (the ai_runs / capture_decisions precedent) — " +
			"without that write, the bans above are scanning a package that touches no tables")
	}
}

// ---- data model: SWT-30 adds exactly ONE migration, 0021 ----------------------

// The internal/classify/structure_test.go guard, rewritten for this ticket —
// NOT deleted and NOT replaced. TestMigration0018_IsTheOnlyOneThisTicketAdds
// globs 0018_*.sql specifically and stays true; this one globs 0021_*.sql. Each
// guard is correct for its ticket forever, and the reason they accumulate is
// that the migrate runner keys on schema_migrations.version with NO checksum:
// a stray or edited file is skipped SILENTLY and the schema diverges with no
// error anywhere.
func TestMigration0021_IsTheOnlyOneThisTicketAdds(t *testing.T) {
	// Control first: 0020 must still be there, or a glob returning nothing below
	// would prove only that the directory moved.
	if _, err := os.Stat(filepath.Join("..", "..", "migrations", "0020_calendar_booking.sql")); err != nil {
		t.Fatalf("migrations/0020_calendar_booking.sql is missing: %v", err)
	}

	matches, err := filepath.Glob(filepath.Join("..", "..", "migrations", "0021_*.sql"))
	if err != nil {
		t.Fatalf("glob migrations/0021_*.sql: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("found %d migrations/0021_*.sql file(s), want exactly 1 (the SPEC names "+
			"0021_classify_promotion.sql). 0020 is the current highest, and this ticket's data-model "+
			"section is ONE migration: the cutover column and the promotion log. Merging a migration is "+
			"not applying it — check `SELECT max(version) FROM schema_migrations` before deploying",
			len(matches))
	}
	if extra, _ := filepath.Glob(filepath.Join("..", "..", "migrations", "0022_*.sql")); len(extra) != 0 {
		t.Errorf("found %d migrations/0022_*.sql file(s): SWT-30 adds ONE migration. (The 0022 the SPEC "+
			"mentions is classify's ADVISORY LOCK key, not a migration number — the two families share "+
			"digits and nothing else. The literal key is deliberately NOT spelled here: "+
			"internal/classify/structure_test.go's collision scan walks internal/ for it and would "+
			"report this file as a second owner.)", len(extra))
	}

	rel := filepath.Join("migrations", filepath.Base(matches[0]))
	sql := strings.ToLower(prRepoFile(t, rel))

	// (1) D1: the cutover is a COLUMN on projects, nullable, no default.
	alter := regexp.MustCompile(`(?s)alter\s+table\s+projects\s+add\s+column\s+classify_promote_after\s+timestamptz`)
	loc := alter.FindStringIndex(sql)
	if loc == nil {
		t.Errorf("%s does not ALTER TABLE projects ADD COLUMN classify_promote_after TIMESTAMPTZ. D1: the "+
			"cutover is a real COLUMN, not a key in projects.policies — 0016 (ai_locality) and 0018 "+
			"(ai_classify) set that precedent, and an untyped predicate over jsonb is exactly the thing "+
			"this repo keeps paying for", rel)
	} else {
		stmt := sql[loc[0]:]
		if end := strings.Index(stmt, ";"); end >= 0 {
			stmt = stmt[:end]
		}
		if strings.Contains(stmt, "default") {
			t.Errorf("%s gives classify_promote_after a DEFAULT (%q). Criterion 5: NULL means 'this "+
				"project's verdicts never promote', and the operator sets the value in the psql "+
				"statement the runbook prints — which is what makes the cutover a decision with a "+
				"timestamp rather than a deploy side effect", rel, strings.TrimSpace(stmt))
		}
		if strings.Contains(stmt, "not null") {
			t.Errorf("%s declares classify_promote_after NOT NULL. NULL IS the fail-closed side: not "+
				"promoting is a stall, promoting by accident fills a board with rows nobody chose", rel)
		}
	}

	// (2) D4: the promotion decision log.
	if !regexp.MustCompile(`(?s)create\s+table\s+classify_promotions`).MatchString(sql) {
		t.Fatalf("%s does not CREATE TABLE classify_promotions. D4: an append-only decision log with no "+
			"status, no assignee and no claim — the capture_decisions precedent — so it is not a second "+
			"tasks table (invariant 2)", rel)
	}
	for _, want := range []struct{ frag, why string }{
		{"normalized_message_id", "the dedup key and the message the decision is about"},
		{"ai_extraction_id", "which stored verdict produced it — the promoter reads verdicts, never a model"},
		{"raw_source_item_id", "invariant 1: every promoted task is traceable to the provider JSON"},
		{"project_id", "criterion 16: the CURRENT attribution the task was created in"},
		{"task_id", "criterion 12: NULL is the crash artifact, and it must be nullable to be one"},
		{"reason", "criteria 9 and 16 both record their explanation here"},
	} {
		if !strings.Contains(sql, want.frag) {
			t.Errorf("%s's classify_promotions has no %s column — %s", rel, want.frag, want.why)
		}
	}

	check := regexp.MustCompile(`(?s)action\s+text\s+not\s+null\s+check\s*\(\s*action\s+in\s*\(([^)]*)\)`)
	m := check.FindStringSubmatch(sql)
	if m == nil {
		t.Errorf("%s does not constrain action with CHECK (action IN ('task','review','attached')). The "+
			"three actions are the whole vocabulary of this ticket; an unconstrained TEXT lets a fourth "+
			"appear with no migration and no test", rel)
	} else {
		for _, want := range []string{"task", "review", "attached"} {
			if !strings.Contains(m[1], "'"+want+"'") {
				t.Errorf("%s's action CHECK (%s) does not allow %q", rel, strings.TrimSpace(m[1]), want)
			}
		}
	}

	// (3) criterion 11: idempotency is STRUCTURAL, and the index is TOTAL.
	uniq := regexp.MustCompile(`(?s)create\s+unique\s+index\s+\S+\s+on\s+classify_promotions\s*\(\s*normalized_message_id\s*\)([^;]*)`)
	um := uniq.FindStringSubmatch(sql)
	if um == nil {
		t.Errorf("%s has no UNIQUE index on classify_promotions (normalized_message_id). Criterion 11: "+
			"idempotency is structural, not advisory — `ON CONFLICT (normalized_message_id) DO NOTHING "+
			"RETURNING id` is the claim, and losing the race must mean the promoter does not act", rel)
	} else if strings.Contains(um[1], "where") {
		t.Errorf("%s makes the unique index PARTIAL (%q). The SPEC says total on purpose: a partial index "+
			"forces every future ON CONFLICT to restate the predicate (capture_decisions_live_uniq and "+
			"task_events_outbound_observed_uniq both do), and omitting it raises 'no unique or exclusion "+
			"constraint matching the ON CONFLICT specification' at RUNTIME", rel, strings.TrimSpace(um[1]))
	}

	// (4) the cascade, and the reason it is not cosmetic.
	cascade := regexp.MustCompile(`(?s)normalized_message_id[^,]*references\s+normalized_messages\s*\(\s*id\s*\)\s*on\s+delete\s+cascade`)
	if !cascade.MatchString(sql) {
		t.Errorf("%s does not declare normalized_message_id ... ON DELETE CASCADE. capture_decisions' "+
			"recorded reason applies unchanged: 19 integration suites clear fixtures by deleting "+
			"normalized_messages, and without the cascade they fail INSIDE cleanup — which reads like "+
			"the cross-pollution pact breaking rather than like a new FK", rel)
	}
}

// ---- criterion 18: the funnel's promotion line, and the page's read-only rule --

// A source scan and not a rendering assertion, deliberately. The numbers belong
// in an integration test against real rows; what belongs HERE is the pair of
// facts the SPEC states about the page's SHAPE: the promotion counters are a
// fifth independently-degrading section over classify_promotions, and /funnel
// stays "a WINDOW, NOT A CONTROL" — no POST route.
//
// The rendered output is deferred on purpose: criterion 18's line wording
// (created / attached / review / dry-run, per lane) is the implementer's to
// choose, and a test that pinned a string would be pinning prose. Once the
// section's loader exists as a named seam, the counts belong in
// internal/dashboard/funnel_integration_test.go beside the classify block.
func TestFunnel_HasAPromotionSectionAndStaysReadOnly(t *testing.T) {
	const rel = "internal/dashboard/funnel.go"
	src := prRepoFile(t, rel)

	if !strings.Contains(src, "classify_promotions") {
		t.Errorf("%s never reads classify_promotions. Criterion 18: /funnel's classify block gains one "+
			"promotion line per lane, read from the promotion log over the same ?days= window", rel)
	}
	section := regexp.MustCompile(`(?i)\{\s*Name:\s*"[^"]*promot[^"]*"`)
	if !section.MatchString(src) {
		t.Errorf("%s declares no funnelSection whose Name mentions promotion. Criterion 18 makes it a "+
			"FIFTH independently-degrading section (runSections), so a broken promotion query renders "+
			"one named error line and leaves the other four numbers on the page", rel)
	}

	routes := prRepoFile(t, "internal/dashboard/server.go")
	if regexp.MustCompile(`(?i)"POST\s+/funnel`).MatchString(routes) {
		t.Errorf("internal/dashboard/server.go registers a POST route under /funnel. Criterion 18: the " +
			"page stays a WINDOW, NOT A CONTROL — approving a review-lane task is a verb the dashboard " +
			"does not have yet, and inventing it here is a different ticket (Q1's cost, stated)")
	}
}
