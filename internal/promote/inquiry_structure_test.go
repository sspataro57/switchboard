package promote_test

// SWT-40 Part C, the structural criteria (docs/tickets/inquiry-promote_SPEC.md,
// C5, C6, C12, C13, C14): scans hold, the key-spelling rule reaches promote,
// the fold comes from replyfold, the inbox follows the latest decision in ANY
// mode, the migration guard, the ledger, and the docs. ZERO I/O beyond reading
// this repo's own source and docs. Reuses structure_test.go's helpers
// (prRepoFile, prSources, prFileImports).
//
// The existing guards need no edit and still apply to every new file here:
// TestPromote_CannotReachAModelProvider walks promote's imports TRANSITIVELY,
// so importing replyfold and slackweb must keep internal/provider unreachable;
// TestPromote_NeverWritesToolActionTablesDirectly bans direct writes to tasks,
// task_events, deliveries, external_refs and task_dismissals in every
// non-test file, including the new inquiry.go and outcomes.go.
//
// GREENFIELD NOTE — EXPECTED RED: this file compiles against no new symbol,
// so it fails on ASSERTIONS: promote has no replyfold/slackweb use, no inquiry
// inbox literal, migrations/0031_inquiry_promotion.sql does not exist, and the
// runbooks, handoff and IK carry no Part C text. The ledger test is a GREEN
// guard (the ledger was taught 31 alongside this file).

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ---- the pure test stays offline ---------------------------------------------

func TestPromoteInquiryPureTest_ImportsNothingThatCouldDoIO(t *testing.T) {
	const rel = "internal/promote/inquiry_test.go"
	imports := prFileImports(t, rel)
	allowed := map[string]bool{"testing": true, "time": true, prModule + "/internal/promote": true}
	subject := false
	for _, imp := range imports {
		if imp == prModule+"/internal/promote" {
			subject = true
		}
		if !allowed[imp] {
			t.Errorf("%s imports %q. InquiryGate, Decide and InquiryOutcome are pure (invariant 7): the test "+
				"imports testing, time and the package, nothing that could do I/O", rel, imp)
		}
	}
	if !subject {
		t.Errorf("%s does not import internal/promote", rel)
	}
}

// ---- C5: promote asks slackweb, and only about DMs -----------------------------

func TestPromote_UsesOnlyTheDMHelperFromSlackweb(t *testing.T) {
	allowed := map[string]bool{"IsDirectMessageKey": true, "Channel": true}
	use := regexp.MustCompile(`\bslackweb\.([A-Za-z_]\w*)`)
	seen := map[string]bool{}
	for _, rel := range prSources(t, "internal/promote") {
		for _, m := range use.FindAllStringSubmatch(prRepoFile(t, rel), -1) {
			seen[m[1]] = true
			if !allowed[m[1]] {
				t.Errorf("%s uses slackweb.%s. internal/promote may use slackweb.IsDirectMessageKey (C-D3's DM "+
					"clause) and slackweb.Channel; anything else in that package reaches the Slack bridge, and "+
					"the promoter sends nothing (invariant 4)", rel, m[1])
			}
		}
	}
	if !seen["IsDirectMessageKey"] {
		t.Errorf("no internal/promote source calls slackweb.IsDirectMessageKey. C-D3/C-D4: 'a 1:1 Slack DM' is " +
			"asked of the ONE helper beside the key builder, never re-derived here")
	}
}

