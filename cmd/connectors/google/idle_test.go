package main

// imap-idle-watch (SWT-73) criterion 8 / D5 — THE MSN INVARIANT. ZERO I/O: a
// fake MailSource and a fake per-account lock; no Postgres, no IMAP, no
// Microsoft.
//
// What this pins, in D5's words: idleOnce "holds [the per-account lock] across
// OpenIMAPSource only (watch.go:236-263) and releases before going idle, which
// is correct BECAUSE THAT CYCLE MAKES EXACTLY ONE CONNECTION (src.Idle consumes
// the eagerly minted token), a property the code states in a comment at
// :257-263 and nothing enforces."
//
// Why it is worth a test of its own: Microsoft rotates the refresh token on
// every redemption and switchboard stores the new one
// (internal/connector/google/credential.go:29-33, 60-71, 89-96). Two redemptions
// of the same stored token is how the loser gets invalid_grant. Under
// OQ-1 = B-reduced the CronJob and the watcher genuinely run side by side, so
// "safe by a comment" is not good enough: if someone later fetches inside
// idleOnce, the rotation race comes back intermittently, and its symptom is a
// FALSE revoked-consent alarm on the one mailbox that cannot be re-consented
// from a script.
//
// IMPOSED SURFACE (package main, cmd/connectors/google/watch.go; today idleOnce
// takes (*pgxpool.Pool, *google.PGSink, key) and calls the package-level
// google.OpenIMAPSource, so it cannot be driven without a database. The two
// production call arguments become injectable seams and nothing else changes):
//
//	// One IMAP connection, as idleOnce sees it. *google.IMAPClientSource
//	// satisfies it in production.
//	type idleConn interface {
//	    google.MailSource
//	    Close() error
//	}
//
//	// Opens EXACTLY ONE connection for the account, minting the credential:
//	// google.OpenIMAPSource(ctx, pool, acct, key) in production.
//	type openIdleConn func(ctx context.Context, acct google.Account) (idleConn, error)
//
//	type idleDeps struct {
//	    Sink        accountSink                                    // LockAccount / StartRun / FinishRun
//	    Open        openIdleConn
//	    Sleep       func(ctx context.Context, d time.Duration) error
//	    IdleRefresh time.Duration                                  // MAIL_IDLE_REFRESH
//	}
//
//	func idleOnce(ctx context.Context, deps idleDeps, acct google.Account, wake chan<- string) error
//
// GREENFIELD NOTE — EXPECTED RED: idleDeps, idleConn, openIdleConn and
// idleOnce's new signature do not exist, so this file compile-FAILS.
//
// MUTATIONS: open a second IMAP connection inside idleOnce -> criterion 8;
// release the per-account lock before the mint -> criteria 8, 22.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

// note records an event in the same ordered log the fake sink writes its
// lock/release events to, so "the lock was released BEFORE the IDLE began and
// AFTER the mint" is one assertion over one sequence.
func (s *mwFakeSink) note(event string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, event)
}

// mwFakeConn is one IMAP connection. Every verb records itself, so a second
// connect, a fetch inside the idle cycle or a missing Close is visible.
type mwFakeConn struct {
	mu     sync.Mutex
	log    *mwFakeSink
	signal chan struct{}
	idles  int
	closed int
	other  int // Folders/Search/Fetch calls: none belong in an IDLE cycle
}

func (c *mwFakeConn) Folders(context.Context) ([]google.Folder, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.other++
	c.log.note("folders")
	return nil, nil
}

func (c *mwFakeConn) Search(context.Context, string, google.SearchCriteria) ([]uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.other++
	c.log.note("search")
	return nil, nil
}

func (c *mwFakeConn) Fetch(context.Context, string, []uint32, int) ([]google.FetchedMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.other++
	c.log.note("fetch")
	return nil, nil
}

func (c *mwFakeConn) Idle(ctx context.Context, folder string) (<-chan struct{}, error) {
	c.mu.Lock()
	c.idles++
	c.mu.Unlock()
	c.log.note("idle:" + folder)
	return c.signal, nil
}

