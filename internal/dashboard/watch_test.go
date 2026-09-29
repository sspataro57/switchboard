package dashboard

// watch-json (SWT-101, docs/tickets/watch-json_SPEC.md): the unit halves. Criteria
// 1-5 (the token gate: disabled = 404, wrong or missing = 401, the pass cases, the
// mux's 405, and nothing secret in the logs) and 6-10 (watchSessions: who is
// listed, the name and since, dedup, order, [] never null). ZERO Postgres: the
// gate tests wrap a recording `next` in s.watchAuth, and the Handler() tests use
// the pool-less funnel_test.go construction and never send a request that could
// reach watchJSON (it would need a pool).
//
// IMPOSED SURFACE. The SPEC names every symbol below; where it leaves a detail
// open, this file's choice is marked *. Build exactly this:
//
//	// internal/dashboard/watch.go (new)
//	func (s *Server) SetWatchToken(token string)            // * no return value (main.go ignores one)
//	    // strings.TrimSpace; "" / blank / < 32 bytes after trimming -> disabled
//	    // (one slog.Warn, without the value, when set but too short); else stores
//	    // sha256(token) in s.watchDigest.
//	func (s *Server) watchAuth(next http.Handler) http.Handler
//	    // disabled -> http.NotFound; bad/missing bearer -> 401 text/plain; charset=utf-8,
//	    // WWW-Authenticate: Bearer, body "unauthorized\n"; every reply Cache-Control: no-store.
//	func (s *Server) watchJSON(w http.ResponseWriter, r *http.Request)
//	*type watchSession struct {
//	     Session string `json:"session"`
//	     Since   int64  `json:"since"`
//	 }
//	func watchSessions(secs []boardSection, facts map[int64]lightFacts) (waiting, working []watchSession)
//
//	// internal/dashboard/board.go
//	func (s *Server) boardView(r *http.Request) (secs []boardSection, facts map[int64]lightFacts, renderedAt string, err error)
//
//	// internal/dashboard/lights.go
//	lightFacts.StateUnix int64 // display-only; lightFor never reads it
//
//	// internal/dashboard/demo.go
//	func watchScope(ctx context.Context) context.Context // withDemoScope(ctx, demoScope{})
//	var demoOffRoutes = map[string]string{"GET /watch.json": "<one-line reason>"}
//
//	// internal/dashboard/server.go
//	Server.watchDigest []byte
//	mux.Handle("GET /watch.json", s.watchAuth(http.HandlerFunc(s.watchJSON)))
//
// EXPECTED RED: none of the new symbols exists, so package dashboard's test
// binary compile-FAILS (undefined: watchSession, watchSessions, (*Server).SetWatchToken,
// (*Server).watchAuth, (*Server).watchJSON, (*Server).boardView, watchScope,
// demoOffRoutes, unknown field StateUnix in lightFacts). That is the expected
// failure; once it compiles, each test fails at its assertion until the
// behaviour exists.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"): M3 (drop the
// disabled check) -> TestWatchAuth_DisabledIs404ForEveryRequest; M9 (drop
// no-store) -> TestWatchAuth_WrongOrMissingTokenIs401; M5 (stale in working) ->
// TestWatchSessions_WorkingIsOnlyFreshSessionRows; M6 (no dedup, or working
// wins) -> TestWatchSessions_OneEntryPerSessionOldestAndWaitingWins; M10
// (descending) and M11 (nil slices) -> TestWatchSessions_OrderAndEmptyLists; M12
// (Light.Session) -> TestWatchSessions_NameIsTheStoredNameNeverTheDisplayText.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Compile-time pins of the SPEC's signatures: a different shape is a compile
// error here, not a silent drift.
var (
	_ func(*Server, string)                                                              = (*Server).SetWatchToken
	_ func(*Server, http.Handler) http.Handler                                           = (*Server).watchAuth
	_ func(*Server, http.ResponseWriter, *http.Request)                                  = (*Server).watchJSON
	_ func(*Server, *http.Request) ([]boardSection, map[int64]lightFacts, string, error) = (*Server).boardView
	_ func([]boardSection, map[int64]lightFacts) ([]watchSession, []watchSession)        = watchSessions
	_ func(context.Context) context.Context                                              = watchScope
	_ map[string]string                                                                  = demoOffRoutes
	_ int64                                                                              = lightFacts{}.StateUnix
)

