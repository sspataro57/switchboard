package ticketstatus_test

// Structural tests for SWT-34 (docs/tickets/qa-delivered-drop_SPEC.md) — the
// criteria this ticket asks to be enforced mechanically rather than by review:
// 1-4 (migration 0025's shape), 6 (the ledger learns 25), 9 (the fold's file),
// 12 (the fold is NEVER re-spelled in SQL), 22 (only the driver and the report
// read the column), 25 (the counter vocabulary), 27 (the report's two columns),
// 31 (the two stale comments) and 32 (the runbook, including E10's deferred
// gap).
//
// ZERO I/O beyond reading this repo's own source and docs — same shape as
// structure_test.go, whose helpers (tsRepoFile, tsSources, tsFileImports) this
// file reuses rather than re-declaring. Every assertion REQUIRES its subject to
// exist first: a source scan that passes because there was nothing to scan is
// this repo's "fixture that proves nothing" landmine wearing a lab coat.
//
// GREENFIELD NOTE — EXPECTED RED. migrations/0025_*.sql does not exist,
// internal/ticketstatus/deliveredstatus.go does not exist, nothing selects
// projects.ticket_delivered_statuses, Stats has no ClosedTicketDelivered,
// cmd/opsctl's report knows nothing about a delivered set, jira/facts.go still
// says the status name is diagnostic-only, and the runbook has no delivered-set
// section, so each guard below fails on its own sentence once the package
// builds. It does not build yet: the package's test binary is ONE unit and
// decide_delivered_test.go names fields that do not exist, so `go test
// ./internal/ticketstatus/` reports the compile errors first. This file
// deliberately names no unwritten Go surface directly — Stats is read by
// REFLECTION and the fold's file by go/parser — so it produces its sentences
// the moment the surface lands, without needing to be edited again.
//
// NO ADVISORY-LOCK LITERAL IS SPELLED HERE, for structure_test.go's recorded
// reason: internal/classify's repo-wide collision scan reads a restated key as a
// key taken twice.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/ticketstatus"
)

// The seeded name, assembled rather than written, so this file's own scan of
// migration 0025 (criterion 2 bans the literal there) cannot be confused about
// which file it came from. It is also the string criterion 30's widened
// statusNames ban refuses in every non-test source.
var qdSeededName = "TT-" + "In QA"

// ---- criteria 1-4, 31: migration 0025 ---------------------------------------

