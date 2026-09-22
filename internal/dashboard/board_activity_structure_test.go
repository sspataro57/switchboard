package dashboard

// activity-resurfaces (SWT-72, docs/tickets/activity-resurfaces_SPEC.md) D5, D6
// and criteria 27, 28, 30, 31, 32 and 33: the board's four display-only facts,
// the widened FIRST statement, the two template additions and the Requeue
// route. ZERO I/O beyond this package's own source and the embedded tasks.html.
//
// board_incoming_structure_test.go (SWT-59) is the neighbour; its helpers
// (lightFactsStatements, coalesceSpans, funcBodySrc, structFieldNames,
// tasksHTML, readSrc, parseDashboardSource) are reused rather than respelled.
//
// GREENFIELD NOTE, EXPECTED RED: none of the four fields, the widened
// statement, activityRemark, the template additions or the route exist.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - replace the needs_review expression with false -> FirstStatementReadsTheActivityFacts.
//   - drop the nm PK join -> the same test (and criterion 40's sender/remark).
//   - lightFor reads NeedsReview -> DisplayOnlyFieldsNeverFeedTheLight.
//   - the Requeue form outside {{if .NeedsReview}} -> RequeueFormLivesUnderNeedsReview.
//   - the priority select defaulting to 0 -> the same test (a careless tap would
//     silently demote an elevated task).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strings"
	"testing"
)

// Criteria 27 and 30: the new fields exist and are typed. Compile-time.
var _ = lightFacts{NeedsReview: true, ActivityChannel: "jira", ActivitySender: "Katie Evans (JIRA)",
	ActivityStamp: "2026-09-22 13:20:00.000000"}
var _ = taskRow{NeedsReview: true, ActivityFrom: "Katie Evans (JIRA)", ActivityStamp: "2026-09-22 13:20:00.000000"}

// ---- criterion 28: the first statement --------------------------------------------

var activityFactTokens = []struct {
	name string
	re   *regexp.Regexp
}{
	{"t.activity_at IS NOT NULL", regexp.MustCompile(`t\.activity_at\s+IS\s+NOT\s+NULL`)},
	{"t.reviewed_at IS NULL OR t.activity_at > t.reviewed_at",
		regexp.MustCompile(`t\.reviewed_at\s+IS\s+NULL\s+OR\s+t\.activity_at\s*>\s*t\.reviewed_at`)},
	{"nm.channel", regexp.MustCompile(`\bnm\.channel\b`)},
	{"nm.sender", regexp.MustCompile(`\bnm\.sender\b`)},
	{"the nm PK join",
		regexp.MustCompile(`LEFT\s+JOIN\s+normalized_messages\s+nm\s+ON\s+nm\.id\s*=\s*t\.activity_by_message_id`)},
}

func TestBoardLightFacts_FirstStatementReadsTheActivityFacts(t *testing.T) {
	body, first, second := lightFactsStatements(t)
	if n := strings.Count(body, "s.pool.Query"); n > 2 {
		t.Errorf("boardLightFacts issues %d statements, want at most two (criterion 28: still at most two)", n)
	}
	for _, tok := range activityFactTokens {
		if !tok.re.MatchString(first) {
			t.Errorf("boardLightFacts' FIRST statement does not carry %s (criterion 28)", tok.name)
		}
		if second != "" && tok.re.MatchString(second) {
			t.Errorf("boardLightFacts' SECOND statement mentions %s; the facts come from the first, and the "+
				"second is byte-unchanged (criterion 28)", tok.name)
		}
	}
	// Each of the four aliases is selected AND read by the outer list.
	for _, alias := range []string{"needs_review", "activity_channel", "activity_sender", "activity_stamp"} {
		if !regexp.MustCompile(`\bAS\s+` + alias + `\b`).MatchString(first) {
			t.Errorf("the first statement has no expression aliased AS %s (criterion 28)", alias)
		}
		if !regexp.MustCompile(`\bf\.` + alias + `\b`).MatchString(first) {
			t.Errorf("the outer select list does not read f.%s (criterion 28: inner f select AND outer list)", alias)
		}
	}
	// NULL safety, the SWT-59 discipline: each of the four is COALESCEd.
	spans := coalesceSpans(first)
	inSpan := func(at int) bool {
		for _, s := range spans {
			if s[0] <= at && at < s[1] {
				return true
			}
		}
		return false
	}
	for _, tok := range activityFactTokens[:4] { // the join is not an expression
		for _, l := range tok.re.FindAllStringIndex(first, -1) {
			if !inSpan(l[0]) {
				t.Errorf("the first statement mentions %s outside any COALESCE( ... ); a NULL activity_at or an "+
					"unjoined nm row must render as false / \"\", never as a scan error (criterion 28)", tok.name)
			}
		}
	}
	// The stamp is Postgres', in BoardTimeZone, at microsecond precision — the
	// ordering key of D5's fourth clause. NO Go clock is involved.
	if !regexp.MustCompile(`to_char\(\s*t\.activity_at\s+AT\s+TIME\s+ZONE\s+\$2\s*,\s*'YYYY-MM-DD HH24:MI:SS\.US'\s*\)`).
		MatchString(first) {
		t.Errorf("the activity_stamp expression is not to_char(t.activity_at AT TIME ZONE $2, " +
			"'YYYY-MM-DD HH24:MI:SS.US'). Criterion 28: the sort key is a lexicographically sortable string the " +
			"DATABASE produces; a Go clock here would make the board's order depend on the pod's timezone")
	}
	for _, f := range []string{"NeedsReview", "ActivityChannel", "ActivitySender", "ActivityStamp"} {
		if !strings.Contains(first, f) {
			t.Errorf("boardLightFacts never sets lightFacts.%s from the first statement's scan (criterion 28)", f)
		}
	}
}

