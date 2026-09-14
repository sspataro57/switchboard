package dashboard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The project select applies itself on change (SWT-26). The board is a plain
// GET form — the attribute is the whole feature, and it is exactly the kind of
// "unused attribute" a template cleanup would strip with every test staying
// green. Assert it, and assert the text inputs did NOT grow the same behaviour:
// submitting per keystroke would make status/assignee_type/subproject untypable.
func TestBoardTemplate_ProjectFilterAutoSubmits(t *testing.T) {
	raw, err := templateFS.ReadFile("templates/tasks.html")
	if err != nil {
		t.Fatalf("read embedded tasks.html: %v", err)
	}
	s := string(raw)

	if !strings.Contains(s, `<select name="project" onchange="this.form.submit()">`) {
		t.Fatalf("the project select no longer auto-submits on change; switching project must apply the filter without a second click (SWT-26)")
	}
	if n := strings.Count(s, "onchange"); n != 1 {
		t.Fatalf("expected exactly one onchange in tasks.html (the project select), found %d — text inputs must not submit while being typed in", n)
	}
}

// ---- SWT-31 criteria 16 + 17: the dismiss control and the redirect ------------

// The per-row dismiss form (criterion 16). The board has NO HTMX (D8) — a plain
// GET filter form and POST -> 303 -> flash, as /plans and /deliveries do — so the
// control is an ordinary <form method="post"> with a <select> and a text input.
//
// The load-bearing half is the NEGATIVE: the select must NOT carry `onchange`.
// TestBoardTemplate_ProjectFilterAutoSubmits above asserts EXACTLY ONE onchange
// in tasks.html, and copying the project filter's auto-submit into the dismiss
// select would both turn that test red and make a stray click a dismissal.
//
// GREENFIELD NOTE — EXPECTED RED: tasks.html has no form and no select named
// reason_code yet.
func TestBoardTemplate_DismissFormIsPlainPostWithNoAutoSubmit(t *testing.T) {
	raw, err := templateFS.ReadFile("templates/tasks.html")
	if err != nil {
		t.Fatalf("read embedded tasks.html: %v", err)
	}
	s := string(raw)

	if !strings.Contains(s, `method="post"`) {
		t.Fatalf("tasks.html has no POST form. Criterion 16: reason_code comes from a <select> and note " +
			"from a text input inside a per-row <form method=\"post\">, posting to /tasks/{id}/dismiss")
	}
	if !strings.Contains(s, `name="reason_code"`) {
		t.Errorf("tasks.html has no `reason_code` field. D4's enum is the whole label; a dismissal with " +
			"no code is a close, and task_close already exists")
	}
	if !strings.Contains(s, `name="note"`) {
		t.Errorf("tasks.html has no `note` field. The optional free text is where 'duplicate of the " +
			"Tuesday thread' goes — Salvador's own words, stored, never sent (invariant 6)")
	}
	if !strings.Contains(s, "/dismiss") {
		t.Errorf("tasks.html posts to no /dismiss action; the route is POST /tasks/{id}/dismiss (criterion 15)")
	}

	// The negative, spelled separately from the count assertion above so the
	// failure names the reason rather than a number.
	if strings.Count(s, "onchange") != 1 {
		t.Errorf("tasks.html has %d onchange attributes, want exactly 1 (the project filter). Criterion "+
			"16: the dismiss select must NOT auto-submit — a filter that applies itself is a "+
			"convenience, a dismissal that applies itself is a closed task nobody chose",
			strings.Count(s, "onchange"))
	}
	// D6: the control renders on EVERY row the board renders, not only for
	// holding/ready. Rendering only for some statuses would restate closeTask's
	// status list in a template, where it drifts silently; the refusal surfaces
	// as the existing flash instead. Structural proxy: the form lives inside the
	// per-task range, so it cannot be conditioned on a status without an {{if}}
	// naming one.
	for _, banned := range []string{`{{if eq .Status "ready"}}`, `{{if eq .Status "holding"}}`} {
		if strings.Contains(s, banned) {
			t.Errorf("tasks.html conditions markup on %s. D6: the dismiss control renders on every row "+
				"the board renders — a hidden affordance is a second, drifting copy of closeTask's "+
				"status list, and a visible error beats it", banned)
		}
	}
}

