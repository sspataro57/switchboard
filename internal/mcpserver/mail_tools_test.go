package mcpserver_test

// Unit tests for the MCP surface added by SWT-11 (SPEC imap-mail-connector,
// acceptance criteria 16 and 17). ZERO network: the adapter is driven with the
// existing fakeExec (adapter_test.go).
//
// Two things become MCP-visible this ticket:
//   1. mail_search / mail_read_thread — new, agent-facing, read-only over
//      normalized_messages (NEVER live IMAP).
//   2. approve_delivery / send_delivery — already registered and already
//      policy.humanOnly; listing them is what makes an interactive session able
//      to approve and send. They remain human-gated: an autonomous worker
//      identity is denied with rule human_only (internal/policy tests).
//
// There is deliberately NO compose-and-send tool: approve and send are two
// separate calls, so a single model turn cannot do both in one step.
//
// GREENFIELD NOTE: agentTools carries none of the four yet, so these fail today
// (and adapter_test.go's exact-allowlist assertion, updated in the same commit,
// fails with them missing) — the expected failure mode.
//
// SWT-42 (mail-attachments) criteria 21 and 22: mail_list_attachments and
// mail_read_attachment join the surface (both profiles). Their schemas carry
// exactly the SPEC's API arguments — never a worker_id, never a path (callers
// pick a part; the handler computes where a file goes, invariant 3) — and
// their descriptions say "ingest", that private mail is never shown, and that
// attachment content is untrusted text written by someone else. mail_search
// and mail_read_thread gain the pointer to mail_list_attachments. EXPECTED RED
// until schemas.go carries the entries and the description edits.

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/mcpserver"
)

