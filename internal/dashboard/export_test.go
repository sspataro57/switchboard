package dashboard_test

// Offline golden tests for the deterministic board exports (SPEC 10-plan-import,
// criterion 12). CSV/JSON are pure functions of a pre-sorted row slice — no LLM,
// no I/O — so the format (pinned CSV header + field order, NULL rendering, JSON
// field set) is golden-tested here; the route wiring + filter honoring is
// covered in the dashboard integration test. ZERO network, ZERO Postgres.
//
// GREENFIELD NOTE: the export formatter is not implemented yet; this file
// compile-FAILs under `go test ./...` until it exists — the expected failure
// mode for the new dashboard export surface (the dashboard package has no prior
// test suite, so nothing existing regresses). Imposed exported surface
// (internal/dashboard/export.go), followed by the implementer — the handlers
// select rows (id ASC, filtered) and hand them to these formatters:
//
//   // TaskExportRow is one board row, field order == the pinned CSV header.
//   type TaskExportRow struct {
//       ID           int64
//       Project      string
//       Subproject   string
//       ParentID     *int64  // nil -> empty cell / null
//       Title        string
//       Status       string
//       AssigneeType string
//       WorkerType   string
//       Priority     int
//       PlanOrder    *int    // nil -> empty cell / null
//       CreatedAt    string  // RFC3339 text (COALESCE(created_at::text,''))
//       UpdatedAt    string
//   }
//
//   // WriteTasksCSV writes the pinned header then one line per row (rows already
//   // ordered id ASC by the handler). RFC 4180 quoting; nil pointers -> empty.
//   func WriteTasksCSV(w io.Writer, rows []TaskExportRow) error
//
//   // WriteTasksJSON writes a JSON array of objects with the SAME fields
//   // (nil pointers -> null).
//   func WriteTasksJSON(w io.Writer, rows []TaskExportRow) error

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/dashboard"
)

// pinnedHeader is the exact CSV header the SPEC fixes (criterion 12).
const pinnedHeader = "id,project,subproject,parent_id,title,status,assignee_type,worker_type,priority,plan_order,created_at,updated_at"

func i64p(v int64) *int64 { return &v }
func intp(v int) *int     { return &v }

func fixtureRows() []dashboard.TaskExportRow {
	return []dashboard.TaskExportRow{
		{
			ID: 1, Project: "switchboard", Subproject: "", ParentID: nil,
			Title: "Root A", Status: "ready", AssigneeType: "claude", WorkerType: "",
			Priority: 2, PlanOrder: intp(1),
			CreatedAt: "2026-07-01T10:00:00Z", UpdatedAt: "2026-07-01T10:00:00Z",
		},
		{
			ID: 2, Project: "switchboard", Subproject: "sub", ParentID: i64p(1),
			Title: "Child, needs quoting", Status: "blocked", AssigneeType: "human", WorkerType: "coordination",
			Priority: 0, PlanOrder: intp(1),
			CreatedAt: "2026-07-01T10:05:00Z", UpdatedAt: "2026-07-01T10:06:00Z",
		},
	}
}

// TestWriteTasksCSV_Golden pins the header line verbatim and the two data rows,
// including empty cells for nil parent_id/plan_order and RFC-4180 quoting of the
// comma-bearing title.
func TestWriteTasksCSV_Golden(t *testing.T) {
	var buf bytes.Buffer
	if err := dashboard.WriteTasksCSV(&buf, fixtureRows()); err != nil {
		t.Fatalf("WriteTasksCSV: %v", err)
	}
	want := pinnedHeader + "\r\n" +
		"1,switchboard,,,Root A,ready,claude,,2,1,2026-07-01T10:00:00Z,2026-07-01T10:00:00Z\r\n" +
		"2,switchboard,sub,1,\"Child, needs quoting\",blocked,human,coordination,0,1,2026-07-01T10:05:00Z,2026-07-01T10:06:00Z\r\n"
	if got := buf.String(); got != want {
		t.Errorf("CSV mismatch\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// TestWriteTasksCSV_HeaderExact isolates the load-bearing contract: the first
// line is EXACTLY the pinned header (a downstream consumer parses by position).
func TestWriteTasksCSV_HeaderExact(t *testing.T) {
	var buf bytes.Buffer
	if err := dashboard.WriteTasksCSV(&buf, nil); err != nil {
		t.Fatalf("WriteTasksCSV(nil): %v", err)
	}
	first, _, _ := strings.Cut(buf.String(), "\n")
	first = strings.TrimSuffix(first, "\r")
	if first != pinnedHeader {
		t.Errorf("CSV header = %q, want %q", first, pinnedHeader)
	}
}

// TestWriteTasksJSON_Fields: the JSON export is an array of objects carrying the
// same field set; nil pointers render as null; values round-trip.
func TestWriteTasksJSON_Fields(t *testing.T) {
	var buf bytes.Buffer
	if err := dashboard.WriteTasksJSON(&buf, fixtureRows()); err != nil {
		t.Fatalf("WriteTasksJSON: %v", err)
	}
	var out []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("export JSON is not an array of objects: %v\n%s", err, buf.String())
	}
	if len(out) != 2 {
		t.Fatalf("JSON rows = %d, want 2", len(out))
	}
	wantKeys := []string{"id", "project", "subproject", "parent_id", "title", "status",
		"assignee_type", "worker_type", "priority", "plan_order", "created_at", "updated_at"}
	for _, k := range wantKeys {
		if _, ok := out[0][k]; !ok {
			t.Errorf("JSON object missing field %q", k)
		}
	}
	// Row 0: nil parent_id -> null; id numeric.
	if out[0]["parent_id"] != nil {
		t.Errorf("row0.parent_id = %v, want null", out[0]["parent_id"])
	}
	if out[0]["id"] != float64(1) {
		t.Errorf("row0.id = %v, want 1", out[0]["id"])
	}
	// Row 1: parent_id 1, title with comma preserved.
	if out[1]["parent_id"] != float64(1) {
		t.Errorf("row1.parent_id = %v, want 1", out[1]["parent_id"])
	}
	if out[1]["title"] != "Child, needs quoting" {
		t.Errorf("row1.title = %v, want the comma-bearing title verbatim", out[1]["title"])
	}
}
