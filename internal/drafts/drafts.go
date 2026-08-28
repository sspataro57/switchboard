// Package drafts is the GPT draft worker (SPEC 08-draft-deliveries): a
// triage-shaped queue consumer that drafts outbound client communications for
// R3 Deliver tasks. Reads direct; writes ONLY through the executor
// (draft_delivery on success, task_append_log on skip). The model contract is
// strictly {subject, body} — From/To/channel/thread are deterministic store
// resolution, never model output.
package drafts

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/provider"
)

const PromptVersion = "drafts-v1"

const SchemaName = "delivery_draft"

// SystemPrompt: Salvador's terse register, no sign-offs beyond a plain name,
// never any AI attribution.
const SystemPrompt = `You draft outbound client messages for a solo contract engineer (Salvador).
Write in his terse register: short sentences, plain words, no fluff, no
corporate padding, no exclamation marks. One idea per paragraph. Sign off
with "Salvador" or nothing. NEVER mention AI, assistants, automation, or add
any attribution line (no "Generated with", no Co-Authored-By). The message
should tell the client what was done and what happens next, grounded ONLY in
the task summary and thread context provided.

OUTPUT CONTRACT:
- subject: short reply subject (reuse the thread's subject with Re: when
  replying; empty for chat channels).
- body: the message text, self-contained, ready to send verbatim.`

