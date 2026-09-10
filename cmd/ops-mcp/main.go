// ops-mcp is the stdio MCP server exposing the agent-facing task tools (SPEC
// 04-mcp-task-tools). It is a thin adapter: every tools/call maps to
// executor.Execute — validate → policy → audit → handler (invariant 3).
//
//	DATABASE_URL   ops db, required
//	OPS_WORKER_ID  the caller's identity, required (wrapper sets it when
//	               spawning claude; interactive sessions use manual:<user>)
//
// This binary serves the FULL allowlist and wires the senders: worker consoles
// and this repo's .mcp.json. The install every other repo sees is the read-only
// cmd/ops-mcp-read (SWT-35).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/mcpserver"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

func main() {
	// stdout carries the protocol; logs go to stderr.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if err := run(); err != nil {
		slog.Error("ops-mcp failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	workerID := os.Getenv("OPS_WORKER_ID")
	if workerID == "" {
		return fmt.Errorf("OPS_WORKER_ID is not set (identity is never model-chosen)")
	}

	ctx := context.Background()
	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer pool.Close()

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	ex := executor.New(reg, checker, audit.NewPGStore(pool))

	// SWT-11 criterion 17 MCP-lists send_delivery, so this binary must be able to
	// actually send — otherwise the newly reachable tool fails with "no gmail send
	// adapter wired" the first time Salvador uses it. The human-actor gate is
	// unchanged and is what keeps a worker identity out.
	mailSender, _, err := google.WireMailSender(pool)
	if err != nil {
		return err
	}
	if mailSender != nil {
		tools.SetGmailSender(mailSender)
	}
	// SWT-28 criterion 24: ops-mcp MUST wire the booker — book_calendar_block
	// is agent-facing and the auto tier is dead on arrival without it (every
	// worker call would fail "no calendar booking adapter wired"). Absent
	// configuration leaves the seam nil and booking refused by name.
	if pdURL := os.Getenv("PIPEDREAM_CALENDAR_URL"); pdURL != "" {
		token, err := google.PipedreamTokenFromEnv()
		if err != nil {
			return fmt.Errorf("configure calendar booker: %w", err)
		}
		booker, err := google.NewPipedreamCalendarClient(pdURL, token, nil)
		if err != nil {
			return fmt.Errorf("configure calendar booker: %w", err)
		}
		tools.SetCalendarBooker(booker)
	}

	adapter := mcpserver.New(ex, workerID)
	slog.Info("ops-mcp serving", "worker_id", workerID, "tools", len(adapter.ListTools()))
	return adapter.Serve(ctx, "ops-mcp")
}
