package mcpserver_test

// gmail-delivery-cc (SWT-69) criteria 17 and 18, plus D3's validate-stage
// refusal over MCP. ZERO network, ZERO Postgres.
//
//   - draft_delivery and update_delivery each gain a `cc` property, an ARRAY OF
//     STRINGS, optional, whose description names: gmail only, one address per
//     entry, display names dropped, max 10 — and, for update_delivery, that []
//     clears it.
//   - both tools are already on userProfileTools, so the property reaches the
//     USER profile with no profile change. That is the install a session in the
//     collaboratory repo runs, and it is the whole point of the ticket, so it is
//     asserted on the schema the USER profile actually lists — not on the full
//     profile's.
//   - the adapter forwards the caller's cc UNALTERED (it rewrites nothing; the
//     validator refuses).
//   - through the user profile a malformed cc is refused at VALIDATE, and a cc
//     on a non-gmail draft is refused by name through the FULL profile too (the
//     user profile's own channel pin would otherwise mask the cc rule).
//
// Reuses fakeExec/forwardCall/keyList/denyAllExecutor from this package's other
// test files.
//
// EXPECTED RED: the schemas carry no cc property, and the validator does not
// know the word.
//
// MUTATIONS THAT MUST TURN THIS FILE RED: leave either schema alone; type cc as
// a string ("a, b") instead of an array; mark it required; drop the gmail-only
// or syntax refusal from validateDraftDelivery / validateUpdateDelivery.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/mcpserver"
)

// ccSchemaOf returns the parsed InputSchema of one tool as the given profile
// lists it.
func ccSchemaOf(t *testing.T, srv *mcpserver.Server, tool string) map[string]any {
	t.Helper()
	for _, tl := range srv.ListTools() {
		if tl.Name != tool {
			continue
		}
		var s map[string]any
		if err := json.Unmarshal(tl.InputSchema, &s); err != nil {
			t.Fatalf("%s schema is not JSON: %v", tool, err)
		}
		return s
	}
	t.Fatalf("this profile does not list %s", tool)
	return nil
}

func ccProperty(t *testing.T, schema map[string]any, tool, profile string) map[string]any {
	t.Helper()
	props, _ := schema["properties"].(map[string]any)
	cc, ok := props["cc"].(map[string]any)
	if !ok {
		t.Fatalf("%s's schema on the %s profile has no `cc` property: %v\n\nCriterion 17: without it no session "+
			"can pass a Cc, which is the entire ticket.", tool, profile, props)
	}
	return cc
}

func TestMCPSchemas_DraftAndUpdateCarryCcAsAnArrayOfStrings(t *testing.T) {
	for _, prof := range []struct {
		name string
		srv  *mcpserver.Server
	}{
		// Criterion 17: the USER profile is the one that matters (a session
		// outside this repo); the full profile lists the same schema.
		{"user", mcpserver.NewWithProfile(&fakeExec{}, "manual:salvo", mcpserver.ProfileUser)},
		{"full", mcpserver.New(&fakeExec{}, "manual:salvo")},
	} {
		for _, tool := range []string{"draft_delivery", "update_delivery"} {
			t.Run(prof.name+"/"+tool, func(t *testing.T) {
				schema := ccSchemaOf(t, prof.srv, tool)
				cc := ccProperty(t, schema, tool, prof.name)

				if cc["type"] != "array" {
					t.Errorf("%s.cc type = %v, want \"array\": one address per entry, so nothing has to guess "+
						"how a caller's separator works", tool, cc["type"])
				}
				items, _ := cc["items"].(map[string]any)
				if items == nil || items["type"] != "string" {
					t.Errorf("%s.cc items = %v, want {\"type\":\"string\"}", tool, cc["items"])
				}
				// Optional, always: a delivery with no Cc is the normal case.
				if req, ok := schema["required"].([]any); ok {
					for _, r := range req {
						if r == "cc" {
							t.Errorf("%s marks cc REQUIRED; it is optional", tool)
						}
					}
				}
				desc, _ := cc["description"].(string)
				lower := strings.ToLower(desc)
				for _, want := range []string{"gmail", "10"} {
					if !strings.Contains(lower, want) {
						t.Errorf("%s.cc description = %q; it must name %q (criterion 17)", tool, desc, want)
					}
				}
				if !strings.Contains(lower, "display name") && !strings.Contains(lower, "display-name") {
					t.Errorf("%s.cc description = %q; it must say a display name is DROPPED — otherwise a "+
						"session passes \"Katie <k@x>\" and silently loses the words it chose", tool, desc)
				}
				if tool == "update_delivery" && !strings.Contains(desc, "[]") {
					t.Errorf("update_delivery.cc description = %q; it must say that [] CLEARS the list "+
						"(absent leaves it unchanged — the caller cannot guess that)", desc)
				}
			})
		}
	}
}

