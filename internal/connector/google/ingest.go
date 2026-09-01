package google

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/chash"
)

const (
	// DefaultOverlap is the trailing re-read window on the gmail cursor.
	DefaultOverlap = time.Hour
	// DefaultBackfill is the initial gmail window on a fresh cursor.
	DefaultBackfill = 90 * 24 * time.Hour

	// CalendarWindowPast / CalendarWindowFuture bound the calendar initial
	// window (the official sync recipe). EXPORTED (SWT-24 criterion 7): the
	// availability wiring refuses a propose_slots window outside
	// [now-Past, now+Future] against THESE constants — nothing was ever fetched
	// beyond them, so an empty busy set there means "we never looked", and a
	// re-spelled copy would silently drift the first time this window changes.
	CalendarWindowPast   = 30 * 24 * time.Hour
	CalendarWindowFuture = 90 * 24 * time.Hour
)

// Account is one provider='google' source_accounts row.
type Account struct {
	ID                     int64
	Email                  string
	CalendarInAvailability bool
	// AuthType decides which ingest and send path this account uses:
	// 'oauth' (Gmail API) or 'app_password' (IMAP/SMTP). Defaults to oauth so a
	// row written before migration 0014 keeps its behaviour.
	AuthType string
	// Per-account endpoints; empty means the Gmail defaults.
	IMAPHost string
	IMAPPort int
	SMTPHost string
	SMTPPort int
}

// Hosts renders the account's endpoints with the Gmail defaults filled in.
func (a Account) Hosts() MailHosts {
	return MailHosts{
		IMAPHost: a.IMAPHost, IMAPPort: a.IMAPPort,
		SMTPHost: a.SMTPHost, SMTPPort: a.SMTPPort,
	}.WithDefaults()
}

// Cursor holds both incremental positions side by side in sync_cursor.
type Cursor struct {
	GmailInternalDateMS int64  `json:"gmail_internal_date_ms"`
	CalendarSyncToken   string `json:"calendar_sync_token"`
	// IMAPFolders is the per-folder UID position for the IMAP source (SWT-11).
	// It sits alongside the two keys above rather than replacing them: an account
	// may be ingested by the Gmail API today and IMAP tomorrow, and a save from
	// either path must not erase the other's position (criterion 8).
	IMAPFolders map[string]FolderCursor `json:"imap_folders,omitempty"`
}

// Stats is per-run bookkeeping (sync_runs.stats).
type Stats struct {
	GmailListed    int `json:"gmail_listed"`
	GmailFetched   int `json:"gmail_fetched"`
	CalendarListed int `json:"calendar_listed"`
	RawInserted    int `json:"raw_inserted"`
	RawUpdated     int `json:"raw_updated"`
	RawUnchanged   int `json:"raw_unchanged"`
	Normalized     int `json:"normalized"`
	DedupSkipped   int `json:"dedup_skipped"`
	// CalendarResets counts sync-token resets applied as replacements, and
	// CalendarSuperseded the observations the replacement no longer carried —
	// both were previously invisible to the operator.
	CalendarResets     int `json:"calendar_resets"`
	CalendarSuperseded int `json:"calendar_superseded"`
	// AccountsBusy counts accounts skipped because another pass held the lock.
	AccountsBusy int `json:"accounts_busy"`
	// IMAPFetched counts messages pulled from IMAP; IMAPTruncated how many of
	// those exceeded the size cap and were captured with headers and text only, attachments skipped. Truncation is
	// lossy, so it is counted rather than left silent.
	IMAPFetched   int `json:"imap_fetched"`
	IMAPTruncated int `json:"imap_truncated"`
}

func (s *Stats) add(o Stats) {
	s.GmailListed += o.GmailListed
	s.GmailFetched += o.GmailFetched
	s.CalendarListed += o.CalendarListed
	s.CalendarResets += o.CalendarResets
	s.CalendarSuperseded += o.CalendarSuperseded
	s.AccountsBusy += o.AccountsBusy
	s.RawInserted += o.RawInserted
	s.RawUpdated += o.RawUpdated
	s.RawUnchanged += o.RawUnchanged
	s.Normalized += o.Normalized
	s.DedupSkipped += o.DedupSkipped
}

// Config selects run modes. Now is an injectable clock (zero => time.Now()).
// AccountEmail, when set, limits the ingest to that one account (debugging);
// the normalize phase stays global regardless.
type Config struct {
	// MaxMessageBytes caps a single IMAP fetch; 0 means DefaultMaxMessageBytes.
	MaxMessageBytes int
	// Folders overrides IMAP folder discovery (MAIL_FOLDERS); nil means discover.
	Folders      []string
	Full         bool
	All          bool
	Overlap      time.Duration
	Backfill     time.Duration
	Now          time.Time
	AccountEmail string
}

func (c Config) now() time.Time {
	if c.Now.IsZero() {
		return time.Now()
	}
	return c.Now
}

func (c Config) backfill() time.Duration {
	if c.Backfill == 0 {
		return DefaultBackfill
	}
	return c.Backfill
}

