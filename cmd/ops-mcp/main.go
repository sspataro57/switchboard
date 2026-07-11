// ops-mcp is the stdio MCP server exposing the agent-facing task tools (SPEC
// 04-mcp-task-tools). It is a thin adapter: every tools/call maps to
// executor.Execute — validate → policy → audit → handler (invariant 3).
//
//	DATABASE_URL   ops db, required
//	OPS_WORKER_ID  the caller's identity, required (wrapper sets it when
//	               spawning claude; interactive sessions use manual:<user>)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sspataro57/switchboard/internal/audit"
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
	ex := executor.New(reg, policy.NewStatic(reg.Names()...), audit.NewPGStore(pool))
	adapter := mcpserver.New(ex, workerID)

	srv := mcp.NewServer(&mcp.Implementation{Name: "ops-mcp", Version: "0.1.0"}, nil)
	for _, t := range adapter.ListTools() {
		srv.AddTool(
			&mcp.Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				out, err := adapter.CallTool(ctx, req.Params.Name, req.Params.Arguments)
				if err != nil {
					return &mcp.CallToolResult{
						IsError: true,
						Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
					}, nil
				}
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: string(out)}},
				}, nil
			})
	}

	slog.Info("ops-mcp serving", "worker_id", workerID, "tools", len(adapter.ListTools()))
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
