package tools

// slack-send-queue (SWT-76) "Data model changes" — the half of migration
// 0042_slack_send_queue.sql that needs no database: the SHAPE of the file, and
// the living migration ledger's ownership line for 42.
//
// The other half — the two columns as POSTGRES sees them, nullable, on a real
// `deliveries` — is TestMigration0042_Integration_SendQueueColumns in
// delivery_slack_queue_integration_test.go. Both are needed, and the 0041 pair
// is the template: this one catches a file nobody applied, that one catches an
// application that diverged, because cmd/tools/migrate keys on
// schema_migrations.version with NO checksum and therefore skips an edited file
// SILENTLY (IK landmine).
//
// EXPECTED RED: migrations/0042_slack_send_queue.sql does not exist, and the
// ledger does not name 42.
//
// D5's argument, restated so the shape is not mistaken for taste: the queue
// fact goes in TWO NULLABLE COLUMNS, not a jsonb path inside `policy_result`.
// policy_result is the policy matrix's VERDICT (0001_initial.sql:199); storing
// transport state there forks the vocabulary and — the part that matters for
// tests — gives the dashboard and the assertions a jsonb path they can agree on
// by hand. A column is what a SELECT can drop and a mutation test can catch
// (IK: test the column, not the fixture).

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const sendQueueMigration = "0042_slack_send_queue.sql"

// sendQueueMigrationSQL returns the migration with comments stripped and
// whitespace normalised — slackWatchMigrationSQL's spelling, kept local so the
// two guards cannot drift by one helper edit.
func sendQueueMigrationSQL(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "migrations", sendQueueMigration))
	if err != nil {
		t.Fatalf("read migrations/%s: %v (D5: a queued send is state on the EXISTING deliveries row — two "+
			"nullable columns, no new table, no new status value)", sendQueueMigration, err)
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

func TestMigration0042_SendQueueColumns(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("read migrations/: %v", err)
	}
	var n42 []string
	for _, e := range entries {
		m := regexp.MustCompile(`^(\d{4})_.*\.sql$`).FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if v, _ := strconv.Atoi(m[1]); v == 42 {
			n42 = append(n42, e.Name())
		}
	}
	if len(n42) != 1 || n42[0] != sendQueueMigration {
		t.Fatalf("migrations/0042_*.sql = %v, want exactly [%s] (0041 is SWT-75's slack_watch; forward-only, "+
			"numbered, never edited after apply)", n42, sendQueueMigration)
	}

	sql := sendQueueMigrationSQL(t)
	for _, want := range []struct{ re, why string }{
		{`alter table (public\.)?deliveries\b`,
			"D5: the queue is state on the existing deliveries row (invariant 2: one funnel, no new table)"},
		{`add column (if not exists )?send_queued_at timestamptz`,
			"when the leaf accepted the job — the dashboard's elapsed time and the lease's reference point"},
		{`add column (if not exists )?send_queue_job_id text`,
			"the leaf's job id, so the mini's log line and the delivery row can be tied together by hand"},
	} {
		if !regexp.MustCompile(want.re).MatchString(sql) {
			t.Errorf("0042 does not match /%s/ — %s\n\ngot: %s", want.re, want.why, sql)
		}
	}

	// Both columns NULLABLE, with no backfill: no row has ever been queued, and
	// a NOT NULL would need a default that lies about every historical send.
	for _, banned := range []struct{ re, why string }{
		{`send_queued_at[^,;]*not null|send_queue_job_id[^,;]*not null`,
			"both columns are NULLABLE — NULL is 'this send was never queued', which is true of every row " +
				"that exists today"},
		{`\bdefault\b`,
			"no default: a default would make every pre-existing delivery claim a queue state it never had"},
		{`\bupdate\b|\binsert into\b`,
			"\"No backfill: no row has ever been queued\""},
		{`create index|create unique index`,
			"D5 / 0012's argument (migrations/0012_slack_send_attempts.sql:59): rows are located by id or by " +
				"the dashboard's bounded scan, so an index buys nothing and costs a write on every delivery"},
		{`\bstatus\b`,
			"D4: NO new status value. deliveries.status' CHECK (0001_initial.sql:197) is untouched — a queued " +
				"send is a 'sending' row whose attempt has not settled"},
		{`\bpolicy_result\b`,
			"D5: policy_result is the policy matrix's verdict, not transport state"},
		{`drop table|drop column`, "forward-only"},
		{`\btasks\b|\btask_events\b|\bsource_accounts\b|\bsync_runs\b|\bslack_watch\b`,
			"D11: no other table is touched by this migration"},
	} {
		if regexp.MustCompile(banned.re).MatchString(sql) {
			t.Errorf("0042 matches /%s/ — %s\n\ngot: %s", banned.re, banned.why, sql)
		}
	}
	if !strings.Contains(sql, "if not exists") && !strings.Contains(sql, "do $$") {
		t.Errorf("0042 has neither IF NOT EXISTS nor a DO $$ guard; applying it twice must be safe:\n%s", sql)
	}
}

// The living registry in internal/classify/structure_test.go must NAME 42's
// owner, or `ls migrations/` grows a file no SPEC accounts for. Same green
// guard as SWT-75's TestMigrationLedger_Learns0041.
func TestMigrationLedger_Learns0042(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "classify", "structure_test.go"))
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	src := string(b)
	i := strings.Index(src, "THIS LEDGER IS THE LIVING REGISTRY")
	if i < 0 {
		t.Fatalf("the migration ledger marker is gone; REWRITE the guard, never delete it")
	}
	start, end := max(i-7000, 0), min(i+2000, len(src))
	if !regexp.MustCompile(`n\s*!=\s*42\b`).MatchString(src[start:end]) {
		t.Errorf("the ledger does not accept 42 (`n != 42`); migrations/%s would be flagged as unowned",
			sendQueueMigration)
	}
	if !strings.Contains(src[start:i], "42 is SWT-76") {
		t.Errorf("the ledger accepts 42 without the ownership note \"42 is SWT-76 ...\" ABOVE the marker")
	}
	sg, err := os.ReadFile("signal_structure_test.go")
	if err != nil {
		t.Fatalf("read signal_structure_test.go: %v", err)
	}
	if !regexp.MustCompile(`v\s*!=\s*42\b`).Match(sg) {
		t.Errorf("TestMigration0033_TaskWorkingStateShape's exemption list does not include 42; it flags every " +
			"migration above 33 that no ticket claims there")
	}
}
