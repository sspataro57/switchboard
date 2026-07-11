// upworkcrm is the one-shot Upwork CRM connector (SPEC 02-upwork-crm-connector):
// ingest phase (source -> raw_source_items, raw-first) then normalize phase
// (raw -> canonical objects). Scheduling is external (manual / cron).
//
//	DATABASE_URL            sink (ops db), required
//	UPWORK_CRM_DATABASE_URL source, required unless --normalize-only
//
//	upworkcrm [--full] [--normalize-only] [--all] [--overlap 24h]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/upworkcrm"
	"github.com/sspataro57/switchboard/internal/store"
)

func main() {
	full := flag.Bool("full", false, "rescan communications from the beginning (ignore cursor)")
	normalizeOnly := flag.Bool("normalize-only", false, "skip ingest; normalize from raw_source_items alone")
	all := flag.Bool("all", false, "normalize every raw row, not only pending ones")
	overlap := flag.Duration("overlap", upworkcrm.DefaultOverlap, "cursor re-read window")
	flag.Parse()

	if err := run(*full, *normalizeOnly, *all, *overlap); err != nil {
		fmt.Fprintln(os.Stderr, "upworkcrm:", err)
		os.Exit(1)
	}
}

func run(full, normalizeOnly, all bool, overlap time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	sinkPool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect sink: %w", err)
	}
	defer sinkPool.Close()
	sink := upworkcrm.NewSink(sinkPool)

	cfg := upworkcrm.Config{Full: full, All: all, Overlap: overlap}

	if !normalizeOnly {
		dsn := os.Getenv("UPWORK_CRM_DATABASE_URL")
		if dsn == "" {
			return fmt.Errorf("UPWORK_CRM_DATABASE_URL is not set (required unless --normalize-only)")
		}
		srcPool, err := store.NewPoolDSN(ctx, dsn)
		if err != nil {
			return fmt.Errorf("connect source: %w", err)
		}
		defer srcPool.Close()

		stats, err := upworkcrm.Ingest(ctx, upworkcrm.NewSource(srcPool), sink, cfg)
		if err != nil {
			return fmt.Errorf("ingest: %w", err)
		}
		printStats("ingest", stats)
	}

	stats, err := upworkcrm.Normalize(ctx, sink, cfg)
	if err != nil {
		return fmt.Errorf("normalize: %w", err)
	}
	printStats("normalize", stats)
	return nil
}

func printStats(phase string, stats upworkcrm.Stats) {
	out, _ := json.Marshal(stats)
	fmt.Printf("%s: %s\n", phase, out)
}
