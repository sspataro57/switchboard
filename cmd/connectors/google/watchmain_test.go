package main

// imap-idle-watch (SWT-73) criteria 11 and 12 / D8: the refusal the watcher
// makes at STARTUP, and the one line it prints when it accepts. ZERO I/O: env
// vars and pure functions.
//
// The landmine, named in the SPEC and recorded in
// docs/runbooks/HANDOFF-kube-jira-activity-revive.md:60: "A google --watch loop,
// if one is ever deployed, only logs that error and prints a zero counter line
// on every wake, so it looks alive while capturing nothing." Under cron a bad
// CAPTURE_RULES_SINCE is one red run an operator sees; in a resident loop it is
// a pod that passes its liveness probe forever and captures nothing.
//
// IMPOSED SURFACE (package main, cmd/connectors/google/watch.go; the SPEC fixes
// the behaviour, the names mirror cmd/connectors/slackweb's
// checkCaptureConfig/startupLine):
//
//	// D8: exits non-zero when capture.RulesConfig{…}.normalize() would error —
//	// today that is a LIVE horizon below capture.MinLiveRulesHorizon. It does
//	// NOT refuse mode=shadow: shadow is a legitimate configuration and
//	// RulesMode's fail-safe (rules_store.go:165-176) must not be inverted.
//	func checkCaptureConfig(cfg capture.RulesConfig) error
//
//	// Criterion 12's one line:
//	//   watch: mode=live horizon=720h reconcile=10m idle_refresh=25m
//	//   pass_timeout=10m accounts=4 health=:8092
//	func startupLine(cfg watchConfig, rules capture.RulesConfig, accounts int) string
//
// GREENFIELD NOTE — EXPECTED RED: neither function exists, so package main's
// test build compile-FAILS.
//
// MUTATIONS: accept a sub-2h live CAPTURE_RULES_SINCE / refuse to start in
// shadow mode -> criterion 11.

import (
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/capture"
)

// Criterion 11: the refusal, and its limit.
func TestCheckCaptureConfig_RefusesASilentLiveNoOpButNotShadow(t *testing.T) {
	cases := []struct {
		name    string
		cfg     capture.RulesConfig
		wantErr bool
	}{
		{"live, the production horizon", capture.RulesConfig{Mode: capture.RulesModeLive, Horizon: 720 * time.Hour}, false},
		{"live, exactly the floor", capture.RulesConfig{Mode: capture.RulesModeLive, Horizon: capture.MinLiveRulesHorizon}, false},
		{"live, one minute under the floor", capture.RulesConfig{Mode: capture.RulesModeLive, Horizon: capture.MinLiveRulesHorizon - time.Minute}, true},
		{"live, the 720ns typo", capture.RulesConfig{Mode: capture.RulesModeLive, Horizon: 720 * time.Nanosecond}, true},
		{"shadow, unbounded", capture.RulesConfig{Mode: capture.RulesModeShadow}, false},
		{"shadow, tiny horizon", capture.RulesConfig{Mode: capture.RulesModeShadow, Horizon: time.Minute}, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := checkCaptureConfig(tc.cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("checkCaptureConfig(%s, %s) = nil, want a refusal at STARTUP, before the first "+
						"pass. A CronJob that fails this check is one red run an operator sees; a resident "+
						"loop that fails it logs an error on every wake, prints a zero counter line, and "+
						"looks alive while capturing nothing (D8, criterion 11)", tc.cfg.Mode, tc.cfg.Horizon)
				}
				for _, want := range []string{"CAPTURE_RULES_SINCE", "MinLiveRulesHorizon"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the refusal reads %q; criterion 11 has it NAME %q, because the operator "+
							"reading `kubectl logs` has to know which manifest value to change and what "+
							"the floor is", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Errorf("checkCaptureConfig(%s, %s) = %v, want nil. Criterion 11: it does NOT refuse "+
					"mode=shadow — shadow is a legitimate way to run the watcher while rules are tuned, and "+
					"capture.RulesMode's fail-safe (anything that is not exactly \"live\" is shadow) must "+
					"not be inverted into a startup refusal", tc.cfg.Mode, tc.cfg.Horizon, err)
			}
		})
	}
}

// Criterion 12: one startup line carrying everything needed to explain the
// watcher's behaviour without reading the manifest. "A pass that would capture
// nothing is legible from the first ten lines of kubectl logs" — which is only
// true if mode and horizon are on it.
func TestStartupLine_NamesEveryKnob(t *testing.T) {
	cfg := watchConfig{
		Reconcile:    10 * time.Minute,
		IdleRefresh:  25 * time.Minute,
		PassTimeout:  10 * time.Minute,
		StandbyRetry: 15 * time.Second,
		HealthAddr:   ":8092",
	}
	rules := capture.RulesConfig{Mode: capture.RulesModeLive, Horizon: 720 * time.Hour, Actor: "capture:google"}
	line := startupLine(cfg, rules, 4)

	if strings.Count(line, "\n") != 0 {
		t.Errorf("startupLine returned %d lines; D8 says ONE: %q", strings.Count(line, "\n")+1, line)
	}
	for _, want := range []string{
		"watch:", "mode=live", "horizon=720h", "reconcile=10m", "idle_refresh=25m",
		"pass_timeout=10m", "accounts=4", "health=:8092",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("startupLine = %q, missing %q. D8 prints it verbatim: `watch: mode=live horizon=720h "+
				"reconcile=10m idle_refresh=25m pass_timeout=10m accounts=4 health=:8092`", line, want)
		}
	}

	// The shadow spelling has to be legible too: it is the configuration that
	// silently creates nothing, and criterion 24's env-parity check is what
	// catches a manifest that forgot CAPTURE_RULES_MODE=live.
	shadow := startupLine(cfg, capture.RulesConfig{Mode: capture.RulesModeShadow, Horizon: 0}, 4)
	if !strings.Contains(shadow, "mode=shadow") {
		t.Errorf("startupLine in shadow mode = %q, want it to say mode=shadow. D8 does not REFUSE shadow; "+
			"it prints it, and printing it is the only warning an operator gets that this pod will create "+
			"no tasks at all (criteria 11, 12)", shadow)
	}
}

// Criterion 11 again, from the configuration side: the watcher resolves the
// capture config through captureRulesConfig() — the shared reader that exists
// so the one-shot pass and the resident loop "must not be configured
// differently" (main.go's own comment). A second os.Getenv spelling in the
// watch path is how the two drivers drift.
func TestCaptureRulesConfig_IsWhatTheWatcherChecks(t *testing.T) {
	t.Setenv("CAPTURE_RULES_MODE", "live")
	t.Setenv("CAPTURE_RULES_SINCE", "30m") // under the 2h floor

	cfg := captureRulesConfig()
	if cfg.Mode != capture.RulesModeLive {
		t.Fatalf("captureRulesConfig().Mode = %q with CAPTURE_RULES_MODE=live", cfg.Mode)
	}
	if cfg.Horizon != 30*time.Minute {
		t.Fatalf("captureRulesConfig().Horizon = %s with CAPTURE_RULES_SINCE=30m", cfg.Horizon)
	}
	if err := checkCaptureConfig(cfg); err == nil {
		t.Error("checkCaptureConfig accepted the live/30m configuration the deployed watcher would actually " +
			"read. Criterion 11 wires the CHECK to captureRulesConfig()'s output, not to a second local " +
			"parse of the same two variables")
	}
}
