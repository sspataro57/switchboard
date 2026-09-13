package main

// B8 amendment 2026-09-13: `opsctl route-candidates add|remove --provider X`
// passes `provider` to the tool, so an address that exists under several
// providers (prod: salvador@handsonconnect.org is google, jira and jira_lookup)
// can be targeted. Absent flag → no provider key (the tool's single-match rule
// applies); an explicitly empty flag is refused rather than silently dropped.
// ZERO network: the parse functions only build the call.

import (
	"encoding/json"
	"testing"
)

func TestOpsctl_RouteCandidates_ProviderFlag(t *testing.T) {
	type parser func([]string) (string, json.RawMessage, error)
	for _, tc := range []struct {
		name  string
		parse parser
		tool  string
		base  []string
	}{
		{"add", parseRouteCandidateAdd, "route_candidate_add",
			[]string{"--account", "salvador@handsonconnect.org", "--project", "collaboratory", "--description", "d"}},
		{"remove", parseRouteCandidateRemove, "route_candidate_remove",
			[]string{"--account", "salvador@handsonconnect.org", "--project", "collaboratory"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decode := func(raw json.RawMessage) map[string]any {
				t.Helper()
				var m map[string]any
				if err := json.Unmarshal(raw, &m); err != nil {
					t.Fatalf("args are not a JSON object: %v (%s)", err, raw)
				}
				return m
			}

			tool, raw, err := tc.parse(append(append([]string{}, tc.base...), "--provider", "google"))
			if err != nil {
				t.Fatalf("--provider google: %v", err)
			}
			if tool != tc.tool {
				t.Errorf("tool = %q, want %q", tool, tc.tool)
			}
			if got := decode(raw)["provider"]; got != "google" {
				t.Errorf("--provider google built provider=%v, want \"google\" (args %s)", got, raw)
			}

			_, raw, err = tc.parse(tc.base)
			if err != nil {
				t.Fatalf("no --provider: %v", err)
			}
			if _, ok := decode(raw)["provider"]; ok {
				t.Errorf("no --provider still sent a provider key: %s", raw)
			}

			for _, empty := range []string{"", "  "} {
				if _, raw, err := tc.parse(append(append([]string{}, tc.base...), "--provider", empty)); err == nil {
					t.Errorf("--provider %q accepted (args %s); an explicit empty provider is refused, not dropped", empty, raw)
				}
			}
		})
	}
}
