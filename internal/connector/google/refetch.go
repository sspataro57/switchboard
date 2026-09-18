package google

// Targeted IMAP re-fetch (SWT-64). The size cap is applied at FETCH time, not at
// parse time, so a message that was over the cap when it was first ingested has
// no attachment bytes stored anywhere — reprocessing cannot recover them. Only a
// re-fetch from the server at a larger cap can, and the incremental pass will
// never revisit an old UID (it searches FromUID = stored.UIDNext).
//
// This pass takes an explicit, bounded set of already-ingested rows and refetches
// exactly those UIDs, writing through the same raw-first path the connector uses.
// It never moves a folder cursor: it reads its UIDs from raw_source_items, so it
// has no business writing a position, and writing one would rewind or advance the
// always-on connector's window over a six-figure mailbox.
//
// THE DANGEROUS FAILURE, and why so much of this file is refusals: raw_json is
// overwritten in place and raw_source_items keeps no version history. A write
// that carries the wrong message — or less of the right one — destroys that
// row's bytes with no way back. Eight refusals, in two families.
//
// Wrong message:
//   - a folder generation that has rolled (the UID now names a different message
//     or nothing) — UIDValidityChanged;
//   - a folder outside the selectable set (the same UID exists in other folders)
//     — FolderNotSelectable;
//   - a row whose envelope disagrees with its external_id (not written by this
//     connector) — EnvelopeMismatch;
//   - a UID the server returned nothing for — Gone;
//   - a target belonging to another account than this pass runs as — WrongAccount;
//   - a row that disappeared between selection and write — RowVanished.
//
// Less of the right one:
//   - a truncated fetch against a COMPLETE stored row — WouldDowngrade;
//   - a replacement carrying fewer bytes than the row already holds, which the
//     truncated FLAG cannot see because two truncated captures are not equal —
//     WouldShrink.
//
// Four of these were found by review rounds rather than by a test: WrongAccount,
// RowVanished, WouldDowngrade and WouldShrink. Three of the four are the same
// mistake — validating a FLAG (or nothing) instead of the value that lands; the
// fourth, RowVanished, is an insert branch inherited from the ingest pass that
// has no meaning for a repair.
//
// A refusal is an OUTCOME, not an error: one bad row must not abandon the rest of
// the batch. A transport failure IS an error — it means the pass cannot know what
// it did not see.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/chash"
)

// RefetchMaxMessageBytes is this tool's fetch cap: 100 MiB (owner, 2026-09-16).
//
// Deliberately NOT DefaultMaxMessageBytes and deliberately NOT read from
// MAIL_MAX_MESSAGE_BYTES. The connector's cap is an always-on bound over ~117k
// messages, where one pathological message would outweigh thousands; this one is
// a human naming a handful of messages and accepting their size. Two numbers,
// two different bargains.
const RefetchMaxMessageBytes = 100 << 20

// RefetchPhase is the sync_runs phase for this pass. Distinct from "imap" so
// availability's calendar readiness (stats->>'phase' = 'calendar') and the
// dashboard funnel's per-phase grouping both see it as its own bucket.
const RefetchPhase = "imap_refetch"

// RefetchQuery selects the rows to refetch. It is always bounded: Limit <= 0 is
// refused rather than treated as "no limit".
type RefetchQuery struct {
	AccountEmail string
	From         string
	Since, Until time.Time
	RawIDs       []int64
	Limit        int
}

// RefetchTarget is one already-ingested row, with the IMAP coordinates recovered
// from its stored envelope.
type RefetchTarget struct {
	RawID, AccountID int64
	AccountEmail     string
	ExternalID       string
	Folder           string
	UIDValidity, UID uint32
	StoredSize       int
	Truncated        bool
	// StoredB64Len is the length of the rfc822_b64 ALREADY stored. The floor in
	// writeRefetched compares against it, because `truncated` is a flag and a
	// flag cannot say whether the replacement carries fewer bytes than the row
	// it would overwrite.
	StoredB64Len    int
	Sender, Subject string
	SentAt          time.Time
}

