package tools

// SWT-35 (docs/tickets/task-list-mcp_SPEC.md) criteria 5 and 6 — the two things
// the SPEC wants spelled ONCE, enforced mechanically. ZERO I/O beyond reading
// this package's own source.
//
// HOW IT SCANS, and why that way:
//   - Literals are counted in Go STRING LITERALS (go/ast BasicLit), whitespace-
//     collapsed, across the NON-TEST .go files of internal/tools. Comments are
//     not code: prose may quote the old spelling to explain why it is gone
//     (upworkcrm/keyspelling_test.go's rule), and a copy reformatted across
//     lines is still a copy.
//   - "References the const" is proven by REACHABILITY, not by grepping the
//     handler's body: from the handler, follow every identifier naming a
//     package-level func, const or var, transitively, until the const is found.
//     The SPEC asks for the WHERE fragment to be built ONCE (L8), so task_list
//     will reach the in-play const through a helper; a body grep would fail a
//     correct implementation and pass one that names the const in a dead
//     variable.
//   - Every scan first REQUIRES its subject (the handlers, the file that
//     declares each const, a SQL literal the scan can see). A scan that passes
//     because it saw nothing is the "fixture that proves nothing" landmine
//     wearing a lab coat (internal/classify/inquiry_structure_test.go).
//
// GREENFIELD NOTE — EXPECTED RED. taskQueueOrder and inPlayPredicate are not
// declared, so this file compile-FAILS internal/tools alongside tasklist_test.go.
// Once they exist, the reachability checks stay red until taskList and
// projectList exist and use them.
//
// IMPOSED SURFACE: see tasklist_test.go's header. This file additionally pins
// WHERE each const lives: taskQueueOrder in getnext.go (L7: "an unexported const
// in getnext.go, used by both handlers") and inPlayPredicate in tasklist.go
// ("Files likely to touch": "the in-play predicate const").

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type toolsLit struct{ file, text string }

type toolsSource struct {
	funcs     map[string]*ast.FuncDecl
	values    map[string]*ast.ValueSpec
	valueFile map[string]string
	lits      []toolsLit
}

var wsRun = regexp.MustCompile(`\s+`)

