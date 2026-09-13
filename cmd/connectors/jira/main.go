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
	"os"
	"time"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/connector/jira"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/pipeline"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/ticketstatus"
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

	// The token-decrypting factory, built once and shared by the poller and the
	// ticket-status pass's candidate-driven lookup. NIL when OPS_TOKEN_KEY is
	// absent — the reconciler then skips its lookup half LOUDLY (SWT-32 D21)
	// while the status half still runs from stored raw. One spelling, shared
	// with pipelined's capture-time gate (jira.TokenClientFactory).
	factory := jira.TokenClientFactory(pool, os.Getenv("OPS_TOKEN_KEY"))

	if !normalizeOnly {
		if factory == nil {
			return fmt.Errorf("OPS_TOKEN_KEY is not set")
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
		"\"tasks_created\":%d,\"appended\":%d,\"reopened\":%d,\"revived\":%d,\"surfaced_created\":%d,\"deferred\":%d,\"blind\":%d}\n",
		rulesCfg.Mode, rules.Considered, rules.Matched, rules.Unmatched, rules.TasksCreated, rules.Appended, rules.Reopened, rules.Revived, rules.SurfacedCreated, rules.Deferred, rules.Blind)
	if err != nil {
		return fmt.Errorf("capture rules: %w", err)
	}
	// SWT-40 E2: wake the pipeline stages now the decisions are committed.
	// Never fails the run: an unset or dead broker costs latency (the stages
	// sweep), never work.
	pipeline.AnnounceCaptured(ctx, os.Getenv("MQTT_BROKER"), "jira", rules)

	// Ticket-status reconciliation (SWT-32), AFTER capture on purpose (D8): a
	// notification about a ticket that is already Done, or already someone
	// else's, creates the task in capture's half of this tick and the pass
	// closes it in the SAME tick — reversed, the task would sit on the board
	// until the next run. Counters print before the error for the capture
	// block's recorded reason: zeros included, "found nothing" and "never ran"
	// are different lines.
	ts, err := ticketstatus.Run(ctx, pool, ex, ticketstatus.Config{Lookup: factory})
	fmt.Printf("ticket_status: {\"considered\":%d,\"closed_ticket_done\":%d,\"closed_ticket_delivered\":%d,"+
		"\"closed_not_assigned\":%d,"+
		"\"reopened\":%d,\"refused_active\":%d,\"suppressed_dismissed\":%d,\"resurfaced\":%d,\"converged\":%d,"+
		"\"unpolled\":%d,\"ambiguous\":%d,\"unreadable\":%d,"+
		"\"fetched\":%d,\"fetch_skipped_ttl\":%d,\"fetch_failed\":%d}\n",
		ts.Considered, ts.ClosedTicketDone, ts.ClosedTicketDelivered, ts.ClosedNotAssigned,
		ts.Reopened, ts.RefusedActive, ts.SuppressedDismissed, ts.Resurfaced, ts.Converged,
		ts.Unpolled, ts.Ambiguous, ts.Unreadable,
		ts.Fetched, ts.FetchSkippedTTL, ts.FetchFailed)
	if err != nil {
		return fmt.Errorf("ticket status: %w", err)
	}
	return nil
}

func printStats(phase string, stats jira.Stats) {
	out, _ := json.Marshal(stats)
	fmt.Printf("%s: %s\n", phase, out)
}
