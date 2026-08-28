//go:build integration

package triage_test

// Integration test for SWT-21 acceptance criterion 20: `triage report` shows the
// SKIPPED lane, broken down by avail_reason and by class_reason, and says in
// WORDS that an idle triage is idle by design.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run ReportSkipped ./internal/triage/
//
// Why this is the criterion that decides whether the whole ticket is operable:
// after SWT-21 and before SWT-22, EVERY message is skipped, every pass. Today's
// report joins `status='ok'` rows only (report.go), so a fully-skipped pass
// renders as "processed: 0" with no explanation — indistinguishable from a
// broken poller, a dead API key, or an empty inbox. The first person to read one
// opens an incident, and the obvious "fix" is a fallback to the hosted lane,
// which is the one change this ticket exists to prevent.
//
// Mutual-cleanup pact: this test owns ai_runs rows with model
// 'itest-locality-report' and deletes them before and after. Its assertions are
// all "the output contains X", never global counts, so other suites' rows in the
// same window cannot break it and it cannot break them.
//
// GREENFIELD NOTE: fails until report.go grows the skipped lane.

import (
	"bytes"
	"context"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/triage"
)

func TestReportSkipped_ShowsTheSkippedLaneAndSaysWhy(t *testing.T) {
	ctx := context.Background()
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
	// Registered BEFORE the row cleanup so it runs AFTER it: t.Cleanup is LIFO,
	// and a `defer pool.Close()` here would close the pool first and make the
	// delete fail with "closed pool" — leaving this suite's rows behind for the
	// next one to trip over.
	t.Cleanup(pool.Close)

	clean := func() {
		if _, err := pool.Exec(ctx, `DELETE FROM ai_runs WHERE model = 'itest-locality-report'`); err != nil {
			t.Logf("cleanup ai_runs: %v", err)
		}
	}
	clean()
	t.Cleanup(clean)

	// One aggregate pass-level refusal (the no-local-provider case) and one
	// per-message refusal, exactly as criterion 19 specifies them.
	seed := func(input string) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO ai_runs (worker_type, provider, model, input, output, status,
			                      prompt_tokens, completion_tokens, latency_ms)
			 VALUES ('triage','local','itest-locality-report',$1::jsonb,'{}','skipped',0,0,0)`, input); err != nil {
			t.Fatalf("seed skipped ai_run: %v", err)
		}
	}
	seed(`{"avail_reason":"no_local_provider",
	       "class_reasons":{"unseen":2,"unmatched":7,"project_local_only":1},
	       "skipped_count":10,"message_ids":[1,2,3]}`)
	seed(`{"avail_reason":"local_unreachable","normalized_message_id":4242,"skipped_count":1}`)
	// A MIXED pass: the probe TTL expired part-way, so two reasons coexist.
	// avail_reason is only the dominant one, and a report that files all 12 under
	// it prints a wrong number in the one place an operator looks — worse than
	// printing no breakdown. The row also carries class_reasons, so this pins
	// that reading the breakdown does not skip the restriction counts.
	seed(`{"avail_reason":"no_local_provider",
	       "avail_reasons":{"no_local_provider":9,"local_unreachable":3},
	       "class_reasons":{"unmatched":12},
	       "skipped_count":12,"message_ids":[9,10]}`)

	var out bytes.Buffer
	if err := triage.Report(ctx, pool, &out, 0.7, time.Hour); err != nil {
		t.Fatalf("Report: %v", err)
	}
	got := strings.ToLower(out.String())

	if !strings.Contains(got, "skipped") {
		t.Fatalf("the report never says 'skipped'. Criterion 20: today's report joins status='ok' rows only, "+
			"so refusals are INVISIBLE — which is the failure mode, not the fix.\n%s", out.String())
	}
	for _, want := range []string{"no_local_provider", "local_unreachable"} {
		if !strings.Contains(got, want) {
			t.Errorf("the report does not break skips down by avail_reason (%q missing). The three reasons have "+
				"three different fixes: configure a local model / correct a non-private endpoint / wait for a "+
				"busy box.\n%s", want, out.String())
		}
	}
	for _, want := range []string{"unseen", "unmatched", "project_local_only"} {
		if !strings.Contains(got, want) {
			t.Errorf("the report does not break skips down by class_reason (%q missing). Criterion 5 keeps "+
				"unseen and unmatched distinct precisely so this line can tell them apart — one means the "+
				"capture pass has not run, the other means it ran and placed nothing.\n%s", want, out.String())
		}
	}

	// The mixed row's minority reason must be visible with ITS OWN count, not
	// swallowed into the dominant one.
	if !strings.Contains(got, "local_unreachable") {
		t.Errorf("the minority reason of a mixed pass is missing from the report:\n%s", out.String())
	}
	// The COUNTS, matched on the rendered line rather than as loose substrings —
	// a bare `strings.Contains(out, " 4")` would happily match the "4242" in the
	// per-message row's own id and assert nothing.
	//
	// local_unreachable: 1 (per-message row) + 3 (the mixed row's minority share).
	// no_local_provider: 10 (first aggregate) + 9 (the mixed row's share) = 19.
	// If the report ignored avail_reasons these would read 1 and 22.
	for _, want := range []struct {
		reason string
		count  int
	}{
		{"local_unreachable", 4},
		{"no_local_provider", 19},
	} {
		line := regexp.MustCompile(`(?m)^\s+why the lane refused\s+` + want.reason + `\s+(\d+)\s*$`)
		m := line.FindStringSubmatch(out.String())
		if m == nil {
			t.Errorf("no breakdown line for %q in:\n%s", want.reason, out.String())
			continue
		}
		if m[1] != strconv.Itoa(want.count) {
			t.Errorf("%s totals %s, want %d. The mixed pass carries avail_reasons; filing its whole "+
				"skipped_count under the dominant reason prints a wrong number in the one place an "+
				"operator looks.\n%s", want.reason, m[1], want.count, out.String())
		}
	}

	// class_reasons must accumulate from the breakdown rows TOO: 7 (first
	// aggregate) + 12 (the mixed row) = 19. An early `continue` on the
	// avail_reasons branch would silently drop the restriction counts for exactly
	// the rows that carry a breakdown, and every reason name would still appear —
	// so only the number catches it.
	classLine := regexp.MustCompile(`(?m)^\s+why it was restricted\s+unmatched\s+(\d+)\s*$`)
	if m := classLine.FindStringSubmatch(out.String()); m == nil {
		t.Errorf("no class_reasons line for \"unmatched\" in:\n%s", out.String())
	} else if m[1] != "19" {
		t.Errorf("class_reasons.unmatched totals %s, want 19 (7 + 12); a row with avail_reasons must still "+
			"contribute its class_reasons.\n%s", m[1], out.String())
	}

	// Criterion 20, the half a counter cannot carry: "it must say so in words,
	// not only in counts". Nothing in the numbers distinguishes "idle by design,
	// waiting for SWT-22" from "broken".
	idle := strings.Contains(got, "swt-22") ||
		strings.Contains(got, "by design") ||
		strings.Contains(got, "local model") && strings.Contains(got, "expected")
	if !idle {
		t.Errorf("the report shows skip COUNTS but never says, in words, that an all-skipped pass is expected "+
			"until a local model exists. After this ticket the report is the ONLY place an operator learns "+
			"that triage is idle by design rather than broken.\n%s", out.String())
	}
}
