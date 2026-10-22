package tools

// calendar_list_events — the read-only agenda (SWT-109 criteria 1-6). Every
// session's answer to "what's on my calendar": the synced Google events of
// every in-scope account plus switchboard's own live reservations, in AVAIL_TZ.
//
// It FAILS CLOSED exactly like propose_slots: availability.LoadAgenda runs the
// same shared readiness check as LoadBusy before it reads an event, and its
// refusal propagates verbatim. There is no override and no "flagged stale"
// mode — offering times from a flagged stale copy is precisely how delivery
// 236 offered a slot over a live standup (2026-10-21).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/availability"
	"github.com/sspataro57/switchboard/internal/connector/google"
)

const (
	agendaMaxWindow  = 31 * 24 * time.Hour
	agendaDefaultLen = 7 * 24 * time.Hour
	agendaMaxEvents  = 300
)

type calendarListEventsArgs struct {
	WindowStart string `json:"window_start,omitempty"`
	WindowEnd   string `json:"window_end,omitempty"`
}

// parseAgendaBound reads RFC3339, or a bare YYYY-MM-DD as local midnight in loc.
func parseAgendaBound(v string, loc *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	t, err := time.ParseInLocation("2006-01-02", v, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is neither RFC3339 nor YYYY-MM-DD", v)
	}
	return t, nil
}

func validateCalendarListEvents(args []byte) error {
	var a calendarListEventsArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	for name, v := range map[string]string{"window_start": a.WindowStart, "window_end": a.WindowEnd} {
		if v == "" {
			continue
		}
		if _, err := parseAgendaBound(v, time.UTC); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// agendaWindow resolves the window (criterion 1): default today 00:00 in loc
// for seven days; an empty, inverted or longer-than-31-day window refuses.
func agendaWindow(a calendarListEventsArgs, now time.Time, loc *time.Location) (time.Time, time.Time, error) {
	local := now.In(loc)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	if a.WindowStart != "" {
		t, err := parseAgendaBound(a.WindowStart, loc)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("window_start: %w", err)
		}
		start = t
	}
	end := start.Add(agendaDefaultLen)
	if a.WindowEnd != "" {
		t, err := parseAgendaBound(a.WindowEnd, loc)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("window_end: %w", err)
		}
		end = t
	}
	if !end.After(start) {
		return time.Time{}, time.Time{}, errors.New("window_end must be after window_start (an empty or inverted window has no events to answer for)")
	}
	if end.Sub(start) > agendaMaxWindow {
		return time.Time{}, time.Time{}, fmt.Errorf("window %s is longer than 31 days; ask for a shorter span", end.Sub(start))
	}
	return start, end, nil
}

// agendaReservationInfo is a reservation's switchboard-side facts, for the
// renderer. confirmed reservations come from a separate lookup by event id.
type agendaReservationInfo struct {
	availability.Reservation
	Confirmed bool
}

// reservationState names where a reservation is (criterion 4): pending until
// a push may have reached Google, pushed until a poll observes it, confirmed
// after.
func reservationState(r agendaReservationInfo) string {
	switch {
	case r.Confirmed:
		return "confirmed"
	case r.ExternalID != "":
		return "pushed"
	default:
		return "pending"
	}
}

