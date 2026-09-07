package google

// The Pipedream calendar WRITE route (SWT-28): create_event through the same
// workflow, endpoint and Bearer token as the read poll. One client, one
// constructor validation, one classified-error discipline — read
// pipedream.go:15-19 before touching an error string here: NO SECRET REACHES
// AN ERROR STRING, and the endpoint URL is the token's neighbour.
//
// Retry contract (criterion 7): EXACTLY ONE retry, and only on a transport
// error or context deadline — never on any HTTP response, however shaped. The
// retry is safe only because the event id is OURS (committed with
// status='sending' before the POST), so a duplicate insert answers 409, which
// the workflow resolves with events.get + created:false. A non-2xx response
// proves the workflow was reached and must not be re-driven.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CreateEventRequest is the write envelope (criterion 2). It carries NO
// attendees field, ever: an own block has no attendees, and the approve-tier
// matrix row (invites with others) is deliberately unreachable from this
// struct so that work cannot half-arrive.
type CreateEventRequest struct {
	SchemaVersion int    `json:"schema_version"`
	Action        string `json:"action"` // "create_event"
	CalendarID    string `json:"calendar_id"`
	EventID       string `json:"event_id"`
	Start         string `json:"start"` // RFC3339
	End           string `json:"end"`   // RFC3339
	Summary       string `json:"summary"`
	Description   string `json:"description"`
}

// CreateEventResponse is the workflow's answer. Event is the Google Calendar
// v3 resource VERBATIM — criterion 22 stores it raw, and the echo checks read
// id/start/end from it.
type CreateEventResponse struct {
	SchemaVersion int             `json:"schema_version"`
	Action        string          `json:"action"`
	CalendarID    string          `json:"calendar_id"`
	Status        string          `json:"status"` // "ok" | "error"
	Created       bool            `json:"created"`
	Event         json.RawMessage `json:"event"`
	Error         string          `json:"error"`
}

// CalendarEventID derives the client-chosen Google event id (criterion 6).
// base32hex ([0-9a-v]) because that is Google's id alphabet: events.insert
// rejects anything else, and a rejected id reserves nothing — the whole
// retry-safety argument rests on this id being legal.
func CalendarEventID(deliveryID int64, nonce int64) string {
	return "sb" + strconv.FormatInt(deliveryID, 32) + "t" + strconv.FormatInt(nonce, 32)
}

// CalendarExternalID is THE ONE spelling of a calendar raw item's external id.
// The id the send path writes and the id the next */20 poll writes must be
// byte-identical, or the content_hash short-circuit misses, the poll's
// windowed replacement supersedes our own row, and confirmCalendarDelivery's
// exact-id match never fires — silently, forever.
func CalendarExternalID(eventID string) string { return "calendar:" + eventID }

// CreateEvent performs the one write POST of a booking, retrying exactly once
// on a transport failure, and VERIFIES the echo before returning success
// (criterion 4): nothing downstream re-reads the workflow's answer, and the
// returned resource is stored raw into the busy set as though it were the
// event we asked for.
func (c *PipedreamCalendarClient) CreateEvent(ctx context.Context, req CreateEventRequest) (CreateEventResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return CreateEventResponse{}, fmt.Errorf("marshal pipedream create_event request: %w", err)
	}

	resp, err := c.postOnce(ctx, body)
	if err != nil {
		// Exactly one retry, and only here: c.client.Do failed, so no HTTP
		// response exists and the workflow may never have been reached. The
		// id is ours, so if it WAS reached the replay answers 409 and the
		// workflow returns the event with created:false.
		resp, err = c.postOnce(ctx, body)
	}
	if err != nil {
		// CLASSIFIED, never %w-wrapped: *url.Error embeds the whole endpoint
		// URL and the net errors under it embed the host (pipedream.go:15-19).
		reason := "transport failure"
		if ctx.Err() != nil {
			reason = "cancelled or timed out"
		}
		return CreateEventResponse{}, fmt.Errorf(
			"pipedream calendar workflow unreachable (%s; endpoint withheld — check PIPEDREAM_CALENDAR_URL and the network)", reason)
	}
	defer resp.Body.Close()

	out, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBytes+1))
	if err != nil {
		return CreateEventResponse{}, fmt.Errorf("read pipedream create_event response: body read failed")
	}
	if resp.StatusCode != http.StatusOK {
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return CreateEventResponse{}, fmt.Errorf(
			"pipedream calendar workflow: HTTP %d: %s", resp.StatusCode, snippet)
	}
	if int64(len(out)) >= c.maxBytes {
		return CreateEventResponse{}, fmt.Errorf(
			"pipedream create_event response at or over %d bytes; refusing a possibly truncated answer", c.maxBytes)
	}
	var decoded CreateEventResponse
	if err := json.Unmarshal(out, &decoded); err != nil {
		return CreateEventResponse{}, fmt.Errorf("parse pipedream create_event response: not the envelope")
	}
	if err := verifyCreateEventEcho(req, decoded); err != nil {
		return CreateEventResponse{}, err
	}
	return decoded, nil
}

