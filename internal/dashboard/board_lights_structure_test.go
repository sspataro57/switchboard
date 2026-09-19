package dashboard

// SWT-52 (docs/tickets/board-status-lights_SPEC.md) criteria 3, 5, 7-11, 14 and
// 15 (the header half): the structural and pure-SQL contract of the lights and
// the done-until-midnight predicate. ZERO I/O beyond this package's own source
// and embedded templates; boardQuery is CALLED with an httptest request, so the
// exact default SQL and its bind parameters are asserted, not grepped.
//
// IMPOSED SURFACE (SPEC criteria 3, 5, 10):
//
//	// board.go
//	const BoardTimeZone = "America/New_York"
//	func boardDayStart(p string) string // date_trunc('day', now() AT TIME ZONE <p>) AT TIME ZONE <p>
//	func (s *Server) boardLightFacts(ctx context.Context, rows []TaskExportRow) (…)
//	type taskRow struct{ …; Light light }
//
// GREENFIELD NOTE — EXPECTED RED: BoardTimeZone, boardDayStart, light and
// taskRow.Light do not exist, so package dashboard's test binary compile-FAILS.
// Once it compiles, every test here fails until the SPEC is implemented —
// TestTasksTemplate_VerbFormsByteUnchanged included: since D15 it requires
// exactly one hidden refresh input in each verb form (criteria 9, 32).
//
// Criterion 7 as amended by D15: no HTMX, and the ONLY JavaScript is the one
// refresh script — board_refresh_test.go pins it. Criteria 30-32 live there.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var _ = taskRow{Light: light{Class: "none", Label: "ready, queued"}} // criterion 3: taskRow gains Light

// normSQL collapses whitespace and the spaces just inside parentheses, so a
// reformatted literal compares equal and a changed one does not.
func normSQL(s string) string {
	s = regexp.MustCompile(`\s+`).ReplaceAllString(strings.TrimSpace(s), " ")
	s = strings.ReplaceAll(s, "( ", "(")
	return strings.ReplaceAll(s, " )", ")")
}

const boardSelect = `SELECT t.id, COALESCE(p.slug,''), COALESCE(t.subproject,''), t.parent_id,
	t.title, t.status, t.assignee_type, COALESCE(t.worker_type,''), t.priority, t.plan_order,
	COALESCE(t.created_at::text,''), COALESCE(t.updated_at::text,'')
	FROM tasks t JOIN projects p ON p.id = t.project_id`

func defaultPredicate(n string) string {
	return `(t.status <> 'closed'
	 OR (COALESCE(t.closed_at, t.updated_at) >= date_trunc('day', now() AT TIME ZONE ` + n + `) AT TIME ZONE ` + n + `
	     AND NOT EXISTS (SELECT 1 FROM task_dismissals d
	                      WHERE d.task_id = t.id AND d.reopened_at IS NULL)))`
}

// Criterion 10: one helper, one spelling of local midnight on the DB clock.
func TestBoardDayStart_OneSpelling(t *testing.T) {
	if got, want := boardDayStart("$1"), "date_trunc('day', now() AT TIME ZONE $1) AT TIME ZONE $1"; got != want {
		t.Errorf("boardDayStart(\"$1\") = %q, want %q (criterion 10)", got, want)
	}
	if got := boardDayStart("$3"); !strings.Contains(got, "AT TIME ZONE $3) AT TIME ZONE $3") {
		t.Errorf("boardDayStart(\"$3\") = %q: the parameter must be used for both conversions", got)
	}
}

