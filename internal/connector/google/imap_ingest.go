package google

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/chash"
)

// IngestIMAP is the raw-first IMAP ingest pass for one account (criteria 5-9).
//
// It writes raw_source_items and the cursor and NOTHING else — no normalized
// rows, no tasks, no deliveries. Normalization is a separate phase reading only
// raw_json (invariant 1), which is what makes `--normalize-only --all` a full
// rebuild that never touches the network.
//
// Shape of a pass: list folders, keep INBOX + Sent, and for each one search
// (SINCE on a first pass, UID range on an incremental one), fetch, write every
// message raw, and only then advance that folder's cursor.
func IngestIMAP(ctx context.Context, src MailSource, sink Sink, acct Account, cfg Config) (Stats, error) {
	var stats Stats

	runID, err := sink.StartRun(ctx, acct.ID, "imap")
	if err != nil {
		return stats, fmt.Errorf("start imap run for %s: %w", acct.Email, err)
	}
	// fail marks the run and returns — the cursor is deliberately NOT saved on
	// this path, so the next pass re-fetches from the last known-good position
	// rather than skipping whatever the failed pass did not durably store.
	fail := func(err error) (Stats, error) {
		_ = sink.FinishRun(ctx, runID, "error", stats, err.Error())
		return stats, err
	}

	maxBytes := cfg.MaxMessageBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxMessageBytes
	}
	backfill := cfg.Backfill
	if backfill <= 0 {
		backfill = DefaultBackfill
	}
	now := cfg.Now
	if now.IsZero() {
		now = time.Now()
	}

	all, err := src.Folders(ctx)
	if err != nil {
		return fail(fmt.Errorf("list imap folders for %s: %w", acct.Email, err))
	}
	folders := SelectFolders(all, cfg.Folders)

	cursor, err := sink.Cursor(ctx, acct.ID)
	if err != nil {
		return fail(fmt.Errorf("read cursor for %s: %w", acct.Email, err))
	}
	if cursor.IMAPFolders == nil {
		cursor.IMAPFolders = map[string]FolderCursor{}
	}

	for _, folder := range folders {
		stored := cursor.IMAPFolders[folder.Name]

		// A UIDVALIDITY change means every stored UID for this folder now refers
		// to a different message (or to nothing). The stored position is
		// meaningless, so it is discarded and the SINCE window re-run. Old raw
		// rows survive untouched because uidvalidity is part of external_id —
		// they are history, and normalize-time Message-ID dedup collapses the
		// duplicates into one normalized row.
		resync := stored.UIDValidity != folder.UIDValidity
		var crit SearchCriteria
		switch {
		case resync || stored.UIDNext == 0:
			// First pass, or a forced resync: bound by time, never unbounded.
			crit.Since = now.Add(-backfill)
		case cfg.Full:
			// --full deliberately re-runs the window rather than trusting UIDs.
			crit.Since = now.Add(-backfill)
		default:
			crit.FromUID = stored.UIDNext
		}

		uids, err := src.Search(ctx, folder.Name, crit)
		if err != nil {
			return fail(fmt.Errorf("search %s/%s: %w", acct.Email, folder.Name, err))
		}
		if len(uids) == 0 {
			// Nothing new. Still record the folder's generation so a later
			// UIDVALIDITY change is detectable, but never move UIDNext backwards.
			next := stored.UIDNext
			if resync {
				next = 0
			}
			cursor.IMAPFolders[folder.Name] = FolderCursor{UIDValidity: folder.UIDValidity, UIDNext: next}
			continue
		}

		msgs, err := src.Fetch(ctx, folder.Name, uids, maxBytes)
		if err != nil {
			return fail(fmt.Errorf("fetch %s/%s: %w", acct.Email, folder.Name, err))
		}

		maxUID := stored.UIDNext
		if resync {
			maxUID = 0
		}
		for _, m := range msgs {
			stats.IMAPFetched++
			if m.Truncated {
				stats.IMAPTruncated++
			}
			externalID := imapExternalID(folder.Name, folder.UIDValidity, m.UID)
			raw, err := buildIMAPEnvelope(folder.Name, folder.UIDValidity, m)
			if err != nil {
				return fail(fmt.Errorf("build envelope for %s: %w", externalID, err))
			}
			hash, err := chash.ContentHash(raw)
			if err != nil {
				return fail(fmt.Errorf("hash %s: %w", externalID, err))
			}

			prev, exists, err := sink.RawHash(ctx, acct.ID, externalID)
			if err != nil {
				return fail(fmt.Errorf("read raw hash for %s: %w", externalID, err))
			}
			switch {
			case !exists:
				if err := sink.InsertRaw(ctx, acct.ID, externalID, raw, hash); err != nil {
					return fail(fmt.Errorf("insert raw %s: %w", externalID, err))
				}
				stats.RawInserted++
			case prev != hash:
				// A stored message's bytes should not change under a stable UID;
				// if the server says otherwise, the newest wins and it is counted.
				if err := sink.UpdateRaw(ctx, acct.ID, externalID, raw, hash); err != nil {
					return fail(fmt.Errorf("update raw %s: %w", externalID, err))
				}
				stats.RawUpdated++
			default:
				stats.RawUnchanged++
			}

			if m.UID+1 > maxUID {
				maxUID = m.UID + 1
			}
		}

		// Cursor advanced ONLY here, after every message in this folder's pass is
		// durably written. Any error above returned early, leaving the stored
		// position untouched.
		cursor.IMAPFolders[folder.Name] = FolderCursor{UIDValidity: folder.UIDValidity, UIDNext: maxUID}
	}

	if err := sink.SaveCursor(ctx, acct.ID, cursor); err != nil {
		return fail(fmt.Errorf("save cursor for %s: %w", acct.Email, err))
	}
	if err := sink.FinishRun(ctx, runID, "ok", stats, ""); err != nil {
		return stats, fmt.Errorf("finish imap run for %s: %w", acct.Email, err)
	}
	return stats, nil
}

// buildIMAPEnvelope renders the raw_json for one message (criterion 5).
//
// The RFC822 bytes ride base64 because raw_json is JSONB: mail is 8-bit-clean
// and routinely carries bytes that are not valid UTF-8, which Postgres would
// reject outright — losing the message rather than storing it imperfectly.
// The "source":"imap" key doubles as the normalize dispatch discriminator.
func buildIMAPEnvelope(folder string, uidValidity uint32, m FetchedMessage) (json.RawMessage, error) {
	flags := m.Flags
	if flags == nil {
		flags = []string{}
	}
	env := imapRawEnvelope{
		Source:       "imap",
		Folder:       folder,
		UIDValidity:  uidValidity,
		UID:          m.UID,
		InternalDate: m.InternalDate.UTC().Format(time.RFC3339),
		Flags:        flags,
		Size:         m.Size,
		Truncated:    m.Truncated,
		RFC822B64:    base64.StdEncoding.EncodeToString(m.RFC822),
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("marshal imap envelope: %w", err)
	}
	return raw, nil
}
