//go:build integration

package store_test

// Integration tests for SWT-21 migration 0016 — acceptance criteria 9 and 11.
// Build-tagged `integration` AND env-gated on DATABASE_URL. No LLM, no provider,
// no network beyond Postgres: this suite reads the SHAPE of the projects table.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run AILocality ./internal/store/
//
// GREENFIELD NOTE: fails until migration 0016 is applied to the compose db
// (`make db-up && make migrate`) — "column ai_locality does not exist" is the
// expected first failure.
//
// What is NOT tested here, deliberately: the migration's STATEMENT ORDER. On a
// fresh database `UPDATE projects SET ai_locality='any'` touches zero rows, so
// no query afterwards can tell a correct migration from one with the UPDATE and
// the personal INSERT swapped — the evidence only exists in a database that had
// projects before 0016 ran. That is why criterion 9's order is pinned by a
// source test (internal/provider/structure_test.go) and re-checked by hand
// against production in the verification protocol.
//
// Mutual-cleanup pact: this suite owns projects with slug LIKE 'itest-locality-%'
// and deletes them before and after. It touches no other table, makes no global
// count assertion, and never deletes the seeded `personal` row.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/store"
)

func localityPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use the compose db on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)

	clean := func() {
		if _, err := pool.Exec(ctx, `DELETE FROM projects WHERE slug LIKE 'itest-locality-%'`); err != nil {
			t.Logf("cleanup projects: %v", err)
		}
	}
	clean()
	t.Cleanup(clean)
	return pool
}

// Criterion 9: the column defaults to 'local_only' — FAIL CLOSED. A project
// created later without thinking about this column is restricted, which stalls
// (recoverable with one UPDATE, visible in the skipped lane) rather than leaks
// (irreversible).
func TestAILocality_DefaultsToLocalOnly(t *testing.T) {
	ctx := context.Background()
	pool := localityPool(t, ctx)

	// The fixture that says nothing.
	var silent string
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery)
		 VALUES ('itest-locality silent', 'itest-locality-silent', 'itest-locality', 'manual', 'dashboard')
		 RETURNING ai_locality`).Scan(&silent); err != nil {
		t.Fatalf("insert project without ai_locality: %v", err)
	}
	if silent != "local_only" {
		t.Errorf("a project inserted without naming ai_locality got %q, want 'local_only'. The default is the "+
			"whole of criterion 9: a leak is irreversible, a stall is one UPDATE and shows up in the skipped lane",
			silent)
	}

	// The fixture that says 'any' — and the two must DIFFER, or this test is
	// asserting a constant.
	var explicit string
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, ai_locality)
		 VALUES ('itest-locality explicit', 'itest-locality-explicit', 'itest-locality', 'manual', 'dashboard', 'any')
		 RETURNING ai_locality`).Scan(&explicit); err != nil {
		t.Fatalf("insert project with ai_locality='any': %v", err)
	}
	if explicit != "any" {
		t.Errorf("explicit ai_locality='any' stored as %q", explicit)
	}
	if silent == explicit {
		t.Errorf("both fixtures read back %q — the column does not discriminate, and every locality test in "+
			"this repo would pass against a constant", silent)
	}
}

// Criterion 9's CHECK. A third spelling must be impossible: the boundary reads
// this column and has no sensible answer for a value that is neither.
func TestAILocality_CheckRejectsOtherValues(t *testing.T) {
	ctx := context.Background()
	pool := localityPool(t, ctx)

	_, err := pool.Exec(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, ai_locality)
		 VALUES ('itest-locality bogus', 'itest-locality-bogus', 'itest-locality', 'manual', 'dashboard', 'hosted')`)
	if err == nil {
		t.Errorf("ai_locality='hosted' was accepted; criterion 9 constrains the column to " +
			"('local_only','any') so a typo cannot invent a third state the boundary has to guess about")
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, ai_locality)
		 VALUES ('itest-locality null', 'itest-locality-null', 'itest-locality', 'manual', 'dashboard', NULL)`); err == nil {
		t.Errorf("ai_locality=NULL was accepted; the column is NOT NULL precisely because 'absent' is the state " +
			"this ticket must treat as unsafe — that is also why it is a column and not a key in policies jsonb")
	}
}

// Criterion 11: the migration seeds the PROJECT (and only the project).
func TestAILocality_PersonalProjectIsSeeded(t *testing.T) {
	ctx := context.Background()
	pool := localityPool(t, ctx)

	var locality, execution, delivery string
	var client *string
	err := pool.QueryRow(ctx,
		`SELECT ai_locality, execution, delivery, client FROM projects WHERE slug = 'personal'`).
		Scan(&locality, &execution, &delivery, &client)
	if err != nil {
		t.Fatalf("no project with slug 'personal': %v (migration 0016 seeds it)", err)
	}

	if locality != "local_only" {
		t.Errorf("personal.ai_locality = %q, want 'local_only'. If this reads 'any', the migration's UPDATE ran "+
			"AFTER the INSERT — the silent reordering criterion 9 warns about, and the one failure mode that "+
			"produces no error anywhere", locality)
	}
	if execution != "manual" {
		t.Errorf("personal.execution = %q, want 'manual'", execution)
	}
	if delivery != "dashboard" {
		t.Errorf("personal.delivery = %q, want 'dashboard'", delivery)
	}
	if client != nil {
		t.Errorf("personal.client = %q, want NULL. This is not cosmetic: the SPEC DROPPED task_get_next's "+
			"locality predicate on the grounds that `p.client = $1` (internal/tools/getnext.go) already "+
			"excludes a project whose client is NULL. Give personal a client and that reasoning is void — a "+
			"worker running as that client can be handed personal tasks with no guard anywhere", *client)
	}
}

// Criterion 12: the migration does NOT seed capture rules. SWT-17's precedent —
// routing is configuration with an enabled flag and an audit trail, and seeding
// it puts production routing into every test database.
func TestAILocality_MigrationSeedsNoCaptureRules(t *testing.T) {
	ctx := context.Background()
	pool := localityPool(t, ctx)

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM capture_rules r
		   JOIN projects p ON p.id = r.project_id
		  WHERE p.slug = 'personal'`).Scan(&n); err != nil {
		t.Fatalf("count personal capture_rules: %v", err)
	}
	if n != 0 {
		t.Errorf("%d capture_rules rows exist for the personal project in a database that has only been "+
			"migrated. Criterion 12: the rules go in with `opsctl capture-rules add`, from a measured sender "+
			"list, so they carry an enabled flag and an audit row and a rule edit is not a new migration", n)
	}
}
