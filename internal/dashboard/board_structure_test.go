package dashboard

import (
	"os"
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
	for _, k := range []string{`"project"`, `"status"`, `"assignee_type"`, `"subproject"`} {
		if !strings.Contains(s, k) {
			t.Errorf("board.go never names the filter key %s; the redirect rebuilds from the four known "+
				"keys and boardQuery reads the same four", k)
		}
	}
}

// Criterion 18: "A dismissed task is gone from the default board with NO change
// to boardQuery." The characterization is one line and it is the entire claim —
// `t.status <> 'closed'` already hides it, and any edit to that predicate is a
// change to what the board means.
func TestBoardQuery_ClosedStaysHiddenByDefault(t *testing.T) {
	src, err := os.ReadFile("board.go")
	if err != nil {
		t.Fatalf("read internal/dashboard/board.go: %v", err)
	}
	if !strings.Contains(string(src), `"t.status <> 'closed'"`) {
		t.Errorf("boardQuery no longer defaults to `t.status <> 'closed'`. SWT-31 criterion 18 is that " +
			"the row disappears with NO change here: a dismissal is an ordinary close, and the board " +
			"lane stays a FILTER (?status=closed), never a table (invariant 2)")
	}
}
