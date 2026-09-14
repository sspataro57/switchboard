//go:build integration

package capture_test

// SWT-54 criterion 17 (D9, D10): capture.DryRunRules — `opsctl capture-rules
// try`'s engine — decides a CANDIDATE rule over the stored corpus and writes
// NOTHING: no capture_decisions row, no executor call, no lock. Shares the
// prreview_integration_test.go harness (the candidate is NOT inserted).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_isopr?sslmode=disable TZ=UTC \
//	  go test -tags integration -p 1 -count=1 -run CaptureDryRun ./internal/capture/
//
// IMPOSED SURFACE:
//
//	type CandidateRule struct {
//	    Project, CriteriaType, Pattern, Subproject, ExternalSystem, KeyRegex, URLTemplate string
//	    Priority int; PRReview bool; ExcludePRAuthors []string
//	}
//	type DryRunConfig struct {
//	    Candidate CandidateRule
//	    Since     time.Duration // window on sent_at; 0 = unbounded
//	    Show      string        // "all" | "wins"
//	    Out       io.Writer
//	}
//	func DryRunRules(ctx context.Context, pool *pgxpool.Pool, cfg DryRunConfig) (<summary>, error)
//
// The OUTPUT is asserted by substring only; the one format constraint imposed
// is that each D9 backfill payload is ONE line ("ready to paste": an
// `opsctl call --tool … --args '{…}'` line), so a line naming link_external_ref
// and a PR key is that PR's payload.
//
// RED TODAY: 0035 is absent and capture.DryRunRules does not exist.

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/capture"
)

func TestCaptureDryRun_Integration_TryWritesNothingAndRollsUpEveryPR(t *testing.T) {
	ctx := context.Background()
	s := newPRRSuite(t, ctx, prrOpts{noPRRule: true})

	s.colleague(t, ctx, prrWWW, 9901, "joseg-avviato", "Ranking widget", 50)
	s.mail(t, ctx, ghMail{repo: prrWWW, pr: 9902, reason: "author", sender: "ananthsekar007", recipient: prrLogin,
		subject: "Re: [treetopllc/itest-prr-www] His change (PR #9902)", body: "approved", minsAgo: 45})
	s.colleague(t, ctx, prrThird, 9903, "dependabot[bot]", "Bump sanitize-html from 2.11.0 to 2.12.1", 40)
	s.colleague(t, ctx, prrWWW, 9904, "joseg-avviato", "Old spike", 35)
	s.mail(t, ctx, ghMail{repo: prrWWW, pr: 9904, reason: "state_change", sender: "joseg-avviato", recipient: prrLogin,
		subject: "Re: [treetopllc/itest-prr-www] Old spike (PR #9904)", body: "Closed #9904.", minsAgo: 30})

	// CURRENT decisions under rules 6/10/1 only: the dry run prints them beside
	// the candidate's proposed decision.
	s.pass(t, ctx, "live")

	var maxID int64
	if err := s.pool.QueryRow(ctx, `SELECT max(id) FROM capture_rules`).Scan(&maxID); err != nil {
		t.Fatalf("max rule id: %v", err)
	}
	tables := []string{"capture_decisions", "capture_rules", "tasks", "external_refs", "task_events", "audit_events"}
	counts := func() map[string]int {
		out := map[string]int{}
		for _, tb := range tables {
			out[tb] = s.n(t, ctx, `SELECT count(*) FROM `+tb)
		}
		return out
	}
	before := counts()

	// D10: "no lock". Hold capture's advisory lock on another session; a dry run
	// that took (or tried) it would skip or block instead of reporting.
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, int64(0x5157_0015)).Scan(&locked); err != nil || !locked {
		t.Fatalf("fixture: could not hold capture's advisory lock (locked=%v, err=%v)", locked, err)
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, int64(0x5157_0015)) //nolint:errcheck

	var out bytes.Buffer
	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if _, err := capture.DryRunRules(runCtx, s.pool, capture.DryRunConfig{
		Candidate: capture.CandidateRule{
			Project: prrCollab, CriteriaType: "thread_key_contains", Pattern: prrPattern,
			ExternalSystem: "github", KeyRegex: prrKeyRegex, Priority: 91, PRReview: true,
		},
		Since: 720 * time.Hour, Show: "all", Out: &out,
	}); err != nil {
		t.Fatalf("DryRunRules: %v", err)
	}

	after := counts()
	for _, tb := range tables {
		if after[tb] != before[tb] {
			t.Errorf("%s changed across a dry run: %d -> %d (criterion 17: it writes NOTHING)", tb, before[tb], after[tb])
		}
	}

	text := out.String()
	for _, want := range []struct{ frag, why string }{
		{fmt.Sprintf("rule %d", maxID+1), "the candidate carries the id it WOULD get, max(id)+1 (D10)"},
		{prrKey(prrWWW, 9901), "the per-PR rollup, canonical key"},
		{prrKey(prrWWW, 9902), "his own PR is in the rollup too (verdict own)"},
		{prrKey(prrThird, 9903), "the bot PR"},
		{prrKey(prrWWW, 9904), "the closed PR"},
		{"joseg-avviato", "the D1 evidence names the author"},
		{"dependabot[bot]", "OQ-1 = (b): the bot PR is a colleague's, named"},
		{"other", "verdict word"},
		{"own", "verdict word"},
		{"attributed", "the CURRENT latest decision is printed beside the proposed one"},
		{"Review PR #9901 — itest-prr-www: Ranking widget", "the title prReviewTitle would produce"},
		{"create_task", "D9 payload 1"},
		{"link_external_ref", "D9 payload 2"},
		{"task_set_source_thread", "D9 payload 3"},
		{prrURL(prrWWW, 9901), "the link payload carries github.PRURL"},
	} {
		if !strings.Contains(text, want.frag) {
			t.Errorf("dry-run output lacks %q — %s\noutput:\n%s", want.frag, want.why, text)
		}
	}
	linkLine := func(key string) bool {
		for _, line := range strings.Split(text, "\n") {
			if strings.Contains(line, "link_external_ref") && strings.Contains(line, key) {
				return true
			}
		}
		return false
	}
	for _, k := range []string{prrKey(prrWWW, 9901), prrKey(prrThird, 9903)} {
		if !linkLine(k) {
			t.Errorf("no one-line link_external_ref payload for %s: D9 backfills other/undetermined PRs with recent mail "+
				"and no notice", k)
		}
	}
	for _, c := range []struct{ key, why string }{
		{prrKey(prrWWW, 9902), "his own PR is never backfilled"},
		{prrKey(prrWWW, 9904), "a PR whose close notice was seen is never backfilled"},
	} {
		if linkLine(c.key) {
			t.Errorf("a link_external_ref payload names %s — %s", c.key, c.why)
		}
	}
}
