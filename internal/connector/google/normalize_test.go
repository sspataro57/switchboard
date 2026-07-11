package google_test

// Unit tests for the deterministic Gmail/Calendar raw -> canonical normalizers
// (SPEC 07-google-oauth-pollers, acceptance criteria 7, 8, 10; invariant 7
// discipline transfer: normalize is a PURE function of the raw provider JSON —
// zero network, zero Postgres, no LLM). Input is exactly the raw_source_items
// .raw_json bytes: for Gmail, the `format=full` messages.get JSON; for Calendar,
// a singleEvents events.list item. Criterion 4 (re-normalize from raw alone)
// requires these to depend on nothing but the raw row and the own-account set.
//
// GREENFIELD NOTE: package internal/connector/google does not exist yet; this
// file compile-FAILs under `go test ./...` until it is implemented — the
// expected failure mode. For greenfield code the SPEC's contract IS the
// signature; the imposed exported surface exercised here (the SPEC's
// normalize.go) is:
//
//   // Channel value for every Gmail message.
//   const Channel = "gmail"
//
//   type NormalizedMessage struct {
//       ThreadKey         string    // gmail:{account_email}:{gmailThreadId}
//       ExternalMessageID string    // RFC 5322 Message-ID verbatim (brackets kept),
//                                    // trimmed; fallback gmail:{gmailMessageId} when absent
//       Direction         string    // inbound | outbound (own-account rule, criterion 8)
//       SentAt            time.Time  // from internalDate (unix ms)
//       Subject           string     // Subject header
//       Sender            string     // From header (verbatim, may include display name)
//       BodyText          string     // first text/plain part (base64url-decoded, nested
//                                    // multipart walked) else the API snippet
//       Channel           string     // "gmail"
//       GmailMessageID    string
//       GmailThreadID     string
//   }
//
//   // Pure mapper. accountEmail names the mailbox this raw copy came from
//   // (thread key qualifier); ownEmails is the set of ALL provider='google'
//   // account emails, loaded once at normalize start — the direction rule keys
//   // on it (criterion 8). Address matching is case-insensitive on the email
//   // address extracted from a possibly "Display Name <addr>" From header.
//   func NormalizeGmailMessage(raw json.RawMessage, accountEmail string, ownEmails map[string]bool) (NormalizedMessage, error)
//
//   type Attendee struct {
//       Email          string `json:"email"`
//       ResponseStatus string `json:"response_status"`
//       Organizer      bool   `json:"organizer"`
//       Self           bool   `json:"self"`
//   }
//   type NormalizedEvent struct {
//       StartsAt     time.Time
//       EndsAt       time.Time
//       Title        string   // summary
//       Status       string   // confirmed | tentative | cancelled (verbatim)
//       Transparency string   // absent => "opaque" (criterion 10)
//       AllDay       bool     // true for the all-day date form
//       Attendees    []Attendee
//   }
//   func NormalizeCalendarEvent(raw json.RawMessage) (NormalizedEvent, error)

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

const (
	acctA = "itest-google-a@example.com"
	acctB = "itest-google-b@example.com"
)

