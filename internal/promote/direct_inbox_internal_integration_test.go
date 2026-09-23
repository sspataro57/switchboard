//go:build integration

package promote

// TestRegression_SWT78_DMSkipsBothInquiryInboxes — bug
// slack-messages-not-becoming-tasks (Jira SWT-78, swb #521;
// docs/bugs/slack-messages-not-becoming-tasks_DIAGNOSIS.md item G.5). "Fix it so
// DMs skip qwen" (Salvador, 2026-09-23), proven on the REAL inbox SQL, not a
// fixture's decision row:
//
//   - classify's inboxWhereInquiry (via classify.Store.PendingMessages, the query
//     the qwen lane runs) — the message is never sent to the model;
//   - promote's inquiryInbox (this package's unexported reader, hence an
//     internal test) — a stale needs_reply=true verdict recorded before the
//     deploy can never promote or gate the message either.
//
// The DM and the channel message are decided by a REAL live capture pass
// (capture.EvaluateRules under rules 8/9's shape: a source_slack_workspace rule
// with no external_system), so the capture_decisions rows are what the fix
// writes, not what this test assumes it writes. The channel message is the
// positive control in both inboxes: without it an inbox that returned nothing
// would pass.
//
// On 2026-09-22 the DM's live decision was `attributed`, so both inboxes held
// it: 16 DMs got qwen needs_reply=false (read by nothing) and 403664 got true
// and was gated `answered` — no task either way.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_dmtasks?sslmode=disable \
//	  TZ=UTC go test -tags integration -p 1 -count=1 -run SWT78 ./internal/promote/
//
// Build-tagged `integration`, env-gated on DATABASE_URL, FATAL on
// 192.168.50.49. NO LLM: the verdicts are ai_runs/ai_extractions rows in the
// classify_inquiry shape, written directly. Isolated database only: the
// capture pass decides every pending inbound message in the database.
//
// Signatures: capture.EvaluateRules, classify.Store.PendingMessages and
// inquiryInbox exist today, so this file compiles and fails on BEHAVIOUR.

import (
	"context"
	"encoding/json"
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
	dmiSlug     = "itest-dmtinbox"
	dmiProvider = "itest-dmtinbox-src"
	dmiWS       = "TITESTDMI"
	dmiAccount  = "titestdmi@slack-web.local"
	dmiSubject  = "itest-dmtinbox"
	dmiModel    = "itest-dmtinbox-model"
	dmiActor    = "capture:itest-dmtinbox"
)

func dmiCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE provider='` + dmiProvider + `')`
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`
	const msgs = `(SELECT id FROM normalized_messages WHERE raw_source_item_id IN ` + raws + `)`
	const projs = `(SELECT id FROM projects WHERE slug='` + dmiSlug + `')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	for _, q := range []string{
		`DELETE FROM classify_promotions WHERE normalized_message_id IN ` + msgs + ` OR project_id IN ` + projs,
		`DELETE FROM capture_decisions`, // wholesale: the capture pass decides the whole pending set
		`DELETE FROM external_refs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor='` + dmiActor + `')`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor='` + dmiActor + `'`,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM capture_rules WHERE project_id IN ` + projs,
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM ai_runs WHERE model='` + dmiModel + `'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE subject='` + dmiSubject + `'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider='` + dmiProvider + `'`,
		`DELETE FROM projects WHERE slug='` + dmiSlug + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func TestRegression_SWT78_DMSkipsBothInquiryInboxes(t *testing.T) {
	ctx := context.Background()
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
	dmiCleanup(t, ctx, pool)
	t.Cleanup(func() { dmiCleanup(t, context.Background(), pool) })

	id := func(q string, args ...any) int64 {
		t.Helper()
		var v int64
		if err := pool.QueryRow(ctx, q, args...).Scan(&v); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		return v
	}

	// collaboratory's shape: inquiry-armed with the promote cutover a day ago.
	project := id(`INSERT INTO projects (name, slug, client, execution, delivery, ai_locality, ai_classify, ai_inquiry,
	                                     inquiry_promote_after, notifier_senders)
	               VALUES ($1,$1,'LlamaSite','manual','dashboard','any',false,true, now() - interval '1 day', '{Jira}')
	               RETURNING id`, dmiSlug)
	id(`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, priority, enabled, note)
	    VALUES ($1,'source_slack_workspace',$2,NULL,1,true,'itest-dmtinbox') RETURNING id`, project, dmiWS)
	account := id(`INSERT INTO source_accounts (provider, account_email, scopes, send_enabled, calendar_in_availability)
	               VALUES ($1,$2,'{}',false,false) RETURNING id`, dmiProvider, dmiAccount)

	type fixture struct{ msg, raw int64 }
	seed := func(label, conv, convType, sender, body string) fixture {
		t.Helper()
		rawJSON, _ := json.Marshal(map[string]any{
			"kind":         "message",
			"workspace":    map[string]any{"id": dmiWS, "own_user_id": "U0DMISELF"},
			"conversation": map[string]any{"id": conv, "name": conv, "type": convType},
			"message":      map[string]any{"id": label, "author": sender, "author_id": "U0DMI" + label, "text": body},
		})
		raw := id(`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		           VALUES ($1,$2,$3::jsonb,$4, now()) RETURNING id`, account, dmiSubject+"-"+label, string(rawJSON), "h-"+label)
		th := id(`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,$2,'[]') RETURNING id`,
			"slack:"+dmiWS+":"+conv, dmiSubject)
		msg := id(`INSERT INTO normalized_messages
		             (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		           VALUES ($1,$2,'inbound',$3, now() - interval '30 minutes', $4, '', $5, 'slack') RETURNING id`,
			raw, th, dmiSubject+"-"+label, body, sender)
		return fixture{msg, raw}
	}
	// 403664's shape: Katie asks a scheduling question in a 1:1 DM.
	dm := seed("dm", "D0DMIKATIE", "dm", "Katie", "can we move tomorrow's call to 3?")
	// A C… group DM (decision 3), from the raw type only.
	group := seed("mpdm", "C0DMIMPDM", "group_dm", "Dana Ruiz", "can one of you confirm the date?")
	// The control: the same ask in a public channel keeps the classifier.
	channel := seed("chan", "C0DMIGENERAL", "public_channel", "Dana Ruiz", "can someone confirm the date?")

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	ex := executor.New(reg, checker, audit.NewPGStore(pool))
	if _, err := capture.EvaluateRules(ctx, pool, ex,
		capture.RulesConfig{Mode: capture.RulesModeLive, Actor: dmiActor}); err != nil {
		t.Fatalf("capture.EvaluateRules(live): %v", err)
	}

	// ---- classify's inquiry inbox: what the qwen lane would read NOW ----
	rows, err := classify.NewStore(pool).PendingMessages(ctx, classify.Config{Lane: classify.LaneInquiry, Since: 24 * time.Hour})
	if err != nil {
		t.Fatalf("classify PendingMessages(inquiry): %v", err)
	}
	inClassify := map[int64]bool{}
	for _, r := range rows {
		inClassify[r.MessageID] = true
	}
	if !inClassify[channel.msg] {
		t.Fatalf("POSITIVE CONTROL: the public-channel message %d is not in classify's inquiry inbox; the fixture "+
			"never reaches the lane, so the DM assertions below would prove nothing", channel.msg)
	}
	for name, f := range map[string]fixture{"1:1 DM": dm, "group DM": group} {
		if inClassify[f.msg] {
			t.Errorf("%s message %d IS in classify's inquiry inbox (inboxWhereInquiry): it goes to qwen3:8b, whose "+
				"needs_reply=false is read by nothing — the 2026-09-22 loss. A person's DM must be decided in capture "+
				"(live task/task_log), which InquiryEligibleLatestSQL already excludes", name, f.msg)
		}
	}

	// ---- promote's inquiry inbox: a verdict recorded before the deploy ----
	// needs_reply=true for all three, recorded 10 minutes ago (after the cutover).
	verdict := func(f fixture, conv string) {
		t.Helper()
		fields, _ := json.Marshal(map[string]any{
			"needs_reply": true, "ask_kind": "scheduling", "asker": "Katie", "ask": "move the call",
			"reason": "asks directly", "sender": "Katie", "subject": "", "channel": "slack",
			"project_id": project, "project_slug": dmiSlug, "normalized_message_id": f.msg,
			"thread_key": "slack:" + dmiWS + ":" + conv, "thread_scope": "conversation",
			"external_message_id": "", "context_messages": 0,
		})
		run := id(`INSERT INTO ai_runs (worker_type, provider, model, input, output, status, created_at)
		           VALUES ('classify_inquiry','ollama',$1, '{}'::jsonb, '{}', 'ok', now() - interval '10 minutes')
		           RETURNING id`, dmiModel)
		id(`INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields) VALUES ($1,$2,$3::jsonb) RETURNING id`,
			run, f.raw, string(fields))
	}
	verdict(dm, "D0DMIKATIE")
	verdict(group, "C0DMIMPDM")
	verdict(channel, "C0DMIGENERAL")

	inbox, err := inquiryInbox(ctx, pool, InquiryMaxAge)
	if err != nil {
		t.Fatalf("inquiryInbox: %v", err)
	}
	inPromote := map[int64]bool{}
	for _, r := range inbox {
		inPromote[r.v.MessageID] = true
	}
	if !inPromote[channel.msg] {
		t.Fatalf("POSITIVE CONTROL: the channel message %d with a needs_reply=true verdict is not in promote's "+
			"inquiryInbox", channel.msg)
	}
	for name, f := range map[string]fixture{"1:1 DM": dm, "group DM": group} {
		if inPromote[f.msg] {
			t.Errorf("%s message %d IS in promote's inquiryInbox: its fate is InquiryGate's (`answered` gated 403664 "+
				"and wrote nothing). A DM's capture decision must take it out of both inquiry inboxes", name, f.msg)
		}
	}

	// And the DM is a task, which is the point.
	var tasks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM capture_decisions WHERE mode='live' AND message_id = ANY($1)
	                                  AND action IN ('task','task_log') AND task_id IS NOT NULL`,
		[]int64{dm.msg, group.msg}).Scan(&tasks); err != nil {
		t.Fatalf("read decisions: %v", err)
	}
	if tasks != 2 {
		t.Errorf("%d of the 2 DM messages have a live task/task_log decision with a task, want 2", tasks)
	}
}
