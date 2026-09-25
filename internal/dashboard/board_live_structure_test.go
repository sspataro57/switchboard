package dashboard

// board-streaming (SWT-89, docs/tickets/board-streaming_SPEC.md): the
// STRUCTURE halves — criterion 1 (the migration file's shape), 9 (live.go is
// read-only), 10 (the route), 12 (the handler runs no SQL and reads no query),
// 14 (cmd/dashboard/main.go), 15 (boardData), 17-20 (the template and its one
// script). ZERO I/O beyond the repo's own source, the embedded templates and
// migrations/0044_*.sql.
//
// This file names NO new Go symbol, so it compiles before live.go exists and
// each test fails on its own: a missing file, a missing route, missing markup.
// The criteria that need the new symbols (4, 6, 7, 11, 16) are live_test.go.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - the WHEN (OLD.* IS DISTINCT FROM NEW.*) clause dropped -> Migration0044_Shape (and integration 3b).
//   - a trigger on normalized_messages -> Migration0044_Shape.
//   - the handler runs s.pool.QueryRow(...) -> BoardStream_RunsNoSQLAndReadsNoQuery.
//   - the route registered without s.auth.Require -> StreamRouteIsAuthenticatedAndBeatsTheIDRoute.
//   - WriteTimeout: 30 * time.Second added to cmd/dashboard/main.go -> DashboardMain_WiresTheHub.
//   - the swap written with innerHTML / insertAdjacentHTML / createContextualFragment -> LiveScriptContract.
//   - data-live on <body> or on a wrapper holding the <script> -> FiveLiveRegions.
//   - data-live="alert" placed inside {{with .OrchAlert}} -> FiveLiveRegions (depth).
//   - busy() loses details[open] -> LiveScriptContract.
//   - the indicator gains a child <span> before the time -> IndicatorWordsAndDownNote.

import (
	"go/scanner"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ---- helpers ----------------------------------------------------------------------

// goCode is src's token stream with every comment dropped, tokens joined with
// no separator — enough for substring checks like "ex.Execute(" that must not be
// satisfied or tripped by prose. String literals keep their quotes.
func goCode(t *testing.T, file string, src []byte) string {
	t.Helper()
	fset := token.NewFileSet()
	f := fset.AddFile(file, fset.Base(), len(src))
	var s scanner.Scanner
	s.Init(f, src, nil, 0)
	var b strings.Builder
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if lit != "" {
			b.WriteString(lit)
		} else {
			b.WriteString(tok.String())
		}
		b.WriteString(" ")
	}
	return b.String()
}

// goStringLits returns every string literal in src, unquoted.
func goStringLits(t *testing.T, file string, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	f := fset.AddFile(file, fset.Base(), len(src))
	var s scanner.Scanner
	s.Init(f, src, nil, 0)
	var out []string
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			return out
		}
		if tok == token.STRING {
			if len(lit) >= 2 {
				out = append(out, lit[1:len(lit)-1])
			}
		}
	}
}

// liveScript is the one <script>…</script> of tasks.html and its opening tag.
func liveScript(t *testing.T, s string) (tag, script string) {
	t.Helper()
	i := strings.Index(s, "<script")
	if i < 0 {
		t.Fatalf("tasks.html has no <script")
	}
	j := strings.Index(s[i:], "</script>")
	if j < 0 {
		t.Fatalf("the <script is never closed")
	}
	return s[i : i+strings.Index(s[i:], ">")+1], s[i : i+j]
}

// liveRegion finds the element carrying data-live="name": its tag name, its
// opening tag, and its [start, end) span.
func liveRegion(s, name string) (tag, open string, start, end int, ok bool) {
	attr := `data-live="` + name + `"`
	i := strings.Index(s, attr)
	if i < 0 {
		return "", "", 0, 0, false
	}
	start = strings.LastIndex(s[:i], "<")
	if start < 0 {
		return "", "", 0, 0, false
	}
	m := regexp.MustCompile(`^<([a-zA-Z0-9]+)`).FindStringSubmatch(s[start:])
	if m == nil {
		return "", "", 0, 0, false
	}
	tag = m[1]
	open = s[start : start+strings.Index(s[start:], ">")+1]
	end, ok = elementEnd(s, start, tag)
	return tag, open, start, end, ok
}

