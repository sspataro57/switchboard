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
//
// SWT-38 (docs/tickets/mcp-task-capture_SPEC.md) criteria 11 and 16: the user
// profile gains create_task, task_append_log and task_set_priority (nine
// tools). A session in any repo can now log the work Salvador hands it as a
// HUMAN task, write progress on human tasks and reorder priority. It still
// cannot claim, create worker (claude) tasks, log on worker tasks, draft,
// approve, send, book, link, decide, read mail or reopen. The two claude
// refusals are the profile pin (require_assignee_type:"human", C4; see
// task_capture_test.go), and a worker's refusal on task_set_priority is
// policy.humanOnly (C6).
//
// IMPOSED SURFACE (SPEC "API / MCP tool changes"):
//
//	var userProfileTools = readProfileTools + {"task_dismiss", "task_close", "task_mark_delivered",
//	                                           "create_task", "task_append_log", "task_set_priority"}
//
// EXPECTED RED until adapter.go's userProfileTools gains the three:
// TestUserProfile_ListsExactly (six listed, nine wanted) and
// TestUserProfile_NoToolReachesTheSendSnapshot (positive control: six checked,
// nine wanted).
//
// SWT-42 (docs/tickets/mail-attachments_SPEC.md) criterion 22, owner decision
// O1 (Salvador, 2026-09-12: "yes expose it on the user mcp too"): the user
// profile gains mail_list_attachments and mail_read_attachment — eleven tools.
// Both carry the SWT-21 locality gate in the handler, for every caller, and
// the finder returns headers and attachment names, never a body; mail_search
// and mail_read_thread (the BODY reads) stay forbidden here.
// TestUserProfile_AttachmentToolsAreTheFullProfilesEntries pins that the two
// entries are a slice of agentTools, byte for byte.
//
// EXPECTED RED until adapter.go's userProfileTools and schemas.go gain both:
// TestUserProfile_ListsExactly (nine listed, eleven wanted),
// TestUserProfile_NoToolReachesTheSendSnapshot (positive control: nine checked,
// eleven wanted) and the new schema test.
//
// SWT-44 (user-profile-drafts, Salvador 2026-09-12): the user profile gains
// draft_delivery and update_delivery — thirteen tools. The paragraphs above
// that say it cannot draft were true of SWT-37/38/42 and are superseded here:
// a session in any repo now writes a GMAIL reply as a drafted delivery row
// (pinned require_channel:"gmail", on a thread filed under the task's project:
// require_thread_in_task_project) and edits only gmail drafts its ACTOR created
// (require_own_draft + require_channel; all pins in user_drafts_test.go). It still
// cannot approve or send: Salvador does both on the dashboard, and the approve
// is bound to the words the page showed (expect_content_hash).

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

