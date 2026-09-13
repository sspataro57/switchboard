package classify_test

// SWT-29 (docs/tickets/funnel-view_SPEC.md, criteria 11 and 12): the classify
// section of /funnel comes from a REFACTOR, not a re-spelling.
//
// classify.Summarize owns the verdict query, the link-state fold and the
// skipped-run fold; ReportForWorker keeps its signature and becomes a renderer
// over Summary. There is no second copy of any of the three anywhere — and,
// symmetrically, the report's PROSE does not move: the no-fallback note, the
// all-skipped sentence and the runbook pointer stay as string literals inside
// internal/classify/report.go, because TestReports_ShareTheNoFallbackNote scans
// THAT FILE's literals with a vacuity floor. Move them into summary.go and that
// test fatals on an empty scan rather than failing on a missing note, which is
// the least legible red this package can produce.
//
// Plain unit test — no build tag, no database, no network, and it references no
// symbol that does not exist yet, so it fails on ASSERTIONS rather than on a
// compile error. That is deliberate: the message below is the whole point.
//
// The numbers themselves are pinned in summary_integration_test.go, where
// Postgres produces them (its golden report is C6's "Summarize output is
// byte-identical" guard for the SWT-40 move below).

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/classify"
)

// Criterion 11: "ReportForWorker keeps its signature". cmd/classify report is
// untouched, and a compile-time pin says so more precisely than prose.
var _ func(context.Context, *pgxpool.Pool, io.Writer, time.Duration, string) error = classify.ReportForWorker

func csRead(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(name))
	if err != nil {
		t.Fatalf("read internal/classify/%s: %v", name, err)
	}
	if len(b) < 200 {
		t.Fatalf("internal/classify/%s is %d bytes; the scans below would pass vacuously", name, len(b))
	}
	return string(b)
}

// csOutsideComments ignores whole-line // comments only, so prose may quote the
// thing it explains (the upworkcrm keyspelling allowance).
func csOutsideComments(src, needle string) bool {
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

// ---- criterion 11: the SQL and the folds MOVED --------------------------------

func TestClassifySummary_OwnsTheQueriesAndTheFolds(t *testing.T) {
	summary := csRead(t, "summary.go")

	for _, want := range []string{"func Summarize(", "type Summary struct", "type Flag struct"} {
		if !strings.Contains(summary, want) {
			t.Errorf("internal/classify/summary.go has no %q. Criterion 11 names the seam: "+
				"Summarize(ctx, pool, since, workerType) (Summary, error)", want)
		}
	}
	// Criterion 13: sender and subject come from the STORED
	// ai_extractions.fields, exactly as the CLI report reads them. A join back
	// to normalized_messages for a second copy of those values is a second
	// source of truth for what the model was shown, and it silently disagrees
	// the moment a message is re-normalized.
	//
	// AMENDED BY SWT-33 (criterion 30, 2026-09-10), not relaxed: exactly ONE
	// join back is sanctioned — the inquiry lane's "still open" fold, which is
	// deliberately a statement about the world NOW (a later outbound message on
	// the thread) and not a second copy of what was classified.
	//
	// AMENDED AGAIN BY SWT-40 Part C (C-D7, criterion C6, 2026-09-12): that fold
	// MOVED to the leaf package internal/replyfold (replyfold.JoinSQL and
	// replyfold.RepliedSinceCol), so the inquiry promoter reads the same
	// spelling. Its identifier ALLOWLIST moved with it
	// (internal/replyfold/replyfold_test.go). What stays here: summary.go
	// declares no fold SQL of its own, references the two replyfold constants,
	// never names normalized_messages or the fold's aliases (t., lo.) outside
	// comments, and keeps its verdict select list pinned — so nothing it adds can
	// reach the joined row unqualified.
	for _, gone := range []string{"const repliedSinceSQL", "const repliedSinceCol"} {
		if strings.Contains(summary, gone) {
			t.Errorf("internal/classify/summary.go still declares %q. SWT-40 C-D7: the replied-since fold lives in "+
				"internal/replyfold, shared with the inquiry promoter — a second copy here drifts from the one "+
				"that decides promotion", gone)
		}
	}
	for _, want := range []string{"replyfold.JoinSQL", "replyfold.RepliedSinceCol"} {
		if !csOutsideComments(summary, want) {
			t.Errorf("internal/classify/summary.go does not reference %s. The inquiry report reads the SAME fold "+
				"the promoter gates on (C6)", want)
		}
	}
	aliasUse := regexp.MustCompile(`\b(t|lo)\.[a-z_]+`)
	for _, line := range strings.Split(summary, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		if aliasUse.MatchString(line) {
			t.Errorf("internal/classify/summary.go reaches the replied-since join's aliases:\n"+
				"\t%s\nThe joined row may only be read inside replyfold's constants", strings.TrimSpace(line))
		}
	}
	// UNQUALIFIED columns (SWT-33 second re-review): ai_runs and ai_extractions
	// carry no body_text, sender, subject or channel, so a bare column added to
	// the verdict select list would silently resolve to the joined t row on the
	// inquiry lane. So the select lists are pinned: the verdict query's is
	// exactly `e.fields, r.created_at, <replied>`, and there are two SELECTs
	// (verdicts, skipped runs) and no third.
	if !strings.Contains(summary, "q := `SELECT e.fields, r.created_at, ` + replied + `\n") {
		t.Errorf("internal/classify/summary.go's verdict query no longer selects exactly `e.fields, " +
			"r.created_at, ` + replied. Anything added there reaches the replied-since join's row unqualified")
	}
	selects := 0
	for _, line := range strings.Split(summary, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "//") {
			selects += strings.Count(line, "SELECT")
		}
	}
	if selects != 2 {
		t.Errorf("internal/classify/summary.go carries %d SELECT(s), want 2 "+
			"(the verdict query and the skipped-run query)", selects)
	}
	if csOutsideComments(summary, "normalized_messages") {
		t.Errorf("internal/classify/summary.go names normalized_messages. Criterion 13: the flag list's " +
			"sender and subject come from the stored ai_extractions.fields, and the one sanctioned join back " +
			"is replyfold's")
	}

	// The two queries and the four link states must live here now.
	for _, want := range []string{"ai_extractions", "ai_runs", "'skipped'", "link_candidates"} {
		if !csOutsideComments(summary, want) {
			t.Errorf("internal/classify/summary.go does not mention %q. Summarize owns the verdict query, "+
				"the link-state fold AND the skipped-run fold — reportSkipped's FOLD moves here; only its "+
				"PRINTING stays in report.go", want)
		}
	}
}