var liveNames = []string{"alert", "tally", "main", "counts", "indicator"}

// createTriggerRE matches one CREATE TRIGGER statement: name, timing/events,
// table, the rest (FOR EACH …, WHEN …), and the function it executes.
var createTriggerRE = regexp.MustCompile(`(?is)CREATE\s+(?:OR\s+REPLACE\s+)?TRIGGER\s+(\w+)\s+(.*?)\s+ON\s+(\w+)\s+(.*?)EXECUTE\s+(?:FUNCTION|PROCEDURE)\s+(\w+)\s*\(\s*\)`)

func emDash() string { return string(rune(0x2014)) }

func downNote() string {
	return `<p id="live-down" class="muted" hidden>live updates down ` + emDash() + ` polling every {{.RefreshSeconds}} s</p>`
}

const liveIndicator = `<p id="auto-refresh" class="muted" data-live="indicator">auto-refresh on (last refreshed {{.RenderedAt}})</p>`

// ---- criterion 1: the migration file -----------------------------------------------

func TestMigration0044_Shape(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "0044_*.sql"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) != 1 || filepath.Base(files[0]) != "0044_board_changed_notify.sql" {
		t.Fatalf("migrations/0044_*.sql = %v, want exactly migrations/0044_board_changed_notify.sql (criterion 1)", files)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read %s: %v", files[0], err)
	}
	var lines []string
	for _, l := range strings.Split(string(raw), "\n") {
		if i := strings.Index(l, "--"); i >= 0 {
			l = l[:i]
		}
		lines = append(lines, l)
	}
	code := strings.Join(lines, "\n")
	flat := regexp.MustCompile(`\s+`).ReplaceAllString(code, " ")

	for _, want := range []struct{ re, why string }{
		{`(?i)CREATE (OR REPLACE )?FUNCTION board_changed_notify\(\) RETURNS trigger`, "one function, board_changed_notify(), RETURNS trigger"},
		{`(?i)LANGUAGE plpgsql`, "plpgsql"},
		{`(?i)pg_notify\('board_changed', TG_TABLE_NAME \|\| ':' \|\|`, "pg_notify('board_changed', TG_TABLE_NAME || ':' || <id>)"},
		{`(?i)TG_OP = 'DELETE'`, "the id comes from OLD on DELETE"},
		{`(?i)OLD\.id`, "…OLD.id"},
		{`(?i)NEW\.id`, "…and NEW.id otherwise"},
		{`(?i)RETURN NULL`, "an AFTER trigger returns NULL"},
	} {
		if !regexp.MustCompile(want.re).MatchString(flat) {
			t.Errorf("0044 does not match /%s/ — %s (criterion 1)", want.re, want.why)
		}
	}
	if n := strings.Count(code, "'board_changed'"); n != 1 {
		t.Errorf("0044 spells the channel 'board_changed' %d times, want exactly once (criterion 9)", n)
	}

	type trig struct{ events, rest string }
	byTable := map[string][]trig{}
	all := createTriggerRE.FindAllStringSubmatch(code, -1)
	for _, m := range all {
		if m[5] != "board_changed_notify" {
			t.Errorf("trigger %s executes %s, want board_changed_notify (criterion 1: one function)", m[1], m[5])
		}
		byTable[m[3]] = append(byTable[m[3]], trig{events: flatWS(m[2]), rest: flatWS(m[4])})
	}
	if len(all) != 8 {
		t.Errorf("0044 creates %d triggers, want 8: INSERT/DELETE plus UPDATE-when-changed on each of four tables", len(all))
	}
	for _, tb := range []string{"tasks", "task_dismissals", "classify_promotions", "external_refs"} {
		ts := byTable[tb]
		var insdel, upd int
		for _, tr := range ts {
			ev := strings.ToUpper(tr.events)
			rest := strings.ToUpper(tr.rest)
			if !strings.HasPrefix(ev, "AFTER ") || !strings.Contains(rest, "FOR EACH ROW") {
				t.Errorf("a trigger on %s is not AFTER … FOR EACH ROW: %s ON %s %s (criterion 1; S2: a statement trigger "+
					"fires on zero-row UPDATEs)", tb, tr.events, tb, tr.rest)
				continue
			}
			switch {
			case strings.Contains(ev, "INSERT") && strings.Contains(ev, "DELETE") && !strings.Contains(ev, "UPDATE"):
				insdel++
				if strings.Contains(rest, "WHEN") {
					t.Errorf("the INSERT OR DELETE trigger on %s carries a WHEN clause", tb)
				}
			case strings.Contains(ev, "UPDATE") && !strings.Contains(ev, "INSERT") && !strings.Contains(ev, "DELETE"):
				upd++
				if !regexp.MustCompile(`WHEN \( ?OLD\.\* IS DISTINCT FROM NEW\.\* ?\)`).MatchString(rest) {
					t.Errorf("the UPDATE trigger on %s lacks WHEN (OLD.* IS DISTINCT FROM NEW.*): %s (criterion 1: a "+
						"no-op UPDATE is silent)", tb, tr.rest)
				}
			default:
				t.Errorf("a trigger on %s fires on %q; criterion 1 splits AFTER INSERT OR DELETE from AFTER UPDATE … WHEN", tb, tr.events)
			}
		}
		if insdel != 1 || upd != 1 {
			t.Errorf("%s has %d INSERT OR DELETE and %d UPDATE triggers, want exactly one of each (criterion 1)", tb, insdel, upd)
		}
	}
	for tb := range byTable {
		switch tb {
		case "tasks", "task_dismissals", "classify_promotions", "external_refs":
		default:
			t.Errorf("0044 creates a trigger on %s. Criterion 1: exactly four tables; normalized_messages and projects "+
				"are deliberately left to the 60 s tick (S2)", tb)
		}
	}
	lower := strings.ToLower(code)
	for _, banned := range []string{"create table", "alter table", "create index", "drop table", "task_events",
		"drop function", "insert into", "delete from"} {
		if strings.Contains(lower, banned) {
			t.Errorf("0044 contains %q outside comments. Criterion 1: nothing else is in the file — no table, no column, "+
				"no change to task_events_notify", banned)
		}
	}
	if regexp.MustCompile(`(?i)\bUPDATE\s+\w+\s+SET\b`).MatchString(code) {
		t.Errorf("0044 runs an UPDATE … SET; criterion 1: the migration writes nothing")
	}
}