// The key-spelling scan (SWT-33 criterion 14) extends to promote and replyfold
// (C5): no line that handles a thread key picks it apart.
func TestPromote_DoesNotRespellTheSlackThreadKey(t *testing.T) {
	surgery := regexp.MustCompile(`(?i)\bLIKE\b|split_part|strings\.(Split|SplitN|Count|LastIndex|Index|HasSuffix|HasPrefix|Contains|Fields|TrimPrefix)\b`)
	scanned := 0
	for _, pkg := range []string{"internal/promote", "internal/replyfold"} {
		entries, err := os.ReadDir(filepath.Join("..", "..", pkg))
		if err != nil {
			t.Errorf("read %s: %v", pkg, err)
			continue
		}
		for _, e := range entries {
			n := e.Name()
			if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
				continue
			}
			scanned++
			for i, line := range strings.Split(prRepoFile(t, filepath.Join(pkg, n)), "\n") {
				tl := strings.TrimSpace(line)
				if strings.HasPrefix(tl, "//") {
					continue
				}
				if (strings.Contains(line, "hreadKey") || strings.Contains(line, "thread_key") ||
					strings.Contains(line, "slack:")) && surgery.MatchString(line) {
					t.Errorf("%s/%s:%d picks apart a thread key: %s — the DM and rooted rules have ONE spelling each, "+
						"in internal/connector/slackweb (C5)", pkg, n, i+1, tl)
				}
			}
		}
	}
	if scanned < 3 {
		t.Errorf("scanned only %d source files across promote and replyfold; the scan is blind", scanned)
	}
}

// ---- C6/C-D7: the fold is replyfold's, not a second copy ------------------------

func TestPromote_ReadsTheReplyFoldFromReplyfold(t *testing.T) {
	var all strings.Builder
	for _, rel := range prSources(t, "internal/promote") {
		all.WriteString(prRepoFile(t, rel))
	}
	src := all.String()
	for _, want := range []string{"replyfold.JoinSQL", "replyfold.RepliedSinceCol", "replyfold.PriorParticipationCol"} {
		if !strings.Contains(src, want) {
			t.Errorf("internal/promote never references %s. C-D7: the replied-since fold and the prior-participation "+
				"fragment live in internal/replyfold, shared with classify.Summarize — one spelling", want)
		}
	}
	for _, banned := range []string{"last_outbound", "first_outbound", "max(sent_at)", "min(sent_at)"} {
		if strings.Contains(strings.ToLower(src), banned) {
			t.Errorf("internal/promote spells %q itself: a second copy of the fold drifts from classify's report", banned)
		}
	}
}

// ---- C2: the latest decision in ANY mode ---------------------------------------

// "latest action='attributed' in any mode, including route and gate". Part B's
// migration (the 'route' mode) is not on this branch, so the integration suite
// can seed a gate fixture but not a route one; this scan is the route half: the
// inquiry inbox's latest-decision subquery carries NO mode predicate at all.
func TestPromoteInquiryInbox_LatestDecisionHasNoModePredicate(t *testing.T) {
	lit := regexp.MustCompile("(?s)`[^`]*`")
	found := 0
	for _, rel := range prSources(t, "internal/promote") {
		for _, l := range lit.FindAllString(prRepoFile(t, rel), -1) {
			if !strings.Contains(l, "classify_inquiry") || !strings.Contains(l, "capture_decisions") {
				continue
			}
			found++
			i := strings.Index(l, "capture_decisions")
			rest := l[i:]
			end := strings.Index(strings.ToUpper(rest), "LIMIT 1")
			if end < 0 {
				t.Errorf("%s: the inquiry inbox reads capture_decisions without a `LIMIT 1` latest-row subquery:\n%s", rel, l)
				continue
			}
			if regexp.MustCompile(`\bmode\b`).MatchString(rest[:end]) {
				t.Errorf("%s: the inquiry inbox's latest-decision subquery filters on mode:\n%s\nC2: the LATEST row in "+
					"ANY mode decides — a gate resolution (mode='gate') or a route row (mode='route', Part B) is the "+
					"message's current attribution, and every other latest-decision reader follows it", rel, rest[:end])
			}
		}
	}
	if found == 0 {
		t.Errorf("no raw SQL literal in internal/promote names both classify_inquiry and capture_decisions: the " +
			"inquiry inbox (C2) does not exist yet")
	}
}

