package dashboard

// board-streaming (SWT-89, docs/tickets/board-streaming_SPEC.md): the board is
// pushed, not polled.
//
// Postgres NOTIFYs `board_changed` on every row change of the tables the board
// reads (migration 0044). One BoardHub per dashboard process holds a single
// LISTEN connection, coalesces bursts, adds a 60 s tick for the facts nothing
// writes (elapsed time, the stale ring, midnight), and fans a payload-free
// "change" signal out to every open board over Server-Sent Events. Each board
// then re-fetches its own server-rendered /tasks URL and swaps the changed
// regions in place: Go stays the only author of every fact on the page.
//
// Read-only by construction (invariant 3): the hub's only statements are the
// SET and the LISTEN; the stream handler runs no SQL and no executor call; the
// notification payload is a wake-up only and is never read.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// boardChannel is the NOTIFY channel migration 0044 writes. Board traffic never
// goes on the orchestrator's `task_events` channel.
const boardChannel = "board_changed"

// The live cadence (S4, S5, S8). Go consts, never URL values (the D15 rule).
const (
	boardLiveDebounce  = 250 * time.Millisecond // leading edge after the first change
	boardLiveMinGap    = 2 * time.Second        // at most one broadcast per gap during a burst
	boardLiveTick      = 60 * time.Second       // time-only facts, and the safety net
	boardLiveHeartbeat = 20 * time.Second       // under ingress-nginx's 60 s proxy-read-timeout
	boardLiveRetry     = 5 * time.Second        // SSE retry:, and the browser's re-open delay
)

// boardNotifyTables are the tables migration 0044 triggers on. Every table the
// board's SQL reads must be here or in boardTickOnlyTables (criterion 4's
// coverage test), so a future board query cannot silently miss the push.
var boardNotifyTables = []string{"tasks", "task_dismissals", "classify_promotions", "external_refs"}

// boardTickOnlyTables are read by the board but deliberately not triggered; the
// 60 s tick covers them.
var boardTickOnlyTables = map[string]string{
	"projects":            "slugs change about once a month",
	"normalized_messages": "the ingestion firehose; the board reads it only through tasks.activity_by_message_id, a tasks write that already fires",
}

// boardVersion identifies the board template the page was rendered from, so an
// open board notices a deploy and reloads once instead of swapping regions from
// a different page shape (S6).
var boardVersion = func() string {
	raw, err := templateFS.ReadFile("templates/tasks.html")
	if err != nil {
		panic(fmt.Sprintf("board-streaming: read embedded tasks.html: %v", err))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:12]
}()

// boardLiveConfig lets tests shrink the cadence without touching a request. A
// zero field means its const.
type boardLiveConfig struct{ Debounce, MinGap, Tick, Heartbeat, Retry time.Duration }

func (c boardLiveConfig) withDefaults() boardLiveConfig {
	if c.Debounce == 0 {
		c.Debounce = boardLiveDebounce
	}
	if c.MinGap == 0 {
		c.MinGap = boardLiveMinGap
	}
	if c.Tick == 0 {
		c.Tick = boardLiveTick
	}
	if c.Heartbeat == 0 {
		c.Heartbeat = boardLiveHeartbeat
	}
	if c.Retry == 0 {
		c.Retry = boardLiveRetry
	}
	return c
}

// boardCoalescer turns notifications into broadcasts (S4, S5). It is pure: the
// caller supplies every instant. A burst broadcasts Debounce after its first
// notification, then at most once per MinGap, and always once more after the
// last notification (the trailing edge). With no notifications it broadcasts
// once per Tick.
type boardCoalescer struct {
	cfg           boardLiveConfig
	tickAt        time.Time
	pending       bool
	pendingSince  time.Time
	lastBroadcast time.Time
}

func newBoardCoalescer(cfg boardLiveConfig, now time.Time) *boardCoalescer {
	cfg = cfg.withDefaults()
	return &boardCoalescer{cfg: cfg, tickAt: now.Add(cfg.Tick)}
}

