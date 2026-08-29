// triage is the GPT triage worker, SHADOW MODE (SPEC 06-gpt-triage): it
// extracts everything and creates nothing.
//
//	triage run    [--limit N] [--since 720h]
//	triage report [--threshold 0.7] [--since 720h]
//
//	DATABASE_URL                 ops db, required
//	OPENAI_API_KEY               OPTIONAL since SWT-21 — a pass that only ever
//	                             touches restricted content never needs it
//	OPS_LOCAL_* (two of them)    the local lane's base URL and model; the code
//	                             below is the authority on their exact names, and
//	                             docs/runbooks/provider-locality.md documents them
//	TRIAGE_MODEL                 default gpt-5-mini
//	OPENAI_BASE_URL              optional
//	TRIAGE_CONFIDENCE_THRESHOLD  report default 0.7
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
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

	// OPENAI_API_KEY is NO LONGER REQUIRED to start (SWT-21, criterion 21).
	//
	// After the locality boundary, a pass that never touches the general lane is
	// the NORMAL case — triage's whole inbox is unmatched, unmatched is
	// restricted, so until a local adapter exists (SWT-22) every message is
	// skipped and no hosted call is made at all. Refusing to start without a key
	// would make the safe configuration the one that cannot run.
	apiKey := os.Getenv("OPENAI_API_KEY")
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

	// Two lanes, and which one a message may use is decided per message by the
	// boundary rather than here (SWT-21). The general client is built only when
	// a key exists; the local one only when its base URL is set — and that URL is
	// also what PROVES its locality, since LocalityOf reads the endpoint. Point
	// it at a hosted API and it stops being local, so restricted content stops
	// flowing rather than silently leaking.
	var general provider.Client
	if apiKey != "" {
		general = provider.NewOpenAI(apiKey, os.Getenv("OPENAI_BASE_URL"))
	}
	var local provider.Client
	var localModel string
	if base := os.Getenv("OPS_LOCAL_PROVIDER_URL"); base != "" {
		// OPS_LOCAL_MODEL is REQUIRED once a local URL is set, and there is
		// deliberately NO fallback to the hosted model name (SWT-22 criterion 9).
		//
		// The previous spelling assigned the HOSTED model name into the local
		// one, defaulting this lane to gpt-5-mini. On ollama that is a 404 PER
		// MESSAGE — an
		// unclassified error on every one of ~14,000 messages, which trips the
		// ratio raise and reads as a broken adapter. Missing the model name must
		// instead leave the lane ABSENT: one logged refusal and a skipped pass,
		// which is a state an operator can act on.
		localModel = os.Getenv("OPS_LOCAL_MODEL")
		if localModel == "" {
			slog.Error("OPS_LOCAL_PROVIDER_URL is set but OPS_LOCAL_MODEL is empty; the local lane is DISABLED",
				"url", base,
				"why", "guessing the hosted model name here would 404 on every message instead of skipping",
				"fix", "export OPS_LOCAL_MODEL=qwen3:8b")
		} else {
			// The ollama NATIVE adapter, not NewOpenAI pointed at :11434. The
			// difference is not cosmetic: /v1 has nowhere to carry `think`, and
			// with thinking on qwen3 returns empty content with done_reason
			// "length" — the 0.00-scoring configuration, invisible unless you
			// read the raw response.
			local = provider.NewOllama(base, localModel)
			// Say at startup what the boundary will decide, so a misconfigured
			// URL is found by reading one log line rather than by wondering why
			// every message skipped. A non-local URL is not an error here — the
			// pass still runs and still refuses restricted content — but it is
			// something the operator meant to get right.
			if loc := provider.LocalityOf(local.Describe()); loc != provider.LocalityLocal {
				slog.Warn("OPS_LOCAL_PROVIDER_URL is not a local endpoint; restricted content will be skipped",
					"url", base, "locality", loc)
			}
		}
	}
	router := provider.NewRouter(general, local, 0)

	stats, runErr := triage.Run(ctx, st, router, triage.Config{
		Model: model, LocalModel: localModel, MaxTokens: 2048, Limit: *limit, Since: *since,
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
