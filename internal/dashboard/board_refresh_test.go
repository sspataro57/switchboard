package dashboard

// SWT-52 (docs/tickets/board-status-lights_SPEC.md) D15, criteria 30, 31 and 32
// (the unit and structure halves) and 34's structure check: the opt-in board
// auto-refresh. ZERO I/O beyond this package's own source and templates.
//
// IMPOSED SURFACE (SPEC criterion 30; the helper's NAME and SIGNATURE are this
// file's choice — the SPEC asks for "ONE unexported pure helper"):
//
//	// board.go
//	const boardRefreshInterval = 5 * time.Second
//	var boardKeys = []string{"project", "status", "assignee_type", "subproject", "refresh"}
//	type boardData struct{ …; AutoRefresh bool; RefreshSeconds int;
//	                       RefreshToggleURL, ReloadURL, RenderedAt string }
//	// boardRefreshURLs re-encodes the non-empty boardKeys values of q via
//	// url.Values: toggle flips refresh (on → absent, anything else → on),
//	// reload keeps refresh=on; both are "/tasks" + "?" + encoded (no "?" when
//	// empty), omit flash and every key outside boardKeys.
//	func boardRefreshURLs(q url.Values) (toggle, reload string)
//
// Criterion 33 (the script's runtime behaviour) is verified by the manual
// browser smoke (Verification 4b); the structure test below is its regression
// guard.
//
// GREENFIELD NOTE — EXPECTED RED: boardRefreshInterval, boardKeys, the boardData
// fields and boardRefreshURLs do not exist, so the package's test binary
// compile-FAILS.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

var _ = boardData{AutoRefresh: true, RefreshSeconds: 5, RefreshToggleURL: "/tasks", ReloadURL: "/tasks?refresh=on", RenderedAt: "10:05:00"}

