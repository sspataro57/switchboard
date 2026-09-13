package tools_test

// SWT-40 Part B criterion B8's unit half (docs/tickets/inquiry-promote_SPEC.md,
// B-D1, "API / MCP tool changes"): route_candidate_add and
// route_candidate_remove are registered executor tools, and their argument
// checks live in VALIDATE, so a malformed call fails before any handler runs.
// Driven through executor.Execute with a NIL pool: every case here stops at
// Validate. ZERO network, ZERO Postgres. The database-dependent refusals
// (unknown account, unknown project, a second default, an ambiguous account
// email) are in routecandidates_integration_test.go; humanOnly is pinned in
// internal/policy/matrix_routecandidates_test.go and off-MCP in
// internal/mcpserver/route_candidates_test.go.
//
// ---- IMPOSED SURFACE (internal/tools/routecandidates.go) ---------------------
//
//	route_candidate_add    {account_email, project, description, is_default?, provider?}
//	route_candidate_remove {account_email, project, provider?}
//	  account_email: source_accounts.account_email (refused when it names no
//	                 account, or more than one across providers and no
//	                 provider was given)
//	  provider:      optional source_accounts.provider; when given, non-empty
//	                 after trimming (B8 amendment 2026-09-13)
//	  project:       projects.slug
//	  description:   non-empty after trimming (it reaches the prompt, B-D4)
//
// GREENFIELD NOTE — EXPECTED RED: neither tool is registered, so Execute returns
// "unknown tool", which every case rejects as NOT a validation failure. A case
// that passes Validate would panic on the nil pool: that panic is the signal
// that a check landed in the handler instead.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

func TestRegister_RouteCandidateToolsRegistered(t *testing.T) {
	reg := executor.NewRegistry()
	tools.Register(reg, nil)
	got := map[string]bool{}
	for _, n := range reg.Names() {
		got[n] = true
	}
	for _, want := range []string{"route_candidate_add", "route_candidate_remove"} {
		if !got[want] {
			t.Errorf("tool %q is not registered by tools.Register. B-D1: the candidate table is written only through "+
				"these humanOnly executor tools, so 'who authorised routing into this project' is an audit row", want)
		}
	}
}

func TestValidate_RouteCandidateTools_RefuseMalformedArgs(t *testing.T) {
	reg := executor.NewRegistry()
	tools.Register(reg, nil) // nil pool: nothing below reaches a handler
	ex := executor.New(reg, policy.NewStatic(reg.Names()...), audit.NewMemStore())
	ctx := context.Background()
	for _, tc := range []struct {
		tool, args, why string
	}{
		{"route_candidate_add", `{}`, "empty"},
		{"route_candidate_add", `{"project":"collaboratory","description":"d"}`, "no account_email"},
		{"route_candidate_add", `{"account_email":"a@example.test","description":"d"}`, "no project"},
		{"route_candidate_add", `{"account_email":"a@example.test","project":"collaboratory"}`, "no description"},
		{"route_candidate_add", `{"account_email":"a@example.test","project":"collaboratory","description":"  \n\t "}`,
			"a whitespace-only description (B8: an empty description is refused)"},
		{"route_candidate_remove", `{}`, "empty"},
		{"route_candidate_remove", `{"project":"collaboratory"}`, "no account_email"},
		{"route_candidate_remove", `{"account_email":"a@example.test"}`, "no project"},
		// provider (B8 amendment 2026-09-13): optional, but when given it must say
		// something. An empty one would read as "no provider" and fall back to the
		// ambiguous refusal, hiding a typo in the caller.
		{"route_candidate_add", `{"account_email":"a@example.test","project":"collaboratory","description":"d","provider":""}`,
			"an empty provider"},
		{"route_candidate_add", `{"account_email":"a@example.test","project":"collaboratory","description":"d","provider":" \t "}`,
			"a whitespace-only provider"},
		{"route_candidate_remove", `{"account_email":"a@example.test","project":"collaboratory","provider":""}`,
			"an empty provider"},
		{"route_candidate_remove", `{"account_email":"a@example.test","project":"collaboratory","provider":"  "}`,
			"a whitespace-only provider"},
	} {
		t.Run(tc.tool+"/"+tc.why, func(t *testing.T) {
			_, err := ex.Execute(ctx, executor.Call{Tool: tc.tool, Actor: "opsctl:salvo", Args: json.RawMessage(tc.args)})
			if err == nil {
				t.Fatalf("%s %s succeeded; want a validation failure (%s)", tc.tool, tc.args, tc.why)
			}
			if strings.Contains(err.Error(), "unknown tool") {
				t.Fatalf("%s is not registered (%v); that is not a validation failure", tc.tool, err)
			}
		})
	}
}
