// jira is the one-shot Jira poller (SPEC 09-jira-github-connectors):
// raw-first issue/comment polling → normalize (threads/messages channel jira,
// shadow triage sees them automatically) → own-comment loop closure.
//
//	jira [--full] [--normalize-only] [--all]
//
//	DATABASE_URL        ops db, required
//	OPS_TOKEN_KEY       required unless --normalize-only
//	CAPTURE_RULES_MODE  shadow (default) | live
//	CAPTURE_RULES_SINCE Go duration bounding the capture-rules pass
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/connector/jira"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
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
	// Externally-sent messages: log them on the tasks they correspond to, so a
	// reply sent by hand does not leave the task looking untouched (SWT-16).
	// After Normalize, so this pass's own delivery confirmations are already
	// stamped and a message switchboard sent is never mislabeled as external.
	observed, err := capture.ObserveOutbound(ctx, pool, capture.Jira)
	if err != nil {
		return fmt.Errorf("observe outbound: %w", err)
	}
	// Printed unconditionally: a silent pass and a pass that did not run look
	// identical in the logs, and this one is expected to find nothing most times.
	fmt.Printf("capture: {\"outbound_observed\":%d}\n", observed)

	// Deterministic project assignment (SWT-17). Channel-agnostic, so this pass
	// covers messages every connector ingested, not only Jira's — four CronJobs
	// racing is expected and is what the advisory lock inside EvaluateRules is
	// for. This main built no executor before; tasks/external_refs/task_events
	// are reachable only through it (invariant 3).
	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	ex := executor.New(reg, checker, audit.NewPGStore(pool))

	// Mode and horizon come from capture's own readers, not from a local
	// os.Getenv: the "720 is not a Go duration" defence has ONE spelling, and a
	// second copy in each of five mains is exactly how four of them end up
	// disagreeing. Actor names this connector so the audit trail says which
	// CronJob won the lock.
	mode := capture.RulesMode()
	rulesCfg := capture.RulesConfig{
		Mode:    mode,
		Horizon: capture.RulesHorizon(mode),
		Actor:   "capture:jira",
	}
	rules, err := capture.EvaluateRules(ctx, pool, ex, rulesCfg)
	// Same rule as the line above, and the reason this printf is not guarded by
	// err: the counts go out unconditionally, zeros included, so "matched
	// nothing" and "never ran" are different lines in a CronJob log.
	fmt.Printf("capture_rules: {\"mode\":%q,\"considered\":%d,\"matched\":%d,\"unmatched\":%d,"+
		"\"tasks_created\":%d,\"appended\":%d}\n",
		rulesCfg.Mode, rules.Considered, rules.Matched, rules.Unmatched, rules.TasksCreated, rules.Appended)
	if err != nil {
		return fmt.Errorf("capture rules: %w", err)
	}
	return nil
}

func printStats(phase string, stats jira.Stats) {
	out, _ := json.Marshal(stats)
	fmt.Printf("%s: %s\n", phase, out)
}
