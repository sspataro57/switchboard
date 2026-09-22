//go:build integration

package dashboard_test

// comms-inbox (SWT-74, docs/tickets/comms-inbox_SPEC.md) against a real
// database and the REAL dashboard.Server — criterion 42's BOARD half and
// criteria 37, 38 and 47's board half:
//
//   - a real capture pass over an ARMED rule puts José's mail at the top of
//     INCOMING as its OWN row, remark `new email`, sender in the title cell,
//     while the TICKET task stays in QUEUE;
//   - `actions` -> Attach with the ticket's id routes the comm and closes it:
//     the flash shows once, the comm LEAVES the board, the ticket gains the
//     ids-only pointer and does NOT move to INCOMING;
//   - a second Attach of the same pair is a clean no-op.
//
// That is Verification Step 4's smoke, automated. Nothing is faked: the route,
// the executor, the REAL policy matrix, the real capture pass and the real
// board query.
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_comms?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run BoardComm ./internal/dashboard/
//
// USE AN ISOLATED DATABASE (IK 2026-09-12: the compose `ops` db is shared by
// every worktree, and this test runs a capture pass, whose pending set is
// GLOBAL).
//
// Reuses dashGuard / dashPool / newDashServer / snippet
// (dashboard_integration_test.go), bdInsID / bdCount
// (board_dismiss_integration_test.go), lightsExecutor
// (board_lights_integration_test.go), layoutBoard / layoutSections / lyRender
// (board_layout_integration_test.go) and boardRow.
//
// GREENFIELD NOTE, EXPECTED RED: migration 0040 is not applied (bcRequire0040
// fails with one sentence), then capture has no comm path and task_attach is
// not registered.
//
// SPEC AMENDMENT (2026-09-22 12:20, swb #491, on main): capture's actionTask
// branch also marks a task it CREATES, so the TICKET task is seeded by a first
// pass and its activity is CLEARED before the comm pass — otherwise "the ticket
// task is in QUEUE" would be asserting the seed, not the behaviour.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - drop markRuleActivity on the COMM -> TheCommIsItsOwnIncomingRow (the row
//     renders in QUEUE, not INCOMING: the ticket's whole point).
//   - keep markRuleActivity on the TARGET -> the same (the ticket leaves QUEUE).
//   - the Attach form gated on NeedsReview -> AttachFromTheBoardRoutesAndCloses
//     (the form is not in the popup after the row has been reviewed).
//   - task_attach marks activity on the target -> the same (the ticket appears
//     in INCOMING after the attach).

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/capture"
)

const (
	bcSlug     = "itest-comm-proj"
	bcClient   = "itest-comm-client"
	bcProvider = "itest-comm"
	bcAccount  = "itest-comm@local"
	bcActor    = "capture:itest-comm" // the capture:{connector} shape
	bcJose     = "José Garcia <jose.g@avviato.example>"
	bcKey      = "BCC-9001"
)

func bcRequire0040(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE (table_name='capture_rules' AND column_name='comm_task')
		     OR (table_name='capture_decisions' AND column_name='comm_task_id')`).Scan(&n); err != nil {
		t.Fatalf("probe 0040's columns: %v", err)
	}
	if n != 2 {
		t.Fatalf("found %d of the 2 columns migration 0040 adds. Criterion 1: apply "+
			"migrations/0040_capture_comm_tasks.sql (`make migrate LOCAL_DB_URL=...`)", n)
	}
}

func cleanupComm(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const projs = `(SELECT id FROM projects WHERE slug = '` + bcSlug + `')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	const accts = `(SELECT id FROM source_accounts WHERE provider = '` + bcProvider + `')`
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`
	const msgs = `(SELECT id FROM normalized_messages WHERE raw_source_item_id IN ` + raws + `)`
	for _, q := range []string{
		// capture_decisions SCOPED to this suite's own messages: the capture
		// package deletes them wholesale, this one must not.
		`DELETE FROM capture_decisions WHERE message_id IN ` + msgs,
		`DELETE FROM external_refs WHERE task_id IN ` + tasksOf,
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor LIKE 'itest-comm%'
		      OR actor = '` + bcActor + `')`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor LIKE 'itest-comm%'
		   OR actor = '` + bcActor + `'`,
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM capture_rules WHERE project_id IN ` + projs,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'itest-comm:%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider = '` + bcProvider + `'`,
		`DELETE FROM projects WHERE slug = '` + bcSlug + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

type bcFixture struct {
	pool          *pgxpool.Pool
	project, acct int64
	seq           int
}

func newBCFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *bcFixture {
	t.Helper()
	f := &bcFixture{pool: pool}
	f.project = bdInsID(t, ctx, pool,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,$2,'manual','dashboard','/tmp/itest-comm','any') RETURNING id`, bcSlug, bcClient)
	f.acct = bdInsID(t, ctx, pool,
		`INSERT INTO source_accounts (provider, account_email, send_enabled) VALUES ($1,$2,false) RETURNING id`,
		bcProvider, bcAccount)
	return f
}

// rule seeds the armed rule THROUGH THE COLUMN (IK: test the column, not the
// fixture): rule 75's shape — a body_regex rule on a jira key, comm_task true.
func (f *bcFixture) rule(t *testing.T, ctx context.Context, armed bool) int64 {
	t.Helper()
	return bdInsID(t, ctx, f.pool,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, url_template,
		                            priority, enabled, note, comm_task)
		 VALUES ($1,'body_regex',$2,'jira',$3,'https://itest.jira.example/browse/{key}',90,true,'itest-comm',$4)
		 RETURNING id`, f.project, `\bBCC-[0-9]+\b`, `\b(BCC-[0-9]+)\b`, armed)
}

