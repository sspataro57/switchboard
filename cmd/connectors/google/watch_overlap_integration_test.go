//go:build integration

package main

// imap-idle-watch (SWT-73) criteria 21 and 22 against a real Postgres: the
// overlap between the resident watcher and the one-shot pass.
//
// These are LOAD-BEARING, not regression guards. OQ-1 was answered B-reduced
// (Salvador, 2026-09-22: "move it to every 2 hours to catch misses"), so
// cronjob/connector-google keeps running beside deployment/connector-google-watch
// and the two WILL meet on the same four mailboxes — twelve times a day, by
// design. D5's analysis is that this is safe, but safe "by an invariant, not by
// a schema constraint": the per-account advisory lock serializes across
// PROCESSES because pg_advisory_lock is database-wide, and idleOnce's early
// release is correct only because its cycle makes exactly one connection.
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_idlewatch?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run MailWatchOverlap ./cmd/connectors/google/
//
// NO live IMAP and NO live Microsoft: the mailboxes point at 127.0.0.1:9 (a
// refused connection, instantly, provably local) and the token endpoint is an
// httptest server. Everything asserted here happens at credential-resolution
// and lock time, before any dial — which is exactly why it is testable offline.
//
// Seeded rows are deleted BY ID (mwDeleteAccount), before and after, so the
// suite is rerunnable against a persistent database.
//
// GREENFIELD NOTE — EXPECTED RED: idleOnce's new signature (idleDeps, idleConn,
// openIdleConn) does not exist, so this file compile-FAILS under
// -tags integration. runIMAPIngest keeps today's signature and is unchanged by
// this ticket.
//
// MUTATIONS: drop the per-account lock from runIMAPIngest -> criteria 21, 22;
// release the per-account lock before the mint -> criteria 8, 22.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

// ---- criterion 21: the loser skips, and nothing rolls backwards ---------------

func TestMailWatchOverlap_Integration_TheLoserCountsAccountsBusy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := mwPool(t, ctx)
	t.Setenv("OPS_TOKEN_KEY", mwAcctKey)

	email := "itest-idlewatch-overlap@example.com"
	id := mwSeedAccount(t, ctx, pool, email)
	sink := google.NewPGSink(pool)

	// A cursor to protect: a UID position the watcher already reached. The
	// failure this pins is the one mailsource.go's comment describes — two
	// passes both read the cursor, both spend a long time in IMAP, and the
	// slower one writes back last, committing a position for messages the other
	// stored, so the gap between them is never re-fetched.
	const seededUIDNext = 100
	seeded := google.Cursor{IMAPFolders: map[string]google.FolderCursor{
		google.InboxFolder: {UIDValidity: 7, UIDNext: seededUIDNext},
	}}
	if err := sink.SaveCursor(ctx, id, seeded); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}
	rawBefore := mwCountRaw(t, ctx, pool, id)

	acct := google.Account{ID: id, Email: email, AuthType: google.AuthTypeAppPassword,
		IMAPHost: "127.0.0.1", IMAPPort: 9, SMTPHost: "127.0.0.1", SMTPPort: 9}

	opened := make(chan struct{})
	proceed := make(chan struct{})
	open := func(octx context.Context, a google.Account) (idleConn, error) {
		// The watcher is inside its lock window: credential resolved, connection
		// about to be handed back.
		src, err := google.OpenIMAPSource(octx, pool, a, mwAcctKey)
		close(opened)
		<-proceed
		if err != nil {
			return nil, err
		}
		return src, nil
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// The error is expected: the IDLE itself cannot dial 127.0.0.1:9. What
		// matters is that the LOCK was held across the resolution.
		_ = idleOnce(ctx, idleDeps{Sink: sink, Open: open, Sleep: (&mwClock{}).Sleep,
			IdleRefresh: time.Minute}, acct, make(chan string, 1))
	}()

	select {
	case <-opened:
	case <-time.After(10 * time.Second):
		t.Fatal("idleOnce never reached its open step")
	}

	// The CronJob's tick lands mid-window.
	stats, err := runIMAPIngest(ctx, pool, sink, google.Config{AccountEmail: email})
	close(proceed)
	wg.Wait()

	if err != nil {
		t.Fatalf("runIMAPIngest over a busy account = %v; an overlap is normal operation under D3's "+
			"2-hourly net, not a failed run", err)
	}
	if stats.AccountsBusy != 1 {
		t.Errorf("stats.AccountsBusy = %d, want 1. Criterion 21: with the watcher holding the account's "+
			"advisory lock, the one-shot pass must SKIP and say so — pg_advisory_lock is database-wide, so "+
			"the per-account lock serializes across PROCESSES, which is the whole reason side-by-side "+
			"operation is safe (D5)", stats.AccountsBusy)
	}
	if stats.IMAPFetched != 0 || stats.RawInserted != 0 {
		t.Errorf("the skipped pass still fetched (%+v); a busy account is not read at all", stats)
	}
	if got := mwCountRaw(t, ctx, pool, id); got != rawBefore {
		t.Errorf("raw_source_items for the account went from %d to %d during an overlap. Criterion 21: no "+
			"duplicate raw row appears beyond the content-hash upsert", rawBefore, got)
	}

	after, err := sink.Cursor(ctx, id)
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if got := after.IMAPFolders[google.InboxFolder].UIDNext; got < seededUIDNext {
		t.Errorf("the INBOX cursor moved BACKWARDS, %d -> %d. Criterion 21: it ends at the higher of the "+
			"two positions, never lower — a rolled-back UID position is mail that is never re-fetched",
			seededUIDNext, got)
	}
}

