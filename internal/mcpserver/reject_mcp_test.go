package mcpserver_test

// mcp-reject-delivery (docs/tickets/mcp-reject-delivery_SPEC.md, swb 4565;
// Salvador 2026-11-22: "we need to allow claude to discharge stale comms on
// swb"): reject_delivery, DISCARD form only {delivery_id, note?}, on ops-mcp
// (full) and ops-mcp-user (user). It amends SWT-43 D9, which kept the verb off
// MCP. ZERO network, ZERO Postgres: the adapter is driven with fakeExec and the
// production policy matrix with a loader that must not run.
//
// MUTATIONS THAT MUST TURN THIS FILE RED:
//   - drop the CallTool guard call → RedraftRefused, GuardFailsClosed;
//   - add "redraft" to the schema → Schema;
//   - drop it from userProfileTools → ListedOnFullAndUserProfiles;
//   - drop reject_delivery from policy.humanOnly → WorkerDenied;
//   - drop the instructions line or the exception → Instructions.

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/mcpserver"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

const rejectName = "reject_delivery"

// Acceptance 1: full 38, user 27 (user-mail-read: +mail_read_thread), read 3; listed on full and user with the
// same entry byte for byte.
func TestRejectDelivery_ListedOnFullAndUserProfiles(t *testing.T) {
	byProfile := map[mcpserver.Profile]map[string]mcpserver.Tool{}
	for _, p := range []mcpserver.Profile{mcpserver.ProfileFull, mcpserver.ProfileUser, mcpserver.ProfileRead} {
		byProfile[p] = toolMap(mcpserver.NewWithProfile(&fakeExec{}, testWorkerID, p).ListTools())
	}
	for _, c := range []struct {
		name    string
		profile mcpserver.Profile
		listed  bool
		count   int
	}{
		{"full", mcpserver.ProfileFull, true, 38},
		{"user", mcpserver.ProfileUser, true, 27}, // +mail_read_thread (user-mail-read)
		{"read", mcpserver.ProfileRead, false, 3},
	} {
		got := byProfile[c.profile]
		if _, ok := got[rejectName]; ok != c.listed {
			t.Errorf("the %s profile lists %s = %v, want %v", c.name, rejectName, ok, c.listed)
		}
		if len(got) != c.count {
			t.Errorf("the %s profile lists %d tools, want %d", c.name, len(got), c.count)
		}
	}
	f, fok := byProfile[mcpserver.ProfileFull][rejectName]
	u, uok := byProfile[mcpserver.ProfileUser][rejectName]
	if fok && uok && (f.Description != u.Description || string(f.InputSchema) != string(u.InputSchema)) {
		t.Errorf("the user profile's %s entry differs from the full profile's: it must be a slice of agentTools", rejectName)
	}
}

// Acceptance 2: discard form only.
func TestRejectDelivery_Schema(t *testing.T) {
	tl := listedTool(t, rejectName)
	s := parseVerbSchema(t, rejectName, tl.InputSchema)
	assertVerbProps(t, rejectName, s, map[string]string{"delivery_id": "integer", "note": "string"}, []string{"delivery_id"})
	if len(s.Properties) != 2 {
		t.Errorf("%s schema has %d properties, want exactly delivery_id and note: %s", rejectName, len(s.Properties), tl.InputSchema)
	}
	raw := string(tl.InputSchema)
	for _, forbidden := range []string{"redraft", "worker_id", "expect_content_hash"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("%s's schema mentions %q (D3: discard only; worker_id is injected)", rejectName, forbidden)
		}
	}
	d := strings.ToLower(tl.Description)
	for _, want := range []string{"sends nothing", "human", "cannot be undone", "sending or sent"} {
		if !strings.Contains(d, want) {
			t.Errorf("%s description does not say %q: %s", rejectName, want, tl.Description)
		}
	}
}

