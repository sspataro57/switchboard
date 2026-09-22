package capture

// comms-inbox (SWT-74, docs/tickets/comms-inbox_SPEC.md), the STRUCTURAL half
// of the capture side. ZERO I/O beyond reading this repo's own files.
//
//	criterion 1  — migration 0040's shape, and both living ledgers learning 40;
//	criterion 2  — internal/capture/comm.go is PURE (resurface_structure_test.go's shape);
//	criterion 5  — commInput carries no dismissed / connectorCopy / prNotice / prClose
//	               field, each documented in comm.go with D2's inert-predicate argument;
//	criterion 6  — loadRules selects r.comm_task, and `comm_task` is named by NO
//	               other non-test Go file (the notifier_senders criterion 10 shape);
//	criterion 7  — decideMessage's found branch sets d.comm from commTask(…) with
//	               armed = winner.commTask and words the reason by MODE;
//	criterion 8  — EvaluateRules' actionTaskLog branch, in D3's order, and the
//	               TARGET's mark skipped when a comm task is created;
//	criterion 9  — RulesStats.CommTasks and the five counter lines;
//	criterion 12 — commTaskArgs is pure, and the comm path calls neither
//	               link_external_ref nor task_mark_surfaced;
//	criterion 15 — the pointer log's exact text, ids only;
//	criterion 19 — the dry run names the comm proposal and prints the flag.
//
// ---- IMPOSED SURFACE (SPEC D1, D3, D4 and criteria 1, 6-9, 12, 15, 19) --------
//
//	-- migrations/0040_capture_comm_tasks.sql (new; its header states the
//	-- Verification Step 5 rollout BARRIER, the 0034/0035/0039 precedent)
//	ALTER TABLE capture_rules
//	  ADD COLUMN comm_task BOOLEAN NOT NULL DEFAULT false,
//	  ADD CONSTRAINT capture_rules_comm_task_shape
//	    CHECK (NOT comm_task OR (external_system IS NOT NULL AND NOT pr_review));
//	ALTER TABLE capture_decisions
//	  ADD COLUMN comm_task_id BIGINT REFERENCES tasks(id),
//	  ADD CONSTRAINT capture_decisions_comm_task_is_task_log
//	    CHECK (comm_task_id IS NULL OR action = 'task_log');
//	-- no index, no backfill, no other table, NO rule armed by the migration.
//
//	// internal/capture/rules_store.go
//	type storedRule  struct { …; commTask bool }   // loadRules selects r.comm_task
//	type ruleDecision struct { …; comm bool }      // carried, never a column
//	type RulesStats  struct { …; CommTasks int }
//	func createCommTask(ctx, ex, actor, pm, winner, system, key string/int64…, relatedTaskID int64) (int64, error)
//	func commTaskArgs(pm pendingMessage, winner storedRule, system, key string, relatedTaskID int64) map[string]any
//	func recordDecisionCommTask(ctx, pool, decisionID, commTaskID int64) error
//	func appendCommPointer(ctx, ex, actor, pm, targetID, commID …) error
//	// the pointer's TEXT, ids only, no title/sender/preview/body:
//	//   capture: comm #<new id> created from this message (message <M>)
//
//	// cmd/connectors/{google,jira,slackweb,upworkcrm}/main.go and cmd/opsctl/main.go
//	//   gain "comm_tasks":%d in the SAME capture_rules: format.
//
// GREENFIELD NOTE — EXPECTED RED: the migration, comm.go, the columns, the
// helpers, the counter and the dry-run flag do not exist, and this package's
// unit binary does not compile until comm.go does (comm_test.go). Each guard
// below then fails on its own sentence.
//
// SPEC AMENDMENT (2026-09-22 12:20, swb #491, on main — the SPEC predates it):
// capture's actionTask branch ALSO calls task_mark_activity on a task it
// CREATES (after provenance, before task_mark_surfaced; excluded for a
// pr_review rule's create). So D8's "a capture-created ticket task still lands
// in QUEUE" is STALE — it lands in INCOMING. D8's RULE stands: the comm path
// lives strictly in the found / task_log branch, and criterion 8's scans below
// therefore look INSIDE `case actionTaskLog:` and never at the function as a
// whole (the SWT-72 structure test was amended the same way).
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - loadRules selects a literal false instead of r.comm_task -> LoadRulesSelectsTheColumn.
//   - keep markRuleActivity on the TARGET when a comm is created -> TheTargetIsNotMarked.
//   - drop setRuleProvenance / markRuleActivity on the COMM -> TheCommBranchIsInOrder.
//   - call link_external_ref for the comm task -> TheCommPathLinksNoRef.
//   - the pointer log carries the subject, sender or preview -> ThePointerIsIdsOnly.
//   - allow comm_task with pr_review (drop the CHECK) -> Migration0040Shape.

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

