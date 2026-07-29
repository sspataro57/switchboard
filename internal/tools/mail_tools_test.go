package tools_test

// Unit tests for the read-only mail tools added by SWT-11 (SPEC
// imap-mail-connector, acceptance criterion 16). Greenfield: these fail until
// `internal/tools/mail.go` exists and createtask.go registers both tools.
//
// NOTE: this file was rewritten on 2026-07-27 after the original was destroyed
// by a filename collision while the SWT-11 tests were parked out of the tree
// during the gmail-bridge delivery. It re-encodes the SPEC's criterion 16
// contract; if the original covered more, restore from that ticket's branch.

import (
	"context"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/tools"
)

// registeredToolNames is the seam every other tools test uses: Register wires
// the whole surface onto a bare executor.
func registeredMailTools(t *testing.T) map[string]bool {
	t.Helper()
	ex := executor.New(nil, nil, nil)
	tools.Register(ex, nil)
	names := map[string]bool{}
	for _, name := range ex.ToolNames() {
		names[name] = true
	}
	return names
}

func TestRegister_MailToolsRegistered(t *testing.T) {
	names := registeredMailTools(t)
	for _, want := range []string{"mail_search", "mail_read_thread"} {
		if !names[want] {
			t.Errorf("%s is not registered; the read-only mail surface is missing", want)
		}
	}
}

func TestValidate_MailSearch_RequiresASelector(t *testing.T) {
	// An unselective search over a 106k-message corpus is not a query, it is a
	// dump: at least one of query/from/thread_key must be present.
	ex := executor.New(nil, nil, nil)
	tools.Register(ex, nil)

	_, err := ex.Execute(context.Background(), executor.Call{
		Tool: "mail_search", Actor: "manual:test", Args: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("mail_search accepted no selector at all")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "query") &&
		!strings.Contains(strings.ToLower(err.Error()), "selector") {
		t.Fatalf("mail_search error = %v, want it to name the missing selector", err)
	}
}

func TestValidate_MailSearch_RejectsBlankSelectors(t *testing.T) {
	ex := executor.New(nil, nil, nil)
	tools.Register(ex, nil)

	for _, args := range []string{
		`{"query":""}`,
		`{"from":"   "}`,
		`{"thread_key":""}`,
	} {
		if _, err := ex.Execute(context.Background(), executor.Call{
			Tool: "mail_search", Actor: "manual:test", Args: []byte(args),
		}); err == nil {
			t.Errorf("mail_search accepted a blank selector: %s", args)
		}
	}
}

func TestValidate_MailSearch_RejectsUnknownDirection(t *testing.T) {
	ex := executor.New(nil, nil, nil)
	tools.Register(ex, nil)

	if _, err := ex.Execute(context.Background(), executor.Call{
		Tool: "mail_search", Actor: "manual:test",
		Args: []byte(`{"query":"invoice","direction":"sideways"}`),
	}); err == nil {
		t.Fatal("mail_search accepted a direction outside inbound/outbound")
	}
}

func TestValidate_MailReadThread_RequiresAnIdentifier(t *testing.T) {
	ex := executor.New(nil, nil, nil)
	tools.Register(ex, nil)

	if _, err := ex.Execute(context.Background(), executor.Call{
		Tool: "mail_read_thread", Actor: "manual:test", Args: []byte(`{}`),
	}); err == nil {
		t.Fatal("mail_read_thread accepted neither thread_id nor thread_key")
	}
}
