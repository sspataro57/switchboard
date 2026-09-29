package dashboard

// watch-json (SWT-101, docs/tickets/watch-json_SPEC.md) structure checks:
// criteria 11 (watchJSON reads through boardView + boardTallies and nothing else;
// watchAuth compares in constant time) and 12 (the registration, the demo
// off-scope and its one exception), plus the D2 purity of watchSessions, D3's
// wiring in cmd/dashboard, and "no new demoExempt / demoSeams entry" (D4). ZERO
// I/O beyond reading this repo's source. The amended board tests (criterion 13)
// live in their own files; their shared reader, boardBuildSrc, is in
// board_layout_structure_test.go.
//
// EXPECTED RED: watch.go does not exist, so funcBodySrc finds no watchJSON /
// watchAuth / watchSessions / SetWatchToken and server.go registers no
// /watch.json; with watch_test.go the package does not compile at all today.
//
// MUTATIONS THAT MUST TURN THIS FILE RED: M1 (a SELECT count(*) in watchJSON),
// M2 (== instead of ConstantTimeCompare), M4 (s.auth.Require around the route),
// M7 (no watchScope call).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// codeOnly drops // comment lines, so a ban is about code, not about prose that
// explains the ban.
func codeOnly(body string) string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// funcDecl returns the parsed declaration of the named top-level func or method
// in file, with the file's bytes and fileset.
func funcDecl(t *testing.T, file, name string) (*ast.FuncDecl, []byte, *token.FileSet) {
	t.Helper()
	src := []byte(readSrc(t, file))
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == name && fn.Body != nil {
			return fn, src, fset
		}
	}
	return nil, src, fset
}

// ---- criterion 11: watchJSON reads the board's own pipeline ---------------------------

func TestWatchJSON_ReadsTheBoardPipelineAndNothingElse(t *testing.T) {
	fn, src, fset := funcDecl(t, "watch.go", "watchJSON")
	if fn == nil {
		t.Fatalf("watch.go declares no watchJSON (criterion 11: func (s *Server) watchJSON(w, r) in internal/dashboard/watch.go)")
	}
	body := codeOnly(string(src[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset]))
	for _, want := range []struct{ frag, why string }{
		{"s.boardView(", "D1: the rows come from the SAME boardView listTasks uses"},
		{"boardTallies(", "D1: the four numbers are boardTallies(secs), the header's own spelling"},
		{"watchSessions(", "D2: the lists come from the one pure function"},
		{"watchScope(", "criterion 12 / D4: the scope is explicitly OFF (M7)"},
	} {
		if !strings.Contains(body, want.frag) {
			t.Errorf("watchJSON does not call %s — %s", want.frag, want.why)
		}
	}
	for _, banned := range []struct{ frag, why string }{
		{"s.pool", "criterion 11: no SQL of its own (M1)"},
		{"demoQuery", "criterion 11: no SQL of its own, not even a marked one (M1)"},
		{"s.ex", "criterion 11 / D5: no executor call"},
		{"Execute(", "criterion 11 / D5: no executor call"},
		{"boardRows(", "D1: the rows come through boardView, not a second assembly"},
		{"boardLightFacts(", "D1: the facts come through boardView, not a second read"},
	} {
		if strings.Contains(body, banned.frag) {
			t.Errorf("watchJSON contains %s — %s", banned.frag, banned.why)
		}
	}
	sqlWord := regexp.MustCompile(`(?i)\b(SELECT|INSERT|UPDATE|DELETE)\b|count\(`)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && sqlWord.MatchString(lit.Value) {
			t.Errorf("watchJSON carries a SQL string literal %s; criterion 11: the counts come from boardTallies, "+
				"never a SELECT count(*) (M1)", lit.Value)
		}
		return true
	})
	if regexp.MustCompile(`\b(INSERT|UPDATE|DELETE)\b`).MatchString(body) {
		t.Errorf("watchJSON mentions INSERT/UPDATE/DELETE; criterion 11 / D5: the route writes nothing")
	}
}

// ---- criterion 11: watchAuth compares in constant time --------------------------------

// secretish names the operands a timing-leaky comparison would involve.
var secretish = regexp.MustCompile(`(?i)digest|token|sum|secret|bearer|auth|presented|cred|hdr|header|want|got`)

