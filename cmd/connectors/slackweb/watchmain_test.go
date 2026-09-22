package main

// slack-watch-sweep (SWT-75) criteria 8 and 13: the two refusals the watcher
// makes at STARTUP, and the one line it prints when it accepts. ZERO I/O: env
// vars and pure functions.
//
// IMPOSED SURFACE (names chosen here; the SPEC fixes the behaviour):
//
//	// D8: SLACK_WEB_BRIDGE_URL selects the HTTP bridge. Unset, newSource falls
//	// back to CommandBridge, whose Export IGNORES the request entirely
//	// (bridge.go:42-58) — a targeted pass over that transport would silently run
//	// a FULL export every minute. watchSource refuses instead.
//	func watchSource() (slackweb.Source, error)
//
//	// criterion 13's one line:
//	//   slack watch: interval=60s rotation=30m budget=150s targets=2 mode=live
//	//   horizon=720h health=:8093
//	func startupLine(cfg slackweb.WatchConfig, targets int, mode string, horizon time.Duration) string
//
//	// criterion 13's refusal: a LIVE capture config whose horizon is under
//	// capture.MinLiveRulesHorizon makes every pass a silent no-op. shadow is
//	// NOT refused.
//	func checkCaptureConfig(mode string, horizon time.Duration) error
//
// GREENFIELD NOTE — EXPECTED RED: none of the three exists, so package main's
// test build compile-FAILS.
//
// MUTATION: allow --watch without SLACK_WEB_BRIDGE_URL -> criterion 8.

import (
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/connector/slackweb"
)

// Criterion 8: the watcher exits non-zero at startup when SLACK_WEB_BRIDGE_URL
// is unset, NAMING CommandBridge as the reason. Naming it matters: the failure
// it prevents is invisible — a full 15-minute export every minute, reported as
// success, on a transport that discards the targets it was given.
func TestWatchSource_RefusesTheCommandBridge(t *testing.T) {
	t.Setenv("SLACK_WEB_BRIDGE_URL", "")
	t.Setenv("SLACK_WEB_BRIDGE_SCRIPT", "/tmp/fake-bridge.js")
	t.Setenv("SLACK_WEB_NODE", "node")

	src, err := watchSource()
	if err == nil {
		t.Fatalf("watchSource() with SLACK_WEB_BRIDGE_URL unset returned %T and no error. D8/criterion 8: "+
			"CommandBridge.Export IGNORES the request (\"the CLI transport has no body\", bridge.go:42-58), so "+
			"a targeted pass over it would run a FULL export every minute — the opposite of this ticket", src)
	}
	if !strings.Contains(err.Error(), "CommandBridge") {
		t.Errorf("watchSource() error = %q, want it to NAME CommandBridge (criterion 8)", err)
	}
	if !strings.Contains(err.Error(), "SLACK_WEB_BRIDGE_URL") {
		t.Errorf("watchSource() error = %q, want it to name the env var the operator must set", err)
	}
}

// The configuration that IS accepted. (NewHTTPBridge validates the URL and the
// >= 32-character token itself; this only pins that the URL path is taken.)
func TestWatchSource_TakesTheHTTPBridge(t *testing.T) {
	t.Setenv("SLACK_WEB_BRIDGE_URL", "http://192.168.50.130:8787")
	t.Setenv("SLACK_WEB_BRIDGE_TOKEN", strings.Repeat("k", 40))
	if _, err := watchSource(); err != nil {
		t.Errorf("watchSource() with a bridge URL and a token = %v, want a bridge", err)
	}
}

// Criterion 13: one startup line, with every value an operator needs to explain
// the watcher's behaviour without reading the manifest.
func TestStartupLine_NamesEveryKnob(t *testing.T) {
	cfg := slackweb.WatchConfig{
		Interval:         60 * time.Second,
		RotationInterval: 30 * time.Minute,
		BudgetMS:         150000,
		BridgeGrace:      120 * time.Second,
		HealthAddr:       ":8093",
	}
	line := startupLine(cfg, 2, capture.RulesModeLive, 720*time.Hour)

	if strings.Count(line, "\n") != 0 {
		t.Errorf("startupLine returned %d lines; criterion 13 says ONE: %q", strings.Count(line, "\n")+1, line)
	}
	for _, want := range []string{
		"slack watch:", "interval=60s", "rotation=30m", "budget=150s", "targets=2",
		"mode=live", "horizon=720h", "health=:8093",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("startupLine = %q, missing %q (criterion 13 prints the line verbatim: `slack watch: "+
				"interval=60s rotation=30m budget=150s targets=2 mode=live horizon=720h health=:8093`)", line, want)
		}
	}
}

// Criterion 13's refusal, and its limit. A live capture pass whose horizon is
// under capture.MinLiveRulesHorizon errors — "the CAPTURE_RULES_SINCE-below-
// MinLiveRulesHorizon landmine turns a failed CronJob run into a resident loop
// that looks alive while capturing nothing" (D8). Shadow is NOT refused: shadow
// is a legitimate way to run the watcher while its rules are being tuned.
func TestCheckCaptureConfig_RefusesASilentLiveNoOp(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    string
		horizon time.Duration
		wantErr bool
	}{
		{"live, well above the floor", capture.RulesModeLive, 720 * time.Hour, false},
		{"live, exactly the floor", capture.RulesModeLive, capture.MinLiveRulesHorizon, false},
		{"live, one minute under the floor", capture.RulesModeLive, capture.MinLiveRulesHorizon - time.Minute, true},
		{"live, the 720ns typo", capture.RulesModeLive, 720 * time.Nanosecond, true},
		{"shadow, unbounded", capture.RulesModeShadow, 0, false},
		{"shadow, tiny horizon", capture.RulesModeShadow, time.Minute, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := checkCaptureConfig(tc.mode, tc.horizon)
			if tc.wantErr && err == nil {
				t.Errorf("checkCaptureConfig(%s, %s) = nil, want a refusal at STARTUP. A CronJob that fails "+
					"this is one red run an operator sees; a resident loop that fails it is a process that "+
					"looks alive forever while capturing nothing (D8, criterion 13)", tc.mode, tc.horizon)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("checkCaptureConfig(%s, %s) = %v, want nil — criterion 13: it does NOT refuse "+
					"mode=shadow", tc.mode, tc.horizon, err)
			}
		})
	}
}
