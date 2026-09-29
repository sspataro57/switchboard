package dashboard

// demo-mode (SWT-99, docs/tickets/demo-mode_SPEC.md): the unit halves — criterion 1
// (decodeDemoScope implements the D1 table), 2 (the per-request scope rides the
// request context; no scope means demo ON with empty lists), 4 (the open routes
// never read the flag) and 6 (demoSQL's bind numbering, one bind per scope value
// per statement, and an error on an unknown marker), plus the Go-side twin
// accountVisible (criterion 5/20). ZERO Postgres: the only pool here is a lazy
// one pointed at a closed port (127.0.0.1:1), so any statement fails at once and
// a handler that reaches it is caught without a database.
//
// IMPOSED SURFACE. The SPEC names demoScope, decodeDemoScope(raw, present),
// demoSQL(sc, q, args), sc.accountVisible, visibleTask/visibleDelivery/
// visiblePlan, demoExempt and demoSeams, and the six markers. It leaves the
// field names, the context helpers and the return shapes open; this file's
// spelling of those is marked *. Build exactly this in internal/dashboard/demo.go:
//
//	*type demoScope struct {
//	     On       bool
//	     Projects []string // project slugs, compared exactly
//	     Accounts []string // the flag's "source_accounts", compared case-insensitively
//	 }
//	func decodeDemoScope(raw []byte, present bool) demoScope // the D1 table; never an error
//	*func withDemoScope(ctx context.Context, sc demoScope) context.Context
//	*func demoScopeFrom(ctx context.Context) demoScope // no scope in ctx -> demoScope{On: true}, empty lists
//	*func demoSQL(sc demoScope, q string, args []any) (string, []any, error)
//	 // Expands @demo.project(p) @demo.task(t) @demo.delivery(d) @demo.account(a)
//	 // @demo.message(nm) @demo.ref(er). Returns args with the caller's first,
//	 // unchanged, then each scope value it USES bound once (On as a bool,
//	 // Projects as []string, Accounts lower-cased as []string); the R7 brief
//	 // pattern may be inlined or bound once. Unknown marker -> error.
//	func (sc demoScope) accountVisible(email string) bool // off -> true
//	*func (s *Server) visibleTask(ctx context.Context, sc demoScope, id int64) (bool, error)
//	*func (s *Server) visibleDelivery(ctx context.Context, sc demoScope, id int64) (bool, error)
//	*func (s *Server) visiblePlan(ctx context.Context, sc demoScope, id int64) (bool, error)
//	var demoExempt = map[string]string{ /* func or package-level var/const name -> one-line reason */ }
//	var demoSeams  = map[string]string{ /* "pkg.Func" -> one-line reason */ }
//
// The per-request loader and the wrapper around the authenticated routes in
// Handler() are behavioural only (criteria 2-4, D1's 503 row); their names are
// the implementer's. The wrapper must NOT wrap /healthz, /static/* or /dev/login.
//
// GREENFIELD NOTE — EXPECTED RED: demo.go does not exist, so package dashboard's
// test binary compile-FAILS on every symbol above (undefined: demoScope,
// decodeDemoScope, demoSQL, withDemoScope, demoScopeFrom, visibleTask, ...,
// demoExempt, demoSeams). That is the expected failure.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - M3 decode error -> off instead of on: every fail-closed row of TestDecodeDemoScope_D1Table.
//   - a scope value bound once per marker: TestDemoSQL_BindsEachScopeValueOncePerStatement.
//   - the wrapper applied to /healthz, /static or /dev/login: TestDemoScope_OpenRoutesNeverReadTheFlag.
//   - a load error treated as "off": TestDemoScope_FlagReadErrorIs503.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Compile-time pins of the imposed method signatures (criterion 17's pre-executor
// checks). A different shape is a compile error here, not a silent drift.
var (
	_ func(*Server, context.Context, demoScope, int64) (bool, error) = (*Server).visibleTask
	_ func(*Server, context.Context, demoScope, int64) (bool, error) = (*Server).visibleDelivery
	_ func(*Server, context.Context, demoScope, int64) (bool, error) = (*Server).visiblePlan
	_ map[string]string                                              = demoExempt
	_ map[string]string                                              = demoSeams
)

