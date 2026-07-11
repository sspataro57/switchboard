package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// request_feedback (agent-facing) and answer_feedback (spine-facing).

type requestFeedbackArgs struct {
	TaskID   int64  `json:"task_id"`
	WorkerID string `json:"worker_id"`
	Question string `json:"question"`
}

func validateRequestFeedback(args []byte) error {
	var a requestFeedbackArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.TaskID == 0 {
		return errors.New("missing task_id")
	}
	if a.WorkerID == "" {
		return errors.New("missing worker_id")
	}
	if a.Question == "" {
		return errors.New("missing question")
	}
	return nil
}

func requestFeedback(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a requestFeedbackArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	var feedbackID int64
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := activeClaim(ctx, tx, a.TaskID, a.WorkerID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO feedback_requests (task_id, question, status)
			 VALUES ($1, $2, 'open') RETURNING id`,
			a.TaskID, a.Question).Scan(&feedbackID); err != nil {
			return fmt.Errorf("insert feedback request: %w", err)
		}
		tag, err := tx.Exec(ctx,
			`UPDATE tasks SET status='needs_feedback', updated_at=now()
			 WHERE id=$1 AND status IN ('in_progress','claimed')`, a.TaskID)
		if err != nil {
			return fmt.Errorf("mark task %d needs_feedback: %w", a.TaskID, err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("task %d is not in_progress; cannot request feedback", a.TaskID)
		}
		_, err = insertTaskEvent(ctx, tx, a.TaskID, "feedback_requested",
			map[string]any{"feedback_request_id": feedbackID, "question": a.Question, "worker_id": a.WorkerID})
		return err
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{"feedback_request_id": feedbackID})
}

type answerFeedbackArgs struct {
	FeedbackRequestID int64  `json:"feedback_request_id"`
	Answer            string `json:"answer"`
}

func validateAnswerFeedback(args []byte) error {
	var a answerFeedbackArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if a.FeedbackRequestID == 0 {
		return errors.New("missing feedback_request_id")
	}
	if a.Answer == "" {
		return errors.New("missing answer")
	}
	return nil
}

// answerFeedback records the answer. The task STAYS needs_feedback — the flip
// to in_progress happens at resume time (next holder task_context).
func answerFeedback(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	var a answerFeedbackArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	var taskID int64
	var client string
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`UPDATE feedback_requests SET answer=$2, status='answered', answered_at=now()
			 WHERE id=$1 AND status='open' RETURNING task_id`,
			a.FeedbackRequestID, a.Answer).Scan(&taskID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("feedback request %d not found or not open", a.FeedbackRequestID)
		}
		if err != nil {
			return fmt.Errorf("answer feedback %d: %w", a.FeedbackRequestID, err)
		}
		if err := tx.QueryRow(ctx,
			`SELECT p.client FROM tasks t JOIN projects p ON p.id=t.project_id WHERE t.id=$1`,
			taskID).Scan(&client); err != nil {
			return fmt.Errorf("resolve client for task %d: %w", taskID, err)
		}
		_, err = insertTaskEvent(ctx, tx, taskID, "feedback_answered",
			map[string]any{"feedback_request_id": a.FeedbackRequestID})
		return err
	})
	if err != nil {
		return nil, err
	}
	return marshalResult(map[string]any{
		"feedback_request_id": a.FeedbackRequestID,
		"task_id":             taskID,
		"client":              client,
	})
}