// Sink is the ops-db side of the raw-first ingest phase. sync_runs are per
// account × phase ("gmail" | "calendar").
type Sink interface {
	Cursor(ctx context.Context, accountID int64) (Cursor, error)
	SaveCursor(ctx context.Context, accountID int64, c Cursor) error
	// SaveCursorField writes ONE cursor key. The calendar phase uses it
	// (SWT-24 criterion 19): SaveCursor replaces the whole blob, so a calendar
	// save would clobber an IMAP folder position written between its read and
	// its write — and a rolled-back UID position re-reads or SKIPS mail.
	SaveCursorField(ctx context.Context, accountID int64, field string, value any) error
	StartRun(ctx context.Context, accountID int64, phase string) (runID int64, err error)
	FinishRun(ctx context.Context, runID int64, status string, stats Stats, errMsg string) error
	RawHash(ctx context.Context, accountID int64, externalID string) (hash string, exists bool, err error)
	InsertRaw(ctx context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) error
	UpdateRaw(ctx context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) error
}

// Clients bundles the per-account API clients.
type Clients struct {
	Gmail    *GmailClient
	Calendar *CalendarClient
}

// ClientFactory builds the per-account clients (production: oauth2 token
// source from the decrypted refresh token; tests: fake-server clients).
type ClientFactory func(ctx context.Context, acct Account) (Clients, error)

// IngestGmail is the raw-first gmail phase for one account.
func IngestGmail(ctx context.Context, gc *GmailClient, sink Sink, acct Account, cfg Config) (Stats, error) {
	var stats Stats
	runID, err := sink.StartRun(ctx, acct.ID, "gmail")
	if err != nil {
		return stats, fmt.Errorf("start gmail run: %w", err)
	}
	fail := func(cause error) (Stats, error) {
		_ = sink.FinishRun(ctx, runID, "error", stats, cause.Error())
		return stats, cause
	}

	cur, err := sink.Cursor(ctx, acct.ID)
	if err != nil {
		return fail(fmt.Errorf("read cursor: %w", err))
	}

	overlap := cfg.Overlap
	if overlap == 0 {
		overlap = DefaultOverlap
	}
	var afterSec int64
	if cfg.Full || cur.GmailInternalDateMS == 0 {
		afterSec = cfg.now().Add(-cfg.backfill()).Unix()
	} else {
		afterSec = (cur.GmailInternalDateMS - overlap.Milliseconds()) / 1000
	}
	// Skip drafts and chats at fetch time (working state, not communications).
	q := fmt.Sprintf("after:%d -in:draft -in:chats", afterSec)

	maxInternal := cur.GmailInternalDateMS
	pageToken := ""
	for {
		items, next, err := gc.ListMessages(ctx, q, pageToken)
		if err != nil {
			return fail(fmt.Errorf("list messages: %w", err))
		}
		stats.GmailListed += len(items)
		for _, it := range items {
			raw, err := gc.GetMessage(ctx, it.ID)
			if err != nil {
				return fail(fmt.Errorf("get message %s: %w", it.ID, err))
			}
			stats.GmailFetched++
			if err := upsertRaw(ctx, sink, acct.ID, "gmail:"+it.ID, raw, &stats); err != nil {
				return fail(err)
			}
			var meta struct {
				InternalDate string `json:"internalDate"`
			}
			if json.Unmarshal(raw, &meta) == nil {
				if ms, err := parseInt64(meta.InternalDate); err == nil && ms > maxInternal {
					maxInternal = ms
				}
			}
		}
		if next == "" {
			break
		}
		pageToken = next
	}

	if maxInternal > cur.GmailInternalDateMS {
		cur.GmailInternalDateMS = maxInternal
		if err := sink.SaveCursor(ctx, acct.ID, cur); err != nil {
			return fail(fmt.Errorf("save cursor: %w", err))
		}
	}
	if err := sink.FinishRun(ctx, runID, "ok", stats, ""); err != nil {
		return stats, fmt.Errorf("finish gmail run: %w", err)
	}
	return stats, nil
}

