package mcpserver_test

// SWT-37 (docs/tickets/mcp-task-verbs_SPEC.md) criterion 8: the schemas and
// descriptions of the three task verbs now in agentTools (V5). ZERO network;
// listedTool is mail_tools_test.go's, sortedCopy is queue_tools_test.go's.
//
// IMPOSED SURFACE (SPEC V5):
//
//	// internal/mcpserver/schemas.go — three agentTools entries
//	task_dismiss         {"task_id": integer, "reason_code": enum, "note": string}, required [task_id, reason_code]
//	task_close           {"task_id": integer, "reason": string},                    required [task_id, reason]
//	task_mark_delivered  {"task_id": integer, "reason": string},                    required [task_id]
//	// internal/tools/close.go
//	func DismissReasonCodes() []string // a COPY of dismissCodes, in order
//
// GREENFIELD NOTE — EXPECTED RED. tools.DismissReasonCodes does not exist, so
// this package's tests compile-FAIL. Once it compiles, listedTool fails with
// `tool "task_dismiss" is not MCP-listed` until schemas.go gains the entries.
//
// Why each schema tells the truth up front: a required field the schema hides
// costs the model a validation-error round trip. task_close's reason is already
// REQUIRED by validateClose (it is the status_changed payload's human trail), so
// the schema says so. The reason_code enum comes from ONE source,
// tools.DismissReasonCodes, which is pinned to migration 0022's CHECK by the
// validator's own tests.

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/tools"
)

type verbSchemaProp struct {
	Type string   `json:"type"`
	Enum []string `json:"enum"`
}

type verbSchema struct {
	Type       string                    `json:"type"`
	Properties map[string]verbSchemaProp `json:"properties"`
	Required   []string                  `json:"required"`
}

func parseVerbSchema(t *testing.T, name string, raw json.RawMessage) verbSchema {
	t.Helper()
	var s verbSchema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("%s InputSchema is not a JSON Schema object: %v (%s)", name, err, raw)
	}
	if s.Type != "object" {
		t.Errorf("%s schema type = %q, want object", name, s.Type)
	}
	return s
}

func assertVerbProps(t *testing.T, name string, s verbSchema, want map[string]string, required []string) {
	t.Helper()
	for prop, typ := range want {
		p, ok := s.Properties[prop]
		if !ok {
			t.Errorf("%s schema has no %q property", name, prop)
			continue
		}
		if p.Type != typ {
			t.Errorf("%s schema %s type = %q, want %q", name, prop, p.Type, typ)
		}
	}
	for prop := range s.Properties {
		if _, ok := want[prop]; !ok {
			t.Errorf("%s schema declares an extra property %q; V5 lists exactly %v", name, prop, want)
		}
	}
	if got, w := strings.Join(sortedCopy(s.Required), ","), strings.Join(sortedCopy(required), ","); got != w {
		t.Errorf("%s schema required = %v, want exactly %v (V5)", name, s.Required, required)
	}
}

func TestTaskVerbSchemas(t *testing.T) {
	t.Run("task_dismiss", func(t *testing.T) {
		tl := listedTool(t, "task_dismiss")
		s := parseVerbSchema(t, "task_dismiss", tl.InputSchema)
		assertVerbProps(t, "task_dismiss", s,
			map[string]string{"task_id": "integer", "reason_code": "string", "note": "string"},
			[]string{"task_id", "reason_code"})
		got := strings.Join(sortedCopy(s.Properties["reason_code"].Enum), ",")
		want := strings.Join(sortedCopy(tools.DismissReasonCodes()), ",")
		if got != want {
			t.Errorf("task_dismiss reason_code enum = %v, want set-equal to tools.DismissReasonCodes() = %v: "+
				"the enum a model reads must be exactly what the validator (and 0022's CHECK) accepts",
				s.Properties["reason_code"].Enum, tools.DismissReasonCodes())
		}
		if len(tools.DismissReasonCodes()) != 4 {
			t.Errorf("tools.DismissReasonCodes() = %v; POSITIVE CONTROL: the four 0022 codes", tools.DismissReasonCodes())
		}
	})

	t.Run("task_close", func(t *testing.T) {
		tl := listedTool(t, "task_close")
		s := parseVerbSchema(t, "task_close", tl.InputSchema)
		assertVerbProps(t, "task_close", s,
			map[string]string{"task_id": "integer", "reason": "string"},
			[]string{"task_id", "reason"})
	})

	t.Run("task_mark_delivered", func(t *testing.T) {
		tl := listedTool(t, "task_mark_delivered")
		s := parseVerbSchema(t, "task_mark_delivered", tl.InputSchema)
		assertVerbProps(t, "task_mark_delivered", s,
			map[string]string{"task_id": "integer", "reason": "string"},
			[]string{"task_id"})
		if !strings.Contains(tl.Description, "done_locally") {
			t.Errorf("task_mark_delivered description does not name done_locally — only a done_locally task "+
				"moves (V5). Description: %q", tl.Description)
		}
	})

	// All three descriptions, case-insensitively.
	for _, name := range []string{"task_dismiss", "task_close", "task_mark_delivered"} {
		name := name
		t.Run(name+"/description", func(t *testing.T) {
			tl := listedTool(t, name)
			d := strings.ToLower(tl.Description)
			for _, want := range []struct{ re, why string }{
				{`human sessions? only|human identities only|human.{0,40}only|worker.{0,80}(refused|denied)|(refused|denied).{0,80}worker`,
					"it is for human sessions only; a worker console is refused by policy (V1/V2)"},
				{`nothing is sent|sends nothing|never sends|does not send|no message is sent`,
					"nothing is sent (V0: the accepted risk rests on it)"},
			} {
				if !regexp.MustCompile(want.re).MatchString(d) {
					t.Errorf("%s description does not match /%s/ — %s. Description: %q", name, want.re, want.why, tl.Description)
				}
			}
			if strings.Contains(d, "worker_id") || strings.Contains(string(tl.InputSchema), "worker_id") {
				t.Errorf("%s mentions worker_id; identity is injected from OPS_WORKER_ID, never model-supplied", name)
			}
		})
	}

	t.Run("cross-references", func(t *testing.T) {
		if d := strings.ToLower(listedTool(t, "task_dismiss").Description); !regexp.MustCompile(`label|training`).MatchString(d) {
			t.Errorf("task_dismiss description mentions neither 'label' nor 'training': a dismissal records why "+
				"as labelled training data, which is what separates it from task_close. Description: %q", d)
		}
		if d := listedTool(t, "task_close").Description; !strings.Contains(d, "task_dismiss") {
			t.Errorf("task_close description does not point at task_dismiss ('if the task should never have "+
				"existed, use task_dismiss'). Description: %q", d)
		}
	})
}
