//go:build integration

package classify_test

// SWT-29 review finding 2: the 50-flag cap belongs to the PAGE
// (Summary.Flags), never to the CLI report — the first implementation leaked
// it into ReportForWorker and would have silently truncated 1,257 production
// flagged lines to 50. The golden characterization seeds ONE flagged row and
// structurally cannot see this, so this test pins the >cap case: 52 flagged
// verdicts must render 52 report lines while Summary.Flags stays at 50.
//
// Own worker_type, own marker, DELTA-free absolute assertions (the population
// is isolated). ANTI-DATE-ROT: rows seeded at now(), read with since=0.

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/classify"
)

const capReportWorker = "itest-swt29-reportcap"

func TestClassifyReport_Integration_RendersEveryFlagBeyondThePageCap(t *testing.T) {
	ctx := context.Background()
	s := newSumSuite(t, ctx)

	// newSumSuite's cleanup keys on the shared sumMarker, which run() stamps
	// into every row — this worker_type rides the same pact.
	for i := 0; i < 52; i++ {
		run := s.run(t, ctx, capReportWorker, "ok", "")
		s.extraction(t, ctx, run, fmt.Sprintf(`{"actionable":true,"kind":"deadline",
		    "title":"itest-swt29 repline %02d","reason":"itest","sender":"cap@swt29.example",
		    "subject":"Repline %02d","normalized_message_id":%d,"link_candidates":0}`, i, i, 9500+i))
	}

	var out bytes.Buffer
	if err := classify.ReportForWorker(ctx, s.pool, &out, 0, capReportWorker); err != nil {
		t.Fatalf("ReportForWorker: %v", err)
	}
	if n := strings.Count(out.String(), "itest-swt29 repline"); n != 52 {
		t.Fatalf("the report rendered %d flagged lines for 52 flagged verdicts, want ALL 52. The 50-row "+
			"cap is the PAGE's (Summary.Flags); the CLI report is byte-identical to its pre-refactor "+
			"output, which printed every line", n)
	}

	sum, err := classify.Summarize(ctx, s.pool, time.Hour, capReportWorker)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(sum.Flags) != 50 || sum.Flagged != 52 {
		t.Fatalf("Summary = %d Flags / %d Flagged, want 50 / 52 — the page list is capped, the count and "+
			"the report are not", len(sum.Flags), sum.Flagged)
	}
}
