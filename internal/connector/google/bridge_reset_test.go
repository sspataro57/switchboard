package google

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// These cover the three defects the adversarial review found in the bridge:
// a Calendar reset that advanced the token without replacing prior state,
// unserialized passes that could pair stale raw with a newer cursor, and
// definite pre-send failures reported as ambiguous.

func resetBridgeSource(reset bool, events ...string) *bridgeFixtureSource {
	raw := make([]json.RawMessage, 0, len(events))
	for _, id := range events {
		raw = append(raw, json.RawMessage(`{"id":"`+id+`","status":"confirmed",`+
			`"start":{"dateTime":"2026-07-01T10:00:00Z"},"end":{"dateTime":"2026-07-01T11:00:00Z"}}`))
	}
	account := BridgeAccount{Alias: "work", Email: "work@example.com"}
	return &bridgeFixtureSource{
		accounts: BridgeAccounts{SchemaVersion: BridgeSchemaVersion, Accounts: []BridgeAccount{account}},
		exports: map[string]BridgeExport{"work": {
			SchemaVersion: BridgeSchemaVersion, Account: account, Messages: []json.RawMessage{},
		}},
		calendarExports: map[string]BridgeCalendarExport{"work": {
			SchemaVersion: BridgeSchemaVersion, Account: account,
			Events:        raw,
			NextSyncToken: "token-after-reset",
			Reset:         reset,
		}},
	}
}

func TestBridgeCalendarReset_SupersedesEventsAbsentFromTheSnapshot(t *testing.T) {
	// The whole point: Google dropped our token, so the replacement snapshot is
	// the truth. An event deleted while we were blind is simply absent from it,
	// and without supersession it stays active forever — the fresh token means
	// no future delta ever mentions it again.
	var trace []string
	sink := newBridgeFixtureSink(&trace)
	sink.raw["calendar:kept"] = json.RawMessage(`{"id":"kept"}`)
	sink.raw["calendar:deleted-while-blind"] = json.RawMessage(`{"id":"deleted-while-blind"}`)

	if _, err := RunBridge(context.Background(), resetBridgeSource(true, "kept"), sink, Config{}); err != nil {
		t.Fatalf("RunBridge: %v", err)
	}
	if !sink.superseded["calendar:deleted-while-blind"] {
		t.Fatal("event absent from the reset snapshot was not superseded; it stays busy in availability forever")
	}
	if sink.superseded["calendar:kept"] {
		t.Fatal("event present in the reset snapshot was superseded")
	}
}

func TestBridgeCalendarReset_SupersedesBeforeAdvancingTheCursor(t *testing.T) {
	// Ordering is the invariant: a fresh token over unreplaced state is
	// unrepairable, so supersession must be durable first.
	var trace []string
	sink := newBridgeFixtureSink(&trace)
	sink.raw["calendar:gone"] = json.RawMessage(`{"id":"gone"}`)

	if _, err := RunBridge(context.Background(), resetBridgeSource(true, "kept"), sink, Config{}); err != nil {
		t.Fatalf("RunBridge: %v", err)
	}
	supersedeAt, saveAt := -1, -1
	for i, e := range trace {
		if strings.HasPrefix(e, "sink:supersede:") && supersedeAt < 0 {
			supersedeAt = i
		}
		if strings.HasPrefix(e, "sink:save:") {
			saveAt = i
		}
	}
	if supersedeAt < 0 || saveAt < 0 || supersedeAt > saveAt {
		t.Fatalf("supersede at %d, cursor save at %d; supersede must come first\ntrace: %v", supersedeAt, saveAt, trace)
	}
}

func TestBridgeCalendarReset_FailureLeavesTheOldToken(t *testing.T) {
	var trace []string
	sink := newBridgeFixtureSink(&trace)
	sink.raw["calendar:gone"] = json.RawMessage(`{"id":"gone"}`)
	sink.supersedeErr = errors.New("supersede exploded")

	_, err := RunBridge(context.Background(), resetBridgeSource(true, "kept"), sink, Config{})
	if err == nil {
		t.Fatal("RunBridge succeeded despite a failed reset replacement")
	}
	for _, saved := range sink.savedCursors {
		if saved.CalendarSyncToken == "token-after-reset" {
			t.Fatal("cursor advanced after the replacement failed; the next pass can never repair the stale events")
		}
	}
}