// Acceptance 3: redraft is refused before the executor on both profiles; the
// plain discard forwards with the MCP actor.
func TestRejectDelivery_RedraftRefused(t *testing.T) {
	for _, p := range []mcpserver.Profile{mcpserver.ProfileFull, mcpserver.ProfileUser} {
		for _, args := range []string{
			`{"delivery_id":7,"redraft":true}`,
			`{"delivery_id":7,"note":"x","redraft":true}`,
			`{"delivery_id":7,"REDRAFT":true}`,                 // encoding/json folds case
			`{"delivery_id":7,"redraft":false,"Redraft":true}`, // fold duplicate: ambiguous
			`{"delivery_id":7,"redraft":false,"redraft":true}`, // exact duplicate: last wins
		} {
			fe := &fakeExec{}
			_, err := mcpserver.NewWithProfile(fe, testWorkerID, p).CallTool(context.Background(), rejectName, json.RawMessage(args))
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "redraft") {
				t.Errorf("%s %s(%s) err = %v, want a refusal naming redraft (D3)", p, rejectName, args, err)
			}
			if fe.called {
				t.Errorf("%s %s(%s) reached the executor; the guard must refuse first", p, rejectName, args)
			}
		}
		for _, args := range []string{`{"delivery_id":7}`, `{"delivery_id":7,"note":"superseded","redraft":false}`} {
			fe := &fakeExec{result: executor.Result{Output: json.RawMessage(`{}`)}}
			if _, err := mcpserver.NewWithProfile(fe, testWorkerID, p).CallTool(context.Background(), rejectName, json.RawMessage(args)); err != nil {
				t.Errorf("%s %s(%s) = %v, want forwarded", p, rejectName, args, err)
				continue
			}
			if !fe.called || fe.lastCall.Tool != rejectName || fe.lastCall.Actor != "mcp:"+testWorkerID {
				t.Errorf("%s %s(%s) forwarded %+v, want tool %s as mcp:%s", p, rejectName, args, fe.lastCall, rejectName, testWorkerID)
			}
		}
	}
}

// Acceptance 3, fail-closed: args that do not decode as the discard form are
// refused before the executor.
func TestRejectDelivery_GuardFailsClosed(t *testing.T) {
	for _, args := range []string{`{"delivery_id":7,"redraft":"yes"}`, `{"delivery_id":7,"redraft":1}`} {
		fe := &fakeExec{}
		if _, err := mcpserver.New(fe, testWorkerID).CallTool(context.Background(), rejectName, json.RawMessage(args)); err == nil {
			t.Errorf("%s(%s) was accepted; a non-boolean redraft must be refused", rejectName, args)
		}
		if fe.called {
			t.Errorf("%s(%s) reached the executor", rejectName, args)
		}
	}
}

type noSnapshotLoader struct{ t *testing.T }

func (l noSnapshotLoader) Load(_ context.Context, req policy.Request) (policy.Snapshot, error) {
	l.t.Errorf("the matrix loaded a send snapshot for %s: reject_delivery must not be send-shaped", req.Tool)
	return policy.Snapshot{}, nil
}

// Acceptance 4: an interactive session passes, a worker console is denied.
func TestRejectDelivery_WorkerDenied(t *testing.T) {
	reg := executor.NewRegistry()
	tools.Register(reg, nil)
	checker := policy.NewMatrix(noSnapshotLoader{t: t}, policy.NewStatic(reg.Names()...))
	for _, c := range []struct {
		actor string
		allow bool
	}{
		{"mcp:manual:salvo", true},
		{"mcp:foundry", false},
		{"mcp:foundry.qa", false},
	} {
		d, err := checker.Check(context.Background(), policy.Request{Tool: rejectName, Actor: c.actor})
		if err != nil {
			t.Fatalf("Check(%s): %v", c.actor, err)
		}
		if (d.Decision == "allow") != c.allow {
			t.Errorf("%s as %s = %s/%s, want allow=%v", rejectName, c.actor, d.Decision, d.Rule, c.allow)
		}
		if !c.allow && d.Rule != "human_only" {
			t.Errorf("%s as %s denied by rule %q, want human_only", rejectName, c.actor, d.Rule)
		}
	}
}

// Acceptance 5: the verb line and the unasked exception (D5).
func TestServeInstructions_CarryTheDropLine(t *testing.T) {
	d := strings.ToLower(mcpserver.Instructions)
	for _, want := range []struct{ re, why string }{
		{`swb drop <delivery id>.{0,80}reject_delivery`, "'swb drop <delivery id>' → reject_delivery"},
		{`swb drop <delivery id>[^\n]*sends nothing`, "the drop line says it sends nothing"},
		{`without being asked[^\n]{0,900}reject_delivery[^\n]{0,200}you drafted earlier in this conversation`,
			"the third exception: dropping a delivery it drafted earlier in this conversation"},
		{`any other drop needs salvador.s ask`, "every other drop needs Salvador's ask"},
	} {
		if !regexp.MustCompile(want.re).MatchString(d) {
			t.Errorf("mcpserver.Instructions does not match /%s/: %s", want.re, want.why)
		}
	}
}