// message seeds one INBOUND gmail message from a PERSON, on its own thread.
func (f *bcFixture) message(t *testing.T, ctx context.Context, label, body string) int64 {
	t.Helper()
	f.seq++
	tag := label + "-" + strconv.Itoa(f.seq)
	raw := bdInsID(t, ctx, f.pool,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,'{}',$3, now()) RETURNING id`, f.acct, "itest-comm-"+tag, "itest-comm-h-"+tag)
	th := bdInsID(t, ctx, f.pool,
		`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,'itest-comm','[]') RETURNING id`,
		"itest-comm:"+tag)
	return bdInsID(t, ctx, f.pool,
		`INSERT INTO normalized_messages (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		                                  body_text, subject, sender, channel)
		 VALUES ($1,$2,'inbound',$3, now(),$4,$5,$6,'gmail') RETURNING id`,
		raw, th, "<itest-comm-"+tag+"@mail.example>", body, bcKey+" blocks the import", bcJose)
}

func (f *bcFixture) pass(t *testing.T, ctx context.Context) capture.RulesStats {
	t.Helper()
	st, err := capture.EvaluateRules(ctx, f.pool, lightsExecutor(f.pool),
		capture.RulesConfig{Mode: capture.RulesModeLive, Actor: bcActor})
	if err != nil {
		t.Fatalf("capture.EvaluateRules(live): %v", err)
	}
	return st
}

func (f *bcFixture) exec(t *testing.T, ctx context.Context, q string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(ctx, q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func bcIn(secs []lySection, key string, id int64) bool {
	for _, s := range secs {
		if s.key != key {
			continue
		}
		for _, v := range s.ids {
			if v == id {
				return true
			}
		}
	}
	return false
}

func bcAnywhere(secs []lySection, id int64) bool {
	for _, s := range secs {
		for _, v := range s.ids {
			if v == id {
				return true
			}
		}
	}
	return false
}

// bcTicketAndComm runs the whole capture story: a first message creates the
// TICKET task (whose own activity mark is then cleared — swb #491), then the
// armed rule's second message creates the COMM task.
func bcTicketAndComm(t *testing.T, ctx context.Context, f *bcFixture) (ticket, comm, mail int64) {
	t.Helper()
	f.rule(t, ctx, true)
	first := f.message(t, ctx, "first", "please look at "+bcKey+" when you can")
	if st := f.pass(t, ctx); st.TasksCreated != 1 {
		t.Fatalf("setup: the first pass created %d ticket task(s), want 1 (D8: the first message about a NEW "+
			"key becomes the ticket task and NO comm task)", st.TasksCreated)
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT r.task_id FROM external_refs r JOIN tasks t ON t.id = r.task_id
		  WHERE r.system='jira' AND r.external_key=$1 AND t.project_id=$2`, bcKey, f.project).Scan(&ticket); err != nil {
		t.Fatalf("setup: no task linked to %s: %v", bcKey, err)
	}
	// swb #491: the create path marks the new task too, so clear it — this test
	// is about what the COMM pass does, not what the seed did.
	f.exec(t, ctx, `UPDATE tasks SET activity_at = NULL, activity_by_message_id = NULL WHERE id=$1`, ticket)
	_ = first

	mail = f.message(t, ctx, "comm", "Hi Salvador,\n"+bcKey+" still rejects the file. Can you look?")
	if st := f.pass(t, ctx); st.CommTasks != 1 {
		t.Fatalf("setup: the comm pass created %d comm task(s), want 1", st.CommTasks)
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT comm_task_id FROM capture_decisions WHERE message_id=$1 AND mode='live'`, mail).Scan(&comm); err != nil {
		t.Fatalf("setup: no comm_task_id for message %d: %v", mail, err)
	}
	return ticket, comm, mail
}

// ---- criterion 42's board half ---------------------------------------------------------

func TestBoardComm_Integration_TheCommIsItsOwnIncomingRow(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	bcRequire0040(t, ctx, pool)
	cleanupComm(t, ctx, pool)
	defer cleanupComm(t, ctx, pool)

	f := newBCFixture(t, ctx, pool)
	ticket, comm, _ := bcTicketAndComm(t, ctx, f)

	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	body := layoutBoard(t, client, ts.URL, "project="+bcSlug)
	secs := layoutSections(body)
	if !bcIn(secs, "incoming", comm) {
		t.Errorf("the comm task %d is not in section-incoming (%s). \"Usable alone\": José's next mail about "+
			"the key appears as its OWN row at the top of INCOMING — and it gets there through SWT-72's "+
			"activity columns, with NO board change at all (D5)", comm, lyRender(secs, nil))
	}
	if !bcIn(secs, "queue", ticket) {
		t.Errorf("the TICKET task %d is not in QUEUE (%s). D3: arming the rule MOVES the mail from \"the "+
			"ticket task jumps to INCOMING\" to \"a new row in INCOMING, the ticket stays in QUEUE\" — that "+
			"swap IS the ticket", ticket, lyRender(secs, nil))
	}
	if bcIn(secs, "incoming", ticket) {
		t.Errorf("the ticket task is ALSO in INCOMING; surfacing both would double the rows (D11's sentence, " +
			"unchanged)")
	}

	row := boardRow(body, comm)
	if row == "" {
		t.Fatalf("the comm task renders no row")
	}
	if !strings.Contains(row, "new email") {
		t.Errorf("the comm row does not carry the remark `new email` (D5: the remark is activityRemark(channel) "+
			"— `new comment` / `new email` / `new slack`, exactly the words he asked for):\n%s", snippet(row))
	}
	if !strings.Contains(row, "José Garcia") {
		t.Errorf("the comm row does not carry the sender in its title cell (D5):\n%s", snippet(row))
	}
	// D4: the title itself leads with the sender, so it still reads after the
	// muted `from …` span disappears at review.
	if !strings.Contains(row, "José Garcia") || !strings.Contains(row, bcKey) {
		t.Errorf("the comm row's title does not carry the sender and the message's words (criterion 14):\n%s",
			snippet(row))
	}
}

// ---- criteria 37, 38 and 47: Attach from the board --------------------------------------

func TestBoardComm_Integration_AttachFromTheBoardRoutesAndCloses(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	bcRequire0040(t, ctx, pool)
	cleanupComm(t, ctx, pool)
	defer cleanupComm(t, ctx, pool)

	f := newBCFixture(t, ctx, pool)
	ticket, comm, _ := bcTicketAndComm(t, ctx, f)

	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	// The form is IN the popup for every row (criterion 38), including this one.
	body := layoutBoard(t, client, ts.URL, "project="+bcSlug)
	if !strings.Contains(body, "/tasks/"+strconv.FormatInt(comm, 10)+"/attach") {
		t.Fatalf("the rendered board has no Attach form for the comm row (criteria 37, 38)")
	}

	logsBefore := bcCount(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='log'`, ticket)
	before := bcStamps(t, ctx, pool, ticket)

	flash := bcAttach(t, client, ts.URL, comm, ticket, "belongs with the ticket")
	if flash == "" {
		t.Errorf("the Attach redirect carries no flash; every board verb shows its result once")
	}

	// The comm is CLOSED and off the board.
	var status string
	var reviewedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT status, reviewed_at FROM tasks WHERE id=$1`, comm).
		Scan(&status, &reviewedAt); err != nil {
		t.Fatalf("read the comm task: %v", err)
	}
	if status != "closed" {
		t.Errorf("the comm task is %q after being routed, want closed (D7 step 5)", status)
	}
	if reviewedAt == nil {
		t.Errorf("the routed comm carries no reviewed_at; closeTransition stamps it (SWT-72 D8), which is what " +
			"takes the row off INCOMING for good")
	}
	after := layoutSections(layoutBoard(t, client, ts.URL, "project="+bcSlug))
	// Criterion 47's "shows nowhere" read against SWT-57 L1 (amended 2026-09-22
	// on implementation): a row closed TODAY renders in DONE, exactly as a Done
	// tap does — what must be true is that it left every WORKING section.
	for _, key := range []string{"incoming", "queue", "in_flight", "blocked"} {
		if bcIn(after, key, comm) {
			t.Errorf("the routed comm is still in %s (%s); criterion 47: it is closed and belongs to no working "+
				"section", key, lyRender(after, nil))
		}
	}

	// The TICKET gained exactly one ids-only pointer and did NOT move.
	logs := bcCount(t, ctx, pool, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='log'`, ticket)
	if logs != logsBefore+1 {
		t.Errorf("the ticket gained %d log event(s), want exactly 1 (criterion 47)", logs-logsBefore)
	}
	nowStamps := bcStamps(t, ctx, pool, ticket)
	if !bcSameStamp(before.activityAt, nowStamps.activityAt) || !bcSameStamp(before.reviewedAt, nowStamps.reviewedAt) {
		t.Errorf("the ticket's stamps moved (%v/%v -> %v/%v). Criterion 34 / D7: the target is NOT surfaced — "+
			"he just looked at the comm and decided where it belongs; re-raising the destination is the "+
			"double-row D11 refused", before.activityAt, before.reviewedAt, nowStamps.activityAt, nowStamps.reviewedAt)
	}
	if bcIn(after, "incoming", ticket) {
		t.Errorf("the ticket task moved to INCOMING after the attach (%s)", lyRender(after, nil))
	}
	if !bcIn(after, "queue", ticket) {
		t.Errorf("the ticket task left QUEUE after the attach (%s)", lyRender(after, nil))
	}

	// A second Attach of the SAME pair is a clean no-op (criterion 33).
	bcAttach(t, client, ts.URL, comm, ticket, "again")
	if n := bcCount(t, ctx, pool,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='log'`, ticket); n != logs {
		t.Errorf("a second identical Attach wrote %d further log event(s) on the ticket, want 0 (D7 step 3: "+
			"already_attached is a SUCCESS that changes nothing — the board's form is one tap and a "+
			"double-tap must be harmless)", n-logs)
	}
}

// ---- helpers -----------------------------------------------------------------------------

type bcStampRow struct {
	activityAt *time.Time
	reviewedAt *time.Time
}

func bcStamps(t *testing.T, ctx context.Context, pool *pgxpool.Pool, task int64) bcStampRow {
	t.Helper()
	var r bcStampRow
	if err := pool.QueryRow(ctx, `SELECT activity_at, reviewed_at FROM tasks WHERE id=$1`, task).
		Scan(&r.activityAt, &r.reviewedAt); err != nil {
		t.Fatalf("read the stamps of task %d: %v", task, err)
	}
	return r
}

func bcSameStamp(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func bcCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	return bdCount(t, ctx, pool, q, args...)
}

// bcAttach posts the board's per-row Attach form and returns the flash the
// redirect carries. Nothing is faked: the route, the executor, the real policy
// matrix and the handler.
func bcAttach(t *testing.T, client *http.Client, base string, source, target int64, note string) string {
	t.Helper()
	resp, err := client.PostForm(base+"/tasks/"+strconv.FormatInt(source, 10)+"/attach",
		url.Values{
			"target_task_id": {strconv.FormatInt(target, 10)},
			"note":           {note},
			"project":        {bcSlug},
		})
	if err != nil {
		t.Fatalf("POST /tasks/%d/attach: %v", source, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasSuffix(resp.Request.URL.Path, "/tasks") {
		t.Fatalf("POST /tasks/%d/attach landed on %s with status %d, want /tasks (200 after the 303). "+
			"Criterion 37: the route is POST /tasks/{id}/attach on the AUTH-REQUIRED mux, redirecting 303 "+
			"back to the filtered board with a flash", source, resp.Request.URL, resp.StatusCode)
	}
	return resp.Request.URL.Query().Get("flash")
}