// The user profile, exactly, sorted. SWT-37 criterion 9 listed six; SWT-38
// (mcp-task-capture) criterion 11 adds three, deliberately:
//   - create_task: log the work Salvador hands a session as a swb task. Always
//     assignee human (C1): the profile pin refuses claude (C4).
//   - task_append_log: progress lines, on human tasks only (C4). A log line on
//     a claude task is text inside a future worker prompt (SPEC fact 5).
//   - task_set_priority: reorder any task (C5); humanOnly, so no worker can (C6).
//
// SWT-42 (mail-attachments) criterion 22, O1: mail_list_attachments and
// mail_read_attachment, eleven then. Attachment reads, gated by the SWT-21
// locality rule in the handler (not by this list), and — through the finder
// form — the only way a session outside the switchboard repo reaches a message.
//
// SWT-44: draft_delivery (gmail only, by pin) and update_delivery (gmail drafts
// created by the caller's actor — mcp:manual:salvo, any interactive session —
// by pin), thirteen in all.
var wantUserProfileTools = []string{
	"create_task", "draft_delivery", "mail_list_attachments", "mail_read_attachment", "project_list",
	"task_append_log", "task_close", "task_dismiss",
	"task_get_next", "task_list", "task_mark_delivered", "task_set_priority", "update_delivery",
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

// Criterion 9 (nine names since SWT-38 criterion 11). Each entry is the full profile's entry
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

// Criterion 11 (SWT-37), AMENDED — not deleted — by SWT-38 criterion 16 and
// SWT-44. By NAME, so a later edit to userProfileTools fails with the offending
// name rather than a count. Nothing that claims, creates child work, approves,
// sends, books, links, decides, reads mail bodies or reopens (drafting left
// this list in SWT-44; see the amendment below).
func TestUserProfile_NamesNoWriteSurface(t *testing.T) {
	listed := map[string]bool{}
	for _, tool := range mcpserver.NewWithProfile(&fakeExec{}, testWorkerID, mcpserver.ProfileUser).ListTools() {
		listed[tool.Name] = true
	}
	for _, name := range []string{
		// create_task and task_append_log LEFT this list in SWT-38 (C3/C4;
		// Salvador, 2026-09-10: "if it's not in swb it should be able to log
		// it"). The user profile now creates HUMAN tasks and logs on them. What
		// keeps that away from the worker consoles is the profile pin
		// (require_assignee_type:"human", enforced in the validator and
		// handler), not this list: see TestUserProfile_PinsHumanAssignee and
		// internal/tools criterion 20.
		"create_child_task", // creates child work, any assignee (claude included)
		"task_claim",        // claims
		"task_context",      // flips claimed → in_progress for the holder
		"request_feedback", "mark_done_local",
		"record_decision", // decides
		// AMENDED — not deleted — by SWT-44 (Salvador, 2026-09-12): draft_delivery
		// and update_delivery joined the user profile, so a session in any repo can
		// write and fix a client reply as a drafted delivery row. Approve and send
		// stay off it: a session must not approve its own client email (the human
		// gate the policy matrix puts on client-facing mail, invariant 4), and the
		// same sessions read untrusted attachment text (SWT-42).
		"approve_delivery", "send_delivery", // approves, sends
		"mark_delivery_sent",  // records a send
		"book_calendar_block", // books
		"link_external_ref",   // links
		// AMENDED — not deleted — by SWT-42 criterion 22 (owner decision O1,
		// 2026-09-12): the ATTACHMENT reads mail_list_attachments and
		// mail_read_attachment were added to the user profile. They carry the SWT-21
		// locality gate (only mail filed under a non-local_only project, or O2's
		// unfiled mail on a clean mailbox) and the finder returns no bodies. The BODY
		// reads below stay forbidden: listing them would put private message bodies
		// into every repo's session, which O1 did not grant.
		"mail_search", "mail_read_thread", // reads private mail bodies
		"task_reopen", // reopens (Future work)
	} {
		if listed[name] {
			t.Errorf("the user profile lists %q. SWT-37 V3/criterion 11, SWT-38 criterion 16: the user-scope "+
				"install sits in every repo's session and reads untrusted content; it may dismiss, close, mark "+
				"delivered, create human tasks, log on them, set priority, read non-private attachments and "+
				"draft gmail replies (SWT-44), and nothing else", name)
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

// SWT-42 criterion 22: the user profile's two attachment tools are the full
// profile's entries byte for byte — a slice of agentTools, never a second
// spelling. The descriptions carry the untrusted-content rule (criterion 21);
// a second spelling of the schema or description is how one profile loses it.
func TestUserProfile_AttachmentToolsAreTheFullProfilesEntries(t *testing.T) {
	full := map[string]mcpserver.Tool{}
	for _, tool := range mcpserver.New(&fakeExec{}, testWorkerID).ListTools() {
		full[tool.Name] = tool
	}
	user := map[string]mcpserver.Tool{}
	for _, tool := range mcpserver.NewWithProfile(&fakeExec{}, "manual:salvo", mcpserver.ProfileUser).ListTools() {
		user[tool.Name] = tool
	}
	for _, name := range []string{"mail_list_attachments", "mail_read_attachment"} {
		f, inFull := full[name]
		u, inUser := user[name]
		if !inFull || !inUser {
			t.Errorf("%s listed: full profile %v, user profile %v — want both (SWT-42 O1)", name, inFull, inUser)
			continue
		}
		if string(f.InputSchema) != string(u.InputSchema) {
			t.Errorf("%s: the user profile's schema %s is not byte-identical to the full profile's %s", name,
				u.InputSchema, f.InputSchema)
		}
		if f.Description != u.Description {
			t.Errorf("%s: the user profile's description differs from the full profile's", name)
		}
	}
}