const commMigration = "0040_capture_comm_tasks.sql"

// ---- criterion 1: the migration ----------------------------------------------------

func TestMigration0040_CaptureCommTasksShape(t *testing.T) {
	dir := filepath.Join("..", "..", "migrations")
	if _, err := os.Stat(filepath.Join(dir, "0039_task_activity_review.sql")); err != nil {
		t.Fatalf("control: migrations/0039_task_activity_review.sql is missing (%v); 0040 follows it", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "0040_*.sql"))
	if err != nil {
		t.Fatalf("glob migrations/0040_*.sql: %v", err)
	}
	if len(matches) != 1 || filepath.Base(matches[0]) != commMigration {
		var names []string
		for _, m := range matches {
			names = append(names, filepath.Base(m))
		}
		t.Fatalf("migrations/0040_*.sql = %v, want exactly [%s]. Criterion 1: this ticket's ONLY migration "+
			"(SWT-75 already owns 0041)", names, commMigration)
	}

	raw := mustReadRepoFile(t, "migrations/"+commMigration)
	var code []string
	for _, line := range strings.Split(raw, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		code = append(code, line)
	}
	sql := strings.ToLower(regexp.MustCompile(`\s+`).ReplaceAllString(strings.Join(code, " "), " "))
	sql = strings.ReplaceAll(strings.ReplaceAll(sql, "( ", "("), " )", ")")
	sql = regexp.MustCompile(`\s*,\s*`).ReplaceAllString(sql, ",")

	for _, want := range []struct{ re, why string }{
		{`alter table (public\.)?capture_rules\b`, "the arming flag is a per-rule COLUMN (D1), not a table"},
		{`add column (if not exists )?comm_task boolean not null default false`,
			"D1: defaults FALSE, so a db with 0040 applied and nothing armed behaves byte-identically to 0.7.39"},
		{`check\(not comm_task or\(external_system is not null and not pr_review\)\)`,
			"D1's schema-level refusals: a keyless rule can never reach the task_log branch (an armed keyless " +
				"rule would be an INERT flag), and GitHub notification mail is a notice stream with its own " +
				"review tasks (SWT-54)"},
		{`alter table (public\.)?capture_decisions\b`, "the pointer to what was created lives on capture's own log"},
		{`add column (if not exists )?comm_task_id bigint references (public\.)?tasks\(id\)`,
			"criterion 1: the created comm task, recorded on the decision BEFORE the follow-ups (D3 step 3)"},
		{`check\(comm_task_id is null or action = 'task_log'\)`,
			"criterion 1: a comm decision IS a task_log (no new action value — that would break every " +
				"latest-decision reader, including replyfold.InquiryEligibleLatestSQL)"},
	} {
		if !regexp.MustCompile(want.re).MatchString(sql) {
			t.Errorf("%s does not match /%s/ — %s\n\ngot: %s", commMigration, want.re, want.why, sql)
		}
	}
	for _, banned := range []struct{ re, why string }{
		{`create (unique )?index`, "criterion 1: no index"},
		{`\bupdate\b`, "criterion 1: no backfill and NO RULE ARMED BY THE MIGRATION (0035's sentence) — arming " +
			"is capture_rule_add or one UPDATE by hand, recorded in the delivery summary"},
		{`insert into`, "criterion 1: no seeding"},
		{`delete from`, "nothing deleted"},
		{`create table`, "no new table: a comm is a row in the one tasks table (invariant 2, his own framing)"},
		{`\bdrop\b`, "forward-only"},
		{`\btasks\b\s+add column|alter table (public\.)?tasks\b`, "criterion 1: no other table is touched"},
		{`normalized_|external_refs|task_dismissals|classify_promotions`, "criterion 1: no other table is touched"},
	} {
		if regexp.MustCompile(banned.re).MatchString(sql) {
			t.Errorf("%s matches /%s/ — %s", commMigration, banned.re, banned.why)
		}
	}
	if !strings.Contains(sql, "if not exists") && !strings.Contains(sql, "do $$") {
		t.Errorf("%s has neither IF NOT EXISTS nor a DO $$ guard; applying it twice must be safe:\n%s",
			commMigration, sql)
	}
	if !strings.Contains(strings.ToLower(raw), "barrier") {
		t.Errorf("%s's header does not state the rollout BARRIER. Criterion 1 / Verification Step 5: every "+
			"capture pass built from this branch selects capture_rules.comm_task, so a new image on a pre-0040 "+
			"db fails capture for EVERY connector (the 0034/0035 precedent)", commMigration)
	}
}