// notify records that a notification arrived at now.
func (c *boardCoalescer) notify(now time.Time) {
	if !c.pending {
		c.pending, c.pendingSince = true, now
	}
}

func (c *boardCoalescer) due() time.Time {
	d := c.pendingSince.Add(c.cfg.Debounce)
	if !c.lastBroadcast.IsZero() {
		if g := c.lastBroadcast.Add(c.cfg.MinGap); g.After(d) {
			d = g
		}
	}
	return d
}

// wake is the next instant fire must be called: the pending change's due time
// or the tick, whichever is sooner. Never zero.
func (c *boardCoalescer) wake() time.Time {
	if c.pending && c.due().Before(c.tickAt) {
		return c.due()
	}
	return c.tickAt
}

// fire reports whether to broadcast at now.
func (c *boardCoalescer) fire(now time.Time) bool {
	fired := c.pending && !now.Before(c.due())
	if !now.Before(c.tickAt) {
		fired = true
		for !now.Before(c.tickAt) {
			c.tickAt = c.tickAt.Add(c.cfg.Tick)
		}
	}
	if fired {
		c.pending = false
		c.lastBroadcast = now
	}
	return fired
}

type boardSub struct {
	c    chan uint64
	done chan struct{}
}

// BoardHub is the dashboard process's one LISTEN, fanned out to every open
// board stream. It holds no task data: a counter and a subscriber set.
type BoardHub struct {
	pool      *pgxpool.Pool
	cfg       boardLiveConfig
	mu        sync.Mutex
	subs      map[*boardSub]struct{}
	listening bool
	n         uint64
}

// NewBoardHub builds the hub with the production cadence. Start it with Run.
func NewBoardHub(pool *pgxpool.Pool) *BoardHub { return newBoardHubWithConfig(pool, boardLiveConfig{}) }

func newBoardHubWithConfig(pool *pgxpool.Pool, cfg boardLiveConfig) *BoardHub {
	return &BoardHub{pool: pool, cfg: cfg.withDefaults(), subs: map[*boardSub]struct{}{}}
}

// Subscribe registers one stream. signals has capacity 1 and is never blocked
// on: a pending signal already means "re-fetch". done closes when the hub goes
// blind, so the stream ends and the browser falls back honestly. cancel
// unsubscribes and is safe to call more than once.
func (h *BoardHub) Subscribe() (<-chan uint64, <-chan struct{}, func()) {
	s := &boardSub{c: make(chan uint64, 1), done: make(chan struct{})}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return s.c, s.done, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, s)
			h.mu.Unlock()
		})
	}
}

// subscribeIfListening is Subscribe for a stream: it checks and subscribes
// under one lock, so a hub going blind between the two can never leave a new
// stream open with a done channel nobody will close.
func (h *BoardHub) subscribeIfListening() (<-chan uint64, <-chan struct{}, func(), bool) {
	h.mu.Lock()
	if !h.listening {
		h.mu.Unlock()
		return nil, nil, nil, false
	}
	s := &boardSub{c: make(chan uint64, 1), done: make(chan struct{})}
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return s.c, s.done, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, s)
			h.mu.Unlock()
		})
	}, true
}

// Listening reports whether the hub currently holds its LISTEN.
func (h *BoardHub) Listening() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.listening
}

// Subscribers is the number of open streams.
func (h *BoardHub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// setListening flips the state; going blind closes every current subscriber's
// done channel and forgets them.
func (h *BoardHub) setListening(v bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.listening && !v {
		for s := range h.subs {
			close(s.done)
		}
		h.subs = map[*boardSub]struct{}{}
	}
	h.listening = v
}

// broadcast sends one non-blocking signal to every subscriber.
func (h *BoardHub) broadcast() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.n++
	for s := range h.subs {
		select {
		case s.c <- h.n:
		default: // one pending signal is enough
		}
	}
}

