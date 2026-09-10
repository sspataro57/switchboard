package ticketstatus_test

// Structural tests for SWT-32 (docs/tickets/jira-status-sync_SPEC.md) — the
// criteria the SPEC asks to be enforced mechanically rather than by review:
// 1, 2 and 3 (migration 0023's shape), 8 (no status-name list, no email, no
// display-name comparison), 15 (external_url is never read), 20 (the decision
// stays pure), 34 (the raw id is never built in SQL), 40 (tasks/task_events only
// through the executor), 41 (the advisory lock has its own key), and the wiring
// criteria 12, 13, 45, 46 and 47.
//
// ZERO I/O beyond reading this repo's own source and docs. Same shape as
// internal/promote/structure_test.go and internal/tools/dismiss_structure_test.go,
// retargeted; every assertion REQUIRES its subject to exist first, because a
// source scan that passes because there was nothing to scan is this repo's
// "fixture that proves nothing" landmine wearing a lab coat.
//
// GREENFIELD NOTE — EXPECTED RED. internal/ticketstatus has no non-test source,
// migrations/0023_ticket_status_sync.sql does not exist, and neither
// cmd/connectors/jira/main.go nor cmd/opsctl/main.go nor cmd/jira-auth/main.go
// nor docs/runbooks/ticket-status-sync.md knows about the pass — so this file's
// package does not build (no non-test Go files) and, once the package lands,
// each guard below fails until its subject does.
//
// THE ADVISORY-LOCK KEY IS NEVER SPELLED IN THIS FILE. The repo-wide collision
// scan in internal/classify/structure_test.go walks internal/ for
// 0x5157xxxx literals and fails on duplicates, so a key restated in a test is a
// key that reads as taken twice. TestAdvisoryLockKey_IsThisPackagesAlone below
// EXTRACTS the literal from store.go with a regexp instead — which is also the
// only way to assert "spelled exactly once".

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const tsModule = "github.com/sspataro57/switchboard"

// ---- helpers -----------------------------------------------------------------

// tsRepoFile reads a path relative to the repo root (this package sits at
// internal/ticketstatus, two levels down).
func tsRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v — the file this criterion is about does not exist, and a scan with "+
			"nothing to scan proves nothing", rel, err)
	}
	return string(b)
}

// tsSources lists a package's NON-test .go files, relative to the repo root, and
// FAILS when there are none.
func tsSources(t *testing.T, pkg string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("..", "..", pkg))
	if err != nil {
		t.Fatalf("read %s: %v", pkg, err)
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(pkg, n))
	}
	if len(out) == 0 {
		t.Fatalf("no non-test .go files in %s; a scan with nothing to scan proves nothing (D1: the "+
			"separate package is what makes the executor boundary structural)", pkg)
	}
	return out
}

// tsFileImports returns the import PATHS of one .go file, parsed rather than
// grepped — so this file can NAME a banned path in an argument without banning
// itself.
func tsFileImports(t *testing.T, rel string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join("..", "..", rel), nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	var out []string
	for _, spec := range f.Imports {
		out = append(out, strings.Trim(spec.Path.Value, `"`))
	}
	return out
}

// ---- criterion 20: the decision is pure, and its test stays offline ----------

// "ticketstatus.Decide(obs Observation, state *State) Decision is a pure
// function of its arguments: a structure test scans its file for pgx, context,
// os.Getenv, net and jira (the client), in the shape of
// internal/capture/rules_structure_test.go."
//
// The FILE is the boundary, so purity cannot rot into "pure except for one
// lookup" — and the reason it matters here is that the reconciler's inputs come
// from six joined tables: the moment Decide can reach a pool, the 18-row table
// above stops being a proof of anything.
func TestDecideGo_IsPure(t *testing.T) {
	const rel = "internal/ticketstatus/decide.go"
	src := tsRepoFile(t, rel)

	if !strings.Contains(src, "func Decide(") {
		t.Fatalf("%s does not declare Decide; criterion 20 pins it to THIS file, because the file is "+
			"what makes the purity checkable", rel)
	}
	for _, b := range []struct{ token, why string }{
		{`"context"`, "a context parameter is the first thing an I/O call needs; its absence is what keeps Decide offline"},
		{"pgx", "no database: the decision must be testable with zero network (invariant 7)"},
		{"os.Getenv", "configuration is passed in (the TTL, the gate), never read from the environment inside the decision"},
		{`"net/http"`, "no network"},
		{tsModule + "/internal/connector/jira", "no client: the facts arrive as VALUES read from a stored raw row (D19), never as a fetch"},
		{tsModule + "/internal/executor", "no executor: Decide returns a decision; the driver makes the calls"},
	} {
		if strings.Contains(src, b.token) {
			t.Errorf("%s mentions %q — criterion 20 forbids it: %s", rel, b.token, b.why)
		}
	}
}

