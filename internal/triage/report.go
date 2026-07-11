package triage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	fmt.Fprintln(w, strings.Join(lines, "\n"))
	return nil
}
