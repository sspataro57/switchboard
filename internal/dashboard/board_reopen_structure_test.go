package dashboard

// SWT-36 (docs/tickets/dismiss-reopen-on-activity_SPEC.md) criterion 17 / D12,
// the structural half: the "reopened after dismissal (<reason_code>)" marker
// is a SEPARATE read in the board path, NOT a column in boardQuery — the
// CSV/JSON exports share boardQuery and pin their header (export_test.go).
// ZERO I/O beyond reading this package's own source and embedded templates.
//
// IMPOSED SURFACE: none by name. The marker read may live in listTasks or in a
// helper it calls; this file only pins that (a) boardQuery's SELECT LIST never
// mentions the dismissal table, (b) board.go reads reopened_by_message_id
// somewhere else, and (c) the literal marker text is rendered (template or Go).
//
// AMENDED — not deleted — by SWT-52 (board-status-lights) criterion 16: the
// ban on `task_dismissals` / `reopened` was over boardQuery's whole BODY. Since
// SWT-52 the default WHERE reads OPEN dismissals as a VISIBILITY filter (a
// closed-today task with an open dismissal leaves the board at once: NOT EXISTS
// (… task_dismissals d … d.reopened_at IS NULL)). A filter is not a column: the
// exports' columns come from the SELECT LIST, so the ban is scoped to it — the
// text between `SELECT` and `FROM tasks`. The predicate must NOT be hidden in a
// const to keep the old wording green.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

func TestBoard_ReopenMarkerIsASeparateReadNotABoardQueryColumn(t *testing.T) {
	b, err := os.ReadFile("board.go")
	if err != nil {
		t.Fatalf("read board.go: %v", err)
	}
	src := string(b)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "board.go", b, 0)
	if err != nil {
		t.Fatalf("parse board.go: %v", err)
	}
	var queryBody string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "boardQuery" || fn.Body == nil {
			continue
		}
		queryBody = src[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset]
	}
	if queryBody == "" {
		t.Fatalf("board.go declares no boardQuery; the exports and the board share it (SWT-10), and this " +
			"guard is meaningless without it")
	}
	// The SELECT LIST: from the first SELECT to the FROM tasks after it.
	sel := strings.Index(queryBody, "SELECT")
	from := -1
	if sel >= 0 {
		if k := strings.Index(queryBody[sel:], "FROM tasks"); k >= 0 {
			from = sel + k
		}
	}
	if sel < 0 || from < 0 {
		t.Fatalf("POSITIVE CONTROL FAILED: boardQuery's body has no `SELECT … FROM tasks`; the select-list scan " +
			"would see nothing")
	}
	selectList := queryBody[sel:from]
	if !strings.Contains(selectList, "t.id") || !strings.Contains(selectList, "t.status") {
		t.Fatalf("POSITIVE CONTROL FAILED: the scanned select list %q holds no t.id / t.status", selectList)
	}
	for _, banned := range []string{"task_dismissals", "reopened", "working_state"} {
		if strings.Contains(selectList, banned) {
			t.Errorf("boardQuery's SELECT list mentions %q. D12 (SWT-36), D3 (SWT-52): markers and lights are "+
				"separate reads — boardQuery feeds /export/tasks.csv and .json, whose header is pinned, so a column "+
				"there changes the exports", banned)
		}
	}
	rest := strings.Replace(src, queryBody, "", 1)
	if !strings.Contains(rest, "reopened_by_message_id") {
		t.Errorf("board.go never reads task_dismissals.reopened_by_message_id outside boardQuery. D12: the " +
			"marker shows when the task is not closed AND its NEWEST dismissal row has " +
			"reopened_by_message_id IS NOT NULL — the column is the whole predicate, and a marker keyed on " +
			"reopened_at alone would also fire for a human's plain reopen (the mis-click signal)")
	}

	tmpl, err := templateFS.ReadFile("templates/tasks.html")
	if err != nil {
		t.Fatalf("read embedded tasks.html: %v", err)
	}
	if !strings.Contains(string(tmpl), "reopened after dismissal") && !strings.Contains(src, "reopened after dismissal") {
		t.Errorf("neither tasks.html nor board.go spells the marker `reopened after dismissal (<reason_code>)`. " +
			"The SPEC's 'usable alone' names it verbatim: it is what Salvador sees on the board row")
	}
}
