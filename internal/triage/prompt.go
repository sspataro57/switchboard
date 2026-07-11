package triage

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PromptVersion is recorded in every ai_runs.input so a prompt change can
// drive a re-extraction sweep later.
const PromptVersion = "triage-v1"

// SchemaName is the strict json_schema name sent to the provider.
const SchemaName = "triage_extraction"

// SystemPrompt is the ported CRM triage craft (rubric structure, decisive
// register, OUTPUT CONTRACT section) applied to established-client messages.
// Edit the prompt here without touching wiring.
const SystemPrompt = `You are a triage assistant for a solo contract-engineering business.
The input is ONE inbound message from an EXISTING client thread, with recent
thread context and a list of currently-open tasks for that client's project.
Decide whether the message is actionable and emit a structured extraction.
Be decisive. Err toward NOT actionable for pleasantries and noise.

ACTIONABLE signals:
- Direct requests for work: bugs to fix, features to build, changes to make.
- Questions that need an answer from us.
- Scheduling asks (calls, meetings, deadlines proposed).
- Deadline or scope changes on work in flight.
- Approvals or answers that unblock work we are waiting on.

NOT ACTIONABLE:
- Acknowledgements, thanks, social pleasantries.
- FYI notes with no ask.
- Our own commitments echoed back by the client.
- Automated notifications with no decision or work attached.

ATTACH vs CREATE:
- ATTACH when the message is progress, a reply, or additional detail on one
  of the offered candidate tasks: set attach_to_task_id to that candidate's id.
- CREATE when it is a distinct new ask: leave attach_to_task_id null.
- When torn, prefer ATTACH — a duplicate task costs more than a mis-attached
  log line.
- ONLY ids from the offered candidate list are valid. No candidates offered
  means attach_to_task_id MUST be null.

Confidence discipline:
- Every field carries its own confidence 0..1 reflecting THAT field, not
  overall vibes. Be decisive — 0.5 everywhere is a wasted verdict.

OUTPUT CONTRACT:
- actionable: {value: boolean, confidence}
- kind: {value: one of action_request | question | scheduling | status_update | fyi, confidence}
- title: {value: string, confidence} — imperative, <= 80 chars, terse register
  (e.g. "Fix staging login redirect"), even when not actionable (summarize).
- body: {value: string, confidence} — what a worker needs to act, self-contained.
- priority: {value: integer 0..3, confidence} — 0 normal, 1 elevated, 2 high,
  3 urgent (explicit deadline pressure or blocking-the-client).
- attach_to_task_id: {value: candidate id or null, confidence}
- summary: one line, decisive rationale for the verdict.`

// ExtractionSchema is the strict JSON Schema for the model output. Every
// property required, additionalProperties false at every level.
var ExtractionSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["actionable", "kind", "title", "body", "priority", "attach_to_task_id", "summary"],
  "properties": {
    "actionable": {
      "type": "object", "additionalProperties": false, "required": ["value", "confidence"],
      "properties": {"value": {"type": "boolean"}, "confidence": {"type": "number"}}
    },
    "kind": {
      "type": "object", "additionalProperties": false, "required": ["value", "confidence"],
      "properties": {"value": {"type": "string", "enum": ["action_request", "question", "scheduling", "status_update", "fyi"]}, "confidence": {"type": "number"}}
    },
    "title": {
      "type": "object", "additionalProperties": false, "required": ["value", "confidence"],
      "properties": {"value": {"type": "string"}, "confidence": {"type": "number"}}
    },
    "body": {
      "type": "object", "additionalProperties": false, "required": ["value", "confidence"],
      "properties": {"value": {"type": "string"}, "confidence": {"type": "number"}}
    },
    "priority": {
      "type": "object", "additionalProperties": false, "required": ["value", "confidence"],
      "properties": {"value": {"type": "integer"}, "confidence": {"type": "number"}}
    },
    "attach_to_task_id": {
      "type": "object", "additionalProperties": false, "required": ["value", "confidence"],
      "properties": {"value": {"type": ["integer", "null"]}, "confidence": {"type": "number"}}
    },
    "summary": {"type": "string"}
  }
}`)

// renderUser builds the user message: the triaged message, thread context
// (both directions — outbound is legitimate history), person, and the
// candidate tasks as data.
func renderUser(mc MessageContext) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Client / person: %s\n", orUnknown(mc.PersonName))
	if mc.ProjectSlug != "" {
		fmt.Fprintf(&b, "Project: %s\n", mc.ProjectSlug)
	} else {
		b.WriteString("Project: UNMAPPED (no project is linked to this person)\n")
	}

	b.WriteString("\nOpen candidate tasks (attach_to_task_id must be one of these ids, or null):\n")
	if len(mc.Candidates) == 0 {
		b.WriteString("  (none — attach_to_task_id MUST be null)\n")
	}
	for _, c := range mc.Candidates {
		fmt.Fprintf(&b, "  - id %d [%s] %s\n", c.ID, c.Status, c.Title)
	}

	if len(mc.Thread) > 0 {
		b.WriteString("\nRecent thread context (oldest first):\n")
		for _, t := range mc.Thread {
			fmt.Fprintf(&b, "  [%s %s] %s: %s\n",
				t.SentAt.Format("2006-01-02"), t.Direction, t.Sender, truncate(t.BodyText, 400))
		}
	}

	m := mc.Message
	fmt.Fprintf(&b, "\nMESSAGE TO TRIAGE (inbound, %s, sent %s):\nFrom: %s\nSubject: %s\n---\n%s\n---\n",
		m.Channel, m.SentAt.Format("2006-01-02 15:04"), m.Sender, m.Subject, m.BodyText)
	return b.String()
}

func orUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
