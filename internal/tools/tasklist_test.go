package tools

// task_list / project_list — SWT-35 (docs/tickets/task-list-mcp_SPEC.md)
// acceptance criteria 3, 4 and 7, plus L9's clamp. ZERO network, ZERO Postgres.
//
// SPEC CHANGE APPLIED 2026-09-10 (Salvador, relayed by the coordinator):
// task_list does NOT refuse ai_locality='local_only' projects, and project_list
// rows carry NO `local_only` key at all — exactly {slug,name,in_play}, plus
// `client` when set. Criterion 7's project-row half below encodes that.
//
// WHY `package tools` AND NOT `package tools_test`: reopen_test.go's reason,
// unchanged. Driving Execute with a nil pool can only assert REFUSALS — an
// accepted call runs the handler and derefs the nil pool — and most of
// criterion 3 is that values are ACCEPTED (all twelve statuses, limit 0 and
// 10,000, an injected worker_id). And criterion 7 marshals the row, filter and
// project-row TYPES directly, which are unexported. The registration half (that
// `task_list` reaches Validate at all rather than returning "unknown tool")
// stays in tools_unit_test.go, where allToolNames and toolsUnderTest already
// enumerate the registry.
//
// GREENFIELD NOTE — EXPECTED RED. internal/tools/tasklist.go does not exist, so
// this file compile-FAILS internal/tools (undefined: validateTaskList, …) until
// it lands. That is the intended red state for every unit test in the package.
//
// IMPOSED SURFACE. The SPEC fixes the JSON contract, the file
// (internal/tools/tasklist.go), the name `taskStatuses`, and "the row, filter
// and project-row types". The exact Go spellings below are this file's (and
// tasklist_structure_test.go's), chosen as the smallest thing the criteria can
// be tested through; match them or amend the tests deliberately:
//
//	// internal/tools/getnext.go — L7, ONE spelling of the queue order, used by
//	// getNext AND taskList (a leading "ORDER BY" is optional).
//	const taskQueueOrder = `t.priority DESC, t.plan_order ASC NULLS LAST, t.created_at ASC, t.id ASC`
//
//	// internal/tools/tasklist.go
//	// L4 — the in_play predicate, spelled ONCE, used by taskList AND projectList.
//	const inPlayPredicate = `t.status NOT IN ('closed','delivered')`
//
//	// L5 — the twelve values of the tasks.status CHECK (0001), validated against.
//	var taskStatuses = []string{"holding", "ready", "claimed", "in_progress",
//	    "needs_feedback", "pr_open", "awaiting_ci", "awaiting_merge",
//	    "done_locally", "delivered", "closed", "blocked"}
//
//	type taskListArgs struct {
//	    Project      string `json:"project"`
//	    Status       string `json:"status,omitempty"`
//	    AssigneeType string `json:"assignee_type,omitempty"`
//	    Subproject   string `json:"subproject,omitempty"`
//	    Limit        int    `json:"limit,omitempty"`
//	}
//	func validateTaskList(args []byte) error
//	func validateProjectList(args []byte) error // accepts {}, an injected worker_id, and an EMPTY body
//
//	// L8/L9 — the echoed filter. status is always present ("in_play" when not
//	// given), limit always present (the APPLIED value: default 25, clamped to
//	// 200), assignee_type/subproject only when given.
//	type taskListFilter struct {
//	    Status       string `json:"status"`
//	    AssigneeType string `json:"assignee_type,omitempty"`
//	    Subproject   string `json:"subproject,omitempty"`
//	    Limit        int    `json:"limit"`
//	}
//	func newTaskListFilter(a taskListArgs) taskListFilter
//
//	// L6 — a compact row. subproject / parent_id OMITTED (not null) when NULL.
//	type taskListRow struct {
//	    ID           int64   `json:"id"`
//	    Title        string  `json:"title"`
//	    Status       string  `json:"status"`
//	    Priority     int     `json:"priority"`
//	    AssigneeType string  `json:"assignee_type"`
//	    Subproject   *string `json:"subproject,omitempty"`
//	    ParentID     *int64  `json:"parent_id,omitempty"`
//	}
//
//	// L11 (as amended 2026-09-10) — a directory row. client only when set.
//	// There is NO local_only field: nothing refuses a local_only project any
//	// more, so the flag carries no information a caller can act on.
//	type projectListRow struct {
//	    Slug   string  `json:"slug"`
//	    Name   string  `json:"name"`
//	    Client *string `json:"client,omitempty"`
//	    InPlay int     `json:"in_play"`
//	}
//
//	// Handlers, registered in createtask.go's Register as
//	// {"task_list", validateTaskList, taskList} and
//	// {"project_list", validateProjectList, projectList}.
//	func taskList(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error)
//	func projectList(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error)

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// theTwelveStatuses is the tasks.status CHECK from migrations/0001_initial.sql,
// copied by hand ON PURPOSE: it is the independent side of the comparison.
// Criterion 14's integration test compares taskStatuses with the live CHECK via
// pg_get_constraintdef; this list only has to be right on the day it was
// written (2026-09-10; the CHECK has never been altered since 0001).
var theTwelveStatuses = []string{
	"holding", "ready", "claimed", "in_progress", "needs_feedback",
	"pr_open", "awaiting_ci", "awaiting_merge", "done_locally",
	"delivered", "closed", "blocked",
}

