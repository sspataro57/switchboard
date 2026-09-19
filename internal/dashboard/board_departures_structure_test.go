package dashboard

// board-departures (SWT-67, docs/tickets/board-departures_SPEC.md) criteria
// 8-14 (the read and the handler), 17-20 and the row-content halves of 22
// (B8's priority mark, B9's chip, B6's elapsed cell), the header/ticker
// furniture of B15, and Part 5's static assets (criteria 30-32).
// ZERO I/O beyond this package's own source, the embedded templates and the
// embedded static FS.
//
// Criteria owned by the REWRITTEN tests of Part 7, not restated here:
//   21, 22 (cell order), 27, 28, 29 -> board_layout_structure_test.go
//   23, 24                          -> board_lights_structure_test.go
//   25, 26                          -> carried over unchanged (VerbFormsByteUnchanged,
//                                      DoneFormIsHumanOnlyAndStatusBlind, NoIncoming,
//                                      NoBranchNoHTMXNoRawHTML)
//
// IMPOSED SURFACE (the SPEC names all of it except the two marked *, which are
// this file's spelling of markup the SPEC describes but does not quote):
//
//	// board.go
//	const boardPageInterval = 9 * time.Second
//	type taskRow struct{ …; Remark, Elapsed string; ProjectHue int; HighPriority bool }
//	type boardData struct{ …; Panes []boardPane; Tally boardTally;
//	                       ProjectLabel, RefreshMode string; PageSeconds int }
//	// lights.go
//	type lightFacts struct{ …; StateAgeMinutes int }
//	// display.go — forced by criterion 26 (see the note in the sign-header test)
//	type boardTallyItem struct{ Class, Label string; Count int }
//	func (boardTally) Items() []boardTallyItem
//	// server.go
//	//go:embed static
//	var staticFS embed.FS
//	// templates/tasks.html
//	<span class="el">{{.Elapsed}}</span>            * the elapsed cell (B6, mock .el)
//	<span class="gate">{{if .Light.Session}}…</span> * the Gate cell wrapper (B4 cell 6)
//
// GREENFIELD NOTE — EXPECTED RED: display.go, staticFS, boardPageInterval and
// the new taskRow/boardData/lightFacts fields do not exist, so package
// dashboard's test binary compile-FAILS. Once it compiles, every test here
// fails until the SPEC is implemented.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - lightFor reads StateAgeMinutes -> StateAgeIsDisplayOnly.
//   - state_age_min's expression replaced by a literal 0 -> FirstStatementComputesTheStateAge
//     (the SHAPE only; the VALUE is criterion 34's column-fed integration assert).
//   - a Columns field added to boardData -> BoardDataCarriesPanesAndTally.
//   - the @font-face src put back on fonts.gstatic.com -> NoThirdPartyURL.
//   - /static/ wrapped in s.auth.Require -> StaticRouteIsOpenAndTheRestIsNot.
//   - a woff2 replaced by a placeholder -> StaticAssets.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ---- shared helper: how deep in {{if}}/{{range}}/{{with}} an offset sits -------

// templateDepthAt counts open template blocks (if/range/with/block/define minus
// end) before idx. 0 means "outside every conditional".
func templateDepthAt(s string, idx int) int {
	depth, pos := 0, 0
	for pos < idx {
		j := strings.Index(s[pos:], "{{")
		if j < 0 || pos+j >= idx {
			return depth
		}
		j += pos
		k := strings.Index(s[j:], "}}")
		if k < 0 {
			return depth
		}
		action := strings.TrimSpace(strings.Trim(strings.TrimSpace(s[j+2:j+k]), "-"))
		word := ""
		if f := strings.Fields(action); len(f) > 0 {
			word = f[0]
		}
		switch word {
		case "if", "range", "with", "block", "define":
			depth++
		case "end":
			depth--
		}
		pos = j + k + 2
	}
	return depth
}

// ---- criterion 8: StateAgeMinutes is display-only -------------------------------

