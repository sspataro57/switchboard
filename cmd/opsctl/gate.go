package main

// `opsctl capture-rules gate [--dry-run] [--shadow] [--limit N]` (SWT-40 Part D
// review fix 2): one capture-time gate pass by hand, for smoke tests and V5.
//
//	--dry-run   decide every hold from the STORED snapshots only (no fetch, no
//	            lock, no writes) and print one line per hold: message, key and
//	            the would-be outcome (task / task_log / attributed:<reason> /
//	            pending_lookup / budget_skipped)
//	--shadow    with --dry-run: read the latest shadow held rows, not the live ones
//
// Without --dry-run it runs capture.RunGate once, exactly as pipelined's gate
// stage does, with the same token-decrypting lookup factory (OPS_TOKEN_KEY).
// Both paths share the gate's one decide step; nothing here decides.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/connector/jira"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

type gateOpts struct {
	dryRun, shadow bool
	limit          int
}

func parseCaptureRulesGate(argv []string) (gateOpts, error) {
	fs := flag.NewFlagSet("capture-rules gate", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "decide from the stored snapshots only: no fetch, no writes; print one line per hold")
	shadow := fs.Bool("shadow", false, "with --dry-run: read the latest shadow held rows instead of the live ones")
	limit := fs.Int("limit", 0, "max holds read (0 = the gate's default)")
	if err := fs.Parse(argv); err != nil {
		return gateOpts{}, err
	}
	if *shadow && !*dryRun {
		// The gate resolves live holds only; a shadow hold is read, never acted on.
		return gateOpts{}, fmt.Errorf("--shadow needs --dry-run: the gate never resolves shadow holds")
	}
	return gateOpts{dryRun: *dryRun, shadow: *shadow, limit: *limit}, nil
}

func runCaptureRulesGate(argv []string) error {
	opts, err := parseCaptureRulesGate(argv)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), captureRulesRunTimeout)
	defer cancel()
	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	if opts.dryRun {
		st, err := capture.DryRunGate(ctx, pool, capture.GateDryRunConfig{
			Limit: opts.limit, Shadow: opts.shadow, Out: os.Stdout,
		})
		printGateStats("dry-run", st)
		if err != nil {
			return fmt.Errorf("capture gate dry run: %w", err)
		}
		return nil
	}

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	ex := executor.New(reg, checker, audit.NewPGStore(pool))
	lookup := jira.TokenClientFactory(pool, os.Getenv("OPS_TOKEN_KEY"))
	if lookup == nil {
		fmt.Fprintln(os.Stderr, "opsctl: OPS_TOKEN_KEY is not set; no Jira lookup — holds without a fresh "+
			"stored snapshot stay pending until they expire (gate_unverified_expired)")
	}
	st, err := capture.RunGate(ctx, pool, ex, capture.GateConfig{Limit: opts.limit, Lookup: lookup})
	printGateStats("live", st)
	if errors.Is(err, capture.ErrGateLockHeld) {
		return fmt.Errorf("%w; a capture or gate pass is running, retry shortly", err)
	}
	if err != nil {
		return fmt.Errorf("capture gate: %w", err)
	}
	return nil
}

// printGateStats prints the counters unconditionally, zeros included: a pass
// that found nothing and a pass that never ran must not look the same.
//
// revived and surfaced_created (SWT-45 criterion 28) are ALWAYS 0 here, by
// design rather than by omission: the gate stage never revives a closed task and
// never surfaces one it creates (the SPEC's Part D section — a warranted-only
// task needs no hold, and non-addressed activity follows the gate). They are
// printed so every capture counter line reads the same.
//
// deferred and blind (SWT-45 J17) are ALWAYS 0 here for the same reason: they
// count the own-action guard, which runs only on the revive/surface path, and
// the gate stage takes neither.
func printGateStats(mode string, st capture.GateStats) {
	const revived, surfacedCreated, deferred, blind = 0, 0, 0, 0
	fmt.Printf("capture_gate: {\"mode\":%q,\"tasks_created\":%d,\"appended\":%d,\"reopened\":%d,"+
		"\"revived\":%d,\"surfaced_created\":%d,\"deferred\":%d,\"blind\":%d,"+
		"\"attributed\":%d,\"pending_lookup\":%d,\"budget_skipped\":%d,\"resolved\":%d}\n",
		mode, st.TasksCreated, st.Appended, st.Reopened, revived, surfacedCreated, deferred, blind,
		st.Attributed, st.PendingLookup, st.BudgetSkipped, st.Resolved)
}