// watchTok is a 64-byte token, the shape `openssl rand -hex 32` produces.
var watchTok = strings.Repeat("9f3a", 16)

// watchShort is 31 bytes: one short of the SPEC's 32-byte floor.
var watchShort = strings.Repeat("k", 31)

// recNext counts the calls that got past the gate.
type recNext struct {
	mu sync.Mutex
	n  int
}

func (h *recNext) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.n++
	h.mu.Unlock()
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("passed"))
}

func (h *recNext) calls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

// watchReq is one request shape against the gate.
type watchReq struct {
	name   string
	target string // path + query
	auth   string // Authorization header; "" = none
	cookie *http.Cookie
}

func (q watchReq) do(h http.Handler) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, q.target, nil)
	if q.auth != "" {
		r.Header.Set("Authorization", q.auth)
	}
	if q.cookie != nil {
		r.AddCookie(q.cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func gatedServer(token string) (*Server, *recNext, http.Handler) {
	s := &Server{}
	s.SetWatchToken(token)
	next := &recNext{}
	return s, next, s.watchAuth(next)
}

// ---- criterion 1: disabled is a 404 for every request ------------------------------

func disabledConfigs() []struct{ name, value string } {
	return []struct{ name, value string }{
		{"unset", ""},
		{"blank", "   "},
		{"31 bytes", watchShort},
		{"31 bytes inside whitespace (trimmed first)", "  " + watchShort + "\n"},
	}
}

func disabledRequests(value string) []watchReq {
	return []watchReq{
		{name: "no header", target: "/watch.json"},
		{name: "bearer of the configured value itself", target: "/watch.json", auth: "Bearer " + strings.TrimSpace(value)},
		{name: "bearer of a real-looking token", target: "/watch.json", auth: "Bearer " + watchTok},
		{name: "bearer, empty", target: "/watch.json", auth: "Bearer "},
	}
}

func TestWatchAuth_DisabledIs404ForEveryRequest(t *testing.T) {
	for _, cfg := range disabledConfigs() {
		_, next, h := gatedServer(cfg.value)
		for _, q := range disabledRequests(cfg.value) {
			rec := q.do(h)
			if rec.Code != http.StatusNotFound {
				t.Errorf("token %s, %s: status %d, want 404. Criterion 1 / D3: an unset, blank or <32-byte "+
					"SWB_WATCH_TOKEN leaves the route disabled, http.NotFound whatever the header says (M3)",
					cfg.name, q.name, rec.Code)
			}
			if strings.Contains(rec.Body.String(), "{") {
				t.Errorf("token %s, %s: the 404 body carries a '{' (%q); criterion 1: no JSON", cfg.name, q.name, rec.Body.String())
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("token %s, %s: Cache-Control = %q, want no-store (D5: every response from the handler, the 404 "+
					"included)", cfg.name, q.name, got)
			}
		}
		if n := next.calls(); n != 0 {
			t.Errorf("token %s: next was called %d times, want 0 (criterion 1: a disabled route never reads the board)", cfg.name, n)
		}
	}
}

// The floor is 32 bytes AFTER trimming, and the trim is what makes a Secret's
// trailing newline harmless (D3).
func TestSetWatchToken_ThirtyTwoBytesIsEnoughAndTheValueIsTrimmed(t *testing.T) {
	tok32 := strings.Repeat("q", 32)
	for _, tc := range []struct{ name, configured, presented string }{
		{"exactly 32 bytes", tok32, tok32},
		{"a Secret value with a trailing newline", watchTok + "\n", watchTok},
		{"surrounding spaces", "  " + watchTok + "  ", watchTok},
	} {
		_, next, h := gatedServer(tc.configured)
		rec := watchReq{name: tc.name, target: "/watch.json", auth: "Bearer " + tc.presented}.do(h)
		if rec.Code != http.StatusOK || next.calls() != 1 {
			t.Errorf("%s: status %d, next called %d times; want the request through (D3: strings.TrimSpace, then a "+
				"32-byte floor)", tc.name, rec.Code, next.calls())
		}
	}
}

// ---- criterion 2: wrong or missing is a 401 with no data ------------------------------

func badRequests() []watchReq {
	basic := base64.StdEncoding.EncodeToString([]byte(watchTok))
	return []watchReq{
		{name: "no Authorization header", target: "/watch.json"},
		{name: "Bearer wrong", target: "/watch.json", auth: "Bearer wrong"},
		{name: "Basic <b64 of the token>", target: "/watch.json", auth: "Basic " + basic},
		{name: "Bearer <token>x", target: "/watch.json", auth: "Bearer " + watchTok + "x"},
		{name: "Bearer <token minus its last byte>", target: "/watch.json", auth: "Bearer " + watchTok[:len(watchTok)-1]},
		{name: "?token=<token>, no header", target: "/watch.json?" + url.Values{"token": {watchTok}}.Encode()},
		{name: "?access_token=<token>, no header", target: "/watch.json?" + url.Values{"access_token": {watchTok}}.Encode()},
		{name: "cookie token=<token>, no header", target: "/watch.json", cookie: &http.Cookie{Name: "token", Value: watchTok}},
		{name: "the bare token, no scheme", target: "/watch.json", auth: watchTok},
	}
}

func TestWatchAuth_WrongOrMissingTokenIs401(t *testing.T) {
	_, next, h := gatedServer(watchTok)
	for _, q := range badRequests() {
		rec := q.do(h)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401 (criterion 2)", q.name, rec.Code)
			continue
		}
		if got := rec.Body.String(); got != "unauthorized\n" {
			t.Errorf("%s: body %q, want exactly \"unauthorized\\n\" (criterion 2: no counts, no names, no JSON)", q.name, got)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
			t.Errorf("%s: WWW-Authenticate = %q, want Bearer (criterion 2, RFC 6750)", q.name, got)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store (criterion 2, M9)", q.name, got)
		}
		if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
			t.Errorf("%s: Content-Type = %q, want text/plain; charset=utf-8 (D3)", q.name, got)
		}
	}
	if n := next.calls(); n != 0 {
		t.Errorf("next was called %d times across the bad requests, want 0 (criterion 2)", n)
	}
}

