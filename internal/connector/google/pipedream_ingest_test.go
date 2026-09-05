package google

// Unit tests for the Pipedream calendar snapshot ingest (pipedream-calendar /
// docs/tickets/pipedream-calendar_SPEC.md, acceptance criteria 5, 6, 7, 8, 9,
// 10, 11, 12, 13, 14 and 17). ZERO network, ZERO Postgres, ZERO Pipedream
// credentials: the source is a fake returning a decoded envelope and the sink
// is a fixture, the bridge_ingest_test.go bridgeFixtureSink shape.
//
// WHY SO MANY REFUSALS ARE TESTED HERE RATHER THAN LEFT TO REVIEW. SPEC premise
// 2: Normalize returns on the FIRST unparseable calendar item, and
// cmd/connectors/google/main.go returns on that error BEFORE ObserveOutbound
// and EvaluateRules run. So one reshaped event object from a third party would
// stall mail normalization, outbound observation and the capture pass — and
// keep stalling every pass until the row is superseded by hand. That is the
// sharpest new risk of a third-party transport, and it is why criterion 8
// validates every item BEFORE any raw row is written.
//
// ANTI-ROT: every event time here is RELATIVE to now. A frozen-date calendar
// fixture in this exact area failed on 2026-09-02 when it aged out of the
// now-30d window overnight (see bridge_pg_integration_test.go's evt helper).
//
// GREENFIELD NOTE: RunPipedreamCalendar and CalendarSnapshotSink do not exist
// yet, so this file compile-FAILS — the expected red for SPEC-named greenfield
// surface. Imposed contract (internal/connector/google/pipedream_ingest.go;
// the SPEC gives the signature in "Files likely to touch" verbatim, the sink
// interface is spelled out there too):
//
//	// CalendarSnapshotSink is Sink + LockAccount + SupersedeAbsentCalendar.
//	// The cursor methods are inherited from Sink but MUST NOT be called:
//	// criterion 12 says the Pipedream path writes no cursor at all.
//	type CalendarSnapshotSink interface {
//	    Sink
//	    LockAccount(ctx context.Context, accountID int64) (release func(), ok bool, err error)
//	    SupersedeAbsentCalendar(ctx context.Context, accountID int64, keep []string,
//	        windowFrom, windowTo time.Time) (int, error)
//	}
//
//	func RunPipedreamCalendar(ctx context.Context, source PipedreamCalendarSource,
//	    sink CalendarSnapshotSink, accounts []Account, cfg Config) (Stats, error)
//
// And two Stats fields (criterion 17, ingest.go), diagnostic ONLY — nothing may
// branch on either, readiness keys on status and stats->>'phase' alone:
//
//	CalendarSource        string `json:"calendar_source,omitempty"`
//	CalendarEmptySnapshot int    `json:"calendar_empty_snapshot,omitempty"`

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// fakePipedreamSource is the endpoint. It records every request (so criterion
// 5's window and calendar list are assertable) and returns a canned envelope
// or a transport error.
type fakePipedreamSource struct {
	requests []PipedreamCalendarRequest
	resp     PipedreamCalendarResponse
	err      error
	// echoRequest makes the response echo the request's window, which is what
	// a correct workflow does. Cases testing the echo check set it false and
	// spell the returned window themselves.
	echoRequest bool
}

func (f *fakePipedreamSource) FetchCalendars(_ context.Context, req PipedreamCalendarRequest) (PipedreamCalendarResponse, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		return PipedreamCalendarResponse{}, f.err
	}
	resp := f.resp
	if f.echoRequest {
		resp.TimeMin, resp.TimeMax = req.TimeMin, req.TimeMax
	}
	return resp, nil
}

// pipedreamRun is one recorded sync_runs row.
type pipedreamRun struct {
	accountID int64
	phase     string
	status    string
	stats     Stats
	errMsg    string
}

// pipedreamSupersede is one recorded SupersedeAbsentCalendar call.
type pipedreamSupersede struct {
	accountID int64
	keep      []string
	from, to  time.Time
}

// pipedreamFixtureSink is the CalendarSnapshotSink fixture. It mirrors
// bridgeFixtureSink but is separate on purpose: this path has no cursor and no
// EnsureBridgeAccount, and the cursor methods here FAIL the test rather than
// record (criterion 12).
type pipedreamFixtureSink struct {
	t     *testing.T
	trace []string

	stored map[string]string          // rawKey -> content hash
	raw    map[string]json.RawMessage // rawKey -> raw bytes
	starts []pipedreamRun
	runs   []pipedreamRun
	sups   []pipedreamSupersede

	superseded map[string]bool

	lockBusy map[int64]bool
	nextRun  int64
}

func newPipedreamSink(t *testing.T) *pipedreamFixtureSink {
	return &pipedreamFixtureSink{
		t: t, stored: map[string]string{}, raw: map[string]json.RawMessage{},
		superseded: map[string]bool{}, lockBusy: map[int64]bool{}, nextRun: 5000,
	}
}

func pipedreamRawKey(accountID int64, externalID string) string {
	return fmt.Sprintf("%d|%s", accountID, externalID)
}

func (s *pipedreamFixtureSink) event(v string) { s.trace = append(s.trace, v) }

// --- the two cursor methods that must never be reached (criterion 12) ------

func (s *pipedreamFixtureSink) Cursor(_ context.Context, accountID int64) (Cursor, error) {
	s.t.Helper()
	s.t.Errorf("the Pipedream path read the cursor for account %d. It has no sync token to carry, and the "+
		"cursor blob also holds imap_folders — reading it is the first half of writing it back stale, "+
		"which rolls the IMAP position back and skips mail (criterion 12, invariant 5)", accountID)
	return Cursor{}, nil
}

func (s *pipedreamFixtureSink) SaveCursor(_ context.Context, accountID int64, _ Cursor) error {
	s.t.Helper()
	s.t.Errorf("the Pipedream path called SaveCursor for account %d. Criterion 12: it writes NO cursor at "+
		"all, and a whole-blob write would clobber imap_folders — a skipped mail is a delivery "+
		"confirmation that never lands", accountID)
	return nil
}

func (s *pipedreamFixtureSink) SaveCursorField(_ context.Context, accountID int64, field string, _ any) error {
	s.t.Helper()
	s.t.Errorf("the Pipedream path wrote cursor field %q for account %d. Criterion 12: it writes NO cursor "+
		"at all — there is no sync token on this transport, and every poll is a full snapshot", field, accountID)
	return nil
}

// --- runs -----------------------------------------------------------------

func (s *pipedreamFixtureSink) StartRun(_ context.Context, accountID int64, phase string) (int64, error) {
	s.event(fmt.Sprintf("start:%d:%s", accountID, phase))
	s.nextRun++
	s.starts = append(s.starts, pipedreamRun{accountID: accountID, phase: phase})
	s.runs = append(s.runs, pipedreamRun{accountID: accountID, phase: phase, status: "running"})
	return s.nextRun, nil
}

func (s *pipedreamFixtureSink) FinishRun(_ context.Context, runID int64, status string, stats Stats, errMsg string) error {
	s.event(fmt.Sprintf("finish:%d:%s", runID, status))
	idx := int(runID - 5001)
	if idx < 0 || idx >= len(s.runs) {
		return fmt.Errorf("finish for unknown run %d", runID)
	}
	s.runs[idx].status = status
	s.runs[idx].stats = stats
	s.runs[idx].errMsg = errMsg
	return nil
}

