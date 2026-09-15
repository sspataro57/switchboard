package dashboard

// board-incoming-first (SWT-59, docs/tickets/board-incoming-first_SPEC.md)
// criteria 2, 3, 5, 6 (structure halves) and 11-14. ZERO I/O beyond this
// package's own source and the embedded tasks.html.
//
// This file references NO symbol the SPEC adds: it reads source text only, so
// each test here fails on its own assertion. (The package's test binary still
// compile-FAILS until sections_incoming_test.go finds its symbols.)
//
// Two tests here are GUARDS that would already pass on main 8028d3c if the
// binary compiled: TestBoardLightFacts_SecondStatementByteUnchanged and
// TestTasksTemplate_NoIncoming. They pin what the implementation must NOT
// change. TestBoardLightFacts_NeverReadsThreadSurfacingOrAttached is a guard too.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - boardSections calls sectionFor instead of boardSectionOf -> IncomingFunctionsDeclaredAndPure.
//   - `cp.task_id IS NOT NULL` or the COALESCE dropped -> FirstStatementReadsTheIncomingFacts.
//   - t.source_thread_id IS NOT NULL as the message fact -> NeverReadsThreadSurfacingOrAttached,
//     FirstStatementReadsTheIncomingFacts.
//   - 'attached' added to the action list -> both of the above.
//   - the facts moved into a third statement -> FirstStatementReadsTheIncomingFacts (the count).
//   - lightFor reads FromMessage -> IncomingFactsAreDisplayOnly.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"
)

// ---- helpers --------------------------------------------------------------------

// lightFactsStatements splits boardLightFacts' body at its s.pool.Query calls:
// first is the slice from the first call up to the second, second the rest.
func lightFactsStatements(t *testing.T) (body, first, second string) {
	t.Helper()
	body = funcBodySrc(t, "board.go", "boardLightFacts")
	if body == "" {
		t.Fatalf("board.go declares no boardLightFacts")
	}
	const q = "s.pool.Query("
	i := strings.Index(body, q)
	if i < 0 {
		t.Fatalf("boardLightFacts runs no s.pool.Query")
	}
	first = body[i:]
	if j := strings.Index(first[len(q):], q); j >= 0 {
		first, second = first[:len(q)+j], first[len(q)+j:]
	}
	if !strings.Contains(first, "HH24:MI:SS") {
		t.Fatalf("CONTROL: the first statement does not carry the render time; the split is not reading statement 1")
	}
	return body, first, second
}

// coalesceSpans returns [start, end) of every COALESCE( ... ) in s, matching
// parentheses and skipping single-quoted SQL strings.
func coalesceSpans(s string) [][2]int {
	var out [][2]int
	for i := 0; ; {
		j := strings.Index(s[i:], "COALESCE(")
		if j < 0 {
			return out
		}
		start := i + j
		depth, inStr, end := 0, false, -1
	scan:
		for k := start + len("COALESCE"); k < len(s); k++ {
			c := s[k]
			if inStr {
				if c == '\'' {
					inStr = false
				}
				continue
			}
			switch c {
			case '\'':
				inStr = true
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					end = k + 1
					break scan
				}
			}
		}
		if end > 0 {
			out = append(out, [2]int{start, end})
		}
		i = start + len("COALESCE(")
	}
}

var incomingFactTokens = []struct {
	name string
	re   *regexp.Regexp
}{
	{"classify_promotions", regexp.MustCompile(`\bclassify_promotions\b`)},
	{"cp.task_id IS NOT NULL", regexp.MustCompile(`cp\.task_id\s+IS\s+NOT\s+NULL`)},
	{"('task','review')", regexp.MustCompile(`\(\s*'task'\s*,\s*'review'\s*\)`)},
	{"external_refs", regexp.MustCompile(`\bexternal_refs\b`)},
	{"er.system = 'github'", regexp.MustCompile(`er\.system\s*=\s*'github'`)},
	{"t.assignee_type = 'human'", regexp.MustCompile(`t\.assignee_type\s*=\s*'human'`)},
}

// ---- criteria 2, 3 and 11: the declarations and their purity ----------------------