// renderAgenda is the pure half (criteria 4 and 5): every timestamp in loc,
// all-day events as plain dates, at most 300 events ordered by start, and a
// reservation observed on Google shown ONCE, as the Google event carrying its
// reservation object.
func renderAgenda(ag availability.Agenda, confirmed map[string]agendaReservationInfo,
	loc *time.Location, windowStart, windowEnd, now time.Time, maxAge time.Duration, frozen bool) map[string]any {

	type row struct {
		start time.Time
		id    int64
		m     map[string]any
	}
	var rows []row

	resObj := func(r agendaReservationInfo) map[string]any {
		kind := "booking"
		if r.HoldFor != 0 {
			kind = "hold"
		}
		obj := map[string]any{
			"delivery_id": r.DeliveryID,
			"task_id":     r.TaskID,
			"label":       r.Label,
			"kind":        kind,
			"state":       reservationState(r),
			"verified":    r.Verified,
		}
		reason := r.PendingReason
		if reservationState(r) == "pending" && reason == "" && frozen {
			reason = "sending_frozen"
		}
		if reason != "" && reservationState(r) != "confirmed" {
			obj["pending_reason"] = reason
		}
		if kind == "hold" {
			h := map[string]any{"offer_delivery_id": r.HoldFor, "kept": r.Kept}
			if r.HoldExpiresAt != nil && !r.Kept {
				h["expires_at"] = r.HoldExpiresAt.In(loc).Format(time.RFC3339)
			}
			obj["hold"] = h
		}
		return obj
	}

	for _, e := range ag.Events {
		m := map[string]any{
			"title":     e.Title,
			"all_day":   e.AllDay,
			"account":   e.Account,
			"busy":      e.Transparency != "transparent",
			"on_google": true,
		}
		if e.AllDay {
			// All-day dates are stored as UTC midnight (normalize.go): the DATE
			// is the fact, so it is printed from UTC and never shifted by loc.
			m["start"] = e.Start.UTC().Format("2006-01-02")
			m["end"] = e.End.UTC().Format("2006-01-02")
		} else {
			m["start"] = e.Start.In(loc).Format(time.RFC3339)
			m["end"] = e.End.In(loc).Format(time.RFC3339)
		}
		if r, ok := confirmed[e.ExternalID]; ok {
			m["reservation"] = resObj(r)
		}
		rows = append(rows, row{start: e.Start, m: m})
	}
	onGoogle := map[string]bool{}
	for _, e := range ag.Events {
		onGoogle[e.ExternalID] = true
	}
	for _, res := range ag.Reservations {
		if res.ExternalID != "" && onGoogle[res.ExternalID] {
			continue // already shown as its Google event, with this reservation attached
		}
		r := agendaReservationInfo{Reservation: res}
		rows = append(rows, row{start: r.Start, id: r.DeliveryID, m: map[string]any{
			"title":       r.Label,
			"start":       r.Start.In(loc).Format(time.RFC3339),
			"end":         r.End.In(loc).Format(time.RFC3339),
			"all_day":     false,
			"account":     r.Account,
			"busy":        true,
			"on_google":   false,
			"reservation": resObj(r),
		}})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].start.Equal(rows[j].start) {
			return rows[i].start.Before(rows[j].start)
		}
		return rows[i].id < rows[j].id
	})
	truncated := false
	if len(rows) > agendaMaxEvents || len(ag.Events) >= availability.AgendaEventLimit {
		truncated = true
	}
	if len(rows) > agendaMaxEvents {
		rows = rows[:agendaMaxEvents]
	}
	events := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		events = append(events, r.m)
	}

	accounts := make([]map[string]any, 0, len(ag.Accounts))
	for _, a := range ag.Accounts {
		acct := map[string]any{"email": a.Email, "write_enabled": a.WriteEnabled}
		if a.LastCalendarSync.IsZero() {
			acct["last_sync"] = nil
		} else {
			acct["last_sync"] = a.LastCalendarSync.In(loc).Format(time.RFC3339)
			acct["age"] = now.Sub(a.LastCalendarSync).Round(time.Minute).String()
		}
		accounts = append(accounts, acct)
	}

	return map[string]any{
		"timezone":     loc.String(),
		"window":       map[string]string{"start": windowStart.In(loc).Format(time.RFC3339), "end": windowEnd.In(loc).Format(time.RFC3339)},
		"max_sync_age": maxAge.String(),
		"accounts":     accounts,
		"events":       events,
		"truncated":    truncated,
	}
}

func calendarListEvents(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a calendarListEventsArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}
	cfg, maxSyncAge, err := availabilityConfig()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	start, end, err := agendaWindow(a, now, cfg.Location)
	if err != nil {
		return nil, err
	}
	ag, err := availability.LoadAgenda(ctx, pool, availability.Request{
		WindowStart:   start,
		WindowEnd:     end,
		Now:           now,
		MaxSyncAge:    maxSyncAge,
		HorizonPast:   google.CalendarWindowPast,
		HorizonFuture: google.CalendarWindowFuture,
	})
	if err != nil {
		return nil, err // VERBATIM: it must read exactly like a propose_slots refusal
	}

	confirmed, err := confirmedReservationsFor(ctx, pool, ag.Events)
	if err != nil {
		return nil, err
	}
	frozen, err := sendingFrozen(ctx, pool)
	if err != nil {
		return nil, err
	}
	return marshalResult(renderAgenda(ag, confirmed, cfg.Location, start, end, now, maxSyncAge, frozen))
}

// confirmedReservationsFor maps a Google event's external id to the live
// reservation it is, so the agenda shows that reservation once, as the Google
// event (criterion 4) — pushed rows too: the push records the event into the
// busy set at once, long before a poll confirms it. deliveries only.
func confirmedReservationsFor(ctx context.Context, pool *pgxpool.Pool, events []availability.AgendaEvent) (map[string]agendaReservationInfo, error) {
	var ids []string
	for _, e := range events {
		if strings.HasPrefix(e.ExternalID, "calendar:sb") {
			ids = append(ids, e.ExternalID)
		}
	}
	out := map[string]agendaReservationInfo{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := pool.Query(ctx,
		`SELECT d.id, d.task_id, d.starts_at, d.ends_at, d.status, COALESCE(d.reservation_label,''),
		        d.sent_external_id, COALESCE(d.booking_verified, true),
		        COALESCE(d.hold_for_delivery_id, 0), d.hold_expires_at, d.hold_kept_at IS NOT NULL,
		        d.confirmed_at IS NOT NULL
		   FROM deliveries d
		  WHERE d.channel = 'calendar'
		    AND d.released_at IS NULL AND d.sent_external_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("select confirmed reservations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r agendaReservationInfo
		if err := rows.Scan(&r.DeliveryID, &r.TaskID, &r.Start, &r.End, &r.Status, &r.Label,
			&r.ExternalID, &r.Verified, &r.HoldFor, &r.HoldExpiresAt, &r.Kept, &r.Confirmed); err != nil {
			return nil, fmt.Errorf("scan confirmed reservation: %w", err)
		}
		out[r.ExternalID] = r
	}
	return out, rows.Err()
}

// sendingFrozen reads the kill switch the way the policy loader does: an
// absent row or NULL means not frozen.
func sendingFrozen(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var frozen *bool
	err := pool.QueryRow(ctx,
		`SELECT (value->>'frozen')::boolean FROM ops_flags WHERE name='sending_frozen'`).Scan(&frozen)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("read kill switch: %w", err)
	}
	return frozen != nil && *frozen, nil
}
