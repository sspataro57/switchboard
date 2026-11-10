package mcpserver_test

// mcp-task-reopen (docs/tickets/mcp-task-reopen_SPEC.md, Salvador 2026-11-10:
// "add ability to claude to reopen taks on swb"): task_reopen's PLAIN form
// {task_id, reason, status?} on ops-mcp (full) and ops-mcp-user (user).
// Criteria 1-3 (surface), 5-7 (the adapter guard, F7) and 12-13 (the words,
// Q1 = b). ZERO network, ZERO Postgres: the adapter is driven with fakeExec.
//
// The policy half (criteria 8-11) lives in internal/policy (matrix_mcpverbs_test.go,
// matrix_reopen_test.go, the two shape tests) and in adapter_test.go's
// TestMCPListing_DoesNotMakeTaskVerbsWorkerCallable row. Criterion 4
// (tools.OpenStatuses returns a copy) is internal/tools/reopen_test.go.
// Criteria 16-18 are reopen_integration_test.go (build tag integration).
//
// IMPOSED SURFACE (SPEC "API / MCP tool changes"):
//
//	// internal/tools/close.go
//	func OpenStatuses() []string // a COPY of openStatuses
//	// internal/mcpserver/adapter.go, in CallTool after rejectFoldDuplicateKeys:
//	func rejectReopenActivityArgs(args json.RawMessage) error
//
// GREENFIELD NOTE — EXPECTED RED: until close.go exports OpenStatuses this
// package compile-FAILs; then, until schemas.go lists task_reopen, adapter.go
// lists it in userProfileTools and guards it, and serve.go / SKILL.md carry the
// verbatim lines, every test here fails on its own assertion.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Verification protocol" 3):
//   - drop the CallTool guard call → ActivityFormsRefused, GuardFailsClosed;
//   - add "message_id" to the schema → TaskReopenSchema;
//   - drop it from userProfileTools → ListedOnFullAndUserProfiles;
//   - revert the closing paragraph to "The one exception" → the Instructions
//     exception pins.

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/mcpserver"
	"github.com/sspataro57/switchboard/internal/tools"
)

const reopenName = "task_reopen"

// ---- Surface ---------------------------------------------------------------

// Criterion 1: ProfileFull lists task_reopen (37 tools), ProfileUser lists it
// (25 tools), ProfileRead does not, and the user entry is the full entry byte
// for byte (name, description, schema) — a slice of agentTools, never a second
// spelling.
func TestTaskReopen_ListedOnFullAndUserProfiles(t *testing.T) {
	byProfile := map[mcpserver.Profile]map[string]mcpserver.Tool{}
	for _, p := range []mcpserver.Profile{mcpserver.ProfileFull, mcpserver.ProfileUser, mcpserver.ProfileRead} {
		m := map[string]mcpserver.Tool{}
		for _, tl := range mcpserver.NewWithProfile(&fakeExec{}, testWorkerID, p).ListTools() {
			m[tl.Name] = tl
		}
		byProfile[p] = m
	}
	for _, c := range []struct {
		name    string
		profile mcpserver.Profile
		listed  bool
		count   int
	}{
		{"full", mcpserver.ProfileFull, true, 37},
		{"user", mcpserver.ProfileUser, true, 25},
		{"read", mcpserver.ProfileRead, false, 3},
	} {
		got := byProfile[c.profile]
		if _, ok := got[reopenName]; ok != c.listed {
			t.Errorf("the %s profile lists %s = %v, want %v (criterion 1: both MCP profiles serve the plain form; "+
				"the read profile is queue READS only)", c.name, reopenName, ok, c.listed)
		}
		if len(got) != c.count {
			t.Errorf("the %s profile lists %d tools, want %d (criterion 1)", c.name, len(got), c.count)
		}
	}
	// New(...) is the full profile: the SPEC states the criterion against it.
	if _, ok := toolMap(mcpserver.New(&fakeExec{}, testWorkerID).ListTools())[reopenName]; !ok {
		t.Errorf("mcpserver.New(...) does not list %s (criterion 1)", reopenName)
	}
	f, fok := byProfile[mcpserver.ProfileFull][reopenName]
	u, uok := byProfile[mcpserver.ProfileUser][reopenName]
	if fok && uok {
		if f.Name != u.Name || f.Description != u.Description || string(f.InputSchema) != string(u.InputSchema) {
			t.Errorf("the user profile's %s entry differs from the full profile's: the user profile takes its "+
				"entries FROM agentTools (criterion 1)", reopenName)
		}
	}
}

