package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/availability"
)

// propose_slots — the deterministic availability tool (SPEC
// 07-google-oauth-pollers, criterion 12). Read-only; the policy matrix pins
// calendar blocks as "always via availability service propose_slots", so step
// 8's write path consumes this existing audited surface.

type proposeSlotsArgs struct {
	DurationMinutes int    `json:"duration_minutes"`
	WindowStart     string `json:"window_start,omitempty"`
	WindowEnd       string `json:"window_end,omitempty"`
	Count           int    `json:"count,omitempty"`
}

func validateProposeSlots(args []byte) error {
	var a proposeSlotsArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.DurationMinutes <= 0 {
		return errors.New("missing or non-positive duration_minutes")
	}
	if a.WindowStart != "" {
		if _, err := time.Parse(time.RFC3339, a.WindowStart); err != nil {
			return fmt.Errorf("window_start: %w", err)
		}
	}
	if a.WindowEnd != "" {
		if _, err := time.Parse(time.RFC3339, a.WindowEnd); err != nil {
			return fmt.Errorf("window_end: %w", err)
		}
	}
	if a.WindowStart != "" && a.WindowEnd != "" {
		ws, _ := time.Parse(time.RFC3339, a.WindowStart)
		we, _ := time.Parse(time.RFC3339, a.WindowEnd)
		if !we.After(ws) {
			return errors.New("window_end must be after window_start")
		}
	}
	return nil
}

func proposeSlots(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a proposeSlotsArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	cfg := availabilityConfig()
	cfg.Duration = time.Duration(a.DurationMinutes) * time.Minute
	cfg.Count = a.Count
	if cfg.Count <= 0 {
		cfg.Count = 3
	}

	now := time.Now().In(cfg.Location)
	if a.WindowStart != "" {
		ws, _ := time.Parse(time.RFC3339, a.WindowStart)
		cfg.WindowStart = ws
	} else {
		cfg.WindowStart = now
	}
	if a.WindowEnd != "" {
		we, _ := time.Parse(time.RFC3339, a.WindowEnd)
		cfg.WindowEnd = we
	} else {
		cfg.WindowEnd = cfg.WindowStart.AddDate(0, 0, 7) // ~next 5 business days
	}

	events, err := availability.LoadEvents(ctx, pool, cfg.WindowStart, cfg.WindowEnd)
	if err != nil {
		return nil, err
	}
	slots := availability.ProposeSlots(availability.Busy(events), cfg)

	out := make([]map[string]string, 0, len(slots))
	for _, s := range slots {
		out = append(out, map[string]string{
			"start": s.Start.Format(time.RFC3339),
			"end":   s.End.Format(time.RFC3339),
		})
	}
	return marshalResult(map[string]any{"slots": out})
}

// availabilityConfig reads the working-hours env (defaults: Mon-Fri 09-18,
// Europe/Rome). The pure functions never read env — only this wiring does.
func availabilityConfig() availability.Config {
	cfg := availability.Config{WorkStart: 9, WorkEnd: 18,
		Days: []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}}

	tz := os.Getenv("AVAIL_TZ")
	if tz == "" {
		tz = "Europe/Rome"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	cfg.Location = loc

	if v := os.Getenv("AVAIL_WORK_START"); v != "" {
		fmt.Sscanf(v, "%d", &cfg.WorkStart)
	}
	if v := os.Getenv("AVAIL_WORK_END"); v != "" {
		fmt.Sscanf(v, "%d", &cfg.WorkEnd)
	}
	if v := os.Getenv("AVAIL_WORK_DAYS"); v != "" {
		names := map[string]time.Weekday{"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday,
			"wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday}
		var days []time.Weekday
		for _, part := range strings.Split(v, ",") {
			if d, ok := names[strings.ToLower(strings.TrimSpace(part))]; ok {
				days = append(days, d)
			}
		}
		if len(days) > 0 {
			cfg.Days = days
		}
	}
	return cfg
}