func TestBridgeCalendarNonReset_DoesNotSupersede(t *testing.T) {
	// An ordinary incremental delta carries only changes; absence means
	// unchanged, not deleted. Superseding here would erase live events.
	var trace []string
	sink := newBridgeFixtureSink(&trace)
	sink.raw["calendar:untouched"] = json.RawMessage(`{"id":"untouched"}`)

	stats, err := RunBridge(context.Background(), resetBridgeSource(false, "changed"), sink, Config{})
	if err != nil {
		t.Fatalf("RunBridge: %v", err)
	}
	if sink.superseded["calendar:untouched"] {
		t.Fatal("incremental export superseded an event it simply did not mention")
	}
	if stats.CalendarResets != 0 {
		t.Fatalf("CalendarResets = %d on a non-reset export, want 0", stats.CalendarResets)
	}
}

func TestBridgeReset_ReportedInStats(t *testing.T) {
	// The reset used to be parsed and dropped: no stat, no log, invisible.
	var trace []string
	sink := newBridgeFixtureSink(&trace)
	sink.raw["calendar:gone"] = json.RawMessage(`{"id":"gone"}`)

	stats, err := RunBridge(context.Background(), resetBridgeSource(true, "kept"), sink, Config{})
	if err != nil {
		t.Fatalf("RunBridge: %v", err)
	}
	if stats.CalendarResets != 1 || stats.CalendarSuperseded != 1 {
		t.Fatalf("stats resets=%d superseded=%d, want 1/1", stats.CalendarResets, stats.CalendarSuperseded)
	}
}

func TestBridgeAccount_SkippedWhenAnotherPassHoldsTheLock(t *testing.T) {
	// Two interleaved passes can pair an older export's raw with a newer run's
	// token. Skipping is correct: the holder is doing the same work.
	var trace []string
	sink := newBridgeFixtureSink(&trace)
	sink.lockBusy = true

	stats, err := RunBridge(context.Background(), resetBridgeSource(false, "e1"), sink, Config{})
	if err != nil {
		t.Fatalf("RunBridge with a busy account: %v", err)
	}
	if stats.AccountsBusy != 1 {
		t.Fatalf("AccountsBusy = %d, want 1", stats.AccountsBusy)
	}
	if len(sink.inserts) != 0 || len(sink.savedCursors) != 0 {
		t.Fatal("a locked-out pass still wrote raw or advanced a cursor")
	}
}

func TestBridgeAccount_LockReleasedAfterThePass(t *testing.T) {
	var trace []string
	sink := newBridgeFixtureSink(&trace)
	if _, err := RunBridge(context.Background(), resetBridgeSource(false, "e1"), sink, Config{}); err != nil {
		t.Fatalf("RunBridge: %v", err)
	}
	if sink.locked != 0 {
		t.Fatalf("lock still held (%d) after the pass; the pooled connection leaks it", sink.locked)
	}
}

func TestBridgeCursor_SavesOneFieldAtATime(t *testing.T) {
	// Whole-blob writes are how one phase clobbers the other's newer value.
	var trace []string
	sink := newBridgeFixtureSink(&trace)
	if _, err := RunBridge(context.Background(), resetBridgeSource(false, "e1"), sink, Config{}); err != nil {
		t.Fatalf("RunBridge: %v", err)
	}
	for _, field := range sink.savedCursorFields {
		if field != "gmail_internal_date_ms" && field != "calendar_sync_token" {
			t.Fatalf("cursor written through unexpected field %q", field)
		}
	}
	if len(sink.savedCursorFields) == 0 {
		t.Fatal("no field-scoped cursor write happened")
	}
}

func TestBridgeSender_DefinitePreSendFailuresAreRejections(t *testing.T) {
	// These provably never reached Gmail, so the reservation must be released
	// or the delivery is stuck needing manual SQL to re-approve.
	var rejected *SendRejectedError

	unconfigured := &BridgeSender{}
	_, err := unconfigured.Send(context.Background(), "a@example.com", []byte("x"), "")
	if !errors.As(err, &rejected) {
		t.Fatalf("unconfigured bridge = %v, want SendRejectedError", err)
	}
}

