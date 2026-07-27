package google

// Greenfield ingest surface imposed by docs/tickets/gmail-local-bridge_SPEC.md:
//
//   type BridgeSource interface {
//       Accounts(context.Context) (BridgeAccounts, error)
//       Export(context.Context, BridgeExportRequest) (BridgeExport, error)
//   }
//   type BridgeSink interface {
//       Sink
//       EnsureBridgeAccount(context.Context, BridgeAccount) (Account, error)
//   }
//   func RunBridge(context.Context, BridgeSource, BridgeSink, Config) (Stats, error)
//
// Keeping BridgeSink as the existing raw Sink plus one token-free account
// discovery method mechanically reuses the current cursor/content-hash path.
// RunBridge is Gmail-only; Normalize remains the existing separate phase.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

type bridgeFixtureSource struct {
	accounts           BridgeAccounts
	exports            map[string]BridgeExport
	exportErrs         map[string]error
	requests           []BridgeExportRequest
	calendarExports    map[string]BridgeCalendarExport
	calendarExportErrs map[string]error
	calendarRequests   []BridgeCalendarExportRequest
	trace              *[]string
}

func (s *bridgeFixtureSource) Accounts(context.Context) (BridgeAccounts, error) {
	if s.trace != nil {
		*s.trace = append(*s.trace, "source:accounts")
	}
	return s.accounts, nil
}

func (s *bridgeFixtureSource) Export(_ context.Context, request BridgeExportRequest) (BridgeExport, error) {
	s.requests = append(s.requests, request)
	if s.trace != nil {
		*s.trace = append(*s.trace, "source:export:"+request.Account)
	}
	if err := s.exportErrs[request.Account]; err != nil {
		return BridgeExport{}, err
	}
	return s.exports[request.Account], nil
}

func (s *bridgeFixtureSource) CalendarExport(_ context.Context, request BridgeCalendarExportRequest) (BridgeCalendarExport, error) {
	s.calendarRequests = append(s.calendarRequests, request)
	if s.trace != nil {
		*s.trace = append(*s.trace, "source:calendar-export:"+request.Account)
	}
	if err := s.calendarExportErrs[request.Account]; err != nil {
		return BridgeCalendarExport{}, err
	}
	if exported, ok := s.calendarExports[request.Account]; ok {
		return exported, nil
	}
	var account BridgeAccount
	for _, discovered := range s.accounts.Accounts {
		if strings.EqualFold(discovered.Alias, request.Account) {
			account = discovered
			break
		}
	}
	next := request.SyncToken
	if next == "" {
		next = "calendar-empty-" + request.Account
	}
	return BridgeCalendarExport{
		SchemaVersion: BridgeSchemaVersion, Account: account,
		Events: []json.RawMessage{}, NextSyncToken: next,
	}, nil
}

type bridgeRawWrite struct {
	accountID  int64
	externalID string
	raw        json.RawMessage
	hash       string
}

type bridgeFixtureSink struct {
	trace *[]string

	accountsByEmail map[string]Account
	nextAccountID   int64
	cursors         map[int64]Cursor
	stored          map[string]string
	raw             map[string]json.RawMessage
	inserts         []bridgeRawWrite
	updates         []bridgeRawWrite
	saves           []Account
	savedCursors    []Cursor
	finishStatuses  []string
	failInsertID    string

	// review-fix fixtures
	lockBusy          bool
	lockErr           error
	locked            int
	supersedeErr      error
	superseded        map[string]bool
	savedCursorFields []string
}

func newBridgeFixtureSink(trace *[]string) *bridgeFixtureSink {
	return &bridgeFixtureSink{
		trace: trace, accountsByEmail: map[string]Account{}, nextAccountID: 100,
		cursors: map[int64]Cursor{}, stored: map[string]string{}, raw: map[string]json.RawMessage{},
	}
}

func (s *bridgeFixtureSink) event(value string) {
	if s.trace != nil {
		*s.trace = append(*s.trace, value)
	}
}

func (s *bridgeFixtureSink) EnsureBridgeAccount(_ context.Context, discovered BridgeAccount) (Account, error) {
	email := strings.ToLower(discovered.Email)
	s.event("sink:ensure:" + discovered.Alias + ":" + email)
	if account, ok := s.accountsByEmail[email]; ok {
		return account, nil
	}
	s.nextAccountID++
	account := Account{ID: s.nextAccountID, Email: email}
	s.accountsByEmail[email] = account
	return account, nil
}

