package google

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func batchEvent(id, start, end string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"id": id, "start": map[string]string{"dateTime": start}, "end": map[string]string{"dateTime": end}})
	return raw
}

// SWT-109 criterion 21: create_events carries no attendees, every ok entry is
// echo-verified, and every requested id must be answered exactly once.
func TestCreateEvents_EnvelopeAndEcho(t *testing.T) {
	var body []byte
	answer := func(w http.ResponseWriter, results []CreateEventsResult) {
		_ = json.NewEncoder(w).Encode(CreateEventsResponse{SchemaVersion: PipedreamCalendarSchemaVersion, Action: "create_events", Results: results})
	}
	var mode atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		switch mode.Load() {
		case 0:
			answer(w, []CreateEventsResult{
				{CalendarID: "a@x", EventID: "sb1t1", Status: "ok", Created: true, Event: batchEvent("sb1t1", "2026-10-27T15:00:00-04:00", "2026-10-27T16:00:00-04:00")},
				{CalendarID: "a@x", EventID: "sb2t1", Status: "error", Error: "quota"},
			})
		case 1: // one id missing
			answer(w, []CreateEventsResult{{CalendarID: "a@x", EventID: "sb1t1", Status: "ok", Event: batchEvent("sb1t1", "2026-10-27T19:00:00Z", "2026-10-27T20:00:00Z")}})
		case 2: // ok entry whose instant differs
			answer(w, []CreateEventsResult{
				{CalendarID: "a@x", EventID: "sb1t1", Status: "ok", Event: batchEvent("sb1t1", "2026-10-27T18:00:00Z", "2026-10-27T20:00:00Z")},
				{CalendarID: "a@x", EventID: "sb2t1", Status: "error"},
			})
		}
	}))
	defer srv.Close()
	c := writeClient(t, srv)
	req := CreateEventsRequest{SchemaVersion: PipedreamCalendarSchemaVersion, Action: "create_events", Events: []CalendarEventSpec{
		{CalendarID: "a@x", EventID: "sb1t1", Start: "2026-10-27T19:00:00Z", End: "2026-10-27T20:00:00Z", Summary: "Busy", Description: "Reserved."},
		{CalendarID: "a@x", EventID: "sb2t1", Start: "2026-10-28T19:00:00Z", End: "2026-10-28T20:00:00Z", Summary: "Hold", Description: "Reserved."},
	}}
	resp, err := c.CreateEvents(context.Background(), req)
	if err != nil {
		t.Fatalf("a valid mixed answer: %v", err)
	}
	if len(resp.Results) != 2 || resp.Results[1].Status != "error" {
		t.Errorf("results = %+v", resp.Results)
	}
	if strings.Contains(string(body), "attendees") {
		t.Errorf("the request carries an attendees field: %s", body)
	}
	mode.Store(1)
	if _, err := c.CreateEvents(context.Background(), req); err == nil {
		t.Errorf("an answer missing a requested id was accepted")
	}
	mode.Store(2)
	if _, err := c.CreateEvents(context.Background(), req); err == nil {
		t.Errorf("an ok entry at a different instant was accepted")
	}
}

// SWT-109 criterion 23: delete_events refuses any id switchboard did not mint
// BEFORE the network call, and maps the workflow's answers.
func TestDeleteEvents_RefusesForeignIDsBeforeTheCall(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var req DeleteEventsRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		out := DeleteEventsResponse{SchemaVersion: PipedreamCalendarSchemaVersion, Action: "delete_events"}
		for i, e := range req.Events {
			st := "deleted"
			if i == 1 {
				st = "gone"
			}
			out.Results = append(out.Results, DeleteEventsResult{CalendarID: e.CalendarID, EventID: e.EventID, Status: st})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	c := writeClient(t, srv)
	for _, bad := range []string{"abc123", "sb1t1x", "", "SB1T1", "7kq2m0standup"} {
		_, err := c.DeleteEvents(context.Background(), DeleteEventsRequest{SchemaVersion: 1, Action: "delete_events",
			Events: []CalendarDeleteSpec{{CalendarID: "a@x", EventID: "sb1t1"}, {CalendarID: "a@x", EventID: bad}}})
		if err == nil {
			t.Errorf("foreign id %q was not refused", bad)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("%d request(s) reached the workflow with a foreign id in the batch", hits.Load())
	}
	resp, err := c.DeleteEvents(context.Background(), DeleteEventsRequest{SchemaVersion: 1, Action: "delete_events",
		Events: []CalendarDeleteSpec{{CalendarID: "a@x", EventID: "sb1t1"}, {CalendarID: "a@x", EventID: "sb2tv9"}}})
	if err != nil || len(resp.Results) != 2 || resp.Results[1].Status != "gone" {
		t.Errorf("valid delete = %+v, %v", resp, err)
	}
}
