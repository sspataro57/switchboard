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

	client := provider.NewOpenAI(apiKey, os.Getenv("OPENAI_BASE_URL"))
	stats, runErr := drafts.Run(ctx, drafts.NewStore(pool), client, ex,
		drafts.Config{Model: model, MaxTokens: 2048, Limit: limit})
	out, _ := json.Marshal(stats)
	fmt.Println(string(out))
	return runErr
}
