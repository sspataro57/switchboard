//go:build integration

package slackweb_test

// slack-watch-sweep (SWT-75) criteria 6 (the database half), 16, 17 and 20
// against Postgres: the phase a targeted pass stamps on sync_runs, the run rows
// a quiet pass does NOT write, and the watch list being re-read on every pass.
//
//	psql '...' -c "CREATE DATABASE ops_slackwatch"
//	make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_slackwatch?sslmode=disable'
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_slackwatch?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run WatchSweep ./internal/connector/slackweb/
//
// Build-tagged AND env-gated, and it FATALs on the prod host — this suite
// deletes rows. Never the shared compose `ops` (SPEC Verification step 3).
//
// These four belong HERE and not in watch_test.go for one reason, and it is the
// IK rule "test the column, not the fixture": every predicate under test reads
// a COLUMN (slack_watch.enabled, source_accounts.provider, sync_runs.stats
// ->>'phase'), and a unit test that supplies those values is testing its own
// fixture. Mutate the SELECT to a literal and these must go red.
//
// This suite owns the account 'twsweep@slack-web.local', the workspaces
// 'TWSWEEP%' and the thread prefix 'slack:TWSWEEP', and cleans its OWN corpus
// in FK order, rerunnably, at start and end.
//
// RED TODAY: migration 0041 is not applied, and IngestWith / WatchTargets /
// the four-argument StartRun do not exist, so this file compile-FAILs under
// -tags integration.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	wsWorkspace = "TWSWEEP"
	wsOther     = "TWSWEEPX"
	wsAccount   = "twsweep@slack-web.local"
	wsConv      = "DWSWEEP01"
	wsConv2     = "CWSWEEP02"
	wsOwnUser   = "UWSWEEPOWN"
)

// wsSource is a targeted leaf: one workspace, one DM, N messages, and a
// coverage block that reports mode "targeted".
type wsSource struct {
	texts []string
	// bad makes the workspace fail validateConversation, the shape of a
	// mid-pass failure that has already opened its run.
	bad bool
	err error
	// mode overrides the coverage mode ("" = targeted).
	mode string
}

func (s *wsSource) Export(context.Context, slackweb.ExportRequest) (slackweb.Export, error) {
	if s.err != nil {
		return slackweb.Export{}, s.err
	}
	mode := s.mode
	if mode == "" {
		mode = slackweb.CoverageModeTargeted
	}
	convType := "dm"
	if s.bad {
		convType = "not-a-conversation-type"
	}
	msgs := make([]slackweb.Message, 0, len(s.texts))
	for i, text := range s.texts {
		msgs = append(msgs, slackweb.Message{
			ID: "p17890000000000" + string(rune('0'+i)), Timestamp: "2026-09-22T09:0" + string(rune('0'+i)) + ":00Z",
			Author: "José", AuthorID: "UJOSEWS", Text: text,
		})
	}
	return slackweb.Export{SchemaVersion: slackweb.SchemaVersion, Workspaces: []slackweb.Workspace{{
		ID: wsWorkspace, Name: "Sweep", URL: "https://app.slack.com/client/" + wsWorkspace, OwnUserID: wsOwnUser,
		Conversations: []slackweb.Conversation{{
			ID: wsConv, Name: "jose", Type: convType,
			URL: "https://app.slack.com/client/" + wsWorkspace + "/" + wsConv, Messages: msgs,
		}},
		Read:     []string{wsConv},
		Coverage: &slackweb.Coverage{EnumeratedCount: 1, ReadCount: 1, ElapsedMS: 18000, Mode: mode},
	}}}, nil
}

func newWSPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must never run against the real ops database")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	cleanupWatchSweep(t, ctx, pool)
	t.Cleanup(func() { cleanupWatchSweep(t, context.Background(), pool) })
	return pool
}

func cleanupWatchSweep(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE provider='slack_web' AND account_email='` + wsAccount + `')`
	for _, q := range []string{
		// One statement, so the threads referenced only by this suite's messages
		// go with them (the FK is checked at statement end) — and no LIKE on the
		// thread key: its format has ONE spelling (threadscope_test.go).
		`WITH mine AS (SELECT id, thread_id FROM normalized_messages
		                WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)),
		      gone AS (DELETE FROM normalized_messages WHERE id IN (SELECT id FROM mine))
		 DELETE FROM normalized_threads t WHERE t.id IN (SELECT thread_id FROM mine)
		   AND NOT EXISTS (SELECT 1 FROM normalized_messages m WHERE m.thread_id = t.id AND m.id NOT IN (SELECT id FROM mine))`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider='slack_web' AND account_email='` + wsAccount + `'`,
		`DELETE FROM slack_watch WHERE workspace_id LIKE 'TWSWEEP%'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			if strings.Contains(err.Error(), "slack_watch") {
				t.Fatalf("cleanup %q: %v — apply migrations/0041_slack_watch.sql first", q, err)
			}
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func wsRunRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, phase string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM sync_runs r JOIN source_accounts a ON a.id=r.source_account_id
		  WHERE a.provider='slack_web' AND a.account_email=$1 AND r.stats->>'phase' = $2`,
		wsAccount, phase).Scan(&n); err != nil {
		t.Fatalf("count %s runs: %v", phase, err)
	}
	return n
}

