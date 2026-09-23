package tools_test

// send_slack_reply's VALIDATE stage (slack-auto-tier / SWT-77,
// docs/tickets/slack-auto-tier_SPEC.md, acceptance criteria 2 and 3). ZERO
// network, ZERO Postgres: every refusal stops at the executor's validate stage
// (a nil *pgxpool.Pool is never dereferenced — the tools_unit_test.go idiom).
// The ACCEPT half uses a static policy that allows nothing: an accepted call
// reaches policy and comes back "denied by policy", which proves validate
// passed without ever running the handler.
//
// IMPOSED SURFACE (SPEC "New executor tool"):
//
//	send_slack_reply {task_id: int, target_ref: string, text: string}, all required
//
// GREENFIELD NOTE — EXPECTED RED: send_slack_reply is not registered, so every
// call returns `unknown tool "send_slack_reply"`, which each assertion rejects
// explicitly as "not a validation failure".
//
// Criterion 3 is the SWT-61 rule: validate the value that LANDS. The stored
// body and the posted text are google.ScrubAIAttribution(text); text made only
// of attribution lines scrubs to "" and would post an empty message — so it is
// refused by name, and the refusal says why.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

const ssrValidTarget = "https://app.slack.com/client/T0360B84U/DSA806DHA"

func ssrExecutor(checker policy.Checker) *executor.Executor {
	reg := executor.NewRegistry()
	tools.Register(reg, nil)
	if checker == nil {
		checker = policy.NewStatic(reg.Names()...)
	}
	return executor.New(reg, checker, audit.NewMemStore())
}

func ssrArgs(taskID, target, text string) string {
	parts := []string{}
	if taskID != "" {
		parts = append(parts, `"task_id":`+taskID)
	}
	if target != "\x00" {
		parts = append(parts, `"target_ref":`+ssrJSON(target))
	}
	if text != "\x00" {
		parts = append(parts, `"text":`+ssrJSON(text))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// assertSSRValidateRefusal: refused at VALIDATE (never "unknown tool", never a
// policy denial), and the message mentions at least one of wantAnyOf.
func assertSSRValidateRefusal(t *testing.T, args string, wantAnyOf ...string) {
	t.Helper()
	_, err := ssrExecutor(nil).Execute(context.Background(), executor.Call{
		Tool: "send_slack_reply", Actor: "mcp:manual:salvo", Args: []byte(args),
	})
	if err == nil {
		t.Fatalf("send_slack_reply ACCEPTED %s; want a validate refusal", args)
	}
	msg := err.Error()
	if strings.Contains(msg, "unknown tool") {
		t.Fatalf("send_slack_reply is not registered (%v). Criterion 1: {\"send_slack_reply\", "+
			"validateSendSlackReply, sendSlackReplyTool} in tools.Register", err)
	}
	if !strings.Contains(msg, "validate send_slack_reply args") {
		t.Fatalf("refusal of %s did not come from the validate stage: %v", args, err)
	}
	low := strings.ToLower(msg)
	for _, w := range wantAnyOf {
		if strings.Contains(low, strings.ToLower(w)) {
			return
		}
	}
	t.Errorf("validate refusal of %s = %q, want it to name one of %v", args, msg, wantAnyOf)
}

// Criterion 2: task_id required and non-zero.
func TestValidateSendSlackReply_RequiresTaskID(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		assertSSRValidateRefusal(t, ssrArgs("", ssrValidTarget, "on it"), "task_id")
	})
	t.Run("zero", func(t *testing.T) {
		assertSSRValidateRefusal(t, ssrArgs("0", ssrValidTarget, "on it"), "task_id")
	})
	t.Run("empty object", func(t *testing.T) {
		assertSSRValidateRefusal(t, `{}`, "task_id", "target_ref", "text", "missing")
	})
}

