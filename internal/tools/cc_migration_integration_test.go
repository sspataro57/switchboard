//go:build integration

package tools_test

// gmail-delivery-cc (SWT-69) criterion 2: the per-ticket migration guard, as
// POSTGRES sees it. The static shape guard (TestMigration0038_CcColumnAndChecks)
// reads the file; this one reads the applied schema and exercises both CHECKs
// with real INSERTs — the IK rule that a guard fed by a column needs a test
// that makes Postgres produce the value.
//
// ISOLATED scratch database only (never the shared compose `ops`, never prod):
//
//	psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c 'CREATE DATABASE ops_swt69'
//	make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_swt69?sslmode=disable'
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_swt69?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run Migration0038 ./internal/tools/
//
// EXPECTED RED: the migration does not exist, so neither the column nor the
// constraints are there.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/tools"
)

func TestMigration0038_Integration_DeliveryCcShape(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()

	body, err := os.ReadFile(filepath.Join("..", "..", "migrations", "0038_delivery_cc.sql"))
	if err != nil {
		t.Fatalf("read migrations/0038_delivery_cc.sql: %v", err)
	}

	tablesBefore := ccScanInt(t, ctx, pool,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema='public'`)

	// Criterion 1: applying it twice is safe (the 0037 idempotent-guard style).
	for i := 1; i <= 2; i++ {
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply 0038 (pass %d): %v — applying it twice must be a no-op, not an error", i, err)
		}
	}
	if got := ccScanInt(t, ctx, pool,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema='public'`); got != tablesBefore {
		t.Errorf("public tables went from %d to %d; this ticket adds NO table (invariant 2)", tablesBefore, got)
	}

	// The column: TEXT[] (udt _text), NOT NULL, DEFAULT '{}'.
	var udt, nullable, def string
	if err := pool.QueryRow(ctx,
		`SELECT udt_name, is_nullable, COALESCE(column_default,'')
		   FROM information_schema.columns
		  WHERE table_name='deliveries' AND column_name='cc'`).Scan(&udt, &nullable, &def); err != nil {
		t.Fatalf("deliveries.cc is not there after applying 0038: %v", err)
	}
	if udt != "_text" {
		t.Errorf("deliveries.cc is %s, want _text (TEXT[]): every short list of short strings in this schema is "+
			"TEXT[]; jsonb here is for provider payloads (D4)", udt)
	}
	if nullable != "NO" {
		t.Errorf("deliveries.cc is nullable; NOT NULL DEFAULT '{}' gives exactly ONE representation of \"no Cc\" " +
			"(a nullable array would give two and every reader would have to COALESCE — the SWT-61 NULL-subject trap)")
	}
	if !strings.Contains(def, "{}") {
		t.Errorf("deliveries.cc default = %q, want '{}'::text[]", def)
	}

	// Both CHECKs exist, by NAME (a name is how a later ticket widens one).
	defs := map[string]string{}
	for _, name := range []string{"deliveries_cc_gmail_check", "deliveries_cc_shape_check"} {
		var d string
		if err := pool.QueryRow(ctx,
			`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname=$1`, name).Scan(&d); err != nil {
			t.Fatalf("constraint %s is missing after applying 0038: %v", name, err)
		}
		defs[name] = d
	}
	// Mutation 4: the Go const and the schema limit must be the same number.
	if !strings.Contains(defs["deliveries_cc_shape_check"], strconv.Itoa(tools.MaxCcAddresses)) {
		t.Errorf("deliveries_cc_shape_check is %q but tools.MaxCcAddresses is %d; raising one without the other "+
			"makes the Go rule and the backstop disagree", defs["deliveries_cc_shape_check"], tools.MaxCcAddresses)
	}

	// ...and they BITE, through real INSERTs on a real task.
	cleanupDeliveryData(t, ctx, pool)
	defer cleanupDeliveryData(t, ctx, pool)
	fx := seedDeliveryFixture(t, ctx, pool)

	insertGmail := func(cc []string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO deliveries (task_id, channel, body, status, cc, created_by, from_account_id, thread_id)
			 VALUES ($1,'gmail','b','drafted',$2,$3,$4,$5)`,
			fx.parentID, cc, delActor, fx.accountID, fx.threadID)
		return err
	}
	if err := insertGmail([]string{}); err != nil {
		t.Errorf("a gmail row with no cc was refused: %v", err)
	}
	if err := insertGmail(ccAddresses(tools.MaxCcAddresses)); err != nil {
		t.Errorf("a gmail row with exactly %d addresses was refused: %v", tools.MaxCcAddresses, err)
	}
	if err := insertGmail(ccAddresses(tools.MaxCcAddresses + 1)); err == nil {
		t.Errorf("a gmail row with %d addresses was accepted; deliveries_cc_shape_check caps it at %d",
			tools.MaxCcAddresses+1, tools.MaxCcAddresses)
	} else if !strings.Contains(err.Error(), "deliveries_cc_shape_check") {
		t.Errorf("the over-limit refusal came from something else: %v", err)
	}
	if err := insertGmail([]string{""}); err == nil {
		t.Error("a gmail row with an EMPTY cc element was accepted; an empty string is not an address and would " +
			"reach the header as one")
	} else if !strings.Contains(err.Error(), "deliveries_cc_shape_check") {
		t.Errorf("the empty-element refusal came from something else: %v", err)
	}

	// The gmail-only backstop. slack_reply is the channel whose direct INSERT
	// has no other shape constraint to trip over, so a refusal here is
	// unambiguously this CHECK's.
	_, err = pool.Exec(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, cc, created_by)
		 VALUES ($1,'slack_reply','https://app.slack.com/client/TITEST/CITEST/p1750000000000000','b','drafted',$2,$3)`,
		fx.parentID, []string{"kevans@cecollaboratory.com"}, delActor)
	if err == nil {
		t.Error("a slack_reply row with a cc was accepted; deliveries_cc_gmail_check must refuse it (D12)")
	} else if !strings.Contains(err.Error(), "deliveries_cc_gmail_check") {
		t.Errorf("the slack_reply refusal came from something else: %v", err)
	}
}

func ccScanInt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func ccAddresses(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, "cc"+strconv.Itoa(i)+"@itest-del.example")
	}
	return out
}