// ---- criterion 3: validateTaskList ------------------------------------------

func TestValidateTaskList(t *testing.T) {
	type tc struct {
		name    string
		args    string
		wantErr bool
		wantIn  string // substring the error must contain (only when wantErr)
		why     string
	}
	cases := []tc{
		{"empty object", `{}`, true, "missing project",
			"L2: project is REQUIRED and the server infers nothing — not from OPS_WORKER_ID, not for any actor"},
		{"blank project", `{"project":"  "}`, true, "project",
			"a whitespace slug is a missing slug; resolving it would fail later with a less useful message"},
		{"typo'd status", `{"project":"x","status":"redy"}`, true, "redy",
			"L5: an unknown status is a validation error NAMING THE VALUE, never an empty list — " +
				"`status=redy` must not read as 'nothing ready'"},
		{"unknown assignee_type", `{"project":"x","assignee_type":"robot"}`, true, "",
			"assignee_type is the tasks CHECK's human|claude, nothing else"},
		{"negative limit", `{"project":"x","limit":-1}`, true, "",
			"L9: negative is REFUSED (mail_search's shape); only the large end is clamped"},

		{"assignee human", `{"project":"x","assignee_type":"human"}`, false, "", "human is valid"},
		{"assignee claude", `{"project":"x","assignee_type":"claude"}`, false, "", "claude is valid"},
		{"limit zero", `{"project":"x","limit":0}`, false, "",
			"L9: 0 means 'default', applied at handle time"},
		{"limit huge", `{"project":"x","limit":10000}`, false, "",
			"L9: clamped to 200 at handle time, not refused"},
		{"injected worker_id", `{"project":"x","worker_id":"mcp-injected"}`, false, "",
			"fact 9: every MCP call's args carry an injected worker_id; a DisallowUnknownFields " +
				"decoder would break every MCP call to this tool"},
		{"all filters at once", `{"project":"x","status":"ready","assignee_type":"claude","subproject":"main","limit":25}`,
			false, "", "the SPEC's own request example"},
	}
	// Every one of the twelve CHECK statuses is accepted — INCLUDING closed and
	// delivered: the in_play default hides them, but an explicit status still
	// returns exactly that set (L4).
	for _, s := range theTwelveStatuses {
		cases = append(cases, tc{"status " + s, `{"project":"x","status":"` + s + `"}`, false, "",
			"L4/L5: every CHECK status is a legal explicit filter"})
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			err := validateTaskList([]byte(c.args))
			if !c.wantErr {
				if err != nil {
					t.Fatalf("validateTaskList(%s) = %v, want nil — %s", c.args, err, c.why)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateTaskList(%s) = nil, want an error — %s", c.args, c.why)
			}
			if c.wantIn != "" && !strings.Contains(err.Error(), c.wantIn) {
				t.Errorf("validateTaskList(%s) = %q, want it to contain %q — %s", c.args, err, c.wantIn, c.why)
			}
		})
	}
}

