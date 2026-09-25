package dashboard

// board-streaming (SWT-89, docs/tickets/board-streaming_SPEC.md): the unit
// halves that need the NEW symbols — criterion 4 (coverage: what the board reads
// is either triggered or ticked), 6 (the coalescer), 7 (the fan-out), 11 (the
// stream handler) and 16 (boardVersion), plus S8's constants. ZERO network,
// ZERO Postgres: the hub is never Run here, and the handler is served by an
// httptest.Server around (*Server).boardStream alone.
//
// Criterion 4 lives here, not in board_live_structure_test.go, because it reads
// the real boardNotifyTables / boardTickOnlyTables values rather than parsing
// them out of live.go; the structure file stays compilable before live.go
// exists.
//
// IMPOSED SURFACE. The SPEC names NewBoardHub, Run, Subscribe, Listening,
// Subscribers, SetBoardHub, boardStream, boardVersion, the consts and the two
// table lists; it asks for "a small config struct whose zero value means the
// consts" and a coalescer driven by "an injected timer and clock" without
// spelling them. This file's spelling of what the SPEC leaves open is marked *:
//
//	// live.go
//	const boardChannel       = "board_changed"
//	const boardLiveDebounce  = 250 * time.Millisecond
//	const boardLiveMinGap    = 2 * time.Second
//	const boardLiveTick      = 60 * time.Second
//	const boardLiveHeartbeat = 20 * time.Second
//	const boardLiveRetry     = 5 * time.Second
//	var boardNotifyTables   = []string{"tasks", "task_dismissals", "classify_promotions", "external_refs"}
//	var boardTickOnlyTables = map[string]string{"projects": "…", "normalized_messages": "…"}
//	var boardVersion string // hex(sha256(embedded templates/tasks.html))[:12]
//
//	*type boardLiveConfig struct{ Debounce, MinGap, Tick, Heartbeat, Retry time.Duration }
//	 // a zero field means its const
//
//	*func newBoardCoalescer(cfg boardLiveConfig, now time.Time) *boardCoalescer
//	*func (c *boardCoalescer) notify(now time.Time)      // a NOTIFY arrived at now
//	*func (c *boardCoalescer) wake() time.Time            // the next instant fire must be called; never zero (the tick)
//	*func (c *boardCoalescer) fire(now time.Time) bool    // called at wake(); true = broadcast now
//
//	func NewBoardHub(pool *pgxpool.Pool) *BoardHub        // == newBoardHubWithConfig(pool, boardLiveConfig{})
//	*func newBoardHubWithConfig(pool *pgxpool.Pool, cfg boardLiveConfig) *BoardHub
//	func (h *BoardHub) Run(ctx context.Context) error
//	func (h *BoardHub) Subscribe() (signals <-chan T, done <-chan struct{}, cancel func())
//	 // signals has capacity 1 (T is the implementer's; the tests never name it);
//	 // done closes when the hub stops listening; cancel unsubscribes.
//	func (h *BoardHub) Listening() bool
//	func (h *BoardHub) Subscribers() int
//	*func (h *BoardHub) broadcast()          // one non-blocking signal to every subscriber, now
//	*func (h *BoardHub) setListening(v bool) // true -> false closes every subscriber's done
//	func (s *Server) SetBoardHub(h *BoardHub)
//	func (s *Server) boardStream(w http.ResponseWriter, r *http.Request)
//
// The "fake hub" of criterion 11 is a real *BoardHub that is never Run: the test
// flips it listening and broadcasts by hand through the two unexported hooks.
//
// GREENFIELD NOTE, EXPECTED RED: live.go does not exist, so package dashboard's
// test binary compile-FAILS on every symbol above. That is the expected failure;
// the text-only structure tests (board_live_structure_test.go) and the Part 7
// amendments are shown failing on their own by moving this file aside.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - the coalescer broadcasts once per notification -> ...CoalescesABurst.
//   - the coalescer drops the trailing edge -> ...AlwaysAnnouncesTheLastChange.
//   - a subscriber send made blocking -> ...SlowSubscriberNeverDelaysAnother.
//   - the hub does not close subscribers when blind -> ...BlindHubClosesEverySubscriber, ...EndsWhenTheHubGoesBlind.
//   - the handler omits X-Accel-Buffering: no -> ...HeadersAndRetry.
//   - JOIN deliveries added to boardLightFacts, unclassified -> ...EveryBoardTableIsTriggeredOrTicked.
//   - a trigger on normalized_messages -> ...MigrationTriggersExactlyTheNotifyTables.
//   - boardVersion hard-coded -> ...BoardVersionIsTheTemplateHash.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- S8: the constants ---------------------------------------------------------