// The other half, in the internal/promote/structure_test.go shape: the pure
// test's own import block. The day someone needs "just one lookup" in
// decide_test.go is the day Decide stops being a pure function, and the import
// block is where that shows first.
func TestDecideTest_ImportsNothingThatCouldDoIO(t *testing.T) {
	const rel = "internal/ticketstatus/decide_test.go"
	imports := tsFileImports(t, rel)
	if len(imports) == 0 {
		t.Fatalf("%s imports nothing at all; it cannot be exercising ticketstatus.Decide", rel)
	}
	subject := false
	for _, imp := range imports {
		if imp == tsModule+"/internal/ticketstatus" {
			subject = true
		}
		for _, banned := range []struct{ frag, why string }{
			{"pgx", "no database (invariant 7)"},
			{"net", "no network"},
			{tsModule + "/internal/connector/jira", "no client, and no jira.Facts: Observation carries VALUES"},
			{tsModule + "/internal/store", "no pool constructor — that is the integration suite's job"},
			{tsModule + "/internal/executor", "the pure test asserts decisions, not tool calls"},
		} {
			if imp == banned.frag || strings.Contains(imp, banned.frag+"/") || strings.HasSuffix(imp, "/"+banned.frag) {
				t.Errorf("%s imports %q — criterion 20 forbids it: %s", rel, imp, banned.why)
			}
		}
	}
	if !subject {
		t.Errorf("%s does not import %s/internal/ticketstatus; whatever it tests, it is not the "+
			"reconciler's decision", rel, tsModule)
	}
}

// ---- criterion 40: every task write goes through the executor ----------------

// "A structural test asserts internal/ticketstatus contains no UPDATE tasks,
// INSERT INTO tasks or INSERT INTO task_events; its only direct writes are
// ticket_status_syncs (raw writes belong to internal/connector/jira, criterion
// 9)."
//
// Invariant 3 for this package. The control at the end matters as much as the
// bans: if this package wrote nothing at all, the four bans would be passing
// over a package that touches no tables.
func TestTicketStatus_NeverWritesToolActionTablesDirectly(t *testing.T) {
	files := tsSources(t, "internal/ticketstatus")
	insertBanned := regexp.MustCompile(`(?is)insert\s+into\s+(tasks|task_events|deliveries|external_refs|raw_source_items|task_dismissals)\b`)
	updateBanned := regexp.MustCompile(`(?is)update\s+(tasks|task_events|deliveries|external_refs|raw_source_items|task_dismissals)\b`)
	deleteBanned := regexp.MustCompile(`(?is)delete\s+from\s+(tasks|task_events|external_refs|raw_source_items|task_dismissals)\b`)

	var writesItsOwnState bool
	own := regexp.MustCompile(`(?is)insert\s+into\s+ticket_status_syncs\b`)
	for _, rel := range files {
		src := tsRepoFile(t, rel)
		if m := insertBanned.FindString(src); m != "" {
			t.Errorf("%s contains %q — invariant 3: every status change is a task_close / task_reopen "+
				"call and every log line a task_append_log call, all with actor ticketstatus:jira and "+
				"Call.TaskID set. The lookup's RAW write belongs to internal/connector/jira "+
				"(criterion 9), not here", rel, m)
		}
		if m := updateBanned.FindString(src); m != "" {
			t.Errorf("%s contains %q — same rule: a direct mutation skips validate -> policy -> audit "+
				"and leaves no answer to 'who did this'", rel, m)
		}
		if m := deleteBanned.FindString(src); m != "" {
			t.Errorf("%s contains %q — this pass never removes anything. Closed is closed; nothing is "+
				"deleted or archived (Out of scope)", rel, m)
		}
		if own.MatchString(src) {
			writesItsOwnState = true
		}
	}
	if !writesItsOwnState {
		t.Errorf("no file in internal/ticketstatus contains `INSERT INTO ticket_status_syncs`. D3: the " +
			"pass's own state row is written directly by its own package (the capture_decisions / " +
			"classify_promotions precedent) — without that write, the bans above are scanning a " +
			"package that touches no tables at all")
	}
}

// ---- criterion 8: no name list, no email, no display name --------------------