func toolMap(ts []mcpserver.Tool) map[string]mcpserver.Tool {
	m := map[string]mcpserver.Tool{}
	for _, tl := range ts {
		m[tl.Name] = tl
	}
	return m
}

// Criterion 2: exactly task_id (integer), reason (string), status (string);
// required exactly [task_id, reason]; status.enum == tools.OpenStatuses()
// element for element and IN ORDER; and the schema text names none of the
// identity or activity-form arguments.
func TestTaskReopenSchema(t *testing.T) {
	tl := listedTool(t, reopenName)
	s := parseVerbSchema(t, reopenName, tl.InputSchema)
	assertVerbProps(t, reopenName, s,
		map[string]string{"task_id": "integer", "reason": "string", "status": "string"},
		[]string{"task_id", "reason"})

	got := s.Properties["status"].Enum
	want := tools.OpenStatuses()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s status enum = %v, want tools.OpenStatuses() = %v element for element and in order (criterion "+
			"2, D6): the enum a model reads must be exactly what validateReopen accepts", reopenName, got, want)
	}
	if strings.Join(want, ",") != "holding,ready,blocked,done_locally,delivered" {
		t.Errorf("POSITIVE CONTROL: tools.OpenStatuses() = %v, want [holding ready blocked done_locally delivered]", want)
	}

	raw := string(tl.InputSchema)
	for _, forbidden := range []string{"worker_id", "dismissal_id", "message_id", "revive", "notifier_copy"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("%s's schema mentions %q (criterion 2). worker_id is injected from OPS_WORKER_ID, never "+
				"model-supplied; the activity forms are spine-only (capture/promote). Schema: %s", reopenName, forbidden, raw)
		}
	}
}

// Criterion 3: the description, pinned by regex (lower-cased). D4: it says the
// reopen marks the task SURFACED for the Jira status sync — not that it lands
// in the board's INCOMING section, which reads activity_at.
func TestTaskReopenDescription(t *testing.T) {
	tl := listedTool(t, reopenName)
	d := strings.ToLower(tl.Description)
	for _, want := range []struct{ re, why string }{
		{`(?s)closed.{0,80}back.{0,40}open status`, "it moves a closed task back to an open status"},
		{`ready[^.]{0,60}omit|omit[^.]{0,60}ready`, "ready is used when status is omitted"},
		{`(?s)not closed.{0,80}idempotent`, "a task that is not closed is an idempotent success…"},
		{`reopened:\s?false`, "…with reopened:false"},
		{`(?s)open dismissal.{0,40}reopened`, "it stamps an open dismissal as reopened"},
		{`(?s)human reopen.{0,60}surfaced`, "a human reopen marks the task surfaced…"},
		{`(?s)surfaced.{0,120}jira status sync.{0,60}(does not|doesn.t|never|won.t) close`,
			"…so the Jira status sync does not close it again"},
		{`nothing is sent|sends nothing`, "nothing is sent"},
		{`human sessions only`, "human sessions only…"},
		{`(?s)worker.{0,40}refused by policy`, "…and a worker console is refused by policy"},
	} {
		if !regexp.MustCompile(want.re).MatchString(d) {
			t.Errorf("%s's description does not match /%s/ — criterion 3: %s. Description: %q",
				reopenName, want.re, want.why, tl.Description)
		}
	}
	for _, wrong := range []string{"incoming", "on the board"} {
		if strings.Contains(d, wrong) {
			t.Errorf("%s's description says %q. D4: a plain reopen does NOT put the task in INCOMING (that is "+
				"activity_at > reviewed_at); surfaced_at is the Jira-sync hold. Description: %q", reopenName, wrong, tl.Description)
		}
	}
}

// ---- Adapter guard (F7) ----------------------------------------------------

// reopenGuardWorkers are the identities criterion 5 says the guard holds for:
// every worker id, manual:salvo included (D2: this repo's own full-profile
// session too).
var reopenGuardWorkers = []string{"manual:salvo", "acme", "acme.main", "worker:avviato"}

var reopenGuardProfiles = []struct {
	name    string
	profile mcpserver.Profile
}{
	{"full", mcpserver.ProfileFull},
	{"user", mcpserver.ProfileUser},
}

