package dashboard

// board-layout-compact (SWT-57, docs/tickets/board-layout-compact_SPEC.md)
// criteria 6, 7 and 8 (the structure halves) and 9-17 (the template). ZERO I/O
// beyond this package's own source and the embedded tasks.html.
//
// REWRITTEN IN PLACE by board-departures (SWT-67,
// docs/tickets/board-departures_SPEC.md) Part 7: the table markup became grid
// rows in two panes, so five tests here keep their assertions and change their
// selectors, and two are amended additively. Nothing is deleted.
//
//	TestTasksTemplate_FirstLineIsTheTopbar        -> …HeaderBandsInOrder (B15)
//	TestTasksTemplate_SectionsRange               -> …PanesAndPanels (criterion 21)
//	TestTasksTemplate_RowCellsAndHeaders          -> …RowCells (criterion 22)
//	TestTasksTemplate_RowVerbsBehindAnActionsPopup-> …RowVerbsOutsideTheRowLink (criterion 22)
//	TestTasksTemplate_ScriptPostponesWhileAPopupIsOpen -> …ScriptContract (criteria 27, 28)
//	TestTasksTemplate_CompactLayoutStyles         -> …DeparturesStyles (criterion 29)
//	TestListTasks_BuildsSectionsAndAdvancedFilters: additive (criteria 11, 12)
//	TestTasksTemplate_NoBranchNoHTMXNoRawHTML:     additive (display.go joins the scan)
//
// MUTATIONS THAT MUST TURN THIS FILE RED (both SPECs' "Mutations"):
//   - the advanced <details> rendered with `open` → AdvancedFilterPopup.
//   - the three advanced inputs moved into a separate popup form → AdvancedFilterPopup, HeaderBandsInOrder.
//   - the summary's {{if .AdvancedFilters}} marker dropped → AdvancedFilterPopup.
//   - details[open] removed from busy(), or the page-not-1 clause → ScriptContract.
//   - the legend put back above the sections → LegendAndNoteAtTheBottom (and the amended LegendAndRingStyles).
//   - the <details class="row-verbs"> moved INSIDE the row <a> → RowVerbsOutsideTheRowLink.
//   - two <script> blocks (paging split out) → ScriptContract.
//   - a Columns field added to boardData → BuildsSectionsAndAdvancedFilters.

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
//
// SWT-67: {{range .Sections}} is now the INNER range — the panes range over
// .Panes and each pane ranges over its .Sections — so strings.Index finds the
// per-section block, which is what every caller here wants. The legend and the
// board note still start after it, because they live in the <footer> below
// <main>.
func sectionsBlock(t *testing.T, s string) (string, int) {
	t.Helper()
	const open = "{{range .Sections}}"
	i := strings.Index(s, open)
	if i < 0 {
		t.Fatalf("tasks.html has no %s (criterion 21: each pane renders its sections)", open)
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
// CARRIED OVER UNCHANGED by SWT-67: the stamp's SQL does not move (the MM/DD
// respelling is a named out-of-scope deviation; the Time column is widened
// instead). state_age_min joins the same statement — see
// TestBoardLightFacts_FirstStatementComputesTheStateAge.

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
// AMENDED — ADDITIVELY — by SWT-67 criteria 11 and 12: listTasks now also feeds
// boardPanes and boardTallies from the SAME .Sections it already built, and
// boardData carries the five new display fields. Every SWT-57 assertion, the
// Columns / statusColumn / byStatus bans included, is unchanged.

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
		// SWT-67 criteria 11, 12.
		{`boardPanes\(\s*data\.Sections\s*\)`, "data.Panes = boardPanes(data.Sections) — the panes are a VIEW of the same sections (B2)"},
		{`boardTallies\(\s*data\.Sections\s*\)`, "data.Tally = boardTallies(data.Sections) — counts of what the board is SHOWING (B16)"},
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
		"Projects", "Filters", "Flash", "OrchAlert", "AutoRefresh", "RefreshSeconds", "RefreshToggleURL", "ReloadURL", "RenderedAt",
		// SWT-67 criterion 11.
		"Panes", "Tally", "ProjectLabel", "RefreshMode", "PageSeconds"} {
		if !fields[f] {
			t.Errorf("boardData has no %s field (criterion 7 / SWT-67 criterion 11)", f)
		}
	}
	row := structFieldNames(t, "board.go", "taskRow")
	for _, f := range []string{"QueueRank", "Updated", "UpdatedAt", "Light",
		// SWT-67 criterion 10.
		"Remark", "Elapsed", "ProjectHue", "HighPriority"} {
		if !row[f] {
			t.Errorf("taskRow has no %s field (criteria 5, 7 / SWT-67 criterion 10)", f)
		}
	}
}

