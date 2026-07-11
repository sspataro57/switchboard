package availability_test

// Unit tests for the deterministic availability service (SPEC
// 07-google-oauth-pollers, acceptance criterion 11; invariant 7 discipline: the
// whole package is PURE — no LLM, no network, no Postgres, CLAUDE.md pins
// propose_slots as deterministic). The one SQL read (busy intervals from
// normalized_events ⋈ source_accounts with the criterion-11 filter) lives in
// store.go and is exercised by the connector integration suite; here every input
// is an in-memory slice.
//
// GREENFIELD NOTE: package internal/availability does not exist yet; this file
// compile-FAILs under `go test ./...` until it is implemented — the expected
// failure mode. For greenfield code the SPEC's contract IS the signature; the
// imposed exported surface (the SPEC's availability.go) is:
//
//   type Interval struct { Start, End time.Time }
//   type Slot     struct { Start, End time.Time }
//
//   // Event is one normalized calendar row projected for availability. Busy
//   // applies the criterion-11 filter purely (no SQL): an event contributes to
//   // busy iff InAvailability (account calendar_in_availability=true) AND
//   // Status != "cancelled" AND Transparency != "transparent" (Google all-day
//   // events default transparent, so they fall out here without a special case).
//   type Event struct {
//       Start, End     time.Time
//       Status         string
//       Transparency   string
//       InAvailability bool
//   }
//   func Busy(events []Event) []Interval
//
//   // Merge sorts by start and coalesces overlapping intervals.
//   func Merge(busy []Interval) []Interval
//
//   // Config is the ProposeSlots search space. Days is the set of working
//   // weekdays; WorkStart/WorkEnd are hours [0..24) in Location; Duration is the
//   // exact slot length; Window bounds the search; Count caps the result.
//   type Config struct {
//       WorkStart, WorkEnd int
//       Days               []time.Weekday
//       Location           *time.Location
//       Duration           time.Duration
//       WindowStart        time.Time
//       WindowEnd          time.Time
//       Count              int
//   }
//   // ProposeSlots returns earliest-first, 30-min-aligned, exactly-Duration
//   // slots inside working hours and the window, not overlapping busy, up to
//   // Count. Deterministic: same input => same output.
//   func ProposeSlots(busy []Interval, cfg Config) []Slot

import (
	"reflect"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/availability"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return tm
}

func iv(t *testing.T, start, end string) availability.Interval {
	return availability.Interval{Start: mustTime(t, start), End: mustTime(t, end)}
}

// defaultCfg is Mon-Fri 09:00-18:00 in UTC (tests use UTC so wall-clock math is
// obvious), 30-minute duration, count 3, over a window the test sets.
func defaultCfg(t *testing.T, windowStart, windowEnd string) availability.Config {
	return availability.Config{
		WorkStart:   9,
		WorkEnd:     18,
		Days:        []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday},
		Location:    time.UTC,
		Duration:    30 * time.Minute,
		WindowStart: mustTime(t, windowStart),
		WindowEnd:   mustTime(t, windowEnd),
		Count:       3,
	}
}

// ---- Busy filter (criterion 11) ------------------------------------------

// Busy keeps only opaque, non-cancelled events on availability accounts.
func TestBusy_FilterRules(t *testing.T) {
	events := []availability.Event{
		// kept: opaque, confirmed, in availability
		{Start: mustTime(t, "2026-07-13T09:00:00Z"), End: mustTime(t, "2026-07-13T10:00:00Z"), Status: "confirmed", Transparency: "opaque", InAvailability: true},
		// dropped: cancelled
		{Start: mustTime(t, "2026-07-13T11:00:00Z"), End: mustTime(t, "2026-07-13T12:00:00Z"), Status: "cancelled", Transparency: "opaque", InAvailability: true},
		// dropped: transparent (also the all-day case, which Google marks transparent)
		{Start: mustTime(t, "2026-07-13T13:00:00Z"), End: mustTime(t, "2026-07-13T14:00:00Z"), Status: "confirmed", Transparency: "transparent", InAvailability: true},
		// dropped: account not in availability
		{Start: mustTime(t, "2026-07-13T15:00:00Z"), End: mustTime(t, "2026-07-13T16:00:00Z"), Status: "confirmed", Transparency: "opaque", InAvailability: false},
	}
	got := availability.Busy(events)
	want := []availability.Interval{iv(t, "2026-07-13T09:00:00Z", "2026-07-13T10:00:00Z")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Busy = %+v, want only the opaque/confirmed/in-availability event %+v", got, want)
	}
}

