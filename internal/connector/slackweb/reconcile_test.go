package slackweb

// Unit test for the unconfirmed-send flag threshold (SWT-12 criterion 11 / Q2).
// ZERO I/O — this is only the env parse.
//
// Q2 ANSWERED: N = 3 COMPLETED SUCCESSFUL EXPORT PASSES, env-tunable as
// SLACK_UNCONFIRMED_FLAG_PASSES. Wall time was rejected: a paused poller, a
// suspended CronJob, or a mini that is simply off accumulates no passes and
// therefore cannot false-flag — whereas wall time would raise "the send may have
// failed" when the fact is "the poller didn't run". Those are different facts and
// must not share an alarm.
//
// GREENFIELD NOTE: UnconfirmedFlagPasses does not exist yet, so this file
// compile-FAILs — the expected red state. Imposed surface (the SPEC names the env
// var and the value but not the accessor):
//
//	const DefaultUnconfirmedFlagPasses = 3
//	func UnconfirmedFlagPasses() int

import "testing"

func TestUnconfirmedFlagPasses(t *testing.T) {
	if DefaultUnconfirmedFlagPasses != 3 {
		t.Errorf("DefaultUnconfirmedFlagPasses = %d, want 3 (SWT-12 Q2)", DefaultUnconfirmedFlagPasses)
	}

	t.Run("unset -> 3", func(t *testing.T) {
		t.Setenv("SLACK_UNCONFIRMED_FLAG_PASSES", "")
		if got := UnconfirmedFlagPasses(); got != 3 {
			t.Errorf("UnconfirmedFlagPasses() = %d, want 3", got)
		}
	})
	t.Run("override honoured", func(t *testing.T) {
		t.Setenv("SLACK_UNCONFIRMED_FLAG_PASSES", "5")
		if got := UnconfirmedFlagPasses(); got != 5 {
			t.Errorf("UnconfirmedFlagPasses() = %d, want the SLACK_UNCONFIRMED_FLAG_PASSES override 5", got)
		}
	})
	t.Run("garbage and non-positive fall back to the default", func(t *testing.T) {
		// A misconfigured 0 must not mean "flag every unconfirmed row on the first
		// pass" — that turns an operational typo into a stream of false alarms.
		for _, v := range []string{"0", "-1", "many"} {
			t.Setenv("SLACK_UNCONFIRMED_FLAG_PASSES", v)
			if got := UnconfirmedFlagPasses(); got != 3 {
				t.Errorf("UnconfirmedFlagPasses() with %q = %d, want the default 3", v, got)
			}
		}
	})
}
