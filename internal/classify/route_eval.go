package classify

// The route lane's eval (SWT-40 Part B, B-D7 and B10): the go-live gate for
// arming an account. The labels are the RULES tier's own answers — messages on
// the mailbox that capture rules attributed, with the answer hidden — so the
// label set is deterministic output, not Salvador's judgement, and every line
// carries `stratum: rules`. It is biased easy (a rule matched these for a
// reason), which is why the output lists EVERY disagreement by id: each one is
// read by hand before arming.
//
// The score is what route_apply would do for the message at steps 2-4 (single
// candidate, a grounded model choice, else the account's default); step 1 is a
// thread fact the label set does not freeze, so it is not scored. Counts
// always; a ratio only at EvalResultThreshold scored labels or more, with
// EvalIndicativeMarker below that — one spelling of "no ratio below 120".
//
// Like every eval it writes NO ai_runs rows: a historical verdict injected with
// a fresh timestamp would pass the route inbox's arming cutover.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sspataro57/switchboard/internal/provider"
)

// routeUnrouted is the predicted class of a message the tier would leave
// unmatched (no grounded choice and no default).
const routeUnrouted = "(unrouted)"

// routeNoVerdict is the predicted class when the model produced no verdict at
// all within its budget (provider.ErrIncomplete): scored as a miss.
const routeNoVerdict = "(no verdict)"

// routeUnparseable is the predicted class when the model answered but the
// answer does not decode as a route verdict: scored as a miss and counted,
// never an abort of a multi-hour batch.
const routeUnparseable = "(unparseable)"

// routeCompleter is the one method evalRoute needs from the local lane.
type routeCompleter interface {
	Complete(ctx context.Context, req provider.Request) (provider.Response, error)
}

// RoutePredict is the steps 2-4 subset of B-D2 the eval scores: one candidate →
// it (single); a resolved, grounded model choice → it (model); else the
// account's default (default); else unrouted. It returns the chosen slug and
// the step.
func RoutePredict(cands []RouteCandidate, idx *int, evidence, subject, body string) (string, string) {
	if len(cands) == 1 {
		return cands[0].Slug, "single"
	}
	if c, ok := ResolveCandidate(cands, idx); ok && Grounded(evidence, subject, body) {
		return c.Slug, "model"
	}
	for _, c := range cands {
		if c.IsDefault {
			return c.Slug, "default"
		}
	}
	return routeUnrouted, ""
}

