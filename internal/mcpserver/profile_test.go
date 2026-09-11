package mcpserver_test

// SWT-35 (docs/tickets/task-list-mcp_SPEC.md) criteria 23 and 24, added after
// the Codex and go-reviewer passes of 2026-09-10: the READ profile that
// cmd/ops-mcp-read — the user-scope install — served until SWT-37.
//
// SWT-37 (docs/tickets/mcp-task-verbs_SPEC.md) criteria 9–14: the USER profile
// that the renamed cmd/ops-mcp-user now serves at user scope = the read slice
// plus task_dismiss, task_close and task_mark_delivered (V3). After SWT-37 no
// binary builds ProfileRead. It stays as the named fail-closed floor that an
// unknown profile lands on (criterion 14), so the ProfileRead tests below stay.
//
// IMPOSED SURFACE (SPEC V3):
//
//	const ProfileUser Profile = "user"
//	var userProfileTools = readProfileTools + {"task_dismiss", "task_close", "task_mark_delivered"}
//	// NewWithProfile: ProfileFull → agentTools; ProfileUser → userProfileTools;
//	// default → readProfileTools (fail closed to the smallest slice).
//
// GREENFIELD NOTE — EXPECTED RED. mcpserver.ProfileUser does not exist, so this
// package's tests compile-FAIL until adapter.go declares it.
//
// Why a read-only binary and not an omitted secret: a stdio MCP server inherits
// the environment of the shell that launched claude (this repo's own ops-mcp
// processes carry OPS_TOKEN_KEY in /proc/<pid>/environ although .mcp.json never
// sets it), and ~/.bashrc exports OPS_TOKEN_KEY. Leaving it out of
// `claude mcp add -e` withheld nothing. And why a binary rather than a setting on
// ops-mcp: an unset setting would have to mean "full" for every existing
// launcher, so forgetting it in the install would silently restore the write
// surface. A session in an unrelated repo reads untrusted content all day;
// SWT-37 (V0, the owner decision) lets it dismiss, close and mark delivered,
// and nothing else: it still cannot create, claim, draft, approve, send, book,
// link, log, decide, read mail or reopen (criterion 11).

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/mcpserver"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

// The read profile, exactly. Each writes nothing but its audit row.
// task_context is absent on purpose: fetched by the claim holder it flips
// claimed → in_progress.
var wantReadProfileTools = []string{"project_list", "task_get_next", "task_list"}

// The user profile, exactly (SWT-37 criterion 9), sorted.
var wantUserProfileTools = []string{
	"project_list", "task_close", "task_dismiss", "task_get_next", "task_list", "task_mark_delivered",
}

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

	// SWT-37 criterion 14: the switch's default is the READ slice, not the user
	// slice. "USER" is not ProfileUser ("user"). An unknown value that landed
	// on the user profile would be a door to three writes.
	t.Run("USER", func(t *testing.T) {
		srv := mcpserver.NewWithProfile(&fakeExec{}, testWorkerID, mcpserver.Profile("USER"))
		var got []string
		for _, tool := range srv.ListTools() {
			got = append(got, tool.Name)
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(wantReadProfileTools, ",") {
			t.Errorf(`Profile("USER") lists %v, want the %d-tool read slice %v: NewWithProfile's default `+
				"must fail closed to the SMALLEST slice (SWT-37 V3)", got, len(wantReadProfileTools), wantReadProfileTools)
		}
	})
}

// ---- SWT-37: the user profile -------------------------------------------------