// Criterion 5: any of dismissal_id, message_id, revive, notifier_copy decoding
// NON-ZERO is refused at the adapter — an error, and the executor is never
// called — on both profiles, for every worker id, under the exact key AND a
// case-folded key (encoding/json matches keys case-insensitively, F2). The
// error names the field and says the activity forms are spine-only
// (capture/promote).
func TestTaskReopen_ActivityFormsRefusedAtTheAdapter(t *testing.T) {
	cases := []struct {
		key, value, field string
	}{
		{"dismissal_id", `7`, "dismissal_id"},
		{"message_id", `9`, "message_id"},
		{"revive", `true`, "revive"},
		{"notifier_copy", `true`, "notifier_copy"},
		// Case-folded spellings (criterion 5 names these four).
		{"Dismissal_Id", `7`, "dismissal_id"},
		{"MESSAGE_ID", `9`, "message_id"},
		{"Revive", `true`, "revive"},
		{"NOTIFIER_COPY", `true`, "notifier_copy"},
		// go-reviewer hardening: a Unicode fold (U+017F folds to s) decodes as
		// the real field, and an exact duplicate whose later value is null
		// leaves the earlier 9 in place — both must stay refused if the mirror
		// struct or the guard order ever changes.
		{"meſſage_id", `9`, "message_id"},
		{"message_id", `9,"message_id":null`, "message_id"},
	}
	for _, p := range reopenGuardProfiles {
		for _, worker := range reopenGuardWorkers {
			for _, c := range cases {
				p, worker, c := p, worker, c
				t.Run(p.name+"/"+worker+"/"+c.key, func(t *testing.T) {
					fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{}`)}}
					srv := mcpserver.NewWithProfile(fx, worker, p.profile)
					args := `{"task_id":412,"reason":"more to add","` + c.key + `":` + c.value + `}`
					_, err := srv.CallTool(context.Background(), reopenName, json.RawMessage(args))
					if err == nil {
						t.Fatalf("CallTool(%s, %s) on the %s profile as %q = nil error, want a refusal. Criterion 5 / "+
							"F7: an argument absent from the schema is not absent from the call; the guard, not the "+
							"schema, keeps the message-id forms spine-only", reopenName, args, p.name, worker)
					}
					if fx.called {
						t.Errorf("CallTool(%s, %s) reached the executor; the guard must refuse BEFORE it (no audit "+
							"row, the rejectParentID precedent)", reopenName, args)
					}
					msg := strings.ToLower(err.Error())
					if !strings.Contains(msg, c.field) {
						t.Errorf("refusal %q does not name the field %q (criterion 5)", err, c.field)
					}
					if !strings.Contains(msg, "spine") || !strings.Contains(msg, "capture") || !strings.Contains(msg, "promote") {
						t.Errorf("refusal %q does not say the activity forms are spine-only (capture/promote) "+
							"(criterion 5)", err)
					}
				})
			}
		}
	}
}

// Criterion 6, the fail-closed half (D1): a guard field that does not decode
// is refused the same way — never waved through to a handler that might read
// it differently.
func TestTaskReopen_GuardFailsClosedOnADecodeError(t *testing.T) {
	for _, args := range []string{
		`{"task_id":1,"reason":"x","revive":"yes"}`, // the SPEC's example
		`{"task_id":1,"reason":"x","message_id":"9"}`,
		`{"task_id":1,"reason":"x","dismissal_id":1.5}`,
		`{"task_id":1,"reason":"x","notifier_copy":1}`,
		`{"task_id":1,"reason":"x","MESSAGE_ID":"abc"}`,
	} {
		for _, p := range reopenGuardProfiles {
			args, p := args, p
			t.Run(p.name+"/"+args, func(t *testing.T) {
				fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{}`)}}
				srv := mcpserver.NewWithProfile(fx, "manual:salvo", p.profile)
				if _, err := srv.CallTool(context.Background(), reopenName, json.RawMessage(args)); err == nil {
					t.Errorf("CallTool(%s, %s) = nil error, want a refusal: D1, the guard fails closed on its own "+
						"decode error", reopenName, args)
				}
				if fx.called {
					t.Errorf("CallTool(%s, %s) reached the executor; a guard decode error must be refused before it",
						reopenName, args)
				}
			})
		}
	}
}