// DemoBareHandlers exposes three handlers WITHOUT the demo wrapper to the
// external integration suite (criterion 2's DB half), in the export_test idiom
// of board_live_export_test.go: a request that reaches them carries no scope.
func DemoBareHandlers(s *Server) map[string]http.Handler {
	return map[string]http.Handler{
		"tasks":      http.HandlerFunc(s.listTasks),
		"deliveries": http.HandlerFunc(s.listDeliveries),
		"export.csv": http.HandlerFunc(s.exportCSV),
	}
}

// ---- criterion 1: the D1 table ---------------------------------------------------

func TestDecodeDemoScope_D1Table(t *testing.T) {
	for _, tc := range []struct {
		name     string
		raw      string
		nilRaw   bool
		present  bool
		on       bool
		projects []string
		accounts []string
	}{
		// Row 1: no demo_mode row -> off, whatever the bytes.
		{name: "no row", nilRaw: true, present: false, on: false},
		{name: "no row, stray bytes ignored", raw: `{"on":true,"projects":["a"]}`, present: false, on: false},
		// Row 2: decodes, on:false -> off.
		{name: "on false", raw: `{"on":false}`, present: true, on: false},
		{name: "on false with lists", raw: `{"on":false,"projects":["a"],"source_accounts":["x@y.z"]}`, present: true, on: false},
		// Row 3: decodes, on:true -> on with the lists; a missing list is empty.
		{name: "on true, both lists", raw: `{"on":true,"projects":["collaboratory","a-millon"],"source_accounts":["Ops@Example.com","b@c.d"]}`,
			present: true, on: true, projects: []string{"collaboratory", "a-millon"}, accounts: []string{"ops@example.com", "b@c.d"}},
		{name: "on true, no lists", raw: `{"on":true}`, present: true, on: true},
		{name: "on true, projects only", raw: `{"on":true,"projects":["switchboard"]}`, present: true, on: true, projects: []string{"switchboard"}},
		{name: "on true, accounts only", raw: `{"on":true,"source_accounts":["a@b.c"]}`, present: true, on: true, accounts: []string{"a@b.c"}},
		// Row 4: on missing / not a bool / undecodable -> ON with EMPTY lists.
		{name: `{"on":"yes"}`, raw: `{"on":"yes"}`, present: true, on: true},
		{name: `{"on":true,"projects":5}`, raw: `{"on":true,"projects":5}`, present: true, on: true},
		{name: `{"on":false,"projects":5} fails closed`, raw: `{"on":false,"projects":5}`, present: true, on: true},
		{name: `{"on":true,"source_accounts":["a",3]}`, raw: `{"on":true,"projects":["x"],"source_accounts":["a",3]}`, present: true, on: true},
		{name: "null", raw: `null`, present: true, on: true},
		{name: "{}", raw: `{}`, present: true, on: true},
		{name: "on missing, lists given", raw: `{"projects":["x"],"source_accounts":["a@b.c"]}`, present: true, on: true},
		{name: "on null", raw: `{"on":null}`, present: true, on: true},
		{name: "on 1", raw: `{"on":1}`, present: true, on: true},
		{name: "not json", raw: `on`, present: true, on: true},
		{name: "empty bytes", raw: ``, present: true, on: true},
		{name: "array", raw: `[true]`, present: true, on: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw []byte
			if !tc.nilRaw {
				raw = []byte(tc.raw)
			}
			got := decodeDemoScope(raw, tc.present)
			if got.On != tc.on {
				t.Errorf("decodeDemoScope(%q, present=%v).On = %v, want %v (D1: every ambiguous case fails CLOSED — on, "+
					"nothing visible; only an absent row or a clean on:false is off)", tc.raw, tc.present, got.On, tc.on)
			}
			if !tc.on {
				return // off: the lists are never consulted
			}
			if !sameStrings(got.Projects, tc.projects, false) {
				t.Errorf("Projects = %q, want %q (D1: an undecodable value is on with EMPTY lists; slugs compare exactly)",
					got.Projects, tc.projects)
			}
			if !sameStrings(got.Accounts, tc.accounts, true) {
				t.Errorf("Accounts = %q, want %q (D1: an undecodable value is on with EMPTY lists)", got.Accounts, tc.accounts)
			}
		})
	}
}

