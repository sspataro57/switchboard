package classify

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

// Report renders the shadow-mode verdicts from ai_extractions alone: what the
// classifier flagged and what it skipped. Deterministic — no LLM, no network
// beyond Postgres.
func Report(ctx context.Context, pool *pgxpool.Pool, w io.Writer, since time.Duration) error {
	q := `SELECT e.fields, r.created_at
	      FROM ai_extractions e
	      JOIN ai_runs r ON r.id = e.ai_run_id AND r.worker_type='classify' AND r.status='ok'`
	args := []any{}
	if since > 0 {
		args = append(args, since.String())
		q += ` WHERE r.created_at >= now() - $1::interval`
	}
	q += ` ORDER BY r.created_at DESC`

	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("select verdicts: %w", err)
	}
	defer rows.Close()

	var classified, flagged int
	byKind := map[string]int{}
	var lines []string
	for rows.Next() {
		var raw []byte
		var createdAt time.Time
		if err := rows.Scan(&raw, &createdAt); err != nil {
			return fmt.Errorf("scan verdict: %w", err)
		}
		var f struct {
			Actionable bool   `json:"actionable"`
			Kind       string `json:"kind"`
			Title      string `json:"title"`
			Reason     string `json:"reason"`
			MessageID  int64  `json:"normalized_message_id"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			continue
		}
		classified++
		byKind[f.Kind]++
		if !f.Actionable {
			continue
		}
		flagged++
		lines = append(lines, fmt.Sprintf("  %s  #%-7d %-16s %s",
			createdAt.Format("2006-01-02 15:04"), f.MessageID, f.Kind, f.Title))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate verdicts: %w", err)
	}

	fmt.Fprintln(w, "Classify shadow report (personal actionability)")
	fmt.Fprintf(w, "  classified: %d  flagged: %d\n", classified, flagged)
	if len(byKind) > 0 {
		for _, k := range sortedKeys(byKind) {
			fmt.Fprintf(w, "    by kind  %-18s %d\n", k, byKind[k])
		}
	}
	fmt.Fprintln(w)

	if err := reportSkipped(ctx, pool, w, since); err != nil {
		return err
	}

	if len(lines) > 0 {
		fmt.Fprintln(w, "flagged, newest first:")
		fmt.Fprintln(w, strings.Join(lines, "\n"))
		fmt.Fprintln(w)
	}
	return nil
}

// reportSkipped renders the lane the extraction join above CANNOT see, in
// internal/triage/report.go's shape and vocabulary.
//
// A refused message writes no extraction at all — that is what keeps "no
// permitted provider looked" structurally different from "the model looked and
// found nothing". The cost is that without this section a fully-skipped pass
// renders as `classified: 0`, which is indistinguishable from an empty inbox or
// a dead poller.
func reportSkipped(ctx context.Context, pool *pgxpool.Pool, w io.Writer, since time.Duration) error {
	q := `SELECT input FROM ai_runs WHERE worker_type='classify' AND status='skipped'`
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
	total := 0
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
		n := 1
		if rec.SkippedCount != nil {
			n = *rec.SkippedCount
		}
		total += n
		// Prefer the breakdown when the row carries one: a pass can refuse for
		// more than one reason, and filing the whole count under the dominant one
		// prints a wrong number in the one place an operator looks.
		if len(rec.AvailReasons) > 0 {
			for k, v := range rec.AvailReasons {
				byAvail[k] += v
			}
		} else {
			reason := rec.AvailReason
			if reason == "" {
				reason = "unrecorded"
			}
			byAvail[reason] += n
		}
		for k, v := range rec.ClassReasons {
			byClass[k] += v
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate skipped runs: %w", err)
	}
	if total == 0 {
		return nil
	}

	fmt.Fprintf(w, "  skipped: %d (never sent to any provider)\n", total)
	for _, k := range sortedKeys(byAvail) {
		fmt.Fprintf(w, "    why the lane refused   %-28s %d\n", k, byAvail[k])
	}
	for _, k := range sortedKeys(byClass) {
		fmt.Fprintf(w, "    why it was restricted  %-28s %d\n", k, byClass[k])
	}
	// The half a counter cannot carry. Nothing in the numbers distinguishes "the
	// local box is off" from "nothing was actionable", and the fix a reader
	// invents for the first is a fallback to the hosted lane — the one change the
	// boundary exists to prevent. By the time they open the runbook they have
	// already invented it, so it has to be said here.
	fmt.Fprintln(w, "    NOTE: an all-skipped pass is EXPECTED when the local model is not running.")
	fmt.Fprintln(w, "          Personal mail is only ever classified locally; falling back to a hosted")
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
