//go:build integration

package promote

// TestSWT79_* — slack-channel-mentions (Jira SWT-79,
// docs/tickets/slack-channel-mentions_SPEC.md) criterion 10, decisions D1/D2:
// "channels is only when they mention me", proven on the REAL inbox SQL of BOTH
// inquiry inboxes:
//
//   - classify's inboxWhereInquiry (via classify.Store.PendingMessages, the query
//     the qwen lane runs): an unmentioned channel message is never sent to qwen;
//   - promote's inquiryInbox (unexported, hence an internal test): a stale
//     needs_reply=true verdict on it can never promote either.
//
// The decisions are written by a REAL capture pass (capture.EvaluateRules) over
// real rows, so capture_decisions.channel_unmentioned is what capture recorded,
// not what this test assumes. Positive controls in both inboxes: the mentioned
// channel message (and the resurfaced task_log) must be PRESENT, or an inbox
// that returned nothing would pass.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_chanmention?sslmode=disable \
//	  TZ=UTC go test -tags integration -p 1 -count=1 -run SWT79 ./internal/promote/
//
// Build-tagged `integration`, env-gated on DATABASE_URL, FATAL on
// 192.168.50.49. NO LLM: verdicts are ai_runs/ai_extractions rows in the
// classify_inquiry shape, written directly. Isolated database only: the capture
// pass decides every pending inbound message in the database.
//
// MUTATIONS (V3): remove `AND NOT latest.channel_unmentioned` from
// replyfold.InquiryEligibleLatestSQL → the absence assertions go red; select
// the column from the `live` row instead of `latest` → the newer-shadow-row
// assertions go red.
//
// EXPECTED RED TODAY: before 0043 + the capture change, the unmentioned channel
// message is attributed with no fact recorded and sits in BOTH inboxes; the
// shadow-row flip reads a column that does not exist. Signatures all exist today.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	cmiSlug     = "itest-chminbox"
	cmiProvider = "itest-chminbox-src"
	cmiWS       = "TITESTCMI"
	cmiAccount  = "titestcmi@slack-web.local"
	cmiSubject  = "itest-chminbox"
	cmiModel    = "itest-chminbox-model"
	cmiActor    = "capture:itest-chminbox"
	cmiCloser   = "opsctl:itest-chminbox"
)

func cmiCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE provider='` + cmiProvider + `')`
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`
	const msgs = `(SELECT id FROM normalized_messages WHERE raw_source_item_id IN ` + raws + `)`
	const projs = `(SELECT id FROM projects WHERE slug='` + cmiSlug + `')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	const actors = `('` + cmiActor + `','` + cmiCloser + `')`
	for _, q := range []string{
		`DELETE FROM classify_promotions WHERE normalized_message_id IN ` + msgs + ` OR project_id IN ` + projs,
		`DELETE FROM capture_decisions`, // wholesale: the capture pass decides the whole pending set
		`DELETE FROM ticket_status_syncs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM external_refs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor IN ` + actors + `)`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor IN ` + actors,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM capture_rules WHERE project_id IN ` + projs,
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM ai_runs WHERE model='` + cmiModel + `'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE subject='` + cmiSubject + `'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider='` + cmiProvider + `'`,
		`DELETE FROM projects WHERE slug='` + cmiSlug + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

type cmiFixture struct{ msg, raw int64 }

type cmiSuite struct {
	pool    *pgxpool.Pool
	ex      *executor.Executor
	project int64
	account int64
	threads map[string]int64
	seq     int
}

