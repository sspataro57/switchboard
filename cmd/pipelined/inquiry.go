package main

// The two inquiry stages (SWT-40 Part C, C-D1):
//
//   - inquiry (GPU): classify.Run on the inquiry lane, --since 72h, under the
//     shared classify lock 0x5157_0022 (classify.AdvisoryLockKey). Woken by
//     captured, gated and routed; publishes inquiry_classified after a pass that
//     wrote >= 1 verdict.
//   - inquiry_promote: promote.Run on the inquiry lane, under 0x5157_0021
//     (promote.AdvisoryLockKey), actor promote:inquiry. Woken by
//     inquiry_classified and by the sweep, which is what releases a verdict
//     pending inside its 1h grace. Publishes promoted.
//
// A lost lock is not an error here: it maps to pipeline.ErrLockHeld, and the
// loop retries once after 30 s, then waits for the sweep.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/pipeline"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/promote"
	"github.com/sspataro57/switchboard/internal/provider"
	"github.com/sspataro57/switchboard/internal/tools"
)

// inquiryStageSince is the inquiry stage's classify --since (C-D1: 72h).
// C-D6: it is >= promote.InquiryMaxAge, or an ask the promoter could still act
// on would never get a verdict.
const inquiryStageSince = 72 * time.Hour

const (
	// inquiryStageLimit bounds one inquiry pass: at the measured ~10 s/verdict
	// on the z4's 90 W cap, 25 verdicts is ~4 min, well inside PassTimeout. A
	// pass that writes 25 verdicts is repeated at once.
	inquiryStageLimit = 25
	// inquiryPromoteStageLimit bounds the verdicts one promote pass acts on
	// (gated verdicts never count).
	inquiryPromoteStageLimit = 50
	// inquiryMaxTokens is classify run's value.
	inquiryMaxTokens = 512
)

// inquiryPass builds the inquiry stage's PassFunc. processed is the number of
// verdicts written: a message the router refused writes no extraction and
// stays in the inbox, so it never counts (a re-counted skip would make the
// loop re-run at once).
func inquiryPass(pool *pgxpool.Pool) pipeline.PassFunc {
	st := classify.NewStore(pool)
	router, model := localRouter()
	return func(ctx context.Context) (int, error) {
		held, release, err := st.TryLock(ctx)
		if err != nil {
			return 0, fmt.Errorf("inquiry: %w", err)
		}
		if !held {
			return 0, fmt.Errorf("inquiry: classify lock 0x%X: %w", classify.AdvisoryLockKey, pipeline.ErrLockHeld)
		}
		defer release()
		stats, err := classify.Run(ctx, st, router, classify.Config{
			Model: model, MaxTokens: inquiryMaxTokens, Limit: inquiryStageLimit,
			Since: inquiryStageSince, Lane: classify.LaneInquiry,
		})
		slog.Info("inquiry pass", "processed", stats.Processed, "flagged", stats.Flagged,
			"skipped", stats.Skipped, "errors", stats.Errors)
		return stats.Processed, err
	}
}

// inquiryPromotePass builds the inquiry_promote stage's PassFunc. processed is
// the verdicts ACTED ON (created, attached or lost to an existing claim: each
// left the inbox); a gated verdict writes no row, stays in the inbox and never
// counts.
func inquiryPromotePass(pool *pgxpool.Pool) pipeline.PassFunc {
	// The full executor stack (validate -> policy -> audit -> handler,
	// invariant 3): every task write is create_task / task_append_log /
	// task_set_source_thread / guarded task_reopen, as promote:inquiry.
	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	ex := executor.New(reg, checker, audit.NewPGStore(pool))
	return func(ctx context.Context) (int, error) {
		st, err := promote.Run(ctx, pool, ex, promote.Config{Lane: promote.LaneInquiry, Limit: inquiryPromoteStageLimit})
		if errors.Is(err, promote.ErrLockHeld) {
			return 0, fmt.Errorf("inquiry_promote: %w", pipeline.ErrLockHeld)
		}
		// Counters unconditionally, zeros included: "found nothing" and "never
		// ran" are different lines.
		slog.Info("inquiry_promote pass", "review", st.Review, "created", st.Created,
			"attached", st.Attached, "lost_claims", st.Lost, "reopened", st.Reopened, "gated", st.Gated)
		return st.Review + st.Created + st.Attached + st.Lost, err
	}
}

// localRouter is cmd/classify's buildRouter shape for this daemon: the local
// lane only, general = nil ALWAYS. The inquiry lane's routed class is pinned
// restricted, and a nil general client means there is nothing to fall back to.
// OPS_LOCAL_MODEL is required once the URL is set, with no fallback; unset,
// every message is skipped and recorded as such (never a hosted call).
func localRouter() (*provider.Router, string) {
	base := os.Getenv("OPS_LOCAL_PROVIDER_URL")
	model := os.Getenv("OPS_LOCAL_MODEL")
	if base == "" || model == "" {
		slog.Warn("inquiry: OPS_LOCAL_PROVIDER_URL and OPS_LOCAL_MODEL are not both set; every message will be skipped",
			"why", "client conversation is only ever classified locally",
			"fix", "set OPS_LOCAL_PROVIDER_URL=http://192.168.50.55:11434 (an IP literal) and OPS_LOCAL_MODEL=qwen3:8b")
		return provider.NewRouter(nil, nil, 0), ""
	}
	local := provider.NewOllama(base, model)
	if loc := provider.LocalityOf(local.Describe()); loc != provider.LocalityLocal {
		slog.Warn("inquiry: OPS_LOCAL_PROVIDER_URL is not a local endpoint; every message will be skipped",
			"url", base, "locality", loc)
	}
	return provider.NewRouter(nil, local, 0), model
}
