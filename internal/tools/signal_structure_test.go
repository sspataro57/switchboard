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
		if v > 33 {
			t.Errorf("migrations/%s exists: criterion 17 — 0033 is the only migration this ticket adds and none above it exists", e.Name())
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

var sigWrite = regexp.MustCompile(`(?is)\bset\b[^;]*?\bworking_state(_at)?\s*=[^=]` +
	`|\bset\s*\([^)]*\bworking_state(_at)?\b[^)]*\)\s*=` +
	`|insert\s+into\s+tasks\s*\([^)]*\bworking_state`)

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

func TestWorkingState_OnlySignalAndCloseWriteIt(t *testing.T) {
	allowed := map[string]bool{"internal/tools/signal.go": true, "internal/tools/close.go": true}
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
							"(internal/tools/signal.go) and closeTransition (internal/tools/close.go) write them; the "+
							"dashboard only READS for the lights (invariant 3)", rel, m)
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
	if writers["internal/tools/close.go"] == 0 {
		t.Errorf("POSITIVE CONTROL FAILED: internal/tools/close.go writes no working_state. D9: closeTransition's "+
			"transitioning UPDATE clears both columns on a real close (writers seen: %v)", seen)
	}
}
