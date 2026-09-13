package tools_test

// Structural tests for SWT-43 (docs/tickets/delivery-deny_SPEC.md):
//
//   - criterion 1: migrations/0028_delivery_rejection.sql's four clauses,
//     asserted by clause (the 0027 guard's shape,
//     internal/connector/slackweb/migration0027_structure_test.go);
//   - criterion 8: rejectDelivery takes the task row SHARE lock through a
//     helper refuseClosedTask ALSO calls (one spelling of the lock), then
//     FOR UPDATE on the delivery, and does not call refuseClosedTask itself
//     (a plain reject on a closed task is allowed);
//   - criterion 27: the loop bound. Nothing writes redraft_requested_at
//     except internal/tools; internal/drafts and internal/dashboard may read it.
//
// ZERO I/O beyond reading this repo's own files. Reuses dsRepoFile
// (dismiss_structure_test.go) and drSQLCode (dismissal_reopen_structure_test.go).
//
// IMPOSED SURFACE (SPEC "Data model changes", verbatim):
//
//	ALTER TABLE deliveries DROP CONSTRAINT deliveries_status_check;
//	ALTER TABLE deliveries ADD CONSTRAINT deliveries_status_check CHECK (status IN
//	  ('drafted','approved','sending','sent','failed','rejected'));
//	ALTER TABLE deliveries
//	  ADD COLUMN rejection_note       TEXT,
//	  ADD COLUMN redraft_requested_at TIMESTAMPTZ;
//	ALTER TABLE deliveries ADD CONSTRAINT deliveries_rejected_unsent_check
//	  CHECK (status <> 'rejected' OR (sent_external_id IS NULL AND confirmed_at IS NULL));
//	ALTER TABLE deliveries ADD CONSTRAINT deliveries_rejection_fields_check
//	  CHECK ((redraft_requested_at IS NULL AND rejection_note IS NULL) OR status = 'rejected');
//
// COLLISION RULE (criterion 3): SWT-40 also names 0028. Whichever branch
// merges SECOND renumbers its file, this guard and the classify ledger line.
//
// GREENFIELD NOTE — EXPECTED RED: no 0028 file, no rejectDelivery, and no
// writer of redraft_requested_at exists yet (the writer scan's positive
// control fails).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/tools"
)

// ---- criterion 1: migration 0028 ---------------------------------------------