// b64url encodes a Gmail message body part as UNPADDED base64url — exactly what
// the Gmail API returns for body.data. The normalizer must decode base64url,
// tolerating the missing padding.
func b64url(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

// ---- Gmail ---------------------------------------------------------------

// A plain inbound message: Message-ID kept verbatim (brackets), From/Subject
// headers extracted, text/plain body base64url-decoded, sent_at from
// internalDate, thread key qualified by the receiving mailbox, direction inbound
// because From is a stranger.
func TestNormalizeGmailMessage_HeadersAndBody(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "gm-1",
		"threadId": "th-1",
		"labelIds": ["INBOX"],
		"snippet": "preview that must be ignored when text/plain exists",
		"internalDate": "1751362245000",
		"payload": {
			"mimeType": "multipart/alternative",
			"headers": [
				{"name":"Message-ID","value":"<abc-123@mail.acme.example>"},
				{"name":"Subject","value":"Milestone review"},
				{"name":"From","value":"Alice Client <alice@acme.example>"},
				{"name":"To","value":"` + acctA + `"}
			],
			"parts": [
				{"mimeType":"text/plain","body":{"data":"` + b64url("the real plain body") + `"}},
				{"mimeType":"text/html","body":{"data":"` + b64url("<p>html copy</p>") + `"}}
			]
		}
	}`)

	own := map[string]bool{acctA: true, acctB: true}
	nm, err := google.NormalizeGmailMessage(raw, acctA, own)
	if err != nil {
		t.Fatalf("NormalizeGmailMessage: %v", err)
	}

	wantSent := time.UnixMilli(1751362245000).UTC()
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"ExternalMessageID", nm.ExternalMessageID, "<abc-123@mail.acme.example>"},
		{"ThreadKey", nm.ThreadKey, "gmail:" + acctA + ":th-1"},
		{"Direction", nm.Direction, "inbound"},
		{"SentAt", nm.SentAt.UTC(), wantSent},
		{"Subject", nm.Subject, "Milestone review"},
		{"Sender", nm.Sender, "Alice Client <alice@acme.example>"},
		{"BodyText", nm.BodyText, "the real plain body"},
		{"Channel", nm.Channel, "gmail"},
		{"GmailMessageID", nm.GmailMessageID, "gm-1"},
		{"GmailThreadID", nm.GmailThreadID, "th-1"},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// Nested multipart: multipart/mixed -> multipart/alternative -> text/plain. The
// walker must recurse into nested parts to find the first text/plain leaf.
func TestNormalizeGmailMessage_NestedMultipartTextPlain(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "gm-2",
		"threadId": "th-2",
		"internalDate": "1751362245000",
		"payload": {
			"mimeType": "multipart/mixed",
			"headers": [
				{"name":"Message-ID","value":"<nested@mail.example>"},
				{"name":"From","value":"stranger@x.example"}
			],
			"parts": [
				{"mimeType":"multipart/alternative","parts":[
					{"mimeType":"text/plain","body":{"data":"` + b64url("deep plain text") + `"}},
					{"mimeType":"text/html","body":{"data":"` + b64url("<b>x</b>") + `"}}
				]},
				{"mimeType":"application/pdf","body":{"attachmentId":"att-1"}}
			]
		}
	}`)

	nm, err := google.NormalizeGmailMessage(raw, acctA, map[string]bool{acctA: true})
	if err != nil {
		t.Fatalf("NormalizeGmailMessage: %v", err)
	}
	if nm.BodyText != "deep plain text" {
		t.Errorf("BodyText = %q, want %q (nested text/plain must be walked)", nm.BodyText, "deep plain text")
	}
}

// Direction rule (criterion 8, invariant 5 seam): From address matching ANY
// google account_email (even in "Display Name <addr>" form, case-insensitively)
// makes the message outbound in every mailbox copy — so our own sends can never
// be re-triaged as inbound.
func TestNormalizeGmailMessage_DirectionOutboundWhenFromInAccountSet(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "gm-3",
		"threadId": "th-3",
		"internalDate": "1751362245000",
		"payload": {
			"headers": [
				{"name":"Message-ID","value":"<own@mail.example>"},
				{"name":"From","value":"Me <` + acctA + `>"},
				{"name":"To","value":"client@acme.example"}
			],
			"mimeType":"text/plain",
			"body":{"data":"` + b64url("our own reply") + `"}
		}
	}`)

	// Normalize the copy that landed in mailbox B (recipient); From is still one
	// of OUR accounts, so it must be outbound here too.
	own := map[string]bool{acctA: true, acctB: true}
	nm, err := google.NormalizeGmailMessage(raw, acctB, own)
	if err != nil {
		t.Fatalf("NormalizeGmailMessage: %v", err)
	}
	if nm.Direction != "outbound" {
		t.Errorf("Direction = %q, want outbound (From is one of our accounts, even in the recipient mailbox copy)", nm.Direction)
	}
}

// From a stranger -> inbound (the common case, asserted independently of the
// combined field test).
func TestNormalizeGmailMessage_DirectionInboundForStranger(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "gm-3b",
		"threadId": "th-3b",
		"internalDate": "1751362245000",
		"payload": {
			"headers": [
				{"name":"Message-ID","value":"<in@mail.example>"},
				{"name":"From","value":"outsider@world.example"}
			],
			"mimeType":"text/plain",
			"body":{"data":"` + b64url("hi") + `"}
		}
	}`)
	nm, err := google.NormalizeGmailMessage(raw, acctA, map[string]bool{acctA: true, acctB: true})
	if err != nil {
		t.Fatalf("NormalizeGmailMessage: %v", err)
	}
	if nm.Direction != "inbound" {
		t.Errorf("Direction = %q, want inbound (From not in the account set)", nm.Direction)
	}
}

