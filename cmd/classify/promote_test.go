package main

// SWT-40 Part C's CLI half (docs/tickets/inquiry-promote_SPEC.md, E5 moved to
// Part C, C1, C11, C14; "API / MCP tool changes → CLI"):
//
//	classify promote [--lane personal|inquiry] [--dry-run] [--limit N]
//	                 [--max-age <dur>, dry-run only] [--outcomes [--since]]
//
// Pure: flag parsing and rendering, no database. The seams below are what
// promoteCmd calls; the database half is internal/promote's integration suite.
//
// ---- IMPOSED SURFACE (cmd/classify) ------------------------------------------
//
//	type promoteOpts struct {
//	    lane     promote.Lane
//	    dryRun   bool
//	    limit    int
//	    maxAge   time.Duration
//	    outcomes bool
//	    since    time.Duration
//	}
//	func parsePromoteFlags(argv []string) (promoteOpts, error)
//	// formatPromoteStats renders one pass's line, trailing newline included.
//	// The personal lane is BYTE-IDENTICAL to today's promoteCmd output (C1:
//	// the classify-promote CronJob's log must not change).
//	func formatPromoteStats(lane promote.Lane, dryRun bool, st promote.Stats) (string, error)
//	// formatOutcomes renders the C-D12 readout: counts ALWAYS; a precision
//	// ratio (TP / (TP+FP)) only at >= classify.EvalResultThreshold decided,
//	// else classify.EvalIndicativeMarker.
//	func formatOutcomes(c promote.OutcomeCounts) string
//
// GREENFIELD NOTE — EXPECTED RED: none of the three functions exists, so
// cmd/classify's test build compile-FAILS ("undefined: parsePromoteFlags").

import (
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/promote"
)

func TestParsePromoteFlags(t *testing.T) {
	// C1: the CronJob runs `classify promote` with no --lane — personal.
	o, err := parsePromoteFlags(nil)
	if err != nil {
		t.Fatalf("parsePromoteFlags(nil): %v", err)
	}
	if o.lane != promote.LanePersonal || o.dryRun || o.limit != 0 || o.maxAge != 0 || o.outcomes {
		t.Errorf("defaults = %+v, want lane personal and nothing else set (C1: byte-identical CronJob)", o)
	}
	if o, err := parsePromoteFlags([]string{"--lane", "inquiry", "--limit", "5"}); err != nil ||
		o.lane != promote.LaneInquiry || o.limit != 5 {
		t.Errorf("--lane inquiry --limit 5 = %+v, %v", o, err)
	}
	if o, err := parsePromoteFlags([]string{"--lane", "inquiry", "--dry-run", "--max-age", "720h"}); err != nil ||
		!o.dryRun || o.maxAge != 720*time.Hour {
		t.Errorf("--dry-run --max-age 720h = %+v, %v (V5's read)", o, err)
	}
	if o, err := parsePromoteFlags([]string{"--lane", "inquiry", "--outcomes", "--since", "336h"}); err != nil ||
		!o.outcomes || o.since != 336*time.Hour {
		t.Errorf("--outcomes --since 336h = %+v, %v (C14)", o, err)
	}
	for _, bad := range []struct {
		argv []string
		why  string
	}{
		{[]string{"--lane", "residue"}, "the residue lane never promotes (SWT-30 criterion 3)"},
		{[]string{"--lane", "bogus"}, "an unknown lane is refused, never defaulted"},
		{[]string{"--lane", "inquiry", "--max-age", "720h"}, "--max-age is refused unless --dry-run (C11)"},
		{[]string{"--max-age", "720h"}, "--max-age is refused unless --dry-run (C11)"},
	} {
		if _, err := parsePromoteFlags(bad.argv); err == nil {
			t.Errorf("parsePromoteFlags(%v) accepted it — %s", bad.argv, bad.why)
		}
	}
}

// C1, pinned to the byte: this is exactly what promoteCmd prints today
// (`fmt.Printf("promote: %s\n", out)` over a map json.Marshal sorts).
func TestFormatPromoteStats_PersonalIsByteIdentical(t *testing.T) {
	st := promote.Stats{Considered: 7, Created: 1, Review: 4, Attached: 3, Lost: 2, Reopened: 1}
	for _, tc := range []struct {
		dry  bool
		want string
	}{
		{false, `promote: {"attached":3,"considered":7,"created":1,"lost_claims":2,"mode":"live","reopened":1,"review":4}` + "\n"},
		{true, `promote: {"attached":3,"considered":7,"created":1,"lost_claims":2,"mode":"dry-run","reopened":1,"review":4}` + "\n"},
	} {
		got, err := formatPromoteStats(promote.LanePersonal, tc.dry, st)
		if err != nil {
			t.Fatalf("formatPromoteStats: %v", err)
		}
		if got != tc.want {
			t.Errorf("personal lane output changed (C1):\n got %q\nwant %q", got, tc.want)
		}
	}
}

