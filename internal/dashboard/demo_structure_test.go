package dashboard

// demo-mode (SWT-99, docs/tickets/demo-mode_SPEC.md) structure checks: criterion 7
// (every SQL literal that FROM/JOINs a guarded table carries an @demo. marker or
// sits in a demoExempt owner), 8 (every pool-taking call into another package is
// a named seam in demoSeams), 31 (only demo.go may spell demo_hidden, and nothing
// writes it) and 5's single spelling of R7's brief pattern. ZERO I/O beyond
// reading this repo's source. Precedents: boardSQLTables (live_test.go) and
// internal/capture/gate_structure_test.go. Each scan first REQUIRES its subject:
// a scan with nothing to scan proves nothing.
//
// A "literal" here is one SQL statement as written: a string literal, or a whole
// `+` chain of them (boardLightFacts splices boardDayStart(...) between pieces),
// joined. Its OWNER is the enclosing top-level func/method name, or the name of
// the package-level var/const it initialises (resolveSourceSQL, threadSQL).
//
// GREENFIELD NOTE — EXPECTED RED: demoExempt, demoSeams and demo.go do not exist,
// so this file compile-FAILS with demo_test.go. Once they exist, every guarded
// literal without a marker is listed by name.
//
// MUTATIONS THAT MUST TURN THIS FILE RED: M1 (delete @demo.task from boardQuery),
// M9 (remove @demo.account from slackWatchRows), M11 (a new SELECT … FROM tasks
// func with no marker), M15 (UPDATE tasks SET demo_hidden in a handler), a new
// pool-taking seam not in demoSeams, a stale demoExempt entry.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// demoGuardedTables is criterion 7's list, verbatim.
var demoGuardedTables = []string{
	"tasks", "deliveries", "plan_imports", "projects", "source_accounts", "raw_source_items",
	"normalized_messages", "slack_watch", "sync_runs", "task_events", "feedback_requests",
	"external_refs", "task_dependencies", "task_dismissals", "classify_promotions",
	"capture_decisions", "ai_extractions",
}

var demoFromJoin = regexp.MustCompile(`\b(?:FROM|JOIN)\s+([A-Za-z_][A-Za-z0-9_.]*)\b(\s*\()?`)

type demoLiteral struct {
	file, owner string
	line        int
	sql         string
	tables      []string // guarded tables it FROM/JOINs
}

// demoPackageFiles parses every non-test .go file in internal/dashboard.
func demoPackageFiles(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, n := range names {
		if strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		files[n] = f
	}
	if len(files) < 5 {
		t.Fatalf("CONTROL: parsed %d non-test files in internal/dashboard; the scan is not reading the package", len(files))
	}
	return fset, files
}

// demoStatements returns every string literal / `+` chain of literals under n.
func demoStatements(n ast.Node) []struct {
	pos token.Pos
	sql string
} {
	var out []struct {
		pos token.Pos
		sql string
	}
	var visit func(n ast.Node)
	var leaves func(e ast.Expr, acc *[]string, rest *[]ast.Expr)
	leaves = func(e ast.Expr, acc *[]string, rest *[]ast.Expr) {
		switch x := e.(type) {
		case *ast.ParenExpr:
			leaves(x.X, acc, rest)
		case *ast.BinaryExpr:
			if x.Op == token.ADD {
				leaves(x.X, acc, rest)
				leaves(x.Y, acc, rest)
				return
			}
			*rest = append(*rest, x)
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				if s, err := strconv.Unquote(x.Value); err == nil {
					*acc = append(*acc, s)
				}
				return
			}
		default:
			*rest = append(*rest, x)
		}
	}
	visit = func(n ast.Node) {
		ast.Inspect(n, func(m ast.Node) bool {
			switch x := m.(type) {
			case *ast.BinaryExpr:
				if x.Op != token.ADD {
					return true
				}
				var acc []string
				var rest []ast.Expr
				leaves(x, &acc, &rest)
				if len(acc) > 0 {
					out = append(out, struct {
						pos token.Pos
						sql string
					}{x.Pos(), strings.Join(acc, " ")})
				}
				for _, r := range rest {
					visit(r)
				}
				return false
			case *ast.BasicLit:
				if x.Kind == token.STRING {
					if s, err := strconv.Unquote(x.Value); err == nil {
						out = append(out, struct {
							pos token.Pos
							sql string
						}{x.Pos(), s})
					}
				}
			}
			return true
		})
	}
	visit(n)
	return out
}

