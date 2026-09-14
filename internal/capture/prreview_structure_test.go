package capture

// Structural tests for SWT-54 (docs/tickets/treetop-pr-review-tasks_SPEC.md).
// ZERO I/O beyond reading this repo's own files. Each check first REQUIRES its
// subject to exist: a scan with nothing to scan proves nothing.
//
//   - criterion 7: decidePRAuthor and prReviewTitle are pure (the gate.go
//     DecideGate precedent), and prreview.go imports no I/O;
//   - criterion 6: ONE PR-key spelling under internal/ — PGTaskResolver.Resolve
//     calls github.PRKey, and no other "%s#%d"-shaped key is built;
//   - D2 point 3 / D5: the driver canonicalizes through github.ParsePRRef /
//     PRKey / PRURL and detects notices with github.PRStateNotice, closes
//     through task_close and treats the active-work refusal as a skip;
//   - D1: prreview_store.go reads the raw envelope through
//     google.RawMailHeaders and github.NotificationFacts, inbound only, capped;
//     it and dryrun.go write nothing;
//   - criterion 17: dryrun.go writes nothing — no INSERT, UPDATE, DELETE, no
//     Execute(, no executor import, no advisory lock, and it shares
//     decideMessage (the DryRunGate precedent);
//   - criterion 15: every capture counter line prints pr_author_skipped and
//     pr_closed, zeros included — the capture_gate line too;
//   - criterion 3: opsctl's list selects both columns, and `try` is a verb;
//   - criterion 1: migration 0035's shape (the SPEC says 0034; SWT-53 owns it).
//
// GREENFIELD NOTE — EXPECTED RED: prreview.go, prreview_store.go, dryrun.go,
// internal/connector/github/prref.go and migrations/0035_capture_rules_pr_review.sql
// do not exist; store.go still spells "%s#%d"; no printer prints the counters.
// TestMigrationLedger_Learns0035 is GREEN by design: the ledger learns 35 in
// the same change as this file, and a ledger entry cannot fail before the file
// exists — TestMigration0035_CaptureRulesPRReviewShape is the failing guard.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ---- criterion 7: the verdict and the title are pure ------------------------------

func TestPRReviewGo_IsPure(t *testing.T) {
	src := mustReadRepoFile(t, "internal/capture/prreview.go")
	for _, b := range []struct{ token, why string }{
		{`"context"`, "a pure verdict takes no context"},
		{"pgx", "no database: the facts arrive as values (prReviewFacts reads, decidePRAuthor decides)"},
		{"os.Getenv", "his login is DATA (X-GitHub-Recipient, exclude_pr_authors), never the environment"},
		{"net/http", "no network"},
		{"internal/provider", "no model: invariant 7"},
		{"time.Now", "no clock"},
	} {
		if strings.Contains(src, b.token) {
			t.Errorf("internal/capture/prreview.go mentions %q — %s", b.token, b.why)
		}
	}
	for _, fn := range []string{"decidePRAuthor", "prReviewTitle", "matchExcludedAuthor"} {
		body := rvFuncSrc(src, fn)
		if body == "" {
			t.Errorf("internal/capture/prreview.go does not declare %s (the SPEC's \"Files likely to touch\")", fn)
			continue
		}
		for _, b := range []string{"ctx", "pool", ".Query", ".Exec", "Execute(", "time.Now", "time.Since", "os.Getenv"} {
			if strings.Contains(body, b) {
				t.Errorf("%s's body contains %q — criterion 7: a pure function of stored facts.\nbody:\n%s", fn, b, body)
			}
		}
	}
	// His login is data, not a Go literal (D1).
	if strings.Contains(src, `"sspataro57"`) {
		t.Errorf("internal/capture/prreview.go spells his login as a literal; D1: row 2 reads it per mail from " +
			"X-GitHub-Recipient and extra logins live in capture_rules.exclude_pr_authors")
	}
}

// ---- criterion 6: one PR-key spelling ---------------------------------------------

// A Go format literal that builds `{repo}#{n}`. The poller's raw-item id
// ("pr:%s#%d", a raw_source_items.external_id, not an external_refs key) does
// not match this and is deliberately out of scope.
var prKeyFormat = regexp.MustCompile(`^%s#%[dvs]$`)