func TestIncomingSections_IncomingFunctionsDeclaredAndPure(t *testing.T) {
	src := readSrc(t, "sections.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "sections.go", src, 0)
	if err != nil {
		t.Fatalf("parse sections.go: %v", err)
	}
	consts := map[string]string{}
	sigs := map[string]string{}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.GenDecl:
			if d.Tok != token.CONST {
				continue
			}
			for _, s := range d.Specs {
				vs := s.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if i < len(vs.Values) {
						if bl, ok := vs.Values[i].(*ast.BasicLit); ok {
							consts[n.Name] = bl.Value
						}
					}
				}
			}
		case *ast.FuncDecl:
			if d.Recv == nil && d.Body != nil {
				sigs[d.Name.Name] = flatWS(src[fset.Position(d.Pos()).Offset:fset.Position(d.Body.Pos()).Offset])
			}
		}
	}
	for name, want := range map[string]string{"incomingMessage": `"message"`, "incomingPRReview": `"pr_review"`} {
		if consts[name] != want {
			t.Errorf("sections.go does not declare const %s = %s (criterion 2); got %q", name, want, consts[name])
		}
	}
	for name, want := range map[string]string{
		"incomingKind":   "func incomingKind(fromMessage, prReview bool) string",
		"boardSectionOf": "func boardSectionOf(r taskRow) string",
		"sectionFor":     "func sectionFor(status string, l light) string", // unchanged (criterion 2)
	} {
		if sigs[name] != want {
			t.Errorf("sections.go: %s's signature is %q, want %q (criterion 2)", name, sigs[name], want)
		}
	}
	for _, fn := range []string{"incomingKind", "boardSectionOf"} {
		body := funcBodySrc(t, "sections.go", fn)
		if body == "" {
			t.Errorf("sections.go declares no func %s (criteria 2, 11)", fn)
			continue
		}
		for _, banned := range []string{"s.pool", "Query(", "Exec(", "time."} {
			if strings.Contains(body, banned) {
				t.Errorf("%s contains %q: it must be a pure function (criterion 11, invariant 7)", fn, banned)
			}
		}
	}
	if body := funcBodySrc(t, "sections.go", "boardSectionOf"); body != "" &&
		!regexp.MustCompile(`sectionFor\(\s*r\.Status\s*,\s*r\.Light\s*\)`).MatchString(body) {
		t.Errorf("boardSectionOf does not fall back to sectionFor(r.Status, r.Light) (criterion 2: one table, not a second spelling)")
	}
	if body := funcBodySrc(t, "sections.go", "sectionFor"); strings.Contains(strings.ToLower(body), "incoming") {
		t.Errorf("sectionFor's body mentions incoming; its table is unchanged and it never returns incoming (criterion 2)")
	}
	sb := funcBodySrc(t, "sections.go", "boardSections")
	if sb == "" {
		t.Fatalf("sections.go declares no boardSections")
	}
	if !strings.Contains(sb, "boardSectionOf(") {
		t.Errorf("boardSections' body does not call boardSectionOf( (criteria 3, 11)")
	}
	if strings.Contains(sb, "sectionFor(") {
		t.Errorf("boardSections calls sectionFor directly; it groups through boardSectionOf only (criterion 3)")
	}
}

// ---- criteria 5 and 12: the first statement carries both facts ---------------------

