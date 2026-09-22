package mcpserver_test

// comms-inbox (SWT-74, docs/tickets/comms-inbox_SPEC.md) criteria 27 and 36:
// task_match and task_attach on BOTH MCP profiles — "Routing is one verb and
// one read", reachable from any Claude Code session as `swb match <id>` and
// `swb attach <id> <target>` — and named in the server's Instructions and the
// skill's trigger table so a session knows the verbs exist without reading a
// schema.
//
// task_requeue is the sibling this copies for task_attach (humanOnly, listed in
// both profiles, no pin); task_list is the sibling for task_match (read-only,
// not humanOnly, both profiles).
//
// The counts in serve_test.go (ProfileFull / ProfileUser) and the name lists in
// adapter_test.go (wantAgentTools) and profile_test.go (wantUserProfileTools)
// are amended BY NAME in the same diff, so a later edit fails with the
// offending name rather than an arithmetic mismatch.
//
// ---- IMPOSED SURFACE (SPEC's "API / MCP tool changes" table) ------------------
//
//	// internal/mcpserver/schemas.go — agentTools gains both entries:
//	task_match  {message_id? | task_id? | text?, project?, limit?}  (nothing required)
//	task_attach {task_id, target_task_id, note?}                    (the first two required)
//	// internal/mcpserver/adapter.go — userProfileTools gains both NAMES, each a
//	// SLICE of agentTools (never a second spelling of a schema).
//	// internal/mcpserver/serve.go Instructions and skills/swb-status/SKILL.md
//	// gain a `swb match <id>` row and a `swb attach <id> <target>` row.
//
// GREENFIELD NOTE, EXPECTED RED: schemas.go has no entry for either tool and
// adapter.go's userProfileTools lists neither, so every test here fails on its
// own assertion.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - leave either tool off agentTools -> ListedOnBothProfiles.
//   - leave either off userProfileTools -> the same (the user half).
//   - list either on ProfileRead -> the same (the read profile is queue READS only).
//   - remove task_attach from humanOnly -> ListingDoesNotMakeAttachWorkerCallable
//     (and internal/policy's criterion 35).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/mcpserver"
)

const (
	matchName  = "task_match"
	attachName = "task_attach"
)

// Criterion 27 / 36: ProfileFull and ProfileUser list both; ProfileRead lists
// neither.
func TestCommTools_ListedOnBothProfiles(t *testing.T) {
	for _, name := range []string{matchName, attachName} {
		for _, p := range []struct {
			label   string
			profile mcpserver.Profile
			want    bool
		}{
			{"full", mcpserver.ProfileFull, true},
			{"user", mcpserver.ProfileUser, true},
			{"read", mcpserver.ProfileRead, false},
		} {
			name, p := name, p
			t.Run(name+"/"+p.label, func(t *testing.T) {
				listed := false
				for _, tl := range mcpserver.NewWithProfile(&fakeExec{}, testWorkerID, p.profile).ListTools() {
					if tl.Name == name {
						listed = true
					}
				}
				if listed != p.want {
					t.Errorf("the %s profile lists %s = %v, want %v. Criteria 27/36: both go in agentTools "+
						"(schemas.go) and in userProfileTools, so `swb match <id>` and `swb attach <id> "+
						"<target>` work from ANY repo's session — which is where he reads INCOMING from; "+
						"the read profile is queue READS only", p.label, name, listed, p.want)
				}
			})
		}
	}
}

// The user profile's entries are the full profile's, byte for byte: a slice of
// agentTools, never a second spelling of a schema.
func TestCommTools_UserProfileEntriesAreTheFullProfilesEntries(t *testing.T) {
	full := map[string]mcpserver.Tool{}
	for _, tl := range mcpserver.New(&fakeExec{}, testWorkerID).ListTools() {
		full[tl.Name] = tl
	}
	for _, tl := range mcpserver.NewWithProfile(&fakeExec{}, testWorkerID, mcpserver.ProfileUser).ListTools() {
		if tl.Name != matchName && tl.Name != attachName {
			continue
		}
		f, ok := full[tl.Name]
		if !ok {
			t.Errorf("user-profile tool %q is not in the full profile at all", tl.Name)
			continue
		}
		if f.Description != tl.Description || string(f.InputSchema) != string(tl.InputSchema) {
			t.Errorf("user-profile %q differs from the full profile's entry; the user profile must be a SLICE "+
				"of agentTools", tl.Name)
		}
	}
}

