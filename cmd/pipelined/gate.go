package main

// The gate stage (SWT-40 Part D, D-D4): capture.RunGate resolving live `held`
// decisions, woken by `captured` and the sweep. This is the only place the gate
// touches a credential: the token-decrypting lookup factory, shared with
// cmd/connectors/jira (jira.TokenClientFactory).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/connector/jira"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/pipeline"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

// gateStageLimit is the gate pass's --limit: the held rows one pass reads. A
// pass that resolves that many is repeated at once.
const gateStageLimit = capture.GateDefaultLimit

// gatePass builds the gate's PassFunc over pool. With OPS_TOKEN_KEY unset the
// lookup factory is nil: nothing is fetched, fresh holds stay pending_lookup,
// and expired ones still resolve (attributed, gate_unverified_expired).
func gatePass(pool *pgxpool.Pool) pipeline.PassFunc {
	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	ex := executor.New(reg, checker, audit.NewPGStore(pool))
	lookup := jira.TokenClientFactory(pool, os.Getenv("OPS_TOKEN_KEY"))
	if lookup == nil {
		slog.Warn("gate: OPS_TOKEN_KEY is not set; no Jira lookup",
			"consequence", "holds stay pending_lookup until they expire (gate_unverified_expired)")
	}
	return func(ctx context.Context) (int, error) {
		st, err := capture.RunGate(ctx, pool, ex, capture.GateConfig{Limit: gateStageLimit, Lookup: lookup})
		if errors.Is(err, capture.ErrGateLockHeld) {
			return 0, fmt.Errorf("gate: %w", pipeline.ErrLockHeld)
		}
		// Counters unconditionally, zeros included: "found nothing" and "never
		// ran" are different lines.
		slog.Info("gate pass", "tasks_created", st.TasksCreated, "appended", st.Appended,
			"reopened", st.Reopened, "attributed", st.Attributed, "pending_lookup", st.PendingLookup,
			"resolved", st.Resolved)
		// Resolutions only: a hold that stays pending never left the inbox.
		return st.Resolved, err
	}
}
