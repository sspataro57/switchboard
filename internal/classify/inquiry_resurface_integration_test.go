//go:build integration

package classify_test

// chat-on-closed-task (SWT-53, docs/tickets/chat-on-closed-task_SPEC.md)
// criterion 12, against a real database: classify's inquiry inbox
// (inboxWhereInquiry) admits a message whose LATEST capture decision is a
// task_log that capture recorded with resurface=true, onto a task that is
// STILL closed, in an ai_inquiry project — exactly like an `attributed` one
// (CC5). One fixture per clause it must still exclude.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_isochat?sslmode=disable \
//	  TZ=UTC go test -tags integration -p 1 -count=1 -run ClassifyInquiryResurface ./internal/classify/
//
// Build-tagged `integration`, env-gated on DATABASE_URL, FATAL on
// 192.168.50.49. NO LLM: the inbox is read through Store.PendingMessages, the
// query the lane runs; nothing is classified.
//
// "TEST THE COLUMN": every clause turns on a value Postgres holds —
// capture_decisions.resurface, capture_decisions.task_id, tasks.status as it
// is NOW (re-read, CC5), p.ai_inquiry. MUTATIONS named inline.
//
// Owns and clears: projects itest-ccq-%, provider itest-ccq-src, threads with
// subject itest-ccq (never by key prefix: the slack-key spelling scan).
//
// RED TODAY: capture_decisions has no resurface column; the suite FATALs in
// rsqRequire0034 until 0034 is applied.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	rsqProvider = "itest-ccq-src"
	rsqArmed    = "itest-ccq-armed"
	rsqUnarmed  = "itest-ccq-unarmed"
	rsqSubject  = "itest-ccq"
	rsqWS       = "T0ITESTCCQ"
)

type rsqSuite struct {
	pool           *pgxpool.Pool
	armed, unarmed int64
	account        int64
}