// Both living ledgers must OWN 40, or `ls migrations/` grows a file no SPEC
// accounts for — and the migrate runner keys on schema_migrations.version with
// NO checksum, so a stray file is skipped silently.
func TestMigrationLedger_Learns0040(t *testing.T) {
	src := mustReadRepoFile(t, "internal/classify/structure_test.go")
	i := strings.Index(src, "THIS LEDGER IS THE LIVING REGISTRY")
	if i < 0 {
		t.Fatalf("the migration ledger marker is gone; REWRITE the guard, never delete it")
	}
	above := src[max(i-6000, 0):i]
	if !strings.Contains(above, "40 is SWT-74") {
		t.Errorf("the ledger has no ownership note \"40 is SWT-74 …\" ABOVE the marker (criterion 1). Today it " +
			"says 0040 is \"on an adjacent branch and not written yet\"; this ticket is that branch")
	}
	pred := regexp.MustCompile(`if\s+n\s*>\s*17(\s*&&\s*n\s*!=\s*\d+)+`).FindString(src[i:min(i+2500, len(src))])
	if pred == "" {
		t.Fatalf("the ledger's `if n > 17 && n != …` predicate is gone from below the marker")
	}
	if !regexp.MustCompile(`n\s*!=\s*40\b`).MatchString(pred) {
		t.Errorf("the ledger predicate %q does not accept 40; migrations/%s would be flagged unowned", pred, commMigration)
	}

	// The second ledger: internal/tools/signal_structure_test.go exempts by
	// NUMBER everything above 33 that another ticket owns.
	sig := mustReadRepoFile(t, "internal/tools/signal_structure_test.go")
	if !regexp.MustCompile(`v\s*!=\s*40\b`).MatchString(sig) {
		t.Errorf("TestMigration0033_TaskWorkingStateShape's exemption list does not include 40; it flags every " +
			"migration above 33 that no ticket claims there (it currently says 0040 is \"deliberately not " +
			"exempted here until that file exists\")")
	}
	if !strings.Contains(sig, "SWT-74") {
		t.Errorf("internal/tools/signal_structure_test.go's amendment comment does not name SWT-74 as 40's owner")
	}
}

// ---- criterion 2: comm.go is pure ---------------------------------------------------

func TestCommGo_IsPure(t *testing.T) {
	src := mustReadRepoFile(t, "internal/capture/comm.go")
	for _, fn := range []string{"func commTask(", "type commInput struct"} {
		if !strings.Contains(src, fn) {
			t.Errorf("internal/capture/comm.go does not declare %s — criterion 2 pins the split to this file, "+
				"because the FILE is what makes the purity checkable (resurface.go's precedent)", fn)
		}
	}
	for _, b := range []struct{ token, why string }{
		{`"context"`, "a pure decision takes no context"},
		{"pgx", "no database: the status, the flag and the sender facts arrive as VALUES (invariant 7's habit)"},
		{"pool", "no pool"},
		{"internal/connector/", "no connector import: D2 has no connectorCopy clause at all, and a channel " +
			"literal here would make the comm path channel-keyed (criterion 45)"},
		{"internal/provider", "no model: D1 refused a classifier lane outright (the local lane's measured " +
			"precision is 0.50, and \"half the board is noise\" is the failure this ticket prevents)"},
		{"net/http", "no network"},
		{"os.Getenv", "the arming flag is a COLUMN (capture_rules.comm_task), never the environment"},
		{"time.Now", "no clock: the same inputs must always give the same answer"},
		{"regexp", "equality and status comparison only"},
	} {
		if strings.Contains(src, b.token) {
			t.Errorf("internal/capture/comm.go mentions %q — %s", b.token, b.why)
		}
	}
}

// ---- criterion 5: the three clauses that are deliberately ABSENT ---------------------