func newCMISuite(t *testing.T, ctx context.Context) *cmiSuite {
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
	cmiCleanup(t, ctx, pool)
	t.Cleanup(func() { cmiCleanup(t, context.Background(), pool) })

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	s := &cmiSuite{pool: pool, ex: executor.New(reg, checker, audit.NewPGStore(pool)), threads: map[string]int64{}}

	// collaboratory's shape: inquiry-armed with the promote cutover a day ago.
	s.project = s.id(t, ctx, `INSERT INTO projects (name, slug, client, execution, delivery, ai_locality, ai_classify,
	                                                ai_inquiry, inquiry_promote_after, notifier_senders)
	                          VALUES ($1,$1,'LlamaSite','manual','dashboard','any',false,true, now() - interval '1 day', '{Jira}')
	                          RETURNING id`, cmiSlug)
	s.id(t, ctx, `INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, priority, enabled, note)
	              VALUES ($1,'source_slack_workspace',$2,NULL,1,true,'itest-chminbox') RETURNING id`, s.project, cmiWS)
	s.id(t, ctx, `INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, priority, enabled, note)
	              VALUES ($1,'body_regex','CMI-[0-9]+','jira',NULL,90,true,'itest-chminbox') RETURNING id`, s.project)
	s.account = s.id(t, ctx, `INSERT INTO source_accounts (provider, account_email, scopes, send_enabled, calendar_in_availability)
	                          VALUES ($1,$2,'{}',false,false) RETURNING id`, cmiProvider, cmiAccount)
	return s
}

func (s *cmiSuite) id(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var v int64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return v
}

func (s *cmiSuite) seed(t *testing.T, ctx context.Context, conv, convType, sender, body string) cmiFixture {
	t.Helper()
	s.seq++
	label := fmt.Sprintf("%s-%d", cmiSubject, s.seq)
	rawJSON, _ := json.Marshal(map[string]any{
		"kind":         "message",
		"workspace":    map[string]any{"id": cmiWS, "own_user_id": "U0CMISELF"},
		"conversation": map[string]any{"id": conv, "name": conv, "type": convType},
		"message":      map[string]any{"id": label, "author": sender, "author_id": "U0CMI" + fmt.Sprint(s.seq), "text": body},
	})
	raw := s.id(t, ctx, `INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
	                     VALUES ($1,$2,$3::jsonb,$4, now()) RETURNING id`, s.account, label, string(rawJSON), "h-"+label)
	key := "slack:" + cmiWS + ":" + conv
	th, ok := s.threads[key]
	if !ok {
		th = s.id(t, ctx, `INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,$2,'[]') RETURNING id`,
			key, cmiSubject)
		s.threads[key] = th
	}
	msg := s.id(t, ctx, `INSERT INTO normalized_messages
	                       (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
	                     VALUES ($1,$2,'inbound',$3, now() - interval '90 minutes' + make_interval(secs => $6), $4, '', $5, 'slack')
	                     RETURNING id`, raw, th, label, body, sender, s.seq)
	return cmiFixture{msg, raw}
}

func (s *cmiSuite) pass(t *testing.T, ctx context.Context, mode string) {
	t.Helper()
	if _, err := capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{Mode: mode, Actor: cmiActor}); err != nil {
		t.Fatalf("capture.EvaluateRules(%s): %v", mode, err)
	}
}

// verdict writes a needs_reply=true classify_inquiry verdict (10 minutes ago,
// after the cutover) for f in conversation conv.
func (s *cmiSuite) verdict(t *testing.T, ctx context.Context, f cmiFixture, conv string) {
	t.Helper()
	fields, _ := json.Marshal(map[string]any{
		"needs_reply": true, "ask_kind": "question", "asker": "Dana Ruiz", "ask": "confirm the date",
		"reason": "asks directly", "sender": "Dana Ruiz", "subject": "", "channel": "slack",
		"project_id": s.project, "project_slug": cmiSlug, "normalized_message_id": f.msg,
		"thread_key": "slack:" + cmiWS + ":" + conv, "thread_scope": "conversation",
		"external_message_id": "", "context_messages": 0,
	})
	run := s.id(t, ctx, `INSERT INTO ai_runs (worker_type, provider, model, input, output, status, created_at)
	                     VALUES ('classify_inquiry','ollama',$1, '{}'::jsonb, '{}', 'ok', now() - interval '10 minutes')
	                     RETURNING id`, cmiModel)
	s.id(t, ctx, `INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields) VALUES ($1,$2,$3::jsonb) RETURNING id`,
		run, f.raw, string(fields))
}