// Fallbacks: no Message-ID header -> external_message_id = gmail:{gmailMessageId};
// no text/plain part -> body_text = the API snippet.
func TestNormalizeGmailMessage_FallbackMessageIDAndSnippet(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "gm-4",
		"threadId": "th-4",
		"snippet": "snippet is the fallback body",
		"internalDate": "1751362245000",
		"payload": {
			"mimeType": "text/html",
			"headers": [
				{"name":"From","value":"noheader@x.example"},
				{"name":"Subject","value":"no message id"}
			],
			"body":{"data":"` + b64url("<p>only html here</p>") + `"}
		}
	}`)

	nm, err := google.NormalizeGmailMessage(raw, acctA, map[string]bool{acctA: true})
	if err != nil {
		t.Fatalf("NormalizeGmailMessage: %v", err)
	}
	if nm.ExternalMessageID != "gmail:gm-4" {
		t.Errorf("ExternalMessageID = %q, want gmail:gm-4 (fallback when Message-ID header absent)", nm.ExternalMessageID)
	}
	if nm.BodyText != "snippet is the fallback body" {
		t.Errorf("BodyText = %q, want the snippet (no text/plain part present)", nm.BodyText)
	}
}

// Determinism (invariant 7): identical raw -> identical output on repeated calls.
func TestNormalizeGmailMessage_Deterministic(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "gm-5",
		"threadId": "th-5",
		"internalDate": "1751362245000",
		"payload": {
			"headers": [
				{"name":"Message-ID","value":"<det@mail.example>"},
				{"name":"From","value":"a@b.example"}
			],
			"mimeType":"text/plain",
			"body":{"data":"` + b64url("stable") + `"}
		}
	}`)
	own := map[string]bool{acctA: true}
	a, err := google.NormalizeGmailMessage(raw, acctA, own)
	if err != nil {
		t.Fatalf("NormalizeGmailMessage (a): %v", err)
	}
	b, err := google.NormalizeGmailMessage(raw, acctA, own)
	if err != nil {
		t.Fatalf("NormalizeGmailMessage (b): %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("normalization not deterministic:\n a=%+v\n b=%+v", a, b)
	}
}

// ---- Calendar ------------------------------------------------------------