func sameStrings(got, want []string, fold bool) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if fold && !strings.EqualFold(got[i], want[i]) || !fold && got[i] != want[i] {
			return false
		}
	}
	return true
}

// ---- criterion 5/20: the Go-side twin ------------------------------------------------

func TestDemoScope_AccountVisible(t *testing.T) {
	off := decodeDemoScope([]byte(`{"on":false}`), true)
	absent := decodeDemoScope(nil, false)
	on := decodeDemoScope([]byte(`{"on":true,"projects":["p"],"source_accounts":["Ops@Example.com"]}`), true)
	empty := decodeDemoScope([]byte(`{"on":true}`), true)
	for _, tc := range []struct {
		name  string
		sc    demoScope
		email string
		want  bool
	}{
		{"off shows everything", off, "anyone@personal.example", true},
		{"no row shows everything", absent, "anyone@personal.example", true},
		{"listed, same case", on, "Ops@Example.com", true},
		{"listed, lower case", on, "ops@example.com", true},
		{"listed, upper case", on, "OPS@EXAMPLE.COM", true},
		{"not listed", on, "other@example.com", false},
		{"empty address", on, "", false},
		{"on with an empty list shows nothing", empty, "ops@example.com", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.sc.accountVisible(tc.email); got != tc.want {
				t.Errorf("accountVisible(%q) = %v, want %v (D1: lower() on both sides; off shows every account)", tc.email, got, tc.want)
			}
		})
	}
}

// ---- criterion 2: the scope rides the request context ------------------------------

func TestDemoScope_NoScopeInContextIsOnWithEmptyLists(t *testing.T) {
	sc := demoScopeFrom(context.Background())
	if !sc.On || len(sc.Projects) != 0 || len(sc.Accounts) != 0 {
		t.Errorf("demoScopeFrom(a context with no scope) = %+v, want on with empty lists (criterion 2: a handler that "+
			"somehow runs without the wrapper shows NOTHING, never everything)", sc)
	}
	want := demoScope{On: true, Projects: []string{"switchboard"}, Accounts: []string{"a@b.c"}}
	got := demoScopeFrom(withDemoScope(context.Background(), want))
	if got.On != want.On || !sameStrings(got.Projects, want.Projects, false) || !sameStrings(got.Accounts, want.Accounts, true) {
		t.Errorf("demoScopeFrom(withDemoScope(ctx, %+v)) = %+v; the wrapper's scope must reach the handler unchanged", want, got)
	}
	offCtx := withDemoScope(context.Background(), demoScope{On: false})
	if demoScopeFrom(offCtx).On {
		t.Errorf("an explicit off scope in the context read back as on")
	}
}

// demoDeadPool is a pool that can never connect: pgxpool is lazy (MinConns 0),
// so construction succeeds and every statement fails fast with a refused dial.
func demoDeadPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgres://nobody:nothing@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("parse dead DSN: %v", err)
	}
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("dead pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func demoDeadServer(t *testing.T, ex Exec) (*Server, http.Handler, []*http.Cookie) {
	t.Helper()
	auth, err := NewAuth(context.Background(), "", "", "", "")
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	s, err := NewServer(demoDeadPool(t), ex, auth)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	h := s.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dev/login?user=salvo", nil))
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("dev login set no session cookie (status %d): /dev/login must work with no database (criterion 4)", rec.Code)
	}
	return s, h, cookies
}