// Criterion 11: the exact default predicate, BoardTimeZone bound; the status
// branch byte-unchanged; the SELECT list (the exports' columns) unchanged.
func TestBoardQuery_DefaultPredicateExactSQL(t *testing.T) {
	t.Run("no filters", func(t *testing.T) {
		q, args := boardQuery(httptest.NewRequest("GET", "/tasks", nil))
		want := boardSelect + ` WHERE ` + defaultPredicate("$1") + ` ORDER BY t.id ASC`
		if normSQL(q) != normSQL(want) {
			t.Errorf("default boardQuery =\n  %s\nwant\n  %s", normSQL(q), normSQL(want))
		}
		if len(args) != 1 || args[0] != "America/New_York" {
			t.Errorf("default boardQuery args = %v, want exactly [America/New_York]: the zone is a BOUND "+
				"parameter (D5), never a literal and never Go's clock", args)
		}
	})
	t.Run("project filter", func(t *testing.T) {
		q, args := boardQuery(httptest.NewRequest("GET", "/tasks?project=saka", nil))
		n := normSQL(q)
		if !strings.Contains(n, "p.slug = $1") || !strings.Contains(n, normSQL(defaultPredicate("$2"))) {
			t.Errorf("boardQuery(?project=saka) = %s; want p.slug = $1 AND the default predicate on $2", n)
		}
		if len(args) != 2 || args[0] != "saka" || args[1] != BoardTimeZone {
			t.Errorf("boardQuery(?project=saka) args = %v, want [saka America/New_York]", args)
		}
	})
	for _, st := range []string{"ready", "closed"} {
		t.Run("status="+st, func(t *testing.T) {
			q, args := boardQuery(httptest.NewRequest("GET", "/tasks?status="+st, nil))
			want := boardSelect + ` WHERE t.status = $1 ORDER BY t.id ASC`
			if normSQL(q) != normSQL(want) {
				t.Errorf("boardQuery(?status=%s) =\n  %s\nwant it byte-unchanged:\n  %s (criterion 11)", st, normSQL(q), normSQL(want))
			}
			if len(args) != 1 || args[0] != st {
				t.Errorf("boardQuery(?status=%s) args = %v, want [%s] — no zone parameter under a status filter", st, args, st)
			}
		})
	}
}

// Criterion 3 / 15: the exports keep their columns — TaskExportRow is unchanged.
func TestTaskExportRow_FieldsUnchanged(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "export.go", nil, 0)
	if err != nil {
		t.Fatalf("parse export.go: %v", err)
	}
	var got []string
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "TaskExportRow" {
			return true
		}
		for _, fl := range ts.Type.(*ast.StructType).Fields.List {
			for _, nm := range fl.Names {
				got = append(got, nm.Name)
			}
		}
		return false
	})
	want := "ID,Project,Subproject,ParentID,Title,Status,AssigneeType,WorkerType,Priority,PlanOrder,CreatedAt,UpdatedAt"
	if strings.Join(got, ",") != want {
		t.Errorf("TaskExportRow fields = %v, want exactly %s: none of boardLightFacts' columns enters the export row (D3)", got, want)
	}
}

// pkgSource is this package's non-test source, indexed for reachability.
type pkgSource struct {
	text   map[string]string // func/method/const/var name -> its source
	idents map[string][]string
}

func parseDashboardSource(t *testing.T) pkgSource {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	ps := pkgSource{text: map[string]string{}, idents: map[string][]string{}}
	fset := token.NewFileSet()
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		f, err := parser.ParseFile(fset, name, b, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		add := func(key string, node ast.Node) {
			ps.text[key] += string(b[fset.Position(node.Pos()).Offset:fset.Position(node.End()).Offset])
			ast.Inspect(node, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok {
					ps.idents[key] = append(ps.idents[key], id.Name)
				}
				return true
			})
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Body != nil {
					add(d.Name.Name, d.Body)
				}
			case *ast.GenDecl:
				if d.Tok != token.CONST && d.Tok != token.VAR {
					continue
				}
				for _, s := range d.Specs {
					vs := s.(*ast.ValueSpec)
					for _, n := range vs.Names {
						add(n.Name, vs)
					}
				}
			}
		}
	}
	return ps
}

// reach returns the concatenated source of root and everything it reaches
// through identifiers naming package-level funcs, methods, consts or vars.
func (ps pkgSource) reach(root string) (string, map[string]bool) {
	seen := map[string]bool{}
	queue := []string{root}
	var sb strings.Builder
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		src, ok := ps.text[n]
		if !ok || seen[n] {
			continue
		}
		seen[n] = true
		sb.WriteString(src)
		sb.WriteString("\n")
		queue = append(queue, ps.idents[n]...)
	}
	return sb.String(), seen
}