// parseToolsSource parses every non-test .go file of this package (the test
// binary's working directory is the package directory).
func parseToolsSource(t *testing.T) toolsSource {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob internal/tools/*.go: %v", err)
	}
	src := toolsSource{
		funcs:     map[string]*ast.FuncDecl{},
		values:    map[string]*ast.ValueSpec{},
		valueFile: map[string]string{},
	}
	fset := token.NewFileSet()
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse internal/tools/%s: %v", name, err)
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					src.funcs[d.Name.Name] = d
				}
			case *ast.GenDecl:
				if d.Tok != token.CONST && d.Tok != token.VAR {
					continue
				}
				for _, s := range d.Specs {
					vs := s.(*ast.ValueSpec)
					for _, n := range vs.Names {
						src.values[n.Name] = vs
						src.valueFile[n.Name] = name
					}
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING {
				src.lits = append(src.lits, toolsLit{name, wsRun.ReplaceAllString(bl.Value, " ")})
			}
			return true
		})
	}
	return src
}

// reaches reports whether the function root, following identifiers that name
// package-level funcs, consts or vars transitively, references target.
func (s toolsSource) reaches(root, target string) bool {
	seen := map[string]bool{}
	queue := []string{root}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		var node ast.Node
		if fd, ok := s.funcs[name]; ok && fd.Body != nil {
			node = fd.Body
		} else if vs, ok := s.values[name]; ok {
			node = vs
		} else {
			continue
		}
		found := false
		ast.Inspect(node, func(n ast.Node) bool {
			if found {
				return false
			}
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			if id.Name == target {
				found = true
				return false
			}
			if _, ok := s.funcs[id.Name]; ok {
				queue = append(queue, id.Name)
			}
			if _, ok := s.values[id.Name]; ok {
				queue = append(queue, id.Name)
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// requireSQLVisible is the positive control shared by both scans: the literal
// walk must see at least one query against tasks, or "zero second spellings"
// means "zero literals seen".
func requireSQLVisible(t *testing.T, src toolsSource) {
	t.Helper()
	fromTasks := regexp.MustCompile(`(?i)\bfrom\s+tasks\b`)
	for _, l := range src.lits {
		if fromTasks.MatchString(l.text) {
			return
		}
	}
	t.Fatalf("POSITIVE CONTROL FAILED: no string literal in internal/tools non-test sources contains " +
		"`FROM tasks`, yet getnext.go queries it. This scan is not seeing the files it claims to check")
}

// ---- criterion 5: ONE spelling of the queue order ---------------------------

// L7: "The ORDER BY fragment becomes an unexported const in getnext.go, used by
// both handlers. A structural test fails on a second copy of the literal."
//
// The failure mode is silent drift: task_list exists so that its first
// ready/claude row is the task a worker would be handed (criterion 8 proves it
// against a db). Two copies of the ORDER BY agree for exactly as long as nobody
// edits one of them, and after that "what's in my queue" answers in an order no
// worker follows.
func TestTaskList_OrderingSpelledOnce(t *testing.T) {
	src := parseToolsSource(t)
	for _, fn := range []string{"getNext", "taskList"} {
		if _, ok := src.funcs[fn]; !ok {
			t.Fatalf("internal/tools declares no func %s; criterion 5 checks that BOTH handlers share one "+
				"ORDER BY, and a scan over a missing handler proves nothing", fn)
		}
	}
	requireSQLVisible(t, src)

	if got := src.valueFile["taskQueueOrder"]; got != "getnext.go" {
		t.Errorf("taskQueueOrder is declared in %q, want getnext.go. L7: the fragment moves OUT of "+
			"getNext's query into a const beside it, where getnext_ordering_integration_test.go already "+
			"pins its behaviour", got)
	}
	const wantOrder = "t.priority DESC, t.plan_order ASC NULLS LAST, t.created_at ASC, t.id ASC"
	if !strings.Contains(wsRun.ReplaceAllString(taskQueueOrder, " "), wantOrder) {
		t.Errorf("taskQueueOrder = %q, want it to hold %q — getnext.go's behaviour is unchanged by the "+
			"extraction (SPEC, 'getnext.go')", taskQueueOrder, wantOrder)
	}

	literal := regexp.MustCompile(`(?i)\bt\.plan_order\s+ASC\s+NULLS\s+LAST\b`)
	count := 0
	var where []string
	for _, l := range src.lits {
		if n := len(literal.FindAllStringIndex(l.text, -1)); n > 0 {
			count += n
			where = append(where, l.file)
		}
	}
	if count != 1 {
		t.Errorf("the literal `t.plan_order ASC NULLS LAST` occurs %d time(s) in internal/tools string "+
			"literals (in %v), want exactly 1 — the taskQueueOrder const. L7: task_list and task_get_next "+
			"share ONE spelling of the queue order", count, where)
	}

	for _, fn := range []string{"getNext", "taskList"} {
		if !src.reaches(fn, "taskQueueOrder") {
			t.Errorf("%s never reaches taskQueueOrder (directly or through a helper). L7: the ORDER BY is "+
				"used by BOTH handlers; a handler with its own ORDER BY is the second spelling this "+
				"criterion exists to stop", fn)
		}
	}
}

// ---- criterion 26: ONE read-only snapshot -----------------------------------

// Added after the Codex pass of 2026-09-10. Reusing the WHERE string (L8) stops
// the rows and the counts drifting in PREDICATE; it does not stop them drifting
// in TIME. Two statements on the pool can straddle a claim or a close, and the
// response then carries a row its own counts exclude. The resolve, the page and
// the counts run on one REPEATABLE READ, READ ONLY transaction; the semantics
// are Postgres's, so what is pinned here is that taskList asks for them and
// never bypasses the transaction.
func TestTaskList_ReadsInOneReadOnlySnapshot(t *testing.T) {
	src := parseToolsSource(t)
	fd, ok := src.funcs["taskList"]
	if !ok {
		t.Fatal("internal/tools declares no func taskList; nothing to check")
	}
	var begins, txReads int
	var repeatable, readOnly bool
	var bypass []string
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if x, ok := n.X.(*ast.Ident); ok && x.Name == "pgx" {
				repeatable = repeatable || n.Sel.Name == "RepeatableRead"
				readOnly = readOnly || n.Sel.Name == "ReadOnly"
			}
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			x, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			switch {
			case x.Name == "pool" && sel.Sel.Name == "BeginTx":
				begins++
			case x.Name == "pool":
				bypass = append(bypass, "pool."+sel.Sel.Name)
			case x.Name == "tx" && (sel.Sel.Name == "Query" || sel.Sel.Name == "QueryRow"):
				txReads++
			}
		}
		return true
	})
	if begins != 1 {
		t.Fatalf("taskList calls pool.BeginTx %d time(s), want exactly 1 — the one snapshot", begins)
	}
	if txReads < 3 {
		t.Fatalf("POSITIVE CONTROL FAILED: taskList makes %d tx.Query/QueryRow call(s), want 3 (resolve, page, "+
			"counts). The scan is not seeing the reads it claims to check", txReads)
	}
	if !repeatable || !readOnly {
		t.Errorf("taskList's transaction options name RepeatableRead=%v ReadOnly=%v, want both: one snapshot for "+
			"rows and counts, and Postgres refusing any write", repeatable, readOnly)
	}
	if len(bypass) > 0 {
		t.Errorf("taskList reads outside its snapshot via %v — a row and its counts could then straddle a "+
			"concurrent claim or close", bypass)
	}
}

// ---- criterion 6: ONE spelling of the in_play predicate ---------------------

// L4: "The predicate is spelled once, as `t.status NOT IN ('closed','delivered')`,
// in a const shared by both tools." L11: project_list's in_play count is "the
// total task_list(project) would report under its default, computed from the
// SAME predicate const" — criterion 16 proves the parity against a db, this
// proves there is nothing to drift.
//
// SCOPE — why the scan is internal/tools ONLY, deliberately:
//   - internal/dashboard/board.go:72 hides only `closed` (`t.status <> 'closed'`)
//     when no ?status= is given. That is the BOARD's default, a human view with
//     room for finished work; task_list diverges from it ON PURPOSE (Q2 = b,
//     "filter so no junk and wasted tokens"). Sharing the const would change the
//     board, which this ticket does not touch.
//   - internal/promote spells `NOT IN ('closed','delivered')` for its own purpose
//     (promote/store.go:309, promote.go:86: "does this thread already have a live
//     task"). It is a sibling concept in another package that happens to be
//     spelled alike; coupling the two would let a change to "what is in a
//     model's queue" silently change what the promoter dedupes against.
//     Both are named as out of scope in the SPEC ("Out of scope": "sharing the
//     in-play const with internal/dashboard or internal/promote").
func TestTaskList_InPlayPredicateSpelledOnce(t *testing.T) {
	src := parseToolsSource(t)
	for _, fn := range []string{"taskList", "projectList"} {
		if _, ok := src.funcs[fn]; !ok {
			t.Fatalf("internal/tools declares no func %s; criterion 6 checks that task_list and "+
				"project_list share ONE in_play predicate, and a scan over a missing handler proves nothing", fn)
		}
	}
	requireSQLVisible(t, src)

	if got := src.valueFile["inPlayPredicate"]; got != "tasklist.go" {
		t.Errorf("inPlayPredicate is declared in %q, want tasklist.go (the SPEC's file for the in-play "+
			"predicate const)", got)
	}
	literal := regexp.MustCompile(`(?i)NOT\s+IN\s*\(\s*'closed'\s*,\s*'delivered'\s*\)|NOT\s+IN\s*\(\s*'delivered'\s*,\s*'closed'\s*\)`)
	pred := wsRun.ReplaceAllString(inPlayPredicate, " ")
	if !literal.MatchString(pred) || !strings.Contains(pred, "t.status") {
		t.Errorf("inPlayPredicate = %q, want `t.status NOT IN ('closed','delivered')` (L4). The alias is "+
			"part of the contract: project_list uses it inside count(t.id) FILTER (WHERE …)", inPlayPredicate)
	}

	count := 0
	var where []string
	for _, l := range src.lits {
		if n := len(literal.FindAllStringIndex(l.text, -1)); n > 0 {
			count += n
			where = append(where, l.file)
		}
	}
	if count != 1 {
		t.Errorf("`NOT IN ('closed','delivered')` occurs %d time(s) in internal/tools string literals (in "+
			"%v), want exactly 1 — the inPlayPredicate const (L4/L11)", count, where)
	}

	// The other way to write a second copy: two inequalities. Scoped to
	// tasklist.go, where it would be a restatement; elsewhere in the package
	// such a clause means something else.
	inequality := regexp.MustCompile(`(?i)status\s*(<>|!=)\s*'(closed|delivered)'`)
	for _, l := range src.lits {
		if l.file == "tasklist.go" && inequality.MatchString(l.text) {
			t.Errorf("tasklist.go spells the in_play set as an inequality (%q). That is a second spelling of "+
				"inPlayPredicate: the two drift the first time someone edits one", inequality.FindString(l.text))
		}
	}

	for _, fn := range []string{"taskList", "projectList"} {
		if !src.reaches(fn, "inPlayPredicate") {
			t.Errorf("%s never reaches inPlayPredicate (directly or through a helper). L4/L11: task_list's "+
				"default AND project_list's in_play count use the SAME const — otherwise project_list can "+
				"report a count task_list will not return", fn)
		}
	}
}