// The migration is 0025, not 0024: SWT-33 (inquiry-classify) holds 0024 and is
// in flight. The runner tracks applied versions individually (schema_migrations
// keys on a TEXT version, one row per file), so a NUMBER GAP is harmless and a
// number COLLISION is not — which is why this guard asserts "exactly one 0025"
// and deliberately does NOT assert "nothing above 0025 exists". The
// unowned-number check belongs to the living ledger in
// internal/classify/structure_test.go, asserted below.
func TestMigration0025_IsTheOnlyOneThisTicketAdds(t *testing.T) {
	// Control first: 0023 must still be there, or a glob returning nothing below
	// would prove only that the directory moved.
	if _, err := os.Stat(filepath.Join("..", "..", "migrations", "0023_ticket_status_sync.sql")); err != nil {
		t.Fatalf("migrations/0023_ticket_status_sync.sql is missing: %v — SWT-32 owns it and this "+
			"ticket EXTENDS its reconciler; without it there is no drop_reason CHECK to widen", err)
	}

	matches, err := filepath.Glob(filepath.Join("..", "..", "migrations", "0025_*.sql"))
	if err != nil {
		t.Fatalf("glob migrations/0025_*.sql: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("found %d migrations/0025_*.sql file(s), want exactly 1 (the SPEC names "+
			"0025_ticket_delivered_statuses.sql). This ticket's data-model section is ONE migration: "+
			"the projects column and the drop_reason CHECK swap. Merging a migration is not applying "+
			"it — check `SELECT max(version) FROM schema_migrations` before deploying", len(matches))
	}

	rel := filepath.Join("migrations", filepath.Base(matches[0]))
	raw := tsRepoFile(t, rel)
	sql := strings.ToLower(raw)

	// (1) criterion 1: the typed column, fail-closed default.
	addCol := regexp.MustCompile(
		`alter\s+table\s+projects\s+add\s+column\s+ticket_delivered_statuses\s+text\s*\[\s*\]\s+not\s+null\s+default\s+'\{\}'`)
	if !addCol.MatchString(sql) {
		t.Errorf("%s does not `ALTER TABLE projects ADD COLUMN ticket_delivered_statuses TEXT[] NOT "+
			"NULL DEFAULT '{}'`. E1: a typed column (not a policies jsonb key — 0016 and 0018 record "+
			"why), NOT NULL so 0018's nullable-column trap cannot happen (`AND p.ai_classify` "+
			"silently excluding every row nobody set), and '{}' because empty must be TODAY'S "+
			"BEHAVIOUR EXACTLY", rel)
	}

	// (2) criterion 2: no arming, no index, no name literal, no down section.
	if regexp.MustCompile(`update\s+projects\s+set[^;]*ticket_delivered_statuses`).MatchString(sql) {
		t.Errorf("%s ARMS a project with an UPDATE. Criterion 2: arming is an operator act recorded in "+
			"the runbook, exactly as classify_promote_after and ticket_assignee_gate are — a "+
			"migration that armed collaboratory would drop eight tasks off a live board as a DEPLOY "+
			"SIDE EFFECT, at whatever hour the rollout happened", rel)
	}
	if regexp.MustCompile(`create\s+index[^;]*ticket_delivered_statuses`).MatchString(sql) {
		t.Errorf("%s indexes ticket_delivered_statuses. `projects` holds tens of rows and every reader "+
			"reaches it by primary key; an index nothing uses is a permanent claim some query needs "+
			"it (0016's and 0018's recorded argument)", rel)
	}
	if strings.Contains(raw, qdSeededName) {
		t.Errorf("%s contains the status-name literal %q. The whole point of E1 is that the name lives "+
			"in the DATABASE, as data an operator wrote, not in anything that ships — and criterion "+
			"30 refuses the same string in the code. A migration that seeds it is code that ships it",
			rel, qdSeededName)
	}
	if strings.Contains(sql, "drop column") || strings.Contains(sql, "drop table") ||
		regexp.MustCompile(`(?s)--\s*down`).MatchString(sql) {
		t.Errorf("%s looks like it carries a down migration or a DROP. Migrations here are "+
			"FORWARD-ONLY, no exceptions", rel)
	}

	// (3) criterion 3: the CHECK swap, by 0009's precedent, naming the
	// constraint Postgres generated for 0023's inline CHECK — and re-using that
	// exact name on the way back in.
	const constraintName = "ticket_status_syncs_drop_reason_check"
	dropC := regexp.MustCompile(`alter\s+table\s+ticket_status_syncs\s+drop\s+constraint\s+(if\s+exists\s+)?` + constraintName)
	if !dropC.MatchString(sql) {
		t.Errorf("%s does not DROP CONSTRAINT %s on ticket_status_syncs. Criterion 3: the constraint "+
			"is named EXPLICITLY (Postgres's generated name for 0023's inline CHECK) and the ADD "+
			"re-uses it, so the next reader greps one name and finds both halves", rel, constraintName)
	}
	addC := regexp.MustCompile(`(?s)alter\s+table\s+ticket_status_syncs\s+add\s+constraint\s+` +
		constraintName + `\s+check\s*\(\s*drop_reason\s+in\s*\(([^)]*)\)`)
	m := addC.FindStringSubmatch(sql)
	if m == nil {
		t.Fatalf("%s does not ADD CONSTRAINT %s CHECK (drop_reason IN (...)). Without the ADD the "+
			"column is left UNCONSTRAINED — strictly worse than before, and invisible until a typo "+
			"becomes a stored value", rel, constraintName)
	}
	for _, v := range []string{"ticket_done", "ticket_delivered", "not_assigned"} {
		if !strings.Contains(m[1], "'"+v+"'") {
			t.Errorf("%s's widened drop_reason CHECK (%s) does not allow %q. All three, in one CHECK: "+
				"the vocabulary is the contract, and the ordered precedence list in the code is "+
				"meaningless if the database refuses one of its values", rel, strings.TrimSpace(m[1]), v)
		}
	}
	if !strings.Contains(sql, "transaction") {
		t.Errorf("%s never says that the drop/add pair is safe ONLY because the migrate runner "+
			"executes each file in one transaction (0009's recorded precedent for "+
			"deliveries_channel_check). A reader who copies this pattern into a runner without that "+
			"property leaves the table unconstrained between two statements", rel)
	}

	// (4) criterion 4: the swap is SELF-VERIFYING. A DROP CONSTRAINT against a
	// name Postgres did not generate is a silent no-op, and the first real
	// ticket_delivered insert then fails months later at runtime, every tick, on
	// the same ref.
	selfCheck := regexp.MustCompile(`(?s)do\s+\$\$.*pg_constraint.*drop_reason.*raise\s+exception.*\$\$`)
	if !selfCheck.MatchString(sql) {
		t.Errorf("%s has no `DO $$ ... $$` block that reads pg_constraint and RAISEs unless EXACTLY "+
			"one CHECK mentioning drop_reason survives on ticket_status_syncs. Criterion 4: without "+
			"it, a mis-named DROP is a no-op that passes migration, passes every structural scan of "+
			"the TEXT above, and fails in production the first time a QA ticket drops", rel)
	}
	if !regexp.MustCompile(`(?s)do\s+\$\$.*count\(\*\).*\$\$`).MatchString(sql) {
		t.Errorf("%s's self-check does not COUNT the surviving constraints. 'At least one exists' is "+
			"the assertion that passes when the old two-value CHECK is still there beside the new "+
			"one — which is the exact failure a mis-named DROP produces", rel)
	}

	// (5) criterion 31: 0023 is applied and must NOT be edited, so its stale
	// "nothing branches on it" comment is superseded HERE, by name.
	if !strings.Contains(sql, "status_name") {
		t.Errorf("%s never mentions status_name. Criterion 31: 0023 says the column is 'DIAGNOSTIC "+
			"only (D2) — nothing branches on it', that file is applied and unamendable, and from this "+
			"migration on a per-project CONFIGURED set of names may branch on it. A comment that "+
			"states the opposite of its code is a recorded defect class in this repo", rel)
	}
}

// Criterion 6: the living registry of owned migration numbers is the ledger in
// internal/classify/structure_test.go, and it must learn 25.
//
// This guard READS that file and never writes it — the ledger's own test is the
// enforcement, this is the reminder that fires from the ticket that creates the
// number. (Concurrency note for whoever reads this next: SWT-33 owns 0024 and
// teaches the same line about 24, so the merged expression excludes both.)
func TestMigrationLedger_LearnsTwentyFive(t *testing.T) {
	const rel = "internal/classify/structure_test.go"
	src := tsRepoFile(t, rel)

	ledger := regexp.MustCompile(`if\s+n\s*>\s*17(\s*&&\s*n\s*!=\s*\d+)+`).FindString(src)
	if ledger == "" {
		t.Fatalf("%s no longer holds the `if n > 17 && n != 18 ...` migration ledger; the registry "+
			"criterion 6 is about has moved and this guard is scanning nothing", rel)
	}
	if !regexp.MustCompile(`n\s*!=\s*23`).MatchString(ledger) {
		t.Fatalf("the ledger (%s) does not exempt 23 — the control for the assertion below. Something "+
			"other than a missing 25 is wrong here", ledger)
	}
	if !regexp.MustCompile(`n\s*!=\s*25`).MatchString(ledger) {
		t.Errorf("the migration ledger in %s is %q and does not exempt 25. Criterion 6: rewrite the "+
			"guard to the new truth, never delete it — the migrate runner keys on "+
			"schema_migrations.version with NO checksum, so a file nobody's ticket owns is skipped "+
			"SILENTLY and the schema diverges with no error anywhere", rel, ledger)
	}
}

// ---- criterion 9: the fold's file ------------------------------------------

// "internal/ticketstatus/deliveredstatus.go declares two pure functions and no
// others: NormalizeStatusName and IsDeliveredStatus. Zero I/O; the file imports
// only strings."
//
// The FILE is the boundary, exactly as decide.go's purity guard makes the file
// the boundary: "pure except for one lookup" is how the 36-row decision table
// stops proving anything.
func TestDeliveredStatusGo_IsTwoPureFunctionsImportingOnlyStrings(t *testing.T) {
	const rel = "internal/ticketstatus/deliveredstatus.go"
	if _, err := os.Stat(filepath.Join("..", "..", rel)); err != nil {
		t.Fatalf("%s does not exist: %v — criterion 9 puts the fold in its OWN file precisely so its "+
			"import block is the proof of purity", rel, err)
	}

	imports := tsFileImports(t, rel)
	if len(imports) != 1 || imports[0] != "strings" {
		t.Errorf("%s imports %v, want exactly [strings]. E3: the fold is lowercase + "+
			"strings.Fields whitespace collapse and nothing else — no regexp (a regexp is a second "+
			"spelling waiting to disagree), no unicode tables, and absolutely nothing that could "+
			"reach a database", rel, imports)
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join("..", "..", rel), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	var funcs []string
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok {
			funcs = append(funcs, fn.Name.Name)
		}
	}
	want := map[string]bool{"NormalizeStatusName": true, "IsDeliveredStatus": true}
	for _, name := range funcs {
		if !want[name] {
			t.Errorf("%s also declares %s. Criterion 9 says TWO functions and no others: the fold has "+
				"one spelling, and a helper here is where the second one starts", rel, name)
		}
		delete(want, name)
	}
	for name := range want {
		t.Errorf("%s does not declare %s — the SPEC fixes both names, and the report re-uses "+
			"IsDeliveredStatus rather than making a second comparison (criterion 27)", rel, name)
	}
}

// ---- criterion 12: the fold is NEVER re-spelled in SQL ----------------------

// E3, mechanically. `internal/textmatch`'s recorded rule applied to a second
// kind of label: "Do NOT re-spell it in SQL — Postgres's POSIX \s does not cover
// the unicode spaces Go's strings.Fields does, so an NBSP alone makes the two
// disagree, silently."
//
// The shape is upworkcrm/keyspelling_test.go's: flag a string literal that
// mentions the column AND does something set-ish or fold-ish with it. The column
// may only ever appear in a SELECT list — the value crosses into Go and the
// comparison happens in the pure function, which is also what keeps the
// predicate inside the audited path rather than hidden in a WHERE clause
// (invariant 3's note in the SPEC).
var qdSQLSurgery = regexp.MustCompile(`(?i)=\s*ANY|@>|<@|&&|unnest\(|lower\(|upper\(|btrim\(|trim\(`)

// Scanned as whole units so a multi-line query whose column and whose `= ANY`
// sit on different lines is still seen.
var qdRawStringLit = regexp.MustCompile("(?s)`[^`]*`")

func TestDeliveredSet_IsNeverComparedInSQL(t *testing.T) {
	const column = "ticket_delivered_statuses"

	// Positive control: a scanner that matches nothing passes everything.
	for _, probe := range []string{
		"WHERE lower(btrim(s.status_name)) = ANY(p." + column + ")",
		"SELECT 1 FROM projects p WHERE p." + column + " && ARRAY[$1]",
		"SELECT unnest(p." + column + ") FROM projects p",
	} {
		if !(strings.Contains(probe, column) && qdSQLSurgery.MatchString(probe)) {
			t.Fatalf("the scanner does not flag its own probe %q; the patterns have stopped matching "+
				"and every file below would pass for the wrong reason", probe)
		}
	}

	selected := false
	scanned := 0
	for _, root := range []string{"internal", "cmd"} {
		err := filepath.Walk(filepath.Join("..", "..", root), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			rel := strings.TrimPrefix(filepath.ToSlash(path), "../../")
			if rel == "internal/ticketstatus/delivered_structure_test.go" {
				return nil // this file necessarily contains the patterns it bans
			}
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			scanned++
			src := string(b)
			if !strings.Contains(src, column) {
				return nil
			}
			if !strings.HasSuffix(rel, "_test.go") {
				selected = true
			}
			report := func(where, snippet string) {
				t.Errorf("%s %s spells the delivered-set comparison in SQL:\n\t%s\nE3: the column is "+
					"SELECTed as-is and membership is decided in Go by ticketstatus.IsDeliveredStatus. "+
					"Postgres's POSIX \\s does not cover the unicode spaces strings.Fields splits on, "+
					"so an NBSP alone makes the two spellings disagree with no error anywhere — and a "+
					"predicate in the WHERE clause also moves the decision out of the pure Decide "+
					"where invariant 7 wants it", rel, where, snippet)
			}
			for _, lit := range qdRawStringLit.FindAllString(src, -1) {
				if strings.Contains(lit, column) && qdSQLSurgery.MatchString(lit) {
					report("(raw string literal)", qdFirstLines(lit, 4))
				}
			}
			for i, line := range strings.Split(src, "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
					continue // prose may explain why the SQL spelling is refused
				}
				if strings.Contains(line, "`") {
					continue // covered by the raw-literal pass
				}
				if strings.Contains(line, column) && qdSQLSurgery.MatchString(line) {
					report("line "+qdItoa(i+1), trimmed)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s/: %v", root, err)
		}
	}
	if scanned == 0 {
		t.Fatalf("the scan visited no .go file; it is not seeing the files it claims to check")
	}
	if !selected {
		t.Errorf("no non-test .go file under internal/ or cmd/ mentions %s at all. The ban above is "+
			"scanning a repo that never reads the column — criterion 12 asserts it IS selected "+
			"somewhere, because a guard over an unused column certifies nothing (SWT-21's 6th "+
			"landmine: a guard whose column no query selected)", column)
	}
}

// Criterion 22: "loadCandidates selects p.ticket_delivered_statuses and the
// driver copies it into Observation.DeliveredStatuses. No other query in the
// repo reads the column" — plus criterion 27's report, which is the one
// deliberate second reader and is listed here by name.
func TestDeliveredSet_IsReadByTheDriverAndTheReportOnly(t *testing.T) {
	const column = "ticket_delivered_statuses"
	allowed := map[string]string{
		"internal/ticketstatus/store.go": "the candidate query (criterion 22): the value crosses into " +
			"Observation.DeliveredStatuses and the decision stays pure",
		"cmd/opsctl/main.go": "the report's armed-set column (E12): an armed set that matches nothing " +
			"looks exactly like an unarmed one",
	}

	found := map[string]bool{}
	for _, root := range []string{"internal", "cmd"} {
		err := filepath.Walk(filepath.Join("..", "..", root), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return err
			}
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if !strings.Contains(string(b), column) {
				return nil
			}
			rel := strings.TrimPrefix(filepath.ToSlash(path), "../../")
			if _, ok := allowed[rel]; !ok {
				t.Errorf("%s reads %s. Criterion 22: exactly two readers exist — the driver's "+
					"candidate query and opsctl's report — and a third is how the funnel grows a "+
					"second, divergent answer to 'is this project armed'", rel, column)
			}
			found[rel] = true
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s/: %v", root, err)
		}
	}
	for rel, why := range allowed {
		if !found[rel] {
			t.Errorf("%s does not read %s — %s", rel, column, why)
		}
	}
}

// ---- criterion 25: the counter vocabulary ----------------------------------

// "Stats gains ClosedTicketDelivered, printed as closed_ticket_delivered in BOTH
// counter lines, unconditionally, zeros included ... and it keeps failing if a
// Stats field exists that no counter line prints."
//
// Derived by REFLECTION from the struct rather than from a list this test
// supplies: the fixture-shaped-like-the-assertion landmine says a hand-written
// list stops covering the field the day someone adds one, which is precisely the
// day it matters. A counter that exists and is never printed is a number nobody
// can read; a smoke that checks "closed_ticket_delivered equals the TT-In QA
// count" needs the line to carry it.
func TestTicketStatus_EveryStatsFieldIsPrintedInBothCounterLines(t *testing.T) {
	lines := map[string]string{
		"cmd/opsctl/main.go":          tsRepoFile(t, "cmd/opsctl/main.go"),
		"cmd/connectors/jira/main.go": tsRepoFile(t, "cmd/connectors/jira/main.go"),
	}
	for rel, src := range lines {
		if !strings.Contains(src, "ticket_status:") {
			t.Fatalf("%s no longer prints a `ticket_status:` counter line; criterion 25 is about two "+
				"lines and this scan has lost one of them", rel)
		}
	}

	typ := reflect.TypeOf(ticketstatus.Stats{})
	if typ.NumField() == 0 {
		t.Fatalf("ticketstatus.Stats has no fields; the reflection below is scanning nothing")
	}
	sawDelivered := false
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i).Name
		printed := qdSnake(field)
		if field == "ClosedTicketDelivered" {
			sawDelivered = true
		}
		for rel, src := range lines {
			if !strings.Contains(src, printed) {
				t.Errorf("%s never prints %q, the counter for Stats.%s. Criterion 25: BOTH lines, "+
					"unconditionally, zeros included — a counter that exists and is never printed "+
					"cannot be read by the operator who has to decide whether the pass did what the "+
					"dry run said it would", rel, printed, field)
			}
		}
	}
	if !sawDelivered {
		t.Errorf("ticketstatus.Stats has no ClosedTicketDelivered field. Criterion 25/26: the new drop " +
			"cause needs its OWN counter — routing it into closed_ticket_done would make the smoke's " +
			"central check (closed_ticket_delivered equals the TT-In QA open-task count, no more and " +
			"no fewer) impossible to run")
	}
}

