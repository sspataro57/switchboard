// ops-mcp-user is the stdio MCP server Claude Code sees at user scope, in every
// repo on the workstation (SWT-35, renamed from ops-mcp-read by SWT-37;
// docs/runbooks/ops-mcp-user-scope.md). It serves exactly the user profile —
// project_list, task_list, task_get_next, task_dismiss, task_close,
// task_mark_delivered, since SWT-38 create_task, task_append_log and
// task_set_priority, since SWT-42 mail_list_attachments and
// mail_read_attachment (attachment reads of non-private mail; the locality
// gate is in the handler), and since SWT-44 draft_delivery and update_delivery
// (gmail reply drafts; approve and send stay on the dashboard) — through the
// same executor pipeline as ops-mcp (validate → policy → audit → handler).
// Policy refuses the verbs, task_set_priority and update_delivery to worker
// identities (human_only / mcp_human_only), so OPS_WORKER_ID must be manual:*;
// the profile's pins keep create_task and task_append_log to human
// (Salvador's-lane) tasks, draft_delivery to gmail (require_channel) on a
// thread already filed under the task's project
// (require_thread_in_task_project), and update_delivery to gmail drafts created
// by the mcp:manual:salvo actor — any interactive session, never the drafts
// worker's or the dashboard's (require_own_draft + require_channel).
//
// It is a separate binary, not a setting on ops-mcp, so that nothing can fall
// back to the full surface: there is no variable whose absence restores it.
// It wires ONE sender, the Slack bridge, and nothing else (SWT-77, Salvador
// 2026-09-22: send_slack_reply is on the user profile, and it is dead without
// the bridge). Every other seam stays nil: main imports no connector but
// slackweb and calls no tools.Set* but SetSlackSender, so no secret inherited
// from the launching shell (~/.bashrc exports OPS_TOKEN_KEY) can arm gmail,
// Jira or the calendar. The bridge is armed only from SLACK_WEB_BRIDGE_URL,
// which the MCP config's env block sets and no shell exports; the command
// bridge (SLACK_WEB_BRIDGE_SCRIPT) is never read here.
//
//	DATABASE_URL                 ops db, required
//	OPS_WORKER_ID                the caller's identity, required (manual:salvo)
//	SLACK_WEB_BRIDGE_URL         the mini's bridge, optional (absent: Slack sends refused by name)
//	SLACK_WEB_BRIDGE_TOKEN_FILE  its bearer token (or SLACK_WEB_BRIDGE_TOKEN)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/connector/slackweb"
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
		slog.Error("ops-mcp-user failed", "err", err)
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

	// SWT-77: the one seam this binary arms. HTTP only, from this process's
	// own config; a misconfiguration is fatal here, before any row is touched.
	if rawURL := os.Getenv("SLACK_WEB_BRIDGE_URL"); rawURL != "" {
		token, err := slackweb.TokenFromEnv()
		if err != nil {
			return fmt.Errorf("configure Slack bridge: %w", err)
		}
		bridge, err := slackweb.NewHTTPBridge(rawURL, token, nil)
		if err != nil {
			return fmt.Errorf("configure Slack bridge: %w", err)
		}
		tools.SetSlackSender(bridge)
	}

	adapter := mcpserver.NewWithProfile(ex, workerID, mcpserver.ProfileUser)
	slog.Info("ops-mcp-user serving", "worker_id", workerID, "tools", len(adapter.ListTools()))
	return adapter.Serve(ctx, "ops-mcp-user")
}
