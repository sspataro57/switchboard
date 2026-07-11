// Package triage is the GPT triage worker in SHADOW MODE (SPEC 06-gpt-triage):
// it interprets un-triaged inbound messages into structured extractions with
// per-field confidence and creates NOTHING — no tasks, no task_events, no
// deliveries. The Store interface deliberately has no task-write method; the
// live slice ADDS an executor create_task call later, it does not remove a
// guard here.
package triage

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/sspataro57/switchboard/internal/provider"
)

// maxConsecutiveErrors aborts the run when the provider looks dead: 5
// consecutive failures are tolerated, the 6th aborts.
const maxConsecutiveErrors = 5

// Config is the per-run configuration (model is per-worker config, never
// global — CLAUDE.md).
type Config struct {
	Model     string
	MaxTokens int
	Limit     int           // 0 = all pending
	Since     time.Duration // 0 = no lower bound on sent_at
}

// PendingMessage is one un-triaged inbound normalized_messages row.
type PendingMessage struct {
	MessageID       int64
	RawSourceItemID int64
	ThreadID        int64
	SentAt          time.Time
	Sender          string
	Subject         string
	Channel         string
	BodyText        string
	Direction       string // always "inbound" from the filter
}

// ThreadMessage is prior thread context (both directions).
type ThreadMessage struct {
	Direction string
	Sender    string
	Subject   string
	BodyText  string
	SentAt    time.Time
}

// Candidate is one find_related_tasks result offered to the model.
type Candidate struct {
	ID         int64
	Title      string
	Status     string
	Subproject string
	UpdatedAt  time.Time
}

// MessageContext is the deterministic context assembled per message.
type MessageContext struct {
	Message     PendingMessage
	Thread      []ThreadMessage
	PersonID    *int64
	PersonName  string
	ProjectID   *int64 // nil = UNMAPPED
	ProjectSlug string
	Candidates  []Candidate
}

// AIRun is one ai_runs row's worth of bookkeeping.
type AIRun struct {
	WorkerType       string
	Provider         string
	Model            string
	Status           string
	Input            json.RawMessage
	Output           json.RawMessage
	PromptTokens     int
	CompletionTokens int
	LatencyMS        int
}

// Store is the pg side. SHADOW GUARANTEE: no method here can touch
// tasks/task_events/deliveries — enforced by a reflection test.
type Store interface {
	PendingMessages(ctx context.Context, cfg Config) ([]PendingMessage, error)
	AssembleContext(ctx context.Context, m PendingMessage) (MessageContext, error)
	RecordRun(ctx context.Context, run AIRun) (aiRunID int64, err error)
	RecordExtraction(ctx context.Context, aiRunID, rawSourceItemID int64, fields json.RawMessage) error
}

// Stats is one run's outcome, printed as JSON by cmd/triage.
type Stats struct {
	Processed  int `json:"processed"`
	Errors     int `json:"errors"`
	Actionable int `json:"actionable"`
	Create     int `json:"create"`
	Attach     int `json:"attach"`
	None       int `json:"none"`
}

// fieldVal is one {value, confidence} extraction field.
type fieldVal struct {
	Value      any     `json:"value"`
	Confidence float64 `json:"confidence"`
}

type extraction struct {
	Actionable     fieldVal `json:"actionable"`
	Kind           fieldVal `json:"kind"`
	Title          fieldVal `json:"title"`
	Body           fieldVal `json:"body"`
	Priority       fieldVal `json:"priority"`
	AttachToTaskID fieldVal `json:"attach_to_task_id"`
	Summary        string   `json:"summary"`
}

