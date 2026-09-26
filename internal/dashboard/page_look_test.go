package dashboard

import (
	"crypto/sha256"
	"encoding/hex"
	"html/template"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

// swb 692 (Salvador, 2026-09-25): "this screen needs a back link" (/tasks/680)
// and "the looks and feel on all the screens should match the board style and
// branding".

func TestBoardBackURL_KeepsOnlyASameHostBoardView(t *testing.T) {
	for _, tc := range []struct{ referer, want string }{
		{"", "/tasks"},
		{"https://switchboard.sspataro.com/tasks?project=collaboratory&refresh=on", "/tasks?project=collaboratory&refresh=on"},
		{"https://switchboard.sspataro.com/tasks?project=foundry&next=https://evil.example", "/tasks?project=foundry"},
		{"https://SWITCHBOARD.sspataro.com/tasks?project=hub", "/tasks?project=hub"},
		{"https://switchboard.sspataro.com/tasks/679?project=hub", "/tasks"},
		{"https://switchboard.sspataro.com/deliveries?project=hub&status=rejected", "/tasks"},
		{"https://evil.example/tasks?project=x", "/tasks"},
		{"//evil.example/tasks?x=1", "/tasks"},
		{"::not a url", "/tasks"},
	} {
		r := httptest.NewRequest("GET", "https://switchboard.sspataro.com/tasks/680", nil)
		if tc.referer != "" {
			r.Header.Set("Referer", tc.referer)
		}
		if got := boardBackURL(r); got != tc.want {
			t.Errorf("Referer %q: back = %q, want %q", tc.referer, got, tc.want)
		}
	}
}

// Every page but the board and the kiosk wears the shared look: the stylesheet,
// the chrome bar around the nav, the sign, and the page panel. The task page
// also carries the Back link.
func TestSecondaryPages_WearTheBoardLook(t *testing.T) {
	for _, name := range []string{"task.html", "deliveries.html", "briefs.html", "plans.html", "plan.html",
		"sources.html", "funnel.html"} {
		raw, err := templateFS.ReadFile("templates/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src := string(raw)
		for _, want := range []string{`href="/static/swb-1.css"`, `<div class="headbar">`, `<header class="sign">`,
			`class="signblock"`, `<div class="page">`} {
			if !strings.Contains(src, want) {
				t.Errorf("templates/%s lacks %s", name, want)
			}
		}
		if strings.Contains(src, "font-family: system-ui") {
			t.Errorf("templates/%s still carries the old system-ui page style", name)
		}
	}
	task, err := templateFS.ReadFile("templates/task.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(task), `<a class="back" href="{{.BackURL}}">`) {
		t.Errorf("templates/task.html has no Back link to .BackURL")
	}
	if _, err := staticFS.ReadFile("static/swb-1.css"); err != nil {
		t.Errorf("static/swb-1.css is not embedded: %v", err)
	}
}

// /static/ is served immutable for a year, so a browser that fetched
// swb-1.css never fetches it again: an in-place edit would never arrive. A
// change ships under a NEW name (swb-2.css, and every template's link with it),
// and this pin moves to the new file's hash.
func TestSharedStylesheet_IsNeverEditedInPlace(t *testing.T) {
	raw, err := staticFS.ReadFile("static/swb-1.css")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != swb1CSSHash {
		t.Errorf("static/swb-1.css changed in place (sha256 %s). /static/ is immutable-cached: copy it to "+
			"swb-2.css, point every template at the new name, and pin the new hash here", got)
	}
}

// The pages must keep matching the board: every token the board declares in
// its :root block carries the same value in swb-1.css.
func TestSharedStylesheet_KeepsTheBoardsTokens(t *testing.T) {
	board, err := templateFS.ReadFile("templates/tasks.html")
	if err != nil {
		t.Fatal(err)
	}
	css, err := staticFS.ReadFile("static/swb-1.css")
	if err != nil {
		t.Fatal(err)
	}
	decl := regexp.MustCompile(`(--[a-z-]+):\s*([^;]+);`)
	root := regexp.MustCompile(`(?s):root\s*\{(.*?)\}`)
	tokens := func(src string) map[string]string {
		m := map[string]string{}
		if r := root.FindStringSubmatch(src); r != nil {
			for _, d := range decl.FindAllStringSubmatch(r[1], -1) {
				m[d[1]] = strings.TrimSpace(d[2])
			}
		}
		return m
	}
	bt, ct := tokens(string(board)), tokens(string(css))
	if len(bt) < 10 {
		t.Fatalf("POSITIVE CONTROL: found %d tokens in the board's :root", len(bt))
	}
	for k, v := range bt {
		if k == "--row" || k == "--grid" {
			continue // the board's own row geometry
		}
		if ct[k] != v {
			t.Errorf("token %s is %q on the board but %q in swb-1.css", k, v, ct[k])
		}
	}
}

const swb1CSSHash = "ca9b2ee2802dbd8b16797586079042a2a09c04aeaf27f96d828a79ad51894eae"

// swb 722: the task page carries the board's verbs, posting to the board's
// endpoints with the board view's filters as hidden fields. Done shows only on
// an open human task; Requeue only when the task needs review.
func TestTaskPage_CarriesTheBoardsVerbs(t *testing.T) {
	tmpl, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{tmpl: tmpl}
	render := func(d taskDetail) string {
		var b strings.Builder
		if err := srv.tmpl.ExecuteTemplate(&b, "task.html", d); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	d := taskDetail{taskRow: taskRow{ID: 707, Title: "t", Status: "ready", AssigneeType: "human"},
		BackURL: "/tasks?project=personal", BackFilters: map[string]string{"project": "personal"}}
	page := render(d)
	for _, want := range []string{`action="/tasks/707/close"`, `action="/tasks/707/dismiss"`, `action="/tasks/707/attach"`,
		`<input type="hidden" name="project" value="personal">`, `name="reason_code"`} {
		if !strings.Contains(page, want) {
			t.Errorf("task page lacks %s", want)
		}
	}
	if strings.Contains(page, `/tasks/707/requeue`) {
		t.Errorf("Requeue shown on a task that does not need review")
	}
	d.NeedsReview = true
	if !strings.Contains(render(d), `action="/tasks/707/requeue"`) {
		t.Errorf("Requeue missing on a task that needs review")
	}
	d.AssigneeType, d.NeedsReview = "claude", false
	if strings.Contains(render(d), `/tasks/707/close`) {
		t.Errorf("Done shown on a claude task (the board shows it on human rows only)")
	}
	d.AssigneeType, d.Status = "human", "closed"
	if strings.Contains(render(d), `/tasks/707/close`) {
		t.Errorf("Done shown on a closed task")
	}
}

func TestTaskPage_NeedsReviewMatchesTheBoard(t *testing.T) {
	src, err := os.ReadFile("board.go")
	if err != nil {
		t.Fatal(err)
	}
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	if !strings.Contains(norm(string(src)), norm(needsReviewSQL)+" AS needs_review") {
		t.Errorf("boardLightFacts' needs_review no longer reads %q: the task page's Requeue would drift from the board's",
			needsReviewSQL)
	}
}

// swb 722: the verbs' hidden fields are the Referer board view's boardKeys only,
// escaped. MUTATIONS: range the Referer's whole query -> flash leaks; drop the
// host check -> the foreign case fills.
func TestBoardBackValues_FeedsOnlyBoardKeysEscaped(t *testing.T) {
	r := httptest.NewRequest("GET", "https://switchboard.sspataro.com/tasks/707", nil)
	r.Header.Set("Referer", `https://switchboard.sspataro.com/tasks?project=a"><script>x&flash=hi&status=ready`)
	v := boardBackValues(r)
	if v.Get("flash") != "" || v.Get("status") != "ready" || v.Get("project") != `a"><script>x` {
		t.Errorf("boardBackValues = %v, want project and status only", v)
	}
	tmpl, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	d := taskDetail{taskRow: taskRow{ID: 707, Status: "ready", AssigneeType: "human"},
		BackFilters: map[string]string{"project": v.Get("project")}}
	if err := tmpl.ExecuteTemplate(&b, "task.html", d); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), `"><script>`) {
		t.Errorf("a hostile filter value reached the page unescaped")
	}
	r.Header.Set("Referer", "https://evil.example/tasks?project=x")
	if got := boardBackValues(r); len(got) != 0 {
		t.Errorf("a foreign Referer fed %v", got)
	}
}