func TestClassifyReport_NoLongerHoldsASecondCopyOfTheQuery(t *testing.T) {
	report := csRead(t, "report.go")

	// POSITIVE CONTROL: report.go must still be the renderer, or the "no SQL
	// here" assertions below pass because the file was gutted.
	if !strings.Contains(report, "func ReportForWorker(") {
		t.Fatalf("POSITIVE CONTROL FAILED: internal/classify/report.go no longer defines ReportForWorker. " +
			"The refactor reduces it to rendering; it does not delete it (cmd/classify report calls it)")
	}
	if !strings.Contains(report, "fmt.Fprint") {
		t.Fatalf("POSITIVE CONTROL FAILED: report.go contains no fmt.Fprint block; the printing is what " +
			"stays here")
	}

	for _, banned := range []string{"FROM ai_extractions", "ai_extractions", "SELECT input FROM ai_runs", "pool.Query("} {
		if csOutsideComments(report, banned) {
			t.Errorf("internal/classify/report.go still contains %q. Criterion 11: the dashboard and the CLI "+
				"read ONE query. A second copy is how the page and `classify report --lane X --since Ndays` "+
				"come to print different numbers for the same window, and that equality is the whole claim "+
				"of the refactor", banned)
		}
	}
	if csOutsideComments(report, "func reportSkipped(ctx context.Context, pool") {
		t.Errorf("reportSkipped still takes a pool. Its FOLD moves into Summarize; what stays in report.go " +
			"is the printing, over the counts Summary already carries")
	}
}

// ---- criterion 12: the prose stays put ----------------------------------------

// These literals are load-bearing in two directions. They are what
// TestReports_ShareTheNoFallbackNote scans (and that scan FATALS below a
// 40-character floor, so moving them out produces a vacuity failure rather than
// a missing-note failure), and they are the half a counter cannot carry:
// nothing in the numbers distinguishes "the local box is off" from "nothing was
// actionable", and the fix a reader invents for the first is a fallback to the
// hosted lane — the one change the locality boundary exists to prevent.
func TestClassifyReport_KeepsItsStringLiteralsWhereTheScanLooks(t *testing.T) {
	report := csRead(t, "report.go")
	summary := csRead(t, "summary.go")

	stay := []string{
		"Classify shadow report (personal actionability)",
		"an all-skipped pass is EXPECTED when the local model is not running.",
		"provider is never the fix. See docs/runbooks/provider-locality.md.",
		"skipped: %d (never sent to any provider)",
		"why the lane refused",
		"why it was restricted",
		"flagged, newest first:",
	}
	for _, lit := range stay {
		if !strings.Contains(report, lit) {
			t.Errorf("internal/classify/report.go no longer contains the literal %q. Criterion 12: "+
				"ReportForWorker's text output is BYTE-IDENTICAL before and after the refactor, and these "+
				"literals must stay in this file — TestReports_ShareTheNoFallbackNote scans report.go's "+
				"string literals with a vacuity floor and would fatal, not fail, if they moved", lit)
		}
		if strings.Contains(summary, lit) {
			t.Errorf("internal/classify/summary.go also contains %q. One spelling: Summarize computes, "+
				"ReportForWorker prints. A note duplicated across the two drifts the first time one is "+
				"edited, and the report is the only place the operator reads it", lit)
		}
	}
}