func (c *mwFakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed++
	c.log.note("close")
	return nil
}

func (c *mwFakeConn) counts() (idles, closed, other int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.idles, c.closed, c.other
}

// Criterion 8: exactly ONE connection per lock acquisition, and the lock is
// released after the mint and before the IDLE begins.
func TestIdleOnce_OneConnectionPerLockAcquisition(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sink := newMWFakeSink()
	signal := make(chan struct{}, 1)
	conn := &mwFakeConn{log: sink, signal: signal}

	opens := 0
	open := func(context.Context, google.Account) (idleConn, error) {
		opens++
		sink.note("open")
		return conn, nil
	}

	wake := make(chan string, 1)
	signal <- struct{}{} // new mail arrives the instant the IDLE is established

	acct := mwAccount(4, "sspataro57@msn.com")
	if err := idleOnce(ctx, idleDeps{Sink: sink, Open: open, Sleep: (&mwClock{}).Sleep,
		IdleRefresh: 25 * time.Minute}, acct, wake); err != nil {
		t.Fatalf("idleOnce = %v, want nil on a cycle that fired normally", err)
	}

	if opens != 1 {
		t.Errorf("idleOnce opened %d connections, want exactly 1. D5: the per-account advisory lock is "+
			"released the moment the source is open, and that is only safe BECAUSE the cycle makes exactly "+
			"one connection — the eagerly minted Microsoft token is consumed by src.Idle. A second connect "+
			"(say, fetching the new message here instead of through watchPass) mints OUTSIDE the lock and "+
			"puts the rotation race back, intermittently, with a false revoked-consent alarm as its "+
			"symptom (criterion 8)", opens)
	}
	idles, closed, other := conn.counts()
	if idles != 1 {
		t.Errorf("src.Idle was called %d times in one cycle, want 1 (criterion 8)", idles)
	}
	if other != 0 {
		t.Errorf("idleOnce called %d folder/search/fetch verb(s) on the IDLE connection. The notification is "+
			"a WAKE-UP, never a payload (imap.go:810-812): the caller re-runs a BOUNDED UID fetch through "+
			"watchPass, so a missed or duplicated notification costs a round trip and never a message "+
			"(criterion 8, invariant 1)", other)
	}
	if closed != 1 {
		t.Errorf("the connection was closed %d times, want exactly 1 — a resident loop that leaks one "+
			"connection per refresh reaches Gmail's 15-per-account limit within a day (D12)", closed)
	}

	order := sink.lockOrder()
	want := []string{"lock", "open", "release", "idle:" + google.InboxFolder, "close"}
	if len(order) < 4 {
		t.Fatalf("event order = %v, want %v (criterion 8)", order, want)
	}
	pos := func(event string) int {
		for i, e := range order {
			if e == event {
				return i
			}
		}
		return -1
	}
	lock, opened, release, idle := pos("lock"), pos("open"), pos("release"), pos("idle:"+google.InboxFolder)
	switch {
	case lock < 0:
		t.Errorf("idleOnce never took the per-account lock. D5: the mint must happen UNDER it or two "+
			"processes redeem the same Microsoft refresh token and the loser gets invalid_grant "+
			"(criterion 8). Order: %v", order)
	case opened < lock:
		t.Errorf("idleOnce opened the connection BEFORE taking the lock, so the credential is minted "+
			"outside it — exactly the race the lock exists to prevent (criteria 8, 22). Order: %v", order)
	case release < opened:
		t.Errorf("idleOnce released the per-account lock BEFORE the mint. The lock covers OpenIMAPSource "+
			"and nothing else, but it must cover ALL of it (criteria 8, 22). Order: %v", order)
	case idle >= 0 && release > idle:
		t.Errorf("idleOnce held the per-account lock across the IDLE itself; that blocks the ingest pass "+
			"for the whole %s refresh interval (D5, watch.go:236-263). Order: %v", 25*time.Minute, order)
	}

	// The wake is published, scoped to the account, so the loop runs a pass for
	// that mailbox and the sweep covers the rest.
	select {
	case got := <-wake:
		if got != acct.Email {
			t.Errorf("wake carried %q, want %q — the loop scopes the pass to the account that woke it", got, acct.Email)
		}
	default:
		t.Error("idleOnce fired without publishing a wake: the mail would sit in the mailbox until the next " +
			"reconcile sweep, which is the ten-minute latency this ticket exists to remove")
	}
}

