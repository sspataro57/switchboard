package classify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sspataro57/switchboard/internal/provider"
	"github.com/sspataro57/switchboard/internal/textmatch"
)

// Label is one line of a labelled-set file (docs/evals/*.jsonl).
//
// It carries NO MESSAGE CONTENT — not a subject, not a body, not a sender. The
// bodies are loaded from the database by id at eval time, so the labelled set is
// safe to commit while the mail it scores never leaves the machine. `Note` is
// free text and is documented as content-free for the same reason.
//
// Stratum (SWT-23) is set on every residue line and absent from the personal
// file: `uniform` is the only stratum a base rate or an honest precision can
// come from, `enriched` is the recall denominator, `domain_gate` records the
// claim-gate samples of criterion 6. It lives IN the file because a precision
// computed over an enriched sample WILL be quoted as production precision
// unless the harness refuses to make that mistake for the reader.
type Label struct {
	MessageID     int64  `json:"message_id"`
	Label         string `json:"label"` // "actionable" | "not"
	SubjectSHA256 string `json:"subject_sha256"`
	Stratum       string `json:"stratum,omitempty"` // "" | uniform | enriched | domain_gate
	Note          string `json:"note,omitempty"`
}

// SubjectHash is the ONE spelling of the fixture's hash. internal/textmatch is
// this repo's single spelling of prefix normalisation; re-spelling it here or in
// SQL is how two hashes of the "same" subject stop agreeing with no error
// anywhere.
func SubjectHash(subject string) string {
	sum := sha256.Sum256([]byte(textmatch.NormalizedPrefix(subject, 120)))
	return hex.EncodeToString(sum[:])[:16]
}