// demoScanLiterals returns every guarded literal in the package, and every
// top-level owner name (funcs, methods, package-level vars and consts).
func demoScanLiterals(t *testing.T) ([]demoLiteral, map[string]bool) {
	t.Helper()
	guarded := map[string]bool{}
	for _, g := range demoGuardedTables {
		guarded[g] = true
	}
	fset, files := demoPackageFiles(t)
	owners := map[string]bool{}
	var lits []demoLiteral
	add := func(file, owner string, n ast.Node) {
		for _, st := range demoStatements(n) {
			var tabs []string
			for _, m := range demoFromJoin.FindAllStringSubmatch(st.sql, -1) {
				if m[2] != "" {
					continue // a function call (EXTRACT(… FROM now()), jsonb_array_elements_text(…))
				}
				if guarded[m[1]] {
					tabs = append(tabs, m[1])
				}
			}
			if len(tabs) == 0 {
				continue
			}
			lits = append(lits, demoLiteral{file: file, owner: owner, line: fset.Position(st.pos).Line, sql: st.sql, tables: tabs})
		}
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, d := range files[name].Decls {
			switch x := d.(type) {
			case *ast.FuncDecl:
				owners[x.Name.Name] = true
				if x.Body != nil {
					add(name, x.Name.Name, x.Body)
				}
			case *ast.GenDecl:
				for _, sp := range x.Specs {
					vs, ok := sp.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, id := range vs.Names {
						owners[id.Name] = true
						if i < len(vs.Values) {
							add(name, id.Name, vs.Values[i])
						}
					}
				}
			}
		}
	}
	return lits, owners
}

// demoExemptAllowed is criterion 7's closed list of what an exemption may cover:
// the task page's sub-reads keyed on an id that already passed @demo.task
// (task_events, feedback_requests) under any owner; reopenMarkers (its ids come
// from boardRows); resolveSourceSQL / threadSQL when the message marker is
// applied at the outer SELECT instead.
var demoExemptAllowed = map[string]map[string]bool{
	// demo.go's own fragment table: the subqueries the markers expand TO. It is
	// the visibility rule, so it cannot itself carry a marker (implementation
	// note, demo-mode; the SPEC did not anticipate the scan reading demo.go).
	"demoFragments":    {"tasks": true, "projects": true, "raw_source_items": true, "source_accounts": true},
	"reopenMarkers":    {"task_dismissals": true},
	"resolveSourceSQL": {"classify_promotions": true, "capture_decisions": true, "normalized_messages": true},
	"threadSQL":        {"normalized_messages": true},
}

func TestDemoStructure_EveryGuardedLiteralCarriesAMarker(t *testing.T) {
	lits, owners := demoScanLiterals(t)
	// CONTROL: the package has well over 15 such statements today.
	if len(lits) < 15 {
		t.Fatalf("CONTROL: the FROM/JOIN scan found %d guarded literals, want at least 15; it is not reading the "+
			"dashboard's SQL", len(lits))
	}
	sawTasks := false
	for _, l := range lits {
		for _, tb := range l.tables {
			if tb == "tasks" {
				sawTasks = true
			}
		}
	}
	if !sawTasks {
		t.Fatalf("CONTROL: no guarded literal reads tasks; the scan is blind")
	}

	for _, name := range demoSortedKeys(demoExempt) {
		reason := demoExempt[name]
		if !owners[name] {
			t.Errorf("demoExempt lists %q, but no func, method, var or const of that name exists in internal/dashboard "+
				"(criterion 7: a stale exemption fails)", name)
		}
		if strings.TrimSpace(reason) == "" || strings.Contains(reason, "\n") {
			t.Errorf("demoExempt[%q] = %q, want a non-empty ONE-line reason", name, reason)
		}
	}

	for _, l := range lits {
		if strings.Contains(l.sql, "@demo.") {
			continue
		}
		if _, ok := demoExempt[l.owner]; !ok {
			t.Errorf("%s:%d (%s) reads %v with no @demo. marker, and %s is not in demoExempt. Criterion 5/7: every "+
				"dashboard read of a guarded table goes through demoSQL's markers (demo.go is the only place visibility "+
				"is spelled).\nSQL: %s", l.file, l.line, l.owner, l.tables, l.owner, demoOneLine(l.sql))
			continue
		}
		// An exempt owner may only read what criterion 7 allows it to.
		allowed := demoExemptAllowed[l.owner]
		for _, tb := range l.tables {
			if tb == "task_events" || tb == "feedback_requests" || allowed[tb] {
				continue
			}
			t.Errorf("%s:%d: %s is exempt but its unmarked literal reads %s. Criterion 7 allows exemptions only for the "+
				"task page's task_events/feedback_requests sub-reads, reopenMarkers (task_dismissals) and "+
				"resolveSourceSQL/threadSQL; everything else carries a marker.\nSQL: %s",
				l.file, l.line, l.owner, tb, demoOneLine(l.sql))
		}
	}
}

func demoOneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return s
}

// ---- criterion 8: the pool-taking seams -------------------------------------------

func TestDemoStructure_EveryPoolSeamIsNamed(t *testing.T) {
	_, files := demoPackageFiles(t)
	found := map[string][]string{}
	for name, f := range files {
		imported := map[string]bool{}
		for _, im := range f.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			local := filepath.Base(p)
			if im.Name != nil {
				local = im.Name.Name
			}
			imported[local] = true
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || !imported[pkg.Name] {
				return true
			}
			for _, a := range call.Args {
				as, ok := a.(*ast.SelectorExpr)
				if !ok || as.Sel.Name != "pool" {
					continue
				}
				if id, ok := as.X.(*ast.Ident); ok && id.Name == "s" {
					key := pkg.Name + "." + sel.Sel.Name
					found[key] = append(found[key], name)
				}
			}
			return true
		})
	}
	// CONTROL: the SPEC's six seams exist today.
	for _, want := range []string{"orchestrator.Health", "capture.AttributionTrend", "classify.Summarize",
		"promote.CountersByLane", "availability.CalendarSyncStates", "tools.ResolveGmailRoute"} {
		if len(found[want]) == 0 {
			t.Errorf("CONTROL: the seam scan did not find %s(…, s.pool, …); it is not reading the calls", want)
		}
	}
	keys := make([]string, 0, len(found))
	for k := range found {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		reason, ok := demoSeams[k]
		if !ok {
			t.Errorf("%s passes s.pool (in %v) but is not in demoSeams. Criterion 8: SQL in another package cannot carry "+
				"@demo. markers, so every pool-taking seam is named with the reason its result is safe (or gated) in demo",
				k, found[k])
			continue
		}
		if strings.TrimSpace(reason) == "" || strings.Contains(reason, "\n") {
			t.Errorf("demoSeams[%q] = %q, want a non-empty ONE-line reason", k, reason)
		}
	}
}

// ---- criterion 5: R7's brief pattern has one spelling in the dashboard -------------

func TestDemoStructure_BriefPatternIsOneSharedConst(t *testing.T) {
	_, files := demoPackageFiles(t)
	var constHolders, others []string
	for name, f := range files {
		for _, d := range f.Decls {
			switch x := d.(type) {
			case *ast.GenDecl:
				for _, sp := range x.Specs {
					vs, ok := sp.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, id := range vs.Names {
						if i >= len(vs.Values) {
							continue
						}
						for _, st := range demoStatements(vs.Values[i]) {
							if strings.Contains(st.sql, "Morning brief ") {
								if x.Tok == token.CONST {
									constHolders = append(constHolders, name+":"+id.Name)
								} else {
									others = append(others, name+":"+id.Name)
								}
							}
						}
					}
				}
			case *ast.FuncDecl:
				if x.Body == nil {
					continue
				}
				for _, st := range demoStatements(x.Body) {
					if strings.Contains(st.sql, "Morning brief ") {
						others = append(others, name+":"+x.Name.Name)
					}
				}
			}
		}
	}
	if len(constHolders) != 1 {
		t.Errorf("the R7 title pattern 'Morning brief %%' is held by %d package-level const(s) %v, want exactly one. "+
			"Criterion 5: @demo.task's brief clause and listBriefs share ONE const (R7's dedup key)", len(constHolders), constHolders)
	}
	if len(others) != 0 {
		t.Errorf("'Morning brief' is spelled outside that const in %v; listBriefs and @demo.task must both use the const", others)
	}
}

// ---- criterion 31: the dashboard never writes demo_hidden --------------------------

