package dashboard

import (
	"net/http"
)

// The sources page answers one question the board could not: is ingestion
// actually working?
//
// Until now the dashboard read tasks, deliveries, briefs and plans — every one of
// them downstream of triage. With triage still in shadow mode those views stay
// near-empty no matter how much mail, Slack and Jira traffic is flowing, so an
// operator looking at a quiet board could not tell a healthy pipeline from a
// dead connector. This page reads the ingestion tables directly.
//
// Read-only: it writes nothing but its audit-free SELECTs, so it needs no
// executor path (invariant 3 governs actions, and there are none here).

// sourceRow is one account's ingestion health.
type sourceRow struct {
	ID       int64
	Provider string
	Email    string
	AuthType string
	// Enabled reports send_enabled, which is the difference between a mailbox we
	// read and one switchboard can also answer from.
	Enabled bool

	LastRunAt     string
	LastRunPhase  string
	LastRunStatus string
	LastRunError  string
	RunsTotal     int

	RawItems      int
	RawNormalized int
	// RawPending is raw rows that have not been normalized yet. A number that
	// keeps growing means normalization is failing while ingestion succeeds —
	// invisible from any other view.
	RawPending int

	Messages   int
	Inbound    int
	Outbound   int
	NewestMsg  string
	Truncated  int
	Attachment int
}

// channelRow summarises the funnel per channel, across accounts.
type channelRow struct {
	Channel   string
	Messages  int
	Inbound   int
	Outbound  int
	OldestMsg string
	NewestMsg string
}

type sourcesPage struct {
	Sources  []sourceRow
	Channels []channelRow
	// Totals are the headline numbers; a zero here is the fastest possible
	// "nothing is being ingested" signal.
	TotalRaw      int
	TotalMessages int
	TotalPending  int
}

func (s *Server) listSources(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	page := sourcesPage{}

	// Per account. Every aggregate is a scalar subquery scoped to the account, so
	// an account with no traffic still appears as a row of zeros rather than
	// vanishing from the page — an absent connector is exactly what we want to
	// see.
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.provider, a.account_email, COALESCE(a.auth_type,'oauth'), a.send_enabled,
		       COALESCE((SELECT to_char(max(started_at),'YYYY-MM-DD HH24:MI') FROM sync_runs sr WHERE sr.source_account_id=a.id), ''),
		       -- phase is a key inside stats, not a column: connectors record it
		       -- via jsonb_build_object('phase', ...) when the run starts.
		       COALESCE((SELECT sr.stats->>'phase' FROM sync_runs sr WHERE sr.source_account_id=a.id
		                  ORDER BY sr.started_at DESC LIMIT 1), ''),
		       COALESCE((SELECT sr.status FROM sync_runs sr WHERE sr.source_account_id=a.id
		                  ORDER BY sr.started_at DESC LIMIT 1), ''),
		       COALESCE((SELECT left(COALESCE(sr.error,''),200) FROM sync_runs sr WHERE sr.source_account_id=a.id
		                  ORDER BY sr.started_at DESC LIMIT 1), ''),
		       (SELECT count(*) FROM sync_runs sr WHERE sr.source_account_id=a.id),
		       (SELECT count(*) FROM raw_source_items ri WHERE ri.source_account_id=a.id),
		       (SELECT count(*) FROM raw_source_items ri WHERE ri.source_account_id=a.id AND ri.normalized_at IS NOT NULL),
		       (SELECT count(*) FROM raw_source_items ri WHERE ri.source_account_id=a.id AND ri.normalized_at IS NULL),
		       (SELECT count(*) FROM normalized_messages m
		          JOIN raw_source_items ri ON ri.id=m.raw_source_item_id WHERE ri.source_account_id=a.id),
		       (SELECT count(*) FROM normalized_messages m
		          JOIN raw_source_items ri ON ri.id=m.raw_source_item_id
		         WHERE ri.source_account_id=a.id AND m.direction='inbound'),
		       (SELECT count(*) FROM normalized_messages m
		          JOIN raw_source_items ri ON ri.id=m.raw_source_item_id
		         WHERE ri.source_account_id=a.id AND m.direction='outbound'),
		       COALESCE((SELECT to_char(max(m.sent_at),'YYYY-MM-DD HH24:MI') FROM normalized_messages m
		          JOIN raw_source_items ri ON ri.id=m.raw_source_item_id WHERE ri.source_account_id=a.id), ''),
		       (SELECT count(*) FROM raw_source_items ri
		         WHERE ri.source_account_id=a.id AND ri.raw_json->>'truncated' = 'true'),
		       (SELECT count(*) FROM raw_source_items ri
		         WHERE ri.source_account_id=a.id AND jsonb_array_length(COALESCE(ri.raw_json->'parts','[]'::jsonb)) > 0)
		  FROM source_accounts a
		 ORDER BY a.provider, a.account_email`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var x sourceRow
		if err := rows.Scan(&x.ID, &x.Provider, &x.Email, &x.AuthType, &x.Enabled,
			&x.LastRunAt, &x.LastRunPhase, &x.LastRunStatus, &x.LastRunError, &x.RunsTotal,
			&x.RawItems, &x.RawNormalized, &x.RawPending,
			&x.Messages, &x.Inbound, &x.Outbound, &x.NewestMsg,
			&x.Truncated, &x.Attachment); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		page.Sources = append(page.Sources, x)
		page.TotalRaw += x.RawItems
		page.TotalMessages += x.Messages
		page.TotalPending += x.RawPending
	}
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	chRows, err := s.pool.Query(ctx, `
		SELECT channel, count(*),
		       count(*) FILTER (WHERE direction='inbound'),
		       count(*) FILTER (WHERE direction='outbound'),
		       COALESCE(min(sent_at)::date::text,''), COALESCE(max(sent_at)::date::text,'')
		  FROM normalized_messages GROUP BY channel ORDER BY count(*) DESC`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer chRows.Close()
	for chRows.Next() {
		var c channelRow
		if err := chRows.Scan(&c.Channel, &c.Messages, &c.Inbound, &c.Outbound, &c.OldestMsg, &c.NewestMsg); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		page.Channels = append(page.Channels, c)
	}
	if err := chRows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := s.tmpl.ExecuteTemplate(w, "sources.html", page); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
