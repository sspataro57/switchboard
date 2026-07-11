// dashboard serves the SWT-8 deliveries slice: approve/edit/send with every
// action through the executor. OIDC (Keycloak) when OIDC_ISSUER is set; dev
// login otherwise. k8s/nginx packaging deferred — runs on the workstation.
//
//	DASHBOARD_ADDR default :8085
//	OIDC_ISSUER / OIDC_CLIENT_ID / OIDC_CLIENT_SECRET / OIDC_REDIRECT_URL
//	DATABASE_URL, OPS_TOKEN_KEY (for real sends), GOOGLE_CLIENT_SECRET_FILE
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/dashboard"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("dashboard failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
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

	// Wire the real gmail send adapter when the credentials exist; without
	// them send_delivery fails cleanly ("no gmail send adapter wired" never
	// happens — the account resolution errors instead).
	if key := os.Getenv("OPS_TOKEN_KEY"); key != "" {
		secretFile := os.Getenv("GOOGLE_CLIENT_SECRET_FILE")
		if secretFile == "" {
			home, _ := os.UserHomeDir()
			secretFile = filepath.Join(home, ".config", "switchboard", "google_client_secret.json")
		}
		if oauthCfg, err := google.LoadOAuthConfig(secretFile, ""); err == nil {
			tools.SetGmailSender(&google.AccountSender{Pool: pool, OAuthCfg: oauthCfg, TokenKey: key})
			slog.Info("gmail send adapter wired")
		} else {
			slog.Warn("gmail send adapter not wired", "err", err)
		}
	}

	auth, err := dashboard.NewAuth(ctx,
		os.Getenv("OIDC_ISSUER"), os.Getenv("OIDC_CLIENT_ID"),
		os.Getenv("OIDC_CLIENT_SECRET"), os.Getenv("OIDC_REDIRECT_URL"))
	if err != nil {
		return err
	}

	srv, err := dashboard.NewServer(pool, ex, auth)
	if err != nil {
		return err
	}

	addr := os.Getenv("DASHBOARD_ADDR")
	if addr == "" {
		addr = ":8085"
	}
	slog.Info("dashboard running", "addr", addr, "oidc", os.Getenv("OIDC_ISSUER") != "")
	hs := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	return hs.ListenAndServe()
}