func TestLightFacts_StateAgeIsDisplayOnly(t *testing.T) {
	body := funcBodySrc(t, "lights.go", "lightFor")
	if body == "" {
		t.Fatalf("lights.go declares no lightFor")
	}
	if strings.Contains(body, "StateAgeMinutes") {
		t.Errorf("lightFor's body mentions StateAgeMinutes. Criterion 8: the elapsed minutes are DISPLAY — the light " +
			"is decided by State/Stale as before (the QueueRank / UpdatedStamp / FromMessage precedent)")
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
	var comments []string
	for _, cg := range f.Comments {
		if cg.Pos() >= st.Pos() && cg.End() <= st.End() {
			comments = append(comments, cg.Text())
		}
	}
	found, isInt, documented := false, false, false
	for _, fl := range st.Fields.List {
		for _, nm := range fl.Names {
			if nm.Name != "StateAgeMinutes" {
				continue
			}
			found = true
			if id, ok := fl.Type.(*ast.Ident); ok && id.Name == "int" {
				isInt = true
			}
		}
	}
	for _, c := range comments {
		if strings.Contains(c, "StateAgeMinutes") && strings.Contains(strings.ToLower(c), "display") {
			documented = true
		}
	}
	switch {
	case !found:
		t.Errorf("lightFacts has no StateAgeMinutes field (criterion 8 / B6)")
	case !isInt:
		t.Errorf("lightFacts.StateAgeMinutes is not an int (criterion 8: whole minutes, computed in SQL)")
	case !documented:
		t.Errorf("lightFacts.StateAgeMinutes is not documented as display-only beside QueueRank/UpdatedStamp (criterion 8)")
	}
}

// ---- criterion 9: one new expression in the FIRST statement ----------------------

func TestBoardLightFacts_FirstStatementComputesTheStateAge(t *testing.T) {
	body, first, second := lightFactsStatements(t)
	if n := strings.Count(body, "s.pool.Query"); n > 2 {
		t.Errorf("boardLightFacts issues %d statements, want at most two (criterion 9: D15's cost table is unchanged — "+
			"no new query, no new statement)", n)
	}
	if second == "" {
		t.Fatalf("boardLightFacts has no second statement (the queue-head candidates)")
	}
	if strings.Contains(second, "state_age_min") {
		t.Errorf("the SECOND statement mentions state_age_min; it is byte-unchanged (criterion 9)")
	}
	// LastIndex: the outer select list names f.state_age_min first; the aliased
	// expression is the inner, later one.
	i := strings.LastIndex(first, "state_age_min")
	if i < 0 {
		t.Fatalf("boardLightFacts' FIRST statement has no state_age_min expression. Criterion 9 / B6: " +
			"COALESCE(GREATEST(0, FLOOR(EXTRACT(EPOCH FROM now() - t.working_state_at) / 60))::int, 0) AS state_age_min")
	}
	start := i - 220
	if start < 0 {
		start = 0
	}
	expr := regexp.MustCompile(`\s+`).ReplaceAllString(first[start:i], " ")
	for _, want := range []struct{ frag, why string }{
		{"AS ", "the expression is ALIASED state_age_min, so the scan reads a named column"},
		{"COALESCE(", "a NULL working_state_at (no signal) must scan as 0, never crash the render (B6)"},
		{"GREATEST(0", "a clock skew must not render a negative elapsed time (B6)"},
		{"now() - t.working_state_at", "the age is the DB clock minus the signal instant — not Date.now(), not time.Now() (B6, B14)"},
	} {
		if !strings.Contains(expr, want.frag) {
			t.Errorf("the state_age_min expression %q does not contain %q — %s (criterion 9)", expr, want.frag, want.why)
		}
	}
	if !strings.Contains(body, "StateAgeMinutes") {
		t.Errorf("boardLightFacts never sets lightFacts.StateAgeMinutes from the state_age_min column (criterion 9)")
	}
	for _, banned := range []string{"time.Now", "time.Since"} {
		if strings.Contains(body, banned) {
			t.Errorf("boardLightFacts uses %s (criterion 9 / B14: the only clock on this page's data is Postgres')", banned)
		}
	}
}

// ---- criteria 10-13: taskRow, boardData, listTasks, the paging const -------------

func TestTaskRow_CarriesTheDisplayFields(t *testing.T) {
	row := structFieldNames(t, "board.go", "taskRow")
	if row == nil {
		t.Fatalf("board.go declares no taskRow")
	}
	for _, f := range []string{"Remark", "Elapsed", "ProjectHue", "HighPriority"} {
		if !row[f] {
			t.Errorf("taskRow has no %s field (criterion 10: board-only, never a TaskExportRow column)", f)
		}
	}
	// The SWT-52/57/59 fields the sections and lights still need.
	for _, f := range []string{"Light", "QueueRank", "Updated", "UpdatedAt", "Incoming", "ReopenedAfterDismissal",
		"Priority", "Project", "AssigneeType", "Status", "Title", "ID"} {
		if !row[f] {
			t.Errorf("taskRow lost its %s field (criterion 10: the row's existing fields are untouched)", f)
		}
	}
	// B5: sub/order/parent/assignee are dropped from the ROW MARKUP, not from the
	// struct — task.html reads them through taskDetail.
	for _, f := range []string{"Subproject", "PlanOrder", "ParentID", "WorkerType"} {
		if !row[f] {
			t.Errorf("taskRow lost %s: B5 drops the CELL, not the field — task.html reads it through taskDetail", f)
		}
	}
}

func TestBoardData_CarriesPanesAndTally(t *testing.T) {
	fields := structFieldNames(t, "board.go", "boardData")
	if fields == nil {
		t.Fatalf("board.go declares no boardData")
	}
	for _, f := range []string{"Panes", "Tally", "ProjectLabel", "RefreshMode", "PageSeconds"} {
		if !fields[f] {
			t.Errorf("boardData has no %s field (criterion 11)", f)
		}
	}
	// Sections STAYS: it is boardPanes' input and four test files read it.
	for _, f := range []string{"Sections", "AdvancedFilters", "ClearAdvancedURL", "Projects", "Filters", "Flash",
		"OrchAlert", "AutoRefresh", "RefreshSeconds", "RefreshToggleURL", "ReloadURL", "RenderedAt"} {
		if !fields[f] {
			t.Errorf("boardData lost its %s field (criterion 11: every SWT-52/57 field survives)", f)
		}
	}
	if fields["Columns"] {
		t.Errorf("boardData has a Columns field (criterion 11: the SWT-57 ban still stands)")
	}
}

func TestListTasks_FeedsTheDisplayHelpers(t *testing.T) {
	body := funcBodySrc(t, "board.go", "listTasks")
	if body == "" {
		t.Fatalf("board.go declares no listTasks")
	}
	for _, want := range []struct{ re, why string }{
		{`remarkFor\(`, "Remark: remarkFor(tr.Light, t.Status) (criterion 12)"},
		{`elapsedFor\(`, "Elapsed: elapsedFor(tr.Light.Class, f.StateAgeMinutes)"},
		{`StateAgeMinutes`, "…fed by the SQL column, never by a Go clock (B6)"},
		{`projectHue\(`, "ProjectHue: projectHue(t.Project)"},
		{`boardPriorityMark`, "HighPriority: t.Priority >= boardPriorityMark — the const, not a literal 2 (B8)"},
		{`boardPanes\(`, "data.Panes = boardPanes(data.Sections)"},
		{`boardTallies\(`, "data.Tally = boardTallies(data.Sections)"},
		{`projectLabel\(`, "data.ProjectLabel = projectLabel(r.URL.Query().Get(\"project\"))"},
		{`boardPageInterval`, "data.PageSeconds = int(boardPageInterval / time.Second)"},
		{`PageSeconds`, "…reaching the script as data-page-interval"},
		{`RefreshMode`, `RefreshMode = "on" exactly when AutoRefresh (B12)`},
		{`boardSections\(`, "Sections still feeds the panes (criterion 11)"},
	} {
		if !regexp.MustCompile(want.re).MatchString(body) {
			t.Errorf("listTasks does not match /%s/ — %s", want.re, want.why)
		}
	}
	// B12: RefreshMode is a DATA field derived from the SAME flag, never a second
	// query read — a second `Get("refresh")` is how the two drift.
	if n := strings.Count(body, `Get("refresh")`); n != 1 {
		t.Errorf("listTasks reads the refresh key %d times, want exactly 1 (B12: AutoRefresh and RefreshMode come from "+
			"the one read)", n)
	}
	for _, banned := range []string{"statusColumn", "Columns", "byStatus"} {
		if strings.Contains(body, banned) {
			t.Errorf("listTasks mentions %s (criterion 11: the SWT-57 ban)", banned)
		}
	}
	// Carried from SWT-52 criterion 34, restated because PageSeconds is a second
	// interval that must NOT come from the request.
	for _, banned := range []string{"strconv", "ParseDuration", "Atoi", "ParseFloat"} {
		if strings.Contains(body, banned) {
			t.Errorf("listTasks uses %s: neither interval is ever read from the URL (B11, D15)", banned)
		}
	}
}

func TestBoardPageInterval_IsNineSecondsAndNeverAURLValue(t *testing.T) {
	if boardPageInterval != 9*time.Second {
		t.Errorf("boardPageInterval = %v, want 9s (criterion 13 / B11: a const in board.go — a caller-set 0.05 would "+
			"be an animation loop on a shared box)", boardPageInterval)
	}
	src := readSrc(t, "board.go")
	if !regexp.MustCompile(`boardPageInterval\s*=\s*9\s*\*\s*time\.Second`).MatchString(src) {
		t.Errorf("board.go does not declare `boardPageInterval = 9 * time.Second` (criterion 13)")
	}
	for _, fn := range []string{"boardQuery", "boardRefreshURLs", "boardAdvanced", "boardBack"} {
		if strings.Contains(funcBodySrc(t, "board.go", fn), "PageInterval") {
			t.Errorf("%s mentions boardPageInterval; paging is never a URL key (criterion 13, B19)", fn)
		}
	}
}

// Criterion 14, GUARD (passes today): the untouched helpers stay untouched. The
// proxy is that none of them learns a display field — that is how "byte-unchanged"
// fails in practice, by a new field leaking into an old function.
func TestBoardDepartures_UntouchedHelpersStayUntouched(t *testing.T) {
	newNames := []string{"Remark", "Elapsed", "ProjectHue", "HighPriority", "Panes", "Tally", "PageSeconds",
		"ProjectLabel", "RefreshMode", "StateAgeMinutes", "boardPanes", "boardTallies", "remarkFor", "elapsedFor"}
	for _, fn := range []string{"boardQuery", "boardBack", "boardRefreshURLs", "boardAdvanced", "boardURL",
		"boardDayStart", "reopenMarkers", "dismissTaskAction", "closeTaskAction"} {
		body := funcBodySrc(t, "board.go", fn)
		if body == "" {
			t.Errorf("board.go declares no %s (criterion 14: it is byte-unchanged, not deleted)", fn)
			continue
		}
		for _, n := range newNames {
			if strings.Contains(body, n) {
				t.Errorf("%s mentions %s; criterion 14 keeps it byte-unchanged — the restyle is a DISPLAY change and "+
					"touches no filter, no redirect and no verb", fn, n)
			}
		}
	}
	if BoardTimeZone != "America/New_York" {
		t.Errorf("BoardTimeZone = %q, want America/New_York (criterion 14)", BoardTimeZone)
	}
	if strings.Join(boardKeys, ",") != "project,status,assignee_type,subproject,refresh" {
		t.Errorf("boardKeys = %v (criterion 14: unchanged — no paging key, no fullscreen key, B19)", boardKeys)
	}
}

// ---- criterion 17: head metas and the manifest link -------------------------------

func TestTasksTemplate_HeadMetasAndManifest(t *testing.T) {
	s := tasksHTML(t)
	head := s
	if i := strings.Index(strings.ToLower(s), "</head>"); i > 0 {
		head = s[:i]
	} else {
		t.Fatalf("tasks.html has no </head>")
	}
	for _, want := range []string{
		`<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">`,
		`<meta name="theme-color" content="#0b0b0c">`,
		`<meta name="mobile-web-app-capable" content="yes">`,
		`<meta name="apple-mobile-web-app-capable" content="yes">`,
		`<link rel="manifest" href="/static/manifest.webmanifest">`,
	} {
		if n := strings.Count(head, want); n != 1 {
			t.Errorf("tasks.html's <head> carries %s %d time(s), want exactly 1 (criterion 17 / B18: the manifest and "+
				"the metas ship NOW, so the cert flip is the only remaining step)", want, n)
		}
	}
	// B18-4: no service worker (secure-context only, and an offline cache nobody
	// asked for).
	for _, banned := range []string{"serviceWorker", "service-worker", "sw.js"} {
		if strings.Contains(s, banned) {
			t.Errorf("tasks.html mentions %s (B18-4: no service worker)", banned)
		}
	}
}

// ---- criterion 18: no third-party URL anywhere -------------------------------------

func TestTasksTemplate_NoThirdPartyURL(t *testing.T) {
	s := tasksHTML(t)
	lower := strings.ToLower(s)
	for _, banned := range []string{"fonts.googleapis.com", "fonts.gstatic.com", "preconnect", "cdn.", "unpkg", "jsdelivr"} {
		if strings.Contains(lower, banned) {
			t.Errorf("tasks.html mentions %q. Criterion 18 / B17: the fonts are vendored under /static/; the page makes "+
				"NO third-party request (and invariant 4: the board sends nothing anywhere)", banned)
		}
	}
	for _, scheme := range []string{"http://", "https://"} {
		if strings.Contains(lower, scheme) {
			t.Errorf("tasks.html contains %q: every URL on the page is an in-app absolute path (criterion 18)", scheme)
		}
	}
	for _, m := range regexp.MustCompile(`(?:src|href)="([^"]*)"`).FindAllStringSubmatch(s, -1) {
		v := m[1]
		switch {
		case strings.HasPrefix(v, "//"):
			t.Errorf("tasks.html has %s: a protocol-relative URL is an off-host fetch (criterion 18)", m[0])
		case strings.HasPrefix(v, "/"), strings.HasPrefix(v, "#"), strings.HasPrefix(v, "{{"):
		default:
			t.Errorf("tasks.html has %s: every src/href is an in-app absolute path (criterion 18)", m[0])
		}
	}
	st, se := strings.Index(lower, "<style>"), strings.Index(lower, "</style>")
	if st < 0 || se < st {
		t.Fatalf("tasks.html has no <style> block")
	}
	style := s[st:se]
	urls := regexp.MustCompile(`url\(\s*['"]?([^'")]*)`).FindAllStringSubmatch(style, -1)
	if len(urls) == 0 {
		t.Errorf("the <style> block has no url( at all: B17 puts five @font-face src: url(/static/fonts/…) rules there")
	}
	for _, m := range urls {
		if !strings.HasPrefix(m[1], "/static/") {
			t.Errorf("the <style> block loads %q; every url( starts /static/ (criterion 18)", m[1])
		}
	}
	if n := strings.Count(style, "@font-face"); n != 5 {
		t.Errorf("the <style> block has %d @font-face rules, want 5 (B17: B612 Mono 400/700 and Barlow Condensed "+
			"500/600/700, self-hosted)", n)
	}
	if !strings.Contains(style, "font-display: swap") {
		t.Errorf("the @font-face rules lack font-display: swap (B17: a failed font costs the look, not the board)")
	}
	for _, fallback := range []string{"DejaVu Sans Mono", "Arial Narrow"} {
		if !strings.Contains(style, fallback) {
			t.Errorf("the <style> block names no %q fallback family (B17)", fallback)
		}
	}
}

// ---- criterion 19: the byte-unchanged furniture, exactly once each ------------------
// (The BAND ORDER of B15 is TestTasksTemplate_HeaderBandsInOrder, the rewritten
// FirstLineIsTheTopbar in board_layout_structure_test.go.)

func TestTasksTemplate_ByteUnchangedFragmentsSurviveTheRestyle(t *testing.T) {
	s := tasksHTML(t)
	for _, frag := range []string{
		`<select name="project" onchange="this.form.submit()">`,
		`<input type="hidden" name="refresh" value="{{index .Filters "refresh"}}">`,
		`<details class="advanced-filter">`,
		`<button>Filter</button>`,
		`<a id="advanced-clear" href="{{.ClearAdvancedURL}}">clear advanced</a>`,
		`<a id="auto-refresh-toggle" href="{{.RefreshToggleURL}}">`,
		`{{if not .AutoRefresh}}`,
		`<p id="auto-refresh" class="muted">auto-refresh on (every {{.RefreshSeconds}} s, last refreshed {{.RenderedAt}})</p>`,
		`{{if .Flash}}`,
		`{{with .OrchAlert}}`,
		`<p class="muted" id="light-legend">`,
		`<form class="filters" method="get" action="/tasks">`,
	} {
		if n := strings.Count(s, frag); n != 1 {
			t.Errorf("tasks.html carries %s %d time(s), want exactly 1 (criterion 19: the chrome bar and the "+
				"indicator are BYTE-UNCHANGED, restyled by CSS only)", frag, n)
		}
	}
	for _, k := range []string{"status", "assignee_type", "subproject"} {
		frag := `<input type="text" name="` + k + `" placeholder="` + k + `" value="{{index $.Filters "` + k + `"}}">`
		if n := strings.Count(s, frag); n != 1 {
			t.Errorf("tasks.html carries the advanced %s input %d time(s), want exactly 1 (criterion 19)", k, n)
		}
	}
	// B15: the flash and the orchestrator alert stay ABOVE <main> — a flash is a
	// verb's only receipt and the alert outranks the board.
	main := strings.Index(s, "<main")
	if main < 0 {
		t.Fatalf("tasks.html has no <main> (criterion 21)")
	}
	for _, m := range []string{"{{if .Flash}}", "{{with .OrchAlert}}"} {
		if i := strings.Index(s, m); i > main {
			t.Errorf("%s renders inside or after <main> (B15: both are full-width blocks above it)", m)
		}
	}
}

// ---- criterion 20: exactly one {{if .AutoRefresh}}, and it wraps the indicator ------

func TestTasksTemplate_OneAutoRefreshConditionalWrappingTheIndicatorOnly(t *testing.T) {
	s := tasksHTML(t)
	const cond = "{{if .AutoRefresh}}"
	if n := strings.Count(s, cond); n != 1 {
		t.Fatalf("tasks.html has %d %s, want exactly 1 (criterion 20 / B12: the SWT-57 landmine — two structure tests "+
			"take the FIRST one as the refresh block, and the toggle's text must stay {{if not .AutoRefresh}})", n, cond)
	}
	block, ok := templateBlockAfter(s, cond)
	if !ok {
		t.Fatalf("the {{if .AutoRefresh}} block is never closed")
	}
	if !strings.Contains(block, `<p id="auto-refresh" class="muted">`) {
		t.Errorf("the {{if .AutoRefresh}} block does not hold the indicator (criterion 20)")
	}
	if strings.Contains(block, "<script") {
		t.Errorf("the {{if .AutoRefresh}} block still holds the <script>. Criterion 20 / B12: the clock, the paging, " +
			"the fullscreen button and the wake lock must run with auto-refresh OFF; the loop arms from data-refresh")
	}
	if strings.Contains(block, `id="auto-refresh-toggle"`) {
		t.Errorf("the toggle sits inside the block; it renders on EVERY board page (the SWT-57 landmine)")
	}
	if flat := regexp.MustCompile(`\s+`).ReplaceAllString(block, " "); strings.Count(flat, "<p") != 1 {
		t.Errorf("the {{if .AutoRefresh}} block wraps more than the indicator (criterion 20): %s", flat)
	}
}

// ---- B15's header band: the sign, the tallies, the clock, FULL ----------------------
// The NUMBERS are criterion 38's integration assert; this pins the markup that
// carries them.

func TestTasksTemplate_SignHeaderTalliesClockAndFullButton(t *testing.T) {
	s := tasksHTML(t)
	hi := strings.Index(s, `<header class="sign">`)
	if hi < 0 {
		t.Fatalf(`tasks.html has no <header class="sign"> (B15 band 2: the yellow sign block)`)
	}
	he, ok := elementEnd(s, hi, "header")
	if !ok {
		t.Fatalf("the sign <header> is never closed")
	}
	head := s[hi:he]
	const h1 = `<h1>Switchboard <small>{{.ProjectLabel}}</small></h1>`
	if !strings.Contains(head, h1) {
		t.Errorf("the sign header does not carry %s (B15: .ProjectLabel is a Go field so the header needs no branch)", h1)
	}
	// The five tallies come from GO, through one range.
	//
	// SPEC CONTRADICTION, resolved here (and reported with the SPEC): criterion 26
	// keeps TestTasksTemplate_NoIncoming green, and that test fails on BOTH
	// `.Incoming` and the lower-cased word `incoming` anywhere in tasks.html. So
	// the header can spell neither {{.Tally.Incoming}} nor the mock's third
	// LABEL. The way out is B3's own principle — the words come from Go — applied
	// to the tally: boardTally.Items() returns the five {Class, Label, Count} in
	// B16's order and the template ranges over them. boardTally keeps criterion
	// 1's six int fields; this is an accessor, not a change of shape.
	if !regexp.MustCompile(`\{\{range \.Tally\.Items\}\}`).MatchString(head) {
		t.Errorf("the sign header does not render the tallies with {{range .Tally.Items}} (B15, B16). It cannot name " +
			"them one by one: {{.Tally.Incoming}} contains `.Incoming` and the word `incoming`, both of which " +
			"TestTasksTemplate_NoIncoming (criterion 26) forbids in this file")
	}
	if !regexp.MustCompile(`<b[^>]*>\{\{\.Count\}\}</b>`).MatchString(head) {
		t.Errorf("each tally's number is not wrapped in its own <b>{{.Count}}</b> (criterion 38 reads the five " +
			"numbers by position)")
	}
	for _, f := range []string{"{{.Label}}", "{{.Class}}"} {
		if !strings.Contains(head, f) {
			t.Errorf("the tally range does not render %s: the label and its colour class are Go's, so the header "+
				"needs no branch and no banned word (B15, B16)", f)
		}
	}
	for _, banned := range []string{"{{.Tally.NeedYou}}", "{{.Tally.InFlight}}", "{{.Tally.Incoming}}",
		"{{.Tally.Queued}}", "{{.Tally.DoneToday}}"} {
		if strings.Contains(head, banned) {
			t.Errorf("the sign header names %s directly; the five tallies render through one range over "+
				"boardTally.Items() (see the note above)", banned)
		}
	}
	if n := strings.Count(s, `id="clock"`); n != 1 {
		t.Errorf("tasks.html has %d id=\"clock\" elements, want exactly 1 (B14: the ONLY device clock on the page)", n)
	}
	ci := strings.Index(head, `id="clock"`)
	if ci < 0 {
		t.Errorf("the flip clock is not in the sign header (B15)")
	}
	// B18-1: a real <button type="button">, OUTSIDE every <form>, so focusing it
	// never makes busy() postpone a reload, and with NO inline handler.
	var btn []int
	for _, m := range regexp.MustCompile(`<button[^>]*>`).FindAllStringIndex(head, -1) {
		tag := head[m[0]:m[1]]
		if strings.Contains(tag, `type="button"`) && strings.Contains(tag, `id="fs"`) {
			btn = m
			break
		}
	}
	if btn == nil {
		t.Fatalf(`the sign header has no <button type="button" … id="fs" …> FULL control (B18-1: a real button, ` +
			`OUTSIDE every form, bound with addEventListener)`)
	}
	if !strings.Contains(head[btn[0]:], "FULL") {
		t.Errorf("the fullscreen button does not read FULL (B18-1)")
	}
	abs := hi + btn[0]
	for _, m := range regexp.MustCompile(`(?s)<form\b.*?</form>`).FindAllStringIndex(s, -1) {
		if abs > m[0] && abs < m[1] {
			t.Errorf("the FULL button sits inside a <form>; B18-1 keeps it outside every form so focusing it never " +
				"makes busy() postpone a reload")
		}
	}
	if regexp.MustCompile(`\son[a-z]+=`).MatchString(head[btn[0]:minInt(btn[0]+200, len(head))]) {
		t.Errorf("the FULL button carries an inline handler; B18-1 registers the click with addEventListener")
	}
}

// ---- B15's footer band: the ticker ----------------------------------------------------

func TestTasksTemplate_TickerFooterCarriesCountsLegendAndNotes(t *testing.T) {
	s := tasksHTML(t)
	fi := strings.Index(s, `<footer class="ticker">`)
	if fi < 0 {
		t.Fatalf(`tasks.html has no <footer class="ticker"> (B15 band 3)`)
	}
	fe, ok := elementEnd(s, fi, "footer")
	if !ok {
		t.Fatalf("the ticker <footer> is never closed")
	}
	foot := s[fi:fe]
	prev := -1
	for _, m := range []string{"{{.Tally.Open}}", `id="light-legend"`, "{{if .AutoRefresh}}", `class="board-notes"`} {
		i := strings.Index(foot, m)
		if i < 0 {
			t.Errorf("the ticker does not carry %s (B15: the counts, the legend, the indicator, then the notes popup)", m)
			continue
		}
		if i <= prev {
			t.Errorf("%s is out of the ticker's order (B15)", m)
		}
		prev = i
	}
	if !strings.Contains(foot, "{{.Tally.DoneToday}}") {
		t.Errorf("the ticker does not carry {{.Tally.DoneToday}} (B15: the overall counts)")
	}
	// The board note moves into a CLOSED <details> popup; the legend stays visible.
	ni := strings.Index(foot, `class="board-notes"`)
	if ni >= 0 {
		ds := strings.LastIndex(foot[:ni], "<details")
		de, ok := elementEnd(foot, ds, "details")
		if ds < 0 || !ok {
			t.Fatalf("the board-notes <details> is malformed")
		}
		if !strings.Contains(foot[ds:de], "Queues are filters on the one tasks table") {
			t.Errorf("the board-notes popup does not carry the byte-unchanged board note (B15)")
		}
		if strings.Contains(foot[ds:de], `id="light-legend"`) {
			t.Errorf("the legend is inside the notes popup; B15 keeps the legend ALWAYS VISIBLE and the prose one tap away")
		}
	}
}

// ---- criterion 22's content half: B8's mark, B9's chip, B6's elapsed cell -------------

func TestTasksTemplate_PriorityMarkChipAndElapsedCell(t *testing.T) {
	s := tasksHTML(t)
	rows := rowsBlock(t, s)

	// B8: real text with an accessible name, not a CSS ::after and not one class
	// per priority value.
	const prio = `{{if .HighPriority}}<span class="prio" role="img" aria-label="priority {{.Priority}}" title="priority {{.Priority}}">▲</span>{{end}}`
	if !strings.Contains(rows, prio) {
		t.Errorf("the row does not render B8's priority mark:\n%s\n(criterion 22: a ::after content is invisible to a "+
			"screen reader and needs one class per priority value)", prio)
	}
	if regexp.MustCompile(`\.p\d\s+\.id::after`).MatchString(s) {
		t.Errorf("the <style> block keeps the mock's `.p2 .id::after { content: \"▲\" }` (B8: the mark is real text)")
	}
	if strings.Contains(rows, "{{if gt .Priority") || strings.Contains(rows, "ge .Priority") {
		t.Errorf("the row compares .Priority in the TEMPLATE; B8 computes HighPriority in Go from boardPriorityMark")
	}

	// B9: the hue is a CSS custom property whose value is digits — the safest
	// thing to put through html/template's CSS context. No template.HTML anywhere.
	const chip = `style="--chip-h:{{.ProjectHue}}"`
	if !strings.Contains(rows, chip) {
		t.Errorf("the chip does not carry %s (criterion 22 / B9)", chip)
	}
	if !regexp.MustCompile(`<span class="chip"[^>]*>\s*\{\{\.Project\}\}`).MatchString(rows) {
		t.Errorf("the chip does not render the FULL {{.Project}} slug (B9: the board ellipsizes in CSS only; Go never " +
			"truncates a name — the SWT-56 precedent)")
	}
	lower := strings.ToLower(s)
	st, se := strings.Index(lower, "<style>"), strings.Index(lower, "</style>")
	style := s[st:se]
	if !regexp.MustCompile(`hsl\(var\(--chip-h`).MatchString(style) {
		t.Errorf("the <style> block does not read the chip hue with hsl(var(--chip-h …)) (criterion 29 / B9)")
	}
	if !regexp.MustCompile(`\.chip\s*\{[^}]*text-overflow:\s*ellipsis`).MatchString(style) &&
		!regexp.MustCompile(`\.chip\s*\{[^}]*max-width`).MatchString(style) {
		t.Errorf("the .chip rule has neither a max-width nor text-overflow: ellipsis (B9: truncation is CSS-only)")
	}

	// B6: the elapsed cell, rendered from the Go string, after the remark.
	const el = `<span class="el">{{.Elapsed}}</span>`
	if n := strings.Count(s, el); n != 1 {
		t.Errorf("tasks.html renders %s %d time(s), want exactly 1 in the row (criterion 22 / B6)", el, n)
	}
	if r, e := strings.Index(rows, "{{.Remark}}"), strings.Index(rows, "{{.Elapsed}}"); r < 0 || e < 0 || e < r {
		t.Errorf("{{.Elapsed}} does not follow {{.Remark}} in the row (B4 cell 7: the light span, the remark, the elapsed)")
	}
	for _, banned := range []string{"Date.now", "new Date(t", "elapsed("} {
		if strings.Contains(s, banned) {
			t.Errorf("tasks.html computes an elapsed time in JS (%q). B6: it is a DB-clock fact formatted by a pure Go "+
				"function; the only browser clock is the decorative wall clock (B14)", banned)
		}
	}
}

// ---- criterion 30: the /static/ route is open, and only it ---------------------------

func TestBoardServer_StaticRouteIsOpenAndTheRestIsNot(t *testing.T) {
	src := readSrc(t, "server.go")
	route := regexp.MustCompile(`mux\.Handle\("GET /static/\{path\.\.\.\}",\s*http\.StripPrefix\("/static/",\s*http\.FileServerFS\(([^)]*)\)\)\)`)
	m := route.FindStringSubmatchIndex(src)
	if m == nil {
		t.Fatalf(`server.go registers no mux.Handle("GET /static/{path...}", http.StripPrefix("/static/", ` +
			`http.FileServerFS(staticSub))). Criterion 30 / B17`)
	}
	line := src[m[0]:m[1]]
	if strings.Contains(line, "Require(") {
		t.Errorf("the /static/ route is wrapped in s.auth.Require: %s. Criterion 30 / B17: a manifest, its icons and a "+
			"@font-face file are fetched WITHOUT credentials, so an authenticated route would 302 them to the login "+
			"page and silently break the installability path", line)
	}
	// The reason is written IN THE CODE: the next reader must not "fix" it.
	before := src[:m[0]]
	if i := strings.LastIndex(before, "\n\n"); i >= 0 {
		before = before[i:]
	}
	if !strings.Contains(before, "//") {
		t.Errorf("the /static/ route carries no comment saying WHY it is unauthenticated (criterion 30)")
	}
	if !strings.Contains(src, "//go:embed static") {
		t.Errorf("server.go has no //go:embed static (criterion 30: the binary still carries everything and nothing " +
			"new lands in the image build)")
	}
	if !strings.Contains(src, `fs.Sub(`) {
		t.Errorf("server.go does not build the sub-FS with fs.Sub (criterion 30: fs.Sub over an embedded FS cannot " +
			"traverse out of static/)")
	}
	if !strings.Contains(src, "Cache-Control") || !strings.Contains(src, "public, max-age=31536000, immutable") {
		t.Errorf("the static handler does not set `Cache-Control: public, max-age=31536000, immutable` (B17)")
	}
	// Everything that reads or writes a task is still behind the session.
	for _, r := range []string{`GET /tasks`, `GET /tasks/{id}`, `POST /tasks/{id}/dismiss`, `POST /tasks/{id}/close`,
		`GET /deliveries`, `GET /funnel`} {
		re := regexp.MustCompile(`mux\.Handle\("` + regexp.QuoteMeta(r) + `",\s*s\.auth\.Require\(`)
		if !re.MatchString(src) {
			t.Errorf("%q is no longer registered with s.auth.Require (criterion 30: only /healthz and /static/ are open)", r)
		}
	}
	open := regexp.MustCompile(`mux\.Handle(?:Func)?\("(GET|POST) ([^"]*)",\s*(?:func|http\.HandlerFunc|http\.StripPrefix)`)
	for _, m := range open.FindAllStringSubmatch(src, -1) {
		if m[2] != "/healthz" && m[2] != "/static/{path...}" {
			t.Errorf("%s %s is registered WITHOUT s.auth.Require; only /healthz and /static/ are open routes "+
				"(criterion 30)", m[1], m[2])
		}
	}
}

// ---- criterion 31: the vendored assets are really there -------------------------------

func TestStaticAssets_FontsLicencesSourcesAndIcons(t *testing.T) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatalf("fs.Sub(staticFS, \"static\"): %v (criterion 30)", err)
	}
	read := func(name string) []byte {
		t.Helper()
		b, err := fs.ReadFile(sub, name)
		if err != nil {
			t.Errorf("the embedded static FS has no %s: %v (criterion 31 / B17)", name, err)
			return nil
		}
		if len(b) == 0 {
			t.Errorf("%s is empty (criterion 31)", name)
		}
		return b
	}
	fonts := []string{
		"fonts/b612mono-400.woff2", "fonts/b612mono-700.woff2",
		"fonts/barlowcondensed-500.woff2", "fonts/barlowcondensed-600.woff2", "fonts/barlowcondensed-700.woff2",
	}
	for _, f := range fonts {
		if b := read(f); len(b) >= 4 && string(b[:4]) != "wOF2" {
			t.Errorf("%s does not start with the woff2 signature wOF2 (criterion 31: a placeholder is not a font)", f)
		}
	}
	for _, l := range []string{"fonts/OFL-B612.txt", "fonts/OFL-BarlowCondensed.txt"} {
		if b := read(l); b != nil && !strings.Contains(string(b), "SIL OPEN FONT LICENSE") {
			t.Errorf("%s does not carry the SIL OPEN FONT LICENSE text. B17: the OFL permits redistribution WITH the "+
				"licence text, which is why it ships beside the fonts", l)
		}
	}
	if b := read("fonts/SOURCES.md"); b != nil {
		src := string(b)
		for _, f := range fonts {
			base := f[len("fonts/"):]
			if !strings.Contains(src, base) {
				t.Errorf("SOURCES.md does not name %s (B17: upstream URL, version and sha256 of EACH file)", base)
			}
			// The provenance check of the SPEC's Verification step 4, run here
			// instead of by hand: the digest SOURCES.md claims must be the digest
			// of the bytes the binary actually carries.
			if fb := read(f); fb != nil {
				sum := fmt.Sprintf("%x", sha256.Sum256(fb))
				if !strings.Contains(src, sum) {
					t.Errorf("SOURCES.md does not carry the sha256 of the embedded %s (%s). B17: the digests are the "+
						"whole point of recording provenance — a font swapped without updating them is invisible", base, sum)
				}
			}
		}
		if n := len(regexp.MustCompile(`(?i)\b[0-9a-f]{64}\b`).FindAllString(src, -1)); n < len(fonts) {
			t.Errorf("SOURCES.md carries %d sha256 digests, want at least %d (one per font file)", n, len(fonts))
		}
		if !regexp.MustCompile(`https?://`).MatchString(src) {
			t.Errorf("SOURCES.md names no upstream URL (B17)")
		}
	}
	for _, icon := range []string{"icon-192.png", "icon-512.png"} {
		if b := read(icon); len(b) >= 8 && string(b[1:4]) != "PNG" {
			t.Errorf("%s is not a PNG (criterion 31)", icon)
		}
	}
	// Invariant 6: nothing client-visible carries an AI byline — not even the
	// vendored metadata or the manifest.
	_ = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, rerr := fs.ReadFile(sub, p)
		if rerr != nil {
			return nil
		}
		if !strings.HasSuffix(p, ".txt") && !strings.HasSuffix(p, ".md") && !strings.HasSuffix(p, ".webmanifest") {
			return nil
		}
		for _, banned := range []string{"Claude", "Anthropic", "Co-Authored-By"} {
			if strings.Contains(string(b), banned) {
				t.Errorf("static/%s mentions %q (invariant 6: no AI byline in the markup, the manifest, the icons or "+
					"the vendored font metadata)", p, banned)
			}
		}
		return nil
	})
}

