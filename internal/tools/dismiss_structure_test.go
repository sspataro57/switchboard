package tools_test

// Structural tests for SWT-31 (docs/tickets/board-dismissals_SPEC.md) Part 2 —
// the criteria the SPEC asks to be enforced mechanically: 7 (migration 0022's
// shape), 11 (the refusal is SHARED, not restated) and 20 (nothing reads a jsonb
// payload for a label). ZERO I/O beyond reading this repo's own source.
//
// Same shape as internal/promote/structure_test.go's migration guard and
// internal/classify/structure_test.go's repo walk, retargeted. Every assertion
// REQUIRES its subject to exist first: a source scan that passes because there
// was nothing to scan is this repo's "fixture that proves nothing" landmine
// wearing a lab coat.
//
// GREENFIELD NOTE — EXPECTED RED. migrations/0022_task_dismissals.sql does not
// exist and internal/tools/close.go declares no shared transition helper, so the
// guards below fail until both land. Criterion 20's scan is GREEN today and must
// stay green — it is a guard, not a discovery.
//
// Criterion 22, obeyed here: this ticket takes NO advisory lock and this file
// writes no 0x5157 literal. `TestAdvisoryLockKey_HasNoCollisionInTheRepo` walks
// internal/ for classify's key and fails any file outside internal/classify that
// contains it; the file this ticket adds is migration 0022, and the two families
// share digits and nothing else.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func dsRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// ---- criterion 7: migration 0022 is the only one this ticket adds -------------

