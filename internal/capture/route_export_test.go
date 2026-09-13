package capture

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ApplyRouteWithCandidates runs route_apply's per-message step (applyRoute) for
// one inbox message with a CALLER-SUPPLIED candidate list, the way a pass uses
// the set it cached at the start of the batch. It lets a test revoke a
// candidate between that read and the insert (SWT-40 Part B review fix 3).
// found=false: the message is not in the route_apply inbox and nothing ran.
func ApplyRouteWithCandidates(ctx context.Context, pool *pgxpool.Pool, messageID int64, since time.Duration,
	cands []RouteCandidate) (stats RouteStats, found bool, err error) {
	stats = RouteStats{ByStep: map[string]int{}, Unrouted: map[string]int{}}
	inbox, err := routeInbox(ctx, pool, since, RouteDefaultLimit)
	if err != nil {
		return stats, false, err
	}
	for _, r := range inbox {
		if r.messageID == messageID {
			return stats, true, applyRoute(ctx, pool, r, cands, &stats)
		}
	}
	return stats, false, nil
}
