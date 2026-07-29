// slackweb is the one-shot Slack Web connector:
// authenticated browser export -> raw observations -> normalized messages.
//
//	slackweb [--normalize-only] [--all]
//
// DATABASE_URL            ops db, required
// SLACK_WEB_BRIDGE_SCRIPT absolute path to the compiled TypeScript bridge
// SLACK_WEB_NODE          Node.js executable (default: node)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
	"github.com/sspataro57/switchboard/internal/store"
)

func main() {
	normalizeOnly := flag.Bool("normalize-only", false, "skip browser export; normalize from raw alone")
	all := flag.Bool("all", false, "normalize every Slack raw row, not only pending")
	flag.Parse()

	if err := run(*normalizeOnly, *all); err != nil {
		fmt.Fprintln(os.Stderr, "slackweb:", err)
		os.Exit(1)
	}
}

func run(normalizeOnly, all bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	sink := slackweb.NewSink(pool)

	if !normalizeOnly {
		bridge, err := newSource()
		if err != nil {
			return err
		}
		stats, err := slackweb.Ingest(ctx, bridge, sink)
		printStats("ingest", stats)
		if err != nil {
			return fmt.Errorf("ingest: %w", err)
		}
	}

	stats, err := slackweb.Normalize(ctx, sink, slackweb.Config{All: all})
	printStats("normalize", stats)
	if err != nil {
		return fmt.Errorf("normalize: %w", err)
	}
	return nil
}

// newSource picks the transport to the Slack connector.
//
// The connector needs an authenticated browser, which is host-bound to the Mac
// mini, so a cluster-resident poller cannot exec it locally: SLACK_WEB_BRIDGE_URL
// selects the HTTP bridge on that host. The local command bridge remains for
// running the poller on the same machine as the browser.
func newSource() (slackweb.Source, error) {
	if rawURL := os.Getenv("SLACK_WEB_BRIDGE_URL"); rawURL != "" {
		token, err := slackweb.TokenFromEnv()
		if err != nil {
			return nil, err
		}
		return slackweb.NewHTTPBridge(rawURL, token, nil)
	}
	return slackweb.NewCommandBridge(os.Getenv("SLACK_WEB_NODE"), os.Getenv("SLACK_WEB_BRIDGE_SCRIPT"))
}

func printStats(phase string, stats slackweb.Stats) {
	out, _ := json.Marshal(stats)
	fmt.Printf("%s: %s\n", phase, out)
}