func TestBoardLive_Constants(t *testing.T) {
	if boardChannel != "board_changed" {
		t.Errorf("boardChannel = %q, want \"board_changed\" (S2: a board-only channel, never task_events)", boardChannel)
	}
	for _, c := range []struct {
		name      string
		got, want time.Duration
		why       string
	}{
		{"boardLiveDebounce", boardLiveDebounce, 250 * time.Millisecond, "S4: the leading edge"},
		{"boardLiveMinGap", boardLiveMinGap, 2 * time.Second, "S4: at most one broadcast per 2 s during a burst"},
		{"boardLiveTick", boardLiveTick, 60 * time.Second, "S5: time-only facts and the safety net"},
		{"boardLiveHeartbeat", boardLiveHeartbeat, 20 * time.Second, "S8: under ingress-nginx's 60 s read timeout"},
		{"boardLiveRetry", boardLiveRetry, 5 * time.Second, "S8: the SSE retry: and the browser's re-open delay"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v (%s; never a URL value, D15)", c.name, c.got, c.want, c.why)
		}
	}
}

// ---- criterion 4: every table the board reads is triggered or ticked -------------

// boardSQLTables returns every identifier that follows FROM or JOIN in the SQL
// string literals of fn (board.go), skipping subqueries (`FROM (SELECT`) and
// function calls (`EXTRACT(EPOCH FROM now() - …)`).
func boardSQLTables(t *testing.T, fn string) []string {
	t.Helper()
	src, err := os.ReadFile("board.go")
	if err != nil {
		t.Fatalf("read board.go: %v", err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "board.go", src, 0)
	if err != nil {
		t.Fatalf("parse board.go: %v", err)
	}
	re := regexp.MustCompile(`\b(?:FROM|JOIN)\s+([A-Za-z_][A-Za-z0-9_.]*)(\s*\()?`)
	var out []string
	found := false
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn || fd.Body == nil {
			continue
		}
		found = true
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, m := range re.FindAllStringSubmatch(s, -1) {
				if m[2] != "" {
					continue // a function call, not a table
				}
				out = append(out, m[1])
			}
			return true
		})
	}
	if !found {
		t.Fatalf("board.go declares no %s (criterion 4 scans its SQL)", fn)
	}
	return out
}

func TestBoardLive_EveryBoardTableIsTriggeredOrTicked(t *testing.T) {
	notify := map[string]bool{}
	for _, tb := range boardNotifyTables {
		if notify[tb] {
			t.Errorf("boardNotifyTables lists %q twice", tb)
		}
		notify[tb] = true
	}
	want := []string{"tasks", "task_dismissals", "classify_promotions", "external_refs"}
	got := append([]string(nil), boardNotifyTables...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("boardNotifyTables = %v, want exactly %v (S2: the four tables whose rows change what the board shows)",
			boardNotifyTables, want)
	}
	for _, tb := range []string{"projects", "normalized_messages"} {
		reason, ok := boardTickOnlyTables[tb]
		if !ok {
			t.Errorf("boardTickOnlyTables has no %q entry (criterion 4: it carries a one-line reason)", tb)
			continue
		}
		if strings.TrimSpace(reason) == "" || strings.Contains(reason, "\n") {
			t.Errorf("boardTickOnlyTables[%q] = %q, want a non-empty ONE-line reason (criterion 4)", tb, reason)
		}
	}
	for tb := range boardTickOnlyTables {
		if notify[tb] {
			t.Errorf("%q is in BOTH boardNotifyTables and boardTickOnlyTables; each table is in exactly one list", tb)
		}
	}

	seen := map[string][]string{}
	for _, fn := range []string{"boardQuery", "reopenMarkers", "boardLightFacts", "listTasks"} {
		for _, tb := range boardSQLTables(t, fn) {
			seen[tb] = append(seen[tb], fn)
		}
	}
	// CONTROL: the scan must see the board's own table, or every check below is
	// vacuous.
	if len(seen["tasks"]) == 0 || len(seen) < 4 {
		t.Fatalf("CONTROL: the FROM/JOIN scan found %v; it is not reading the board's SQL", seen)
	}
	for tb, fns := range seen {
		_, tick := boardTickOnlyTables[tb]
		switch {
		case notify[tb] && tick:
			t.Errorf("%s (read by %v) is in both lists", tb, fns)
		case !notify[tb] && !tick:
			t.Errorf("%s is read by %v but is in neither boardNotifyTables nor boardTickOnlyTables. Criterion 4: a "+
				"board ticket that reads a new table decides whether its writes wake the board (a trigger in a "+
				"migration + boardNotifyTables) or wait for the 60 s tick (boardTickOnlyTables, with a reason)", tb, fns)
		}
	}
}