// runFor returns the single run recorded for one account, or fails.
func (s *pipedreamFixtureSink) runFor(t *testing.T, accountID int64) pipedreamRun {
	t.Helper()
	var found []pipedreamRun
	for _, r := range s.runs {
		if r.accountID == accountID {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("sync_runs rows for account %d = %d, want exactly 1 per pass (criterion 13): %+v",
			accountID, len(found), s.runs)
	}
	return found[0]
}

func (s *pipedreamFixtureSink) hasRun(accountID int64) bool {
	for _, r := range s.runs {
		if r.accountID == accountID {
			return true
		}
	}
	return false
}

// --- raw ------------------------------------------------------------------

func (s *pipedreamFixtureSink) RawHash(_ context.Context, accountID int64, externalID string) (string, bool, error) {
	if s.superseded[pipedreamRawKey(accountID, externalID)] {
		return "", false, nil // a superseded row answers "absent" (sink.go's rule)
	}
	h, ok := s.stored[pipedreamRawKey(accountID, externalID)]
	return h, ok, nil
}

func (s *pipedreamFixtureSink) InsertRaw(_ context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) error {
	key := pipedreamRawKey(accountID, externalID)
	delete(s.superseded, key)
	s.event("insert:" + key)
	s.stored[key] = hash
	s.raw[key] = raw
	return nil
}

func (s *pipedreamFixtureSink) UpdateRaw(_ context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) error {
	key := pipedreamRawKey(accountID, externalID)
	delete(s.superseded, key)
	s.event("update:" + key)
	s.stored[key] = hash
	s.raw[key] = raw
	return nil
}

// rawIDsFor returns the calendar external ids currently live for one account.
func (s *pipedreamFixtureSink) rawIDsFor(accountID int64) []string {
	prefix := fmt.Sprintf("%d|", accountID)
	var out []string
	for key := range s.stored {
		if strings.HasPrefix(key, prefix) && !s.superseded[key] {
			out = append(out, strings.TrimPrefix(key, prefix))
		}
	}
	slices.Sort(out)
	return out
}

// --- lock + supersede ------------------------------------------------------

func (s *pipedreamFixtureSink) LockAccount(_ context.Context, accountID int64) (func(), bool, error) {
	if s.lockBusy[accountID] {
		s.event(fmt.Sprintf("lock-busy:%d", accountID))
		return nil, false, nil
	}
	s.event(fmt.Sprintf("lock:%d", accountID))
	return func() { s.event(fmt.Sprintf("unlock:%d", accountID)) }, true, nil
}

func (s *pipedreamFixtureSink) SupersedeAbsentCalendar(_ context.Context, accountID int64, keep []string, from, to time.Time) (int, error) {
	s.event(fmt.Sprintf("supersede:%d", accountID))
	s.sups = append(s.sups, pipedreamSupersede{accountID: accountID, keep: slices.Clone(keep), from: from, to: to})
	if len(keep) == 0 {
		// The production sink refuses this by design (sink.go:469-473, "an
		// empty replacement is indistinguishable from a broken leaf"), and
		// criterion 11 says the refusal must never be REACHED.
		return 0, fmt.Errorf("refusing to apply an empty Calendar reset snapshot for account %d", accountID)
	}
	kept := map[string]bool{}
	for _, id := range keep {
		kept[id] = true
	}
	n := 0
	prefix := fmt.Sprintf("%d|calendar:", accountID)
	for key := range s.stored {
		if !strings.HasPrefix(key, prefix) || s.superseded[key] {
			continue
		}
		if kept[strings.TrimPrefix(key, fmt.Sprintf("%d|", accountID))] {
			continue
		}
		s.superseded[key] = true
		n++
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// fixture helpers
// ---------------------------------------------------------------------------

var (
	pipedreamAcctA = Account{ID: 41, Email: "a@example.com", CalendarInAvailability: true}
	pipedreamAcctB = Account{ID: 42, Email: "b@example.com", CalendarInAvailability: true}
	pipedreamAcctC = Account{ID: 43, Email: "c@example.com", CalendarInAvailability: true}
)

func pipedreamAccounts() []Account {
	return []Account{pipedreamAcctA, pipedreamAcctB, pipedreamAcctC}
}

// pipedreamCfg pins the clock so the expected window is computable, while
// staying anchored to time.Now() so no fixture can age out of the horizon.
func pipedreamCfg() Config {
	return Config{Now: time.Now().UTC().Truncate(time.Second)}
}

func wantWindow(cfg Config) (string, string) {
	return cfg.Now.Add(-CalendarWindowPast).Format(time.RFC3339),
		cfg.Now.Add(CalendarWindowFuture).Format(time.RFC3339)
}

// pipedreamEvent builds a Google Calendar v3 event object, RELATIVE to now.
func pipedreamEvent(id string, hoursFromNow int) json.RawMessage {
	start := time.Now().UTC().Add(time.Duration(hoursFromNow) * time.Hour).Truncate(time.Hour)
	return json.RawMessage(fmt.Sprintf(
		`{"id":%q,"status":"confirmed","summary":"itest %s","start":{"dateTime":%q},"end":{"dateTime":%q},`+
			`"attendees":[{"email":"someone@example.net","responseStatus":"accepted"}]}`,
		id, id, start.Format(time.RFC3339), start.Add(time.Hour).Format(time.RFC3339)))
}

func countPtr(n int) *int { return &n }

// entry builds an ok entry with a consistent event_count.
func entry(calendarID string, events ...json.RawMessage) PipedreamCalendarEntry {
	return PipedreamCalendarEntry{
		CalendarID: calendarID, Status: "ok",
		EventCount: countPtr(len(events)), Events: events,
	}
}

// okResponse is the happy-path envelope; the window is echoed by the fake.
func okResponse(entries ...PipedreamCalendarEntry) PipedreamCalendarResponse {
	return PipedreamCalendarResponse{
		SchemaVersion: PipedreamCalendarSchemaVersion,
		Calendars:     entries,
	}
}

func assertNoRawWrites(t *testing.T, sink *pipedreamFixtureSink, accountID int64, why string) {
	t.Helper()
	if got := sink.rawIDsFor(accountID); len(got) != 0 {
		t.Errorf("account %d has raw rows %v after a refusal. %s. Criterion 8: a single bad item fails that "+
			"account's snapshot WHOLESALE and writes nothing for it — a stored bad item stalls mail "+
			"normalization, not just calendars (premise 2)", accountID, got, why)
	}
}

func assertNoSupersede(t *testing.T, sink *pipedreamFixtureSink, why string) {
	t.Helper()
	if len(sink.sups) != 0 {
		t.Errorf("SupersedeAbsentCalendar was called %d time(s) (%+v). %s", len(sink.sups), sink.sups, why)
	}
}

// ---------------------------------------------------------------------------
// Criterion 5: the poll window comes from the SHARED constants, never a
// re-spelling. propose_slots refuses a window outside
// [now-CalendarWindowPast, now+CalendarWindowFuture] against those same two
// constants (premise 4), so a poller that fetched a narrower span would leave
// LoadBusy certifying a stretch nothing ever fetched.
// ---------------------------------------------------------------------------

func TestRunPipedreamCalendar_PollsTheSharedHorizonForTheInScopeCalendars(t *testing.T) {
	cfg := pipedreamCfg()
	source := &fakePipedreamSource{echoRequest: true, resp: okResponse(
		entry("a@example.com", pipedreamEvent("a-1", 24)),
		entry("b@example.com"),
		entry("c@example.com"),
	)}
	// The empty entries would trip criterion 11's counter, which is fine here:
	// this test is about the request.
	sink := newPipedreamSink(t)

	if _, err := RunPipedreamCalendar(context.Background(), source, sink, pipedreamAccounts(), cfg); err != nil {
		t.Fatalf("RunPipedreamCalendar: %v", err)
	}

	if len(source.requests) != 1 {
		t.Fatalf("the pass made %d polls, want exactly 1 for all three calendars (the SPEC's "+
			"one-workflow-one-call decision)", len(source.requests))
	}
	req := source.requests[0]
	wantMin, wantMax := wantWindow(cfg)
	if req.TimeMin != wantMin || req.TimeMax != wantMax {
		t.Errorf("polled window = %s..%s, want %s..%s — cfg.now()±google.CalendarWindowPast/Future, "+
			"THE CONSTANTS, never re-spelled (premise 4): propose_slots' horizon refusal keys on the same "+
			"pair, so a re-spelled copy silently certifies a span nothing fetches",
			req.TimeMin, req.TimeMax, wantMin, wantMax)
	}
	if req.SchemaVersion != PipedreamCalendarSchemaVersion {
		t.Errorf("request schema_version = %d, want %d", req.SchemaVersion, PipedreamCalendarSchemaVersion)
	}
	wantCalendars := []string{"a@example.com", "b@example.com", "c@example.com"}
	got := slices.Clone(req.Calendars)
	slices.Sort(got)
	if !slices.Equal(got, wantCalendars) {
		t.Errorf("polled calendars = %v, want the in-scope account emails %v", got, wantCalendars)
	}
}

// ---------------------------------------------------------------------------
// Criterion 6, THE SHARPEST ONE: the time_min/time_max echo must equal what we
// sent, and the refusal must happen BEFORE any supersede.
//
// Every poll is a full snapshot and therefore a full REPLACEMENT (criterion
// 10). A workflow that quietly returns "the next 7 days" while we supersede
// against a 120-day window cancels every real event outside those 7 days on the
// first poll — silently, and the events are gone from the busy set, so
// propose_slots starts booking over real meetings. This is the SPEC's named
// mutation guard: delete the echo comparison and this test must go red.
// ---------------------------------------------------------------------------

func TestRunPipedreamCalendar_RefusesANarrowedWindowEchoBeforeAnySupersede(t *testing.T) {
	cfg := pipedreamCfg()
	sink := newPipedreamSink(t)

	// A real event, already stored, 20 days out — inside the 120-day window we
	// ask for, outside the 7-day window the workflow would be answering with.
	stored := pipedreamEvent("real-meeting", 20*24)
	if err := sink.InsertRaw(context.Background(), pipedreamAcctA.ID, "calendar:real-meeting", stored, "hash-real"); err != nil {
		t.Fatalf("seed raw: %v", err)
	}

	source := &fakePipedreamSource{
		// NOT echoing: the workflow answers with its own, narrower window.
		resp: PipedreamCalendarResponse{
			SchemaVersion: PipedreamCalendarSchemaVersion,
			TimeMin:       cfg.Now.Format(time.RFC3339),
			TimeMax:       cfg.Now.Add(7 * 24 * time.Hour).Format(time.RFC3339),
			Calendars: []PipedreamCalendarEntry{
				entry("a@example.com", pipedreamEvent("soon", 12)),
				entry("b@example.com", pipedreamEvent("soon-b", 12)),
				entry("c@example.com", pipedreamEvent("soon-c", 12)),
			},
		},
	}

	_, err := RunPipedreamCalendar(context.Background(), source, sink, pipedreamAccounts(), cfg)
	if err == nil {
		t.Fatal("RunPipedreamCalendar accepted a response whose echoed window is NOT the one we asked for. " +
			"That is the failure that destroys data: the snapshot is applied as a replacement over the " +
			"120-day window we superseded against, so 113 days of real events are cancelled on the first " +
			"poll and propose_slots starts offering time over real meetings (criterion 6)")
	}

	assertNoSupersede(t, sink, "The echo check must refuse BEFORE the replacement is applied — "+
		"a supersede against a window the snapshot does not cover is exactly the data loss criterion 6 exists to stop")

	if sink.superseded[pipedreamRawKey(pipedreamAcctA.ID, "calendar:real-meeting")] {
		t.Error("the 20-day-out event was superseded by a snapshot that only covered 7 days")
	}
	for _, acct := range pipedreamAccounts() {
		run := sink.runFor(t, acct.ID)
		if run.status != "error" {
			t.Errorf("account %s finished %q, want error: a whole-poll refusal produces an error run for "+
				"EVERY in-scope account (criterion 6)", acct.Email, run.status)
		}
	}
}

// The control the mutation check needs: an echoed window that MATCHES is
// accepted. Without it, "refuse everything" passes the test above.
func TestRunPipedreamCalendar_AcceptsTheEchoedWindowItAskedFor(t *testing.T) {
	cfg := pipedreamCfg()
	sink := newPipedreamSink(t)
	source := &fakePipedreamSource{echoRequest: true, resp: okResponse(
		entry("a@example.com", pipedreamEvent("a-1", 30)),
		entry("b@example.com", pipedreamEvent("b-1", 30)),
		entry("c@example.com", pipedreamEvent("c-1", 30)),
	)}

	if _, err := RunPipedreamCalendar(context.Background(), source, sink, pipedreamAccounts(), cfg); err != nil {
		t.Fatalf("RunPipedreamCalendar refused a correctly echoed window: %v", err)
	}
	for _, acct := range pipedreamAccounts() {
		if run := sink.runFor(t, acct.ID); run.status != "ok" {
			t.Errorf("account %s finished %q (%s), want ok", acct.Email, run.status, run.errMsg)
		}
	}
}

func TestRunPipedreamCalendar_RefusesAnUnknownSchemaVersion(t *testing.T) {
	cfg := pipedreamCfg()
	sink := newPipedreamSink(t)
	source := &fakePipedreamSource{echoRequest: true, resp: PipedreamCalendarResponse{
		SchemaVersion: PipedreamCalendarSchemaVersion + 1,
		Calendars:     []PipedreamCalendarEntry{entry("a@example.com", pipedreamEvent("a-1", 5))},
	}}

	if _, err := RunPipedreamCalendar(context.Background(), source, sink, pipedreamAccounts(), cfg); err == nil {
		t.Fatal("an unknown schema_version was accepted. The envelope is a contract with a third party we " +
			"do not control; a version we have never seen may reshape the very fields the refusals read " +
			"(criterion 6)")
	}
	assertNoRawWrites(t, sink, pipedreamAcctA.ID, "the envelope failed verification")
	for _, acct := range pipedreamAccounts() {
		if run := sink.runFor(t, acct.ID); run.status != "error" {
			t.Errorf("account %s finished %q, want error", acct.Email, run.status)
		}
	}
}

// Criterion 6 + 13: a TRANSPORT failure (we did attempt a poll) writes an error
// run for every in-scope account. Contrast with criterion 3's configuration
// error, which writes nothing at all — that line is drawn in the cmd suite.
func TestRunPipedreamCalendar_TransportFailureErrorsEveryInScopeAccount(t *testing.T) {
	cfg := pipedreamCfg()
	sink := newPipedreamSink(t)
	source := &fakePipedreamSource{err: fmt.Errorf("pipedream calendar workflow returned HTTP 503")}

	if _, err := RunPipedreamCalendar(context.Background(), source, sink, pipedreamAccounts(), cfg); err == nil {
		t.Fatal("a failing poll returned no error")
	}
	for _, acct := range pipedreamAccounts() {
		run := sink.runFor(t, acct.ID)
		if run.status != "error" {
			t.Errorf("account %s finished %q, want error. A Pipedream outage must NEVER produce an ok run: "+
				"an ok run is what makes propose_slots answer from a calendar nobody read (criterion 13)",
				acct.Email, run.status)
		}
		if run.phase != "calendar" {
			t.Errorf("account %s run phase = %q, want \"calendar\" — readiness keys on stats->>'phase' "+
				"(premise 3)", acct.Email, run.phase)
		}
		if run.errMsg == "" {
			t.Errorf("account %s error run carries no reason; an error row that says WHY is the whole "+
				"advantage it has over an absent row", acct.Email)
		}
	}
	assertNoSupersede(t, sink, "nothing may be replaced on the strength of a poll that failed")
}

// ---------------------------------------------------------------------------
// Criterion 7: attribution is by IDENTITY, never by position.
//
// One response carrying three calendars is the new risk this ticket creates:
// writing account A's events under account B's id. The response order is
// rotated relative to the account list so positional assignment CANNOT pass —
// with three accounts a rotation gives every account the wrong entry.
// ---------------------------------------------------------------------------

func TestRunPipedreamCalendar_AttributesByCalendarIDNotByPosition(t *testing.T) {
	cfg := pipedreamCfg()
	sink := newPipedreamSink(t)
	source := &fakePipedreamSource{echoRequest: true, resp: okResponse(
		// Rotated by one, and upper-cased: the match is case-insensitive on
		// the address, which is how Google spells identities back.
		entry("C@Example.com", pipedreamEvent("c-owned", 10)),
		entry("A@Example.com", pipedreamEvent("a-owned", 11)),
		entry("B@Example.com", pipedreamEvent("b-owned", 12)),
	)}

	if _, err := RunPipedreamCalendar(context.Background(), source, sink, pipedreamAccounts(), cfg); err != nil {
		t.Fatalf("RunPipedreamCalendar: %v", err)
	}

	want := map[int64]string{
		pipedreamAcctA.ID: "calendar:a-owned",
		pipedreamAcctB.ID: "calendar:b-owned",
		pipedreamAcctC.ID: "calendar:c-owned",
	}
	for accountID, externalID := range want {
		got := sink.rawIDsFor(accountID)
		if !slices.Equal(got, []string{externalID}) {
			t.Errorf("account %d holds %v, want [%s]. Criterion 7: each entry is attributed to the in-scope "+
				"account whose account_email EQUALS its calendar_id, case-insensitively. Index matching is "+
				"forbidden, and this response is rotated precisely so index matching cannot pass — the "+
				"failure it prevents is writing one client's meetings under another mailbox",
				accountID, got, externalID)
		}
	}
}

// Criterion 7 + 13: an in-scope account ABSENT from the response gets an error
// run. Never an ok run, never silence — silence leaves the account looking
// never-polled, and an ok run would let propose_slots answer for a calendar the
// workflow has stopped returning (a disconnected Google account in Pipedream
// looks exactly like this).
func TestRunPipedreamCalendar_AnAbsentAccountGetsAnErrorRunNeverOk(t *testing.T) {
	cfg := pipedreamCfg()
	sink := newPipedreamSink(t)
	source := &fakePipedreamSource{echoRequest: true, resp: okResponse(
		entry("a@example.com", pipedreamEvent("a-1", 6)),
		entry("c@example.com", pipedreamEvent("c-1", 6)),
		// b@example.com is missing entirely.
	)}

	// The pass's own return value is deliberately NOT asserted here: criterion
	// 14 says the pass fails only if NO account succeeded, and two did. The
	// visibility of the failure lives in the run row below — see
	// TestRunPipedreamCalendar_FailsThePassOnlyWhenNoAccountSucceeded.
	_, _ = RunPipedreamCalendar(context.Background(), source, sink, pipedreamAccounts(), cfg)

	run := sink.runFor(t, pipedreamAcctB.ID)
	if run.status != "error" {
		t.Errorf("the absent account %s finished %q, want error. Criterion 7: an in-scope account absent "+
			"from the response gets a status='error' run — never an ok run, never silence. An ok run here "+
			"is a freshness signal for a calendar the workflow no longer returns", pipedreamAcctB.Email, run.status)
	}
	if !strings.Contains(run.errMsg, pipedreamAcctB.Email) {
		t.Errorf("the absent account's error is %q and does not name %s; the operator's next action is "+
			"reconnecting THAT Google account in Pipedream", run.errMsg, pipedreamAcctB.Email)
	}
	// One failing account never blinds the others (criterion 14).
	for _, acct := range []Account{pipedreamAcctA, pipedreamAcctC} {
		if run := sink.runFor(t, acct.ID); run.status != "ok" {
			t.Errorf("account %s finished %q, want ok: one absent account must not abort the others",
				acct.Email, run.status)
		}
	}
}

// Criterion 7: a returned calendar matching no in-scope account is IGNORED — it
// writes nothing and does not fail the accounts that were answered. (The "and
// is counted and printed by name" half is a printed line at the cmd layer and
// is not asserted here; see the report.)
func TestRunPipedreamCalendar_IgnoresACalendarNoAccountClaims(t *testing.T) {
	cfg := pipedreamCfg()
	sink := newPipedreamSink(t)
	source := &fakePipedreamSource{echoRequest: true, resp: okResponse(
		entry("a@example.com", pipedreamEvent("a-1", 7)),
		entry("b@example.com", pipedreamEvent("b-1", 7)),
		entry("c@example.com", pipedreamEvent("c-1", 7)),
		entry("stranger@example.org", pipedreamEvent("not-ours", 7)),
	)}

	if _, err := RunPipedreamCalendar(context.Background(), source, sink, pipedreamAccounts(), cfg); err != nil {
		t.Fatalf("an unknown calendar in the response failed the whole pass: %v", err)
	}
	for key := range sink.raw {
		if strings.Contains(key, "not-ours") {
			t.Errorf("raw row %q was written for a calendar no in-scope account claims. It means the "+
				"workflow is wired to an account switchboard does not know about; ingesting it would "+
				"attribute a stranger's meetings to nobody", key)
		}
	}
	for _, acct := range pipedreamAccounts() {
		if run := sink.runFor(t, acct.ID); run.status != "ok" {
			t.Errorf("account %s finished %q, want ok", acct.Email, run.status)
		}
	}
}

// ---------------------------------------------------------------------------
// Criterion 8: per-entry integrity, checked BEFORE a single raw row is written
// for that account.
// ---------------------------------------------------------------------------

func TestRunPipedreamCalendar_EntryStatusErrorFailsOnlyThatAccount(t *testing.T) {
	cfg := pipedreamCfg()
	sink := newPipedreamSink(t)
	source := &fakePipedreamSource{echoRequest: true, resp: okResponse(
		entry("a@example.com", pipedreamEvent("a-1", 8)),
		PipedreamCalendarEntry{
			CalendarID: "b@example.com", Status: "error",
			Error: "invalid_grant: token has been expired or revoked",
		},
		entry("c@example.com", pipedreamEvent("c-1", 8)),
	)}

	// Mixed outcome: two accounts succeeded, so the pass itself does not fail
	// (criterion 14). The failure is visible in B's run row.
	_, _ = RunPipedreamCalendar(context.Background(), source, sink, pipedreamAccounts(), cfg)
	run := sink.runFor(t, pipedreamAcctB.ID)
	if run.status != "error" {
		t.Errorf("entry status=error left the run %q, want error", run.status)
	}
	if !strings.Contains(run.errMsg, "invalid_grant") {
		t.Errorf("the run error is %q and does not carry the entry's own error string; the workflow already "+
			"knows why Google refused and that is the only place an operator can read it", run.errMsg)
	}
	assertNoRawWrites(t, sink, pipedreamAcctB.ID, "the entry declared itself failed")
	for _, acct := range []Account{pipedreamAcctA, pipedreamAcctC} {
		if run := sink.runFor(t, acct.ID); run.status != "ok" {
			t.Errorf("account %s finished %q, want ok", acct.Email, run.status)
		}
	}
}

// THE POINTER TEST (criterion 8, premise 11). event_count must be a *int: an
// ABSENT field decoding to zero silently skips the check.
//
// The fixture is chosen so that "change event_count from a pointer to a plain
// int" turns it RED, which is the SPEC's named mutation check. Absent count +
// ZERO events is the only shape that discriminates: with a plain int the absent
// field decodes to 0, 0 == len(events), and the entry is accepted as a verified
// empty snapshot — which under criterion 11 is an OK RUN that keeps stale
// events. So a truncated workflow response that lost its events array and its
// count would be recorded as "we looked, the calendar is empty", forever fresh
// and never wrong out loud. (Absent count with N>0 events is NOT a valid
// mutation probe: 0 != N refuses either way.)
func TestRunPipedreamCalendar_AbsentEventCountIsARefusalNotAZero(t *testing.T) {
	cfg := pipedreamCfg()
	sink := newPipedreamSink(t)
	source := &fakePipedreamSource{echoRequest: true, resp: okResponse(
		entry("a@example.com", pipedreamEvent("a-1", 9)),
		PipedreamCalendarEntry{
			CalendarID: "b@example.com", Status: "ok",
			EventCount: nil,                 // ABSENT
			Events:     []json.RawMessage{}, // and empty
		},
		entry("c@example.com", pipedreamEvent("c-1", 9)),
	)}

	_, _ = RunPipedreamCalendar(context.Background(), source, sink, pipedreamAccounts(), cfg)
	run := sink.runFor(t, pipedreamAcctB.ID)
	if run.status != "error" {
		t.Errorf("an entry that OMITTED event_count finished %q, want error. Criterion 8 + premise 11: the "+
			"field is a POINTER because an omitted field decoding to zero silently skips the check — and "+
			"with zero events it would pass as a verified empty snapshot, i.e. a fresh ok run for a "+
			"calendar we never actually read", run.status)
	}
	if run.stats.CalendarEmptySnapshot != 0 {
		t.Errorf("the unverified entry was counted as an empty snapshot (%d); it was not verified at all",
			run.stats.CalendarEmptySnapshot)
	}
}

func TestRunPipedreamCalendar_EventCountMismatchIsARefusal(t *testing.T) {
	cfg := pipedreamCfg()
	sink := newPipedreamSink(t)
	nine := 9
	source := &fakePipedreamSource{echoRequest: true, resp: okResponse(
		entry("a@example.com", pipedreamEvent("a-1", 10)),
		PipedreamCalendarEntry{
			CalendarID: "b@example.com", Status: "ok",
			EventCount: &nine, // declares nine
			Events:     []json.RawMessage{pipedreamEvent("b-1", 10)},
		},
		entry("c@example.com", pipedreamEvent("c-1", 10)),
	)}

	_, _ = RunPipedreamCalendar(context.Background(), source, sink, pipedreamAccounts(), cfg)
	if run := sink.runFor(t, pipedreamAcctB.ID); run.status != "error" {
		t.Errorf("event_count=9 with 1 event finished %q, want error. A snapshot is a REPLACEMENT: an array "+
			"shorter than its own declared count means events were lost in transit, and applying it would "+
			"supersede every one of them", run.status)
	}
	assertNoRawWrites(t, sink, pipedreamAcctB.ID, "the declared count disagreed with the array")
	assertNoSupersede(t, sink, "no account may be replaced from a snapshot that failed verification")
}

func TestRunPipedreamCalendar_RefusesAnEntryAtTheEventCap(t *testing.T) {
	cfg := pipedreamCfg()
	sink := newPipedreamSink(t)

	events := make([]json.RawMessage, 0, PipedreamMaxEvents)
	for i := 0; i < PipedreamMaxEvents; i++ {
		events = append(events, pipedreamEvent(fmt.Sprintf("bulk-%d", i), i%600))
	}
	source := &fakePipedreamSource{echoRequest: true, resp: okResponse(
		entry("a@example.com", events...),
		entry("b@example.com", pipedreamEvent("b-1", 11)),
		entry("c@example.com", pipedreamEvent("c-1", 11)),
	)}

	_, _ = RunPipedreamCalendar(context.Background(), source, sink, pipedreamAccounts(), cfg)
	if run := sink.runFor(t, pipedreamAcctA.ID); run.status != "error" {
		t.Errorf("an entry of exactly %d events finished %q, want error. Criterion 8: len(events) must be "+
			"BELOW PipedreamMaxEvents — an entry landing exactly on the cap is indistinguishable from a "+
			"truncated one (BridgeMaxMessages' rule, bridge_ingest.go:150-154), and a truncated snapshot "+
			"supersedes everything it dropped", PipedreamMaxEvents, run.status)
	}
	assertNoRawWrites(t, sink, pipedreamAcctA.ID, "the entry hit the cap")
}

// Criterion 8: a non-empty `recurrence` array is the proof the workflow did NOT
// query with singleEvents=true. An unexpanded series is one raw row standing
// for an unbounded number of real meetings — the busy set would show one hour a
// year instead of one hour a week, and propose_slots would book over every
// later instance.
func TestRunPipedreamCalendar_RefusesAnUnexpandedRecurringSeries(t *testing.T) {
	cfg := pipedreamCfg()
	sink := newPipedreamSink(t)

	start := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Hour)
	series := json.RawMessage(fmt.Sprintf(
		`{"id":"weekly-standup","status":"confirmed","summary":"standup","start":{"dateTime":%q},`+
			`"end":{"dateTime":%q},"recurrence":["RRULE:FREQ=WEEKLY;BYDAY=MO"]}`,
		start.Format(time.RFC3339), start.Add(30*time.Minute).Format(time.RFC3339)))

	source := &fakePipedreamSource{echoRequest: true, resp: okResponse(
		entry("a@example.com", pipedreamEvent("a-fine", 12), series),
		entry("b@example.com", pipedreamEvent("b-1", 12)),
		entry("c@example.com", pipedreamEvent("c-1", 12)),
	)}

	_, _ = RunPipedreamCalendar(context.Background(), source, sink, pipedreamAccounts(), cfg)
	if run := sink.runFor(t, pipedreamAcctA.ID); run.status != "error" {
		t.Errorf("an event with a non-empty recurrence array finished %q, want error. Criterion 8: the "+
			"absence of `recurrence` is our only proof the workflow queried with singleEvents=true. An "+
			"unexpanded series is ONE raw row standing for every instance, so the busy set silently loses "+
			"all but the first and propose_slots offers time over a standing meeting", run.status)
	}
	assertNoRawWrites(t, sink, pipedreamAcctA.ID,
		"the good event that preceded the bad one must not be written either — validation is wholesale")
}

// Criterion 8: every event must parse through NormalizeCalendarEvent, the SAME
// pure mapper the Normalize phase will run. Not an approximation of it: one
// spelling of the shape. Premise 2 is why — an item that reaches raw and then
// fails Normalize stalls mail normalization, ObserveOutbound and the capture
// pass on every subsequent run, and keeps stalling until a human supersedes the
// row.
func TestRunPipedreamCalendar_RefusesAnEventTheNormalizerCannotParse(t *testing.T) {
	cases := map[string]json.RawMessage{
		"no id": json.RawMessage(
			`{"status":"confirmed","start":{"dateTime":"2027-01-01T10:00:00Z"},"end":{"dateTime":"2027-01-01T11:00:00Z"}}`),
		"start is not a time": json.RawMessage(
			`{"id":"broken-start","status":"confirmed","start":{"dateTime":"yesterday-ish"},"end":{"dateTime":"2027-01-01T11:00:00Z"}}`),
		"no interval at all on a live event": json.RawMessage(
			`{"id":"no-times","status":"confirmed","summary":"reshaped by a third party"}`),
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			// The premise, asserted rather than assumed: this item really is
			// one the Normalize phase would choke on.
			if _, err := NormalizeCalendarEvent(bad); err == nil {
				t.Fatalf("fixture %q parses fine; it cannot stand for an item that stalls Normalize", name)
			}

			cfg := pipedreamCfg()
			sink := newPipedreamSink(t)
			source := &fakePipedreamSource{echoRequest: true, resp: okResponse(
				entry("a@example.com", pipedreamEvent("a-good", 13), bad),
				entry("b@example.com", pipedreamEvent("b-1", 13)),
				entry("c@example.com", pipedreamEvent("c-1", 13)),
			)}

			_, _ = RunPipedreamCalendar(context.Background(), source, sink, pipedreamAccounts(), cfg)
			if run := sink.runFor(t, pipedreamAcctA.ID); run.status != "error" {
				t.Errorf("run finished %q, want error", run.status)
			}
			assertNoRawWrites(t, sink, pipedreamAcctA.ID,
				"a stored bad item stalls the WHOLE funnel — Normalize returns on the first bad calendar "+
					"item, before ObserveOutbound and EvaluateRules ever run (premise 2)")
		})
	}
}

