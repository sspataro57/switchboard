package google

// Unit tests for the Pipedream calendar WRITE route (SWT-28 /
// docs/tickets/calendar-booking_SPEC.md, acceptance criteria 1-7). ZERO
// network beyond loopback, ZERO Pipedream credentials: every case runs against
// an httptest server or an injected RoundTripper, exactly as
// pipedream_test.go does for the read poll.
//
// This file is the sibling of pipedream_test.go because CreateEvent is a
// METHOD on the same *PipedreamCalendarClient (SPEC "Decisions made
// unilaterally": one endpoint, one token read, one constructor validation, one
// classified-error discipline). Read pipedream.go:15-19 before touching an
// error string here.
//
// GREENFIELD NOTE: none of the write surface exists yet, so this file
// compile-FAILS under `go test ./...` — the expected red for SPEC-named
// greenfield code. The SPEC's contract IS the signature; the Go spelling is
// imposed here (internal/connector/google/pipedream_write.go, and
// calendarids.go or the same file for the two id helpers):
//
//	// Criterion 1: the READ envelope gains Action, omitempty, and the read
//	// poll leaves it EMPTY so the bytes on the wire are unchanged.
//	type PipedreamCalendarRequest struct {
//	    SchemaVersion int      `json:"schema_version"`
//	    Action        string   `json:"action,omitempty"`
//	    TimeMin       string   `json:"time_min"`
//	    TimeMax       string   `json:"time_max"`
//	    Calendars     []string `json:"calendars"`
//	}
//
//	// Criterion 2: the write envelope. NO attendees field, ever — an own block
//	// has no attendees, and the second policy-matrix row (invites with others)
//	// is deliberately unreachable from this struct.
//	type CreateEventRequest struct {
//	    SchemaVersion int    `json:"schema_version"`
//	    Action        string `json:"action"`        // "create_event"
//	    CalendarID    string `json:"calendar_id"`
//	    EventID       string `json:"event_id"`
//	    Start         string `json:"start"`         // RFC3339
//	    End           string `json:"end"`           // RFC3339
//	    Summary       string `json:"summary"`
//	    Description   string `json:"description"`
//	}
//
//	type CreateEventResponse struct {
//	    SchemaVersion int             `json:"schema_version"`
//	    Action        string          `json:"action"`
//	    CalendarID    string          `json:"calendar_id"`
//	    Status        string          `json:"status"`   // "ok" | "error"
//	    Created       bool            `json:"created"`
//	    Event         json.RawMessage `json:"event"`    // the Calendar v3 resource VERBATIM
//	    Error         string          `json:"error"`
//	}
//
//	func (c *PipedreamCalendarClient) CreateEvent(ctx context.Context, req CreateEventRequest) (CreateEventResponse, error)
//	func CalendarEventID(deliveryID int64, nonce int64) string
//	func CalendarExternalID(eventID string) string

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Shared fixtures. All times are derived from time.Now() — never a frozen
// calendar date, which ages out of the connector's [now-30d, now+90d] horizon
// and turns a green suite red for a reason that has nothing to do with the code.
// ---------------------------------------------------------------------------

// writeTimes returns a 15-minute block tomorrow, aligned to the hour, as the Z
// spelling the booker sends.
func writeTimes() (start, end time.Time) {
	start = time.Now().UTC().Add(24 * time.Hour).Truncate(time.Hour)
	return start, start.Add(15 * time.Minute)
}

// romeZone is a FIXED +02:00 offset, not Europe/Rome: the point of criterion 5
// is a byte-different, instant-equal echo, and a DST-dependent zone would make
// the test's own offset a moving target.
var romeZone = time.FixedZone("+0200", 2*60*60)

const writeTestEventID = "sb1zt2abc"

func writeReq(start, end time.Time) CreateEventRequest {
	return CreateEventRequest{
		SchemaVersion: PipedreamCalendarSchemaVersion,
		Action:        "create_event",
		CalendarID:    "sspataro@example.com",
		EventID:       writeTestEventID,
		Start:         start.Format(time.RFC3339),
		End:           end.Format(time.RFC3339),
		Summary:       "Focus block",
		Description:   "reserved for SWT-28 review",
	}
}