// C11: the inquiry lane's stats block carries the lane and the gated counts by
// reason — every reason present, zeros included ("found nothing" and "never
// ran" are different lines, the gate stage's precedent).
func TestFormatPromoteStats_InquiryCarriesTheGatedBlock(t *testing.T) {
	st := promote.Stats{Considered: 2, Review: 2, Attached: 1, Gated: map[string]int{"pending": 1, "answered": 3}}
	got, err := formatPromoteStats(promote.LaneInquiry, true, st)
	if err != nil {
		t.Fatalf("formatPromoteStats: %v", err)
	}
	if !strings.HasPrefix(got, "promote: ") || !strings.HasSuffix(got, "\n") {
		t.Fatalf("inquiry output %q does not keep the `promote: {...}\\n` line shape", got)
	}
	var out struct {
		Mode   string         `json:"mode"`
		Lane   string         `json:"lane"`
		Review int            `json:"review"`
		Gated  map[string]int `json:"gated"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(got, "promote: "))), &out); err != nil {
		t.Fatalf("inquiry output is not one JSON object after `promote: `: %v\n%s", err, got)
	}
	if out.Mode != "dry-run" || out.Lane != "inquiry" || out.Review != 2 {
		t.Errorf("inquiry stats = %+v, want mode dry-run, lane inquiry, review 2", out)
	}
	want := map[string]int{"rethreaded": 0, "kind": 0, "stale": 0, "pending": 1, "answered": 3, "not_addressed": 0}
	for k, v := range want {
		got, ok := out.Gated[k]
		if !ok || got != v {
			t.Errorf("gated[%s] = %d (present %v), want %d — every reason, zeros included", k, got, ok, v)
		}
	}
	if len(out.Gated) != len(want) {
		t.Errorf("gated carries %d keys (%v), want exactly the six C3 reasons", len(out.Gated), out.Gated)
	}
}

// C14: counts always; the ratio only at >= EvalResultThreshold decided, and the
// marker in the one spelling below it. Decided = FP + TP (mis-clicks and
// exclusions are not labels).
func TestFormatOutcomes_CountsAlwaysRatioOnlyAtTheThreshold(t *testing.T) {
	line := func(out, key string, n int) bool {
		return regexp.MustCompile(`(?m)^.*` + key + `\D{0,20}\b` + strconv.Itoa(n) + `\b.*$`).MatchString(out)
	}
	below := promote.OutcomeCounts{FalsePositive: 59, TruePositive: 60, MisClick: 5, Excluded: 10}
	if below.Decided() != classify.EvalResultThreshold-1 {
		t.Fatalf("fixture: decided = %d, want threshold-1", below.Decided())
	}
	out := formatOutcomes(below)
	for key, n := range map[string]int{"false_positive": 59, "true_positive": 60, "mis_click": 5, "excluded": 10} {
		if !line(out, key, n) {
			t.Errorf("below the threshold the readout does not print %s %d (counts ALWAYS):\n%s", key, n, out)
		}
	}
	if !strings.Contains(out, classify.EvalIndicativeMarker) {
		t.Errorf("below %d decided the readout lacks classify.EvalIndicativeMarker:\n%s", classify.EvalResultThreshold, out)
	}
	if strings.Contains(strings.ToLower(out), "precision") {
		t.Errorf("below the threshold the readout prints a precision ratio (no ratio below %d):\n%s", classify.EvalResultThreshold, out)
	}

	at := promote.OutcomeCounts{FalsePositive: 30, TruePositive: 90, MisClick: 1, Excluded: 7}
	out = formatOutcomes(at)
	if !strings.Contains(strings.ToLower(out), "precision") || !strings.Contains(out, "0.75") {
		t.Errorf("at %d decided the readout does not print precision 0.75 (90 / 120):\n%s", at.Decided(), out)
	}
	if strings.Contains(out, classify.EvalIndicativeMarker) {
		t.Errorf("at the threshold the readout still carries the indicative marker:\n%s", out)
	}
	if !line(out, "false_positive", 30) || !line(out, "true_positive", 90) {
		t.Errorf("at the threshold the counts are missing:\n%s", out)
	}
}

// promoteCmd goes through the seams, and the readout through promote's fold
// (the threshold and marker live here, the fold in promote, C-D12).
func TestPromoteCmd_UsesTheSeams(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := string(b)
	i := strings.Index(src, "func promoteCmd(")
	if i < 0 {
		t.Fatalf("POSITIVE CONTROL FAILED: main.go has no promoteCmd")
	}
	body := src[i:]
	if j := strings.Index(body[1:], "\nfunc "); j >= 0 {
		body = body[:j+1]
	}
	for _, want := range []string{"parsePromoteFlags(", "formatPromoteStats(", "formatOutcomes(", "promote.InquiryOutcomes("} {
		if !strings.Contains(body, want) {
			t.Errorf("promoteCmd does not call %s", want)
		}
	}
}
