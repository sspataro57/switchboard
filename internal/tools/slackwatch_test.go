package tools_test

// The three slack_watch_* tools' no-database half — slack-watch-sweep (SWT-75,
// docs/tickets/slack-watch-sweep_SPEC.md) criteria 2 and 3: they are registered
// on the executor, validate their arguments before any handler runs, live in
// their own file, and are ABSENT from every MCP tool list.
//
// Driven through executor.Execute with a NIL pool (requeue_test.go's shape):
// every refusal stops at Validate, so nothing dereferences the pool, and an
// "unknown tool" error is explicitly rejected as NOT a validation failure.
//
// IMPOSED SURFACE (the SPEC's "API / MCP tool changes" table, verbatim —
// greenfield, so the SPEC's contract defines the signature):
//
//	slack_watch_add         {workspace_id, conversation_id, label?}
//	  -> {id, workspace_id, conversation_id, label, enabled}
//	     upsert on the unique key, re-enabling a disabled row
//	slack_watch_set_enabled {id, enabled} -> {id, enabled}; idempotent no-op success
//	slack_watch_list        {enabled?}
//	  -> {rows:[{id, workspace_id, conversation_id, label, enabled, last_read_at}]}
//
//	registered in tools.Register, implemented in internal/tools/slackwatch.go;
//	ALL THREE policy.humanOnly and NOT MCP-listed — the capture_rule_add shape
//	and for the same reason (internal/tools/capturerules.go:3-19): "an agent must
//	not be able to point the browser at a conversation of its choosing, and
//	browser time is a scarce shared resource".
//
// `enabled` on both set_enabled and list is a POINTER (*bool) so an omitted flag
// is an error / "no filter" rather than a silent false — set_sending_frozen's
// and capture_rule_set_enabled's rule.
//
// GREENFIELD NOTE, EXPECTED RED: none of the three tools is registered and
// slackwatch.go does not exist, so the registration, validation and structure
// tests fail on their own assertions. The MCP-absence scan is a GUARD, green
// today and required to stay green (SPEC mutation row: "MCP-list any
// slack_watch_* tool -> criterion 3").
//
// The id CHECKs are deliberately NOT asserted here. Criterion 1 requires them
// "asserted against the database, not a Go validator", so the malformed-id
// refusals live in slackwatch_integration_test.go and stay red if migration
// 0041 drops a CHECK.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	swAddTool    = "slack_watch_add"
	swEnableTool = "slack_watch_set_enabled"
	swListTool   = "slack_watch_list"
)

var slackWatchTools = []string{swAddTool, swEnableTool, swListTool}

func slackWatchExecutor() *executor.Executor {
	reg := executor.NewRegistry()
	tools.Register(reg, nil)
	// The STATIC allow-list, not the matrix: this file tests validation, and the
	// humanOnly gate is internal/policy's matrix_slackwatch_test.go.
	return executor.New(reg, policy.NewStatic(reg.Names()...), audit.NewMemStore())
}

// Criterion 2: all three go through the executor (validate -> policy -> audit
// start -> handler -> audit complete), like every other tool (invariant 3).
func TestRegister_SlackWatchToolsAreRegistered(t *testing.T) {
	reg := executor.NewRegistry()
	tools.Register(reg, nil)
	have := map[string]bool{}
	for _, n := range reg.Names() {
		have[n] = true
	}
	for _, name := range slackWatchTools {
		if !have[name] {
			t.Errorf("tool %q is not registered by tools.Register. Criterion 2: the watch list is written ONLY "+
				"through the executor, so audit_events answers who pointed the browser at a conversation, when, "+
				"and whether it was allowed (invariant 3)", name)
		}
	}
}

