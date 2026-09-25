//go:build integration

package dashboard_test

// SWT-89 review finding: migration 0044's `WHEN (OLD.* IS DISTINCT FROM NEW.*)`
// looks up each column type's equality operator when a row is COMPARED, not
// when the trigger is created. A later migration adding a `json` (or `xml`)
// column to a triggered table would make every UPDATE of a row with a non-NULL
// value there fail with "could not identify an equality operator" — a broken
// write, not a missed notification. This guard reads the applied schema.
// MUTATION: ALTER TABLE tasks ADD COLUMN x json -> red.

import (
	"context"
	"testing"

	"github.com/sspataro57/switchboard/internal/dashboard"
)

func TestBoardLive_Integration_NoColumnTypeWithoutEquality(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	rows, err := pool.Query(ctx,
		`SELECT table_name, column_name, udt_name FROM information_schema.columns
		  WHERE table_schema = 'public' AND table_name = ANY($1) AND udt_name IN ('json', '_json', 'xml', '_xml')`,
		dashboard.BoardNotifyTables())
	if err != nil {
		t.Fatalf("read column types: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var tbl, col, typ string
		if err := rows.Scan(&tbl, &col, &typ); err != nil {
			t.Fatalf("scan: %v", err)
		}
		t.Errorf("%s.%s is %s, which has no equality operator: migration 0044's WHEN (OLD.* IS DISTINCT FROM NEW.*) "+
			"would fail every UPDATE of a row with a value there. Use jsonb, or change the trigger's WHEN.", tbl, col, typ)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
}
