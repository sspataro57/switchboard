package mcpserver_test

// user-mail-read (swb 4683; Salvador 2026-11-23: "I need hda session on the
// levono to be able to read the full emails becuase that session is the one
// that has encrypted storafe and can answer"): mail_read_thread joins the user
// profile, and every user-profile call carries require_general_mail:"true",
// which makes the handler withhold mail the SWT-21 locality rule does not judge
// general (internal/tools/mail_read_gate_integration_test.go). ZERO network,
// ZERO Postgres.
//
// MUTATIONS THAT MUST TURN THIS FILE RED:
//   - drop mail_read_thread from userProfileTools → Listed;
//   - drop its userProfilePins entry → PinIsInjected;
//   - pin it on the full profile too → FullProfileUnpinned.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/mcpserver"
)

const mailReadName = "mail_read_thread"

func TestUserMailRead_Listed(t *testing.T) {
	full := toolMap(mcpserver.NewWithProfile(&fakeExec{}, testWorkerID, mcpserver.ProfileFull).ListTools())
	user := toolMap(mcpserver.NewWithProfile(&fakeExec{}, testWorkerID, mcpserver.ProfileUser).ListTools())
	read := toolMap(mcpserver.NewWithProfile(&fakeExec{}, testWorkerID, mcpserver.ProfileRead).ListTools())
	f, fok := full[mailReadName]
	u, uok := user[mailReadName]
	if !fok || !uok {
		t.Fatalf("%s listed on full=%v user=%v, want both", mailReadName, fok, uok)
	}
	if _, ok := read[mailReadName]; ok {
		t.Errorf("the read profile lists %s; it must not", mailReadName)
	}
	if f.Description != u.Description || string(f.InputSchema) != string(u.InputSchema) {
		t.Errorf("the user profile's %s entry differs from the full profile's", mailReadName)
	}
	if _, ok := user["mail_search"]; ok {
		t.Errorf("the user profile lists mail_search; its snippets are ungated and it must stay off")
	}
}

// The pin overwrites whatever the model passes, fold variants included.
func TestUserMailRead_PinIsInjected(t *testing.T) {
	for _, args := range []string{
		`{"thread_id":7}`,
		`{"thread_id":7,"require_general_mail":""}`,
		`{"thread_id":7,"require_general_mail":"false"}`,
		`{"thread_id":7,"REQUIRE_GENERAL_MAIL":""}`,
		`null`,
	} {
		fe := &fakeExec{result: executor.Result{Output: json.RawMessage(`{}`)}}
		_, err := mcpserver.NewWithProfile(fe, testWorkerID, mcpserver.ProfileUser).CallTool(context.Background(), mailReadName, json.RawMessage(args))
		if err != nil {
			t.Errorf("user %s(%s) = %v, want forwarded", mailReadName, args, err)
			continue
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(fe.lastCall.Args, &got); err != nil {
			t.Fatalf("forwarded args are not an object: %s", fe.lastCall.Args)
		}
		if string(got["require_general_mail"]) != `"true"` {
			t.Errorf("user %s(%s) forwarded %s, want require_general_mail \"true\"", mailReadName, args, fe.lastCall.Args)
		}
		if _, ok := got["REQUIRE_GENERAL_MAIL"]; ok {
			t.Errorf("user %s(%s) kept the fold variant: %s", mailReadName, args, fe.lastCall.Args)
		}
	}
}

func TestUserMailRead_FullProfileUnpinned(t *testing.T) {
	fe := &fakeExec{result: executor.Result{Output: json.RawMessage(`{}`)}}
	if _, err := mcpserver.New(fe, testWorkerID).CallTool(context.Background(), mailReadName, json.RawMessage(`{"thread_id":7}`)); err != nil {
		t.Fatalf("full %s: %v", mailReadName, err)
	}
	var got map[string]json.RawMessage
	_ = json.Unmarshal(fe.lastCall.Args, &got)
	if _, ok := got["require_general_mail"]; ok {
		t.Errorf("the full profile pinned require_general_mail: %s (it reads every thread, as before)", fe.lastCall.Args)
	}
}