// migration0044 is the text of migrations/0044_*.sql (exactly one file).
func migration0044(t *testing.T) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "0044_*.sql"))
	if err != nil {
		t.Fatalf("glob migrations/0044_*.sql: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("migrations/0044_*.sql matches %d files %v, want exactly one (criterion 1: "+
			"migrations/0044_board_changed_notify.sql)", len(files), files)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read %s: %v", files[0], err)
	}
	return string(b)
}

// sqlCode is s with every `--` comment removed, so prose cannot satisfy or trip
// a scan.
func sqlCode(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func TestBoardLive_MigrationTriggersExactlyTheNotifyTables(t *testing.T) {
	code := sqlCode(migration0044(t))
	triggered := map[string]bool{}
	for _, m := range createTriggerRE.FindAllStringSubmatch(code, -1) {
		triggered[m[3]] = true
	}
	var got []string
	for tb := range triggered {
		got = append(got, tb)
	}
	want := append([]string(nil), boardNotifyTables...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("migration 0044 creates triggers on %v, boardNotifyTables is %v; criterion 4(c): the two lists are "+
			"one decision and must be equal", got, want)
	}
}

// ---- criterion 6: the coalescer (virtual time, no sleeps) -------------------------

// blSimulate drives the coalescer through notes (sorted) until `until` and
// returns the broadcast instants. Notifications due at or before the next wake
// are delivered first.
func blSimulate(t *testing.T, start, until time.Time, notes []time.Time) []time.Time {
	t.Helper()
	c := newBoardCoalescer(boardLiveConfig{}, start)
	var out []time.Time
	now, i := start, 0
	for steps := 0; steps < 1_000_000; steps++ {
		w := c.wake()
		if w.IsZero() {
			t.Fatalf("wake() returned the zero time %v into the run; S5: the tick is always pending", now.Sub(start))
		}
		if i < len(notes) && !notes[i].After(w) {
			if notes[i].After(now) {
				now = notes[i]
			}
			c.notify(notes[i])
			i++
			continue
		}
		if w.After(until) {
			return out
		}
		if w.Before(now) {
			w = now
		}
		if c.fire(w) {
			out = append(out, w)
		}
		now = w
	}
	t.Fatalf("the coalescer never advanced past %v: wake() keeps naming an instant at which fire() declines", now.Sub(start))
	return nil
}

func blOffsets(start time.Time, ts []time.Time) []string {
	var s []string
	for _, x := range ts {
		s = append(s, x.Sub(start).String())
	}
	return s
}

const blSlack = 10 * time.Millisecond

func TestBoardCoalescer_OneNotificationBroadcastsOnceAtTheDebounce(t *testing.T) {
	start := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	note := start.Add(time.Second)
	out := blSimulate(t, start, start.Add(30*time.Second), []time.Time{note})
	if len(out) != 1 {
		t.Fatalf("one notification produced %d broadcasts %v in 30 s, want exactly 1 (criterion 6a)", len(out), blOffsets(start, out))
	}
	if out[0].Before(note.Add(boardLiveDebounce)) {
		t.Errorf("the broadcast came %v after the notification, before the %v debounce (criterion 6a: at debounce, not before)",
			out[0].Sub(note), boardLiveDebounce)
	}
	if out[0].After(note.Add(boardLiveDebounce + blSlack)) {
		t.Errorf("the broadcast came %v after the notification, want %v: a single change reaches the tablet in about "+
			"250 ms plus one render (S4)", out[0].Sub(note), boardLiveDebounce)
	}
}

func blBurst(start time.Time) []time.Time {
	var notes []time.Time
	for k := 0; k < 100; k++ { // 100 notifications evenly over 10 s
		notes = append(notes, start.Add(time.Second+time.Duration(k)*100*time.Millisecond))
	}
	return notes
}

func TestBoardCoalescer_CoalescesABurst(t *testing.T) {
	start := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	notes := blBurst(start)
	last := notes[len(notes)-1]
	out := blSimulate(t, start, last.Add(10*time.Second), notes)
	most := int(math.Ceil(float64(10*time.Second)/float64(boardLiveMinGap))) + 2
	if len(out) > most {
		t.Errorf("100 notifications over 10 s produced %d broadcasts, want at most ceil(10s / %v) + 2 = %d (criterion 6b: "+
			"a capture pass costs each tab about six renders, not hundreds). Broadcasts at %v",
			len(out), boardLiveMinGap, most, blOffsets(start, out))
	}
	if len(out) < 5 {
		t.Errorf("100 notifications over 10 s produced %d broadcasts, want at least 5 (criterion 6b: one per %v while "+
			"notifications keep arriving). Broadcasts at %v", len(out), boardLiveMinGap, blOffsets(start, out))
	}
}

func TestBoardCoalescer_AlwaysAnnouncesTheLastChange(t *testing.T) {
	start := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	notes := blBurst(start)
	last := notes[len(notes)-1]
	out := blSimulate(t, start, last.Add(10*time.Second), notes)
	if len(out) == 0 {
		t.Fatalf("a burst produced no broadcast at all (criterion 6)")
	}
	tail := out[len(out)-1]
	if tail.Before(last) {
		t.Errorf("the last broadcast (%v) comes BEFORE the last notification (%v); criterion 6c: there is always a "+
			"trailing broadcast, so no change is left unannounced. Broadcasts at %v",
			tail.Sub(start), last.Sub(start), blOffsets(start, out))
	}
	if tail.After(last.Add(boardLiveMinGap + boardLiveDebounce + blSlack)) {
		t.Errorf("the trailing broadcast comes %v after the last notification, want within minGap + debounce (%v)",
			tail.Sub(last), boardLiveMinGap+boardLiveDebounce)
	}
}

func TestBoardCoalescer_TicksWithNothingWritten(t *testing.T) {
	start := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	out := blSimulate(t, start, start.Add(3*boardLiveTick+boardLiveTick/2), nil)
	if len(out) != 3 {
		t.Fatalf("with no notification for 3.5 ticks the coalescer broadcast %d times %v, want exactly 3 — one per %v "+
			"(criterion 6d / S5: elapsed minutes, the stale ring and the midnight rollover move with nothing written)",
			len(out), blOffsets(start, out), boardLiveTick)
	}
	for k, b := range out {
		want := start.Add(time.Duration(k+1) * boardLiveTick)
		if d := b.Sub(want); d < -blSlack || d > blSlack {
			t.Errorf("tick %d broadcast at %v, want %v", k+1, b.Sub(start), want.Sub(start))
		}
	}
}

// ---- criterion 7: the fan-out ------------------------------------------------------

// blBroadcast runs n broadcasts off the test goroutine and fails if they do not
// return within a second: a blocking send must fail a test, never hang the binary.
func blBroadcast(t *testing.T, h *BoardHub, n int) {
	t.Helper()
	sent := make(chan struct{})
	go func() {
		for i := 0; i < n; i++ {
			h.broadcast()
		}
		close(sent)
	}()
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatalf("%d broadcasts did not return within 1 s. Criterion 7a / S8: sends never block; a slow or dead "+
			"client can never stall the hub or the others", n)
	}
}