func listedTool(t *testing.T, name string) mcpserver.Tool {
	t.Helper()
	srv := mcpserver.New(&fakeExec{}, testWorkerID)
	for _, tl := range srv.ListTools() {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("tool %q is not MCP-listed", name)
	return mcpserver.Tool{}
}

func TestListTools_IncludesReadOnlyMailTools(t *testing.T) {
	for _, name := range []string{"mail_search", "mail_read_thread"} {
		name := name
		t.Run(name, func(t *testing.T) {
			tl := listedTool(t, name)
			if !json.Valid(tl.InputSchema) {
				t.Fatalf("%s InputSchema is not valid JSON: %s", name, tl.InputSchema)
			}
			// Criterion 16: the documented limitation belongs IN the tool
			// description — agents see only what has been ingested.
			if !strings.Contains(strings.ToLower(tl.Description), "ingest") {
				t.Errorf("%s description does not state the ingestion-window limitation: %q", name, tl.Description)
			}
		})
	}

	search := listedTool(t, "mail_search")
	for _, field := range []string{"query", "from", "thread_key", "since", "until", "direction", "limit"} {
		if !strings.Contains(string(search.InputSchema), `"`+field+`"`) {
			t.Errorf("mail_search schema is missing %q: %s", field, search.InputSchema)
		}
	}
	read := listedTool(t, "mail_read_thread")
	for _, field := range []string{"thread_id", "thread_key"} {
		if !strings.Contains(string(read.InputSchema), `"`+field+`"`) {
			t.Errorf("mail_read_thread schema is missing %q: %s", field, read.InputSchema)
		}
	}
}

func TestListTools_IncludesDeliveryApprovalTools(t *testing.T) {
	for _, name := range []string{"approve_delivery", "send_delivery"} {
		name := name
		t.Run(name, func(t *testing.T) {
			tl := listedTool(t, name)
			var schema struct {
				Type       string `json:"type"`
				Properties struct {
					DeliveryID struct {
						Type string `json:"type"`
					} `json:"delivery_id"`
				} `json:"properties"`
				Required []string `json:"required"`
			}
			if err := json.Unmarshal(tl.InputSchema, &schema); err != nil {
				t.Fatalf("%s InputSchema is not a JSON Schema object: %v (%s)", name, err, tl.InputSchema)
			}
			if schema.Properties.DeliveryID.Type != "integer" {
				t.Errorf("%s schema delivery_id type = %q, want integer (criterion 17: {\"delivery_id\": integer})",
					name, schema.Properties.DeliveryID.Type)
			}
			if len(schema.Required) != 1 || schema.Required[0] != "delivery_id" {
				t.Errorf("%s schema required = %v, want [delivery_id]", name, schema.Required)
			}
			// worker_id is never in a schema — identity is injected, not model-chosen.
			if strings.Contains(string(tl.InputSchema), "worker_id") {
				t.Errorf("%s schema mentions worker_id; identity is never model-supplied", name)
			}
		})
	}
}

// There is no compose-and-send verb: nothing on the MCP surface sends without a
// separate prior approve call.
func TestListTools_HasNoComposeAndSendTool(t *testing.T) {
	srv := mcpserver.New(&fakeExec{}, testWorkerID)
	for _, tl := range srv.ListTools() {
		switch tl.Name {
		case "send_mail", "compose_and_send", "draft_and_send", "mail_send":
			t.Errorf("tool %q is MCP-listed; compose-and-send in one call is forbidden", tl.Name)
		}
	}
}

func TestCallTool_MailSearchForwardsWithMCPActor(t *testing.T) {
	fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{"messages":[],"truncated":false}`)}}
	srv := mcpserver.New(fx, testWorkerID)

	if _, err := srv.CallTool(context.Background(), "mail_search", json.RawMessage(`{"query":"staging login"}`)); err != nil {
		t.Fatalf("CallTool(mail_search): %v", err)
	}
	if !fx.called {
		t.Fatal("mail_search never reached the executor (invariant 3)")
	}
	if want := "mcp:" + testWorkerID; fx.lastCall.Actor != want {
		t.Errorf("forwarded Actor = %q, want %q", fx.lastCall.Actor, want)
	}
}

// ---- SWT-42 (mail-attachments) ---------------------------------------------------

func TestListTools_IncludesTheAttachmentTools(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields []string // the SPEC's API section, exactly
	}{
		{"mail_list_attachments", []string{"raw_source_item_id", "message_id", "thread_id", "thread_key",
			"from", "subject", "since", "until", "limit"}},
		{"mail_read_attachment", []string{"raw_source_item_id", "message_id", "index", "filename", "part_id",
			"offset", "to_file"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tl := listedTool(t, tc.name)
			var s struct {
				Type       string                     `json:"type"`
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err := json.Unmarshal(tl.InputSchema, &s); err != nil || s.Type != "object" {
				t.Fatalf("%s InputSchema is not a JSON Schema object: %v (%s)", tc.name, err, tl.InputSchema)
			}
			want := map[string]bool{}
			for _, f := range tc.fields {
				want[f] = true
				if _, ok := s.Properties[f]; !ok {
					t.Errorf("%s schema is missing %q: %s", tc.name, f, tl.InputSchema)
				}
			}
			for f := range s.Properties {
				if !want[f] {
					t.Errorf("%s schema declares %q, which the SPEC's API does not (worker_id is injected; a caller "+
						"never supplies a path)", tc.name, f)
				}
			}

			d := strings.ToLower(tl.Description)
			for _, w := range []struct{ re, why string }{
				{`ingest`, "the SWT-11 criterion 16 pattern: served from what ingestion stored, not a live mailbox"},
				{`(?s)private.{0,80}never|never.{0,80}private`, "private mail is never shown (criteria 13-16)"},
				{`untrusted`, "attachment content is untrusted text…"},
				{`someone else|written by`, "…written by someone else"},
			} {
				if !regexp.MustCompile(w.re).MatchString(d) {
					t.Errorf("%s description does not match /%s/ — %s. Description: %q", tc.name, w.re, w.why, tl.Description)
				}
			}
		})
	}
}

// Criterion 21: the body reads point at the attachment list, and keep "ingest".
func TestMailBodyReads_PointAtTheAttachmentList(t *testing.T) {
	for _, name := range []string{"mail_search", "mail_read_thread"} {
		d := listedTool(t, name).Description
		if !strings.Contains(d, "Attachments are not in the body; list them with mail_list_attachments") {
			t.Errorf("%s description lacks \"Attachments are not in the body; list them with mail_list_attachments\" "+
				"— without it a session concludes attachments are not stored (SPEC fact 3): %q", name, d)
		}
		if !strings.Contains(strings.ToLower(d), "ingest") {
			t.Errorf("%s description no longer states the ingestion-window limitation: %q", name, d)
		}
	}
}

// Criteria 16 and 22: from the user profile both tools forward as
// mcp:manual:salvo with only worker_id added — no profile pin, because the gate
// is in the handler and applies to everyone (SPEC "Executor hook").
func TestCallTool_AttachmentToolsForwardFromTheUserProfile(t *testing.T) {
	for _, tc := range []struct{ tool, args, keys string }{
		{"mail_list_attachments", `{"from":"sana","subject":"Activities Integration"}`, "from,subject,worker_id"},
		{"mail_read_attachment", `{"raw_source_item_id":77761,"filename":"Request.json"}`, "filename,raw_source_item_id,worker_id"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{}`)}}
			srv := mcpserver.NewWithProfile(fx, "manual:salvo", mcpserver.ProfileUser)
			if _, err := srv.CallTool(context.Background(), tc.tool, json.RawMessage(tc.args)); err != nil {
				t.Fatalf("user profile refused %s: %v (SWT-42 O1: both profiles serve it)", tc.tool, err)
			}
			if !fx.called || fx.lastCall.Tool != tc.tool || fx.lastCall.Actor != "mcp:manual:salvo" {
				t.Fatalf("forwarded %+v, want %s as mcp:manual:salvo", fx.lastCall, tc.tool)
			}
			if got := keyList(forwardedKeys(t, fx.lastCall.Args)); got != tc.keys {
				t.Errorf("forwarded keys = %s, want %s — worker_id and nothing else (no pin on these tools)", got, tc.keys)
			}
		})
	}
}
