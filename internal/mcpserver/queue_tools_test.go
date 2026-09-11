package mcpserver_test

// SWT-35 (docs/tickets/task-list-mcp_SPEC.md) criteria 19 and 20: task_list and
// project_list on the MCP surface. ZERO network: the adapter is driven with
// adapter_test.go's fakeExec; listedTool comes from mail_tools_test.go.
//
// The primary consumer is Salvador's own interactive session in each project
// repo, through the user-scope install (L13); worker consoles get the same two
// tools through the one allowlist. The adapter stays SQL-free and adds nothing
// but worker_id (L2, L2a): no client scope, no inferred project.
//
// GREENFIELD NOTE — EXPECTED RED. agentTools carries neither tool, so
// listedTool fails with `tool "task_list" is not MCP-listed` and CallTool
// returns `tool "task_list" is not available over MCP`. adapter_test.go's
// wantAgentTools gains both in the same change (criterion 18), so
// TestListTools_ExactlyAgentAllowlist is red with them missing.

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/mcpserver"
)

// The tasks.status CHECK (migrations/0001_initial.sql), hand-copied: the schema
// enum is what a model reads, so it must offer exactly the values the validator
// accepts (internal/tools taskStatuses, pinned to the CHECK by criterion 14).
var queueTwelveStatuses = []string{
	"holding", "ready", "claimed", "in_progress", "needs_feedback",
	"pr_open", "awaiting_ci", "awaiting_merge", "done_locally",
	"delivered", "closed", "blocked",
}

