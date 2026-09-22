package dashboard

// comms-inbox (SWT-74, docs/tickets/comms-inbox_SPEC.md) criteria 37, 38 and
// 39: the board's ONE new action — Attach — and the fact that NOTHING else on
// the board moves, because a comm task reaches INCOMING through SWT-72's own
// columns.
//
// ZERO I/O beyond this package's own source and the embedded tasks.html.
// board_activity_structure_test.go (SWT-72) is the neighbour; its helpers
// (funcBodySrc, tasksHTML, readSrc) are reused rather than respelled.
//
// ---- IMPOSED SURFACE (SPEC D7, criteria 37-38) --------------------------------
//
//	// internal/dashboard/server.go
//	mux.Handle("POST /tasks/{id}/attach", s.auth.Require(http.HandlerFunc(s.attachTaskAction)))
//	// internal/dashboard/board.go — ONE executeTask call with boardBack(r) and
//	// no SQL of its own (the requeueTaskAction shape):
//	func (s *Server) attachTaskAction(w http.ResponseWriter, r *http.Request)
//	// templates/tasks.html — EXACTLY ONE addition: an Attach form in the per-row
//	// `actions` popup with a numeric target_task_id input and an optional note,
//	// rendered for EVERY row.
//
// WHY EVERY ROW AND NOT {{if .NeedsReview}} (criterion 38): "a comm still needs
// routing after it has been reviewed, so gating it on NeedsReview would hide it
// exactly when he comes back to it."
//
// GREENFIELD NOTE, EXPECTED RED: the route, attachTaskAction and the form do
// not exist, so each guard fails on its own sentence.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - the handler writes SQL of its own -> TheAttachRouteIsOneExecutorCall.
//   - the form is gated on NeedsReview -> TheAttachFormIsOnEveryRow.
//   - the form sends anything but ids and a note -> the same.

import (
	"regexp"
	"strings"
	"testing"
)

// ---- criterion 37: the route ------------------------------------------------------

func TestServer_RegistersTheAttachRoute(t *testing.T) {
	src := readSrc(t, "server.go")
	re := regexp.MustCompile(`mux\.Handle\(\s*"POST /tasks/\{id\}/attach"\s*,\s*s\.auth\.Require\(`)
	if !re.MatchString(src) {
		t.Errorf("server.go does not register POST /tasks/{id}/attach behind auth.Require, beside dismiss, " +
			"close and requeue (criterion 37)")
	}
	if !strings.Contains(src, "attachTaskAction") {
		t.Errorf("server.go does not route to attachTaskAction (criterion 37)")
	}
}

func TestBoard_TheAttachRouteIsOneExecutorCall(t *testing.T) {
	body := funcBodySrc(t, "board.go", "attachTaskAction")
	if body == "" {
		t.Fatalf("board.go declares no attachTaskAction. Criterion 37: POST /tasks/{id}/attach -> ONE " +
			"executeTask call with boardBack(r) and no SQL of its own (invariant 3)")
	}
	if !strings.Contains(body, `"task_attach"`) {
		t.Errorf("attachTaskAction does not call the task_attach tool (invariant 3: every board verb is one " +
			"executor call — validate, policy, audit start, handler, audit complete)")
	}
	if n := strings.Count(body, "executeTask("); n != 1 {
		t.Errorf("attachTaskAction makes %d executeTask call(s), want exactly 1 (criterion 37)", n)
	}
	if !strings.Contains(body, "boardBack(r)") {
		t.Errorf("attachTaskAction does not redirect via boardBack(r); every board verb uses the ONE spelling " +
			"of the redirect keys, so an Attach from a filtered, auto-refreshing board lands back on it")
	}
	for _, banned := range []struct{ tok, why string }{
		{"s.pool", "invariant 3: no SQL of its own"},
		{"Query(", "invariant 3: no SQL of its own"},
		{"Exec(", "invariant 3: no SQL of its own"},
		{"closeTransition", "the CLOSE belongs to the tool, under its own transaction and row locks"},
		{"activity_at", "criterion 34: the target is not surfaced, and the dashboard knows nothing about it"},
	} {
		if strings.Contains(body, banned.tok) {
			t.Errorf("attachTaskAction contains %q — %s", banned.tok, banned.why)
		}
	}
	// The target id is parsed as a NUMBER before the call: the form's input is
	// typed, and a non-numeric paste must be a 400, not a tool error.
	if !strings.Contains(body, "target_task_id") {
		t.Errorf("attachTaskAction never names target_task_id; the form posts it (criterion 38)")
	}
	if !regexp.MustCompile(`ParseInt|Atoi`).MatchString(body) {
		t.Errorf("attachTaskAction does not parse the posted target as a number (criterion 38: a numeric " +
			"target_task_id input; the dismiss/close/requeue handlers parse the path id the same way)")
	}
}

// ---- criterion 38: exactly one template addition ------------------------------------

