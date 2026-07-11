// triage is the GPT triage worker, SHADOW MODE (SPEC 06-gpt-triage): it
// extracts everything and creates nothing.
//
//	triage run    [--limit N] [--since 720h]
//	triage report [--threshold 0.7] [--since 720h]
//
//	DATABASE_URL                 ops db, required
//	OPENAI_API_KEY               required for run
//	TRIAGE_MODEL                 default gpt-5-mini
//	OPENAI_BASE_URL              optional
//	TRIAGE_CONFIDENCE_THRESHOLD  report default 0.7
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/sspataro57/switchboard/internal/provider"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/triage"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: triage <run|report> [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(os.Args[2:])
	case "report":
		err = reportCmd(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "triage:", err)
		os.Exit(1)
	}
}

func runCmd(argv []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	limit := fs.Int("limit", 0, "max messages this run (0 = all pending)")
	since := fs.Duration("since", 0, "only messages with sent_at within this window (0 = all)")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("OPENAI_API_KEY is not set")
	}
	model := os.Getenv("TRIAGE_MODEL")
	if model == "" {
		model = "gpt-5-mini"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	st := triage.NewStore(pool)
	ok, release, err := st.TryLock(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("another triage run holds the advisory lock; exiting")
	}
	defer release()

	client := provider.NewOpenAI(apiKey, os.Getenv("OPENAI_BASE_URL"))
	stats, runErr := triage.Run(ctx, st, client, triage.Config{
		Model: model, MaxTokens: 2048, Limit: *limit, Since: *since,
	})
	out, _ := json.Marshal(stats)
	fmt.Println(string(out))
	return runErr
}

func reportCmd(argv []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	threshold := fs.Float64("threshold", defaultThreshold(), "min-confidence bucket boundary (report only)")
	since := fs.Duration("since", 0, "only extractions within this window (0 = all)")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	return triage.Report(ctx, pool, os.Stdout, *threshold, *since)
}

func defaultThreshold() float64 {
	if v := os.Getenv("TRIAGE_CONFIDENCE_THRESHOLD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return 0.7
}