// ---- criterion 9: live.go is read-only -------------------------------------------------

func TestBoardLive_HubSendsOnlySetAndListen(t *testing.T) {
	raw, err := os.ReadFile("live.go")
	if err != nil {
		t.Fatalf("read live.go: %v (criterion 9: the hub, the handler and the consts live in internal/dashboard/live.go)", err)
	}
	code := goCode(t, "live.go", raw)
	flat := strings.ReplaceAll(code, " ", "")
	for _, banned := range []string{"ex.Execute(", "executeTask(", `"github.com/sspataro57/switchboard/internal/tools"`,
		"tools.", "pg_notify"} {
		if strings.Contains(flat, strings.ReplaceAll(banned, " ", "")) {
			t.Errorf("live.go contains %s. Criterion 9 / invariant 3: the hub and the stream are read-only by construction "+
				"— no executor call, no internal/tools, no write", banned)
		}
	}
	sqlish := regexp.MustCompile(`(?i)^\s*(SELECT|INSERT|UPDATE|DELETE|SET|LISTEN|UNLISTEN|NOTIFY|WITH|CREATE|DROP|ALTER|TRUNCATE|COPY|BEGIN|COMMIT)\b`)
	var listen, setApp bool
	for _, lit := range goStringLits(t, "live.go", raw) {
		up := strings.ToUpper(lit)
		for _, w := range []string{"INSERT ", "UPDATE ", "DELETE "} {
			if strings.Contains(up, w) && sqlish.MatchString(lit) {
				t.Errorf("live.go carries the SQL %q (criterion 9: no INSERT, UPDATE or DELETE)", lit)
			}
		}
		if !sqlish.MatchString(lit) {
			continue
		}
		switch {
		case regexp.MustCompile(`^\s*LISTEN\b`).MatchString(lit):
			listen = true
		case regexp.MustCompile(`(?i)^\s*SET\s+application_name\b`).MatchString(lit):
			setApp = true
		default:
			t.Errorf("live.go carries the SQL string %q. Criterion 9: its only SQL strings are the SET application_name and "+
				"the LISTEN", lit)
		}
	}
	if !listen {
		t.Errorf("live.go has no LISTEN string (criterion 9 / S8: Run runs LISTEN board_changed)")
	}
	if !setApp {
		t.Errorf("live.go has no SET application_name string (S8: pg_stat_activity shows the hub's backend by name)")
	}
	if !strings.Contains(string(raw), "switchboard-board-live") {
		t.Errorf("live.go never names the application_name switchboard-board-live (S8, criterion 8a)")
	}
	for _, want := range []string{"Hijack(", "WaitForNotification(", "Acquire("} {
		if !strings.Contains(flat, want) {
			t.Errorf("live.go never calls %s (S8: Acquire, then Hijack the connection out of the pool, then loop on "+
				"WaitForNotification)", want)
		}
	}
}