func TestPRKey_OneSpellingUnderInternal(t *testing.T) {
	const home = "internal/connector/github/prref.go"
	prref := mustReadRepoFile(t, home)
	if !strings.Contains(prref, "func PRKey(") {
		t.Fatalf("%s does not declare PRKey — the ONE spelling lives there (D2 point 3)", home)
	}
	root := filepath.Join("..", "..", "internal")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		rel := strings.TrimPrefix(filepath.ToSlash(path), "../../")
		if rel == home {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, uerr := strconv.Unquote(lit.Value)
			if uerr == nil && prKeyFormat.MatchString(v) {
				t.Errorf("%s:%d builds a PR key with %q — criterion 6: every github PR key goes through "+
					"github.PRKey. Two spellings of one PR under system='github' let one PR hold two tasks, "+
					"silently (external_refs' UNIQUE cannot see through spellings)", rel, fset.Position(lit.Pos()).Line, v)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}

	store := mustReadRepoFile(t, "internal/connector/github/store.go")
	i := strings.Index(store, "func (r *PGTaskResolver) Resolve(")
	if i < 0 {
		t.Fatalf("internal/connector/github/store.go no longer declares PGTaskResolver.Resolve")
	}
	body := store[i:]
	if j := strings.Index(body[1:], "\nfunc "); j > 0 {
		body = body[:j+1]
	}
	if !strings.Contains(body, "PRKey(") {
		t.Errorf("PGTaskResolver.Resolve does not call PRKey — criterion 6: \"PGTaskResolver.Resolve is refactored " +
			"to call github.PRKey (one spelling)\"")
	}
}

// ---- D2 point 3 / D5: the driver uses the connector's vocabulary -------------------

func TestRulesStore_CanonicalizesAndClosesThroughTheOneVocabulary(t *testing.T) {
	src := mustReadRepoFile(t, "internal/capture/rules_store.go")
	for _, want := range []struct{ token, why string }{
		{"github.ParsePRRef(", "D2 point 3: every github-derived key is canonicalized; a key that does not parse is attribution only"},
		{"github.PRKey(", "D2 point 3: the stored key is github.PRKey's spelling"},
		{"github.PRURL(", "D2 point 3: the ref's URL (url_template is refused for github)"},
		{"github.PRStateNotice(", "D5: the merge/close notice predicate has one spelling, shared with SWT-53"},
		{"prReviewFacts(", "D1: the store-backed read feeding the pure verdict (the ownActionFacts split)"},
		{"decidePRAuthor(", "D1: the verdict is decided by the pure function, not re-spelled inline"},
		{"prReviewTitle(", "D2 point 4: review titles apply to pr_review rules"},
	} {
		if !strings.Contains(src, want.token) {
			t.Errorf("internal/capture/rules_store.go never calls %s — %s", want.token, want.why)
		}
	}
	var close, refusal bool
	for _, f := range capturePackageSourceFiles(t) {
		s := mustReadRepoFile(t, filepath.Join("internal/capture", f))
		close = close || strings.Contains(s, `"task_close"`)
		refusal = refusal || strings.Contains(s, "refusing to close active work")
	}
	if !close {
		t.Errorf("no internal/capture source names the task_close tool — D5: the close goes through the executor " +
			"as capture:{connector} (invariant 3)")
	}
	if !refusal {
		t.Errorf("no internal/capture source matches close's activeWorkRefusal phrase (\"refusing to close active " +
			"work\") — D5: that refusal is a non-fatal skip, the ticketstatus precedent; any other error fails the pass")
	}
}

// ---- D1: the facts read ------------------------------------------------------------

func TestPRReviewStore_ReadsTheRawEnvelopeInboundOnlyAndCapped(t *testing.T) {
	src := mustReadRepoFile(t, "internal/capture/prreview_store.go")
	for _, want := range []struct{ token, why string }{
		{"func prReviewFacts(", "the SPEC names it"},
		{"google.RawMailHeaders(", "the header reader is the connector's, over the raw envelope (criterion 19: not the normalizer)"},
		{"github.NotificationFacts(", "the X-GitHub-* names have one spelling, in the github package"},
		{"raw_json", "authorship is read from raw_source_items.raw_json (criterion 9), never a normalized column"},
		{"external_message_id", "the opening is found by EXACT external_message_id equality (D1)"},
	} {
		if !strings.Contains(src, want.token) {
			t.Errorf("internal/capture/prreview_store.go never mentions %s — %s", want.token, want.why)
		}
	}
	if !regexp.MustCompile(`direction\s*=\s*'inbound'`).MatchString(src) {
		t.Errorf("prreview_store.go does not filter direction = 'inbound' — D1 reads \"every INBOUND message on the " +
			"pending message's thread_id\"; an outbound row is his own words, never GitHub's statement")
	}
	if !regexp.MustCompile(`\b100\b`).MatchString(src) {
		t.Errorf("prreview_store.go carries no 100-row cap — D1: \"Newest first, capped at 100 rows\"")
	}
}

// Both new files are reads only. rules*.go is scanned by
// TestCaptureRules_NeverWritesToolActionTablesDirectly; these names are not.
func TestPRReviewStoreAndDryRun_WriteNoTable(t *testing.T) {
	write := regexp.MustCompile(`(?is)\b(insert\s+into|update\s+\w+\s+set|delete\s+from)\b`)
	for _, f := range []string{"prreview_store.go", "dryrun.go"} {
		for _, lit := range prrStringLiterals(t, f) {
			if m := write.FindString(lit); m != "" {
				t.Errorf("internal/capture/%s has a SQL literal containing %q — it only reads: %q", f, m, lit)
			}
		}
	}
}

// ---- criterion 17: the dry run writes nothing ---------------------------------------

func TestDryRunGo_WritesNothing(t *testing.T) {
	const rel = "internal/capture/dryrun.go"
	src := mustReadRepoFile(t, rel)
	if !strings.Contains(src, "func DryRunRules(") {
		t.Fatalf("%s does not declare DryRunRules (the SPEC: `opsctl capture-rules try` is a CLI over capture.DryRunRules)", rel)
	}
	if !strings.Contains(src, "decideMessage(") {
		t.Errorf("%s does not call decideMessage — D10: the dry run runs the SAME decide step (Evaluate, the D1 reads, "+
			"canonicalization, the D5 check), the decideGateHolds precedent; a second decider is a second spelling", rel)
	}

	for _, lit := range prrStringLiterals(t, "dryrun.go") {
		if regexp.MustCompile(`(?i)\b(insert|update|delete)\b`).MatchString(lit) {
			t.Errorf("%s has a string literal naming INSERT/UPDATE/DELETE: %q — criterion 17 bans all three", rel, lit)
		}
		if regexp.MustCompile(`(?i)pg_(try_)?advisory`).MatchString(lit) {
			t.Errorf("%s takes an advisory lock (%q) — D10: \"no capture_decisions row, no executor call, no lock\"", rel, lit)
		}
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join("..", "..", rel), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); strings.HasSuffix(p, "/internal/executor") {
			t.Errorf("%s imports internal/executor — the dry run \"makes no executor call at all, because it writes nothing\"", rel)
		}
	}
	actor := regexp.MustCompile(`^(insert|record|create|link|set|append|reopen|revive|mark|close)\w*(Rule|Decision)\w*$`)
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := ""
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		}
		switch {
		case name == "Execute", name == "Exec", name == "Begin", name == "tryRulesLock":
			t.Errorf("%s:%d calls %s — criterion 17: the dry run writes nothing and takes no lock", rel,
				fset.Position(call.Pos()).Line, name)
		case actor.MatchString(name):
			t.Errorf("%s:%d calls %s, one of the pass's acting helpers — criterion 17", rel, fset.Position(call.Pos()).Line, name)
		}
		return true
	})
}