// RefetchConfig drives one pass. DryRun lives here rather than only in the CLI so
// that "writes nothing" is exercised against the real sink, not against a command
// branch no test can reach.
type RefetchConfig struct {
	MaxBytes int // 0 => RefetchMaxMessageBytes
	DryRun   bool
	Out      io.Writer
}

// RefetchRefusal is one target the pass declined, carrying the raw id so the
// caller can name it without re-deriving it.
type RefetchRefusal struct {
	RawID  int64
	Reason string
}

// RefetchStats summarises one pass. Counters are printed unconditionally by the
// CLI, zeros included: a pass that refused everything and a pass that never ran
// must not look the same.
type RefetchStats struct {
	Planned, IMAPFetched int
	// RawInserted is structurally unreachable since the vanished-row refusal
	// replaced the insert branch: a refetch repairs an existing row, it never
	// creates one. Retained so the counter shape matches the ingest pass's, and
	// so a future change that reintroduces an insert has somewhere to count it.
	RawInserted, RawUpdated, RawUnchanged int
	UIDValidityChanged, FolderNotSelectable,
	EnvelopeMismatch, Gone int
	// WrongAccount and RowVanished are the two refusals that mean "the world
	// moved under the selection", as opposed to "this row cannot be refetched".
	WrongAccount, RowVanished int
	// WouldDowngrade counts targets whose stored capture is COMPLETE but whose
	// refetch came back truncated — refused, because the write is destructive
	// and there is no version history. WouldShrink is its byte-exact sibling:
	// the replacement carries fewer bytes than the row already holds, which a
	// flag comparison cannot see.
	WouldDowngrade, WouldShrink int
	// StillTruncated counts truncated captures that were actually WRITTEN —
	// incremented only after UpdateRaw succeeds, so no refused target inflates
	// it. It means the repair ran and recovered nothing, which every other
	// counter reports as success.
	StillTruncated int
	Refusals       []RefetchRefusal
}

// refetchLikeEscape makes s a literal substring for ILIKE (default escape
// character backslash).
//
// A SECOND SPELLING of internal/tools/mailattach.go's likeEscape, which is
// unexported in another package and cannot be imported from here. The two must
// agree; if a third caller ever appears, move the rule to a shared home rather
// than adding another copy.
func refetchLikeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (s *RefetchStats) refuse(rawID int64, reason string) {
	s.Refusals = append(s.Refusals, RefetchRefusal{RawID: rawID, Reason: reason})
}

