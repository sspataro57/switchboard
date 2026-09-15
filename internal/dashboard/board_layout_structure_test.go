package dashboard

// board-layout-compact (SWT-57, docs/tickets/board-layout-compact_SPEC.md)
// criteria 6, 7 and 8 (the structure halves) and 9-17 (the template). ZERO I/O
// beyond this package's own source and the embedded tasks.html.
//
// This file references NO symbol the SPEC adds: it reads source text and the
// template only, so each test here fails on its own assertion. (The package's
// test binary still compile-FAILS until sections_test.go and
// board_advanced_test.go find their symbols.)
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - the advanced <details> rendered with `open` → AdvancedFilterPopup.
//   - the three advanced inputs moved into a separate popup form → AdvancedFilterPopup, FirstLineIsTheTopbar.
//   - the summary's {{if .AdvancedFilters}} marker dropped → AdvancedFilterPopup.
//   - details[open] removed from busy() → ScriptPostponesWhileAPopupIsOpen.
//   - the legend put back above the sections → LegendAndNoteAtTheBottom (and the amended LegendAndRingStyles).

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

// elementEnd returns the index just past the close tag of the element whose
// opening tag starts at s[start:], counting nested tags of the same name.
func elementEnd(s string, start int, tag string) (int, bool) {
	open, closeTag := "<"+tag, "</"+tag+">"
	if start < 0 || !strings.HasPrefix(s[start:], open) {
		return 0, false
	}
	depth, i := 0, start
	for i < len(s) {
		o := strings.Index(s[i:], open)
		c := strings.Index(s[i:], closeTag)
		if c < 0 {
			return 0, false
		}
		if o >= 0 && o < c {
			depth++
			i += o + len(open)
			continue
		}
		depth--
		i += c + len(closeTag)
		if depth == 0 {
			return i, true
		}
	}
	return 0, false
}

// allBlocksAfter is templateBlockAfter for every occurrence of open.
func allBlocksAfter(s, open string) []string {
	var out []string
	for i := 0; ; {
		j := strings.Index(s[i:], open)
		if j < 0 {
			return out
		}
		if b, ok := templateBlockAfter(s[i+j:], open); ok {
			out = append(out, b)
		}
		i += j + len(open)
	}
}

var wsRE = regexp.MustCompile(`\s+`)

func flatWS(s string) string { return wsRE.ReplaceAllString(strings.TrimSpace(s), " ") }

// sectionsBlock is the {{range .Sections}} … {{end}} block and the index just
// past its closing {{end}}.
func sectionsBlock(t *testing.T, s string) (string, int) {
	t.Helper()
	const open = "{{range .Sections}}"
	i := strings.Index(s, open)
	if i < 0 {
		t.Fatalf("tasks.html has no %s (criterion 12: the rows render in the light-derived sections)", open)
	}
	b, ok := templateBlockAfter(s, open)
	if !ok {
		t.Fatalf("tasks.html's %s is never closed", open)
	}
	return b, i + len(open) + len(b) + len("{{end}}")
}

func rowsBlock(t *testing.T, s string) string {
	t.Helper()
	sec, _ := sectionsBlock(t, s)
	b, ok := templateBlockAfter(sec, "{{range .Tasks}}")
	if !ok {
		t.Fatalf("the {{range .Sections}} block has no {{range .Tasks}} … {{end}}")
	}
	return b
}

func structFieldNames(t *testing.T, file, typ string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	out := map[string]bool{}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != typ {
			return true
		}
		found = true
		if st, ok := ts.Type.(*ast.StructType); ok {
			for _, fl := range st.Fields.List {
				for _, nm := range fl.Names {
					out[nm.Name] = true
				}
			}
		}
		return false
	})
	if !found {
		return nil
	}
	return out
}

// ---- criterion 6: boardLightFacts --------------------------------------------------

