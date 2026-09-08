package tools_test

// SWT-29 (docs/tickets/funnel-view_SPEC.md, "API / MCP tool changes" item 5):
// the AVAIL_MAX_SYNC_AGE parse is EXTRACTED from availabilityConfig into
// tools.MaxCalendarSyncAge so that /funnel and propose_slots read one value
// through one parse.
//
// Plain unit test — no build tag, no database, no network. Env only, through
// t.Setenv, which restores it for the rest of the package.
//
// WHY THIS SEAM EXISTS, stated where the next reader will look: the rejected
// alternative was the dashboard calling os.Getenv("AVAIL_MAX_SYNC_AGE") with
// its own time.ParseDuration. Two parses drift, and the drift's symptom is a
// GREEN PAGE WHILE propose_slots REFUSES — worse than having no page, because
// the page is what an operator checks before believing the tool. internal/
// availability is not a candidate home: its package doc forbids env and clock
// reads (SWT-24 criterion 10).
//
// GREENFIELD NOTE — EXPECTED RED. tools.MaxCalendarSyncAge does not exist, so
// this file compile-FAILS the internal/tools test binary until it is extracted.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/tools"
)

func TestMaxCalendarSyncAge_DefaultsToOneHour(t *testing.T) {
	t.Setenv("AVAIL_MAX_SYNC_AGE", "")
	got, err := tools.MaxCalendarSyncAge()
	if err != nil {
		t.Fatalf("MaxCalendarSyncAge() with the env unset: %v", err)
	}
	if got != time.Hour {
		t.Errorf("MaxCalendarSyncAge() = %v, want 1h — four missed */15 polls before the service goes quiet", got)
	}
}

func TestMaxCalendarSyncAge_ReadsAValidDuration(t *testing.T) {
	t.Setenv("AVAIL_MAX_SYNC_AGE", "90m")
	got, err := tools.MaxCalendarSyncAge()
	if err != nil {
		t.Fatalf("MaxCalendarSyncAge(): %v", err)
	}
	if got != 90*time.Minute {
		t.Errorf("MaxCalendarSyncAge() = %v, want 90m", got)
	}
}

// A typo must not silently WIDEN a safety window. Unchanged behaviour and
// unchanged error strings: audit_events.error stores a propose_slots refusal
// verbatim, and the funnel page prints the same failure.
func TestMaxCalendarSyncAge_RefusesRatherThanFallingBack(t *testing.T) {
	cases := []struct {
		value string
		want  string
	}{
		{"nonsense", "is not a Go duration"},
		{"60", "is not a Go duration"}, // bare seconds is not a Go duration
		{"0", "must be positive"},
		{"0s", "must be positive"},
		{"-5m", "must be positive"},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("AVAIL_MAX_SYNC_AGE", tc.value)
			got, err := tools.MaxCalendarSyncAge()
			if err == nil {
				t.Fatalf("MaxCalendarSyncAge() accepted %q and returned %v; an unparseable or non-positive "+
					"value is an ERROR returned to the caller, never a silent fallback to 1h", tc.value, got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error for %q = %q, want it to contain %q (the error strings are UNCHANGED by the "+
					"extraction — propose_slots' existing tests read them)", tc.value, err, tc.want)
			}
			if !strings.Contains(err.Error(), "AVAIL_MAX_SYNC_AGE") {
				t.Errorf("error for %q = %q; it must name the variable an operator has to fix", tc.value, err)
			}
		})
	}
}

// The extraction is a MOVE, not a copy. If availabilityConfig keeps its own
// os.Getenv + ParseDuration alongside the new function, propose_slots and
// /funnel are back to two parses and the seam bought nothing.
func TestProposeSlots_HasExactlyOneAvailMaxSyncAgeParse(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("proposeslots.go"))
	if err != nil {
		t.Fatalf("read proposeslots.go: %v", err)
	}
	src := string(b)

	if !strings.Contains(src, "func MaxCalendarSyncAge(") {
		t.Fatalf("internal/tools/proposeslots.go does not define MaxCalendarSyncAge. The SPEC names this " +
			"file as its home so availabilityConfig can call it unchanged")
	}
	if n := strings.Count(src, `os.Getenv("AVAIL_MAX_SYNC_AGE")`); n != 1 {
		t.Errorf("proposeslots.go reads AVAIL_MAX_SYNC_AGE from the environment %d times, want exactly 1 "+
			"(inside MaxCalendarSyncAge). availabilityConfig CALLS it — same behaviour, same error "+
			"strings, one parse", n)
	}
	if !strings.Contains(src, "MaxCalendarSyncAge()") {
		t.Errorf("nothing in proposeslots.go calls MaxCalendarSyncAge(); availabilityConfig must be the " +
			"first caller, or the extraction created a second spelling instead of removing one")
	}
}
