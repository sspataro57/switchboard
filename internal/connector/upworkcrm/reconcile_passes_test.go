package upworkcrm_test

// Unit test for the upwork reconciler's pass threshold (SWT-19 acceptance
// criterion 17). ZERO I/O — this is only the constant and the env parse.
//
// The number is SIX where slackweb's is THREE, and that is arithmetic rather
// than taste: ONE upworkcrm invocation writes TWO sync_runs rows, because
// Ingest calls StartRun (ingest.go:70) and Normalize calls it again
// (normalize.go:191), and both finish 'ok'. Verified against production, where
// the rows arrive in pairs on every */15 tick. A threshold copied from slackweb
// would fire after 1.5 CronJob invocations (~22 minutes) instead of three.
//
// GREENFIELD NOTE: compile-FAILs until reconcile.go lands
// (UnconfirmedFlagPasses / DefaultUnconfirmedFlagPasses). Expected red state.

import (
	"os"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
	"github.com/sspataro57/switchboard/internal/connector/upworkcrm"
)

func TestUpworkUnconfirmedFlagPasses(t *testing.T) {
	if upworkcrm.DefaultUnconfirmedFlagPasses != 6 {
		t.Errorf("DefaultUnconfirmedFlagPasses = %d, want 6 (= 3 invocations, because upworkcrm writes TWO "+
			"sync_runs rows per invocation)", upworkcrm.DefaultUnconfirmedFlagPasses)
	}
	// Not decoration: the whole point of 6 is that it means the SAME number of
	// real connector invocations slackweb's 3 means. Pinning the relationship
	// makes a future change to either side visible instead of silently halving
	// or doubling the alarm's patience.
	if upworkcrm.DefaultUnconfirmedFlagPasses != 2*slackweb.DefaultUnconfirmedFlagPasses {
		t.Errorf("upwork default (%d) != 2x slackweb default (%d): the factor of two IS the double-sync_runs-row "+
			"fact. If slackweb's threshold moved, decide deliberately rather than letting the two drift",
			upworkcrm.DefaultUnconfirmedFlagPasses, slackweb.DefaultUnconfirmedFlagPasses)
	}

	t.Run("unset -> the default", func(t *testing.T) {
		t.Setenv("UPWORK_UNCONFIRMED_FLAG_PASSES", "")
		if got := upworkcrm.UnconfirmedFlagPasses(); got != 6 {
			t.Errorf("UnconfirmedFlagPasses() = %d, want 6", got)
		}
	})
	t.Run("override honoured", func(t *testing.T) {
		t.Setenv("UPWORK_UNCONFIRMED_FLAG_PASSES", "10")
		if got := upworkcrm.UnconfirmedFlagPasses(); got != 10 {
			t.Errorf("UnconfirmedFlagPasses() = %d, want the UPWORK_UNCONFIRMED_FLAG_PASSES override 10", got)
		}
	})
	t.Run("garbage and non-positive fall back to the default", func(t *testing.T) {
		// A misconfigured 0 would mean "flag every unconfirmed row on the first
		// pass", turning an operational typo into a stream of false alarms about
		// rows that are merely waiting for the next run. slackweb's accessor
		// defends the same way; criterion 17 asks for the match explicitly.
		for _, v := range []string{"0", "-1", "6.5", "six", " "} {
			t.Setenv("UPWORK_UNCONFIRMED_FLAG_PASSES", v)
			if got := upworkcrm.UnconfirmedFlagPasses(); got != 6 {
				t.Errorf("UnconfirmedFlagPasses() with %q = %d, want the default 6", v, got)
			}
		}
	})
	t.Run("reads its OWN env var, not slackweb's", func(t *testing.T) {
		// Copying the reconciler is the intended move; copying its env var name
		// with it would make one knob silently retune two connectors.
		t.Setenv("SLACK_UNCONFIRMED_FLAG_PASSES", "99")
		t.Setenv("UPWORK_UNCONFIRMED_FLAG_PASSES", "")
		if got := upworkcrm.UnconfirmedFlagPasses(); got != 6 {
			t.Errorf("UnconfirmedFlagPasses() = %d with only SLACK_UNCONFIRMED_FLAG_PASSES set; want the upwork "+
				"default 6 — the two connectors must not share a knob", got)
		}
	})
}

// Criterion 17 also asks for the reason to be written down where the constant
// lives, because "6" with no explanation is indistinguishable from a typo for
// slackweb's 3 and would be "corrected" by the next reader. Mechanized because
// this specific comment is load-bearing: IK records that the double-run fact was
// discovered while speccing this ticket and is invisible from the call sites.
func TestReconcilerPassCommentExplainsTheDoubleRun(t *testing.T) {
	src, err := os.ReadFile("reconcile.go")
	if err != nil {
		t.Fatalf("read reconcile.go: %v", err)
	}
	body := strings.ToLower(string(src))
	for _, want := range []string{"sync_runs", "invocation", "two"} {
		if !strings.Contains(body, want) {
			t.Errorf("reconcile.go never mentions %q: the pass default is 6 rather than slackweb's 3 ONLY because "+
				"one upworkcrm invocation writes two sync_runs rows, and a reader who does not know that will "+
				"read 6 as a copy error", want)
		}
	}
	// Guard the guard: if the file stops containing the constant this scan is
	// certifying nothing (the failure mode of every source-scanning test).
	if !strings.Contains(string(src), "DefaultUnconfirmedFlagPasses") {
		t.Fatalf("reconcile.go no longer declares DefaultUnconfirmedFlagPasses; this scan has stopped checking anything")
	}
}