// RefetchMessages refetches targets for ONE account and upserts them raw-first.
//
// It contacts IMAP even on a dry run, for exactly one fact: the folder's live
// UIDVALIDITY. That is the fact worth knowing before a live run, because it
// decides whether a target would be refused.
func RefetchMessages(ctx context.Context, src MailSource, sink Sink, acct Account,
	targets []RefetchTarget, cfg RefetchConfig) (RefetchStats, error) {
	stats := RefetchStats{Planned: len(targets)}
	if len(targets) == 0 {
		return stats, nil
	}
	maxBytes := cfg.MaxBytes
	if maxBytes == 0 {
		maxBytes = RefetchMaxMessageBytes
	}

	all, err := src.Folders(ctx)
	if err != nil {
		return stats, fmt.Errorf("list imap folders for %s: %w", acct.Email, err)
	}
	// The SAME selectable set the ingest pass uses — including the MAIL_FOLDERS
	// override, because a row ingested from a custom folder set must stay
	// refetchable. Passing nil here would make those rows permanently
	// unrefetchable behind a refusal naming the wrong reason.
	live := map[string]uint32{}
	for _, f := range SelectFolders(all, FoldersFromEnv()) {
		live[f.Name] = f.UIDValidity
	}

	// Vetting happens before any run row, any lock and any fetch, so a dry run
	// and a live run agree on which targets are eligible.
	byFolder := map[string][]RefetchTarget{}
	for _, t := range targets {
		// The account is checked FIRST, because every later check and the write
		// itself key on this pass's account, not on the row's. ListIMAPAccounts
		// matches lower(account_email), while source_accounts is unique only on
		// (provider, account_email) — case-distinct rows are legal. Writing a
		// target belonging to another account would find no stored hash, take the
		// insert branch and file the recovered bytes on a NEW row under the wrong
		// account, leaving the real row truncated and the counters reading as
		// success.
		if t.AccountID != acct.ID {
			stats.WrongAccount++
			stats.refuse(t.RawID, fmt.Sprintf("belongs to account %d, not %d (%s)", t.AccountID, acct.ID, acct.Email))
			continue
		}
		if imapExternalID(t.Folder, t.UIDValidity, t.UID) != t.ExternalID {
			stats.EnvelopeMismatch++
			stats.refuse(t.RawID, fmt.Sprintf("envelope disagrees with external_id %q: the row was not written by this connector", t.ExternalID))
			continue
		}
		liveUIDValidity, selectable := live[t.Folder]
		if !selectable {
			stats.FolderNotSelectable++
			stats.refuse(t.RawID, fmt.Sprintf("folder %q is not in the selectable set", t.Folder))
			continue
		}
		if liveUIDValidity != t.UIDValidity {
			// The generation rolled: UID t.UID now names a different message, or
			// nothing. Fetching it would file another message's bytes over this
			// row, unrecoverably.
			stats.UIDValidityChanged++
			stats.refuse(t.RawID, fmt.Sprintf("uidvalidity changed for %q: stored %d, live %d", t.Folder, t.UIDValidity, liveUIDValidity))
			continue
		}
		byFolder[t.Folder] = append(byFolder[t.Folder], t)
	}

	if cfg.DryRun {
		printRefetchPlan(cfg.Out, targets, live, stats)
		return stats, nil
	}

	runID, err := sink.StartRun(ctx, acct.ID, RefetchPhase)
	if err != nil {
		return stats, fmt.Errorf("start refetch run for %s: %w", acct.Email, err)
	}
	if err := refetchFolders(ctx, src, sink, acct, byFolder, maxBytes, &stats); err != nil {
		_ = sink.FinishRun(ctx, runID, "error", refetchIngestStats(stats), err.Error())
		return stats, err
	}
	if err := sink.FinishRun(ctx, runID, "ok", refetchIngestStats(stats), ""); err != nil {
		return stats, fmt.Errorf("finish refetch run for %s: %w", acct.Email, err)
	}
	return stats, nil
}

