// opsworker is the per-client worker wrapper (SPEC 04-mcp-task-tools): the
// CLAUDE.md loop — heartbeat(idle) → get_next → claim → claude -p → capture
// session_id → done | park — with the mandatory dead LWT held by a persistent
// MQTT connection.
//
//	opsworker --client <name> [--subproject X] [--once]
//
//	MQTT_BROKER   e.g. tcp://192.168.50.45:1883, required
//	DATABASE_URL  ops db, required
//	CLAUDE_BIN    claude binary (default "claude"; tests point it at a stub)
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/fleet"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
	"github.com/sspataro57/switchboard/internal/worker"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	client := flag.String("client", "", "client name (= worker_id for single console), required")
	subproject := flag.String("subproject", "", "optional subproject filter")
	once := flag.Bool("once", false, "process a single task (or one empty poll) and exit")
	flag.Parse()

	if err := run(*client, *subproject, *once); err != nil {
		slog.Error("opsworker failed", "err", err)
		os.Exit(1)
	}
}

func run(client, subproject string, once bool) error {
	if client == "" {
		return fmt.Errorf("--client is required")
	}
	broker := os.Getenv("MQTT_BROKER")
	if broker == "" {
		return fmt.Errorf("MQTT_BROKER is not set")
	}
	claudeBin := os.Getenv("CLAUDE_BIN")
	if claudeBin == "" {
		claudeBin = "claude"
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer pool.Close()

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	ex := executor.New(reg, checker, audit.NewPGStore(pool))

	fl, err := fleet.NewWorkerClient(ctx, broker, client)
	if err != nil {
		return fmt.Errorf("connect broker: %w", err)
	}
	defer fl.Disconnect()

	systemPrompt, err := os.ReadFile(promptPath())
	if err != nil {
		return fmt.Errorf("read worker system prompt: %w", err)
	}

	// The claude subprocess reaches the same executor through ops-mcp.
	opsMCP, err := buildOpsMCP(ctx)
	if err != nil {
		return fmt.Errorf("build ops-mcp: %w", err)
	}
	mcpConfig, err := worker.WriteMCPConfig(os.TempDir(), opsMCP, os.Getenv("DATABASE_URL"), client)
	if err != nil {
		return err
	}
	defer os.Remove(mcpConfig) // credentialed file must not outlive the wrapper

	loop := worker.New(worker.Config{
		Client:       client,
		Subproject:   subproject,
		Once:         once,
		SystemPrompt: string(systemPrompt),
		MCPConfig:    mcpConfig,
	}, ex, fl, worker.CmdRunner{Bin: claudeBin})

	slog.Info("opsworker running", "client", client, "once", once, "claude", claudeBin)
	return loop.Run(ctx)
}

// promptPath resolves prompts/worker-system.md relative to the repo the
// wrapper is started from.
func promptPath() string {
	if p := os.Getenv("OPS_WORKER_PROMPT"); p != "" {
		return p
	}
	return filepath.Join("prompts", "worker-system.md")
}

// buildOpsMCP compiles ops-mcp into a temp binary so the claude subprocess
// spawns a fixed artifact rather than `go run` (cwd differs per repo).
func buildOpsMCP(ctx context.Context) (string, error) {
	out := filepath.Join(os.TempDir(), "ops-mcp")
	bctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(bctx, "go", "build", "-o", out, "./cmd/ops-mcp")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go build ./cmd/ops-mcp: %w", err)
	}
	return out, nil
}
