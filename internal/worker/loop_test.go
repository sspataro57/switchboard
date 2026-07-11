package worker

// Unit tests for the wrapper's pure pieces (SPEC 04-mcp-task-tools): the
// claude result envelope parser and the session/feedback extraction from a
// context document. Zero network.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseResult(t *testing.T) {
	out := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"done",
		"session_id":"sess-123","num_turns":4,"total_cost_usd":0.12,"extra":"ignored"}`)
	r, err := ParseResult(out)
	if err != nil {
		t.Fatalf("ParseResult: %v", err)
	}
	if r.SessionID != "sess-123" || r.IsError || r.NumTurns != 4 {
		t.Errorf("parsed %+v, want session sess-123 / no error / 4 turns", r)
	}

	if _, err := ParseResult([]byte(`{"is_error":true}`)); err == nil {
		t.Error("ParseResult without session_id must error")
	}
	if _, err := ParseResult([]byte(`not json`)); err == nil {
		t.Error("ParseResult on garbage must error")
	}
}

func TestLatestSessionIDAndAnswer(t *testing.T) {
	var doc contextDoc
	raw := `{
	  "task": {"id": 7, "title": "t", "status": "needs_feedback"},
	  "project": {"repo_path": "/tmp/x", "slug": "p"},
	  "feedback": [
	    {"id":"1","question":"q1","answer":"old","status":"answered"},
	    {"id":"2","question":"q2","answer":"use B","status":"answered"},
	    {"id":"3","question":"q3","answer":"","status":"open"}
	  ],
	  "events": [
	    {"event_type":"session","payload":"{\"session_id\":\"first\"}"},
	    {"event_type":"log","payload":"{}"},
	    {"event_type":"session","payload":"{\"session_id\":\"latest\"}"}
	  ]
	}`
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("unmarshal doc: %v", err)
	}
	if got := latestSessionID(doc); got != "latest" {
		t.Errorf("latestSessionID = %q, want latest", got)
	}
	if got := latestAnswer(doc); got != "use B" {
		t.Errorf("latestAnswer = %q, want the newest answered text", got)
	}
}

func TestRenderPrompt_ContainsContext(t *testing.T) {
	p := renderPrompt(42, json.RawMessage(`{"task":{"title":"fix the thing"}}`))
	if !strings.Contains(p, "#42") || !strings.Contains(p, "fix the thing") {
		t.Errorf("prompt missing task id or context: %s", p)
	}
}
