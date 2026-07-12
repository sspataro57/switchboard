package planimport

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sspataro57/switchboard/internal/provider"
)

// Config is the propose flow's knobs (cmd/planimport reads the env).
type Config struct {
	Model     string
	MaxTokens int
}

// AIRun is one ai_runs row's worth of bookkeeping (triage.AIRun shape).
type AIRun struct {
	WorkerType, Provider, Model, Status string
	Input, Output                       json.RawMessage
	PromptTokens, CompletionTokens      int
	LatencyMS                           int
}

// Store is the planimport pg side. SHADOW-LIKE GUARANTEE (invariant 2): it has
// NO task-write method — the propose flow never touches tasks/task_events; the
// approved tree materializes only through the apply executor tool.
type Store interface {
	EnsurePlanAccount(ctx context.Context) (accountID int64, err error)
	UpsertRaw(ctx context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) (rawItemID int64, err error)
	RecordRun(ctx context.Context, run AIRun) (aiRunID int64, err error)
	RecordExtraction(ctx context.Context, aiRunID, rawSourceItemID int64, fields json.RawMessage) (aiExtractionID int64, err error)
}

// Proposal is what Propose returns; cmd/planimport feeds these ids to the
// executor propose_plan_import call.
type Proposal struct {
	ProjectSlug, SourcePath, ContentHash string
	RawSourceItemID, AIRunID, AIExtractionID int64
}

// Propose is criteria 2-4: raw-first capture (EnsurePlanAccount + UpsertRaw
// with the FULL file content, BEFORE any LLM call) → provider.Complete
// (plan_tree strict schema) → deterministic Validate → RecordRun (+
// RecordExtraction on success). A provider error OR a hard-invalid tree
// records an error ai_run and returns non-nil with NO extraction.
func Propose(ctx context.Context, store Store, client provider.Client, cfg Config,
	projectSlug, sourcePath string, content []byte) (Proposal, error) {
	if IsStub(string(content)) {
		return Proposal{}, fmt.Errorf("%s is already an imported-plan stub; new work goes through create_child_task", sourcePath)
	}

	hash := ContentHash(content)
	prop := Proposal{ProjectSlug: projectSlug, SourcePath: sourcePath, ContentHash: hash}

	// Raw-first (invariant 1): the full file text lands before the model sees it.
	acctID, err := store.EnsurePlanAccount(ctx)
	if err != nil {
		return prop, fmt.Errorf("ensure plan account: %w", err)
	}
	rawDoc, err := json.Marshal(map[string]string{"path": sourcePath, "content": string(content)})
	if err != nil {
		return prop, fmt.Errorf("marshal raw plan: %w", err)
	}
	prop.RawSourceItemID, err = store.UpsertRaw(ctx, acctID, "plan:"+projectSlug+":"+hash, rawDoc, hash)
	if err != nil {
		return prop, fmt.Errorf("store raw plan: %w", err)
	}

	userPrompt := fmt.Sprintf("Project: %s\nPlan file %s:\n\n%s", projectSlug, sourcePath, content)
	req := provider.Request{
		Model:      cfg.Model,
		System:     SystemPrompt,
		User:       userPrompt,
		SchemaName: SchemaName,
		Schema:     PlanTreeSchema,
		MaxTokens:  cfg.MaxTokens,
	}
	input, err := json.Marshal(map[string]any{
		"prompt_version":     PromptVersion,
		"raw_source_item_id": prop.RawSourceItemID,
		"system":             req.System,
		"user":               req.User,
	})
	if err != nil {
		return prop, fmt.Errorf("marshal run input: %w", err)
	}

	start := time.Now()
	resp, provErr := client.Complete(ctx, req)
	run := AIRun{
		WorkerType: "plan_import", Provider: "openai", Model: cfg.Model,
		Status: "ok", Input: input, Output: resp.Raw,
		PromptTokens: resp.PromptTokens, CompletionTokens: resp.CompletionTokens,
		LatencyMS: resp.LatencyMS,
	}
	if run.LatencyMS == 0 {
		run.LatencyMS = int(time.Since(start).Milliseconds())
	}
	if resp.Model != "" {
		run.Model = resp.Model
	}

	fail := func(cause error) (Proposal, error) {
		run.Status = "error"
		run.Output = mustJSON(map[string]string{"error": cause.Error()})
		if resp.Raw != nil {
			run.Output = mustJSON(map[string]any{"error": cause.Error(), "raw": json.RawMessage(resp.Raw)})
		}
		if _, rerr := store.RecordRun(ctx, run); rerr != nil {
			return prop, fmt.Errorf("%w (also failed to record error run: %v)", cause, rerr)
		}
		return prop, cause
	}

	if provErr != nil {
		return fail(fmt.Errorf("provider: %w", provErr))
	}

	var tree Tree
	if err := json.Unmarshal(resp.Raw, &tree); err != nil {
		return fail(fmt.Errorf("parse model tree: %w", err))
	}
	validated, err := Validate(tree)
	if err != nil {
		return fail(err)
	}

	prop.AIRunID, err = store.RecordRun(ctx, run)
	if err != nil {
		return prop, fmt.Errorf("record ai_run: %w", err)
	}
	fields, err := json.Marshal(validated)
	if err != nil {
		return prop, fmt.Errorf("marshal validated tree: %w", err)
	}
	prop.AIExtractionID, err = store.RecordExtraction(ctx, prop.AIRunID, prop.RawSourceItemID, fields)
	if err != nil {
		return prop, fmt.Errorf("record extraction: %w", err)
	}
	return prop, nil
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}
