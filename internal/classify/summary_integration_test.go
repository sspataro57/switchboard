//go:build integration

package classify_test

// SWT-29 (docs/tickets/funnel-view_SPEC.md, criteria 10-13): classify.Summarize
// against a real database, plus the characterization pin that ReportForWorker's
// text does not change while its guts move.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run ClassifySummary ./internal/classify/
//
// WHY INTEGRATION. Every number here is a GROUP-BY-shaped fold over ai_runs and
// ai_extractions. A fake supplying the rows would be supplying the very values
// the fold is supposed to compute — SWT-21's 6th landmine, and the reason this
// repo's rule is that a predicate (or an aggregate) whose input comes from a
// COLUMN gets its regression test here. The pure halves are unit-tested where
// they belong.
//
// CROSS-POLLUTION PACT (IK, "integration suites cross-pollute"). ai_runs and
// ai_extractions are shared with the links, residue and store suites under
// `-p 1`. So:
//   - the LANE assertions are DELTAS (summarize before, seed, summarize after);
//     an absolute "flagged == 1" would be a flake, not a check;
//   - the flag CAP and the golden report use worker_types this suite owns
//     outright, because both need an absolute answer;
//   - every seeded ai_runs row carries input->>'itest' = 'swt29-summary' and
//     cleanup keys on it, at start AND at end.
//
// ANTI-DATE-ROT: every lane row is seeded at now(); the two golden rows carry
// FIXED historical instants on purpose — they are DATA (the instant a verdict
// was produced), not freshness, and the golden is read with since=0 so no
// window filter touches them.
//
// GREENFIELD NOTE — EXPECTED RED. classify.Summarize, classify.Summary and
// classify.Flag do not exist, so this file compile-FAILS the integration build
// of internal/classify until summary.go lands.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	sumMarker = "swt29-summary"
	// Two worker_types this suite owns. They are NOT lanes and must never be
	// mistaken for a third one (internal/classify/lane.go: there are EXACTLY
	// TWO lanes); they exist so an absolute assertion has an isolated
	// population to be absolute about.
	sumGoldenWorker = "itest-swt29-golden"
	sumCapWorker    = "itest-swt29-cap"
)

type sumSuite struct{ pool *pgxpool.Pool }

func newSumSuite(t *testing.T, ctx context.Context) *sumSuite {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use the compose db on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	s := &sumSuite{pool: pool}
	s.cleanup(t, ctx)
	t.Cleanup(func() { s.cleanup(t, context.Background()) })
	return s
}

