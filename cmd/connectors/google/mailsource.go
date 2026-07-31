package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

// mailSource is which path this pass uses to read mail.
type mailSource string

const (
	mailSourceIMAP     mailSource = "imap"
	mailSourceBridge   mailSource = "bridge"
	mailSourceGmailAPI mailSource = "gmail_api"
)

// selectMailSource resolves MAIL_SOURCE + GMAIL_CONNECTOR_BRIDGE into a path.
//
// Explicit rather than inferred, and that is the whole point. Reading the source
// off source_accounts.auth_type would look tidier and would mean the code path
// silently changed the moment a row was added — with no manifest diff, no log
// line, and nothing to grep. An env var is visible in the Deployment, greppable
// in the repo, and testable without a database.
//
// Unset preserves today's behaviour byte for byte: a configured bridge binary
// means the bridge, otherwise the direct Gmail API. Nothing about the existing
// deployment changes until someone sets MAIL_SOURCE deliberately.
//
// An unknown value is an error. The alternative — falling back to a default —
// turns a typo like MAIL_SOURCE=imaps into a connector that quietly keeps using
// OAuth against mailboxes whose tokens were never issued, and reports success.
func selectMailSource(mailSourceEnv, bridgeBinary string) (mailSource, error) {
	switch mailSourceEnv {
	case "":
		if bridgeBinary != "" {
			return mailSourceBridge, nil
		}
		return mailSourceGmailAPI, nil
	case string(mailSourceIMAP):
		return mailSourceIMAP, nil
	case string(mailSourceGmailAPI):
		return mailSourceGmailAPI, nil
	case string(mailSourceBridge):
		if bridgeBinary == "" {
			return "", fmt.Errorf("MAIL_SOURCE=bridge but GMAIL_CONNECTOR_BRIDGE is not set")
		}
		return mailSourceBridge, nil
	default:
		return "", fmt.Errorf("unknown MAIL_SOURCE %q (want imap, bridge or gmail_api)", mailSourceEnv)
	}
}

// runIMAPIngest ingests every app-password account over IMAP.
//
// One connection per account, closed at the end of its pass: a one-shot CronJob
// run has no reason to hold a socket open, and watch mode manages its own.
// Accounts are processed independently — one unreachable mailbox must not stop
// the others, so the error is recorded and the pass continues, failing at the
// end only if nothing succeeded.
func runIMAPIngest(ctx context.Context, pool *pgxpool.Pool, sink *google.PGSink, cfg google.Config) (google.Stats, error) {
	var total google.Stats

	key := os.Getenv("OPS_TOKEN_KEY")
	if key == "" {
		return total, fmt.Errorf("OPS_TOKEN_KEY is not set (required to decrypt app passwords)")
	}

	accounts, err := google.ListAppPasswordAccounts(ctx, pool, cfg.AccountEmail)
	if err != nil {
		return total, err
	}
	if len(accounts) == 0 {
		// Not an error: a deployment may be mid-migration with no app-password
		// mailbox onboarded yet. Silence here would look like a working pass.
		fmt.Printf("imap: no provider='google' accounts with auth_type='app_password'\n")
		return total, nil
	}

	var firstErr error
	succeeded := 0
	for _, acct := range accounts {
		// Same per-account advisory lock the bridge path takes. Without it a
		// stray CronJob overlapping the watch Deployment interleaves with it:
		// both read the cursor, both spend a long time in IMAP, and the slower
		// one writes back last — committing a cursor position for messages the
		// other stored, so the gap between them is never re-fetched.
		release, ok, err := sink.LockAccount(ctx, acct.ID)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !ok {
			// Held by another pass doing the same work; skipping is correct.
			total.AccountsBusy++
			continue
		}

		password, err := google.DecryptAppPassword(ctx, pool, acct.ID, key)
		if err != nil {
			release()
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		src := google.NewIMAPClientSource(acct.Hosts(), acct.Email, password)
		stats, err := google.IngestIMAP(ctx, src, sink, acct, cfg)
		_ = src.Close()
		release()

		total = addStats(total, stats)
		if err != nil {
			// Recorded, not fatal: the account's sync_runs row is already marked
			// error by IngestIMAP, and one bad mailbox must not blind the others.
			fmt.Printf("imap: account %s failed: %v\n", acct.Email, err)
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

// addStats folds one account's counters into the run total.
func addStats(a, b google.Stats) google.Stats {
	a.RawInserted += b.RawInserted
	a.RawUpdated += b.RawUpdated
	a.RawUnchanged += b.RawUnchanged
	a.IMAPFetched += b.IMAPFetched
	a.IMAPTruncated += b.IMAPTruncated
	a.AccountsBusy += b.AccountsBusy
	return a
}