// postOnce is one attempt: build, send, return the raw response or the
// transport error.
func (c *PipedreamCalendarClient) postOnce(ctx context.Context, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		// Can embed the URL; the constructor validated it, so this is
		// unreachable in practice — classify anyway.
		return nil, fmt.Errorf("build pipedream calendar request")
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.token)
	httpReq.Header.Set("Content-Type", "application/json")
	return c.client.Do(httpReq)
}

// verifyCreateEventEcho refuses, each refusal naming which check failed,
// before CreateEvent may return success (criterion 4). The start/end echo is
// an INSTANT comparison, never a string one (criterion 5): Google re-serializes
// event times into the calendar's own timezone, so a block requested in Z comes
// back in +02:00 for a Europe/Rome calendar — the same instant, different
// bytes, every single time (IK: "Exact text comparison across a provider round
// trip").
func verifyCreateEventEcho(req CreateEventRequest, resp CreateEventResponse) error {
	if resp.SchemaVersion != PipedreamCalendarSchemaVersion {
		return fmt.Errorf("pipedream create_event echo: schema_version %d, want %d",
			resp.SchemaVersion, PipedreamCalendarSchemaVersion)
	}
	if resp.Action != "create_event" {
		return fmt.Errorf("pipedream create_event echo: action %q, want \"create_event\"", resp.Action)
	}
	if resp.Status != "ok" {
		return fmt.Errorf("pipedream create_event: status %q: %s", resp.Status, resp.Error)
	}
	if !strings.EqualFold(resp.CalendarID, req.CalendarID) {
		return fmt.Errorf("pipedream create_event echo: calendar_id %q is not the requested calendar %q",
			resp.CalendarID, req.CalendarID)
	}
	if len(resp.Event) == 0 {
		return fmt.Errorf("pipedream create_event echo: no event resource returned")
	}
	var ev struct {
		ID        string            `json:"id"`
		Start     calTime           `json:"start"`
		End       calTime           `json:"end"`
		Attendees []json.RawMessage `json:"attendees"`
	}
	if err := json.Unmarshal(resp.Event, &ev); err != nil {
		return fmt.Errorf("pipedream create_event echo: event resource does not parse")
	}
	if ev.ID != req.EventID {
		return fmt.Errorf("pipedream create_event echo: event id %q is not the requested event_id %q",
			ev.ID, req.EventID)
	}
	wantStart, err := time.Parse(time.RFC3339, req.Start)
	if err != nil {
		return fmt.Errorf("pipedream create_event: requested start %q is not RFC3339", req.Start)
	}
	wantEnd, err := time.Parse(time.RFC3339, req.End)
	if err != nil {
		return fmt.Errorf("pipedream create_event: requested end %q is not RFC3339", req.End)
	}
	gotStart, err := time.Parse(time.RFC3339, ev.Start.DateTime)
	if err != nil {
		return fmt.Errorf("pipedream create_event echo: event start %q does not parse", ev.Start.DateTime)
	}
	if !gotStart.Equal(wantStart) {
		return fmt.Errorf("pipedream create_event echo: start %q is a different instant than the requested %q",
			ev.Start.DateTime, req.Start)
	}
	gotEnd, err := time.Parse(time.RFC3339, ev.End.DateTime)
	if err != nil {
		return fmt.Errorf("pipedream create_event echo: event end %q does not parse", ev.End.DateTime)
	}
	if !gotEnd.Equal(wantEnd) {
		return fmt.Errorf("pipedream create_event echo: end %q is a different instant than the requested %q",
			ev.End.DateTime, req.End)
	}
	if len(ev.Attendees) > 0 {
		return fmt.Errorf("pipedream create_event echo: the returned event carries attendees; an own block has none")
	}
	return nil
}