// Cross-account: only calendar_in_availability accounts contribute, expressed as
// pure input filtering on the InAvailability flag.
func TestBusy_OnlyAvailabilityAccounts(t *testing.T) {
	events := []availability.Event{
		{Start: mustTime(t, "2026-07-13T09:00:00Z"), End: mustTime(t, "2026-07-13T10:00:00Z"), Status: "confirmed", Transparency: "opaque", InAvailability: true},
		{Start: mustTime(t, "2026-07-13T09:30:00Z"), End: mustTime(t, "2026-07-13T10:30:00Z"), Status: "confirmed", Transparency: "opaque", InAvailability: false},
	}
	got := availability.Busy(events)
	if len(got) != 1 {
		t.Fatalf("Busy returned %d intervals, want 1 (only the in-availability account)", len(got))
	}
	if !got[0].Start.Equal(mustTime(t, "2026-07-13T09:00:00Z")) {
		t.Errorf("Busy kept the wrong event: %+v", got[0])
	}
}

// ---- Merge (criterion 11) ------------------------------------------------

// Overlapping intervals coalesce; a gap stays separate; input order does not
// matter. (Inputs are chosen with a clean gap to avoid adjacency ambiguity —
// whether touching intervals coalesce is left to the implementation.)
func TestMerge_CoalescesOverlaps(t *testing.T) {
	busy := []availability.Interval{
		iv(t, "2026-07-13T11:00:00Z", "2026-07-13T12:00:00Z"), // separate (gap after 10:30)
		iv(t, "2026-07-13T09:00:00Z", "2026-07-13T10:00:00Z"),
		iv(t, "2026-07-13T09:30:00Z", "2026-07-13T10:30:00Z"), // overlaps the 09:00-10:00
	}
	got := availability.Merge(busy)
	want := []availability.Interval{
		iv(t, "2026-07-13T09:00:00Z", "2026-07-13T10:30:00Z"),
		iv(t, "2026-07-13T11:00:00Z", "2026-07-13T12:00:00Z"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Merge = %+v, want %+v (overlap coalesced, gap preserved, sorted)", got, want)
	}
}

func TestMerge_Empty(t *testing.T) {
	if got := availability.Merge(nil); len(got) != 0 {
		t.Errorf("Merge(nil) = %+v, want empty", got)
	}
}

// ---- ProposeSlots (criterion 11) -----------------------------------------

// With no busy blocks the first three 30-min slots of a working day start at
// 09:00, 09:30, 10:00 — earliest-first, 30-min aligned, exactly Duration long.
func TestProposeSlots_EarliestAlignedNoBusy(t *testing.T) {
	// 2026-07-13 is a Monday.
	cfg := defaultCfg(t, "2026-07-13T00:00:00Z", "2026-07-14T00:00:00Z")
	got := availability.ProposeSlots(nil, cfg)
	want := []availability.Slot{
		{Start: mustTime(t, "2026-07-13T09:00:00Z"), End: mustTime(t, "2026-07-13T09:30:00Z")},
		{Start: mustTime(t, "2026-07-13T09:30:00Z"), End: mustTime(t, "2026-07-13T10:00:00Z")},
		{Start: mustTime(t, "2026-07-13T10:00:00Z"), End: mustTime(t, "2026-07-13T10:30:00Z")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ProposeSlots = %+v, want the first three aligned slots %+v", got, want)
	}
}

// Busy blocks are dodged: a meeting 09:00-10:00 pushes the first slot to 10:00.
func TestProposeSlots_SkipsBusy(t *testing.T) {
	cfg := defaultCfg(t, "2026-07-13T00:00:00Z", "2026-07-14T00:00:00Z")
	busy := []availability.Interval{iv(t, "2026-07-13T09:00:00Z", "2026-07-13T10:00:00Z")}
	got := availability.ProposeSlots(busy, cfg)
	if len(got) == 0 {
		t.Fatalf("ProposeSlots returned no slots")
	}
	if !got[0].Start.Equal(mustTime(t, "2026-07-13T10:00:00Z")) {
		t.Errorf("first slot start = %v, want 10:00 (09:00-10:00 is busy)", got[0].Start)
	}
	for _, s := range got {
		if s.Start.Before(mustTime(t, "2026-07-13T10:00:00Z")) {
			t.Errorf("slot %v overlaps the busy block", s)
		}
	}
}

// Working-hours clipping: a duration that does not fit before WorkEnd is not
// proposed after the boundary; every slot stays inside 09:00-18:00.
func TestProposeSlots_WorkingHoursClip(t *testing.T) {
	cfg := defaultCfg(t, "2026-07-13T00:00:00Z", "2026-07-14T00:00:00Z")
	cfg.Duration = 60 * time.Minute
	cfg.Count = 100 // ask for more than the day can hold
	got := availability.ProposeSlots(nil, cfg)
	if len(got) == 0 {
		t.Fatalf("ProposeSlots returned no slots")
	}
	end18 := mustTime(t, "2026-07-13T18:00:00Z")
	start9 := mustTime(t, "2026-07-13T09:00:00Z")
	for _, s := range got {
		if s.End.After(end18) {
			t.Errorf("slot %v ends after 18:00 working boundary", s)
		}
		if s.Start.Before(start9) {
			t.Errorf("slot %v starts before 09:00 working boundary", s)
		}
	}
}

// Weekend skip: a window entirely on Saturday/Sunday yields no slots.
func TestProposeSlots_WeekendSkip(t *testing.T) {
	// 2026-07-18 is a Saturday, 2026-07-19 a Sunday.
	cfg := defaultCfg(t, "2026-07-18T00:00:00Z", "2026-07-20T00:00:00Z")
	if got := availability.ProposeSlots(nil, cfg); len(got) != 0 {
		t.Errorf("ProposeSlots over a weekend = %+v, want none (Mon-Fri only)", got)
	}
}

// Empty when there is no room: a fully-busy working day yields no slots.
func TestProposeSlots_EmptyWhenNoRoom(t *testing.T) {
	cfg := defaultCfg(t, "2026-07-13T00:00:00Z", "2026-07-14T00:00:00Z")
	busy := []availability.Interval{iv(t, "2026-07-13T09:00:00Z", "2026-07-13T18:00:00Z")}
	if got := availability.ProposeSlots(busy, cfg); len(got) != 0 {
		t.Errorf("ProposeSlots on a fully-busy day = %+v, want none", got)
	}
}

// Results are chronological and never exceed Count.
func TestProposeSlots_ChronologicalAndCapped(t *testing.T) {
	cfg := defaultCfg(t, "2026-07-13T00:00:00Z", "2026-07-18T00:00:00Z")
	cfg.Count = 4
	got := availability.ProposeSlots(nil, cfg)
	if len(got) > 4 {
		t.Fatalf("ProposeSlots returned %d slots, want <= Count (4)", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Start.Before(got[i-1].Start) {
			t.Errorf("slots not chronological at %d: %v before %v", i, got[i].Start, got[i-1].Start)
		}
	}
}

// Determinism: same input => byte-identical output.
func TestProposeSlots_Deterministic(t *testing.T) {
	cfg := defaultCfg(t, "2026-07-13T00:00:00Z", "2026-07-16T00:00:00Z")
	busy := []availability.Interval{
		iv(t, "2026-07-13T10:00:00Z", "2026-07-13T11:00:00Z"),
		iv(t, "2026-07-14T09:00:00Z", "2026-07-14T09:30:00Z"),
	}
	a := availability.ProposeSlots(busy, cfg)
	b := availability.ProposeSlots(busy, cfg)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("ProposeSlots not deterministic:\n a=%+v\n b=%+v", a, b)
	}
}