// Criterion 2 / D2: slack_watch_add needs both ids by name.
func TestValidate_SlackWatchAdd_RefusesIncompleteArgs(t *testing.T) {
	ex := slackWatchExecutor()
	ctx := context.Background()
	for _, tc := range []struct{ name, args, wantIn string }{
		{"empty object", `{}`, "workspace_id"},
		{"empty workspace", `{"workspace_id":"","conversation_id":"D04F7LXRB8B"}`, "workspace_id"},
		{"missing conversation", `{"workspace_id":"T0360B84U"}`, "conversation_id"},
		{"empty conversation", `{"workspace_id":"T0360B84U","conversation_id":""}`, "conversation_id"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := ex.Execute(ctx, executor.Call{Tool: swAddTool, Actor: "opsctl:salvo", Args: json.RawMessage(tc.args)})
			if err == nil {
				t.Fatalf("%s(%s) succeeded; want a validation failure naming %s", swAddTool, tc.args, tc.wantIn)
			}
			if strings.Contains(err.Error(), "unknown tool") {
				t.Fatalf("%s is not registered (%v); that is not a validation failure", swAddTool, err)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("%s(%s) = %q, want a refusal naming %q", swAddTool, tc.args, err, tc.wantIn)
			}
		})
	}
}

// Criterion 2: a well-formed add — with and without the optional label — must
// reach the HANDLER. The handler is reached with a nil pool, so it panics or
// errors on the connection; either answer proves the ARGUMENTS were accepted.
// Only a "validate slack_watch_add args" failure is a refusal.
func TestValidate_SlackWatchAdd_AcceptsTheSeedingArgs(t *testing.T) {
	ex := slackWatchExecutor()
	for _, args := range []string{
		`{"workspace_id":"T0360B84U","conversation_id":"DSAV4HJ2F","label":"Jose"}`,
		`{"workspace_id":"T0HPR78RX","conversation_id":"D04F7LXRB8B"}`,
		`{"workspace_id":"T0HPR78RX","conversation_id":"C0BST0C6RV3","label":"a channel, not a DM"}`,
	} {
		args := args
		t.Run(args, func(t *testing.T) {
			var err error
			func() {
				defer func() { _ = recover() }() // a nil pool panics: that is past validation
				_, err = ex.Execute(context.Background(),
					executor.Call{Tool: swAddTool, Actor: "opsctl:salvo", Args: json.RawMessage(args)})
			}()
			if err == nil {
				return
			}
			if strings.Contains(err.Error(), "unknown tool") {
				t.Fatalf("%s is not registered (%v)", swAddTool, err)
			}
			if strings.Contains(err.Error(), "validate "+swAddTool) {
				t.Errorf("%s(%s) was refused by the VALIDATOR: %v. These are the SPEC's own seeding commands "+
					"(\"Usable alone means\"), and label is optional", swAddTool, args, err)
			}
		})
	}
}

// Criterion 2: slack_watch_set_enabled needs an id and an EXPLICIT enabled.
// An omitted flag defaulting to false would silently disable a watch row — the
// capture_rule_set_enabled / set_sending_frozen pointer rule.
func TestValidate_SlackWatchSetEnabled_RequiresIDAndAnExplicitFlag(t *testing.T) {
	ex := slackWatchExecutor()
	ctx := context.Background()
	for _, tc := range []struct{ name, args, wantIn string }{
		{"empty object", `{}`, "id"},
		{"zero id", `{"id":0,"enabled":false}`, "id"},
		{"negative id", `{"id":-3,"enabled":true}`, "id"},
		{"missing enabled", `{"id":7}`, "enabled"},
		{"null enabled", `{"id":7,"enabled":null}`, "enabled"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := ex.Execute(ctx, executor.Call{Tool: swEnableTool, Actor: "opsctl:salvo", Args: json.RawMessage(tc.args)})
			if err == nil {
				t.Fatalf("%s(%s) succeeded; want a validation failure naming %s", swEnableTool, tc.args, tc.wantIn)
			}
			if strings.Contains(err.Error(), "unknown tool") {
				t.Fatalf("%s is not registered (%v); that is not a validation failure", swEnableTool, err)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("%s(%s) = %q, want a refusal naming %q (an omitted flag must never mean false: "+
					"disabling a watch row is how a conversation goes back to a 30-minute cadence)",
					swEnableTool, tc.args, err, tc.wantIn)
			}
		})
	}
}