func TestBridgeNotRunError_IsDefiniteNonSend(t *testing.T) {
	// The classifier's contract, independent of how the failure was produced.
	notRun := &BridgeNotRunError{Op: "send-raw", Err: errors.New("no such file")}
	if !strings.Contains(notRun.Error(), "never ran") {
		t.Fatalf("BridgeNotRunError.Error() = %q, want it to say the leaf never ran", notRun.Error())
	}
	if errors.Unwrap(notRun) == nil {
		t.Fatal("BridgeNotRunError must unwrap to its cause")
	}
}

func TestBridgeCalendar_RecreatedEventRevivesASupersededRow(t *testing.T) {
	// The nastiest case in the reset design: Google reuses event ids. If a
	// superseded row kept its flag, a re-created event would be skipped as
	// unchanged (identical bytes) or updated while still superseded — either
	// way it never normalizes, and the fix for one silent-loss bug becomes
	// another one. Re-observation must revive the row.
	var trace []string
	sink := newBridgeFixtureSink(&trace)
	sink.raw["calendar:recurring"] = json.RawMessage(`{"id":"recurring"}`)

	// Pass 1: a reset without it — superseded.
	if _, err := RunBridge(context.Background(), resetBridgeSource(true, "other"), sink, Config{}); err != nil {
		t.Fatalf("reset pass: %v", err)
	}
	if !sink.superseded["calendar:recurring"] {
		t.Fatal("precondition failed: the event was not superseded by the reset")
	}

	// Pass 2: it comes back with the same id.
	if _, err := RunBridge(context.Background(), resetBridgeSource(false, "recurring"), sink, Config{}); err != nil {
		t.Fatalf("re-observation pass: %v", err)
	}
	if sink.superseded["calendar:recurring"] {
		t.Fatal("re-observed event is still superseded; it will never normalize again")
	}
}

// --- the invariant-4 assertions the re-review found missing -----------------

type exitStatusRunner struct{ err error }

func (r exitStatusRunner) Run(context.Context, string, string, []byte, bridgeCommandLimits) ([]byte, []byte, error) {
	return nil, []byte("leaf exploded"), r.err
}

func TestBridgeSender_ProcessRanThenFailed_StaysAmbiguous(t *testing.T) {
	// THE assertion invariant 4 rests on. An *exec.ExitError means the leaf ran
	// — it may have handed the mail to Gmail before dying. Calling that a
	// definite rejection releases the reservation and a re-approval sends the
	// message twice.
	ran := exec.Command("/bin/sh", "-c", "exit 3")
	exitErr := ran.Run() // produces a real *exec.ExitError
	sender := &BridgeSender{Bridge: &CommandBridge{
		binary: "/bin/true", runner: exitStatusRunner{err: exitErr}, limits: defaultBridgeCommandLimits,
	}}

	_, err := sender.Send(context.Background(), "me@example.com", []byte("mime"), "")
	if err == nil {
		t.Fatal("Send succeeded despite the leaf exiting non-zero")
	}
	var rejected *SendRejectedError
	if errors.As(err, &rejected) {
		t.Fatalf("a process that RAN was classified definite (%v); the reservation would be released and the mail could send twice", err)
	}
}

func TestBridgeSender_LeafReportsNotSent_IsDefinite(t *testing.T) {
	sender := &BridgeSender{Bridge: &CommandBridge{
		binary: "/bin/true", limits: defaultBridgeCommandLimits,
		runner: stubResponseRunner(`{"schema_version":1,"sent":false}`),
	}}
	_, err := sender.Send(context.Background(), "me@example.com", []byte("mime"), "")
	var rejected *SendRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("explicit sent=false = %v, want SendRejectedError so the delivery can be re-approved", err)
	}
}

func TestBridgeSender_OmittedSentStaysAmbiguous(t *testing.T) {
	// A version-skewed leaf that omits the field may still have sent.
	sender := &BridgeSender{Bridge: &CommandBridge{
		binary: "/bin/true", limits: defaultBridgeCommandLimits,
		runner: stubResponseRunner(`{"schema_version":1,"message_id":"m"}`),
	}}
	_, err := sender.Send(context.Background(), "me@example.com", []byte("mime"), "")
	var rejected *SendRejectedError
	if err == nil || errors.As(err, &rejected) {
		t.Fatalf("omitted sent = %v, want an ambiguous (untyped) error", err)
	}
}