// ---- criterion 3: the three good spellings ---------------------------------------------

func goodRequests() []watchReq {
	return []watchReq{
		{name: "Bearer <token>", target: "/watch.json", auth: "Bearer " + watchTok},
		{name: "bearer <token> (lowercase scheme, RFC 6750)", target: "/watch.json", auth: "bearer " + watchTok},
		{name: "Bearer  <token>  (extra whitespace)", target: "/watch.json", auth: "Bearer  " + watchTok + " "},
	}
}

func TestWatchAuth_RightTokenCallsNextOnce(t *testing.T) {
	for _, q := range goodRequests() {
		_, next, h := gatedServer(watchTok)
		rec := q.do(h)
		if n := next.calls(); n != 1 {
			t.Errorf("%s: next called %d times, want exactly 1 (criterion 3)", q.name, n)
		}
		if rec.Code != http.StatusOK || rec.Body.String() != "passed" {
			t.Errorf("%s: got %d %q, want next's own 200 \"passed\" (the gate adds nothing to a passed request)",
				q.name, rec.Code, rec.Body.String())
		}
	}
}

// ---- criterion 4 (and the route's registration, M4): through the real mux ----------------

// watchHandler is funnel_test.go's construction (dev auth, NO pool) with the
// token wired the way cmd/dashboard wires it: before Handler().
func watchHandler(t *testing.T, token string) http.Handler {
	t.Helper()
	auth, err := NewAuth(t.Context(), "", "", "", "") // issuer "" -> dev mode, no network
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	srv, err := NewServer(nil, nil, auth)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetWatchToken(token)
	return srv.Handler()
}

