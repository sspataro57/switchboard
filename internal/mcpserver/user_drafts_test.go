package mcpserver_test

// SWT-44 review fixes (user-profile-drafts), zero network: the two new user
// profile pins (the SWT-38 C4 pattern: force-set by OVERWRITE after
// injectWorkerID, hidden from every schema, only narrowing) and the
// validate-stage refusal of a non-gmail draft from the user profile.
//
//	draft_delivery:  {"require_channel": "gmail"}     — sessions outside the
//	                 switchboard repo draft email replies only
//	update_delivery: {"require_own_draft": "true"}    — a session edits only
//	                 drafts its own actor created
//
// MUTATIONS THAT MUST TURN THIS FILE RED:
//   - drop either pin from userProfilePins → its "gains the pin" row.
//   - merge instead of overwrite → the "model-supplied … is overwritten" rows.
//   - pins on the full profile → the full-profile rows.
//   - expose require_channel / require_own_draft / expect_content_hash in a
//     schema → TestNoSchemaExposesTheSWT44Args.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/mcpserver"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

func TestUserProfile_PinsGmailDraftsAndOwnDraftEdits(t *testing.T) {
	user := func() (*mcpserver.Server, *fakeExec) {
		fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{"delivery_id":9}`)}}
		return mcpserver.NewWithProfile(fx, "manual:salvo", mcpserver.ProfileUser), fx
	}

	t.Run("user/draft_delivery gains require_channel gmail", func(t *testing.T) {
		srv, fx := user()
		args := forwardCall(t, srv, fx, "draft_delivery", `{"task_id":4,"channel":"gmail","body":"b","thread_id":5}`)
		if got := keyList(args); got != "body,channel,require_channel,task_id,thread_id,worker_id" {
			t.Errorf("forwarded keys = %s, want body,channel,require_channel,task_id,thread_id,worker_id", got)
		}
		if string(args["require_channel"]) != `"gmail"` {
			t.Errorf("forwarded require_channel = %s, want \"gmail\"", args["require_channel"])
		}
	})
	t.Run("user/draft_delivery model-supplied require_channel is overwritten", func(t *testing.T) {
		srv, fx := user()
		args := forwardCall(t, srv, fx, "draft_delivery",
			`{"task_id":4,"channel":"slack_reply","body":"b","target_ref":"x","require_channel":"slack_reply"}`)
		if string(args["require_channel"]) != `"gmail"` {
			t.Errorf("forwarded require_channel = %s, want \"gmail\": the pin OVERWRITES", args["require_channel"])
		}
		if string(args["channel"]) != `"slack_reply"` {
			t.Errorf("forwarded channel = %s, want \"slack_reply\" untouched: the validator refuses, the adapter never rewrites",
				args["channel"])
		}
	})
	t.Run("user/update_delivery gains require_own_draft", func(t *testing.T) {
		srv, fx := user()
		args := forwardCall(t, srv, fx, "update_delivery", `{"delivery_id":9,"body":"fixed"}`)
		if got := keyList(args); got != "body,delivery_id,require_own_draft,worker_id" {
			t.Errorf("forwarded keys = %s, want body,delivery_id,require_own_draft,worker_id", got)
		}
		if string(args["require_own_draft"]) != `"true"` {
			t.Errorf("forwarded require_own_draft = %s, want \"true\"", args["require_own_draft"])
		}
	})
	t.Run("user/update_delivery model-supplied false is overwritten", func(t *testing.T) {
		for _, supplied := range []string{`"false"`, `false`, `""`} {
			srv, fx := user()
			args := forwardCall(t, srv, fx, "update_delivery", `{"delivery_id":9,"body":"x","require_own_draft":`+supplied+`}`)
			if string(args["require_own_draft"]) != `"true"` {
				t.Errorf("caller sent require_own_draft=%s; forwarded %s, want \"true\" (OVERWRITE)", supplied, args["require_own_draft"])
			}
		}
	})

	full := func() (*mcpserver.Server, *fakeExec) {
		fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{}`)}}
		return mcpserver.New(fx, "manual:salvo"), fx
	}
	t.Run("full/draft_delivery is not pinned", func(t *testing.T) {
		srv, fx := full()
		args := forwardCall(t, srv, fx, "draft_delivery", `{"task_id":4,"channel":"slack_reply","body":"b","target_ref":"x"}`)
		if got := keyList(args); got != "body,channel,target_ref,task_id,worker_id" {
			t.Errorf("full-profile draft_delivery forwarded keys = %s, want no pin", got)
		}
	})
	t.Run("full/update_delivery is not pinned", func(t *testing.T) {
		srv, fx := full()
		args := forwardCall(t, srv, fx, "update_delivery", `{"delivery_id":9,"body":"b"}`)
		if got := keyList(args); got != "body,delivery_id,worker_id" {
			t.Errorf("full-profile update_delivery forwarded keys = %s, want no pin", got)
		}
	})
}

