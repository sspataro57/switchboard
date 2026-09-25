package dashboard

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
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