// taskStatuses is the list the validator reads. Pinned here against the
// hand-copied CHECK so a unit run catches a dropped value without a db; the
// integration suite (criterion 14) is the authority.
func TestTaskStatuses_AreTheTwelveOfTheCheck(t *testing.T) {
	got := append([]string(nil), taskStatuses...)
	want := append([]string(nil), theTwelveStatuses...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("taskStatuses = %v, want exactly the twelve CHECK values %v (L5)", taskStatuses, theTwelveStatuses)
	}
}

// ---- criterion 4: validateProjectList ---------------------------------------

// project_list is the FIRST registered tool with no required field (fact 11),
// which is why tools_unit_test.go's "empty args are illegal for every tool"
// comment is amended to name it.
func TestValidateProjectList(t *testing.T) {
	for _, ok := range []struct{ name, args string }{
		{"empty object", `{}`},
		{"injected worker_id", `{"worker_id":"manual:salvo"}`},
		{"empty body", ``},
	} {
		if err := validateProjectList([]byte(ok.args)); err != nil {
			t.Errorf("validateProjectList(%s: %q) = %v, want nil. L11: project_list takes no arguments, "+
				"and over MCP every call arrives carrying worker_id (fact 9)", ok.name, ok.args, err)
		}
	}
	if err := validateProjectList([]byte(`[]`)); err == nil {
		t.Errorf("validateProjectList([]) = nil, want an error: args that are not a JSON object are " +
			"malformed, not 'no arguments'")
	}
}

// ---- criterion 7: the EXACT key sets (L6 / L11 / L15) -----------------------

// jsonKeys marshals v and returns its top-level keys, sorted.
func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %#v: %v", v, err)
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s is not a JSON object: %v", b, err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func assertKeys(t *testing.T, what string, v any, want ...string) {
	t.Helper()
	sort.Strings(want)
	got := jsonKeys(t, v)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		b, _ := json.Marshal(v)
		t.Errorf("%s marshals to keys %v, want EXACTLY %v (wire: %s). L15: a key appears only when it "+
			"carries information, and this test is what makes a later 'just add one field' a deliberate "+
			"edit rather than drift", what, got, want, b)
	}
}

