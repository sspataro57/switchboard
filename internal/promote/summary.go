package promote

// The /funnel page's promotion counters (SWT-30 criterion 18), behind a named
// seam for SWT-29's reason: the dashboard adds no SQL over ai_runs /
// ai_extractions — one spelling per fact, and this package owns its own log's
// folds the way classify.Summarize owns the verdict query.

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// LaneCounters is one lane's (worker_type's) promotion counts over a window:
// what the promoter did with that lane's verdicts.
type LaneCounters struct {
	Lane     string
	Created  int // action='task' — live tasks
	Attached int // action='attached' — follow-ups appended to an open task
	Review   int // action='review' — holding-lane tasks
}

// CountersByLane folds classify_promotions over the last `days` days, grouped
// by the lane that produced the verdict. Today only the personal lane can
// promote (the residue lane is excluded twice over), but the fold keys on
// worker_type so a lane that starts promoting shows up without a dashboard
// change.
func CountersByLane(ctx context.Context, pool *pgxpool.Pool, days int) ([]LaneCounters, error) {
	rows, err := pool.Query(ctx, `
		SELECT r.worker_type,
		       count(*) FILTER (WHERE cp.action = 'task')     AS created,
		       count(*) FILTER (WHERE cp.action = 'attached') AS attached,
		       count(*) FILTER (WHERE cp.action = 'review')   AS review
		  FROM classify_promotions cp
		  JOIN ai_extractions e ON e.id = cp.ai_extraction_id
		  JOIN ai_runs r ON r.id = e.ai_run_id
		 WHERE cp.created_at >= now() - make_interval(days => $1)
		 GROUP BY 1 ORDER BY 1`, days)
	if err != nil {
		return nil, fmt.Errorf("select promotion counters: %w", err)
	}
	defer rows.Close()

	var out []LaneCounters
	for rows.Next() {
		var c LaneCounters
		if err := rows.Scan(&c.Lane, &c.Created, &c.Attached, &c.Review); err != nil {
			return nil, fmt.Errorf("scan promotion counters: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