func TestMigration0028_DeliveryRejectionShape(t *testing.T) {
	// Control first: 0027 must still be there, or an empty glob below would
	// prove only that the directory moved.
	if _, err := os.Stat(filepath.Join("..", "..", "migrations", "0027_sync_runs_partial.sql")); err != nil {
		t.Fatalf("migrations/0027_sync_runs_partial.sql is missing: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join("..", "..", "migrations", "0028_*.sql"))
	if err != nil {
		t.Fatalf("glob migrations/0028_*.sql: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("found %d migrations/0028_*.sql file(s), want exactly 1 (the SPEC names "+
			"0028_delivery_rejection.sql; 0027 is the current highest). deliveries.status is CHECK "+
			"(status IN ('drafted','approved','sending','sent','failed')) since 0001, so no row can be "+
			"'rejected' until it is widened", len(matches))
	}
	if base := filepath.Base(matches[0]); base != "0028_delivery_rejection.sql" {
		t.Errorf("migration 0028 is named %s, want 0028_delivery_rejection.sql (the SPEC's name)", base)
	}
	rel := filepath.Join("migrations", filepath.Base(matches[0]))
	code, _ := drSQLCode(strings.ToLower(dsRepoFile(t, rel)))
	sql := regexp.MustCompile(`\s+`).ReplaceAllString(code, " ")

	if !regexp.MustCompile(`alter table (if exists )?(public\.)?deliveries\b`).MatchString(sql) {
		t.Errorf("%s never alters deliveries", rel)
	}

	// Clause 1: the status CHECK, dropped and re-added under the SAME name.
	if !regexp.MustCompile(`drop constraint (if exists )?deliveries_status_check\b`).MatchString(sql) {
		t.Errorf("%s does not DROP CONSTRAINT deliveries_status_check, the name Postgres gave 0001's inline "+
			"CHECK. Adding a second CHECK beside it leaves 'rejected' refused by the first", rel)
	}
	add := regexp.MustCompile(`add constraint deliveries_status_check check ?\( ?status in ?\(([^)]*)\) ?\)`).
		FindStringSubmatch(sql)
	if add == nil {
		t.Errorf("%s does not ADD CONSTRAINT deliveries_status_check CHECK (status IN (...)). Dropping the "+
			"CHECK without re-adding it admits any status string", rel)
	} else {
		var values []string
		for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(add[1], -1) {
			values = append(values, m[1])
		}
		sort.Strings(values)
		want := []string{"approved", "drafted", "failed", "rejected", "sending", "sent"}
		if strings.Join(values, ",") != strings.Join(want, ",") {
			t.Errorf("%s's new status CHECK admits %v, want exactly %v: every one of the five existing "+
				"values survives, plus 'rejected' (D1: the spelling approvals.status and plan_imports "+
				"already use — not 'denied', which is the POLICY verb)", rel, values, want)
		}
	}

	// Clause 2: the two columns.
	if !regexp.MustCompile(`add column (if not exists )?rejection_note text\b`).MatchString(sql) {
		t.Errorf("%s does not ADD COLUMN rejection_note TEXT (D8: one optional free-text note, typed, and "+
			"the thing the redraft prompt consumes)", rel)
	}
	if !regexp.MustCompile(`add column (if not exists )?redraft_requested_at timestamptz\b`).MatchString(sql) {
		t.Errorf("%s does not ADD COLUMN redraft_requested_at TIMESTAMPTZ (D3: the Redo intent is a column "+
			"on the rejected row — the drafts unblock)", rel)
	}

	// Clause 3: the invariant-4 backstop.
	if !regexp.MustCompile(`add constraint deliveries_rejected_unsent_check check ?\( ?status ?(<>|!=) ?'rejected' ` +
		`or ?\( ?sent_external_id is null and confirmed_at is null ?\) ?\)`).MatchString(sql) {
		t.Errorf("%s does not add deliveries_rejected_unsent_check as CHECK (status <> 'rejected' OR "+
			"(sent_external_id IS NULL AND confirmed_at IS NULL)). It is criterion 17's backstop: a matcher "+
			"that ever widens its status set to 'rejected' must fail at the database, not stamp the row", rel)
	}

	// Clause 4: the rejection fields live only on rejected rows.
	if !regexp.MustCompile(`add constraint deliveries_rejection_fields_check check ?\( ?\( ?redraft_requested_at ` +
		`is null and rejection_note is null ?\) ?or ?status ?= ?'rejected' ?\)`).MatchString(sql) {
		t.Errorf("%s does not add deliveries_rejection_fields_check as CHECK ((redraft_requested_at IS NULL "+
			"AND rejection_note IS NULL) OR status = 'rejected'). Without it a stray redraft flag on a "+
			"drafted row would unblock the drafts worker for work nobody rejected", rel)
	}

	// "No index" (Data model changes): deliveries_status_idx (0006) already
	// serves status reads, and the drafts NOT EXISTS is per parent task.
	if strings.Contains(sql, "create index") || strings.Contains(sql, "create unique index") {
		t.Errorf("%s creates an index; the SPEC's data model says no index (deliveries_status_idx already "+
			"serves status reads)", rel)
	}
}

// ---- criterion 8: one spelling of the task SHARE lock ------------------------

// rjFuncSource returns fd's source text.
func rjFuncSource(src []byte, fset *token.FileSet, fd *ast.FuncDecl) string {
	return string(src[fset.Position(fd.Pos()).Offset:fset.Position(fd.End()).Offset])
}

// rjLocalCallees returns the package-local functions fd calls by bare
// identifier (helper(…)), minus builtins and the generic tx/marshal helpers
// every handler uses — what is left is what the two functions could share.
func rjLocalCallees(fd *ast.FuncDecl) map[string]bool {
	generic := map[string]bool{
		"len": true, "append": true, "make": true, "new": true, "cap": true, "copy": true, "panic": true,
		"inTx": true, "marshalResult": true, "insertTaskEvent": true,
	}
	out := map[string]bool{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && !generic[id.Name] {
				out[id.Name] = true
			}
		}
		return true
	})
	return out
}

func TestRejectDelivery_SharesTheTaskLockHelperWithRefuseClosedTask(t *testing.T) {
	fset := token.NewFileSet()
	src, err := os.ReadFile("delivery.go")
	if err != nil {
		t.Fatalf("read delivery.go: %v", err)
	}
	f, err := parser.ParseFile(fset, "delivery.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse delivery.go: %v", err)
	}
	funcs := map[string]*ast.FuncDecl{}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil && fd.Recv == nil {
			funcs[fd.Name.Name] = fd
		}
	}
	reject, ok := funcs["rejectDelivery"]
	if !ok {
		t.Fatalf("delivery.go declares no rejectDelivery. SPEC \"API / MCP tool changes\": the handler is " +
			"rejectDelivery in internal/tools/delivery.go, next to approveDelivery")
	}
	refuse, ok := funcs["refuseClosedTask"]
	if !ok {
		t.Fatalf("delivery.go no longer declares refuseClosedTask; send_guard_structure_test.go depends on it")
	}

	rejectCalls, refuseCalls := rjLocalCallees(reject), rjLocalCallees(refuse)
	if rejectCalls["refuseClosedTask"] {
		t.Errorf("rejectDelivery calls refuseClosedTask, which REFUSES a closed task. Criterion 8: a plain " +
			"reject on a closed task is allowed — cleaning up a stale draft is exactly what closed work needs. " +
			"Share the lock helper, not the refusal")
	}
	var shared []string
	for name := range rejectCalls {
		if refuseCalls[name] {
			shared = append(shared, name)
		}
	}
	sort.Strings(shared)
	if len(shared) == 0 {
		t.Fatalf("rejectDelivery and refuseClosedTask share no helper (rejectDelivery calls %v; "+
			"refuseClosedTask calls %v). Criterion 8: the delivery's task row SHARE lock (the "+
			"refuseClosedTask query shape) is factored into ONE helper both call, so the lock order "+
			"task -> delivery has one spelling", sortedKeys(rejectCalls), sortedKeys(refuseCalls))
	}
	helperLocks := false
	for _, name := range shared {
		if fd, ok := funcs[name]; ok && strings.Contains(rjFuncSource(src, fset, fd), "FOR SHARE") {
			helperLocks = true
		}
	}
	if !helperLocks {
		t.Errorf("no helper shared by rejectDelivery and refuseClosedTask (%v) takes FOR SHARE. The task lock "+
			"is SHARE, not UPDATE (refuseClosedTask's own rationale: FOR UPDATE would deadlock with the "+
			"delivery -> task paths)", shared)
	}
	if !strings.Contains(rjFuncSource(src, fset, reject), "FOR UPDATE") {
		t.Errorf("rejectDelivery takes no FOR UPDATE. Criterion 8: after the task SHARE lock it locks the " +
			"delivery row FOR UPDATE, which is what serialises a reject against send_delivery (criterion 14)")
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- criterion 27: the loop bound --------------------------------------------

// A redraft is a model call. One human click per re-draft is the ONLY thing
// bounding the loop (D3), and that holds only while the sole writer of
// redraft_requested_at is the humanOnly reject_delivery handler. A spine rule,
// a connector or the drafts worker writing it would turn Redo into an
// unbounded drafting loop with no human in it.
//
// redraftWrite recognises a WRITE of the column: an assignment with ANY
// right-hand side (`col = now()`, `col=$2`, `col = CASE ...`, `col = NULL`, a
// literal, a function), the multi-column `SET (..., col) = (...)` form, or an
// INSERT INTO deliveries naming it. `col >= x`, `col <= x`, `IS [NOT] NULL` and
// ORDER BY do not match. SWT-43 review (go-reviewer): the first cut listed
// right-hand sides one by one and missed CASE, the form rejectDelivery's own
// Redo UPDATE uses, so a CASE writer anywhere else passed the scan. An equality
// COMPARISON (`WHERE col = $1`) is flagged too; that is the safe direction.
var redraftWrite = regexp.MustCompile(`(?is)redraft_requested_at\s*=[^=]` +
	`|redraft_requested_at\s*\)\s*=` +
	`|insert\s+into\s+deliveries[^;` + "`" + `]*redraft_requested_at`)

// The probe (positive and negative controls on the pattern itself). MUTATION:
// restore the first cut's right-hand-side list (no CASE) → the CASE rows go red.
func TestRedraftWritePattern_Probe(t *testing.T) {
	for _, s := range []string{
		`UPDATE deliveries SET redraft_requested_at=now() WHERE id=$1`,
		`UPDATE deliveries SET redraft_requested_at = CASE WHEN $3::boolean THEN now() END`,
		"UPDATE deliveries SET status='rejected',\n\t\tredraft_requested_at=CASE WHEN $3 THEN now() END",
		`SET redraft_requested_at=$2`,
		`SET redraft_requested_at = NULL`,
		`SET redraft_requested_at = COALESCE(redraft_requested_at, now())`,
		`SET redraft_requested_at = clock_timestamp()`,
		`SET redraft_requested_at = '2026-09-12T00:00:00Z'`,
		`ON CONFLICT (id) DO UPDATE SET redraft_requested_at = EXCLUDED.redraft_requested_at`,
		`UPDATE deliveries SET (status, redraft_requested_at) = ('rejected', now())`,
		"INSERT INTO deliveries (task_id, status, redraft_requested_at)\n VALUES ($1, 'rejected', now())",
	} {
		if !redraftWrite.MatchString(s) {
			t.Errorf("redraftWrite misses a WRITE: %q", s)
		}
	}
	for _, s := range []string{
		`AND NOT (d.status = 'rejected' AND d.redraft_requested_at IS NOT NULL)`,
		`SELECT d.redraft_requested_at IS NOT NULL FROM deliveries d`,
		`WHERE redraft_requested_at IS NULL`,
		`WHERE redraft_requested_at >= now() - interval '1 day'`,
		`WHERE redraft_requested_at <= now()`,
		`ORDER BY d.redraft_requested_at DESC`,
	} {
		if redraftWrite.MatchString(s) {
			t.Errorf("redraftWrite flags a READ as a write: %q", s)
		}
	}
}

func TestRedraftRequestedAt_OnlyInternalToolsWritesIt(t *testing.T) {
	root := filepath.Join("..") // internal/
	readers := map[string]bool{"drafts": true, "dashboard": true}

	writerInTools := false
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(raw)
		if !strings.Contains(src, "redraft_requested_at") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		top := strings.Split(filepath.ToSlash(rel), "/")[0]
		switch {
		case top == "tools":
			if redraftWrite.MatchString(src) {
				writerInTools = true
			}
		case readers[top]:
			if loc := redraftWrite.FindStringIndex(src); loc != nil {
				t.Errorf("internal/%s WRITES redraft_requested_at (%q). internal/%s may read it (criterion "+
					"27 allows readers by name); the only writer is reject_delivery, which is humanOnly — "+
					"that is the loop bound", rel, src[loc[0]:loc[1]], top)
			}
		default:
			t.Errorf("internal/%s mentions redraft_requested_at. Criterion 27: nothing outside internal/tools "+
				"may write it, and only internal/drafts and internal/dashboard are allowed to read it", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}

	// cmd/ (SWT-43 review): a binary writes through the executor, never SQL of
	// its own, so no main may write the column either.
	cmdRoot := filepath.Join("..", "..", "cmd")
	err = filepath.WalkDir(cmdRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if loc := redraftWrite.FindStringIndex(string(raw)); loc != nil {
			rel, _ := filepath.Rel(cmdRoot, path)
			t.Errorf("cmd/%s WRITES redraft_requested_at (%q). Criterion 27: the only writer is reject_delivery "+
				"in internal/tools, which is humanOnly — that is the loop bound", rel, string(raw)[loc[0]:loc[1]])
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk cmd/: %v", err)
	}

	if !writerInTools {
		t.Fatalf("POSITIVE CONTROL FAILED: no non-test file in internal/tools writes redraft_requested_at. " +
			"reject_delivery's Redo (and the D6 upgrade) set it; a scan that finds no writer at all cannot " +
			"tell 'one writer' from 'the pattern is wrong'")
	}
	// Positive control on the real code, per writer: rejectDelivery's Redo
	// UPDATE (`redraft_requested_at=CASE ...`) and the D6 upgrade
	// (`redraft_requested_at=now()`). The first cut's pattern found only the
	// second, so it could not see the CASE shape at all.
	if got := redraftWrite.FindAllString(dsRepoFile(t, "internal/tools/delivery.go"), -1); len(got) != 2 {
		t.Errorf("redraftWrite finds %d writer(s) in internal/tools/delivery.go (%q), want exactly 2: the Redo "+
			"UPDATE (CASE) and the D6 upgrade (now()). A third is a new writer the loop bound must account for",
			len(got), got)
	}
}

// ---- SWT-43 review fix 2: one spelling of the blocking predicate ------------

// The drafts queue (drafts.DeliverTasks' NOT EXISTS) and draftDelivery's
// re-check under the task lock must agree on which rows block a new draft. Two
// spellings would drift: a queue that lists a task the re-check then refuses
// every pass, or a re-check that lets a second draft in.
func TestBlockingDeliverySQL_OneSpellingSharedByQueueAndDraft(t *testing.T) {
	for _, want := range []string{"NOT", "d.status = 'rejected'", "d.redraft_requested_at IS NOT NULL"} {
		if !strings.Contains(tools.BlockingDeliverySQL, want) {
			t.Errorf("tools.BlockingDeliverySQL = %q lacks %q", tools.BlockingDeliverySQL, want)
		}
	}
	store := dsRepoFile(t, "internal/drafts/store.go")
	if !strings.Contains(store, "tools.BlockingDeliverySQL") {
		t.Errorf("internal/drafts/store.go does not use tools.BlockingDeliverySQL in DeliverTasks' NOT EXISTS")
	}
	if strings.Contains(store, "AND NOT (d.status = 'rejected' AND d.redraft_requested_at IS NOT NULL)") {
		t.Errorf("internal/drafts/store.go still spells the blocking predicate itself; use tools.BlockingDeliverySQL")
	}

	fset := token.NewFileSet()
	src, err := os.ReadFile("delivery.go")
	if err != nil {
		t.Fatalf("read delivery.go: %v", err)
	}
	f, err := parser.ParseFile(fset, "delivery.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse delivery.go: %v", err)
	}
	funcs := map[string]*ast.FuncDecl{}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil && fd.Recv == nil {
			funcs[fd.Name.Name] = fd
		}
	}
	draft, helper := funcs["draftDelivery"], funcs["refuseBlockingDelivery"]
	if draft == nil || helper == nil {
		t.Fatalf("delivery.go must declare draftDelivery and refuseBlockingDelivery (the re-check under the task lock)")
	}
	if !rjLocalCallees(draft)["refuseBlockingDelivery"] {
		t.Errorf("draftDelivery does not call refuseBlockingDelivery")
	}
	if !strings.Contains(rjFuncSource(src, fset, helper), "BlockingDeliverySQL") {
		t.Errorf("refuseBlockingDelivery does not use BlockingDeliverySQL")
	}
}
