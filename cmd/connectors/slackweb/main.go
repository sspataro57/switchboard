// slackweb is the one-shot Slack Web connector:
// authenticated browser export -> raw observations -> normalized messages.
//
//	slackweb [--normalize-only] [--all]
//
// DATABASE_URL            ops db, required
// SLACK_WEB_BRIDGE_SCRIPT absolute path to the compiled TypeScript bridge
// SLACK_WEB_NODE          Node.js executable (default: node)
// CAPTURE_RULES_MODE      shadow (default) | live
// CAPTURE_RULES_SINCE     Go duration bounding the capture-rules pass
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/connector/slackweb"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
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

	// After normalize, so this pass's own confirmations are already stamped and
	// a delivery confirmed moments ago is never flagged.
	flagged, err := slackweb.ReconcileUnconfirmed(ctx, sink, slackweb.UnconfirmedFlagPasses())
	if err != nil {
		return fmt.Errorf("reconcile unconfirmed: %w", err)
	}
	if flagged > 0 {
		fmt.Printf("reconcile: {\"flagged_unconfirmed\":%d}\n", flagged)
	}

	// Slack messages Salvador sent by hand (phone, desktop app) get logged on the
	// tasks they correspond to (SWT-16). Runs after the reconciler so an in-flight
	// switchboard send has already had its chance to be confirmed or flagged.
	observed, err := capture.ObserveOutbound(ctx, pool, capture.Slack)
	if err != nil {
		return fmt.Errorf("observe outbound: %w", err)
	}
	// Printed unconditionally: a silent pass and a pass that did not run look
	// identical in the logs, and this one is expected to find nothing most times.
	fmt.Printf("capture: {\"outbound_observed\":%d}\n", observed)

	// Deterministic project assignment (SWT-17). Channel-agnostic, so this pass
	// covers messages every connector ingested, not only Slack's — four CronJobs
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
		Actor:   "capture:slackweb",
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