func (s *sumSuite) cleanup(t *testing.T, ctx context.Context) {
	t.Helper()
	const owned = `(SELECT id FROM ai_runs WHERE input->>'itest' = '` + sumMarker + `')`
	for _, q := range []string{
		`DELETE FROM ai_extractions WHERE ai_run_id IN ` + owned,
		`DELETE FROM ai_runs WHERE input->>'itest' = '` + sumMarker + `'`,
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

// run inserts one ai_runs row at now() and returns its id.
func (s *sumSuite) run(t *testing.T, ctx context.Context, workerType, status, extraInput string) int64 {
	t.Helper()
	input := `{"itest":"` + sumMarker + `"}`
	if extraInput != "" {
		input = `{"itest":"` + sumMarker + `",` + extraInput + `}`
	}
	var id int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO ai_runs (worker_type, provider, model, input, status)
		 VALUES ($1,'ollama','qwen3:8b',$2::jsonb,$3) RETURNING id`, workerType, input, status).Scan(&id); err != nil {
		t.Fatalf("insert ai_run(%s,%s): %v", workerType, status, err)
	}
	return id
}

func (s *sumSuite) extraction(t *testing.T, ctx context.Context, runID int64, fields string) {
	t.Helper()
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields) VALUES ($1,NULL,$2::jsonb)`,
		runID, fields); err != nil {
		t.Fatalf("insert ai_extraction: %v", err)
	}
}

func sumSummarize(t *testing.T, ctx context.Context, pool *pgxpool.Pool, since time.Duration, workerType string) classify.Summary {
	t.Helper()
	got, err := classify.Summarize(ctx, pool, since, workerType)
	if err != nil {
		t.Fatalf("classify.Summarize(%s): %v", workerType, err)
	}
	return got
}

// ---- criteria 10 + 11: both lanes, discriminated, one fold --------------------

// The two lanes are seeded IDENTICALLY except for worker_type, and each lane's
// summary is asserted to move by its own fixture and by nothing else. That is
// the whole content of criterion 10's "BOTH lanes, never as the string
// literals": the discrimination is real, and a summary that merged them would
// mix a measured prompt's numbers with an unmeasured one's.
func TestClassifySummary_Integration_BothLanesAndNeitherLeaksIntoTheOther(t *testing.T) {
	ctx := context.Background()
	s := newSumSuite(t, ctx)
	const since = time.Hour

	lanes := []struct {
		lane   classify.Lane
		title  string
		sender string
		subj   string
	}{
		{classify.LanePersonal, "itest-swt29 personal flag", "bills@swt29-personal.example", "Amount due 12 Sep"},
		{classify.LaneResidue, "itest-swt29 residue flag", "alerts@swt29-residue.example", "Action required"},
	}

	before := map[string]classify.Summary{}
	for _, l := range lanes {
		before[l.lane.WorkerType] = sumSummarize(t, ctx, s.pool, since, l.lane.WorkerType)
	}

	for _, l := range lanes {
		// One ok run with TWO verdicts: one flagged, one not. Criterion 10's
		// "classified" counts both; "flagged" counts one.
		okRun := s.run(t, ctx, l.lane.WorkerType, "ok", "")
		s.extraction(t, ctx, okRun, fmt.Sprintf(`{"actionable":true,"kind":"payment_due","title":%q,
		    "reason":"itest","sender":%q,"subject":%q,"normalized_message_id":8801,
		    "link_candidates":2,"link_index":1,"link_url":"https://swt29.example/pay","link_text":"PAY NOW"}`,
			l.title, l.sender, l.subj))
		s.extraction(t, ctx, okRun, `{"actionable":false,"kind":"informational","title":"itest-swt29 quiet",
		    "reason":"itest","sender":"noreply@swt29.example","subject":"Newsletter",
		    "normalized_message_id":8802,"link_candidates":0}`)
		// A skipped run: the lane the extraction join CANNOT see. Without it a
		// fully-skipped pass renders as classified: 0, which is
		// indistinguishable from an empty inbox or a dead poller.
		s.run(t, ctx, l.lane.WorkerType, "skipped",
			`"avail_reasons":{"itest_swt29_provider_unreachable":3},"class_reasons":{"itest_swt29_restricted":3},"skipped_count":3`)
	}

	for _, l := range lanes {
		got := sumSummarize(t, ctx, s.pool, since, l.lane.WorkerType)
		was := before[l.lane.WorkerType]

		if got.WorkerType != l.lane.WorkerType {
			t.Errorf("Summary.WorkerType = %q for lane %q, want %q — the summary says which population it "+
				"is about, or two lanes rendered side by side are unlabelled",
				got.WorkerType, l.lane.Name, l.lane.WorkerType)
		}
		if d := got.Classified - was.Classified; d != 2 {
			t.Errorf("lane %s: classified moved by %d, want 2", l.lane.Name, d)
		}
		if d := got.Flagged - was.Flagged; d != 1 {
			t.Errorf("lane %s: flagged moved by %d, want 1", l.lane.Name, d)
		}
		if d := got.ByKind["payment_due"] - was.ByKind["payment_due"]; d != 1 {
			t.Errorf("lane %s: by-kind payment_due moved by %d, want 1", l.lane.Name, d)
		}
		if d := got.ByKind["informational"] - was.ByKind["informational"]; d != 1 {
			t.Errorf("lane %s: by-kind informational moved by %d, want 1", l.lane.Name, d)
		}
		// The four link states of SWT-25 criterion 21: a counter that cannot
		// tell "nothing to offer" from "the model declined" from "the model
		// answered nonsense" is an alarm nobody can read.
		if d := got.LinkResolved - was.LinkResolved; d != 1 {
			t.Errorf("lane %s: link resolved moved by %d, want 1", l.lane.Name, d)
		}
		if d := got.LinkNoneOffered - was.LinkNoneOffered; d != 1 {
			t.Errorf("lane %s: link none-offered moved by %d, want 1", l.lane.Name, d)
		}
		if d := got.Skipped - was.Skipped; d != 3 {
			t.Errorf("lane %s: skipped moved by %d, want 3 (skipped_count, not one per row)", l.lane.Name, d)
		}
		if got.ByAvailReason["itest_swt29_provider_unreachable"] != 3 {
			t.Errorf("lane %s: ByAvailReason lost the breakdown: %v. A pass can refuse for more than one "+
				"reason, and filing the whole count under the dominant one prints a wrong number in the "+
				"one place an operator looks", l.lane.Name, got.ByAvailReason)
		}
		if got.ByClassReason["itest_swt29_restricted"] != 3 {
			t.Errorf("lane %s: ByClassReason lost the breakdown: %v", l.lane.Name, got.ByClassReason)
		}

		// Criterion 13: sender/subject/title come from the STORED fields. No
		// normalized_messages row exists for id 8801 in this fixture, so a join
		// back for a second copy could only produce blanks.
		var found *classify.Flag
		for i := range got.Flags {
			if got.Flags[i].Title == l.title {
				found = &got.Flags[i]
				break
			}
		}
		if found == nil {
			t.Fatalf("lane %s: the flagged verdict %q is not in Summary.Flags", l.lane.Name, l.title)
		}
		if found.Sender != l.sender || found.Subject != l.subj {
			t.Errorf("lane %s flag = {sender:%q subject:%q}, want {%q %q} — read from ai_extractions.fields, "+
				"exactly as the CLI report reads them", l.lane.Name, found.Sender, found.Subject, l.sender, l.subj)
		}
		if found.Kind != "payment_due" || found.MessageID != 8801 {
			t.Errorf("lane %s flag = %+v, want kind payment_due and normalized_message_id 8801", l.lane.Name, *found)
		}
		if found.LinkURL != "https://swt29.example/pay" {
			t.Errorf("lane %s flag LinkURL = %q, want the resolved URL. A flagged notice has to be "+
				"actionable from the page instead of sending the reader back to the mailbox", l.lane.Name, found.LinkURL)
		}
		if found.At.IsZero() {
			t.Errorf("lane %s flag has no timestamp; the list is newest-first and a row with no time cannot "+
				"be placed in it", l.lane.Name)
		}
	}

	// No leakage, in both directions. The worker_type values differ precisely so
	// one lane's verdicts are invisible to the other (IK, residue lane).
	personal := sumSummarize(t, ctx, s.pool, since, classify.LanePersonal.WorkerType)
	residue := sumSummarize(t, ctx, s.pool, since, classify.LaneResidue.WorkerType)
	for _, c := range []struct {
		summary classify.Summary
		alien   string
		lane    string
	}{
		{personal, lanes[1].title, "personal"},
		{residue, lanes[0].title, "residue"},
	} {
		for _, f := range c.summary.Flags {
			if f.Title == c.alien {
				t.Errorf("the %s lane's summary contains the OTHER lane's verdict %q. The two lanes are "+
					"discriminated by worker_type and nothing else; merging them mixes a measured prompt's "+
					"numbers with an unmeasured one's", c.lane, c.alien)
			}
		}
		if c.summary.ByAvailReason["itest_swt29_provider_unreachable"] != 3 {
			t.Errorf("the %s lane's skipped breakdown is %v; each lane counts only its own skipped runs",
				c.lane, c.summary.ByAvailReason)
		}
	}
}

// ---- criterion 13: the cap and the ordering ----------------------------------

// 50 per lane, newest first. reportListLimit = 20 exists in capture for the same
// reason ("a report that prints 16,000 unmatched senders is one nobody reads");
// 50 fits a page and stays scannable. An UNCAPPED list is not a cosmetic
// problem: /funnel renders both lanes on one page, so the cap is what keeps a
// bad classify day from producing a page nobody can read.
func TestClassifySummary_Integration_FlagsAreCappedAtFiftyNewestFirst(t *testing.T) {
	ctx := context.Background()
	s := newSumSuite(t, ctx)

	for i := 0; i < 52; i++ {
		run := s.run(t, ctx, sumCapWorker, "ok", "")
		s.extraction(t, ctx, run, fmt.Sprintf(`{"actionable":true,"kind":"deadline",
		    "title":"itest-swt29 cap %02d","reason":"itest","sender":"cap@swt29.example",
		    "subject":"Cap %02d","normalized_message_id":%d,"link_candidates":0}`, i, i, 9000+i))
	}

	got := sumSummarize(t, ctx, s.pool, time.Hour, sumCapWorker)
	if got.Flagged != 52 {
		t.Errorf("Flagged = %d, want 52 — the COUNT is not capped, only the list is. Capping the count "+
			"would make a bad day look like a normal one", got.Flagged)
	}
	if len(got.Flags) != 50 {
		t.Fatalf("len(Flags) = %d, want 50 (criterion 13's cap)", len(got.Flags))
	}
	for i := 1; i < len(got.Flags); i++ {
		if got.Flags[i].At.After(got.Flags[i-1].At) {
			t.Fatalf("Flags are not newest-first: [%d] %s precedes [%d] %s",
				i-1, got.Flags[i-1].At, i, got.Flags[i].At)
		}
	}
	// The cap keeps the NEWEST, not an arbitrary 50: the two dropped rows must
	// be the oldest two.
	for _, f := range got.Flags {
		if f.Title == "itest-swt29 cap 00" || f.Title == "itest-swt29 cap 01" {
			t.Errorf("the cap dropped a NEWER row and kept %q; the list is the 50 newest", f.Title)
		}
	}
}

// ---- criterion 12: the report's text is unchanged -----------------------------

// A CHARACTERIZATION test. It says nothing about whether the format is good; it
// says the refactor did not change it. `cmd/classify report` is a surface
// Salvador reads by hand and diffs against the page (Verification step 4), so a
// silent whitespace or column-width change would break the one equality that
// makes criterion 11's claim checkable.
//
// The fixture uses its own worker_type and `since = 0` (whole history) so the
// rendered text is a pure function of these three rows — no other suite's
// classify verdicts can enter it. The two run timestamps are fixed instants
// because they are DATA; the ONE place they surface in the output is
// substituted below rather than hard-coded, since the report formats them in
// the process's local zone and a golden that pinned a zone would fail on a
// machine set to another one.
func TestClassifyReport_Integration_TextIsUnchangedByTheSummarizeRefactor(t *testing.T) {
	ctx := context.Background()
	s := newSumSuite(t, ctx)

	mkRun := func(at, status, extraInput string) int64 {
		t.Helper()
		input := `{"itest":"` + sumMarker + `"}`
		if extraInput != "" {
			input = `{"itest":"` + sumMarker + `",` + extraInput + `}`
		}
		var id int64
		if err := s.pool.QueryRow(ctx,
			`INSERT INTO ai_runs (worker_type, provider, model, input, status, created_at)
			 VALUES ($1,'ollama','qwen3:8b',$2::jsonb,$3,$4::timestamptz) RETURNING id`,
			sumGoldenWorker, input, status, at).Scan(&id); err != nil {
			t.Fatalf("insert golden ai_run: %v", err)
		}
		return id
	}

	const flaggedAt = "2026-03-04 05:06:07+00"
	flaggedRun := mkRun(flaggedAt, "ok", "")
	s.extraction(t, ctx, flaggedRun, `{"actionable":true,"kind":"payment_due","title":"Golden alpha",
	    "reason":"the words that decided it","sender":"notices@golden.example","subject":"Amount due",
	    "normalized_message_id":4242,"link_candidates":2,"link_index":1,
	    "link_url":"https://golden.example/pay","link_text":"PAY NOW"}`)

	quietRun := mkRun("2026-03-03 05:06:07+00", "ok", "")
	s.extraction(t, ctx, quietRun, `{"actionable":false,"kind":"informational","title":"Golden bravo",
	    "reason":"marketing","sender":"news@golden.example","subject":"This week",
	    "normalized_message_id":4243,"link_candidates":0}`)

	mkRun("2026-03-02 05:06:07+00", "skipped",
		`"avail_reasons":{"provider_unreachable":3},"class_reasons":{"restricted":3},"skipped_count":3`)

	// The instant as Postgres hands it back, formatted the way the report
	// formats it. Read from the database rather than parsed here so the
	// substitution cannot disagree with the row.
	var at time.Time
	if err := s.pool.QueryRow(ctx, `SELECT created_at FROM ai_runs WHERE id=$1`, flaggedRun).Scan(&at); err != nil {
		t.Fatalf("read golden run timestamp: %v", err)
	}

	var out bytes.Buffer
	if err := classify.ReportForWorker(ctx, s.pool, &out, 0, sumGoldenWorker); err != nil {
		t.Fatalf("ReportForWorker: %v", err)
	}
	want := strings.ReplaceAll(sumGoldenReport, "{{AT}}", at.Format("2006-01-02 15:04"))
	if out.String() != want {
		t.Errorf("ReportForWorker's text changed.\n\n--- got ---\n%q\n\n--- want ---\n%q\n\n"+
			"Criterion 12: the text output is BYTE-IDENTICAL before and after the Summarize refactor. "+
			"`classify report --lane X --since Ndays` and the /funnel page must print the same numbers "+
			"for the same window (Verification step 4), and this golden is what makes that checkable.",
			out.String(), want)
	}
}

// sumGoldenReport is the pinned output for the three rows seeded above. Column
// widths, the two-space gutters, the blank lines and the closing note are all
// part of it — the whole string is the assertion.
const sumGoldenReport = `Classify shadow report (personal actionability)
  classified: 2  flagged: 1
    by kind  informational      1
    by kind  payment_due        1
  links  resolved: 1  declined (null): 0  none offered: 1  rejected: 0

  skipped: 3 (never sent to any provider)
    why the lane refused   provider_unreachable         3
    why it was restricted  restricted                   3
    NOTE: an all-skipped pass is EXPECTED when the local model is not running.
          Personal mail is only ever classified locally; falling back to a hosted
          provider is never the fix. See docs/runbooks/provider-locality.md.

flagged, newest first:
  {{AT}}  #4242    payment_due      notices@golden.example             Amount due                               Golden alpha  https://golden.example/pay

`
