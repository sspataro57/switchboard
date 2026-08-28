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
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sspataro57/switchboard/internal/provider"
)

// maxConsecutiveErrors aborts the run when the provider looks dead: 5
// consecutive failures are tolerated, the 6th aborts. It governs the GENERAL
// lane only; the restricted lane has its own, gentler rule below.
const maxConsecutiveErrors = 5

// The restricted lane's abort rule (SWT-21 criterion 18). It is deliberately
// NOT the consecutive counter above.
//
// The local lane is a small model at low priority on a machine that stays usable
// as a desktop, so a run of failures is what a busy box looks like, not what a
// broken one looks like. Aborting on five in a row would abort on the normal
// case. What IS news is a PROPORTION: a genuinely broken adapter fails on
// everything.
//
// Both numbers are GUESSES, and saying so is the point — nothing has been
// measured against a real local outage yet. Tune them against one, not in the
// abstract.
//
// restrictedErrorFloor is the minimum number of restricted-lane attempts before
// the ratio may fire at all. Without it a two-message pass where both messages
// happen to fail cries wolf at 100%. The ratio itself is "more than half",
// spelled as integer arithmetic at the call site so exactly half does not fire.
const restrictedErrorFloor = 20

// Config is the per-run configuration (model is per-worker config, never
// global — CLAUDE.md).
type Config struct {
	Model string
	// LocalModel is the model name for the LOCAL lane. Separate from Model
	// because the two lanes run different models — a hosted gpt-5-mini and a
	// local qwen3:8b — and sending one lane's name to the other produces a
	// "model not found" that reads like an outage. Empty falls back to Model.
	LocalModel string
	MaxTokens  int
	Limit      int           // 0 = all pending
	Since      time.Duration // 0 = no lower bound on sent_at
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

	// The locality boundary's inputs (SWT-21). Filled by AssembleContext from
	// the SAME capture_decisions read that resolves the project, so the class and
	// the project cannot disagree about what the engine decided.
	Attribution      provider.AttributionState
	ProjectLocalOnly bool
	// NeighbourAttribution carries one entry per thread message folded into the
	// prompt. Separate from Thread because Thread is what the MODEL sees and this
	// is what the BOUNDARY sees; conflating them invites someone to trim one and
	// silently shrink the other.
	NeighbourAttribution []NeighbourClass
}

