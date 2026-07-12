package tools

// SWT-10 plan-import tools (SPEC 10-plan-import): propose_plan_import records
// the gate row for an already-captured extraction; approve/reject are the
// dashboard's human verdict; apply materializes the approved tree as tasks in
// ONE transaction. None is MCP-listed — agents' verb for new work stays
// create_child_task.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/planimport"
)

// ---- propose_plan_import -------------------------------------------------------

type proposePlanImportArgs struct {
	Project         string `json:"project"`
	SourcePath      string `json:"source_path"`
	ContentHash     string `json:"content_hash"`
	RawSourceItemID int64  `json:"raw_source_item_id"`
	AIRunID         int64  `json:"ai_run_id"`
	AIExtractionID  int64  `json:"ai_extraction_id"`
}

func validateProposePlanImport(args []byte) error {
	var a proposePlanImportArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.Project == "" {
		return errors.New("missing project")
	}
	if a.SourcePath == "" {
		return errors.New("missing source_path")
	}
	if a.ContentHash == "" {
		return errors.New("missing content_hash")
	}
	if a.RawSourceItemID == 0 || a.AIRunID == 0 || a.AIExtractionID == 0 {
		return errors.New("missing raw_source_item_id / ai_run_id / ai_extraction_id")
	}
	return nil
}

// proposePlanImport inserts the 'proposed' gate row. Defense in depth: the
// stored extraction is re-validated here — a handler never trusts that the
// upstream flow ran Validate. The partial unique index enforces one live
// proposal per (project, content).
func proposePlanImport(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a proposePlanImportArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	var projectID int64
	err := pool.QueryRow(ctx, `SELECT id FROM projects WHERE slug=$1`, a.Project).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("project %q not found", a.Project)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve project %q: %w", a.Project, err)
	}

	if _, err := loadValidatedTree(ctx, pool, a.AIExtractionID); err != nil {
		return nil, err
	}

	var id int64
	err = pool.QueryRow(ctx,
		`INSERT INTO plan_imports
		   (project_id, source_path, content_hash, raw_source_item_id, ai_run_id, ai_extraction_id, status)
		 VALUES ($1,$2,$3,$4,$5,$6,'proposed') RETURNING id`,
		projectID, a.SourcePath, a.ContentHash, a.RawSourceItemID, a.AIRunID, a.AIExtractionID).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("insert plan_import (one live proposal per content — reject the pending one first?): %w", err)
	}
	return marshalResult(map[string]any{"plan_import_id": id})
}

// loadValidatedTree reads ai_extractions.fields and re-runs the deterministic
// validation (the plan_order the flow assigned is preserved from the stored
// fields; validation only gates structure).
func loadValidatedTree(ctx context.Context, q querier, aiExtractionID int64) (planimport.Result, error) {
	var fields []byte
	err := q.QueryRow(ctx, `SELECT fields FROM ai_extractions WHERE id=$1`, aiExtractionID).Scan(&fields)
	if errors.Is(err, pgx.ErrNoRows) {
		return planimport.Result{}, fmt.Errorf("ai_extraction %d not found", aiExtractionID)
	}
	if err != nil {
		return planimport.Result{}, fmt.Errorf("read ai_extraction %d: %w", aiExtractionID, err)
	}
	var stored planimport.Result
	if err := json.Unmarshal(fields, &stored); err != nil {
		return planimport.Result{}, fmt.Errorf("parse extraction %d fields: %w", aiExtractionID, err)
	}
	// Re-validate the structure (refs, cycles, enums, cap).
	tree := planimport.Tree{Summary: stored.Summary}
	for _, v := range stored.Tasks {
		tree.Tasks = append(tree.Tasks, planimport.Node{
			Ref: v.Ref, ParentRef: v.ParentRef, Title: v.Title, Body: v.Body,
			AssigneeType: v.AssigneeType, Subproject: v.Subproject, WorkerType: v.WorkerType,
			Priority: v.Priority, DependsOnRefs: v.DependsOnRefs,
			Confidence: v.Confidence, Notes: v.Notes,
		})
	}
	if _, err := planimport.Validate(tree); err != nil {
		return planimport.Result{}, fmt.Errorf("extraction %d: %w", aiExtractionID, err)
	}
	return stored, nil
}

// ---- approve / reject ------------------------------------------------------------

type planImportIDArgs struct {
	PlanImportID int64  `json:"plan_import_id"`
	Reason       string `json:"reason,omitempty"`
}

func validatePlanImportID(args []byte) error {
	var a planImportIDArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.PlanImportID == 0 {
		return errors.New("missing plan_import_id")
	}
	return nil
}

func approvePlanImport(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	return decidePlanImport(ctx, pool, args, "approved")
}

func rejectPlanImport(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	return decidePlanImport(ctx, pool, args, "rejected")
}

