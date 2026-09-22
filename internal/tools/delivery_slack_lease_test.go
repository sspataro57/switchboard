package tools

// slack-send-queue (SWT-76) Part 2, criterion 14 — the lease arithmetic (D3).
//
// The queued send's ONLY protection against a second, human-declared failure
// landing on top of a click that is still pending is `mark_delivery_failed`'s
// refusal of an unsettled attempt younger than `sendAttemptLease`
// (delivery.go:2195-2205). That protection is FICTION unless the leaf is
// guaranteed to have stopped clicking before the lease expires:
//
//	sendQueueMaxWait + sendQueueClickAllowance <= sendAttemptLease
//
// Ship 10m + 2m <= 15m. Nothing derives the bound from an environment variable,
// because the one place that owns the lease arithmetic must be the one place
// that computes the bound (D3).
//
// ZERO I/O: constants and one env read.
//
// ---------------------------------------------------------------------------
// GREENFIELD NOTE — this file compile-FAILs today: sendQueueMaxWait,
// sendQueueClickAllowance and slackSendQueueMaxWait do not exist. Expected red.
//
// IMPOSED SURFACE (package tools, unexported — nothing outside this package has
// any business computing the bound):
//
//	const sendQueueMaxWait        = 10 * time.Minute
//	const sendQueueClickAllowance = 2 * time.Minute
//	// slackSendQueueMaxWait is the max_queue_ms sendSlackReply puts in the
//	// request. It reads SLACK_SEND_QUEUE_MAX_WAIT, CLAMPS anything above
//	// sendQueueMaxWait down to it (logging the clamp), and falls back to
//	// sendQueueMaxWait for anything unparseable or non-positive.
//	func slackSendQueueMaxWait() time.Duration
//
// IMPOSED, and flagged: the SPEC does not say how SLACK_SEND_QUEUE_MAX_WAIT is
// SPELLED. It cites "the positiveEnv discipline at export_request.go:69-82",
// which is an Atoi over a *_MS name, but this variable carries no _MS suffix
// and its neighbours in the same subsystem (SLACK_WATCH_INTERVAL=180s) are Go
// duration strings. This file imposes a Go duration string, parsed with
// time.ParseDuration. If the implementer prefers milliseconds, change the
// literals in the table below and nothing else — the CLAMP and FALLBACK
// behaviour is the contract, not the spelling.
//
// CONTRADICTION FOUND IN THE SPEC, deliberately left for the implementer and
// reported rather than papered over: criterion 14 says "an unparseable or
// NON-POSITIVE value falls back to the default", while Rollback lever 1 says
// "SLACK_SEND_QUEUE_MAX_WAIT=0 clamps to 'never queue'" and is the no-roll,
// no-restart way to stop producing 202s. Those cannot both be true of the
// value 0. This file encodes the ACCEPTANCE CRITERION (0 -> default), which
// makes Rollback lever 1 unreachable as written; resolving it needs either a
// distinct spelling for "never" (e.g. SLACK_SEND_QUEUE_MAX_WAIT=off) or an
// amendment to criterion 14. See TestSlackSendQueueMaxWait_ClampAndFallback's
// "zero" case.
//
// MUTATION: raise sendQueueMaxWait above sendAttemptLease -> criterion 14 ->
// TestSendQueueBoundFitsInsideTheSendAttemptLease.

import (
	"bytes"
	"log/slog"
	"testing"
	"time"
)