// Criterion 2: slack_watch_list takes an OPTIONAL enabled filter and nothing
// else — `{}` is the /sources and `opsctl slack-watch list` call.
func TestValidate_SlackWatchList_AcceptsAnOptionalEnabledFilter(t *testing.T) {
	ex := slackWatchExecutor()
	for _, args := range []string{`{}`, `{"enabled":true}`, `{"enabled":false}`} {
		args := args
		t.Run(args, func(t *testing.T) {
			var err error
			func() {
				defer func() { _ = recover() }()
				_, err = ex.Execute(context.Background(),
					executor.Call{Tool: swListTool, Actor: "dashboard:salvo", Args: json.RawMessage(args)})
			}()
			if err == nil {
				return
			}
			if strings.Contains(err.Error(), "unknown tool") {
				t.Fatalf("%s is not registered (%v)", swListTool, err)
			}
			if strings.Contains(err.Error(), "validate "+swListTool) {
				t.Errorf("%s(%s) was refused by the VALIDATOR: %v. Criterion 2: enabled is an optional filter, "+
					"and the unfiltered call is what /sources and opsctl use", swListTool, args, err)
			}
		})
	}
}

// Criterion 3, first half: absent from BOTH MCP profiles' schemas. The SPEC's
// mutation row "MCP-list any slack_watch_* tool" turns this red.
//
// GUARD: green today, and must stay green.
func TestSlackWatchTools_StayOffBothMCPProfiles(t *testing.T) {
	for _, rel := range []string{"internal/mcpserver/schemas.go", "internal/mcpserver/adapter.go"} {
		b, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		src := string(b)
		// Control: the scan must be looking at files that list tools at all.
		if !strings.Contains(src, `"task_list"`) {
			t.Fatalf("%s does not mention \"task_list\"; this scan is not looking at a tool list", rel)
		}
		for _, name := range slackWatchTools {
			if strings.Contains(src, `"`+name+`"`) {
				t.Errorf("%s names %q. Criterion 3: the watch tools are off the MCP list AND humanOnly — two "+
					"independent defences. Off the list keeps a worker from SEEING the tool; humanOnly keeps a "+
					"worker-shaped actor from calling it through any other surface (capturerules.go:10-20)", rel, name)
			}
		}
	}
}

// Criterion 2 / D2, the file's shape: the three tools live in their own file,
// the add is an UPSERT that re-enables, and there is no delete. A misfiring
// watch row is turned OFF, never removed — the capture_rules rule, and here it
// also keeps `last_read_at` history joinable to a row that still exists.
func TestSlackWatchFile_UpsertsAndNeverDeletes(t *testing.T) {
	b, err := os.ReadFile("slackwatch.go")
	if err != nil {
		t.Fatalf("read internal/tools/slackwatch.go: %v — the SPEC's \"API / MCP tool changes\" section puts "+
			"the three tools in a new internal/tools/slackwatch.go", err)
	}
	var code []string
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		code = append(code, line)
	}
	src := strings.Join(code, "\n")
	lower := strings.ToLower(src)

	if !strings.Contains(lower, "insert into slack_watch") {
		t.Errorf("slackwatch.go never INSERTs into slack_watch; slack_watch_add is the table's only writer " +
			"(criterion 2)")
	}
	if !strings.Contains(lower, "on conflict") {
		t.Errorf("slackwatch.go has no ON CONFLICT. Criterion 2: slack_watch_add on an existing " +
			"(workspace_id, conversation_id) pair updates the label and RE-ENABLES rather than erroring — " +
			"idempotent, because the owner works the board concurrently and re-running a seed command must succeed")
	}
	if !strings.Contains(lower, "enabled") {
		t.Errorf("slackwatch.go never mentions `enabled`; the re-enable is half of criterion 2's idempotency")
	}
	if strings.Contains(lower, "delete from slack_watch") {
		t.Errorf("slackwatch.go DELETEs from slack_watch. D2/criterion 2: a watch row is turned off with " +
			"slack_watch_set_enabled, never removed — the capture_rules rule, and `last_read_at` on /sources " +
			"needs the row to still exist")
	}
	for _, banned := range []struct{ tok, why string }{
		{"raw_source_items", "invariant 1: there is exactly ONE raw writer, and it is the connector's ingest path"},
		{"tasks", "invariant 2: slack_watch causes tasks only through capture.EvaluateRules, never directly"},
		{"deliveries", "D11: no delivery behaviour is touched by this ticket"},
	} {
		if strings.Contains(lower, banned.tok) {
			t.Errorf("slackwatch.go mentions %s — %s", banned.tok, banned.why)
		}
	}
}