func TestBoardLightFacts_FirstStatementFormatsTheUpdatedStamp(t *testing.T) {
	body := funcBodySrc(t, "board.go", "boardLightFacts")
	if body == "" {
		t.Fatalf("board.go declares no boardLightFacts")
	}
	const q = "s.pool.Query"
	if n := strings.Count(body, q); n > 2 {
		t.Errorf("boardLightFacts issues %d statements, want at most two (criterion 6, D15's count)", n)
	}
	first := strings.Index(body, q+"(")
	if first < 0 {
		t.Fatalf("boardLightFacts runs no s.pool.Query")
	}
	stmt, rest := body[first:], ""
	if second := strings.Index(stmt[len(q):], q); second >= 0 {
		stmt, rest = stmt[:len(q)+second], stmt[len(q)+second:]
	}
	if !strings.Contains(stmt, "HH24:MI:SS") {
		t.Fatalf("CONTROL: the first statement does not carry the render time; the scan is not reading statement 1")
	}
	for _, want := range []struct{ re, why string }{
		{`to_char\(t\.updated_at AT TIME ZONE \$2,\s*'HH24:MI'\)`, "HH:MM for an update since local midnight (L8)"},
		{`to_char\(t\.updated_at AT TIME ZONE \$2,\s*'YYYY-MM-DD'\)`, "the date for an earlier update (L8)"},
		{"t\\.updated_at\\s*>=\\s*`\\s*\\+\\s*boardDayStart\\(\"\\$2\"\\)", "'today' through the ONE spelling of local midnight, boardDayStart(\"$2\") (L8, D5)"},
	} {
		if !regexp.MustCompile(want.re).MatchString(stmt) {
			t.Errorf("boardLightFacts' FIRST statement does not match /%s/ — %s (criterion 6)", want.re, want.why)
		}
	}
	if !strings.Contains(body, "UpdatedStamp") {
		t.Errorf("boardLightFacts never sets lightFacts.UpdatedStamp from the updated column (criterion 6, L8)")
	}
	if rest == "" {
		t.Fatalf("boardLightFacts has no second statement (the queue-head candidates)")
	}
	if !strings.Contains(rest, "QueueRank") {
		t.Errorf("the candidate loop (after the second statement) never sets QueueRank (criterion 6: the 1-based " +
			"candidate position, from Postgres' TaskQueueOrder)")
	}
	if !regexp.MustCompile(`statusOf\[[^\]]+\]\s*[!=]=\s*"ready"`).MatchString(rest) {
		t.Errorf("the candidate loop has no statusOf[id] == \"ready\" guard (criterion 6: the same guard eligible uses)")
	}
}

// ---- criterion 7: listTasks -----------------------------------------------------------

func TestListTasks_BuildsSectionsAndAdvancedFilters(t *testing.T) {
	body := funcBodySrc(t, "board.go", "listTasks")
	if body == "" {
		t.Fatalf("board.go declares no listTasks")
	}
	for _, want := range []struct{ re, why string }{
		{`boardSections\(`, "data.Sections = boardSections(rows)"},
		{`boardAdvanced\(r\.URL\.Query\(\)\)`, "data.AdvancedFilters, data.ClearAdvancedURL = boardAdvanced(r.URL.Query())"},
		{`\.Sections\b`, "the sections reach the template as boardData.Sections"},
		{`AdvancedFilters`, "boardData.AdvancedFilters"},
		{`ClearAdvancedURL`, "boardData.ClearAdvancedURL"},
		{`QueueRank`, "each taskRow carries its QueueRank"},
		{`UpdatedStamp`, "Updated comes from facts[id].UpdatedStamp"},
	} {
		if !regexp.MustCompile(want.re).MatchString(body) {
			t.Errorf("listTasks does not match /%s/ — %s (criterion 7)", want.re, want.why)
		}
	}
	for _, banned := range []string{"statusColumn", "Columns", "byStatus"} {
		if strings.Contains(body, banned) {
			t.Errorf("listTasks still mentions %s: the status columns are replaced by the sections (criterion 7)", banned)
		}
	}
	src, err := os.ReadFile("board.go")
	if err != nil {
		t.Fatalf("read board.go: %v", err)
	}
	if regexp.MustCompile(`type statusColumn\b`).Match(src) {
		t.Errorf("board.go still declares statusColumn (criterion 7: removed)")
	}
	fields := structFieldNames(t, "board.go", "boardData")
	if fields == nil {
		t.Fatalf("board.go declares no boardData")
	}
	if fields["Columns"] {
		t.Errorf("boardData still has Columns (criterion 7: Columns → Sections)")
	}
	for _, f := range []string{"Sections", "AdvancedFilters", "ClearAdvancedURL",
		"Projects", "Filters", "Flash", "OrchAlert", "AutoRefresh", "RefreshSeconds", "RefreshToggleURL", "ReloadURL", "RenderedAt"} {
		if !fields[f] {
			t.Errorf("boardData has no %s field (criterion 7)", f)
		}
	}
	row := structFieldNames(t, "board.go", "taskRow")
	for _, f := range []string{"QueueRank", "Updated", "UpdatedAt", "Light"} {
		if !row[f] {
			t.Errorf("taskRow has no %s field (criteria 5, 7)", f)
		}
	}
}

// ---- criterion 8, structure half: boardAdvanced iterates the one key list -------------