// Run holds the LISTEN until ctx ends, reconnecting with backoff (1 s doubling
// to 30 s, reset after every successful LISTEN). Each successful LISTEN
// broadcasts once, because anything could have changed while the hub was blind.
func (h *BoardHub) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		listened, err := h.listenOnce(ctx)
		h.setListening(false)
		if ctx.Err() != nil {
			return nil
		}
		if listened {
			backoff = time.Second
		}
		log.Printf("board live: not listening (%v); retrying in %s", err, backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// listenOnce takes a connection OUT of the pool (Hijack), so a permanent LISTEN
// never shrinks the render pool, and loops until the connection or ctx fails.
func (h *BoardHub) listenOnce(ctx context.Context) (bool, error) {
	pc, err := h.pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire: %w", err)
	}
	conn := pc.Hijack()
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "SET application_name = 'switchboard-board-live'"); err != nil {
		return false, fmt.Errorf("name the connection: %w", err)
	}
	if _, err := conn.Exec(ctx, "LISTEN "+boardChannel); err != nil {
		return false, fmt.Errorf("subscribe to the channel: %w", err)
	}
	h.setListening(true)
	h.broadcast()

	notes := make(chan struct{}, 1)
	errc := make(chan error, 1)
	wctx, cancel := context.WithCancel(ctx)
	// A pgx connection is not safe for concurrent use: the deferred Close must
	// not run while the waiter is still inside WaitForNotification. Cancel it and
	// wait for its exit (it always reports on errc) before the connection closes.
	waiterDone := false
	defer func() {
		cancel()
		if !waiterDone {
			<-errc
		}
	}()
	go func() {
		for {
			if _, err := conn.WaitForNotification(wctx); err != nil {
				errc <- err
				return
			}
			select {
			case notes <- struct{}{}:
			default: // the payload is a wake-up only; one pending is enough
			}
		}
	}()
	c := newBoardCoalescer(h.cfg, time.Now())
	for {
		timer := time.NewTimer(time.Until(c.wake()))
		select {
		case <-ctx.Done():
			timer.Stop()
			return true, ctx.Err()
		case err := <-errc:
			timer.Stop()
			waiterDone = true
			return true, fmt.Errorf("wait for notification: %w", err)
		case <-notes:
			timer.Stop()
			c.notify(time.Now())
		case <-timer.C:
			if c.fire(time.Now()) {
				h.broadcast()
			}
		}
	}
}

// SetBoardHub attaches the process's hub. A server without one answers the
// stream with 503, and open boards fall back to polling.
func (s *Server) SetBoardHub(h *BoardHub) { s.live = h }

// boardStream is GET /tasks/stream: an SSE stream of payload-free change
// signals. No SQL, no executor call, no parameters.
func (s *Server) boardStream(w http.ResponseWriter, r *http.Request) {
	h := s.live
	var (
		sig    <-chan uint64
		done   <-chan struct{}
		cancel func()
		ok     bool
	)
	if h != nil {
		sig, done, cancel, ok = h.subscribeIfListening()
	}
	if !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(boardLiveRetry/time.Second)))
		http.Error(w, "live updates unavailable", http.StatusServiceUnavailable)
		return
	}
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // ingress-nginx: do not buffer this response
	rc := http.NewResponseController(w)
	if _, err := fmt.Fprintf(w, "retry: %d\n\n", h.cfg.Retry/time.Millisecond); err != nil {
		return
	}
	if rc.Flush() != nil {
		return
	}
	hb := time.NewTicker(h.cfg.Heartbeat)
	defer hb.Stop()
	for {
		var err error
		select {
		case <-r.Context().Done():
			return
		case <-done:
			return
		case n := <-sig:
			_, err = fmt.Fprintf(w, "event: change\ndata: %s\n\n", strconv.FormatUint(n, 10))
		case <-hb.C:
			_, err = fmt.Fprint(w, ": ping\n\n")
		}
		if err != nil || rc.Flush() != nil {
			return
		}
	}
}
