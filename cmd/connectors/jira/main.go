// jira is the one-shot Jira poller (SPEC 09-jira-github-connectors):
// raw-first issue/comment polling → normalize (threads/messages channel jira,
// shadow triage sees them automatically) → own-comment loop closure.
//
//	jira [--full] [--normalize-only] [--all]
//
//	DATABASE_URL   ops db, required
//	OPS_TOKEN_KEY  required unless --normalize-only
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/jira"
	"github.com/sspataro57/switchboard/internal/store"
)

func main() {
	full := flag.Bool("full", false, "rescan (ignore the updated cursor)")
	normalizeOnly := flag.Bool("normalize-only", false, "skip polling; normalize from raw alone")
	all := flag.Bool("all", false, "normalize every raw row, not only pending")
	flag.Parse()

	if err := run(*full, *normalizeOnly, *all); err != nil {
		fmt.Fprintln(os.Stderr, "jira:", err)
		os.Exit(1)
	}
}

func run(full, normalizeOnly, all bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	sink := jira.NewSink(pool)
	cfg := jira.Config{Full: full, All: all}

	if !normalizeOnly {
		key := os.Getenv("OPS_TOKEN_KEY")
		if key == "" {
			return fmt.Errorf("OPS_TOKEN_KEY is not set")
		}
		factory := func(ctx context.Context, acct jira.Account) (*jira.Client, error) {
			var token string
			if err := pool.QueryRow(ctx,
				`SELECT pgp_sym_decrypt(refresh_token_encrypted, $2) FROM source_accounts WHERE id=$1`,
				acct.ID, key).Scan(&token); err != nil {
				return nil, fmt.Errorf("decrypt token for %s: %w", acct.Email, err)
			}
			return jira.NewClient(http.DefaultClient, acct.SiteBaseURL, acct.Email, token), nil
		}
		stats, err := jira.Run(ctx, sink, factory, cfg)
		printStats("ingest", stats)
		if err != nil {
			return fmt.Errorf("ingest: %w", err)
		}
	}

	stats, err := jira.Normalize(ctx, sink, cfg)
	printStats("normalize", stats)
	if err != nil {
		return fmt.Errorf("normalize: %w", err)
	}
	return nil
}

func printStats(phase string, stats jira.Stats) {
	out, _ := json.Marshal(stats)
	fmt.Printf("%s: %s\n", phase, out)
}
