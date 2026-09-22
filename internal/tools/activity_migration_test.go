package tools

// activity-resurfaces (SWT-72, docs/tickets/activity-resurfaces_SPEC.md)
// criterion 1, the half that needs no database: the SHAPE of
// migrations/0039_task_activity_review.sql, and the living migration ledger's
// ownership line for 39.
//
// The other half — the three columns and the FK as POSTGRES sees them — is
// TestMigration0039_Integration_ActivityColumns. Both are needed: this one
// catches a file nobody applied, that one catches an application that diverged
// (the runner keys on schema_migrations.version with NO checksum, so an edited
// file is skipped SILENTLY). The 0038 pair is the template.
//
// EXPECTED RED: migrations/0039_task_activity_review.sql does not exist.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const activityMigration = "0039_task_activity_review.sql"

func activityMigrationSQL(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "migrations", activityMigration))
	if err != nil {
		t.Fatalf("read migrations/%s: %v (criterion 1: the three columns are this ticket's migration, and "+
			"merging one is not applying it)", activityMigration, err)
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

func TestMigration0039_ActivityReviewColumns(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("read migrations/: %v", err)
	}
	var n39 []string
	for _, e := range entries {
		m := regexp.MustCompile(`^(\d{4})_.*\.sql$`).FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if v, _ := strconv.Atoi(m[1]); v == 39 {
			n39 = append(n39, e.Name())
		}
	}
	if len(n39) != 1 || n39[0] != activityMigration {
		t.Fatalf("migrations/0039_*.sql = %v, want exactly [%s] (criterion 1)", n39, activityMigration)
	}

	sql := activityMigrationSQL(t)
	for _, want := range []struct{ re, why string }{
		{`alter table (public\.)?tasks\b`,
			"the three columns go on the one tasks table (invariant 2: INCOMING stays a filter, no new table)"},
		{`add column (if not exists )?activity_at timestamptz`,
			"the last inbound activity a rule filed onto this open task (D1)"},
		{`add column (if not exists )?activity_by_message_id bigint references (public\.)?normalized_messages\(id\) on delete set null`,
			"the provenance message, with the SWT-45 surfaced_by_message_id spelling: ON DELETE SET NULL"},
		{`add column (if not exists )?reviewed_at timestamptz`,
			"the last human/Claude review of that activity (D2: a monotone stamp, never a cleared activity_at)"},
	} {
		if !regexp.MustCompile(want.re).MatchString(sql) {
			t.Errorf("0039 does not match /%s/ — %s\n\ngot: %s", want.re, want.why, sql)
		}
	}
	for _, banned := range []struct{ re, why string }{
		{`create table`, "no new table (invariant 2)"},
		{`drop table|drop column`, "forward-only"},
		{`create (unique )?index`, "no index: read by primary key and per displayed row (criterion 1)"},
		{`\bcheck\b`, "no CHECK (criterion 1)"},
		{`\bupdate tasks\b`, "NO BACKFILL: every existing row is NULL, so no task appears in INCOMING at rollout (D1)"},
		{`activity_kind`, "D11 removes the promoter's ask-attach, so an activity_kind discriminator would be " +
			"constant in production — the inert-predicate landmine. The remark comes from the message's CHANNEL (D5)"},
		{`\bdeliveries\b|\bclassify_promotions\b|\bcapture_decisions\b|\bticket_status_syncs\b|\bexternal_refs\b`,
			"no other table is touched (criterion 1)"},
	} {
		if regexp.MustCompile(banned.re).MatchString(sql) {
			t.Errorf("0039 matches /%s/ — %s", banned.re, banned.why)
		}
	}
	if !strings.Contains(sql, "if not exists") && !strings.Contains(sql, "do $$") {
		t.Errorf("0039 has neither IF NOT EXISTS nor a DO $$ guard; applying it twice must be safe:\n%s", sql)
	}
	// Criterion 1: "Its header states the rollout barrier (Verification Step 5)."
	raw, err := os.ReadFile(filepath.Join("..", "..", "migrations", activityMigration))
	if err != nil {
		t.Fatalf("re-read migrations/%s: %v", activityMigration, err)
	}
	head := strings.ToLower(string(raw))
	if !strings.Contains(head, "barrier") {
		t.Errorf("0039's header does not state the rollout BARRIER. Criterion 1 / Verification Step 5: the new "+
			"dashboard selects these columns on every /tasks render and the new capture/promote binaries call a "+
			"tool that writes them, so a new image on a pre-0039 db fails every render and every capture pass "+
			"(the 0030/0033/0034 precedent):\n%s", raw)
	}
}

// The living registry in internal/classify/structure_test.go must NAME 39's
// owner, or `ls migrations/` grows a file no SPEC accounts for. Same green
// guard as SWT-69's TestMigrationLedger_Learns0038.
func TestMigrationLedger_Learns0039(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "classify", "structure_test.go"))
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	src := string(b)
	i := strings.Index(src, "THIS LEDGER IS THE LIVING REGISTRY")
	if i < 0 {
		t.Fatalf("the migration ledger marker is gone; REWRITE the guard, never delete it")
	}
	start, end := max(i-5000, 0), min(i+2000, len(src))
	if !regexp.MustCompile(`n\s*!=\s*39\b`).MatchString(src[start:end]) {
		t.Errorf("the ledger does not accept 39 (`n != 39`); migrations/%s would be flagged as unowned", activityMigration)
	}
	if !strings.Contains(src[start:i], "39 is SWT-72") {
		t.Errorf("the ledger accepts 39 without the ownership note \"39 is SWT-72 ...\" ABOVE the marker")
	}
	sg, err := os.ReadFile("signal_structure_test.go")
	if err != nil {
		t.Fatalf("read signal_structure_test.go: %v", err)
	}
	if !regexp.MustCompile(`v\s*!=\s*39\b`).Match(sg) {
		t.Errorf("TestMigration0033_TaskWorkingStateShape's exemption list does not include 39; it flags every " +
			"migration above 33 that no ticket claims there")
	}
}