func wsTargetedRequest() slackweb.ExportRequest {
	req, _ := slackweb.BuildTargetedRequest([]slackweb.WatchRow{
		{WorkspaceID: wsWorkspace, ConversationID: wsConv, Enabled: true},
	}, 150000)
	return req
}

// Criterion 17: a targeted pass writes sync_runs with
// stats->>'phase' = 'slack_web_watch'; a rotation pass keeps 'slack_web'.
// Asserted against POSTGRES, because the phase is what criteria 18 and 19 read
// out of the column.
//
// MUTATION: write phase slack_web on a targeted pass -> this, 18 and 19 go red.
func TestWatchSweep_TargetedPassWritesTheWatchPhase(t *testing.T) {
	ctx := context.Background()
	pool := newWSPool(t, ctx)
	sink := slackweb.NewSink(pool)

	if _, err := slackweb.IngestWith(ctx, &wsSource{texts: []string{"first watched message"}}, sink,
		slackweb.IngestOptions{Request: wsTargetedRequest(), Phase: slackweb.PhaseSlackWebWatch}); err != nil {
		t.Fatalf("targeted IngestWith: %v", err)
	}
	if n := wsRunRows(t, ctx, pool, slackweb.PhaseSlackWebWatch); n != 1 {
		t.Errorf("sync_runs rows with phase %q = %d, want 1 (criterion 17)", slackweb.PhaseSlackWebWatch, n)
	}
	if n := wsRunRows(t, ctx, pool, slackweb.PhaseSlackWeb); n != 0 {
		t.Errorf("a targeted pass wrote %d rows with phase %q. D6: without the separate phase, a delivery into "+
			"a watched conversation is flagged \"unconfirmed after 3 export passes\" THREE MINUTES after the "+
			"send — an alarm meaning something entirely different from what it says",
			n, slackweb.PhaseSlackWeb)
	}

	if _, err := slackweb.IngestWith(ctx, &wsSource{texts: []string{"first watched message", "a rotation read"}}, sink,
		slackweb.IngestOptions{Phase: slackweb.PhaseSlackWeb}); err != nil {
		t.Fatalf("rotation IngestWith: %v", err)
	}
	if n := wsRunRows(t, ctx, pool, slackweb.PhaseSlackWeb); n != 1 {
		t.Errorf("sync_runs rows with phase %q after a rotation pass = %d, want 1 — the rotation is "+
			"byte-for-byte today's export (criterion 17, D11)", slackweb.PhaseSlackWeb, n)
	}
}

// Criterion 20: the run-row volume discipline, as the rows actually land.
// A quiet watcher writes nothing; 1,440 rows a day per workspace for nothing is
// worse than the 230/day the mail watcher's D9 already refused.
func TestWatchSweep_QuietTargetedPassesWriteNoRunRow(t *testing.T) {
	ctx := context.Background()
	pool := newWSPool(t, ctx)
	sink := slackweb.NewSink(pool)
	opts := slackweb.IngestOptions{Request: wsTargetedRequest(), Phase: slackweb.PhaseSlackWebWatch, Quiet: true}

	// 1: the first pass ingests, so it writes a row.
	if _, err := slackweb.IngestWith(ctx, &wsSource{texts: []string{"a watched message"}}, sink, opts); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if n := wsRunRows(t, ctx, pool, slackweb.PhaseSlackWebWatch); n != 1 {
		t.Fatalf("after an INGESTING pass: %d run rows, want 1", n)
	}

	// 2: the same bytes again — nothing inserted, nothing updated, no failure.
	if _, err := slackweb.IngestWith(ctx, &wsSource{texts: []string{"a watched message"}}, sink, opts); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if n := wsRunRows(t, ctx, pool, slackweb.PhaseSlackWebWatch); n != 1 {
		t.Errorf("a QUIET pass wrote a run row (%d total, want still 1). Criterion 20 / D6: a pass that "+
			"inserted or updated nothing and did not fail writes NO sync_runs row — and a phase with no rows "+
			"shows nothing on /funnel, which is correct: liveness is /healthz's job", n)
	}

	// 3: a failure always leaves a row, with its error.
	if _, err := slackweb.IngestWith(ctx, &wsSource{texts: []string{"a watched message"}, bad: true}, sink, opts); err == nil {
		t.Fatal("a pass whose workspace fails validation returned nil error")
	}
	if n := wsRunRows(t, ctx, pool, slackweb.PhaseSlackWebWatch); n != 2 {
		t.Errorf("after a FAILING pass: %d run rows, want 2 — a failure is always recorded (criterion 20)", n)
	}

	// 4: the first success after a failure is worth one row.
	if _, err := slackweb.IngestWith(ctx, &wsSource{texts: []string{"a watched message"}}, sink, opts); err != nil {
		t.Fatalf("pass 4: %v", err)
	}
	if n := wsRunRows(t, ctx, pool, slackweb.PhaseSlackWebWatch); n != 3 {
		t.Errorf("the first success after a failure wrote no row (%d total, want 3): the recovery is the one "+
			"quiet pass worth recording (criterion 20)", n)
	}

	// 5: and the next quiet one is silent again.
	if _, err := slackweb.IngestWith(ctx, &wsSource{texts: []string{"a watched message"}}, sink, opts); err != nil {
		t.Fatalf("pass 5: %v", err)
	}
	if n := wsRunRows(t, ctx, pool, slackweb.PhaseSlackWebWatch); n != 3 {
		t.Errorf("a second quiet success after the recovery wrote a row (%d total, want 3) (criterion 20)", n)
	}
}