func (s *bridgeFixtureSink) Cursor(_ context.Context, accountID int64) (Cursor, error) {
	s.event(fmt.Sprintf("sink:cursor:%d", accountID))
	return s.cursors[accountID], nil
}

func (s *bridgeFixtureSink) SaveCursor(_ context.Context, accountID int64, cursor Cursor) error {
	s.event(fmt.Sprintf("sink:save:%d", accountID))
	s.cursors[accountID] = cursor
	s.saves = append(s.saves, Account{ID: accountID, Email: fmt.Sprintf("%d", cursor.GmailInternalDateMS)})
	s.savedCursors = append(s.savedCursors, cursor)
	return nil
}

func (s *bridgeFixtureSink) StartRun(_ context.Context, accountID int64, phase string) (int64, error) {
	s.event(fmt.Sprintf("sink:start:%d:%s", accountID, phase))
	return accountID + 1000, nil
}

func (s *bridgeFixtureSink) FinishRun(_ context.Context, runID int64, status string, _ Stats, _ string) error {
	s.event(fmt.Sprintf("sink:finish:%d:%s", runID, status))
	s.finishStatuses = append(s.finishStatuses, status)
	return nil
}

func bridgeRawKey(accountID int64, externalID string) string {
	return fmt.Sprintf("%d/%s", accountID, externalID)
}

func (s *bridgeFixtureSink) RawHash(_ context.Context, accountID int64, externalID string) (string, bool, error) {
	if s.superseded[externalID] {
		return "", false, nil
	}
	key := bridgeRawKey(accountID, externalID)
	s.event("sink:hash:" + key)
	hash, ok := s.stored[key]
	return hash, ok, nil
}

func (s *bridgeFixtureSink) InsertRaw(_ context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) error {
	delete(s.superseded, externalID) // re-observation revives a superseded row
	key := bridgeRawKey(accountID, externalID)
	s.event("sink:insert:" + key)
	if externalID == s.failInsertID {
		return errors.New("forced raw insert failure")
	}
	copyOfRaw := append(json.RawMessage(nil), raw...)
	s.inserts = append(s.inserts, bridgeRawWrite{accountID: accountID, externalID: externalID, raw: copyOfRaw, hash: hash})
	s.stored[key] = hash
	s.raw[key] = copyOfRaw
	return nil
}

func (s *bridgeFixtureSink) UpdateRaw(_ context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) error {
	delete(s.superseded, externalID) // re-observation revives a superseded row
	key := bridgeRawKey(accountID, externalID)
	s.event("sink:update:" + key)
	copyOfRaw := append(json.RawMessage(nil), raw...)
	s.updates = append(s.updates, bridgeRawWrite{accountID: accountID, externalID: externalID, raw: copyOfRaw, hash: hash})
	s.stored[key] = hash
	s.raw[key] = copyOfRaw
	return nil
}

