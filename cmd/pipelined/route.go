package main

// The two route stages (SWT-40 Part B, B-D6, B-D8, B11):
//
//   - route (GPU): classify.Run on the route lane, under the shared classify
//     lock 0x5157_0022 (classify.AdvisoryLockKey), with localRouter() — the
//     local lane only, general = nil, ALWAYS (B-D8). Woken by captured;
//     buildPass publishes route_classified after a pass that wrote >= 1 verdict.
//     It runs in SHADOW until an account is armed: verdicts only.
//   - route_apply: capture.RunRouteApply under capture's lock 0x5157_0015
//     (E-D4), writing mode='route' capture_decisions rows directly (capture's
//     own log, B5: no task, no tool call). Woken by route_classified and the
//     sweep; buildPass publishes routed after a pass that wrote >= 1 row. An
//     unarmed account (route_after NULL) writes nothing.
//
// A lost lock is not an error: it maps to pipeline.ErrLockHeld, and the loop
// retries once after 30 s, then waits for the sweep. This file wakes nobody
// itself; downstream wakes come from buildPass alone.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/pipeline"
)

const (
	// routeStageSince is the route stage's classify --since. A week covers every
	// fresh unmatched mail with room for a local-model outage; the one-shot
	// post-arming backfill (`classify run --lane route --since 720h`, SPEC V6.5)
	// is a hand run, not this stage.
	routeStageSince = 168 * time.Hour
	// routeStageLimit bounds one route pass: ~10 s/verdict on the z4's 90 W cap,
	// so 25 is ~4 min, well inside PassTimeout. A full pass repeats at once.
	routeStageLimit = 25
	// routeApplyStageSince is route_apply's pass window: the backfill's 720h,
	// so every verdict the backfill writes is inside the window it is applied in.
	routeApplyStageSince = 720 * time.Hour
	// routeApplyStageLimit bounds one route_apply pass (cheap: no GPU).
	routeApplyStageLimit = capture.RouteDefaultLimit
	// routeMaxTokens is classify run's value.
	routeMaxTokens = 512
)

// routePass builds the route stage's PassFunc. processed is the number of
// verdicts written: a message the router refused writes no extraction and stays
// in the inbox, so it never counts.
func routePass(pool *pgxpool.Pool) pipeline.PassFunc {
	st := classify.NewStore(pool)
	router, model := localRouter()
	return func(ctx context.Context) (int, error) {
		held, release, err := st.TryLock(ctx)
		if err != nil {
			return 0, fmt.Errorf("route: %w", err)
		}
		if !held {
			return 0, fmt.Errorf("route: classify lock 0x%X: %w", classify.AdvisoryLockKey, pipeline.ErrLockHeld)
		}
		defer release()
		stats, err := classify.Run(ctx, st, router, classify.Config{
			Model: model, MaxTokens: routeMaxTokens, Limit: routeStageLimit,
			Since: routeStageSince, Lane: classify.LaneRoute,
		})
		slog.Info("route pass", "processed", stats.Processed, "grounded", stats.Flagged,
			"skipped", stats.Skipped, "errors", stats.Errors)
		return stats.Processed, err
	}
}

// routeApplyPass builds the route_apply stage's PassFunc. processed is the
// route rows written; an unrouted message (pending_verdict, no_default,
// verdict_before_arming, candidate_revoked) stays in the inbox and never counts.
func routeApplyPass(pool *pgxpool.Pool) pipeline.PassFunc {
	return func(ctx context.Context) (int, error) {
		st, err := capture.RunRouteApply(ctx, pool, capture.RouteApplyConfig{
			Limit: routeApplyStageLimit, Since: routeApplyStageSince,
		})
		if errors.Is(err, capture.ErrRouteLockHeld) {
			return 0, fmt.Errorf("route_apply: %w", pipeline.ErrLockHeld)
		}
		// Counters unconditionally, zeros included: "found nothing" and "never
		// ran" are different lines.
		slog.Info("route_apply pass", "written", st.Written,
			"thread", st.ByStep[capture.RouteStepThread], "single", st.ByStep[capture.RouteStepSingle],
			"model", st.ByStep[capture.RouteStepModel], "default", st.ByStep[capture.RouteStepDefault],
			"pending_verdict", st.Unrouted[capture.RouteReasonPendingVerdict],
			"no_default", st.Unrouted[capture.RouteReasonNoDefault],
			"verdict_before_arming", st.Unrouted[capture.RouteReasonBeforeArming],
			"candidate_revoked", st.Unrouted[capture.RouteReasonCandidateRevoked])
		return st.Written, err
	}
}