func TestBoardAdvanced_IteratesBoardKeys(t *testing.T) {
	body := funcBodySrc(t, "board.go", "boardAdvanced")
	if body == "" {
		t.Fatalf("board.go declares no boardAdvanced (criterion 8: the helper lives in board.go)")
	}
	if !strings.Contains(body, "boardKeys") {
		t.Errorf("boardAdvanced does not iterate boardKeys (criterion 8: a key added later becomes advanced automatically)")
	}
	for _, banned := range []string{`"status"`, `"assignee_type"`, `"subproject"`} {
		if strings.Contains(body, banned) {
			t.Errorf("boardAdvanced names %s itself; the advanced keys are boardKeys minus project and refresh (criterion 8)", banned)
		}
	}
	if !strings.Contains(body, "boardURL(") {
		t.Errorf("boardAdvanced does not build the clear URL through boardURL (criterion 8: url.Values, re-encoded)")
	}
	if strings.Contains(body, "RawQuery") {
		t.Errorf("boardAdvanced mentions RawQuery (criterion 8)")
	}
}

// ---- criterion 9: the first line ------------------------------------------------------

func TestTasksTemplate_FirstLineIsTheTopbar(t *testing.T) {
	s := tasksHTML(t)
	const topOpen = `<div class="topbar">`
	prev := -1
	// AMENDED deliberately (owner, 2026-09-15: "the filter I want it next to JSON
	// link"): the topbar now shares the nav's line, inside one .headbar row, so it
	// follows <nav> directly and the <h1>, flash and orchestrator alert come after.
	for _, m := range []string{"<nav>", topOpen, "<h1>", "{{if .Flash}}", "{{with .OrchAlert}}"} {
		i := strings.Index(s, m)
		if i < 0 {
			t.Fatalf("tasks.html has no %s (criterion 9: nav, the topbar, then <h1>, flash, orchestrator alert)", m)
		}
		if i <= prev {
			t.Errorf("%s is out of document order (criterion 9: nav, topbar, <h1>, flash, orchestrator alert)", m)
		}
		prev = i
	}
	hb := strings.Index(s, `<div class="headbar">`)
	if hb < 0 || hb > strings.Index(s, "<nav>") {
		t.Errorf(`the nav and the topbar are not wrapped in one <div class="headbar"> row opening before <nav> (the filter sits next to the JSON link)`)
	} else if he, ok := elementEnd(s, hb, "div"); !ok || he < strings.Index(s, topOpen) {
		t.Errorf("the .headbar row does not contain the topbar")
	}
	if n := strings.Count(s, topOpen); n != 1 {
		t.Fatalf("tasks.html has %d %s, want exactly 1", n, topOpen)
	}
	ts := strings.Index(s, topOpen)
	te, ok := elementEnd(s, ts, "div")
	if !ok {
		t.Fatalf("the topbar <div> is never closed")
	}
	top := s[ts:te]

	fi := strings.Index(top, `<form class="filters"`)
	if fi < 0 {
		t.Fatalf("the topbar does not hold the filter form (criterion 9)")
	}
	fe := fi + strings.Index(top[fi:], "</form>")
	// AMENDED deliberately (owner, 2026-09-15: "the filter I want it next to JSON
	// link"): the topbar on the nav's line holds ONLY the filter form and the
	// clear link, so it fits beside JSON on the tablet; the auto-refresh toggle
	// and block moved to the .titlebar row beside <h1>Board</h1>.
	ci := strings.Index(top, `id="advanced-clear"`)
	if ci < 0 {
		t.Errorf("the topbar does not hold the clear advanced link (criterion 9)")
	} else if !(fe < ci) {
		t.Errorf("the topbar's order is not: filter form, clear link (criterion 9)")
	}
	for _, m := range []string{`id="auto-refresh-toggle"`, "{{if .AutoRefresh}}"} {
		if strings.Contains(top, m) {
			t.Errorf("the topbar holds %s; it moved to the .titlebar row so the filter fits next to the JSON link", m)
		}
	}
	const titleOpen = `<div class="titlebar">`
	tb := strings.Index(s, titleOpen)
	if tb < 0 || tb < te {
		t.Fatalf("tasks.html has no %s after the topbar", titleOpen)
	}
	tbe, ok := elementEnd(s, tb, "div")
	if !ok {
		t.Fatalf("the titlebar <div> is never closed")
	}
	title := s[tb:tbe]
	hi := strings.Index(title, "<h1>Board</h1>")
	gi := strings.Index(title, `<a id="auto-refresh-toggle" href="{{.RefreshToggleURL}}">`)
	ai := strings.Index(title, "{{if .AutoRefresh}}")
	if hi < 0 || gi < 0 || ai < 0 || !(hi < gi && gi < ai) {
		t.Errorf("the titlebar's order is not: <h1>Board</h1>, the byte-unchanged toggle, the {{if .AutoRefresh}} block")
	}
	if ai >= 0 {
		blk, ok := templateBlockAfter(title[ai:], "{{if .AutoRefresh}}")
		if !ok {
			t.Errorf("the {{if .AutoRefresh}} block does not close inside the titlebar (criterion 9)")
		} else if !strings.Contains(blk, `<p id="auto-refresh" class="muted">auto-refresh on (every {{.RefreshSeconds}} s, last refreshed {{.RenderedAt}})</p>`) ||
			!strings.Contains(blk, "<script") {
			t.Errorf("the titlebar's {{if .AutoRefresh}} block lacks the byte-unchanged indicator or the script (criterion 9)")
		}
	}

	// The filter form's only controls outside the <details>.
	form := top[fi:fe]
	outside := form
	if di := strings.Index(form, `<details class="advanced-filter">`); di >= 0 {
		if de, ok := elementEnd(form, di, "details"); ok {
			outside = form[:di] + form[de:]
		}
	} else {
		t.Errorf("the filter form holds no <details class=\"advanced-filter\"> (criterion 9)")
	}
	got := regexp.MustCompile(`<(?:input|select|button|textarea)\b[^>]*>`).FindAllString(outside, -1)
	want := []string{`<select name="project" onchange="this.form.submit()">`,
		`<input type="hidden" name="refresh" value="{{index .Filters "refresh"}}">`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the filter form's controls outside the <details> are\n  %s\nwant exactly the byte-unchanged project select and "+
			"hidden refresh input:\n  %s (criterion 9)", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// ---- criterion 10: the Advanced filter popup -------------------------------------------

func TestTasksTemplate_AdvancedFilterPopup(t *testing.T) {
	s := tasksHTML(t)
	const detOpen = `<details class="advanced-filter">`
	if n := strings.Count(s, detOpen); n != 1 {
		t.Fatalf("tasks.html has %d %s, want exactly 1 (criterion 10)", n, detOpen)
	}
	fi := strings.Index(s, `<form class="filters"`)
	if fi < 0 {
		t.Fatalf("tasks.html lost its filter form")
	}
	fe := fi + strings.Index(s[fi:], "</form>")
	di := strings.Index(s, detOpen)
	de, ok := elementEnd(s, di, "details")
	if !ok {
		t.Fatalf("the advanced <details> is never closed")
	}
	if !(fi < di && de <= fe) {
		t.Errorf("the advanced <details> does not lie inside <form class=\"filters\"> (criterion 10: a closed <details>' " +
			"controls are still submitted, so the project select's auto-submit keeps the advanced values)")
	}
	det := s[di:de]
	for _, k := range []string{"status", "assignee_type", "subproject"} {
		re := regexp.MustCompile(`<input type="text" name="` + k + `" placeholder="` + k + `" value="\{\{index \$?\.Filters "` + k + `"\}\}">`)
		if !re.MatchString(det) {
			t.Errorf("the advanced <details> lacks the unchanged %s text input (criterion 10)", k)
		}
		if n := len(re.FindAllString(s, -1)); n != 1 {
			t.Errorf("tasks.html has %d %s text inputs, want exactly 1 (inside the popup)", n, k)
		}
	}
	if !strings.Contains(det, "<button>Filter</button>") || strings.Count(s, "<button>Filter</button>") != 1 {
		t.Errorf("<button>Filter</button> is not (only) inside the advanced <details> (criterion 10)")
	}
	for _, tag := range regexp.MustCompile(`<details\b[^>]*>`).FindAllString(s, -1) {
		if regexp.MustCompile(`\bopen\b`).MatchString(tag) {
			t.Errorf("tasks.html renders %s: no <details> is ever rendered open (criterion 10, L5/L6)", tag)
		}
	}
	if strings.Contains(strings.ToLower(s), "<dialog") {
		t.Errorf("tasks.html uses <dialog>; the popup is a <details> (criterion 10)")
	}

	// The summary: "Advanced filter", plus the marker and the key=value list
	// inside {{if .AdvancedFilters}}.
	si := strings.Index(det, "<summary")
	se := strings.Index(det, "</summary>")
	if si < 0 || se < si {
		t.Fatalf("the advanced <details> has no <summary> (criterion 10)")
	}
	sum := det[si : se+len("</summary>")]
	if !strings.Contains(sum, "Advanced filter") {
		t.Errorf("the summary does not read `Advanced filter` (criterion 10): %s", sum)
	}
	blocks := allBlocksAfter(sum, "{{if .AdvancedFilters}}")
	marked, listed := false, false
	rangeRE := regexp.MustCompile(`\{\{range [^}]*\.AdvancedFilters\}\}`)
	for _, b := range blocks {
		if strings.Contains(b, `class="advanced-active"`) {
			marked = true
		}
		if rangeRE.MatchString(b) && strings.Contains(b, ".Key") && strings.Contains(b, ".Value") && strings.Contains(b, "=") {
			listed = true
		}
	}
	if !marked {
		t.Errorf("the summary carries no class=\"advanced-active\" inside {{if .AdvancedFilters}} (criterion 10: an active "+
			"advanced filter is VISIBLY marked): %s", sum)
	}
	if !listed {
		t.Errorf("the summary renders no key=value list from {{range .AdvancedFilters}} inside {{if .AdvancedFilters}} "+
			"(criterion 10): %s", sum)
	}
	if n := strings.Count(s, `class="advanced-active"`); n != 1 {
		t.Errorf("tasks.html has %d class=\"advanced-active\", want exactly the summary's", n)
	}

	const clear = `<a id="advanced-clear" href="{{.ClearAdvancedURL}}">clear advanced</a>`
	if n := strings.Count(s, clear); n != 1 {
		t.Fatalf("tasks.html has %d %s, want exactly 1 (criterion 10)", n, clear)
	}
	if ci := strings.Index(s, clear); ci >= di && ci < de {
		t.Errorf("the clear advanced link sits inside the popup; it sits beside it so no tap is needed to undo (criterion 10)")
	}
	inIf := false
	for _, b := range allBlocksAfter(s, "{{if .AdvancedFilters}}") {
		if strings.Contains(b, clear) {
			inIf = true
		}
	}
	if !inIf {
		t.Errorf("the clear advanced link is not inside {{if .AdvancedFilters}} (criterion 10)")
	}
}

// ---- criterion 11: the script postpones while a popup is open ---------------------------

func TestTasksTemplate_ScriptPostponesWhileAPopupIsOpen(t *testing.T) {
	s := tasksHTML(t)
	if n := strings.Count(s, "<script"); n != 1 {
		t.Fatalf("tasks.html has %d <script, want exactly 1 (criterion 11)", n)
	}
	i := strings.Index(s, "<script")
	j := strings.Index(s[i:], "</script>")
	if j < 0 {
		t.Fatalf("the <script is never closed")
	}
	script := s[i : i+j]
	const fn = "function busy()"
	b := strings.Index(script, fn)
	if b < 0 {
		t.Fatalf("the refresh script has no %s", fn)
	}
	busy := script[b:]
	if n := strings.Index(busy[len(fn):], "function "); n >= 0 {
		busy = busy[:len(fn)+n]
	}
	if !strings.Contains(busy, `document.querySelector("details[open]")`) && !strings.Contains(busy, `document.querySelector('details[open]')`) {
		t.Errorf("busy() does not postpone on document.querySelector(\"details[open]\") (criterion 11, L6): a reload would "+
			"collapse the popup he just opened. busy():\n%s", busy)
	}
	if n := strings.Count(s, "onchange"); n != 1 {
		t.Errorf("tasks.html has %d onchange, want exactly 1 (the project select; criterion 11)", n)
	}
	handlers := regexp.MustCompile(`\son[a-z]+=`).FindAllString(s, -1)
	if len(handlers) != 1 || strings.TrimSpace(handlers[0]) != "onchange=" {
		t.Errorf("inline handlers %v, want only the project select's onchange (criterion 11)", handlers)
	}
}

// ---- criterion 12: the sections ------------------------------------------------------

func TestTasksTemplate_SectionsRange(t *testing.T) {
	s := tasksHTML(t)
	if strings.Contains(s, ".Columns") {
		t.Errorf("tasks.html still ranges over .Columns (criterion 12: the rows render in .Sections)")
	}
	block, _ := sectionsBlock(t, s)
	const h2 = `<h2 id="section-{{.Key}}">{{.Title}} ({{len .Tasks}})</h2>`
	if !strings.Contains(block, h2) {
		t.Errorf("the {{range .Sections}} block lacks the header %s (criterion 12)", h2)
	}
	if n := strings.Count(s, "<h2"); n != 1 {
		t.Errorf("tasks.html has %d <h2, want exactly the section header", n)
	}
	if n := strings.Count(block, "<table"); n != 1 || strings.Count(s, "<table") != 1 {
		t.Errorf("the {{range .Sections}} block has %d <table, want one table per section (criterion 12)", n)
	}
	if !strings.Contains(block, "{{range .Tasks}}") {
		t.Errorf("the {{range .Sections}} block does not range over each section's .Tasks")
	}
	if !regexp.MustCompile(`\{\{else\}\}\s*<p class="muted">No tasks match the filters\.</p>\s*$`).MatchString(block) {
		t.Errorf("the {{range .Sections}} block's {{else}} does not keep <p class=\"muted\">No tasks match the filters.</p> (criterion 12)")
	}
	if regexp.MustCompile(`<h2[^>]*>[^<]*\{\{\.Status\}\}`).MatchString(s) || regexp.MustCompile(`<th[^>]*>[^<]*\{\{\.Status\}\}`).MatchString(s) {
		t.Errorf("a header still renders {{.Status}} (criterion 12)")
	}
}

// ---- criterion 13: the row --------------------------------------------------------------

func TestTasksTemplate_RowCellsAndHeaders(t *testing.T) {
	s := tasksHTML(t)
	block, _ := sectionsBlock(t, s)
	hdr := regexp.MustCompile(`<tr>\s*((?:<th>[^<]*</th>\s*)+)</tr>`).FindStringSubmatch(block)
	if hdr == nil {
		t.Fatalf("the section table has no header row")
	}
	var ths []string
	for _, m := range regexp.MustCompile(`<th>([^<]*)</th>`).FindAllStringSubmatch(hdr[1], -1) {
		ths = append(ths, m[1])
	}
	if got, want := strings.Join(ths, ","), "id,title,status,project,sub,assignee,prio,order,parent,updated,"; got != want {
		t.Errorf("header cells = %q, want %q (criterion 13: status after title; an empty last header)", got, want)
	}

	rows := rowsBlock(t, s)
	const statusCell = `<td class="muted">{{.Status}}</td>`
	const updatedCell = `<td class="muted" title="{{.UpdatedAt}}">{{.Updated}}</td>`
	for _, c := range []string{statusCell, updatedCell} {
		if n := strings.Count(rows, c); n != 1 {
			t.Errorf("the row has %d %s, want exactly 1 (criterion 13)", n, c)
		}
	}
	if strings.Contains(s, `<td class="muted">{{.UpdatedAt}}</td>`) {
		t.Errorf("the row still prints the raw {{.UpdatedAt}} as cell text (criterion 13, L8: the short stamp, raw in title)")
	}
	if n := strings.Count(rows, "<td"); n != 11 {
		t.Errorf("the row has %d cells, want 11 (criterion 13)", n)
	}
	prev := -1
	for _, m := range []string{lightSpan, `{{.Title}}</a>`, statusCell, `<td>{{.Project}}</td>`, `<td>{{.Subproject}}</td>`,
		`<td>{{.AssigneeType}}`, `<td>{{.Priority}}</td>`, `<td>{{.PlanOrder}}</td>`, `{{if .ParentID}}`, updatedCell,
		`<details class="row-verbs">`} {
		i := strings.Index(rows, m)
		if i < 0 {
			t.Errorf("the row lacks %s (criterion 13)", m)
			continue
		}
		if i <= prev {
			t.Errorf("%s is out of the row's cell order: id, title, status, project, sub, assignee, prio, order, parent, "+
				"updated, actions (criterion 13)", m)
		}
		prev = i
	}
}

// ---- criterion 14: the verbs behind a per-row actions popup -----------------------------

func TestTasksTemplate_RowVerbsBehindAnActionsPopup(t *testing.T) {
	s := tasksHTML(t)
	rows := rowsBlock(t, s)
	if !regexp.MustCompile(`<td>\s*<details class="row-verbs">\s*<summary>actions</summary>\s*<div class="popup">`).MatchString(rows) {
		t.Fatalf(`the row has no <td><details class="row-verbs"><summary>actions</summary><div class="popup"> (criterion 14)`)
	}
	if n := strings.Count(s, `class="row-verbs"`); n != 1 {
		t.Errorf("tasks.html has %d class=\"row-verbs\", want exactly 1 (in the per-task range)", n)
	}
	di := strings.Index(rows, `<details class="row-verbs">`)
	de, ok := elementEnd(rows, di, "details")
	if !ok {
		t.Fatalf("the row-verbs <details> is never closed")
	}
	det := rows[di:de]
	if m := regexp.MustCompile(`<summary>([^<]*)</summary>`).FindStringSubmatch(det); m == nil || m[1] != "actions" || strings.Contains(m[0], "{{") {
		t.Errorf("the row-verbs summary is not a neutral `actions` with no template action (criterion 14)")
	}
	pi := strings.Index(det, `<div class="popup">`)
	pe, ok := elementEnd(det, pi, "div")
	if !ok {
		t.Fatalf("the row-verbs popup <div> is never closed")
	}
	popup := det[pi:pe]
	dismiss := strings.Index(popup, `action="/tasks/{{.ID}}/dismiss"`)
	cond := strings.Index(popup, `{{if eq .AssigneeType "human"}}`)
	done := strings.Index(popup, `action="/tasks/{{.ID}}/close"`)
	if dismiss < 0 || cond < 0 || done < 0 || !(dismiss < cond && cond < done) {
		t.Errorf("the popup does not hold the Dismiss form, then {{if eq .AssigneeType \"human\"}} and the Done form "+
			"(criterion 14). Popup:\n%s", popup)
	}
	if !regexp.MustCompile(`</div>\s*</details>$`).MatchString(det) {
		t.Errorf("the row-verbs <details> does not close as </div></details> (criterion 14)")
	}
	if !regexp.MustCompile(`^\s*</td>\s*</tr>\s*$`).MatchString(rows[de:]) {
		t.Errorf("the actions popup is not the row's last cell (criterion 14). After it:\n%s", rows[de:])
	}
}

// ---- criterion 15: the legend and the note at the bottom ---------------------------------

func TestTasksTemplate_LegendAndNoteAtTheBottom(t *testing.T) {
	s := tasksHTML(t)
	_, end := sectionsBlock(t, s)
	legend := regexp.MustCompile(`(?s)<p class="muted" id="light-legend">.*?</p>`).FindStringIndex(s)
	note := regexp.MustCompile(`<p class="muted">Queues are filters on the one tasks table\.`).FindStringIndex(s)
	if legend == nil || note == nil {
		t.Fatalf("tasks.html lost the legend <p id=\"light-legend\"> or the board note")
	}
	if legend[0] < end {
		t.Errorf("the legend starts before the end of {{range .Sections}} (criterion 15: it moves to the bottom)")
	}
	if note[0] < end {
		t.Errorf("the board note starts before the end of {{range .Sections}} (criterion 15: it moves to the bottom)")
	}
	dot := string(rune(0x00B7))
	golden := `<p class="muted" id="light-legend">Lights: ` +
		`<span class="legend-light light-done"></span>green: done ` + dot + ` ` +
		`<span class="legend-light light-working"></span>yellow: in progress ` + dot + ` ` +
		`<span class="legend-light light-stale"></span>yellow ring: in progress with no recent signal ` + dot + ` ` +
		`<span class="legend-light light-input"></span>red: waiting on your input ` + dot + ` ` +
		`<span class="legend-light light-next"></span>blue: next in queue ` + dot + ` ` +
		`<span class="legend-light light-none"></span>grey ring: not queued (holding, blocked, queued behind another, or dismissed). ` +
		`Hover a light for its reason. ` +
		`A red or yellow light shows its Claude session's name at the start of the title: reply in that session.</p>`
	if got := flatWS(s[legend[0]:legend[1]]); got != golden {
		t.Errorf("the legend's text changed (criterion 15: moved, unedited):\n got %s\nwant %s", got, golden)
	}
	ts := strings.Index(s, `<div class="topbar">`)
	ri := strings.Index(s, "{{range .Sections}}")
	if ts < 0 || ri < ts {
		t.Fatalf("tasks.html has no topbar before {{range .Sections}}")
	}
	for _, banned := range []string{"Lights:", "Queues are filters"} {
		if strings.Contains(s[ts:ri], banned) {
			t.Errorf("between the topbar and the sections the template still says %q (criterion 15)", banned)
		}
	}
}

// ---- criterion 16: the CSS -----------------------------------------------------------------

func TestTasksTemplate_CompactLayoutStyles(t *testing.T) {
	s := tasksHTML(t)
	lower := strings.ToLower(s)
	st, se := strings.Index(lower, "<style>"), strings.Index(lower, "</style>")
	if st < 0 || se < st {
		t.Fatalf("tasks.html has no <style> block")
	}
	style := s[st+len("<style>") : se]
	type rule struct {
		sels []string
		body string
	}
	var rules []rule
	for _, m := range regexp.MustCompile(`([^{}]+)\{([^}]*)\}`).FindAllStringSubmatch(style, -1) {
		var sels []string
		for _, sel := range strings.Split(m[1], ",") {
			sels = append(sels, flatWS(sel))
		}
		rules = append(rules, rule{sels, m[2]})
	}
	bodiesFor := func(match func(sel string) bool) []string {
		var out []string
		for _, r := range rules {
			for _, sel := range r.sels {
				if match(sel) {
					out = append(out, r.body)
					break
				}
			}
		}
		return out
	}
	check := func(name string, match func(string) bool, wants ...string) {
		t.Helper()
		bodies := bodiesFor(match)
		if len(bodies) == 0 {
			t.Errorf("the <style> block has no %s rule (criterion 16)", name)
			return
		}
		for _, b := range bodies {
			ok := true
			for _, w := range wants {
				if !regexp.MustCompile(w).MatchString(b) {
					ok = false
				}
			}
			if ok {
				return
			}
		}
		t.Errorf("no %s rule has all of %v (criterion 16): %v", name, wants, bodies)
	}
	is := func(names ...string) func(string) bool {
		return func(sel string) bool {
			for _, n := range names {
				if sel == n {
					return true
				}
			}
			return false
		}
	}
	suffix := func(sfx string) func(string) bool {
		return func(sel string) bool { return strings.HasSuffix(sel, sfx) }
	}
	check(".topbar", is(".topbar", "div.topbar"), `display:\s*flex`, `wrap`)
	check(".topbar p", suffix(".topbar p"), `margin:\s*0\s*(;|$)`)
	check("details.advanced-filter", suffix("advanced-filter"), `position:\s*relative`, `display:\s*inline-block`)
	check("details.row-verbs", suffix("row-verbs"), `position:\s*relative`, `display:\s*inline-block`)
	check(".popup", is(".popup", "div.popup"), `position:\s*absolute`, `z-index`, `background:\s*#fff\b`, `border`)
	check(".row-verbs .popup", suffix(".row-verbs .popup"), `right:\s*0\b`)
	check(".advanced-active", suffix(".advanced-active"), `bold`, `border`)
	check(".popup form", suffix(".popup form"), `margin:\s*\.2rem 0\b`, `white-space:\s*nowrap`)
	if regexp.MustCompile(`form\.filters\s*\{\s*margin:\s*\.8rem 0 1rem;?\s*\}`).MatchString(style) {
		t.Errorf("the old `form.filters { margin: .8rem 0 1rem; }` rule is still there (criterion 16: replaced)")
	}
	for _, line := range []string{
		`.light-done { background: #2e9d48; border: 2px solid #2e9d48; }`,
		`.light-working { background: #e8b90f; border: 2px solid #e8b90f; }`,
		`.light-stale { background: transparent; border: 2px solid #e8b90f; }`,
		`.light-input { background: #d63a2f; border: 2px solid #d63a2f; }`,
		`.light-next { background: #2f6fd6; border: 2px solid #2f6fd6; }`,
		`.light-none { background: transparent; border: 2px solid #9a9a9a; }`,
		`.session-tag { display: inline-block; max-width: 18rem; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; vertical-align: bottom; font-family: monospace; font-size: .8rem; padding: 0 .35rem; margin-right: .2rem; border: 1px solid #9a9a9a; border-radius: .6rem; }`,
		`.session-input { border-color: #d63a2f; color: #d63a2f; font-weight: bold; }`,
		`.session-stale { border-style: dashed; border-color: #e8b90f; }`,
		`.session-working { border-color: #e8b90f; }`,
	} {
		if !strings.Contains(style, line) {
			t.Errorf("the <style> block lost the unchanged rule %s (criterion 16)", line)
		}
	}
}

// ---- criterion 17: no branch, no HTMX, no raw HTML -------------------------------------------

func TestTasksTemplate_NoBranchNoHTMXNoRawHTML(t *testing.T) {
	s := tasksHTML(t)
	lower := strings.ToLower(s)
	for _, banned := range []string{"eq .Status", "eq .Light"} {
		if strings.Contains(s, banned) {
			t.Errorf("tasks.html contains %q (criterion 17)", banned)
		}
	}
	for _, banned := range []string{"hx-", "htmx"} {
		if strings.Contains(lower, banned) {
			t.Errorf("tasks.html contains %q (criterion 17)", banned)
		}
	}
	for _, f := range []string{"sections.go", "board.go", "lights.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Errorf("read %s: %v (criterion 17 scans the whole path to the page)", f, err)
			continue
		}
		for _, banned := range []string{"template.HTML(", "template.HTMLAttr(", "template.HTML "} {
			if strings.Contains(string(b), banned) {
				t.Errorf("%s uses %s: every dynamic value reaches the page through html/template's escaping (criterion 17)", f, banned)
			}
		}
	}
}
