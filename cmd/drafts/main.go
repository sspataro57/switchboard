// drafts is the GPT draft worker (SPEC 08-draft-deliveries): one-shot queue
// consumer over R3 Deliver tasks, producing drafted deliveries via the
// executor. Scheduling external (cron).
//
//	drafts run [--limit N]
//
//	DATABASE_URL    ops db, required
//	OPENAI_API_KEY  required
//	DRAFTS_MODEL    default gpt-5-mini
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/drafts"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/provider"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

func main() {
	limit := flag.Int("limit", 0, "max deliver tasks this run (0 = all)")
	flag.Parse()

	if err := run(*limit); err != nil {
		fmt.Fprintln(os.Stderr, "drafts:", err)
		os.Exit(1)
	}
}

func run(limit int) error {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("OPENAI_API_KEY is not set")
	}
	model := os.Getenv("DRAFTS_MODEL")
	if model == "" {
		model = "gpt-5-mini"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	ex := executor.New(reg, checker, audit.NewPGStore(pool))

	// The general lane only, and the nil local lane is the POINT (SWT-21).
	//
	// Restricted content therefore always skips here rather than being processed
	// by anything. That is exactly criterion 24: this worker's job is
	// client-facing drafts on projects marked `any`, and a local_only project's
	// Deliver task waits instead.
	//
	// Wiring a local lane in was tried and reverted. It is outside criterion 21
	// (which names cmd/triage), and this worker does not implement criterion 18's
	// tier 1 — an ErrUnavailable from Complete would become a hard error and a
	// non-zero exit, so a busy local box would look like an outage. SWT-22 adds
	// the lane together with the skip semantics that make it safe.
	//
	// OPENAI_API_KEY is still REQUIRED here (checked above). It became optional
	// in cmd/triage only, because triage's whole inbox is restricted and a pass
	// that never touches the hosted lane is its normal case; drafts' inbox is
	// client work on `any` projects, so no key means no work.
	router := provider.NewRouter(provider.NewOpenAI(apiKey, os.Getenv("OPENAI_BASE_URL")), nil, 0)
	stats, runErr := drafts.Run(ctx, drafts.NewStore(pool), router, ex,
		drafts.Config{Model: model, MaxTokens: 2048, Limit: limit})
	out, _ := json.Marshal(stats)
	fmt.Println(string(out))
	return runErr
}
