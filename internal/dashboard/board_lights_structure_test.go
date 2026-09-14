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

func TestTasksTemplate_LightSpanBeforeTheID(t *testing.T) {
	s := tasksHTML(t)
	block, ok := templateBlockAfter(s, "{{range .Tasks}}")
	if !ok {
		t.Fatalf("tasks.html has no {{range .Tasks}} … {{end}} block")
	}
	const idLink = `<a href="/tasks/{{.ID}}">{{.ID}}</a>`
	i := strings.Index(block, lightSpan)
	j := strings.Index(block, idLink)
	if i < 0 {
		t.Fatalf("the per-task range has no light span %s (criterion 7)", lightSpan)
	}
	if j < 0 || i > j {
		t.Fatalf("the light span does not sit BEFORE the id link %s (criterion 7: 'before the id')", idLink)
	}
	if between := strings.TrimSpace(block[i+len(lightSpan) : j]); between != "" {
		t.Errorf("between the light span and the id link: %q, want nothing — the light is in the id cell, right "+
			"before the id", between)
	}
	if n := strings.Count(s, `class="light `); n != 1 {
		t.Errorf("tasks.html has %d light spans, want exactly 1 (in the per-task range)", n)
	}
	// Criterion 8: the span's attributes reference only .Light.Class/.Light.Label.
	for _, m := range regexp.MustCompile(`\{\{([^}]*)\}\}`).FindAllStringSubmatch(lightSpan, -1) {
		if a := strings.TrimSpace(m[1]); a != ".Light.Class" && a != ".Light.Label" {
			t.Errorf("light span references %q", a)
		}
	}
	if strings.Contains(s, "eq .Status") || strings.Contains(s, "eq .Light") {
		t.Errorf("tasks.html branches on the status or the light; the template never branches (D3)")
	}
}

func TestTasksTemplate_LegendAndRingStyles(t *testing.T) {
	s := tasksHTML(t)
	lower := strings.ToLower(s)
	f := strings.Index(s, `<form class="filters"`)
	r := strings.Index(s, "{{range .Columns}}")
	if f < 0 || r < 0 {
		t.Fatalf("tasks.html lost its filter form or its column range")
	}
	end := strings.Index(s[f:], "</form>")
	legend := strings.ToLower(s[f+end : r])
	for _, words := range [][]string{
		{"green", "done"}, {"yellow", "in progress"}, {"yellow ring", "no recent signal"},
		{"red", "waiting on your input"}, {"blue", "next in queue"}, {"grey ring", "not queued"},
	} {
		for _, w := range words {
			if !strings.Contains(legend, w) {
				t.Errorf("the legend under the filter form does not say %q (criterion 7: each light named in words: %v)", w, words)
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