// ---- criterion 32: the manifest ---------------------------------------------------------

func TestStaticManifest_IsAFullScreenManifest(t *testing.T) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatalf("fs.Sub(staticFS, \"static\"): %v", err)
	}
	raw, err := fs.ReadFile(sub, "manifest.webmanifest")
	if err != nil {
		t.Fatalf("the embedded static FS has no manifest.webmanifest: %v (criterion 32)", err)
	}
	var m struct {
		Display         string   `json:"display"`
		DisplayOverride []string `json:"display_override"`
		StartURL        string   `json:"start_url"`
		Scope           string   `json:"scope"`
		ID              string   `json:"id"`
		ThemeColor      string   `json:"theme_color"`
		Icons           []struct {
			Src     string `json:"src"`
			Sizes   string `json:"sizes"`
			Type    string `json:"type"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("manifest.webmanifest is not JSON: %v (criterion 32)", err)
	}
	if m.Display != "fullscreen" {
		t.Errorf("manifest display = %q, want \"fullscreen\" (criterion 32 / B18-2)", m.Display)
	}
	if !contains(m.DisplayOverride, "fullscreen") {
		t.Errorf("manifest display_override = %v, want it to contain \"fullscreen\" (criterion 32)", m.DisplayOverride)
	}
	if m.StartURL != "/tasks" {
		t.Errorf("manifest start_url = %q, want \"/tasks\" (criterion 32: the board IS the app)", m.StartURL)
	}
	if m.Scope != "/" {
		t.Errorf("manifest scope = %q, want \"/\" (criterion 32)", m.Scope)
	}
	if m.ID != "/tasks" {
		t.Errorf("manifest id = %q, want \"/tasks\" (B18-2)", m.ID)
	}
	if m.ThemeColor == "" {
		t.Errorf("manifest has no theme_color (criterion 32)")
	}
	sizes := map[string]bool{}
	for _, ic := range m.Icons {
		sizes[ic.Sizes] = true
		if !strings.Contains(ic.Purpose, "maskable") {
			t.Errorf("manifest icon %s has purpose %q, want it to include maskable (criterion 32: both icons are "+
				"`any maskable`)", ic.Src, ic.Purpose)
		}
		if !strings.HasPrefix(ic.Src, "/static/") {
			t.Errorf("manifest icon src = %q, want an in-app /static/ path (criterion 18's spirit)", ic.Src)
		}
	}
	if len(m.Icons) != 2 || !sizes["192x192"] || !sizes["512x512"] {
		t.Errorf("manifest icons = %v, want exactly two with sizes 192x192 and 512x512 (criterion 32)", m.Icons)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
