package ticketstatus_test

// Structural tests for SWT-45 (docs/tickets/jira-activity-revive_SPEC.md) —
// the criteria enforced mechanically rather than by review: 1 (one 0030, and
// the ledger learns 30), 2-4 and 6 (migration 0030's shape, its self-check and
// its six-value pin), 29 (decide.go bans time.Now), 35 (both counter lines
// print `resurfaced`), 40 (the ticket-status-sync runbook) and 41 (the IK
// entry). ZERO I/O beyond reading this repo. Reuses tsRepoFile from
// structure_test.go.
//
// SWT-32's 0023 vocabulary pin (structure_test.go:563) is deliberately left
// UNCHANGED (criterion 6): 0023 is applied and unamendable, and its five values
// are still what 0023 says. The six-value set is pinned against 0030 here.
//
// What each test asserts: exactly one migrations/0030_*.sql, named
// 0030_jira_activity_revive.sql, with criteria 2-4 and 6's shape (SQL and
// comments checked apart); its last_action CHECK allows exactly six values;
// internal/classify/structure_test.go's ledger names 30 one by one; decide.go
// calls no time.Now/Since/Until; Stats has Resurfaced and both ticket_status
// lines print it; the ticket-status-sync runbook has "Surfaced by activity",
// no longer "The gap, until qa-question-resurface ships", and its D9 paragraph
// names the overriding-rule exception; the IK has an SWT-45 heading covering
// F7, F8, the closed_at/updated_at fallback and J10's cost.
//
// NO ADVISORY-LOCK LITERAL IS SPELLED HERE (criterion 7; structure_test.go's
// recorded reason).

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/ticketstatus"
)

// rsSQL splits a migration into code and `--` comments, lowercased, so a
// structural regex cannot be satisfied by prose (and prose is checked apart).
func rsSQL(t *testing.T, rel string) (code, comments string) {
	t.Helper()
	var c, k strings.Builder
	for _, line := range strings.Split(strings.ToLower(tsRepoFile(t, rel)), "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			k.WriteString(line[i:])
			k.WriteString("\n")
			line = line[:i]
		}
		c.WriteString(line)
		c.WriteString("\n")
	}
	return c.String(), k.String()
}

