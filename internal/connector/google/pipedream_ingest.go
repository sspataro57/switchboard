package google

// The Pipedream calendar snapshot ingest (SWT-27): one poll, envelope
// verification, per-account attribution by IDENTITY, wholesale per-entry
// validation BEFORE any raw write, raw-first upserts, and the snapshot applied
// as a windowed REPLACEMENT via the existing SupersedeAbsentCalendar.
//
// Why validation is wholesale and pre-write (the sharpest new risk of a
// third-party transport): Normalize returns on the FIRST unparseable calendar
// item, and the connector main returns on that error BEFORE ObserveOutbound
// and the capture pass run — so one reshaped event object stored raw would
// stall mail normalization on every pass until a human supersedes the row.
// Nothing gets stored for an account until every one of its items has parsed
// through NormalizeCalendarEvent, the SAME pure mapper Normalize will run.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// CalendarSnapshotSink is Sink + LockAccount + SupersedeAbsentCalendar. The
// cursor methods are inherited from Sink but MUST NOT be called: this
// transport has no sync token, every poll is a full snapshot, and the cursor
// blob also holds imap_folders — which the resident watch loop moves
// underneath any calendar pass.
type CalendarSnapshotSink interface {
	Sink
	LockAccount(ctx context.Context, accountID int64) (release func(), ok bool, err error)
	SupersedeAbsentCalendar(ctx context.Context, accountID int64, keep []string,
		windowFrom, windowTo time.Time) (int, error)
	// ConfirmObservedCalendarDeliveries closes the loop for booked blocks the
	// verified snapshot carries (SWT-28 criterion 26): a send-time-recorded
	// block short-circuits on content_hash, so Normalize's confirm hook never
	// sees it — the poll's observation is the evidence.
	ConfirmObservedCalendarDeliveries(ctx context.Context, accountID int64, present []string) error
}

// pipedreamEventMeta is the slice of an event this layer itself inspects; the
// full shape check is NormalizeCalendarEvent's.
type pipedreamEventMeta struct {
	ID         string   `json:"id"`
	Recurrence []string `json:"recurrence"`
}