// Criterion 28: boardQuery, TaskExportRow and both exports are byte-unchanged.
// The board's facts never enter the exports, whose header is pinned.
func TestBoardActivity_ExportsAndBoardQueryUntouched(t *testing.T) {
	q := funcBodySrc(t, "board.go", "boardQuery")
	if q == "" {
		t.Fatalf("board.go declares no boardQuery")
	}
	for _, tok := range []string{"activity_at", "reviewed_at", "activity_by_message_id", "normalized_messages"} {
		if strings.Contains(q, tok) {
			t.Errorf("boardQuery mentions %s; it is byte-unchanged and it feeds the CSV and JSON exports "+
				"(criterion 28)", tok)
		}
	}
	exp := structFieldNames(t, "export.go", "TaskExportRow")
	if exp == nil {
		t.Fatalf("export.go declares no TaskExportRow")
	}
	for _, f := range []string{"NeedsReview", "ActivityFrom", "ActivityStamp", "ActivityChannel", "ActivitySender"} {
		if exp[f] {
			t.Errorf("TaskExportRow has %s; the activity facts are board-only, never export columns (criterion 30)", f)
		}
	}
}

// ---- criteria 30 and 31: listTasks and the remark ---------------------------------

func TestListTasks_SetsTheActivityFields(t *testing.T) {
	body := funcBodySrc(t, "board.go", "listTasks")
	if body == "" {
		t.Fatalf("board.go declares no listTasks")
	}
	for _, want := range []struct{ re, why string }{
		{`NeedsReview\s*:\s*f\.NeedsReview`, "criterion 30: the row carries the review flag the template branches on"},
		{`ActivityFrom\s*:\s*f\.ActivitySender`, "criterion 30: the muted `from …` span carries nm.sender"},
		{`ActivityStamp\s*:\s*f\.ActivityStamp`, "criterion 30: the section's fourth sort key"},
		{`activityRemark\(\s*f\.ActivityChannel\s*\)`, "criterion 30: when NeedsReview, the Remark is OVERRIDDEN " +
			"by the channel's words"},
	} {
		if !regexp.MustCompile(want.re).MatchString(body) {
			t.Errorf("listTasks does not match /%s/ — %s", want.re, want.why)
		}
	}
	// The override is CONDITIONAL: a reviewed row keeps remarkFor's words.
	i := regexp.MustCompile(`activityRemark\(`).FindStringIndex(body)
	if i != nil {
		window := body[max(i[0]-200, 0):i[0]]
		if !strings.Contains(window, "NeedsReview") {
			t.Errorf("listTasks calls activityRemark unconditionally. Criterion 30: `when NeedsReview, override "+
				"Remark` — a reviewed row keeps remarkFor's words (queued, holding, next up):\n%s", window)
		}
	}
	if rf := funcBodySrc(t, "display.go", "remarkFor"); strings.Contains(rf, "activity") ||
		strings.Contains(rf, "NeedsReview") {
		t.Errorf("remarkFor learned about activity; criterion 31 says it is byte-unchanged and the override " +
			"happens in listTasks")
	}
	if ar := funcBodySrc(t, "display.go", "activityRemark"); ar == "" {
		t.Errorf("display.go declares no func activityRemark (criterion 31)")
	} else {
		for _, banned := range []string{"s.pool", "Query(", "Exec(", "time."} {
			if strings.Contains(ar, banned) {
				t.Errorf("activityRemark contains %q; it is a pure helper over nm.channel (invariant 7)", banned)
			}
		}
	}
}