func TestTasksTemplate_TheAttachFormIsOnEveryRow(t *testing.T) {
	s := tasksHTML(t)

	if !strings.Contains(s, `action="/tasks/{{.ID}}/attach"`) {
		t.Fatalf("tasks.html posts no /tasks/{id}/attach (criteria 37, 38): the Attach form lives in the " +
			"per-row `actions` popup, beside Dismiss, Done and Requeue")
	}
	i := strings.Index(s, `action="/tasks/{{.ID}}/attach"`)
	form := s[i:]
	if j := strings.Index(form, "</form>"); j > 0 {
		form = form[:j]
	}

	// EVERY row: NOT gated on NeedsReview (a comm still needs routing after he
	// has reviewed it, which is exactly when he comes back to it).
	gated := regexp.MustCompile(`(?s)\{\{if \.NeedsReview\}\}\s*(<[^>]*>\s*)*<form[^>]*action="/tasks/\{\{\.ID\}\}/attach"`)
	if gated.MatchString(s) {
		t.Errorf("the Attach form is inside {{if .NeedsReview}}. Criterion 38: it is rendered for EVERY row — " +
			"\"a comm still needs routing after it has been reviewed, so gating it on NeedsReview would hide " +
			"it exactly when he comes back to it\"")
	}

	// A NUMERIC target input and an optional note, and the five board keys so
	// boardBack rebuilds the redirect.
	if !regexp.MustCompile(`<input[^>]*type="number"[^>]*name="target_task_id"`).MatchString(form) &&
		!regexp.MustCompile(`<input[^>]*name="target_task_id"[^>]*type="number"`).MatchString(form) {
		t.Errorf("the Attach form has no `<input type=\"number\" name=\"target_task_id\">`. Criterion 38: a "+
			"NUMERIC input — there is deliberately no proposal picker or prefill (out of scope; that is "+
			"Future work once the matcher proves itself):\n%s", form)
	}
	if !strings.Contains(form, `name="note"`) {
		t.Errorf("the Attach form has no optional note input (criterion 38); the note rides the SOURCE's close "+
			"reason and its own event, never the target (D7):\n%s", form)
	}
	for _, k := range []string{"project", "status", "assignee_type", "subproject", "refresh"} {
		if !strings.Contains(form, `name="`+k+`"`) {
			t.Errorf("the Attach form has no hidden %q input; boardBack rebuilds the redirect from the five "+
				"board keys (criterion 38)", k)
		}
	}
	if !regexp.MustCompile(`<button[^>]*>\s*Attach`).MatchString(form) {
		t.Errorf("the Attach form has no `Attach` button (criterion 38):\n%s", form)
	}

	// Criterion 38's guards, unchanged from SWT-52/SWT-59/SWT-72. The template
	// gains EXACTLY ONE addition.
	if strings.Contains(strings.ToLower(s), "incoming") {
		t.Errorf("tasks.html mentions incoming; the section comes from data (SWT-59 criterion 14), and a comm " +
			"row reaches it through SWT-72's columns alone (D5)")
	}
	if n := strings.Count(s, "<script"); n != 1 {
		t.Errorf("tasks.html has %d <script tags, want exactly 1 (criterion 38)", n)
	}
	if strings.Contains(s, "htmx") || strings.Contains(s, "hx-") {
		t.Errorf("tasks.html uses HTMX; the board has none (pinned)")
	}
	if strings.Contains(s, "task_match") || strings.Contains(s, "proposal") {
		t.Errorf("tasks.html offers a match PROPOSAL. Out of scope (D6): \"a proposal PICKER or prefill in the " +
			"dashboard's attach form\" is Future work — the board calls one verb, with an id he types")
	}
}

// ---- criterion 39: the read path is BYTE-UNCHANGED -------------------------------------

// A comm task reaches INCOMING through SWT-72's own columns, so this ticket
// touches no board READ. The criterion's own words: "the existing SWT-59/SWT-72
// structure tests must pass unamended, which IS the criterion" — this guard
// states the same thing in the form that fails loudly if someone starts
// teaching the board what a comm is.
func TestBoard_LearnsNothingAboutComms(t *testing.T) {
	for _, file := range []string{"board.go", "lights.go", "sections.go", "display.go", "export.go"} {
		src := readSrc(t, file)
		for _, banned := range []struct{ tok, why string }{
			{"comm_task", "criterion 39 / D5: the board is UNTOUCHED — needs_review, activity_channel, " +
				"activity_sender and activity_stamp already do the work, and a comm row is an activity row"},
			{"comm_task_id", "criterion 39: capture's own log is not a board read"},
			{"capture_decisions", "criterion 39: the board never joins capture's log"},
			{`"comm"`, "D5 (unilateral): NO new incoming kind for \"comm\" versus \"activity on a ticket task\" " +
				"— the two look identical on purpose, both are \"someone said something, look\". " +
				"Distinguishing them needs a new stored fact and changes nothing he does"},
		} {
			// board.go legitimately gains the ACTION handler; the ban is on the
			// board's READ vocabulary, and the handler names the TOOL
			// (`task_attach`), never a column of capture's log.
			if strings.Contains(src, banned.tok) {
				t.Errorf("internal/dashboard/%s names %s — %s", file, banned.tok, banned.why)
			}
		}
	}
}