// Run drains the pending filter once, oldest-first. Per-message failures are
// recorded (ai_runs status error, no extraction) and non-fatal; the run exits
// non-zero at the end. More than maxConsecutiveErrors consecutive provider
// failures abort the batch.
func Run(ctx context.Context, store Store, client provider.Client, cfg Config) (Stats, error) {
	var stats Stats

	pending, err := store.PendingMessages(ctx, cfg)
	if err != nil {
		return stats, fmt.Errorf("list pending messages: %w", err)
	}

	consecutive := 0
	for _, m := range pending {
		mc, err := store.AssembleContext(ctx, m)
		if err != nil {
			return stats, fmt.Errorf("assemble context for message %d: %w", m.MessageID, err)
		}

		user := renderUser(mc)
		input := runInput(mc, user)

		resp, callErr := client.Complete(ctx, provider.Request{
			Model:      cfg.Model,
			System:     SystemPrompt,
			User:       user,
			SchemaName: SchemaName,
			Schema:     ExtractionSchema,
			MaxTokens:  cfg.MaxTokens,
		})

		var ext extraction
		parseErr := callErr
		if parseErr == nil {
			if err := json.Unmarshal(resp.Raw, &ext); err != nil {
				parseErr = fmt.Errorf("parse extraction JSON: %w", err)
			}
		}

		if parseErr != nil {
			stats.Errors++
			consecutive++
			slog.Error("triage message failed", "message", m.MessageID, "err", parseErr)
			if _, err := store.RecordRun(ctx, AIRun{
				WorkerType: "triage", Provider: "openai", Model: cfg.Model, Status: "error",
				Input: input, Output: safeJSON(resp.Raw),
				PromptTokens: resp.PromptTokens, CompletionTokens: resp.CompletionTokens, LatencyMS: resp.LatencyMS,
			}); err != nil {
				return stats, fmt.Errorf("record error run for message %d: %w", m.MessageID, err)
			}
			if consecutive > maxConsecutiveErrors {
				return stats, fmt.Errorf("aborting after %d consecutive provider errors (provider down?)", consecutive)
			}
			continue
		}
		consecutive = 0

		fields, verdict := buildFields(mc, ext)
		runID, err := store.RecordRun(ctx, AIRun{
			WorkerType: "triage", Provider: "openai", Model: cfg.Model, Status: "ok",
			Input: input, Output: safeJSON(resp.Raw),
			PromptTokens: resp.PromptTokens, CompletionTokens: resp.CompletionTokens, LatencyMS: resp.LatencyMS,
		})
		if err != nil {
			return stats, fmt.Errorf("record run for message %d: %w", m.MessageID, err)
		}
		if err := store.RecordExtraction(ctx, runID, m.RawSourceItemID, fields); err != nil {
			return stats, fmt.Errorf("record extraction for message %d: %w", m.MessageID, err)
		}

		stats.Processed++
		if b, _ := ext.Actionable.Value.(bool); b {
			stats.Actionable++
		}
		switch verdict {
		case "attach":
			stats.Attach++
		case "create":
			stats.Create++
		default:
			stats.None++
		}
	}

	if stats.Errors > 0 {
		return stats, fmt.Errorf("%d message(s) failed; they will retry on the next run", stats.Errors)
	}
	return stats, nil
}

// runInput is the ai_runs.input bookkeeping: prompt version, ids, candidates,
// and the rendered prompt (reproducibility).
func runInput(mc MessageContext, user string) json.RawMessage {
	candidateIDs := make([]int64, 0, len(mc.Candidates))
	for _, c := range mc.Candidates {
		candidateIDs = append(candidateIDs, c.ID)
	}
	raw, _ := json.Marshal(map[string]any{
		"prompt_version":        PromptVersion,
		"normalized_message_id": mc.Message.MessageID,
		"raw_source_item_id":    mc.Message.RawSourceItemID,
		"thread_id":             mc.Message.ThreadID,
		"candidate_ids":         candidateIDs,
		"user_prompt":           user,
	})
	return raw
}

// buildFields validates + clamps the extraction and assembles the
// ai_extractions.fields document. Every correction is recorded in
// fields.validation.
func buildFields(mc MessageContext, ext extraction) (json.RawMessage, string) {
	var validation []string

	clamp := func(name string, f *fieldVal) {
		if f.Confidence > 1 {
			validation = append(validation, fmt.Sprintf("%s.confidence clamped from %v to 1", name, f.Confidence))
			f.Confidence = 1
		}
		if f.Confidence < 0 {
			validation = append(validation, fmt.Sprintf("%s.confidence clamped from %v to 0", name, f.Confidence))
			f.Confidence = 0
		}
	}
	clamp("actionable", &ext.Actionable)
	clamp("kind", &ext.Kind)
	clamp("title", &ext.Title)
	clamp("body", &ext.Body)
	clamp("priority", &ext.Priority)
	clamp("attach_to_task_id", &ext.AttachToTaskID)

	// Candidate-constrain attach_to_task_id.
	if ext.AttachToTaskID.Value != nil {
		id := int64(0)
		if n, ok := ext.AttachToTaskID.Value.(float64); ok {
			id = int64(n)
		}
		valid := false
		for _, c := range mc.Candidates {
			if c.ID == id {
				valid = true
				break
			}
		}
		if !valid {
			validation = append(validation,
				fmt.Sprintf("attach_to_task_id %d rejected: not in the offered candidate set", id))
			ext.AttachToTaskID.Value = nil
		}
	}

	actionable, _ := ext.Actionable.Value.(bool)
	verdict := "none"
	switch {
	case ext.AttachToTaskID.Value != nil:
		verdict = "attach"
	case actionable:
		verdict = "create"
	}

	doc := map[string]any{
		"actionable":            ext.Actionable,
		"kind":                  ext.Kind,
		"title":                 ext.Title,
		"body":                  ext.Body,
		"priority":              ext.Priority,
		"attach_to_task_id":     ext.AttachToTaskID,
		"summary":               ext.Summary,
		"verdict":               verdict,
		"prompt_version":        PromptVersion,
		"normalized_message_id": mc.Message.MessageID,
		"thread_id":             mc.Message.ThreadID,
		"person_id":             mc.PersonID,
		"project_id":            mc.ProjectID,
	}
	if len(validation) > 0 {
		doc["validation"] = validation
	}
	raw, _ := json.Marshal(doc)
	return raw, verdict
}

// safeJSON returns valid JSON for storage: raw if already valid, a JSON
// string wrapper otherwise, {} when empty.
func safeJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	if json.Valid(raw) {
		return raw
	}
	wrapped, _ := json.Marshal(string(raw))
	return wrapped
}
