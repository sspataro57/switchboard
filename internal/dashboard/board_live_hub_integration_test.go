//go:build integration

package dashboard_test

// board-streaming (SWT-89, docs/tickets/board-streaming_SPEC.md) criteria 8
// (the hub against real Postgres) and 13 (the stream end to end through the
// real Handler(), auth.Require and staticCacheHeaders). Fixtures, cleanup and
// the executor helpers are board_live_integration_test.go's.
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_boardstream?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run 'BoardHub|BoardStream' ./internal/dashboard/
//
// ISOLATED DATABASE REQUIRED: criterion 8(a) counts the hub's backends in
// pg_stat_activity for current_database(), and 8(c)/13 wait for a change signal
// after a quiet window — another suite writing to the same database would fill
// that window.
//
// IMPOSED EXPORTED SURFACE (SPEC S8; package dashboard):
//
//	func NewBoardHub(pool *pgxpool.Pool) *BoardHub
//	func (h *BoardHub) Run(ctx context.Context) error   // the tests ignore the result
//	func (h *BoardHub) Subscribe() (signals <-chan T, done <-chan struct{}, cancel func())
//	func (h *BoardHub) Listening() bool
//	func (h *BoardHub) Subscribers() int
//	func (s *Server) SetBoardHub(h *BoardHub)
//
// SPEC AMBIGUITY RESOLVED HERE (8d, "broadcasts once" after a reconnect): the
// only way to observe that broadcast is a subscriber that exists before it. The
// handler never subscribes while the hub is blind (it answers 503), so this
// suite subscribes DURING the blind window and requires that subscription to
// stay open and receive the catch-up signal. going-blind closes the
// subscriptions that existed at that moment (7d); it does not poison later ones.
//
// GREENFIELD NOTE, EXPECTED RED: live.go does not exist, so the package's
// integration test binary compile-FAILS on the symbols above. Once it compiles,
// a pre-0044 database fails every test here on blRequire0044.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - the hub LISTENs on task_events instead of board_changed -> ActivityMarkReachesASubscriber,
//     StreamEndToEnd, ListensOnAHijackedConnection (the query text).
//   - Acquire without Hijack -> ListensOnAHijackedConnection (8b).
//   - the hub does not close subscribers when blind -> ReconnectsAfterTheBackendDies.
//   - the hub reconnects without the catch-up broadcast -> ReconnectsAfterTheBackendDies.
//   - the handler omits the Flush after an event -> StreamEndToEnd.

import (
	"bufio"
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/dashboard"
)

const blAppName = "switchboard-board-live"

type blHub struct {
	hub     *dashboard.BoardHub
	pool    *pgxpool.Pool
	cancel  context.CancelFunc
	runDone chan struct{}
}

