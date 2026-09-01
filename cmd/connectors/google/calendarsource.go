package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

// calendarPhaseRuns reports whether THIS pass owns the calendar phase (SWT-24
// criterion 18). Only the imap source does: bridge and gmail_api already
// ingest calendar inline (google.RunBridge / google.Run), so running it again
// would give one account two calendar passes in one invocation — two sync_runs
// rows, two token writes, and a race between them for the same cursor key.
func calendarPhaseRuns(source mailSource) bool {
	return source == mailSourceIMAP
}

// calendarClientFactory builds the per-account Calendar client. Injected so
// the failing-token path is testable without a Google credential (SWT-24
// criterion 21): production wraps TokenClient, tests return an error or a
// client aimed at httptest.
type calendarClientFactory func(ctx context.Context, acct google.Account) (*google.CalendarClient, error)

// runCalendarIngest ingests every calendar-credentialed account (SWT-24
// criterion 17), shaped after its sibling runIMAPIngest: one per-account
// advisory lock, one failing account recorded without aborting the others, the
// pass failing only if NONE succeeded, and a printed line when there are zero
// credentialed accounts — a zero-work pass must never look like a working one.
//
// Selection is CREDENTIAL-gated (a refresh token AND the calendar scope),
// never auth_type-gated: auth_type names the MAIL path and keeps saying
// 'app_password' after consent.
func runCalendarIngest(ctx context.Context, pool *pgxpool.Pool, sink *google.PGSink,
	newClient calendarClientFactory, cfg google.Config) (google.Stats, error) {
	var total google.Stats

	accounts, err := google.ListCalendarCredentialedAccounts(ctx, pool, cfg.AccountEmail)
	if err != nil {
		return total, err
	}
	if len(accounts) == 0 {
		// Not an error: this is the state of production until the consent flow
		// has been run (docs/runbooks/calendar-availability.md). No sync_runs
		// row is written — a run row for no work would be a freshness signal
		// for a calendar nobody read.
		fmt.Printf("calendar: no google accounts with OAuth calendar credentials (run google-auth add-calendar first)\n")
		return total, nil
	}

	var firstErr error
	succeeded := 0
	for _, acct := range accounts {
		release, ok, err := sink.LockAccount(ctx, acct.ID)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !ok {
			total.AccountsBusy++
			continue
		}

		cc, err := newClient(ctx, acct)
		if err != nil {
			// A factory failure (revoked consent, refused token refresh) happens
			// BEFORE IngestCalendar can open its own run. Record it anyway: a
			// failure that writes nothing is invisible — the account simply
			// looks never-polled, and nothing distinguishes a revoked consent
			// from a CronJob that has not run yet. The error row also keeps
			// Part A honest: no ok row, so propose_slots keeps refusing.
			if runID, runErr := sink.StartRun(ctx, acct.ID, "calendar"); runErr == nil {
				_ = sink.FinishRun(ctx, runID, "error", google.Stats{}, err.Error())
			}
			release()
			fmt.Printf("calendar: account %s failed: %v\n", acct.Email, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		stats, err := google.IngestCalendar(ctx, cc, sink, acct, cfg)
		release()
		total = addStats(total, stats)
		total.CalendarListed += stats.CalendarListed
		total.CalendarResets += stats.CalendarResets
		total.CalendarSuperseded += stats.CalendarSuperseded
		if err != nil {
			// Recorded, not fatal: IngestCalendar already marked the sync_runs
			// row error, and one bad calendar must not blind the others.
			fmt.Printf("calendar: account %s failed: %v\n", acct.Email, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		succeeded++
	}
	if succeeded == 0 && firstErr != nil {
		return total, firstErr
	}
	return total, nil
}

// productionCalendarClientFactory is the live factory: decrypt the refresh
// token, build the oauth2 client, aim it at the real API. Everything is read
// lazily per account so a deployment with zero credentialed accounts (today's
// production) never needs OPS_TOKEN_KEY-for-calendar or a client secret file.
func productionCalendarClientFactory(pool *pgxpool.Pool) calendarClientFactory {
	return func(ctx context.Context, acct google.Account) (*google.CalendarClient, error) {
		key := os.Getenv("OPS_TOKEN_KEY")
		if key == "" {
			return nil, fmt.Errorf("OPS_TOKEN_KEY is not set (required to decrypt the calendar refresh token)")
		}
		secretFile := os.Getenv("GOOGLE_CLIENT_SECRET_FILE")
		if secretFile == "" {
			home, _ := os.UserHomeDir()
			secretFile = filepath.Join(home, ".config", "switchboard", "google_client_secret.json")
		}
		oauthCfg, err := google.LoadOAuthConfig(secretFile, "")
		if err != nil {
			return nil, err
		}
		// Rotation persistence re-writes the row's scopes, and these rows'
		// scopes ARE the calendar-only set add-calendar stored (criterion 14).
		oauthCfg.Scopes = google.CalendarScopes
		hc, err := google.TokenClient(ctx, pool, oauthCfg, acct, key)
		if err != nil {
			return nil, err
		}
		return google.NewCalendarClient(hc, ""), nil
	}
}