// The inequality D4's whole protection rests on. A queued send's row is
// unsettled from send_attempted_at; the leaf may hold it for at most
// sendQueueMaxWait and then needs sendQueueClickAllowance to drive the browser.
// If the sum could exceed sendAttemptLease, mark_delivery_failed would become
// permitted while a click is still possible, the row would become re-approvable,
// and invariant 4's "never a double send" would be a hope.
func TestSendQueueBoundFitsInsideTheSendAttemptLease(t *testing.T) {
	if sendQueueMaxWait <= 0 {
		t.Fatalf("sendQueueMaxWait = %s, want a positive bound", sendQueueMaxWait)
	}
	if sendQueueClickAllowance <= 0 {
		t.Fatalf("sendQueueClickAllowance = %s, want a positive allowance for the click itself",
			sendQueueClickAllowance)
	}
	if sendQueueMaxWait+sendQueueClickAllowance > sendAttemptLease {
		t.Fatalf("sendQueueMaxWait(%s) + sendQueueClickAllowance(%s) = %s > sendAttemptLease(%s). D3/D6: the "+
			"lease is the ONLY thing stopping a human declaring a queued send failed while the leaf may still "+
			"click it, and a `failed` row is re-approvable (delivery.go:1102) — so a bound that outlives the "+
			"lease is an automatic path to a double post into a client conversation",
			sendQueueMaxWait, sendQueueClickAllowance, sendQueueMaxWait+sendQueueClickAllowance, sendAttemptLease)
	}
	// The shipped values, named so a silent widening is visible in the diff.
	if sendQueueMaxWait != 10*time.Minute {
		t.Errorf("sendQueueMaxWait = %s, want 10m (D3's shipped value)", sendQueueMaxWait)
	}
	if sendQueueClickAllowance != 2*time.Minute {
		t.Errorf("sendQueueClickAllowance = %s, want 2m (D3's shipped value)", sendQueueClickAllowance)
	}
	if sendAttemptLease != 15*time.Minute {
		t.Errorf("sendAttemptLease = %s, want 15m — D11 says this ticket does not move it", sendAttemptLease)
	}
}

// The override is CLAMPED, never obeyed past the bound: a value the lease would
// reject must not reach the leaf. Same discipline as positiveEnv
// (export_request.go:69-82), where a bad value falls back rather than producing
// one the far side refuses.
func TestSlackSendQueueMaxWait_ClampAndFallback(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		want      time.Duration
		wantClamp bool
		// wantQuiet marks the paths that must log NOTHING: the ordinary ones.
		// A fallback (garbage, zero, negative) MAY log — that is the
		// implementer's call and is not asserted either way.
		wantQuiet bool
	}{
		{"unset", "", sendQueueMaxWait, false, true},
		{"under the bound is obeyed", "5m", 5 * time.Minute, false, true},
		{"at the bound", "10m", sendQueueMaxWait, false, true},
		{"above the bound is clamped", "45m", sendQueueMaxWait, true, false},
		{"garbage falls back", "soon", sendQueueMaxWait, false, false},
		// See the CONTRADICTION note at the top of this file: criterion 14's
		// "non-positive falls back" versus Rollback lever 1's "0 = never queue".
		{"zero falls back", "0", sendQueueMaxWait, false, false},
		{"negative falls back", "-3m", sendQueueMaxWait, false, false},
		// 2026-09-22 (review): the rollback lever. "off" is the one non-duration
		// spelling and means never queue — 0 omits max_queue_ms from the request
		// (TestQueue_MaxQueueMSTravelsInTheRequestAndZeroOmitsIt pins the wire).
		{"off never queues", "off", 0, false, true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.env == "" {
				t.Setenv("SLACK_SEND_QUEUE_MAX_WAIT", "")
			} else {
				t.Setenv("SLACK_SEND_QUEUE_MAX_WAIT", tc.env)
			}
			var logged bytes.Buffer
			restore := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
			defer slog.SetDefault(restore)

			got := slackSendQueueMaxWait()
			if got != tc.want {
				t.Fatalf("slackSendQueueMaxWait() with SLACK_SEND_QUEUE_MAX_WAIT=%q = %s, want %s. The bound "+
					"is derived from the lease (D3), never a free env value: a larger one would let the leaf "+
					"hold a send past the moment mark_delivery_failed becomes permitted",
					tc.env, got, tc.want)
			}
			if got+sendQueueClickAllowance > sendAttemptLease {
				t.Fatalf("SLACK_SEND_QUEUE_MAX_WAIT=%q produced %s, which does not fit inside the lease",
					tc.env, got)
			}
			if tc.wantClamp && logged.Len() == 0 {
				t.Errorf("clamping SLACK_SEND_QUEUE_MAX_WAIT=%q logged nothing. D3 requires the clamp to be "+
					"VISIBLE: an operator who set 45m and silently got 10m has no way to learn why sends are "+
					"still being refused during a long rotation", tc.env)
			}
			if tc.wantQuiet && logged.Len() > 0 {
				t.Errorf("SLACK_SEND_QUEUE_MAX_WAIT=%q logged %q; an ordinary value is not worth a line, or "+
					"the log fires on every send", tc.env, logged.String())
			}
		})
	}
}
