package dashboard

// Deterministic board exports (SPEC 10-plan-import, criterion 12): pure
// formatters over a pre-sorted row slice — no LLM, no I/O here. Handlers
// select rows (id ASC, board filters) and hand them over.

import (
	"encoding/csv"
	"encoding/json"
	"io"
	"strconv"
)

// TaskExportRow is one board row, field order == the pinned CSV header.
type TaskExportRow struct {
	ID           int64  `json:"id"`
	Project      string `json:"project"`
	Subproject   string `json:"subproject"`
	ParentID     *int64 `json:"parent_id"`
	Title        string `json:"title"`
	Status       string `json:"status"`
	AssigneeType string `json:"assignee_type"`
	WorkerType   string `json:"worker_type"`
	Priority     int    `json:"priority"`
	PlanOrder    *int   `json:"plan_order"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// exportHeader is the pinned CSV header — downstream consumers parse by position.
var exportHeader = []string{"id", "project", "subproject", "parent_id", "title", "status",
	"assignee_type", "worker_type", "priority", "plan_order", "created_at", "updated_at"}

// WriteTasksCSV writes the pinned header then one line per row (rows already
// ordered id ASC by the handler). RFC 4180 quoting; nil pointers -> empty cell.
func WriteTasksCSV(w io.Writer, rows []TaskExportRow) error {
	cw := csv.NewWriter(w)
	cw.UseCRLF = true
	if err := cw.Write(exportHeader); err != nil {
		return err
	}
	for _, r := range rows {
		parent := ""
		if r.ParentID != nil {
			parent = strconv.FormatInt(*r.ParentID, 10)
		}
		order := ""
		if r.PlanOrder != nil {
			order = strconv.Itoa(*r.PlanOrder)
		}
		rec := []string{
			strconv.FormatInt(r.ID, 10), r.Project, r.Subproject, parent, r.Title,
			r.Status, r.AssigneeType, r.WorkerType, strconv.Itoa(r.Priority), order,
			r.CreatedAt, r.UpdatedAt,
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// WriteTasksJSON writes a JSON array of objects with the same fields
// (nil pointers -> null).
func WriteTasksJSON(w io.Writer, rows []TaskExportRow) error {
	if rows == nil {
		rows = []TaskExportRow{}
	}
	enc := json.NewEncoder(w)
	return enc.Encode(rows)
}