// ---- criterion 27: the report shows the armed set AND membership -------------

// E12: an armed set that matches nothing looks exactly like an unarmed one, and
// E2's whole reason for normalizing is that a mis-typed entry is the likely
// failure. The report turns a silent typo into a visible one — and computes it
// with the SAME IsDeliveredStatus the decision uses, never a second comparison
// (which would be the fold's second spelling, one package over from the SQL one
// criterion 12 already refuses).
func TestOpsctlReport_ShowsTheArmedSetAndWhetherThisRowIsInIt(t *testing.T) {
	const rel = "cmd/opsctl/main.go"
	src := tsRepoFile(t, rel)

	start := strings.Index(src, "func runTicketStatusReport(")
	if start < 0 {
		t.Fatalf("%s no longer declares runTicketStatusReport; criterion 27's subject has moved", rel)
	}
	body := src[start:]
	if end := strings.Index(body[1:], "\nfunc "); end > 0 {
		body = body[:end+1]
	}

	if !strings.Contains(body, "ticket_delivered_statuses") {
		t.Errorf("runTicketStatusReport does not select ticket_delivered_statuses. E12: the operator " +
			"who armed a project by hand has no other way to see WHAT he armed it with — and a " +
			"trailing space or an NBSP in that array is the realistic failure this column exists to " +
			"make visible")
	}
	if !strings.Contains(body, "ticketstatus.IsDeliveredStatus(") {
		t.Errorf("runTicketStatusReport does not call ticketstatus.IsDeliveredStatus. Criterion 27: " +
			"delivered=yes|no is computed with the SAME function the decision uses. A second " +
			"comparison here would be a report that says 'yes' while the pass says 'no' — the worst " +
			"possible diagnostic, because it is confidently wrong")
	}
	if !strings.Contains(body, "delivered=") {
		t.Errorf("runTicketStatusReport prints no `delivered=yes|no` column. The set alone is not " +
			"enough: membership is decided AFTER a fold, so 'the set contains TT-In QA' and 'this " +
			"row's status is in the set' are different facts and the operator needs both")
	}
}

