// Package availability is the deterministic free/busy service (SPEC
// 07-google-oauth-pollers, criterion 11): pure functions over calendar
// intervals — no LLM, no network, no clock reads. The one SQL read lives in
// store.go; everything here takes explicit inputs so tests never touch env.
package availability

import (
	"sort"
	"time"
)

// Interval is one busy span.
type Interval struct {
	Start, End time.Time
}

// Slot is one proposed free span.
type Slot struct {
	Start, End time.Time
}

// Event is one normalized calendar row projected for availability.
type Event struct {
	Start, End     time.Time
	Status         string
	Transparency   string
	InAvailability bool
}

// Busy applies the criterion-11 filter purely: an event contributes to busy
// iff it is on an in-availability account, not cancelled, and not transparent
// (Google all-day events default transparent, so they fall out naturally).
func Busy(events []Event) []Interval {
	var out []Interval
	for _, e := range events {
		if !e.InAvailability || e.Status == "cancelled" || e.Transparency == "transparent" {
			continue
		}
		if !e.End.After(e.Start) {
			continue
		}
		out = append(out, Interval{Start: e.Start, End: e.End})
	}
	return out
}

// Merge sorts by start and coalesces overlapping (or touching) intervals.
func Merge(busy []Interval) []Interval {
	if len(busy) == 0 {
		return nil
	}
	sorted := make([]Interval, len(busy))
	copy(sorted, busy)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start.Before(sorted[j].Start) })

	out := []Interval{sorted[0]}
	for _, iv := range sorted[1:] {
		last := &out[len(out)-1]
		if !iv.Start.After(last.End) {
			if iv.End.After(last.End) {
				last.End = iv.End
			}
			continue
		}
		out = append(out, iv)
	}
	return out
}

// Config is the ProposeSlots search space.
type Config struct {
	WorkStart, WorkEnd int // hours [0..24) in Location
	Days               []time.Weekday
	Location           *time.Location
	Duration           time.Duration
	WindowStart        time.Time
	WindowEnd          time.Time
	Count              int
}

const slotAlign = 30 * time.Minute

// ProposeSlots returns earliest-first, 30-min-aligned, exactly-Duration slots
// inside working hours and the window, not overlapping busy, up to Count.
// Deterministic: same input ⇒ same output.
func ProposeSlots(busy []Interval, cfg Config) []Slot {
	if cfg.Duration <= 0 || cfg.Count <= 0 || !cfg.WindowEnd.After(cfg.WindowStart) {
		return nil
	}
	loc := cfg.Location
	if loc == nil {
		loc = time.UTC
	}
	days := map[time.Weekday]bool{}
	for _, d := range cfg.Days {
		days[d] = true
	}
	merged := Merge(busy)

	var out []Slot
	cur := alignUp(cfg.WindowStart.In(loc))
	for cur.Before(cfg.WindowEnd) && len(out) < cfg.Count {
		end := cur.Add(cfg.Duration)
		if end.After(cfg.WindowEnd) {
			break
		}
		if !days[cur.Weekday()] || !insideWorkHours(cur, end, cfg.WorkStart, cfg.WorkEnd) {
			cur = cur.Add(slotAlign)
			continue
		}
		if overlapsAny(merged, cur, end) {
			cur = cur.Add(slotAlign)
			continue
		}
		out = append(out, Slot{Start: cur, End: end})
		cur = end // non-overlapping proposals
	}
	return out
}

func alignUp(t time.Time) time.Time {
	aligned := t.Truncate(slotAlign)
	if aligned.Before(t) {
		aligned = aligned.Add(slotAlign)
	}
	return aligned
}

// insideWorkHours requires the whole slot inside [WorkStart, WorkEnd) on the
// same day, in the slot's own location.
func insideWorkHours(start, end time.Time, workStart, workEnd int) bool {
	dayStart := time.Date(start.Year(), start.Month(), start.Day(), workStart, 0, 0, 0, start.Location())
	dayEnd := time.Date(start.Year(), start.Month(), start.Day(), workEnd, 0, 0, 0, start.Location())
	return !start.Before(dayStart) && !end.After(dayEnd)
}

func overlapsAny(busy []Interval, start, end time.Time) bool {
	for _, b := range busy {
		if start.Before(b.End) && b.Start.Before(end) {
			return true
		}
	}
	return false
}