func TestBoardLive_ChannelSpelledOnceInGo(t *testing.T) {
	root := filepath.Join("..", "..")
	var hits []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for i := 0; i < strings.Count(string(b), `"board_changed"`); i++ {
			hits = append(hits, filepath.ToSlash(p))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the repo: %v", err)
	}
	if len(hits) != 1 || !strings.HasSuffix(hits[0], "internal/dashboard/live.go") {
		t.Errorf("\"board_changed\" is spelled in non-test Go at %v; criterion 9: exactly once, boardChannel in "+
			"internal/dashboard/live.go (no Go-side notify anywhere — the trigger is the only writer)", hits)
	}
}

// ---- criterion 10: the route ----------------------------------------------------------

func TestBoardServer_StreamRouteIsAuthenticatedAndBeatsTheIDRoute(t *testing.T) {
	src := readSrc(t, "server.go")
	route := regexp.MustCompile(`mux\.Handle\("GET /tasks/stream",\s*s\.auth\.Require\(\s*http\.HandlerFunc\(\s*s\.boardStream\s*\)\s*\)\s*\)`)
	if !route.MatchString(src) {
		t.Errorf(`server.go registers no mux.Handle("GET /tasks/stream", s.auth.Require(http.HandlerFunc(s.boardStream))) ` +
			`(criterion 10)`)
	}

	auth, err := NewAuth(t.Context(), "", "", "", "") // dev mode, no network
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	h := (&Server{auth: auth}).Handler() // no pool, no hub
	serve := func(req *http.Request) (rec *httptest.ResponseRecorder, panicked any) {
		rec = httptest.NewRecorder()
		defer func() { panicked = recover() }()
		h.ServeHTTP(rec, req)
		return rec, nil
	}

	anon, p := serve(httptest.NewRequest(http.MethodGet, "/tasks/stream", nil))
	if p != nil {
		t.Fatalf("an unauthenticated GET /tasks/stream panicked (%v); it must be redirected like every authenticated route", p)
	}
	if anon.Code != http.StatusFound || !strings.Contains(anon.Header().Get("Location"), "/dev/login") {
		t.Errorf("an unauthenticated GET /tasks/stream = %d to %q, want 302 to the login page (criterion 10: the stream "+
			"sits behind s.auth.Require)", anon.Code, anon.Header().Get("Location"))
	}

	login := httptest.NewRecorder()
	h.ServeHTTP(login, httptest.NewRequest(http.MethodGet, "/dev/login?user=salvo", nil))
	cookies := login.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("dev login set no session cookie (status %d)", login.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/tasks/stream", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec, p := serve(req)
	if p != nil {
		t.Fatalf("GET /tasks/stream with a session reached a handler that panicked on the nil pool (%v): it was routed "+
			"to showTask as /tasks/{id} with id=\"stream\". Criterion 10: the literal GET /tasks/stream is registered "+
			"and Go 1.22's mux prefers it over the wildcard", p)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /tasks/stream with a session and no hub = %d, want 503 from boardStream (criterion 10: proven with "+
			"a nil-hub 503, not a 404 from showTask)", rec.Code)
	}
}

// ---- criterion 12: the handler runs no SQL and reads no query value ---------------------

func TestBoardStream_RunsNoSQLAndReadsNoQuery(t *testing.T) {
	if _, err := os.Stat("live.go"); err != nil {
		t.Fatalf("live.go: %v (criterion 12 reads (*Server).boardStream there)", err)
	}
	body := funcBodySrc(t, "live.go", "boardStream")
	if body == "" {
		t.Fatalf("live.go declares no boardStream (criterion 12 / S8: (*Server).boardStream)")
	}
	for _, banned := range []struct{ re, why string }{
		{`s\.pool`, "no SQL: the stream handler issues none (S11)"},
		{`\bQuery\b|\bQueryRow\b|\bExec\b`, "no SQL"},
		{`executeTask`, "no executor call (invariant 3: read-only by construction)"},
		{`\bex\.`, "no executor call"},
		{`URL\.|FormValue|ParseForm|PathValue|\.Form\b`, "reads no URL or form value: the stream takes no parameters (D15)"},
	} {
		if regexp.MustCompile(banned.re).MatchString(body) {
			t.Errorf("boardStream matches /%s/ — %s (criterion 12)", banned.re, banned.why)
		}
	}
	reached, _ := parseDashboardSource(t).reach("boardStream")
	for _, want := range []string{"text/event-stream", "X-Accel-Buffering", "Flush", "Retry-After"} {
		if !strings.Contains(reached, want) {
			t.Errorf("boardStream (and what it reaches) never mentions %s (S8: the headers, the flush after each write, "+
				"the 503's Retry-After)", want)
		}
	}
}

// ---- criterion 14: cmd/dashboard/main.go --------------------------------------------------

func TestDashboardMain_WiresTheHubWithNoWriteTimeout(t *testing.T) {
	src := readSrc(t, filepath.Join("..", "..", "cmd", "dashboard", "main.go"))
	for _, want := range []struct{ re, why string }{
		{`dashboard\.NewBoardHub\(\s*pool\s*\)`, "it constructs the hub over the process pool"},
		{`\bgo\s+\w+\.Run\(`, "it starts hub.Run in a goroutine"},
		{`\.SetBoardHub\(`, "it attaches the hub to the server"},
		{`ReadHeaderTimeout:\s*10\s*\*\s*time\.Second`, "ReadHeaderTimeout is unchanged"},
	} {
		if !regexp.MustCompile(want.re).MatchString(src) {
			t.Errorf("cmd/dashboard/main.go does not match /%s/ — %s (criterion 14)", want.re, want.why)
		}
	}
	if strings.Contains(src, "WriteTimeout") {
		t.Errorf("cmd/dashboard/main.go mentions WriteTimeout. Criterion 14: a write deadline cuts every stream at that age")
	}
	// GUARD: /healthz is unchanged — a blind hub must not get the pod that serves
	// approvals restarted by its liveness probe (S8).
	healthz := regexp.MustCompile(`mux\.HandleFunc\("GET /healthz", func\(w http\.ResponseWriter, r \*http\.Request\) \{\s*fmt\.Fprintln\(w, "ok"\)\s*\}\)`)
	if !healthz.MatchString(readSrc(t, "server.go")) {
		t.Errorf("/healthz is no longer the unconditional `fmt.Fprintln(w, \"ok\")` (S8: the board degrades to polling; " +
			"the pod is never restarted for a blind hub)")
	}
}

// ---- criterion 15: boardData carries the stream fields -------------------------------------

func TestBoardData_CarriesTheStreamFields(t *testing.T) {
	fields := structFieldNames(t, "board.go", "boardData")
	if fields == nil {
		t.Fatalf("board.go declares no boardData")
	}
	for _, f := range []string{"StreamURL", "RetrySeconds", "BoardVersion"} {
		if !fields[f] {
			t.Errorf("boardData has no %s field (criterion 15)", f)
		}
	}
	body := funcBodySrc(t, "board.go", "listTasks")
	for _, want := range []struct{ frag, why string }{
		{"StreamURL", "listTasks sets StreamURL"},
		{"RetrySeconds", "listTasks sets RetrySeconds"},
		{"boardLiveRetry", "RetrySeconds comes from boardLiveRetry (one const for the SSE retry: and the browser)"},
		{"BoardVersion", "listTasks sets BoardVersion"},
		{"boardVersion", "BoardVersion comes from the package var"},
	} {
		if !strings.Contains(body, want.frag) {
			t.Errorf("listTasks never mentions %s — %s (criterion 15)", want.frag, want.why)
		}
	}
}

// ---- criterion 17: the script tag ------------------------------------------------------------

func TestTasksTemplate_ScriptCarriesTheStreamAttributes(t *testing.T) {
	s := tasksHTML(t)
	if n := strings.Count(s, "<script"); n != 1 {
		t.Fatalf("tasks.html has %d <script, want exactly ONE (criterion 17)", n)
	}
	if d := templateDepthAt(s, strings.Index(s, "<script")); d != 0 {
		t.Errorf("the <script> sits %d template block(s) deep, want 0 (criterion 17)", d)
	}
	tag, _ := liveScript(t, s)
	for _, attr := range []string{`data-reload="{{.ReloadURL}}"`, `data-interval="{{.RefreshSeconds}}"`,
		`data-page-interval="{{.PageSeconds}}"`, `data-refresh="{{.RefreshMode}}"`,
		`data-stream="{{.StreamURL}}"`, `data-retry="{{.RetrySeconds}}"`, `data-version="{{.BoardVersion}}"`} {
		if !strings.Contains(tag, attr) {
			t.Errorf("the <script> tag lacks %s (criterion 17: every knob is a server-rendered data attribute). Tag: %s", attr, tag)
		}
	}
}

// ---- criterion 18: the five live regions ------------------------------------------------------

func TestTasksTemplate_FiveLiveRegions(t *testing.T) {
	s := tasksHTML(t)
	_, script := liveScript(t, s)
	markup := strings.Replace(s, script, "", 1)
	attrs := regexp.MustCompile(`\sdata-live(?:="([^"]*)")?`).FindAllStringSubmatch(markup, -1)
	got := map[string]int{}
	for _, m := range attrs {
		got[m[1]]++
	}
	if len(attrs) != 5 {
		t.Errorf("tasks.html carries %d data-live attributes %v outside the script, want exactly 5 (criterion 18)", len(attrs), got)
	}
	for _, n := range liveNames {
		if got[n] != 1 {
			t.Errorf("data-live=%q appears %d times, want exactly once (criterion 18)", n, got[n])
		}
	}

	wantTag := map[string]*regexp.Regexp{
		"alert":     regexp.MustCompile(`^<div data-live="alert">$`),
		"tally":     regexp.MustCompile(`^<div class="tally" data-live="tally">$`),
		"main":      regexp.MustCompile(`^<main data-live="main">$`),
		"counts":    regexp.MustCompile(`^<span data-live="counts">$`),
		"indicator": regexp.MustCompile(`^<p id="auto-refresh" class="muted" data-live="indicator">$`),
	}
	autoBlock, _ := templateBlockAfter(s, "{{if .AutoRefresh}}")
	type span struct {
		name string
		a, b int
	}
	var spans []span
	for _, n := range liveNames {
		_, open, start, end, ok := liveRegion(s, n)
		if !ok {
			t.Errorf("no well-formed element carries data-live=%q (criterion 18)", n)
			continue
		}
		spans = append(spans, span{n, start, end})
		if !wantTag[n].MatchString(open) {
			t.Errorf("the %s region opens with %s, want /%s/ (S6's table: which element each region is)", n, open, wantTag[n])
		}
		depth := templateDepthAt(s, start)
		switch n {
		case "indicator":
			if depth != 1 || !strings.Contains(autoBlock, `data-live="indicator"`) {
				t.Errorf("the indicator region sits at template depth %d; it lives inside the one {{if .AutoRefresh}} "+
					"block (criterion 18)", depth)
			}
		default:
			if depth != 0 {
				t.Errorf("the %s region sits %d template block(s) deep, want 0: a fetched document must always carry it "+
					"(criterion 18; data-live=\"alert\" inside {{with .OrchAlert}} would vanish whenever the alert does)", n, depth)
			}
		}
		region := s[start:end]
		for _, banned := range []string{"<script", `<form class="filters"`, `id="clock"`, `id="fs"`,
			`id="auto-refresh-toggle"`, `id="live-down"`, "{{if .Flash}}"} {
			if strings.Contains(region, banned) {
				t.Errorf("the %s region contains %s. Criterion 18 / S6: a swap never replaces the script, the filter form, "+
					"the clock, FULL, the toggle, the down-note or the flash", n, banned)
			}
		}
		switch n {
		case "alert":
			if !strings.Contains(region, "{{with .OrchAlert}}") {
				t.Errorf("{{with .OrchAlert}} is not inside the alert region (criterion 18)")
			}
		case "tally":
			if !strings.Contains(region, "{{range .Tally.Items}}") {
				t.Errorf("the tally region does not hold {{range .Tally.Items}}")
			}
		case "main":
			if !strings.Contains(region, "{{range .Panes}}") {
				t.Errorf("the main region does not hold {{range .Panes}}")
			}
		case "counts":
			if !strings.Contains(region, "{{.Tally.Open}}") || !strings.Contains(region, "{{.Tally.DoneToday}}") {
				t.Errorf("the counts region does not hold the overall counts {{.Tally.Open}} / {{.Tally.DoneToday}}")
			}
		}
	}
	for i := range spans {
		for j := range spans {
			if i != j && spans[i].a < spans[j].a && spans[j].b <= spans[i].b {
				t.Errorf("live region %s contains live region %s; the five regions are disjoint", spans[i].name, spans[j].name)
			}
		}
	}
	hb := strings.Index(s, `<div class="headbar">`)
	if hb < 0 {
		t.Fatalf("tasks.html has no .headbar")
	}
	he, ok := elementEnd(s, hb, "div")
	if !ok {
		t.Fatalf("the .headbar is never closed")
	}
	if strings.Contains(s[hb:he], "data-live") {
		t.Errorf("the .headbar contains a data-live region; S6: the nav and the filter form keep their typed state")
	}
	for _, tag := range []string{"<body", "<html"} {
		if i := strings.Index(s, tag); i >= 0 && strings.Contains(s[i:i+strings.Index(s[i:], ">")], "data-live") {
			t.Errorf("%s carries data-live; a swap never touches body or html (S6)", tag)
		}
	}
}

// ---- criterion 19: the indicator's words and the down-note ------------------------------------

func TestTasksTemplate_IndicatorWordsAndDownNote(t *testing.T) {
	s := tasksHTML(t)
	block, ok := templateBlockAfter(s, "{{if .AutoRefresh}}")
	if !ok {
		t.Fatalf("tasks.html has no {{if .AutoRefresh}} block")
	}
	if strings.TrimSpace(block) != liveIndicator {
		t.Errorf("the {{if .AutoRefresh}} block is\n  %s\nwant exactly\n  %s\n(criterion 19: \"every 5 s\" is no longer true, "+
			"and the <p> stays child-free so the integration regexes' [^<]* before the time still reads it)",
			strings.TrimSpace(block), liveIndicator)
	}
	note := downNote()
	if n := strings.Count(s, note); n != 1 {
		t.Fatalf("tasks.html carries the down-note %d times, want exactly once:\n  %s\n(criterion 19)", n, note)
	}
	ni := strings.Index(s, note)
	if d := templateDepthAt(s, ni); d != 0 {
		t.Errorf("the down-note sits %d template block(s) deep; it renders outside every conditional (criterion 19: the "+
			"script only toggles its hidden attribute, so the words are server-rendered)", d)
	}
	fi := strings.Index(s, `<footer class="ticker">`)
	fe, ok := elementEnd(s, fi, "footer")
	if fi < 0 || !ok || ni < fi || ni > fe {
		t.Errorf("the down-note is not in the ticker <footer> (criterion 19)")
	}
	ai := strings.Index(s, "{{if .AutoRefresh}}")
	blockEnd := ai + len("{{if .AutoRefresh}}") + len(block) + len("{{end}}")
	bn := strings.Index(s, `class="board-notes"`)
	if !(ai >= 0 && blockEnd <= ni && bn > ni) {
		t.Errorf("the down-note is not after the indicator block and before class=\"board-notes\" (criterion 19)")
	}
	for _, n := range liveNames {
		if _, _, start, end, ok := liveRegion(s, n); ok && ni >= start && ni < end {
			t.Errorf("the down-note sits inside the %s live region; a swap would reset it (criterion 19)", n)
		}
	}
}

// ---- criterion 20: the script's contract ---------------------------------------------------------

func TestTasksTemplate_LiveScriptContract(t *testing.T) {
	s := tasksHTML(t)
	_, script := liveScript(t, s)
	for _, want := range []string{"EventSource", "fetch(", "DOMParser", "parseFromString(", "importNode(", "replaceWith(",
		"[data-live", "data-version", ".redirected", "location.replace(", "setTimeout", "document.hidden",
		"details[open]", "activeElement", "defaultValue", "defaultSelected", "requestFullscreen", "wakeLock",
		"textContent"} {
		if !strings.Contains(script, want) {
			t.Errorf("the script lacks %q (criterion 20a)", want)
		}
	}
	if !strings.Contains(script, `"text/html"`) && !strings.Contains(script, `'text/html'`) {
		t.Errorf("the script never names \"text/html\" (criterion 20a: DOMParser's type, and the content-type check)")
	}
	for _, banned := range []string{"innerHTML", "outerHTML =", "insertAdjacentHTML", "document.write",
		"createContextualFragment", "eval(", "new Function", "srcdoc", "XMLHttpRequest", "htmx", "location.search",
		"location.href", "localStorage", "sessionStorage", "onchange"} {
		if strings.Contains(script, banned) {
			t.Errorf("the script contains %q. Criterion 20b / S6: the swap is DOMParser + importNode + replaceWith; a "+
				"string-to-DOM sink would run whatever a rendered title carries", banned)
		}
	}
	if n := strings.Count(script, "fetch("); n != 1 {
		t.Errorf("the script calls fetch( %d times, want exactly 1 (criterion 20c: one in-place fetch, single-flight)", n)
	}
	if n := strings.Count(script, "new EventSource("); n != 1 {
		t.Errorf("the script has %d new EventSource(, want exactly 1 (criterion 20c)", n)
	}
	if ai := strings.Index(script, "function arm()"); ai < 0 {
		t.Errorf("the script has no function arm() (criterion 20d)")
	} else {
		body := script[ai:]
		if n := strings.Index(body[len("function arm()"):], "function "); n >= 0 {
			body = body[:len("function arm()")+n]
		}
		if !regexp.MustCompile(`if\s*\(\s*!refreshOn\s*\)\s*return`).MatchString(body) {
			t.Errorf("arm() does not return early on !refreshOn (criterion 20d: no stream opens and nothing is fetched "+
				"on a plain board). arm():\n%s", body)
		}
	}
	const fn = "function busy()"
	b := strings.Index(script, fn)
	if b < 0 {
		t.Fatalf("the script has no %s (criterion 20e)", fn)
	}
	busy := script[b:]
	if n := strings.Index(busy[len(fn):], "function "); n >= 0 {
		busy = busy[:len(fn)+n]
	}
	for _, want := range []string{"document.hidden", "details[open]", "activeElement", "dirty("} {
		if !strings.Contains(busy, want) {
			t.Errorf("busy() does not name %q (criterion 20e: a swap waits for a hidden tab, an open row popup, a focused "+
				"or dirty control). busy():\n%s", want, busy)
		}
	}
	for _, gone := range []string{"pageCycleBusy", "cycleDone"} {
		if strings.Contains(s, gone) {
			t.Errorf("tasks.html still names %s. Criterion 20e / S7: B13's page-cycle clause is retired — a swap preserves "+
				"each panel's page, so it no longer waits for page 1", gone)
		}
	}
}
