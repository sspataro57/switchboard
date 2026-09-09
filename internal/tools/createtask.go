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
	// Status is ready (default) or holding (SWT-30: the review lane is a
	// holding task; 06-gpt-triage reserved this parameter for exactly that).
	// Deliberately NOT in internal/mcpserver/schemas.go — agents keep the
	// ready-only surface, and holding is strictly less privileged anyway.
	Status string `json:"status,omitempty"`
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
		// SWT-31: the board's dismiss verb — close + a typed task_dismissals
		// label in one transaction. humanOnly (a dismissal is training data);
		// deliberately NOT in internal/mcpserver/schemas.go — an agent that
		// could dismiss tasks could clear its own queue. See close.go.
		{"task_dismiss", validateDismiss, dismissTask},
		{"record_orchestration", validateRecordOrchestration, recordOrchestration},
		{"propose_slots", validateProposeSlots, proposeSlots},
		{"draft_delivery", validateDraftDelivery, draftDelivery},
		{"update_delivery", validateUpdateDelivery, updateDelivery},
		{"approve_delivery", validateDeliveryIDOnly, approveDelivery},
		{"send_delivery", validateDeliveryIDOnly, sendDelivery},
		// SWT-28: the calendar auto tier's verb — approve + send a drafted
		// calendar row in one audited call. NOT human-only (Q1 = b); the gates
		// are the policy matrix (channel_mismatch, kill switch, rate limit)
		// and the handler's LoadBusy refusal. See delivery_calendar.go.
		{"book_calendar_block", validateDeliveryIDOnly, bookCalendarBlock},
		{"mark_delivery_sent", validateDeliveryIDOnly, markDeliverySent},
		{"mark_delivery_failed", validateDeliveryIDOnly, markDeliveryFailed},
		{"prefill_delivery", validateDeliveryIDOnly, prefillDelivery},
		{"task_mark_delivered", validateMarkDelivered, taskMarkDelivered},
		{"set_sending_frozen", validateSetFrozen, setSendingFrozen},
		{"link_external_ref", validateLinkExternalRef, linkExternalRef},
		// Read-only mail surface (SWT-11). Served from normalized_messages, never
		// from live IMAP — see internal/tools/mail.go.
		{"mail_search", validateMailSearch, mailSearch},
		{"mail_read_thread", validateMailReadThread, mailReadThread},
		{"record_pr_event", validateRecordPREvent, recordPREvent},
		{"record_ci_event", validateRecordCIEvent, recordCIEvent},
		{"task_pr_transition", validatePRTransition, taskPRTransition},
		{"propose_plan_import", validateProposePlanImport, proposePlanImport},
		{"approve_plan_import", validatePlanImportID, approvePlanImport},
		{"reject_plan_import", validatePlanImportID, rejectPlanImport},
		{"apply_plan_import", validatePlanImportID, applyPlanImport},
		// SWT-17 capture rules. Human-only (policy.humanOnly) and deliberately
		// NOT in internal/mcpserver/schemas.go — an agent must not be able to
		// redirect the funnel at itself. See internal/tools/capturerules.go.
		{"capture_rule_add", validateCaptureRuleAdd, captureRuleAdd},
		{"capture_rule_set_enabled", validateCaptureRuleSetEnabled, captureRuleSetEnabled},
		// SWT-20 provenance. Spine-facing and deliberately NOT in
		// internal/mcpserver/schemas.go: this writes the fact that decides where
		// a task's deliveries may be aimed, so an agent with a transport to it
		// could aim them itself. NOT humanOnly — the capture engine
		// (capture:{connector}) is its main caller. See internal/tools/provenance.go.
		{"task_set_source_thread", validateSetSourceThread, taskSetSourceThread},
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
	if a.Status == "" {
		// The default lives HERE, beside AssigneeType's, so "what did this call
		// mean" is a property of the parsed args, not of one SQL literal.
		a.Status = "ready"
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
	if a.Status != "ready" && a.Status != "holding" {
		// By name, both ways: anything else is either a lifecycle transition
		// that belongs to the orchestrator's tools, or a typo that would
		// silently create the wrong thing.
		return fmt.Errorf("status %q: must be ready or holding", a.Status)
	}
	return nil
}

// createTask resolves the project slug and inserts one tasks row with the
// parsed status: `ready` by default (a deliberately created task is ready to
// route), or `holding` — the review/parking lane (SWT-30's promoter and,
// eventually, triage's live slice).
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
		 VALUES ($1, NULLIF($2, ''), $3, NULLIF($4, ''), $5, $6, $7, $8) RETURNING id`,
		projectID, a.Subproject, a.Title, a.Body, a.AssigneeType, a.Status, priority, a.ParentID).Scan(&taskID)
	if err != nil {
		return nil, fmt.Errorf("insert task: %w", err)
	}

	out, err := json.Marshal(map[string]int64{"task_id": taskID})
	if err != nil {
		return nil, fmt.Errorf("marshal result: %w", err)
	}
	return out, nil
}