// Criterion 9. Exactly six names, and each entry is the full profile's entry
// byte for byte (name, description and schema): the user profile is a SLICE of
// agentTools, never a second spelling.
func TestUserProfile_ListsExactly(t *testing.T) {
	full := map[string]mcpserver.Tool{}
	for _, tool := range mcpserver.New(&fakeExec{}, testWorkerID).ListTools() {
		full[tool.Name] = tool
	}

	srv := mcpserver.NewWithProfile(&fakeExec{}, testWorkerID, mcpserver.ProfileUser)
	var got []string
	for _, tool := range srv.ListTools() {
		got = append(got, tool.Name)
		f, ok := full[tool.Name]
		if !ok {
			t.Errorf("user-profile tool %q is not in the full profile at all", tool.Name)
			continue
		}
		if f.Description != tool.Description || string(f.InputSchema) != string(tool.InputSchema) {
			t.Errorf("user-profile tool %q differs from the full profile's entry: the user profile must take "+
				"its entries FROM agentTools (V3)", tool.Name)
		}
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(wantUserProfileTools, ",") {
		t.Errorf("user profile lists %v, want exactly %v (SWT-37 criterion 9)", got, wantUserProfileTools)
	}
}

// Criterion 10. Every other name the repo knows (agent-facing and spine-facing)
// is refused at the MCP layer and never reaches the executor.
func TestUserProfile_RefusesEveryOtherTool(t *testing.T) {
	user := map[string]bool{}
	for _, n := range wantUserProfileTools {
		user[n] = true
	}
	refused := 0
	for _, name := range append(append([]string(nil), wantAgentTools...), spineTools...) {
		if user[name] {
			continue
		}
		fx := &fakeExec{}
		srv := mcpserver.NewWithProfile(fx, testWorkerID, mcpserver.ProfileUser)
		if _, err := srv.CallTool(context.Background(), name, json.RawMessage(`{}`)); err == nil {
			t.Errorf("user profile accepted a call to %q; only %v may be called (SWT-37 criterion 10)", name, wantUserProfileTools)
		}
		if fx.called {
			t.Errorf("user profile forwarded %q to the executor — the refusal must happen at the MCP layer", name)
		}
		refused++
	}
	if refused < 16 {
		t.Fatalf("POSITIVE CONTROL FAILED: only %d tools were tried against the user profile; the refusal "+
			"loop is not seeing the allowlist", refused)
	}
}

// Criterion 11. By NAME, so a later edit to userProfileTools fails with the
// offending name rather than a count. Nothing that creates, claims, drafts,
// approves, sends, books, links, logs, decides, reads mail or reopens.
func TestUserProfile_NamesNoWriteSurface(t *testing.T) {
	listed := map[string]bool{}
	for _, tool := range mcpserver.NewWithProfile(&fakeExec{}, testWorkerID, mcpserver.ProfileUser).ListTools() {
		listed[tool.Name] = true
	}
	for _, name := range []string{
		"create_task", "create_child_task", // creates
		"task_claim",   // claims
		"task_context", // flips claimed → in_progress for the holder
		"task_append_log", "request_feedback", "mark_done_local",
		"record_decision",                                     // decides
		"draft_delivery", "approve_delivery", "send_delivery", // drafts, approves, sends
		"mark_delivery_sent",              // records a send
		"book_calendar_block",             // books
		"link_external_ref",               // links
		"mail_search", "mail_read_thread", // reads private mail
		"task_reopen", // reopens (Future work)
	} {
		if listed[name] {
			t.Errorf("the user profile lists %q. SWT-37 V3/criterion 11: the user-scope install sits in every "+
				"repo's session and reads untrusted content; it may dismiss, close and mark delivered, and "+
				"nothing else", name)
		}
	}
	if len(listed) == 0 {
		t.Fatal("POSITIVE CONTROL FAILED: the user profile lists nothing")
	}
}

// userProfileLoader fails the test if the matrix ever loads a delivery snapshot:
// a user-profile tool that reached the loader would be send-shaped.
type userProfileLoader struct{ t *testing.T }

func (l userProfileLoader) Load(_ context.Context, req policy.Request) (policy.Snapshot, error) {
	l.t.Errorf("the production matrix loaded a send snapshot for user-profile tool %s — the user binary "+
		"serves a send-shaped tool (invariant 4)", req.Tool)
	return policy.Snapshot{}, nil
}

// Criterion 12. Every user-profile tool, checked as mcp:manual:salvo through
// the production matrix (real registry, loader that must not run), is allowed.
// So no tool the user binary serves is send-shaped.
func TestUserProfile_NoToolReachesTheSendSnapshot(t *testing.T) {
	reg := executor.NewRegistry()
	tools.Register(reg, nil) // nil pool: Register only builds closures
	checker := policy.NewMatrix(userProfileLoader{t}, policy.NewStatic(reg.Names()...))

	checked := 0
	for _, tool := range mcpserver.NewWithProfile(&fakeExec{}, "manual:salvo", mcpserver.ProfileUser).ListTools() {
		d, err := checker.Check(context.Background(), policy.Request{Tool: tool.Name, Actor: "mcp:manual:salvo"})
		if err != nil {
			t.Errorf("Check(%s, mcp:manual:salvo): %v", tool.Name, err)
			continue
		}
		if d.Decision != "allow" {
			t.Errorf("%s as mcp:manual:salvo through the production matrix = %s/%s (%s), want allow", tool.Name,
				d.Decision, d.Rule, d.Reason)
		}
		checked++
	}
	if checked != len(wantUserProfileTools) {
		t.Fatalf("POSITIVE CONTROL FAILED: checked %d user-profile tools, want %d", checked, len(wantUserProfileTools))
	}
}

// Criterion 13. task_dismiss through a ProfileUser server with worker id
// manual:salvo is forwarded as mcp:manual:salvo, the args unaltered except the
// injected worker_id. That actor is what task_dismissals.dismissed_by records
// (V7).
func TestUserProfile_ForwardsWithMCPActor(t *testing.T) {
	fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{"task_id":412,"status":"closed","dismissed":true}`)}}
	srv := mcpserver.NewWithProfile(fx, "manual:salvo", mcpserver.ProfileUser)
	out, err := srv.CallTool(context.Background(), "task_dismiss",
		json.RawMessage(`{"task_id":412,"reason_code":"duplicate","note":"dup of 411"}`))
	if err != nil {
		t.Fatalf("user profile refused task_dismiss: %v", err)
	}
	if !fx.called || fx.lastCall.Tool != "task_dismiss" {
		t.Fatalf("forwarded %+v, want task_dismiss", fx.lastCall)
	}
	if fx.lastCall.Actor != "mcp:manual:salvo" {
		t.Errorf("forwarded Actor = %q, want mcp:manual:salvo", fx.lastCall.Actor)
	}
	args := forwardedKeys(t, fx.lastCall.Args)
	if got := keyList(args); got != "note,reason_code,task_id,worker_id" {
		t.Errorf("forwarded args keys = %s, want note,reason_code,task_id,worker_id (the adapter adds worker_id "+
			"and nothing else). Args: %s", got, fx.lastCall.Args)
	}
	for k, want := range map[string]string{
		"task_id": `412`, "reason_code": `"duplicate"`, "note": `"dup of 411"`, "worker_id": `"manual:salvo"`,
	} {
		if string(args[k]) != want {
			t.Errorf("forwarded %s = %s, want %s", k, args[k], want)
		}
	}
	if string(out) != `{"task_id":412,"status":"closed","dismissed":true}` {
		t.Errorf("CallTool output = %s, want the executor result verbatim", out)
	}
}