func TestWatchAuth_ComparesInConstantTime(t *testing.T) {
	fn, src, fset := funcDecl(t, "watch.go", "watchAuth")
	if fn == nil {
		t.Fatalf("watch.go declares no watchAuth (criterion 11: func (s *Server) watchAuth(next http.Handler) http.Handler)")
	}
	text := func(n ast.Node) string {
		return string(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])
	}
	body := codeOnly(text(fn.Body))
	if !strings.Contains(body, "subtle.ConstantTimeCompare(") {
		t.Errorf("watchAuth does not call subtle.ConstantTimeCompare (criterion 11 / D3, M2)")
	}
	if !strings.Contains(body, "sha256.") {
		t.Errorf("watchAuth does not hash the presented token (D3: ConstantTimeCompare(sha256(presented), storedDigest) — " +
			"equal-length digests leak neither the value nor its length)")
	}
	for _, banned := range []string{"bytes.Equal", "reflect.DeepEqual", "strings.EqualFold(tok", "hmac.Equal"} {
		if strings.Contains(body, banned) {
			t.Errorf("watchAuth uses %s; criterion 11: the one comparison is subtle.ConstantTimeCompare", banned)
		}
	}
	// An == / != is allowed only against a literal, nil or a len(...), or on
	// ConstantTimeCompare's own int result (the SPEC's `== 1`).
	benign := func(e ast.Expr) bool {
		switch v := e.(type) {
		case *ast.BasicLit:
			return true
		case *ast.Ident:
			return v.Name == "nil" || v.Name == "true" || v.Name == "false"
		case *ast.CallExpr:
			s := text(v.Fun)
			return s == "len" || strings.HasSuffix(s, "ConstantTimeCompare")
		case *ast.SelectorExpr:
			return strings.HasPrefix(text(v), "http.Method")
		}
		return false
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		be, ok := n.(*ast.BinaryExpr)
		if !ok || (be.Op != token.EQL && be.Op != token.NEQ) {
			return true
		}
		if benign(be.X) || benign(be.Y) {
			return true
		}
		if secretish.MatchString(text(be)) {
			t.Errorf("watchAuth compares %q with %s; criterion 11: no == on the token or digest (M2)", text(be), be.Op)
		}
		return true
	})
	// The digest, never the token, is what the Server keeps (D3).
	fields := structFieldNames(t, "server.go", "Server")
	if !fields["watchDigest"] {
		t.Errorf("Server has no watchDigest field (SPEC Files: server.go, `watchDigest []byte`)")
	}
	for f := range fields {
		if strings.Contains(strings.ToLower(f), "token") {
			t.Errorf("Server has a field %q; D3: the server stores sha256(token), never the token", f)
		}
	}
	set := codeOnly(funcBodySrc(t, "watch.go", "SetWatchToken"))
	if set == "" {
		t.Fatalf("watch.go declares no SetWatchToken (D3)")
	}
	for _, want := range []string{"strings.TrimSpace(", "sha256.", "32", "slog.Warn("} {
		if !strings.Contains(set, want) {
			t.Errorf("SetWatchToken does not contain %s (D3: TrimSpace, a 32-byte floor with one slog.Warn, store the "+
				"sha256 digest)", want)
		}
	}
}

// ---- criterion 12: registration ---------------------------------------------------

func TestWatchRoute_RegistrationIsTheGateOnly(t *testing.T) {
	src := readSrc(t, "server.go")
	re := regexp.MustCompile(`mux\.Handle\("GET /watch\.json",[^\n]*`)
	all := re.FindAllStringIndex(src, -1)
	if len(all) != 1 {
		t.Fatalf("server.go registers GET /watch.json %d times, want exactly once: "+
			`mux.Handle("GET /watch.json", s.watchAuth(http.HandlerFunc(s.watchJSON))) (criterion 12)`, len(all))
	}
	line := src[all[0][0]:all[0][1]]
	exact := regexp.MustCompile(`^mux\.Handle\("GET /watch\.json",\s*s\.watchAuth\(http\.HandlerFunc\(s\.watchJSON\)\)\)`)
	if !exact.MatchString(line) {
		t.Errorf("the route is registered as %q, want mux.Handle(\"GET /watch.json\", "+
			"s.watchAuth(http.HandlerFunc(s.watchJSON))) (criterion 12)", line)
	}
	for _, banned := range []struct{ frag, why string }{
		{"Require(", "the courier has no session: behind s.auth.Require it gets a 302 to the login page (M4)"},
		{"demoScoped(", "D4: the watch always shows the real board; demoScoped would also add a 503 path"},
		{"devLogin", "D3: not behind the dev login"},
	} {
		if strings.Contains(line, banned.frag) {
			t.Errorf("the /watch.json registration contains %s: %s (criterion 12)", banned.frag, banned.why)
		}
	}
	// D3: "with a comment explaining why it is open to the session layer".
	before := src[:all[0][0]]
	if i := strings.LastIndex(before, "\n\n"); i >= 0 {
		before = before[i:]
	}
	if !strings.Contains(before, "//") {
		t.Errorf("the /watch.json route carries no comment saying why it is outside the session layer (D3)")
	}
}

