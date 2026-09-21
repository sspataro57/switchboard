package tools_test

// SWT-52 (docs/tickets/board-status-lights_SPEC.md) criteria 17 and 19: the
// migration's shape, and ONLY internal/tools/signal.go and close.go write
// tasks.working_state / working_state_at. ZERO I/O beyond this repo's files.
//
// HOW THE WRITER SCAN SCANS: Go STRING LITERALS only (go/ast BasicLit), with a
// `+` chain of literals joined into one text, across the non-test .go files of
// internal/ and cmd/. A comment that mentions a column is not a write
// (tasklist_structure_test.go's rule). The pattern (sigWrite) keys on a SET
// context, an assignment inside a multi-column SET, or an INSERT INTO tasks
// naming the column, so the dashboard's READ `t.working_state = 'working'` in a
// SELECT list (criterion 5's staleness expression) is not a write; an equality
// in the WHERE of an UPDATE … SET is flagged, which is the safe direction.
//
// GREENFIELD NOTE — EXPECTED RED: migrations/0033_task_working_state.sql and
// internal/tools/signal.go do not exist, and closeTransition does not clear the
// columns, so the guard and both positive controls fail.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func sigRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// ---- criterion 17: migration 0033 ------------------------------------------------

func TestMigration0033_TaskWorkingStateShape(t *testing.T) {
	dir := filepath.Join("..", "..", "migrations")
	if _, err := os.Stat(filepath.Join(dir, "0032_route_tier.sql")); err != nil {
		t.Fatalf("control: migrations/0032_route_tier.sql is missing: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations/: %v", err)
	}
	var n33 []string
	for _, e := range entries {
		m := regexp.MustCompile(`^(\d{4})_.*\.sql$`).FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, _ := strconv.Atoi(m[1])
		if v == 33 {
			n33 = append(n33, e.Name())
		}
		// AMENDED by chat-on-closed-task (SWT-53) and treetop-pr-review-tasks
		// (SWT-54), never deleted: 0034 is SWT-53's (projects.notifier_senders,
		// capture_decisions.resurface) and 0035 is SWT-54's (capture_rules.pr_review
		// / exclude_pr_authors, guarded by internal/capture
		// TestMigration0035_CaptureRulesPRReviewShape). A number above 33 that
		// another ticket owns is accounted for by the living ledger in
		// internal/classify/structure_test.go; this guard exempts it by number, and
		// any other number above 33 is still flagged here.
		// AMENDED — not deleted — by signal-session-name (SWT-56) criterion 14: 0036
		// is its tasks.working_session, guarded by TestMigration0036_TaskWorkingSessionShape.
		// AMENDED — not deleted — by microsoft-oauth-mail (SWT-66): 0037 widens
		// source_accounts.auth_type to include 'xoauth2', guarded by
		// TestMigration0037_Integration_AppliesTwiceAndConstrainsAuthType.
		// AMENDED — not deleted — by gmail-delivery-cc (SWT-69): 0038 adds
		// deliveries.cc and its two CHECKs, guarded by
		// TestMigration0038_CcColumnAndChecks and
		// TestMigration0038_Integration_DeliveryCcShape.
		if v > 33 && v != 34 && v != 35 && v != 36 && v != 37 && v != 38 {
			t.Errorf("migrations/%s exists: criterion 17 — 0033 is the only migration this ticket adds and none above it exists except a number another ticket owns (34: chat-on-closed-task, 35: treetop-pr-review-tasks, 36: signal-session-name, 37: microsoft-oauth-mail, 38: gmail-delivery-cc)", e.Name())
		}
	}
	if len(n33) != 1 || n33[0] != "0033_task_working_state.sql" {
		t.Fatalf("migrations/0033_*.sql = %v, want exactly [0033_task_working_state.sql] (criterion 17)", n33)
	}
	var code []string
	for _, line := range strings.Split(sigRepoFile(t, "migrations/0033_task_working_state.sql"), "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		code = append(code, line)
	}
	sql := strings.ToLower(regexp.MustCompile(`\s+`).ReplaceAllString(strings.Join(code, " "), " "))
	sql = strings.ReplaceAll(strings.ReplaceAll(sql, "( ", "("), " )", ")")
	sql = regexp.MustCompile(`\s*,\s*`).ReplaceAllString(sql, ",")
	for _, want := range []struct{ re, why string }{
		{`alter table (public\.)?tasks\b`, "the two columns go on the one tasks table (invariant 2)"},
		{`add column working_state text\b`, "the state, nullable TEXT (D6)"},
		{`add column working_state_at timestamptz\b`, "the last signal's time, nullable (D6)"},
		{`add constraint tasks_working_state_check check \(working_state in \('working','needs_input'\)\)`,
			"the NAMED state CHECK, so a later ticket can widen it by name (D14)"},
		{`add constraint tasks_working_state_pair check \(\(working_state is null\) = \(working_state_at is null\)\)`,
			"the NAMED pair CHECK: both NULL together (D6)"},
	} {
		if !regexp.MustCompile(want.re).MatchString(sql) {
			t.Errorf("0033 does not match /%s/ — %s", want.re, want.why)
		}
	}
	for _, banned := range []struct{ re, why string }{
		{`\bdefault\b`, "no default: fixtures that INSERT tasks get NULL = no signal"},
		{`not null`, "both columns are nullable"},
		{`create (unique )?index`, "no index: read by primary key and over ready tasks only"},
		{`\bupdate\b`, "no backfill"},
		{`feedback_requests`, "answer recording is deferred (D14)"},
	} {
		if regexp.MustCompile(banned.re).MatchString(sql) {
			t.Errorf("0033 matches /%s/ — %s", banned.re, banned.why)
		}
	}
}

