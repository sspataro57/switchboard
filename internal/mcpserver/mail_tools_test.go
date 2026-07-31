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

import (
	"context"
	"encoding/json"
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