func rsqRequire0034(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
	                               WHERE table_name='capture_decisions' AND column_name='resurface'`).Scan(&n); err != nil {
		t.Fatalf("probe capture_decisions.resurface: %v", err)
	}
	if n != 1 {
		t.Fatalf("capture_decisions.resurface does not exist; apply migrations/0034_chat_on_closed_task.sql " +
			"(make migrate LOCAL_DB_URL=...)")
	}
}

func rsqCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE provider='` + rsqProvider + `')`
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`
	const msgs = `(SELECT id FROM normalized_messages WHERE raw_source_item_id IN ` + raws + `)`
	const projs = `(SELECT id FROM projects WHERE slug LIKE 'itest-ccq-%')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	for _, q := range []string{
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM classify_promotions WHERE normalized_message_id IN ` + msgs,
		`DELETE FROM capture_decisions WHERE message_id IN ` + msgs + ` OR project_id IN ` + projs +
			` OR task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM projects WHERE slug LIKE 'itest-ccq-%'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE subject = '` + rsqSubject + `'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider='` + rsqProvider + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func newRSQSuite(t *testing.T, ctx context.Context) *rsqSuite {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use an isolated compose db on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	rsqRequire0034(t, ctx, pool)
	rsqCleanup(t, ctx, pool)
	t.Cleanup(func() { rsqCleanup(t, context.Background(), pool) })

	s := &rsqSuite{pool: pool}
	// collaboratory's real shape, every flag named (0016/0018/0024's lesson).
	s.armed = s.id(t, ctx, `INSERT INTO projects (name, slug, client, execution, delivery, ai_locality, ai_classify, ai_inquiry)
	                        VALUES ($1,$1,'LlamaSite','manual','dashboard','any',false,true) RETURNING id`, rsqArmed)
	s.unarmed = s.id(t, ctx, `INSERT INTO projects (name, slug, client, execution, delivery, ai_locality, ai_classify, ai_inquiry)
	                          VALUES ($1,$1,'LlamaSite','manual','dashboard','any',false,false) RETURNING id`, rsqUnarmed)
	s.account = s.id(t, ctx, `INSERT INTO source_accounts (provider, account_email, send_enabled)
	                          VALUES ($1,'itest-ccq@pg-main',false) RETURNING id`, rsqProvider)
	return s
}

func (s *rsqSuite) id(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var v int64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return v
}

func (s *rsqSuite) exec(t *testing.T, ctx context.Context, q string, args ...any) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// task writes a bucket-shaped task row directly: the inbox reads tasks.status
// and nothing else about it.
func (s *rsqSuite) task(t *testing.T, ctx context.Context, project int64, label, status string) int64 {
	t.Helper()
	return s.id(t, ctx, `INSERT INTO tasks (project_id, title, body, status) VALUES ($1,$2,'',$3) RETURNING id`,
		project, "itest-ccq "+label, status)
}

// message is a human's inbound Slack DM naming a key, 30 minutes old.
func (s *rsqSuite) message(t *testing.T, ctx context.Context, label string) int64 {
	t.Helper()
	raw := s.id(t, ctx, `INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
	                     VALUES ($1,$2,'{}',$3, now()) RETURNING id`, s.account, "itest-ccq-"+label, "itest-ccq-h-"+label)
	th := s.id(t, ctx, `INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,$2,'[]') RETURNING id`,
		"slack:"+rsqWS+":D0CCQ"+strings.ToUpper(label), rsqSubject)
	return s.id(t, ctx, `INSERT INTO normalized_messages
	                       (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
	                     VALUES ($1,$2,'inbound',$3, now() - interval '30 minutes', 'can you check CCQ-10355?', '', 'asunda45', 'slack')
	                     RETURNING id`, raw, th, "itest-ccq-ext-"+label)
}

// taskLog writes capture's row shape for a message logged onto task.
func (s *rsqSuite) taskLog(t *testing.T, ctx context.Context, msg, project, task int64, resurface bool) {
	t.Helper()
	s.exec(t, ctx, `INSERT INTO capture_decisions (message_id, mode, action, project_id, external_system, external_key,
	                                               task_id, resurface, reason)
	                VALUES ($1,'live','task_log',$2,'jira','CCQ',$3,$4,'itest-ccq')`, msg, project, task, resurface)
}

// MUTATIONS (each turns exactly one assertion red):
//   - drop the `EXISTS … lt.status = 'closed'` from InquiryEligibleLatestSQL →
//     the reopened fixture is admitted;
//   - drop `live.resurface` → the resurface=false fixture is admitted;
//   - drop `p.ai_inquiry` → the unarmed fixture is admitted;
//   - leave the inbox at `latest.action = 'attributed'` → the headline fixture
//     is not admitted (today's behaviour: the message disappears).
func TestClassifyInquiryResurface_Integration_InboxAdmitsAResurfacedTaskLogWhileTheTaskIsClosed(t *testing.T) {
	ctx := context.Background()
	s := newRSQSuite(t, ctx)

	closed := s.task(t, ctx, s.armed, "closed bucket", "closed")
	reopened := s.task(t, ctx, s.armed, "reopened bucket", "closed")
	open := s.task(t, ctx, s.armed, "open task", "in_progress")
	unarmedClosed := s.task(t, ctx, s.unarmed, "unarmed closed bucket", "closed")

	control := s.message(t, ctx, "control")
	s.exec(t, ctx, `INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
	                VALUES ($1,'live','attributed',$2,'itest-ccq')`, control, s.armed)

	admitted := s.message(t, ctx, "admitted")
	s.taskLog(t, ctx, admitted, s.armed, closed, true)

	excluded := map[string]int64{}
	m := s.message(t, ctx, "noflag")
	s.taskLog(t, ctx, m, s.armed, closed, false)
	excluded["resurface=false (a notifier, activity, a dismissal, the connector copy, or pre-0034)"] = m

	m = s.message(t, ctx, "reopened")
	s.taskLog(t, ctx, m, s.armed, reopened, true)
	// T7: something reopened the task after capture decided. The log line is
	// back on the board, so the message leaves the inbox ("still closed" is re-read).
	s.exec(t, ctx, `UPDATE tasks SET status='ready' WHERE id=$1`, reopened)
	excluded["resurface=true whose task has since been reopened (T7)"] = m

	m = s.message(t, ctx, "unarmed")
	s.taskLog(t, ctx, m, s.unarmed, unarmedClosed, true)
	excluded["resurface=true in a project without ai_inquiry (T12)"] = m

	m = s.message(t, ctx, "ontheopen")
	s.taskLog(t, ctx, m, s.armed, open, false)
	excluded["a task_log onto an OPEN task (T6: the log line is on the board)"] = m

	rows, err := classify.NewStore(s.pool).PendingMessages(ctx,
		classify.Config{Lane: classify.LaneInquiry, Since: 24 * time.Hour})
	if err != nil {
		t.Fatalf("PendingMessages(inquiry): %v", err)
	}
	got := map[int64]classify.PendingMessage{}
	for _, r := range rows {
		got[r.MessageID] = r
	}

	if _, ok := got[control]; !ok {
		t.Fatalf("POSITIVE CONTROL: an `attributed` message in the armed project is not in the inquiry inbox; the " +
			"fixture does not reach the lane at all")
	}
	row, ok := got[admitted]
	if !ok {
		t.Errorf("message %d — latest decision task_log with resurface=true onto a task that is STILL closed, in an "+
			"ai_inquiry project — is NOT in the inquiry inbox. CC5: both inboxes accept it exactly like an "+
			"`attributed` message (replyfold.InquiryEligibleLatestSQL). Today it disappears: no board row, no "+
			"inquiry check", admitted)
	} else if row.ProjectID != s.armed {
		t.Errorf("admitted row ProjectID = %d, want %d (the task_log's project is the attribution)", row.ProjectID, s.armed)
	}
	for why, id := range excluded {
		if _, ok := got[id]; ok {
			t.Errorf("%s: message %d is in the inquiry inbox; criterion 12 excludes it", why, id)
		}
	}
}

// CC5b: a SHADOW decision never admits through the resurface branch. The live
// row says task_log with resurface=false (a notifier, say); a newer shadow
// re-evaluation writes task_log with resurface=true. The latest row is the
// shadow one, and it must not make the lane classify the message. The live
// resurfaced message beside it is the positive control.
// MUTATION: drop `AND lcd.mode = 'live'` from InquiryLiveDecisionJoinSQL →
// the shadow row becomes the `live` row, the message is admitted, red.
func TestClassifyInquiryResurface_Integration_AShadowResurfaceAdmitsNothing(t *testing.T) {
	ctx := context.Background()
	s := newRSQSuite(t, ctx)
	closed := s.task(t, ctx, s.armed, "closed bucket shadow", "closed")

	control := s.message(t, ctx, "live-control")
	s.taskLog(t, ctx, control, s.armed, closed, true)

	shadowed := s.message(t, ctx, "shadowed")
	s.taskLog(t, ctx, shadowed, s.armed, closed, false)
	s.exec(t, ctx, `INSERT INTO capture_decisions (message_id, mode, action, project_id, external_system, external_key,
	                                               task_id, resurface, reason)
	                VALUES ($1,'shadow','task_log',$2,'jira','CCQ',$3,true,'itest-ccq shadow re-evaluation')`,
		shadowed, s.armed, closed)

	rows, err := classify.NewStore(s.pool).PendingMessages(ctx,
		classify.Config{Lane: classify.LaneInquiry, Since: 24 * time.Hour})
	if err != nil {
		t.Fatalf("PendingMessages(inquiry): %v", err)
	}
	got := map[int64]bool{}
	for _, r := range rows {
		got[r.MessageID] = true
	}
	if !got[control] {
		t.Fatalf("POSITIVE CONTROL: a LIVE task_log with resurface=true onto a closed task is not in the inquiry inbox")
	}
	if got[shadowed] {
		t.Errorf("message %d — live task_log resurface=false, then a NEWER shadow task_log resurface=true — is in the "+
			"inquiry inbox. CC5b: only a LIVE decision admits through the resurface branch; a shadow row is a "+
			"what-if and must never start the inquiry lane", shadowed)
	}
}

// CC5b, the reverse: shadow decisions never REMOVE a resurfaced message
// either. Each message's live decision is task_log with resurface=true onto a
// closed task; a newer shadow row of another action sits on top. The message
// stays in the inbox, under the LIVE row's project.
// MUTATIONS: read the resurface fact from the any-mode `latest` row → both are
// dropped, red; join projects on latest.project_id → the `unmatched` one (no
// project) is dropped, red.
func TestClassifyInquiryResurface_Integration_ANewerShadowRowRemovesNothing(t *testing.T) {
	ctx := context.Background()
	s := newRSQSuite(t, ctx)
	closed := s.task(t, ctx, s.armed, "closed bucket reverse", "closed")

	shadowRows := map[string]string{
		"unmatched (no project)": `INSERT INTO capture_decisions (message_id, mode, action, reason)
		                           VALUES ($1,'shadow','unmatched','itest-ccq shadow unmatched')`,
		"task_log resurface=false": `INSERT INTO capture_decisions (message_id, mode, action, project_id, external_system,
		                               external_key, task_id, resurface, reason)
		                             VALUES ($1,'shadow','task_log',$2,'jira','CCQ',$3,false,'itest-ccq shadow task_log')`,
	}
	msgs := map[string]int64{}
	i := 0
	for label, q := range shadowRows {
		i++
		m := s.message(t, ctx, "reverse"+strings.Repeat("x", i))
		s.taskLog(t, ctx, m, s.armed, closed, true)
		if strings.Contains(q, "$3") {
			s.exec(t, ctx, q, m, s.armed, closed)
		} else {
			s.exec(t, ctx, q, m)
		}
		msgs[label] = m
	}

	rows, err := classify.NewStore(s.pool).PendingMessages(ctx,
		classify.Config{Lane: classify.LaneInquiry, Since: 24 * time.Hour})
	if err != nil {
		t.Fatalf("PendingMessages(inquiry): %v", err)
	}
	got := map[int64]classify.PendingMessage{}
	for _, r := range rows {
		got[r.MessageID] = r
	}
	for label, m := range msgs {
		row, ok := got[m]
		if !ok {
			t.Errorf("message %d — live task_log resurface=true onto a closed task, under a NEWER shadow %s row — is "+
				"NOT in the inquiry inbox. CC5b: the resurface branch reads only live decisions, so a shadow pass must "+
				"not remove it", m, label)
			continue
		}
		if row.ProjectID != s.armed {
			t.Errorf("%s: admitted under project %d, want the live row's %d", label, row.ProjectID, s.armed)
		}
	}
}