// Criterion 17: the redirect returns to /tasks WITH the current filters,
// "rebuilt from the four known keys (project, status, assignee_type, subproject)
// via url.Values — never by echoing r.URL.RawQuery (the safeNext lesson in
// internal/dashboard/auth.go)".
//
// A structure scan and not a rendering assertion, deliberately: the exact
// redirect string is the implementer's, and what belongs here is the pair of
// facts the SPEC states about its SHAPE. The behavioural half — that dismissing
// from a filtered board lands back on the SAME filtered board — is deferred to
// the integration suite, where a real 303 Location can be read.
func TestBoardHandler_RebuildsFiltersRatherThanEchoingRawQuery(t *testing.T) {
	src, err := os.ReadFile("board.go")
	if err != nil {
		t.Fatalf("read internal/dashboard/board.go: %v", err)
	}
	s := string(src)

	if !strings.Contains(s, "dismiss") {
		t.Fatalf("internal/dashboard/board.go has no dismiss handler; criteria 15-17 all live in it")
	}
	if !strings.Contains(s, "url.Values") {
		t.Errorf("board.go builds no url.Values. Criterion 17: the four known filter keys are re-encoded, " +
			"which is what makes the redirect target a value this code constructed rather than a string " +
			"a caller supplied")
	}
	if strings.Contains(s, "RawQuery") {
		t.Errorf("board.go mentions RawQuery. Criterion 17 forbids echoing r.URL.RawQuery into the " +
			"redirect — that is the safeNext lesson in internal/dashboard/auth.go, where a " +
			"caller-supplied redirect target had to be reduced to a known set before it could be trusted")
	}
	// The four keys by name, so a fifth filter added later fails LOUDLY here
	// rather than silently dropping out of the round trip.
	// EXTENDED — deliberately — by SWT-52 criterion 32 (D15): `refresh` is the
	// fifth key, so a Done or Dismiss made with auto-refresh on lands back on an
	// auto-refreshing board. The RawQuery ban above is unchanged.
	for _, k := range []string{`"project"`, `"status"`, `"assignee_type"`, `"subproject"`, `"refresh"`} {
		if !strings.Contains(s, k) {
			t.Errorf("board.go never names the filter key %s; the redirect rebuilds from the four known "+
				"keys and boardQuery reads the same four", k)
		}
	}
}

// Criterion 18 (SWT-31): "A dismissed task is gone from the default board with
// NO change to boardQuery." It was TestBoardQuery_ClosedStaysHiddenByDefault and
// asserted the Go literal "t.status <> 'closed'" verbatim.
//
// REWRITTEN — not deleted — by SWT-52 (board-status-lights) criterion 16: the
// default board now keeps tasks closed since local midnight (America/New_York)
// unless they carry an OPEN dismissal (D5). SWT-31's half survives inside the
// new predicate: `t.status <> 'closed'` is still the first disjunct, and an open
// dismissal still hides the row at once (NOT EXISTS … reopened_at IS NULL).
// Scanned in boardQuery's BODY on purpose: the SPEC forbids hiding the predicate
// in a const to keep an old test green.
func TestBoardQuery_DefaultShowsTodaysDoneUntilLocalMidnight(t *testing.T) {
	body := funcBodySrc(t, "board.go", "boardQuery")
	if body == "" {
		t.Fatalf("board.go declares no boardQuery")
	}
	flat := regexp.MustCompile(`\s+`).ReplaceAllString(body, " ")
	for _, want := range []struct{ re, why string }{
		{`\(\s*t\.status <> 'closed' OR `, "`t.status <> 'closed'` is the FIRST disjunct: every open task still shows (SWT-31 criterion 18)"},
		{`COALESCE\(t\.closed_at, t\.updated_at\)`, "the close instant, with the documented 0030 fallback (D5)"},
		{`boardDayStart\(`, "the ONE spelling of local midnight (criterion 10)"},
		{`task_dismissals`, "an open dismissal hides a closed row at once (D5)"},
		{`reopened_at IS NULL`, "…an OPEN one: a reopened dismissal is no longer a verdict (the SWT-36 spelling)"},
		{`"t\.status = \$%d"`, "the status branch is byte-unchanged (criterion 11)"},
	} {
		if !regexp.MustCompile(want.re).MatchString(flat) {
			t.Errorf("boardQuery does not match /%s/ — %s", want.re, want.why)
		}
	}
	if BoardTimeZone != "America/New_York" {
		t.Errorf("BoardTimeZone = %q, want America/New_York (D5: his day, not the pod's UTC and not AVAIL_TZ)", BoardTimeZone)
	}
}

