package capture

// recheckEditedMentions (SWT-79, codex review) keeps the mention gate true
// after a Slack EDIT, in both directions. capture decides a message once per
// mode, but the Slack sink re-ingests a changed message (UpdateRaw bumps
// ingested_at and the normalizer overwrites body_text). So each pass re-reads
// the NEWEST decision in ANY mode — the row both inquiry inboxes read as
// `latest` (codex round 3) — when it is `attributed`, for every Slack message
// ingested (or re-ingested) in the last hour, recomputes the fact from the
// CURRENT text through the same predicate decideMessage uses, and writes it
// when it changed:
//   - an edit that ADDS the mention clears the flag (the message reaches qwen);
//   - an edit that REMOVES it sets the flag (qwen never sees it again, and the
//     inbox drops it before promote's thread rule could make a task of it).
//
// There is deliberately NO "re-ingested after the decision" test (go-reviewer):
// a pass in another connector can read the old text, the sink then writes the
// edit, and the pass's decision lands after the re-ingest — a time comparison
// would never look at it again. Instead every edit refreshes ingested_at and so
// gets an hour of re-checks, whatever the decision's timestamp. Driven from
// raw_source_items.ingested_at (0043's index): a few hundred rows a pass.
// Only capture_decisions is
// written (capture's own table), only on `attributed` rows (the CHECK), and the
// reason records each change.

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
)

const (
	mentionAddedSuffix   = "; edited to mention Salvador: eligible for an inquiry verdict (SWT-79)"
	mentionRemovedSuffix = "; edited to remove the mention of Salvador: no inquiry verdict (SWT-79)"
)

func recheckEditedMentions(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx,
		`SELECT cd.id, cd.channel_unmentioned, COALESCE(nm.channel, ''), COALESCE(nt.thread_key, ''),
		        COALESCE(ri.raw_json->'conversation'->>'type', ''), COALESCE(nm.body_text, '')
		   FROM raw_source_items ri
		   JOIN normalized_messages nm ON nm.raw_source_item_id = ri.id AND nm.direction = 'inbound'
		   LEFT JOIN normalized_threads nt ON nt.id = nm.thread_id
		   JOIN LATERAL (SELECT c.id, c.action, c.channel_unmentioned, c.created_at
		                   FROM capture_decisions c
		                  WHERE c.message_id = nm.id
		                  ORDER BY c.id DESC LIMIT 1) cd ON true
		  WHERE ri.ingested_at > now() - interval '1 hour'
		    AND nm.channel = 'slack'
		    AND cd.action = 'attributed'`)
	if err != nil {
		return fmt.Errorf("select re-ingested attributed decisions: %w", err)
	}
	type change struct {
		id   int64
		flag bool
	}
	var changes []change
	for rows.Next() {
		var id int64
		var flagged bool
		var channel, threadKey, rawType, body string
		if err := rows.Scan(&id, &flagged, &channel, &threadKey, &rawType, &body); err != nil {
			rows.Close()
			return fmt.Errorf("scan re-ingested decision: %w", err)
		}
		now, _ := channelUnmentioned(channelMentionInput{
			slack:     channel == slackweb.Channel,
			dm:        slackweb.IsDirectMessageKey(threadKey),
			groupDM:   rawType == "group_dm",
			mentioned: slackweb.MentionsOwner(body),
		})
		if now != flagged {
			changes = append(changes, change{id: id, flag: now})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate re-ingested decisions: %w", err)
	}
	for _, c := range changes {
		suffix := mentionAddedSuffix
		if c.flag {
			suffix = mentionRemovedSuffix
		}
		if _, err := pool.Exec(ctx,
			`UPDATE capture_decisions SET channel_unmentioned = $2, reason = COALESCE(reason, '') || $3
			  WHERE id = $1 AND action = 'attributed' AND channel_unmentioned <> $2`,
			c.id, c.flag, suffix); err != nil {
			return fmt.Errorf("re-set the mention gate on decision %d after an edit: %w", c.id, err)
		}
	}
	return nil
}