// ---- C12: the migration ----------------------------------------------------------

// SWT-40 Part C adds exactly one migration. The SPEC calls it
// 0028_inquiry_promotion.sql; 0028 went to SWT-43, 0029 to Part D, and 0030 is
// claimed by SWT-45 (jira-activity-revive) on another branch, so Part C takes
// 0031. Whichever of SWT-45 and this branch merges second renumbers its file,
// this guard and the ledger line.
func TestMigration0031_IsTheOnlyOneThisTicketAdds(t *testing.T) {
	if _, err := os.Stat(filepath.Join("..", "..", "migrations", "0029_capture_ticket_gate.sql")); err != nil {
		t.Fatalf("migrations/0029_capture_ticket_gate.sql is missing (%v); Part D's migration precedes this one", err)
	}
	matches, err := filepath.Glob(filepath.Join("..", "..", "migrations", "0031_*.sql"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("found %d migrations/0031_*.sql file(s), want exactly 1 (0031_inquiry_promotion.sql: "+
			"projects.inquiry_promote_after, C-D2)", len(matches))
	}
	rel := filepath.Join("migrations", filepath.Base(matches[0]))
	if filepath.Base(matches[0]) != "0031_inquiry_promotion.sql" {
		t.Errorf("the migration is %s, want 0031_inquiry_promotion.sql (the SPEC's name, renumbered)", rel)
	}
	sql := strings.ToLower(prRepoFile(t, rel))
	var code strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			code.WriteString(line + "\n")
		}
	}
	stmts := code.String()

	alter := regexp.MustCompile(`alter\s+table\s+projects\s+add\s+column\s+inquiry_promote_after\s+timestamptz`)
	loc := alter.FindStringIndex(stmts)
	if loc == nil {
		t.Fatalf("%s does not `ALTER TABLE projects ADD COLUMN inquiry_promote_after TIMESTAMPTZ` (C-D2: its own "+
			"cutover, SWT-33 D3's one-column-per-question rule)", rel)
	}
	stmt := stmts[loc[0]:]
	if end := strings.Index(stmt, ";"); end >= 0 {
		stmt = stmt[:end]
	}
	if strings.Contains(stmt, "default") || strings.Contains(stmt, "not null") {
		t.Errorf("%s gives inquiry_promote_after a DEFAULT or NOT NULL (%q). NULL = off is the fail-closed side: "+
			"the cutover is armed by a hand-run UPDATE, a decision with a timestamp, never a deploy side effect", rel,
			strings.TrimSpace(stmt))
	}
	for _, banned := range []struct{ re, why string }{
		{`\bupdate\b`, "no backfill: arming is a hand-run UPDATE after #110 gets provenance (C-D11, V6.4)"},
		{`create\s+table`, "no new table (invariant 2); C-D12 reads existing tables"},
		{`create\s+(unique\s+)?index`, "no index: projects is tens of rows, reached by primary key"},
		{`insert\s+into`, "no seeding"},
		{`drop\s+`, "forward-only, no down migration"},
		{`inquiry_create_status|create_status`, "O7's Holding-first is a Go constant (inquiryCreateStatus), NOT a column"},
		{`classify_promote_after`, "the personal lane's cutover is untouched (C1)"},
	} {
		if regexp.MustCompile(banned.re).MatchString(stmts) {
			t.Errorf("%s matches /%s/ — %s", rel, banned.re, banned.why)
		}
	}
}