// Criterion 22's schema half, read from the LISTED tool: exactly one of the
// three inputs, nothing required (the validator enforces "exactly one", which a
// JSON schema cannot express), limit bounded 1..20.
func TestTaskMatchSchema(t *testing.T) {
	tl := listedTool(t, matchName)
	s := parseVerbSchema(t, matchName, tl.InputSchema)
	assertVerbProps(t, matchName, s,
		map[string]string{"message_id": "integer", "task_id": "integer", "text": "string",
			"project": "string", "limit": "integer"},
		nil) // nothing is required: EXACTLY ONE of three cannot be spelled in the schema

	d := strings.ToLower(tl.Description)
	for _, want := range []struct{ re, why string }{
		{`exactly one|one of`, "D6: exactly one of message_id, task_id, text — the model has to be told"},
		{`match|route|belong`, "the verb answers \"which task does this belong to\""},
		{`rule`, "D6: the answer names the RULE that matched and why"},
	} {
		if !regexp.MustCompile(want.re).MatchString(d) {
			t.Errorf("%s's description does not match /%s/ — %s. Description: %q", matchName, want.re, want.why,
				tl.Description)
		}
	}
	if strings.Contains(string(tl.InputSchema), "worker_id") {
		t.Errorf("%s's schema mentions worker_id; identity is injected from OPS_WORKER_ID, never model-supplied",
			matchName)
	}
}

func TestTaskAttachSchema(t *testing.T) {
	tl := listedTool(t, attachName)
	s := parseVerbSchema(t, attachName, tl.InputSchema)
	assertVerbProps(t, attachName, s,
		map[string]string{"task_id": "integer", "target_task_id": "integer", "note": "string"},
		[]string{"task_id", "target_task_id"}) // note is OPTIONAL (D7)

	d := strings.ToLower(tl.Description)
	for _, want := range []struct{ re, why string }{
		{`close`, "D7: the verb routes the task onto the target AND CLOSES it — a session has to know that"},
		{`target`, "D7: the destination is target_task_id"},
	} {
		if !regexp.MustCompile(want.re).MatchString(d) {
			t.Errorf("%s's description does not match /%s/ — %s. Description: %q", attachName, want.re, want.why,
				tl.Description)
		}
	}
	if strings.Contains(string(tl.InputSchema), "worker_id") {
		t.Errorf("%s's schema mentions worker_id; identity is injected from OPS_WORKER_ID", attachName)
	}
}

// D7's split, at the TRANSPORT: listing the verb does not make it callable by a
// worker console. The policy matrix is the gate (internal/policy criterion 35);
// this pins the real worker shapes end to end through the adapter, the
// TestMCPListing_DoesNotMakeRequeueWorkerCallable shape.
func TestMCPListing_DoesNotMakeAttachWorkerCallable(t *testing.T) {
	listed := false
	for _, tl := range mcpserver.New(&fakeExec{}, testWorkerID).ListTools() {
		if tl.Name == attachName {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("%s is not in the full profile's tools/list; the rest of this test is vacuous", attachName)
	}
	for _, worker := range []string{"treetop", "treetop.main", "worker:treetop"} {
		worker := worker
		t.Run(worker, func(t *testing.T) {
			fx := &fakeExec{}
			srv := mcpserver.NewWithProfile(fx, worker, mcpserver.ProfileFull)
			if _, err := srv.CallTool(context.Background(), attachName,
				json.RawMessage(`{"task_id":481,"target_task_id":452}`)); err != nil {
				return // a transport refusal is acceptable too
			}
			if fx.lastCall.Actor == "" {
				t.Fatalf("the adapter forwarded %s with no actor", attachName)
			}
			for _, human := range []string{"mcp:manual:", "mcp:dashboard:", "mcp:opsctl:"} {
				if strings.HasPrefix(fx.lastCall.Actor, human) {
					t.Errorf("a worker console's %s was forwarded as %q, a HUMAN actor shape. D7: humanOnly is "+
						"the only thing standing between a worker console and a verb that CLOSES a task",
						attachName, fx.lastCall.Actor)
				}
			}
		})
	}
}

// Criterion 36's words: the server's Instructions and the skill's trigger table
// both carry the two rows (the `swb requeue <id>` precedent).
func ctRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v — the file this criterion is about does not exist", rel, err)
	}
	return string(b)
}

func TestServeInstructionsAndSkill_CarryTheMatchAndAttachLines(t *testing.T) {
	src := ctRepoFile(t, "internal/mcpserver/serve.go")
	skill := ctRepoFile(t, "skills/swb-status/SKILL.md")
	for _, tc := range []struct{ phrase, tool string }{
		{`swb match <id>`, matchName},
		{`swb attach <id> <target>`, attachName},
	} {
		tc := tc
		t.Run(tc.tool, func(t *testing.T) {
			if !strings.Contains(src, tc.phrase) {
				t.Errorf("internal/mcpserver/serve.go's Instructions carry no `%s` line (criterion 36). "+
					"Without it no session learns the verb exists, and the whole \"usable alone\" claim rests "+
					"on `swb match <id>` answering from any Claude Code session", tc.phrase)
			}
			if !regexp.MustCompile(`(?s)` + regexp.QuoteMeta(tc.phrase) + `.{0,200}` + tc.tool).MatchString(src) {
				t.Errorf("the Instructions' `%s` line does not name %s (criterion 36)", tc.phrase, tc.tool)
			}
			if !regexp.MustCompile(`(?s)` + regexp.QuoteMeta(tc.phrase) + `.{0,160}` + tc.tool).MatchString(skill) {
				t.Errorf("skills/swb-status/SKILL.md's trigger table has no `%s` row naming %s (criterion 36). "+
					"A stale skill means the session never offers the verb", tc.phrase, tc.tool)
			}
		})
	}
}