// The adapter never rewrites a caller's value (the SWT-44 rule: the validator
// refuses, the adapter forwards the truth). The pins are unchanged around it.
func TestMCPAdapter_ForwardsCcUnaltered(t *testing.T) {
	newUser := func() (*mcpserver.Server, *fakeExec) {
		fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{"delivery_id":9}`)}}
		return mcpserver.NewWithProfile(fx, "manual:salvo", mcpserver.ProfileUser), fx
	}

	srv, fx := newUser()
	args := forwardCall(t, srv, fx, "draft_delivery",
		`{"task_id":4,"channel":"gmail","body":"b","thread_id":5,"cc":["Katie <kevans@cecollaboratory.com>","b@x.io"]}`)
	// Decoded, not compared as bytes: the adapter re-marshals and Go escapes
	// < and > by default, which is a JSON detail and not a rewrite.
	var cc []string
	if err := json.Unmarshal(args["cc"], &cc); err != nil {
		t.Fatalf("forwarded cc %s is not a JSON array of strings: %v", args["cc"], err)
	}
	want := []string{"Katie <kevans@cecollaboratory.com>", "b@x.io"}
	if len(cc) != len(want) || cc[0] != want[0] || cc[1] != want[1] {
		t.Errorf("forwarded cc = %q, want %q unaltered (normalization is the executor's, so the refusal can "+
			"name what the caller actually sent)", cc, want)
	}
	const wantKeys = "body,cc,channel,require_channel,require_thread_in_task_project,task_id,thread_id,worker_id"
	if got := keyList(args); got != wantKeys {
		t.Errorf("forwarded keys = %s, want %s (criterion 18: neither profile pin changes)", got, wantKeys)
	}

	srv, fx = newUser()
	uargs := forwardCall(t, srv, fx, "update_delivery", `{"delivery_id":9,"cc":[]}`)
	if got := string(uargs["cc"]); got != `[]` {
		t.Errorf("forwarded cc = %s, want [] (the clear verb must survive the adapter)", got)
	}
	if got := keyList(uargs); got != "cc,delivery_id,require_channel,require_own_draft,worker_id" {
		t.Errorf("forwarded keys = %s, want cc,delivery_id,require_channel,require_own_draft,worker_id", got)
	}
}

// D3/criterion 4 at the VALIDATE stage, over the real registry and validators
// behind a deny-all policy: a refusal that mentions "denied by policy" proves
// the call PASSED validation (denyAllExecutor, user_drafts_test.go).
func TestMCPValidate_RefusesABadCcAndACcOnAnotherChannel(t *testing.T) {
	ex := denyAllExecutor()
	user := mcpserver.NewWithProfile(ex, "manual:salvo", mcpserver.ProfileUser)
	full := mcpserver.New(ex, "manual:salvo")

	// A cc on a non-gmail draft, through the FULL profile (the user profile's
	// channel pin would refuse it for the wrong reason).
	_, err := full.CallTool(context.Background(), "draft_delivery",
		json.RawMessage(`{"task_id":4,"channel":"slack_reply","body":"b",`+
			`"target_ref":"https://app.slack.com/client/TITEST/CITEST/p1750000000000000","cc":["k@example.com"]}`))
	if err == nil || !strings.Contains(err.Error(), "cc") {
		t.Errorf("full-profile slack_reply draft with a cc = %v, want a validate refusal naming cc", err)
	}

	// A malformed cc on a gmail draft, through the USER profile.
	for _, bad := range []string{`["not an address"]`, `[""]`, `["josé@example.com"]`, `["a@x.io\r\nBcc: v@x.io"]`} {
		_, err := user.CallTool(context.Background(), "draft_delivery",
			json.RawMessage(`{"task_id":4,"channel":"gmail","body":"b","thread_id":5,"cc":`+bad+`}`))
		if err == nil || strings.Contains(err.Error(), "denied by policy") {
			t.Errorf("user-profile gmail draft with cc %s = %v, want a VALIDATE refusal (it reached policy, so "+
				"the validator accepted it)", bad, err)
		}
	}
	// POSITIVE CONTROL: a good cc passes validation and dies at the deny-all policy.
	_, err = user.CallTool(context.Background(), "draft_delivery",
		json.RawMessage(`{"task_id":4,"channel":"gmail","body":"b","thread_id":5,"cc":["kevans@cecollaboratory.com"]}`))
	if err == nil || !strings.Contains(err.Error(), "denied by policy") {
		t.Errorf("user-profile gmail draft with a valid cc = %v, want it past validate (the deny-all policy)", err)
	}
	// ...and on update_delivery, where cc alone is now enough.
	_, err = user.CallTool(context.Background(), "update_delivery", json.RawMessage(`{"delivery_id":9,"cc":[]}`))
	if err == nil || !strings.Contains(err.Error(), "denied by policy") {
		t.Errorf("update_delivery with cc alone = %v, want it past validate: \"nothing to update\" must accept "+
			"subject OR body OR cc (criterion 7)", err)
	}
}
