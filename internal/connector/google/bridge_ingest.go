package google

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	BridgeMaxMessages = 10_000
	BridgeMaxEvents   = 10_000
)

type BridgeSource interface {
	Accounts(ctx context.Context) (BridgeAccounts, error)
	Export(ctx context.Context, request BridgeExportRequest) (BridgeExport, error)
	CalendarExport(ctx context.Context, request BridgeCalendarExportRequest) (BridgeCalendarExport, error)
}

type BridgeSink interface {
	Sink
	EnsureBridgeAccount(ctx context.Context, account BridgeAccount) (Account, error)
	// LockAccount serializes one account's whole pass; ok=false means another
	// run holds it and this one should skip rather than interleave.
	LockAccount(ctx context.Context, accountID int64) (release func(), ok bool, err error)
	// SaveCursorField writes one cursor key so Gmail and Calendar cannot
	// clobber each other inside the single sync_cursor blob.
	SaveCursorField(ctx context.Context, accountID int64, field string, value any) error
	// SupersedeAbsentCalendar applies a reset as a replacement, bounded to the
	// window the snapshot actually covers.
	SupersedeAbsentCalendar(ctx context.Context, accountID int64, keep []string, windowFrom, windowTo time.Time) (int, error)
}

// RunBridge is the local Gmail + Calendar connector path. It preserves the
// direct connector's cursors and raw-first hash/upsert semantics; Normalize is
// still a separate phase over the exact provider JSON captured here.
func RunBridge(ctx context.Context, source BridgeSource, sink BridgeSink, cfg Config) (Stats, error) {
	var total Stats
	discovered, err := source.Accounts(ctx)
	if err != nil {
		return total, fmt.Errorf("list Gmail bridge accounts: %w", err)
	}
	if err := validateBridgeVersion(discovered.SchemaVersion); err != nil {
		return total, err
	}
	if len(discovered.Accounts) == 0 {
		return total, fmt.Errorf("Gmail bridge returned no accounts")
	}

	matched := false
	for _, bridgeAccount := range discovered.Accounts {
		if cfg.AccountEmail != "" &&
			!strings.EqualFold(cfg.AccountEmail, bridgeAccount.Email) &&
			!strings.EqualFold(cfg.AccountEmail, bridgeAccount.Alias) {
			continue
		}
		matched = true
		if bridgeAccount.Alias == "" || bridgeAccount.Email == "" {
			return total, fmt.Errorf("Gmail bridge account is missing alias or email")
		}
		account, err := sink.EnsureBridgeAccount(ctx, bridgeAccount)
		if err != nil {
			return total, fmt.Errorf("ensure Gmail bridge account %s: %w", bridgeAccount.Alias, err)
		}
		stats, err := runBridgeAccount(ctx, source, sink, bridgeAccount, account, cfg)
		total.add(stats)
		if err != nil {
			return total, fmt.Errorf("Gmail bridge ingest for %s: %w", bridgeAccount.Alias, err)
		}
	}
	if cfg.AccountEmail != "" && !matched {
		return total, fmt.Errorf("no Gmail bridge account matches --account %s", cfg.AccountEmail)
	}
	return total, nil
}

func runBridgeAccount(ctx context.Context, source BridgeSource, sink BridgeSink, bridgeAccount BridgeAccount, account Account, cfg Config) (Stats, error) {
	var total Stats
	// Gmail and Calendar are one pass under one lock: they share the cursor row
	// and interleaving them with another run can pair stale raw with a newer
	// token, which no later delta repairs.
	release, ok, err := sink.LockAccount(ctx, account.ID)
	if err != nil {
		return total, fmt.Errorf("lock account %d: %w", account.ID, err)
	}
	if !ok {
		total.AccountsBusy = 1
		return total, nil
	}
	defer release()

	gmailStats, err := runBridgeGmailAccount(ctx, source, sink, bridgeAccount, account, cfg)
	total.add(gmailStats)
	if err != nil {
		return total, err
	}
	calendarStats, err := runBridgeCalendarAccount(ctx, source, sink, bridgeAccount, account, cfg)
	total.add(calendarStats)
	if err != nil {
		return total, err
	}
	return total, nil
}