// DraftSchema is the strict model contract: exactly {subject, body}.
var DraftSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["subject", "body"],
  "properties": {
    "subject": {"type": "string"},
    "body": {"type": "string"}
  }
}`)

type Config struct {
	Model     string
	MaxTokens int
	Limit     int
}

type ThreadMessage struct {
	Direction string
	Sender    string
	Subject   string
	BodyText  string
	SentAt    time.Time
}

// DeliverTask is one R3 Deliver task whose parent has no delivery row yet,
// with channel + thread resolved deterministically by the store.
type DeliverTask struct {
	DeliverTaskID int64
	ParentTaskID  int64
	ProjectSlug   string
	Channel       string // "gmail" | "upwork_chat" | "" (unresolvable)
	ThreadID      *int64
	TargetRef     string
	ParentTitle   string
	ParentSummary string
	ClientName    string
	Thread        []ThreadMessage

	// The locality boundary's inputs (SWT-21). A Deliver task belongs to a
	// project, so its class comes from that project's ai_locality rather than
	// from a capture decision — but the thread context folded into the prompt is
	// message content, and its attribution counts the same way it does in triage.
	ProjectLocalOnly     bool
	NeighbourAttribution []NeighbourClass
}

// NeighbourClass is one thread message's attribution, for the most-restrictive
// fold. Spelled identically to triage's on purpose: the two workers assemble
// context the same way and must not drift into two spellings of one rule.
type NeighbourClass struct {
	State     provider.AttributionState
	LocalOnly bool
}

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

// Store is the read/bookkeeping side (no task or delivery writes — those go
// through the executor).
type Store interface {
	DeliverTasks(ctx context.Context, cfg Config) ([]DeliverTask, error)
	RecordRun(ctx context.Context, run AIRun) (aiRunID int64, err error)
}

// Executor is the executor.Execute seam (invariant 3).
type Executor interface {
	Execute(ctx context.Context, call executor.Call) (executor.Result, error)
}

type Stats struct {
	Drafted int `json:"drafted"`
	Skipped int `json:"skipped"`
	Errors  int `json:"errors"`
}

// Actor is the worker's executor identity (a bot: the matrix human-only gate
// keeps it away from approve/send).
const Actor = "drafts:gpt"

// Run drains the Deliver-task queue once.
// Run takes a ROUTER rather than a Client since SWT-21, so the boundary decides
// per task rather than per worker. Unlike triage, drafts' hosted lane stays
// genuinely useful: a Deliver task on a project with ai_locality='any' is client
// work and routes general as before.
func Run(ctx context.Context, store Store, router *provider.Router, exec Executor, cfg Config) (Stats, error) {
	var stats Stats

	tasks, err := store.DeliverTasks(ctx, cfg)
	if err != nil {
		return stats, fmt.Errorf("list deliver tasks: %w", err)
	}

	for _, dt := range tasks {
		if dt.Channel == "" || (dt.Channel == "gmail" && dt.ThreadID == nil) {
			// Unresolvable — tell the human on the Deliver task and move on.
			msg, _ := json.Marshal(fmt.Sprintf(
				"draft worker could not resolve a delivery channel/thread for task #%d — draft manually via the dashboard",
				dt.ParentTaskID))
			if _, err := exec.Execute(ctx, executor.Call{Tool: "task_append_log", Actor: Actor,
				Args: []byte(fmt.Sprintf(`{"task_id":%d,"message":%s,"kind":"draft_skip"}`, dt.DeliverTaskID, msg))}); err != nil {
				slog.Warn("append skip log failed", "task", dt.DeliverTaskID, "err", err)
			}
			stats.Skipped++
			continue
		}

		user := renderUser(dt)
		input, _ := json.Marshal(map[string]any{
			"prompt_version":  PromptVersion,
			"deliver_task_id": dt.DeliverTaskID,
			"parent_task_id":  dt.ParentTaskID,
			"channel":         dt.Channel,
			"user_prompt":     user,
		})

		// THE BOUNDARY (SWT-21). The task's own project, folded with every thread
		// neighbour whose body will travel in the prompt.
		classes := []provider.Class{provider.ClassOf(provider.AttrProject, dt.ProjectLocalOnly)}
		for _, n := range dt.NeighbourAttribution {
			classes = append(classes, provider.ClassOf(n.State, n.LocalOnly))
		}
		lane, decision, reason := router.Route(ctx, provider.MostRestrictive(classes...))
		if decision != provider.DecideAllow {
			// Skip, never fall back. A Deliver task on a local-only project whose
			// local lane is down waits for the next pass; it does not get drafted
			// by a hosted model instead.
			//
			// The refusal is logged onto the Deliver task through the SAME
			// draft_skip path an unresolvable channel uses (criterion 24) — a
			// second skip vocabulary would mean a human reading the task sees
			// "nothing happened" in one case and a log line in the other.
			//
			// The reason string is load-bearing, not decoration. A test fixture
			// that forgets ai_locality produces a SKIP rather than a failure
			// mentioning locality, so this line is the only thing that tells its
			// author which of the two they are looking at.
			//
			// Unlike triage's aggregate, this logs per task per pass. Deliver
			// tasks are tens, not the ~16,000 of triage's inbox, so the SWT-17
			// amplification arithmetic does not apply — but if a stuck task ever
			// sits here for weeks, this is the line that will have accumulated.
			stats.Skipped++
			slog.Info("drafts skipped a task: no permitted provider",
				"task", dt.DeliverTaskID, "project", dt.ProjectSlug, "reason", reason)
			msg, _ := json.Marshal(fmt.Sprintf(
				"draft skipped for task #%d: no permitted provider for project %s (%s). "+
					"Restricted content is never drafted by a hosted model; it retries next pass.",
				dt.ParentTaskID, dt.ProjectSlug, reason))
			if _, err := exec.Execute(ctx, executor.Call{Tool: "task_append_log", Actor: Actor,
				Args: []byte(fmt.Sprintf(`{"task_id":%d,"message":%s,"kind":"draft_skip"}`, dt.DeliverTaskID, msg))}); err != nil {
				slog.Warn("append locality skip log failed", "task", dt.DeliverTaskID, "err", err)
			}
			continue
		}

		// ai_runs.provider names the LANE THAT SERVED, matching triage. A
		// constant "openai" here would be the same lie the triage change fixed,
		// in the other worker that can now route local.
		laneName := lane.Describe().Name
		resp, callErr := lane.Complete(ctx, provider.Request{
			Model:      cfg.Model,
			System:     SystemPrompt,
			User:       user,
			SchemaName: SchemaName,
			Schema:     DraftSchema,
			MaxTokens:  cfg.MaxTokens,
		})

		var draft struct {
			Subject string `json:"subject"`
			Body    string `json:"body"`
		}
		parseErr := callErr
		if parseErr == nil {
			if err := json.Unmarshal(resp.Raw, &draft); err != nil {
				parseErr = fmt.Errorf("parse draft JSON: %w", err)
			}
		}
		if parseErr == nil && draft.Body == "" {
			parseErr = fmt.Errorf("model returned an empty body")
		}

		status := "ok"
		if parseErr != nil {
			status = "error"
		}
		if _, err := store.RecordRun(ctx, AIRun{
			WorkerType: "drafts", Provider: laneName, Model: cfg.Model, Status: status,
			Input: input, Output: safeJSON(resp.Raw),
			PromptTokens: resp.PromptTokens, CompletionTokens: resp.CompletionTokens, LatencyMS: resp.LatencyMS,
		}); err != nil {
			return stats, fmt.Errorf("record run: %w", err)
		}
		if parseErr != nil {
			slog.Error("draft failed", "task", dt.ParentTaskID, "err", parseErr)
			stats.Errors++
			continue
		}

		args := map[string]any{
			"task_id": dt.ParentTaskID,
			"channel": dt.Channel,
			"body":    draft.Body,
		}
		if dt.Channel == "gmail" {
			args["subject"] = draft.Subject
			args["thread_id"] = *dt.ThreadID
		} else {
			args["target_ref"] = dt.TargetRef
		}
		rawArgs, _ := json.Marshal(args)
		if _, err := exec.Execute(ctx, executor.Call{Tool: "draft_delivery", Actor: Actor, Args: rawArgs}); err != nil {
			slog.Error("draft_delivery failed", "task", dt.ParentTaskID, "err", err)
			stats.Errors++
			continue
		}
		stats.Drafted++
	}

	if stats.Errors > 0 {
		return stats, fmt.Errorf("%d draft(s) failed; they will retry on the next run", stats.Errors)
	}
	return stats, nil
}

func renderUser(dt DeliverTask) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Client: %s\nProject: %s\nChannel: %s\n", orDash(dt.ClientName), dt.ProjectSlug, dt.Channel)
	fmt.Fprintf(&b, "\nCompleted work (task #%d): %s\nSummary: %s\n", dt.ParentTaskID, dt.ParentTitle, dt.ParentSummary)
	if len(dt.Thread) > 0 {
		b.WriteString("\nRecent thread (oldest first):\n")
		for _, m := range dt.Thread {
			fmt.Fprintf(&b, "  [%s %s] %s: %s\n", m.SentAt.Format("2006-01-02"), m.Direction, m.Sender, truncate(m.BodyText, 300))
		}
	}
	b.WriteString("\nDraft the message telling the client this work is done.")
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

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