func TestBoardHub_SlowSubscriberNeverDelaysAnother(t *testing.T) {
	h := NewBoardHub(nil)
	h.setListening(true)
	// The cancels are NOT deferred: under the "blocking send" mutation the stuck
	// broadcast holds the hub, and a deferred cancel would hang the binary instead
	// of failing this test.
	_, _, cancelA := h.Subscribe() // A never reads
	b, _, cancelB := h.Subscribe()

	blBroadcast(t, h, 5) // while A never reads
	select {
	case <-b:
	case <-time.After(time.Second):
		t.Fatalf("subscriber B received nothing within 1 s of five broadcasts (criterion 7a)")
	}
	cancelA()
	cancelB()
}

func TestBoardHub_ASubscriberHoldsAtMostOnePendingSignal(t *testing.T) {
	h := NewBoardHub(nil)
	h.setListening(true)
	a, _, cancel := h.Subscribe()
	if cap(a) != 1 {
		t.Errorf("a subscriber's channel has capacity %d, want 1 (S8: one pending signal means \"re-fetch\")", cap(a))
	}
	blBroadcast(t, h, 5)
	if len(a) != 1 {
		t.Errorf("after five broadcasts an unread subscriber holds %d pending signals, want exactly 1 (criterion 7b)", len(a))
	}
	cancel()
}

