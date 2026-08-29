// Package classify is the LOCAL actionability classifier for personal mail
// (SWT-22), in SHADOW MODE: it interprets messages into structured verdicts and
// creates NOTHING — no tasks, no task_events, no deliveries. The Store interface
// deliberately has no task-write method; going live ADDS an executor create_task
// call later, it does not remove a guard here.
//
// THE HONESTY LABEL (SWT-22 criterion 13), stated at the top because it governs
// how this package must be read and tested.
//
// The inbox filter selects only messages attributed to a project whose
// ai_locality is 'local_only', so EVERY message this worker sees is
// provider.ClassRestricted BY CONSTRUCTION. The class fold below therefore
// cannot change an outcome here: it is not a guard, and a unit test that
// supplied a class and then asserted on it would be proving its own fixture —
// this repo's seventh instance of "a predicate whose discriminating column is a
// constant in production".
//
// Two things DO protect this worker, and both are pinned where they can fail:
//   - the INBOX FILTER, in store_integration_test.go, where Postgres produces
//     the populations and an ai_locality='any' fixture is the control that dies
//     if the join is dropped; and
//   - the Router's refusal, in worker_test.go's zero-hosted-calls test, which
//     has a control proving the same fixture CAN be classified.
//
// The fold is kept anyway, spelled exactly as triage and drafts spell it, so the
// three workers cannot drift into three readings of one rule — and so that a
// later ticket widening the inbox does not have to remember to add it back.
package classify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/sspataro57/switchboard/internal/provider"
)

// AdvisoryLockKey serialises passes. Distinct from every other worker's key in
// the repo — a collision makes two unrelated workers silently exclude each
// other, which looks like "the cron did not run".
const AdvisoryLockKey = 0x5157_0022

// The restricted lane's abort rule, taken from triage rather than reinvented.
//
// It is NOT a consecutive counter. A local model sharing a card with a desktop
// produces runs of failures when it is busy, so aborting on five in a row would
// abort on the normal case. What is news is a PROPORTION: a genuinely broken
// adapter fails on everything.
//
// Both numbers are guesses and are labelled as such. Tune them against a real
// failure, not in the abstract.
const (
	restrictedErrorFloor = 20
	skipSampleMax        = 100
)

// Config is the per-run configuration. Model is per-worker, never global.
type Config struct {
	Model     string
	MaxTokens int
	Limit     int           // 0 = all pending
	Since     time.Duration // 0 = no lower bound on sent_at
}

// NeighbourClass is one thread neighbour's attribution, for the most-restrictive
// fold. Spelled identically to triage's and drafts' on purpose.
type NeighbourClass struct {
	State     provider.AttributionState
	LocalOnly bool
}