// ---- criterion 12: the demo off-scope is spelled in demo.go, once ---------------------

func TestWatchDemo_OffScopeIsTheOneException(t *testing.T) {
	if len(demoOffRoutes) != 1 {
		t.Errorf("demoOffRoutes has %d entries, want exactly one (criterion 12)", len(demoOffRoutes))
	}
	reason, ok := demoOffRoutes["GET /watch.json"]
	if !ok {
		t.Errorf("demoOffRoutes has no \"GET /watch.json\" key (criterion 12: the key is the mux pattern)")
	}
	if strings.TrimSpace(reason) == "" || strings.Contains(reason, "\n") {
		t.Errorf("demoOffRoutes[\"GET /watch.json\"] = %q, want a non-empty ONE-line reason (criterion 12)", reason)
	}
	// Every other route is still demo-scoped: an off route is one the list names.
	src := readSrc(t, "server.go")
	for _, m := range regexp.MustCompile(`mux\.Handle(?:Func)?\("([A-Z]+ [^"]*)",([^\n]*)`).FindAllStringSubmatch(src, -1) {
		pattern, rest := m[1], m[2]
		if _, off := demoOffRoutes[pattern]; off && strings.Contains(rest, "demoScoped(") {
			t.Errorf("%s is in demoOffRoutes but is wrapped in demoScoped; the list and the mux disagree", pattern)
		}
	}

	// withDemoScope( is called only in demoScoped and watchScope.
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	callers := map[string]int{}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "withDemoScope" {
						callers[name+":"+fn.Name.Name]++
					}
				}
				return true
			})
		}
	}
	for k := range callers {
		if k != "demo.go:demoScoped" && k != "demo.go:watchScope" {
			t.Errorf("withDemoScope is called from %s; criterion 12: only demoScoped and watchScope (demo.go) put a "+
				"scope on a context", k)
		}
	}
	if callers["demo.go:watchScope"] == 0 {
		t.Errorf("demo.go's watchScope does not call withDemoScope (D4: watchScope(ctx) = withDemoScope(ctx, demoScope{}))")
	}
	ws := codeOnly(funcBodySrc(t, "demo.go", "watchScope"))
	if !regexp.MustCompile(`withDemoScope\(\s*\w+\s*,\s*demoScope\{\s*\}\s*\)`).MatchString(ws) {
		t.Errorf("watchScope is %q, want withDemoScope(ctx, demoScope{}) — OFF, spelled as the zero scope (D4)", ws)
	}
	if strings.Contains(ws, "loadDemoScope") || strings.Contains(ws, "ops_flags") {
		t.Errorf("watchScope reads the flag; D4: the watch never follows demo mode and has no flag-read 503")
	}
}

// D4: "Neither demoExempt nor demoSeams fits … So no entry is added to either map."
func TestWatchDemo_NoNewExemptionOrSeam(t *testing.T) {
	for _, m := range []struct {
		name string
		v    map[string]string
	}{{"demoExempt", demoExempt}, {"demoSeams", demoSeams}} {
		for k := range m.v {
			lk := strings.ToLower(k)
			if strings.Contains(lk, "watch") || strings.Contains(lk, "boardview") {
				t.Errorf("%s gained %q; D4: the watch reuses boardRows/boardLightFacts, which already carry their "+
					"markers, and takes no pool-taking seam, so neither map changes", m.name, k)
			}
		}
	}
}

// ---- D2: watchSessions is pure ------------------------------------------------------

func TestWatchSessions_IsPure(t *testing.T) {
	fn, src, fset := funcDecl(t, "watch.go", "watchSessions")
	if fn == nil {
		t.Fatalf("watch.go declares no watchSessions (D2)")
	}
	if fn.Recv != nil {
		t.Errorf("watchSessions is a method; D2: one pure function, no Server")
	}
	body := codeOnly(string(src[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset]))
	for _, banned := range []string{"time.", "s.pool", "Query(", "Exec(", "http.", "slog.", "context."} {
		if strings.Contains(body, banned) {
			t.Errorf("watchSessions contains %q; D2: it does no I/O and reads no clock", banned)
		}
	}
	for _, want := range []struct{ frag, why string }{
		{".Session", "the name is facts[id].Session"},
		{"StateUnix", "since is facts[id].StateUnix"},
		{`"input"`, "waiting = Light.Class \"input\" with a session tag"},
		{`"working"`, "working = Light.Class \"working\" with a session tag (stale excluded)"},
	} {
		if !strings.Contains(body, want.frag) {
			t.Errorf("watchSessions does not mention %s — D2: %s", want.frag, want.why)
		}
	}
	if strings.Contains(body, `"stale"`) {
		t.Errorf("watchSessions mentions \"stale\"; D2: a stale lease is never listed (M5)")
	}
}

