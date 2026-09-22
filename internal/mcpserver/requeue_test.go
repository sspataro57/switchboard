package mcpserver_test

// activity-resurfaces (SWT-72, docs/tickets/activity-resurfaces_SPEC.md) D6 and
// criterion 23: task_requeue on BOTH MCP profiles — the board's third review
// verb reachable from any Claude Code session as `swb requeue <id>` — with the
// priority bounds and level names taken from tools.PriorityMin / PriorityMax /
// PriorityLevels rather than respelled.
//
// task_set_priority is the sibling this copies: humanOnly, listed in both
// profiles, no pin. Listing it removes the transport allowlist as a refusal for
// worker consoles (which share the full profile), so policy.humanOnly is what
// refuses them — pinned in internal/policy's matrix_requeue_test.go and, for
// the real worker shapes, by TestMCPListing_DoesNotMakeRequeueWorkerCallable
// below.
//
// The counts in adapter_test.go (wantAgentTools) and profile_test.go
// (wantUserProfileTools) are amended BY NAME in the same diff, so a later edit
// fails with the offending name rather than an arithmetic mismatch.
//
// GREENFIELD NOTE, EXPECTED RED: schemas.go has no task_requeue entry and
// adapter.go's userProfileTools does not list it, so every test here fails on
// its own assertion.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - leave task_requeue off agentTools -> ListedOnBothProfiles.
//   - leave it off userProfileTools -> the same (the user half).
//   - list it on ProfileRead -> NotOnTheReadProfile.
//   - respell the priority bounds -> RequeueSchema.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/mcpserver"
	"github.com/sspataro57/switchboard/internal/tools"
)

const requeueName = "task_requeue"

// Criterion 23: ProfileFull and ProfileUser list it; ProfileRead does not.
func TestTaskRequeue_ListedOnBothProfiles(t *testing.T) {
	for _, p := range []struct {
		name    string
		profile mcpserver.Profile
		want    bool
	}{
		{"full", mcpserver.ProfileFull, true},
		{"user", mcpserver.ProfileUser, true},
		{"read", mcpserver.ProfileRead, false},
	} {
		p := p
		t.Run(p.name, func(t *testing.T) {
			listed := false
			for _, tl := range mcpserver.NewWithProfile(&fakeExec{}, testWorkerID, p.profile).ListTools() {
				if tl.Name == requeueName {
					listed = true
				}
			}
			if listed != p.want {
				t.Errorf("the %s profile lists %s = %v, want %v. D6: \"in agentTools (schemas.go) and in "+
					"userProfileTools, so both .mcp.json's server and the user-scope ops-mcp-user list it\"; "+
					"the read profile is queue READS only", p.name, requeueName, listed, p.want)
			}
		})
	}
}

// Criterion 23's schema half, the TestTaskSetPrioritySchema shape: the bounds a
// model reads ARE the scale, spelled once in internal/tools/priority.go.
func TestTaskRequeueSchema(t *testing.T) {
	tl := listedTool(t, requeueName)
	s := parseVerbSchema(t, requeueName, tl.InputSchema)
	assertVerbProps(t, requeueName, s,
		map[string]string{"task_id": "integer", "priority": "integer", "note": "string"},
		[]string{"task_id"}) // priority and note are OPTIONAL (D6)

	var b struct {
		Properties map[string]priorityBoundsProp `json:"properties"`
	}
	if err := json.Unmarshal(tl.InputSchema, &b); err != nil {
		t.Fatalf("%s InputSchema: %v", requeueName, err)
	}
	p := b.Properties["priority"]
	if p.Minimum == nil || *p.Minimum != float64(tools.PriorityMin) {
		t.Errorf("%s priority minimum = %v, want tools.PriorityMin = %d. D6: nothing is below 0, so \"low "+
			"priority\" IS 0 and the verb does not invent −1", requeueName, p.Minimum, tools.PriorityMin)
	}
	if p.Maximum == nil || *p.Maximum != float64(tools.PriorityMax) {
		t.Errorf("%s priority maximum = %v, want tools.PriorityMax = %d", requeueName, p.Maximum, tools.PriorityMax)
	}

	d := strings.ToLower(tl.Description)
	for _, level := range tools.PriorityLevels {
		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(level) + `\b`).MatchString(d) {
			t.Errorf("%s's description does not name the level %q — the model maps Salvador's words to a number "+
				"from here. Description: %q", requeueName, level, tl.Description)
		}
	}
	for _, want := range []struct{ re, why string }{
		{`unchanged|omit`, "D6: an OMITTED priority means unchanged; a dropped argument must never demote a task"},
		{`holding`, "D6 step 3 / OQ-1 = B: the verb lifts holding -> ready and nothing else"},
		{`review|requeue`, "it is the third review outcome beside Dismiss and Done"},
	} {
		if !regexp.MustCompile(want.re).MatchString(d) {
			t.Errorf("%s's description does not match /%s/ — %s. Description: %q", requeueName, want.re, want.why,
				tl.Description)
		}
	}
	if strings.Contains(string(tl.InputSchema), "worker_id") {
		t.Errorf("%s's schema mentions worker_id; identity is injected from OPS_WORKER_ID, never model-supplied",
			requeueName)
	}
}