// prrStringLiterals returns every string literal in internal/capture/<file>,
// comments excluded (a comment that says "writes no INSERT" is not a write).
func prrStringLiterals(t *testing.T, file string) []string {
	t.Helper()
	path := filepath.Join("..", "..", "internal", "capture", file)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse internal/capture/%s: %v", file, err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if v, err := strconv.Unquote(lit.Value); err == nil {
				out = append(out, v)
			}
		}
		return true
	})
	return out
}

// ---- criterion 15: the counters on every capture counter line ----------------------

func TestCaptureCounterLines_PrintPRAuthorSkippedAndPRClosed(t *testing.T) {
	printer := regexp.MustCompile(`\\"appended\\":%d`)
	var printers []string
	err := filepath.Walk(filepath.Join("..", "..", "cmd"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		src := string(b)
		if !printer.MatchString(src) {
			return nil
		}
		rel := strings.TrimPrefix(filepath.ToSlash(path), "../../")
		printers = append(printers, rel)
		for _, want := range []string{`\"pr_author_skipped\":%d`, `\"pr_closed\":%d`} {
			if !strings.Contains(src, want) {
				t.Errorf("%s prints the capture counters without %s. Criterion 15: RulesStats gains PRAuthorSkipped "+
					"and PRClosed and every capture_rules: line prints them, zeros included (the capture_gate: line "+
					"prints both as 0); Verification step 6 watches them", rel, want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk cmd/: %v", err)
	}
	for _, want := range []string{
		"cmd/connectors/jira/main.go", "cmd/connectors/slackweb/main.go", "cmd/connectors/upworkcrm/main.go",
		"cmd/connectors/google/main.go", "cmd/opsctl/main.go", "cmd/opsctl/gate.go",
	} {
		found := false
		for _, p := range printers {
			found = found || p == want
		}
		if !found {
			t.Errorf("%s no longer prints a capture counter line (\"appended\":%%d); the scan cannot hold it to "+
				"criterion 15. Found printers: %v", want, printers)
		}
	}
}

// ---- criterion 3: opsctl lists both columns; `try` is a verb ------------------------

func TestOpsctl_CaptureRulesListShowsThePRReviewColumnsAndTryIsAVerb(t *testing.T) {
	src := mustReadRepoFile(t, "cmd/opsctl/main.go")
	list := rvFuncSrc(src, "runCaptureRulesList")
	if list == "" {
		t.Fatalf("cmd/opsctl/main.go no longer declares runCaptureRulesList")
	}
	for _, col := range []string{"r.pr_review", "r.exclude_pr_authors"} {
		if !strings.Contains(list, col) {
			t.Errorf("runCaptureRulesList does not select %s — criterion 3: `capture-rules list` prints both on the "+
				"rule's key line; Verification step 4 reads it to confirm the seed", col)
		}
	}
	dispatch := rvFuncSrc(src, "runCaptureRules")
	if !strings.Contains(dispatch, `case "try":`) {
		t.Errorf("runCaptureRules has no `case \"try\":` — D10: `opsctl capture-rules try` is the write-nothing dry run")
	}
}

// ---- criterion 1: migration 0035 --------------------------------------------------

const prReviewMigration = "migrations/0035_capture_rules_pr_review.sql"

func TestMigration0035_CaptureRulesPRReviewShape(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("..", "..", "migrations", "0035_*.sql"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 1 || filepath.Base(matches[0]) != filepath.Base(prReviewMigration) {
		t.Fatalf("migrations/0035_*.sql = %v, want exactly %s. The SPEC's data-model section says 0034; SWT-53 "+
			"(chat-on-closed-task) owns 0034, so this ticket takes 0035 (the SPEC's status block)", matches, prReviewMigration)
	}
	sql := strings.ToLower(mustReadRepoFile(t, prReviewMigration))
	code := regexp.MustCompile(`(?m)--.*$`).ReplaceAllString(sql, "")
	norm := strings.Join(strings.Fields(code), " ")
	for _, want := range []struct{ re, why string }{
		{`alter table capture_rules`, "two CONFIGURATION columns on capture_rules (invariant 2: no new table)"},
		{`add column pr_review boolean not null default false`, "criterion 1"},
		{`add column exclude_pr_authors text ?\[\] not null default '\{\}'`, "criterion 1"},
		{`add constraint capture_rules_pr_review_github check \( ?not pr_review or \( ?external_system = 'github' and key_regex is not null and not revive ?\) ?\)`,
			"criterion 1: a review rule is a github rule with a key_regex and never revives (revive stays jira-only)"},
		{`add constraint capture_rules_exclude_needs_pr_review check \( ?cardinality ?\( ?exclude_pr_authors ?\) = 0 or pr_review ?\)`,
			"criterion 1: an exclude list means nothing without pr_review"},
	} {
		if !regexp.MustCompile(want.re).MatchString(norm) {
			t.Errorf("%s does not match /%s/ — %s", prReviewMigration, want.re, want.why)
		}
	}
	for _, bad := range []struct{ re, why string }{
		{`insert\s+into`, "no seeding: rules go through capture_rule_add (the audit row is their provenance)"},
		{`\bupdate\b`, "no backfill"},
		{`delete\s+from`, "forward-only, no data change"},
		{`drop\s+column`, "forward-only"},
		{`create\s+table`, "no new table: tasks, external_refs and capture_decisions are reused as is"},
		{`\[bot\]`, "OQ-1 = (b): bot PRs get tasks; nothing seeds a bot exclusion, least of all a migration"},
	} {
		if regexp.MustCompile(bad.re).MatchString(norm) {
			t.Errorf("%s matches /%s/ — %s", prReviewMigration, bad.re, bad.why)
		}
	}
}

// GREEN guard (see the header): the ledger accepts 35 and names its owner above
// the marker. It does not refuse 34 — SWT-53 owns 0034 on another branch.
func TestMigrationLedger_Learns0035(t *testing.T) {
	src := mustReadRepoFile(t, "internal/classify/structure_test.go")
	i := strings.Index(src, "THIS LEDGER IS THE LIVING REGISTRY")
	if i < 0 {
		t.Fatalf("the migration ledger marker is gone; REWRITE the guard, never delete it")
	}
	start, end := max(i-3500, 0), min(i+1800, len(src))
	if !regexp.MustCompile(`n\s*!=\s*35\b`).MatchString(src[start:end]) {
		t.Errorf("the ledger does not accept 35 (`n != 35`); 0035_capture_rules_pr_review.sql would be flagged as unowned")
	}
	if !strings.Contains(src[start:i], "35 is SWT-54") {
		t.Errorf("the ledger accepts 35 without the ownership note \"35 is SWT-54 ...\" ABOVE the marker")
	}
}