func rs0030(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat(filepath.Join("..", "..", "migrations", "0027_sync_runs_partial.sql")); err != nil {
		t.Fatalf("migrations/0027_sync_runs_partial.sql is missing: %v — the control for the glob below", err)
	}
	matches, err := filepath.Glob(filepath.Join("..", "..", "migrations", "0030_*.sql"))
	if err != nil {
		t.Fatalf("glob migrations/0030_*.sql: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("found %d migrations/0030_*.sql file(s), want exactly 1 (the SPEC names "+
			"0030_jira_activity_revive.sql). 0028 (SWT-43) and 0029 (SWT-40 Part D) are owned on sibling "+
			"branches; whichever merges later than a sibling it collides with renumbers. Merging a migration is "+
			"not applying it", len(matches))
	}
	if base := filepath.Base(matches[0]); base != "0030_jira_activity_revive.sql" {
		t.Errorf("migration 0030 is named %s, want 0030_jira_activity_revive.sql", base)
	}
	return filepath.Join("migrations", filepath.Base(matches[0]))
}

// ---- criteria 1-4, 6: migration 0030 ------------------------------------------

func TestMigration0030_JiraActivityReviveShape(t *testing.T) {
	rel := rs0030(t)
	sql, comments := rsSQL(t, rel)

	// (2) the two rule flags and their CHECKs.
	for _, re := range []struct{ pat, why string }{
		{`add\s+column\s+revive\s+boolean\s+not\s+null\s+default\s+false`, "J1: revive, default false — every existing rule keeps today's behaviour"},
		{`add\s+column\s+addressed\s+boolean\s+not\s+null\s+default\s+false`, "J1: addressed, default false"},
		{`constraint\s+capture_rules_revive_needs_key\s+check\s*\(\s*not\s+revive\s+or\s*\(\s*external_system\s+is\s+not\s+null\s+and\s+key_regex\s+is\s+not\s+null\s*\)\s*\)`,
			"J1/F1: a reviving rule needs an explicit key_regex — rule 10 (no key_regex, keys by PREFIX) can never be flagged"},
		{`constraint\s+capture_rules_addressed_implies_revive\s+check\s*\(\s*not\s+addressed\s+or\s+revive\s*\)`,
			"J1: addressed implies revive"},
	} {
		if !regexp.MustCompile(re.pat).MatchString(sql) {
			t.Errorf("%s does not match /%s/ — %s", rel, re.pat, re.why)
		}
	}

	// (3) the four task columns.
	for _, re := range []struct{ pat, why string }{
		{`add\s+column\s+closed_at\s+timestamptz\b`, "J5: the close instant the revive guard compares against"},
		{`add\s+column\s+closed_from_status\s+text\b`, "J5: what a revive restores (NULL -> ready)"},
		{`add\s+column\s+surfaced_at\s+timestamptz\b`, "J5: the last time something other than the reconciler put the task on the board"},
		{`add\s+column\s+surfaced_by_message_id\s+bigint\s+references\s+normalized_messages\s*\(\s*id\s*\)\s+on\s+delete\s+set\s+null`,
			"J5: which message did it; SET NULL — deleting a message must not delete a task"},
	} {
		if !regexp.MustCompile(re.pat).MatchString(sql) {
			t.Errorf("%s does not match /%s/ — %s", rel, re.pat, re.why)
		}
	}
	if regexp.MustCompile(`surfaced_by_message_id[^,;]*on\s+delete\s+cascade`).MatchString(sql) {
		t.Errorf("%s cascades tasks from normalized_messages; the SPEC says ON DELETE SET NULL", rel)
	}
	if regexp.MustCompile(`check\s*\([^;]*(closed_at|closed_from_status|surfaced_at|surfaced_by_message_id)`).MatchString(sql) {
		t.Errorf("%s puts a CHECK on the close/surfacing columns. F5: integration fixtures across many suites INSERT "+
			"closed tasks directly; a CHECK tying status='closed' to closed_at breaks them en masse", rel)
	}
	if regexp.MustCompile(`create\s+(unique\s+)?index\b`).MatchString(sql) {
		t.Errorf("%s creates an index. Criterion 3: none — the columns are read by primary key only", rel)
	}
	if regexp.MustCompile(`update\s+tasks\b`).MatchString(sql) {
		t.Errorf("%s UPDATEs tasks. J5: NO backfill — a NULL closed_at falls back to updated_at in the handler's SQL", rel)
	}
	if !strings.Contains(comments, "fixture") {
		t.Errorf("%s's comments do not name F5 (fixtures INSERT closed tasks directly — why there is no CHECK)", rel)
	}
	if !strings.Contains(comments, "updated_at") {
		t.Errorf("%s's comments do not name F6's fallback: a NULL closed_at makes the revive guard use updated_at, "+
			"which is >= the last close instant", rel)
	}

	// (4) the reconciler's column and the CHECK swap, self-verified.
	if !regexp.MustCompile(`alter\s+table\s+ticket_status_syncs\s+add\s+column\s+surfaced_seen_at\s+timestamptz\b`).MatchString(sql) {
		t.Errorf("%s does not add ticket_status_syncs.surfaced_seen_at TIMESTAMPTZ (J11)", rel)
	}
	if !regexp.MustCompile(`alter\s+table\s+ticket_status_syncs\s+drop\s+constraint\s+(if\s+exists\s+)?ticket_status_syncs_last_action_check\b`).MatchString(sql) {
		t.Errorf("%s does not DROP CONSTRAINT ticket_status_syncs_last_action_check (0023's inline CHECK, by the "+
			"name Postgres generated — the 0009/0025 precedent)", rel)
	}
	selfCheck := regexp.MustCompile(`(?s)do\s+\$\$.*pg_constraint.*last_action.*raise\s+exception.*\$\$`)
	if !selfCheck.MatchString(sql) || !regexp.MustCompile(`(?s)do\s+\$\$.*count\(\*\).*\$\$`).MatchString(sql) {
		t.Errorf("%s has no DO $$ self-check COUNTING the CHECKs on ticket_status_syncs that mention last_action and "+
			"RAISING unless exactly one survives. A DROP of a name Postgres did not generate is a silent no-op, and "+
			"the first 'resurfaced' write then fails at runtime, every tick, on the same ref", rel)
	}
	if !strings.Contains(comments, "transaction") {
		t.Errorf("%s never says the drop/add pair is safe ONLY because migrate runs each file in one transaction", rel)
	}

	// (6) arms nothing, names no status, forward-only, supersedes 0023's comment.
	if regexp.MustCompile(`update\s+capture_rules\b`).MatchString(sql) || regexp.MustCompile(`insert\s+into\b`).MatchString(sql) {
		t.Errorf("%s arms or seeds something. Criterion 6: rules are armed by capture_rule_add through the executor "+
			"(Verification steps 5-6), never by a migration", rel)
	}
	if strings.Contains(sql, "tt-") || strings.Contains(sql, "in qa") {
		t.Errorf("%s carries a Jira status-name literal. Names live in data, never in anything that ships", rel)
	}
	if strings.Contains(sql, "drop column") || strings.Contains(sql, "drop table") {
		t.Errorf("%s drops a column or table. Migrations here are FORWARD-ONLY", rel)
	}
	if !strings.Contains(comments, "drop_reason") || !strings.Contains(comments, "supersede") {
		t.Errorf("%s does not supersede 0023's \"drop_reason NULL unless dropped\" in a comment of its own. 0023 is "+
			"applied and unamendable, and a resurfaced row records the drop fact it holds off", rel)
	}
}

// Criterion 6: "a new pin asserts 0030's six-value set" — EXACTLY six, so a
// seventh value cannot ride in unnoticed and none of SWT-32's five is lost.
func TestMigration0030_LastActionVocabularyIsExactlySix(t *testing.T) {
	rel := rs0030(t)
	sql, _ := rsSQL(t, rel)
	m := regexp.MustCompile(`(?s)add\s+constraint\s+ticket_status_syncs_last_action_check\s+check\s*\(\s*last_action\s+in\s*\(([^)]*)\)`).FindStringSubmatch(sql)
	if m == nil {
		t.Fatalf("%s does not ADD CONSTRAINT ticket_status_syncs_last_action_check CHECK (last_action IN (...)); "+
			"without the ADD, last_action is left UNCONSTRAINED", rel)
	}
	var got []string
	for _, v := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(m[1], -1) {
		got = append(got, v[1])
	}
	sort.Strings(got)
	want := []string{"closed", "none", "refused_active", "reopened", "resurfaced", "suppressed_dismissed"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s's last_action CHECK allows %v, want exactly %v", rel, got, want)
	}
}

// Criterion 1: "the ledger accepts 30 and still fails any unowned number".
// The ledger itself is internal/classify/structure_test.go (the living
// registry); this is the reminder that fires from the ticket creating the number.
func TestMigrationLedger_LearnsThirty(t *testing.T) {
	const rel = "internal/classify/structure_test.go"
	ledger := regexp.MustCompile(`if\s+n\s*>\s*17(\s*&&\s*n\s*!=\s*\d+)+`).FindString(tsRepoFile(t, rel))
	if ledger == "" {
		t.Fatalf("%s no longer holds the `if n > 17 && n != 18 ...` migration ledger", rel)
	}
	if !regexp.MustCompile(`n\s*!=\s*27\b`).MatchString(ledger) {
		t.Fatalf("the ledger (%s) does not exempt 27 — the control for the assertion below", ledger)
	}
	if !regexp.MustCompile(`n\s*!=\s*30\b`).MatchString(ledger) {
		t.Errorf("the migration ledger in %s does not exempt 30 (SWT-45). The migrate runner keys on "+
			"schema_migrations.version with NO checksum, so a file nobody's ticket owns is skipped SILENTLY", rel)
	}
	if regexp.MustCompile(`n\s*<\s*\d+|n\s*>=\s*\d+`).MatchString(ledger) {
		t.Errorf("the ledger (%s) uses a range; it must name owned numbers one by one so an unowned one still fails", ledger)
	}
}

// ---- criterion 29: decide.go stays clock-free ---------------------------------

// "TestDecideGo_IsPure stays green and additionally bans time.Now in
// decide.go." decide.go now imports "time" for the SurfacedAt/SurfacedSeen
// VALUES; the ban is what keeps a value from becoming a lookup of the clock.
func TestDecideGo_BansTimeNow(t *testing.T) {
	src := tsRepoFile(t, "internal/ticketstatus/decide.go")
	if !strings.Contains(src, "func Decide(") {
		t.Fatalf("decide.go does not declare Decide")
	}
	for _, banned := range []string{"time.Now", "time.Since", "time.Until"} {
		if strings.Contains(src, banned) {
			t.Errorf("decide.go calls %s. Criterion 29: SurfacedAt and SurfacedSeen arrive as VALUES; a clock read "+
				"inside Decide makes the decision table stop proving anything (invariant 7)", banned)
		}
	}
}

// ---- criterion 35: the counter ---------------------------------------------------

func TestTicketStatus_StatsHasAResurfacedCounterPrintedInBothLines(t *testing.T) {
	if _, ok := reflect.TypeOf(ticketstatus.Stats{}).FieldByName("Resurfaced"); !ok {
		t.Errorf("ticketstatus.Stats has no Resurfaced field (criterion 35)")
	}
	for rel, want := range map[string]string{
		"cmd/connectors/jira/main.go": `\"resurfaced\":%d`,
		"cmd/opsctl/main.go":          `resurfaced=%d`,
	} {
		src := tsRepoFile(t, rel)
		i := strings.Index(src, "ticket_status:")
		if i < 0 {
			t.Fatalf("%s no longer prints a ticket_status: counter line", rel)
		}
		line := src[i:]
		if j := strings.Index(line, ")\n"); j > 0 {
			line = line[:j]
		}
		if !strings.Contains(line, want) {
			t.Errorf("%s's ticket_status counter line does not print %s. Criterion 35: Verification step 7 reads "+
				"resurfaced=1 then resurfaced=0 — a counter that is never printed cannot be read", rel, want)
		}
	}
}

// ---- criterion 40: the ticket-status-sync runbook -----------------------------

func TestRunbook_DocumentsSurfacedByActivity(t *testing.T) {
	raw := tsRepoFile(t, "docs/runbooks/ticket-status-sync.md")
	doc := strings.ToLower(raw)

	if strings.Contains(doc, "the gap, until `qa-question-resurface` ships") {
		t.Errorf("the runbook still carries \"The gap, until `qa-question-resurface` ships\" and its stand-in query. " +
			"Criterion 40: it is REPLACED by \"Surfaced by activity\" — J11 subsumes SWT-34 E9 for jira-keyed tasks")
	}
	i := strings.Index(doc, "surfaced by activity")
	if i < 0 {
		t.Fatalf("docs/runbooks/ticket-status-sync.md has no \"Surfaced by activity\" section (criterion 40)")
	}
	section := doc[i:]
	if j := strings.Index(section[1:], "\n## "); j > 0 {
		section = section[:j+1]
	}
	for _, want := range []struct{ frag, why string }{
		{"resurfaced", "the new last_action and counter"},
		{"assignee", "how a hold ends: a status, status-name or assignee change"},
		{"status name", "same"},
		{"by hand", "J8: a human's plain task_reopen surfaces (sticky), and a hand close ends the hold for good"},
		{"task_reopen", "J8's verb, and J15's backfill recipe"},
	} {
		if !strings.Contains(section, want.frag) {
			t.Errorf("the runbook's \"Surfaced by activity\" section never mentions %q — %s", want.frag, want.why)
		}
	}
	if !regexp.MustCompile(`clos\w*[^.\n]{0,60}(e-?mail|mail)|(e-?mail|mail)[^.\n]{0,60}clos`).MatchString(section) {
		t.Errorf("the \"Surfaced by activity\" section does not state J10's cost: a ticket-closed EMAIL that arrives " +
			"after the reconciler's close revives the task, which then stays until one hand close")
	}

	// The SWT-36 D9 paragraph ("this pass closes it in the same jira tick") gains
	// the overriding-rule exception.
	for _, para := range strings.Split(doc, "\n\n") {
		if strings.Contains(para, "same jira tick") && strings.Contains(para, "d9") {
			if !strings.Contains(para, "overriding") {
				t.Errorf("the runbook's SWT-36 D9 paragraph still says an activity-reopened task is closed in the same "+
					"jira tick, with no overriding-rule exception (criterion 40, J11: a revive by an overriding rule is "+
					"HELD).\n---\n%s\n---", strings.TrimSpace(para))
			}
			return
		}
	}
	t.Errorf("the runbook no longer has the SWT-36 D9 paragraph mentioning \"same jira tick\"; criterion 40's third " +
		"bullet has nothing to amend")
}

// ---- criterion 41: the IK entry ----------------------------------------------

func TestInstitutionalKnowledge_RecordsSWT45(t *testing.T) {
	doc := strings.ToLower(tsRepoFile(t, ".claude/INSTITUTIONAL_KNOWLEDGE.md"))
	var section string
	for _, loc := range regexp.MustCompile(`(?m)^#{2,4} [^\n]*swt-45[^\n]*$`).FindAllStringIndex(doc, -1) {
		section = doc[loc[0]:]
		if j := regexp.MustCompile(`(?m)^#{2,3} `).FindStringIndex(section[1:]); j != nil {
			section = section[:j[0]+1]
		}
		break
	}
	if section == "" {
		t.Fatalf(".claude/INSTITUTIONAL_KNOWLEDGE.md has no heading naming SWT-45 (criterion 41)")
	}
	for _, want := range []struct {
		frags []string
		why   string
	}{
		{[]string{"schema", "argument"}, "F7: absence from an MCP SCHEMA is not a boundary for ARGUMENTS — the adapter passes them through"},
		{[]string{"unique", "pattern"}, "F8: capture_rules is UNIQUE (project_id, criteria_type, pattern); a rule cannot be re-added with the same pattern"},
		{[]string{"closed_at", "updated_at"}, "the NULL closed_at -> updated_at fallback"},
		{[]string{"close", "mail"}, "J10's cost: the close email revives a task the reconciler just closed"},
	} {
		for _, f := range want.frags {
			if !strings.Contains(section, f) {
				t.Errorf("the IK's SWT-45 entry never mentions %q — %s", f, want.why)
			}
		}
	}
}