// eventResource builds a Google Calendar v3 Event resource with the given echo
// values. attendees is emitted only when non-empty (criterion 4's last check).
func eventResource(id, start, end string, attendees []string) string {
	var att string
	if len(attendees) > 0 {
		parts := make([]string, 0, len(attendees))
		for _, a := range attendees {
			parts = append(parts, `{"email":`+jsonQ(a)+`}`)
		}
		att = `,"attendees":[` + strings.Join(parts, ",") + `]`
	}
	return `{"kind":"calendar#event","id":` + jsonQ(id) +
		`,"status":"confirmed","start":{"dateTime":` + jsonQ(start) +
		`},"end":{"dateTime":` + jsonQ(end) + `}` + att + `}`
}

// jsonQ JSON-quotes a string (named short because this file uses it a lot).
func jsonQ(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// okWriteEnvelope is the workflow's success answer.
func okWriteEnvelope(calendarID, event string, created bool) string {
	c := "false"
	if created {
		c = "true"
	}
	return `{"schema_version":1,"action":"create_event","calendar_id":` + jsonQ(calendarID) +
		`,"status":"ok","created":` + c + `,"event":` + event + `}`
}

// writeClient points a real client at a test server.
func writeClient(t *testing.T, srv *httptest.Server) *PipedreamCalendarClient {
	t.Helper()
	c, err := NewPipedreamCalendarClient(srv.URL+pipedreamTestPath, pipedreamTestToken, srv.Client())
	if err != nil {
		t.Fatalf("NewPipedreamCalendarClient: %v", err)
	}
	return c
}

// ---------------------------------------------------------------------------
// Criterion 1: the READ request's bytes are unchanged. The workflow's trigger
// branches on `action`, and "absent or empty ⇒ the existing read path,
// unchanged" is the contract's FIRST requirement — a read that suddenly ships
// `"action":""` would take a branch nobody has run against production data.
// ---------------------------------------------------------------------------

func TestPipedreamCalendarRequest_ReadMarshalsWithoutAnActionKey(t *testing.T) {
	read := PipedreamCalendarRequest{
		SchemaVersion: PipedreamCalendarSchemaVersion,
		TimeMin:       "2026-09-06T09:00:00Z",
		TimeMax:       "2026-12-05T09:00:00Z",
		Calendars:     []string{"a@example.com"},
	}
	raw, err := json.Marshal(read)
	if err != nil {
		t.Fatalf("marshal read request: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("read request is not an object: %v (%s)", err, raw)
	}
	if _, present := keys["action"]; present {
		t.Errorf("the read request marshalled an `action` key (%s). Criterion 1: the read poll sends Action "+
			"EMPTY and the bytes on the wire must be byte-identical to today's, because the live workflow "+
			"branches on that key and the read path's code is not to be edited at all", raw)
	}
	if len(keys) != 4 {
		t.Errorf("read request has %d top-level keys (%s), want exactly 4: schema_version, time_min, "+
			"time_max, calendars", len(keys), raw)
	}

	// Positive control: without it this test passes against a struct that
	// simply has no Action field, which is the state of the repo today and is
	// NOT criterion 1. The field must exist and it must be omitempty.
	write := PipedreamCalendarRequest{SchemaVersion: 1, Action: "create_event"}
	raw, err = json.Marshal(write)
	if err != nil {
		t.Fatalf("marshal request with Action: %v", err)
	}
	if !strings.Contains(string(raw), `"action":"create_event"`) {
		t.Errorf("PipedreamCalendarRequest with Action set marshalled %s, want an `action` key — the field "+
			"must exist (criterion 1) and be `json:\"action,omitempty\"`", raw)
	}
}

// ---------------------------------------------------------------------------
// Criterion 2 + 3: the write envelope on the wire — same endpoint, same Bearer
// header, and NO ATTENDEES FIELD, EVER. The absence is structural, not a
// convention: "Calendar invites with attendees" is a different policy-matrix
// row (approve tier) with a different consent surface, and the SPEC's Out of
// scope says book_calendar_block must not grow an attendees argument later,
// because that would silently promote an approve-tier row to auto.
// ---------------------------------------------------------------------------

func TestCreateEvent_PostsBearerAndAWriteEnvelopeWithNoAttendees(t *testing.T) {
	start, end := writeTimes()
	var (
		gotMethod, gotPath, gotAuth, gotType string
		gotBody                              []byte
		calls                                int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okWriteEnvelope("sspataro@example.com",
			eventResource(writeTestEventID, start.Format(time.RFC3339), end.Format(time.RFC3339), nil), true)))
	}))
	defer srv.Close()

	resp, err := writeClient(t, srv).CreateEvent(context.Background(), writeReq(start, end))
	if err != nil {
		t.Fatalf("CreateEvent on a well-formed echo: %v", err)
	}
	if calls != 1 {
		t.Errorf("CreateEvent made %d HTTP calls for one booking, want exactly 1", calls)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST (the same endpoint the read poll uses)", gotMethod)
	}
	if gotPath != pipedreamTestPath {
		t.Errorf("path = %q, want the configured workflow path %q — one endpoint, one token", gotPath, pipedreamTestPath)
	}
	if gotAuth != "Bearer "+pipedreamTestToken {
		t.Errorf("Authorization = %q, want the same Bearer header the read poll sends", gotAuth)
	}
	if gotType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotType)
	}
	if !resp.Created {
		t.Errorf("CreateEventResponse.Created = false for a created:true answer; the flag distinguishes a "+
			"fresh insert from the 409 replay that makes a retried timeout safe (output %+v)", resp)
	}
	if len(resp.Event) == 0 {
		t.Errorf("CreateEventResponse.Event is empty; the Calendar v3 resource is returned VERBATIM because " +
			"criterion 22 stores it raw (raw-first) and the echo checks read id/start/end from it")
	}

	var sent map[string]any
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("write request body is not JSON: %v (%s)", err, gotBody)
	}
	if _, present := sent["attendees"]; present {
		t.Errorf("the write request carries an `attendees` key (%s). Criterion 2: the request carries NO "+
			"attendees field, ever — attendee invites are the approve-tier matrix row and this ticket ships "+
			"the auto-tier one; the field's absence is what stops that work half-arriving", gotBody)
	}
	if sent["action"] != "create_event" {
		t.Errorf("write request action = %v, want \"create_event\" (%s)", sent["action"], gotBody)
	}
	if sent["schema_version"] != float64(PipedreamCalendarSchemaVersion) {
		t.Errorf("write request schema_version = %v, want %d", sent["schema_version"], PipedreamCalendarSchemaVersion)
	}
	for _, k := range []string{"calendar_id", "event_id", "start", "end", "summary", "description"} {
		if _, present := sent[k]; !present {
			t.Errorf("write request is missing %q (%s)", k, gotBody)
		}
	}
	want := map[string]bool{"schema_version": true, "action": true, "calendar_id": true, "event_id": true,
		"start": true, "end": true, "summary": true, "description": true}
	for k := range sent {
		if !want[k] {
			t.Errorf("write request carries the unexpected key %q (%s); criterion 2 pins the envelope exactly",
				k, gotBody)
		}
	}
}

