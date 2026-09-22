package tools

// slack-watch-sweep (SWT-75, docs/tickets/slack-watch-sweep_SPEC.md) criterion 1,
// the half that needs no database: the SHAPE of
// migrations/0041_slack_watch.sql, and the living migration ledger's ownership
// line for 41.
//
// The other half — the table and its two id CHECKs as POSTGRES sees them, with
// the refusals the SPEC names ('t0360b84u', 'XYZ') — is
// TestMigration0041_Integration_SlackWatchShape in
// slackwatch_integration_test.go. Both are needed: this one catches a file
// nobody applied, that one catches an application that diverged (the runner
// keys on schema_migrations.version with NO checksum, so an edited file is
// skipped SILENTLY). The 0039 pair is the template.
//
// 0040 is deliberately NOT claimed here: it belongs to comms-inbox (SWT-74),
// which is being specced on an adjacent branch and has not written its file.
// This ticket takes 0041 and the ledger learns 41 alone.
//
// EXPECTED RED: migrations/0041_slack_watch.sql does not exist.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const slackWatchMigration = "0041_slack_watch.sql"

// slackWatchMigrationSQL returns the migration with comments stripped and
// whitespace normalised — activityMigrationSQL's spelling, kept local so the
// two guards cannot drift by one helper edit.
func slackWatchMigrationSQL(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "migrations", slackWatchMigration))
	if err != nil {
		t.Fatalf("read migrations/%s: %v (criterion 1: slack_watch is this ticket's migration, and the "+
			"watch list is a TABLE — D2 refused a source_accounts column, a projects column and a people flag)",
			slackWatchMigration, err)
	}
	var code []string
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		code = append(code, line)
	}
	sql := strings.ToLower(regexp.MustCompile(`\s+`).ReplaceAllString(strings.Join(code, " "), " "))
	sql = strings.ReplaceAll(strings.ReplaceAll(sql, "( ", "("), " )", ")")
	return regexp.MustCompile(`\s*,\s*`).ReplaceAllString(sql, ",")
}