func TestWatchRoute_OnlyGetAndHeadAreServed(t *testing.T) {
	h := watchHandler(t, watchTok)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		r := httptest.NewRequest(m, "/watch.json", strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer "+watchTok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /watch.json with the right token: status %d, want 405 (criterion 4 / D5: the mux pattern "+
				"GET /watch.json serves GET and HEAD only)", m, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "{") {
			t.Errorf("%s /watch.json: the 405 body carries JSON (%q); criterion 4", m, rec.Body.String())
		}
	}
}

// D3: "The route is registered anyway": without it, GET /watch.json falls through
// to GET / -> s.auth.Require -> a 302 to the login page. And it is registered
// OUTSIDE the session layer (M4): a courier has no cookie.
func TestWatchRoute_IsRegisteredOutsideTheSessionLayer(t *testing.T) {
	for _, tc := range []struct {
		name, token, auth string
		want              int
	}{
		{"disabled, no header", "", "", http.StatusNotFound},
		{"disabled, a bearer", "", "Bearer " + watchTok, http.StatusNotFound},
		{"enabled, no header", watchTok, "", http.StatusUnauthorized},
		{"enabled, a wrong bearer", watchTok, "Bearer wrong", http.StatusUnauthorized},
	} {
		h := watchHandler(t, tc.token)
		r := httptest.NewRequest(http.MethodGet, "/watch.json", nil)
		if tc.auth != "" {
			r.Header.Set("Authorization", tc.auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != tc.want {
			t.Errorf("%s: GET /watch.json (no session cookie) = %d (Location %q), want %d. D3: the route is registered "+
				"even when disabled and never behind s.auth.Require, so the courier sees the gate's own answer, not "+
				"a login redirect (M4)", tc.name, rec.Code, rec.Header().Get("Location"), tc.want)
		}
	}
}

// ---- criterion 5: the token is never logged ---------------------------------------------

// captureSlog routes slog (and, through it, the log package) into a buffer for
// the rest of the test.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func TestWatchAuth_NeverLogsTheTokenOrThePresentedHeader(t *testing.T) {
	buf := captureSlog(t)

	// The short value: exactly one warning, which does not carry the value (D3).
	before := strings.Count(buf.String(), "level=WARN")
	(&Server{}).SetWatchToken(watchShort)
	if n := strings.Count(buf.String(), "level=WARN") - before; n != 1 {
		t.Errorf("SetWatchToken(<31 bytes>) logged %d warnings, want exactly 1 (D3: a single slog.Warn says the value "+
			"is too short)\n%s", n, buf.String())
	}

	// Criteria 1-3, replayed under capture.
	for _, cfg := range disabledConfigs() {
		_, _, h := gatedServer(cfg.value)
		for _, q := range disabledRequests(cfg.value) {
			q.do(h)
		}
	}
	_, _, h := gatedServer(watchTok)
	for _, q := range append(badRequests(), goodRequests()...) {
		q.do(h)
	}
	_, _, h = gatedServer(watchTok + "\n")
	goodRequests()[0].do(h)

	out := buf.String()
	for _, secret := range []struct{ name, v string }{
		{"the token", watchTok},
		{"the short configured value", watchShort},
		{"a presented near-miss (token + x)", watchTok + "x"},
		{"a presented near-miss (token minus a byte)", watchTok[:len(watchTok)-1]},
		{"the token's first half", watchTok[:len(watchTok)/2]},
	} {
		if strings.Contains(out, secret.v) {
			t.Errorf("the logs carry %s. Criterion 5 / D3: the token and the presented header are never logged\n%s",
				secret.name, out)
		}
	}
}

// ---- criteria 6-10: watchSessions --------------------------------------------------------

// wsRow builds a board row the way listTasks does: the light from the REAL
// lightFor over the same facts watchSessions reads.
func wsRow(id int64, status string, f lightFacts) taskRow {
	return taskRow{ID: id, Status: status, AssigneeType: "human", Light: lightFor(status, f)}
}

type wsFixture struct {
	rows  []taskRow
	facts map[int64]lightFacts
}

func (x *wsFixture) add(id int64, status string, f lightFacts) {
	if x.facts == nil {
		x.facts = map[int64]lightFacts{}
	}
	x.facts[id] = f
	x.rows = append(x.rows, wsRow(id, status, f))
}

// sections groups the rows with the board's own boardSections, so the input is
// exactly the shape boardView returns.
func (x *wsFixture) sections() []boardSection { return boardSections(x.rows) }

func waitingF(session string, since int64) lightFacts {
	return lightFacts{State: "needs_input", Session: session, StateUnix: since, StateAt: "2026-09-29 09:00"}
}

func workingF(session string, since int64) lightFacts {
	return lightFacts{State: "working", Session: session, StateUnix: since, StateAt: "2026-09-29 09:00"}
}

func wsNames(l []watchSession) []string {
	out := []string{}
	for _, s := range l {
		out = append(out, s.Session)
	}
	return out
}

func TestWatchSessions_WaitingIsOnlySessionNeedsInputRows(t *testing.T) {
	var x wsFixture
	x.add(1, "ready", waitingF("s-wait", 100))
	x.add(2, "blocked", waitingF("s-wait-blocked", 110)) // a session red on a blocked task is still a session red
	// A worker parked on a question: red, counted in need_you, but it carries no
	// session tag. Its facts carry a leftover name on purpose: the light, not
	// the fact, decides membership (D2).
	x.add(3, "needs_feedback", lightFacts{State: "needs_input", Session: "leftover-name", StateUnix: 90})
	x.add(4, "blocked", lightFacts{}) // grey dependency block
	x.add(5, "ready", lightFacts{})   // queued
	x.add(6, "closed", lightFacts{ClosedToday: true})
	waiting, working := watchSessions(x.sections(), x.facts)
	if want := []watchSession{{Session: "s-wait", Since: 100}, {Session: "s-wait-blocked", Since: 110}}; !reflect.DeepEqual(waiting, want) {
		t.Errorf("waiting = %+v, want %+v (criterion 6: only the session needs_input rows; a needs_feedback red has "+
			"no session tag and is not listed)", waiting, want)
	}
	if len(working) != 0 {
		t.Errorf("working = %+v, want empty", working)
	}
}

// The light decides, not the section: a waiting session whose task is also
// incoming sits in the incoming section, and is still listed (D2).
func TestWatchSessions_FollowTheLightNotTheSection(t *testing.T) {
	r := wsRow(7, "ready", waitingF("s-inbox", 300))
	r.Incoming = incomingMessage
	w := wsRow(8, "ready", workingF("s-inbox-work", 310))
	w.Incoming = incomingPRReview
	facts := map[int64]lightFacts{7: waitingF("s-inbox", 300), 8: workingF("s-inbox-work", 310)}
	secs := boardSections([]taskRow{r, w})
	if len(secs) != 1 || secs[0].Key != "incoming" {
		t.Fatalf("CONTROL: the rows landed in %v, want the incoming section only", secs)
	}
	waiting, working := watchSessions(secs, facts)
	if !reflect.DeepEqual(waiting, []watchSession{{Session: "s-inbox", Since: 300}}) ||
		!reflect.DeepEqual(working, []watchSession{{Session: "s-inbox-work", Since: 310}}) {
		t.Errorf("waiting = %+v, working = %+v; D2: membership follows each row's light, whatever its section", waiting, working)
	}
}

func TestWatchSessions_WorkingIsOnlyFreshSessionRows(t *testing.T) {
	var x wsFixture
	x.add(1, "ready", workingF("s-work", 200))
	stale := workingF("s-stale", 50)
	stale.Stale = true
	x.add(2, "ready", stale)
	// Worker rows: yellow by status, no session tag. The leftover name in the
	// facts must not leak them in.
	x.add(3, "in_progress", lightFacts{State: "working", Session: "worker-leftover", StateUnix: 10})
	x.add(4, "claimed", lightFacts{})
	x.add(5, "pr_open", lightFacts{})
	waiting, working := watchSessions(x.sections(), x.facts)
	if want := []watchSession{{Session: "s-work", Since: 200}}; !reflect.DeepEqual(working, want) {
		t.Errorf("working = %+v, want %+v (criterion 7: only a fresh session working row; a stale lease is the "+
			"board's \"no signal\" and must not keep the watch yellow (M5); claimed/in_progress worker rows are not "+
			"listed)", working, want)
	}
	if len(waiting) != 0 {
		t.Errorf("waiting = %+v, want empty", waiting)
	}
	// And the tally still counts all four yellows and the stale ring: len(working)
	// < in_flight is expected (D2).
	if got := boardTallies(x.sections()).InFlight; got != 5 {
		t.Errorf("CONTROL: boardTallies.InFlight = %d, want 5 (the header counts working and stale alike)", got)
	}
}

func TestWatchSessions_NameIsTheStoredNameNeverTheDisplayText(t *testing.T) {
	var x wsFixture
	x.add(1, "ready", waitingF("", 100))         // a pre-0036 marker: lightFor shows "session unknown"
	x.add(2, "ready", workingF("", 110))         // the same, yellow
	x.add(3, "ready", waitingF("unknown", 120))  // a real window called "unknown"
	x.add(4, "ready", workingF("unknown2", 130)) // a real window, yellow
	if x.rows[0].Light.Session != "session unknown" {
		t.Fatalf("CONTROL: lightFor's tag for a nameless marker is %q, want \"session unknown\"", x.rows[0].Light.Session)
	}
	waiting, working := watchSessions(x.sections(), x.facts)
	if want := []watchSession{{Session: "unknown", Since: 120}}; !reflect.DeepEqual(waiting, want) {
		t.Errorf("waiting = %+v, want %+v (criterion 8: an empty stored name is left out; the name is "+
			"facts[id].Session, never Light.Session's \"session unknown\" (M12))", waiting, want)
	}
	if want := []watchSession{{Session: "unknown2", Since: 130}}; !reflect.DeepEqual(working, want) {
		t.Errorf("working = %+v, want %+v (criterion 8)", working, want)
	}
}

func TestWatchSessions_OneEntryPerSessionOldestAndWaitingWins(t *testing.T) {
	// The newer task has the LOWER id, so boardSections meets it first: a
	// keep-the-first dedup keeps 200 and is caught.
	var a wsFixture
	a.add(1, "ready", waitingF("s", 200))
	a.add(2, "blocked", waitingF("s", 100))
	waiting, _ := watchSessions(a.sections(), a.facts)
	if want := []watchSession{{Session: "s", Since: 100}}; !reflect.DeepEqual(waiting, want) {
		t.Errorf("s waiting on two tasks (since 200, 100): waiting = %+v, want %+v (criterion 9: one entry per "+
			"name, the OLDEST since (M6))", waiting, want)
	}

	var b wsFixture
	b.add(1, "ready", workingF("s", 100))
	b.add(2, "ready", waitingF("s", 300))
	b.add(3, "ready", workingF("t", 150))
	b.add(4, "ready", workingF("t", 140))
	waiting, working := watchSessions(b.sections(), b.facts)
	if want := []watchSession{{Session: "s", Since: 300}}; !reflect.DeepEqual(waiting, want) {
		t.Errorf("s waiting (300) and working (100): waiting = %+v, want %+v (criterion 9: waiting wins, with its "+
			"own since)", waiting, want)
	}
	if want := []watchSession{{Session: "t", Since: 140}}; !reflect.DeepEqual(working, want) {
		t.Errorf("working = %+v, want %+v (criterion 9: a name in waiting is removed from working; t deduped to "+
			"its oldest (M6))", working, want)
	}
}

func TestWatchSessions_OrderAndEmptyLists(t *testing.T) {
	// ids chosen so id order is neither since order nor name order.
	var x wsFixture
	x.add(1, "ready", waitingF("b", 50))
	x.add(2, "ready", waitingF("c", 10))
	x.add(3, "ready", waitingF("a", 50))
	x.add(4, "ready", waitingF("d", 70))
	x.add(5, "ready", workingF("z", 5))
	x.add(6, "ready", workingF("y", 5))
	x.add(7, "ready", workingF("x", 900))
	waiting, working := watchSessions(x.sections(), x.facts)
	if got, want := wsNames(waiting), []string{"c", "a", "b", "d"}; !reflect.DeepEqual(got, want) {
		t.Errorf("waiting order = %v, want %v (criterion 10: since ascending, then name ascending (M10))", got, want)
	}
	if got, want := wsNames(working), []string{"y", "z", "x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("working order = %v, want %v (criterion 10)", got, want)
	}

	var quiet wsFixture
	quiet.add(1, "ready", lightFacts{})
	quiet.add(2, "needs_feedback", lightFacts{})
	for _, tc := range []struct {
		name  string
		secs  []boardSection
		facts map[int64]lightFacts
	}{
		{"no rows at all", nil, nil},
		{"rows, none listable", quiet.sections(), quiet.facts},
	} {
		w, k := watchSessions(tc.secs, tc.facts)
		for _, l := range []struct {
			name string
			v    []watchSession
		}{{"waiting", w}, {"working", k}} {
			b, err := json.Marshal(l.v)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(b) != "[]" {
				t.Errorf("%s: %s marshals as %s, want [] (criterion 10: never null (M11))", tc.name, l.name, b)
			}
		}
	}
}

// The payload's element shape: {"session": string, "since": int64}, Unix seconds.
func TestWatchSession_MarshalsAsSessionAndSince(t *testing.T) {
	b, err := json.Marshal([]watchSession{{Session: "pebble", Since: 1790000000}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `[{"session":"pebble","since":1790000000}]`; string(b) != want {
		t.Errorf("watchSession marshals as %s, want %s (SPEC Source / API: the courier reads session and since)", b, want)
	}
}

// D2: StateUnix is display-only. lightFor gives the same light with or without it.
func TestLightFor_IgnoresStateUnix(t *testing.T) {
	for _, status := range []string{"ready", "blocked", "holding", "in_progress", "needs_feedback", "closed"} {
		for _, f := range []lightFacts{waitingF("s", 0), workingF("s", 0), {State: "working", Session: "s", Stale: true}, {}} {
			g := f
			g.StateUnix = 1790000000
			if lightFor(status, f) != lightFor(status, g) {
				t.Errorf("lightFor(%q) changes with StateUnix (%+v vs %+v); D2: StateUnix is display-only",
					status, lightFor(status, f), lightFor(status, g))
			}
		}
	}
}

// ---- D4 mechanics: watchScope is OFF, whatever the context carried ---------------------------

func TestWatchScope_IsDemoOff(t *testing.T) {
	bare := demoScopeFrom(watchScope(context.Background()))
	if bare.On {
		t.Errorf("demoScopeFrom(watchScope(ctx)) = %+v, want off. D4: without an explicit off scope, demoScopeFrom "+
			"defaults to ON with empty lists and every watch count would be 0 (M7)", bare)
	}
	on := withDemoScope(context.Background(), demoScope{On: true, Projects: []string{"x"}})
	if sc := demoScopeFrom(watchScope(on)); sc.On || len(sc.Projects) != 0 || len(sc.Accounts) != 0 {
		t.Errorf("watchScope over an ON context gives %+v, want demoScope{} (D4: the watch always shows the real board)", sc)
	}
}