// D2 names three clauses resurfaces() has that commTask must NOT have, each
// because it would be INERT — the IK's constant-discriminator landmine, which
// has now bitten this repo in five costumes. A predicate that is false for
// every row production produces goes green on a hand-written fixture and
// changes nothing.
func TestCommInput_HasNoInertFields(t *testing.T) {
	typ := reflect.TypeOf(commInput{})
	want := map[string]reflect.Kind{
		"armed": reflect.Bool, "status": reflect.String, "blankSender": reflect.Bool,
		"notifier": reflect.Bool, "ownJiraEdit": reflect.Bool,
	}
	got := map[string]reflect.Kind{}
	for i := 0; i < typ.NumField(); i++ {
		got[typ.Field(i).Name] = typ.Field(i).Type.Kind()
	}
	for name, kind := range want {
		if got[name] != kind {
			t.Errorf("commInput has no %s %s field (D2's five inputs: armed, status, blankSender, notifier, "+
				"ownJiraEdit). Got %v", name, kind, got)
		}
	}
	for _, banned := range []struct{ field, why string }{
		{"dismissed", "D2: taskForExternalRef joins task_dismissals under `AND t.status='closed'`, so an OPEN " +
			"task never carries one — and clause 2 already excluded every closed task. An INERT field"},
		{"connectorCopy", "D2: J3's structural dedup is enforced by NOT ARMING the connector rules, so a Go " +
			"clause would be false on every armed rule that exists"},
		{"prNotice", "D2: impossible under the 0040 CHECK (NOT pr_review), which is a DATA-level guarantee"},
		{"prClose", "D2: same"},
		{"channel", "criterion 45: the comm path is channel-blind"},
	} {
		if _, ok := got[banned.field]; ok {
			t.Errorf("commInput carries a %s field — %s", banned.field, banned.why)
		}
	}
	// Each absence is DOCUMENTED in the file with D2's argument, so the next
	// session adding one has to read why it was left out.
	src := mustReadRepoFile(t, "internal/capture/comm.go")
	for _, word := range []string{"dismiss", "connector", "pr_review"} {
		if !strings.Contains(strings.ToLower(src), word) {
			t.Errorf("comm.go never mentions %q. Criterion 5: each absent clause is documented in the file with "+
				"D2's inert-predicate argument — the comment IS the guard against re-adding it", word)
		}
	}
}

// ---- criterion 6: the column is selected, and has ONE reader -------------------------

func TestCaptureRules_LoadRulesSelectsTheCommColumn(t *testing.T) {
	src := mustReadRepoFile(t, "internal/capture/rules_store.go")
	load := rvFuncSrc(src, "loadRules")
	if load == "" {
		t.Fatalf("rules_store.go no longer declares loadRules")
	}
	if !strings.Contains(load, "r.comm_task") {
		t.Errorf("loadRules does not select r.comm_task. Criterion 6 / IK \"test the column, not the fixture\": " +
			"the flag is read from the COLUMN with the rules, like revive, pr_review and " +
			"ticket_assignee_gate. Selecting a literal false instead is the SPEC's own mutation row and must " +
			"turn criteria 42/43 red")
	}
	if !strings.Contains(load, "commTask") {
		t.Errorf("loadRules scans nothing into storedRule.commTask (criterion 6)")
	}
	// The storedRule field exists and is a bool.
	if f, ok := reflect.TypeOf(storedRule{}).FieldByName("commTask"); !ok {
		t.Errorf("storedRule has no commTask field (criterion 6: the per-rule flags are all COLUMNS loaded " +
			"with the rules)")
	} else if f.Type.Kind() != reflect.Bool {
		t.Errorf("storedRule.commTask is %s, want bool", f.Type)
	}
	// And ruleDecision carries the verdict, never a column (criterion 7).
	if f, ok := reflect.TypeOf(ruleDecision{}).FieldByName("comm"); !ok {
		t.Errorf("ruleDecision has no comm field. Criterion 7: d.comm is CARRIED on the decision, never " +
			"written as a capture_decisions column — the typed outcome is comm_task_id (the SWT-36 D7 / " +
			"SWT-45 precedent)")
	} else if f.Type.Kind() != reflect.Bool {
		t.Errorf("ruleDecision.comm is %s, want bool", f.Type)
	}
}