// ---- SWT-51 (docs/tickets/board-done-button_SPEC.md): the Done verb -----------
//
// Criteria 1-5 and the handler slice of criterion 6. ZERO I/O beyond this
// package's own source and embedded templates. Criterion 4 (exactly one
// onchange) is NOT restated here: TestBoardTemplate_ProjectFilterAutoSubmits
// above already counts it, and the Done form must keep it green.
//
// GREENFIELD NOTE — EXPECTED RED: tasks.html has no /close form and no
// human-only conditional, server.go registers no close route, board.go has no
// closeTaskAction, and dismissTaskAction still carries its own copy of the
// four-key filter loop (D5).
//
// MUTATIONS THAT MUST TURN THIS SECTION RED:
//   - drop the {{if eq .AssigneeType "human"}} conditional → DoneFormIsHumanOnly.
//   - sweep the Dismiss form into that conditional → DoneFormIsHumanOnly.
//   - one form with two submit buttons (D4) → DoneFormIsItsOwnPlainPost.
//   - the handler reads s.pool, or calls task_dismiss, or executeTo → CloseTaskActionRunsNoSQL.
//   - copy-paste the filter loop into closeTaskAction → ShareOneFilterRebuild.

// doneFormRE captures the Done form's opening tag and its body.
var doneFormRE = regexp.MustCompile(`(?s)(<form[^>]*action="/tasks/\{\{\.ID\}\}/close"[^>]*>)(.*?)</form>`)

// dismissFormRE captures the Dismiss form's body.
var dismissFormRE = regexp.MustCompile(`(?s)<form[^>]*action="/tasks/\{\{\.ID\}\}/dismiss"[^>]*>(.*?)</form>`)

// Criterion 1 + D4: a SEPARATE inline POST form to /tasks/{{.ID}}/close with the
// four hidden filter inputs (valued from $.Filters, exactly as the Dismiss form
// does), a `note` text input and a Done button, and NO select and NO onchange —
// the Dismiss reason_code must never ride along with a Done submit.
func TestBoardTemplate_DoneFormIsItsOwnPlainPost(t *testing.T) {
	raw, err := templateFS.ReadFile("templates/tasks.html")
	if err != nil {
		t.Fatalf("read embedded tasks.html: %v", err)
	}
	s := string(raw)

	forms := doneFormRE.FindAllStringSubmatch(s, -1)
	if len(forms) != 1 {
		t.Fatalf(`tasks.html has %d form(s) posting to action="/tasks/{{.ID}}/close", want exactly 1. `+
			`Criterion 1: after the Dismiss form, each row carries <form class="inline" method="post" `+
			`action="/tasks/{{.ID}}/close"> with the four hidden filters, a note input and <button>Done</button>`,
			len(forms))
	}
	tag, inner := forms[0][1], forms[0][2]
	for _, want := range []string{`method="post"`, `class="inline"`} {
		if !strings.Contains(tag, want) {
			t.Errorf("the Done form tag %s lacks %s (criterion 1: an inline plain POST, no HTMX — SWT-31 D8)", tag, want)
		}
	}
	// EXTENDED — deliberately — by SWT-52 criterion 32 (D15): the fifth hidden
	// input, refresh.
	for _, k := range []string{"project", "status", "assignee_type", "subproject", "refresh"} {
		frag := `name="` + k + `" value="{{index $.Filters "` + k + `"}}"`
		if !strings.Contains(inner, frag) || !strings.Contains(inner, `type="hidden"`) {
			t.Errorf("the Done form lacks the hidden filter input %s. Criterion 1 + D5: the redirect lands on the "+
				"same filtered board only if the form posts the four filters back, exactly as the Dismiss form does",
				frag)
		}
	}
	if !strings.Contains(inner, `name="note"`) {
		t.Errorf("the Done form has no `note` input (criterion 1: <input type=\"text\" name=\"note\" " +
			"placeholder=\"note (optional)\">); D1 folds it into task_close's reason")
	}
	if !strings.Contains(inner, `<button>Done</button>`) {
		t.Errorf("the Done form has no <button>Done</button> (criterion 1)")
	}
	for _, banned := range []string{"<select", "reason_code", "onchange", "/dismiss"} {
		if strings.Contains(inner, banned) {
			t.Errorf("the Done form contains %q. D4: two forms, not one with two buttons — the Dismiss "+
				"reason_code select must never ride along with a Done submit, and nothing on the Done form "+
				"submits itself. Form body:\n%s", banned, inner)
		}
	}

	// The other direction of D4: the Dismiss form did not grow a Done button.
	dm := dismissFormRE.FindStringSubmatch(s)
	if dm == nil {
		t.Fatalf("tasks.html lost its Dismiss form (action=\"/tasks/{{.ID}}/dismiss\"); SWT-31 D6 keeps it on every row")
	}
	if strings.Contains(dm[1], "Done") || strings.Contains(dm[1], "/close") {
		t.Errorf("the Dismiss form carries a Done control. D4: a Done must never be sent to /dismiss. Form body:\n%s", dm[1])
	}
	// SWT-52 criterion 32 (D15): the Dismiss form carries the same five hidden
	// inputs, refresh included.
	for _, k := range []string{"project", "status", "assignee_type", "subproject", "refresh"} {
		frag := `name="` + k + `" value="{{index $.Filters "` + k + `"}}"`
		if !strings.Contains(dm[1], frag) {
			t.Errorf("the Dismiss form lacks the hidden input %s (criterion 32: five keys)", frag)
		}
	}
}

