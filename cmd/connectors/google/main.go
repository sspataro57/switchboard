// google is the one-shot Gmail + Calendar poller (SPEC 07-google-oauth-pollers):
// per provider='google' account, gmail phase then calendar phase (raw-first),
// then the normalize phase (dedup by Message-ID, direction rule). Scheduling
// is external (cron, 5-15 min).
//
//	google [--full] [--normalize-only] [--all] [--overlap 1h] [--backfill 2160h] [--account email]
//
//	DATABASE_URL               ops db, required
//	OPS_TOKEN_KEY              required unless --normalize-only
//	GOOGLE_CLIENT_SECRET_FILE  default ~/.config/switchboard/google_client_secret.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/store"
)

func main() {
	full := flag.Bool("full", false, "rescan the backfill window (ignore gmail cursor, drop calendar sync token)")
	normalizeOnly := flag.Bool("normalize-only", false, "skip ingest; normalize from raw_source_items alone")
	all := flag.Bool("all", false, "normalize every raw row, not only pending ones")
	overlap := flag.Duration("overlap", google.DefaultOverlap, "gmail cursor re-read window")
	backfill := flag.Duration("backfill", google.DefaultBackfill, "gmail initial backfill window")
	account := flag.String("account", "", "limit to one account email")
	flag.Parse()

	if err := run(*full, *normalizeOnly, *all, *overlap, *backfill, *account); err != nil {
		fmt.Fprintln(os.Stderr, "google:", err)
		os.Exit(1)
	}
}

func run(full, normalizeOnly, all bool, overlap, backfill time.Duration, account string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	sink := google.NewPGSink(pool)

	cfg := google.Config{Full: full, All: all, Overlap: overlap, Backfill: backfill, AccountEmail: account}

	if !normalizeOnly {
		key := os.Getenv("OPS_TOKEN_KEY")
		if key == "" {
			return fmt.Errorf("OPS_TOKEN_KEY is not set")
		}
		secretFile := os.Getenv("GOOGLE_CLIENT_SECRET_FILE")
		if secretFile == "" {
			home, _ := os.UserHomeDir()
			secretFile = filepath.Join(home, ".config", "switchboard", "google_client_secret.json")
		}
		oauthCfg, err := google.LoadOAuthConfig(secretFile, "")
		if err != nil {
			return err
		}

		factory := func(ctx context.Context, acct google.Account) (google.Clients, error) {
			hc, err := google.TokenClient(ctx, pool, oauthCfg, acct, key)
			if err != nil {
				return google.Clients{}, err
			}
			return google.Clients{
				Gmail:    google.NewGmailClient(hc, "", acct.Email),
				Calendar: google.NewCalendarClient(hc, ""),
			}, nil
		}

		stats, err := google.Run(ctx, sink, factory, cfg)
		printStats("ingest", stats)
		if err != nil {
			return fmt.Errorf("ingest: %w", err)
		}
	}

	stats, err := google.Normalize(ctx, sink, cfg)
	printStats("normalize", stats)
	if err != nil {
		return fmt.Errorf("normalize: %w", err)
	}
	return nil
}

func printStats(phase string, stats google.Stats) {
	out, _ := json.Marshal(stats)
	fmt.Printf("%s: %s\n", phase, out)
}
