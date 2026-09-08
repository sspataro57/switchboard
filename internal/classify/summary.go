package classify

// Summarize owns the classify report's SQL and folds (SWT-29 criterion 11):
// the verdict query, the four-way link-state fold and the skipped-run fold all
// MOVED here from report.go, so the /funnel page and `classify report` read
// ONE query and print the same numbers for the same window. ReportForWorker is
// now a renderer over Summary; its printed text — including the no-fallback
// note the structure tests scan for — stays in report.go, byte-identical.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// summaryFlagCap bounds Summary.Flags (criterion 13): 50 fits a page and stays
// scannable (capture's reportListLimit exists for the same reason). The COUNTS
// are never capped — capping Flagged would make a bad day look normal.
const summaryFlagCap = 50

// Summary is one lane's shadow-classification numbers for a window.
type Summary struct {
	WorkerType          string
	Classified, Flagged int
	ByKind              map[string]int
	LinkResolved        int
	LinkDeclined        int
	LinkNoneOffered     int
	LinkRejected        int
	Skipped             int
	ByAvailReason       map[string]int
	ByClassReason       map[string]int
	Flags               []Flag // newest first, capped at summaryFlagCap
}

// Flag is one flagged verdict. Sender and subject come from the STORED
// ai_extractions.fields, exactly as the CLI report reads them (criterion 13) —
// never a join back to normalized_messages for a second copy of what the model
// was shown.
type Flag struct {
	At        time.Time
	MessageID int64
	Kind      string
	Sender    string
	Subject   string
	Title     string
	LinkURL   string
}

// Summarize computes one lane's Summary. since == 0 means the whole history.
func Summarize(ctx context.Context, pool *pgxpool.Pool, since time.Duration, workerType string) (Summary, error) {
	s := Summary{
		WorkerType:    workerType,
		ByKind:        map[string]int{},
		ByAvailReason: map[string]int{},
		ByClassReason: map[string]int{},
	}

	q := `SELECT e.fields, r.created_at
	      FROM ai_extractions e
	      JOIN ai_runs r ON r.id = e.ai_run_id AND r.worker_type=$1 AND r.status='ok'`
	args := []any{workerType}
	if since > 0 {
		args = append(args, since.String())
		q += ` WHERE r.created_at >= now() - $2::interval`
	}
	q += ` ORDER BY r.created_at DESC`

	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return s, fmt.Errorf("select verdicts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var raw []byte
		var createdAt time.Time
		if err := rows.Scan(&raw, &createdAt); err != nil {
			return s, fmt.Errorf("scan verdict: %w", err)
		}
		var f struct {
			Actionable     bool     `json:"actionable"`
			Kind           string   `json:"kind"`
			Title          string   `json:"title"`
			Reason         string   `json:"reason"`
			Sender         string   `json:"sender"`
			Subject        string   `json:"subject"`
			MessageID      int64    `json:"normalized_message_id"`
			LinkCandidates *int     `json:"link_candidates"`
			LinkURL        *string  `json:"link_url"`
			LinkRejectedAs *float64 `json:"link_index_rejected"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			continue
		}
		s.Classified++
		s.ByKind[f.Kind]++
		// The four link states of SWT-25 criterion 21, counted separately: a
		// counter that cannot tell "nothing to offer" from "the model declined"
		// from "the model answered nonsense" is an alarm nobody can read. Rows
		// predating SWT-25 carry no link_candidates and are not counted at all.
		switch {
		case f.LinkCandidates == nil:
			// pre-SWT-25 verdict; nothing to count
		case f.LinkURL != nil && *f.LinkURL != "":
			s.LinkResolved++
		case f.LinkRejectedAs != nil:
			s.LinkRejected++
		case *f.LinkCandidates == 0:
			s.LinkNoneOffered++
		default:
			s.LinkDeclined++
		}
		if !f.Actionable {
			continue
		}
		s.Flagged++
		if len(s.Flags) < summaryFlagCap {
			link := ""
			if f.LinkURL != nil {
				link = *f.LinkURL
			}
			s.Flags = append(s.Flags, Flag{
				At: createdAt, MessageID: f.MessageID, Kind: f.Kind,
				Sender: f.Sender, Subject: f.Subject, Title: f.Title, LinkURL: link,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return s, fmt.Errorf("iterate verdicts: %w", err)
	}

	if err := summarizeSkipped(ctx, pool, since, workerType, &s); err != nil {
		return s, err
	}
	return s, nil
}

// summarizeSkipped folds the lane the extraction join above CANNOT see. A
// refused message writes no extraction at all — that is what keeps "no
// permitted provider looked" structurally different from "the model looked and
// found nothing"; without this fold a fully-skipped pass summarizes as
// classified: 0, indistinguishable from an empty inbox or a dead poller.
func summarizeSkipped(ctx context.Context, pool *pgxpool.Pool, since time.Duration, workerType string, s *Summary) error {
	q := `SELECT input FROM ai_runs WHERE worker_type=$1 AND status='skipped'`
	args := []any{workerType}
	if since > 0 {
		args = append(args, since.String())
		q += ` AND created_at >= now() - $2::interval`
	}
	q += ` ORDER BY created_at`

	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("select skipped runs: %w", err)
	}
	defer rows.Close()

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
		s.Skipped += n
		// Prefer the breakdown when the row carries one: a pass can refuse for
		// more than one reason, and filing the whole count under the dominant
		// one prints a wrong number in the one place an operator looks.
		if len(rec.AvailReasons) > 0 {
			for k, v := range rec.AvailReasons {
				s.ByAvailReason[k] += v
			}
		} else {
			reason := rec.AvailReason
			if reason == "" {
				reason = "unrecorded"
			}
			s.ByAvailReason[reason] += n
		}
		for k, v := range rec.ClassReasons {
			s.ByClassReason[k] += v
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate skipped runs: %w", err)
	}
	return nil
}