// ---------------------------------------------------------------------------
// Criteria 9 + 10: raw-first, then windowed replacement.
// ---------------------------------------------------------------------------

func TestRunPipedreamCalendar_WritesRawVerbatimUnderTheCalendarPrefix(t *testing.T) {
	cfg := pipedreamCfg()
	sink := newPipedreamSink(t)
	eventA := pipedreamEvent("a-verbatim", 14)
	source := &fakePipedreamSource{echoRequest: true, resp: okResponse(
		entry("a@example.com", eventA),
		entry("b@example.com", pipedreamEvent("b-1", 14)),
		entry("c@example.com", pipedreamEvent("c-1", 14)),
	)}

	stats, err := RunPipedreamCalendar(context.Background(), source, sink, pipedreamAccounts(), cfg)
	if err != nil {
		t.Fatalf("RunPipedreamCalendar: %v", err)
	}

	key := pipedreamRawKey(pipedreamAcctA.ID, "calendar:a-verbatim")
	got, ok := sink.raw[key]
	if !ok {
		t.Fatalf("no raw row at %q. Criterion 9: each event object is written under "+
			"external_id = \"calendar:\" + id for that account's source_account_id, through the existing "+
			"upsertRaw path — provider JSON plus content_hash BEFORE anything normalizes (invariant 1)", key)
	}
	if string(got) != string(eventA) {
		t.Errorf("raw bytes were rewritten.\n got: %s\nwant: %s\nCriterion 9 says VERBATIM AS RECEIVED: the "+
			"raw row is the provider JSON as the third party handed it to us, and reprocessing "+
			"(--normalize-only --all) must be able to rebuild events from it with no Pipedream call",
			got, eventA)
	}
	if stats.RawInserted != 3 {
		t.Errorf("RawInserted = %d, want 3", stats.RawInserted)
	}

	// Re-polling the same snapshot writes nothing: upsertRaw's content_hash
	// short-circuit is what makes a full snapshot every 20 minutes cheap.
	source.requests = nil
	stats2, err := RunPipedreamCalendar(context.Background(), source, sink, pipedreamAccounts(), cfg)
	if err != nil {
		t.Fatalf("second RunPipedreamCalendar: %v", err)
	}
	if stats2.RawInserted != 0 || stats2.RawUpdated != 0 || stats2.RawUnchanged != 3 {
		t.Errorf("re-polling an unchanged snapshot gave %+v, want three unchanged and no writes — the "+
			"content_hash short-circuit is why every-poll-is-a-full-snapshot costs no database churn", stats2)
	}
}