// IngestCalendar is the raw-first calendar phase for one account: initial
// window sync capturing nextSyncToken, then token-driven increments; HTTP 410
// drops the token and re-windows.
func IngestCalendar(ctx context.Context, cc *CalendarClient, sink Sink, acct Account, cfg Config) (Stats, error) {
	var stats Stats
	runID, err := sink.StartRun(ctx, acct.ID, "calendar")
	if err != nil {
		return stats, fmt.Errorf("start calendar run: %w", err)
	}
	fail := func(cause error) (Stats, error) {
		_ = sink.FinishRun(ctx, runID, "error", stats, cause.Error())
		return stats, cause
	}

	cur, err := sink.Cursor(ctx, acct.ID)
	if err != nil {
		return fail(fmt.Errorf("read cursor: %w", err))
	}

	now := cfg.now()
	timeMin := now.Add(-CalendarWindowPast).Format(time.RFC3339)
	timeMax := now.Add(CalendarWindowFuture).Format(time.RFC3339)

	syncToken := cur.CalendarSyncToken
	if cfg.Full {
		syncToken = ""
	}

	newToken, n, err := drainCalendar(ctx, cc, sink, acct, syncToken, timeMin, timeMax, &stats)
	if err == errSyncTokenGone {
		// full-resync recipe: drop the token, re-window
		newToken, n, err = drainCalendar(ctx, cc, sink, acct, "", timeMin, timeMax, &stats)
	}
	if err != nil {
		return fail(fmt.Errorf("list events: %w", err))
	}
	stats.CalendarListed += n

	if newToken != "" && newToken != cur.CalendarSyncToken {
		// Field-scoped, never the whole blob (SWT-24 criterion 19): between
		// this pass's cursor read and now, the resident watch loop may have
		// advanced imap_folders, and writing the stale snapshot back would
		// roll the mail position under it — skipped mail is a delivery
		// confirmation that never lands (invariant 5). Same guard the bridge
		// path has carried since its calendar ingest shipped.
		if err := sink.SaveCursorField(ctx, acct.ID, "calendar_sync_token", newToken); err != nil {
			return fail(fmt.Errorf("save cursor: %w", err))
		}
	}
	if err := sink.FinishRun(ctx, runID, "ok", stats, ""); err != nil {
		return stats, fmt.Errorf("finish calendar run: %w", err)
	}
	return stats, nil
}

func drainCalendar(ctx context.Context, cc *CalendarClient, sink Sink, acct Account, syncToken, timeMin, timeMax string, stats *Stats) (nextSync string, listed int, err error) {
	pageToken := ""
	for {
		page, err := cc.ListEvents(ctx, syncToken, timeMin, timeMax, pageToken)
		if err != nil {
			return "", listed, err
		}
		for _, item := range page.Items {
			var meta struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(item, &meta); err != nil || meta.ID == "" {
				return "", listed, fmt.Errorf("calendar item missing id: %.100s", item)
			}
			listed++
			if err := upsertRaw(ctx, sink, acct.ID, "calendar:"+meta.ID, item, stats); err != nil {
				return "", listed, err
			}
		}
		if page.NextPageToken == "" {
			return page.NextSyncToken, listed, nil
		}
		pageToken = page.NextPageToken
	}
}

// upsertRaw is the shared hash-compare decision (insert / update+reset /
// unchanged) — same idiom as upworkcrm, in the phase for unit testability.
func upsertRaw(ctx context.Context, sink Sink, accountID int64, externalID string, raw json.RawMessage, stats *Stats) error {
	h, err := chash.ContentHash(raw)
	if err != nil {
		return fmt.Errorf("hash %s: %w", externalID, err)
	}
	stored, exists, err := sink.RawHash(ctx, accountID, externalID)
	if err != nil {
		return fmt.Errorf("read stored hash for %s: %w", externalID, err)
	}
	switch {
	case !exists:
		if err := sink.InsertRaw(ctx, accountID, externalID, raw, h); err != nil {
			return fmt.Errorf("insert raw %s: %w", externalID, err)
		}
		stats.RawInserted++
	case stored == h:
		stats.RawUnchanged++
	default:
		if err := sink.UpdateRaw(ctx, accountID, externalID, raw, h); err != nil {
			return fmt.Errorf("update raw %s: %w", externalID, err)
		}
		stats.RawUpdated++
	}
	return nil
}

func parseInt64(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

// Run ingests every provider='google' account: gmail phase then calendar
// phase, per account. It errors if no accounts exist ("run google-auth add
// first"). Normalization is the separate Normalize phase.
func Run(ctx context.Context, sink *PGSink, factory ClientFactory, cfg Config) (Stats, error) {
	var total Stats

	accounts, err := sink.ListAccounts(ctx)
	if err != nil {
		return total, fmt.Errorf("list google accounts: %w", err)
	}
	if len(accounts) == 0 {
		return total, fmt.Errorf("no provider='google' accounts exist; run google-auth add first")
	}

	matched := false
	for _, acct := range accounts {
		if cfg.AccountEmail != "" && !strings.EqualFold(acct.Email, cfg.AccountEmail) {
			continue
		}
		matched = true
		clients, err := factory(ctx, acct)
		if err != nil {
			return total, fmt.Errorf("build clients for %s: %w", acct.Email, err)
		}
		gs, err := IngestGmail(ctx, clients.Gmail, sink, acct, cfg)
		total.add(gs)
		if err != nil {
			return total, fmt.Errorf("gmail ingest for %s: %w", acct.Email, err)
		}
		cs, err := IngestCalendar(ctx, clients.Calendar, sink, acct, cfg)
		total.add(cs)
		if err != nil {
			return total, fmt.Errorf("calendar ingest for %s: %w", acct.Email, err)
		}
	}
	if cfg.AccountEmail != "" && !matched {
		return total, fmt.Errorf("no google account matches --account %s", cfg.AccountEmail)
	}
	return total, nil
}