// NeighbourClass is one thread neighbour's attribution, for the most-restrictive
// fold. A neighbour's body travels with the focus message, so its class counts
// even though it is not the message being triaged.
type NeighbourClass struct {
	State     provider.AttributionState
	LocalOnly bool
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
	Processed int `json:"processed"`
	Errors    int `json:"errors"`
	// Skipped counts messages the locality boundary refused (SWT-21). It is
	// SEPARATE from Errors on purpose: "no permitted provider looked at this"
	// and "the model looked and found nothing" are different facts and must not
	// share an alarm — the same separation the reconcilers make between a poller
	// that did not run and a poller that found nothing.
	//
	// After SWT-21 and before SWT-22, EVERY message lands here: triage's whole
	// inbox is unmatched, unmatched is restricted, and no local adapter exists
	// yet. An all-skipped pass is the SUCCESS state in that window, not a fault.
	Skipped    int `json:"skipped"`
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
// Run takes a ROUTER rather than a Client since SWT-21. A worker that holds a
// single client has no way to be told "not this one, for this message", and the
// boundary would then have to live in each worker's own discipline — which is
// exactly the configuration-shaped guard this ticket replaces.
func Run(ctx context.Context, store Store, router *provider.Router, cfg Config) (Stats, error) {
	var stats Stats

	pending, err := store.PendingMessages(ctx, cfg)
	if err != nil {
		return stats, fmt.Errorf("list pending messages: %w", err)
	}

	consecutive := 0
	// Route-refusal accounting, flushed as ONE row after the loop. See the skip
	// branch. Counted SEPARATELY from stats.Skipped, which also includes the
	// per-message skips below: the aggregate row exists because a refused lane
	// refuses the whole inbox identically, and a per-message failure is not that.
	routeSkipped := 0
	// A COUNT per reason, not a single last-wins reason. A pass can refuse for
	// more than one: some messages before the probe TTL expires and some after,
	// so `no_local_provider` and `local_unreachable` legitimately coexist. The
	// earlier spelling kept only the last and filed the whole count under it,
	// which is a wrong number rather than a missing one — and the three reasons
	// have three different fixes.
	skipReasons := map[string]int{}
	skipClasses := map[string]int{}
	skipSample := make([]int64, 0, skipSampleMax)

	// Restricted-lane call accounting for the ratio abort.
	restrictedAttempts := 0
	restrictedUnclassified := 0

	for _, m := range pending {
		mc, err := store.AssembleContext(ctx, m)
		if err != nil {
			return stats, fmt.Errorf("assemble context for message %d: %w", m.MessageID, err)
		}

		user := renderUser(mc)
		input := runInput(mc, user)

		// THE BOUNDARY (SWT-21). Classify before rendering a request, and route
		// before spending anything.
		//
		// The class is the MOST RESTRICTIVE over the focus message and every
		// neighbour AssembleContext folded in: that context is pulled by
		// thread_id alone, with no reference to the neighbours' own attribution,
		// so a restricted sibling's body would otherwise ride along.
		//
		// Note what this cannot do today, said out loud so nobody mistakes it for
		// protection it is not providing: the triaged message is unmatched by
		// construction (that is what triage's inbox IS), so it is already
		// restricted and the fold cannot change the outcome here. The fold is
		// load-bearing in drafts, and it is spelled the same way in both so the
		// two cannot drift.
		classes := []provider.Class{provider.ClassOf(mc.Attribution, mc.ProjectLocalOnly)}
		for _, n := range mc.NeighbourAttribution {
			classes = append(classes, provider.ClassOf(n.State, n.LocalOnly))
		}
		class := provider.MostRestrictive(classes...)

		lane, decision, reason := router.Route(ctx, class)
		if decision != provider.DecideAllow {
			// NOT an error, and deliberately not counted toward the consecutive
			// abort: a refused message is the boundary working.
			//
			// ONE AGGREGATE ROW PER PASS, not one per message. This is SWT-17's
			// amplification lesson applied before it bites rather than after:
			// triage's whole inbox is unmatched and therefore restricted, so
			// until a local adapter exists EVERY message skips — a row each would
			// be ~16,000 ai_runs rows per pass, per pass, forever, recording
			// nothing that the aggregate does not say better.
			stats.Skipped++
			routeSkipped++
			skipReasons[string(reason)]++
			skipClasses[classReasonOf(mc)]++
			if len(skipSample) < skipSampleMax {
				skipSample = append(skipSample, m.MessageID)
			}
			continue
		}

		model := cfg.Model
		restricted := reason == provider.ReasonLocalReady
		if restricted {
			restrictedAttempts++
			if cfg.LocalModel != "" {
				model = cfg.LocalModel
			}
		}
		// ai_runs.provider names the LANE that actually served, not a constant.
		// It was hardcoded "openai", which after this ticket would label every
		// locally-processed row as hosted — the exact question this column exists
		// to answer, answered wrongly, in the audit trail.
		laneName := lane.Describe().Name
		resp, callErr := lane.Complete(ctx, provider.Request{
			Model:      model,
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
			// The restricted lane fails DIFFERENTLY (criterion 18). A failure
			// there skips the MESSAGE — no extraction, so it stays in the pending
			// filter and retries next pass — and never falls through to the
			// hosted lane. It is recorded per message rather than in the pass
			// aggregate because, unlike a refused lane, each failure is its own
			// fact: the aggregate exists for the case where every message is
			// refused for the same reason.
			if restricted {
				skipReason := provider.ReasonUnclassifiedError
				if errors.Is(parseErr, provider.ErrUnavailable) {
					// Tier 1: normal operation. It does not count toward the
					// ratio, or a busy afternoon would look like an outage.
					skipReason = provider.ReasonLocalUnreachable
				} else {
					restrictedUnclassified++
				}
				stats.Skipped++
				slog.Warn("triage message skipped", "message", m.MessageID, "reason", skipReason, "err", parseErr)
				payload, _ := json.Marshal(map[string]any{
					"avail_reason":          string(skipReason),
					"normalized_message_id": m.MessageID,
					"raw_source_item_id":    m.RawSourceItemID,
					"error":                 parseErr.Error(),
				})
				if _, err := store.RecordRun(ctx, AIRun{
					WorkerType: "triage", Provider: laneName, Model: model, Status: "skipped",
					Input: payload, Output: safeJSON(resp.Raw),
					PromptTokens: resp.PromptTokens, CompletionTokens: resp.CompletionTokens,
					LatencyMS: resp.LatencyMS,
				}); err != nil {
					return stats, fmt.Errorf("record skip for message %d: %w", m.MessageID, err)
				}
				continue
			}

			stats.Errors++
			consecutive++
			slog.Error("triage message failed", "message", m.MessageID, "err", parseErr)
			if _, err := store.RecordRun(ctx, AIRun{
				WorkerType: "triage", Provider: laneName, Model: model, Status: "error",
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
			WorkerType: "triage", Provider: laneName, Model: model, Status: "ok",
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

	// Flush BEFORE the two raising returns below, not after. A pass that raises
	// still refused messages, and losing that record is how a real refusal
	// becomes invisible behind an unrelated error — reachable when a local
	// adapter errors on most attempts while the probe TTL expires mid-pass and
	// later messages route-skip.
	// A flush FAILURE must not mask a message failure. Both end in a non-zero
	// exit, but they mean different things — "the boundary's record could not be
	// written" and "N messages did not process" have different fixes — and the
	// message failure is the one an operator is looking for. Joined rather than
	// prioritised, so neither is lost.
	flushErr := flushSkips(ctx, store, cfg, routeSkipped, skipReasons, skipClasses, skipSample)

	if stats.Errors > 0 {
		err := fmt.Errorf("%d message(s) failed; they will retry on the next run", stats.Errors)
		if flushErr != nil {
			return stats, errors.Join(err, flushErr)
		}
		return stats, err
	}
	// The restricted lane raises on the PATTERN, not on the count. Integer
	// arithmetic rather than a float ratio so "more than half" cannot become
	// "half" through rounding.
	if restrictedAttempts >= restrictedErrorFloor && restrictedUnclassified*2 > restrictedAttempts {
		err := fmt.Errorf("local lane failed %d of %d attempts with unclassified errors; "+
			"that is a broken adapter, not a busy one", restrictedUnclassified, restrictedAttempts)
		if flushErr != nil {
			return stats, errors.Join(err, flushErr)
		}
		return stats, err
	}
	if flushErr != nil {
		return stats, flushErr
	}
	return stats, nil
}

// flushSkips writes the pass's ONE aggregate route-refusal row.
//
// One row per pass, not one per message. This is SWT-17's amplification lesson
// applied before it bites rather than after: triage's whole inbox is unmatched
// and therefore restricted, so until a local adapter exists EVERY message skips
// — a row each would be ~16,000 ai_runs rows per pass, per pass, forever,
// recording nothing the aggregate does not say better.
//
// provider='local' rather than the reason, because the reason belongs in the
// payload where it sits beside the counts; the provider column answers "which
// lane", and the lane is what was unavailable.
func flushSkips(ctx context.Context, store Store, cfg Config, routeSkipped int,
	reasons, classes map[string]int, sample []int64) error {
	if routeSkipped == 0 {
		return nil
	}
	// The message ids are a SAMPLE, capped. An unbounded array here would put the
	// whole inbox in one jsonb column and reintroduce by the back door exactly
	// the volume this aggregate exists to avoid.
	payload, _ := json.Marshal(map[string]any{
		// avail_reason stays a single string because the report groups by it and
		// an operator reads it; avail_reasons carries the full breakdown when a
		// pass refused for more than one. When there is only one they agree, and
		// that is the overwhelmingly common case.
		"avail_reason":  dominantReason(reasons),
		"avail_reasons": reasons,
		"class_reasons": classes,
		"skipped_count": routeSkipped,
		"message_ids":   sample,
		"sampled":       len(sample) < routeSkipped,
	})
	if _, err := store.RecordRun(ctx, AIRun{
		WorkerType: "triage", Provider: "local", Model: cfg.Model,
		Status: "skipped", Input: payload,
	}); err != nil {
		return fmt.Errorf("record pass skip summary: %w", err)
	}
	return nil
}

// dominantReason picks the reason that accounts for the most refusals, ties
// broken alphabetically so the same pass always renders the same way.
func dominantReason(reasons map[string]int) string {
	best, bestN := "", -1
	for r, n := range reasons {
		if n > bestN || (n == bestN && r < best) {
			best, bestN = r, n
		}
	}
	return best
}

// skipSampleMax caps the ids carried in the pass's skip record. Enough to chase
// a few by hand; not so many that the aggregate becomes the thing it replaced.
const skipSampleMax = 100

// classReasonOf names WHY a message was restricted, for the skip record's
// breakdown. The three states are SWT-17 §8's, reused rather than re-invented,
// so a reader comparing the capture report and the triage report sees the same
// vocabulary in both.
func classReasonOf(mc MessageContext) string {
	switch mc.Attribution {
	case provider.AttrUnseen:
		return "unseen"
	case provider.AttrUnmatched:
		return "unmatched"
	default:
		if mc.ProjectLocalOnly {
			return "project_local_only"
		}
		// Restricted despite a general project: a neighbour in the thread was
		// restricted and the fold carried it. Worth its own name — otherwise this
		// case looks like a bug in the project lookup.
		return "thread_context"
	}
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
