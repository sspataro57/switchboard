// hooksd is the GitHub webhook receiver (SPEC 09-jira-github-connectors):
// HMAC verify → raw-first delivery store → PR/task resolve → spine tool
// dispatch through the executor. Public exposure is deploy/operator work; the
// interim gh-token poller (cmd/connectors/github) covers the gap.
//
//	HOOKSD_ADDR            default :8090
//	GITHUB_WEBHOOK_SECRET  required
//	DATABASE_URL           required
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/connector/github"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("hooksd failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	secret := os.Getenv("GITHUB_WEBHOOK_SECRET")
	if secret == "" {
		return fmt.Errorf("GITHUB_WEBHOOK_SECRET is not set")
	}

	ctx := context.Background()
	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	ex := executor.New(reg, checker, audit.NewPGStore(pool))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.Handle("POST /webhook", github.NewReceiver(secret,
		github.NewPGRawStore(pool), github.NewPGTaskResolver(pool, ex, github.Actor), ex))

	addr := os.Getenv("HOOKSD_ADDR")
	if addr == "" {
		addr = ":8090"
	}
	slog.Info("hooksd running", "addr", addr)
	hs := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return hs.ListenAndServe()
}
