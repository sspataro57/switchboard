package classify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
// a CLOSED vocabulary (LabelNoteAllowed), never free text, for the same reason.
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
	// One label per message, refused here rather than trusted to the loader
	// (Codex adversarial review, round 6): the sub-threshold refusal gates on
	// the SCORED count, so a file that repeated its rows could cross
	// EvalResultThreshold — and print ratios — with no new judgement behind
	// them. Every caller of this exported function gets the check, and it runs
	// BEFORE the router below is asked anything (round 7: Route probes the
	// local endpoint, which is I/O).
	ids := make([]int64, 0, len(labels))
	seenLabel := make(map[int64]bool, len(labels))
	for _, l := range labels {
		if seenLabel[l.MessageID] {
			return fmt.Errorf("the labelled set lists message %d more than once; one label per message — a "+
				"repeated row adds no judgement and would inflate the scored count past the ratio threshold",
				l.MessageID)
		}
		seenLabel[l.MessageID] = true
		ids = append(ids, l.MessageID)
	}
	// Refuse FIRST, before a single message is read or sent. The check is the
	// router's own answer for the class this worker's inbox always carries.
	lane, decision, reason := router.Route(ctx, provider.ClassRestricted)
	if decision != provider.DecideAllow || lane == nil {
		return fmt.Errorf("eval refused: the boundary did not permit the local lane (%s). "+
			"Scoring on the hosted lane would post the entire labelled corpus to a hosted API and "+
			"produce a number for a model that will never run", reason)
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
	blanket := map[int64]bool{}
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
		// The LANE's positive token (criterion 23): scored against
		// "actionable", every needs_reply label would count as a negative.
		wantActionable[l.MessageID] = l.Label == cfg.Lane.Contract.PositiveLabel
		stratumOf[l.MessageID] = l.Stratum
		blanket[l.MessageID] = l.Note == OwnerBlanketNote
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
	// ckptKey binds every checkpoint line to WHICH evaluation produced it
	// (Codex adversarial review, rounds 3 and 4): the lane's worker_type and
	// prompt version, a fingerprint of the system prompt and output schema
	// actually sent, the configured model, and every knob that shapes the
	// output (think, max tokens, context size). A checkpoint path reused across
	// lanes, prompts, models or budgets would otherwise feed verdicts to a
	// different evaluation into this score without re-asking. It is checked at
	// LOAD, before any request, so even a fully checkpointed resume — where no
	// request, and so no server-reported-model check below, would ever run — is
	// covered. A mismatch is refused, never merged.
	promptFP := sha256.Sum256([]byte(cfg.Lane.System + "\x00" + string(cfg.Lane.Contract.Schema)))
	ckptKey := fmt.Sprintf("%s/%s/%s/model=%s/think=%t/max=%d/ctx=%d",
		cfg.Lane.WorkerType, cfg.Lane.PromptVersion, hex.EncodeToString(promptFP[:])[:12],
		cfg.Model, cfg.Think, cfg.MaxTokens, cfg.NumCtx)
	if cfg.EvalCheckpoint != "" {
		if raw, err := os.ReadFile(cfg.EvalCheckpoint); err == nil {
			for _, line := range strings.Split(string(raw), "\n") {
				parts := strings.Split(line, "\t")
				if len(parts) < 4 {
					continue
				}
				if len(parts) < 5 || parts[4] != ckptKey {
					got := "(unbound: written before checkpoints recorded their evaluation)"
					if len(parts) >= 5 {
						got = parts[4]
					}
					return fmt.Errorf("checkpoint %s holds verdicts from a different evaluation (%s); this run is "+
						"%s. Delete the checkpoint (rescoring everything) or point --checkpoint at this "+
						"evaluation's own file", cfg.EvalCheckpoint, got, ckptKey)
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
			User:       renderUser(cfg.Lane, m),
			SchemaName: cfg.Lane.Contract.SchemaName,
			Schema:     cfg.Lane.Contract.Schema,
			MaxTokens:  cfg.MaxTokens,
			Think:      cfg.Think,
			NumCtx:     cfg.NumCtx,
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
			if errors.Is(err, provider.ErrIncomplete) {
				// The model exhausted its whole generation budget without
				// producing a verdict — with thinking on, this is a real
				// failure mode of the CONFIGURATION UNDER TEST, so it is
				// SCORED (as not-actionable: no verdict flags nothing, so a
				// wanted-actionable label becomes a false negative) rather
				// than aborting a multi-hour batch on one over-thinker.
				o := outcome{id: m.MessageID, actionable: false, latencyMS: 0}
				outcomes = append(outcomes, o)
				if ckpt != nil {
					model := scoredModel
					if model == "" {
						model = cfg.Model
					}
					if _, err := fmt.Fprintf(ckpt, "%d\t%t\t%d\t%s\t%s\n", o.id, o.actionable, o.latencyMS, model, ckptKey); err != nil {
						return fmt.Errorf("append eval checkpoint: %w", err)
					}
				}
				fmt.Fprintf(w, "note: message %d scored as a miss — %v\n", m.MessageID, err)
				continue
			}
			return fmt.Errorf("classify message %d: %w", m.MessageID, err)
		}
		// The lane's decision field (criterion 23): reading `actionable` off an
		// inquiry verdict decodes to false on every row — a total miss that
		// looks like a terrible prompt rather than a scorer on the wrong key.
		decision, err := decodeDecision(cfg.Lane, resp.Raw)
		if err != nil {
			return fmt.Errorf("parse verdict for message %d: %w", m.MessageID, err)
		}
		// A checkpoint from ANOTHER model must refuse, not merge: the header's
		// whole purpose is "what was this number measured on", and a resume
		// after a CLASSIFY_MODEL change would silently blend two models into
		// one score.
		if ckptModel != "" && resp.Model != ckptModel {
			return fmt.Errorf("checkpoint %s holds verdicts from model %s but the server reports %s; "+
				"delete the checkpoint (rescoring everything) or restore the model",
				cfg.EvalCheckpoint, ckptModel, resp.Model)
		}
		if scoredModel == "" {
			scoredModel = resp.Model
		}
		o := outcome{id: m.MessageID, actionable: decision, latencyMS: resp.LatencyMS}
		outcomes = append(outcomes, o)
		if ckpt != nil {
			// One line per verdict, flushed by the unbuffered write: the whole
			// point is surviving an abrupt death.
			if _, err := fmt.Fprintf(ckpt, "%d\t%t\t%d\t%s\t%s\n", o.id, o.actionable, o.latencyMS, resp.Model, ckptKey); err != nil {
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
	var blanketN, blanketFlagged, blanketUniformFlagged int
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
		if blanket[o.id] {
			blanketN++
			// Only a flag on a row labelled NEGATIVE is a possible bulk-labelled
			// ask; the line below says exactly that, so it counts exactly that.
			if o.actionable && !want {
				blanketFlagged++
				if stratum == "uniform" {
					blanketUniformFlagged++
				}
			}
		}
	}

	fmt.Fprintf(w, "classify eval — model %s — n=%d scored (%d labels in the file)\n",
		displayModel(scoredModel), len(outcomes), len(labels))
	// pos is the lane's positive token. Every line below names it rather than
	// "actionable": the same bytes on the personal and residue lanes, and on the
	// inquiry lane a count phrased in the lane's own question (D1).
	pos := cfg.Lane.Contract.PositiveLabel
	if len(outcomes) < EvalResultThreshold {
		// SWT-33 criterion 31 / D7. Below the threshold the eval prints COUNTS
		// and the marker and NO ratio-shaped number anywhere — not a rounder
		// one, not one in parentheses, none. A percentage at n=40 is noise that
		// reads exactly like a measurement once it is pasted somewhere else,
		// and this repo has already shipped a 25-29x cost error by quoting a
		// number out of the context that produced it. A convention did not stop
		// that; this refusal does.
		//
		// The counts are NOT withheld: the refusal replaces the ratio, it does
		// not delete the result.
		//
		// The vocabulary stays ("recall", "precision") so a reader looking for
		// those numbers finds them — what changes is that they are COUNTS, a
		// fraction of labelled messages, not a decimal that can be lifted out
		// of this output and quoted as a rate.
		//
		// EVERY lane, on the SCORED n, enforced HERE. The first cut scoped it to
		// the inquiry lane plus a caller-set flag, which any caller of this
		// exported function could leave unset (Codex adversarial re-review). The
		// measured lanes' own fixtures (280, 874; the personal file's minimum IS
		// the threshold) sit above it, so their published output is unchanged.
		// A stratified set keeps its three-line semantics — recall over all
		// strata, precision and base rate from the uniform stratum — as counts.
		if hasStrata {
			fmt.Fprintf(w, "  recall    %d of %d labelled %s caught (all strata — %s-shaped mail is over-represented by design; counts only)\n",
				tp, tp+fn, pos, pos)
			fmt.Fprintf(w, "  precision uniform stratum only: %d of %d flagged were labelled %s\n",
				uniformTP, uniformTP+uniformFP, pos)
			fmt.Fprintf(w, "  base rate %d of %d uniform labels %s\n", uniformActionable, uniformN, pos)
		} else {
			fmt.Fprintf(w, "  recall    %d of %d labelled %s caught (counts only, see below)\n",
				tp, tp+fn, pos)
			fmt.Fprintf(w, "  precision %d of %d flagged were labelled %s\n",
				tp, tp+fp, pos)
		}
		fmt.Fprintf(w, "  median latency %d ms\n", median(lat))
		fmt.Fprintf(w, "  %s (n=%d < %d)\n\n", EvalIndicativeMarker, len(outcomes), EvalResultThreshold)
	} else if hasStrata {
		// The three lines of SWT-23 criterion 20, each saying what it is worth —
		// three bare numbers with no note beside them are three numbers a reader
		// will quote interchangeably.
		fmt.Fprintf(w, "  recall    %s   (%d of %d %s caught; all strata — %s-shaped mail is over-represented by design)\n",
			ratio(tp, tp+fn), tp, tp+fn, pos, pos)
		fmt.Fprintf(w, "  precision %s   (uniform stratum only: %d of %d flagged were %s — the only precision that describes production)\n",
			ratio(uniformTP, uniformTP+uniformFP), uniformTP, uniformTP+uniformFP, pos)
		fmt.Fprintf(w, "  base rate %d of %d uniform labels %s — the number that decides this lane's future\n",
			uniformActionable, uniformN, pos)
		fmt.Fprintf(w, "  median latency %d ms\n\n", median(lat))
	} else {
		// A strata-less set prints the SWT-22 output byte-for-byte: this is the
		// path every personal number was measured through, and a stratum
		// breakdown here would be lines computed over an empty uniform stratum.
		fmt.Fprintf(w, "  recall    %s   (%d of %d %s messages caught)\n", ratio(tp, tp+fn), tp, tp+fn, pos)
		fmt.Fprintf(w, "  precision %s   (%d of %d flagged were %s)\n", ratio(tp, tp+fp), tp, tp+fp, pos)
		fmt.Fprintf(w, "  median latency %d ms\n\n", median(lat))
	}

	// Labels the owner set by a BLANKET instruction rather than one by one
	// (Codex adversarial review, round 5). They are his judgement and stay in
	// the score, but they may answer "already dealt with" rather than "was it an
	// ask when it arrived", so a flag on one can be a real ask scored as a false
	// positive. Printed on their own line so the counts above can be read with
	// that subtracted. Only printed when present, so the personal and residue
	// output (whose files carry no such note) is unchanged.
	//
	// On a STRATIFIED set the precision line is uniform-stratum only, so the
	// all-strata count cannot be subtracted from it (most enriched rows are the
	// model's own earlier flags); the uniform share is printed beside it, and
	// that is the number that corrects the precision line.
	if blanketN > 0 {
		if hasStrata {
			fmt.Fprintf(w, "owner-blanket labels: %d scored, %d labelled `not` and flagged (%d in the uniform "+
				"stratum — the share of the precision line's false positives that may be real asks labelled in "+
				"bulk)\n\n", blanketN, blanketFlagged, blanketUniformFlagged)
		} else {
			fmt.Fprintf(w, "owner-blanket labels: %d scored, %d labelled `not` and flagged — each may be a real "+
				"ask the owner labelled `not` in bulk\n\n", blanketN, blanketFlagged)
		}
	}

	// The misses, by id. A score without ids tells an operator that something is
	// wrong and nothing about what.
	fmt.Fprintf(w, "false negatives (%d) — labelled %s, classified not:\n", len(falseNegatives), pos)
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
	if cfg.Lane.Name == LaneInquiry.Name {
		// The inquiry lane's objective is the OPPOSITE of the other two
		// (criterion 8), and its closing note says so.
		fmt.Fprintln(w, "Precision is this lane's objective: a false \"someone is waiting on you\" costs trust in a")
		fmt.Fprintln(w, "surface meant to be relied on, and most of this conversation is chatter. Tune against")
		fmt.Fprintln(w, "these labels, never against intuition — and read the counts, not a rate, until the set")
		fmt.Fprintf(w, "reaches %d labels.\n", EvalResultThreshold)
		return nil
	}
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

// EvalResultThreshold is the number of scored labels below which Eval refuses
// to print a ratio (SWT-33 D7). It is ALSO the size of the stratified set the
// runbook commits to (uniform >= 80 + enriched >= 40) — one fact, so it gets
// one spelling: the labelled-set guard reads THIS constant rather than
// restating 120, or the two drift the first time the minimum is raised.
const EvalResultThreshold = 120

// EvalIndicativeMarker is the one spelling of the sub-threshold warning. Worded
// so it cannot be quoted out of context: the half that survives being pasted
// into a Jira comment is "this is not a measurement".
const EvalIndicativeMarker = "INDICATIVE ONLY — this is not a measurement"

// OwnerBlanketNotePrefix marks a label the owner set by blanket instruction
// rather than one by one (docs/evals/inquiry-needs-reply.jsonl's `note`). Eval
// reports those rows on their own line; one spelling, read here and written in
// the fixture.
const OwnerBlanketNotePrefix = "owner-blanket"

// OwnerBlanketNote is the exact note carried by a blanket-labelled row.
const OwnerBlanketNote = OwnerBlanketNotePrefix + ": labelled by the owner's blanket instruction, not one by one"

// LabelStratumAllowed says whether a label's `stratum` is in the closed
// vocabulary: absent, or one of the three strata the eval understands. One
// spelling, read by cmd/classify's loader (Codex adversarial review, round 9:
// an unconstrained string field is a place client text can ride along).
func LabelStratumAllowed(stratum string) bool {
	switch stratum {
	case "", "uniform", "enriched", "domain_gate":
		return true
	}
	return false
}

// LabelNoteAllowed says whether a label file's `note` value is in the CLOSED
// vocabulary. A note is never free text (Codex adversarial review, round 8):
// the labelled sets are committed, and a free-text field is one paste away
// from putting a client's message into git — the exact leak the ids-plus-hash
// format exists to prevent. Add a value here, deliberately, to allow it.
func LabelNoteAllowed(note string) bool {
	switch note {
	case "", OwnerBlanketNote:
		return true
	}
	return false
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
