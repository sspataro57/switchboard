package dashboard

// The /funnel page (SWT-29): the ingestion/classification funnel as one
// read-only screen — is each connector still syncing, is mail still arriving,
// how much of it do the capture rules claim, and what did the classifier say.
//
// A WINDOW, NOT A CONTROL. The page performs no tool calls and registers no
// POST route; if an action is ever wanted here it goes through s.execute(...)
// like /deliveries does, and criterion 2 gets renegotiated first. Every number
// is computed from canonical tables at request time (no cache, no rollup —
// invariant 2), and the calendar verdict is the system's OWN rule: the same
// availability.CalendarSyncStates rows, the same availability.NotReady
// predicate and the same tools.MaxCalendarSyncAge() value propose_slots uses,
// so this page can never say green while the tool refuses.
//
// Sections degrade independently (criterion 17): one failing query renders an
// inline error line naming its section while the other three still show.

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/sspataro57/switchboard/internal/availability"
	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/tools"
)

// funnelDisplayStaleAfter judges every NON-calendar phase (criterion 6). A
// package constant and not an env var on purpose: it gates nothing, and an env
// knob implies a contract. */15 CronJobs plus the resident mail watch loop make
// 3h about 12 missed passes. Calendar rows are judged by the real
// AVAIL_MAX_SYNC_AGE instead — see the health loader.
const funnelDisplayStaleAfter = 3 * time.Hour

// funnelFreshness classifies one (account, phase)'s last successful sync
// against an injected now (criterion 4, pure). "never" and "stale" are
// DIFFERENT answers: a connector that has never worked and one that stopped
// need different fixes. Inclusive at the floor, and a future-dated run is ok
// rather than an error — seconds of clock skew must not paint red rows on an
// ops page.
func funnelFreshness(last, now time.Time, max time.Duration) string {
	if last.IsZero() {
		return "never"
	}
	if now.Sub(last) <= max {
		return "ok"
	}
	return "stale"
}

// clampDays parses ?days= (criterion 7): default 14, clamped to [1,90] — an
// unbounded URL parameter must not schedule a full scan, and an unparseable
// one falls back rather than erroring the page.
func clampDays(raw string) int {
	if raw == "" {
		return 14
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 14
	}
	if n < 1 {
		return 1
	}
	if n > 90 {
		return 90
	}
	return n
}

// intakeDay is one day of the intake trend, bucketed on the PIPELINE clock
// (raw_source_items.ingested_at, normalized_messages.created_at — criterion 9).
type intakeDay struct {
	Day        time.Time
	PerAccount map[string]int
	RawTotal   int
	Messages   int
}

// fillDays renders a gap as a ZERO row (criterion 8): an absent row is exactly
// the signal the page exists to show, and a missing table row is not a signal
// anyone notices. Pure, with an injected now; newest first; rows outside the
// window are dropped.
func fillDays(rows []intakeDay, now time.Time, days int) []intakeDay {
	byDay := map[string]intakeDay{}
	for _, r := range rows {
		byDay[r.Day.Format("2006-01-02")] = r
	}
	out := make([]intakeDay, 0, days)
	for k := 0; k < days; k++ {
		d := now.AddDate(0, 0, -k)
		day := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, d.Location())
		if r, ok := byDay[day.Format("2006-01-02")]; ok {
			r.Day = day
			if r.PerAccount == nil {
				r.PerAccount = map[string]int{}
			}
			out = append(out, r)
			continue
		}
		out = append(out, intakeDay{Day: day, PerAccount: map[string]int{}})
	}
	return out
}

// funnelSection is one independently-degrading loader (criterion 17).
type funnelSection struct {
	Name string
	Load func() error
}

// runSections runs EVERY loader and returns one line per failure, each naming
// its section — an unnamed error on a four-section page tells the reader
// nothing about which number to distrust.
func runSections(sections []funnelSection) []string {
	var errs []string
	for _, s := range sections {
		if err := s.Load(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", s.Name, err))
		}
	}
	return errs
}

