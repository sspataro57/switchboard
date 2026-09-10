package classify

import (
	"context"

	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Report renders the shadow-mode verdicts from ai_extractions alone: what the
// classifier flagged and what it skipped. Deterministic — no LLM, no network
// beyond Postgres.
func Report(ctx context.Context, pool *pgxpool.Pool, w io.Writer, since time.Duration) error {
	return ReportForWorker(ctx, pool, w, since, LanePersonal.WorkerType)
}

// ReportForWorker renders the report for ONE lane's worker_type — "classify"
// (personal) or "classify_residue" (SWT-23). The two lanes' verdicts share the
// tables and are discriminated exactly here; a report that merged them would
// mix a measured prompt's numbers with an unmeasured one's.
func ReportForWorker(ctx context.Context, pool *pgxpool.Pool, w io.Writer, since time.Duration, workerType string) error {
	// SWT-29 criterion 11: the SQL and the folds live in Summarize — one query
	// serves this report and the /funnel page, so the two cannot print
	// different numbers for the same window. What stays here is the PRINTING,
	// byte-identical to the pre-refactor output (pinned by a characterization
	// test).
	s, err := Summarize(ctx, pool, since, workerType)
	if err != nil {
		return err
	}
	// The inquiry lane has its own printer (SWT-33 criterion 20) rather than a
	// branch inside this one, so the personal and residue text stays
	// byte-identical under its characterization test.
	if lane, ok := LaneByWorkerType(workerType); ok && lane.Name == LaneInquiry.Name {
		renderInquiryReport(w, s)
		return nil
	}

	var lines []string
	// allFlags, not Flags: the report prints EVERY flagged line, exactly as it
	// did before the refactor — the 50-row cap is the PAGE's (criterion 13),
	// and a residue window carries a thousand-plus flagged lines that a silent
	// truncation would eat (go-reviewer finding, 2026-09-08).
	for _, f := range s.allFlags {
		// The resolved URL on every flagged line (SWT-25 criterion 22) — that
		// is this ticket's usable-alone claim: a flagged notice is actionable
		// from the report instead of sending the reader back to the mailbox.
		// The placeholder matters too: an empty column reads as a rendering
		// bug, and no-candidates is the COMMON case.
		linkCol := "—"
		if f.LinkURL != "" {
			linkCol = f.LinkURL
		}
		// Sender and subject as well as the verdict (criterion 20): the title is
		// the model's summary, and an operator deciding whether to trust it needs
		// to see what it was summarising.
		lines = append(lines, fmt.Sprintf("  %s  #%-7d %-16s %-34s %-40s %s  %s",
			f.At.Format("2006-01-02 15:04"), f.MessageID, f.Kind,
			trunc(f.Sender, 34), trunc(f.Subject, 40), f.Title, linkCol))
	}

	fmt.Fprintln(w, "Classify shadow report (personal actionability)")
	fmt.Fprintf(w, "  classified: %d  flagged: %d\n", s.Classified, s.Flagged)
	if len(s.ByKind) > 0 {
		for _, k := range sortedKeys(s.ByKind) {
			fmt.Fprintf(w, "    by kind  %-18s %d\n", k, s.ByKind[k])
		}
	}
	if s.Classified > 0 {
		fmt.Fprintf(w, "  links  resolved: %d  declined (null): %d  none offered: %d  rejected: %d\n",
			s.LinkResolved, s.LinkDeclined, s.LinkNoneOffered, s.LinkRejected)
	}
	fmt.Fprintln(w)

	renderSkipped(w, s)

	if len(lines) > 0 {
		fmt.Fprintln(w, "flagged, newest first:")
		fmt.Fprintln(w, strings.Join(lines, "\n"))
		fmt.Fprintln(w)
	}
	return nil
}

// renderSkipped prints the lane the extraction join CANNOT see, in
// internal/triage/report.go's shape and vocabulary. The FOLD lives in
// Summarize (SWT-29); what stays here is the printing, over the counts Summary
// already carries.
//
// A refused message writes no extraction at all — that is what keeps "no
// permitted provider looked" structurally different from "the model looked and
// found nothing". The cost is that without this section a fully-skipped pass
// renders as `classified: 0`, which is indistinguishable from an empty inbox or
// a dead poller.
func renderSkipped(w io.Writer, s Summary) {
	if s.Skipped == 0 {
		return
	}
	renderSkipCounts(w, s)
	// The half a counter cannot carry. Nothing in the numbers distinguishes "the
	// local box is off" from "nothing was actionable", and the fix a reader
	// invents for the first is a fallback to the hosted lane — the one change the
	// boundary exists to prevent. By the time they open the runbook they have
	// already invented it, so it has to be said here.
	fmt.Fprintln(w, "    NOTE: an all-skipped pass is EXPECTED when the local model is not running.")
	fmt.Fprintln(w, "          Personal mail is only ever classified locally; falling back to a hosted")
	fmt.Fprintln(w, "          provider is never the fix. See docs/runbooks/provider-locality.md.")
	fmt.Fprintln(w)
}

// renderSkipCounts is the skipped section's counting half, shared by every
// lane's printer; the note that follows it is lane-specific.
func renderSkipCounts(w io.Writer, s Summary) {
	fmt.Fprintf(w, "  skipped: %d (never sent to any provider)\n", s.Skipped)
	for _, k := range sortedKeys(s.ByAvailReason) {
		fmt.Fprintf(w, "    why the lane refused   %-28s %d\n", k, s.ByAvailReason[k])
	}
	for _, k := range sortedKeys(s.ByClassReason) {
		fmt.Fprintf(w, "    why it was restricted  %-28s %d\n", k, s.ByClassReason[k])
	}
}

// renderInquiryReport prints the inquiry lane (SWT-33 criteria 20 and 30): the
// three open/answered states, every count by channel, and per flagged verdict
// the ask, the asker, the thread scope and the state. Every number comes from
// Summarize — the /funnel block renders the same Summary.
func renderInquiryReport(w io.Writer, s Summary) {
	fmt.Fprintln(w, "Classify shadow report (inquiry — does this need a reply from Salvador?)")
	fmt.Fprintf(w, "  classified: %d  flagged: %d\n", s.Classified, s.Flagged)
	fmt.Fprintf(w, "  open: %d  answered in thread: %d  spoke in conversation since: %d\n",
		s.Open, s.AnsweredInThread, s.SpokeInConversationSince)
	for _, k := range sortedKeys(s.ByKind) {
		fmt.Fprintf(w, "    by ask_kind  %-14s %d\n", k, s.ByKind[k])
	}
	if len(s.ByChannel) > 0 {
		// By channel (criterion 30): the diagnostic the rejected --channel flag
		// would have given — a bad number still says which message shape broke.
		fmt.Fprintln(w, "  by channel      classified  flagged   open  answered-in-thread  spoke-since  skipped")
		channels := make([]string, 0, len(s.ByChannel))
		for ch := range s.ByChannel {
			channels = append(channels, ch)
		}
		sort.Strings(channels)
		for _, ch := range channels {
			c := s.ByChannel[ch]
			fmt.Fprintf(w, "    %-13s %10d %8d %6d %19d %12d %8d\n", ch,
				c.Classified, c.Flagged, c.Open, c.AnsweredInThread, c.SpokeInConversationSince, c.Skipped)
		}
	}
	fmt.Fprintln(w, "  answered in thread = a later outbound on a thread-exact key (a reply in that thread);")
	fmt.Fprintln(w, "  spoke in conversation since = a later outbound anywhere in an unthreaded channel or DM,")
	fmt.Fprintln(w, "  a weaker claim that is never counted as answered.")
	fmt.Fprintln(w)

	if s.Skipped > 0 {
		renderSkipCounts(w, s)
		fmt.Fprintln(w, "    NOTE: an all-skipped pass is EXPECTED when the local model is not running.")
		fmt.Fprintln(w, "          This lane's class is pinned to restricted, so it is only ever classified")
		fmt.Fprintln(w, "          locally; falling back to a hosted provider is never the fix. See")
		fmt.Fprintln(w, "          docs/runbooks/local-classifier.md (the inquiry lane).")
		fmt.Fprintln(w)
	}

	if len(s.allFlags) > 0 {
		fmt.Fprintln(w, "flagged, newest first:")
		for _, f := range s.allFlags {
			fmt.Fprintf(w, "  %s  #%-7d %-10s %-6s %-24s %-28s %s  (asker: %s)  [thread_scope=%s]  %s\n",
				f.At.Format("2006-01-02 15:04"), f.MessageID, f.Kind, f.Channel,
				trunc(f.Sender, 24), trunc(f.Subject, 28), f.Title, f.Asker, f.ThreadScope, f.State)
		}
		fmt.Fprintln(w)
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