// Criterion 2: target_ref required and must parse with slackweb.ParseTargetURL.
func TestValidateSendSlackReply_RefusesBadTargetRef(t *testing.T) {
	for name, target := range map[string]string{
		"missing":             "\x00",
		"empty":               "",
		"not slack":           "https://example.com/client/T0360B84U/DSA806DHA",
		"http":                "http://app.slack.com/client/T0360B84U/DSA806DHA",
		"query string":        ssrValidTarget + "?x=1",
		"fragment":            ssrValidTarget + "#m",
		"no conversation":     "https://app.slack.com/client/T0360B84U",
		"lowercase workspace": "https://app.slack.com/client/t0360b84u/DSA806DHA",
		"bad message id":      ssrValidTarget + "/12345",
	} {
		target := target
		t.Run(name, func(t *testing.T) {
			assertSSRValidateRefusal(t, ssrArgs("7", target, "on it"), "target_ref", "slack target")
		})
	}
}

// Criterion 2: text required, and whitespace is not text.
func TestValidateSendSlackReply_RefusesEmptyText(t *testing.T) {
	for name, text := range map[string]string{
		"missing":    "\x00",
		"empty":      "",
		"whitespace": "  \n\t \n",
	} {
		text := text
		t.Run(name, func(t *testing.T) {
			assertSSRValidateRefusal(t, ssrArgs("7", ssrValidTarget, text), "text")
		})
	}
}

// Criterion 3: text that is empty AFTER google.ScrubAIAttribution is refused
// by name, and the refusal says so — the value that lands is what is
// validated (SWT-61), not the value the caller sent.
func TestValidateSendSlackReply_RefusesTextEmptyAfterScrub(t *testing.T) {
	for name, text := range map[string]string{
		"only a co-authored-by trailer": "Co-Authored-By: Claude <noreply@anthropic.com>",
		"only a generated-with footer":  "🤖 Generated with Claude Code",
		"both, plus blank lines":        "\nCo-Authored-By: Claude <noreply@anthropic.com>\n\n🤖 Generated with Claude Code\n",
	} {
		text := text
		t.Run(name, func(t *testing.T) {
			assertSSRValidateRefusal(t, ssrArgs("7", ssrValidTarget, text), "attribution", "scrub")
		})
	}
}

// The ACCEPT half: these reach policy (and are denied there by an allow-nothing
// static checker), proving validate passed. A trailing-slash target is legal
// (draftDelivery canonicalises it); text with real words plus a trailer is
// legal (the trailer is scrubbed, the words land); and the MCP adapter's
// injected worker_id must not trip the validator.
func TestValidateSendSlackReply_AcceptsValidArgs(t *testing.T) {
	ex := ssrExecutor(policy.NewStatic()) // allows nothing: an accepted call stops at policy
	for name, args := range map[string]string{
		"plain":                  ssrArgs("7", ssrValidTarget, "on it — pushing the fix tonight"),
		"thread url":             ssrArgs("7", ssrValidTarget+"/p1726000000000100", "done"),
		"trailing slash":         ssrArgs("7", ssrValidTarget+"/", "done"),
		"words plus trailer":     ssrArgs("7", ssrValidTarget, "done\nCo-Authored-By: Claude <noreply@anthropic.com>"),
		"mcp-injected worker_id": `{"task_id":7,"target_ref":"` + ssrValidTarget + `","text":"done","worker_id":"manual:salvo"}`,
	} {
		args := args
		t.Run(name, func(t *testing.T) {
			_, err := ex.Execute(context.Background(), executor.Call{
				Tool: "send_slack_reply", Actor: "mcp:manual:salvo", Args: []byte(args),
			})
			if err == nil {
				t.Fatalf("an allow-nothing policy let send_slack_reply through")
			}
			if strings.Contains(err.Error(), "unknown tool") {
				t.Fatalf("send_slack_reply is not registered: %v", err)
			}
			if !strings.Contains(err.Error(), "denied by policy") {
				t.Errorf("valid args %s were refused before policy: %v", args, err)
			}
		})
	}
}

// ssrJSON is a real JSON string encoding. quoteJSON (delivery_upwork_target_test.go)
// escapes only quotes and backslashes, and these tests send newlines.
func ssrJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