// ---- criterion 31: the comment that would otherwise state the opposite ------

// `jira.Facts.StatusName`'s comment says "DIAGNOSTIC ONLY; nothing branches on
// it". After this ticket something does — through configuration. In a boundary
// file the comment is what the next session trusts, and "a comment can be a
// defect" is a recorded defect class in this repo (SWT-21's third finding).
func TestJiraFacts_StatusNameCommentNoLongerSaysNothingBranchesOnIt(t *testing.T) {
	const rel = "internal/connector/jira/facts.go"
	src := tsRepoFile(t, rel)

	lines := strings.Split(src, "\n")
	idx := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "StatusName ") {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("%s no longer declares a StatusName field; this ticket's whole reading half is "+
			"'stop ignoring a value already in hand', and that value is this one", rel)
	}
	from := idx - 4
	if from < 0 {
		from = 0
	}
	around := strings.ToLower(strings.Join(lines[from:idx+1], "\n"))

	if strings.Contains(around, "nothing branches on it") {
		t.Errorf("%s still documents StatusName as \"nothing branches on it\". Criterion 31: nothing "+
			"in CODE branches on it — still true and worth keeping — but from this ticket on a "+
			"per-project CONFIGURED set does. A comment that states the opposite of its code is how "+
			"the next reader concludes the field is dead and deletes the parse", rel)
	}
	if !strings.Contains(around, "configur") {
		t.Errorf("%s's StatusName comment does not mention that a per-project CONFIGURED set may "+
			"branch on it. The correction is not just deleting the false half: the reader needs to "+
			"know WHERE the branch lives, or the next SPEC re-derives the D2 tension from scratch", rel)
	}
}

