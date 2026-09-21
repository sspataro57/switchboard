package tools

// gmail-delivery-cc (SWT-69) criteria 1 and 2, the halves that need no
// database: the SHAPE of migrations/0038_delivery_cc.sql, and the living
// migration ledger's ownership line for 38.
//
// The other half — the column and the CHECKs as POSTGRES sees them — is
// TestMigration0038_Integration_DeliveryCcShape. Both are needed: this one
// catches a file nobody applied, that one catches an application that diverged
// (the runner keys on schema_migrations.version with NO checksum, so an edited
// file is skipped SILENTLY).
//
// EXPECTED RED: migrations/0038_delivery_cc.sql does not exist.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const ccMigration = "0038_delivery_cc.sql"

func ccMigrationSQL(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "migrations", ccMigration))
	if err != nil {
		t.Fatalf("read migrations/%s: %v (criterion 1: the column and its two CHECKs are this ticket's "+
			"migration, and merging one is not applying it)", ccMigration, err)
	}
	// Strip comments, then flatten whitespace: the assertions are about SQL,
	// not formatting.
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

func TestMigration0038_CcColumnAndChecks(t *testing.T) {
	// Exactly one 0038, named as the SPEC's data-model section names it.
	entries, err := os.ReadDir(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("read migrations/: %v", err)
	}
	var n38 []string
	for _, e := range entries {
		m := regexp.MustCompile(`^(\d{4})_.*\.sql$`).FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if v, _ := strconv.Atoi(m[1]); v == 38 {
			n38 = append(n38, e.Name())
		}
	}
	if len(n38) != 1 || n38[0] != ccMigration {
		t.Fatalf("migrations/0038_*.sql = %v, want exactly [%s]", n38, ccMigration)
	}

	sql := ccMigrationSQL(t)
	for _, want := range []struct{ re, why string }{
		{`alter table (public\.)?deliveries\b`, "the column goes on the one deliveries table (invariant 2: no delivery_recipients table)"},
		{`add column (if not exists )?cc text\[\] not null default '\{\}'`,
			"TEXT[] NOT NULL DEFAULT '{}' (D4): one representation of \"no Cc\", and pgx scans it into []string"},
		{`constraint deliveries_cc_gmail_check check\s*\(\s*channel\s*=\s*'gmail'\s+or\s+cc\s*=\s*'\{\}'`,
			"the NAMED gmail-only backstop (D12), so a direct writer cannot put a Cc on a channel whose send path drops it"},
		{`constraint deliveries_cc_shape_check check\s*\(\s*cardinality\s*\(cc\)\s*<=\s*` + strconv.Itoa(MaxCcAddresses) +
			`\s+and\s+not\s*\(\s*''\s*=\s*any\s*\(cc\)`,
			"the NAMED shape backstop, with the SAME limit as tools.MaxCcAddresses (mutation 4: changing one without the other)"},
	} {
		if !regexp.MustCompile(want.re).MatchString(sql) {
			t.Errorf("0038 does not match /%s/ — %s\n\ngot: %s", want.re, want.why, sql)
		}
	}
	for _, banned := range []struct{ re, why string }{
		{`create table`, "no new table (invariant 2)"},
		{`drop table`, "forward-only"},
		{`\bbcc\b`, "Bcc is out of scope, permanently"},
		{`\bregexp|similar to| ~ `, "RFC 5322 syntax is NOT re-spelled as a Postgres regex: net/mail is the one parser (D12)"},
		{`\bupdate deliveries\b`, "no backfill: existing rows get {} from the DEFAULT"},
	} {
		if regexp.MustCompile(banned.re).MatchString(sql) {
			t.Errorf("0038 matches /%s/ — %s", banned.re, banned.why)
		}
	}
	// Applying it twice must be safe (criterion 1, the 0037 style).
	if !strings.Contains(sql, "if not exists") && !strings.Contains(sql, "do $$") {
		t.Errorf("0038 has neither IF NOT EXISTS nor a DO $$ guard; criterion 1 requires applying it twice to "+
			"be safe:\n%s", sql)
	}
}

// The living registry in internal/classify/structure_test.go must NAME 38's
// owner, or `ls migrations/` grows a file no SPEC accounts for. Same green
// guard as SWT-54's TestMigrationLedger_Learns0035.
func TestMigrationLedger_Learns0038(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "classify", "structure_test.go"))
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	src := string(b)
	i := strings.Index(src, "THIS LEDGER IS THE LIVING REGISTRY")
	if i < 0 {
		t.Fatalf("the migration ledger marker is gone; REWRITE the guard, never delete it")
	}
	start, end := max(i-4500, 0), min(i+2000, len(src))
	if !regexp.MustCompile(`n\s*!=\s*38\b`).MatchString(src[start:end]) {
		t.Errorf("the ledger does not accept 38 (`n != 38`); migrations/%s would be flagged as unowned", ccMigration)
	}
	if !strings.Contains(src[start:i], "38 is SWT-69") {
		t.Errorf("the ledger accepts 38 without the ownership note \"38 is SWT-69 ...\" ABOVE the marker")
	}
	// The sibling guard in this package enumerates the numbers it tolerates
	// above 33; without 38 it flags this ticket's own migration.
	sg, err := os.ReadFile("signal_structure_test.go")
	if err != nil {
		t.Fatalf("read signal_structure_test.go: %v", err)
	}
	if !regexp.MustCompile(`v\s*!=\s*38\b`).Match(sg) {
		t.Errorf("TestMigration0033_TaskWorkingStateShape's exemption list does not include 38; it flags every " +
			"migration above 33 that no ticket claims there")
	}
}
