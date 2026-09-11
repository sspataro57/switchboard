package slackweb_test

// Structural test for bug slackweb-collab-export-stale (Jira SWT-39), fix D:
// migration 0027 widens sync_runs.status to admit 'partial'. ZERO I/O beyond
// reading this repo's migrations directory. The shape follows
// internal/tools/dismissal_reopen_structure_test.go.
//
// 0001_initial.sql declares
//
//	status TEXT NOT NULL DEFAULT 'running' CHECK (status IN ('running','ok','error'))
//
// and Postgres names that inline constraint sync_runs_status_check (verified
// on the compose db). Ingest's new FinishRun(..., "partial", ...) fails
// against it, and so would the whole run row.
//
// Expected shape, one transaction (migrate runs each file in one):
//
//	ALTER TABLE sync_runs DROP CONSTRAINT sync_runs_status_check;
//	ALTER TABLE sync_runs ADD CONSTRAINT sync_runs_status_check
//	  CHECK (status IN ('running','ok','partial','error'));
//
// Deploy note: prod is at 0026. Apply 0027 BEFORE any image that writes
// 'partial', or every partial run fails its FinishRun.
//
// EXPECTED RED: no migrations/0027_*.sql exists.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func swt39StripSQLComments(sql string) string {
	var b strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

func TestRegression_SWT39_Migration0027AddsPartialToSyncRunsStatus(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "migrations")
	// Control first: without 0026 an empty glob below would prove only that the
	// directory moved.
	if _, err := os.Stat(filepath.Join(dir, "0026_dismissal_reopen.sql")); err != nil {
		t.Fatalf("migrations/0026_dismissal_reopen.sql is missing: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "0027_*.sql"))
	if err != nil {
		t.Fatalf("glob migrations/0027_*.sql: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("found %d migrations/0027_*.sql file(s), want exactly 1. sync_runs.status is "+
			"CHECK (status IN ('running','ok','error')) since 0001, so Ingest cannot record a partial run "+
			"without widening it. 0026 is the highest applied in prod", len(matches))
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read %s: %v", matches[0], err)
	}
	sql := strings.ToLower(swt39StripSQLComments(string(raw)))
	name := filepath.Base(matches[0])

	if !regexp.MustCompile(`alter\s+table\s+(if\s+exists\s+)?(public\.)?sync_runs\b`).MatchString(sql) {
		t.Errorf("%s never alters sync_runs", name)
	}
	if !regexp.MustCompile(`drop\s+constraint\s+(if\s+exists\s+)?sync_runs_status_check\b`).MatchString(sql) {
		t.Errorf("%s does not DROP CONSTRAINT sync_runs_status_check, the name Postgres gave 0001's inline "+
			"CHECK. Adding a second CHECK beside it leaves 'partial' rejected by the first", name)
	}
	add := regexp.MustCompile(`add\s+constraint\s+sync_runs_status_check\s+check\s*\(\s*status\s+in\s*\(([^)]*)\)\s*\)`).
		FindStringSubmatch(sql)
	if add == nil {
		t.Fatalf("%s does not ADD CONSTRAINT sync_runs_status_check CHECK (status IN (...)). Dropping the "+
			"CHECK without re-adding it would admit any status string", name)
	}
	var values []string
	for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(add[1], -1) {
		values = append(values, m[1])
	}
	sort.Strings(values)
	if want := []string{"error", "ok", "partial", "running"}; strings.Join(values, ",") != strings.Join(want, ",") {
		t.Errorf("%s's new CHECK admits %v, want exactly %v: every existing status kept, and 'partial' "+
			"added for a run whose leaf deferred or could not read conversations", name, values, want)
	}
}