func TestBoardHub_UnsubscribeLeavesNoSubscriber(t *testing.T) {
	h := NewBoardHub(nil)
	h.setListening(true)
	_, _, c1 := h.Subscribe()
	_, _, c2 := h.Subscribe()
	if n := h.Subscribers(); n != 2 {
		t.Errorf("CONTROL: two open subscriptions report Subscribers() = %d", n)
	}
	c1()
	c2()
	for i := 0; i < 200; i++ {
		_, _, cancel := h.Subscribe()
		blBroadcast(t, h, 1)
		cancel()
	}
	if n := h.Subscribers(); n != 0 {
		t.Errorf("after 200 subscribe/unsubscribe cycles Subscribers() = %d, want 0 (criterion 7c: the handler's defer "+
			"unsubscribes, and a leak is one goroutine-held channel per closed tab)", n)
	}
}

func TestBoardHub_BlindHubClosesEverySubscriber(t *testing.T) {
	h := NewBoardHub(nil)
	h.setListening(true)
	if !h.Listening() {
		t.Fatalf("CONTROL: setListening(true) did not make Listening() true")
	}
	var dones []<-chan struct{}
	for i := 0; i < 3; i++ {
		_, done, cancel := h.Subscribe()
		defer cancel()
		dones = append(dones, done)
	}
	h.setListening(false)
	if h.Listening() {
		t.Errorf("setListening(false) left Listening() true")
	}
	for i, d := range dones {
		select {
		case <-d:
		case <-time.After(time.Second):
			t.Errorf("subscriber %d's done channel is still open 1 s after the hub went blind. Criterion 7d / S8: a blind "+
				"hub ends every open stream, so browsers fall back to polling honestly", i)
		}
	}
}

// ---- criterion 11: the stream handler ------------------------------------------------

func TestBoardStream_RefusesWithoutAListeningHub(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    *Server
	}{
		{"no hub", &Server{}},
		{"hub not listening", func() *Server { s := &Server{}; s.SetBoardHub(NewBoardHub(nil)); return s }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.s.boardStream(rec, httptest.NewRequest(http.MethodGet, "/tasks/stream", nil))
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("boardStream answered %d, want 503 (criterion 11a: the board falls back to polling)", rec.Code)
			}
			if rec.Header().Get("Retry-After") == "" {
				t.Errorf("the 503 carries no Retry-After (criterion 11a)")
			}
			if strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") {
				t.Errorf("the 503 claims Content-Type text/event-stream; a refused stream is not a stream (criterion 11a)")
			}
		})
	}
}

type blStream struct {
	resp     *http.Response
	lines    chan string
	cancel   context.CancelFunc
	returned chan struct{}
	close    func()
}