// inboxes returns the message ids in classify's and promote's inquiry inboxes.
// classify's is read BEFORE any verdict exists for a message (its NOT EXISTS
// drops a message once qwen has answered), so callers read it first.
func (s *cmiSuite) classifyInbox(t *testing.T, ctx context.Context) map[int64]bool {
	t.Helper()
	rows, err := classify.NewStore(s.pool).PendingMessages(ctx, classify.Config{Lane: classify.LaneInquiry, Since: 24 * time.Hour})
	if err != nil {
		t.Fatalf("classify PendingMessages(inquiry): %v", err)
	}
	out := map[int64]bool{}
	for _, r := range rows {
		out[r.MessageID] = true
	}
	return out
}

func (s *cmiSuite) promoteInbox(t *testing.T, ctx context.Context) map[int64]bool {
	t.Helper()
	rows, err := inquiryInbox(ctx, s.pool, InquiryMaxAge)
	if err != nil {
		t.Fatalf("inquiryInbox: %v", err)
	}
	out := map[int64]bool{}
	for _, r := range rows {
		out[r.v.MessageID] = true
	}
	return out
}

// ---- criterion 10: absent when unmentioned, present when mentioned -------------

func TestSWT79_UnmentionedChannelMessageSkipsBothInquiryInboxes(t *testing.T) {
	ctx := context.Background()
	s := newCMISuite(t, ctx)

	quiet := s.seed(t, ctx, "C0CMIGENERAL", "public_channel", "Dana Ruiz", "can someone confirm the date?")
	loud := s.seed(t, ctx, "C0CMIGENERAL", "public_channel", "Dana Ruiz", "@Salvador can you confirm the date?")
	s.pass(t, ctx, capture.RulesModeLive)

	in := s.classifyInbox(t, ctx)
	if !in[loud.msg] {
		t.Fatalf("POSITIVE CONTROL: the MENTIONED channel message %d is not in classify's inquiry inbox; the "+
			"absence assertion below would prove nothing (\"qwen decides\" a mention)", loud.msg)
	}
	if in[quiet.msg] {
		t.Errorf("the UNMENTIONED channel message %d IS in classify's inquiry inbox (inboxWhereInquiry): it goes to "+
			"qwen. SWT-79: \"channels is only when they mention me\" — capture records channel_unmentioned and "+
			"InquiryEligibleLatestSQL excludes it", quiet.msg)
	}

	s.verdict(t, ctx, quiet, "C0CMIGENERAL")
	s.verdict(t, ctx, loud, "C0CMIGENERAL")
	pin := s.promoteInbox(t, ctx)
	if !pin[loud.msg] {
		t.Fatalf("POSITIVE CONTROL: the mentioned channel message %d with a needs_reply=true verdict is not in "+
			"promote's inquiryInbox", loud.msg)
	}
	if pin[quiet.msg] {
		t.Errorf("the unmentioned channel message %d IS in promote's inquiryInbox: a verdict recorded before the "+
			"deploy could still promote it. Both inboxes share replyfold.InquiryEligibleLatestSQL", quiet.msg)
	}
}