// The notifier_senders criterion-10 shape: capture_rules.comm_task is named by
// NO non-test Go file other than rules_store.go. A second reader is a second,
// divergent answer to "is this rule armed for comms".
func TestCommTaskColumn_IsReadByCaptureRulesStoreOnly(t *testing.T) {
	const column = "comm_task"
	const allowed = "internal/capture/rules_store.go"
	// capturerules.go legitimately WRITES the column (capture_rule_add's INSERT,
	// criterion 18), so it is the one other file that may name it.
	const writer = "internal/tools/capturerules.go"
	found := false
	for _, root := range []string{"internal", "cmd"} {
		err := filepath.Walk(filepath.Join("..", "..", root), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			// comm_task_id is capture_decisions' column, a different name; and
			// "comm_tasks" is criterion 9's COUNTER key, printed by every main —
			// a count, not a read of the column (amended 2026-09-22 on
			// implementation: the two spellings share a prefix).
			src := regexp.MustCompile(`comm_task_id|comm_tasks`).ReplaceAllString(string(b), "")
			if !strings.Contains(src, column) {
				return nil
			}
			rel := strings.TrimPrefix(filepath.ToSlash(path), "../../")
			switch rel {
			case allowed:
				found = true
			case writer, "cmd/opsctl/main.go":
				// the INSERT that arms a rule (criterion 18), and opsctl's
				// --comm-task flag passed to it BY NAME as a tool argument (the
				// --pr-review precedent; amended 2026-09-22 on implementation)
			default:
				t.Errorf("%s names %s. Criterion 6: the column is READ by %s alone (loadRules) and WRITTEN by "+
					"%s alone (capture_rule_add); a third reader is a second answer to \"is this rule armed\" "+
					"(the notifier_senders criterion 10 precedent)", rel, column, allowed, writer)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s/: %v", root, err)
		}
	}
	if !found {
		t.Errorf("%s does not read %s — criterion 6: loadRules selects it into storedRule.commTask", allowed, column)
	}
}

// ---- criterion 7: decideMessage words the decision -----------------------------------

func TestDecideMessage_SetsCommFromThePureSplit(t *testing.T) {
	src := mustReadRepoFile(t, "internal/capture/rules_store.go")
	body := rvFuncSrc(src, "decideMessage")
	if body == "" {
		t.Fatalf("rules_store.go no longer declares decideMessage")
	}
	if !strings.Contains(body, "commTask(") {
		t.Fatalf("decideMessage never calls commTask. Criterion 7: the found branch sets d.comm from the PURE " +
			"commTask(commInput{…}) — the split is a function of values, not a second decision here")
	}
	// It is called with the WINNER's column, not a constant.
	if !regexp.MustCompile(`armed:\s*winner\.commTask`).MatchString(body) {
		t.Errorf("decideMessage does not pass `armed: winner.commTask` to commTask. Criterion 7 / IK \"test the " +
			"column, not the fixture\": arming is the RULE's stored flag; a literal here is the mutation the " +
			"SPEC names first")
	}
	for _, want := range []struct{ tok, why string }{
		{"blankSender(", "D2 clause 3 reuses resurface.go's spelling, never a second blank test"},
		{"notifierSender(", "D2 clause 4 reuses resurface.go's spelling"},
		{"anonymousJiraActor(", "D2 clause 5 reuses ownaction.go's parse"},
	} {
		if !strings.Contains(body, want.tok) {
			t.Errorf("decideMessage does not build commInput with %s — %s", want.tok, want.why)
		}
	}
	// The reason is worded by MODE, decideMessage's existing requested/would
	// pattern (SWT-45 criterion 25).
	if !strings.Contains(body, "comm task requested") {
		t.Errorf("decideMessage never writes \"comm task requested\". Criterion 7: live mode REQUESTS, and the " +
			"reason is the only record of what a pass decided")
	}
	if !strings.Contains(body, "would create a comm task") {
		t.Errorf("decideMessage never writes \"would create a comm task\". Criterion 7/10: a SHADOW pass says " +
			"\"would\" and creates nothing — that is how an arming decision is measured before it is made")
	}
}

// ---- criterion 8: EvaluateRules' order, and the target's mark ------------------------

// commLogBranch returns the source of EvaluateRules' `case actionTaskLog:`
// branch. Scoped deliberately: since 2026-09-22 (swb #491) the actionTask
// branch also calls markRuleActivity, so a function-wide scan would read the
// CREATE path's call and pass vacuously.
func commLogBranch(t *testing.T) string {
	t.Helper()
	body := rvFuncSrc(mustReadRepoFile(t, "internal/capture/rules_store.go"), "EvaluateRules")
	if body == "" {
		t.Fatalf("rules_store.go no longer declares EvaluateRules")
	}
	i := strings.Index(body, "case actionTaskLog:")
	if i < 0 {
		t.Fatalf("EvaluateRules has no `case actionTaskLog:` branch; D3's whole table lives there")
	}
	return body[i:]
}

func TestEvaluateRules_TheCommBranchIsInOrder(t *testing.T) {
	branch := commLogBranch(t)
	// D3's table, in order. Every step's ABSENCE is a named mutation in the SPEC.
	steps := []struct{ call, why string }{
		{"appendRuleLog(", "D3 step 1: byte-identical, first — a crash after it leaves exactly today's behaviour"},
		{"createCommTask(", "D3 step 2: the comm, through create_task on the executor (invariant 3)"},
		{"recordDecisionCommTask(", "D3 step 3: the claim is spent, so a later failure must not lose the " +
			"pointer to what was created (recordDecisionTask's argument, verbatim)"},
		{"setRuleProvenance(", "D3 step 4: without it draft_delivery refuses the task, and \"answer the " +
			"question\" is the point"},
		{"markRuleActivity(", "D3 step 5: this, and ONLY this, is what puts the comm in INCOMING"},
		{"appendCommPointer(", "D3 step 6: LAST — a crash before it leaves a complete, visible comm task"},
	}
	at := 0
	for _, s := range steps {
		j := strings.Index(branch[at:], s.call)
		if j < 0 {
			t.Errorf("EvaluateRules' actionTaskLog branch does not call %s after the previous step — %s\n\n%s",
				s.call, s.why, branch)
			continue
		}
		at += j + len(s.call)
	}
	if !strings.Contains(branch, "decision.comm") {
		t.Errorf("the actionTaskLog branch never reads decision.comm; criterion 8: when it is FALSE the branch " +
			"is byte-unchanged from SWT-72")
	}
	if !strings.Contains(branch, "stats.CommTasks") {
		t.Errorf("the actionTaskLog branch never increments stats.CommTasks (criterion 9)")
	}
}

// The swap that IS the ticket: an armed comm REPLACES SWT-72's mark on the
// target. "Arming rule 75 MOVES José's mail from `task 452 jumps to INCOMING`
// to `a new row in INCOMING, 452 stays in QUEUE`" (D3).
func TestEvaluateRules_TheTargetIsNotMarkedWhenACommIsCreated(t *testing.T) {
	branch := commLogBranch(t)
	guard := regexp.MustCompile(`(?m)^\s*if\s+!decision\.prClose[^{]*\{`).FindString(branch)
	if guard == "" {
		t.Fatalf("the actionTaskLog branch no longer guards the TARGET's markRuleActivity with " +
			"`if !decision.prClose`; SWT-72's one exclusion is gone or respelled, and criterion 8's second " +
			"exclusion has nowhere to live")
	}
	if !strings.Contains(guard, "comm") {
		t.Errorf("the TARGET's activity mark is guarded by %q, which says nothing about decision.comm. "+
			"Criterion 8 / D3: `markRuleActivity on the TARGET is NOT called` when a comm task is created — "+
			"\"the new task is the thing to look at, and surfacing the old one too would double the rows\" "+
			"(D11's sentence, unchanged). Keeping the mark is the SPEC's own mutation row and must turn "+
			"criterion 42 red on the ticket task's activity_at", guard)
	}
}

// ---- criterion 12: no ref, no surfacing, and a pure args builder ---------------------

func TestCommTaskArgs_IsPureAndTheCommPathLinksNoRef(t *testing.T) {
	src := mustReadRepoFile(t, "internal/capture/rules_store.go")
	args := rvFuncSrc(src, "commTaskArgs")
	if args == "" {
		t.Fatalf("rules_store.go declares no commTaskArgs. Criterion 12: createCommTask's args come from a PURE " +
			"function (ruleCreateTaskArgs' precedent), so the payload is testable without a database")
	}
	for _, banned := range []struct{ tok, why string }{
		{"ctx", "pure: no context"},
		{"pool", "pure: no database"},
		{"ex.Execute", "pure: it BUILDS the call, it does not make it"},
		{"time.Now", "pure: no clock"},
	} {
		if strings.Contains(args, banned.tok) {
			t.Errorf("commTaskArgs mentions %q — %s", banned.tok, banned.why)
		}
	}
	for _, want := range []struct{ tok, why string }{
		{`"assignee_type"`, "D4: human — it is his to answer; no worker console may claim it"},
		{`"human"`, "D4: human"},
		{"commTaskTitle(", "D4: the title is {sender}: {subject else first line} (criterion 14)"},
		{"ruleTaskBody(", "D4: the body is ruleTaskBody's plus `related_task: N` (criterion 13)"},
		{`"project"`, "D4: the RULE's project, even if the target task's project differs"},
	} {
		if !strings.Contains(args, want.tok) {
			t.Errorf("commTaskArgs does not carry %s — %s", want.tok, want.why)
		}
	}

	// No external_refs row, and no surfacing. The ref ban is LOAD-BEARING, not
	// an omission: external_refs is UNIQUE (system, external_key) and
	// taskForExternalRef takes the NEWEST ref, so a second ref row would
	// silently hijack every future attach for that ticket onto the comm task.
	create := rvFuncSrc(src, "createCommTask")
	if create == "" {
		t.Fatalf("rules_store.go declares no createCommTask (criterion 12)")
	}
	branch := commLogBranch(t)
	for _, scope := range []struct{ name, body string }{
		{"createCommTask", create}, {"EvaluateRules' actionTaskLog branch", branch},
	} {
		if strings.Contains(scope.body, "linkRuleRef(") || strings.Contains(scope.body, "link_external_ref") {
			t.Errorf("%s calls link_external_ref. Criterion 12 / D4: NO external_refs row for a comm task — the "+
				"key already belongs to the ticket task, external_refs is UNIQUE (system, external_key) and "+
				"taskForExternalRef takes the NEWEST ref, so a second row would hijack every future attach for "+
				"that ticket onto the comm", scope.name)
		}
		if strings.Contains(scope.body, "markRuleSurfaced(") || strings.Contains(scope.body, "task_mark_surfaced") {
			t.Errorf("%s calls task_mark_surfaced. Criterion 12: surfacing is SWT-45's machinery for an "+
				"overriding CREATE; a comm reaches INCOMING through SWT-72's activity columns alone (D5)", scope.name)
		}
		if strings.Contains(scope.body, `"parent_id"`) || strings.Contains(scope.body, "task_add_dependency") {
			t.Errorf("%s sets a parent or a dependency. D4: parent_id carries plan ordering and lifecycle "+
				"meaning, and nothing here is blocked (D11's reasons, verbatim)", scope.name)
		}
	}
}

// ---- criterion 15: the pointer log, ids only -----------------------------------------

// The exact text D3 pins, as a template:
//
//	capture: comm #<new id> created from this message (message <M>)
var commPointerText = regexp.MustCompile(`capture: comm #%d created from this message \(message %d\)`)

func TestCommPointer_IsIdsOnly(t *testing.T) {
	src := mustReadRepoFile(t, "internal/capture/rules_store.go")
	body := rvFuncSrc(src, "appendCommPointer")
	if body == "" {
		t.Fatalf("rules_store.go declares no appendCommPointer (criterion 15, D3 step 6)")
	}
	if !commPointerText.MatchString(body) {
		t.Errorf("appendCommPointer does not carry D3's exact text `capture: comm #%%d created from this "+
			"message (message %%d)`. Criterion 15: the ids-only pointer is what makes the call safe on a "+
			"CLAUDE task, whose log feeds a worker prompt (the C-D13 worry):\n%s", body)
	}
	for _, banned := range []struct{ tok, why string }{
		{"msg.Subject", "criterion 15: ids only — no subject"},
		{"msg.Sender", "criterion 15: ids only — no sender"},
		{"msg.BodyText", "criterion 15: ids only — no body"},
		{"rulesPreview", "criterion 15: ids only — no preview"},
		{"textmatch.", "criterion 15: there is no text to truncate; that is appendRuleLog's job, not this one"},
		{"commTaskTitle", "criterion 15: ids only — no title"},
	} {
		if strings.Contains(body, banned.tok) {
			t.Errorf("appendCommPointer mentions %s — %s", banned.tok, banned.why)
		}
	}
	if !strings.Contains(body, `"task_append_log"`) {
		t.Errorf("appendCommPointer does not call task_append_log; the pointer is an executor call, never a " +
			"direct INSERT (invariant 3)")
	}
	if strings.Contains(body, "task_mark_activity") {
		t.Errorf("appendCommPointer also marks activity on the TARGET. D3: the mark MOVES to the comm task; " +
			"surfacing the target too would double the rows")
	}
	// CONTROL: appendRuleLog DOES carry message text, so the scan above is
	// discriminating between two functions rather than passing vacuously.
	if full := rvFuncSrc(src, "appendRuleLog"); full == "" || !strings.Contains(full, "rulesPreview") {
		t.Fatalf("CONTROL FAILED: appendRuleLog no longer carries the message preview, so the ids-only scan " +
			"above cannot be distinguishing anything")
	}
}

// ---- criterion 9: the counter ---------------------------------------------------------

func TestRulesStats_HasCommTasks(t *testing.T) {
	f, ok := reflect.TypeOf(RulesStats{}).FieldByName("CommTasks")
	if !ok {
		t.Fatalf("RulesStats has no CommTasks field. Criterion 9: it counts CREATED comm tasks, which is the " +
			"number Verification Step 5.4 watches for a day after arming one rule")
	}
	if f.Type.Kind() != reflect.Int {
		t.Errorf("RulesStats.CommTasks is %s, want int (every RulesStats counter is an int)", f.Type)
	}
}

const commCounterFmt = `\"comm_tasks\":%d`

// Every capture counter line prints "comm_tasks", zeros included — the five
// copies of the line the SPEC names, found by what they ALREADY print
// ("appended"), so a new printer is held to the same line. Reuses
// captureCounterFormats (resurface_structure_test.go), which scopes the scan to
// the capture format and not the whole file.
func TestCaptureCounterLines_PrintCommTasks(t *testing.T) {
	var printers []string
	err := filepath.Walk(filepath.Join("..", "..", "cmd"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		formats := captureCounterFormats(string(b))
		if len(formats) == 0 {
			return nil
		}
		rel := strings.TrimPrefix(filepath.ToSlash(path), "../../")
		printers = append(printers, rel)
		for _, f := range formats {
			if !strings.Contains(f, commCounterFmt) {
				t.Errorf("%s prints the capture counters without \\\"comm_tasks\\\":%%d in the SAME format "+
					"(criterion 9). Zeros included: Step 5.4 reads \"comm_tasks\":N in the connector logs for a "+
					"day after ONE rule is armed, and a pass that created none must not look like a pass that "+
					"never ran", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk cmd/: %v", err)
	}
	for _, want := range []string{
		"cmd/connectors/jira/main.go", "cmd/connectors/slackweb/main.go", "cmd/connectors/upworkcrm/main.go",
		"cmd/connectors/google/main.go", "cmd/opsctl/main.go",
	} {
		found := false
		for _, p := range printers {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s no longer prints the capture counter line (\"appended\":%%d); the scan cannot hold it "+
				"to criterion 9. Found printers: %v", want, printers)
		}
	}
	// pipeline.CapturedWake's counts map is UNCHANGED (criterion 9: it carries
	// five keys by design).
	wake := mustReadRepoFile(t, "internal/pipeline/contract.go") // the wake lives in contract.go
	if strings.Contains(wake, "comm_tasks") {
		t.Errorf("internal/pipeline/pipeline.go names comm_tasks; criterion 9: CapturedWake's counts map is " +
			"unchanged — it carries five keys by design and the MQTT contract does not move (SPEC \"MQTT topics: None\")")
	}
}

// ---- criterion 19: the dry run ---------------------------------------------------------

func TestDryRun_NamesTheCommProposalAndPrintsTheFlag(t *testing.T) {
	src := mustReadRepoFile(t, "internal/capture/dryrun.go")
	proposed := rvFuncSrc(src, "dryRunProposed")
	if proposed == "" {
		t.Fatalf("dryrun.go no longer declares dryRunProposed")
	}
	if !strings.Contains(proposed, "comm") {
		t.Errorf("dryRunProposed never names the comm proposal. Criterion 19: `opsctl capture-rules try " +
			"--comm-task --show all` is how he sees what arming WOULD do before anything is armed — that is " +
			"Verification Step 0a's gate made visible")
	}
	flags := rvFuncSrc(src, "dryRunRuleFlags")
	if flags == "" {
		t.Fatalf("dryrun.go no longer declares dryRunRuleFlags")
	}
	if !strings.Contains(flags, "commTask") {
		t.Errorf("dryRunRuleFlags does not print the comm_task flag (criterion 19; the pr_review line is the " +
			"precedent)")
	}
	if !strings.Contains(src, "CommTask") {
		t.Errorf("internal/capture/dryrun.go's CandidateRule has no CommTask field; `opsctl capture-rules try " +
			"--comm-task` has nothing to pass (criterion 19)")
	}
}

// ---- criterion 41's structural neighbour: the reconciler learns nothing ---------------

// internal/ticketstatus has its own scan (its SWT-72 neighbour test); this one
// keeps the CAPTURE side honest about the reverse direction — nothing in the
// comm path reads or writes surfaced_*, whose reader is the J11 hold.
func TestCommPath_NeverTouchesTheSurfacedColumns(t *testing.T) {
	branch := commLogBranch(t)
	for _, banned := range []string{"surfaced_at", "surfaced_by_message_id"} {
		if strings.Contains(branch, banned) {
			t.Errorf("EvaluateRules' actionTaskLog branch names %s. Criterion 41: a comm task reaches INCOMING "+
				"through SWT-72's activity columns; reusing SWT-45's surfaced_* would hold every commented-on "+
				"ticket's task open against the reconciler (SWT-72 D1's landmine)", banned)
		}
	}
}