// ---- criterion 19: the only writers -----------------------------------------------

// AMENDED — not deleted — by SWT-56 (signal-session-name) criterion 12: the
// pattern covers working_session in every shape it covers for the other two
// columns: SET … =, a parenthesized SET (…) =, and INSERT INTO tasks (…).
var sigWrite = regexp.MustCompile(`(?is)\bset\b[^;]*?\bworking_(state(_at)?|session)\s*=[^=]` +
	`|\bset\s*\([^)]*\bworking_(state(_at)?|session)\b[^)]*\)\s*=` +
	`|insert\s+into\s+tasks\s*\([^)]*\bworking_(state|session)`)

// sessWrite / sessClear: the SWT-56 positive controls' patterns.
var (
	sessWrite = regexp.MustCompile(`(?is)\bset\b[^;]*?\bworking_session\s*=[^=]`)
	sessClear = regexp.MustCompile(`(?i)\bworking_session\s*=\s*NULL\b`)
)

func TestWorkingStateWritePattern_Probe(t *testing.T) {
	for _, s := range []string{
		`UPDATE tasks SET working_state=$2, working_state_at=now() WHERE id=$1`,
		`UPDATE tasks SET working_state = CASE WHEN $2 = 'clear' THEN NULL ELSE $2 END WHERE id=$1`,
		"UPDATE tasks SET status=$2, updated_at=now(), closed_at=now(), closed_from_status=$3,\n\t working_state = NULL, working_state_at = NULL WHERE id=$1",
		`UPDATE tasks SET working_state_at = now() WHERE id=$1`,
		`SET working_state_at=NULL`,
		`UPDATE tasks SET (working_state, working_state_at) = ('working', now()) WHERE id=$1`,
		"INSERT INTO tasks (project_id, title, working_state, working_state_at)\n VALUES ($1,$2,'working',now())",
		`ON CONFLICT (id) DO UPDATE SET working_state_at = EXCLUDED.working_state_at`,
		`WITH x AS (UPDATE tasks SET priority = 1, working_state = 'working' WHERE id = $1 RETURNING id) SELECT 1`,
		// SWT-56 criterion 12: working_session in each write shape.
		`UPDATE tasks SET working_state = $2, working_state_at = now(), working_session = $3 WHERE id = $1`,
		`UPDATE tasks SET working_session = NULL WHERE id=$1`,
		`UPDATE tasks SET (working_session) = ('kube-c7') WHERE id=$1`,
		"INSERT INTO tasks (project_id, title, working_session)\n VALUES ($1,$2,'kube-c7')",
	} {
		if !sigWrite.MatchString(s) {
			t.Errorf("sigWrite misses a WRITE: %q", s)
		}
	}
	for _, s := range []string{
		`SELECT t.id, COALESCE(t.working_state,''), to_char(t.working_state_at AT TIME ZONE $1, 'YYYY-MM-DD HH24:MI') FROM tasks t`,
		`SELECT t.working_state = 'working' AND t.working_state_at < now() - make_interval(secs => $2) FROM tasks t`,
		`SELECT id FROM tasks WHERE working_state IS NOT NULL ORDER BY working_state_at DESC`,
		`SELECT id FROM tasks t WHERE t.working_state = 'needs_input' OFFSET 0`,
		`SELECT working_state, working_state_at FROM tasks WHERE id=$1 FOR UPDATE`,
		// SWT-56 criterion 12 / S13: the gated read in task_context's SELECT list and
		// the board's COALESCE are READS.
		`SELECT t.id, COALESCE(CASE WHEN t.working_state IS NOT NULL THEN t.working_session END, '') FROM tasks t WHERE t.id = $1`,
		`SELECT COALESCE(t.working_session, '') AS session FROM tasks t WHERE t.id = ANY($1)`,
		`SELECT COALESCE(working_state,''), COALESCE(working_session,'') FROM tasks WHERE id=$1 FOR UPDATE`,
	} {
		if sigWrite.MatchString(s) {
			t.Errorf("sigWrite flags a READ as a write: %q", s)
		}
	}
}

