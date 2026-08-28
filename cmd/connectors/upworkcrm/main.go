// upworkcrm is the one-shot Upwork CRM connector (SPEC 02-upwork-crm-connector):
// ingest phase (source -> raw_source_items, raw-first) then normalize phase
// (raw -> canonical objects). Scheduling is external (manual / cron).
//
//	DATABASE_URL            sink (ops db), required
//	UPWORK_CRM_DATABASE_URL source, required unless --normalize-only
//	CAPTURE_RULES_MODE      shadow (default) | live
//	CAPTURE_RULES_SINCE     Go duration bounding the capture-rules pass
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

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/connector/upworkcrm"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
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

	// Printed unconditionally, including the zero, so "flagged nothing" and "did
	// not run" look different in the CronJob log (the slackweb main's shape).
	// Without that distinction a silent detector is indistinguishable from a
	// detector that was never wired up — which is the failure this whole
	// reconciler exists to avoid.
	flagged, err := upworkcrm.ReconcileUnconfirmed(ctx, sink, upworkcrm.UnconfirmedFlagPasses())
	if err != nil {
		return fmt.Errorf("reconcile unconfirmed: %w", err)
	}
	fmt.Printf("reconcile: {\"flagged\":%d}\n", flagged)

	// Deterministic project assignment (SWT-17). This is the FIRST capture call
	// in this main; there is no capture.Channel for upwork, and this pass needs
	// none — it is channel-agnostic and reads normalized_messages regardless of
	// channel, so it covers what every connector ingested.
	//
	// Order: LAST, after ReconcileUnconfirmed, matching cmd/connectors/slackweb,
	// the only sibling that runs both a reconciler and a capture pass. Two
	// reasons, neither cosmetic. (1) The reconciler is an alarm on OUR OWN
	// unconfirmed sends and this pass is inbound-only, so they never contend for
	// a row — but the reconciler must not be delayed or skipped by a failure in
	// the newer, larger pass, and putting it first means an error here cannot
	// suppress the flagged count above. (2) The pass reaches the executor and
	// can create tasks; letting delivery confirmation settle first keeps the
	// "our own sends are resolved before we act on the corpus" ordering every
	// other main already has.
	//
	// This main built no executor before; tasks/external_refs/task_events are
	// reachable only through it (invariant 3).
	reg := executor.NewRegistry()
	tools.Register(reg, sinkPool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(sinkPool), policy.NewStatic(reg.Names()...))
	ex := executor.New(reg, checker, audit.NewPGStore(sinkPool))

	// Mode and horizon come from capture's own readers, not from a local
	// os.Getenv: the "720 is not a Go duration" defence has ONE spelling, and a
	// second copy in each of five mains is exactly how four of them end up
	// disagreeing. Actor names this connector so the audit trail says which
	// CronJob won the lock.
	mode := capture.RulesMode()
	rulesCfg := capture.RulesConfig{
		Mode:    mode,
		Horizon: capture.RulesHorizon(mode),
		Actor:   "capture:upworkcrm",
	}
	rules, err := capture.EvaluateRules(ctx, sinkPool, ex, rulesCfg)
	// Printed unconditionally, zeros included, and before the error check, for
	// the same reason the reconcile line above is: "matched nothing" and "never
	// ran" must be different lines in a CronJob log.
	fmt.Printf("capture_rules: {\"mode\":%q,\"considered\":%d,\"matched\":%d,\"unmatched\":%d,"+
		"\"tasks_created\":%d,\"appended\":%d}\n",
		rulesCfg.Mode, rules.Considered, rules.Matched, rules.Unmatched, rules.TasksCreated, rules.Appended)
	if err != nil {
		return fmt.Errorf("capture rules: %w", err)
	}
	return nil
}

func printStats(phase string, stats upworkcrm.Stats) {
	out, _ := json.Marshal(stats)
	fmt.Printf("%s: %s\n", phase, out)
}