func TestBridgeSender_ContradictorySentFalseWithMessageIDStaysAmbiguous(t *testing.T) {
	sender := &BridgeSender{Bridge: &CommandBridge{
		binary: "/bin/true", limits: defaultBridgeCommandLimits,
		runner: stubResponseRunner(`{"schema_version":1,"sent":false,"message_id":"m-1"}`),
	}}
	_, err := sender.Send(context.Background(), "me@example.com", []byte("mime"), "")
	var rejected *SendRejectedError
	if err == nil || errors.As(err, &rejected) {
		t.Fatalf("sent=false WITH a message id = %v, want ambiguous — the two claims contradict", err)
	}
}

func gmailExportSource(count int, watermark int64) *bridgeFixtureSource {
	account := BridgeAccount{Alias: "work", Email: "work@example.com"}
	messages := make([]json.RawMessage, 0, count)
	for i := 0; i < count; i++ {
		messages = append(messages, json.RawMessage(
			`{"id":"m`+itoaTest(i)+`","internalDate":"1784995200000"}`))
	}
	return &bridgeFixtureSource{
		accounts: BridgeAccounts{SchemaVersion: BridgeSchemaVersion, Accounts: []BridgeAccount{account}},
		exports: map[string]BridgeExport{"work": {
			SchemaVersion: BridgeSchemaVersion, Account: account,
			Messages: messages, MaxInternalDateMS: watermark,
		}},
	}
}

func TestBridgeGmail_ExportExactlyAtTheCapIsRefused(t *testing.T) {
	// Indistinguishable from a truncated export, and the cursor would jump past
	// whatever was dropped — permanently.
	var trace []string
	sink := newBridgeFixtureSink(&trace)
	_, err := RunBridge(context.Background(), gmailExportSource(BridgeMaxMessages, 1784995200000), sink, Config{})
	if err == nil {
		t.Fatal("an export landing exactly on the cap was accepted")
	}
	if len(sink.savedCursors) != 0 {
		t.Fatal("cursor advanced on a possibly-truncated export")
	}
}

func TestBridgeGmail_MissingWatermarkOnANonEmptyExportIsRefused(t *testing.T) {
	var trace []string
	sink := newBridgeFixtureSink(&trace)
	_, err := RunBridge(context.Background(), gmailExportSource(3, 0), sink, Config{})
	if err == nil {
		t.Fatal("a non-empty export without max_internal_date_ms was accepted")
	}
}

func TestBridgeGmail_WatermarkAheadOfWhatWasStoredIsRefused(t *testing.T) {
	var trace []string
	sink := newBridgeFixtureSink(&trace)
	// Leaf claims it exported through a later message than any it handed us.
	_, err := RunBridge(context.Background(), gmailExportSource(2, 1999999999999), sink, Config{})
	if err == nil {
		t.Fatal("a watermark ahead of the stored maximum was accepted; messages went missing between leaf and sink")
	}
	if len(sink.savedCursors) != 0 {
		t.Fatal("cursor advanced despite the watermark mismatch")
	}
}

func TestBridgeGmail_WatermarkBehindTheCursorIsNotAMismatch(t *testing.T) {
	// The negative case: an overlap re-read legitimately returns only messages
	// older than the cursor. Failing here would wedge ingestion forever.
	var trace []string
	sink := newBridgeFixtureSink(&trace)
	sink.cursors[100] = Cursor{GmailInternalDateMS: 1999999999999}
	if _, err := RunBridge(context.Background(), gmailExportSource(2, 1784995200000), sink, Config{}); err != nil {
		t.Fatalf("overlap re-read below the cursor was rejected: %v", err)
	}
}

func itoaTest(i int) string { return strconv.Itoa(i) }

func stubResponseRunner(response string) bridgeCommandRunner {
	return exitStatusRunner{}.withResponse(response)
}

type responseRunner struct{ response string }

func (r responseRunner) Run(context.Context, string, string, []byte, bridgeCommandLimits) ([]byte, []byte, error) {
	return []byte(r.response), nil, nil
}

func (exitStatusRunner) withResponse(response string) bridgeCommandRunner {
	return responseRunner{response: response}
}