// ---- criterion 8, structure half: boardAdvanced iterates the one key list -------------
// CARRIED OVER UNCHANGED by SWT-67 (criterion 14).

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

// ---- criterion 9 → SWT-67 criterion 19: the three header bands ------------------------
//
// REWRITTEN — not deleted — from TestTasksTemplate_FirstLineIsTheTopbar. SWT-57's
// two-line top (.headbar + .titlebar) becomes B15's three bands:
//
//  1. .headbar — <nav> and .topbar (the filter form + clear advanced). The CHROME
//     bar, byte-unchanged, restyled dark and small. It is not the sign.
//  2. <header class="sign"> — the yellow sign block: the plane, the title with
//     .ProjectLabel, the tallies, the flip clock, FULL, and the auto-refresh
//     toggle (byte-unchanged, with its {{if not .AutoRefresh}} landmine).
//  3. {{if .Flash}} and {{with .OrchAlert}} as full-width blocks, then <main>.
//
// Every SWT-57 assertion survives: the topbar holds ONLY the filter form and the
// clear link; the form's controls outside the <details> are exactly the project
// select and the hidden refresh input; the flash and alert stay full-width
// blocks; the toggle's text uses {{if not .AutoRefresh}}.
func TestTasksTemplate_HeaderBandsInOrder(t *testing.T) {
	s := tasksHTML(t)
	const topOpen = `<div class="topbar">`
	prev := -1
	for _, m := range []string{`<div class="headbar">`, "<nav>", topOpen, `<header class="sign">`,
		"{{if .Flash}}", "{{with .OrchAlert}}", "<main"} {
		i := strings.Index(s, m)
		if i < 0 {
			t.Fatalf("tasks.html has no %s (criterion 19 / B15: headbar(nav, topbar), the sign header, flash, "+
				"orchestrator alert, then <main>)", m)
		}
		if i <= prev {
			t.Errorf("%s is out of document order (B15's three bands, then <main>)", m)
		}
		prev = i
	}
	hb := strings.Index(s, `<div class="headbar">`)
	he, ok := elementEnd(s, hb, "div")
	if !ok {
		t.Fatalf("the .headbar row is never closed")
	}
	if he < strings.Index(s, topOpen) {
		t.Errorf("the .headbar row does not contain the topbar (the filter sits next to the JSON link)")
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
		t.Fatalf("the topbar does not hold the filter form (criterion 19)")
	}
	fe := fi + strings.Index(top[fi:], "</form>")
	ci := strings.Index(top, `id="advanced-clear"`)
	if ci < 0 {
		t.Errorf("the topbar does not hold the clear advanced link (criterion 19)")
	} else if !(fe < ci) {
		t.Errorf("the topbar's order is not: filter form, clear link (criterion 19)")
	}
	for _, m := range []string{`id="auto-refresh-toggle"`, "{{if .AutoRefresh}}", `id="clock"`, `{{.Tally.`} {
		if strings.Contains(top, m) {
			t.Errorf("the topbar holds %s; the chrome bar carries the filters only — the sign header carries the "+
				"title, the tallies, the clock, FULL and the toggle (B15)", m)
		}
	}

	// Band 2: the sign header, which now owns the toggle and the refresh block.
	sg := strings.Index(s, `<header class="sign">`)
	if sg < te {
		t.Fatalf("the sign header opens inside the .headbar row; it is the second band (B15)")
	}
	sge, ok := elementEnd(s, sg, "header")
	if !ok {
		t.Fatalf("the sign <header> is never closed")
	}
	sign := s[sg:sge]
	const toggle = `<a id="auto-refresh-toggle" href="{{.RefreshToggleURL}}">`
	if !strings.Contains(sign, toggle) {
		t.Errorf("the sign header does not hold the byte-unchanged toggle %s (B15)", toggle)
	}
	// THE LANDMINE (SWT-57): the toggle's TEXT uses {{if not .AutoRefresh}}.
	// Writing it {{if .AutoRefresh}} makes two structure tests read the toggle as
	// the refresh block.
	ti := strings.Index(sign, toggle)
	if ti >= 0 && !strings.Contains(sign[ti:minInt(ti+220, len(sign))], "{{if not .AutoRefresh}}") {
		t.Errorf("the toggle's text is not written with {{if not .AutoRefresh}} (the SWT-57 landmine: the file keeps " +
			"exactly ONE {{if .AutoRefresh}}, and it is the indicator's)")
	}
	if strings.Contains(sign, "<h1>Board</h1>") {
		t.Errorf("the sign header still says <h1>Board</h1>; B15's title is <h1>Switchboard <small>{{.ProjectLabel}}</small></h1>")
	}
	if n := strings.Count(s, "<h1"); n != 1 {
		t.Errorf("tasks.html has %d <h1, want exactly 1 (the sign's title)", n)
	}
	if strings.Contains(s, `<div class="titlebar">`) {
		t.Errorf("tasks.html still renders the SWT-57 .titlebar row; its contents moved into the sign header (B15)")
	}

	// Band 3: the flash and the alert are full-width blocks BELOW the sign and
	// ABOVE <main> — a flash is a verb's only receipt, and the orchestrator alert
	// is the one thing that outranks the board.
	for _, m := range []string{"{{if .Flash}}", "{{with .OrchAlert}}"} {
		i := strings.Index(s, m)
		if i < sge {
			t.Errorf("%s renders inside the sign header; it stays a full-width block below it (B15)", m)
		}
	}

	// The filter form's only controls outside the <details> (SWT-57, unchanged).
	form := top[fi:fe]
	outside := form
	if di := strings.Index(form, `<details class="advanced-filter">`); di >= 0 {
		if de, ok := elementEnd(form, di, "details"); ok {
			outside = form[:di] + form[de:]
		}
	} else {
		t.Errorf("the filter form holds no <details class=\"advanced-filter\"> (criterion 19)")
	}
	got := regexp.MustCompile(`<(?:input|select|button|textarea)\b[^>]*>`).FindAllString(outside, -1)
	want := []string{`<select name="project" onchange="this.form.submit()">`,
		`<input type="hidden" name="refresh" value="{{index .Filters "refresh"}}">`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the filter form's controls outside the <details> are\n  %s\nwant exactly the byte-unchanged project select and "+
			"hidden refresh input:\n  %s (criterion 19)", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---- criterion 10: the Advanced filter popup -------------------------------------------
// CARRIED OVER UNCHANGED by SWT-67 (criterion 19: byte-unchanged fragments).

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

// ---- criterion 11 → SWT-67 criteria 27 and 28: the ONE script -------------------------
//
// REWRITTEN — not deleted — from TestTasksTemplate_ScriptPostponesWhileAPopupIsOpen.
// The script is now unconditional (B12: the clock, the paging, the fullscreen
// button and the wake lock must run with auto-refresh OFF), the refresh loop
// arms from data-refresh, and busy() gains B13's fifth clause. Every SWT-52/57
// rule — setTimeout self-arming, location.replace, document.hidden,
// details[open], activeElement, the select's defaultSelected rule, the banned
// tokens, the one-onchange count — is unchanged.
func TestTasksTemplate_ScriptContract(t *testing.T) {
	s := tasksHTML(t)
	if n := strings.Count(s, "<script"); n != 1 {
		t.Fatalf("tasks.html has %d <script, want exactly 1 (criterion 27: paging split into a second block would "+
			"break the count two tests keep)", n)
	}
	i := strings.Index(s, "<script")
	if d := templateDepthAt(s, i); d != 0 {
		t.Errorf("the <script> sits %d template block(s) deep; criterion 27 / B12 renders it OUTSIDE every conditional", d)
	}
	tagEnd := i + strings.Index(s[i:], ">")
	tag := s[i : tagEnd+1]
	for _, attr := range []string{`data-reload="{{.ReloadURL}}"`, `data-interval="{{.RefreshSeconds}}"`,
		`data-page-interval="{{.PageSeconds}}"`, `data-refresh="{{.RefreshMode}}"`} {
		if !strings.Contains(tag, attr) {
			t.Errorf("the <script> tag lacks %s (criterion 27: every knob is a server-rendered data attribute, never "+
				"a URL value). Tag: %s", attr, tag)
		}
	}
	j := strings.Index(s[i:], "</script>")
	if j < 0 {
		t.Fatalf("the <script is never closed")
	}
	script := s[i : i+j]

	// B12: the reload loop arms only when data-refresh === "on".
	if !regexp.MustCompile(`data-refresh`).MatchString(script) || !strings.Contains(script, `"on"`) {
		t.Errorf("the script does not gate its reload loop on data-refresh === \"on\" (B12: a DATA field, not a " +
			"second {{if .AutoRefresh}})")
	}
	for _, want := range []string{"setTimeout", "location.replace(", "document.hidden", "activeElement",
		"defaultValue", "defaultSelected", "details[open]", "requestFullscreen", "wakeLock", "textContent"} {
		if !strings.Contains(script, want) {
			t.Errorf("the script lacks %q (criterion 27)", want)
		}
	}
	if !strings.Contains(script, `addEventListener("visibilitychange"`) && !strings.Contains(script, `addEventListener('visibilitychange'`) {
		t.Errorf("the script does not re-arm on visibilitychange via addEventListener (criterion 27)")
	}
	for _, banned := range []string{"fetch(", "XMLHttpRequest", "htmx", "location.search", "location.href", "innerHTML",
		"localStorage", "sessionStorage", "onchange"} {
		if strings.Contains(script, banned) {
			t.Errorf("the script contains %q. Criterion 27 / B11: paging SHOWS and HIDES server-rendered rows — it "+
				"never builds one — and the script stores nothing (B19)", banned)
		}
	}
	// B12: arm() itself refuses when refresh is off. The plain board now renders
	// the script too, so this guard is the only thing keeping it from reloading.
	if ai := strings.Index(script, "function arm()"); ai < 0 {
		t.Errorf("the script has no function arm()")
	} else {
		body := script[ai:]
		if n := strings.Index(body[len("function arm()"):], "function "); n >= 0 {
			body = body[:len("function arm()")+n]
		}
		if !regexp.MustCompile(`if\s*\(\s*!refreshOn\s*\)\s*return`).MatchString(body) {
			t.Errorf("arm() does not return early when refresh is off (B12). arm():\n%s", body)
		}
	}
	// B18-3: the wake lock is guarded and its rejection swallowed, so an insecure
	// context costs nothing — no console error, no banner.
	if !regexp.MustCompile(`if\s*\(\s*navigator\.wakeLock`).MatchString(script) {
		t.Errorf("the wake lock is not guarded by `if (navigator.wakeLock)` (B18-3: it is undefined on a non-secure " +
			"origin, which is today)")
	}
	if !regexp.MustCompile(`\.catch\(`).MatchString(script) {
		t.Errorf("the wake lock's promise rejection is not swallowed with .catch (B18-3)")
	}

	// Criterion 28: busy() postpones on all FIVE conditions.
	const fn = "function busy()"
	b := strings.Index(script, fn)
	if b < 0 {
		t.Fatalf("the script has no %s", fn)
	}
	busy := script[b:]
	if n := strings.Index(busy[len(fn):], "function "); n >= 0 {
		busy = busy[:len(fn)+n]
	}
	for _, want := range []struct{ frag, why string }{
		{"document.hidden", "a hidden tab (D15)"},
		{"details[open]", "an open <details> — a reload would collapse the popup he just opened (SWT-57 L6)"},
		{"activeElement", "a focused form control (D15)"},
		{"dirty(", "a control holding unsaved input, with the select's defaultSelected rule (D15)"},
	} {
		if !strings.Contains(busy, want.frag) {
			t.Errorf("busy() does not postpone on %q — %s (criterion 28). busy():\n%s", want.frag, want.why, busy)
		}
	}
	if !regexp.MustCompile(`(?i)page`).MatchString(busy) {
		t.Errorf("busy() has no page-not-1 clause. Criterion 28 / B13: with paging at 9 s and reloads at 5 s, a reload "+
			"would reset every panel to page 1 before page 2 was ever drawn — the second page would be unreachable "+
			"exactly when he is watching. busy():\n%s", busy)
	}

	if n := strings.Count(s, "onchange"); n != 1 {
		t.Errorf("tasks.html has %d onchange, want exactly 1 (the project select; criterion 27)", n)
	}
	handlers := regexp.MustCompile(`\son[a-z]+=`).FindAllString(s, -1)
	if len(handlers) != 1 || strings.TrimSpace(handlers[0]) != "onchange=" {
		t.Errorf("inline handlers %v, want only the project select's onchange (criterion 26: the FULL button binds "+
			"with addEventListener)", handlers)
	}
}

// ---- criterion 12 → SWT-67 criterion 21: the panes and the panels ---------------------
//
// REWRITTEN — not deleted — from TestTasksTemplate_SectionsRange. The one table
// per section becomes one <section class="panel …"> per section inside one of
// two pane columns. The <h2> is byte-unchanged (layoutSections() keeps parsing
// it) and the {{else}} moves to the PANE range.
func TestTasksTemplate_PanesAndPanels(t *testing.T) {
	s := tasksHTML(t)
	if strings.Contains(s, ".Columns") {
		t.Errorf("tasks.html ranges over .Columns (criterion 21: the rows render in .Panes → .Sections)")
	}
	const paneOpen = "{{range .Panes}}"
	pi := strings.Index(s, paneOpen)
	if pi < 0 {
		t.Fatalf("tasks.html has no %s (criterion 21: <main> holds the two panes)", paneOpen)
	}
	pane, ok := templateBlockAfter(s, paneOpen)
	if !ok {
		t.Fatalf("the {{range .Panes}} block is never closed")
	}
	if !strings.Contains(pane, `<div class="col col-{{.Key}}">`) {
		t.Errorf("the pane range does not render <div class=\"col col-{{.Key}}\"> (criterion 21)")
	}
	if !regexp.MustCompile(`\{\{else\}\}\s*<p class="muted">No tasks match the filters\.</p>\s*$`).MatchString(pane) {
		t.Errorf("the {{range .Panes}} block's {{else}} does not keep <p class=\"muted\">No tasks match the "+
			"filters.</p> (criterion 21). Block tail: %q", flatWS(pane[maxInt(0, len(pane)-160):]))
	}
	mi := strings.Index(s, "<main")
	if mi < 0 || mi > pi {
		t.Errorf("the pane range is not inside <main> (criterion 21)")
	}

	block, _ := sectionsBlock(t, s)
	const h2 = `<h2 id="section-{{.Key}}">{{.Title}} ({{len .Tasks}})</h2>`
	if !strings.Contains(block, h2) {
		t.Errorf("the {{range .Sections}} block lacks the BYTE-UNCHANGED header %s (criterion 21 / B3: layoutSections() "+
			"keeps parsing it, and the page indicator is a SIBLING because the existing regex requires </h2> right "+
			"after the count)", h2)
	}
	if n := strings.Count(s, "<h2"); n != 1 {
		t.Errorf("tasks.html has %d <h2, want exactly the section header", n)
	}
	if !strings.Contains(block, `<section class="{{.Class}}" style="order:{{.Order}}">`) {
		t.Errorf(`the panel is not <section class="{{.Class}}" style="order:{{.Order}}"> (criterion 21 / B2: both ` +
			`come from the Go table, so the template needs no branch)`)
	}
	for _, want := range []string{`<div class="panelhead">`, `<span class="pg"></span>`, `<div class="cols">`, `<div class="rows">`} {
		if !strings.Contains(block, want) {
			t.Errorf("the panel lacks %s (criterion 21)", want)
		}
	}
	// The page indicator is a SIBLING of the <h2>, never inside it.
	hi := strings.Index(block, h2)
	pg := strings.Index(block, `<span class="pg"></span>`)
	if hi >= 0 && pg >= 0 && pg < hi+len(h2) {
		t.Errorf("the <span class=\"pg\"> sits inside the <h2>; B3 keeps the heading byte-unchanged so lyH2 still parses it")
	}
	rows := strings.Index(block, `<div class="rows">`)
	if rows < 0 || rows < pg {
		t.Errorf("the rows box does not follow the panel head (criterion 21)")
	}
	if !strings.Contains(block, "{{range .Tasks}}") {
		t.Errorf("the {{range .Sections}} block does not range over each section's .Tasks")
	}
	for _, tag := range []string{"<table", "<tr", "<th", "<td"} {
		if strings.Contains(s, tag) {
			t.Errorf("tasks.html still contains %s (criterion 21: the table markup is gone; every row is a grid strip)", tag)
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---- criterion 13 → SWT-67 criterion 22: the row's cells -------------------------------
//
// REWRITTEN — not deleted — from TestTasksTemplate_RowCellsAndHeaders. Eleven
// <td>s become B4's seven cells inside one anchor; the status column goes (B5
// folds the status into the Remarks words) and so do the rarely-filled sub /
// order / parent / assignee cells. The `updated` cell keeps its short stamp and
// its raw title — SWT-57 L8 is untouched.
func TestTasksTemplate_RowCells(t *testing.T) {
	s := tasksHTML(t)
	rows := rowsBlock(t, s)

	if !strings.Contains(rows, `<div class="row s-{{.Light.Class}}">`) {
		t.Errorf(`the row is not wrapped in <div class="row s-{{.Light.Class}}"> (criterion 22 / B4: a <form> or ` +
			`<details> inside an <a> is invalid HTML, so the row is a wrapper holding two siblings)`)
	}
	if !strings.Contains(rows, `<a class="r" href="/tasks/{{.ID}}">`) {
		t.Errorf(`the row has no <a class="r" href="/tasks/{{.ID}}"> (criterion 22: the whole strip is one link)`)
	}
	const updatedCell = `<span class="time" title="{{.UpdatedAt}}">{{.Updated}}</span>`
	if n := strings.Count(s, updatedCell); n != 1 {
		t.Errorf("tasks.html has %d %s, want exactly 1 (criterion 22: SWT-57 L8's stamp, unchanged — the short text, "+
			"the raw updated_at in the title)", n, updatedCell)
	}
	prev := -1
	for _, cell := range []string{`class="mdot`, `class="time"`, `class="id"`, `class="title"`, `class="chip"`,
		`class="gate"`, `class="rem"`} {
		i := strings.Index(rows, cell)
		if i < 0 {
			t.Errorf("the row lacks the %s cell (criterion 22 / B4: mdot, time, id, title, chip, gate, rem)", cell)
			continue
		}
		if i <= prev {
			t.Errorf("%s is out of B4's cell order: mdot, time, id, title, chip, gate, rem (criterion 22)", cell)
		}
		prev = i
	}
	// B5: the status COLUMN goes; the status itself survives as remarkFor's words.
	if strings.Contains(rows, "{{.Status}}") {
		t.Errorf("the row still prints {{.Status}} (criterion 22 / B5: the status is folded into the Remarks words, " +
			"which is how pr_open vs in_progress survives the dropped column)")
	}
	if !strings.Contains(rows, "{{.Remark}}") {
		t.Errorf("the row renders no {{.Remark}} (B5: nothing is lost; one column is)")
	}
	for _, gone := range []string{"{{.Subproject}}", "{{.PlanOrder}}", "{{.ParentID}}", "{{.AssigneeType}}", "{{.WorkerType}}"} {
		if strings.Contains(rows, gone) {
			t.Errorf("the row still renders %s (criterion 22 / B5: the rarely-filled cells are dropped — SWT-57's own "+
				"Future work listed exactly this; the FIELDS stay on taskRow for task.html)", gone)
		}
	}
	// The reopened-after-dismissal note stays, in the title cell (SWT-36 D12).
	if !strings.Contains(rows, "reopened after dismissal") {
		t.Errorf("the row lost the `reopened after dismissal` note (criterion 22, B4 cell 4)")
	}
	if strings.Contains(s, `<td class="muted">{{.UpdatedAt}}</td>`) {
		t.Errorf("the row still prints the raw {{.UpdatedAt}} as cell text (SWT-57 L8)")
	}
}

// ---- criterion 14 → SWT-67 criterion 22's second half: the verbs ------------------------
//
// REWRITTEN — not deleted — from TestTasksTemplate_RowVerbsBehindAnActionsPopup.
// The <details> leaves the last <td> and becomes the row wrapper's LAST CHILD,
// a SIBLING of the anchor: a <form> or <details> inside an <a> is invalid HTML.
// Same popup, same two forms, same order, same neutral summary — now spelled ⋯
// with aria-label="actions" (B4).
func TestTasksTemplate_RowVerbsOutsideTheRowLink(t *testing.T) {
	s := tasksHTML(t)
	rows := rowsBlock(t, s)

	ai := strings.Index(rows, `<a class="r" href="/tasks/{{.ID}}">`)
	if ai < 0 {
		t.Fatalf(`the row has no <a class="r" href="/tasks/{{.ID}}"> (criterion 22)`)
	}
	ae, ok := elementEnd(rows, ai, "a")
	if !ok {
		t.Fatalf("the row anchor is never closed")
	}
	anchor := rows[ai:ae]
	for _, banned := range []string{"<form", "<details", "<button", "<input"} {
		if strings.Contains(anchor, banned) {
			t.Errorf("the row anchor contains %s. Criterion 22 / B4: nothing interactive may sit inside an <a> — it "+
				"is invalid HTML and it steals the row's touch target. Anchor:\n%s", banned, flatWS(anchor))
		}
	}
	di := strings.Index(rows, `<details class="row-verbs">`)
	if di < 0 {
		t.Fatalf(`the row has no <details class="row-verbs"> (criterion 22)`)
	}
	if di < ae {
		t.Errorf("the <details class=\"row-verbs\"> opens at %d, before the row link closes at %d; it is the anchor's "+
			"SIBLING, positioned over the anchor's reserved last column (criterion 22 / B4)", di, ae)
	}
	if n := strings.Count(s, `class="row-verbs"`); n != 1 {
		t.Errorf("tasks.html has %d class=\"row-verbs\", want exactly 1 (in the per-task range)", n)
	}
	de, ok := elementEnd(rows, di, "details")
	if !ok {
		t.Fatalf("the row-verbs <details> is never closed")
	}
	det := rows[di:de]
	// The <details> is the wrapper's LAST child: only the wrapper's </div> follows.
	if !regexp.MustCompile(`^\s*</div>\s*$`).MatchString(rows[de:]) {
		t.Errorf("the row-verbs <details> is not the row wrapper's last child (criterion 22). After it:\n%q", rows[de:])
	}
	m := regexp.MustCompile(`<summary([^>]*)>([^<]*)</summary>`).FindStringSubmatch(det)
	if m == nil {
		t.Fatalf("the row-verbs <details> has no <summary> (criterion 22)")
	}
	if strings.TrimSpace(m[2]) != "\u22ef" {
		t.Errorf("the row-verbs summary reads %q, want the ⋯ (U+22EF) affordance of B4", m[2])
	}
	if !strings.Contains(m[1], `aria-label="actions"`) {
		t.Errorf("the row-verbs summary has no aria-label=\"actions\"; the glyph alone names nothing (B4). Tag: <summary%s>", m[1])
	}
	if strings.Contains(m[0], "{{") {
		t.Errorf("the row-verbs summary carries a template action; it is a neutral, row-independent affordance")
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
			"(criterion 25: the verbs move inside the DOM and NOTHING else about them moves). Popup:\n%s", popup)
	}
	if !regexp.MustCompile(`</div>\s*</details>$`).MatchString(det) {
		t.Errorf("the row-verbs <details> does not close as </div></details> (criterion 22)")
	}
}

// ---- criterion 15: the legend and the note at the bottom ---------------------------------
// CARRIED OVER by SWT-67 with ONE documented change of anchor: sectionsBlock now
// finds the INNER {{range .Sections}} (the panes range over .Panes and each pane
// over its .Sections). The legend and the note still start after it, because
// they live in the ticker <footer> below <main>. The legend's golden text is
// byte-unchanged.

func TestTasksTemplate_LegendAndNoteAtTheBottom(t *testing.T) {
	s := tasksHTML(t)
	_, end := sectionsBlock(t, s)
	legend := regexp.MustCompile(`(?s)<p class="muted" id="light-legend">.*?</p>`).FindStringIndex(s)
	note := regexp.MustCompile(`<p class="muted">Queues are filters on the one tasks table\.`).FindStringIndex(s)
	if legend == nil || note == nil {
		t.Fatalf("tasks.html lost the legend <p id=\"light-legend\"> or the board note")
	}
	if legend[0] < end {
		t.Errorf("the legend starts before the end of {{range .Sections}} (criterion 15: it stays at the bottom)")
	}
	if note[0] < end {
		t.Errorf("the board note starts before the end of {{range .Sections}} (criterion 15: it stays at the bottom)")
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
		t.Errorf("the legend's text changed (criterion 15: moved and restyled, never edited):\n got %s\nwant %s", got, golden)
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

// ---- criterion 16 → SWT-67 criterion 29: the CSS ---------------------------------------
//
// REWRITTEN — not deleted — from TestTasksTemplate_CompactLayoutStyles. The
// palette is the ticket, so the colour goldens go; what KEEPS its assertions is
// the shape contract: the popups, the advanced marker, the session tag's
// CSS-only ellipsis, the six lights with the two rings — plus the departures
// rules the design needs (the two breakpoints, the uppercase remarks, the
// scrollable rows that make the board degrade without JS).
func TestTasksTemplate_DeparturesStyles(t *testing.T) {
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
			t.Errorf("the <style> block has no %s rule (criterion 29)", name)
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
		t.Errorf("no %s rule has all of %v (criterion 29): %v", name, wants, bodies)
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
	// KEPT from SWT-57: the popups and the advanced marker still behave.
	check("details.advanced-filter", suffix("advanced-filter"), `position:\s*relative`)
	check("details.row-verbs", suffix("row-verbs"), `position:\s*absolute`, `right:\s*0\b`)
	check(".popup", is(".popup", "div.popup"), `position:\s*absolute`, `z-index`, `border`)
	check(".advanced-active", suffix(".advanced-active"), `bold`, `border`)
	check(".popup form", suffix(".popup form"), `white-space:\s*nowrap`)
	// KEPT from SWT-56: the session tag truncates in CSS only.
	check(".session-tag", suffix(".session-tag"), `max-width`, `text-overflow:\s*ellipsis`)
	// KEPT from SWT-52: six lights, and stale/none are RINGS — shape as well as
	// colour separates them.
	for _, c := range []string{"done", "working", "stale", "input", "next", "none"} {
		m := regexp.MustCompile(`\.light-` + c + `\s*\{([^}]*)\}`).FindStringSubmatch(style)
		if m == nil {
			t.Errorf("the <style> block has no .light-%s rule (criterion 29: the six classes survive the repaint)", c)
			continue
		}
		if c == "stale" || c == "none" {
			if !strings.Contains(m[1], "border") || !strings.Contains(m[1], "transparent") {
				t.Errorf(".light-%s {%s} is not a RING (a border with a transparent fill) (criterion 29)", c, m[1])
			}
		}
	}
	// NEW with the departures skin.
	check(".rem", suffix(".rem"), `text-transform:\s*uppercase`)
	check(".rows", suffix(".rows"), `overflow-y:\s*auto`)
	check(".chip", suffix(".chip"), `hsl\(var\(--chip-h`)
	for _, q := range []string{`@media\s*\(\s*max-width:\s*760px\s*\)`, `@media\s*\(\s*min-width:\s*761px\s*\)`} {
		if !regexp.MustCompile(q).MatchString(style) {
			t.Errorf("the <style> block has no /%s/ breakpoint (criterion 29: the phone's machine-status view and the "+
				"tablet's hidden .mdot)", q)
		}
	}
	mi := regexp.MustCompile(`@media\s*\(\s*min-width:\s*761px\s*\)\s*\{([^{]*\{[^}]*\}\s*)*`).FindString(style)
	if mi != "" && !strings.Contains(mi, ".mdot") {
		t.Errorf("the (min-width: 761px) block does not hide .mdot (criterion 29: the dot is the phone's light)")
	}
	if !regexp.MustCompile(`\.row\b`).MatchString(style) {
		t.Errorf("the <style> block has no .row rule (criterion 29: the row wrapper positions the verbs affordance)")
	}
	if regexp.MustCompile(`form\.filters\s*\{\s*margin:\s*\.8rem 0 1rem;?\s*\}`).MatchString(style) {
		t.Errorf("the old `form.filters { margin: .8rem 0 1rem; }` rule is still there (replaced)")
	}
}

// ---- criterion 17: no branch, no HTMX, no raw HTML -------------------------------------------
// AMENDED — ADDITIVELY — by SWT-67: display.go joins the scanned file list, so
// the new display helpers cannot reach the page through template.HTML either
// (B9: a custom property whose value is digits is the safest thing to put
// through html/template's CSS context).

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
	for _, f := range []string{"sections.go", "board.go", "lights.go", "display.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Errorf("read %s: %v (criterion 17 scans the whole path to the page)", f, err)
			continue
		}
		for _, banned := range []string{"template.HTML(", "template.HTMLAttr(", "template.HTML ", "template.CSS"} {
			if strings.Contains(string(b), banned) {
				t.Errorf("%s uses %s: every dynamic value reaches the page through html/template's escaping (criterion 17)", f, banned)
			}
		}
	}
}