// Criterion 5: boardLightFacts is a SEPARATE read with these ingredients.
func TestBoardLightFacts_IsASeparateRead(t *testing.T) {
	ps := parseDashboardSource(t)
	if _, ok := ps.text["boardLightFacts"]; !ok {
		t.Fatalf("internal/dashboard declares no boardLightFacts. Criterion 5: (*Server).boardLightFacts(ctx, rows) " +
			"is the lights' own read, the SWT-36 reopenMarkers precedent")
	}
	src, seen := ps.reach("boardLightFacts")
	if seen["boardQuery"] {
		t.Errorf("boardLightFacts reaches boardQuery. Criterion 5: it is a SEPARATE read; boardQuery feeds the exports")
	}
	flat := regexp.MustCompile(`\s+`).ReplaceAllString(src, " ")
	for _, want := range []struct{ tok, why string }{
		{"working_state", "the session state (red vs yellow)"},
		{"working_state_at", "the signal time, for the labels and for staleness"},
		// SWT-56 (signal-session-name) criterion 17: the session name, read in the
		// same first statement (no new query; D15's count is unchanged).
		{"working_session", "the Claude session's name for the tag (S8)"},
		{"to_char(", "the time is formatted IN SQL, in BoardTimeZone"},
		{"make_interval(secs", "staleness on the DB clock: working_state_at < now() - make_interval(secs => $lease)"},
		{"WorkingLease", "the lease is tools.WorkingLease, spelled once"},
		{"BoardTimeZone", "the zone is bound from the one const"},
		{"boardDayStart(", "'closed today' uses criterion 11's one spelling of local midnight"},
		{"COALESCE(t.closed_at, t.updated_at)", "…against the close instant with its 0030 fallback"},
		{"reason_code", "the newest OPEN dismissal's code (rule 1a)"},
		{"reopened_at IS NULL", "…OPEN: reopened_at IS NULL"},
		{"'ready'", "every ready task in the database is a queue candidate, not the displayed rows only (D2)"},
		{"TaskQueueOrder", "candidates in tools.TaskQueueOrder, the ordering task_get_next uses"},
		{"lightFor(", "eligibility calls the same pure function (D2: one spelling)"},
		{"pickQueueHeads(", "the heads come from the pure pick"},
	} {
		if !strings.Contains(flat, want.tok) {
			t.Errorf("boardLightFacts (and what it reaches) never mentions %q — %s", want.tok, want.why)
		}
	}
	for _, banned := range []string{"feedback_requests", "task_events"} {
		if strings.Contains(flat, banned) {
			t.Errorf("boardLightFacts reads %s. Criterion 5: the lights read neither; red for a worker comes from "+
				"needs_feedback, and an Answer row's red is Future work", banned)
		}
	}
}

// Criterion 10: no Go clock feeds visibility or a light.
func TestBoard_NoGoClockFeedsVisibilityOrALight(t *testing.T) {
	ps := parseDashboardSource(t)
	for _, root := range []string{"boardQuery", "boardDayStart", "boardLightFacts", "lightFor", "pickQueueHeads"} {
		if _, ok := ps.text[root]; !ok {
			t.Errorf("internal/dashboard declares no %s", root)
			continue
		}
		src, _ := ps.reach(root)
		for _, banned := range []string{"time.Now", "time.Local", "LoadLocation", ".In(", "time.Since", "time.Until"} {
			if strings.Contains(src, banned) {
				t.Errorf("%s (or what it reaches) uses %s. Criterion 10 / D5: 'today' and 'stale' are decided on "+
					"Postgres now() in BoardTimeZone — the pods run in UTC (the SWT-48 lesson)", root, banned)
			}
		}
	}
}

// ---- the template (criteria 7, 8, 14) ------------------------------------------

func tasksHTML(t *testing.T) string {
	t.Helper()
	raw, err := templateFS.ReadFile("templates/tasks.html")
	if err != nil {
		t.Fatalf("read embedded tasks.html: %v", err)
	}
	return string(raw)
}

const lightSpan = `<span class="light light-{{.Light.Class}}" role="img" aria-label="{{.Light.Label}}" title="{{.Light.Label}}"></span>`