// ---- criterion 27: the four facts are display-only ---------------------------------

func TestLightFacts_ActivityFieldsNeverFeedTheLight(t *testing.T) {
	body := funcBodySrc(t, "lights.go", "lightFor")
	if body == "" {
		t.Fatalf("lights.go declares no lightFor")
	}
	for _, f := range []string{"NeedsReview", "ActivityChannel", "ActivitySender", "ActivityStamp"} {
		if strings.Contains(body, f) {
			t.Errorf("lightFor's body mentions %s. Criterion 27: incoming is PLACEMENT; the light never reads "+
				"it, which is why a claude task keeps its yellow light and its session tag when activity "+
				"surfaces it (D7 / OQ-2 = A)", f)
		}
	}
	// Each field is documented as display-only, the SWT-59 shape.
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
	for _, name := range []string{"NeedsReview", "ActivityChannel", "ActivitySender", "ActivityStamp"} {
		found, documented := false, false
		for _, fl := range st.Fields.List {
			for _, nm := range fl.Names {
				if nm.Name != name {
					continue
				}
				found = true
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
		if !found {
			t.Errorf("lightFacts has no %s field (criterion 27)", name)
		} else if !documented {
			t.Errorf("lightFacts.%s is not documented as display-only (criterion 27)", name)
		}
	}
}

// ---- criterion 32: the template's exactly two additions ----------------------------

func TestTasksTemplate_RequeueFormAndSenderSpanUnderNeedsReview(t *testing.T) {
	s := tasksHTML(t)

	// 1. The muted `from {{.ActivityFrom}}` span, in the TITLE cell, exactly
	//    where `reopened after dismissal` already sits, and only when non-empty.
	if !regexp.MustCompile(`\{\{if \.NeedsReview\}\}.*\{\{\.ActivityFrom\}\}`).MatchString(s) &&
		!regexp.MustCompile(`\{\{if (and )?\.ActivityFrom.*\}\}.*\{\{\.ActivityFrom\}\}`).MatchString(s) {
		t.Errorf("tasks.html has no guarded `from {{.ActivityFrom}}` span. Criterion 32: one muted span in the " +
			"title cell, inside {{if .NeedsReview}}, only when non-empty (0c: an empty sender renders no span)")
	}
	if !strings.Contains(s, "ActivityFrom") {
		t.Errorf("tasks.html never renders .ActivityFrom; D5: \"the sender in the title cell\" is how he tells " +
			"Katie's comment from his own Anonymous (JIRA) edit and Requeues it in one tap (D10)")
	}
	title := s
	if i := strings.Index(s, `<span class="title">`); i >= 0 {
		title = s[i:min(i+600, len(s))]
	}
	if !strings.Contains(title, "ActivityFrom") {
		t.Errorf("the `from …` span is not in the title cell, beside `reopened after dismissal` (criterion 32):\n%s", title)
	}

	// 2. The Requeue form, in the per-row actions popup, UNDER {{if .NeedsReview}}.
	form := regexp.MustCompile(`(?s)\{\{if \.NeedsReview\}\}\s*<form[^>]*action="/tasks/\{\{\.ID\}\}/requeue"`)
	if !form.MatchString(s) {
		t.Errorf("tasks.html has no Requeue form under {{if .NeedsReview}}. Criterion 32/D6: the form lives in " +
			"the per-row `actions` popup, exactly where there is something to clear")
	}
	if !strings.Contains(s, `action="/tasks/{{.ID}}/requeue"`) {
		t.Errorf("tasks.html posts no /tasks/{id}/requeue (criteria 32, 33)")
	}
	// The priority <select> leads with `unchanged` (empty value), then 0..3.
	sel := regexp.MustCompile(`(?s)<select name="priority">\s*<option value="">([^<]*)</option>`)
	m := sel.FindStringSubmatch(s)
	if m == nil {
		t.Errorf("the Requeue form has no `<select name=\"priority\">` leading with an EMPTY-valued option. " +
			"D6: it leads with `priority: unchanged` (omitted from the args) then 0 normal … 3 urgent; a " +
			"default of 0 would silently demote an elevated task on a careless tap")
	} else if !strings.Contains(strings.ToLower(m[1]), "unchanged") {
		t.Errorf("the priority select's first option reads %q, want it to say `unchanged` (D6)", m[1])
	}
	for _, level := range []string{"0", "1", "2", "3"} {
		if !strings.Contains(s, `<option value="`+level+`"`) {
			t.Errorf("the priority select has no option for %s; D6: 0 normal … 3 urgent, the existing scale", level)
		}
	}
	if strings.Contains(s, `<option value="-1"`) {
		t.Errorf("the priority select offers -1. D6: priority is 0..3 and \"low priority\" IS 0; the verb does " +
			"not invent a value below the scale")
	}
	// The form carries the five board keys, like Dismiss and Done (boardBack).
	if i := strings.Index(s, `action="/tasks/{{.ID}}/requeue"`); i >= 0 {
		window := s[i:min(i+900, len(s))]
		for _, k := range []string{"project", "status", "assignee_type", "subproject", "refresh"} {
			if !strings.Contains(window, `name="`+k+`"`) {
				t.Errorf("the Requeue form has no hidden %q input; boardBack rebuilds the redirect from the five "+
					"board keys, so a Requeue from a filtered, auto-refreshing board must land back on it", k)
			}
		}
	}
	// Criterion 32's guards, unchanged from SWT-59 and SWT-52.
	if strings.Contains(strings.ToLower(s), "incoming") {
		t.Errorf("tasks.html mentions incoming; the section comes from data (SWT-59 criterion 14)")
	}
	if n := strings.Count(s, "<script"); n != 1 {
		t.Errorf("tasks.html has %d <script tags, want exactly 1 (criterion 32)", n)
	}
	if strings.Contains(s, "htmx") || strings.Contains(s, "hx-") {
		t.Errorf("tasks.html uses HTMX; the board has none (pinned)")
	}
}

// ---- criterion 33: the route ------------------------------------------------------

func TestServer_RegistersTheRequeueRoute(t *testing.T) {
	src := readSrc(t, "server.go")
	re := regexp.MustCompile(`mux\.Handle\(\s*"POST /tasks/\{id\}/requeue"\s*,\s*s\.auth\.Require\(`)
	if !re.MatchString(src) {
		t.Errorf("server.go does not register POST /tasks/{id}/requeue behind auth.Require, beside dismiss and " +
			"close (criterion 33)")
	}
	if !strings.Contains(src, "requeueTaskAction") {
		t.Errorf("server.go does not route to requeueTaskAction (criterion 33)")
	}
	body := funcBodySrc(t, "board.go", "requeueTaskAction")
	if body == "" {
		t.Fatalf("board.go declares no requeueTaskAction. D6: POST /tasks/{id}/requeue -> one executeTask with " +
			"boardBack(r), no SQL of its own (invariant 3)")
	}
	if !strings.Contains(body, `"task_requeue"`) {
		t.Errorf("requeueTaskAction does not call the task_requeue tool (invariant 3)")
	}
	if !strings.Contains(body, "boardBack(r)") {
		t.Errorf("requeueTaskAction does not redirect via boardBack(r); every board verb uses the ONE spelling " +
			"of the redirect keys (D5's rule, SWT-52 D15)")
	}
	for _, banned := range []string{"s.pool", "Query(", "Exec("} {
		if strings.Contains(body, banned) {
			t.Errorf("requeueTaskAction contains %q; it is ONE executeTask call and no SQL of its own "+
				"(invariant 3)", banned)
		}
	}
	// An OMITTED priority must reach the tool as an omitted argument, never 0.
	if !regexp.MustCompile(`(?s)priority`).MatchString(body) {
		t.Errorf("requeueTaskAction never mentions priority; the form's select posts it (D6 step 4)")
	}
	if regexp.MustCompile(`"priority"\s*:\s*0\b`).MatchString(body) {
		t.Errorf("requeueTaskAction hardcodes priority 0. D6: `priority: unchanged` is the select's first option " +
			"and it is OMITTED from the args — a default of 0 would silently demote an elevated task")
	}
}