func TestMigration0022_IsTheOnlyOneThisTicketAdds(t *testing.T) {
	// Control first: 0021 must still be there, or a glob returning nothing below
	// would prove only that the directory moved.
	if _, err := os.Stat(filepath.Join("..", "..", "migrations", "0021_classify_promotion.sql")); err != nil {
		t.Fatalf("migrations/0021_classify_promotion.sql is missing: %v", err)
	}

	matches, err := filepath.Glob(filepath.Join("..", "..", "migrations", "0022_*.sql"))
	if err != nil {
		t.Fatalf("glob migrations/0022_*.sql: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("found %d migrations/0022_*.sql file(s), want exactly 1 (the SPEC names "+
			"0022_task_dismissals.sql). 0021 is the current highest, and this ticket's data-model "+
			"section is ONE migration. Merging a migration is not applying it — check "+
			"`SELECT max(version) FROM schema_migrations` before deploying", len(matches))
	}
	if extra, _ := filepath.Glob(filepath.Join("..", "..", "migrations", "0023_*.sql")); len(extra) != 0 {
		t.Errorf("found %d migrations/0023_*.sql file(s): SWT-31 adds ONE migration, and criterion 21 "+
			"says nothing above 0022 exists", len(extra))
	}

	rel := filepath.Join("migrations", filepath.Base(matches[0]))
	sql := strings.ToLower(dsRepoFile(t, rel))

	if !regexp.MustCompile(`(?s)create\s+table\s+task_dismissals`).MatchString(sql) {
		t.Fatalf("%s does not CREATE TABLE task_dismissals. D2: the label is a TYPED TABLE, not a "+
			"task_events.payload key — capture_decisions (0015) and classify_promotions (0021) are the "+
			"precedent, and an untyped predicate over jsonb is the thing this repo keeps paying for", rel)
	}

	// (1) the FK, and the cascade that keeps integration cleanups honest.
	cascade := regexp.MustCompile(`(?s)task_id[^,]*references\s+tasks\s*\(\s*id\s*\)\s*on\s+delete\s+cascade`)
	if !cascade.MatchString(sql) {
		t.Errorf("%s does not declare task_id ... REFERENCES tasks(id) ON DELETE CASCADE. A dismissal "+
			"without its task means nothing, and the integration suites clear fixtures by deleting "+
			"tasks — without the cascade they fail INSIDE cleanup, which reads like the "+
			"cross-pollution pact breaking rather than like a new FK", rel)
	}

	// (2) D4's enum as a CHECK constraint, all four values by name.
	check := regexp.MustCompile(`(?s)reason_code[^,]*check\s*\(\s*reason_code\s+in\s*\(([^)]*)\)`)
	m := check.FindStringSubmatch(sql)
	if m == nil {
		t.Errorf("%s does not constrain reason_code with CHECK (reason_code IN (...)). D4: a free-text "+
			"reason_code is a column nothing can GROUP BY — the `kind`-enum argument from "+
			"internal/classify's schema, verbatim. Widening the set later is one more migration, and "+
			"that cost is stated", rel)
	} else {
		for _, want := range []string{"not_actionable", "wrong_kind", "duplicate", "handled_elsewhere"} {
			if !strings.Contains(m[1], "'"+want+"'") {
				t.Errorf("%s's reason_code CHECK (%s) does not allow %q", rel, strings.TrimSpace(m[1]), want)
			}
		}
	}

	// (3) dismissed_by NOT NULL. It duplicates the executor actor already in
	// audit_events, deliberately: the label is training data and must be
	// readable with ONE typed query, not by parsing audit_events.args.
	if !regexp.MustCompile(`(?s)dismissed_by\s+text\s+not\s+null`).MatchString(sql) {
		t.Errorf("%s does not declare dismissed_by TEXT NOT NULL. A label with no author is not "+
			"training data — 'which human judged this' is the column the future precision ticket "+
			"filters on", rel)
	}
	for _, col := range []string{"note", "created_at"} {
		if !strings.Contains(sql, col) {
			t.Errorf("%s has no %s column", rel, col)
		}
	}

	// (4) criterion 13's idempotency, structurally: the unique index is TOTAL.
	uniq := regexp.MustCompile(`(?s)create\s+unique\s+index\s+\S+\s+on\s+task_dismissals\s*\(\s*task_id\s*\)([^;]*)`)
	um := uniq.FindStringSubmatch(sql)
	if um == nil {
		t.Errorf("%s has no UNIQUE index on task_dismissals (task_id). The index IS criterion 13: "+
			"`ON CONFLICT (task_id) DO NOTHING` is what makes a second dismiss a success that adds "+
			"no second row", rel)
	} else if strings.Contains(um[1], "where") {
		t.Errorf("%s makes the unique index PARTIAL (%q). The SPEC says TOTAL on purpose: a partial "+
			"index forces every ON CONFLICT to restate the predicate (capture_decisions_live_uniq and "+
			"task_events_outbound_observed_uniq both do), and omitting it raises 'no unique or "+
			"exclusion constraint matching the ON CONFLICT specification' at RUNTIME", rel, strings.TrimSpace(um[1]))
	}

	// (5) "and no other index, deliberately." An index nothing uses is a
	// permanent claim that some query needs it, which the next reader has to
	// disprove (0018's recorded argument).
	idx := regexp.MustCompile(`create\s+(unique\s+)?index\s+\S+\s+on\s+task_dismissals`)
	if n := len(idx.FindAllString(sql, -1)); n != 1 {
		t.Errorf("%s creates %d indexes on task_dismissals, want exactly 1 (the total unique on task_id). "+
			"The join targets hold tens to hundreds of rows and every reader reaches tasks by primary "+
			"key", rel, n)
	}

	// (6) invariant 2: it is a LOG, not a second tasks table.
	table := sql
	if i := strings.Index(sql, "create table task_dismissals"); i >= 0 {
		table = sql[i:]
		if j := strings.Index(table, ");"); j > 0 {
			table = table[:j]
		}
	}
	for _, banned := range []struct{ col, why string }{
		{"status", "a dismissal has no lifecycle; the dismissed item stays a row in `tasks` with status='closed'"},
		{"assignee", "nothing ever works a dismissal row"},
		{"claim", "no claim: queues are FILTERS on the one tasks table, never new tables"},
		{"priority", "same"},
	} {
		if strings.Contains(table, banned.col) {
			t.Errorf("%s's task_dismissals declares a %q column — invariant 2: %s", rel, banned.col, banned.why)
		}
	}
	if !strings.Contains(sql, "invariant 2") {
		t.Errorf("%s does not say out loud that task_dismissals is not a second tasks table. 0015 and "+
			"0021 both state it in the migration comment, and the SPEC asks this one to as well — the "+
			"next reader's first instinct is to add a status column", rel)
	}
}

// ---- criterion 11: ONE refusal, shared by both verbs --------------------------

// "The refusal is shared, not restated: closeTask and dismissTask call one
// unexported helper, and task_dismiss refuses claimed, in_progress and
// needs_feedback with the same message `task %d is %s; refusing to close active
// work`."
//
// Structural because the failure mode is silent drift: two spellings of the
// active-work list stay identical for exactly as long as nobody edits one of
// them, and the divergence shows up as a dismissal that closes work out from
// under a running worker.
func TestDismiss_SharesOneRefusalWithClose(t *testing.T) {
	const rel = "internal/tools/close.go"
	src := dsRepoFile(t, rel)

	if !strings.Contains(src, "func dismissTask(") {
		t.Fatalf("%s does not declare dismissTask; the SPEC puts it here, beside closeTask, precisely so "+
			"the shared helper is unavoidable", rel)
	}
	const refusal = `refusing to close active work`
	if n := strings.Count(src, refusal); n != 1 {
		t.Errorf("%s contains the refusal message %q %d times, want exactly 1. D3: `closeTask` and "+
			"`dismissTask` call ONE shared, unexported transition helper — reuse was the preference "+
			"and this is where it matters. A second spelling of the active-work list drifts silently",
			rel, refusal, n)
	}
	// And the list itself is written once.
	if n := strings.Count(src, `"claimed", "in_progress", "needs_feedback"`); n > 1 {
		t.Errorf("%s spells the active-work status list %d times; it belongs in the shared helper only", rel,
			strings.Count(src, `"claimed", "in_progress", "needs_feedback"`))
	}
}

// ---- criterion 20: no jsonb payload predicate for a label ---------------------

// "Nothing reads a jsonb payload for a label: a structural test asserts no
// `payload->>'reason'`, `payload->>'reason_code'` or equivalent appears in
// internal/ in connection with dismissals."
//
// SCOPED, and the scoping is the honest part — twice.
//
//  1. `payload->>'reason_code'` and any `payload->>'dismiss*'` key are banned
//     EVERYWHERE, tests included: there is no legitimate reader of a dismissal
//     label in task_events, because the label is not written there (D2).
//  2. `payload->>'reason'` is banned only in NON-TEST files that mention
//     dismissals. Criterion 12 requires the status_changed event to keep
//     carrying a PROSE reason composed from the code and the note, so a test
//     characterizing that payload must read it — and one does
//     (internal/dashboard/board_dismiss_integration_test.go). Production code
//     reaching for the same string is the label query D2 refuses; a test
//     asserting the event's shape is the thing that keeps the shape honest.
//
// `payload->>'reason'` already appears in internal/orchestrator/integration_test.go
// too, reading the prose reason of an ordinary close. That is fine and stays fine.
func TestDismissals_NeverQueryAJSONBPayloadForALabel(t *testing.T) {
	labelKey := regexp.MustCompile(`payload\s*->>\s*'(reason_code|dismiss\w*)'`)
	proseReason := regexp.MustCompile(`payload\s*->>\s*'reason'`)
	anyPayload := regexp.MustCompile(`payload\s*->>`)

	sawAnyPayloadAccessor := false
	sawANonTestFile := false
	err := filepath.Walk(filepath.Join("..", "..", "internal"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		src := string(b)
		rel := strings.TrimPrefix(filepath.ToSlash(path), "../../")
		if rel == "internal/tools/dismiss_structure_test.go" {
			return nil // this file names the patterns it bans
		}
		if anyPayload.MatchString(src) {
			sawAnyPayloadAccessor = true
		}
		isTest := strings.HasSuffix(rel, "_test.go")
		if !isTest {
			sawANonTestFile = true
		}
		if m := labelKey.FindString(src); m != "" {
			t.Errorf("%s contains %q. D2: the label is task_dismissals.reason_code, a typed column a "+
				"future precision ticket can GROUP BY. The status_changed event still carries a PROSE "+
				"reason (criterion 12) and nothing may query that payload for labels", rel, m)
		}
		if !isTest && strings.Contains(strings.ToLower(src), "dismiss") {
			if m := proseReason.FindString(src); m != "" {
				t.Errorf("%s mentions dismissals AND contains %q. Criterion 20: a dismissal label read "+
					"out of task_events.payload is exactly the untyped predicate D2 refuses — "+
					"task_dismissals is the store, task_events is not. (A TEST may read the payload to "+
					"characterize criterion 12's prose reason; production code reaching for it is the "+
					"report query this ban exists to stop.)", rel, m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
	// Controls: the regexes must be able to see a payload accessor at all, and
	// the walk must reach non-test files, or a green result means "the scan
	// matched nothing" rather than "no label query".
	if !sawAnyPayloadAccessor {
		t.Fatalf("the scan found no `payload->>` anywhere in internal/, yet internal/orchestrator/facts.go " +
			"and internal/tools/prci.go both use it; this scan is not seeing the files it claims to check")
	}
	if !sawANonTestFile {
		t.Fatalf("the scan visited no non-test .go file under internal/; the half of the ban that matters " +
			"(production code) was never applied")
	}
}

// The runbook half of criterion 23, kept with the other structural guards: the
// two facts a future reader needs before writing a report query. A test on prose
// earns its place the same way TestRunbook_DocumentsCaptureBeforeTriage does —
// nothing in the code can stop someone counting labels out of task_events.
func TestRunbook_RecordsTheTitleShapeAndTheLabelStore(t *testing.T) {
	doc := strings.ToLower(dsRepoFile(t, "docs/runbooks/capture-rules.md"))

	if !strings.Contains(doc, "task_dismissals") {
		t.Errorf("docs/runbooks/capture-rules.md never mentions task_dismissals. Criterion 23: a runbook " +
			"line must state that task_dismissals is the labelled-data store and task_events is NOT — " +
			"the prose reason in the status_changed payload is there for humans reading a task, and it " +
			"is the first thing a report query would reach for")
	}
	if !strings.Contains(doc, "task_events") {
		t.Errorf("the runbook says where labels live but not where they do NOT. Naming task_events " +
			"explicitly is the point: the negative is what stops the next GROUP BY")
	}
	// The title shape, with a before/after example, so the change is legible to
	// whoever next reads a capture-created board row.
	if !strings.Contains(doc, "—") || !strings.Contains(doc, "sender") {
		t.Errorf("the runbook does not record the new title shape ({sender} — {first line}, falling back " +
			"to the project name). Criterion 23 asks for a before/after example, because five live " +
			"titles were corrected by hand and nothing in code can re-title them (D5)")
	}
}