func runBridgeGmailAccount(ctx context.Context, source BridgeSource, sink BridgeSink, bridgeAccount BridgeAccount, account Account, cfg Config) (Stats, error) {
	var stats Stats
	runID, err := sink.StartRun(ctx, account.ID, "gmail")
	if err != nil {
		return stats, fmt.Errorf("start Gmail bridge run: %w", err)
	}
	fail := func(cause error) (Stats, error) {
		_ = sink.FinishRun(ctx, runID, "error", stats, cause.Error())
		return stats, cause
	}

	cursor, err := sink.Cursor(ctx, account.ID)
	if err != nil {
		return fail(fmt.Errorf("read Gmail bridge cursor: %w", err))
	}
	overlap := cfg.Overlap
	if overlap == 0 {
		overlap = DefaultOverlap
	}
	var afterUnix int64
	if cfg.Full || cursor.GmailInternalDateMS == 0 {
		afterUnix = cfg.now().Add(-cfg.backfill()).Unix()
	} else {
		afterUnix = (cursor.GmailInternalDateMS - overlap.Milliseconds()) / 1000
	}
	exported, err := source.Export(ctx, BridgeExportRequest{
		Account: bridgeAccount.Alias, AfterUnix: afterUnix, MaxMessages: BridgeMaxMessages,
	})
	if err != nil {
		return fail(fmt.Errorf("export Gmail observations: %w", err))
	}
	if err := validateBridgeVersion(exported.SchemaVersion); err != nil {
		return fail(err)
	}
	if !strings.EqualFold(exported.Account.Alias, bridgeAccount.Alias) ||
		!strings.EqualFold(exported.Account.Email, bridgeAccount.Email) {
		return fail(fmt.Errorf("Gmail bridge export identity does not match discovered account %s", bridgeAccount.Alias))
	}
	if len(exported.Messages) > BridgeMaxMessages {
		return fail(fmt.Errorf("Gmail bridge export exceeded %d messages", BridgeMaxMessages))
	}
	// A truncated export is otherwise undetectable, and the damage is
	// permanent: the cursor jumps to the newest message returned and the
	// unexported older ones are never asked for again. An export that lands
	// exactly on the cap is indistinguishable from a truncated one.
	if len(exported.Messages) == BridgeMaxMessages {
		return fail(fmt.Errorf("Gmail bridge export returned exactly the %d-message cap; refusing to advance the cursor past a possibly truncated export", BridgeMaxMessages))
	}

	stats.GmailListed = len(exported.Messages)
	maxInternal := cursor.GmailInternalDateMS
	for _, raw := range exported.Messages {
		var meta struct {
			ID           string `json:"id"`
			InternalDate string `json:"internalDate"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			return fail(fmt.Errorf("parse Gmail bridge message identity: %w", err))
		}
		if meta.ID == "" {
			return fail(fmt.Errorf("Gmail bridge message has no id"))
		}
		stats.GmailFetched++
		if err := upsertRaw(ctx, sink, account.ID, "gmail:"+meta.ID, raw, &stats); err != nil {
			return fail(err)
		}
		if millis, err := parseInt64(meta.InternalDate); err == nil && millis > maxInternal {
			maxInternal = millis
		}
	}
	// The leaf reports the high-water mark it exported through. Requiring it on
	// any non-empty export closes the hole where an omitted field (decoding to
	// zero) silently skips the check and lets a truncated export advance the
	// cursor past messages that were never stored.
	if len(exported.Messages) > 0 {
		if exported.MaxInternalDateMS == 0 {
			return fail(fmt.Errorf("Gmail bridge export of %d messages omitted max_internal_date_ms; refusing to advance the cursor",
				len(exported.Messages)))
		}
		// Only compare when the watermark is ahead of the cursor: an overlap
		// re-read legitimately returns only messages older than it.
		if exported.MaxInternalDateMS > cursor.GmailInternalDateMS &&
			exported.MaxInternalDateMS != maxInternal {
			return fail(fmt.Errorf("Gmail bridge reported max internal date %d but %d was stored; refusing to advance the cursor",
				exported.MaxInternalDateMS, maxInternal))
		}
	}
	if maxInternal > cursor.GmailInternalDateMS {
		// Field-scoped: a whole-blob write would clobber a Calendar token
		// saved by another writer since this pass read the cursor.
		if err := sink.SaveCursorField(ctx, account.ID, "gmail_internal_date_ms", maxInternal); err != nil {
			return fail(fmt.Errorf("save Gmail bridge cursor: %w", err))
		}
	}
	if err := sink.FinishRun(ctx, runID, "ok", stats, ""); err != nil {
		return stats, fmt.Errorf("finish Gmail bridge run: %w", err)
	}
	return stats, nil
}

func runBridgeCalendarAccount(ctx context.Context, source BridgeSource, sink BridgeSink, bridgeAccount BridgeAccount, account Account, cfg Config) (Stats, error) {
	var stats Stats
	runID, err := sink.StartRun(ctx, account.ID, "calendar")
	if err != nil {
		return stats, fmt.Errorf("start Calendar bridge run: %w", err)
	}
	fail := func(cause error) (Stats, error) {
		_ = sink.FinishRun(ctx, runID, "error", stats, cause.Error())
		return stats, cause
	}

	cursor, err := sink.Cursor(ctx, account.ID)
	if err != nil {
		return fail(fmt.Errorf("read Calendar bridge cursor: %w", err))
	}
	now := cfg.now()
	timeMin := now.Add(-calWindowPast).Format(time.RFC3339)
	timeMax := now.Add(calWindowFuture).Format(time.RFC3339)
	syncToken := cursor.CalendarSyncToken
	if cfg.Full {
		syncToken = ""
	}
	exported, err := source.CalendarExport(ctx, BridgeCalendarExportRequest{
		Account: bridgeAccount.Alias, SyncToken: syncToken,
		TimeMin: timeMin, TimeMax: timeMax, MaxEvents: BridgeMaxEvents,
	})
	if err != nil {
		return fail(fmt.Errorf("export Calendar observations: %w", err))
	}
	if err := validateBridgeVersion(exported.SchemaVersion); err != nil {
		return fail(err)
	}
	if !strings.EqualFold(exported.Account.Alias, bridgeAccount.Alias) ||
		!strings.EqualFold(exported.Account.Email, bridgeAccount.Email) {
		return fail(fmt.Errorf("Gmail bridge Calendar export identity does not match discovered account %s", bridgeAccount.Alias))
	}
	if strings.TrimSpace(exported.NextSyncToken) == "" {
		return fail(fmt.Errorf("Gmail bridge Calendar export has no next_sync_token"))
	}
	if len(exported.Events) > BridgeMaxEvents {
		return fail(fmt.Errorf("Gmail bridge Calendar export exceeded %d events", BridgeMaxEvents))
	}

	stats.CalendarListed = len(exported.Events)
	present := make([]string, 0, len(exported.Events))
	for _, raw := range exported.Events {
		var meta struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			return fail(fmt.Errorf("parse Gmail bridge Calendar event identity: %w", err))
		}
		if strings.TrimSpace(meta.ID) == "" {
			return fail(fmt.Errorf("Gmail bridge Calendar event has no id"))
		}
		externalID := "calendar:" + meta.ID
		present = append(present, externalID)
		if err := upsertRaw(ctx, sink, account.ID, externalID, raw, &stats); err != nil {
			return fail(err)
		}
	}

	// A reset is a REPLACEMENT, not an overlay. Google dropped our token, so
	// this snapshot is the whole truth for the window: anything absent was
	// deleted while we were blind. Upserting only what came back and then
	// saving the fresh token would strand those events as permanently active —
	// no future delta mentions them again. Supersede BEFORE the cursor moves,
	// so a failure here leaves the old token and the next pass retries.
	if exported.Reset {
		stats.CalendarResets = 1
		// Bounded to the window this snapshot covers. Account-wide would cancel
		// every event older than the 30-day past window on the first reset —
		// their absence says nothing, they were never asked for.
		superseded, err := sink.SupersedeAbsentCalendar(ctx, account.ID, present,
			now.Add(-calWindowPast), now.Add(calWindowFuture))
		if err != nil {
			return fail(fmt.Errorf("apply Calendar reset replacement: %w", err))
		}
		stats.CalendarSuperseded = superseded
	}

	if exported.NextSyncToken != cursor.CalendarSyncToken {
		// Field-scoped, mirroring Gmail: never rewrite the sibling field.
		if err := sink.SaveCursorField(ctx, account.ID, "calendar_sync_token", exported.NextSyncToken); err != nil {
			return fail(fmt.Errorf("save Calendar bridge cursor: %w", err))
		}
	}
	if err := sink.FinishRun(ctx, runID, "ok", stats, ""); err != nil {
		return stats, fmt.Errorf("finish Calendar bridge run: %w", err)
	}
	return stats, nil
}
