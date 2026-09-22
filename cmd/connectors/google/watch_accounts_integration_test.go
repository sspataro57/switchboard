//go:build integration

package main

// imap-idle-watch (SWT-73) criterion 14 / D10 against a real Postgres: "watch
// the accounts table, not a snapshot of it". The unit half (watch_test.go)
// proves the loop re-reads its Accounts dep; this half proves the production
// reader — google.ListIMAPAccounts over source_accounts — is what it re-reads,
// so a row inserted by `google-auth add` mid-run is picked up within one
// reconcile tick.
//
// Nothing here touches IMAP: the IDLE seam is a fake that blocks on ctx. The
// question is which ACCOUNTS get a goroutine, and that question is a database
// question.
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_idlewatch?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run MailWatchAccounts ./cmd/connectors/google/
//
// Cleanup deletes the rows this test inserted BY ID, before and after, so the
// suite is rerunnable against a persistent database (IK: the executor
// integration test that passed on a fresh db and failed on rerun).
//
// GREENFIELD NOTE — EXPECTED RED: newWatcher, watchConfig, watchDeps and
// lockHandle do not exist, so this file compile-FAILS under -tags integration.
//
// MUTATION: list accounts once at startup -> criterion 14.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

const mwAcctKey = "itest-idlewatch-accounts-key"

// mwSeedAccount inserts one app_password mailbox and registers its deletion.
func mwSeedAccount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, email string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts
		   (provider, account_email, auth_type, app_password_encrypted, scopes, send_enabled,
		    calendar_in_availability, imap_host, imap_port, smtp_host, smtp_port)
		 VALUES ('google', $1, $2, pgp_sym_encrypt($3::text, $4), '{}', false, false,
		         '127.0.0.1', 9, '127.0.0.1', 9)
		 RETURNING id`,
		email, google.AuthTypeAppPassword, "app-password-"+email, mwAcctKey).Scan(&id); err != nil {
		t.Fatalf("seed account %s: %v", email, err)
	}
	t.Cleanup(func() { mwDeleteAccount(t, context.Background(), pool, id) })
	return id
}

// mwDeleteAccount removes one seeded mailbox and its children BY ID — never by
// a LIKE over a key column (repo rule).
func mwDeleteAccount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) {
	t.Helper()
	for _, stmt := range []string{
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id=$1)`,
		`DELETE FROM normalized_events   WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id=$1)`,
		`DELETE FROM raw_source_items    WHERE source_account_id=$1`,
		`DELETE FROM sync_runs           WHERE source_account_id=$1`,
		`DELETE FROM source_accounts     WHERE id=$1`,
	} {
		if _, err := pool.Exec(ctx, stmt, id); err != nil {
			t.Fatalf("cleanup %q for account %d: %v", stmt, id, err)
		}
	}
}

func TestMailWatchAccounts_Integration_AMailboxAddedMidRunGetsAnIdleGoroutine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := mwPool(t, ctx)
	t.Setenv("OPS_TOKEN_KEY", mwAcctKey)

	firstEmail := "itest-idlewatch-first@example.com"
	secondEmail := "itest-idlewatch-second@example.com"
	mwSeedAccount(t, ctx, pool, firstEmail)

	lock := &mwFakeLock{}
	var mu sync.Mutex
	starts := map[string]int{}
	seenFirst := make(chan struct{})
	seenSecond := make(chan struct{})
	var onceFirst, onceSecond sync.Once

	w := newWatcher(
		watchConfig{Reconcile: 50 * time.Millisecond, IdleRefresh: time.Minute, PassTimeout: 5 * time.Second,
			StandbyRetry: 15 * time.Second, HealthAddr: ":0"},
		google.Config{},
		watchDeps{
			Now:     time.Now,
			Sleep:   (&mwClock{}).Sleep,
			Acquire: func(context.Context) (lockHandle, bool, error) { return lock, true, nil },
			// The PRODUCTION reader: whatever this returns is the set of mailboxes
			// the watcher listens to.
			Accounts: func(ctx context.Context) ([]google.Account, error) {
				return google.ListIMAPAccounts(ctx, pool, "")
			},
			Idle: func(ctx context.Context, acct google.Account, _ chan<- string) error {
				mu.Lock()
				starts[acct.Email]++
				mu.Unlock()
				switch acct.Email {
				case firstEmail:
					onceFirst.Do(func() { close(seenFirst) })
				case secondEmail:
					onceSecond.Do(func() { close(seenSecond) })
				}
				<-ctx.Done()
				return ctx.Err()
			},
			Pass: func(context.Context, google.Config, string) {},
			Sink: newMWFakeSink(),
		})

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	select {
	case <-seenFirst:
	case err := <-done:
		t.Fatalf("Run returned early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the mailbox that existed at startup never got an IDLE goroutine")
	}

	// `google-auth add` while the watcher runs.
	mwSeedAccount(t, ctx, pool, secondEmail)

	select {
	case <-seenSecond:
	case err := <-done:
		t.Fatalf("Run returned early: %v", err)
	case <-time.After(10 * time.Second):
		mu.Lock()
		got := map[string]int{}
		for k, v := range starts {
			got[k] = v
		}
		mu.Unlock()
		t.Fatalf("a mailbox inserted into source_accounts mid-run never got an IDLE goroutine (starts: %v). "+
			"Criterion 14 / D10: the account list is RE-READ on every reconcile tick. Listing once at "+
			"startup (watch.go:88-95 today) means a mailbox onboarded at 10:05 is ingested only by the "+
			"sweep, silently at ten-minute latency — precisely the complaint this ticket answers", got)
	}

	// Let several more ticks pass, then assert nobody was started twice.
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation (criterion 4)")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, email := range []string{firstEmail, secondEmail} {
		if starts[email] != 1 {
			t.Errorf("%s got %d IDLE goroutines across ~6 reconcile ticks, want exactly 1. A goroutine per "+
				"tick per account is four new IMAP connections every ten minutes and four wakes per "+
				"message (criterion 14, D12)", email, starts[email])
		}
	}
	if len(w.Watching()) < 2 {
		t.Errorf("Watching() = %v, want it to include both seeded mailboxes", w.Watching())
	}
}