// refetchFolders fetches and writes one folder at a time. A transport failure
// aborts the pass: a partial batch whose remainder was never attempted must not
// be reported as a completed run.
func refetchFolders(ctx context.Context, src MailSource, sink Sink, acct Account,
	byFolder map[string][]RefetchTarget, maxBytes int, stats *RefetchStats) error {
	for folder, group := range byFolder {
		uids := make([]uint32, 0, len(group))
		for _, t := range group {
			uids = append(uids, t.UID)
		}
		msgs, err := src.Fetch(ctx, folder, uids, maxBytes)
		if err != nil {
			return fmt.Errorf("fetch %s/%s: %w", acct.Email, folder, err)
		}
		// Matched BY UID, never by position: a server may answer in any order,
		// and pairing result[i] with group[i] is the same unrecoverable overwrite
		// as a stale UIDVALIDITY, reached by a different mistake.
		got := map[uint32]FetchedMessage{}
		for _, m := range msgs {
			got[m.UID] = m
		}
		for _, t := range group {
			m, ok := got[t.UID]
			if !ok {
				stats.Gone++
				stats.refuse(t.RawID, fmt.Sprintf("gone: the server returned no message for %s/%d", folder, t.UID))
				continue
			}
			stats.IMAPFetched++
			if err := writeRefetched(ctx, sink, acct, t, m, stats); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeRefetched stores one refetched message raw-first, with the same
// hash-compare and the same three outcomes as the ingest pass's writeBatch.
func writeRefetched(ctx context.Context, sink Sink, acct Account, t RefetchTarget,
	m FetchedMessage, stats *RefetchStats) error {
	// NEVER DOWNGRADE a stored capture. The selection does not filter on
	// `truncated` (a sender window legitimately spans complete and truncated
	// rows), and a --max-bytes below a stored COMPLETE message makes this fetch
	// truncate: the hash then differs and the update would replace a full
	// capture with headers-plus-one-text-part, unrecoverably — raw_json is
	// overwritten in place with no version history.
	//
	// The rollback note's premise ("the new bytes strictly contain the old")
	// only holds when the stored row was already truncated. That case still
	// writes, and StillTruncated says it recovered nothing.
	if m.Truncated && !t.Truncated {
		stats.WouldDowngrade++
		stats.refuse(t.RawID, fmt.Sprintf("refused: the refetch came back truncated (cap too low) but the stored "+
			"row %q is COMPLETE; writing it would destroy stored bytes", t.ExternalID))
		return nil
	}
	// THE BYTE FLOOR, and the reason it exists rather than just the flag above:
	// `truncated` is a flag, and two truncated captures are not equal. A row
	// stored under the 1 MiB cap holds headers plus one text part; refetching it
	// with a smaller --max-bytes yields headers plus a SHORTER text part, or
	// headers alone when no text leaf fits. The flag says "truncated" in both
	// cases, the hash differs, and the update would shrink the row — with no
	// version history to undo it.
	//
	// Compared in encoded form so nothing has to be decoded: EncodedLen is exact
	// and monotonic in the byte count. It also catches a nil body section coming
	// back from the server, which routes an ordinary fetch into the oversize
	// branch without the operator asking for anything.
	if newLen := base64.StdEncoding.EncodedLen(len(m.RFC822)); newLen < t.StoredB64Len {
		stats.WouldShrink++
		stats.refuse(t.RawID, fmt.Sprintf("refused: the refetch carries fewer bytes (%d) than the stored row %q "+
			"already holds (%d); a repair may not shrink a capture", newLen, t.ExternalID, t.StoredB64Len))
		return nil
	}
	raw, err := buildIMAPEnvelope(t.Folder, t.UIDValidity, m)
	if err != nil {
		return fmt.Errorf("build envelope for %s: %w", t.ExternalID, err)
	}
	hash, err := chash.ContentHash(raw)
	if err != nil {
		return fmt.Errorf("hash %s: %w", t.ExternalID, err)
	}
	prev, exists, err := sink.RawHash(ctx, acct.ID, t.ExternalID)
	if err != nil {
		return fmt.Errorf("read raw hash for %s: %w", t.ExternalID, err)
	}
	switch {
	case !exists:
		// Unreachable by construction: the target came FROM this row. Reaching it
		// means the row moved or vanished between selection and write, so the
		// pass no longer knows what it is repairing — inserting here would create
		// a second row rather than fix the first. Refused, not written.
		stats.RowVanished++
		stats.refuse(t.RawID, fmt.Sprintf("row %q no longer exists for account %d: it moved or was deleted "+
			"between selection and write; re-run the selection", t.ExternalID, acct.ID))
	case prev != hash:
		if err := sink.UpdateRaw(ctx, acct.ID, t.ExternalID, raw, hash); err != nil {
			return fmt.Errorf("update raw %s: %w", t.ExternalID, err)
		}
		stats.RawUpdated++
		if m.Truncated {
			// Counted HERE, after the write SUCCEEDED, so it means exactly "a
			// truncated capture was written". Anywhere earlier and a row refused
			// further down (a vanished row, say) inflates it, and the counter,
			// the CLI's advice and the durable sync_runs value all describe a
			// write that never happened.
			stats.StillTruncated++
		}
	default:
		// The refetch produced the same bytes: the row was not truncated after
		// all, or the cap was already high enough.
		stats.RawUnchanged++
	}
	return nil
}

// refetchIngestStats maps the pass's counters onto the connector's Stats, which
// is what FinishRun records.
func refetchIngestStats(s RefetchStats) Stats {
	return Stats{
		IMAPFetched:  s.IMAPFetched,
		RawInserted:  s.RawInserted,
		RawUpdated:   s.RawUpdated,
		RawUnchanged: s.RawUnchanged,
		// Carried so the durable row can distinguish a pass that refused every
		// target from one that had nothing to do.
		RefetchUIDValidityChanged:  s.UIDValidityChanged,
		RefetchFolderNotSelectable: s.FolderNotSelectable,
		RefetchEnvelopeMismatch:    s.EnvelopeMismatch,
		RefetchGone:                s.Gone,
		RefetchWrongAccount:        s.WrongAccount,
		RefetchRowVanished:         s.RowVanished,
		RefetchStillTruncated:      s.StillTruncated,
		RefetchWouldDowngrade:      s.WouldDowngrade,
		RefetchWouldShrink:         s.WouldShrink,
	}
}

// printRefetchPlan prints one line per target: what would be fetched, and above
// all which targets would be REFUSED and why. Refused targets are printed too —
// a plan that silently omitted them would read as "these will be recovered".
func printRefetchPlan(out io.Writer, targets []RefetchTarget, live map[string]uint32, stats RefetchStats) {
	if out == nil {
		return
	}
	reasons := map[int64]string{}
	for _, r := range stats.Refusals {
		reasons[r.RawID] = r.Reason
	}
	for _, t := range targets {
		liveUIDValidity, selectable := live[t.Folder]
		liveText := fmt.Sprint(liveUIDValidity)
		if !selectable {
			liveText = "(folder not selectable)"
		}
		verdict := "refetch"
		if r, refused := reasons[t.RawID]; refused {
			verdict = "REFUSE " + r
		}
		fmt.Fprintf(out, "raw %d  %s  %s  uid %d  uidvalidity stored %d live %s  size %d  truncated=%t  %s  %q  %s  %s\n",
			t.RawID, t.AccountEmail, t.Folder, t.UID, t.UIDValidity, liveText,
			t.StoredSize, t.Truncated, t.SentAt.UTC().Format(time.RFC3339), t.Subject, t.Sender, verdict)
	}
}

// ---- selection ----------------------------------------------------------------

// refetchSelect reads the IMAP coordinates out of the stored envelope. The
// predicate's inputs are COLUMNS (raw_json->>'folder', ->>'uidvalidity',
// ->>'uid'), which is what makes the refusals meaningful: a literal here would
// certify nothing.
const refetchSelect = `
	SELECT r.id, r.source_account_id, sa.account_email, r.external_id,
	       COALESCE(r.raw_json->>'folder',''),
	       COALESCE((r.raw_json->>'uidvalidity')::bigint, 0),
	       COALESCE((r.raw_json->>'uid')::bigint, 0),
	       COALESCE((r.raw_json->>'size')::int, 0),
	       COALESCE((r.raw_json->>'truncated')::boolean, false),
	       COALESCE(octet_length(r.raw_json->>'rfc822_b64'), 0),
	       COALESCE(nm.sender,''), COALESCE(nm.subject,''),
	       COALESCE(nm.sent_at, r.ingested_at)
	  FROM raw_source_items r
	  JOIN source_accounts sa ON sa.id = r.source_account_id
	  LEFT JOIN normalized_messages nm ON nm.raw_source_item_id = r.id`

// SelectRefetchTargets resolves a query to targets, oldest first.
//
// It refuses an unbounded run: this pass overwrites raw_json in place with no
// undo, so "it defaulted to everything" must not be reachable by forgetting a
// flag. A row named explicitly that is not IMAP-sourced is an ERROR naming the
// id — those rows never stored attachment bytes at all, so "refetch it" has no
// meaning and a silent skip would read as "nothing to do".
func SelectRefetchTargets(ctx context.Context, pool *pgxpool.Pool, q RefetchQuery) ([]RefetchTarget, error) {
	if q.Limit <= 0 {
		// Spelled identically to the CLI's refusal (cmd/opsctl/mailrefetch.go):
		// a caller who hits this one must be able to grep for the other.
		return nil, errors.New("refetch: --limit is required and must be >= 1: this tool never runs unbounded, " +
			"because it overwrites stored mail in place with no version history")
	}
	if len(q.RawIDs) > 0 && strings.TrimSpace(q.From) != "" {
		return nil, errors.New("refetch: --from and --raw-id are different selector families; give one")
	}

	where := []string{}
	args := []any{}
	add := func(clause string, val any) {
		args = append(args, val)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if len(q.RawIDs) > 0 {
		add("r.id = ANY($%d)", q.RawIDs)
	} else {
		if strings.TrimSpace(q.From) == "" {
			return nil, errors.New("refetch: one of --from or --raw-id is required")
		}
		// Escaped: --from is a literal substring, never a pattern. Unescaped,
		// `--from %` widens to every sender in the mailbox, bounded only by
		// --limit — on a pass that overwrites rows in place.
		add("nm.sender ILIKE '%%' || $%d || '%%'", refetchLikeEscape(strings.TrimSpace(q.From)))
		// The finder form only ever considers IMAP rows; the explicit form
		// reports a non-IMAP row by name instead (below).
		where = append(where, "r.external_id LIKE 'imap:%'")
	}
	if q.AccountEmail != "" {
		add("lower(sa.account_email) = lower($%d)", q.AccountEmail)
	}
	if !q.Since.IsZero() {
		add("COALESCE(nm.sent_at, r.ingested_at) >= $%d", q.Since)
	}
	if !q.Until.IsZero() {
		add("COALESCE(nm.sent_at, r.ingested_at) <= $%d", q.Until)
	}
	args = append(args, q.Limit)

	sql := refetchSelect + "\n WHERE " + strings.Join(where, "\n   AND ") +
		fmt.Sprintf("\n ORDER BY COALESCE(nm.sent_at, r.ingested_at) ASC, r.id ASC\n LIMIT $%d", len(args))

	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("select refetch targets: %w", err)
	}
	defer rows.Close()

	var out []RefetchTarget
	for rows.Next() {
		var t RefetchTarget
		var uidValidity, uid int64
		if err := rows.Scan(&t.RawID, &t.AccountID, &t.AccountEmail, &t.ExternalID,
			&t.Folder, &uidValidity, &uid, &t.StoredSize, &t.Truncated, &t.StoredB64Len,
			&t.Sender, &t.Subject, &t.SentAt); err != nil {
			return nil, fmt.Errorf("scan refetch target: %w", err)
		}
		if !strings.HasPrefix(t.ExternalID, "imap:") {
			return nil, fmt.Errorf("refetch: raw_source_item %d (%s) is not an IMAP-sourced row: "+
				"it has no folder or UID to refetch, and its attachment bytes were never stored by this path",
				t.RawID, t.ExternalID)
		}
		t.UIDValidity, t.UID = uint32(uidValidity), uint32(uid)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate refetch targets: %w", err)
	}
	// An id the caller named explicitly and that matched nothing is an ERROR,
	// not an empty result: "no such row" and "nothing to do" are different
	// answers, and the same function refuses a non-IMAP row by name for exactly
	// this reason.
	if len(q.RawIDs) > 0 && len(out) != len(q.RawIDs) {
		found := map[int64]bool{}
		for _, t := range out {
			found[t.RawID] = true
		}
		var missing []string
		for _, id := range q.RawIDs {
			if !found[id] {
				missing = append(missing, fmt.Sprint(id))
			}
		}
		return nil, fmt.Errorf("refetch: --raw-id named %d row(s) that do not exist or are outside the "+
			"selection: %s", len(missing), strings.Join(missing, ", "))
	}
	return out, nil
}
