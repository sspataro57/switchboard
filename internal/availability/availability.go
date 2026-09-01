// Package availability is the deterministic free/busy service (SPEC
// 07-google-oauth-pollers, criterion 11): pure functions over calendar
// intervals — no LLM, no network, no clock reads. The one SQL read lives in
// store.go; everything here takes explicit inputs so tests never touch env.
package availability

import (
	"fmt"
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

// AccountState is one in-scope calendar account and the last time a calendar
// sync SUCCEEDED for it. Scope is source_accounts rows with provider='google'
// AND calendar_in_availability (SWT-24 criterion 3); the SQL that produces
// these lives in store.go (LoadAccountStates).
type AccountState struct {
	AccountID        int64
	Email            string
	LastCalendarSync time.Time // zero value => never synced successfully
}

// NotReady returns the in-scope accounts whose last successful calendar sync
// is missing or older than maxAge, in input order. Pure: no clock read, no env
// read, no pool (criterion 10) — the verdict follows the now ARGUMENT.
//
// The boundary is inclusive (finished_at >= now - maxAge is ready), and a
// future-dated sync (clock skew between us and Postgres) is ready, not an
// error: refusing there would turn a second of skew into an outage, and the
// direction of the skew says nothing about whether we hold data.
//
// An empty scope returns nothing — NotReady over no rows honestly has nothing
// to report. The scope-empty REFUSAL belongs to LoadBusy, the one caller that
// knows the difference between "no calendar is in scope" and "never asked".
func NotReady(states []AccountState, now time.Time, maxAge time.Duration) []AccountState {
	floor := now.Add(-maxAge)
	var out []AccountState
	for _, s := range states {
		if s.LastCalendarSync.IsZero() || s.LastCalendarSync.Before(floor) {
			out = append(out, s)
		}
	}
	return out
}

// NotReadyError is the typed, errors.As-able refusal (criterion 5). Its text
// names each offending account email and its last successful calendar sync, or
// the word "never" — audit_events.error stores this string verbatim, so it is
// the only thing an operator reading a refusal will ever see.
type NotReadyError struct {
	Accounts []AccountState // empty => the scope itself is empty (criterion 5b)
	MaxAge   time.Duration
}

func (e *NotReadyError) Error() string {
	if len(e.Accounts) == 0 {
		return "calendar not ready: no google calendar is in availability scope — " +
			"an empty scope is not an empty week (add an account, or run google-auth add-calendar)"
	}
	msg := "calendar not ready: "
	for i, a := range e.Accounts {
		if i > 0 {
			msg += "; "
		}
		if a.LastCalendarSync.IsZero() {
			msg += fmt.Sprintf("%s last successful calendar sync never", a.Email)
		} else {
			msg += fmt.Sprintf("%s last successful calendar sync %s (older than max sync age %s)",
				a.Email, a.LastCalendarSync.UTC().Format(time.RFC3339), e.MaxAge)
		}
	}
	return msg
}
