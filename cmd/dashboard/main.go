// dashboard serves the SWT-8 deliveries slice: approve/edit/send with every
// action through the executor. OIDC (Keycloak) when OIDC_ISSUER is set; dev
// login otherwise. k8s/nginx packaging deferred — runs on the workstation.
//
//	DASHBOARD_ADDR default :8085
//	OIDC_ISSUER / OIDC_CLIENT_ID / OIDC_CLIENT_SECRET / OIDC_REDIRECT_URL
//	DATABASE_URL; GMAIL_CONNECTOR_BRIDGE or OPS_TOKEN_KEY + GOOGLE_CLIENT_SECRET_FILE
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/connector/jira"
	"github.com/sspataro57/switchboard/internal/connector/slackweb"
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
	if key := os.Getenv("OPS_TOKEN_KEY"); key != "" {
		tools.SetJiraSender(&jira.AccountSender{Pool: pool, TokenKey: key})
	}

	// One adapter that routes per account auth_type (SWT-11 criterion 13): an
	// app-password mailbox sends over SMTP, an OAuth one keeps the bridge/direct
	// path unchanged. Without this the dashboard could not send from any mailbox
	// onboarded with an app password.
	if sender, how := google.WireMailSender(pool); sender != nil {
		tools.SetGmailSender(sender)
		slog.Info("mail send adapter wired", "transports", how)
	} else {
		slog.Warn("no mail send adapter wired: gmail sends will be refused")
	}

	// Slack: one bridge serves both seams — prefill_delivery drafts through it,
	// send_delivery sends through it (SWT-12 criterion 15). The dashboard had no
	// Slack wiring at all before this, so the Send button could not have worked.
	if bridge, err := slackweb.NewDeliveryBridgeFromEnv(); err != nil {
		return fmt.Errorf("configure Slack bridge: %w", err)
	} else if bridge != nil {
		tools.SetSlackDrafter(bridge)
		tools.SetSlackSender(bridge)
		slog.Info("slack bridge draft+send adapters wired")
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