// Hidden, like require_assignee_type and expect_task_status: each only
// narrows, so a model has no reason to see it.
func TestNoSchemaExposesTheSWT44Args(t *testing.T) {
	listed := 0
	for _, tl := range mcpserver.New(&fakeExec{}, testWorkerID).ListTools() {
		listed++
		for _, hidden := range []string{"require_channel", "require_own_draft", "expect_content_hash"} {
			if strings.Contains(string(tl.InputSchema), hidden) {
				t.Errorf("%s's schema exposes %s; it is set by the adapter or the dashboard, never advertised. Schema: %s",
					tl.Name, hidden, tl.InputSchema)
			}
		}
	}
	if listed < 20 {
		t.Fatalf("POSITIVE CONTROL FAILED: the full profile lists only %d tools", listed)
	}
}

// denyAllExecutor is the real registry and validators (nil pool) behind a
// static policy that allows NOTHING: validate runs before policy, so a
// "denied by policy" error proves the call PASSED validation, and no handler
// ever runs (none could, on a nil pool).
func denyAllExecutor() *executor.Executor {
	reg := executor.NewRegistry()
	tools.Register(reg, nil)
	return executor.New(reg, policy.NewStatic(), audit.NewMemStore())
}

// SWT-44 fix 3: through the user profile, a calendar or slack_reply draft is
// refused at VALIDATE; the same call through the full profile passes it.
func TestUserProfile_RefusesANonGmailDraft(t *testing.T) {
	ex := denyAllExecutor()
	userSrv := mcpserver.NewWithProfile(ex, "manual:salvo", mcpserver.ProfileUser)
	fullSrv := mcpserver.New(ex, "manual:salvo")

	for _, tc := range []struct{ name, args string }{
		{"slack_reply", `{"task_id":4,"channel":"slack_reply","body":"b",` +
			`"target_ref":"https://app.slack.com/client/TITEST/CITEST/p1750000000000000"}`},
		{"calendar", `{"task_id":4,"channel":"calendar","body":"b","subject":"focus","target_ref":"a@example.com",` +
			`"start":"2030-01-01T10:00:00Z","end":"2030-01-01T10:15:00Z"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := userSrv.CallTool(context.Background(), "draft_delivery", json.RawMessage(tc.args))
			if err == nil || !strings.Contains(err.Error(), "validate") || !strings.Contains(err.Error(), "gmail") {
				t.Errorf("user-profile %s draft = %v, want a validate refusal naming gmail", tc.name, err)
			}
			_, err = fullSrv.CallTool(context.Background(), "draft_delivery", json.RawMessage(tc.args))
			if err == nil || !strings.Contains(err.Error(), "denied by policy") {
				t.Errorf("POSITIVE CONTROL: full-profile %s draft = %v, want it past validate (the deny-all policy)", tc.name, err)
			}
		})
	}

	// And a gmail draft passes the user profile's validation.
	_, err := userSrv.CallTool(context.Background(), "draft_delivery",
		json.RawMessage(`{"task_id":4,"channel":"gmail","body":"b","thread_id":5}`))
	if err == nil || !strings.Contains(err.Error(), "denied by policy") {
		t.Errorf("user-profile gmail draft = %v, want it past validate (the deny-all policy)", err)
	}
}