func TestQueueReadOutput_KeySetsAreCompact(t *testing.T) {
	t.Run("task row, NULL subproject and parent", func(t *testing.T) {
		// Priority 0 on purpose: priority is KEPT (L6 — it says why a row sits
		// where it does), so a zero must not be omitted as if it were absent.
		row := taskListRow{ID: 412, Title: "fix the login", Status: "ready", Priority: 0, AssigneeType: "claude"}
		assertKeys(t, "a task row with NULL subproject/parent_id", row,
			"id", "title", "status", "priority", "assignee_type")
	})

	t.Run("task row, subproject and parent set", func(t *testing.T) {
		sub := "main"
		parent := int64(401)
		row := taskListRow{ID: 418, Title: "child", Status: "blocked", Priority: 3, AssigneeType: "human",
			Subproject: &sub, ParentID: &parent}
		assertKeys(t, "a task row with subproject and parent_id set", row,
			"id", "title", "status", "priority", "assignee_type", "subproject", "parent_id")
	})

	t.Run("task row never carries body, plan_order or created_at", func(t *testing.T) {
		sub := "main"
		parent := int64(1)
		for _, row := range []taskListRow{{}, {Subproject: &sub, ParentID: &parent}} {
			for _, k := range jsonKeys(t, row) {
				switch k {
				case "body":
					t.Errorf("a task row carries %q. L6: capture and promotion put raw client message text "+
						"there and it is the single largest token cost; task_context is the audited per-task read", k)
				case "plan_order":
					t.Errorf("a task row carries %q. L6: row POSITION already encodes the order the server applied", k)
				case "created_at", "updated_at":
					t.Errorf("a task row carries %q. L6: 'what's in my queue' does not ask how old a task is", k)
				}
			}
		}
	})

	t.Run("default filter", func(t *testing.T) {
		f := newTaskListFilter(taskListArgs{Project: "collaboratory"})
		assertKeys(t, "the default filter", f, "status", "limit")
		if f.Status != "in_play" {
			t.Errorf("default filter status = %q, want \"in_play\" (L4: never 'open' — openStatuses already "+
				"means task_close's source set)", f.Status)
		}
		if f.Limit != 25 {
			t.Errorf("default filter limit = %d, want 25 (L9: one screenful; the counts carry the rest)", f.Limit)
		}
	})

	t.Run("filter echoes only the keys that apply", func(t *testing.T) {
		f := newTaskListFilter(taskListArgs{Project: "x", Status: "ready", AssigneeType: "claude", Subproject: "main", Limit: 10})
		assertKeys(t, "a fully specified filter", f, "status", "assignee_type", "subproject", "limit")
		if f.Status != "ready" || f.AssigneeType != "claude" || f.Subproject != "main" || f.Limit != 10 {
			t.Errorf("filter = %+v, want the caller's values echoed back", f)
		}
	})

	t.Run("project row, NULL client", func(t *testing.T) {
		// InPlay 0 on purpose: L11 — a project with nothing in play is still a
		// slug a repo may memorise, and its count must read 0, not be absent.
		row := projectListRow{Slug: "homelab", Name: "Homelab", InPlay: 0}
		assertKeys(t, "a project row with NULL client", row, "slug", "name", "in_play")
	})

	t.Run("project row, client set", func(t *testing.T) {
		client := "Mario Cruz"
		row := projectListRow{Slug: "saka", Name: "Saka", Client: &client, InPlay: 7}
		assertKeys(t, "a project row with a client", row, "slug", "name", "in_play", "client")
	})

	// SPEC change 2026-09-10: the local_only key is REMOVED ENTIRELY. Asserted on
	// the TYPE, not only on a marshalled value — a `local_only,omitempty` bool
	// left at false would marshal to exactly the key set above and pass every
	// subtest in this function while the field still existed.
	t.Run("project row never carries local_only", func(t *testing.T) {
		rt := reflect.TypeOf(projectListRow{})
		for i := 0; i < rt.NumField(); i++ {
			name := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
			if name == "local_only" || strings.EqualFold(rt.Field(i).Name, "LocalOnly") {
				t.Errorf("projectListRow declares %s (json %q). Salvador, 2026-09-10: task_list lists "+
					"local_only projects, so the flag no longer warns a session off a slug — it is a key "+
					"that carries no actionable information, which L15 says does not appear", rt.Field(i).Name, name)
			}
		}
		client := "c"
		for _, row := range []projectListRow{{Slug: "personal", Name: "Personal", InPlay: 7},
			{Slug: "saka", Name: "Saka", Client: &client, InPlay: 1}} {
			for _, k := range jsonKeys(t, row) {
				if k == "local_only" {
					t.Errorf("a project row marshals a local_only key: %v", jsonKeys(t, row))
				}
			}
		}
	})
}

// ---- L9: default 25, clamped to 200, the APPLIED value echoed ---------------

func TestTaskListFilter_LimitDefaultsAndClamps(t *testing.T) {
	for _, c := range []struct{ in, want int }{
		{0, 25},      // absent -> default
		{7, 7},       // honoured
		{25, 25},     //
		{200, 200},   // the max itself
		{201, 200},   // clamped, not refused
		{10000, 200}, // criterion 3's large value
	} {
		if got := newTaskListFilter(taskListArgs{Project: "x", Limit: c.in}).Limit; got != c.want {
			t.Errorf("newTaskListFilter(limit=%d).Limit = %d, want %d. L9: default 25, max 200, clamped "+
				"rather than refused, and the APPLIED value is what the response echoes", c.in, got, c.want)
		}
	}
	if got := newTaskListFilter(taskListArgs{Project: "x", Status: "delivered"}).Status; got != "delivered" {
		t.Errorf("newTaskListFilter(status=delivered).Status = %q, want \"delivered\" — an explicit status "+
			"replaces the in_play default (L4)", got)
	}
}