func mwCountRaw(t *testing.T, ctx context.Context, pool *pgxpool.Pool, accountID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM raw_source_items WHERE source_account_id=$1`,
		accountID).Scan(&n); err != nil {
		t.Fatalf("count raw_source_items: %v", err)
	}
	return n
}

// ---- criterion 22: the MSN rotation, interleaved ------------------------------

// mwRotatingAuthority is a Microsoft token endpoint that ROTATES on every
// redemption and refuses a stale token exactly as the real one does. It counts
// mints and, crucially, stale redemptions: a stale redemption is what produces
// invalid_grant on the one mailbox that cannot be re-consented from a script,
// and it is indistinguishable in sync_runs from a genuinely revoked consent.
type mwRotatingAuthority struct {
	mu      sync.Mutex
	current string
	issued  []string
	mints   int
	stale   int
	pause   time.Duration
}

func mwNewRotatingAuthority(t *testing.T, first string, pause time.Duration) (*mwRotatingAuthority, *httptest.Server) {
	t.Helper()
	a := &mwRotatingAuthority{current: first, pause: pause}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		presented := r.Form.Get("refresh_token")
		a.mu.Lock()
		pause := a.pause
		stale := presented != a.current
		if stale {
			a.stale++
		}
		a.mu.Unlock()
		if pause > 0 {
			time.Sleep(pause)
		}
		w.Header().Set("Content-Type", "application/json")
		if stale {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":             "invalid_grant",
				"error_description": "AADSTS700082: the refresh token presented has already been redeemed",
			})
			return
		}
		a.mu.Lock()
		a.mints++
		next := "rotated-refresh-token-" + time.Now().Format("150405.000000000")
		a.current = next
		a.issued = append(a.issued, next)
		a.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-token",
			"refresh_token": next,
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return a, srv
}

func (a *mwRotatingAuthority) counts() (mints, stale int, last string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.issued) > 0 {
		last = a.issued[len(a.issued)-1]
	}
	return a.mints, a.stale, last
}

// mwSeedXOAuth2 seeds the MSN-shaped mailbox: xoauth2, a stored refresh token,
// an endpoint that refuses instantly and locally.
func mwSeedXOAuth2(t *testing.T, ctx context.Context, pool *pgxpool.Pool, email, refresh string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts
		   (provider, account_email, auth_type, refresh_token_encrypted, scopes, send_enabled,
		    calendar_in_availability, imap_host, imap_port, smtp_host, smtp_port)
		 VALUES ('google', $1, $2, pgp_sym_encrypt($3::text, $4), '{}', false, false,
		         '127.0.0.1', 9, '127.0.0.1', 9)
		 RETURNING id`,
		email, google.AuthTypeXOAuth2, refresh, mwAcctKey).Scan(&id); err != nil {
		t.Fatalf("seed xoauth2 account %s: %v", email, err)
	}
	t.Cleanup(func() { mwDeleteAccount(t, context.Background(), pool, id) })
	return id
}

