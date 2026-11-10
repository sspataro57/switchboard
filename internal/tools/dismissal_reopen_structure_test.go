package tools_test

// Structural tests for SWT-36 (docs/tickets/dismiss-reopen-on-activity_SPEC.md)
// criteria 6 (the partial-index ON CONFLICT is restated everywhere), 7
// (task_reopen stays off the MCP schemas) and 19 (migration 0026's shape).
// (Criterion 7's scan is AMENDED — not deleted — by mcp-task-reopen, 2026-11-10:
// see TestTaskReopen_StaysOffTheMCPSchemas.)
// ZERO I/O beyond reading this repo's own source. Same shape as
// dismiss_structure_test.go's TestMigration0022_* guard, whose helper
// (dsRepoFile) this file reuses.
//
// IMPOSED SURFACE (the SPEC's "Data model changes", verbatim in substance):
//
//	migrations/0026_dismissal_reopen.sql
//	  ALTER TABLE task_dismissals
//	    ADD COLUMN closed_from_status     TEXT,
//	    ADD COLUMN reopened_at            TIMESTAMPTZ,
//	    ADD COLUMN reopened_by            TEXT,
//	    ADD COLUMN reopened_by_message_id BIGINT REFERENCES normalized_messages(id) ON DELETE SET NULL,
//	    ADD CONSTRAINT task_dismissals_reopen_pair CHECK ((reopened_at IS NULL) = (reopened_by IS NULL)),
//	    ADD CONSTRAINT task_dismissals_reopen_msg  CHECK (reopened_by_message_id IS NULL OR reopened_at IS NOT NULL);
//	  UPDATE task_dismissals d SET reopened_at = now(), reopened_by = 'migration:0026'
//	    FROM tasks t WHERE t.id = d.task_id AND t.status <> 'closed' AND d.reopened_at IS NULL;
//	  DROP INDEX task_dismissals_task_uniq;
//	  CREATE UNIQUE INDEX task_dismissals_open_uniq ON task_dismissals (task_id) WHERE reopened_at IS NULL;
//
//	internal/tools/close.go, dismissTask's insert:
//	  ... ON CONFLICT (task_id) WHERE reopened_at IS NULL DO NOTHING
//
// GREENFIELD NOTE — EXPECTED RED: 0026 does not exist, and close.go's insert
// still spells the TOTAL-index `ON CONFLICT (task_id) DO NOTHING`. Criterion 7's
// scan is GREEN today and must stay green — it is a guard, not a discovery.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// drSQLCode strips `--` line comments so a structural regex cannot be
// satisfied (or miscounted) by prose. The comments are checked separately.
func drSQLCode(sql string) (code, comments string) {
	var c, k strings.Builder
	for _, line := range strings.Split(sql, "\n") {
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

// ---- criterion 19: migration 0026 -------------------------------------------

func TestMigration0026_DismissalReopenShape(t *testing.T) {
	// Control first: 0025 must still be there, or an empty glob below would
	// prove only that the directory moved.
	if _, err := os.Stat(filepath.Join("..", "..", "migrations", "0025_ticket_delivered_statuses.sql")); err != nil {
		t.Fatalf("migrations/0025_ticket_delivered_statuses.sql is missing: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join("..", "..", "migrations", "0026_*.sql"))
	if err != nil {
		t.Fatalf("glob migrations/0026_*.sql: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("found %d migrations/0026_*.sql file(s), want exactly 1 (the SPEC names "+
			"0026_dismissal_reopen.sql; 0025 is the current highest). Merging a migration is not applying "+
			"it — check `SELECT max(version) FROM schema_migrations` = 25 before deploying, and apply 0026 "+
			"BEFORE any image carrying this code", len(matches))
	}
	if base := filepath.Base(matches[0]); base != "0026_dismissal_reopen.sql" {
		t.Errorf("migration 0026 is named %s, want 0026_dismissal_reopen.sql (the SPEC's name)", base)
	}
	rel := filepath.Join("migrations", filepath.Base(matches[0]))
	raw := strings.ToLower(dsRepoFile(t, rel))
	sql, comments := drSQLCode(raw)

	// (1) the four columns, with their types.
	for _, col := range []struct{ re, why string }{
		{`add\s+column\s+closed_from_status\s+text\b`,
			"D5: the pre-dismissal status the activity reopen restores; NULL = was already closed / pre-0026"},
		{`add\s+column\s+reopened_at\s+timestamptz\b`,
			"D6: NULL means the dismissal is OPEN — the predicate every reader and the partial index key on"},
		{`add\s+column\s+reopened_by\s+text\b`,
			"D6: the executor actor of the reopen"},
		{`add\s+column\s+reopened_by_message_id\s+bigint\s+references\s+normalized_messages\s*\(\s*id\s*\)\s+on\s+delete\s+set\s+null`,
			"D6/D7: the ONE typed place the outcome lives; SET NULL, not CASCADE — deleting a message must not delete a human label"},
	} {
		if !regexp.MustCompile(col.re).MatchString(sql) {
			t.Errorf("%s does not match /%s/ — %s", rel, col.re, col.why)
		}
	}
	if regexp.MustCompile(`reopened_by_message_id[^,;]*on\s+delete\s+cascade`).MatchString(sql) {
		t.Errorf("%s cascades task_dismissals from normalized_messages. The SPEC says ON DELETE SET NULL: "+
			"a dismissal is a human judgement and outlives the message that overtook it", rel)
	}
	// No CHECK on closed_from_status (the 0023 precedent): openStatuses in
	// close.go stays the ONE spelling, and the handler's fallback to ready is the guard.
	if regexp.MustCompile(`check\s*\([^;]*closed_from_status`).MatchString(sql) {
		t.Errorf("%s puts a CHECK on closed_from_status. The SPEC refuses one on purpose: the status list "+
			"lives once, in close.go's openStatuses, and a second spelling in SQL is how the two drift", rel)
	}

	// (2) both CHECKs, by name.
	for _, ck := range []struct{ re, why string }{
		{`constraint\s+task_dismissals_reopen_pair\s+check\s*\(\s*\(\s*reopened_at\s+is\s+null\s*\)\s*=\s*\(\s*reopened_by\s+is\s+null\s*\)\s*\)`,
			"a reopen with no author (or an author with no reopen) is a row nothing can interpret"},
		{`constraint\s+task_dismissals_reopen_msg\s+check\s*\(\s*reopened_by_message_id\s+is\s+null\s+or\s+reopened_at\s+is\s+not\s+null\s*\)`,
			"a message id on an OPEN dismissal would read as 'overtaken' while the task is still down"},
	} {
		if !regexp.MustCompile(ck.re).MatchString(sql) {
			t.Errorf("%s does not match /%s/ — %s", rel, ck.re, ck.why)
		}
	}

	// (3) the TOTAL index is dropped. Under it, re-dismissing after an activity
	// reopen is a silent label loss (DO NOTHING), and the owner's rule breaks on
	// the second dismissal (D6).
	if !regexp.MustCompile(`drop\s+index\s+(if\s+exists\s+)?task_dismissals_task_uniq\b`).MatchString(sql) {
		t.Errorf("%s does not DROP INDEX task_dismissals_task_uniq (0022's total unique index). D6: with it "+
			"in place a task can hold only one dismissal row ever", rel)
	}

	// (4) the partial unique: at most one OPEN dismissal per task.
	if !regexp.MustCompile(`create\s+unique\s+index\s+task_dismissals_open_uniq\s+on\s+task_dismissals\s*\(\s*task_id\s*\)\s*where\s+reopened_at\s+is\s+null`).MatchString(sql) {
		t.Errorf("%s does not CREATE UNIQUE INDEX task_dismissals_open_uniq ON task_dismissals (task_id) "+
			"WHERE reopened_at IS NULL. D6: one OPEN dismissal per task, any number over time", rel)
	}

	// (5) D14's backfill: an open dismissal must imply a closed task.
	backfill := regexp.MustCompile(`update\s+task_dismissals\b[^;]*set\s[^;]*reopened_at\s*=\s*now\(\)[^;]*` +
		`reopened_by\s*=\s*'migration:0026'[^;]*status\s*<>\s*'closed'[^;]*reopened_at\s+is\s+null`)
	if !backfill.MatchString(sql) {
		t.Errorf("%s has no D14 backfill (UPDATE task_dismissals SET reopened_at = now(), reopened_by = "+
			"'migration:0026' ... WHERE t.status <> 'closed' AND d.reopened_at IS NULL). Without it a task "+
			"reopened by psql or by SWT-32's reopen keeps an OPEN dismissal, and re-dismissing it conflicts on "+
			"the partial index and loses the label", rel)
	}

	// (6) no other index, and no new table (invariant 2: task_dismissals stays a log).
	if n := len(regexp.MustCompile(`create\s+(unique\s+)?index\b`).FindAllString(sql, -1)); n != 1 {
		t.Errorf("%s creates %d indexes, want exactly 1 (task_dismissals_open_uniq). 0022's argument still "+
			"holds: an index nothing uses is a permanent claim that some query needs it", rel, n)
	}
	if regexp.MustCompile(`create\s+table\b`).MatchString(sql) {
		t.Errorf("%s creates a table. The SPEC adds none: task_dismissals stays a LOG (invariant 2), and the "+
			"reopen outcome is three columns on it", rel)
	}

	// (7) the IK partial-index landmine, stated where the next writer will look.
	if !strings.Contains(comments, "on conflict") || !strings.Contains(comments, "reopened_at is null") ||
		!strings.Contains(comments, "restat") {
		t.Errorf("%s's comments do not carry the partial-index landmine: every ON CONFLICT against "+
			"task_dismissals must RESTATE `WHERE reopened_at IS NULL`, or arbiter inference fails at runtime "+
			"with 'no unique or exclusion constraint matching the ON CONFLICT specification' (the "+
			"capture_decisions_live_uniq / task_events_outbound_observed_uniq precedent)", rel)
	}
}

// ---- criterion 6: every ON CONFLICT against task_dismissals restates the predicate

// "task_dismiss's insert is `ON CONFLICT (task_id) WHERE reopened_at IS NULL DO
// NOTHING`, with the predicate RESTATED."
//
// Structural because the failure is a RUNTIME error on a human's click, not a
// compile error, and not even a wrong answer on a fresh db that still carries
// the total index. Every non-test file under internal/ is scanned, so a second
// writer added later is held to the same spelling.
func TestDismissals_EveryOnConflictRestatesThePartialPredicate(t *testing.T) {
	insert := regexp.MustCompile("(?is)insert\\s+into\\s+task_dismissals\\b[^`]*")
	conflict := regexp.MustCompile(`(?is)on\s+conflict\b`)
	restated := regexp.MustCompile(`(?is)on\s+conflict\s*\(\s*task_id\s*\)\s*where\s+reopened_at\s+is\s+null\s+do\s+nothing`)

	sawCloseGo := false
	err := filepath.Walk(filepath.Join("..", "..", "internal"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel := strings.TrimPrefix(filepath.ToSlash(path), "../../")
		for _, stmt := range insert.FindAllString(string(b), -1) {
			if rel == "internal/tools/close.go" {
				sawCloseGo = true
			}
			if conflict.MatchString(stmt) && !restated.MatchString(stmt) {
				t.Errorf("%s inserts into task_dismissals with an ON CONFLICT that does not restate the partial "+
					"predicate:\n%s\nCriterion 6 / 0026: the unique index is PARTIAL (WHERE reopened_at IS NULL), "+
					"so the arbiter must say `ON CONFLICT (task_id) WHERE reopened_at IS NULL DO NOTHING`", rel,
					strings.TrimSpace(stmt))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
	if !sawCloseGo {
		t.Fatalf("no INSERT INTO task_dismissals found in internal/tools/close.go — the scan saw nothing to " +
			"check, which proves nothing (dismissTask is the ONE writer)")
	}
}

// ---- criterion 7: task_reopen stays spine-facing -----------------------------

// "task_reopen stays off internal/mcpserver/schemas.go and out of humanOnly."
// The humanOnly half is internal/policy/matrix_reopen_test.go; the MCP-listing
// half is also asserted behaviourally in internal/mcpserver/adapter_test.go
// (SWT-32 criterion 39). This is the source half: a schema entry is the first
// step of exposing a verb to workers, and it is where the change would be made.
//
// AMENDED — not deleted — by mcp-task-reopen (docs/tickets/mcp-task-reopen_SPEC.md,
// 2026-11-10). Salvador's decision ("add ability to claude to reopen taks on
// swb") MCP-lists task_reopen in its PLAIN form, so schemas.go now names
// "task_reopen" and the absence check above is retired. The concern it guarded
// — a worker resurrecting dismissed work "with a message id it chose itself" —
// is kept as a source-level F7 check: the task_reopen entry (from its Name: to
// the line that closes the entry) names none of dismissal_id, message_id,
// revive, notifier_copy. The schema is NOT the boundary (IK LANDMINE F7: an
// argument absent from the schema is not absent from the call); the adapter
// guard rejectReopenActivityArgs is, pinned in internal/mcpserver/reopen_test.go.
// This scan only keeps the schema from ADVERTISING the activity forms. The
// function keeps its SWT-36 name so the criterion-7 trail still greps.
//
// The entry ends at the first line that is exactly "}," after trimming — NOT
// at the first "}," in the text, which occurs inside the schema's JSON literal
// right after "task_id":{"type":"integer"} and would scan almost nothing.
func TestTaskReopen_StaysOffTheMCPSchemas(t *testing.T) {
	src := dsRepoFile(t, "internal/mcpserver/schemas.go")
	// Control: the scan must be looking at the file that lists tools at all.
	if !strings.Contains(src, `"task_append_log"`) {
		t.Fatalf("internal/mcpserver/schemas.go does not mention \"task_append_log\"; this scan is not " +
			"looking at the tool schema list it claims to check")
	}
	loc := regexp.MustCompile(`Name:\s*"task_reopen"`).FindStringIndex(src)
	if loc == nil {
		t.Fatalf("internal/mcpserver/schemas.go has no `Name: \"task_reopen\"` entry. mcp-task-reopen criterion 1: " +
			"the plain form is MCP-listed on both profiles")
	}
	rest := src[loc[0]:]
	entry := ""
	for i, line := range strings.SplitAfter(rest, "\n") {
		entry += line
		if i > 0 && strings.TrimSpace(line) == "}," {
			break
		}
	}
	if !strings.Contains(entry, "InputSchema") {
		t.Fatalf("the scanned task_reopen entry carries no InputSchema — the entry boundary is wrong, so the scan "+
			"below proves nothing. Scanned: %q", entry)
	}
	for _, forbidden := range []string{"dismissal_id", "message_id", "revive", "notifier_copy"} {
		if strings.Contains(entry, forbidden) {
			t.Errorf("schemas.go's task_reopen entry names %q. mcp-task-reopen F7 / criterion 2: MCP serves the "+
				"PLAIN form only; the guarded, revive and notifier_copy forms carry an inbound message id the "+
				"handler trusts and stay capture/promote-only. A schema that advertises them invites the call "+
				"the adapter guard must then refuse", forbidden)
		}
	}
}