// Criterion 8, the busy branch: an ingest pass holds the per-account lock, so
// this cycle opens NOTHING — no connection, no mint — and WAITS before
// returning. watchAccount treats a nil return as success and re-enters
// immediately, so returning straight away spins on pg_try_advisory_lock for the
// whole pass: thousands of round trips a second, each taking a pooled
// connection, contending with the very pass being waited on.
//
// A wait and not an error, deliberately: an error here would write an imap_idle
// error run every time an ingest pass overlaps — the cry-wolf failure the
// locking was added to avoid (and, under D9, a row that looks exactly like a
// revoked consent).
func TestIdleOnce_BusyAccountOpensNothingAndWaits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sink := newMWFakeSink()
	sink.busy = true
	clock := newMWClock(0)

	opens := 0
	open := func(context.Context, google.Account) (idleConn, error) {
		opens++
		return nil, errors.New("unreachable")
	}

	err := idleOnce(ctx, idleDeps{Sink: sink, Open: open, Sleep: clock.Sleep, IdleRefresh: 25 * time.Minute},
		mwAccount(2, "sspataro@gmail.com"), make(chan string, 1))
	if err != nil {
		t.Errorf("idleOnce = %v while an ingest pass held the account lock, want nil. An error here writes "+
			"an imap_idle error run on every ordinary overlap — an alarm that fires on normal traffic is "+
			"worse than no alarm (criterion 8/9)", err)
	}
	if opens != 0 {
		t.Errorf("idleOnce opened %d connection(s) without the lock. The whole point of the lock is that "+
			"the mint happens under it (criteria 8, 22)", opens)
	}
	if len(sink.snapshot()) != 0 {
		t.Errorf("a busy cycle wrote sync_runs rows: %v. An overlap is normal operation under D3's "+
			"2-hourly CronJob, not a failure", sink.snapshot())
	}
	waits := clock.sleeps()
	if len(waits) != 1 || waits[0] <= 0 {
		t.Errorf("a busy cycle slept %v, want exactly one positive wait. Returning immediately makes "+
			"watchAccount re-enter at once and spin on pg_try_advisory_lock for the whole ingest pass", waits)
	}
}

// Criterion 8, the failure branch: when the open FAILS the lock must still be
// released. A cycle that leaks the per-account lock blocks every later ingest
// pass for that mailbox — which under D9 then counts accounts_busy forever and
// looks exactly like a healthy skip.
func TestIdleOnce_ReleasesTheLockWhenTheMintFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sink := newMWFakeSink()
	open := func(context.Context, google.Account) (idleConn, error) {
		sink.note("open")
		return nil, errors.New("no credential for sspataro57@msn.com: refresh token rejected: invalid_grant")
	}

	err := idleOnce(ctx, idleDeps{Sink: sink, Open: open, Sleep: (&mwClock{}).Sleep, IdleRefresh: time.Minute},
		mwAccount(4, "sspataro57@msn.com"), make(chan string, 1))
	if err == nil {
		t.Error("idleOnce swallowed a credential failure. watchAccount needs the error to back off and to " +
			"write the imap_idle error row (criteria 9, 10)")
	}
	order := sink.lockOrder()
	locks, releases := 0, 0
	for _, e := range order {
		switch e {
		case "lock":
			locks++
		case "release":
			releases++
		}
	}
	if locks != releases {
		t.Errorf("the per-account lock was taken %d time(s) and released %d time(s) on a failing cycle "+
			"(order: %v). A leaked advisory lock is held for the life of the pooled connection and every "+
			"later ingest pass for that mailbox counts accounts_busy and does nothing — indistinguishable "+
			"from a healthy skip", locks, releases, order)
	}
}
