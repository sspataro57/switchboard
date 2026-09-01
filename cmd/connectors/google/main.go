// google is the one-shot Google poller. With GMAIL_CONNECTOR_BRIDGE set it
// performs Gmail + Calendar ingestion through the sibling local connector.
// Without it, the existing database-token Gmail + Calendar path is unchanged.
//
//	google [--full] [--normalize-only] [--all] [--calendar-only] [--overlap 1h] [--backfill 2160h] [--account email]
//
//	DATABASE_URL               ops db, required
//	GMAIL_CONNECTOR_BRIDGE     optional absolute local bridge binary
//	OPS_TOKEN_KEY              required for direct mode unless --normalize-only
//	GOOGLE_CLIENT_SECRET_FILE  default ~/.config/switchboard/google_client_secret.json
//	CAPTURE_RULES_MODE         shadow (default) | live
//	CAPTURE_RULES_SINCE        Go duration bounding the capture-rules pass
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

func main() {
	full := flag.Bool("full", false, "rescan the backfill window: in gmail_api mode ignore the gmail cursor (imap keeps per-folder UID cursors and is unaffected); in the calendar phase (imap mode, or inline in gmail_api/bridge) drop the calendar sync token")
	normalizeOnly := flag.Bool("normalize-only", false, "skip ingest; normalize from raw_source_items alone")
	all := flag.Bool("all", false, "normalize every raw row, not only pending ones")
	overlap := flag.Duration("overlap", google.DefaultOverlap, "gmail cursor re-read window")
	backfill := flag.Duration("backfill", google.DefaultBackfill, "gmail initial backfill window")
	account := flag.String("account", "", "limit to one account email")
	watch := flag.Bool("watch", false, "stay resident: IMAP IDLE plus a periodic reconcile sweep")
	calendarOnly := flag.Bool("calendar-only", false, "run only the calendar phase and normalize — no mail ingest, no outbound observation, no capture-rules pass (for a CronJob keeping availability fresh)")
	flag.Parse()

	if *watch {
		if err := watchMain(*backfill, *account); err != nil {
			fmt.Fprintln(os.Stderr, "google:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(*full, *normalizeOnly, *all, *calendarOnly, *overlap, *backfill, *account); err != nil {
		fmt.Fprintln(os.Stderr, "google:", err)
		os.Exit(1)
	}
}

// watchMain owns its own pool and context: the one-shot path bounds itself to
// ten minutes, which must never bound a resident process.
func watchMain(backfill time.Duration, account string) error {
	pool, err := newWatchPool(context.Background())
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	return runWatch(pool, google.Config{
		Backfill:        backfill,
		AccountEmail:    account,
		MaxMessageBytes: google.MaxMessageBytes(),
		Folders:         google.FoldersFromEnv(),
	})
}

func run(full, normalizeOnly, all, calendarOnly bool, overlap, backfill time.Duration, account string) error {
	// calErr carries a calendar-phase failure to the END of the pass (SWT-24):
	// the mail funnel (normalize, outbound observation, capture rules) always
	// runs first, then the error surfaces in the exit code.
	var calErr error
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	sink := google.NewPGSink(pool)

	cfg := google.Config{
		Full: full, All: all, Overlap: overlap, Backfill: backfill, AccountEmail: account,
		MaxMessageBytes: google.MaxMessageBytes(),
		Folders:         google.FoldersFromEnv(),
	}

	// SWT-24 criterion 20: --calendar-only is the calendar phase plus Normalize
	// and NOTHING else. No mail ingest, no ObserveOutbound, no capture-rules
	// pass — those belong to the mail funnel, and the watch loop already runs
	// them. This is what a future CronJob calls to keep availability fresh.
	if calendarOnly {
		stats, err := runCalendarIngest(ctx, pool, sink, productionCalendarClientFactory(pool), cfg)
		printStats("calendar", stats)
		if err != nil {
			return fmt.Errorf("calendar ingest: %w", err)
		}
		nstats, err := google.Normalize(ctx, sink, cfg)
		printStats("normalize", nstats)
		if err != nil {
			return fmt.Errorf("normalize: %w", err)
		}
		return nil
	}

	if !normalizeOnly {
		var stats google.Stats
		binary := os.Getenv("GMAIL_CONNECTOR_BRIDGE")
		source, srcErr := selectMailSource(os.Getenv("MAIL_SOURCE"), binary)
		if srcErr != nil {
			return srcErr
		}
		switch {
		case source == mailSourceIMAP:
			stats, err = runIMAPIngest(ctx, pool, sink, cfg)
		case source == mailSourceBridge:
			// bridgeErr, not err: `bridge, err :=` would declare a block-scoped
			// err, RunBridge's failure would die at the closing brace, and the
			// check below would read the outer (nil) err — a failed ingest
			// exiting 0. Same reason the direct branch uses loadErr/clientErr.
			bridge, bridgeErr := google.NewCommandBridge(binary)
			if bridgeErr != nil {
				return fmt.Errorf("configure Gmail connector bridge: %w", bridgeErr)
			}
			stats, err = google.RunBridge(ctx, bridge, sink, cfg)
		default:
			key := os.Getenv("OPS_TOKEN_KEY")
			if key == "" {
				return fmt.Errorf("OPS_TOKEN_KEY is not set")
			}
			secretFile := os.Getenv("GOOGLE_CLIENT_SECRET_FILE")
			if secretFile == "" {
				home, _ := os.UserHomeDir()
				secretFile = filepath.Join(home, ".config", "switchboard", "google_client_secret.json")
			}
			oauthCfg, loadErr := google.LoadOAuthConfig(secretFile, "")
			if loadErr != nil {
				return loadErr
			}

			factory := func(ctx context.Context, acct google.Account) (google.Clients, error) {
				hc, clientErr := google.TokenClient(ctx, pool, oauthCfg, acct, key)
				if clientErr != nil {
					return google.Clients{}, clientErr
				}
				return google.Clients{
					Gmail:    google.NewGmailClient(hc, "", acct.Email),
					Calendar: google.NewCalendarClient(hc, ""),
				}, nil
			}
			stats, err = google.Run(ctx, sink, factory, cfg)
		}
		printStats("ingest", stats)
		if err != nil {
			return fmt.Errorf("ingest: %w", err)
		}

		// The calendar phase (SWT-24 criterion 17): imap mode only — bridge and
		// gmail_api already ingest calendar inline, and a second pass would
		// race the first for the same cursor key (criterion 18). Its error is
		// HELD, not returned: a per-deployment calendar failure (an unset key,
		// a revoked consent) must not stop mail normalization below, or the
		// funnel stalls with only an exit code to say so. The pass still exits
		// non-zero at the end, and the per-account sync_runs error rows keep
		// propose_slots refusing honestly either way.
		if calendarPhaseRuns(source) {
			calStats, err := runCalendarIngest(ctx, pool, sink, productionCalendarClientFactory(pool), cfg)
			printStats("calendar", calStats)
			calErr = err
		}
	}

	stats, err := google.Normalize(ctx, sink, cfg)
	printStats("normalize", stats)
	if err != nil {
		return fmt.Errorf("normalize: %w", err)
	}
	// Externally-sent messages: log them on the tasks they correspond to, so a
	// reply sent by hand does not leave the task looking untouched (SWT-16).
	// After Normalize, so this pass's own delivery confirmations are already
	// stamped and a message switchboard sent is never mislabeled as external.
	observed, err := capture.ObserveOutbound(ctx, pool, capture.Gmail)
	if err != nil {
		return fmt.Errorf("observe outbound: %w", err)
	}
	// Printed unconditionally: a silent pass and a pass that did not run look
	// identical in the logs, and this one is expected to find nothing most times.
	fmt.Printf("capture: {\"outbound_observed\":%d}\n", observed)

	// Deterministic project assignment (SWT-17). Channel-agnostic, so this pass
	// covers messages every connector ingested, not only Gmail's — four CronJobs
	// racing is expected and is what the advisory lock inside EvaluateRules is
	// for. This main built no executor before; tasks/external_refs/task_events
	// are reachable only through it (invariant 3).
	rulesCfg := captureRulesConfig()
	rules, err := capture.EvaluateRules(ctx, pool, newExecutor(pool), rulesCfg)
	// Same rule as the line above, and the reason this printf is not guarded by
	// err: the counts go out unconditionally, zeros included, so "matched
	// nothing" and "never ran" are different lines in a CronJob log.
	printCaptureRules(rulesCfg, rules)
	if err != nil {
		return fmt.Errorf("capture rules: %w", err)
	}
	if calErr != nil {
		return fmt.Errorf("calendar ingest: %w", calErr)
	}
	return nil
}

// captureRulesConfig builds the pass's configuration.
//
// Mode and horizon come from capture's own readers, not from a local os.Getenv:
// the "720 is not a Go duration" defence has ONE spelling, and a second copy in
// each of five mains is exactly how four of them end up disagreeing. Actor names
// this connector so the audit trail says which CronJob won the lock.
//
// It lives in this file rather than being inlined because the google connector has
// TWO drivers — the one-shot pass and the resident watch loop — and they must not
// be configured differently.
func captureRulesConfig() capture.RulesConfig {
	mode := capture.RulesMode()
	return capture.RulesConfig{
		Mode:    mode,
		Horizon: capture.RulesHorizon(mode),
		Actor:   "capture:google",
	}
}

// printCaptureRules emits the pass's counts, always, including all zeros.
func printCaptureRules(cfg capture.RulesConfig, stats capture.RulesStats) {
	fmt.Printf("capture_rules: {\"mode\":%q,\"considered\":%d,\"matched\":%d,\"unmatched\":%d,"+
		"\"tasks_created\":%d,\"appended\":%d}\n",
		cfg.Mode, stats.Considered, stats.Matched, stats.Unmatched, stats.TasksCreated, stats.Appended)
}

// newExecutor is the four-line block cmd/connectors/github/main.go established:
// registry → tools.Register → policy.NewMatrix → executor.New. Shared by the
// one-shot path and the watch loop, which builds it once rather than per pass.
func newExecutor(pool *pgxpool.Pool) *executor.Executor {
	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	return executor.New(reg, checker, audit.NewPGStore(pool))
}

func printStats(phase string, stats google.Stats) {
	out, _ := json.Marshal(stats)
	fmt.Printf("%s: %s\n", phase, out)
}
