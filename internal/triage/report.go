package triage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Report renders the shadow-mode diff from ai_extractions alone: what WOULD
// have been created or attached. Deterministic — no LLM, no network beyond
// Postgres. Threshold buckets only; it enforces nothing this step.
func Report(ctx context.Context, pool *pgxpool.Pool, w io.Writer, threshold float64, since time.Duration) error {
	q := `SELECT e.fields, r.created_at
	      FROM ai_extractions e
	      JOIN ai_runs r ON r.id = e.ai_run_id AND r.worker_type='triage' AND r.status='ok'`
	args := []any{}
	if since > 0 {
		args = append(args, since.String())
		q += ` WHERE r.created_at >= now() - $1::interval`
	}
	q += ` ORDER BY r.created_at`

	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("select extractions: %w", err)
	}
	defer rows.Close()

	var total, actionable, wouldCreate, wouldAttach, belowThreshold, unmapped int
	var lines []string

	for rows.Next() {
		var raw []byte
		var createdAt time.Time
		if err := rows.Scan(&raw, &createdAt); err != nil {
			return fmt.Errorf("scan extraction: %w", err)
		}
		var f struct {
			Actionable     fieldVal `json:"actionable"`
			Kind           fieldVal `json:"kind"`
			Title          fieldVal `json:"title"`
			Priority       fieldVal `json:"priority"`
			AttachToTaskID fieldVal `json:"attach_to_task_id"`
			Verdict        string   `json:"verdict"`
			ProjectID      *int64   `json:"project_id"`
			PersonID       *int64   `json:"person_id"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			continue
		}
		total++

		minConf := f.Actionable.Confidence
		for _, c := range []float64{f.Kind.Confidence, f.Title.Confidence, f.AttachToTaskID.Confidence} {
			if c < minConf {
				minConf = c
			}
		}

		project := "UNMAPPED"
		if f.ProjectID != nil {
			project = fmt.Sprintf("project:%d", *f.ProjectID)
		} else {
			unmapped++
		}

		verdictNote := f.Verdict
		switch f.Verdict {
		case "create":
			actionable++
			if minConf < threshold {
				belowThreshold++
				verdictNote = "create→HUMAN-REVIEW"
			} else {
				wouldCreate++
			}
		case "attach":
			actionable++
			target := "?"
			if n, ok := f.AttachToTaskID.Value.(float64); ok {
				target = fmt.Sprintf("#%d", int64(n))
			}
			if minConf < threshold {
				belowThreshold++
				verdictNote = "attach " + target + "→HUMAN-REVIEW"
			} else {
				wouldAttach++
				verdictNote = "attach " + target
			}
		}

		title, _ := f.Title.Value.(string)
		lines = append(lines, fmt.Sprintf("%s  %-12s %-24s %-8s conf=%.2f  %s",
			createdAt.Format("2006-01-02 15:04"), project, verdictNote,
			fmt.Sprintf("%v", f.Kind.Value), minConf, title))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate extractions: %w", err)
	}

	fmt.Fprintf(w, "Triage shadow report (threshold %.2f)\n", threshold)
	fmt.Fprintf(w, "  processed: %d  actionable: %d  would-create: %d  would-attach: %d  human-review: %d  unmapped: %d\n\n",
		total, actionable, wouldCreate, wouldAttach, belowThreshold, unmapped)
	if err := reportSkipped(ctx, pool, w, since); err != nil {
		return err
	}
	fmt.Fprintln(w, strings.Join(lines, "\n"))
	return nil
}

// reportSkipped renders the lane the extraction join above CANNOT see (SWT-21
// criterion 20).
//
// The report joins `status='ok'` rows, and a refused message writes no
// extraction at all — that is what keeps "no permitted provider looked"
// structurally different from "the model looked and found nothing". The cost is
// that without this section a fully-skipped pass renders as `processed: 0` and
// is indistinguishable from a dead poller, a bad key, or an empty inbox. After
// this ticket and before SWT-22 that is EVERY pass, so the section is not a nice
// extra: it is the only place an operator learns the difference.
func reportSkipped(ctx context.Context, pool *pgxpool.Pool, w io.Writer, since time.Duration) error {
	q := `SELECT input FROM ai_runs WHERE worker_type='triage' AND status='skipped'`
	args := []any{}
	if since > 0 {
		args = append(args, since.String())
		q += ` AND created_at >= now() - $1::interval`
	}
	q += ` ORDER BY created_at`

	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("select skipped runs: %w", err)
	}
	defer rows.Close()

	byAvail := map[string]int{}
	byClass := map[string]int{}
	totalSkipped := 0
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return fmt.Errorf("scan skipped run: %w", err)
		}
		var rec struct {
			AvailReason  string         `json:"avail_reason"`
			AvailReasons map[string]int `json:"avail_reasons"`
			ClassReasons map[string]int `json:"class_reasons"`
			SkippedCount *int           `json:"skipped_count"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			continue
		}
		// A pass aggregate carries skipped_count; a per-message skip is one
		// message and carries none. Defaulting to 1 keeps the two row shapes
		// summable in the same column instead of needing two reports.
		n := 1
		if rec.SkippedCount != nil {
			n = *rec.SkippedCount
		}
		totalSkipped += n
		// Prefer the BREAKDOWN when the row carries one. A pass can refuse for
		// more than one reason — the probe TTL expiring mid-pass makes
		// `no_local_provider` and `local_unreachable` coexist — and
		// `avail_reason` is only the dominant one. Filing the whole count under
		// it would print a wrong number in the one place an operator looks, which
		// is worse than printing no breakdown at all.
		if len(rec.AvailReasons) > 0 {
			for k, v := range rec.AvailReasons {
				byAvail[k] += v
			}
		} else {
			// Older rows, and every per-message skip, carry a single reason.
			reason := rec.AvailReason
			if reason == "" {
				reason = "unrecorded"
			}
			byAvail[reason] += n
		}
		// class_reasons accumulates either way. An early `continue` above put it
		// on the breakdown branch only, which silently emptied the whole
		// "why it was restricted" section for exactly the rows that have one.
		for k, v := range rec.ClassReasons {
			byClass[k] += v
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate skipped runs: %w", err)
	}
	if totalSkipped == 0 {
		return nil
	}

	fmt.Fprintf(w, "  skipped: %d (never sent to any provider)\n", totalSkipped)
	// Sorted, because an operator compares two reports by eye and map order
	// would make an unchanged breakdown look different every run.
	for _, k := range sortedKeys(byAvail) {
		fmt.Fprintf(w, "    why the lane refused   %-28s %d\n", k, byAvail[k])
	}
	for _, k := range sortedKeys(byClass) {
		fmt.Fprintf(w, "    why it was restricted  %-28s %d\n", k, byClass[k])
	}
	// The half a counter cannot carry. Nothing in the numbers above distinguishes
	// "idle by design" from "broken", and the obvious fix a reader invents for
	// "broken" is a fallback to the hosted lane — the one change this ticket
	// exists to prevent.
	fmt.Fprintln(w, "    NOTE: an all-skipped pass is EXPECTED by design until a local model exists (SWT-22).")
	fmt.Fprintln(w, "          Personal content is only ever processed locally; falling back to a hosted")
	fmt.Fprintln(w, "          provider is never the fix. See docs/runbooks/provider-locality.md.")
	fmt.Fprintln(w)
	return nil
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
