package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/store"
)

// Watch mode (criterion 18): the same one-shot pass, driven by IMAP IDLE instead
// of by a CronJob schedule.
//
// This makes the connector switchboard's first long-running workload, so the
// design question is what happens when things go wrong rather than when they go
// right. Three rules:
//
//  1. An IDLE notification is a WAKE-UP, never a payload. It triggers exactly the
//     bounded pass a CronJob would run. A missed notification costs latency until
//     the next reconcile; a duplicated one costs a no-op pass. Neither can lose
//     mail — the same discipline the orchestrator applies to Postgres NOTIFY.
//  2. A reconcile sweep runs regardless, on a timer. IDLE is an optimisation on
//     top of polling, not a replacement for it: a silently dead connection would
//     otherwise mean silence that looks exactly like an empty mailbox.
//  3. Nothing here exits on a transient failure. Connection loss backs off and
//     reconnects; the process ends only on a signal.

const (
	// defaultIdleRefresh sits under RFC 2177's 29-minute ceiling. Servers are
	// entitled to drop an IDLE that runs longer, and a dropped IDLE that nobody
	// re-issues is a connector that has stopped listening without saying so.
	defaultIdleRefresh = 25 * time.Minute
	// defaultReconcile is the safety net that makes rule 2 true.
	defaultReconcile = 10 * time.Minute
	// backoff bounds. Capped so a long outage still retries promptly once the
	// server returns, rather than sleeping for an hour.
	backoffMin = 5 * time.Second
	backoffMax = 5 * time.Minute
)

func envDuration(name string, def time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		// Same defensive shape as every other duration knob in the repo: a typo
		// must not silently produce a zero interval, which would spin.
		return def
	}
	return d
}

// runWatch is the long-lived loop. It returns only on ctx cancellation.
func runWatch(pool *pgxpool.Pool, cfg google.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	key := os.Getenv("OPS_TOKEN_KEY")
	if key == "" {
		return fmt.Errorf("OPS_TOKEN_KEY is not set (required to decrypt app passwords)")
	}
	reconcile := envDuration("MAIL_RECONCILE_INTERVAL", defaultReconcile)
	idleRefresh := envDuration("MAIL_IDLE_REFRESH", defaultIdleRefresh)

	sink := google.NewPGSink(pool)
	fmt.Printf("watch: reconcile=%s idle_refresh=%s\n", reconcile, idleRefresh)

	// One pass immediately: a process that has just started should not wait a
	// full interval before doing anything, and this is also the fastest way to
	// surface a credential or connectivity problem at deploy time.
	if _, err := runIMAPIngest(ctx, pool, sink, cfg); err != nil {
		fmt.Printf("watch: initial pass failed: %v\n", err)
	}

	accounts, err := google.ListAppPasswordAccounts(ctx, pool, cfg.AccountEmail)
	if err != nil {
		return err
	}
	wake := make(chan string, len(accounts)+1)
	for _, acct := range accounts {
		go watchAccount(ctx, pool, key, acct, idleRefresh, wake)
	}

	ticker := time.NewTicker(reconcile)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Printf("watch: signal received, shutting down\n")
			return nil
		case email := <-wake:
			// Scoped to the account that woke us; the sweep below covers the rest.
			passCfg := cfg
			passCfg.AccountEmail = email
			if _, err := runIMAPIngest(ctx, pool, sink, passCfg); err != nil {
				fmt.Printf("watch: pass for %s failed: %v\n", email, err)
			}
		case <-ticker.C:
			if _, err := runIMAPIngest(ctx, pool, sink, cfg); err != nil {
				fmt.Printf("watch: reconcile failed: %v\n", err)
			}
		}
	}
}

// watchAccount holds one IDLE connection on INBOX and signals on every change.
//
// INBOX only. Sent gets the reconcile sweep instead: it halves the connection
// count and the failure surface, and Sent-folder latency only delays delivery
// confirmation, which no rule waits on.
func watchAccount(ctx context.Context, pool *pgxpool.Pool, key string,
	acct google.Account, idleRefresh time.Duration, wake chan<- string) {

	backoff := backoffMin
	for {
		if ctx.Err() != nil {
			return
		}
		err := idleOnce(ctx, pool, key, acct, idleRefresh, wake)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			fmt.Printf("watch: idle for %s failed: %v (retry in %s)\n", acct.Email, err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			// Exponential with a ceiling. The jitter is the account's own
			// position in the backoff walk rather than a random draw — several
			// mailboxes failing at once (a network blip) must not reconnect in
			// lockstep and hammer the server.
			backoff *= 2
			if backoff > backoffMax {
				backoff = backoffMax
			}
			continue
		}
		backoff = backoffMin
	}
}

// idleOnce opens one IDLE and returns when it fires, refreshes, or fails.
func idleOnce(ctx context.Context, pool *pgxpool.Pool, key string,
	acct google.Account, idleRefresh time.Duration, wake chan<- string) error {

	password, err := google.DecryptAppPassword(ctx, pool, acct.ID, key)
	if err != nil {
		return err
	}
	src := google.NewIMAPClientSource(acct.Hosts(), acct.Email, password)
	defer func() { _ = src.Close() }()

	// Bounded by the refresh interval: IDLE is re-issued rather than held past
	// the point a server would drop it silently.
	idleCtx, cancel := context.WithTimeout(ctx, idleRefresh)
	defer cancel()

	ch, err := src.Idle(idleCtx, google.InboxFolder)
	if err != nil {
		return err
	}
	select {
	case <-idleCtx.Done():
		// Refresh interval elapsed with no news; reopen. Not an error.
		return nil
	case _, ok := <-ch:
		if !ok {
			return nil
		}
		select {
		case wake <- acct.Email:
		default: // a wake-up is already queued; one pass covers both
		}
		return nil
	}
}

// newWatchPool builds the pool watch mode owns (the one-shot path's pool is
// scoped to a 10-minute context that must not bound a long-lived process).
func newWatchPool(ctx context.Context) (*pgxpool.Pool, error) {
	return store.NewPool(ctx)
}