// The LEDGER (internal/classify/structure_test.go) must own 31, naming this
// ticket, within the window TestMigrationLedger_Learns0024 already reads.
// GREEN guard: the ledger was taught 31 in the same change as this file.
func TestMigrationLedger_Learns0031(t *testing.T) {
	src := prRepoFile(t, "internal/classify/structure_test.go")
	i := strings.Index(src, "THIS LEDGER IS THE LIVING REGISTRY")
	if i < 0 {
		t.Fatalf("the migration ledger marker is gone; REWRITE the guard, never delete it")
	}
	start, end := max(i-3000, 0), min(i+1800, len(src))
	ledger := src[start:end]
	if !regexp.MustCompile(`n\s*!=\s*31\b`).MatchString(ledger) {
		t.Errorf("the ledger does not accept 31 (`n != 31`); 0031_inquiry_promotion.sql would be flagged as unowned")
	}
	if !strings.Contains(ledger, "31 is SWT-40 Part C") {
		t.Errorf("the ledger adds 31 without the ownership note \"31 is SWT-40 Part C ...\" above the marker")
	}
	if regexp.MustCompile(`n\s*!=\s*30\b`).MatchString(ledger) {
		t.Errorf("the ledger accepts 30 on this branch; 0030 is SWT-45's, and it learns it when SWT-45 merges")
	}
}

// ---- C13/C14: the docs -------------------------------------------------------------

func prDocMustMention(t *testing.T, rel string, why string, tokens ...string) {
	t.Helper()
	doc := strings.ToLower(prRepoFile(t, rel))
	for _, tok := range tokens {
		if !strings.Contains(doc, strings.ToLower(tok)) {
			t.Errorf("%s never mentions %q — %s", rel, tok, why)
		}
	}
}

// C14: the runbook documents the readout as O7's flip signal — where to look,
// what the counts mean, and that the flip is inquiryCreateStatus = "ready".
func TestRunbook_LocalClassifierDocumentsInquiryPromotion(t *testing.T) {
	prDocMustMention(t, "docs/runbooks/local-classifier.md", "C13/C14: the inquiry promotion section",
		"classify promote --lane inquiry",
		"inquiry_promote_after",
		"--outcomes",
		`inquiryCreateStatus = "ready"`,
		"O7",
		"holding",
		"--max-age",
		"not_actionable",
		"handled_elsewhere",
		"mis-click",
		"precision",
		"recall",
	)
}

func TestRunbook_PipelineDocumentsTheInquiryStages(t *testing.T) {
	prDocMustMention(t, "docs/runbooks/pipeline.md", "C13: the two inquiry stages, their locks and the grace release",
		"inquiry_promote",
		// Assembled from pieces: internal/classify's collision guard allows the
		// GPU key's literal only inside internal/classify.
		"0x5157_"+"0022",
		"0x5157_0021",
		"promote:inquiry",
		"grace",
		"inquiry_promote_after",
	)
}

func TestHandoff_ListsPartC(t *testing.T) {
	const rel = "docs/runbooks/HANDOFF-kube-inquiry-promote.md"
	prDocMustMention(t, rel, "C13: the kube session's Part C rows",
		"## Part C",
		"0031_inquiry_promotion.sql",
		"PIPELINE_STAGES=gate,inquiry,inquiry_promote",
		"OPS_LOCAL_PROVIDER_URL",
		"OPS_LOCAL_MODEL",
		"--lane personal",
	)
}

// C13: the IK entry restates that classify eval writes no ai_runs and that
// --max-age is dry-run-only.
func TestInstitutionalKnowledge_RecordsPartC(t *testing.T) {
	doc := prRepoFile(t, ".claude/INSTITUTIONAL_KNOWLEDGE.md")
	i := strings.Index(doc, "SWT-40 Part C")
	if i < 0 {
		t.Fatalf("the IK has no \"SWT-40 Part C\" entry (C13)")
	}
	entry := strings.ToLower(doc[i:min(i+5000, len(doc))])
	for _, tok := range []string{"inquiry_promote_after", "--max-age", "dry-run", "classify eval", "ai_runs",
		"inquirycreatestatus", "holding", "promote:inquiry", "gated"} {
		if !strings.Contains(entry, tok) {
			t.Errorf("the SWT-40 Part C IK entry never mentions %q", tok)
		}
	}
}