type queueSchemaProp struct {
	Type string   `json:"type"`
	Enum []string `json:"enum"`
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func TestListTools_IncludesQueueReadTools(t *testing.T) {
	t.Run("task_list", func(t *testing.T) {
		tl := listedTool(t, "task_list")
		if !json.Valid(tl.InputSchema) {
			t.Fatalf("task_list InputSchema is not valid JSON: %s", tl.InputSchema)
		}
		if strings.Contains(string(tl.InputSchema), "worker_id") {
			t.Errorf("task_list schema mentions worker_id; identity is injected from OPS_WORKER_ID, never model-supplied")
		}
		var s struct {
			Type       string                     `json:"type"`
			Properties map[string]queueSchemaProp `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if err := json.Unmarshal(tl.InputSchema, &s); err != nil {
			t.Fatalf("task_list InputSchema is not a JSON Schema object: %v (%s)", err, tl.InputSchema)
		}
		if s.Type != "object" {
			t.Errorf("task_list schema type = %q, want object", s.Type)
		}
		if len(s.Required) != 1 || s.Required[0] != "project" {
			t.Errorf("task_list schema required = %v, want exactly [project]. L2: project is the ONLY required "+
				"field, supplied from the caller's own memory; the server infers nothing", s.Required)
		}
		wantProps := map[string]string{
			"project": "string", "status": "string", "assignee_type": "string",
			"subproject": "string", "limit": "integer",
		}
		for name, typ := range wantProps {
			p, ok := s.Properties[name]
			if !ok {
				t.Errorf("task_list schema has no %q property: %s", name, tl.InputSchema)
				continue
			}
			if p.Type != typ {
				t.Errorf("task_list schema %s type = %q, want %q", name, p.Type, typ)
			}
		}
		for name := range s.Properties {
			if _, ok := wantProps[name]; !ok {
				t.Errorf("task_list schema declares an extra property %q. L15: every field must earn its "+
					"tokens; multi-status, title search and worker_type are explicitly out of scope", name)
			}
		}
		if got, want := sortedCopy(s.Properties["status"].Enum), sortedCopy(queueTwelveStatuses); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("task_list status enum = %v, want exactly the twelve CHECK values %v (L5: ONE value from "+
				"the list pinned to the CHECK)", s.Properties["status"].Enum, queueTwelveStatuses)
		}
		if got := sortedCopy(s.Properties["assignee_type"].Enum); strings.Join(got, ",") != "claude,human" {
			t.Errorf("task_list assignee_type enum = %v, want [human claude]", s.Properties["assignee_type"].Enum)
		}

		d := strings.ToLower(tl.Description)
		for _, want := range []struct{ re, why string }{
			{`read-only`, "it changes nothing (L0)"},
			{`does not claim|doesn't claim|never claims`, "listing is not taking: L14's 'does not claim'"},
			{`task_get_next`, "L14: 'a worker takes work only via task_get_next' — the prompt rule, restated where a worker reads it"},
			{`closed`, "closed tasks are hidden by default (L4)"},
			{`delivered`, "…AND delivered ones, which the board does NOT hide — the divergence a caller must be told about (Q2)"},
			{`hid(e|es|den)|exclud|omit`, "that the default hides them"},
			{`status`, "and that an explicit status returns them"},
			{`slug`, "project is the caller's slug"},
			{`project_list`, "…which project_list lists (L10's error names it too)"},
			{`\bswb\b`, "Salvador's shorthand: 'swb queue' must resolve to this tool"},
		} {
			if !regexp.MustCompile(want.re).MatchString(d) {
				t.Errorf("task_list description does not match /%s/ — %s. Description: %q", want.re, want.why, tl.Description)
			}
		}
	})

	t.Run("project_list", func(t *testing.T) {
		tl := listedTool(t, "project_list")
		if !json.Valid(tl.InputSchema) {
			t.Fatalf("project_list InputSchema is not valid JSON: %s", tl.InputSchema)
		}
		if strings.Contains(string(tl.InputSchema), "worker_id") {
			t.Errorf("project_list schema mentions worker_id; identity is never model-supplied")
		}
		raw := map[string]json.RawMessage{}
		if err := json.Unmarshal(tl.InputSchema, &raw); err != nil {
			t.Fatalf("project_list InputSchema is not an object: %v", err)
		}
		if _, ok := raw["required"]; ok {
			t.Errorf("project_list schema declares required %s; L11: it takes NO arguments", raw["required"])
		}
		if p, ok := raw["properties"]; ok {
			props := map[string]json.RawMessage{}
			if err := json.Unmarshal(p, &props); err != nil || len(props) != 0 {
				t.Errorf("project_list schema declares properties %s; L11: it takes NO arguments", p)
			}
		}
		d := strings.ToLower(tl.Description)
		for _, want := range []struct{ re, why string }{
			{`slug`, "it lists project slugs"},
			{`memori[sz]|remember`, "…so a session can confirm one before memorising it (L0)"},
			{`confirm`, "the confirmation is the tool's reason to exist"},
			{`\bswb\b`, "Salvador's shorthand: 'list swb projects' must resolve to this tool"},
		} {
			if !regexp.MustCompile(want.re).MatchString(d) {
				t.Errorf("project_list description does not match /%s/ — %s. Description: %q", want.re, want.why, tl.Description)
			}
		}
	})
}

// The server instructions (sent at initialize, placed in the session's system
// prompt by Claude Code) teach the "swb" shorthand: "list swb projects" is
// project_list and "swb queue" is task_list, in every repo's session.
func TestInstructions_TeachTheSwbShorthand(t *testing.T) {
	d := strings.ToLower(mcpserver.Instructions)
	for _, want := range []struct{ re, why string }{
		{`"swb"`, "names the alias"},
		{`list swb projects.{0,40}project_list`, "'list swb projects' → project_list"},
		{`swb queue.{0,80}task_list`, "'swb queue' → task_list"},
		{`memori[sz]ed`, "…with the repo's memorised slug"},
	} {
		if !regexp.MustCompile(want.re).MatchString(d) {
			t.Errorf("mcpserver.Instructions does not match /%s/ — %s. Instructions: %q", want.re, want.why, mcpserver.Instructions)
		}
	}
}

// forwardedKeys returns the sorted top-level keys of the args the adapter
// forwarded.
func forwardedKeys(t *testing.T, args json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(args, &m); err != nil {
		t.Fatalf("forwarded args %s are not an object: %v", args, err)
	}
	return m
}

func keyList(m map[string]json.RawMessage) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

// Criterion 20: the adapter forwards task_list with Actor "mcp:"+id, injects
// worker_id, passes the caller's project UNALTERED, and adds nothing else — no
// client scope (L2a, Q1 = a), no inferred project (L2).
func TestCallTool_TaskListForwardsWithMCPActor(t *testing.T) {
	t.Run("interactive session", func(t *testing.T) {
		fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{"project":"collaboratory","tasks":[]}`)}}
		srv := mcpserver.New(fx, testWorkerID)

		out, err := srv.CallTool(context.Background(), "task_list", json.RawMessage(`{"project":"collaboratory"}`))
		if err != nil {
			t.Fatalf("CallTool(task_list): %v", err)
		}
		if !fx.called {
			t.Fatal("task_list never reached the executor (invariant 3: the adapter is SQL-free)")
		}
		if fx.lastCall.Tool != "task_list" {
			t.Errorf("forwarded Tool = %q, want task_list", fx.lastCall.Tool)
		}
		if want := "mcp:" + testWorkerID; fx.lastCall.Actor != want {
			t.Errorf("forwarded Actor = %q, want %q", fx.lastCall.Actor, want)
		}
		args := forwardedKeys(t, fx.lastCall.Args)
		if got := keyList(args); got != "project,worker_id" {
			t.Errorf("forwarded args keys = %s, want exactly project,worker_id — the adapter adds worker_id "+
				"and NOTHING else (L2, L2a). Args: %s", got, fx.lastCall.Args)
		}
		if string(args["project"]) != `"collaboratory"` {
			t.Errorf("forwarded project = %s, want \"collaboratory\" unaltered", args["project"])
		}
		if string(args["worker_id"]) != `"`+testWorkerID+`"` {
			t.Errorf("forwarded worker_id = %s, want %q", args["worker_id"], testWorkerID)
		}
		if string(out) != `{"project":"collaboratory","tasks":[]}` {
			t.Errorf("CallTool output = %s, want the executor result verbatim", out)
		}
	})

	t.Run("worker console names another client's project", func(t *testing.T) {
		// A worker's real identity is the bare client (SPEC fact 3). Q1 = a: it
		// may read ANY project it names — the adapter neither refuses nor
		// rewrites, exactly as task_get_next and task_context today.
		fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{}`)}}
		srv := mcpserver.New(fx, "acme")
		if _, err := srv.CallTool(context.Background(), "task_list",
			json.RawMessage(`{"project":"personal","status":"ready","worker_id":"victim"}`)); err != nil {
			t.Fatalf("CallTool(task_list) as a worker: %v", err)
		}
		if fx.lastCall.Actor != "mcp:acme" {
			t.Errorf("forwarded Actor = %q, want mcp:acme", fx.lastCall.Actor)
		}
		args := forwardedKeys(t, fx.lastCall.Args)
		if got := keyList(args); got != "project,status,worker_id" {
			t.Errorf("forwarded args keys = %s, want project,status,worker_id (nothing added). Args: %s", got, fx.lastCall.Args)
		}
		if string(args["project"]) != `"personal"` {
			t.Errorf("forwarded project = %s, want \"personal\" unaltered — no client scoping (L2a)", args["project"])
		}
		if string(args["worker_id"]) != `"acme"` {
			t.Errorf("forwarded worker_id = %s, want \"acme\" (a model-supplied worker_id is overwritten)", args["worker_id"])
		}
	})

	t.Run("project_list", func(t *testing.T) {
		for _, in := range []json.RawMessage{json.RawMessage(`{}`), nil} {
			fx := &fakeExec{result: executor.Result{Output: json.RawMessage(`{"projects":[]}`)}}
			srv := mcpserver.New(fx, testWorkerID)
			if _, err := srv.CallTool(context.Background(), "project_list", in); err != nil {
				t.Fatalf("CallTool(project_list, %q): %v", in, err)
			}
			if fx.lastCall.Tool != "project_list" || fx.lastCall.Actor != "mcp:"+testWorkerID {
				t.Errorf("forwarded %q as %q, want project_list as mcp:%s", fx.lastCall.Tool, fx.lastCall.Actor, testWorkerID)
			}
			if got := keyList(forwardedKeys(t, fx.lastCall.Args)); got != "worker_id" {
				t.Errorf("forwarded project_list args keys = %s, want exactly worker_id", got)
			}
		}
	})
}