// funnelHealthRow is one (source_account, phase) of connector health.
type funnelHealthRow struct {
	Provider string
	Email    string
	Phase    string // "(none)" for runs with no phase key
	LastOK   string // formatted, or "" when never
	Age      string
	Runs     int // run count in the window; upwork counts two per invocation
	Verdict  string
}

// funnelLane is one classify lane's rendered block.
type funnelLane struct {
	Name    string
	Summary classify.Summary
}

type funnelPage struct {
	Days      int
	MaxAge    string // the effective AVAIL_MAX_SYNC_AGE, printed next to the section
	Errors    []string
	Health    []funnelHealthRow
	Intake    []intakeDay
	Accounts  []string // intake column order
	Capture   []capture.DayAttribution
	Lanes     []funnelLane
	Generated string
}

func (s *Server) showFunnel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	days := clampDays(r.URL.Query().Get("days"))
	page := funnelPage{Days: days, Generated: now.Format("2006-01-02 15:04:05")}

	page.Errors = runSections([]funnelSection{
		{Name: "connector health", Load: func() error {
			// The calendar seams first: the same value and predicate
			// propose_slots uses (criterion 5). An unparseable
			// AVAIL_MAX_SYNC_AGE fails THIS section loudly rather than
			// defaulting — a typo must not turn a refusing calendar green.
			maxAge, err := tools.MaxCalendarSyncAge()
			if err != nil {
				return err
			}
			page.MaxAge = maxAge.String()
			states, err := availability.CalendarSyncStates(ctx, s.pool)
			if err != nil {
				return fmt.Errorf("calendar sync states: %w", err)
			}
			inScope := map[string]bool{}
			for _, st := range states {
				inScope[st.Email] = true
			}
			calStale := map[string]bool{}
			for _, st := range availability.NotReady(states, now, maxAge) {
				calStale[st.Email] = true
			}

			rows, err := s.pool.Query(ctx, `
				SELECT a.provider, a.account_email,
				       COALESCE(p.phase, ''), p.last_ok, COALESCE(wnd.runs, 0)
				  FROM source_accounts a
				  LEFT JOIN (SELECT source_account_id, COALESCE(stats->>'phase','') AS phase,
				                    max(finished_at) FILTER (WHERE status='ok' AND finished_at IS NOT NULL) AS last_ok
				               FROM sync_runs GROUP BY 1,2) p ON p.source_account_id = a.id
				  LEFT JOIN (SELECT source_account_id, COALESCE(stats->>'phase','') AS phase, count(*) AS runs
				               FROM sync_runs
				              WHERE started_at >= now() - make_interval(days => $1)
				              GROUP BY 1,2) wnd ON wnd.source_account_id = a.id AND wnd.phase = p.phase
				 ORDER BY a.provider, a.account_email, p.phase NULLS FIRST`, days)
			if err != nil {
				return fmt.Errorf("health query: %w", err)
			}
			defer rows.Close()
			for rows.Next() {
				var h funnelHealthRow
				var phase *string
				var lastOK *time.Time
				if err := rows.Scan(&h.Provider, &h.Email, &phase, &lastOK, &h.Runs); err != nil {
					return fmt.Errorf("scan health row: %w", err)
				}
				name := "(none)"
				if phase != nil && *phase != "" {
					name = *phase
				}
				h.Phase = name
				last := time.Time{}
				if lastOK != nil {
					last = *lastOK
				}
				if !last.IsZero() {
					h.LastOK = last.Format("2006-01-02 15:04")
					h.Age = now.Sub(last).Truncate(time.Minute).String()
				}
				if name == "calendar" && inScope[h.Email] {
					// The system's own rule: NotReady membership, never a
					// second spelling of the freshness predicate.
					switch {
					case last.IsZero():
						h.Verdict = "never"
					case calStale[h.Email]:
						h.Verdict = "stale"
					default:
						h.Verdict = "ok"
					}
				} else {
					h.Verdict = funnelFreshness(last, now, funnelDisplayStaleAfter)
				}
				page.Health = append(page.Health, h)
			}
			return rows.Err()
		}},
		{Name: "intake trend", Load: func() error {
			byDay := map[string]*intakeDay{}
			get := func(day time.Time) *intakeDay {
				key := day.Format("2006-01-02")
				if d, ok := byDay[key]; ok {
					return d
				}
				d := &intakeDay{Day: day, PerAccount: map[string]int{}}
				byDay[key] = d
				return d
			}
			rows, err := s.pool.Query(ctx, `
				SELECT r.ingested_at::date, a.account_email, count(*)
				  FROM raw_source_items r JOIN source_accounts a ON a.id = r.source_account_id
				 WHERE r.ingested_at >= now() - make_interval(days => $1)
				 GROUP BY 1, 2`, days)
			if err != nil {
				return fmt.Errorf("intake raw query: %w", err)
			}

			for rows.Next() {
				var day time.Time
				var email string
				var n int
				if err := rows.Scan(&day, &email, &n); err != nil {
					rows.Close()
					return fmt.Errorf("scan intake raw day: %w", err)
				}
				d := get(day)
				d.PerAccount[email] += n
				d.RawTotal += n

			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return fmt.Errorf("iterate intake raw days: %w", err)
			}
			msgRows, err := s.pool.Query(ctx, `
				SELECT created_at::date, count(*)
				  FROM normalized_messages
				 WHERE created_at >= now() - make_interval(days => $1)
				 GROUP BY 1`, days)
			if err != nil {
				return fmt.Errorf("intake message query: %w", err)
			}
			for msgRows.Next() {
				var day time.Time
				var n int
				if err := msgRows.Scan(&day, &n); err != nil {
					msgRows.Close()
					return fmt.Errorf("scan intake message day: %w", err)
				}
				get(day).Messages += n
			}
			msgRows.Close()
			if err := msgRows.Err(); err != nil {
				return fmt.Errorf("iterate intake message days: %w", err)
			}
			flat := make([]intakeDay, 0, len(byDay))
			for _, d := range byDay {
				flat = append(flat, *d)
			}
			page.Intake = fillDays(flat, now, days)
			// EVERY account gets a column (criterion 7), not only the ones that
			// ingested something in the window: a connector that went quiet
			// renders a column of zeros, for the same reason fillDays renders
			// empty days — absence is the signal (go-reviewer finding 3).
			acctRows, err := s.pool.Query(ctx,
				`SELECT account_email FROM source_accounts ORDER BY provider, account_email`)
			if err != nil {
				return fmt.Errorf("list intake accounts: %w", err)
			}
			for acctRows.Next() {
				var email string
				if err := acctRows.Scan(&email); err != nil {
					acctRows.Close()
					return fmt.Errorf("scan intake account: %w", err)
				}
				page.Accounts = append(page.Accounts, email)
			}
			acctRows.Close()
			return acctRows.Err()
		}},
		{Name: "capture attribution", Load: func() error {
			// The seams wrap their own errors; runSections adds the section name.
			trend, err := capture.AttributionTrend(ctx, s.pool, days)
			if err != nil {
				return err
			}
			page.Capture = trend
			return nil
		}},
		{Name: "classify shadow summary", Load: func() error {
			since := time.Duration(days) * 24 * time.Hour
			for _, lane := range []classify.Lane{classify.LanePersonal, classify.LaneResidue} {
				sum, err := classify.Summarize(ctx, s.pool, since, lane.WorkerType)
				if err != nil {
					return err
				}
				page.Lanes = append(page.Lanes, funnelLane{Name: lane.Name, Summary: sum})
			}
			return nil
		}},
	})

	if err := s.tmpl.ExecuteTemplate(w, "funnel.html", page); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