func evalRoute(ctx context.Context, store Store, lane routeCompleter, cfg Config, labels []Label,
	ids []int64, w io.Writer) error {
	msgs, err := store.MessagesByID(ctx, cfg, ids)
	if err != nil {
		return fmt.Errorf("load labelled messages: %w", err)
	}
	byID := make(map[int64]PendingMessage, len(msgs))
	for _, m := range msgs {
		byID[m.MessageID] = m
	}

	var scored []PendingMessage
	want := map[int64]string{}
	var missing, drifted, noCandidates []int64
	for _, l := range labels {
		m, ok := byID[l.MessageID]
		if !ok {
			missing = append(missing, l.MessageID)
			continue
		}
		if SubjectHash(m.Subject) != l.SubjectSHA256 {
			drifted = append(drifted, l.MessageID)
			continue
		}
		if len(m.Candidates) == 0 {
			// The account lost its candidate rows since the label was written: the
			// tier would not route this message at all, so there is nothing to score.
			noCandidates = append(noCandidates, l.MessageID)
			continue
		}
		scored = append(scored, m)
		want[l.MessageID] = l.Label
	}
	if len(missing) > 0 || len(drifted) > 0 || len(noCandidates) > 0 {
		fmt.Fprintf(w, "label drift: %d excluded\n", len(missing)+len(drifted)+len(noCandidates))
		if len(drifted) > 0 {
			fmt.Fprintf(w, "  subject hash mismatch (excluded): %s\n", joinIDs(drifted))
		}
		if len(missing) > 0 {
			fmt.Fprintf(w, "  not found in the database (excluded): %s\n", joinIDs(missing))
		}
		if len(noCandidates) > 0 {
			fmt.Fprintf(w, "  receiving account has no route candidates (excluded): %s\n", joinIDs(noCandidates))
		}
		fmt.Fprintln(w)
	}

	type outcome struct {
		id          int64
		label, pred string
		step        string
		latencyMS   int
	}
	// CHECKPOINT: the generic eval's per-item resume (eval.go), in the route
	// lane's own line shape — id, predicted class, step, latency, model, and the
	// evaluation key evalCheckpointKey spells. Loaded and key-checked before any
	// request; a line from another evaluation (a generic-lane line included,
	// which has no sixth field) is refused, never merged. Removed on success.
	ckptKey := evalCheckpointKey(cfg)
	done := map[int64]outcome{}
	ckptModel := ""
	if cfg.EvalCheckpoint != "" {
		if raw, err := os.ReadFile(cfg.EvalCheckpoint); err == nil {
			for _, line := range strings.Split(string(raw), "\n") {
				parts := strings.Split(line, "\t")
				if len(parts) < 4 {
					continue
				}
				if len(parts) < 6 || parts[5] != ckptKey {
					got := "(not a route-lane checkpoint line)"
					if len(parts) >= 6 {
						got = parts[5]
					}
					return fmt.Errorf("checkpoint %s holds verdicts from a different evaluation (%s); this run is "+
						"%s. Delete the checkpoint (rescoring everything) or point --checkpoint at this "+
						"evaluation's own file", cfg.EvalCheckpoint, got, ckptKey)
				}
				id, err1 := strconv.ParseInt(parts[0], 10, 64)
				ms, err2 := strconv.Atoi(parts[3])
				if err1 != nil || err2 != nil {
					continue
				}
				done[id] = outcome{id: id, pred: parts[1], step: parts[2], latencyMS: ms}
				if ckptModel == "" {
					ckptModel = parts[4]
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

	var outcomes []outcome
	scoredModel := ckptModel
	// record scores one outcome and appends it to the checkpoint, one unbuffered
	// line per verdict: the point is surviving an abrupt death.
	record := func(o outcome, model string) error {
		outcomes = append(outcomes, o)
		if ckpt == nil {
			return nil
		}
		if model == "" {
			model = scoredModel
		}
		if model == "" {
			model = cfg.Model
		}
		if _, err := fmt.Fprintf(ckpt, "%d\t%s\t%s\t%d\t%s\t%s\n", o.id, o.pred, o.step, o.latencyMS, model, ckptKey); err != nil {
			return fmt.Errorf("append eval checkpoint: %w", err)
		}
		return nil
	}
	for _, m := range scored {
		if o, ok := done[m.MessageID]; ok {
			// Scored before the crash; the request would be byte-identical.
			o.label = want[m.MessageID]
			outcomes = append(outcomes, o)
			continue
		}
		req := provider.Request{
			Model:      "", // the adapter's constructed model, as the generic eval
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
			// ONE bounded retry, the generic eval's rule: a transient stall must
			// not scrap the batch; a hard-down provider still fails fast.
			time.Sleep(15 * time.Second)
			resp, err = lane.Complete(ctx, req)
		}
		if err != nil {
			if errors.Is(err, provider.ErrIncomplete) {
				if err := record(outcome{id: m.MessageID, label: want[m.MessageID], pred: routeNoVerdict}, ""); err != nil {
					return err
				}
				fmt.Fprintf(w, "note: message %d scored as a miss — %v\n", m.MessageID, err)
				continue
			}
			// A transport failure that outlived the retry still fails fast (a
			// hard-down provider must be seen); the checkpoint keeps every verdict
			// that landed, so a rerun with the same --checkpoint resumes.
			return fmt.Errorf("classify message %d: %w", m.MessageID, err)
		}
		// A checkpoint from ANOTHER model must refuse, not merge.
		if ckptModel != "" && resp.Model != ckptModel {
			return fmt.Errorf("checkpoint %s holds verdicts from model %s but the server reports %s; "+
				"delete the checkpoint (rescoring everything) or restore the model",
				cfg.EvalCheckpoint, ckptModel, resp.Model)
		}
		if scoredModel == "" {
			scoredModel = resp.Model
		}
		var v routeVerdict
		if err := json.Unmarshal(resp.Raw, &v); err != nil {
			if err := record(outcome{id: m.MessageID, label: want[m.MessageID], pred: routeUnparseable,
				latencyMS: resp.LatencyMS}, resp.Model); err != nil {
				return err
			}
			fmt.Fprintf(w, "note: message %d scored as a miss — unparseable verdict: %v\n", m.MessageID, err)
			continue
		}
		pred, step := RoutePredict(m.Candidates, v.ProjectIndex, v.Evidence, m.Subject, m.BodyText)
		if err := record(outcome{id: m.MessageID, label: want[m.MessageID], pred: pred, step: step,
			latencyMS: resp.LatencyMS}, resp.Model); err != nil {
			return err
		}
	}
	if ckpt != nil {
		_ = ckpt.Close()
		if err := os.Remove(cfg.EvalCheckpoint); err != nil {
			fmt.Fprintf(w, "note: could not remove checkpoint %s: %v\n", cfg.EvalCheckpoint, err)
		}
	}

	// The multi-class count table: rows are the rules' labels, columns what the
	// routing tier would do.
	table := map[string]map[string]int{}
	labelSet, predSet := map[string]bool{}, map[string]bool{}
	agree := 0
	var disagreements []outcome
	lat := make([]int, 0, len(outcomes))
	for _, o := range outcomes {
		if table[o.label] == nil {
			table[o.label] = map[string]int{}
		}
		table[o.label][o.pred]++
		labelSet[o.label], predSet[o.pred] = true, true
		if o.label == o.pred {
			agree++
		} else {
			disagreements = append(disagreements, o)
		}
		lat = append(lat, o.latencyMS)
	}
	labelsSorted, predsSorted := sortedSet(labelSet), sortedSet(predSet)

	fmt.Fprintf(w, "classify eval (route) — model %s — n=%d scored (%d labels in the file)\n",
		displayModel(scoredModel), len(outcomes), len(labels))
	fmt.Fprintln(w, "  labels are the rules tier's attributions (stratum rules): deterministic, and biased easy")
	fmt.Fprintf(w, "  %-28s", "label \\ routed")
	for _, p := range predsSorted {
		fmt.Fprintf(w, " %14s", trunc(p, 14))
	}
	fmt.Fprintln(w)
	for _, l := range labelsSorted {
		fmt.Fprintf(w, "  %-28s", trunc(l, 28))
		for _, p := range predsSorted {
			fmt.Fprintf(w, " %14d", table[l][p])
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w)
	enough := len(outcomes) >= EvalResultThreshold
	fmt.Fprintln(w, "  agreement per project (label = routed):")
	for _, l := range labelsSorted {
		n := 0
		for _, c := range table[l] {
			n += c
		}
		if enough {
			fmt.Fprintf(w, "    %-28s %d of %d  %s\n", l, table[l][l], n, ratio(table[l][l], n))
		} else {
			fmt.Fprintf(w, "    %-28s %d of %d\n", l, table[l][l], n)
		}
	}
	if enough {
		fmt.Fprintf(w, "  agreement %s   (%d of %d scored)\n", ratio(agree, len(outcomes)), agree, len(outcomes))
	} else {
		fmt.Fprintf(w, "  agreement %d of %d scored (counts only)\n", agree, len(outcomes))
	}
	// Counted over the MERGED outcomes, so a miss resumed from the checkpoint
	// still shows on this line.
	var noVerdict, unparseable int
	for _, o := range outcomes {
		switch o.pred {
		case routeNoVerdict:
			noVerdict++
		case routeUnparseable:
			unparseable++
		}
	}
	fmt.Fprintf(w, "  misses: %d no verdict, %d unparseable (scored as disagreements, never dropped)\n",
		noVerdict, unparseable)
	fmt.Fprintln(w, "  overall agreement is inflated by default fallbacks: a message the model leaves unrouted lands on")
	fmt.Fprintln(w, "  the default and agrees with every default-project label for free. Read the non-default rows.")
	fmt.Fprintf(w, "  median latency %d ms\n", median(lat))
	if !enough {
		fmt.Fprintf(w, "  %s (n=%d < %d)\n", EvalIndicativeMarker, len(outcomes), EvalResultThreshold)
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "disagreements (%d) — read every one by hand before arming (B-D7):\n", len(disagreements))
	if len(disagreements) == 0 {
		fmt.Fprintln(w, "  none")
	}
	for _, o := range disagreements {
		subject := ""
		if m, ok := byID[o.id]; ok {
			subject = m.Subject
		}
		step := o.step
		if step == "" {
			step = "-"
		}
		fmt.Fprintf(w, "  message %d  label %s  routed %s (%s)  %s\n", o.id, o.label, o.pred, step, subject)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "The labels are the rules' answers, not a human's: agreement here says the tier matches the rules")
	fmt.Fprintln(w, "where the rules could decide. It says nothing about the messages the rules could not decide —")
	fmt.Fprintln(w, "skim the dry-run list of real routes for those.")
	return nil
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
