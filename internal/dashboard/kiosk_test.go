package dashboard

// board-departures (SWT-67), the B21 amendment: a full-page reload exits the
// browser's fullscreen, so FULL and auto-refresh cannot combine on /tasks
// itself. GET /kiosk is a shell that holds the board in an iframe: the SHELL
// goes fullscreen and the board reloads inside it.

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestKioskBoardURL(t *testing.T) {
	for _, tc := range []struct{ project, want string }{
		{"", "/tasks?refresh=on"},
		{"saka", "/tasks?project=saka&refresh=on"},
		{`a b&refresh=off"><script>`, "/tasks?project=a+b%26refresh%3Doff%22%3E%3Cscript%3E&refresh=on"},
	} {
		if got := kioskBoardURL(tc.project); got != tc.want {
			t.Errorf("kioskBoardURL(%q) = %q, want %q (the board inside the shell always refreshes; only the "+
				"project filter is carried, and it is query-escaped)", tc.project, got, tc.want)
		}
	}
	if got := kioskURL(""); got != "/kiosk" {
		t.Errorf("kioskURL(\"\") = %q, want /kiosk", got)
	}
	if got := kioskURL("saka"); got != "/kiosk?project=saka" {
		t.Errorf("kioskURL(saka) = %q, want /kiosk?project=saka", got)
	}
}

func TestKiosk_RendersTheShellAroundTheBoard(t *testing.T) {
	s, err := NewServer(nil, nil, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	rec := httptest.NewRecorder()
	s.showKiosk(rec, httptest.NewRequest(http.MethodGet, "/kiosk?project=saka&status=closed", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /kiosk = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	m := regexp.MustCompile(`<iframe[^>]*\ssrc="([^"]*)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("the shell holds no <iframe src=…>:\n%s", body)
	}
	u, err := url.Parse(strings.ReplaceAll(m[1], "&amp;", "&"))
	if err != nil || u.Path != "/tasks" || u.Query().Get("refresh") != "on" || u.Query().Get("project") != "saka" {
		t.Errorf("the iframe src = %q, want /tasks with refresh=on and project=saka", m[1])
	}
	if u != nil && u.Query().Has("status") {
		t.Errorf("the iframe src carries status; the shell carries the project filter only")
	}
}

// The render path escapes a hostile project too: the value reaches the iframe
// src and the leave link only as a percent-escaped query value behind the
// literal /tasks? prefix.
func TestKiosk_HostileProjectStaysAQueryValue(t *testing.T) {
	s, err := NewServer(nil, nil, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	hostile := `javascript:alert(1)" onload="x"><script>`
	rec := httptest.NewRecorder()
	s.showKiosk(rec, httptest.NewRequest(http.MethodGet, "/kiosk?project="+url.QueryEscape(hostile), nil))
	body := rec.Body.String()
	if strings.Contains(body, "<script>x") || strings.Contains(body, `onload="x"`) || strings.Contains(body, "ZgotmplZ") {
		t.Fatalf("the hostile project broke out of its attribute:\n%s", body)
	}
	for _, m := range regexp.MustCompile(`(?:src|href)="([^"]*)"`).FindAllStringSubmatch(body, -1) {
		if !strings.HasPrefix(m[1], "/") || strings.HasPrefix(m[1], "//") {
			t.Errorf("the shell renders %s: every URL is an in-app absolute path", m[0])
		}
	}
}

// staticCacheHeaders: a real embedded file is served immutable; a miss or a
// directory is a bare 404 that never reaches the file server and carries no
// cache header (an immutable 404 would be cached for a year; the open route
// lists nothing). Every response refuses third-party framing.
func TestStaticCacheHeaders(t *testing.T) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	reached := 0
	h := staticCacheHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached++ }), sub)
	do := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec
	}
	if rec := do(http.MethodGet, "/static/fonts/b612mono-400.woff2"); !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") || reached != 1 {
		t.Errorf("a real file: Cache-Control %q, next reached %d times; want immutable and 1", rec.Header().Get("Cache-Control"), reached)
	}
	for _, p := range []string{"/static/", "/static/fonts", "/static/fonts/", "/static/nope.woff2", "/static/../server.go"} {
		before := reached
		rec := do(http.MethodGet, p)
		if rec.Code != http.StatusNotFound || rec.Header().Get("Cache-Control") != "" || reached != before {
			t.Errorf("GET %s = %d, Cache-Control %q, next reached: %v; want a bare 404 that never reaches the file server",
				p, rec.Code, rec.Header().Get("Cache-Control"), reached != before)
		}
	}
	if rec := do(http.MethodPost, "/static/fonts/b612mono-400.woff2"); rec.Header().Get("Cache-Control") != "" {
		t.Errorf("POST to a real file carries Cache-Control %q; only GET and HEAD are cacheable", rec.Header().Get("Cache-Control"))
	}
	before := reached
	if rec := do(http.MethodGet, "/tasks"); reached != before+1 || rec.Header().Get("Cache-Control") != "" {
		t.Errorf("a non-static path must pass through untouched")
	}
	for _, p := range []string{"/tasks", "/kiosk", "/static/icon-192.png"} {
		if got := do(http.MethodGet, p).Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
			t.Errorf("GET %s X-Frame-Options = %q, want SAMEORIGIN (never DENY: /kiosk frames the board)", p, got)
		}
	}
}

