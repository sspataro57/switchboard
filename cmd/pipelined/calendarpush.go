package main

// calendar_push (SWT-109): the resident stage that writes swb-first calendar
// reservations to Google and deletes released ones. Sweep-only (no upstream
// event). Each pass calls push_calendar_reservations and then
// apply_hold_releases THROUGH THE EXECUTOR as pipeline:calendar_push — each
// only when its inbox has something DUE, so an idle pass spends no Pipedream
// credit and writes no audit row. Rows waiting on a gate (a calendar not yet
// write-enabled, the budget spent) are re-checked every 30 minutes. Both verbs are sendShaped + freezeGated:
// the kill switch stops this stage's writes, never booking.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/pipeline"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	calendarPushStageLimit = 25
	calendarPushActor      = "pipeline:calendar_push"
)

func calendarPushPass(pool *pgxpool.Pool) pipeline.PassFunc {
	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	ex := executor.New(reg, checker, audit.NewPGStore(pool))
	args, _ := json.Marshal(map[string]int{"limit": calendarPushStageLimit})
	// The pass reports 0 processed, always: the stage loop re-runs a pass
	// that hit its limit at once, and rows waiting on a gate (a calendar not
	// yet write-enabled, the month's budget spent) stay in the inbox — a
	// re-run would only re-write their audit rows. The sweep is the cadence.
	return func(ctx context.Context) (int, error) {
		moved := 0
		pushDue, err := tools.PushInboxCount(ctx, pool)
		if err != nil {
			return 0, fmt.Errorf("calendar_push: count push inbox: %w", err)
		}
		if pushDue > 0 {
			res, err := ex.Execute(ctx, executor.Call{Tool: "push_calendar_reservations", Actor: calendarPushActor, Args: args})
			if err != nil {
				// A policy deny (kill switch, rate limit) leaves every row
				// untouched and saved; the next sweep tries again.
				slog.Warn("calendar_push: push not run", "err", err)
			} else {
				slog.Info("calendar_push: push pass", "result", string(res.Output))
				moved++
			}
		}
		releaseDue, err := tools.ReleaseInboxCount(ctx, pool)
		if err != nil {
			return 0, fmt.Errorf("calendar_push: count release inbox: %w", err)
		}
		if releaseDue > 0 {
			res, err := ex.Execute(ctx, executor.Call{Tool: "apply_hold_releases", Actor: calendarPushActor, Args: args})
			if err != nil {
				slog.Warn("calendar_push: releases not run", "err", err)
			} else {
				slog.Info("calendar_push: release pass", "result", string(res.Output))
				moved++
			}
		}
		slog.Debug("calendar_push pass", "calls", moved, "push_due", pushDue, "release_due", releaseDue)
		return 0, nil
	}
}

// armCalendarBooker wires the Pipedream write adapter for the calendar_push
// stage. Unlike ops-mcp, an absent PIPEDREAM_CALENDAR_URL is FATAL here: the
// operator asked for the stage, and a stage that can never push would only
// fill audit_events with refusals.
func armCalendarBooker() error {
	pdURL := os.Getenv("PIPEDREAM_CALENDAR_URL")
	if pdURL == "" {
		return fmt.Errorf("stage calendar_push needs PIPEDREAM_CALENDAR_URL (and the workflow token)")
	}
	token, err := google.PipedreamTokenFromEnv()
	if err != nil {
		return fmt.Errorf("configure calendar booker: %w", err)
	}
	booker, err := google.NewPipedreamCalendarClient(pdURL, token, nil)
	if err != nil {
		return fmt.Errorf("configure calendar booker: %w", err)
	}
	tools.SetCalendarBooker(booker)
	return nil
}