// "A structural test asserts that neither internal/ticketstatus nor the new jira
// reader contains a status-name list, an email address, or a display-name
// comparison."
//
// Three separate defects with one shape — a human-readable label standing in for
// an identifier:
//   - a status NAME list is D2's magic-literal defect (names are per-project
//     workflow configuration);
//   - an email address is D12's ("me" is an accountId, per source account, never
//     a constant in code);
//   - a display-name comparison is the recorded slackweb landmine, verbatim: no
//     display-name matching, ever.
func TestTicketStatus_HasNoNameListNoEmailNoDisplayName(t *testing.T) {
	packages := []string{"internal/ticketstatus"}
	jiraReaders := []string{"facts.go", "rawid.go", "lookup.go"}

	var files []string
	for _, pkg := range packages {
		files = append(files, tsSources(t, pkg)...)
	}
	for _, name := range jiraReaders {
		rel := filepath.Join("internal/connector/jira", name)
		if _, err := os.Stat(filepath.Join("..", "..", rel)); err != nil {
			t.Fatalf("%s does not exist: %v — the SPEC's Files-likely-to-touch names it, and the scan "+
				"below is only meaningful over files that exist", rel, err)
		}
		files = append(files, rel)
	}

	email := regexp.MustCompile(`"[^"\s]+@[^"\s]+\.[a-z]{2,}"`)
	// SWT-34 criterion 30 STRENGTHENS this list rather than softening it. The
	// qa-delivered-drop ticket adds a per-project CONFIGURED set of status
	// names — which does NOT overturn D2, it relocates the knowledge: a
	// workflow-shaped fact belongs in configuration, never in code. So the three
	// REAL Treetop names join the generic six, and the very strings the runbook
	// seeds into projects.ticket_delivered_statuses are the ones no source file
	// here may contain. The names live in the database or they do not exist.
	statusNames := []string{`"Done"`, `"Closed"`, `"Resolved"`, `"Won't Do"`, `"In Progress"`, `"To Do"`,
		`"TT-In QA"`, `"TT-In Review"`, `"TT-Verified"`}

	// Positive control for the widening: a scan whose needles no longer match
	// anything passes every file for the wrong reason. Each banned literal must
	// be found in a probe that CONTAINS it — cheap, and it is the only thing
	// standing between this list and a quoting change that silently disarms it.
	for _, name := range statusNames {
		probe := "\t\tcase " + name + ": // a hypothetical hardcoded workflow name\n"
		if !strings.Contains(probe, name) {
			t.Fatalf("the scan's needle %s does not match its own probe %q; the ban has stopped "+
				"matching anything", name, probe)
		}
	}

	for _, rel := range files {
		src := tsRepoFile(t, rel)
		for _, name := range statusNames {
			if strings.Contains(src, name) {
				t.Errorf("%s contains the status NAME literal %s. D2: the discriminator is "+
					"fields.status.statusCategory.key — every custom status maps into one of three "+
					"Jira-level keys, while a name list passes every fixture and then silently stops "+
					"closing tasks the day a client renames a column", rel, name)
			}
		}
		if m := email.FindString(src); m != "" {
			t.Errorf("%s contains the email literal %s. D12: 'me' is the polling account's own "+
				"accountId, read per source account from sync_cursor->>'own_account_id' — an address "+
				"in code is one Atlassian identity hard-wired into a rule that must be correct on two "+
				"sites", rel, m)
		}
		if strings.Contains(src, "displayName") || strings.Contains(src, "DisplayName") {
			t.Errorf("%s reads a display name. Display names change and are not unique (the slackweb "+
				"landmine: no display-name matching, ever); the comparison is on accountId", rel)
		}
	}
}

// ---- criterion 15: external_url is never read -------------------------------

// "A ref never causes a fetch outside its account's declared prefixes ... the
// external_url column is never read for routing (D18's SSRF argument)."
//
// Structural because the behavioural proof (the integration case that seeds a
// ref pointing at another host) can only show that TODAY's router ignores it.
// external_refs.external_url is written by `link_external_ref`, which IS on the
// MCP agent surface — so a router that read it would let a crafted ref aim a
// stored API token at a host of the caller's choosing.
func TestTicketStatus_NeverReadsExternalURL(t *testing.T) {
	for _, rel := range tsSources(t, "internal/ticketstatus") {
		if strings.Contains(tsRepoFile(t, rel), "external_url") {
			t.Errorf("%s mentions external_url. D18: routing is by ticket-key PREFIX against the "+
				"account's mandatory scopes; external_url is agent-facing free text and is not an "+
				"input to any decision this pass makes", rel)
		}
	}
}

// ---- criterion 34: the raw id is computed in Go, never built in SQL ---------