// RunPipedreamCalendar polls once for every in-scope account and applies each
// verified entry as a windowed snapshot replacement. One failing account never
// aborts the others; the pass fails only when NONE succeeded (or the whole
// poll was refused).
func RunPipedreamCalendar(ctx context.Context, source PipedreamCalendarSource,
	sink CalendarSnapshotSink, accounts []Account, cfg Config) (Stats, error) {
	total := Stats{CalendarSource: "pipedream"}
	if len(accounts) == 0 {
		return total, nil
	}

	now := cfg.now()
	windowFrom := now.Add(-CalendarWindowPast)
	windowTo := now.Add(CalendarWindowFuture)
	req := PipedreamCalendarRequest{
		SchemaVersion: PipedreamCalendarSchemaVersion,
		TimeMin:       windowFrom.Format(time.RFC3339),
		TimeMax:       windowTo.Format(time.RFC3339),
		Calendars:     make([]string, 0, len(accounts)),
	}
	for _, acct := range accounts {
		req.Calendars = append(req.Calendars, acct.Email)
	}

	resp, fetchErr := source.FetchCalendars(ctx, req)

	// Whole-poll verification. Any failure here produces an ERROR run for
	// every in-scope account: we attempted a poll, and that is a calendar
	// fact — an ok run is what would let propose_slots answer from a busy set
	// nobody fetched.
	wholePollErr := fetchErr
	if wholePollErr == nil && resp.SchemaVersion != PipedreamCalendarSchemaVersion {
		wholePollErr = fmt.Errorf("pipedream calendar response schema_version %d, want %d — the envelope is a "+
			"contract with a third party and an unknown version may reshape the fields the refusals read",
			resp.SchemaVersion, PipedreamCalendarSchemaVersion)
	}
	if wholePollErr == nil && (resp.TimeMin != req.TimeMin || resp.TimeMax != req.TimeMax) {
		// THE ECHO CHECK. Every poll is a full snapshot applied as a
		// replacement over [TimeMin, TimeMax); a workflow that quietly answers
		// a narrower window than asked would have every event outside it
		// superseded on the first poll — silent, and gone from the busy set.
		wholePollErr = fmt.Errorf("pipedream calendar response echoed window %s..%s, want the requested %s..%s; "+
			"refusing before any replacement is applied", resp.TimeMin, resp.TimeMax, req.TimeMin, req.TimeMax)
	}
	claimed := make(map[string]bool, len(accounts))
	for _, acct := range accounts {
		claimed[strings.ToLower(acct.Email)] = true
	}
	if wholePollErr == nil {
		// A declared count that disagrees with its array is evidence of loss
		// IN TRANSIT — and transit is shared by every entry in the response,
		// so the whole snapshot is suspect: no account may be replaced from
		// it. (The other per-entry failures — a status=error, an absent
		// account, a recurrence rule — are per-calendar facts and fail only
		// their own account.) Blast radius, stated because someone will hit
		// it at 9am: one miscounted CLAIMED entry errors every account's run,
		// and after AVAIL_MAX_SYNC_AGE propose_slots refuses for everyone —
		// fail-closed on purpose. Entries for calendars no in-scope account
		// claims are EXCLUDED from this scan (criterion 7 says strangers are
		// ignored); a stranger's malformed bookkeeping must not take the real
		// calendars down with it.
		for _, entry := range resp.Calendars {
			if !claimed[strings.ToLower(entry.CalendarID)] {
				continue
			}
			if entry.Status == "ok" && entry.EventCount != nil && *entry.EventCount != len(entry.Events) {
				wholePollErr = fmt.Errorf("pipedream entry for %s declares %d events but carries %d; a "+
					"snapshot shorter than its own count lost events in transit, and transit is shared — "+
					"refusing the whole poll before any replacement is applied",
					entry.CalendarID, *entry.EventCount, len(entry.Events))
				break
			}
		}
	}
	if wholePollErr != nil {
		for _, acct := range accounts {
			pipedreamErrorRun(ctx, sink, acct, &total, wholePollErr.Error())
		}
		return total, wholePollErr
	}

	// Attribution by IDENTITY, never by position: the response entries are
	// indexed by lower-cased calendar_id, and the loop below iterates OUR
	// in-scope list. A calendar no account claims is ignored (and named): it
	// means the workflow is wired to an account switchboard does not know.
	entries := make(map[string]PipedreamCalendarEntry, len(resp.Calendars))
	for _, entry := range resp.Calendars {
		entries[strings.ToLower(entry.CalendarID)] = entry
	}
	var unclaimed []string
	for key, entry := range entries {
		if !claimed[key] {
			unclaimed = append(unclaimed, entry.CalendarID)
		}
	}
	sort.Strings(unclaimed)
	if len(unclaimed) > 0 {
		fmt.Printf("calendar: pipedream response carried %d calendar(s) no in-scope account claims: %s\n",
			len(unclaimed), strings.Join(unclaimed, ", "))
	}

	var firstErr error
	succeeded := 0
	for _, acct := range accounts {
		release, ok, err := sink.LockAccount(ctx, acct.ID)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !ok {
			// Another pass is doing the same work; no run row — a second row
			// would be a freshness signal for a poll this process never made.
			total.AccountsBusy++
			continue
		}
		perr := pipedreamAccountPass(ctx, sink, acct, entries, windowFrom, windowTo, &total)
		release()
		if perr != nil {
			fmt.Printf("calendar: account %s failed: %v\n", acct.Email, perr)
			if firstErr == nil {
				firstErr = perr
			}
			continue
		}
		succeeded++
	}
	if succeeded == 0 && firstErr != nil {
		return total, firstErr
	}
	return total, nil
}

// pipedreamErrorRun records a failed attempt for one account: lock → start →
// finish(error) → unlock. A busy account is skipped silently — the holder is
// doing the same work.
func pipedreamErrorRun(ctx context.Context, sink CalendarSnapshotSink, acct Account, total *Stats, reason string) {
	release, ok, err := sink.LockAccount(ctx, acct.ID)
	if err != nil || !ok {
		if !ok && err == nil {
			total.AccountsBusy++
		}
		return
	}
	defer release()
	runID, err := sink.StartRun(ctx, acct.ID, "calendar")
	if err != nil {
		return
	}
	_ = sink.FinishRun(ctx, runID, "error", Stats{CalendarSource: "pipedream"}, reason)
}