// REWRITTEN — not deleted — by board-departures (SWT-67,
// docs/tickets/board-departures_SPEC.md, criterion 23): the light span's MARKUP
// is byte-unchanged, it still appears exactly ONCE, and its actions still
// reference only .Light.Class and .Light.Label. What moves is WHERE it sits. The
// row is no longer a table: it is one anchor of seven cells (B4), and the light
// opens the `rem` cell immediately before {{.Remark}} — the light and the words
// that replace the dropped status column (B5) read as one phrase.
func TestTasksTemplate_LightSpanInTheRemarkCell(t *testing.T) {
	s := tasksHTML(t)
	block, ok := templateBlockAfter(s, "{{range .Tasks}}")
	if !ok {
		t.Fatalf("tasks.html has no {{range .Tasks}} … {{end}} block")
	}
	ri := strings.Index(block, `<span class="rem"`)
	if ri < 0 {
		t.Fatalf("the per-task range has no `rem` cell (criterion 23 / B4 cell 7). Row:\n%s", block)
	}
	re, ok := elementEnd(block, ri, "span")
	if !ok {
		t.Fatalf("the `rem` cell <span> is never closed")
	}
	cell := block[ri:re]
	i := strings.Index(cell, lightSpan)
	if i < 0 {
		t.Fatalf("the `rem` cell has no light span %s (criterion 23)\ncell: %s", lightSpan, cell)
	}
	if open := strings.Index(cell, ">") + 1; strings.TrimSpace(cell[open:i]) != "" {
		t.Errorf("the light span is not the FIRST thing in the `rem` cell; before it: %q (criterion 23)", cell[open:i])
	}
	rem := strings.Index(cell, "{{.Remark}}")
	if rem < 0 || rem < i {
		t.Fatalf("the `rem` cell does not render {{.Remark}} after the light span (criterion 23)\ncell: %s", cell)
	}
	if between := strings.TrimSpace(cell[i+len(lightSpan) : rem]); between != "" {
		t.Errorf("between the light span and {{.Remark}}: %q, want nothing — the light is the remark's bullet "+
			"(criterion 23)", between)
	}
	if n := strings.Count(s, `class="light `); n != 1 {
		t.Errorf("tasks.html has %d light spans, want exactly 1 (in the per-task range)", n)
	}
	// It MOVED: the id cell is no longer the light's home (B4's cell table).
	if ii := strings.Index(block, `<span class="id"`); ii >= 0 {
		if ie, ok := elementEnd(block, ii, "span"); ok && strings.Contains(block[ii:ie], `class="light `) {
			t.Errorf("the light span is still inside the id cell; criterion 23 puts it in the `rem` cell")
		}
	}
	// Criterion 8 (SWT-52), unchanged: the span's attributes reference only
	// .Light.Class/.Light.Label.
	for _, m := range regexp.MustCompile(`\{\{([^}]*)\}\}`).FindAllStringSubmatch(lightSpan, -1) {
		if a := strings.TrimSpace(m[1]); a != ".Light.Class" && a != ".Light.Label" {
			t.Errorf("light span references %q", a)
		}
	}
	if strings.Contains(s, "eq .Status") || strings.Contains(s, "eq .Light") {
		t.Errorf("tasks.html branches on the status or the light; the template never branches (D3)")
	}
}