// Eval scores the classifier against a hand-checked labelled set, on the lane
// cfg names (the lane's own system prompt, the lane's own loader — a number
// measured through a prompt nobody runs describes nothing).
//
// It REFUSES to run on anything but the local lane. An eval on the hosted lane
// would be two failures at once: the whole labelled corpus — the most sensitive
// mail in the system, selected for being sensitive — posted to a hosted API, and
// a number that describes a model other than the one that will actually run.
//
// It deliberately does NOT apply the residue run's --since refusal: an eval is
// bounded by its label file, and refusing it would refuse the one command the
// ticket's numbers come from.
//
// It also reports and EXCLUDES label drift. The labels are the fixture, and this
// fixture has already been wrong once: the spike's first eval scored every model
// 0.10–0.27 recall because the labels called "your statement is available"
// actionable while the prompt said informational notices were not. The models
// were right and the fixture was wrong. A silently re-pointed id would move the
// score with no visible cause, which is indistinguishable from the prompt
// getting better or worse.
func Eval(ctx context.Context, store Store, router *provider.Router, cfg Config,
	labels []Label, w io.Writer) error {
	if err := cfg.Lane.validate(); err != nil {
		return err
	}
	// Refuse FIRST, before a single message is read or sent. The check is the
	// router's own answer for the class this worker's inbox always carries.
	lane, decision, reason := router.Route(ctx, provider.ClassRestricted)
	if decision != provider.DecideAllow || lane == nil {
		return fmt.Errorf("eval refused: the boundary did not permit the local lane (%s). "+
			"Scoring on the hosted lane would post the entire labelled corpus to a hosted API and "+
			"produce a number for a model that will never run", reason)
	}

	ids := make([]int64, 0, len(labels))
	for _, l := range labels {
		ids = append(ids, l.MessageID)
	}
	msgs, err := store.MessagesByID(ctx, cfg, ids)
	if err != nil {
		return fmt.Errorf("load labelled messages: %w", err)
	}
	byID := make(map[int64]PendingMessage, len(msgs))
	for _, m := range msgs {
		byID[m.MessageID] = m
	}

	// Drift and absence are the same class of problem — a score computed over a
	// set that quietly lost rows is a score nobody can reproduce — so both are
	// printed and both are excluded BEFORE anything is classified.
	var scored []PendingMessage
	wantActionable := map[int64]bool{}
	stratumOf := map[int64]string{}
	var missing, drifted []int64
	for _, l := range labels {
		m, ok := byID[l.MessageID]
		if !ok {
			missing = append(missing, l.MessageID)
			continue
		}
		if got := SubjectHash(m.Subject); got != l.SubjectSHA256 {
			drifted = append(drifted, l.MessageID)
			continue
		}
		scored = append(scored, m)
		wantActionable[l.MessageID] = l.Label == "actionable"
		stratumOf[l.MessageID] = l.Stratum
	}

	if len(missing) > 0 || len(drifted) > 0 {
		fmt.Fprintf(w, "label drift: %d excluded\n", len(missing)+len(drifted))
		if len(drifted) > 0 {
			fmt.Fprintf(w, "  subject hash mismatch (excluded): %s\n", joinIDs(drifted))
		}
		if len(missing) > 0 {
			fmt.Fprintf(w, "  not found in the database (excluded): %s\n", joinIDs(missing))
		}
		fmt.Fprintln(w, "  a label whose subject no longer matches is a fixture that moved, not a model that changed.")
		fmt.Fprintln(w)
	}

	// SWT-23 criterion 17: the residue loader has no action predicate, so a
	// scored label may since have been claimed by a rule. SAY so — Phase 1
	// exists to move messages out of the residue, and a score drifting because
	// the population changed must not look like a prompt getting worse.
	if cfg.Lane.Name == LaneResidue.Name {
		var claimed []int64
		for _, m := range scored {
			if m.Attribution != provider.AttrUnmatched {
				claimed = append(claimed, m.MessageID)
			}
		}
		if len(claimed) > 0 {
			fmt.Fprintf(w, "no longer unmatched (a rule has claimed them since): %d — %s\n",
				len(claimed), joinIDs(claimed))
			fmt.Fprintln(w, "  still scored: the label file is the population, and dropping them would be")
			fmt.Fprintln(w, "  label drift by another mechanism.")
			fmt.Fprintln(w)
		}
	}

	// One verdict per surviving message, through the same request shape the
	// worker uses — a different one here would score a prompt nobody runs.
	type outcome struct {
		id         int64
		actionable bool
		latencyMS  int
	}
	// CHECKPOINT (added after the 874-label run died twice, hours in, on
	// transient provider stalls that outlasted the retry). Every verdict is
	// appended to cfg.EvalCheckpoint as it lands; on restart the finished ids
	// are loaded and skipped, so a crash costs minutes, not the batch. The file
	// is DELETED on success — a deliberate rerun must re-classify, never reuse
	// stale verdicts. Empty path = disabled (the unit suites' path).
	done := map[int64]outcome{}
	ckptModel := ""
	if cfg.EvalCheckpoint != "" {
		if raw, err := os.ReadFile(cfg.EvalCheckpoint); err == nil {
			for _, line := range strings.Split(string(raw), "\n") {
				parts := strings.Split(line, "\t")
				if len(parts) < 4 {
					continue
				}
				id, err1 := strconv.ParseInt(parts[0], 10, 64)
				act, err2 := strconv.ParseBool(parts[1])
				ms, err3 := strconv.Atoi(parts[2])
				if err1 != nil || err2 != nil || err3 != nil {
					continue
				}
				done[id] = outcome{id: id, actionable: act, latencyMS: ms}
				if ckptModel == "" {
					ckptModel = parts[3]
				}
			}
		}
		if len(done) > 0 {
			fmt.Fprintf(w, "resumed %d verdicts from checkpoint %s\n\n", len(done), cfg.EvalCheckpoint)
		}
	}
	var ckpt *os.File
	if cfg.EvalCheckpoint != "" {
		f, err := os.OpenFile(cfg.EvalCheckpoint, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("open eval checkpoint %s: %w", cfg.EvalCheckpoint, err)
		}
		ckpt = f
		defer ckpt.Close()
	}
	// The model as the API REPORTED it, not as we asked for it. An eval whose
	// output does not name what it scored is a number nobody can reproduce, and
	// the server's own answer is the truthful source — it reflects what actually
	// ran, including a `:latest` the caller did not spell out.
	scoredModel := ckptModel
	var outcomes []outcome
	for _, m := range scored {
		if o, ok := done[m.MessageID]; ok {
			// Scored before the crash; the request would be byte-identical.
			outcomes = append(outcomes, o)
			continue
		}
		req := provider.Request{
			// Empty: the adapter uses the model it was CONSTRUCTED with, which
			// cmd/classify resolves once for both `run` and `eval`. Naming a model
			// here would be a second source of truth for the same fact.
			Model:      "",
			System:     cfg.Lane.System,
			User:       renderUser(m),
			SchemaName: SchemaName,
			Schema:     VerdictSchema,
			MaxTokens:  512,
		}
		resp, err := lane.Complete(ctx, req)
		if err != nil && ctx.Err() == nil {
			// ONE bounded retry per message. An eval is a multi-hour batch with
			// no checkpoint, and the 874-label residue run died 3h in on a
			// single transient `context deadline exceeded` (GPU contention
			// pushed one response past the client timeout; the message was 932
			// bytes). One transient transport fault must not scrap the batch.
			// One retry, not a loop: a hard-down provider should still fail
			// fast, and the caller's fix (restart ollama, rerun) needs to see
			// the error. The retried request is byte-identical, so verdict
			// integrity is unchanged.
			time.Sleep(15 * time.Second)
			resp, err = lane.Complete(ctx, req)
		}
		if err != nil {
			return fmt.Errorf("classify message %d: %w", m.MessageID, err)
		}
		var v verdict
		if err := json.Unmarshal(resp.Raw, &v); err != nil {
			return fmt.Errorf("parse verdict for message %d: %w", m.MessageID, err)
		}
		if scoredModel == "" {
			scoredModel = resp.Model
		}
		o := outcome{id: m.MessageID, actionable: v.Actionable, latencyMS: resp.LatencyMS}
		outcomes = append(outcomes, o)
		if ckpt != nil {
			// One line per verdict, flushed by the unbuffered write: the whole
			// point is surviving an abrupt death.
			if _, err := fmt.Fprintf(ckpt, "%d\t%t\t%d\t%s\n", o.id, o.actionable, o.latencyMS, resp.Model); err != nil {
				return fmt.Errorf("append eval checkpoint: %w", err)
			}
		}
	}
	if ckpt != nil {
		_ = ckpt.Close()
		if err := os.Remove(cfg.EvalCheckpoint); err != nil {
			fmt.Fprintf(w, "note: could not remove checkpoint %s: %v\n", cfg.EvalCheckpoint, err)
		}
	}

	var tp, fp, fn int
	var uniformTP, uniformFP, uniformN, uniformActionable int
	hasStrata := false
	var falseNegatives []int64
	lat := make([]int, 0, len(outcomes))
	for _, o := range outcomes {
		want := wantActionable[o.id]
		stratum := stratumOf[o.id]
		if stratum != "" {
			hasStrata = true
		}
		switch {
		case want && o.actionable:
			tp++
		case want && !o.actionable:
			fn++
			falseNegatives = append(falseNegatives, o.id)
		case !want && o.actionable:
			fp++
		}
		if stratum == "uniform" {
			uniformN++
			if want {
				uniformActionable++
			}
			if o.actionable {
				if want {
					uniformTP++
				} else {
					uniformFP++
				}
			}
		}
		lat = append(lat, o.latencyMS)
	}

	fmt.Fprintf(w, "classify eval — model %s — n=%d scored (%d labels in the file)\n",
		displayModel(scoredModel), len(outcomes), len(labels))
	if hasStrata {
		// The three lines of SWT-23 criterion 20, each saying what it is worth —
		// three bare numbers with no note beside them are three numbers a reader
		// will quote interchangeably.
		fmt.Fprintf(w, "  recall    %s   (%d of %d actionable caught; all strata — actionable-shaped mail is over-represented by design)\n",
			ratio(tp, tp+fn), tp, tp+fn)
		fmt.Fprintf(w, "  precision %s   (uniform stratum only: %d of %d flagged were actionable — the only precision that describes production)\n",
			ratio(uniformTP, uniformTP+uniformFP), uniformTP, uniformTP+uniformFP)
		fmt.Fprintf(w, "  base rate %d of %d uniform labels actionable — the number that decides this lane's future\n",
			uniformActionable, uniformN)
		fmt.Fprintf(w, "  median latency %d ms\n\n", median(lat))
	} else {
		// A strata-less set prints the SWT-22 output byte-for-byte: this is the
		// path every personal number was measured through, and a stratum
		// breakdown here would be lines computed over an empty uniform stratum.
		fmt.Fprintf(w, "  recall    %s   (%d of %d actionable messages caught)\n", ratio(tp, tp+fn), tp, tp+fn)
		fmt.Fprintf(w, "  precision %s   (%d of %d flagged were actionable)\n", ratio(tp, tp+fp), tp, tp+fp)
		fmt.Fprintf(w, "  median latency %d ms\n\n", median(lat))
	}

	// RECALL IS THE OBJECTIVE, so the misses are the output that matters. A score
	// without ids tells an operator that something is wrong and nothing about
	// what.
	fmt.Fprintf(w, "false negatives (%d) — labelled actionable, classified not:\n", len(falseNegatives))
	if len(falseNegatives) == 0 {
		fmt.Fprintln(w, "  none")
	}
	for _, id := range falseNegatives {
		subject := ""
		if m, ok := byID[id]; ok {
			subject = m.Subject
		}
		fmt.Fprintf(w, "  message %d  %s\n", id, subject)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Recall is the objective: a missed payment or fine notice is a late fee, a false alarm")
	fmt.Fprintln(w, "costs a second to dismiss. Tune against these labels, never against intuition — this")
	fmt.Fprintln(w, "fixture has been wrong before and the models were right.")
	return nil
}

// displayModel names the model in the header. An eval whose output does not say
// what it scored is a number nobody can reproduce.
func displayModel(model string) string {
	if model == "" {
		return "(model not reported)"
	}
	return model
}

func ratio(num, den int) string {
	if den == 0 {
		return "  n/a"
	}
	return fmt.Sprintf("%.2f", float64(num)/float64(den))
}

func median(xs []int) int {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int(nil), xs...)
	sort.Ints(s)
	return s[len(s)/2]
}

func joinIDs(ids []int64) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%d", id)
	}
	return out
}
