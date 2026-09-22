package dashboard

// The /sources Slack-watch panel's read (SWT-75 criterion 28). Kept out of
// sources.go on purpose: that file must not name run rows (connector health
// belongs to /funnel, funnel_test.go), and "last read" here is per
// conversation — the rotation's last visit, the same number the rotation's
// own ordering uses — not a connector's last run.

import (
	"context"
	"fmt"
)

func (s *Server) slackWatchRows(ctx context.Context) ([]slackWatchRow, error) {
	rows, err := s.pool.Query(ctx, `
		WITH last_read AS (
		  SELECT lower(a.account_email) AS account_email, rd.conversation_id, max(r.started_at) AS at
		    FROM sync_runs r
		    JOIN source_accounts a ON a.id = r.source_account_id AND a.provider = 'slack_web'
		    CROSS JOIN LATERAL jsonb_array_elements_text(r.stats->'read') AS rd(conversation_id)
		   WHERE r.status IN ('ok','partial') AND COALESCE(r.stats->>'phase','slack_web') = 'slack_web'
		     AND jsonb_typeof(r.stats->'read') = 'array'
		     AND r.started_at > now() - interval '30 days'
		   GROUP BY 1, 2
		)
		SELECT w.id, w.label, w.workspace_id, w.conversation_id, w.enabled,
		       COALESCE(to_char(lr.at AT TIME ZONE $1, 'YYYY-MM-DD HH24:MI'), '')
		  FROM slack_watch w
		  LEFT JOIN last_read lr ON lr.account_email = lower(w.workspace_id) || '@slack-web.local'
		                        AND lr.conversation_id = w.conversation_id
		 ORDER BY w.enabled DESC, w.workspace_id, w.id`, BoardTimeZone)
	if err != nil {
		return nil, fmt.Errorf("select slack watch: %w", err)
	}
	defer rows.Close()
	var out []slackWatchRow
	for rows.Next() {
		var x slackWatchRow
		if err := rows.Scan(&x.ID, &x.Label, &x.Workspace, &x.Conversation, &x.Enabled, &x.LastRead); err != nil {
			return nil, fmt.Errorf("scan slack watch: %w", err)
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read slack watch: %w", err)
	}
	return out, nil
}