func TestRunPipedreamCalendar_SupersedesAbsentEventsWithinTheEchoedWindow(t *testing.T) {
	ctx := context.Background()
	cfg := pipedreamCfg()
	sink := newPipedreamSink(t)

	// Two events already stored for A; the next snapshot carries only one.
	if err := sink.InsertRaw(ctx, pipedreamAcctA.ID, "calendar:kept", pipedreamEvent("kept", 15), "hash-kept"); err != nil {
		t.Fatalf("seed kept: %v", err)
	}
	if err := sink.InsertRaw(ctx, pipedreamAcctA.ID, "calendar:deleted", pipedreamEvent("deleted", 16), "hash-deleted"); err != nil {
		t.Fatalf("seed deleted: %v", err)
	}

	source := &fakePipedreamSource{echoRequest: true, resp: okResponse(
		entry("a@example.com", pipedreamEvent("kept", 15)),
		entry("b@example.com", pipedreamEvent("b-1", 15)),
		entry("c@example.com", pipedreamEvent("c-1", 15)),
	)}

	stats, err := RunPipedreamCalendar(ctx, source, sink, pipedreamAccounts(), cfg)
	if err != nil {
		t.Fatalf("RunPipedreamCalendar: %v", err)
	}

	var forA *pipedreamSupersede
	for i := range sink.sups {
		if sink.sups[i].accountID == pipedreamAcctA.ID {
			forA = &sink.sups[i]
		}
	}
	if forA == nil {
		t.Fatalf("SupersedeAbsentCalendar was never called for account %d. Criterion 10: every poll is a "+
			"full snapshot, so every poll is the bridge's reset case — an event deleted in Google is absent "+
			"from the next snapshot and must be superseded, or it contributes busy time forever",
			pipedreamAcctA.ID)
	}
	if !slices.Equal(forA.keep, []string{"calendar:kept"}) {
		t.Errorf("keep = %v, want [calendar:kept] — every external id PRESENT in this account's entry", forA.keep)
	}
	wantFrom, wantTo := cfg.Now.Add(-CalendarWindowPast), cfg.Now.Add(CalendarWindowFuture)
	if !forA.from.Equal(wantFrom) || !forA.to.Equal(wantTo) {
		t.Errorf("supersede window = %s..%s, want %s..%s — THE SAME BOUNDS THE REQUEST USED (criterion 10). "+
			"A wider window cancels events that were never asked for; a narrower one strands deletions",
			forA.from.Format(time.RFC3339), forA.to.Format(time.RFC3339),
			wantFrom.Format(time.RFC3339), wantTo.Format(time.RFC3339))
	}
	if !sink.superseded[pipedreamRawKey(pipedreamAcctA.ID, "calendar:deleted")] {
		t.Error("the event absent from the snapshot was not superseded; it keeps contributing busy time forever")
	}
	if sink.superseded[pipedreamRawKey(pipedreamAcctA.ID, "calendar:kept")] {
		t.Error("an event PRESENT in the snapshot was superseded")
	}
	if stats.CalendarSuperseded != 1 {
		t.Errorf("CalendarSuperseded = %d, want 1 — CalendarSuperseded carries the meaningful count on this "+
			"transport (CalendarResets deliberately stays zero: it means \"Google dropped our token\", which "+
			"never happens here, and incrementing it every poll would make it meaningless)",
			stats.CalendarSuperseded)
	}
	if stats.CalendarResets != 0 {
		t.Errorf("CalendarResets = %d, want 0 on the Pipedream transport", stats.CalendarResets)
	}
}