// ---- criterion 32: the runbook, including E10's deferred gap ----------------

// A test on prose earns its place the way TestRunbook_DocumentsTheReconciler
// does: nothing in the code can stop an operator arming the wrong project's set,
// and nothing in the code can tell him that a client question on a dropped QA
// ticket is recorded and surfaced NOWHERE. He must read that sentence when he
// arms the column, not discover it (E10).
func TestRunbook_DocumentsTheDeliveredSetAndItsDeferredGap(t *testing.T) {
	const rel = "docs/runbooks/ticket-status-sync.md"
	raw := tsRepoFile(t, rel)
	doc := strings.ToLower(raw)

	for _, want := range []struct{ frag, why string }{
		{"ticket_delivered_statuses", "the column an operator arms — the whole feature is one UPDATE to it"},
		{"ticket_delivered", "the drop_reason, which is what `opsctl ticket-status report` and the counters say"},
		{"not my turn", "the distinction from statusCategory: TT-In QA and TT-Work In Progress are BOTH " +
			"indeterminate, so no function of the category can separate 'delivered' from 'mid-build'"},
		{"statuscategory", "the fact this clause does NOT overturn (D2 stands): finished is still Jira's own structure"},
		{"qa-question-resurface", "the deferred half, by its ticket name, so the gap has an owner"},
	} {
		if !strings.Contains(doc, want.frag) {
			t.Errorf("%s never mentions %q — criterion 32 asks for %s", rel, want.frag, want.why)
		}
	}

	// The arming UPDATE, exactly, and its exact inverse. Whitespace-collapsed
	// before matching so the runbook may format it as it likes; the ARRAY and
	// the slug are what has to be there, because a half-remembered arming
	// command is how a different project gets emptied.
	flat := strings.Join(strings.Fields(doc), " ")
	arm := regexp.MustCompile(`update projects set ticket_delivered_statuses = array\['tt-in qa'\] where slug = 'collaboratory'`)
	if !arm.MatchString(flat) {
		t.Errorf("%s does not carry the exact arming UPDATE (`UPDATE projects SET "+
			"ticket_delivered_statuses = ARRAY['TT-In QA'] WHERE slug = 'collaboratory';`). E11: "+
			"TT-In Review is deliberately NOT in it — it has two readings and guessing is how a task "+
			"silently disappears while the ball is in his court", rel)
	}
	revert := regexp.MustCompile(`ticket_delivered_statuses = '\{\}'`)
	if !revert.MatchString(flat) {
		t.Errorf("%s does not carry the reversal (`SET ticket_delivered_statuses = '{}'`). Reverting "+
			"is the same one UPDATE back, and the next pass restores the tasks — an operator who "+
			"cannot find that sentence will not arm the column in the first place", rel)
	}
	if !strings.Contains(flat, "'{}'") || !(strings.Contains(doc, "today") || strings.Contains(doc, "unchanged")) {
		t.Errorf("%s does not say that an EMPTY array is today's behaviour exactly. That is the "+
			"sentence that tells the operator the other six projects cannot move, and it is the "+
			"reason this migration is safe to deploy before anyone decides anything", rel)
	}

	// E10's stand-in query: while a task is dropped as delivered, a client
	// question on that ticket IS recorded — capture's appendRuleLog appends a
	// `log` task_event to the CLOSED task, with no status filter — and is
	// surfaced nowhere. Until qa-question-resurface ships, this query is the
	// only way to see it.
	var gapQuery bool
	for _, block := range regexp.MustCompile("(?s)```.*?```").FindAllString(doc, -1) {
		if strings.Contains(block, "ticket_delivered") && strings.Contains(block, "task_events") {
			gapQuery = true
		}
	}
	if !gapQuery {
		t.Errorf("%s has no stand-in query listing `log` task_events that landed on "+
			"ticket_delivered-dropped tasks since their close. E10: the honest gap goes in the "+
			"RUNBOOK, not in a future reader's debugging session — a question arriving on a dropped "+
			"QA ticket is recorded on the closed task by capture and surfaced NOWHERE, and this "+
			"query is the manual substitute until the follow-up ships", rel)
	}
	if !strings.Contains(doc, "task_events") {
		t.Errorf("%s never mentions task_events. The gap sentence has to name WHERE the question "+
			"lands (a `log` task_event on the closed task), or it reads as 'the question is lost', "+
			"which is a different and worse claim", rel)
	}
}

// ---- small helpers -----------------------------------------------------------

// qdSnake turns a Go field name into the counter name the two mains print,
// treating a run of capitals as one word: FetchSkippedTTL -> fetch_skipped_ttl.
func qdSnake(field string) string {
	var out []rune
	runes := []rune(field)
	for i, r := range runes {
		upper := r >= 'A' && r <= 'Z'
		if upper && i > 0 {
			prevLower := runes[i-1] >= 'a' && runes[i-1] <= 'z'
			nextLower := i+1 < len(runes) && runes[i+1] >= 'a' && runes[i+1] <= 'z'
			if prevLower || nextLower {
				out = append(out, '_')
			}
		}
		if upper {
			r = r - 'A' + 'a'
		}
		out = append(out, r)
	}
	return string(out)
}

func qdFirstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n\t")
}

func qdItoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