// ---------------------------------------------------------------------------
// Criterion 3: NO SECRET REACHES AN ERROR STRING, on the write route too. The
// endpoint URL is the token's NEIGHBOUR — a leaked workflow path plus a leaked
// token is the whole secret — so the transport error must be CLASSIFIED, never
// %w-wrapped: Go's *url.Error embeds the whole URL and the net errors under it
// embed the host.
//
// Under the auto tier this matters more than it did for the read poll: an
// unattended booking failure lands in audit_events.error and in deliveries.error,
// both of which are read by humans and shipped in logs.
// ---------------------------------------------------------------------------

func TestCreateEvent_ErrorsCarryNoTokenAndNoEndpoint(t *testing.T) {
	start, end := writeTimes()
	longBody := strings.Repeat("workflow blew up. ", 40) // > 200 chars

	cases := []struct {
		name      string
		handler   http.HandlerFunc
		dead      bool
		wantInErr string
	}{
		{
			name:      "401 unauthorized",
			handler:   func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "unauthorized", 401) },
			wantInErr: "401",
		},
		{
			name:      "500 with a long body",
			handler:   func(w http.ResponseWriter, _ *http.Request) { http.Error(w, longBody, 500) },
			wantInErr: "500",
		},
		{
			name:    "200 with a body that is not JSON",
			handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>oops</html>")) },
		},
		{
			name: "status:error from the workflow",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"schema_version":1,"action":"create_event","calendar_id":"sspataro@example.com","status":"error","error":"calendar not connected"}`))
			},
		},
		{name: "dial failure", dead: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				endpoint string
				hc       *http.Client
			)
			if tc.dead {
				// .invalid never resolves (RFC 2606): a dial failure with no
				// network dependency at all.
				endpoint = "http://pipedream-DISTINCTIVEHOST.invalid" + pipedreamTestPath
			} else {
				srv := httptest.NewServer(tc.handler)
				defer srv.Close()
				endpoint, hc = srv.URL+pipedreamTestPath, srv.Client()
			}
			parsed, err := url.Parse(endpoint)
			if err != nil {
				t.Fatalf("parse endpoint: %v", err)
			}
			client, err := NewPipedreamCalendarClient(endpoint, pipedreamTestToken, hc)
			if err != nil {
				t.Fatalf("NewPipedreamCalendarClient: %v", err)
			}

			_, err = client.CreateEvent(context.Background(), writeReq(start, end))
			if err == nil {
				t.Fatalf("CreateEvent succeeded on %s; every one of these is a refusal", tc.name)
			}
			msg := err.Error()
			if strings.Contains(msg, pipedreamTestToken) {
				t.Errorf("the error leaks the bearer token: %q", msg)
			}
			if strings.Contains(msg, parsed.Host) {
				t.Errorf("the error leaks the endpoint host %q: %q. The endpoint is the token's neighbour; "+
					"classify the transport error, never %%w-wrap it (*url.Error embeds the whole URL)",
					parsed.Host, msg)
			}
			if strings.Contains(msg, parsed.Path) {
				t.Errorf("the error leaks the workflow path %q: %q", parsed.Path, msg)
			}
			if tc.wantInErr != "" && !strings.Contains(msg, tc.wantInErr) {
				t.Errorf("error %q does not carry the HTTP status %s; an operator needs to tell 401 "+
					"(rotate the token) from 500 (the workflow is broken)", msg, tc.wantInErr)
			}
			if len(msg) > 400 {
				t.Errorf("error is %d characters; the body snippet is capped at 200 so a failure cannot dump "+
					"calendar content into a CronJob log", len(msg))
			}
		})
	}
}

// The maxBytes+1 capped read is the read client's, reused verbatim (criterion
// 3). A body at or over the cap is a refusal: a complete document padded to
// exactly the cap with more bytes behind it is the one truncation that stays
// valid JSON.
func TestCreateEvent_RefusesABodyAtOrOverTheCap(t *testing.T) {
	start, end := writeTimes()
	doc := okWriteEnvelope("sspataro@example.com",
		eventResource(writeTestEventID, start.Format(time.RFC3339), end.Format(time.RFC3339), nil), true)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(doc))
	}))
	defer srv.Close()

	client := writeClient(t, srv)
	client.maxBytes = int64(len(doc)) // exactly at the cap

	if _, err := client.CreateEvent(context.Background(), writeReq(start, end)); err == nil {
		t.Fatalf("a %d-byte body against a %d-byte cap was accepted; the write route reuses the read "+
			"client's maxBytes+1 LimitReader (criterion 3)", len(doc), len(doc))
	}
}

// ---------------------------------------------------------------------------
// Criterion 4: CreateEvent VERIFIES the response before returning success, and
// each refusal names which check failed.
//
// This is the auto tier's last line inside the transport: nothing downstream
// re-reads the workflow's answer, and criterion 22 stores the returned resource
// RAW into raw_source_items. A resource that is not the event we asked for would
// be written into the busy set as though it were.
// ---------------------------------------------------------------------------

func TestCreateEvent_VerifiesTheEchoBeforeReturningSuccess(t *testing.T) {
	start, end := writeTimes()
	startZ, endZ := start.Format(time.RFC3339), end.Format(time.RFC3339)
	req := writeReq(start, end)

	cases := []struct {
		name      string
		body      string
		wantAnyOf []string
	}{
		{
			name: "wrong schema_version",
			body: `{"schema_version":2,"action":"create_event","calendar_id":"sspataro@example.com","status":"ok","created":true,"event":` +
				eventResource(writeTestEventID, startZ, endZ, nil) + `}`,
			wantAnyOf: []string{"schema_version"},
		},
		{
			name: "wrong action",
			body: `{"schema_version":1,"action":"list_events","calendar_id":"sspataro@example.com","status":"ok","created":true,"event":` +
				eventResource(writeTestEventID, startZ, endZ, nil) + `}`,
			wantAnyOf: []string{"action"},
		},
		{
			name:      "status is not ok",
			body:      `{"schema_version":1,"action":"create_event","calendar_id":"sspataro@example.com","status":"error","error":"calendar not connected"}`,
			wantAnyOf: []string{"status", "calendar not connected"},
		},
		{
			name: "calendar_id is a different calendar",
			body: `{"schema_version":1,"action":"create_event","calendar_id":"someone-else@example.com","status":"ok","created":true,"event":` +
				eventResource(writeTestEventID, startZ, endZ, nil) + `}`,
			wantAnyOf: []string{"calendar_id", "calendar"},
		},
		{
			name: "the returned event has a different id",
			body: okWriteEnvelope("sspataro@example.com",
				eventResource("sbSOMETHINGELSE", startZ, endZ, nil), true),
			wantAnyOf: []string{"event_id", "id"},
		},
		{
			name: "the returned start is a different instant",
			body: okWriteEnvelope("sspataro@example.com",
				eventResource(writeTestEventID, start.Add(30*time.Minute).Format(time.RFC3339), endZ, nil), true),
			wantAnyOf: []string{"start"},
		},
		{
			name: "the returned end is a different instant",
			body: okWriteEnvelope("sspataro@example.com",
				eventResource(writeTestEventID, startZ, end.Add(45*time.Minute).Format(time.RFC3339), nil), true),
			wantAnyOf: []string{"end"},
		},
		{
			name: "the returned event carries attendees",
			body: okWriteEnvelope("sspataro@example.com",
				eventResource(writeTestEventID, startZ, endZ, []string{"someone@client.example"}), true),
			wantAnyOf: []string{"attendees"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			_, err := writeClient(t, srv).CreateEvent(context.Background(), req)
			if err == nil {
				t.Fatalf("CreateEvent returned SUCCESS for %s. Criterion 4 refuses each of these before "+
					"returning: nothing downstream re-reads the workflow's answer, and criterion 22 stores "+
					"the returned resource raw into raw_source_items and then into the busy set", tc.name)
			}
			msg := strings.ToLower(err.Error())
			ok := false
			for _, want := range tc.wantAnyOf {
				if strings.Contains(msg, strings.ToLower(want)) {
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("refusal for %s = %q; it must NAME which check failed (any of %v). A booking "+
					"refusal is read by a human in audit_events.error", tc.name, err, tc.wantAnyOf)
			}
		})
	}

	// The calendar_id echo is compared case-INSENSITIVELY (criterion 4 names
	// strings.EqualFold): an email address's domain is case-insensitive and
	// Google is free to hand back a different casing than we asked for.
	t.Run("calendar_id echo differing only in case is accepted", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(okWriteEnvelope("SSPataro@Example.COM",
				eventResource(writeTestEventID, startZ, endZ, nil), true)))
		}))
		defer srv.Close()
		if _, err := writeClient(t, srv).CreateEvent(context.Background(), req); err != nil {
			t.Errorf("CreateEvent refused a calendar_id echo differing only in case: %v. Criterion 4 pins "+
				"strings.EqualFold — a case-sensitive compare turns a landed booking into a spurious failure", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Criterion 5: THE START/END ECHO IS AN INSTANT COMPARISON, NOT A STRING ONE.
//
// This is the repo's oldest recorded landmine wearing a new costume (IK, "Exact
// text comparison across a provider round trip"): a provider may re-serialize a
// value without changing it, and code that compares the bytes then reports a
// difference that does not exist. Google normalizes event times to the
// CALENDAR'S timezone, so a block requested in Z comes back in +02:00 for a
// Europe/Rome calendar — the same instant, different bytes, every single time.
//
// A byte comparison here does not fail loudly: it fails AFTER the event is
// already on the calendar, marks the delivery failed with sent_external_id kept
// (SPEC "Decisions"), and leaves a real block that switchboard believes it
// never made.
// ---------------------------------------------------------------------------

func TestCreateEvent_EchoComparesInstantsNotStrings(t *testing.T) {
	start, end := writeTimes()
	req := writeReq(start, end) // Z spelling on the wire

	// The SAME instants, re-serialized with a +02:00 offset. Byte-different,
	// Equal-true.
	echoStart := start.In(romeZone).Format(time.RFC3339)
	echoEnd := end.In(romeZone).Format(time.RFC3339)
	if echoStart == req.Start || echoEnd == req.End {
		t.Fatalf("the test's own fixture is not byte-different (%q vs %q); the case proves nothing", echoStart, req.Start)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(okWriteEnvelope("sspataro@example.com",
			eventResource(writeTestEventID, echoStart, echoEnd, nil), true)))
	}))
	defer srv.Close()

	if _, err := writeClient(t, srv).CreateEvent(context.Background(), req); err != nil {
		t.Fatalf("CreateEvent refused an echo of %q/%q for a request of %q/%q: %v\n\n"+
			"These are THE SAME INSTANT. Criterion 5: parse both sides to time.Time and compare with "+
			"time.Time.Equal — never string equality. Google re-serializes event times into the calendar's "+
			"own timezone, so this is not an edge case, it is what production returns for every Europe/Rome "+
			"booking. The failure would land after the block is already on the calendar.",
			echoStart, echoEnd, req.Start, req.End, err)
	}
}

// ---------------------------------------------------------------------------
// Criterion 7: EXACTLY ONE retry, and ONLY on a transport error or context
// deadline — never on any HTTP response, however shaped.
//
// The asymmetry is the whole point. A retry is safe only because the id is
// OURS and a duplicate insert answers 409, which the workflow turns into
// events.get + created:false. A non-2xx response PROVES the workflow was
// reached and must not be re-driven: it may have inserted and then failed on
// its own response step, and a second drive would run its logic twice.
// ---------------------------------------------------------------------------

// flakyTransport fails the first n round trips with a transport error, then
// delegates. The error deliberately embeds the host, exactly as *url.Error
// does — so an implementation that %w-wraps it also fails the leak test above.
type flakyTransport struct {
	failFirst int
	calls     int
	inner     http.RoundTripper
}

func (f *flakyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.calls++
	if f.calls <= f.failFirst {
		return nil, fmt.Errorf("simulated connection reset talking to %s", r.URL.Host)
	}
	return f.inner.RoundTrip(r)
}

func TestCreateEvent_RetriesOnceOnTransportErrorAndNeverOnAnHTTPResponse(t *testing.T) {
	start, end := writeTimes()
	startZ, endZ := start.Format(time.RFC3339), end.Format(time.RFC3339)

	t.Run("transport error on the first connection: one retry, then success", func(t *testing.T) {
		var handled int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			handled++
			_, _ = w.Write([]byte(okWriteEnvelope("sspataro@example.com",
				eventResource(writeTestEventID, startZ, endZ, nil), false)))
		}))
		defer srv.Close()

		ft := &flakyTransport{failFirst: 1, inner: srv.Client().Transport}
		client, err := NewPipedreamCalendarClient(srv.URL+pipedreamTestPath, pipedreamTestToken, &http.Client{Transport: ft})
		if err != nil {
			t.Fatalf("NewPipedreamCalendarClient: %v", err)
		}
		resp, err := client.CreateEvent(context.Background(), writeReq(start, end))
		if err != nil {
			t.Fatalf("CreateEvent did not retry a transport failure: %v. Criterion 7: exactly one retry on a "+
				"transport error — safe precisely because the event id is ours and a duplicate insert answers "+
				"409, which the workflow resolves with events.get + created:false", err)
		}
		if ft.calls != 2 {
			t.Errorf("round trips = %d, want exactly 2 (one failure + one retry). More than one retry is not "+
				"'exactly once'", ft.calls)
		}
		if handled != 1 {
			t.Errorf("the workflow was reached %d times, want 1", handled)
		}
		if resp.Created {
			t.Errorf("Created = true for a created:false answer; the flag is how the 409 replay is visible")
		}
	})

	t.Run("HTTP 500: exactly ONE request, no retry", func(t *testing.T) {
		var handled int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			handled++
			http.Error(w, "workflow step failed", http.StatusInternalServerError)
		}))
		defer srv.Close()

		if _, err := writeClient(t, srv).CreateEvent(context.Background(), writeReq(start, end)); err == nil {
			t.Fatal("CreateEvent succeeded on a 500")
		}
		if handled != 1 {
			t.Errorf("a 500 produced %d requests, want exactly 1. Criterion 7: a non-2xx response PROVES the "+
				"workflow was reached, so it must not be re-driven — it may have inserted the event and then "+
				"failed on its own response step, and a second drive runs its logic twice", handled)
		}
	})

	t.Run("HTTP 409: exactly ONE request, no retry", func(t *testing.T) {
		// The workflow is specified to resolve 409 itself (events.get +
		// created:false), so a 409 arriving HERE means the workflow did not,
		// and re-driving it cannot help.
		var handled int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			handled++
			http.Error(w, "duplicate id", http.StatusConflict)
		}))
		defer srv.Close()

		if _, err := writeClient(t, srv).CreateEvent(context.Background(), writeReq(start, end)); err == nil {
			t.Fatal("CreateEvent succeeded on a 409")
		}
		if handled != 1 {
			t.Errorf("a 409 produced %d requests, want exactly 1 (never retry on an HTTP response)", handled)
		}
	})

	t.Run("status:error envelope: exactly ONE request, no retry", func(t *testing.T) {
		var handled int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			handled++
			_, _ = w.Write([]byte(`{"schema_version":1,"action":"create_event","calendar_id":"sspataro@example.com","status":"error","error":"nope"}`))
		}))
		defer srv.Close()

		if _, err := writeClient(t, srv).CreateEvent(context.Background(), writeReq(start, end)); err == nil {
			t.Fatal("CreateEvent succeeded on a status:error envelope")
		}
		if handled != 1 {
			t.Errorf("a status:error envelope produced %d requests, want exactly 1", handled)
		}
	})

	t.Run("a transport error on BOTH attempts fails after exactly two", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
		defer srv.Close()
		ft := &flakyTransport{failFirst: 99, inner: srv.Client().Transport}
		client, err := NewPipedreamCalendarClient(srv.URL+pipedreamTestPath, pipedreamTestToken, &http.Client{Transport: ft})
		if err != nil {
			t.Fatalf("NewPipedreamCalendarClient: %v", err)
		}
		if _, err := client.CreateEvent(context.Background(), writeReq(start, end)); err == nil {
			t.Fatal("CreateEvent succeeded with every round trip failing")
		}
		if ft.calls != 2 {
			t.Errorf("round trips = %d, want exactly 2 (one attempt + one retry, then give up)", ft.calls)
		}
	})
}

// ---------------------------------------------------------------------------
// Criterion 6a: CalendarEventID's alphabet and length.
//
// Google's events.insert accepts a CLIENT-SUPPLIED id, and that is the whole
// idempotency story: the id is chosen and committed with status='sending'
// BEFORE the POST, so a retried timeout answers 409 instead of double-booking.
// The id must therefore be legal to Google or the reservation is worthless —
// base32hex, [0-9a-v], 5..1024 characters.
// ---------------------------------------------------------------------------

func TestCalendarEventID_IsBase32HexAndLongEnough(t *testing.T) {
	cases := []struct {
		name     string
		delivery int64
		nonce    int64
	}{
		{"the first delivery, a plausible nonce", 1, time.Now().UnixNano()},
		{"a four-digit delivery", 4207, time.Now().UnixNano()},
		{"a large delivery id", 9_000_000_000, time.Now().UnixNano()},
		{"a small nonce", 12, 31},
		{"nonce zero", 7, 0},
		{"delivery and nonce both zero", 0, 0},
		{"max int64 nonce", 5, 9223372036854775807},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CalendarEventID(tc.delivery, tc.nonce)
			if len(got) < 5 {
				t.Errorf("CalendarEventID(%d,%d) = %q (%d chars); Google requires 5..1024",
					tc.delivery, tc.nonce, got, len(got))
			}
			if len(got) > 1024 {
				t.Errorf("CalendarEventID(%d,%d) is %d chars; Google's cap is 1024", tc.delivery, tc.nonce, len(got))
			}
			for i, r := range got {
				if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'v') {
					t.Errorf("CalendarEventID(%d,%d) = %q: character %d (%q) is outside Google's base32hex "+
						"id alphabet [0-9a-v]. events.insert rejects the id, so the pre-committed "+
						"sent_external_id reserves nothing and the retry-safety argument collapses",
						tc.delivery, tc.nonce, got, i, string(r))
					break
				}
			}
			seen[got] = true
		})
	}
	if len(seen) != len(cases) {
		t.Errorf("the %d id inputs produced %d distinct ids; two deliveries sharing an id would collide on "+
			"deliveries_sent_external_idx and lock the second row out permanently", len(cases), len(seen))
	}
}

// ---------------------------------------------------------------------------
// Criterion 6b: `calendar:{event_id}` has ONE SPELLING, and the three ingest
// sites call it.
//
// A structural test, in the shape of internal/textmatch/callsites_test.go and
// internal/connector/upworkcrm/keyspelling_test.go, because the repo has paid
// four times for a rule that lived only in prose. The stakes here: the raw
// item's external_id written at SEND time and the one written by the next
// */20 read poll must be BYTE-IDENTICAL, or the content_hash short-circuit
// misses, the poll's windowed replacement supersedes our own row, and
// confirmCalendarDelivery's exact-id match never fires — a delivery that can
// never close its loop, with no error anywhere.
// ---------------------------------------------------------------------------

func TestCalendarExternalID_IsTheOneSpelling(t *testing.T) {
	// The three sites criterion 6 names by path.
	callers := []string{"ingest.go", "pipedream_ingest.go", "bridge_ingest.go"}

	definition, err := os.ReadFile("pipedream_write.go")
	if err != nil {
		// calendarids.go is the SPEC's alternative home for the two helpers.
		definition, err = os.ReadFile("calendarids.go")
	}
	if err != nil {
		t.Fatalf("neither pipedream_write.go nor calendarids.go exists; criterion 6 puts CalendarEventID and "+
			"CalendarExternalID in one of them: %v", err)
	}
	if !strings.Contains(string(definition), "func CalendarExternalID(") {
		t.Errorf("no `func CalendarExternalID(` in the write file. Criterion 6: it returns " +
			"\"calendar:\" + eventID and is the ONE spelling")
	}
	if !strings.Contains(string(definition), "func CalendarEventID(") {
		t.Errorf("no `func CalendarEventID(` in the write file (criterion 6)")
	}

	for _, name := range callers {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if len(src) == 0 {
			t.Fatalf("%s is empty; the scan would be vacuous", name)
		}
		text := string(src)
		if !strings.Contains(text, "CalendarExternalID(") {
			t.Errorf("%s does not call CalendarExternalID. Criterion 6 changes all three ingest sites to "+
				"call it, so the id the send path writes and the id the next poll writes cannot drift",
				filepath.Join("internal", "connector", "google", name))
		}
		if buildsTheLiteral(text) {
			t.Errorf("%s still builds the external id from the raw \"calendar:\" literal. Two spellings of "+
				"one key is how the send-time raw row and the poll's raw row come to disagree: the "+
				"content_hash short-circuit misses, the poll's windowed REPLACEMENT supersedes our own "+
				"row, and confirmCalendarDelivery's exact-id match never fires — silently, forever", name)
		}
	}
}

// buildsTheLiteral reports whether the source CONCATENATES the raw prefix,
// ignoring whole-line // comments so prose may quote the spelling it bans (the
// upworkcrm keyspelling allowance). A prefix TEST (normalize.go's
// strings.HasPrefix dispatch) is not a construction and is deliberately not
// scanned here — criterion 6 changes the three construction sites only.
func buildsTheLiteral(src string) bool {
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.Contains(line, `"calendar:"`) {
			return true
		}
	}
	return false
}