// Criterion 22: a watcher cycle and a one-shot pass interleaved on the same
// xoauth2 account never redeem the same stored refresh token twice, and the
// stored token equals the last one the authority issued.
//
// D5, in full: "the ingest pass holds [the lock] from before OpenIMAPSource
// until after src.Close(), so every mint that pass makes happens under the
// lock; idleOnce holds it across OpenIMAPSource only and releases before going
// idle, which is correct because that cycle makes exactly one connection."
// Removing the lock from either call site makes the two mints race, the loser
// presents a token the authority has already redeemed, and D9 dutifully records
// a per-account error row that looks exactly like a genuinely revoked consent —
// an alarm that fires on ordinary traffic.
func TestMailWatchOverlap_Integration_XOAuth2RotationSurvivesInterleaving(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := mwPool(t, ctx)

	auth, srv := mwNewRotatingAuthority(t, "seed-refresh-token", 150*time.Millisecond)
	t.Setenv("OPS_TOKEN_KEY", mwAcctKey)
	t.Setenv("MS_OAUTH_CLIENT_ID", "11111111-2222-3333-4444-555555555555")
	t.Setenv("MS_OAUTH_AUTHORITY", srv.URL)

	email := "itest-idlewatch-msn@example.com"
	id := mwSeedXOAuth2(t, ctx, pool, email, "seed-refresh-token")
	sink := google.NewPGSink(pool)
	acct := google.Account{ID: id, Email: email, AuthType: google.AuthTypeXOAuth2,
		IMAPHost: "127.0.0.1", IMAPPort: 9, SMTPHost: "127.0.0.1", SMTPPort: 9}

	// --- phase 1: genuinely concurrent, the window held open on purpose -------
	opened := make(chan struct{})
	proceed := make(chan struct{})
	open := func(octx context.Context, a google.Account) (idleConn, error) {
		src, err := google.OpenIMAPSource(octx, pool, a, mwAcctKey) // the mint, under the lock
		close(opened)
		<-proceed
		if err != nil {
			return nil, err
		}
		return src, nil
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = idleOnce(ctx, idleDeps{Sink: sink, Open: open, Sleep: (&mwClock{}).Sleep,
			IdleRefresh: time.Minute}, acct, make(chan string, 1))
	}()
	select {
	case <-opened:
	case <-time.After(15 * time.Second):
		t.Fatal("idleOnce never reached its open step")
	}
	_, _ = runIMAPIngest(ctx, pool, sink, google.Config{AccountEmail: email})
	close(proceed)
	wg.Wait()

	mints, stale, last := auth.counts()
	if stale != 0 {
		t.Errorf("%d redemption(s) presented an already-redeemed refresh token. Criterion 22: with the "+
			"per-account lock held across the mint at BOTH call sites, mint-versus-mint is serialized; a "+
			"stale redemption is invalid_grant on the one mailbox that cannot be re-consented from a "+
			"script, and it reads in sync_runs exactly like a revoked consent (D5)", stale)
	}
	if mints != 1 {
		t.Errorf("the authority minted %d times while the watcher held the account lock, want exactly 1. "+
			"The one-shot pass must count accounts_busy and mint NOTHING (criteria 21, 22)", mints)
	}
	if got := mwStoredRefresh(t, ctx, pool, id); got != last {
		t.Errorf("the stored refresh token is %q, the last one issued is %q. Rotation is persisted as a "+
			"side effect of minting (credential.go:60-71); dropping it kills the mailbox days later, when "+
			"the old token ages out, for no visible reason", got, last)
	}

	// --- phase 2: the same two callers, now sequential ------------------------
	// Both mint, in turn, each under the lock, each presenting the token the
	// previous one stored. This is the steady state under D3's 2-hourly net.
	_, _ = runIMAPIngest(ctx, pool, sink, google.Config{AccountEmail: email})
	_ = idleOnce(ctx, idleDeps{
		Sink: sink,
		Open: func(octx context.Context, a google.Account) (idleConn, error) {
			src, err := google.OpenIMAPSource(octx, pool, a, mwAcctKey)
			if err != nil {
				return nil, err
			}
			return src, nil
		},
		Sleep:       (&mwClock{}).Sleep,
		IdleRefresh: time.Minute,
	}, acct, make(chan string, 1))

	mints, stale, last = auth.counts()
	if stale != 0 {
		t.Errorf("%d stale redemption(s) after two sequential passes; each mint must present the token the "+
			"PREVIOUS one stored (criterion 22)", stale)
	}
	if mints != 3 {
		t.Errorf("the authority minted %d times in total, want 3 (one in the concurrent phase, one per "+
			"sequential pass). Fewer means a pass silently skipped; more means a cycle minted twice, which "+
			"is criterion 8's invariant breaking", mints)
	}
	if got := mwStoredRefresh(t, ctx, pool, id); got != last {
		t.Errorf("after interleaved passes the stored refresh token is %q, want the last one issued (%q) "+
			"(criterion 22)", got, last)
	}
}

func mwStoredRefresh(t *testing.T, ctx context.Context, pool *pgxpool.Pool, accountID int64) string {
	t.Helper()
	got, err := google.DecryptRefreshToken(ctx, pool, accountID, mwAcctKey)
	if err != nil {
		t.Fatalf("decrypt the stored refresh token: %v", err)
	}
	return got
}
