package mcpserver_test

// SWT-35 (docs/tickets/task-list-mcp_SPEC.md) criteria 23 and 24, added after
// the Codex and go-reviewer passes of 2026-09-10: the READ profile that
// cmd/ops-mcp-read — the user-scope install — serves.
//
// Why a read-only binary and not an omitted secret: a stdio MCP server inherits
// the environment of the shell that launched claude (this repo's own ops-mcp
// processes carry OPS_TOKEN_KEY in /proc/<pid>/environ although .mcp.json never
// sets it), and ~/.bashrc exports OPS_TOKEN_KEY. Leaving it out of
// `claude mcp add -e` withheld nothing. And why a binary rather than a setting on
// ops-mcp: an unset setting would have to mean "full" for every existing
// launcher, so forgetting it in the install would silently restore the write
// surface. A session in an unrelated repo reads untrusted content all day; what
// it is handed must be unable to change anything — not merely unable to send.

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/mcpserver"
)

// The read profile, exactly. Each writes nothing but its audit row.
// task_context is absent on purpose: fetched by the claim holder it flips
// claimed → in_progress.
var wantReadProfileTools = []string{"project_list", "task_get_next", "task_list"}

func TestReadProfile_ListsExactlyTheQueueReads(t *testing.T) {
	full := map[string]string{}
	for _, tool := range mcpserver.New(&fakeExec{}, testWorkerID).ListTools() {
		full[tool.Name] = string(tool.InputSchema)
	}

	srv := mcpserver.NewWithProfile(&fakeExec{}, testWorkerID, mcpserver.ProfileRead)
	var got []string
	for _, tool := range srv.ListTools() {
		got = append(got, tool.Name)
		if s, ok := full[tool.Name]; !ok || s != string(tool.InputSchema) {
			t.Errorf("read-profile tool %q is not the full profile's entry (schema %s vs %s): the read "+
				"profile must be a SLICE of agentTools, never a second spelling of a schema", tool.Name,
				tool.InputSchema, s)
		}
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(wantReadProfileTools, ",") {
		t.Errorf("read profile lists %v, want exactly %v (criterion 23)", got, wantReadProfileTools)
	}
}

func TestReadProfile_RefusesEveryOtherTool(t *testing.T) {
	read := map[string]bool{}
	for _, n := range wantReadProfileTools {
		read[n] = true
	}
	refused := 0
	for _, name := range append(append([]string(nil), wantAgentTools...), spineTools...) {
		if read[name] {
			continue
		}
		fx := &fakeExec{}
		srv := mcpserver.NewWithProfile(fx, testWorkerID, mcpserver.ProfileRead)
		if _, err := srv.CallTool(context.Background(), name, json.RawMessage(`{}`)); err == nil {
			t.Errorf("read profile accepted a call to %q; only %v may be called (criterion 23)", name, wantReadProfileTools)
		}
		if fx.called {
			t.Errorf("read profile forwarded %q to the executor — the refusal must happen at the MCP layer", name)
		}
		refused++
	}
	if refused < 20 {
		t.Fatalf("POSITIVE CONTROL FAILED: only %d tools were tried against the read profile; the refusal "+
			"loop is not seeing the allowlist", refused)
	}
}

func TestReadProfile_ForwardsAQueueReadWithMCPActor(t *testing.T) {
	fx := &fakeExec{}
	srv := mcpserver.NewWithProfile(fx, "manual:salvo", mcpserver.ProfileRead)
	if _, err := srv.CallTool(context.Background(), "task_list", json.RawMessage(`{"project":"saka"}`)); err != nil {
		t.Fatalf("read profile refused task_list: %v", err)
	}
	if !fx.called || fx.lastCall.Tool != "task_list" || fx.lastCall.Actor != "mcp:manual:salvo" {
		t.Errorf("forwarded %+v, want task_list as mcp:manual:salvo", fx.lastCall)
	}
}

// Criterion 24: a profile value New does not know is not a door to the full
// surface — NewWithProfile keeps agentTools whole ONLY for ProfileFull.
func TestNewWithProfile_UnknownProfileIsTheReadSlice(t *testing.T) {
	srv := mcpserver.NewWithProfile(&fakeExec{}, testWorkerID, mcpserver.Profile("READ"))
	if n := len(srv.ListTools()); n != len(wantReadProfileTools) {
		t.Errorf("an unknown profile lists %d tools, want the %d-tool read slice: only ProfileFull may serve "+
			"the write surface", n, len(wantReadProfileTools))
	}
}