// ---- D2: state_unix is one additive column, display-only -----------------------------

func TestLightFacts_StateUnixIsOneAdditiveDisplayOnlyColumn(t *testing.T) {
	if lf := funcBodySrc(t, "lights.go", "lightFor"); lf == "" {
		t.Fatalf("lights.go declares no lightFor")
	} else if strings.Contains(lf, "StateUnix") {
		t.Errorf("lightFor reads StateUnix; D2: it is display-only")
	}
	if !structFieldNames(t, "lights.go", "lightFacts")["StateUnix"] {
		t.Errorf("lightFacts has no StateUnix field (D2)")
	}
	facts := funcBodySrc(t, "board.go", "boardLightFacts")
	if !regexp.MustCompile(`COALESCE\(\s*EXTRACT\(\s*EPOCH\s+FROM\s+\(?\s*t\.working_state_at\s*\)?\s*\)::bigint\s*,\s*0\s*\)\s+AS\s+state_unix`).MatchString(facts) {
		t.Errorf("boardLightFacts does not select COALESCE(EXTRACT(EPOCH FROM t.working_state_at)::bigint, 0) AS " +
			"state_unix (D2: one additive column in the SAME statement)")
	}
	if !strings.Contains(facts, "StateUnix") {
		t.Errorf("boardLightFacts never sets lightFacts.StateUnix (D2)")
	}
}

// ---- D1: boardView is the rows-and-sections half of listTasks --------------------------

func TestBoardView_IsTheSharedRowPipeline(t *testing.T) {
	view := codeOnly(funcBodySrc(t, "board.go", "boardView"))
	if view == "" {
		t.Fatalf("board.go declares no boardView (D1: the row-and-section part of listTasks moves there, unchanged)")
	}
	order := []string{"s.boardRows(", "s.reopenMarkers(", "s.boardLightFacts(", "lightFor(", "boardSections("}
	last := -1
	for _, frag := range order {
		i := strings.Index(view, frag)
		if i < 0 {
			t.Errorf("boardView does not call %s (D1's order: boardRows → reopenMarkers → boardLightFacts → the "+
				"taskRow loop → boardSections)", frag)
			continue
		}
		if i < last {
			t.Errorf("boardView calls %s out of D1's order", frag)
		}
		last = i
	}
	for _, banned := range []string{"boardTallies(", "boardPanes(", "ExecuteTemplate", `Get("refresh")`, "orchestrator.Health"} {
		if strings.Contains(view, banned) {
			t.Errorf("boardView contains %s; D1: display work stays in listTasks", banned)
		}
	}
	list := codeOnly(funcBodySrc(t, "board.go", "listTasks"))
	if !strings.Contains(list, "s.boardView(") {
		t.Errorf("listTasks does not call s.boardView (D1: both /tasks and /watch.json build rows through it)")
	}
	for _, moved := range []string{"s.boardRows(", "s.boardLightFacts(", "s.reopenMarkers("} {
		if strings.Contains(list, moved) {
			t.Errorf("listTasks still calls %s itself; D1: the row assembly lives in boardView only (one spelling)", moved)
		}
	}
}

// ---- D3: cmd/dashboard wires the env var before Handler() ------------------------------

func TestWatchToken_WiredFromTheEnvironmentBeforeHandler(t *testing.T) {
	main := codeOnly(readSrc(t, filepath.Join("..", "..", "cmd", "dashboard", "main.go")))
	i := strings.Index(main, `SetWatchToken(os.Getenv("SWB_WATCH_TOKEN"))`)
	if i < 0 {
		t.Fatalf(`cmd/dashboard/main.go does not call srv.SetWatchToken(os.Getenv("SWB_WATCH_TOKEN")) (D3)`)
	}
	if j := strings.Index(main, ".Handler()"); j >= 0 && j < i {
		t.Errorf("main.go builds srv.Handler() before SetWatchToken; D3: the token is set first (the SetBoardHub pattern)")
	}
	raw := readSrc(t, filepath.Join("..", "..", "cmd", "dashboard", "main.go"))
	if head, _, _ := strings.Cut(raw, "package main"); !strings.Contains(head, "SWB_WATCH_TOKEN") {
		t.Errorf("main.go's header comment does not name SWB_WATCH_TOKEN (SPEC Files: \"the env var in the header comment\")")
	}
}