// Criterion 6, the zero-value half: 0, false and null are the plain form —
// the handler already treats them so — and are forwarded.
func TestTaskReopen_GuardPassesZeroValues(t *testing.T) {
	for _, args := range []string{
		`{"task_id":412,"reason":"more to add","message_id":0}`,
		`{"task_id":412,"reason":"more to add","revive":false}`,
		`{"task_id":412,"reason":"more to add","message_id":null}`,
		`{"task_id":412,"reason":"more to add","dismissal_id":0,"notifier_copy":false}`,
		`{"task_id":412,"reason":"more to add","revive":null,"notifier_copy":null,"dismissal_id":null}`,
	} {
		for _, p := range reopenGuardProfiles {
			args, p := args, p
			t.Run(p.name+"/"+args, func(t *testing.T) {
				fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{"task_id":412,"status":"ready","reopened":true}`)}}
				srv := mcpserver.NewWithProfile(fx, "manual:salvo", p.profile)
				if _, err := srv.CallTool(context.Background(), reopenName, json.RawMessage(args)); err != nil {
					t.Fatalf("CallTool(%s, %s) = %v, want it forwarded: a zero value is the plain form (criterion 6)",
						reopenName, args, err)
				}
				if !fx.called || fx.lastCall.Tool != reopenName {
					t.Errorf("CallTool(%s, %s) forwarded %+v, want %s", reopenName, args, fx.lastCall, reopenName)
				}
			})
		}
	}
}

// Criterion 7: the plain call through ProfileUser as manual:salvo is forwarded
// as Tool task_reopen, Actor mcp:manual:salvo, with arg keys exactly
// reason,status,task_id,worker_id — the adapter adds worker_id and nothing else
// (no profile pin, D3) — and the executor's output verbatim.
func TestTaskReopen_UserProfileForwardsThePlainForm(t *testing.T) {
	const result = `{"task_id":412,"status":"ready","reopened":true}`
	fx := &fakeExec{result: executor.Result{Output: json.RawMessage(result)}}
	srv := mcpserver.NewWithProfile(fx, "manual:salvo", mcpserver.ProfileUser)
	out, err := srv.CallTool(context.Background(), reopenName,
		json.RawMessage(`{"task_id":412,"reason":"more to add","status":"ready"}`))
	if err != nil {
		t.Fatalf("the user profile refused the plain %s: %v (criterion 7)", reopenName, err)
	}
	if !fx.called || fx.lastCall.Tool != reopenName {
		t.Fatalf("forwarded %+v, want Tool %s", fx.lastCall, reopenName)
	}
	if fx.lastCall.Actor != "mcp:manual:salvo" {
		t.Errorf("forwarded Actor = %q, want mcp:manual:salvo (what task_dismissals.reopened_by records)", fx.lastCall.Actor)
	}
	args := forwardedKeys(t, fx.lastCall.Args)
	if got := keyList(args); got != "reason,status,task_id,worker_id" {
		t.Errorf("forwarded arg keys = %s, want exactly reason,status,task_id,worker_id (criterion 7: no pin, the "+
			"adapter adds worker_id and nothing else). Args: %s", got, fx.lastCall.Args)
	}
	for k, want := range map[string]string{
		"task_id": `412`, "reason": `"more to add"`, "status": `"ready"`, "worker_id": `"manual:salvo"`,
	} {
		if string(args[k]) != want {
			t.Errorf("forwarded %s = %s, want %s", k, args[k], want)
		}
	}
	if string(out) != result {
		t.Errorf("CallTool output = %s, want the executor result verbatim", out)
	}
}

// ---- Words (Q1 = b) ----------------------------------------------------------

// Criterion 12: serve.go's Instructions gain the `swb reopen` line right after
// the `swb done <id>` line, and the closing rule carries two exceptions, both
// "without being asked" — the second is task_reopen on a HUMAN task the session
// itself closed earlier in this conversation, then saying so and signalling
// working again. Every other reopen needs Salvador's ask. Lower-cased, like the
// existing Instructions tests (queue_tools_test.go).
func TestServeInstructions_CarryTheReopenLineAndOwnCloseRule(t *testing.T) {
	d := strings.ToLower(mcpserver.Instructions)
	for _, want := range []struct{ re, why string }{
		{`swb reopen <id>.{0,80}task_reopen`, "'swb reopen <id> [status]' → task_reopen"},
		{`not closed`, "say that it was not closed (nothing changed)"},
		{`swb reopen <id>[^\n]*task_signal with working again`, "the reopen line: call task_signal with working again"},
		{`without being asked[^\n]{0,400}task_reopen[^\n]{0,200}closed earlier in this conversation`,
			"the second exception (Q1 = b): task_reopen on a human task it closed earlier in this conversation"},
		{`any other reopen needs salvador.s ask`, "every other reopen needs Salvador's ask"},
	} {
		if !regexp.MustCompile(want.re).MatchString(d) {
			t.Errorf("mcpserver.Instructions does not match /%s/ — criterion 12: %s", want.re, want.why)
		}
	}
	if strings.Contains(d, "the one exception") {
		t.Errorf("mcpserver.Instructions still says \"The one exception\": criterion 12(b) rewrites the closing " +
			"paragraph whole, with two exceptions")
	}

	// 12(a): placement — the swb reopen line is the line right after swb done.
	lines := strings.Split(mcpserver.Instructions, "\n")
	done := -1
	for i, l := range lines {
		if strings.HasPrefix(l, `- "swb done <id>"`) {
			done = i
		}
	}
	if done < 0 {
		t.Fatalf("POSITIVE CONTROL: mcpserver.Instructions has no `- \"swb done <id>\"` line")
	}
	if done+1 >= len(lines) || !strings.HasPrefix(lines[done+1], `- "swb reopen <id> [status]" → call task_reopen`) {
		next := ""
		if done+1 < len(lines) {
			next = lines[done+1]
		}
		t.Errorf("the line after `swb done <id>` is %q, want the `- \"swb reopen <id> [status]\" → call task_reopen …` "+
			"line (criterion 12a: same bullet form, right after swb done)", next)
	}
}

// Criterion 13: skills/swb-status/SKILL.md gains the point-4 bullet right after
// "When you finish: `task_close`…" and the point-7 trigger row right after
// `swb done <id>`.
func TestSkill_CarriesTheReopenBulletAndRow(t *testing.T) {
	doc := readSkill(t)
	lower := strings.ToLower(doc)
	for _, want := range []struct{ re, why string }{
		{`(?s)swb reopen <id>.{0,80}task_reopen`, "point 7's row: `swb reopen <id> [status]` → `task_reopen`"},
		{`(?s)task_reopen.{0,400}task_signal.{0,40}working`, "point 4: reopen, then task_signal working again"},
		// The SPEC writes this pin without (?s); its verbatim bullet breaks the
		// line between "yourself" and "in this conversation", so (?s) is what
		// lets the SPEC's own text satisfy it.
		{`(?s)closed yourself.{0,40}in this conversation`, "point 4: only a HUMAN task you closed yourself in this conversation"},
		{`(?s)any other reopen.{0,80}swb reopen <id>`, "point 4: any other reopen waits for his `swb reopen <id>`"},
	} {
		if !regexp.MustCompile(want.re).MatchString(lower) {
			t.Errorf("%s does not match /%s/ — criterion 13: %s", skillRel, want.re, want.why)
		}
	}

	// 13(a): the bullet sits right after the "When you finish" bullet, inside point 4.
	finish := strings.Index(doc, "When you finish: `task_close`")
	bullet := strings.Index(doc, "If you closed it and, in this same conversation")
	point5 := strings.Index(doc, "\n5. ")
	if finish < 0 {
		t.Fatalf("POSITIVE CONTROL: %s has no \"When you finish: `task_close`\" bullet", skillRel)
	}
	if bullet < 0 {
		t.Errorf("%s has no point-4 bullet \"If you closed it and, in this same conversation…\" (criterion 13a)", skillRel)
	} else {
		between := doc[finish:bullet]
		if strings.Count(between, "\n   - ") != 1 {
			t.Errorf("the reopen bullet is not the bullet right after \"When you finish\" (criterion 13a); between "+
				"them: %q", between)
		}
		if point5 >= 0 && bullet > point5 {
			t.Errorf("the reopen bullet sits after point 5; criterion 13a puts it in point 4")
		}
	}

	// 13(b): the row right after `swb done <id>` in point 7's table.
	var rows []string
	for _, l := range strings.Split(doc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "|") {
			rows = append(rows, strings.TrimSpace(l))
		}
	}
	doneRow := -1
	for i, r := range rows {
		if strings.HasPrefix(r, "| `swb done <id>`") {
			doneRow = i
		}
	}
	if doneRow < 0 {
		t.Fatalf("POSITIVE CONTROL: %s's trigger table has no `swb done <id>` row", skillRel)
	}
	if doneRow+1 >= len(rows) || !strings.HasPrefix(rows[doneRow+1], "| `swb reopen <id> [status]` | `task_reopen`") {
		next := ""
		if doneRow+1 < len(rows) {
			next = rows[doneRow+1]
		}
		t.Errorf("the trigger-table row after `swb done <id>` is %q, want the `swb reopen <id> [status]` | `task_reopen` "+
			"row (criterion 13b)", next)
	}
}