// AMENDED — not deleted — by board-layout-compact (SWT-57), the SPEC's one
// deliberate amendment: the legend used to be the text between the filter
// form's </form> and {{range .Columns}}. Both anchors moved (the range is now
// {{range .Sections}}, and the legend sits below it), so the legend is now the
// <p … id="light-legend"> … </p> element itself, and it must start after the end
// of the {{range .Sections}} block. The six word pairs, the <style> ring
// assertions and the HTMX ban are unchanged.
func TestTasksTemplate_LegendAndRingStyles(t *testing.T) {
	s := tasksHTML(t)
	lower := strings.ToLower(s)
	const sectionsOpen = "{{range .Sections}}"
	loc := regexp.MustCompile(`(?s)<p[^>]*id="light-legend"[^>]*>.*?</p>`).FindStringIndex(s)
	r := strings.Index(s, sectionsOpen)
	if loc == nil || r < 0 {
		t.Fatalf("tasks.html lost its legend <p id=\"light-legend\"> or its {{range .Sections}}")
	}
	block, ok := templateBlockAfter(s, sectionsOpen)
	if !ok {
		t.Fatalf("tasks.html's {{range .Sections}} is never closed")
	}
	if end := r + len(sectionsOpen) + len(block) + len("{{end}}"); loc[0] < end {
		t.Errorf("the legend starts before the end of the {{range .Sections}} block (board-layout-compact L9: the legend is at the bottom)")
	}
	legend := strings.ToLower(s[loc[0]:loc[1]])
	for _, words := range [][]string{
		{"green", "done"}, {"yellow", "in progress"}, {"yellow ring", "no recent signal"},
		{"red", "waiting on your input"}, {"blue", "next in queue"}, {"grey ring", "not queued"},
	} {
		for _, w := range words {
			if !strings.Contains(legend, w) {
				t.Errorf("the legend does not say %q (criterion 7: each light named in words: %v)", w, words)
			}
		}
	}
	st, se := strings.Index(lower, "<style>"), strings.Index(lower, "</style>")
	if st < 0 || se < st {
		t.Fatalf("tasks.html has no <style> block")
	}
	style := s[st:se]
	for _, c := range []string{"done", "working", "stale", "input", "next", "none"} {
		m := regexp.MustCompile(`\.light-` + c + `\s*\{([^}]*)\}`).FindStringSubmatch(style)
		if m == nil {
			t.Errorf("the <style> block has no .light-%s rule (criterion 7: six classes)", c)
			continue
		}
		if c == "stale" || c == "none" {
			if !strings.Contains(m[1], "border") || !strings.Contains(m[1], "transparent") {
				t.Errorf(".light-%s {%s} is not a RING (a border with a transparent fill): shape as well as colour "+
					"separates it (criterion 7)", c, m[1])
			}
		}
	}
	if strings.Contains(lower, "hx-") || strings.Contains(lower, "htmx") {
		t.Errorf("tasks.html uses HTMX; the board has none (criterion 7)")
	}
	// Criterion 7 (D15): the one refresh script is pinned by
	// TestTasksTemplate_AutoRefreshToggleIndicatorAndOneScript.
}

func TestTasksTemplate_BoardNote(t *testing.T) {
	s := tasksHTML(t)
	const note = "Queues are filters on the one tasks table. Tasks closed today stay (green) until midnight " +
		"America/New_York; dismissed tasks leave at once; ?status=closed shows every closed task."
	if !strings.Contains(regexp.MustCompile(`\s+`).ReplaceAllString(s, " "), note) {
		t.Errorf("tasks.html does not carry the board note %q (criterion 14)", note)
	}
	if strings.Contains(s, "Closed tasks are hidden unless ?status=closed.") {
		t.Errorf("tasks.html still says closed tasks are hidden: false since SWT-52")
	}
}

// Criterion 9 (as amended by D15): the Dismiss and Done forms are
// byte-unchanged EXCEPT for exactly one added hidden input each,
// <input type="hidden" name="refresh" value="{{index $.Filters "refresh"}}">
// (criterion 32). The test removes that one line and compares the rest with
// SWT-51's text byte for byte.
func TestTasksTemplate_VerbFormsByteUnchanged(t *testing.T) {
	s := tasksHTML(t)
	const dismiss = `<form class="inline" method="post" action="/tasks/{{.ID}}/dismiss">
          <input type="hidden" name="project" value="{{index $.Filters "project"}}">
          <input type="hidden" name="status" value="{{index $.Filters "status"}}">
          <input type="hidden" name="assignee_type" value="{{index $.Filters "assignee_type"}}">
          <input type="hidden" name="subproject" value="{{index $.Filters "subproject"}}">
          <select name="reason_code">
            <option value="not_actionable">not actionable</option>
            <option value="wrong_kind">wrong kind</option>
            <option value="duplicate">duplicate</option>
            <option value="handled_elsewhere">handled elsewhere</option>
          </select>
          <input type="text" name="note" placeholder="note (optional)">
          <button>Dismiss</button>
        </form>`
	const done = `<form class="inline" method="post" action="/tasks/{{.ID}}/close">
          <input type="hidden" name="project" value="{{index $.Filters "project"}}">
          <input type="hidden" name="status" value="{{index $.Filters "status"}}">
          <input type="hidden" name="assignee_type" value="{{index $.Filters "assignee_type"}}">
          <input type="hidden" name="subproject" value="{{index $.Filters "subproject"}}">
          <input type="text" name="note" placeholder="note (optional)">
          <button>Done</button>
        </form>`
	refreshLine := regexp.MustCompile(`\n[ \t]*<input type="hidden" name="refresh" value="\{\{index \$\.Filters "refresh"\}\}">`)
	for _, tc := range []struct{ name, verb, golden string }{
		{"Dismiss", "dismiss", dismiss}, {"Done", "close", done},
	} {
		form := regexp.MustCompile(`(?s)<form class="inline" method="post" action="/tasks/\{\{\.ID\}\}/` + tc.verb + `">.*?</form>`).
			FindString(s)
		if form == "" {
			t.Errorf("tasks.html has no %s form", tc.name)
			continue
		}
		if n := len(refreshLine.FindAllString(form, -1)); n != 1 {
			t.Errorf("the %s form carries %d hidden refresh input(s), want exactly 1 (criteria 9, 32)", tc.name, n)
		}
		if rest := refreshLine.ReplaceAllString(form, ""); rest != tc.golden {
			t.Errorf("the %s form, minus its refresh input, is not byte-identical to SWT-51's (criterion 9):\n%s", tc.name, rest)
		}
	}
}