func TestMigration0041_SlackWatchTable(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("read migrations/: %v", err)
	}
	var n41 []string
	for _, e := range entries {
		m := regexp.MustCompile(`^(\d{4})_.*\.sql$`).FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if v, _ := strconv.Atoi(m[1]); v == 41 {
			n41 = append(n41, e.Name())
		}
	}
	if len(n41) != 1 || n41[0] != slackWatchMigration {
		t.Fatalf("migrations/0041_*.sql = %v, want exactly [%s] (criterion 1; 0040 is comms-inbox's, SWT-74)",
			n41, slackWatchMigration)
	}

	sql := slackWatchMigrationSQL(t)
	for _, want := range []struct{ re, why string }{
		{`create table (if not exists )?(public\.)?slack_watch\b`,
			"D2: the watch list is its own configuration table, keyed by conversation"},
		{`id bigserial primary key`, "the id slack_watch_set_enabled takes"},
		{`workspace_id text not null`, "the workspace the leaf must open"},
		{`conversation_id text not null`, "the conversation the targeted pass reads"},
		{`label text not null default ''`, "the human label the /sources panel and opsctl print"},
		{`enabled boolean not null default true`,
			"a watch row is turned OFF, never deleted — the capture_rules shape"},
		{`created_at timestamptz not null default now\(\)`, "provenance"},
		{`updated_at timestamptz not null default now\(\)`, "the upsert's stamp (criterion 2)"},
		{`unique\(workspace_id,conversation_id\)`,
			"the key slack_watch_add upserts on (criterion 2: adding an existing pair re-enables, never errors)"},
	} {
		if !regexp.MustCompile(want.re).MatchString(sql) {
			t.Errorf("0041 does not match /%s/ — %s\n\ngot: %s", want.re, want.why, sql)
		}
	}

	// The two id CHECKs are asserted on the RAW text, case included: they are the
	// LEAF's own regexes (export_request.go:88-90 == http-bridge.ts:140,146) and a
	// lowercased copy would accept ids the leaf drops. D2: BuildExportRequest
	// silently DROPS a malformed id, so the constraint that makes a dropped watch
	// row impossible belongs in the database, where the write fails loudly.
	raw, err := os.ReadFile(filepath.Join("..", "..", "migrations", slackWatchMigration))
	if err != nil {
		t.Fatalf("re-read migrations/%s: %v", slackWatchMigration, err)
	}
	for _, want := range []struct{ re, why string }{
		{`workspace_id\s*~\s*'\^T\[A-Z0-9\]\{5,\}\$'`,
			"the leaf's workspace rule, restated where a bad value cannot be entered (criterion 1)"},
		{`conversation_id\s*~\s*'\^\[CDG\]\[A-Z0-9\]\{5,\}\$'`,
			"the leaf's conversation rule: C channel, D dm, G group dm (criterion 1)"},
	} {
		if !regexp.MustCompile(want.re).Match(raw) {
			t.Errorf("0041 does not match /%s/ — %s. Dropping the CHECK is the SPEC's first mutation row:\n%s",
				want.re, want.why, raw)
		}
	}

	for _, banned := range []struct{ re, why string }{
		{`\btasks\b|\bdeliveries\b|\bcapture_decisions\b|\bcapture_rules\b|\bsource_accounts\b|\bsync_runs\b`,
			"D11: no other table is touched. slack_watch is configuration, and the phase change is code, not schema"},
		{`\bstatus\b|\bassignee|\bpriority\b|\bproject_id\b`,
			"invariant 2: slack_watch holds no work, no status, no assignee — it is the capture_rules shape, " +
				"not a second Slack inbox"},
		{`\binsert into\b`,
			"\"No rows are seeded by the migration\": production ids are not frozen literals (IK), and which " +
				"conversations matter is Salvador's call, made with opsctl after Step 0a"},
		{`drop table|drop column`, "forward-only"},
	} {
		if regexp.MustCompile(banned.re).MatchString(sql) {
			t.Errorf("0041 matches /%s/ — %s", banned.re, banned.why)
		}
	}
	if !strings.Contains(sql, "if not exists") && !strings.Contains(sql, "do $$") {
		t.Errorf("0041 has neither IF NOT EXISTS nor a DO $$ guard; applying it twice must be safe:\n%s", sql)
	}
}

// The living registry in internal/classify/structure_test.go must NAME 41's
// owner, or `ls migrations/` grows a file no SPEC accounts for. Same green
// guard as SWT-72's TestMigrationLedger_Learns0039.
//
// 40 is NOT accepted here on purpose: comms-inbox (SWT-74) owns it and has not
// written it. Whichever of the two branches needs to renumber does so with its
// own line.
func TestMigrationLedger_Learns0041(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "classify", "structure_test.go"))
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	src := string(b)
	i := strings.Index(src, "THIS LEDGER IS THE LIVING REGISTRY")
	if i < 0 {
		t.Fatalf("the migration ledger marker is gone; REWRITE the guard, never delete it")
	}
	start, end := max(i-6000, 0), min(i+2000, len(src))
	if !regexp.MustCompile(`n\s*!=\s*41\b`).MatchString(src[start:end]) {
		t.Errorf("the ledger does not accept 41 (`n != 41`); migrations/%s would be flagged as unowned",
			slackWatchMigration)
	}
	if !strings.Contains(src[start:i], "41 is SWT-75") {
		t.Errorf("the ledger accepts 41 without the ownership note \"41 is SWT-75 ...\" ABOVE the marker")
	}
	sg, err := os.ReadFile("signal_structure_test.go")
	if err != nil {
		t.Fatalf("read signal_structure_test.go: %v", err)
	}
	if !regexp.MustCompile(`v\s*!=\s*41\b`).Match(sg) {
		t.Errorf("TestMigration0033_TaskWorkingStateShape's exemption list does not include 41; it flags every " +
			"migration above 33 that no ticket claims there")
	}
}
