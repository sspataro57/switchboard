// Package tools registers switchboard's internal tools on the executor
// registry. Handlers are unexported closures — the registry (and therefore
// Executor.Execute) is the only way to reach them (invariant 3).
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
)

// createTaskArgs is create_task's argument schema (SPEC 01-schema-executor).
type createTaskArgs struct {
	Project      string `json:"project"` // project slug
	Title        string `json:"title"`
	Body         string `json:"body,omitempty"`
	AssigneeType string `json:"assignee_type,omitempty"` // human (default) | claude
	Priority     *int   `json:"priority,omitempty"`
	Subproject   string `json:"subproject,omitempty"`
	ParentID     *int64 `json:"parent_id,omitempty"` // orchestrator lifecycle tasks link to their source
}

// Register wires every internal tool into the registry. The registry is the
// ONLY route to any handler (invariant 3); ops-mcp additionally restricts which
// of these are agent-visible (task_release and answer_feedback are
// spine-facing).
func Register(reg *executor.Registry, pool *pgxpool.Pool) {
	type tool struct {
		name     string
		validate func([]byte) error
		handle   func(context.Context, *pgxpool.Pool, []byte) ([]byte, error)
	}
	for _, t := range []tool{
		{"create_task", validateCreateTask, createTask},
		{"task_get_next", validateGetNext, getNext},
		{"task_claim", validateClaim, claimTask},
		{"task_context", validateContext, taskContext},
		{"task_append_log", validateAppendLog, appendLog},
		{"request_feedback", validateRequestFeedback, requestFeedback},
		{"mark_done_local", validateDoneLocal, markDoneLocal},
		{"create_child_task", validateChildTask, createChildTask},
		{"record_decision", validateDecision, recordDecision},
		{"task_release", validateRelease, releaseTask},
		{"answer_feedback", validateAnswerFeedback, answerFeedback},
		{"task_add_dependency", validateAddDependency, addDependency},
		{"task_block", validateBlockUnblock, blockTask},
		{"task_unblock", validateBlockUnblock, unblockTask},
		{"task_close", validateClose, closeTask},
		{"record_orchestration", validateRecordOrchestration, recordOrchestration},
		{"propose_slots", validateProposeSlots, proposeSlots},
		{"draft_delivery", validateDraftDelivery, draftDelivery},
		{"update_delivery", validateUpdateDelivery, updateDelivery},
		{"approve_delivery", validateDeliveryIDOnly, approveDelivery},
		{"send_delivery", validateDeliveryIDOnly, sendDelivery},
		{"mark_delivery_sent", validateDeliveryIDOnly, markDeliverySent},
		{"task_mark_delivered", validateMarkDelivered, taskMarkDelivered},
		{"set_sending_frozen", validateSetFrozen, setSendingFrozen},
	} {
		t := t
		reg.Register(executor.Tool{
			Name:     t.name,
			Validate: t.validate,
			Handle: func(ctx context.Context, args []byte) ([]byte, error) {
				return t.handle(ctx, pool, args)
			},
		})
	}
}

func parseCreateTask(args []byte) (createTaskArgs, error) {
	var a createTaskArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return a, fmt.Errorf("parse args: %w", err)
	}
	if a.AssigneeType == "" {
		a.AssigneeType = "human"
	}
	return a, nil
}

func validateCreateTask(args []byte) error {
	a, err := parseCreateTask(args)
	if err != nil {
		return err
	}
	if a.Project == "" {
		return errors.New("missing project")
	}
	if a.Title == "" {
		return errors.New("missing title")
	}
	if a.AssigneeType != "human" && a.AssigneeType != "claude" {
		return fmt.Errorf("assignee_type %q: must be human or claude", a.AssigneeType)
	}
	return nil
}

// createTask resolves the project slug and inserts one tasks row with status
// `ready` (a human deliberately creating a task means it is ready to route;
// `holding` is triage's parking lane).
func createTask(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	a, err := parseCreateTask(args)
	if err != nil {
		return nil, err
	}

	var projectID int64
	err = pool.QueryRow(ctx, `SELECT id FROM projects WHERE slug = $1`, a.Project).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("project %q not found", a.Project)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve project %q: %w", a.Project, err)
	}

	priority := 0
	if a.Priority != nil {
		priority = *a.Priority
	}

	var taskID int64
	err = pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, subproject, title, body, assignee_type, status, priority, parent_id)
		 VALUES ($1, NULLIF($2, ''), $3, NULLIF($4, ''), $5, 'ready', $6, $7) RETURNING id`,
		projectID, a.Subproject, a.Title, a.Body, a.AssigneeType, priority, a.ParentID).Scan(&taskID)
	if err != nil {
		return nil, fmt.Errorf("insert task: %w", err)
	}

	out, err := json.Marshal(map[string]int64{"task_id": taskID})
	if err != nil {
		return nil, fmt.Errorf("marshal result: %w", err)
	}
	return out, nil
}