func TestDemoStructure_OnlyDemoGoMentionsDemoHidden(t *testing.T) {
	root := filepath.Join("..", "..")
	const allowed = "internal/dashboard/demo.go"
	sawAllowed := false
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			base := d.Name()
			if rel != "." && (strings.HasPrefix(base, ".") || base == "docs" || base == "migrations" ||
				base == "vendor" || base == "node_modules" || base == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		switch filepath.Ext(rel) {
		case ".go", ".html", ".json", ".js", ".tmpl":
		default:
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		if !strings.Contains(string(b), "demo_hidden") {
			return nil
		}
		if rel == allowed {
			sawAllowed = true
			return nil
		}
		t.Errorf("%s mentions demo_hidden. Criterion 31: only internal/dashboard/demo.go may (inside @demo.task); no "+
			"tool, MCP schema, form field, template or handler reads or takes it, and psql is its only writer", rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
	if scanned < 50 {
		t.Fatalf("CONTROL: scanned %d files; the walk is not reading the repo", scanned)
	}
	if !sawAllowed {
		t.Errorf("internal/dashboard/demo.go does not mention demo_hidden; @demo.task must walk it (D9, criterion 5)")
	}

	src, err := os.ReadFile("demo.go")
	if err != nil {
		t.Fatalf("read demo.go: %v", err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "demo.go", src, 0)
	if err != nil {
		t.Fatalf("parse demo.go: %v", err)
	}
	write := regexp.MustCompile(`(?is)\b(update|insert|set\s+demo_hidden|demo_hidden\s*:=)\b`)
	lits := 0
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil || !strings.Contains(s, "demo_hidden") {
			return true
		}
		lits++
		if m := write.FindString(s); m != "" {
			t.Errorf("demo.go:%d: a literal naming demo_hidden contains %q. Criterion 31: the dashboard never writes it",
				fset.Position(lit.Pos()).Line, m)
		}
		return true
	})
	if lits == 0 {
		t.Errorf("no string literal in demo.go names demo_hidden; the @demo.task fragment must read it (D9)")
	}
}

// ---- criterion 28: migration 0048's shape --------------------------------------------

func TestMigration0048_TasksDemoHiddenShape(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "0048_*.sql"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) != 1 || filepath.Base(files[0]) != "0048_tasks_demo_hidden.sql" {
		t.Fatalf("migrations/0048_*.sql = %v, want exactly [0048_tasks_demo_hidden.sql] (criterion 28)", files)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read %s: %v", files[0], err)
	}
	src := string(b)
	var header, code []string
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			header = append(header, line[i:])
			line = line[:i]
		}
		code = append(code, line)
	}
	comments := strings.Join(header, "\n")
	for _, want := range []string{"demo-mode_SPEC", "psql"} {
		if !strings.Contains(comments, want) {
			t.Errorf("0048's header does not mention %q (criterion 28: it names this SPEC and states that only psql "+
				"writes the column)", want)
		}
	}
	sql := strings.ToLower(strings.Join(strings.Fields(strings.Join(code, " ")), " "))
	if !regexp.MustCompile(`alter table (public\.)?tasks add column demo_hidden boolean not null default false`).MatchString(sql) {
		t.Errorf("0048 is not `ALTER TABLE tasks ADD COLUMN demo_hidden boolean NOT NULL DEFAULT false` (criterion 28):\n%s", sql)
	}
	for _, bad := range []struct{ re, why string }{
		{`create (unique )?index`, "no index: the walk is by parent_id and tasks is small"},
		{`\bupdate\b`, "no backfill: psql from the runbook is the only writer"},
		{`\binsert\b`, "no seed: the demo_mode flag row is never seeded"},
		{`\bdrop\b`, "forward-only"},
		{`ops_flags`, "the switch is data written in psql, never by a migration"},
	} {
		if regexp.MustCompile(bad.re).MatchString(sql) {
			t.Errorf("0048 matches /%s/ — %s", bad.re, bad.why)
		}
	}
}

// Review hardening: demoStub turns demo filtering off; only DB-less unit test
// harnesses may set it, never production code.
func TestDemoStructure_DemoStubOnlyInTests(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	seen := 0
	for _, n := range names {
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		if !strings.Contains(string(b), "demoStub") {
			continue
		}
		seen++
		if strings.HasSuffix(n, "_test.go") {
			continue
		}
		if regexp.MustCompile(`demoStub\s*(:|=)\s*true`).Match(b) {
			t.Errorf("%s sets demoStub = true; only _test.go harnesses may (it disables demo filtering)", n)
		}
	}
	if seen == 0 {
		t.Fatalf("CONTROL: no file mentions demoStub; the scan is blind")
	}
}