func TestKioskTemplate_Contract(t *testing.T) {
	raw, err := templateFS.ReadFile("templates/kiosk.html")
	if err != nil {
		t.Fatalf("read embedded kiosk.html: %v", err)
	}
	s := string(raw)
	for _, want := range []string{
		`<iframe id="board" src="{{.BoardURL}}"`,
		`<button type="button" id="enter"`,
		"requestFullscreen",
		"fullscreenchange",
		`<link rel="manifest" href="/static/manifest.webmanifest">`,
		`<meta name="theme-color" content="#0b0b0c">`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("kiosk.html lacks %s", want)
		}
	}
	if n := strings.Count(s, "<script"); n != 1 {
		t.Errorf("kiosk.html has %d <script, want 1", n)
	}
	lower := strings.ToLower(s)
	for _, banned := range []string{"http://", "https://", "fetch(", "xmlhttprequest", "innerhtml", "localstorage",
		"sessionstorage", "serviceworker", "htmx"} {
		if strings.Contains(lower, banned) {
			t.Errorf("kiosk.html contains %q (the board's rules hold for its shell: no third-party URL, nothing stored, no fetch)", banned)
		}
	}
	if regexp.MustCompile(`\son[a-z]+=`).MatchString(s) {
		t.Errorf("kiosk.html carries an inline handler; bind with addEventListener")
	}
	if !regexp.MustCompile(`if\s*\(\s*navigator\.wakeLock`).MatchString(s) || !strings.Contains(s, ".catch(") {
		t.Errorf("the shell's wake lock is not guarded and swallowed (B18-3)")
	}
}

func TestKioskRoute_RequiresASession(t *testing.T) {
	src := readSrc(t, "server.go")
	if !regexp.MustCompile(`mux\.Handle\("GET /kiosk",\s*s\.auth\.Require\(`).MatchString(src) {
		t.Errorf("GET /kiosk is not registered with s.auth.Require")
	}
}

// The board's FULL button: inside the shell it toggles the SHELL's fullscreen;
// on a bare /tasks it opens the shell, because fullscreen on /tasks itself
// would end at the next reload.
func TestTasksTemplate_FullButtonOpensTheKioskShell(t *testing.T) {
	s := tasksHTML(t)
	if !strings.Contains(s, `id="fs" data-kiosk="{{.KioskURL}}"`) {
		t.Errorf(`the FULL button does not carry data-kiosk="{{.KioskURL}}"`)
	}
	i := strings.Index(s, "<script")
	script := s[i:]
	for _, want := range []string{"data-kiosk", "location.assign(", "window.top", "requestFullscreen"} {
		if !strings.Contains(script, want) {
			t.Errorf("the script lacks %q (FULL: toggle the shell's fullscreen when framed, else open the shell)", want)
		}
	}
	if !strings.Contains(funcBodySrc(t, "board.go", "listTasks"), "kioskURL(") {
		t.Errorf("listTasks does not set KioskURL from kioskURL(project)")
	}
}