// blOpenStream serves boardStream alone (no auth, no mux) and opens one GET.
func blOpenStream(t *testing.T, s *Server) *blStream {
	t.Helper()
	returned := make(chan struct{})
	var once sync.Once
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer once.Do(func() { close(returned) })
		s.boardStream(w, r)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/tasks/stream?tick=1ms&heartbeat=1ms", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		ts.Close()
		t.Fatalf("GET /tasks/stream: %v", err)
	}
	lines := make(chan string, 256)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	st := &blStream{resp: resp, lines: lines, cancel: cancel, returned: returned}
	st.close = func() {
		cancel()
		resp.Body.Close()
		ts.Close()
	}
	return st
}

// next returns the next line, or ok=false on EOF or after d.
func (st *blStream) next(d time.Duration) (line string, ok, eof bool) {
	select {
	case l, open := <-st.lines:
		if !open {
			return "", false, true
		}
		return l, true, false
	case <-time.After(d):
		return "", false, false
	}
}

// until reads lines until pred matches one, within d; it returns what it read.
func (st *blStream) until(d time.Duration, pred func(string) bool) (bool, []string) {
	deadline := time.Now().Add(d)
	var seen []string
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return false, seen
		}
		l, ok, eof := st.next(left)
		if eof || !ok {
			return false, seen
		}
		seen = append(seen, l)
		if pred(l) {
			return true, seen
		}
	}
}

func blListeningServer(heartbeat time.Duration) (*Server, *BoardHub) {
	h := newBoardHubWithConfig(nil, boardLiveConfig{Heartbeat: heartbeat})
	h.setListening(true)
	s := &Server{}
	s.SetBoardHub(h)
	return s, h
}

func TestBoardStream_HeadersAndRetry(t *testing.T) {
	s, _ := blListeningServer(0)
	st := blOpenStream(t, s)
	defer st.close()
	if st.resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /tasks/stream with a listening hub = %d, want 200", st.resp.StatusCode)
	}
	for k, want := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"X-Accel-Buffering": "no",
	} {
		if got := st.resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want exactly %q (criterion 11b; X-Accel-Buffering: no is what stops ingress-nginx "+
				"buffering the stream)", k, got, want)
		}
	}
	first, ok, _ := st.next(2 * time.Second)
	if !ok || first != "retry: 5000" {
		t.Errorf("the stream's first line is %q (ok=%v), want \"retry: 5000\" (criterion 11c: boardLiveRetry in ms, "+
			"flushed before anything else)", first, ok)
	}
	blank, ok, _ := st.next(2 * time.Second)
	if !ok || blank != "" {
		t.Errorf("the retry line is followed by %q (ok=%v), want a blank line (criterion 11c)", blank, ok)
	}
	// The request carried ?tick=1ms&heartbeat=1ms. Criterion 12 / D15: the stream
	// takes no parameters, so the default 20 s heartbeat sends nothing yet.
	if found, seen := st.until(300*time.Millisecond, func(l string) bool { return l != "" }); found {
		t.Errorf("an idle stream with the default heartbeat wrote %q within 300 ms of opening; a query value reached "+
			"the handler's timing (criterion 12: the stream takes no parameters)", seen)
	}
}

func TestBoardStream_BroadcastBecomesAChangeEvent(t *testing.T) {
	s, h := blListeningServer(0)
	st := blOpenStream(t, s)
	defer st.close()
	if found, seen := st.until(2*time.Second, func(l string) bool { return strings.HasPrefix(l, "retry:") }); !found {
		t.Fatalf("no retry: line before the first event; read %q", seen)
	}
	blBroadcast(t, h, 1)
	found, seen := st.until(2*time.Second, func(l string) bool { return l == "event: change" })
	if !found {
		t.Fatalf("a hub broadcast produced no `event: change` line within 2 s (criterion 11d); read %q", seen)
	}
	data, ok, _ := st.next(2 * time.Second)
	if !ok || !strings.HasPrefix(data, "data: ") || strings.TrimSpace(strings.TrimPrefix(data, "data: ")) == "" {
		t.Errorf("`event: change` is followed by %q, want a `data: <n>` line (criterion 11d; the counter is opaque)", data)
	}
}