// "No SQL in this ticket concatenates the raw id: ids are computed in Go via
// jira.IssueRawID and bound as one array parameter (= ANY($1)), the way
// ClientThreadPrefix is passed. Structural test asserts no `'issue:' ||` or
// `format('issue:%s')` in internal/."
//
// The upworkcrm keyspelling_test.go precedent, narrowed to what can actually
// discriminate: the SQL-side CONSTRUCTIONS, not the literal `issue:` itself
// (criterion 5 says out loud why a repo-wide literal ban is refused here — the
// string is too generic, and a guard that cannot match only its target is worse
// than none).
func TestIssueRawID_IsNeverBuiltInSQL(t *testing.T) {
	concat := regexp.MustCompile(`'issue:'\s*\|\||\|\|\s*'issue:'|format\(\s*'issue:`)
	like := regexp.MustCompile(`(?i)like\s+'issue:`)

	sawAFile := false
	err := filepath.Walk(filepath.Join("..", "..", "internal"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		rel := strings.TrimPrefix(filepath.ToSlash(path), "../../")
		if rel == "internal/ticketstatus/structure_test.go" {
			return nil // this file names the patterns it bans
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		sawAFile = true
		src := string(b)
		if m := concat.FindString(src); m != "" {
			t.Errorf("%s builds an issue raw id in SQL (%q). Criterion 34: the id is computed in Go by "+
				"jira.IssueRawID and BOUND as one array parameter — a second spelling in SQL is how "+
				"the poller's rows and the lookup's rows end up under different external_ids for the "+
				"same issue", rel, m)
		}
		if m := like.FindString(src); m != "" {
			t.Errorf("%s matches raw ids with %q. A LIKE over external_id is the untyped predicate the "+
				"= ANY($1) bind replaces; it also silently matches `issue:WEB-1234-2`", rel, m)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
	if !sawAFile {
		t.Fatalf("the scan visited no .go file under internal/; it is not seeing the files it claims to check")
	}
}

// ---- criterion 41: this pass's own advisory lock ----------------------------

// "The pass takes an advisory lock on its own key ... The key follows the
// established convention — the same four hex digits as this ticket's migration
// number — and its freeness is checked mechanically by the collision scan in
// internal/classify/structure_test.go."
//
// This test does NOT restate the literal: it extracts it. Two reasons, and the
// second is the one that bites. (1) A key written into a second file reads as
// taken twice to the repo-wide collision scan. (2) "Spelled exactly once" is not
// assertable by comparing against a constant the test itself supplies — that is
// the fixture-shaped-like-the-assertion landmine, and it is how a package ends
// up with the lock key in three places that agree until one is edited.
// tsLockKeyPattern matches an advisory-lock literal WITHOUT being one: the
// repo-wide collision scan looks for 0x5157 followed by four hex digits, and
// `0x5157_?([0-9A-Fa-f]{4})` is not that. Shared with
// store_integration_test.go, which takes the lock to prove the contention
// policy.
var tsLockKeyPattern = regexp.MustCompile(`0x5157_?([0-9A-Fa-f]{4})`)

// tsLockLiterals returns every advisory-lock literal in this package's non-test
// sources, as "file: literal".
func tsLockLiterals(t *testing.T) (literals []string, digits string) {
	t.Helper()
	for _, rel := range tsSources(t, "internal/ticketstatus") {
		for _, m := range tsLockKeyPattern.FindAllStringSubmatch(tsRepoFile(t, rel), -1) {
			literals = append(literals, rel+": "+m[0])
			digits = strings.ToLower(m[1])
		}
	}
	return literals, digits
}

// tsLockKeyFromSource parses the pass's advisory-lock key out of its own source
// so a TEST can take the lock without RESTATING the literal (which would read as
// a collision to internal/classify/structure_test.go's repo-wide scan).
func tsLockKeyFromSource(t *testing.T) int64 {
	t.Helper()
	literals, digits := tsLockLiterals(t)
	if len(literals) != 1 {
		t.Fatalf("internal/ticketstatus declares %d advisory-lock literals (%v), want exactly 1",
			len(literals), literals)
	}
	n, err := strconv.ParseInt("5157"+digits, 16, 64)
	if err != nil {
		t.Fatalf("parse advisory-lock key from %v: %v", literals, err)
	}
	return n
}

func TestAdvisoryLockKey_IsThisPackagesAlone(t *testing.T) {
	keyPattern := tsLockKeyPattern

	// (1) exactly ONE literal, in the package's non-test sources.
	literals, digits := tsLockLiterals(t)
	if len(literals) != 1 {
		t.Fatalf("internal/ticketstatus declares %d advisory-lock literals (%v), want exactly 1. The "+
			"lock is taken on a dedicated connection with an explicit unlock before the connection is "+
			"released (capture.tryRulesLock and promote.tryLock are the two spellings to copy), and "+
			"the key belongs in ONE place", len(literals), literals)
	}

	// (2) the convention: the same four hex digits as the migration of the
	// ticket that CREATED the lock — SWT-32's 0023, and it stays 0023 forever.
	//
	// DO NOT "align" this to a newer number. SWT-34 adds migration 0025 to this
	// same package and deliberately leaves the assertion here alone: the key is
	// LIVE, a pass running the old code holds 0x5157_0023, and changing the
	// literal would let two passes run concurrently once and then read as a
	// fresh collision to internal/classify's repo-wide scan. The convention
	// names the lock's birth, not the newest migration to touch the package.
	if digits != "0023" {
		t.Errorf("the advisory-lock key's low four hex digits are %q, want \"0023\" — the established "+
			"convention is the ticket's migration number, which is what makes the collision scan mechanical. "+
			"If 0023 were taken, the SPEC says take the next free number and say why in a comment "+
			"beside it", digits)
	}

	// (3) the collision half, over the whole of internal/. classify's scan owns
	// the general rule; this one is about THIS key, and it fails loudly rather
	// than leaving two CronJobs silently excluding each other on a schedule.
	var elsewhere []string
	err := filepath.Walk(filepath.Join("..", "..", "internal"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		rel := strings.TrimPrefix(filepath.ToSlash(path), "../../")
		if strings.HasPrefix(rel, "internal/ticketstatus/") {
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, m := range keyPattern.FindAllStringSubmatch(string(b), -1) {
			if strings.EqualFold(m[1], digits) {
				elsewhere = append(elsewhere, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
	if len(elsewhere) > 0 {
		t.Errorf("this pass's advisory-lock key also appears in %v. Two workers sharing a key silently "+
			"exclude each other: whichever runs second exits as if another instance were already "+
			"running, on a schedule, with no error anywhere", elsewhere)
	}
}

// ---- criteria 1, 2, 3: migration 0023 --------------------------------------

func TestMigration0023_IsTheOnlyOneThisTicketAdds(t *testing.T) {
	// Control first: 0022 must still be there, or a glob returning nothing below
	// would prove only that the directory moved.
	if _, err := os.Stat(filepath.Join("..", "..", "migrations", "0022_task_dismissals.sql")); err != nil {
		t.Fatalf("migrations/0022_task_dismissals.sql is missing: %v — SWT-31 owns it and this "+
			"ticket's precondition is that it merged first", err)
	}

	matches, err := filepath.Glob(filepath.Join("..", "..", "migrations", "0023_*.sql"))
	if err != nil {
		t.Fatalf("glob migrations/0023_*.sql: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("found %d migrations/0023_*.sql file(s), want exactly 1 (the SPEC names "+
			"0023_ticket_status_sync.sql). 0022 is the current highest, and this ticket's data-model "+
			"section is ONE migration: the state table and the projects gate column. Merging a "+
			"migration is not applying it — check `SELECT max(version) FROM schema_migrations` before "+
			"deploying", len(matches))
	}
	// The LIVING registry of owned migration numbers is the ledger in
	// internal/classify/structure_test.go; this guard keeps only its own
	// ticket's claim, exactly one 0023 (the SWT-30/31 amendment shape).

	rel := filepath.Join("migrations", filepath.Base(matches[0]))
	sql := strings.ToLower(tsRepoFile(t, rel))

	// (1) criterion 2: the gate is a typed COLUMN, default false, armed by hand.
	alter := regexp.MustCompile(`alter\s+table\s+projects\s+add\s+column\s+ticket_assignee_gate\s+boolean\s+not\s+null\s+default\s+false`)
	if !alter.MatchString(sql) {
		t.Errorf("%s does not `ALTER TABLE projects ADD COLUMN ticket_assignee_gate BOOLEAN NOT NULL "+
			"DEFAULT false`. D11: a typed column, not a policies jsonb key (0016 and 0018 set that "+
			"precedent), and DEFAULT FALSE is the fail-closed side HERE — not gating leaves today's "+
			"behaviour, while gating by accident silently empties a client's board", rel)
	}
	if regexp.MustCompile(`update\s+projects\s+set[^;]*ticket_assignee_gate`).MatchString(sql) {
		t.Errorf("%s ARMS the gate with an UPDATE. Criterion 2: arming is an operator act recorded in "+
			"the runbook, exactly as classify_promote_after is — a migration that armed reengine "+
			"would empty part of a board as a deploy side effect", rel)
	}
	if regexp.MustCompile(`create\s+index[^;]*ticket_assignee_gate`).MatchString(sql) {
		t.Errorf("%s indexes ticket_assignee_gate. `projects` holds tens of rows and every reader "+
			"reaches it by primary key; an index nothing uses is a permanent claim some query needs "+
			"it (0016's and 0018's recorded argument)", rel)
	}

	// (2) criterion 3: NO source_accounts DDL. `provider` has no CHECK
	// constraint (0001:13), so `jira_lookup` is a VALUE, not a schema change.
	if regexp.MustCompile(`alter\s+table\s+source_accounts`).MatchString(sql) {
		t.Errorf("%s alters source_accounts. Criterion 3 / D17: provider has no CHECK constraint and "+
			"the table is UNIQUE (provider, account_email), so the lookup account needs no DDL — and "+
			"the same email may exist under both providers without colliding", rel)
	}

	// (3) forward-only, always.
	if strings.Contains(sql, "drop column") || strings.Contains(sql, "drop table") ||
		regexp.MustCompile(`(?s)--\s*down`).MatchString(sql) {
		t.Errorf("%s looks like it carries a down migration or a DROP. Migrations here are "+
			"FORWARD-ONLY, no exceptions", rel)
	}

	// (4) criterion 1: the state table.
	if !regexp.MustCompile(`create\s+table\s+ticket_status_syncs`).MatchString(sql) {
		t.Fatalf("%s does not CREATE TABLE ticket_status_syncs. D3: a TYPED state row per "+
			"external_ref — not external_refs.sync_cursor (an untyped TEXT column with no writer) and "+
			"not an inference from task_events.payload->>'reason', which SWT-31 criterion 20 forbids "+
			"outright", rel)
	}
	for _, want := range []struct{ frag, why string }{
		{"external_ref_id", "one row per jira external_ref — the pass's unit of work"},
		{"task_id", "the task the ref points at, so the report joins without a second lookup"},
		{"status_category", "the fact the decision turns on, as last observed"},
		{"status_name", "DIAGNOSTIC only (D2) — nothing branches on it"},
		{"assignee_account_id", "the accountId as last observed, for the report"},
		{"assigned_to_self", "recorded, never read as an input: a cached boolean would go stale the day the polling identity changes"},
		{"last_action", "'closed' is the only value that authorises a later reopen (D3)"},
		{"drop_reason", "WHICH fact dropped it, for 'why did this leave the board'"},
		{"closed_from_status", "so a reopen restores the status instead of flattening it to ready (D6)"},
		{"observed_at", "when the snapshot was last read"},
		{"acted_at", "when the executor call was made, NULL for a no-op"},
	} {
		if !strings.Contains(sql, want.frag) {
			t.Errorf("%s's ticket_status_syncs has no %s column — %s", rel, want.frag, want.why)
		}
	}

	// (5) the three CHECKs, by value. An unconstrained TEXT lets a fourth
	// last_action or a third drop_reason appear with no migration and no test.
	for _, c := range []struct {
		column string
		values []string
	}{
		{"status_category", []string{"new", "indeterminate", "done"}},
		{"last_action", []string{"none", "closed", "reopened", "refused_active", "suppressed_dismissed"}},
		{"drop_reason", []string{"ticket_done", "not_assigned"}},
	} {
		re := regexp.MustCompile(`(?s)` + c.column + `[^,]*check\s*\(\s*` + c.column + `\s+in\s*\(([^)]*)\)`)
		m := re.FindStringSubmatch(sql)
		if m == nil {
			t.Errorf("%s does not constrain %s with CHECK (%s IN (...)). The vocabulary is the whole "+
				"contract of this ticket; an unconstrained column is how a fourth value appears with "+
				"no migration and no test", rel, c.column, c.column)
			continue
		}
		for _, v := range c.values {
			if !strings.Contains(m[1], "'"+v+"'") {
				t.Errorf("%s's %s CHECK (%s) does not allow %q", rel, c.column, strings.TrimSpace(m[1]), v)
			}
		}
	}

	// (6) the FKs, both cascading.
	for _, fk := range []struct{ column, table string }{
		{"external_ref_id", "external_refs"},
		{"task_id", "tasks"},
	} {
		re := regexp.MustCompile(`(?s)` + fk.column + `[^,]*references\s+` + fk.table + `\s*\(\s*id\s*\)\s*on\s+delete\s+cascade`)
		if !re.MatchString(sql) {
			t.Errorf("%s does not declare %s ... REFERENCES %s(id) ON DELETE CASCADE. capture_decisions' "+
				"recorded reason applies unchanged: the integration suites clear fixtures by deleting "+
				"tasks, and without the cascade they fail INSIDE cleanup — which reads like the "+
				"cross-pollution pact breaking rather than like a new FK", rel, fk.column, fk.table)
		}
	}

	// (7) the unique index is TOTAL, and it is the ONLY index.
	uniq := regexp.MustCompile(`(?s)create\s+unique\s+index\s+\S+\s+on\s+ticket_status_syncs\s*\(\s*external_ref_id\s*\)([^;]*)`)
	um := uniq.FindStringSubmatch(sql)
	if um == nil {
		t.Errorf("%s has no UNIQUE index on ticket_status_syncs (external_ref_id). One row per ref, "+
			"UPSERTed in place — and the index is what lets `ON CONFLICT (external_ref_id) DO UPDATE` "+
			"omit a restated predicate", rel)
	} else if strings.Contains(um[1], "where") {
		t.Errorf("%s makes the unique index PARTIAL (%q). The SPEC says TOTAL on purpose: a partial "+
			"index forces every ON CONFLICT to restate the predicate (capture_decisions_live_uniq and "+
			"task_events_outbound_observed_uniq both do), and omitting it raises 'no unique or "+
			"exclusion constraint matching the ON CONFLICT specification' at RUNTIME", rel,
			strings.TrimSpace(um[1]))
	}
	idx := regexp.MustCompile(`create\s+(unique\s+)?index\s+\S+\s+on\s+ticket_status_syncs`)
	if n := len(idx.FindAllString(sql, -1)); n != 1 {
		t.Errorf("%s creates %d indexes on ticket_status_syncs, want exactly 1. One row per jira "+
			"external ref — tens today — and every read either scans the table whole or reaches it by "+
			"external_ref_id", rel, n)
	}

	// (8) invariant 2, said out loud, as 0015, 0021 and 0022 do.
	if !strings.Contains(sql, "invariant 2") {
		t.Errorf("%s does not say out loud that ticket_status_syncs is not a second tasks table. The "+
			"next reader's first instinct is to add a status column and a worker to it", rel)
	}
	table := sql
	if i := strings.Index(sql, "create table ticket_status_syncs"); i >= 0 {
		table = sql[i:]
		if j := strings.Index(table, ");"); j > 0 {
			table = table[:j]
		}
	}
	for _, banned := range []struct{ col, why string }{
		{"title", "the items stay rows in `tasks`; a title here is a second board"},
		{"assignee_type", "nothing ever works a ticket_status_syncs row"},
		{"priority", "queues are FILTERS on the one tasks table, never new tables"},
		{"claim", "no claim, for the same reason"},
	} {
		if strings.Contains(table, banned.col) {
			t.Errorf("%s's ticket_status_syncs declares a %q column — invariant 2: %s", rel, banned.col, banned.why)
		}
	}
}

// ---- criterion 45: the pass runs AFTER capture in the connector main --------

// "cmd/connectors/jira/main.go runs the pass AFTER capture.EvaluateRules (D8,
// with the reason in a comment); its counters print before its error is
// returned, matching the capture block directly above it."
//
// The order is load-bearing in a small, pleasant way: a notification about a
// ticket that is already Done, or already someone else's, creates the task in
// capture's half of the tick and this pass closes it in the SAME tick — so the
// state Salvador is complaining about never persists for 15 minutes.
func TestConnectorMain_RunsThePassAfterCapture(t *testing.T) {
	const rel = "cmd/connectors/jira/main.go"
	src := tsRepoFile(t, rel)

	capIdx := strings.Index(src, "capture.EvaluateRules(")
	if capIdx < 0 {
		t.Fatalf("%s no longer calls capture.EvaluateRules; the ordering criterion 45 is about has "+
			"lost one of its two halves", rel)
	}
	passIdx := strings.Index(src, "ticketstatus.Run(")
	if passIdx < 0 {
		t.Fatalf("%s never calls ticketstatus.Run. Criterion 45: the recurring path IS this call — "+
			"without it the pass is hand-run forever and the */15 reconciliation never happens", rel)
	}
	if passIdx < capIdx {
		t.Errorf("%s calls ticketstatus.Run BEFORE capture.EvaluateRules. D8: capture creates the task "+
			"for a notification about an already-Done ticket, and this pass closes it in the same "+
			"tick; reversed, the task is created and left on the board until the next run", rel)
	}
	// Counters before the error return, matching the capture block above it: a
	// silent pass and a pass that did not run must not look the same in a
	// CronJob log.
	tail := src[passIdx:]
	printIdx := strings.Index(tail, "Printf(")
	errIdx := strings.Index(tail, "return fmt.Errorf(")
	if printIdx < 0 {
		t.Errorf("%s prints no counters after ticketstatus.Run (criterion 43: unconditionally, zeros "+
			"included)", rel)
	} else if errIdx >= 0 && errIdx < printIdx {
		t.Errorf("%s returns the pass's error BEFORE printing its counters. The capture block "+
			"directly above says why it does not: the counts go out unconditionally, so 'found "+
			"nothing' and 'never ran' are different lines", rel)
	}
}

// ---- criterion 46: the opsctl subcommand ------------------------------------

func TestOpsctl_HasTheTicketStatusSubcommand(t *testing.T) {
	const rel = "cmd/opsctl/main.go"
	src := tsRepoFile(t, rel)

	if !strings.Contains(src, `case "ticket-status":`) {
		t.Errorf("%s has no `ticket-status` case in its subcommand switch. Criterion 46: `opsctl "+
			"ticket-status sync [--dry-run] [--force] [--limit N]` and `opsctl ticket-status report`, "+
			"following the capture-rules block — and the hand-run IS the 'usable alone' deliverable, "+
			"since the CronJob keeps running the pinned old image until the kube session re-pins it", rel)
	}
	if !strings.Contains(src, "ticket-status") || !strings.Contains(src, "usage: opsctl <") {
		t.Fatalf("%s does not mention ticket-status at all", rel)
	}
	usage := src[strings.Index(src, "usage: opsctl <"):]
	if end := strings.Index(usage, "\n"); end > 0 {
		usage = usage[:end]
	}
	if !strings.Contains(usage, "ticket-status") {
		t.Errorf("%s's usage line (%q) does not list ticket-status. A subcommand nobody can discover "+
			"from --help is a subcommand the next operator will not run", rel, usage)
	}
}

// ---- criteria 12 + 13: the jira_lookup account is visible and hand-made -----

// Criterion 12's LAST clause and criterion 13, both about cmd/jira-auth. The
// isolation proofs (ListAccounts / pendingRaw / the normalizer) are integration
// tests against a real schema; what belongs HERE is the one query that must be
// WIDENED and the flag that creates the row.
//
// "a page that looks empty may be the wrong page": a lookup account invisible to
// `jira-auth list` is an account whose existence nobody can confirm, and D17's
// whole safety argument is that forgetting a clause makes the account INVISIBLE.
// That is the safe direction only if the operator has one place that still shows
// it.
func TestJiraAuth_ListsBothProvidersAndCanCreateALookupAccount(t *testing.T) {
	const rel = "cmd/jira-auth/main.go"
	src := tsRepoFile(t, rel)

	if !strings.Contains(src, "jira_lookup") {
		t.Fatalf("%s never mentions jira_lookup. Criteria 12 and 13: `list` is the ONE query widened "+
			"to both provider values, and `add --lookup-only` is the only thing that creates such a "+
			"row", rel)
	}
	widened := regexp.MustCompile(`(?is)provider\s+in\s*\(\s*'jira'\s*,\s*'jira_lookup'\s*\)`)
	if !widened.MatchString(src) {
		t.Errorf("%s does not select `provider IN ('jira','jira_lookup')`. Cost of D17, stated in the "+
			"SPEC: anything that means 'all Jira accounts' must now name both values — today that is "+
			"exactly one query, this one", rel)
	}
	if !strings.Contains(src, "lookup-only") {
		t.Errorf("%s declares no --lookup-only flag on `add`. Criterion 13: the existing add path, one "+
			"flag — same site URL, same pgcrypto encryption, same Myself verification before anything "+
			"is stored", rel)
	}
	// --projects stays REQUIRED for a lookup account, and the message says why.
	if !strings.Contains(strings.ToLower(src), "any issue") {
		t.Errorf("%s's refusal of an unscoped lookup account does not say that it could otherwise "+
			"fetch ANY issue on the site (criterion 13). The scoping IS the SSRF boundary (D18); a "+
			"refusal that reads like a missing-argument nag invites `--projects '*'`", rel)
	}
}

// ---- criterion 47: the runbook ----------------------------------------------

// A test on prose earns its place the same way TestRunbook_DocumentsCaptureBefore
// Triage does: nothing in the code can stop an operator arming the wrong
// project's gate, or re-running the pass with --force in a loop. Each fact below
// is one the SPEC names explicitly.
func TestRunbook_DocumentsTheReconciler(t *testing.T) {
	doc := strings.ToLower(tsRepoFile(t, "docs/runbooks/ticket-status-sync.md"))

	for _, want := range []struct{ frag, why string }{
		{"statuscategory", "the discriminator, and why not a list of status names (D2)"},
		{"ticket_assignee_gate", "what arms the gate — the hand-run UPDATE, per project"},
		{"own_account_id", "what 'me' means: an accountId, per source account, not an email (D12)"},
		{"unassigned", "that unassigned counts as not-mine (D14)"},
		{"jira_lookup", "the account shape the lookup half needs (D17)"},
		{"ticket_lookup_ttl", "the freshness bound and its default"},
		{"--force", "how to bypass the TTL for a smoke"},
		{"task_dismissals", "the dismissal suppression: a dismissed task never resurfaces (D4)"},
		{"opsctl ticket-status", "the one-off reconciliation command itself"},
	} {
		if !strings.Contains(doc, want.frag) {
			t.Errorf("docs/runbooks/ticket-status-sync.md never mentions %q — criterion 47 asks for %s",
				want.frag, want.why)
		}
	}
	// The accepted limit, stated where an operator will read it. It is the one
	// thing about this design that will surprise someone: an Avviato ticket
	// assigned to Salvador that never produced a Slack message or an email never
	// becomes a task, because nothing ever made it a candidate.
	if !strings.Contains(doc, "candidate") {
		t.Errorf("the runbook never explains the candidate-driven lookup. Criterion 47: the accepted " +
			"limit (an unnotified ticket is never a candidate) is what stops the next reader treating " +
			"a missing task as a bug and 'fixing' it with a project-wide poll — the alternative Q1 " +
			"rejected")
	}
}