// Criterion 10, "a newer shadow row decides, in either direction (any-mode
// latest)". The shadow rows are written by a real SHADOW capture pass after the
// message body changed, so each carries the fact capture computed for it.
func TestSWT79_ANewerShadowRowDecidesEitherWay(t *testing.T) {
	ctx := context.Background()
	s := newCMISuite(t, ctx)

	toLoud := s.seed(t, ctx, "C0CMISHADOW", "public_channel", "Dana Ruiz", "anyone around?")
	toQuiet := s.seed(t, ctx, "C0CMISHADOW", "public_channel", "Dana Ruiz", "@Salvador anyone around?")
	s.pass(t, ctx, capture.RulesModeLive)

	// Flip the bodies, then let a shadow pass decide them again (its own mode's
	// pending set: messages with no shadow decision yet).
	for _, u := range []struct {
		m    int64
		body string
	}{{toLoud.msg, "@Salvador anyone around?"}, {toQuiet.msg, "anyone around?"}} {
		if _, err := s.pool.Exec(ctx, `UPDATE normalized_messages SET body_text=$2 WHERE id=$1`, u.m, u.body); err != nil {
			t.Fatalf("update body: %v", err)
		}
	}
	s.pass(t, ctx, capture.RulesModeShadow)
	for _, m := range []int64{toLoud.msg, toQuiet.msg} {
		var mode string
		if err := s.pool.QueryRow(ctx, `SELECT mode FROM capture_decisions WHERE message_id=$1 ORDER BY id DESC LIMIT 1`,
			m).Scan(&mode); err != nil || mode != "shadow" {
			t.Fatalf("fixture: message %d's newest decision is %q (%v), want the shadow pass's row", m, mode, err)
		}
	}

	in := s.classifyInbox(t, ctx)
	if !in[toLoud.msg] {
		t.Errorf("message %d: live row flagged, NEWER shadow row not flagged → want it IN classify's inbox (the "+
			"latest decision in ANY mode decides, C2)", toLoud.msg)
	}
	if in[toQuiet.msg] {
		t.Errorf("message %d: live row not flagged, NEWER shadow row flagged → want it ABSENT from classify's inbox",
			toQuiet.msg)
	}
	s.verdict(t, ctx, toLoud, "C0CMISHADOW")
	s.verdict(t, ctx, toQuiet, "C0CMISHADOW")
	pin := s.promoteInbox(t, ctx)
	if !pin[toLoud.msg] {
		t.Errorf("message %d: want it IN promote's inquiryInbox (newer shadow row not flagged)", toLoud.msg)
	}
	if pin[toQuiet.msg] {
		t.Errorf("message %d: want it ABSENT from promote's inquiryInbox (newer shadow row flagged)", toQuiet.msg)
	}
}

// Criterion 10, SWT-53 unchanged: an UNMENTIONED channel message that capture
// logs onto a CLOSED ticket task with resurface=true is still admitted by the
// resurface branch, byte-identical (it is a task_log, so it is never flagged).
func TestSWT79_ResurfacedChannelTaskLogIsStillAdmitted(t *testing.T) {
	ctx := context.Background()
	s := newCMISuite(t, ctx)

	s.seed(t, ctx, "C0CMIENG", "public_channel", "Dana Ruiz", "please look at CMI-7")
	s.pass(t, ctx, capture.RulesModeLive)
	var ticket int64
	if err := s.pool.QueryRow(ctx, `SELECT r.task_id FROM external_refs r JOIN tasks t ON t.id=r.task_id
	                                 WHERE r.system='jira' AND r.external_key='CMI-7' AND t.project_id=$1`, s.project).
		Scan(&ticket); err != nil {
		t.Fatalf("setup: no ticket task for CMI-7: %v", err)
	}
	if _, err := s.ex.Execute(ctx, executor.Call{Tool: "task_close", Actor: cmiCloser, TaskID: &ticket,
		Args: []byte(fmt.Sprintf(`{"task_id":%d,"reason":"itest-chminbox done"}`, ticket))}); err != nil {
		t.Fatalf("setup: task_close: %v", err)
	}

	again := s.seed(t, ctx, "C0CMIENG", "public_channel", "Dana Ruiz", "CMI-7 is broken again, any news?")
	s.pass(t, ctx, capture.RulesModeLive)
	var action string
	var resurface bool
	if err := s.pool.QueryRow(ctx, `SELECT action, resurface FROM capture_decisions WHERE message_id=$1 AND mode='live'`,
		again.msg).Scan(&action, &resurface); err != nil {
		t.Fatalf("read the resurface decision: %v", err)
	}
	if action != "task_log" || !resurface {
		t.Fatalf("fixture: the channel message onto closed CMI-7 decided %s resurface=%v; want task_log resurface=true", action, resurface)
	}
	if !s.classifyInbox(t, ctx)[again.msg] {
		t.Errorf("the resurfaced (unmentioned) channel task_log %d is NOT in classify's inquiry inbox; SWT-53's "+
			"resurface branch must stay byte-identical", again.msg)
	}
	s.verdict(t, ctx, again, "C0CMIENG")
	if !s.promoteInbox(t, ctx)[again.msg] {
		t.Errorf("the resurfaced (unmentioned) channel task_log %d is NOT in promote's inquiryInbox (SWT-53 unchanged)",
			again.msg)
	}
}