func TestBoardStream_HeartbeatComments(t *testing.T) {
	s, _ := blListeningServer(50 * time.Millisecond)
	st := blOpenStream(t, s)
	defer st.close()
	pings := 0
	deadline := time.Now().Add(2 * time.Second)
	for pings < 2 && time.Now().Before(deadline) {
		l, ok, eof := st.next(time.Until(deadline))
		if eof || !ok {
			break
		}
		if l == ": ping" {
			pings++
		}
	}
	if pings < 2 {
		t.Errorf("with a 50 ms heartbeat the stream carried %d `: ping` lines in 2 s, want at least 2 (criterion 11e: "+
			"the comment keeps ingress-nginx's 60 s read timeout from cutting an idle stream)", pings)
	}
}

func TestBoardStream_ClientGoneReturnsAndUnsubscribes(t *testing.T) {
	s, h := blListeningServer(0)
	st := blOpenStream(t, s)
	defer st.close()
	if found, seen := st.until(2*time.Second, func(l string) bool { return strings.HasPrefix(l, "retry:") }); !found {
		t.Fatalf("no retry: line; read %q", seen)
	}
	if n := h.Subscribers(); n != 1 {
		t.Errorf("CONTROL: an open stream reports Subscribers() = %d, want 1", n)
	}
	st.cancel()
	select {
	case <-st.returned:
	case <-time.After(2 * time.Second):
		t.Fatalf("boardStream did not return within 2 s of the client going away (criterion 11f)")
	}
	deadline := time.Now().Add(2 * time.Second)
	for h.Subscribers() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := h.Subscribers(); n != 0 {
		t.Errorf("after the client left Subscribers() = %d, want 0 (criterion 11f: unsubscribe in the handler's defer)", n)
	}
}

func TestBoardStream_EndsWhenTheHubGoesBlind(t *testing.T) {
	s, h := blListeningServer(0)
	st := blOpenStream(t, s)
	defer st.close()
	if found, seen := st.until(2*time.Second, func(l string) bool { return strings.HasPrefix(l, "retry:") }); !found {
		t.Fatalf("no retry: line; read %q", seen)
	}
	h.setListening(false)
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, ok, eof := st.next(time.Until(deadline))
		if eof {
			break
		}
		if !ok {
			t.Fatalf("the response is still open 2 s after the hub went blind. Criterion 11g / S8: a blind hub closes " +
				"the stream so the browser falls back to polling honestly, instead of trusting a dead feed")
		}
	}
	select {
	case <-st.returned:
	case <-time.After(2 * time.Second):
		t.Errorf("the body ended but boardStream never returned")
	}
}

// ---- criterion 16: boardVersion --------------------------------------------------------

func TestBoardLive_BoardVersionIsTheTemplateHash(t *testing.T) {
	raw, err := templateFS.ReadFile("templates/tasks.html")
	if err != nil {
		t.Fatalf("read embedded tasks.html: %v", err)
	}
	sum := sha256.Sum256(raw)
	want := hex.EncodeToString(sum[:])[:12]
	if boardVersion != want {
		t.Errorf("boardVersion = %q, want %q = hex(sha256(embedded templates/tasks.html))[:12] (criterion 16: a deploy "+
			"that changes the page changes the version, and the open board reloads once)", boardVersion, want)
	}
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(boardVersion) {
		t.Errorf("boardVersion %q is not 12 lower-case hex characters", boardVersion)
	}
	src := readSrc(t, "live.go")
	if regexp.MustCompile(`boardVersion\s*=\s*"`).MatchString(src) {
		t.Errorf("live.go assigns boardVersion a string literal; criterion 16: it is computed from the embedded " +
			"template once, at package init, never hard-coded")
	}
	ps := parseDashboardSource(t)
	computed, _ := ps.reach("boardVersion")
	computed += ps.text["init"]
	if !strings.Contains(computed, "sha256.") || !strings.Contains(computed, "templateFS") {
		t.Errorf("boardVersion is not computed from sha256 over templateFS (its initializer, what it reaches, or an init())")
	}
}