// blStartHub runs a hub on its OWN pool (so 8b can read that pool's count) and
// waits for it to listen.
func blStartHub(t *testing.T, ctx context.Context) *blHub {
	t.Helper()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("hub pool: %v", err)
	}
	h := &blHub{hub: dashboard.NewBoardHub(pool), pool: pool, runDone: make(chan struct{})}
	runCtx, cancel := context.WithCancel(ctx)
	h.cancel = cancel
	go func() {
		defer close(h.runDone)
		h.hub.Run(runCtx)
	}()
	t.Cleanup(func() { h.stop(t) })
	deadline := time.Now().Add(5 * time.Second)
	for !h.hub.Listening() {
		if time.Now().After(deadline) {
			t.Fatalf("the hub did not report Listening() within 5 s of Run (criterion 8a)")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return h
}

func (h *blHub) stop(t *testing.T) {
	if h.cancel == nil {
		return
	}
	h.cancel()
	h.cancel = nil
	select {
	case <-h.runDone:
	case <-time.After(5 * time.Second):
		t.Errorf("Run did not return within 5 s of its context being cancelled")
	}
	h.pool.Close()
}

type blBackend struct {
	pid   int32
	query string
}

func blHubBackends(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []blBackend {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT pid, COALESCE(query, '') FROM pg_stat_activity
		  WHERE application_name = $1 AND datname = current_database() ORDER BY pid`, blAppName)
	if err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	defer rows.Close()
	var out []blBackend
	for rows.Next() {
		var b blBackend
		if err := rows.Scan(&b.pid, &b.query); err != nil {
			t.Fatalf("scan pg_stat_activity: %v", err)
		}
		out = append(out, b)
	}
	return out
}

// blOneBackend waits (up to d) for exactly one hub backend and returns it.
func blOneBackend(t *testing.T, ctx context.Context, pool *pgxpool.Pool, d time.Duration) (blBackend, bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	var last []blBackend
	for time.Now().Before(deadline) {
		last = blHubBackends(t, ctx, pool)
		if len(last) == 1 {
			return last[0], true
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("pg_stat_activity shows %d backend(s) with application_name=%s, want exactly one: %v", len(last), blAppName, last)
	return blBackend{}, false
}

func blIsListenQuery(q string) bool {
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(q), ";")) == "LISTEN board_changed"
}

// blQuiet drains sig until it has been silent for d (or fails after max).
func blQuiet(t *testing.T, sig func() <-chan struct{}, d, max time.Duration) {
	t.Helper()
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		select {
		case <-sig():
		case <-time.After(d):
			return
		}
	}
	t.Fatalf("the hub never went quiet for %v within %v; is another suite writing to this database?", d, max)
}

// ---- criterion 8(a)+(b): one named backend, LISTENing, hijacked out of the pool ----------

func TestBoardHub_Integration_ListensOnAHijackedConnection(t *testing.T) {
	ctx, pool, _ := blSetup(t)
	blRequire0044(t, ctx, pool)
	h := blStartHub(t, ctx)

	b, ok := blOneBackend(t, ctx, pool, 5*time.Second)
	if ok && !blIsListenQuery(b.query) {
		t.Errorf("the hub's backend (pid %d) last ran %q, want LISTEN board_changed (criterion 8a: the only statements "+
			"it sends are the SET and the LISTEN, and the channel is board_changed, never task_events)", b.pid, b.query)
	}
	deadline := time.Now().Add(2 * time.Second)
	for h.pool.Stat().TotalConns() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := h.pool.Stat().TotalConns(); n != 0 {
		t.Errorf("the hub's pool counts %d connection(s) while the hub listens, want 0. Criterion 8b / S8: Run Acquires "+
			"the LISTEN connection and Hijacks it out of the pool, so a permanent LISTEN never shrinks the render pool", n)
	}
}

// ---- criterion 8(c): an activity mark reaches a subscriber ----------------------------------

func TestBoardHub_Integration_ActivityMarkReachesASubscriber(t *testing.T) {
	ctx, pool, f := blSetup(t)
	blRequire0044(t, ctx, pool)
	tk := f.task(t, ctx, "BOARDLIVE hub mark", "human", "ready")
	msg, _ := f.message(t, ctx, "hub-mark")
	h := blStartHub(t, ctx)

	sig, done, cancel := h.hub.Subscribe()
	defer cancel()
	wake := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-sig:
				select {
				case wake <- struct{}{}:
				default:
				}
			case <-done:
				return
			}
		}
	}()
	blQuiet(t, func() <-chan struct{} { return wake }, 2500*time.Millisecond, 20*time.Second)

	f.mark(t, ctx, tk, msg)
	select {
	case <-wake:
	case <-done:
		t.Fatalf("the subscription closed instead of signalling (criterion 8c)")
	case <-time.After(2 * time.Second):
		t.Fatalf("task_mark_activity through the executor did not reach a hub subscriber within 2 s. Criterion 8c: the " +
			"mark writes no task_events row, so only the board_changed trigger can carry it")
	}
}

// ---- criterion 8(d): a dead backend -> blind, closed, reconnected, one catch-up signal -----

func TestBoardHub_Integration_ReconnectsAfterTheBackendDies(t *testing.T) {
	ctx, pool, _ := blSetup(t)
	blRequire0044(t, ctx, pool)
	h := blStartHub(t, ctx)
	old, ok := blOneBackend(t, ctx, pool, 5*time.Second)
	if !ok {
		t.FailNow()
	}
	_, done1, cancel1 := h.hub.Subscribe()
	defer cancel1()

	var killed bool
	if err := pool.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, old.pid).Scan(&killed); err != nil || !killed {
		t.Fatalf("pg_terminate_backend(%d) = %v, %v", old.pid, killed, err)
	}
	select {
	case <-done1:
	case <-time.After(5 * time.Second):
		t.Fatalf("5 s after its backend was terminated the hub had not closed its open subscription. Criterion 8d / S8: " +
			"on any error it marks itself not listening, which ends every open stream")
	}
	if h.hub.Listening() {
		t.Errorf("the hub reports Listening() right after its subscriptions were closed; it is blind until it re-LISTENs")
	}

	sig2, done2, cancel2 := h.hub.Subscribe() // during the blind window (see the note at the top)
	defer cancel2()
	deadline := time.Now().Add(5 * time.Second)
	for !h.hub.Listening() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !h.hub.Listening() {
		t.Fatalf("the hub did not reconnect and report Listening() within 5 s of its backend dying (criterion 8d: " +
			"backoff 1 s doubling to 30 s)")
	}
	if nb, ok := blOneBackend(t, ctx, pool, 5*time.Second); ok {
		if nb.pid == old.pid {
			t.Errorf("the hub's backend is still pid %d; it should be a new connection", nb.pid)
		}
		if !blIsListenQuery(nb.query) {
			t.Errorf("the reconnected backend last ran %q, want LISTEN board_changed", nb.query)
		}
	}
	select {
	case <-sig2:
	case <-done2:
		t.Fatalf("the subscription made while the hub was blind was closed instead of receiving the catch-up signal")
	case <-time.After(5 * time.Second):
		t.Fatalf("after re-LISTENing the hub broadcast nothing within 5 s. Criterion 8d / S8: anything could have " +
			"changed while it was blind, so it broadcasts once")
	}
}

// ---- criterion 8(e): cancelling Run releases the backend -----------------------------------

func TestBoardHub_Integration_CancelReleasesTheConnection(t *testing.T) {
	ctx, pool, _ := blSetup(t)
	blRequire0044(t, ctx, pool)
	h := blStartHub(t, ctx)
	if _, ok := blOneBackend(t, ctx, pool, 5*time.Second); !ok {
		t.FailNow()
	}
	h.stop(t)
	deadline := time.Now().Add(5 * time.Second)
	var left []blBackend
	for time.Now().Before(deadline) {
		if left = blHubBackends(t, ctx, pool); len(left) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("5 s after Run's context was cancelled pg_stat_activity still shows the hub's backend %v (criterion 8e: a "+
		"hijacked connection is the hub's to close)", left)
}

// ---- criterion 13: end to end through the real Handler() ------------------------------------

func TestBoardStream_Integration_EndToEnd(t *testing.T) {
	ctx, pool, f := blSetup(t)
	blRequire0044(t, ctx, pool)
	tk := f.task(t, ctx, "BOARDLIVE e2e", "human", "ready")
	msg, _ := f.message(t, ctx, "e2e")
	h := blStartHub(t, ctx)

	auth, err := dashboard.NewAuth(ctx, "", "", "", "") // dev mode
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	srv, err := dashboard.NewServer(pool, f.ex, auth)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetBoardHub(h.hub)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	if _, err := client.Get(ts.URL + "/dev/login?user=salvo"); err != nil {
		t.Fatalf("dev login: %v", err)
	}

	reqCtx, cancelReq := context.WithCancel(ctx)
	defer cancelReq()
	req, _ := http.NewRequestWithContext(reqCtx, http.MethodGet, ts.URL+"/tasks/stream", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /tasks/stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("GET /tasks/stream with a session and a listening hub = %d %q, want 200 text/event-stream (criterion 13)",
			resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	events := make(chan struct{}, 16)
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if sc.Text() == "event: change" {
				select {
				case events <- struct{}{}:
				default:
				}
			}
		}
	}()
	quiet := func() <-chan struct{} { return events }
	blQuiet(t, quiet, 2500*time.Millisecond, 20*time.Second)

	f.mark(t, ctx, tk, msg)
	select {
	case <-events:
	case <-closed:
		t.Fatalf("the stream ended before any change arrived (criterion 13)")
	case <-time.After(3 * time.Second):
		t.Fatalf("no `event: change` line was read from the live response within 3 s of task_mark_activity. " +
			"Criterion 13: the handler flushes after every write, through staticCacheHeaders and auth.Require")
	}
}