// Criteria 2 and 3 (D2, D3): the Done form, and ONLY the Done form, sits inside
// {{if eq .AssigneeType "human"}} … {{end}}; the Dismiss form stays
// unconditional. And there is no status conditional anywhere in tasks.html —
// SWT-31's two banned literals generalized to both verbs: one spelling of the
// refusable statuses, in closeTransition, and a visible refusal beats a hidden
// button that drifts from it.
func TestBoardTemplate_DoneFormIsHumanOnlyAndStatusBlind(t *testing.T) {
	raw, err := templateFS.ReadFile("templates/tasks.html")
	if err != nil {
		t.Fatalf("read embedded tasks.html: %v", err)
	}
	s := string(raw)

	const cond = `{{if eq .AssigneeType "human"}}`
	if n := strings.Count(s, cond); n != 1 {
		t.Fatalf("tasks.html contains %s %d time(s), want exactly 1. Criterion 2 / D2: the Done button renders only "+
			"on human rows — task_close accepts pr_open/awaiting_* where a worker's claim is still live, so a "+
			"Done on a claude row would close work out from under its CI/PR loop and strand the claim", cond, n)
	}
	block, ok := templateBlockAfter(s, cond)
	if !ok {
		t.Fatalf("could not find the {{end}} closing %s in tasks.html", cond)
	}
	if !strings.Contains(block, "/close") {
		t.Errorf("the human-only conditional does not contain the Done form's /close action (criterion 2). Block:\n%s", block)
	}
	if strings.Contains(block, "/dismiss") {
		t.Errorf("the human-only conditional contains the Dismiss form. Criterion 2 / SWT-31 D6: Dismiss stays "+
			"unconditional and renders on EVERY row, claude rows included. Block:\n%s", block)
	}
	// And the Done form is not ALSO rendered outside the conditional.
	outside := strings.Replace(s, block, "", 1)
	if strings.Contains(outside, "/close") {
		t.Errorf("tasks.html has a /close action outside the human-only conditional; D2 renders Done on human rows only")
	}

	if strings.Contains(s, "eq .Status") {
		t.Errorf("tasks.html contains `eq .Status`. Criterion 3 / D3: neither verb is conditioned on a status — " +
			"the refusable set is spelled once, in closeTransition, and the refusal surfaces as the flash")
	}
}

// templateBlockAfter returns the text between the first occurrence of open and
// the {{end}} that closes it, honouring nested if/range/with/block/define.
func templateBlockAfter(s, open string) (string, bool) {
	i := strings.Index(s, open)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(open):]
	depth, pos := 1, 0
	for {
		j := strings.Index(rest[pos:], "{{")
		if j < 0 {
			return "", false
		}
		j += pos
		k := strings.Index(rest[j:], "}}")
		if k < 0 {
			return "", false
		}
		action := strings.TrimSpace(strings.Trim(strings.TrimSpace(rest[j+2:j+k]), "-"))
		word := ""
		if f := strings.Fields(action); len(f) > 0 {
			word = f[0]
		}
		switch word {
		case "if", "range", "with", "block", "define":
			depth++
		case "end":
			depth--
			if depth == 0 {
				return rest[:j], true
			}
		}
		pos = j + k + 2
	}
}