// D6's split, at the TRANSPORT: listing the verb does not make it callable by a
// worker console. The policy matrix is the gate (internal/policy criterion 22);
// this pins the real worker shapes end to end through the adapter, the
// TestMCPListing_DoesNotMakeSetPriorityWorkerCallable shape.
func TestMCPListing_DoesNotMakeRequeueWorkerCallable(t *testing.T) {
	listed := false
	for _, tl := range mcpserver.New(&fakeExec{}, testWorkerID).ListTools() {
		if tl.Name == requeueName {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("%s is not in the full profile's tools/list; the rest of this test is vacuous", requeueName)
	}
	// The actor the adapter forwards for a worker console must NOT be a human
	// shape: that is what policy.HumanActor refuses.
	for _, worker := range []string{"treetop", "treetop.main", "worker:treetop"} {
		worker := worker
		t.Run(worker, func(t *testing.T) {
			fx := &fakeExec{}
			srv := mcpserver.NewWithProfile(fx, worker, mcpserver.ProfileFull)
			if _, err := srv.CallTool(context.Background(), requeueName, json.RawMessage(`{"task_id":452}`)); err != nil {
				// A transport refusal is acceptable too; what must not happen is
				// the call reaching the executor under a HUMAN actor.
				return
			}
			if fx.lastCall.Actor == "" {
				t.Fatalf("the adapter forwarded %s with no actor", requeueName)
			}
			if !strings.HasPrefix(fx.lastCall.Actor, "mcp:") {
				t.Errorf("a worker console's %s was forwarded as actor %q; the adapter's shape is mcp:{client}",
					requeueName, fx.lastCall.Actor)
			}
			for _, human := range []string{"mcp:manual:", "mcp:dashboard:", "mcp:opsctl:"} {
				if strings.HasPrefix(fx.lastCall.Actor, human) {
					t.Errorf("a worker console's %s was forwarded as %q, a HUMAN actor shape. D6: humanOnly is "+
						"the only thing standing between a worker and a verb that lifts a status and raises a "+
						"priority", requeueName, fx.lastCall.Actor)
				}
			}
		})
	}
}

// Criterion 23's words: the server's Instructions and the skill's trigger table
// both carry `swb requeue <id>`, so a session knows the verb exists without
// reading the schema (the `swb prioritize` precedent, pinned in runbook_test.go
// and skill_test.go).
func rqRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v — the file this criterion is about does not exist", rel, err)
	}
	return string(b)
}

func TestServeInstructions_CarryTheRequeueLine(t *testing.T) {
	src := rqRepoFile(t, "internal/mcpserver/serve.go")
	if !regexp.MustCompile(`swb requeue <id>`).MatchString(src) {
		t.Errorf("internal/mcpserver/serve.go's Instructions carry no `swb requeue <id>` line (criterion 23)")
	}
	if !regexp.MustCompile(`(?s)swb requeue <id>.{0,200}task_requeue`).MatchString(src) {
		t.Errorf("the Instructions' `swb requeue` line does not name task_requeue (criterion 23)")
	}
	skill := rqRepoFile(t, "skills/swb-status/SKILL.md")
	if !regexp.MustCompile(`(?s)swb requeue <id>.{0,120}task_requeue`).MatchString(skill) {
		t.Errorf("skills/swb-status/SKILL.md's trigger table has no `swb requeue <id>` row naming task_requeue " +
			"(criterion 23). A stale skill means the session never offers the verb")
	}
}