func bridgeGmailRaw(id, threadID string, internalDateMS int64, from, to, messageID, bodyData string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
		"id":%q,
		"threadId":%q,
		"internalDate":%q,
		"payload":{
			"mimeType":"text/plain",
			"headers":[
				{"name":"Message-ID","value":%q},
				{"name":"From","value":%q},
				{"name":"To","value":%q}
			],
			"body":{"data":%q}
		}
	}`, id, threadID, fmt.Sprint(internalDateMS), messageID, from, to, bodyData))
}

func oneBridgeAccountSource(alias, email string, raw ...json.RawMessage) *bridgeFixtureSource {
	var maxInternal int64
	for _, message := range raw {
		var meta struct {
			InternalDate string `json:"internalDate"`
		}
		_ = json.Unmarshal(message, &meta)
		var value int64
		_, _ = fmt.Sscan(meta.InternalDate, &value)
		if value > maxInternal {
			maxInternal = value
		}
	}
	account := BridgeAccount{Alias: alias, Email: email}
	return &bridgeFixtureSource{
		accounts: BridgeAccounts{SchemaVersion: BridgeSchemaVersion, Accounts: []BridgeAccount{account}},
		exports: map[string]BridgeExport{
			alias: {
				SchemaVersion: BridgeSchemaVersion, Account: account,
				Messages: raw, MaxInternalDateMS: maxInternal,
			},
		},
		exportErrs: map[string]error{},
	}
}

func TestRunBridge_DiscoversAccountsAndWritesExactRawBeforeCursor(t *testing.T) {
	var trace []string
	personalRaw := bridgeGmailRaw("p-1", "pt-1", 1784995200100, "client@example.net", "me@example.com", "<p-1@example.net>", "cGVyc29uYWw")
	workRaw1 := bridgeGmailRaw("w-1", "wt-1", 1784995200200, "buyer@corp.net", "me@corp.example", "<w-1@corp.net>", "d29yay0x")
	workRaw2 := bridgeGmailRaw("w-2", "wt-2", 1784995200300, "lead@corp.net", "me@corp.example", "<w-2@corp.net>", "d29yay0y")
	personal := BridgeAccount{Alias: "personal", Email: "Me@Example.com"}
	work := BridgeAccount{Alias: "work-main", Email: "Me@Corp.Example"}
	source := &bridgeFixtureSource{
		accounts: BridgeAccounts{SchemaVersion: BridgeSchemaVersion, Accounts: []BridgeAccount{personal, work}},
		exports: map[string]BridgeExport{
			"personal":  {SchemaVersion: BridgeSchemaVersion, Account: personal, Messages: []json.RawMessage{personalRaw}, MaxInternalDateMS: 1784995200100},
			"work-main": {SchemaVersion: BridgeSchemaVersion, Account: work, Messages: []json.RawMessage{workRaw1, workRaw2}, MaxInternalDateMS: 1784995200300},
		},
		exportErrs: map[string]error{}, trace: &trace,
	}
	sink := newBridgeFixtureSink(&trace)

	stats, err := RunBridge(context.Background(), source, sink, Config{
		Now: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC), Backfill: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("RunBridge: %v", err)
	}
	if stats.GmailListed != 3 || stats.GmailFetched != 3 || stats.RawInserted != 3 {
		t.Errorf("stats = %+v, want 3 listed/fetched/raw inserted", stats)
	}
	if len(source.requests) != 2 || source.requests[0].Account != "personal" || source.requests[1].Account != "work-main" {
		t.Fatalf("export requests = %+v, want one per discovered alias", source.requests)
	}
	for _, request := range source.requests {
		if request.MaxMessages <= 0 {
			t.Errorf("export request for %s is unbounded: %+v", request.Account, request)
		}
	}

	personalID := sink.accountsByEmail["me@example.com"].ID
	workID := sink.accountsByEmail["me@corp.example"].ID
	wants := []struct {
		key string
		raw json.RawMessage
	}{
		{bridgeRawKey(personalID, "gmail:p-1"), personalRaw},
		{bridgeRawKey(workID, "gmail:w-1"), workRaw1},
		{bridgeRawKey(workID, "gmail:w-2"), workRaw2},
	}
	for _, want := range wants {
		got, ok := sink.raw[want.key]
		if !ok {
			t.Errorf("missing raw observation %s", want.key)
			continue
		}
		if string(got) != string(want.raw) {
			t.Errorf("raw %s was reshaped before persistence:\n got %s\nwant %s", want.key, got, want.raw)
		}
	}

	// Cursor movement is the commit marker for a complete export. Every raw
	// insert for an account must precede that account's SaveCursor.
	assertTraceBefore(t, trace, fmt.Sprintf("sink:insert:%s", bridgeRawKey(personalID, "gmail:p-1")), fmt.Sprintf("sink:save:%d", personalID))
	assertTraceBefore(t, trace, fmt.Sprintf("sink:insert:%s", bridgeRawKey(workID, "gmail:w-1")), fmt.Sprintf("sink:save:%d", workID))
	assertTraceBefore(t, trace, fmt.Sprintf("sink:insert:%s", bridgeRawKey(workID, "gmail:w-2")), fmt.Sprintf("sink:save:%d", workID))
	if !slices.Equal(sink.finishStatuses, []string{"ok", "ok", "ok", "ok"}) {
		t.Errorf("run statuses = %v, want Gmail+Calendar ok for both accounts", sink.finishStatuses)
	}
}

func TestRunBridge_CalculatesCursorOverlapAndFreshBackfill(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		cursorMS  int64
		cfg       Config
		wantAfter int64
	}{
		{
			name: "incremental cursor minus overlap", cursorMS: 1784995200000,
			cfg:       Config{Now: now, Overlap: 2 * time.Hour, Backfill: 48 * time.Hour},
			wantAfter: (1784995200000 - (2 * time.Hour).Milliseconds()) / 1000,
		},
		{
			name: "fresh cursor backfill", cursorMS: 0,
			cfg:       Config{Now: now, Overlap: 2 * time.Hour, Backfill: 48 * time.Hour},
			wantAfter: now.Add(-48 * time.Hour).Unix(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			source := oneBridgeAccountSource("personal", "me@example.com")
			sink := newBridgeFixtureSink(nil)
			sink.accountsByEmail["me@example.com"] = Account{ID: 77, Email: "me@example.com"}
			sink.cursors[77] = Cursor{GmailInternalDateMS: tc.cursorMS, CalendarSyncToken: "calendar-must-survive"}

			if _, err := RunBridge(context.Background(), source, sink, tc.cfg); err != nil {
				t.Fatalf("RunBridge: %v", err)
			}
			if len(source.requests) != 1 {
				t.Fatalf("export requests = %+v", source.requests)
			}
			if got := source.requests[0].AfterUnix; got != tc.wantAfter {
				t.Errorf("after_unix = %d, want %d", got, tc.wantAfter)
			}
			if source.requests[0].MaxMessages <= 0 {
				t.Errorf("max_messages = %d, want a finite positive bound", source.requests[0].MaxMessages)
			}
		})
	}
}

func TestRunBridge_DoesNotAdvanceCursorAfterPartialRawFailure(t *testing.T) {
	raw1 := bridgeGmailRaw("m-1", "t-1", 1784995200100, "a@example.net", "me@example.com", "<m-1@example.net>", "b25l")
	raw2 := bridgeGmailRaw("m-2", "t-2", 1784995200200, "b@example.net", "me@example.com", "<m-2@example.net>", "dHdv")
	source := oneBridgeAccountSource("personal", "me@example.com", raw1, raw2)
	sink := newBridgeFixtureSink(nil)
	sink.accountsByEmail["me@example.com"] = Account{ID: 77, Email: "me@example.com"}
	sink.cursors[77] = Cursor{GmailInternalDateMS: 1784990000000, CalendarSyncToken: "keep-calendar-token"}
	sink.failInsertID = "gmail:m-2"

	_, err := RunBridge(context.Background(), source, sink, Config{Overlap: time.Hour})
	if err == nil {
		t.Fatal("RunBridge succeeded after a raw insert failed")
	}
	if len(sink.saves) != 0 {
		t.Fatalf("cursor advanced after partial raw persistence: %+v", sink.saves)
	}
	if got := sink.cursors[77]; got.GmailInternalDateMS != 1784990000000 || got.CalendarSyncToken != "keep-calendar-token" {
		t.Errorf("cursor changed after failure: %+v", got)
	}
	if !slices.Equal(sink.finishStatuses, []string{"error"}) {
		t.Errorf("run statuses = %v, want [error]", sink.finishStatuses)
	}
}

func TestRunBridge_ReingestUnchangedMessagesIsIdempotent(t *testing.T) {
	raw := bridgeGmailRaw("same-1", "same-thread", 1784995200100, "client@example.net", "me@example.com", "<same-1@example.net>", "c2FtZQ")
	source := oneBridgeAccountSource("personal", "me@example.com", raw)
	sink := newBridgeFixtureSink(nil)

	first, err := RunBridge(context.Background(), source, sink, Config{Now: time.Now(), Backfill: 24 * time.Hour})
	if err != nil {
		t.Fatalf("first RunBridge: %v", err)
	}
	second, err := RunBridge(context.Background(), source, sink, Config{Now: time.Now(), Backfill: 24 * time.Hour})
	if err != nil {
		t.Fatalf("second RunBridge: %v", err)
	}
	if first.RawInserted != 1 {
		t.Fatalf("first stats = %+v, want one insert", first)
	}
	if second.RawUnchanged != 1 || second.RawInserted != 0 || second.RawUpdated != 0 {
		t.Errorf("second stats = %+v, want exactly one unchanged raw", second)
	}
	if len(sink.inserts) != 1 || len(sink.updates) != 0 {
		t.Errorf("writes after unchanged re-ingest: inserts=%d updates=%d", len(sink.inserts), len(sink.updates))
	}
}

func TestBridgeRawMessageUsesExistingGmailNormalizer(t *testing.T) {
	raw := bridgeGmailRaw(
		"gm-existing", "thread-existing", 1784995200100,
		"Client <client@example.net>", "me@example.com", "<gm-existing@example.net>", "ZXhhY3QgYnJpZGdlIGJvZHk",
	)
	normalized, err := NormalizeGmailMessage(raw, "me@example.com", map[string]bool{"me@example.com": true})
	if err != nil {
		t.Fatalf("NormalizeGmailMessage on bridge-captured raw: %v", err)
	}
	if normalized.GmailMessageID != "gm-existing" || normalized.GmailThreadID != "thread-existing" {
		t.Errorf("existing normalizer lost Gmail identity: %+v", normalized)
	}
	if normalized.ThreadKey != "gmail:me@example.com:thread-existing" || normalized.BodyText != "exact bridge body" {
		t.Errorf("existing canonical model not reused: %+v", normalized)
	}
}

func assertTraceBefore(t *testing.T, trace []string, first, second string) {
	t.Helper()
	firstIndex, secondIndex := slices.Index(trace, first), slices.Index(trace, second)
	if firstIndex < 0 || secondIndex < 0 || firstIndex >= secondIndex {
		t.Errorf("trace order %q before %q not satisfied: %v", first, second, trace)
	}
}

// --- serialization + reset replacement fakes (added with the review fixes) ---

func (s *bridgeFixtureSink) LockAccount(_ context.Context, accountID int64) (func(), bool, error) {
	if s.lockErr != nil {
		return nil, false, s.lockErr
	}
	if s.lockBusy {
		s.event(fmt.Sprintf("sink:lock-busy:%d", accountID))
		return nil, false, nil
	}
	s.event(fmt.Sprintf("sink:lock:%d", accountID))
	s.locked++
	return func() {
		s.locked--
		s.event(fmt.Sprintf("sink:unlock:%d", accountID))
	}, true, nil
}

// SaveCursorField mirrors the jsonb_set semantics: it touches ONE field and
// leaves its sibling alone, which is the property the concurrency fix rests on.
func (s *bridgeFixtureSink) SaveCursorField(_ context.Context, accountID int64, field string, value any) error {
	s.event(fmt.Sprintf("sink:save:%d", accountID))
	cursor := s.cursors[accountID]
	switch field {
	case "gmail_internal_date_ms":
		millis, ok := value.(int64)
		if !ok {
			return fmt.Errorf("gmail cursor field got %T, want int64", value)
		}
		cursor.GmailInternalDateMS = millis
	case "calendar_sync_token":
		token, ok := value.(string)
		if !ok {
			return fmt.Errorf("calendar cursor field got %T, want string", value)
		}
		cursor.CalendarSyncToken = token
	default:
		return fmt.Errorf("unknown cursor field %q", field)
	}
	s.cursors[accountID] = cursor
	s.savedCursorFields = append(s.savedCursorFields, field)
	s.saves = append(s.saves, Account{ID: accountID, Email: fmt.Sprintf("%d", cursor.GmailInternalDateMS)})
	s.savedCursors = append(s.savedCursors, cursor)
	return nil
}

func (s *bridgeFixtureSink) SupersedeAbsentCalendar(_ context.Context, accountID int64, keep []string, _, _ time.Time) (int, error) {
	s.event(fmt.Sprintf("sink:supersede:%d", accountID))
	if s.supersedeErr != nil {
		return 0, s.supersedeErr
	}
	if len(keep) == 0 {
		return 0, fmt.Errorf("refusing an empty Calendar reset snapshot")
	}
	kept := map[string]bool{}
	for _, id := range keep {
		kept[id] = true
	}
	superseded := 0
	for externalID := range s.raw {
		if !strings.HasPrefix(externalID, "calendar:") || kept[externalID] {
			continue
		}
		if s.superseded == nil {
			s.superseded = map[string]bool{}
		}
		if !s.superseded[externalID] {
			s.superseded[externalID] = true
			superseded++
		}
	}
	return superseded, nil
}