// Criterion 5: the route is on the auth-required mux, beside the dismiss route,
// and dispatches to closeTaskAction.
func TestBoardServer_CloseRouteIsRegistered(t *testing.T) {
	raw, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	route := regexp.MustCompile(`mux\.Handle\("POST /tasks/\{id\}/close",\s*s\.auth\.Require\(\s*http\.HandlerFunc\(\s*s\.closeTaskAction\s*\)\s*\)\s*\)`)
	if !route.MatchString(string(raw)) {
		t.Errorf(`server.go registers no mux.Handle("POST /tasks/{id}/close", s.auth.Require(http.HandlerFunc(s.closeTaskAction))). ` +
			`Criterion 5: the Done route lives on the auth-required mux beside POST /tasks/{id}/dismiss`)
	}
}

// funcBodySrc returns the source text of the named top-level function's body
// in file, or "" when the file declares no such function.
func funcBodySrc(t *testing.T, file, name string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, b, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name || fn.Body == nil {
			continue
		}
		return string(b[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset])
	}
	return ""
}

// Criterion 6 + invariant 3: closeTaskAction makes exactly ONE executeTask call
// with task_close, builds args with json.Marshal, and runs no SQL of its own —
// no s.pool, no pre-check, no task_dismiss (Done writes no label), no RawQuery.
// executeTo is banned by name: it sets no Call.TaskID, which is criterion 10's
// NULL audit_events.task_id.
func TestBoardHandler_CloseTaskActionRunsNoSQL(t *testing.T) {
	body := funcBodySrc(t, "board.go", "closeTaskAction")
	if body == "" {
		t.Fatalf("board.go declares no closeTaskAction. Criterion 6: POST /tasks/{id}/close is handled by " +
			"(*Server).closeTaskAction in internal/dashboard/board.go, the sibling of dismissTaskAction")
	}
	for _, want := range []string{`"task_close"`, "executeTask(", "json.Marshal"} {
		if !strings.Contains(body, want) {
			t.Errorf("closeTaskAction does not contain %s. Criterion 6: args by json.Marshal of exactly "+
				"{task_id, reason}, one s.executeTask(w, r, \"task_close\", args, id, back) call", want)
		}
	}
	if n := strings.Count(body, "executeTask("); n > 1 {
		t.Errorf("closeTaskAction calls executeTask %d times, want exactly one (criterion 6)", n)
	}
	for _, banned := range []struct{ frag, why string }{
		{"s.pool", "invariant 3: the handler performs no SQL of its own — D2 rejects a dashboard-side assignee pre-check as a side door"},
		{`"task_dismiss"`, "Done writes NO task_dismissals row (criterion 12): a finished task is not a labelled negative"},
		{"RawQuery", "criterion 8: the redirect is rebuilt from the four filter keys, never an echo of r.URL.RawQuery"},
		{"executeTo(", "criterion 10: executeTo sets no Call.TaskID, so audit_events.task_id would be NULL"},
	} {
		if strings.Contains(body, banned.frag) {
			t.Errorf("closeTaskAction contains %s — %s", banned.frag, banned.why)
		}
	}
}

// D5: one spelling of the filter rebuild. The four-key url.Values loop moves out
// of dismissTaskAction into one unexported helper in board.go that BOTH handlers
// call; a second copy-pasted loop is how a fifth filter ends up surviving one
// verb's redirect but not the other's. Proxy: neither handler body names a
// filter key itself.
func TestBoardHandlers_ShareOneFilterRebuild(t *testing.T) {
	for _, fn := range []string{"dismissTaskAction", "closeTaskAction"} {
		body := funcBodySrc(t, "board.go", fn)
		if body == "" {
			t.Errorf("board.go declares no %s", fn)
			continue
		}
		for _, k := range []string{`"project"`, `"assignee_type"`, `"subproject"`} {
			if strings.Contains(body, k) {
				t.Errorf("%s names the filter key %s itself. D5: the four-key rebuild lives in ONE helper in "+
					"board.go (e.g. boardBack(r) url.Values) called by both the Dismiss and the Done handler", fn, k)
			}
		}
	}
}