func TestBoardLightFacts_FirstStatementReadsTheIncomingFacts(t *testing.T) {
	body, first, second := lightFactsStatements(t)
	if n := strings.Count(body, "s.pool.Query"); n > 2 {
		t.Errorf("boardLightFacts issues %d statements, want at most two (criterion 5, D15's count)", n)
	}
	spans := coalesceSpans(first)
	inSpan := func(at int) bool {
		for _, s := range spans {
			if s[0] <= at && at < s[1] {
				return true
			}
		}
		return false
	}
	for _, tok := range incomingFactTokens {
		locs := tok.re.FindAllStringIndex(first, -1)
		if len(locs) == 0 {
			t.Errorf("boardLightFacts' FIRST statement does not mention %s (criterion 12)", tok.name)
			continue
		}
		for _, l := range locs {
			if !inSpan(l[0]) {
				t.Errorf("boardLightFacts' first statement mentions %s outside any COALESCE( ... ) (criterion 12, I1's NULL safety)", tok.name)
			}
		}
		if second != "" && tok.re.MatchString(second) {
			t.Errorf("boardLightFacts' SECOND statement mentions %s; the facts come from the first (criterion 12)", tok.name)
		}
	}

	// Each fact is ONE COALESCEd IN (subquery) expression ending in `, false)`,
	// aliased as the column the scan reads.
	for _, fact := range []struct {
		alias string
		must  []*regexp.Regexp
	}{
		{"from_message", []*regexp.Regexp{
			regexp.MustCompile(`t\.id\s+IN\s*\(\s*SELECT\s+cp\.task_id\s+FROM\s+classify_promotions\s+cp\b`),
			incomingFactTokens[1].re, incomingFactTokens[2].re,
		}},
		{"pr_review", []*regexp.Regexp{
			incomingFactTokens[5].re,
			regexp.MustCompile(`t\.id\s+IN\s*\(\s*SELECT\s+er\.task_id\s+FROM\s+external_refs\s+er\b`),
			incomingFactTokens[4].re,
		}},
	} {
		found := false
		for _, s := range spans {
			expr := first[s[0]:s[1]]
			all := true
			for _, re := range fact.must {
				if !re.MatchString(expr) {
					all = false
				}
			}
			if !all {
				continue
			}
			found = true
			if !regexp.MustCompile(`,\s*false\s*\)$`).MatchString(expr) {
				t.Errorf("the %s expression does not end in `, false)`: COALESCE(..., false) (criterion 5, I1)", fact.alias)
			}
			if !regexp.MustCompile(`^\s*AS\s+` + fact.alias + `\b`).MatchString(first[s[1]:]) {
				t.Errorf("the COALESCEd %s expression is not aliased AS %s (criterion 5)", fact.alias, fact.alias)
			}
		}
		if !found {
			t.Errorf("boardLightFacts' first statement has no single COALESCE( ... ) holding the %s IN (subquery) expression "+
				"of criterion 5", fact.alias)
		}
		if !regexp.MustCompile(`\bf\.` + fact.alias + `\b`).MatchString(first) {
			t.Errorf("the outer select list does not read f.%s (criterion 5: inner f select AND outer list)", fact.alias)
		}
	}
	for _, f := range []string{"FromMessage", "PRReview"} {
		if !strings.Contains(first, f) {
			t.Errorf("boardLightFacts never sets lightFacts.%s from the first statement's scan (criterion 5)", f)
		}
	}
}

// Criterion 5: the second statement (the queue-head candidates) is byte-unchanged.
// GUARD: passes on main.
func TestBoardLightFacts_SecondStatementByteUnchanged(t *testing.T) {
	_, _, second := lightFactsStatements(t)
	if second == "" {
		t.Fatalf("boardLightFacts has no second statement (the queue-head candidates)")
	}
	end := strings.Index(second, "if err")
	if end < 0 {
		t.Fatalf("cannot find the end of the second statement")
	}
	const want = "s.pool.Query(ctx,\n`SELECT t.id, t.assignee_type, t.project_id, COALESCE(p.slug,''), COALESCE(p.client,''), COALESCE(t.subproject,'')\n" +
		"FROM tasks t JOIN projects p ON p.id = t.project_id\nWHERE t.status = 'ready'\nORDER BY `+tools.TaskQueueOrder)"
	if got := normSQL(second[:end]); got != normSQL(want) {
		t.Errorf("boardLightFacts' second statement changed (criterion 5: byte-unchanged):\n got %s\nwant %s", got, normSQL(want))
	}
}

// Criteria 5 and 12 (I1, I7): boardLightFacts and what it reaches never read
// the thread, the SWT-45 surfacing column, the message table or 'attached'.
// GUARD: passes on main.
func TestBoardLightFacts_NeverReadsThreadSurfacingOrAttached(t *testing.T) {
	ps := parseDashboardSource(t)
	if _, ok := ps.text["boardLightFacts"]; !ok {
		t.Fatalf("internal/dashboard declares no boardLightFacts")
	}
	src, _ := ps.reach("boardLightFacts")
	for _, banned := range []struct{ tok, why string }{
		{"source_thread_id", "capture sets it on every ticket task too (I1)"},
		{"surfaced_by_message_id", "that is SWT-45's Jira-activity revive (I1)"},
		{"normalized_messages", "no thread_id index: a scan every 5 s (I1, I7)"},
		{"'attached'", "an attached promotion created nothing (I1)"},
	} {
		if strings.Contains(src, banned.tok) {
			t.Errorf("boardLightFacts (or what it reaches) mentions %s: %s (criterion 5)", banned.tok, banned.why)
		}
	}
}