// ---------------------------------------------------------------------------
// Criterion 11: an empty snapshot must NEVER reach the supersede.
//
// The sink refuses an empty keep by design and that refusal must not be
// reached. A verified entry with zero events finishes ok — we did look, and the
// answer was valid — sets calendar_empty_snapshot, and keeps what we hold. The
// residual error is over-busy, the conservative direction, self-healing on the
// first non-empty snapshot; the alternative (letting the sink's refusal error
// the run) would take propose_slots down for EVERY account the moment one
// calendar is genuinely empty.
// ---------------------------------------------------------------------------

func TestRunPipedreamCalendar_EmptySnapshotFinishesOkAndKeepsStaleEvents(t *testing.T) {
	ctx := context.Background()

	t.Run("empty with nothing stored", func(t *testing.T) {
		cfg := pipedreamCfg()
		sink := newPipedreamSink(t)
		source := &fakePipedreamSource{echoRequest: true, resp: okResponse(
			entry("a@example.com"), // verified, zero events
			entry("b@example.com", pipedreamEvent("b-1", 17)),
			entry("c@example.com", pipedreamEvent("c-1", 17)),
		)}

		stats, err := RunPipedreamCalendar(ctx, source, sink, pipedreamAccounts(), cfg)
		if err != nil {
			t.Fatalf("a verified empty snapshot failed the pass: %v", err)
		}
		run := sink.runFor(t, pipedreamAcctA.ID)
		if run.status != "ok" {
			t.Errorf("a verified empty snapshot finished %q, want ok (%s). The run means \"we looked and got "+
				"a valid answer\", which is true. Erroring it would refuse forever for a genuinely empty "+
				"calendar and take propose_slots down for every account with it", run.status, run.errMsg)
		}
		if run.stats.CalendarEmptySnapshot != 1 {
			t.Errorf("calendar_empty_snapshot = %d on the run's stats, want 1 — counted and printed rather "+
				"than silent, because this is the ONE place the design accepts stale data",
				run.stats.CalendarEmptySnapshot)
		}
		if stats.CalendarEmptySnapshot != 1 {
			t.Errorf("returned Stats.CalendarEmptySnapshot = %d, want 1", stats.CalendarEmptySnapshot)
		}
		for _, s := range sink.sups {
			if s.accountID == pipedreamAcctA.ID {
				t.Errorf("SupersedeAbsentCalendar was called for the empty account with keep=%v. The sink "+
					"REFUSES an empty keep by design (sink.go:469-473) and criterion 11 says that refusal "+
					"must never be reached", s.keep)
			}
		}
	})

	t.Run("empty with events already stored", func(t *testing.T) {
		cfg := pipedreamCfg()
		sink := newPipedreamSink(t)
		if err := sink.InsertRaw(ctx, pipedreamAcctA.ID, "calendar:stale", pipedreamEvent("stale", 18), "hash-stale"); err != nil {
			t.Fatalf("seed stale: %v", err)
		}
		source := &fakePipedreamSource{echoRequest: true, resp: okResponse(
			entry("a@example.com"),
			entry("b@example.com", pipedreamEvent("b-1", 18)),
			entry("c@example.com", pipedreamEvent("c-1", 18)),
		)}

		if _, err := RunPipedreamCalendar(ctx, source, sink, pipedreamAccounts(), cfg); err != nil {
			t.Fatalf("a verified empty snapshot over stored events failed the pass: %v", err)
		}
		if run := sink.runFor(t, pipedreamAcctA.ID); run.status != "ok" || run.stats.CalendarEmptySnapshot != 1 {
			t.Errorf("run = %q with calendar_empty_snapshot=%d, want ok and 1",
				run.status, run.stats.CalendarEmptySnapshot)
		}
		if sink.superseded[pipedreamRawKey(pipedreamAcctA.ID, "calendar:stale")] {
			t.Error("the stale event was superseded on an empty snapshot. Criterion 11 keeps it: the " +
				"residual error is over-busy — refusing slots that are actually free — which is the " +
				"direction that cannot book over a real meeting, and it self-heals on the first non-empty poll")
		}
		if got := sink.rawIDsFor(pipedreamAcctA.ID); !slices.Equal(got, []string{"calendar:stale"}) {
			t.Errorf("account A holds %v, want the stale event still present", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Criterion 14: the advisory lock wraps the run row, and a busy account is
// skipped with NO run row at all.
// ---------------------------------------------------------------------------

func TestRunPipedreamCalendar_LocksAroundEachAccountAndSkipsBusyOnesWithoutARun(t *testing.T) {
	cfg := pipedreamCfg()
	sink := newPipedreamSink(t)
	sink.lockBusy[pipedreamAcctB.ID] = true

	source := &fakePipedreamSource{echoRequest: true, resp: okResponse(
		entry("a@example.com", pipedreamEvent("a-1", 19)),
		entry("b@example.com", pipedreamEvent("b-1", 19)),
		entry("c@example.com", pipedreamEvent("c-1", 19)),
	)}

	stats, err := RunPipedreamCalendar(context.Background(), source, sink, pipedreamAccounts(), cfg)
	if err != nil {
		t.Fatalf("a busy account failed the pass: %v", err)
	}
	if stats.AccountsBusy != 1 {
		t.Errorf("AccountsBusy = %d, want 1", stats.AccountsBusy)
	}
	if sink.hasRun(pipedreamAcctB.ID) {
		t.Errorf("a busy account got a sync_runs row: %+v. Criterion 14: it is skipped with NO run row — "+
			"another pass is doing the same work, and a second row would be a freshness signal for a poll "+
			"this process never made", sink.runs)
	}
	if got := sink.rawIDsFor(pipedreamAcctB.ID); len(got) != 0 {
		t.Errorf("a busy account was written to anyway: %v", got)
	}

	// Lock before StartRun, unlock after FinishRun: a run row opened outside the
	// lock is a row a concurrent pass can interleave with.
	assertPipedreamOrder(t, sink.trace, "lock:41", "start:41:calendar")
	assertPipedreamOrder(t, sink.trace, "finish:5001:ok", "unlock:41")
}

func assertPipedreamOrder(t *testing.T, trace []string, first, second string) {
	t.Helper()
	i, j := slices.Index(trace, first), slices.Index(trace, second)
	if i < 0 || j < 0 || i >= j {
		t.Errorf("trace does not show %q before %q: %v", first, second, trace)
	}
}

// ---------------------------------------------------------------------------
// Criterion 13: one sync_runs row per in-scope account per pass, phase
// "calendar", ok ONLY when that account's entry was verified, written and
// superseded.
// ---------------------------------------------------------------------------

func TestRunPipedreamCalendar_WritesExactlyOneCalendarRunPerAccount(t *testing.T) {
	cfg := pipedreamCfg()
	sink := newPipedreamSink(t)
	source := &fakePipedreamSource{echoRequest: true, resp: okResponse(
		entry("a@example.com", pipedreamEvent("a-1", 20)),
		entry("b@example.com", pipedreamEvent("b-1", 20)),
		entry("c@example.com", pipedreamEvent("c-1", 20)),
	)}

	if _, err := RunPipedreamCalendar(context.Background(), source, sink, pipedreamAccounts(), cfg); err != nil {
		t.Fatalf("RunPipedreamCalendar: %v", err)
	}
	if len(sink.runs) != 3 {
		t.Fatalf("sync_runs rows = %d, want exactly 3 (one per in-scope account per pass): %+v",
			len(sink.runs), sink.runs)
	}
	for _, run := range sink.runs {
		if run.phase != "calendar" {
			t.Errorf("run for account %d has phase %q, want \"calendar\": readiness keys on "+
				"stats->>'phase'='calendar' and nothing else (premise 3)", run.accountID, run.phase)
		}
		if run.status != "ok" {
			t.Errorf("run for account %d = %q (%s), want ok", run.accountID, run.status, run.errMsg)
		}
	}
}

// ---------------------------------------------------------------------------
// Criterion 17: the two new Stats fields are DIAGNOSTIC ONLY.
//
// omitempty on both, so a run from another transport does not grow keys that
// are present-and-zero. That is not cosmetic: IK, "One upworkcrm invocation
// writes TWO sync_runs rows" — telling run kinds apart by a stats payload is a
// landmine this repo has already paid for, because a shared struct makes the
// discriminating keys present-and-zero rather than absent.
// ---------------------------------------------------------------------------

func TestStats_CalendarSourceAndEmptySnapshotAreOmittedWhenUnset(t *testing.T) {
	raw, err := json.Marshal(Stats{})
	if err != nil {
		t.Fatalf("marshal Stats: %v", err)
	}
	for _, key := range []string{"calendar_source", "calendar_empty_snapshot"} {
		if strings.Contains(string(raw), key) {
			t.Errorf("a zero Stats marshals %q into sync_runs.stats: %s. Criterion 17 pins omitempty on "+
				"both — a key that is present-and-zero on every OTHER transport's runs invites exactly the "+
				"stats-payload discrimination the IK landmine records", key, raw)
		}
	}

	raw, err = json.Marshal(Stats{CalendarSource: "pipedream", CalendarEmptySnapshot: 1})
	if err != nil {
		t.Fatalf("marshal Stats: %v", err)
	}
	if !strings.Contains(string(raw), `"calendar_source":"pipedream"`) ||
		!strings.Contains(string(raw), `"calendar_empty_snapshot":1`) {
		t.Errorf("Stats marshalled to %s, want both diagnostic keys with the SPEC's spellings", raw)
	}
}

func TestRunPipedreamCalendar_StampsTheTransportOnTheRunForDiagnosisOnly(t *testing.T) {
	cfg := pipedreamCfg()
	sink := newPipedreamSink(t)
	source := &fakePipedreamSource{echoRequest: true, resp: okResponse(
		entry("a@example.com", pipedreamEvent("a-1", 21)),
		entry("b@example.com", pipedreamEvent("b-1", 21)),
		entry("c@example.com", pipedreamEvent("c-1", 21)),
	)}

	if _, err := RunPipedreamCalendar(context.Background(), source, sink, pipedreamAccounts(), cfg); err != nil {
		t.Fatalf("RunPipedreamCalendar: %v", err)
	}
	if got := sink.runFor(t, pipedreamAcctA.ID).stats.CalendarSource; got != "pipedream" {
		t.Errorf("run stats calendar_source = %q, want \"pipedream\". There are now two calendar transports "+
			"and \"which one ran\" is answered by CAL_SOURCE in the manifest PLUS this key on the run "+
			"(criterion 17) — diagnostic only: nothing may branch on it", got)
	}
}

// Criterion 14's last clause, stated on its own because six tests above
// deliberately do NOT assert the pass's return value: one failing account never
// aborts the others, and the pass fails only if NONE succeeded. That is
// runCalendarIngest's contract (premise 9), reused — a CronJob that exits
// non-zero because one of three calendars is broken is a CronJob whose exit
// code stops meaning anything.
func TestRunPipedreamCalendar_FailsThePassOnlyWhenNoAccountSucceeded(t *testing.T) {
	broken := func(calendarID string) PipedreamCalendarEntry {
		return PipedreamCalendarEntry{
			CalendarID: calendarID, Status: "error",
			Error: "invalid_grant: token has been expired or revoked",
		}
	}

	t.Run("one of three fails", func(t *testing.T) {
		cfg := pipedreamCfg()
		sink := newPipedreamSink(t)
		source := &fakePipedreamSource{echoRequest: true, resp: okResponse(
			entry("a@example.com", pipedreamEvent("a-1", 22)),
			broken("b@example.com"),
			entry("c@example.com", pipedreamEvent("c-1", 22)),
		)}
		if _, err := RunPipedreamCalendar(context.Background(), source, sink, pipedreamAccounts(), cfg); err != nil {
			t.Errorf("the pass failed because ONE account is broken: %v. One bad calendar must not blind "+
				"the others, and its error run already keeps propose_slots refusing honestly", err)
		}
		if run := sink.runFor(t, pipedreamAcctB.ID); run.status != "error" {
			t.Errorf("the broken account finished %q, want error", run.status)
		}
	})

	t.Run("all three fail", func(t *testing.T) {
		cfg := pipedreamCfg()
		sink := newPipedreamSink(t)
		source := &fakePipedreamSource{echoRequest: true, resp: okResponse(
			broken("a@example.com"), broken("b@example.com"), broken("c@example.com"),
		)}
		if _, err := RunPipedreamCalendar(context.Background(), source, sink, pipedreamAccounts(), cfg); err == nil {
			t.Error("the pass returned nil with EVERY account failing. Silence there lets a completely " +
				"broken workflow exit 0 in a CronJob log, and the only signal left is availability going " +
				"quiet an hour later")
		}
	})
}