// Criteria 6 and 16: WatchTargets reads the COLUMNS — enabled, and a workspace
// that has a slack_web source_accounts row — and it is read on EVERY pass, so a
// row added between passes is swept on the next one with no restart (the
// SWT-73 D10 "watch the table, not a snapshot" rule).
//
// MUTATION: replace the enabled predicate with a literal true, or list the
// rows once at startup, and this goes red.
func TestWatchSweep_WatchTargetsAreReReadEveryPass(t *testing.T) {
	ctx := context.Background()
	pool := newWSPool(t, ctx)
	sink := slackweb.NewSink(pool)

	// The account exists only once a pass has ingested from the workspace.
	if _, err := slackweb.IngestWith(ctx, &wsSource{texts: []string{"seed the account"}}, sink,
		slackweb.IngestOptions{Request: wsTargetedRequest(), Phase: slackweb.PhaseSlackWebWatch}); err != nil {
		t.Fatalf("seed pass: %v", err)
	}

	for _, row := range []struct {
		ws, conv string
		enabled  bool
	}{
		{wsWorkspace, wsConv, true},
		{wsWorkspace, wsConv2, false}, // disabled: not swept
		{wsOther, "DWSWEEPZZ", true},  // enabled, but no source_accounts row for TWSWEEPX
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO slack_watch (workspace_id, conversation_id, enabled) VALUES ($1,$2,$3)`,
			row.ws, row.conv, row.enabled); err != nil {
			t.Fatalf("seed slack_watch(%s,%s): %v", row.ws, row.conv, err)
		}
	}

	got, err := sink.WatchTargets(ctx)
	if err != nil {
		t.Fatalf("WatchTargets: %v", err)
	}
	mine := map[string]slackweb.WatchRow{}
	for _, r := range got {
		if strings.HasPrefix(r.WorkspaceID, "TWSWEEP") {
			mine[r.WorkspaceID+"/"+r.ConversationID] = r
		}
	}
	if _, ok := mine[wsWorkspace+"/"+wsConv]; !ok {
		t.Errorf("WatchTargets omitted the enabled row %s/%s: %v", wsWorkspace, wsConv, got)
	}
	if _, ok := mine[wsWorkspace+"/"+wsConv2]; ok {
		t.Errorf("WatchTargets returned the DISABLED row %s/%s. Criterion 6: disabling a row is the mildest "+
			"rollback the SPEC offers, and it must actually stop the sweep", wsWorkspace, wsConv2)
	}
	if _, ok := mine[wsOther+"/DWSWEEPZZ"]; ok {
		t.Errorf("WatchTargets returned a row for %s, which has no slack_web source_accounts entry. Criterion 6: "+
			"the leaf cannot open a workspace this switchboard has never ingested, and asking it to burns a "+
			"navigation per minute", wsOther)
	}
	if len(mine) != 1 {
		t.Fatalf("WatchTargets returned %d of this suite's rows, want exactly 1: %v", len(mine), mine)
	}

	// Criterion 16: a row added between passes, with no restart.
	if _, err := pool.Exec(ctx,
		`UPDATE slack_watch SET enabled=true WHERE workspace_id=$1 AND conversation_id=$2`,
		wsWorkspace, wsConv2); err != nil {
		t.Fatalf("enable %s: %v", wsConv2, err)
	}
	after, err := sink.WatchTargets(ctx)
	if err != nil {
		t.Fatalf("WatchTargets (second call): %v", err)
	}
	found := 0
	for _, r := range after {
		if r.WorkspaceID == wsWorkspace && (r.ConversationID == wsConv || r.ConversationID == wsConv2) {
			found++
		}
	}
	if found != 2 {
		t.Errorf("after enabling a second row WatchTargets saw %d of 2. Criterion 16: the watch list is "+
			"re-read from the database on EVERY pass — `opsctl slack-watch add` must take effect on the next "+
			"minute, not on the next pod restart", found)
	}
}

// The failure mode a watcher must survive: WatchTargets against a database that
// has not had 0041 applied returns an ERROR naming the table, rather than an
// empty list that would look like "nothing is watched" forever.
func TestWatchSweep_WatchTargetsFailsLoudlyWithoutTheTable(t *testing.T) {
	ctx := context.Background()
	pool := newWSPool(t, ctx)
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('slack_watch') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("to_regclass: %v", err)
	}
	if !exists {
		if _, err := slackweb.NewSink(pool).WatchTargets(ctx); err == nil {
			t.Error("WatchTargets against a pre-0041 database returned no error; an empty list would read as " +
				"\"nothing is watched\" and the watcher would sweep nothing, forever, while logging success")
		} else if !strings.Contains(err.Error(), "slack_watch") {
			t.Errorf("WatchTargets error %q does not name slack_watch", err)
		}
		return
	}
	// 0041 IS applied: nothing to assert here, and saying so beats a silent skip.
	t.Log("0041 is applied; the missing-table path is not exercised")
}

// Criterion 7's other half (review follow-up, 2026-09-22): a WHOLE-pass
// failure — the export itself failing, or a non-targeted answer — leaves ONE
// sync_runs error row per targeted workspace, and a second failure in a row
// leaves none (a leaf broken for hours costs one row, not one per minute). A
// busy 503 leaves nothing: it is an expected skip.
func TestWatchSweep_WholePassFailureWritesOneErrorRow(t *testing.T) {
	ctx := context.Background()
	pool := newWSPool(t, ctx)
	sink := slackweb.NewSink(pool)
	opts := slackweb.IngestOptions{Request: wsTargetedRequest(), Phase: slackweb.PhaseSlackWebWatch, Quiet: true, Targeted: true}

	// The account must exist (a targeted request only names ingested workspaces).
	if _, err := slackweb.IngestWith(ctx, &wsSource{texts: []string{"seed"}}, sink, opts); err != nil {
		t.Fatalf("seed pass: %v", err)
	}
	before := wsRunRows(t, ctx, pool, slackweb.PhaseSlackWebWatch)

	// A busy bridge: skipped, nothing written.
	busy := &wsSource{err: &slackweb.BridgeBusyError{RetryAfter: 30 * time.Second, Body: "sweep running"}}
	if _, err := slackweb.IngestWith(ctx, busy, sink, opts); err == nil {
		t.Fatal("a busy export returned nil")
	}
	if n := wsRunRows(t, ctx, pool, slackweb.PhaseSlackWebWatch); n != before {
		t.Errorf("a 503 wrote %d run row(s); a busy bridge is a skip, not a failure", n-before)
	}

	// A non-targeted answer: one error row.
	full := &wsSource{texts: []string{"seed"}, mode: slackweb.CoverageModeFull}
	if _, err := slackweb.IngestWith(ctx, full, sink, opts); !errors.Is(err, slackweb.ErrNotTargeted) {
		t.Fatalf("a full answer to a targeted request = %v, want ErrNotTargeted", err)
	}
	if n := wsRunRows(t, ctx, pool, slackweb.PhaseSlackWebWatch); n != before+1 {
		t.Fatalf("after a refused answer: %d new run rows, want 1 (criterion 7: one sync_runs error row)", n-before)
	}
	var status, errMsg string
	if err := pool.QueryRow(ctx,
		`SELECT r.status, COALESCE(r.error,'') FROM sync_runs r JOIN source_accounts a ON a.id=r.source_account_id
		  WHERE a.account_email=$1 AND r.stats->>'phase'=$2 ORDER BY r.id DESC LIMIT 1`,
		wsAccount, slackweb.PhaseSlackWebWatch).Scan(&status, &errMsg); err != nil {
		t.Fatalf("read the failure row: %v", err)
	}
	if status != "error" || !strings.Contains(errMsg, "targeted") {
		t.Errorf("failure row = %s %q, want status error naming the targeted refusal", status, errMsg)
	}

	// The same failure again: no second row.
	if _, err := slackweb.IngestWith(ctx, full, sink, opts); err == nil {
		t.Fatal("second refused answer returned nil")
	}
	if n := wsRunRows(t, ctx, pool, slackweb.PhaseSlackWebWatch); n != before+1 {
		t.Errorf("a repeated failure wrote another row (%d new); only the FIRST failure in a row is recorded", n-before)
	}
}