// dateTime form: timed event with start/end, title, status, explicit
// transparency, attendees mapped to the canonical shape; all_day=false.
func TestNormalizeCalendarEvent_DateTimeForm(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "evt-1",
		"status": "confirmed",
		"summary": "Standup",
		"transparency": "opaque",
		"start": {"dateTime":"2026-07-13T09:00:00+02:00"},
		"end":   {"dateTime":"2026-07-13T09:30:00+02:00"},
		"attendees": [
			{"email":"a@x.example","responseStatus":"accepted","organizer":true,"self":true},
			{"email":"b@y.example","responseStatus":"needsAction"}
		]
	}`)

	ne, err := google.NormalizeCalendarEvent(raw)
	if err != nil {
		t.Fatalf("NormalizeCalendarEvent: %v", err)
	}
	wantStart, _ := time.Parse(time.RFC3339, "2026-07-13T09:00:00+02:00")
	wantEnd, _ := time.Parse(time.RFC3339, "2026-07-13T09:30:00+02:00")
	if !ne.StartsAt.Equal(wantStart) {
		t.Errorf("StartsAt = %v, want %v", ne.StartsAt, wantStart)
	}
	if !ne.EndsAt.Equal(wantEnd) {
		t.Errorf("EndsAt = %v, want %v", ne.EndsAt, wantEnd)
	}
	if ne.Title != "Standup" {
		t.Errorf("Title = %q, want Standup", ne.Title)
	}
	if ne.Status != "confirmed" {
		t.Errorf("Status = %q, want confirmed", ne.Status)
	}
	if ne.Transparency != "opaque" {
		t.Errorf("Transparency = %q, want opaque", ne.Transparency)
	}
	if ne.AllDay {
		t.Errorf("AllDay = true, want false for the dateTime form")
	}
	want := []google.Attendee{
		{Email: "a@x.example", ResponseStatus: "accepted", Organizer: true, Self: true},
		{Email: "b@y.example", ResponseStatus: "needsAction", Organizer: false, Self: false},
	}
	if !reflect.DeepEqual(ne.Attendees, want) {
		t.Errorf("Attendees = %+v, want %+v", ne.Attendees, want)
	}
}

// all-day date form: start.date/end.date -> all_day=true; Google marks all-day
// events transparent, so they will fall out of busy naturally (criterion 11).
func TestNormalizeCalendarEvent_AllDayDateForm(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "evt-2",
		"status": "confirmed",
		"summary": "Company holiday",
		"transparency": "transparent",
		"start": {"date":"2026-07-14"},
		"end":   {"date":"2026-07-15"}
	}`)

	ne, err := google.NormalizeCalendarEvent(raw)
	if err != nil {
		t.Fatalf("NormalizeCalendarEvent: %v", err)
	}
	if !ne.AllDay {
		t.Errorf("AllDay = false, want true for the date form")
	}
	if ne.Transparency != "transparent" {
		t.Errorf("Transparency = %q, want transparent (all-day default)", ne.Transparency)
	}
	wantStart, _ := time.Parse("2006-01-02", "2026-07-14")
	if ne.StartsAt.Year() != wantStart.Year() || ne.StartsAt.Month() != wantStart.Month() || ne.StartsAt.Day() != wantStart.Day() {
		t.Errorf("StartsAt = %v, want the 2026-07-14 date", ne.StartsAt)
	}
}

// Absent transparency -> "opaque" (criterion 10 default).
func TestNormalizeCalendarEvent_TransparencyDefaultsOpaque(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "evt-3",
		"status": "confirmed",
		"summary": "Focus block",
		"start": {"dateTime":"2026-07-13T14:00:00+02:00"},
		"end":   {"dateTime":"2026-07-13T15:00:00+02:00"}
	}`)
	ne, err := google.NormalizeCalendarEvent(raw)
	if err != nil {
		t.Fatalf("NormalizeCalendarEvent: %v", err)
	}
	if ne.Transparency != "opaque" {
		t.Errorf("Transparency = %q, want opaque when the field is absent", ne.Transparency)
	}
}

// Cancelled instances are normalized with status='cancelled', never dropped
// (criterion 6 — availability later excludes them).
func TestNormalizeCalendarEvent_CancelledStatusPreserved(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "evt-4_20260713T090000Z",
		"status": "cancelled",
		"start": {"dateTime":"2026-07-13T09:00:00+02:00"},
		"end":   {"dateTime":"2026-07-13T09:30:00+02:00"}
	}`)
	ne, err := google.NormalizeCalendarEvent(raw)
	if err != nil {
		t.Fatalf("NormalizeCalendarEvent: %v", err)
	}
	if ne.Status != "cancelled" {
		t.Errorf("Status = %q, want cancelled (never deleted)", ne.Status)
	}
}

// Determinism for the calendar mapper too.
func TestNormalizeCalendarEvent_Deterministic(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "evt-5",
		"status": "confirmed",
		"summary": "Sync",
		"transparency": "opaque",
		"start": {"dateTime":"2026-07-13T11:00:00+02:00"},
		"end":   {"dateTime":"2026-07-13T11:30:00+02:00"},
		"attendees": [{"email":"a@x.example","responseStatus":"accepted"}]
	}`)
	a, err := google.NormalizeCalendarEvent(raw)
	if err != nil {
		t.Fatalf("NormalizeCalendarEvent (a): %v", err)
	}
	b, err := google.NormalizeCalendarEvent(raw)
	if err != nil {
		t.Fatalf("NormalizeCalendarEvent (b): %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("calendar normalization not deterministic:\n a=%+v\n b=%+v", a, b)
	}
}
