package capture

// Unit tests for outbound-capture (SWT-16, docs/tickets/outbound-capture_SPEC.md):
// the horizon env accessor (criterion 10) and the slack thread_key → target_ref
// canonicalization (criterion 1). ZERO I/O — no database, no provider, no LLM.
//
// GREENFIELD NOTE: internal/capture does not exist yet, so this file compile-FAILs
// on every symbol below — the expected red state. IMPOSED surface (the SPEC names
// the env var, the default, and the join, but not the Go identifiers; these mirror
// slackweb.DefaultUnconfirmedFlagPasses / slackweb.UnconfirmedFlagPasses):
//
//	const DefaultObserveHorizon = 720 * time.Hour
//	func ObserveHorizon() time.Duration
//	func SlackTargetRefs(threadKey string) []string
//
// SlackTargetRefs exists because criterion 1 REQUIRES a test pinning equality with
// slackweb.Target.CanonicalURL's output: SWT-13's landmine was an exact-string
// target_ref match that silently never fires, and a second spelling of the same
// URL builder would reproduce it with no error anywhere. Building the URLs inside
// an unexported helper is fine — but then this test cannot pin it, so the helper is
// exported.

import (
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
)

// ---- criterion 10: the horizon env, all four corners ----------------------------

func TestObserveHorizon(t *testing.T) {
	if DefaultObserveHorizon != 720*time.Hour {
		t.Errorf("DefaultObserveHorizon = %v, want 720h (SPEC criterion 10)", DefaultObserveHorizon)
	}

	t.Run("unset -> 720h", func(t *testing.T) {
		t.Setenv("OUTBOUND_OBSERVE_HORIZON", "")
		if got := ObserveHorizon(); got != 720*time.Hour {
			t.Errorf("ObserveHorizon() = %v, want 720h", got)
		}
	})

	t.Run("override honoured", func(t *testing.T) {
		t.Setenv("OUTBOUND_OBSERVE_HORIZON", "36h")
		if got := ObserveHorizon(); got != 36*time.Hour {
			t.Errorf("ObserveHorizon() = %v, want the OUTBOUND_OBSERVE_HORIZON override 36h", got)
		}
		t.Setenv("OUTBOUND_OBSERVE_HORIZON", "90m")
		if got := ObserveHorizon(); got != 90*time.Minute {
			t.Errorf("ObserveHorizon() = %v, want 90m", got)
		}
	})

	t.Run("unparseable falls back to the default", func(t *testing.T) {
		// "720" is the realistic typo: a bare number is NOT a Go duration, and
		// treating it as 720ns would silently disable capture entirely.
		for _, v := range []string{"720", "a month", "30 days", "-"} {
			t.Setenv("OUTBOUND_OBSERVE_HORIZON", v)
			if got := ObserveHorizon(); got != DefaultObserveHorizon {
				t.Errorf("ObserveHorizon() with %q = %v, want the default %v", v, got, DefaultObserveHorizon)
			}
		}
	})

	t.Run("non-positive falls back to the default", func(t *testing.T) {
		// A misconfigured 0 or a negative would make the scan window empty, so
		// capture would log nothing forever with no error — the same class of
		// invisible failure slackweb.UnconfirmedFlagPasses guards against.
		for _, v := range []string{"0", "0s", "-1h", "-720h"} {
			t.Setenv("OUTBOUND_OBSERVE_HORIZON", v)
			if got := ObserveHorizon(); got != DefaultObserveHorizon {
				t.Errorf("ObserveHorizon() with %q = %v, want the default %v", v, got, DefaultObserveHorizon)
			}
		}
	})
}

// ---- criterion 1: the slack join must be slackweb's canonicalization ------------

// A slack thread_key is slack:{ws}:{conv}[:{root}]; the delivery it corresponds to
// stores Target.CanonicalURL(). Both forms must match, because a delivery targeting
// the CONVERSATION corresponds on the whole conversation including its threads.
func TestSlackTargetRefsPinEqualityWithCanonicalURL(t *testing.T) {
	const (
		workspace = "TCAPTURE"
		conv      = "CCAPTURE"
		root      = "p1780000000000001"
	)
	conversationRef := slackweb.Target{WorkspaceID: workspace, ConversationID: conv}.CanonicalURL()
	threadRef := slackweb.Target{WorkspaceID: workspace, ConversationID: conv, MessageID: root}.CanonicalURL()

	t.Run("threaded key yields the thread AND conversation forms", func(t *testing.T) {
		got := SlackTargetRefs("slack:" + workspace + ":" + conv + ":" + root)
		want := map[string]bool{conversationRef: true, threadRef: true}
		assertRefSet(t, got, want)
	})

	t.Run("conversation-level key yields the conversation form", func(t *testing.T) {
		got := SlackTargetRefs("slack:" + workspace + ":" + conv)
		assertRefSet(t, got, map[string]bool{conversationRef: true})
	})

	t.Run("keys that are not slack thread keys yield nothing", func(t *testing.T) {
		// Refusing to guess, per the SPEC: a malformed key must produce no join
		// candidate rather than a URL that can never match.
		for _, key := range []string{
			"",
			"slack",
			"slack:" + workspace,
			"slack:" + workspace + ":" + conv + ":" + root + ":extra",
			"jira:capture.local:CAP-1",
			"gmail:itest-capture:t-1",
		} {
			if got := SlackTargetRefs(key); len(got) != 0 {
				t.Errorf("SlackTargetRefs(%q) = %v, want no candidates", key, got)
			}
		}
	})
}

func assertRefSet(t *testing.T, got []string, want map[string]bool) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("SlackTargetRefs returned %d refs (%v), want exactly %d (%v)", len(got), got, len(want), keysOf(want))
	}
	seen := map[string]bool{}
	for _, ref := range got {
		if !want[ref] {
			t.Errorf("SlackTargetRefs returned %q, which is not one of %v — the join matches target_ref by "+
				"EXACT string, so any second spelling of the canonical URL is a permanent silent miss (SWT-13)",
				ref, keysOf(want))
		}
		if seen[ref] {
			t.Errorf("SlackTargetRefs returned %q twice; duplicate candidates would fan the join out", ref)
		}
		seen[ref] = true
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