// literalTexts returns the string-literal texts of one Go file, each `+` chain
// of literals joined (a non-literal operand becomes a placeholder).
func literalTexts(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	consumed := map[*ast.BasicLit]bool{}
	var flatten func(e ast.Expr) string
	flatten = func(e ast.Expr) string {
		switch e := e.(type) {
		case *ast.BinaryExpr:
			if e.Op == token.ADD {
				return flatten(e.X) + flatten(e.Y)
			}
		case *ast.ParenExpr:
			return flatten(e.X)
		case *ast.BasicLit:
			if e.Kind == token.STRING {
				consumed[e] = true
				if s, err := strconv.Unquote(e.Value); err == nil {
					return s
				}
				return e.Value
			}
		}
		return " ? "
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BinaryExpr:
			if n.Op == token.ADD {
				out = append(out, flatten(n))
				return false
			}
		case *ast.BasicLit:
			if n.Kind == token.STRING && !consumed[n] {
				if s, err := strconv.Unquote(n.Value); err == nil {
					out = append(out, s)
				}
			}
		}
		return true
	})
	return out
}

// RENAMED by SWT-56 (signal-session-name) criterion 12 from
// TestWorkingState_OnlySignalAndCloseWriteIt, a name already stale: the
// allow-list has held claim.go since the SWT-52 D9 amendment. The scan now also
// covers working_session (sigWrite, amended below the migration guard).
func TestWorkingState_OnlySignalCloseAndClaimWriteIt(t *testing.T) {
	// The allow-list, extended DELIBERATELY by the SWT-52 D9 amendment
	// (2026-09-14): task_signal (signal.go) sets and clears; closeTransition
	// (close.go) clears on a real close AND on every reopen — the reopen clear is
	// a second writing literal in close.go (Codex: an old binary's close leaves
	// the marker, and a reopen would resurrect it); task_claim (claim.go) clears
	// on a claim (go-reviewer: a marker survived a claim/release cycle). Each
	// entry carries a minimum writer-literal count, so dropping one of the clears
	// fails the positive controls below as well as the integration tests.
	allowed := map[string]bool{
		"internal/tools/signal.go": true,
		"internal/tools/close.go":  true,
		"internal/tools/claim.go":  true,
	}
	writers := map[string]int{}
	for _, root := range []string{"internal", "cmd"} {
		base := filepath.Join("..", "..", root)
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(filepath.Join("..", ".."), path)
			rel = filepath.ToSlash(rel)
			for _, lit := range literalTexts(t, path) {
				if m := sigWrite.FindString(lit); m != "" {
					writers[rel]++
					if !allowed[rel] {
						t.Errorf("%s WRITES working_state/working_state_at (%q). Criterion 19: only task_signal "+
							"(internal/tools/signal.go), closeTransition (internal/tools/close.go) and task_claim "+
							"(internal/tools/claim.go) write them; the dashboard only READS for the lights (invariant 3)", rel, m)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	var seen []string
	for f := range writers {
		seen = append(seen, f)
	}
	sort.Strings(seen)
	if writers["internal/tools/signal.go"] == 0 {
		t.Errorf("POSITIVE CONTROL FAILED: internal/tools/signal.go writes no working_state (writers seen: %v). "+
			"task_signal sets and clears it (D10); a scan that finds no writer cannot tell 'one writer' from "+
			"'the pattern is wrong'", seen)
	}
	if n := writers["internal/tools/close.go"]; n < 2 {
		t.Errorf("POSITIVE CONTROL FAILED: internal/tools/close.go has %d working_state-writing literal(s), want 2. "+
			"D9 and its 2026-09-14 amendment: closeTransition clears both columns on a real close AND in the reopen "+
			"UPDATE (writers seen: %v)", n, seen)
	}
	if writers["internal/tools/claim.go"] == 0 {
		t.Errorf("POSITIVE CONTROL FAILED: internal/tools/claim.go writes no working_state. D9 amendment "+
			"(2026-09-14): task_claim's ready → claimed UPDATE clears both columns (writers seen: %v)", seen)
	}
	// SWT-56 (signal-session-name) criterion 12: working_session travels with the
	// marker, so each allowed file writes it too, with the same minimum counts.
	sess := map[string]int{}
	for _, f := range []string{"internal/tools/signal.go", "internal/tools/close.go", "internal/tools/claim.go"} {
		for _, lit := range literalTexts(t, filepath.Join("..", "..", f)) {
			if sessWrite.MatchString(lit) {
				sess[f]++
			}
			if sessClear.MatchString(lit) {
				sess[f+" clear"]++
			}
		}
	}
	if n := sess["internal/tools/signal.go"]; n < 2 {
		t.Errorf("POSITIVE CONTROL FAILED: signal.go writes working_session in %d literal(s), want >= 2: the set "+
			"(working_session = $3) and the clear (working_session = NULL), S7", n)
	}
	if n := sess["internal/tools/close.go clear"]; n < 2 {
		t.Errorf("POSITIVE CONTROL FAILED: close.go has %d literal(s) clearing working_session, want >= 2: the real "+
			"close and the reopen UPDATE (S7)", n)
	}
	if n := sess["internal/tools/claim.go clear"]; n < 1 {
		t.Errorf("POSITIVE CONTROL FAILED: claim.go has %d literal(s) clearing working_session, want >= 1: the "+
			"ready → claimed UPDATE (S7)", n)
	}
}

// ---- SWT-56 criterion 13: the session name travels with the marker ---------------

var (
	wsNull     = regexp.MustCompile(`(?i)\bworking_state\s*=\s*NULL\b`)
	wsSet      = regexp.MustCompile(`(?i)\bworking_state\s*=\s*([^\s,)]+)`)
	wsParenSet = regexp.MustCompile(`(?is)\bset\s*\(([^)]*\bworking_state\b[^)]*)\)\s*=`)
	wsInsert   = regexp.MustCompile(`(?is)insert\s+into\s+tasks\s*\(([^)]*\bworking_state\b[^)]*)\)`)
	wssAssign  = regexp.MustCompile(`(?i)\bworking_session\s*=`)
)

// travelViolations names every way one write literal moves working_state
// without working_session (S7): a clear that leaves the name, or a set (plain,
// parenthesized or INSERT) that omits it. Reads are not writes (sigWrite gate).
func travelViolations(lit string) []string {
	if !sigWrite.MatchString(lit) {
		return nil
	}
	var out []string
	if wsNull.MatchString(lit) && !sessClear.MatchString(lit) {
		out = append(out, "NULLs working_state but not working_session")
	}
	for _, m := range wsSet.FindAllStringSubmatch(lit, -1) {
		if !strings.EqualFold(m[1], "NULL") && !wssAssign.MatchString(lit) {
			out = append(out, "sets working_state to "+m[1]+" without setting working_session")
		}
	}
	for _, re := range []*regexp.Regexp{wsParenSet, wsInsert} {
		for _, m := range re.FindAllStringSubmatch(lit, -1) {
			if !strings.Contains(m[1], "working_session") {
				out = append(out, "names working_state in a column list without working_session: ("+m[1]+")")
			}
		}
	}
	return out
}

func TestWorkingSession_TravelsWithWorkingState_Probe(t *testing.T) {
	for _, s := range []string{
		// a four-column clear missing the name
		"UPDATE tasks SET status=$2, updated_at=now(), working_state = NULL, working_state_at = NULL WHERE id=$1",
		// a set missing the name
		`UPDATE tasks SET working_state = $2, working_state_at = now() WHERE id = $1`,
		`UPDATE tasks SET (working_state, working_state_at) = ('working', now()) WHERE id=$1`,
		"INSERT INTO tasks (project_id, title, working_state, working_state_at)\n VALUES ($1,$2,'working',now())",
	} {
		if len(travelViolations(s)) == 0 {
			t.Errorf("the travels-with rule does not bite on %q (criterion 13)", s)
		}
	}
	// The real statement shapes after S7: signal's clear and set, close, reopen, claim.
	for _, s := range []string{
		`UPDATE tasks SET working_state = NULL, working_state_at = NULL, working_session = NULL WHERE id=$1`,
		"UPDATE tasks\n   SET working_state = $2, working_state_at = now(), working_session = $3\n WHERE id = $1\n RETURNING working_state_at::text",
		"UPDATE tasks SET status=$2, updated_at=now(), closed_at=now(), closed_from_status=$3,\n working_state = NULL, working_state_at = NULL, working_session = NULL WHERE id=$1",
		"UPDATE tasks SET status=$2, updated_at=now(), closed_at=NULL, closed_from_status=NULL,\n working_state = NULL, working_state_at = NULL, working_session = NULL WHERE id=$1",
		"UPDATE tasks SET status = 'claimed', updated_at = now(),\n working_state = NULL, working_state_at = NULL, working_session = NULL WHERE id = $1",
		// reads
		`SELECT t.id, COALESCE(CASE WHEN t.working_state IS NOT NULL THEN t.working_session END, '') FROM tasks t WHERE t.id = $1`,
		`SELECT t.working_state = 'working' AND t.working_state_at < now() - make_interval(secs => $2) FROM tasks t`,
	} {
		if v := travelViolations(s); len(v) != 0 {
			t.Errorf("the travels-with rule flags a correct statement %q: %v", s, v)
		}
	}
}

func TestWorkingSession_TravelsWithWorkingState(t *testing.T) {
	stateWriters := 0
	for _, root := range []string{"internal", "cmd"} {
		base := filepath.Join("..", "..", root)
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(filepath.Join("..", ".."), path)
			for _, lit := range literalTexts(t, path) {
				if !sigWrite.MatchString(lit) {
					continue
				}
				if wsNull.MatchString(lit) || wsSet.MatchString(lit) || wsParenSet.MatchString(lit) || wsInsert.MatchString(lit) {
					stateWriters++
				}
				for _, v := range travelViolations(lit) {
					t.Errorf("%s: a literal %s. Criterion 13 / S7: every statement that NULLs working_state NULLs "+
						"working_session in the SAME statement, and every set writes it. Literal: %q",
						filepath.ToSlash(rel), v, lit)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if stateWriters < 5 {
		t.Errorf("POSITIVE CONTROL FAILED: the scan saw %d working_state-writing literals, want >= 5 (signal's set "+
			"and clear, close, reopen, claim)", stateWriters)
	}
}

// ---- SWT-56 criterion 14: migration 0036 ------------------------------------------

func TestMigration0036_TaskWorkingSessionShape(t *testing.T) {
	dir := filepath.Join("..", "..", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations/: %v", err)
	}
	var n36 []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "0036_") && strings.HasSuffix(e.Name(), ".sql") {
			n36 = append(n36, e.Name())
		}
	}
	if len(n36) != 1 || n36[0] != "0036_task_working_session.sql" {
		t.Fatalf("migrations/0036_*.sql = %v, want exactly [0036_task_working_session.sql] (criterion 14)", n36)
	}
	var code []string
	for _, line := range strings.Split(sigRepoFile(t, "migrations/0036_task_working_session.sql"), "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		code = append(code, line)
	}
	sql := strings.ToLower(strings.TrimSpace(regexp.MustCompile(`\s+`).ReplaceAllString(strings.Join(code, " "), " ")))
	if !regexp.MustCompile(`alter table (public\.)?tasks add column working_session text\b`).MatchString(sql) {
		t.Errorf("0036 = %q, want ALTER TABLE tasks ADD COLUMN working_session TEXT (S4)", sql)
	}
	for _, banned := range []struct{ re, why string }{
		{`\bdefault\b`, "no default: a marker set before this ticket has no recorded session (S9)"},
		{`not null`, "nullable: old markers have no session, so 'a state implies a session' cannot hold (S4)"},
		{`\bcheck\b`, "no CHECK: an old binary's close/reopen/claim of a named marker would FAIL under one, and the " +
			"200-rune cap and character set are spelled once, in Go (S4)"},
		{`create (unique )?index`, "no index: read by primary key and in the board's one facts statement (S4)"},
		{`\bupdate\b`, "no backfill: inventing a name would put a false 'reply here' on the board (S9)"},
	} {
		if regexp.MustCompile(banned.re).MatchString(sql) {
			t.Errorf("0036 matches /%s/ — %s", banned.re, banned.why)
		}
	}
}