func TestBoardRefresh_IntervalAndKeys(t *testing.T) {
	if boardRefreshInterval != 5*time.Second {
		t.Errorf("boardRefreshInterval = %v, want 5s (D15: one const, never a URL value)", boardRefreshInterval)
	}
	if strings.Join(boardKeys, ",") != "project,status,assignee_type,subproject,refresh" {
		t.Errorf("boardKeys = %v, want [project status assignee_type subproject refresh] (criterion 30)", boardKeys)
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

// Criterion 30's table.
func TestBoardRefreshURLs(t *testing.T) {
	q := func(raw string) url.Values {
		v, err := url.ParseQuery(raw)
		if err != nil {
			t.Fatalf("ParseQuery(%q): %v", raw, err)
		}
		return v
	}
	filters := "project=saka&status=ready&assignee_type=human&subproject=web"

	t.Run("filters plus refresh=on round-trip", func(t *testing.T) {
		toggle, reload := boardRefreshURLs(q(filters + "&refresh=on"))
		tu, ru := mustParse(t, toggle), mustParse(t, reload)
		if tu.Path != "/tasks" || ru.Path != "/tasks" {
			t.Errorf("toggle %q / reload %q do not point at /tasks", toggle, reload)
		}
		for _, k := range []string{"project", "status", "assignee_type", "subproject"} {
			want := q(filters).Get(k)
			if tu.Query().Get(k) != want || ru.Query().Get(k) != want {
				t.Errorf("%s: toggle %q, reload %q; want %q in both", k, tu.Query().Get(k), ru.Query().Get(k), want)
			}
		}
		if _, on := tu.Query()["refresh"]; on {
			t.Errorf("toggle %q keeps refresh; with refresh=on the toggle turns it OFF (the key absent)", toggle)
		}
		if ru.Query().Get("refresh") != "on" {
			t.Errorf("reload %q lacks refresh=on", reload)
		}
	})
	for _, v := range []string{"5", "1", "off", "ON", "0.1"} {
		v := v
		t.Run("refresh="+v+" means off", func(t *testing.T) {
			toggle, _ := boardRefreshURLs(q("project=saka&refresh=" + v))
			tu := mustParse(t, toggle)
			if tu.Query().Get("refresh") != "on" || tu.Query().Get("project") != "saka" {
				t.Errorf("toggle for refresh=%s = %q, want project=saka&refresh=on: only `on` counts (D15)", v, toggle)
			}
			if strings.Contains(toggle, "refresh="+v) && v != "on" {
				t.Errorf("toggle %q echoes the caller's refresh value", toggle)
			}
		})
	}
	t.Run("flash and foreign keys never carried", func(t *testing.T) {
		toggle, reload := boardRefreshURLs(q(filters + "&refresh=on&flash=task_close+ok&next=%2F%2Fevil.example&x=1"))
		for _, u := range []string{toggle, reload} {
			for _, banned := range []string{"flash", "next", "evil", "x=1"} {
				if strings.Contains(u, banned) {
					t.Errorf("%q carries %q: both URLs hold boardKeys only, and never flash (criterion 30, 35)", u, banned)
				}
			}
		}
	})
	t.Run("empty values are omitted", func(t *testing.T) {
		toggle, _ := boardRefreshURLs(q("project=&status=&refresh="))
		if toggle != "/tasks?refresh=on" {
			t.Errorf("toggle for empty filters = %q, want /tasks?refresh=on", toggle)
		}
	})
	t.Run("empty query", func(t *testing.T) {
		toggle, _ := boardRefreshURLs(url.Values{})
		if toggle != "/tasks?refresh=on" {
			t.Errorf("toggle for an empty query = %q, want exactly /tasks?refresh=on (criterion 30)", toggle)
		}
	})
}

// Criterion 30: boardQuery reads no refresh key; the exports ignore it.
func TestBoardQuery_IgnoresRefresh(t *testing.T) {
	if body := funcBodySrc(t, "board.go", "boardQuery"); strings.Contains(body, "refresh") {
		t.Errorf("boardQuery's body mentions refresh; D15: refresh never reaches SQL or the exports")
	}
	q1, a1 := boardQuery(httptest.NewRequest("GET", "/tasks?project=saka", nil))
	q2, a2 := boardQuery(httptest.NewRequest("GET", "/tasks?project=saka&refresh=on", nil))
	if q1 != q2 || len(a1) != len(a2) {
		t.Errorf("boardQuery differs with refresh=on:\n%s %v\n%s %v", q1, a1, q2, a2)
	}
}

// Criterion 32: boardBack iterates boardKeys — five keys, nothing else.
func TestBoardBack_RebuildsFiveKeys(t *testing.T) {
	if body := funcBodySrc(t, "board.go", "boardBack"); !strings.Contains(body, "boardKeys") {
		t.Errorf("boardBack does not iterate boardKeys (criterion 32: one key list for the POST and GET sides)")
	}
	form := url.Values{"project": {"saka"}, "status": {"ready"}, "assignee_type": {"human"}, "subproject": {"web"},
		"refresh": {"on"}, "flash": {"old"}, "next": {"//evil.example"}, "note": {"n"}, "reason_code": {"duplicate"}}
	r := httptest.NewRequest("POST", "/tasks/1/close", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	back := boardBack(r)
	var keys []string
	for k := range back {
		keys = append(keys, k)
	}
	if len(back) != 5 || back.Get("refresh") != "on" || back.Get("project") != "saka" || back.Get("subproject") != "web" {
		t.Errorf("boardBack = %v (keys %v), want exactly the five boardKeys with refresh=on (criterion 32)", back, keys)
	}
	for _, banned := range []string{"flash", "next", "note", "reason_code"} {
		if _, ok := back[banned]; ok {
			t.Errorf("boardBack carries %q; it rebuilds boardKeys only", banned)
		}
	}
}

// Criteria 30 and 34: listTasks decides AutoRefresh from `== "on"`, takes the
// interval from the const, and runs no refresh-conditioned SQL.
func TestListTasks_RefreshIsARenderFlagNotAQuery(t *testing.T) {
	body := funcBodySrc(t, "board.go", "listTasks")
	if body == "" {
		t.Fatalf("board.go declares no listTasks")
	}
	if !regexp.MustCompile(`Get\("refresh"\)\s*==\s*"on"`).MatchString(body) {
		t.Errorf("listTasks does not set AutoRefresh from r.URL.Query().Get(\"refresh\") == \"on\" (criterion 30)")
	}
	if !strings.Contains(body, "boardRefreshInterval") || !strings.Contains(body, "RefreshSeconds") {
		t.Errorf("listTasks does not set RefreshSeconds from boardRefreshInterval (criterion 30)")
	}
	for _, banned := range []string{"strconv", "ParseDuration", "Atoi", "ParseFloat"} {
		if strings.Contains(body, banned) {
			t.Errorf("listTasks uses %s: the interval never comes from the request (D15: refresh=0.1 must not flood pg-main)", banned)
		}
	}
	// No SQL inside a branch conditioned on the refresh flag.
	fset := token.NewFileSet()
	src, err := os.ReadFile("board.go")
	if err != nil {
		t.Fatalf("read board.go: %v", err)
	}
	f, err := parser.ParseFile(fset, "board.go", src, 0)
	if err != nil {
		t.Fatalf("parse board.go: %v", err)
	}
	text := func(n ast.Node) string {
		return string(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "listTasks" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			ifs, ok := n.(*ast.IfStmt)
			if !ok {
				return true
			}
			cond := strings.ToLower(text(ifs.Cond))
			if !strings.Contains(cond, "refresh") {
				return true
			}
			branch := text(ifs.Body)
			if ifs.Else != nil {
				branch += text(ifs.Else)
			}
			for _, q := range []string{"Query", "Exec", "s.pool", "boardLightFacts", "reopenMarkers"} {
				if strings.Contains(branch, q) {
					t.Errorf("listTasks runs %s inside a refresh-conditioned branch; criterion 34: a refresh render "+
						"issues no query beyond an ordinary render", q)
				}
			}
			return true
		})
	}
}

// ---- criterion 31: the template ----------------------------------------------

func TestTasksTemplate_AutoRefreshToggleIndicatorAndOneScript(t *testing.T) {
	s := tasksHTML(t)
	const cond = "{{if .AutoRefresh}}"
	if strings.Contains(s, "eq .AutoRefresh") || strings.Contains(s, "eq .Status") {
		t.Errorf("tasks.html compares .AutoRefresh or .Status with eq; the conditional is the bool itself (criterion 31)")
	}
	block, ok := templateBlockAfter(s, cond)
	if !ok {
		t.Fatalf("tasks.html has no %s … {{end}} block (criterion 31)", cond)
	}

	const toggle = `<a id="auto-refresh-toggle" href="{{.RefreshToggleURL}}">`
	if !strings.Contains(s, toggle) {
		t.Errorf("tasks.html lacks the toggle %s (criterion 31)", toggle)
	} else if strings.Contains(block, toggle) {
		t.Errorf("the toggle sits inside %s; it renders on EVERY board page, or auto-refresh could never be turned on", cond)
	}

	f := strings.Index(s, `<form class="filters"`)
	if f < 0 {
		t.Fatalf("tasks.html lost its filter form")
	}
	filterForm := s[f : f+strings.Index(s[f:], "</form>")]
	if !strings.Contains(filterForm, `<input type="hidden" name="refresh" value="{{index .Filters "refresh"}}">`) {
		t.Errorf("the GET filter form lacks the hidden refresh input; changing the project select would silently turn " +
			"auto-refresh off (criterion 31)")
	}

	const indicator = `<p id="auto-refresh" class="muted">auto-refresh on (every {{.RefreshSeconds}} s, last refreshed {{.RenderedAt}})</p>`
	if !strings.Contains(block, indicator) {
		t.Errorf("the %s block lacks the indicator %s (criterion 31)", cond, indicator)
	}
	if strings.Count(s, `id="auto-refresh"`) != 1 {
		t.Errorf("tasks.html has %d id=\"auto-refresh\" elements, want exactly the one indicator", strings.Count(s, `id="auto-refresh"`))
	}

	if n := strings.Count(s, "<script"); n != 1 {
		t.Fatalf("tasks.html contains %d <script, want exactly ONE (criteria 7, 31)", n)
	}
	if !strings.Contains(block, "<script") {
		t.Fatalf("the one <script lies outside the %s block; it must render only when auto-refresh is on", cond)
	}
	for _, attr := range []string{`data-reload="{{.ReloadURL}}"`, `data-interval="{{.RefreshSeconds}}"`} {
		if !strings.Contains(block, attr) {
			t.Errorf("the %s block lacks %s: the script reads the server-rendered URL and interval (criterion 31)", cond, attr)
		}
	}
	i := strings.Index(s, "<script")
	j := strings.Index(s[i:], "</script>")
	if j < 0 {
		t.Fatalf("the <script is never closed")
	}
	script := s[i : i+j]
	for _, want := range []string{"setTimeout", "location.replace(", "document.hidden", "activeElement", "defaultValue"} {
		if !strings.Contains(script, want) {
			t.Errorf("the refresh script lacks %q (criterion 31 / D15's rules)", want)
		}
	}
	if !strings.Contains(script, `addEventListener("visibilitychange"`) && !strings.Contains(script, `addEventListener('visibilitychange'`) {
		t.Errorf("the refresh script does not re-arm on visibilitychange via addEventListener (criterion 31)")
	}
	for _, banned := range []string{"fetch(", "XMLHttpRequest", "htmx", "location.search", "location.href", "innerHTML",
		"localStorage", "sessionStorage", "onchange"} {
		if strings.Contains(script, banned) {
			t.Errorf("the refresh script contains %q; D15: it builds no URL, fetches nothing, rewrites no DOM, stores "+
				"nothing (and the one-onchange count covers the script too)", banned)
		}
	}
	// Outside the project select, no inline event handler anywhere.
	handlers := regexp.MustCompile(`\son[a-z]+=`).FindAllString(s, -1)
	if len(handlers) != 1 || strings.TrimSpace(handlers[0]) != "onchange=" {
		t.Errorf("tasks.html has inline event-handler attributes %v, want only the project select's one onchange "+
			"(criterion 31)", handlers)
	}
	if n := strings.Count(s, "onchange"); n != 1 {
		t.Errorf("tasks.html has %d onchange, want exactly 1 (the project select)", n)
	}
}