// pipedreamAccountPass verifies, writes and supersedes ONE account's entry,
// under the caller's lock, with its own run row.
func pipedreamAccountPass(ctx context.Context, sink CalendarSnapshotSink, acct Account,
	entries map[string]PipedreamCalendarEntry, windowFrom, windowTo time.Time, total *Stats) error {
	runID, err := sink.StartRun(ctx, acct.ID, "calendar")
	if err != nil {
		return fmt.Errorf("start calendar run for %s: %w", acct.Email, err)
	}
	stats := Stats{CalendarSource: "pipedream"}
	fail := func(cause error) error {
		_ = sink.FinishRun(ctx, runID, "error", stats, cause.Error())
		return cause
	}

	entry, present := entries[strings.ToLower(acct.Email)]
	if !present {
		// Never an ok run, never silence: an absent account is what a
		// disconnected Google account in Pipedream looks like, and an ok run
		// would be a freshness signal for a calendar the workflow no longer
		// returns.
		return fail(fmt.Errorf("pipedream response carries no entry for %s; reconnect that Google account "+
			"in the workflow", acct.Email))
	}
	if entry.Status != "ok" {
		return fail(fmt.Errorf("pipedream entry for %s reports status %q: %s", acct.Email, entry.Status, entry.Error))
	}
	if entry.EventCount == nil {
		return fail(fmt.Errorf("pipedream entry for %s omits event_count; an absent count cannot vouch for a "+
			"snapshot, and with zero events it would pass as a verified empty calendar", acct.Email))
	}
	if *entry.EventCount != len(entry.Events) {
		// DEFENCE IN DEPTH, normally unreachable: a mismatch on a CLAIMED ok
		// entry is escalated to a whole-poll refusal before this loop runs
		// (transit is shared). This restatement exists so the per-account path
		// stays safe if that escalation is ever narrowed — do not read it as
		// the live guard, and do not delete the escalation believing this
		// holds the line alone.
		return fail(fmt.Errorf("pipedream entry for %s declares %d events but carries %d; a snapshot shorter "+
			"than its own count lost events in transit, and applying it would supersede every one of them",
			acct.Email, *entry.EventCount, len(entry.Events)))
	}
	if len(entry.Events) >= PipedreamMaxEvents {
		return fail(fmt.Errorf("pipedream entry for %s carries %d events, at or over the %d cap; an entry at "+
			"the cap is indistinguishable from a truncated one", acct.Email, len(entry.Events), PipedreamMaxEvents))
	}

	// Wholesale validation BEFORE any write: every event must parse through
	// the SAME mapper Normalize will run, and must be a single instance
	// (non-empty `recurrence` is the proof the workflow did NOT query with
	// singleEvents=true — one unexpanded series is one raw row standing for
	// every future instance).
	type validated struct {
		externalID string
		raw        json.RawMessage
	}
	items := make([]validated, 0, len(entry.Events))
	for i, raw := range entry.Events {
		var meta pipedreamEventMeta
		if err := json.Unmarshal(raw, &meta); err != nil {
			return fail(fmt.Errorf("pipedream entry for %s: event %d is not an object: %v", acct.Email, i, err))
		}
		if len(meta.Recurrence) > 0 {
			return fail(fmt.Errorf("pipedream entry for %s: event %q carries a recurrence rule; the workflow "+
				"must query with singleEvents=true or the busy set holds one instance of a standing series",
				acct.Email, meta.ID))
		}
		if _, err := NormalizeCalendarEvent(raw); err != nil {
			return fail(fmt.Errorf("pipedream entry for %s: event %d would stall Normalize (and with it mail "+
				"normalization, outbound observation and the capture pass): %v", acct.Email, i, err))
		}
		if strings.TrimSpace(meta.ID) == "" {
			return fail(fmt.Errorf("pipedream entry for %s: event %d has no id", acct.Email, i))
		}
		items = append(items, validated{externalID: CalendarExternalID(meta.ID), raw: raw})
	}

	// Raw-first: provider JSON verbatim, content_hash, before anything
	// normalizes. The content_hash short-circuit is what makes a full
	// snapshot every 20 minutes cost no database churn.
	present2 := make([]string, 0, len(items))
	for _, item := range items {
		if err := upsertRaw(ctx, sink, acct.ID, item.externalID, item.raw, &stats); err != nil {
			return fail(err)
		}
		present2 = append(present2, item.externalID)
	}
	stats.CalendarListed = len(items)

	if len(present2) == 0 {
		// A VERIFIED empty snapshot: we looked, and the answer was valid. The
		// sink refuses an empty keep by design and that refusal must never be
		// reached — erroring here would refuse propose_slots forever for a
		// genuinely empty calendar. The stale events we hold keep contributing
		// busy time: over-busy, the conservative direction, self-healing on
		// the first non-empty snapshot. Counted and printed, never silent.
		stats.CalendarEmptySnapshot = 1
		fmt.Printf("calendar: %s returned a verified EMPTY snapshot; keeping whatever in-window observations "+
			"are already stored — stale events stay busy until the first non-empty poll\n", acct.Email)
	} else {
		superseded, err := sink.SupersedeAbsentCalendar(ctx, acct.ID, present2, windowFrom, windowTo)
		if err != nil {
			return fail(fmt.Errorf("apply pipedream snapshot replacement for %s: %w", acct.Email, err))
		}
		stats.CalendarSuperseded = superseded
		// SWT-28 criterion 26: confirm every booked block this verified
		// snapshot observed (see the sink method's comment for why Normalize's
		// hook alone cannot — the send-time record short-circuits the hash).
		if err := sink.ConfirmObservedCalendarDeliveries(ctx, acct.ID, present2); err != nil {
			return fail(fmt.Errorf("confirm observed calendar deliveries for %s: %w", acct.Email, err))
		}
	}

	if err := sink.FinishRun(ctx, runID, "ok", stats, ""); err != nil {
		return fmt.Errorf("finish calendar run for %s: %w", acct.Email, err)
	}
	total.add(stats)
	total.CalendarEmptySnapshot += stats.CalendarEmptySnapshot
	return nil
}
