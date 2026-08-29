package classify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/sspataro57/switchboard/internal/provider"
	"github.com/sspataro57/switchboard/internal/textmatch"
)

// Label is one line of docs/evals/personal-actionability.jsonl.
//
// It carries NO MESSAGE CONTENT — not a subject, not a body, not a sender. The
// bodies are loaded from the database by id at eval time, so the labelled set is
// safe to commit while the mail it scores never leaves the machine. `Note` is
// free text and is documented as content-free for the same reason.
type Label struct {
	MessageID     int64  `json:"message_id"`
	Label         string `json:"label"` // "actionable" | "not"
	SubjectSHA256 string `json:"subject_sha256"`
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

// Eval scores the classifier against a hand-checked labelled set.
//
// It REFUSES to run on anything but the local lane. An eval on the hosted lane
// would be two failures at once: the whole labelled corpus — the most sensitive
// mail in the system, selected for being sensitive — posted to a hosted API, and
// a number that describes a model other than the one that will actually run.
//
// It also reports and EXCLUDES label drift. The labels are the fixture, and this
// fixture has already been wrong once: the spike's first eval scored every model
// 0.10–0.27 recall because the labels called "your statement is available"
// actionable while the prompt said informational notices were not. The models
// were right and the fixture was wrong. A silently re-pointed id would move the
// score with no visible cause, which is indistinguishable from the prompt
// getting better or worse.
func Eval(ctx context.Context, store Store, router *provider.Router, labels []Label, w io.Writer) error {
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
	msgs, err := store.MessagesByID(ctx, ids)
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

	// One verdict per surviving message, through the same request shape the
	// worker uses — a different one here would score a prompt nobody runs.
	type outcome struct {
		id         int64
		actionable bool
		latencyMS  int
	}
	var outcomes []outcome
	for _, m := range scored {
		resp, err := lane.Complete(ctx, provider.Request{
			Model:      "",
			System:     SystemPrompt,
			User:       renderUser(m),
			SchemaName: SchemaName,
			Schema:     VerdictSchema,
			MaxTokens:  512,
		})
		if err != nil {
			return fmt.Errorf("classify message %d: %w", m.MessageID, err)
		}
		var v verdict
		if err := json.Unmarshal(resp.Raw, &v); err != nil {
			return fmt.Errorf("parse verdict for message %d: %w", m.MessageID, err)
		}
		outcomes = append(outcomes, outcome{id: m.MessageID, actionable: v.Actionable, latencyMS: resp.LatencyMS})
	}

	var tp, fp, fn int
	var falseNegatives []int64
	lat := make([]int, 0, len(outcomes))
	for _, o := range outcomes {
		want := wantActionable[o.id]
		switch {
		case want && o.actionable:
			tp++
		case want && !o.actionable:
			fn++
			falseNegatives = append(falseNegatives, o.id)
		case !want && o.actionable:
			fp++
		}
		lat = append(lat, o.latencyMS)
	}

	fmt.Fprintf(w, "classify eval — n=%d scored (%d labels in the file)\n", len(outcomes), len(labels))
	fmt.Fprintf(w, "  recall    %s   (%d of %d actionable messages caught)\n", ratio(tp, tp+fn), tp, tp+fn)
	fmt.Fprintf(w, "  precision %s   (%d of %d flagged were actionable)\n", ratio(tp, tp+fp), tp, tp+fp)
	fmt.Fprintf(w, "  median latency %d ms\n\n", median(lat))

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