// ---- SWT-56 (signal-session-name) criteria 17-19 --------------------------------

// Criterion 17: the name is selected in boardLightFacts' FIRST statement — the
// one that also returns the render time — so no query is added.
func TestBoardLightFacts_FirstStatementSelectsTheSession(t *testing.T) {
	body := funcBodySrc(t, "board.go", "boardLightFacts")
	if body == "" {
		t.Fatalf("board.go declares no boardLightFacts")
	}
	first := strings.Index(body, "s.pool.Query(")
	if first < 0 {
		t.Fatalf("boardLightFacts runs no s.pool.Query")
	}
	stmt := body[first:]
	if second := strings.Index(stmt[len("s.pool.Query("):], "s.pool.Query("); second >= 0 {
		stmt = stmt[:len("s.pool.Query(")+second]
	}
	if !strings.Contains(stmt, "HH24:MI:SS") {
		t.Fatalf("CONTROL: the first statement does not carry the render time; the scan is not reading statement 1")
	}
	if !regexp.MustCompile(`COALESCE\(t\.working_session,\s*''\)`).MatchString(stmt) {
		t.Errorf("boardLightFacts' first statement does not select COALESCE(t.working_session, '') (criterion 17)")
	}
}

// Criterion 18: the tag is inside {{if .Light.Session}} and references only
// .Light.Session/.Class/.Label.
//
// REWRITTEN — not deleted — by board-departures (SWT-67, criterion 24): the
// tag's MARKUP is byte-unchanged (class, title and text all still come from
// .Light), it still renders exactly once and only inside {{if .Light.Session}};
// it moves out of the title cell into its OWN Gate cell (B10). The right pane
// hides that column in CSS — the value stays in the markup, so the SWT-56 CSS
// rules and the integration helper keep their anchors.
func TestTasksTemplate_SessionTagIsTheGateCell(t *testing.T) {
	s := tasksHTML(t)
	block, ok := templateBlockAfter(s, "{{range .Tasks}}")
	if !ok {
		t.Fatalf("tasks.html has no {{range .Tasks}} … {{end}} block")
	}
	tag := regexp.MustCompile(`<span class="gate">\s*\{\{if \.Light\.Session\}\}\s*` +
		`<span class="session-tag session-\{\{\.Light\.Class\}\}" title="\{\{\.Light\.Label\}\}">\{\{\.Light\.Session\}\}</span>` +
		`\s*\{\{end\}\}\s*</span>`)
	m := tag.FindString(block)
	if m == "" {
		t.Fatalf("the per-task range has no S8 session tag in its own `gate` cell, inside {{if .Light.Session}} "+
			"(criterion 24 / B10). Row:\n%s", block)
	}
	// It MOVED: the title cell carries the title and the reopen note, nothing else.
	if ti := strings.Index(block, `<span class="title"`); ti >= 0 {
		if te, ok := elementEnd(block, ti, "span"); ok && strings.Contains(block[ti:te], "session-tag") {
			t.Errorf("the session tag is still inside the title cell; criterion 24 gives it the Gate cell")
		}
	}
	for _, a := range regexp.MustCompile(`\{\{([^}]*)\}\}`).FindAllStringSubmatch(m, -1) {
		switch strings.TrimSpace(a[1]) {
		case "if .Light.Session", "end", ".Light.Session", ".Light.Class", ".Light.Label", ".ID", ".Title":
		default:
			t.Errorf("the session tag references %q; only .Light.Session, .Light.Class and .Light.Label (criterion 18)", a[1])
		}
	}
	if n := strings.Count(s, `class="session-tag `); n != 1 {
		t.Errorf("tasks.html has %d session tags, want exactly 1 (in the per-task range)", n)
	}
	if n := strings.Count(s, `class="light `); n != 1 {
		t.Errorf("tasks.html has %d light spans, want still exactly 1: the tag's class is session-tag", n)
	}
	if strings.Contains(s, "eq .Status") || strings.Contains(s, "eq .Light") {
		t.Errorf("tasks.html branches on the status or the light (the class comes from .Light.Class)")
	}

	lower := strings.ToLower(s)
	st, se := strings.Index(lower, "<style>"), strings.Index(lower, "</style>")
	if st < 0 || se < st {
		t.Fatalf("tasks.html has no <style> block")
	}
	style := s[st:se]
	rule := regexp.MustCompile(`\.session-tag\s*\{([^}]*)\}`).FindStringSubmatch(style)
	if rule == nil {
		t.Errorf("the <style> block has no .session-tag rule (criterion 18)")
	} else {
		if !regexp.MustCompile(`text-overflow:\s*ellipsis`).MatchString(rule[1]) || !strings.Contains(rule[1], "max-width") {
			t.Errorf(".session-tag {%s} lacks text-overflow: ellipsis and a max-width: truncation is visual only (S8)", rule[1])
		}
	}
	if !regexp.MustCompile(`\.session-input\s*\{`).MatchString(style) {
		t.Errorf("the <style> block has no .session-input rule (criterion 18)")
	}
	const legend = "A red or yellow light shows its Claude session's name at the start of the title: reply in that session."
	if !strings.Contains(regexp.MustCompile(`\s+`).ReplaceAllString(s, " "), legend) {
		t.Errorf("tasks.html's legend does not carry the S8 sentence %q (criterion 18)", legend)
	}
}

