//go:build integration

package tools_test

// activity-resurfaces (SWT-72) criterion 1, the half only POSTGRES sees: the
// three columns of migrations/0039_task_activity_review.sql as APPLIED. The
// static shape guard (TestMigration0039_ActivityReviewColumns) catches a file
// nobody applied; this one catches an application that diverged, because the
// migrate runner keys on schema_migrations.version with NO checksum (IK
// landmine: "an edited migration never reaches a DB that already applied it").
// The 0038 pair is the template.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_swt72?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run Migration0039 ./internal/tools/
//
// RED TODAY: 0039 is not applied.

import (
	"context"
	"testing"
)

func TestMigration0039_Integration_ActivityColumns(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()

	for _, c := range []struct{ name, typ, nullable string }{
		{"activity_at", "timestamp with time zone", "YES"},
		{"activity_by_message_id", "bigint", "YES"},
		{"reviewed_at", "timestamp with time zone", "YES"},
	} {
		var typ, nullable, def *string
		err := pool.QueryRow(ctx,
			`SELECT data_type, is_nullable, column_default FROM information_schema.columns
			  WHERE table_name='tasks' AND column_name=$1`, c.name).Scan(&typ, &nullable, &def)
		if err != nil {
			t.Errorf("tasks.%s does not exist (%v). Criterion 1: apply migrations/0039_task_activity_review.sql "+
				"(`make migrate LOCAL_DB_URL=...`) — merging a migration is not applying it", c.name, err)
			continue
		}
		if *typ != c.typ || *nullable != c.nullable {
			t.Errorf("tasks.%s is %s/%s nullable, want %s/%s", c.name, *typ, *nullable, c.typ, c.nullable)
		}
		if def != nil {
			t.Errorf("tasks.%s has DEFAULT %s; D1: every existing row is NULL at rollout, so no task appears in "+
				"INCOMING and no backfill is needed", c.name, *def)
		}
	}

	// The FK, with ON DELETE SET NULL (the SWT-45 surfaced_by_message_id shape):
	// deleting a message must never delete the task.
	var rule string
	if err := pool.QueryRow(ctx,
		`SELECT rc.delete_rule
		   FROM information_schema.table_constraints tc
		   JOIN information_schema.key_column_usage kcu ON kcu.constraint_name = tc.constraint_name
		   JOIN information_schema.referential_constraints rc ON rc.constraint_name = tc.constraint_name
		  WHERE tc.table_name='tasks' AND tc.constraint_type='FOREIGN KEY'
		    AND kcu.column_name='activity_by_message_id'`).Scan(&rule); err != nil {
		t.Fatalf("tasks.activity_by_message_id has no FOREIGN KEY to normalized_messages (%v) — criterion 1", err)
	}
	if rule != "SET NULL" {
		t.Errorf("tasks.activity_by_message_id's FK delete rule = %q, want SET NULL (criterion 1: a purged "+
			"message must never take its task with it)", rule)
	}

	// Criterion 1: no index on the three columns — they are read by primary key
	// and per displayed row.
	var idx int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE tablename='tasks'
		   AND (indexdef ILIKE '%activity_at%' OR indexdef ILIKE '%activity_by_message_id%'
		        OR indexdef ILIKE '%reviewed_at%')`).Scan(&idx); err != nil {
		t.Fatalf("probe indexes on tasks: %v", err)
	}
	if idx != 0 {
		t.Errorf("%d indexes mention the new columns, want 0 (criterion 1)", idx)
	}
}