// Criterion 2: a VERB handler called WITHOUT the wrapper finds no scope, so demo
// is on with empty lists and the id is not visible: the executor is never called.
func TestDemoScope_UnwrappedVerbNeverReachesTheExecutor(t *testing.T) {
	ex := &captureExec{}
	s, _, _ := demoDeadServer(t, ex)
	req := httptest.NewRequest(http.MethodPost, "/tasks/7/close", strings.NewReader(url.Values{"note": {"x"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", "7")
	rec := httptest.NewRecorder()
	s.closeTaskAction(rec, req)
	if len(ex.calls) != 0 {
		t.Errorf("closeTaskAction with no scope in its context made %d executor call(s) %+v, want 0 (criterion 2: no scope = "+
			"demo on with empty lists, so task 7 is not visible and criterion 17's pre-check refuses before the executor)",
			len(ex.calls), ex.calls)
	}
	if rec.Code >= 200 && rec.Code < 300 {
		t.Errorf("closeTaskAction with no scope answered %d; a refused verb redirects (or fails), it never succeeds", rec.Code)
	}
}

// ---- D1's last row: a flag read error is a 503 with no body data ------------------

func TestDemoScope_FlagReadErrorIs503(t *testing.T) {
	ex := &captureExec{}
	_, h, cookies := demoDeadServer(t, ex)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/tasks"},
		{http.MethodGet, "/deliveries"},
		{http.MethodGet, "/tasks/7"},
		{http.MethodGet, "/sources"},
		{http.MethodGet, "/export/tasks.csv"},
		{http.MethodPost, "/tasks/7/close"},
		{http.MethodPost, "/deliveries/7/send"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.method == http.MethodPost {
				req = httptest.NewRequest(tc.method, tc.path, strings.NewReader("content_hash=x"))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			for _, c := range cookies {
				req.AddCookie(c)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("%s %s with an unreadable flag answered %d, want 503 (D1: the flag read errors -> 503, "+
					"never \"off\"). Body: %.200s", tc.method, tc.path, rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "<main") || strings.Contains(rec.Body.String(), "<table") {
				t.Errorf("the 503 carries page data; D1: no body data")
			}
		})
	}
	if len(ex.calls) != 0 {
		t.Errorf("the executor saw %d call(s) while the flag was unreadable; a verb must not run without a scope", len(ex.calls))
	}
}

// ---- criterion 4: the open routes never read the flag ---------------------------

func TestDemoScope_OpenRoutesNeverReadTheFlag(t *testing.T) {
	_, h, _ := demoDeadServer(t, &captureExec{}) // its own /dev/login already had to work
	for _, tc := range []struct {
		path string
		ok   func(int) bool
		want string
	}{
		{"/healthz", func(c int) bool { return c == http.StatusOK }, "200"},
		{"/static/icon-192.png", func(c int) bool { return c == http.StatusOK }, "200"},
		{"/dev/login?user=salvo", func(c int) bool { return c >= 300 && c < 400 }, "a 3xx"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if !tc.ok(rec.Code) {
			t.Errorf("GET %s with no reachable database answered %d, want %s (criterion 4: /healthz, /static/* and "+
				"/dev/login never read the flag, so the demo wrapper must not sit in front of them)", tc.path, rec.Code, tc.want)
		}
	}
}

// ---- criterion 6: demoSQL ----------------------------------------------------------

var demoPlaceholder = regexp.MustCompile(`\$(\d+)`)

func demoPlaceholders(s string) []int {
	var out []int
	for _, m := range demoPlaceholder.FindAllStringSubmatch(s, -1) {
		n, _ := strconv.Atoi(m[1])
		out = append(out, n)
	}
	return out
}

func demoTestScope() demoScope {
	return demoScope{On: true, Projects: []string{"switchboard", "homelab"}, Accounts: []string{"Ops@Example.com", "b@c.d"}}
}

// demoAddedKinds classifies the binds demoSQL appended after the caller's n
// args. Each is one of: the On bool, the Projects list, the lower-cased
// Accounts list, or the R7 brief pattern.
func demoAddedKinds(t *testing.T, sc demoScope, out []any, n int) map[string]int {
	t.Helper()
	kinds := map[string]int{}
	lowered := make([]string, len(sc.Accounts))
	for i, a := range sc.Accounts {
		lowered[i] = strings.ToLower(a)
	}
	for i := n; i < len(out); i++ {
		switch v := out[i].(type) {
		case bool:
			if v != sc.On {
				t.Errorf("bind $%d = %v, want the scope's On (%v)", i+1, v, sc.On)
			}
			kinds["on"]++
		case []string:
			switch {
			case sameStrings(v, sc.Projects, false):
				kinds["projects"]++
			case sameStrings(v, lowered, false):
				kinds["accounts"]++
			case sameStrings(v, sc.Accounts, false):
				t.Errorf("bind $%d = %q: the accounts are bound as given, but @demo.account compares lower(a.account_email); "+
					"bind them lower-cased (D1: case-insensitive)", i+1, v)
				kinds["accounts"]++
			default:
				t.Errorf("bind $%d = %q is neither the scope's projects nor its lower-cased accounts", i+1, v)
			}
		case string:
			if v != "Morning brief %" {
				t.Errorf("bind $%d = %q, want only scope values (or the R7 pattern 'Morning brief %%')", i+1, v)
			}
			kinds["brief"]++
		default:
			t.Errorf("bind $%d = %#v (%T): demoSQL binds only On (bool), Projects and Accounts ([]string)", i+1, out[i], out[i])
		}
	}
	return kinds
}

func TestDemoSQL_NumbersItsBindsAfterTheCallers(t *testing.T) {
	sc := demoTestScope()
	const prefix = `SELECT t.id FROM tasks t JOIN projects p ON p.id = t.project_id
	  WHERE t.id = $1 AND p.slug = $2 AND t.status = $3 AND `
	caller := []any{int64(7), "switchboard", "ready"}
	q, args, err := demoSQL(sc, prefix+`@demo.task(t)`, append([]any(nil), caller...))
	if err != nil {
		t.Fatalf("demoSQL: %v", err)
	}
	if strings.Contains(q, "@demo.") {
		t.Fatalf("demoSQL left a marker unexpanded:\n%s", q)
	}
	if !strings.HasPrefix(q, prefix) {
		t.Fatalf("demoSQL changed the caller's SQL around the marker:\n%s", q)
	}
	if len(args) < len(caller)+1 {
		t.Fatalf("demoSQL returned %d args, want the caller's 3 plus at least the On bind", len(args))
	}
	for i, a := range caller {
		if args[i] != a {
			t.Errorf("arg %d = %#v, want the caller's %#v unchanged (criterion 6)", i, args[i], a)
		}
	}
	frag := q[len(prefix):]
	ps := demoPlaceholders(frag)
	if len(ps) == 0 {
		t.Fatalf("the @demo.task fragment binds nothing; every fragment is wrapped in (NOT $on OR …):\n%s", frag)
	}
	for _, n := range ps {
		if n < 4 || n > len(args) {
			t.Errorf("the fragment references $%d; with three args already bound its binds are $4..$%d (criterion 6)\n%s",
				n, len(args), frag)
		}
	}
	used := map[int]bool{}
	for _, n := range demoPlaceholders(q) {
		used[n] = true
	}
	for i := len(caller) + 1; i <= len(args); i++ {
		if !used[i] {
			t.Errorf("demoSQL bound $%d (%#v) but the SQL never references it; Postgres refuses an untyped unused "+
				"parameter", i, args[i-1])
		}
	}
	demoAddedKinds(t, sc, args, len(caller))
}

func TestDemoSQL_BindsEachScopeValueOncePerStatement(t *testing.T) {
	sc := demoTestScope()
	q := `SELECT 1 FROM tasks t JOIN projects p ON p.id = t.project_id
	        JOIN deliveries d ON d.task_id = t.id
	        JOIN normalized_messages nm ON true
	        JOIN raw_source_items ri ON ri.id = nm.raw_source_item_id
	        JOIN source_accounts a ON a.id = ri.source_account_id
	        JOIN external_refs er ON er.task_id = t.id
	       WHERE t.id = $1 AND @demo.task(t) AND @demo.project(p) AND @demo.delivery(d)
	         AND @demo.message(nm) AND @demo.account(a) AND @demo.ref(er) AND @demo.task(t)`
	out, args, err := demoSQL(sc, q, []any{int64(1)})
	if err != nil {
		t.Fatalf("demoSQL: %v", err)
	}
	if strings.Contains(out, "@demo.") {
		t.Fatalf("demoSQL left a marker unexpanded:\n%s", out)
	}
	kinds := demoAddedKinds(t, sc, args, 1)
	for _, k := range []string{"on", "projects", "accounts"} {
		if kinds[k] != 1 {
			t.Errorf("the scope's %s is bound %d time(s) in one statement with seven markers, want exactly once "+
				"(criterion 6)", k, kinds[k])
		}
	}
	if kinds["brief"] > 1 {
		t.Errorf("the brief pattern is bound %d times, want at most once", kinds["brief"])
	}
	used := map[int]bool{}
	for _, n := range demoPlaceholders(out) {
		if n > len(args) {
			t.Errorf("the SQL references $%d but only %d args are bound", n, len(args))
		}
		used[n] = true
	}
	for i := 2; i <= len(args); i++ {
		if !used[i] {
			t.Errorf("bind $%d (%#v) is never referenced", i, args[i-1])
		}
	}

	// A statement whose only marker needs only $on binds only $on.
	refOnly, refArgs, err := demoSQL(sc, `SELECT 1 FROM external_refs er WHERE er.task_id = $1 AND @demo.ref(er)`, []any{int64(1)})
	if err != nil {
		t.Fatalf("demoSQL(@demo.ref): %v", err)
	}
	if k := demoAddedKinds(t, sc, refArgs, 1); k["projects"] != 0 || k["accounts"] != 0 || k["on"] != 1 {
		t.Errorf("@demo.ref alone bound %v, want only the On bool: an unused []string bind is an untyped parameter "+
			"Postgres refuses\n%s", k, refOnly)
	}
}

func TestDemoSQL_UnknownMarkerIsAnError(t *testing.T) {
	for _, q := range []string{
		`SELECT 1 FROM tasks t WHERE @demo.bogus(t)`,
		`SELECT 1 FROM tasks t WHERE @demo.tasks(t)`,
		`SELECT 1 FROM tasks t WHERE @demo.task(t) AND @demo.plan(t)`,
	} {
		if out, _, err := demoSQL(demoTestScope(), q, nil); err == nil {
			t.Errorf("demoSQL(%q) returned no error (SQL %q); criterion 6: an unknown marker is an error, never passed "+
				"through or dropped", q, out)
		}
	}
}

// Criterion 5: demo-off SQL is the SAME text, with $on=false; there is no code
// path that skips the predicate.
func TestDemoSQL_OffRunsTheSameText(t *testing.T) {
	q := `SELECT 1 FROM tasks t JOIN projects p ON p.id = t.project_id WHERE @demo.task(t) AND @demo.project(p)`
	on, onArgs, err := demoSQL(demoTestScope(), q, nil)
	if err != nil {
		t.Fatalf("demoSQL(on): %v", err)
	}
	offSc := demoScope{On: false}
	off, offArgs, err := demoSQL(offSc, q, nil)
	if err != nil {
		t.Fatalf("demoSQL(off): %v", err)
	}
	if on != off {
		t.Errorf("demo-off SQL differs from demo-on SQL; criterion 5: the same text runs with $on=false\non:  %s\noff: %s", on, off)
	}
	if len(onArgs) != len(offArgs) {
		t.Errorf("demo-on binds %d values, demo-off %d; the same text takes the same binds", len(onArgs), len(offArgs))
	}
	sawFalse := false
	for _, a := range offArgs {
		if b, ok := a.(bool); ok {
			if b {
				t.Errorf("demo-off bound On=true")
			}
			sawFalse = true
		}
	}
	if !sawFalse {
		t.Errorf("demo-off SQL binds no On=false; the predicate must be present and switched off by its bind")
	}
}

// Criterion 5: each fragment's content, alias-substituted and wrapped in
// (NOT $on OR …).
func TestDemoSQL_FragmentShapes(t *testing.T) {
	sc := demoTestScope()
	notOn := regexp.MustCompile(`(?i)\bNOT\s+\$(\d+)`)
	for _, tc := range []struct {
		marker    string
		must      []string
		mustNot   []string
		hidden    bool // the fragment carries the demo_hidden walk
		checkWalk bool
	}{
		{marker: "@demo.project(zp)", must: []string{"zp.slug", "ANY("}},
		{marker: "@demo.task(zt)", must: []string{"zt.", "NOT LIKE", "Morning brief %", "WITH RECURSIVE", "UNION", "parent_id"},
			mustNot: []string{"UNION ALL"}, hidden: true, checkWalk: true},
		{marker: "@demo.delivery(zd)", must: []string{"zd.channel", "upwork_chat"}, hidden: true},
		{marker: "@demo.account(za)", must: []string{"lower(za.account_email)", "ANY("}},
		{marker: "@demo.message(zm)", must: []string{"zm.channel", "'upwork'", "zm.raw_source_item_id"}},
		{marker: "@demo.ref(zr)", must: []string{"zr.system", "upwork_crm"}},
	} {
		t.Run(tc.marker, func(t *testing.T) {
			const prefix = "SELECT 1 WHERE "
			out, args, err := demoSQL(sc, prefix+tc.marker, nil)
			if err != nil {
				t.Fatalf("demoSQL(%s): %v", tc.marker, err)
			}
			frag := out[len(prefix):]
			// The brief pattern may be bound instead of inlined.
			boundBrief := false
			for _, a := range args {
				if s, ok := a.(string); ok && s == "Morning brief %" {
					boundBrief = true
				}
			}
			for _, m := range tc.must {
				if m == "Morning brief %" && boundBrief {
					continue
				}
				if !strings.Contains(frag, m) {
					t.Errorf("%s expands to a fragment without %q (criterion 5):\n%s", tc.marker, m, frag)
				}
			}
			for _, m := range tc.mustNot {
				if strings.Contains(frag, m) {
					t.Errorf("%s expands to a fragment with %q; criterion 5: the ancestor walk uses UNION so a corrupt "+
						"parent_id cycle terminates:\n%s", tc.marker, m, frag)
				}
			}
			if got := strings.Contains(frag, "demo_hidden"); got != tc.hidden {
				t.Errorf("%s: fragment mentions demo_hidden = %v, want %v (D9: the override lives in @demo.task, and a "+
					"delivery passes it through its task; nothing else reads it)\n%s", tc.marker, got, tc.hidden, frag)
			}
			m := notOn.FindStringSubmatch(frag)
			if m == nil {
				t.Fatalf("%s is not wrapped in (NOT $on OR …):\n%s", tc.marker, frag)
			}
			k, _ := strconv.Atoi(m[1])
			if k < 1 || k > len(args) {
				t.Fatalf("%s: NOT $%d names no bind (%d args)", tc.marker, k, len(args))
			}
			if b, ok := args[k-1].(bool); !ok || b != sc.On {
				t.Errorf("%s: NOT $%d is bound to %#v, want the scope's On bool", tc.marker, k, args[k-1])
			}
		})
	}
}

// demoSortedKeys is a small helper shared with demo_structure_test.go.
func demoSortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Review hardening: an alias that spells a placeholder word is inserted last,
// so it can never be rewritten into a bind or a nested fragment.
func TestDemoSQL_UppercaseAliasIsNotAPlaceholder(t *testing.T) {
	sc := demoTestScope()
	for _, alias := range []string{"PROJECTS", "ACCOUNTS", "BRIEF", "SEQ", "ALIAS_x"} {
		out, _, err := demoSQL(sc, `SELECT 1 FROM tasks `+alias+` WHERE @demo.task(`+alias+`)`, nil)
		if err != nil {
			t.Fatalf("demoSQL(alias %s): %v", alias, err)
		}
		if !strings.Contains(out, alias+".project_id") || !strings.Contains(out, alias+".title NOT LIKE") {
			t.Errorf("alias %s was rewritten inside its own fragment:\n%s", alias, out)
		}
	}
}
