package capture

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// reportRoute is SWT-40 B9's capture-report half: the routing tier's rows on
// their own lines, one per route_step, counted over rows of mode='route' in the
// window (not latest decisions — a route row is one per message forever, and
// its count is the tier's output). The model's side of the tier (verdicts,
// grounding, pending_verdict by account) is `classify report --lane route`.
func reportRoute(ctx context.Context, pool *pgxpool.Pool, window *time.Time, b *strings.Builder) error {
	rows, err := pool.Query(ctx, `
	  SELECT route_step, count(*) FROM capture_decisions
	   WHERE mode = 'route' AND ($1::timestamptz IS NULL OR created_at >= $1)
	   GROUP BY 1`, window)
	if err != nil {
		return fmt.Errorf("select route decisions: %w", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	total := 0
	for rows.Next() {
		var step string
		var n int
		if err := rows.Scan(&step, &n); err != nil {
			return fmt.Errorf("scan route decision: %w", err)
		}
		counts[step] += n
		total += n
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate route decisions: %w", err)
	}

	b.WriteString("ROUTE (local-LLM routing tier: mode='route' rows by step)\n")
	if total == 0 {
		b.WriteString("  (none — no message was routed in this window)\n")
		return nil
	}
	for _, step := range []string{RouteStepThread, RouteStepSingle, RouteStepModel, RouteStepDefault} {
		fmt.Fprintf(b, "  %-44s %6d\n", "route "+step, counts[step])
	}
	fmt.Fprintf(b, "  %-44s %6d\n", "routed (all steps)", total)
	return nil
}