// decidePlanImport flips proposed→approved|rejected + an approvals row
// (approveDelivery idiom). Already-at-target is an idempotent no-op success;
// any other prior state is an error (approve-after-reject and vice versa).
func decidePlanImport(ctx context.Context, pool *pgxpool.Pool, args []byte, target string) ([]byte, error) {
	var a planImportIDArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx,
			`SELECT status FROM plan_imports WHERE id=$1 FOR UPDATE`, a.PlanImportID).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("plan_import %d not found", a.PlanImportID)
			}
			return fmt.Errorf("lock plan_import %d: %w", a.PlanImportID, err)
		}
		if status == target {
			return nil // idempotent replay
		}
		if status != "proposed" {
			return fmt.Errorf("plan_import %d is %s; only proposed plans can be %s", a.PlanImportID, status, target)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE plan_imports SET status=$2, decided_by=$3, decided_at=now() WHERE id=$1`,
			a.PlanImportID, target, executor.ActorFrom(ctx)); err != nil {
			return fmt.Errorf("%s plan_import %d: %w", target, a.PlanImportID, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO approvals (subject_type, subject_id, status, decided_by, decided_at)
			 VALUES ('plan_import', $1, $2, $3, now())`,
			a.PlanImportID, target, executor.ActorFrom(ctx)); err != nil {
			return fmt.Errorf("record approval: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"plan_import_id": a.PlanImportID, "status": target})
}

// ---- apply_plan_import ------------------------------------------------------------

// applyPlanImport materializes the approved tree in ONE transaction: tasks in
// parents-first order (status ready — R4 blocks dependents off the emitted
// dependency_added events on the next drain), task_dependencies rows, and
// child_created / dependency_added / plan_imported task_events. Already
// applied → idempotent no-op returning the stored result.
func applyPlanImport(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a planImportIDArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	var result json.RawMessage
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		var status string
		var projectID, extractionID int64
		var stored *json.RawMessage
		if err := tx.QueryRow(ctx,
			`SELECT status, project_id, ai_extraction_id, result
			 FROM plan_imports WHERE id=$1 FOR UPDATE`, a.PlanImportID).
			Scan(&status, &projectID, &extractionID, &stored); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("plan_import %d not found", a.PlanImportID)
			}
			return fmt.Errorf("lock plan_import %d: %w", a.PlanImportID, err)
		}
		if status == "applied" {
			if stored != nil {
				result = *stored
			}
			return nil // idempotent replay: stored result, zero writes
		}
		if status != "approved" {
			return fmt.Errorf("plan_import %d is %s; only approved plans apply (dashboard approval required)", a.PlanImportID, status)
		}

		tree, err := loadValidatedTree(ctx, tx, extractionID)
		if err != nil {
			return err
		}

		refToID := make(map[string]int64, len(tree.Tasks))
		for _, n := range tree.Tasks {
			var parentID *int64
			if n.ParentRef != nil {
				pid, ok := refToID[*n.ParentRef]
				if !ok {
					return fmt.Errorf("parent ref %q not inserted before %q (tree not parents-first)", *n.ParentRef, n.Ref)
				}
				parentID = &pid
			}
			var taskID int64
			if err := tx.QueryRow(ctx,
				`INSERT INTO tasks (project_id, subproject, title, body, assignee_type,
				                    worker_type, status, priority, parent_id, plan_order)
				 VALUES ($1, $2, $3, NULLIF($4,''), $5, $6, 'ready', $7, $8, $9) RETURNING id`,
				projectID, n.Subproject, n.Title, n.Body, n.AssigneeType,
				n.WorkerType, n.Priority, parentID, n.PlanOrder).Scan(&taskID); err != nil {
				return fmt.Errorf("insert task %q: %w", n.Ref, err)
			}
			refToID[n.Ref] = taskID

			if parentID != nil {
				if _, err := insertTaskEvent(ctx, tx, *parentID, "child_created",
					map[string]any{"child_task_id": taskID, "plan_import_id": a.PlanImportID}); err != nil {
					return err
				}
			} else {
				if _, err := insertTaskEvent(ctx, tx, taskID, "plan_imported",
					map[string]any{"plan_import_id": a.PlanImportID}); err != nil {
					return err
				}
			}
		}

		// Dependencies after all tasks exist (a dep may point at a later sibling).
		for _, n := range tree.Tasks {
			for _, dep := range n.DependsOnRefs {
				if _, err := tx.Exec(ctx,
					`INSERT INTO task_dependencies (task_id, depends_on_task_id) VALUES ($1,$2)
					 ON CONFLICT DO NOTHING`, refToID[n.Ref], refToID[dep]); err != nil {
					return fmt.Errorf("insert dependency %q -> %q: %w", n.Ref, dep, err)
				}
				if _, err := insertTaskEvent(ctx, tx, refToID[n.Ref], "dependency_added",
					map[string]any{"depends_on_task_id": refToID[dep], "plan_import_id": a.PlanImportID}); err != nil {
					return err
				}
			}
		}

		result, err = json.Marshal(map[string]any{"tasks": refToID})
		if err != nil {
			return fmt.Errorf("marshal result map: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE plan_imports SET status='applied', applied_at=now(), result=$2 WHERE id=$1`,
			a.PlanImportID, result); err != nil {
			return fmt.Errorf("mark applied: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"plan_import_id": a.PlanImportID, "result": json.RawMessage(result)})
}