// Criterion 19: nothing on the name's path turns it into trusted HTML, so
// html/template escapes it in the text node and in the title attribute.
func TestDashboard_NoRawHTMLOnTheSessionPath(t *testing.T) {
	ps := parseDashboardSource(t)
	var srcs []string
	for _, root := range []string{"lightFor", "boardLightFacts"} {
		if _, ok := ps.text[root]; !ok {
			t.Fatalf("internal/dashboard declares no %s", root)
		}
		src, _ := ps.reach(root)
		srcs = append(srcs, src)
	}
	for _, f := range []string{"lights.go", "board.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		srcs = append(srcs, string(b))
	}
	for _, src := range srcs {
		for _, banned := range []string{"template.HTML(", "template.HTMLAttr(", "safeHTML"} {
			if strings.Contains(src, banned) {
				t.Errorf("the session name's path uses %s (criterion 19): the name is self-reported text and must reach "+
					"the page through html/template's contextual escaping", banned)
			}
		}
	}
	// The tag's field is a plain string (a template.HTML field would bypass escaping).
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "lights.go", nil, 0)
	if err != nil {
		t.Fatalf("parse lights.go: %v", err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "light" {
			return true
		}
		for _, fl := range ts.Type.(*ast.StructType).Fields.List {
			for _, nm := range fl.Names {
				if nm.Name == "Session" {
					found = true
					if id, ok := fl.Type.(*ast.Ident); !ok || id.Name != "string" {
						t.Errorf("light.Session is not a plain string (criterion 19)")
					}
				}
			}
		}
		return false
	})
	if !found {
		t.Errorf("lights.go's light has no Session field (S8)")
	}
}