// PendingMessage is one row of the inbox, with its class inputs folded in the
// SAME query as its body — drafts' pattern, which SWT-21's deviations name as
// the better one: the class and the project cannot disagree if they are one read.
type PendingMessage struct {
	MessageID       int64
	RawSourceItemID int64
	ThreadID        int64
	SentAt          time.Time
	Sender          string
	Subject         string
	Channel         string
	BodyText        string
	Direction       string

	ProjectID        int64
	ProjectSlug      string
	ProjectLocalOnly bool
	Attribution      provider.AttributionState
	// Neighbours are INBOUND thread neighbours only. Outbound messages can never
	// carry a capture decision (the engine filters direction='inbound', which is
	// invariant 5), so folding one would read "no decision" as "unclassified"
	// when it means "not applicable" — and would restrict every replied-on
	// thread forever. See internal/drafts/store.go for the long note.
	Neighbours []NeighbourClass
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

// Store is the pg side. NOTE the shadow guarantee: it has NO task-write method,
// so nothing here can insert into tasks/task_events/deliveries/external_refs.
type Store interface {
	PendingMessages(ctx context.Context, cfg Config) ([]PendingMessage, error)
	MessagesByID(ctx context.Context, ids []int64) ([]PendingMessage, error)
	RecordRun(ctx context.Context, run AIRun) (aiRunID int64, err error)
	RecordExtraction(ctx context.Context, aiRunID, rawSourceItemID int64, fields json.RawMessage) error
}

// Local aliases for the attribution states, so the SQL layer reads without a
// package qualifier on every line. Same values, one spelling.
const (
	attrUnseen    = provider.AttrUnseen
	attrUnmatched = provider.AttrUnmatched
	attrProject   = provider.AttrProject
)

// Stats is the pass summary.
type Stats struct {
	Processed int `json:"processed"`
	Flagged   int `json:"flagged"`
	Skipped   int `json:"skipped"`
	Errors    int `json:"errors"`
}

// verdict is the model's structured answer. It mirrors VerdictSchema, and
// carries no confidence field for the reason prompt.go states.
type verdict struct {
	Actionable bool   `json:"actionable"`
	Kind       string `json:"kind"`
	Title      string `json:"title"`
	Reason     string `json:"reason"`
}

// Run classifies one pass of the inbox.
func Run(ctx context.Context, store Store, router *provider.Router, cfg Config) (Stats, error) {
	pending, err := store.PendingMessages(ctx, cfg)
	if err != nil {
		return Stats{}, fmt.Errorf("list pending messages: %w", err)
	}
	return classifyAll(ctx, store, router, cfg, pending)
}

// classifyAll is the loop, shared by Run and Eval so the two cannot diverge on
// the boundary, the skip semantics or the request shape.
func classifyAll(ctx context.Context, store Store, router *provider.Router, cfg Config,
	pending []PendingMessage) (Stats, error) {
	var stats Stats

	// Route-refusal accounting, flushed as ONE row after the loop. Counted
	// separately from stats.Skipped, which also includes per-message skips: the
	// aggregate exists because a refused lane refuses the whole inbox
	// identically, and a per-message failure is not that.
	routeSkipped := 0
	skipReasons := map[string]int{}
	skipClasses := map[string]int{}
	skipSample := make([]int64, 0, skipSampleMax)

	restrictedAttempts := 0
	restrictedUnclassified := 0

	for _, m := range pending {
		class := classOf(m)

		lane, decision, reason := router.Route(ctx, class)
		if decision != provider.DecideAllow {
			// NOT an error. A refused message is the boundary working, and it
			// writes no extraction, so it stays in the inbox for the next pass.
			stats.Skipped++
			routeSkipped++
			skipReasons[string(reason)]++
			skipClasses[classReasonOf(m)]++
			if len(skipSample) < skipSampleMax {
				skipSample = append(skipSample, m.MessageID)
			}
			continue
		}

		restricted := reason == provider.ReasonLocalReady
		if restricted {
			restrictedAttempts++
		}

		user := renderUser(m)
		input := runInput(m, user)
		laneName := lane.Describe().Name

		resp, callErr := lane.Complete(ctx, provider.Request{
			Model:      cfg.Model,
			System:     SystemPrompt,
			User:       user,
			SchemaName: SchemaName,
			Schema:     VerdictSchema,
			MaxTokens:  cfg.MaxTokens,
		})

		var v verdict
		parseErr := callErr
		if parseErr == nil {
			if err := json.Unmarshal(resp.Raw, &v); err != nil {
				parseErr = fmt.Errorf("parse verdict JSON: %w", err)
			}
		}

		if parseErr != nil {
			// The restricted lane fails DIFFERENTLY, and the distinction is what
			// makes the ratio computable: ErrUnavailable is a busy box (normal
			// operation for a 5.9 GB model on a shared card), anything else is a
			// broken adapter.
			if restricted {
				skipReason := provider.ReasonUnclassifiedError
				if errors.Is(parseErr, provider.ErrUnavailable) {
					skipReason = provider.ReasonLocalUnreachable
				} else {
					restrictedUnclassified++
				}
				stats.Skipped++
				slog.Warn("classify skipped a message", "message", m.MessageID,
					"reason", skipReason, "err", parseErr)
				payload, _ := json.Marshal(map[string]any{
					"avail_reason":          string(skipReason),
					"normalized_message_id": m.MessageID,
					"raw_source_item_id":    m.RawSourceItemID,
					"error":                 parseErr.Error(),
				})
				if _, err := store.RecordRun(ctx, AIRun{
					WorkerType: "classify", Provider: laneName, Model: cfg.Model, Status: "skipped",
					Input: payload, Output: safeJSON(resp.Raw),
					PromptTokens: resp.PromptTokens, CompletionTokens: resp.CompletionTokens,
					LatencyMS: resp.LatencyMS,
				}); err != nil {
					return stats, fmt.Errorf("record skip for message %d: %w", m.MessageID, err)
				}
				continue
			}

			stats.Errors++
			slog.Error("classify message failed", "message", m.MessageID, "err", parseErr)
			if _, err := store.RecordRun(ctx, AIRun{
				WorkerType: "classify", Provider: laneName, Model: cfg.Model, Status: "error",
				Input: input, Output: safeJSON(resp.Raw),
				PromptTokens: resp.PromptTokens, CompletionTokens: resp.CompletionTokens,
				LatencyMS: resp.LatencyMS,
			}); err != nil {
				return stats, fmt.Errorf("record error run for message %d: %w", m.MessageID, err)
			}
			continue
		}

		runID, err := store.RecordRun(ctx, AIRun{
			WorkerType: "classify", Provider: laneName, Model: cfg.Model, Status: "ok",
			Input: input, Output: safeJSON(resp.Raw),
			PromptTokens: resp.PromptTokens, CompletionTokens: resp.CompletionTokens,
			LatencyMS: resp.LatencyMS,
		})
		if err != nil {
			return stats, fmt.Errorf("record run for message %d: %w", m.MessageID, err)
		}
		// The extraction is what removes the message from the inbox, so it is
		// written for EVERY classified message — including a not-actionable one.
		// "The classifier looked and found nothing" and "nothing looked" must
		// stay structurally different.
		fields, _ := json.Marshal(map[string]any{
			"actionable":            v.Actionable,
			"kind":                  v.Kind,
			"title":                 v.Title,
			"reason":                v.Reason,
			"project_id":            m.ProjectID,
			"project_slug":          m.ProjectSlug,
			"normalized_message_id": m.MessageID,
		})
		if err := store.RecordExtraction(ctx, runID, m.RawSourceItemID, fields); err != nil {
			return stats, fmt.Errorf("record extraction for message %d: %w", m.MessageID, err)
		}

		stats.Processed++
		if v.Actionable {
			stats.Flagged++
		}
	}

	// Flush BEFORE the raising return: a pass that raises still refused messages,
	// and losing that record is how a real refusal becomes invisible behind an
	// unrelated error.
	flushErr := flushSkips(ctx, store, cfg, routeSkipped, skipReasons, skipClasses, skipSample)

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
	if stats.Errors > 0 {
		return stats, fmt.Errorf("%d message(s) failed; they will retry on the next run", stats.Errors)
	}
	return stats, nil
}

// classOf folds the message's own class with every neighbour whose body travels
// in the prompt.
//
// Constant in production — see the package comment. Kept so the three workers
// spell one rule one way, and so a ticket that widens the inbox inherits it.
func classOf(m PendingMessage) provider.Class {
	classes := []provider.Class{provider.ClassOf(m.Attribution, m.ProjectLocalOnly)}
	for _, n := range m.Neighbours {
		classes = append(classes, provider.ClassOf(n.State, n.LocalOnly))
	}
	return provider.MostRestrictive(classes...)
}

// classReasonOf names WHY a message was restricted, for the skip record.
// Constant here (always project_local_only), and the skip test says so rather
// than pretending it discriminates.
func classReasonOf(m PendingMessage) string {
	switch m.Attribution {
	case provider.AttrUnseen:
		return "unseen"
	case provider.AttrUnmatched:
		return "unmatched"
	default:
		if m.ProjectLocalOnly {
			return "project_local_only"
		}
		return "thread_context"
	}
}

// flushSkips writes the pass's ONE aggregate route-refusal row.
func flushSkips(ctx context.Context, store Store, cfg Config, routeSkipped int,
	reasons, classes map[string]int, sample []int64) error {
	if routeSkipped == 0 {
		return nil
	}
	payload, _ := json.Marshal(map[string]any{
		// avail_reason is the dominant one, which the report groups by;
		// avail_reasons carries the full breakdown, because a pass can refuse for
		// more than one reason and filing the whole count under the dominant one
		// prints a wrong number where an operator looks.
		"avail_reason":  dominantReason(reasons),
		"avail_reasons": reasons,
		"class_reasons": classes,
		"skipped_count": routeSkipped,
		"message_ids":   sample,
		"sampled":       len(sample) < routeSkipped,
	})
	if _, err := store.RecordRun(ctx, AIRun{
		WorkerType: "classify", Provider: "local", Model: cfg.Model,
		Status: "skipped", Input: payload,
	}); err != nil {
		return fmt.Errorf("record pass skip summary: %w", err)
	}
	return nil
}

func dominantReason(reasons map[string]int) string {
	keys := make([]string, 0, len(reasons))
	for k := range reasons {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	best, bestN := "", -1
	for _, k := range keys {
		if reasons[k] > bestN {
			best, bestN = k, reasons[k]
		}
	}
	return best
}

// renderUser builds the message half of the prompt.
//
// Sender and subject are enough for most senders (measured), but the BODY is
// required for the templated ones — the HOA manager wraps its subject in
// "[#XN######] Message from <Association> - …" with the topic truncated away, so
// the subject alone carries no signal at all.
func renderUser(m PendingMessage) string {
	body := m.BodyText
	const maxBody = 4000
	if len(body) > maxBody {
		body = body[:maxBody] + "\n…(truncated)"
	}
	return fmt.Sprintf("From: %s\nSubject: %s\nDate: %s\n\n%s",
		m.Sender, m.Subject, m.SentAt.UTC().Format(time.RFC3339), body)
}

// runInput is the ai_runs.input bookkeeping: prompt version, ids, and the
// rendered prompt, so a verdict can be reproduced.
func runInput(m PendingMessage, user string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"prompt_version":        PromptVersion,
		"normalized_message_id": m.MessageID,
		"raw_source_item_id":    m.RawSourceItemID,
		"thread_id":             m.ThreadID,
		"project_id":            m.ProjectID,
		"project_slug":          m.ProjectSlug,
		"user_prompt":           user,
	})
	return raw
}

func safeJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || !json.Valid(raw) {
		return json.RawMessage(`{}`)
	}
	return raw
}
