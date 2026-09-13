package promote

// The inquiry lane's dismissal readout (SWT-40 C-D12, criterion C14): the
// lane's PIPELINE-precision label source and O7's flip signal. Read-only, not
// an eval: it measures precision, never recall, and nothing here enters the
// committed labelled set. The fold lives here; the threshold and marker are
// applied in cmd/classify.

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/policy"
)

// The outcome vocabulary, printed by `classify promote --outcomes`.
const (
	OutcomeFalsePositive = "false_positive"
	OutcomeTruePositive  = "true_positive"
	OutcomeMisClick      = "mis_click"
	OutcomeExcluded      = "excluded"
)

// Dismissal is a promoted task's FIRST task_dismissals row: its reason code
// and who (if anyone) reopened it.
type Dismissal struct {
	ReasonCode string
	ReopenedBy string
}

// InquiryOutcome is C-D12's table, pure:
//
//   - false positive: FIRST dismissal not_actionable or wrong_kind;
//   - true positive: closed or delivered with no dismissal, or FIRST dismissal
//     handled_elsewhere (a real ask, answered elsewhere);
//   - mis-click: the first dismissal was reopened by a HUMAN actor
//     (policy.HumanActor, SWT-36 D6);
//   - excluded: duplicate, an unknown code, or still open.
//
// An ACTIVITY reopen (a non-human actor, e.g. promote:inquiry or capture) does
// not undo the label: the first dismissal decides, whatever happened after.
func InquiryOutcome(status string, first *Dismissal) string {
	if first == nil {
		if status == "closed" || status == "delivered" {
			return OutcomeTruePositive
		}
		return OutcomeExcluded
	}
	if first.ReopenedBy != "" && policy.HumanActor(first.ReopenedBy) {
		return OutcomeMisClick
	}
	switch first.ReasonCode {
	case "not_actionable", "wrong_kind":
		return OutcomeFalsePositive
	case "handled_elsewhere":
		return OutcomeTruePositive
	default:
		return OutcomeExcluded
	}
}

// OutcomeCounts is the readout over a window.
type OutcomeCounts struct {
	FalsePositive, TruePositive, MisClick, Excluded int
}

// Decided is FalsePositive + TruePositive: mis-clicks and exclusions are not
// labels.
func (c OutcomeCounts) Decided() int { return c.FalsePositive + c.TruePositive }

func (c *OutcomeCounts) add(outcome string) {
	switch outcome {
	case OutcomeFalsePositive:
		c.FalsePositive++
	case OutcomeTruePositive:
		c.TruePositive++
	case OutcomeMisClick:
		c.MisClick++
	default:
		c.Excluded++
	}
}

// InquiryOutcomes folds the inquiry lane's promotion rows (a classify_inquiry
// extraction, with a task) on each task's FIRST dismissal. `attached`
// promotions are counted excluded: the unit is a promoted task, and an attach
// promoted nothing. since 0 = all; the window is on the promotion row's
// created_at.
func InquiryOutcomes(ctx context.Context, pool *pgxpool.Pool, since time.Duration) (OutcomeCounts, error) {
	var c OutcomeCounts
	rows, err := pool.Query(ctx, `
		SELECT cp.action, t.status, d.reason_code, d.reopened_by
		  FROM classify_promotions cp
		  JOIN ai_extractions e ON e.id = cp.ai_extraction_id
		  JOIN ai_runs r ON r.id = e.ai_run_id AND r.worker_type = 'classify_inquiry'
		  JOIN tasks t ON t.id = cp.task_id
		  LEFT JOIN LATERAL (SELECT reason_code, reopened_by FROM task_dismissals
		                      WHERE task_id = t.id
		                      ORDER BY created_at ASC, id ASC LIMIT 1) d ON true
		 WHERE $1::double precision <= 0 OR cp.created_at >= now() - make_interval(secs => $1)`,
		since.Seconds())
	if err != nil {
		return c, fmt.Errorf("promote: select inquiry outcomes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			action, status string
			code, by       *string
		)
		if err := rows.Scan(&action, &status, &code, &by); err != nil {
			return c, fmt.Errorf("promote: scan inquiry outcome: %w", err)
		}
		if action == "attached" {
			c.add(OutcomeExcluded)
			continue
		}
		var first *Dismissal
		if code != nil {
			first = &Dismissal{ReasonCode: *code}
			if by != nil {
				first.ReopenedBy = *by
			}
		}
		c.add(InquiryOutcome(status, first))
	}
	return c, rows.Err()
}

// StuckClaim is one lane's claims with no task: classify_promotions rows whose
// task_id is still NULL. Claim-before-act (criterion 12) means a crash or a
// transient error between the claim and the executor's create/attach leaves
// such a row, and the inbox then excludes that message for good: at most once,
// never a duplicate task. The contract stays as it is; this makes the
// stragglers visible. A claim a running pass is still working on also shows
// here for the seconds it takes.
type StuckClaim struct {
	Lane   string
	Count  int
	Oldest time.Time // zero when Count is 0
}

// laneOfWorkerType names the lane a verdict's worker_type promotes on. Other
// values are reported under their worker_type instead of being dropped.
func laneOfWorkerType(wt string) string {
	switch wt {
	case "classify":
		return string(LanePersonal)
	case "classify_inquiry":
		return string(LaneInquiry)
	case "":
		return "unknown"
	}
	return "unknown (worker_type " + wt + ")"
}

// StuckClaims counts every lane's claims with no task, all time (no window: a
// stuck claim never recovers on its own). Personal and inquiry always come
// first, zeros included, followed by any claim whose lane is not derivable.
func StuckClaims(ctx context.Context, pool *pgxpool.Pool) ([]StuckClaim, error) {
	rows, err := pool.Query(ctx, `
		SELECT COALESCE(r.worker_type, ''), count(*), min(cp.created_at)
		  FROM classify_promotions cp
		  LEFT JOIN ai_extractions e ON e.id = cp.ai_extraction_id
		  LEFT JOIN ai_runs r ON r.id = e.ai_run_id
		 WHERE cp.task_id IS NULL
		 GROUP BY 1
		 ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("promote: select stuck claims: %w", err)
	}
	defer rows.Close()
	out := []StuckClaim{{Lane: string(LanePersonal)}, {Lane: string(LaneInquiry)}}
	for rows.Next() {
		var (
			wt     string
			n      int
			oldest time.Time
		)
		if err := rows.Scan(&wt, &n, &oldest); err != nil {
			return nil, fmt.Errorf("promote: scan stuck claims: %w", err)
		}
		lane := laneOfWorkerType(wt)
		found := false
		for i := range out {
			if out[i].Lane == lane {
				out[i].Count, out[i].Oldest, found = n, oldest, true
			}
		}
		if !found {
			out = append(out, StuckClaim{Lane: lane, Count: n, Oldest: oldest})
		}
	}
	return out, rows.Err()
}
