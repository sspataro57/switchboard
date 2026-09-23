package main

// `opsctl capture-rules direct-backfill --message <id[,id...]> [--dry-run]`
// (SWT-78): puts named Slack DMs that the inquiry lane dropped onto their
// conversation tasks, through capture.DirectBackfill — the live pass's own
// decision and executor calls, with no capture_decisions write. Idempotent:
// a message already backfilled is reported and left alone.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

type directBackfillOpts struct {
	ids    []int64
	dryRun bool
}

func parseCaptureRulesDirectBackfill(argv []string) (directBackfillOpts, error) {
	fs := flag.NewFlagSet("capture-rules direct-backfill", flag.ContinueOnError)
	messages := fs.String("message", "", "normalized message ids to backfill, comma-separated (required)")
	dryRun := fs.Bool("dry-run", false, "print what would be created or attached; write nothing")
	if err := fs.Parse(argv); err != nil {
		return directBackfillOpts{}, err
	}
	var ids []int64
	for _, p := range strings.Split(*messages, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		id, err := strconv.ParseInt(p, 10, 64)
		if err != nil || id <= 0 {
			return directBackfillOpts{}, fmt.Errorf("--message: %q is not a message id", p)
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return directBackfillOpts{}, errors.New("--message is required: name the messages to backfill")
	}
	return directBackfillOpts{ids: ids, dryRun: *dryRun}, nil
}

func runCaptureRulesDirectBackfill(argv []string) error {
	opts, err := parseCaptureRulesDirectBackfill(argv)
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

	var ex *executor.Executor
	if !opts.dryRun {
		reg := executor.NewRegistry()
		tools.Register(reg, pool)
		checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
		ex = executor.New(reg, checker, audit.NewPGStore(pool))
	}
	st, err := capture.DirectBackfill(ctx, pool, ex, opts.ids, opts.dryRun, os.Stdout)
	label := "live"
	if opts.dryRun {
		label = "dry-run"
	}
	fmt.Printf("direct-backfill (%s): named=%d created=%d attached=%d already=%d skipped=%d\n",
		label, st.Named, st.Created, st.Attached, st.AlreadyDone, st.Skipped)
	if errors.Is(err, capture.ErrDirectBackfillLockHeld) {
		return fmt.Errorf("%w; a capture pass is running, retry shortly", err)
	}
	if err != nil {
		return fmt.Errorf("direct backfill: %w", err)
	}
	return nil
}