// ---- criterion 6: listTasks, taskRow, the exports ------------------------------------

func TestListTasks_SetsIncomingFromTheFacts(t *testing.T) {
	body := funcBodySrc(t, "board.go", "listTasks")
	if body == "" {
		t.Fatalf("board.go declares no listTasks")
	}
	if !regexp.MustCompile(`Incoming\s*:?=?\s*incomingKind\(\s*f\.FromMessage\s*,\s*f\.PRReview\s*\)`).MatchString(body) {
		t.Errorf("listTasks does not set tr.Incoming = incomingKind(f.FromMessage, f.PRReview) (criterion 6)")
	}
	row := structFieldNames(t, "board.go", "taskRow")
	if !row["Incoming"] {
		t.Errorf("taskRow has no Incoming field (criterion 4)")
	}
	exp := structFieldNames(t, "export.go", "TaskExportRow")
	if exp == nil {
		t.Fatalf("export.go declares no TaskExportRow")
	}
	for _, f := range []string{"Incoming", "FromMessage", "PRReview"} {
		if exp[f] {
			t.Errorf("TaskExportRow has %s; incoming is board-only, never an export column (criterion 4)", f)
		}
	}
	if q := funcBodySrc(t, "board.go", "boardQuery"); strings.Contains(q, "classify_promotions") || strings.Contains(q, "external_refs") {
		t.Errorf("boardQuery reads classify_promotions or external_refs; it is byte-unchanged (criterion 6)")
	}
}

// ---- criterion 13: the two facts are display-only -------------------------------------

func TestLightFacts_IncomingFactsAreDisplayOnly(t *testing.T) {
	body := funcBodySrc(t, "lights.go", "lightFor")
	if body == "" {
		t.Fatalf("lights.go declares no lightFor")
	}
	for _, f := range []string{"FromMessage", "PRReview"} {
		if strings.Contains(body, f) {
			t.Errorf("lightFor's body mentions %s. Criterion 13: incoming is placement; the light never reads it", f)
		}
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "lights.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse lights.go: %v", err)
	}
	var st *ast.StructType
	ast.Inspect(f, func(n ast.Node) bool {
		if ts, ok := n.(*ast.TypeSpec); ok && ts.Name.Name == "lightFacts" {
			st, _ = ts.Type.(*ast.StructType)
			return false
		}
		return true
	})
	if st == nil {
		t.Fatalf("lights.go declares no lightFacts struct")
	}
	var structComments []string
	for _, cg := range f.Comments {
		if cg.Pos() >= st.Pos() && cg.End() <= st.End() {
			structComments = append(structComments, cg.Text())
		}
	}
	for _, name := range []string{"FromMessage", "PRReview"} {
		found, isBool, documented := false, false, false
		for _, fl := range st.Fields.List {
			for _, nm := range fl.Names {
				if nm.Name != name {
					continue
				}
				found = true
				if id, ok := fl.Type.(*ast.Ident); ok && id.Name == "bool" {
					isBool = true
				}
				for _, cg := range []*ast.CommentGroup{fl.Doc, fl.Comment} {
					if cg != nil && strings.Contains(strings.ToLower(cg.Text()), "display") {
						documented = true
					}
				}
			}
		}
		for _, c := range structComments {
			if strings.Contains(c, name) && strings.Contains(strings.ToLower(c), "display") {
				documented = true
			}
		}
		switch {
		case !found:
			t.Errorf("lightFacts has no %s field (criterion 4)", name)
		case !isBool:
			t.Errorf("lightFacts.%s is not a bool (criterion 4)", name)
		case !documented:
			t.Errorf("lightFacts.%s is not documented as display-only (criterion 13)", name)
		}
	}
}

// ---- criterion 14: the template stays status- and kind-blind ------------------------
// GUARD: passes on main.

func TestTasksTemplate_NoIncoming(t *testing.T) {
	s := tasksHTML(t)
	if strings.Contains(s, ".Incoming") {
		t.Errorf("tasks.html references .Incoming; the section comes from data (criterion 14)")
	}
	if strings.Contains(strings.ToLower(s), "incoming") {
		t.Errorf("tasks.html mentions incoming; the template is byte-unchanged and ranges over .Sections (criterion 14)")
	}
}

func readSrc(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	return string(b)
}
