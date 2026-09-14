package capture

// The store-backed half of the PR-review rule's authorship check (SWT-54 D1):
// prReviewFacts reads stored raw mail and hands decidePRAuthor plain values.
// Reads only — this file writes no table.

import (
	"context"
	"fmt"
	"log"
	"net/mail"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/github"
	"github.com/sspataro57/switchboard/internal/connector/google"
)

// prReviewReadCap bounds each read: newest first, at most 100 rows.
const prReviewReadCap = 100

// prReviewFacts reads D1's evidence for PR ref, as seen from pending message pm:
//
//   - every INBOUND message on pm's thread (the thread key ends in the PR's
//     root Message-ID, so that is every mail of the PR in pm's account), plus
//     pm itself when it has no thread;
//   - the opening notification, found by EXACT external_message_id equality on
//     github.PRRootMessageID(ref) across every account, inbound only.
//
// Each row's raw_source_items.raw_json is decoded to its HEADERS only
// (google.RawMailHeaders) — never a normalized column, which carries no
// X-GitHub-* header. A row that is not an IMAP envelope contributes nothing.
// An undecodable envelope also contributes nothing, logged: one corrupt raw row
// must not wedge every later capture pass on that message (D1 is fail-open).
//
// Residual (pre-check 0f found 2 receiving accounts): a PR whose later mail
// went to the other account is not read on the thread side; only the opening
// lookup crosses accounts.
func prReviewFacts(ctx context.Context, pool *pgxpool.Pool, pm pendingMessage, ref github.PRRef) (prAuthorFacts, error) {
	var f prAuthorFacts

	rows, err := pool.Query(ctx,
		`SELECT m.id, ri.raw_json
		   FROM normalized_messages m
		   LEFT JOIN raw_source_items ri ON ri.id = m.raw_source_item_id
		  WHERE m.direction = 'inbound'
		    AND (m.id = $1 OR ($2::bigint IS NOT NULL AND m.thread_id = $2))
		  ORDER BY COALESCE(m.sent_at, m.created_at) DESC, m.id DESC
		  LIMIT 100`, pm.msg.ID, pm.threadID)
	if err != nil {
		return f, fmt.Errorf("pr review: read thread mail for %s: %w", github.PRKey(ref), err)
	}
	for rows.Next() {
		var id int64
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return f, fmt.Errorf("pr review: scan thread mail for %s: %w", github.PRKey(ref), err)
		}
		if n, ok := prNotificationOf(id, raw, ref); ok {
			f.reasons = append(f.reasons, n.Reason)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return f, fmt.Errorf("pr review: iterate thread mail for %s: %w", github.PRKey(ref), err)
	}

	// The opening, by exact external_message_id. Two accounts can both hold a
	// copy; the first one that names a sender is the one that answers.
	rows, err = pool.Query(ctx,
		`SELECT m.id, ri.raw_json
		   FROM normalized_messages m
		   LEFT JOIN raw_source_items ri ON ri.id = m.raw_source_item_id
		  WHERE m.external_message_id = $1 AND m.direction = 'inbound'
		  ORDER BY COALESCE(m.sent_at, m.created_at) DESC, m.id DESC
		  LIMIT 100`, github.PRRootMessageID(ref))
	if err != nil {
		return f, fmt.Errorf("pr review: read opening mail for %s: %w", github.PRKey(ref), err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return f, fmt.Errorf("pr review: scan opening mail for %s: %w", github.PRKey(ref), err)
		}
		n, ok := prNotificationOf(id, raw, ref)
		if !ok {
			continue
		}
		f.reasons = append(f.reasons, n.Reason)
		if !f.openingFound || (f.openingSender == "" && n.Sender != "") {
			f.openingFound = true
			f.openingSender = n.Sender
			f.openingRecipient = n.Recipient
		}
	}
	if err := rows.Err(); err != nil {
		return f, fmt.Errorf("pr review: iterate opening mail for %s: %w", github.PRKey(ref), err)
	}
	return f, nil
}

// prNotificationOf decodes one stored raw row's GitHub headers; ok=false when
// the row carries no readable RFC822 header, OR when prUntrustedReason says the
// mail is not a trusted GitHub notification about PR ref (the SPEC's D1
// amendment of 2026-09-14: single-instance headers, Gmail's dkim=pass for
// github.com, and the PR bound to the signed List-ID and Subject). A forged
// mail's X-GitHub-Reason: author must not suppress a colleague's review task,
// and a forged opening must not name its author.
func prNotificationOf(messageID int64, raw []byte, ref github.PRRef) (github.Notification, bool) {
	h, ok := prStoredHeaders(messageID, raw)
	if !ok || prUntrustedReason(h, ref) != "" {
		return github.Notification{}, false
	}
	return github.NotificationFacts(h), true
}

// prStoredHeaders is one stored raw row's RFC822 header; ok=false when there is
// none to read (logged when the envelope is corrupt, never a pass failure).
func prStoredHeaders(messageID int64, raw []byte) (mail.Header, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	h, ok, err := google.RawMailHeaders(raw)
	if err != nil {
		log.Printf("capture rules: pr review: message %d: raw envelope unreadable, treated as untrusted GitHub mail: %v",
			messageID, err)
		return nil, false
	}
	return h, ok
}

// prMailTrusted reads the pending message's OWN stored raw row and reports
// whether it is a trusted GitHub notification about PR ref (prUntrustedReason).
// A pr_review rule acts on nothing else: an untrusted mail falls through to the
// next rule (decideMessage). why is the reason's evidence when untrusted, and
// names the check that failed.
func prMailTrusted(ctx context.Context, pool *pgxpool.Pool, pm pendingMessage, ref github.PRRef) (bool, string, error) {
	if pm.rawItemID == nil {
		return false, "no stored raw row", nil
	}
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT raw_json FROM raw_source_items WHERE id = $1`, *pm.rawItemID).
		Scan(&raw); err != nil {
		return false, "", fmt.Errorf("pr review: read raw row %d of message %d: %w", *pm.rawItemID, pm.msg.ID, err)
	}
	h, ok := prStoredHeaders(pm.msg.ID, raw)
	if !ok {
		return false, "no readable stored RFC822 header", nil
	}
	if why := prUntrustedReason(h, ref); why != "" {
		return false, why, nil
	}
	return true, "", nil
}
