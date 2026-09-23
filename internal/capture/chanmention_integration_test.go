//go:build integration

package capture_test

// TestSWT79_* — slack-channel-mentions (Jira SWT-79,
// docs/tickets/slack-channel-mentions_SPEC.md), the CAPTURE half against a real
// database: criteria 5, 6, 7, 13 (applied schema), 14, and the D8 rule-swap
// ordering property for #a-millon.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_chanmention?sslmode=disable \
//	  TZ=UTC go test -tags integration -p 1 -count=1 -run SWT79 ./internal/capture/
//
// Build-tagged `integration`, env-gated on DATABASE_URL, FATAL on
// 192.168.50.49. NO LLM, NO network. USE AN ISOLATED DATABASE (IK "Test
// infrastructure", 2026-09-12): this suite deletes capture_decisions WHOLESALE
// (EvaluateRules' pending set is global; the rules suites' precedent).
//
// "TEST THE COLUMN, NOT THE FIXTURE": every decision row read here was written
// by a live or shadow capture.EvaluateRules pass over REAL rows — a real rule,
// project and raw item carrying conversation.type — and the assertion reads
// capture_decisions.channel_unmentioned itself. The group-DM fact lives ONLY in
// raw_source_items.raw_json->'conversation'->>'type'; the notifier list only in
// projects.notifier_senders.
//
// MUTATIONS (V3), each turns a named test red:
//   - drop the raw conversation type from pendingMessageCols (select '') →
//     CaptureRecordsTheColumn's notifier group-DM case (it reads as a channel,
//     unmentioned → true) and its person group-DM case (no DM task);
//   - insertDecision omits the column → every `true` case reads false;
//   - apply the fact on only the catch-all exit → the keyless-rule case.
//
// EXPECTED RED TODAY: capture_decisions.channel_unmentioned does not exist
// until migrations/0043_slack_channel_mentions.sql is applied, so every test
// here FAILS AT SETUP (chmRequire0043) with a message naming the migration.
// The rule-swap and applied-migration tests ALSO need the `a-millon` project row
// that 0043 inserts (D6); they read it and never create it, so they stay red
// until 0043 exists, by design.
//
// SIGNATURES: every entry point here exists today (capture.EvaluateRules,
// capture.DryRunRules, capture.ExplainMessage, classify.Store.PendingMessages,
// the capture_rule_add / capture_rule_set_enabled tools), so this file compiles
// and fails on BEHAVIOUR.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
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
	chmSlug         = "itest-chm"
	chmBulkSlug     = "itest-chm-bulk"
	chmProvider     = "itest-chm-src"
	chmMailProvider = "itest-chm-mail"
	chmWS           = "TITESTCHM"
	chmAccount      = "titestchm@slack-web.local" // source_slack_workspace matches pattern+"@slack-web.local"
	chmMailAccount  = "itest-chm@mail.example.test"
	chmSubject      = "itest-chm"
	chmActor        = "capture:itest-chm"
	chmHuman        = "opsctl:itest-chm"
	chmNote         = "itest-chm"
	chmMailSender   = "dana@chm.example.test"
	chmTicket       = `CHM-[0-9]+`
	// chmSuffix is criterion 4's exact reason suffix.
	chmSuffix = "; a channel message that does not mention Salvador: no inquiry verdict (SWT-79)"
	// chmAMillon is the D6 slug migration 0043 creates.
	chmAMillon = "a-millon"
)

type chmSuite struct {
	pool      *pgxpool.Pool
	ex        *executor.Executor
	project   int64
	slackAcct int64
	mailAcct  int64
	threads   map[string]int64
	seq       int
	sentClock time.Time
}

func chmRequire0043(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
	                               WHERE table_name='capture_decisions' AND column_name='channel_unmentioned'`).Scan(&n); err != nil {
		t.Fatalf("probe capture_decisions.channel_unmentioned: %v", err)
	}
	if n != 1 {
		t.Fatalf("capture_decisions.channel_unmentioned does not exist; apply migrations/0043_slack_channel_mentions.sql " +
			"(make migrate LOCAL_DB_URL=...). SWT-79 D2: the fact is a capture-recorded COLUMN the lanes only read")
	}
}

func newCHMSuite(t *testing.T, ctx context.Context) *chmSuite {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db (cleanup deletes capture_decisions " +
			"wholesale); use an isolated compose db on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	chmRequire0043(t, ctx, pool)
	chmCleanup(t, ctx, pool)
	t.Cleanup(func() { chmCleanup(t, context.Background(), pool) })

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	s := &chmSuite{pool: pool, ex: executor.New(reg, checker, audit.NewPGStore(pool)), threads: map[string]int64{}}
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp() - interval '2 hours'`).Scan(&s.sentClock); err != nil {
		t.Fatalf("db clock: %v", err)
	}

	// collaboratory's real shape: inquiry-armed, gate off, the Jira app on the
	// notifier list (0034's column).
	s.project = s.id(t, ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality, ai_classify,
		                       ai_inquiry, inquiry_promote_after, ticket_assignee_gate, notifier_senders)
		 VALUES ($1,$1,'itest-chm-client','manual','dashboard','/tmp/itest-chm','any',false,
		         true, now() - interval '1 day', false, '{Jira}') RETURNING id`, chmSlug)
	// Rules 8/9's shape: the workspace catch-all, NO external_system.
	s.id(t, ctx, `INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, priority, enabled, note)
	              VALUES ($1,'source_slack_workspace',$2,NULL,1,true,$3) RETURNING id`, s.project, chmWS, chmNote)
	// Rule 10's shape: a jira body_regex rule (key = the whole match).
	s.id(t, ctx, `INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, priority, enabled, note)
	              VALUES ($1,'body_regex',$2,'jira',NULL,90,true,$3) RETURNING id`, s.project, chmTicket, chmNote)
	// A keyed rule that derives no key: the second attributed exit.
	s.id(t, ctx, `INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, priority, enabled, note)
	              VALUES ($1,'body_regex','KEYLESS','jira','NEVERMATCHES-([0-9]+)',95,true,$2) RETURNING id`, s.project, chmNote)
	// A gmail sender rule, attribution only.
	s.id(t, ctx, `INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, priority, enabled, note)
	              VALUES ($1,'sender',$2,NULL,5,true,$3) RETURNING id`, s.project, chmMailSender, chmNote)
	s.slackAcct = s.id(t, ctx,
		`INSERT INTO source_accounts (provider, account_email, scopes, send_enabled, calendar_in_availability)
		 VALUES ($1,$2,'{}',false,false) RETURNING id`, chmProvider, chmAccount)
	s.mailAcct = s.id(t, ctx,
		`INSERT INTO source_accounts (provider, account_email, scopes, send_enabled, calendar_in_availability)
		 VALUES ($1,$2,'{}',false,false) RETURNING id`, chmMailProvider, chmMailAccount)
	return s
}

func chmCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE provider IN ('` + chmProvider + `','` + chmMailProvider + `'))`
	const projs = `(SELECT id FROM projects WHERE slug IN ('` + chmSlug + `','` + chmBulkSlug + `'))`
	// Tasks in the suite's projects, plus any task a-millon got from THIS suite's
	// messages (the a-millon project row itself is 0043's and is never deleted).
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `
	                   OR source_thread_id IN (SELECT id FROM normalized_threads WHERE subject='` + chmSubject + `'))`
	const actors = `('` + chmActor + `','` + chmHuman + `')`
	for _, q := range []string{
		`DELETE FROM capture_decisions`, // wholesale: see the header
		`DELETE FROM ticket_status_syncs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM external_refs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM deliveries WHERE task_id IN ` + tasksOf,
		`DELETE FROM classify_promotions WHERE task_id IN ` + tasksOf + ` OR project_id IN ` + projs,
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor IN ` + actors + `)`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor IN ` + actors,
		`DELETE FROM tasks WHERE id IN ` + tasksOf,
		`DELETE FROM capture_rules WHERE project_id IN ` + projs + ` OR note LIKE '` + chmNote + `%'`,
		`DELETE FROM projects WHERE slug IN ('` + chmSlug + `','` + chmBulkSlug + `')`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN
		   (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`,
		`DELETE FROM normalized_threads WHERE subject = '` + chmSubject + `'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider IN ('` + chmProvider + `','` + chmMailProvider + `')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

// ---- fixtures -----------------------------------------------------------------

func (s *chmSuite) id(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var v int64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return v
}

func (s *chmSuite) count(t *testing.T, ctx context.Context, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

func (s *chmSuite) execute(t *testing.T, ctx context.Context, tool, actor string, taskID *int64, args string) []byte {
	t.Helper()
	res, err := s.ex.Execute(ctx, executor.Call{Tool: tool, Actor: actor, Args: []byte(args), TaskID: taskID})
	if err != nil {
		t.Fatalf("%s(%s) as %s: %v", tool, args, actor, err)
	}
	return res.Output
}

func chmKey(conv string) string { return "slack:" + chmWS + ":" + conv }

func (s *chmSuite) thread(t *testing.T, ctx context.Context, key string) int64 {
	t.Helper()
	if id, ok := s.threads[key]; ok {
		return id
	}
	id := s.id(t, ctx, `INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,$2,'[]') RETURNING id`,
		key, chmSubject)
	s.threads[key] = id
	return id
}

// slack seeds one inbound Slack message in conversation conv, typed convType in
// the raw observation exactly as the leaf writes it. key "" = chmKey(conv).
func (s *chmSuite) slack(t *testing.T, ctx context.Context, conv, convType, sender, body string) int64 {
	t.Helper()
	return s.slackKeyed(t, ctx, chmKey(conv), conv, convType, sender, body)
}

func (s *chmSuite) slackKeyed(t *testing.T, ctx context.Context, key, conv, convType, sender, body string) int64 {
	t.Helper()
	s.seq++
	label := fmt.Sprintf("%s-%d", chmSubject, s.seq)
	raw, err := json.Marshal(map[string]any{
		"kind":         "message",
		"workspace":    map[string]any{"id": chmWS, "own_user_id": "U0CHMSELF"},
		"conversation": map[string]any{"id": conv, "name": conv, "type": convType},
		"message":      map[string]any{"id": label, "author": sender, "author_id": "U0CHM" + fmt.Sprint(s.seq), "text": body},
	})
	if err != nil {
		t.Fatalf("marshal raw: %v", err)
	}
	rawID := s.id(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,$3::jsonb,$4, now()) RETURNING id`, s.slackAcct, label, string(raw), "h-"+label)
	return s.id(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1,$2,'inbound',$3,$4,$5,'',$6,'slack') RETURNING id`,
		rawID, s.thread(t, ctx, key), label, s.sentClock.Add(time.Duration(s.seq)*time.Minute), body, sender)
}

// mail seeds one inbound gmail message from sender.
func (s *chmSuite) mail(t *testing.T, ctx context.Context, sender, body string) int64 {
	t.Helper()
	s.seq++
	label := fmt.Sprintf("%s-mail-%d", chmSubject, s.seq)
	rawID := s.id(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,'{}'::jsonb,$3, now()) RETURNING id`, s.mailAcct, label, "h-"+label)
	return s.id(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1,$2,'inbound',$3,$4,$5,'itest-chm mail',$6,'gmail') RETURNING id`,
		rawID, s.thread(t, ctx, "gmail:"+chmMailAccount+":"+label), "<"+label+"@mail.example.test>",
		s.sentClock.Add(time.Duration(s.seq)*time.Minute), body, sender)
}

func (s *chmSuite) pass(t *testing.T, ctx context.Context, mode string) capture.RulesStats {
	t.Helper()
	st, err := capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{Mode: mode, Actor: chmActor})
	if err != nil {
		t.Fatalf("EvaluateRules(%s): %v", mode, err)
	}
	return st
}

type chmDecision struct {
	action     string
	flagged    bool
	project    string
	taskID     *int64
	extSystem  *string
	reason     string
	ambiguous  bool
	matchedIDs []int64
}

func (d chmDecision) String() string {
	return fmt.Sprintf("{action=%s channel_unmentioned=%v project=%s ambiguous=%v reason=%q}",
		d.action, d.flagged, d.project, d.ambiguous, d.reason)
}

// decision reads msg's newest decision in mode, INCLUDING the new column.
func (s *chmSuite) decision(t *testing.T, ctx context.Context, msg int64, mode string) chmDecision {
	t.Helper()
	var d chmDecision
	err := s.pool.QueryRow(ctx,
		`SELECT cd.action, cd.channel_unmentioned, COALESCE(p.slug,''), cd.task_id, cd.external_system,
		        COALESCE(cd.reason,''), cd.ambiguous, cd.matched_rule_ids
		   FROM capture_decisions cd LEFT JOIN projects p ON p.id = cd.project_id
		  WHERE cd.message_id=$1 AND cd.mode=$2 ORDER BY cd.id DESC LIMIT 1`, msg, mode).
		Scan(&d.action, &d.flagged, &d.project, &d.taskID, &d.extSystem, &d.reason, &d.ambiguous, &d.matchedIDs)
	if err != nil {
		t.Fatalf("read %s decision (with channel_unmentioned) for message %d: %v", mode, msg, err)
	}
	return d
}

func (s *chmSuite) inquiryInbox(t *testing.T, ctx context.Context) map[int64]bool {
	t.Helper()
	rows, err := classify.NewStore(s.pool).PendingMessages(ctx,
		classify.Config{Lane: classify.LaneInquiry, Since: 24 * time.Hour})
	if err != nil {
		t.Fatalf("classify PendingMessages(inquiry): %v", err)
	}
	out := map[int64]bool{}
	for _, r := range rows {
		out[r.MessageID] = true
	}
	return out
}

// ---- criterion 5: the column, written by a live pass over real rows -------------

func TestSWT79_CaptureRecordsChannelUnmentioned_FromTheColumn(t *testing.T) {
	ctx := context.Background()
	s := newCHMSuite(t, ctx)

	type want struct {
		action  string
		flagged bool
		why     string
	}
	msgs := map[int64]want{}
	add := func(id int64, w want) { msgs[id] = w }

	add(s.slack(t, ctx, "C0CHMGENERAL", "public_channel", "Dana Ruiz", "can someone confirm the date?"),
		want{"attributed", true, "a public_channel message with no mention: flagged, so qwen never sees it"})
	add(s.slack(t, ctx, "C0CHMGENERAL", "public_channel", "Dana Ruiz", "@Salvador can someone confirm the date?"),
		want{"attributed", false, "the same text with @Salvador: admitted to the inquiry lane (\"qwen decides\")"})
	add(s.slack(t, ctx, "D0CHMPERSON", "dm", "Katie", "can we move the call?"),
		want{"task", false, "a person's 1:1 DM: SWT-78's task (unchanged), and a task is never flagged"})
	add(s.slack(t, ctx, "D0CHMJIRAAPP", "dm", "Jira", "Your daily digest is ready"),
		want{"attributed", false, "a notifier (Jira) DM with no key: attributed, but a DM is never a channel (D11)"})
	add(s.slack(t, ctx, "C0CHMMPDM", "group_dm", "Dana Ruiz", "can one of you look?"),
		want{"task", false, "a person's C… group DM (raw group_dm): the SWT-78 path, unchanged"})
	add(s.slack(t, ctx, "C0CHMMPDMBOT", "group_dm", "Jira", "digest"),
		want{"attributed", false, "a notifier in a C… group DM: attributed, but the RAW type says group DM, never a " +
			"channel (mutation: drop the raw type from pendingMessageCols → true)"})
	add(s.mail(t, ctx, "Dana Ruiz <"+chmMailSender+">", "can you confirm?"),
		want{"attributed", false, "gmail attributed by a sender rule: never flagged (not Slack)"})
	add(s.slack(t, ctx, "C0CHMKEYLESS", "public_channel", "Dana Ruiz", "KEYLESS status please"),
		want{"attributed", true, "a Slack channel message on a keyed rule that derived no key: EVERY attributed exit " +
			"records the fact (the SWT-78 codex lesson)"})
	add(s.slack(t, ctx, "C0CHMENG", "public_channel", "Dana Ruiz", "CHM-1 is broken"),
		want{"task", false, "a Slack channel message on a jira-keyed rule: task as today (rule outcomes unchanged), " +
			"never flagged"})

	s.pass(t, ctx, capture.RulesModeLive)

	for msg, w := range msgs {
		d := s.decision(t, ctx, msg, capture.RulesModeLive)
		if d.action != w.action {
			t.Errorf("message %d: live decision %v, want action=%s — %s", msg, d, w.action, w.why)
			continue
		}
		if d.flagged != w.flagged {
			t.Errorf("message %d: channel_unmentioned = %v, want %v — %s (decision %v)", msg, d.flagged, w.flagged, w.why, d)
		}
		if n := strings.Count(d.reason, chmSuffix); (w.flagged && n != 1) || (!w.flagged && n != 0) {
			t.Errorf("message %d: reason carries the SWT-79 suffix %d time(s) with channel_unmentioned=%v — %s "+
				"(criterion 4: once when true, the reason unchanged otherwise)", msg, n, w.flagged, d.reason)
		}
	}
}

// ---- criterion 6: the CHECK ------------------------------------------------------

func TestSWT79_CheckRefusesAFlaggedNonAttributedRow(t *testing.T) {
	ctx := context.Background()
	s := newCHMSuite(t, ctx)
	msg := s.slack(t, ctx, "C0CHMCHECK", "public_channel", "Dana Ruiz", "hello")

	// Positive control: an attributed row may carry the fact.
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO capture_decisions (message_id, mode, action, project_id, reason, channel_unmentioned)
		 VALUES ($1,'shadow','attributed',$2,'itest-chm probe',true)`, msg, s.project); err != nil {
		t.Fatalf("POSITIVE CONTROL: an attributed row with channel_unmentioned=true was refused: %v", err)
	}
	for _, probe := range []struct {
		action  string
		project *int64
	}{
		{"unmatched", nil},
		{"held", &s.project},
	} {
		_, err := s.pool.Exec(ctx,
			`INSERT INTO capture_decisions (message_id, mode, action, project_id, reason, channel_unmentioned,
			                                matched_rule_id, external_system, external_key)
			 VALUES ($1,'shadow',$2,$3,'itest-chm probe',true, NULL, NULL, NULL)`, msg, probe.action, probe.project)
		if err == nil {
			t.Errorf("a %s row with channel_unmentioned=true was ACCEPTED; D2's CHECK "+
				"(NOT channel_unmentioned OR action = 'attributed') must refuse it", probe.action)
			continue
		}
		if !strings.Contains(err.Error(), "capture_decisions_channel_unmentioned_is_attributed") {
			t.Errorf("a %s row with channel_unmentioned=true failed with %v; want the named CHECK "+
				"capture_decisions_channel_unmentioned_is_attributed", probe.action, err)
		}
	}
}

// ---- criterion 7: shadow, the dry run and task_match show the fact ----------------

func TestSWT79_ShadowDryRunAndExplainShowTheFact(t *testing.T) {
	ctx := context.Background()
	s := newCHMSuite(t, ctx)

	quiet := s.slack(t, ctx, "C0CHMDRY", "public_channel", "Dana Ruiz", "lunch?")
	loud := s.slack(t, ctx, "C0CHMDRY", "public_channel", "Dana Ruiz", "@salvador lunch?")

	counts := func() string {
		var v string
		if err := s.pool.QueryRow(ctx, `SELECT concat_ws(',',
		    (SELECT count(*) FROM tasks WHERE project_id=$1), (SELECT count(*) FROM external_refs),
		    (SELECT count(*) FROM audit_events WHERE actor=$2))`, s.project, chmActor).Scan(&v); err != nil {
			t.Fatalf("counts: %v", err)
		}
		return v
	}
	before := counts()
	s.pass(t, ctx, capture.RulesModeShadow)
	if after := counts(); after != before {
		t.Errorf("a shadow pass wrote: tasks,external_refs,audit %s -> %s (shadow creates nothing)", before, after)
	}
	if d := s.decision(t, ctx, quiet, capture.RulesModeShadow); d.action != "attributed" || !d.flagged ||
		strings.Count(d.reason, chmSuffix) != 1 {
		t.Errorf("shadow decision for the unmentioned channel message = %v; want attributed, channel_unmentioned=true, "+
			"the SWT-79 suffix once (shadow records the same fact)", d)
	}
	if d := s.decision(t, ctx, loud, capture.RulesModeShadow); d.flagged || strings.Contains(d.reason, chmSuffix) {
		t.Errorf("shadow decision for the MENTIONED channel message = %v; want not flagged, no suffix", d)
	}

	// task_match (ExplainMessage) runs the same decideMessage.
	for msg, flagged := range map[int64]bool{quiet: true, loud: false} {
		ex, err := capture.ExplainMessage(ctx, s.pool, msg)
		if err != nil {
			t.Fatalf("ExplainMessage(%d): %v", msg, err)
		}
		if got := strings.Count(ex.Reason, chmSuffix); (flagged && got != 1) || (!flagged && got != 0) {
			t.Errorf("ExplainMessage(%d).Reason = %q; want the SWT-79 suffix %v (criterion 7: task_match shows it)",
				msg, ex.Reason, map[bool]string{true: "exactly once", false: "absent"}[flagged])
		}
	}

	// opsctl capture-rules try (DryRunRules) prints each message's reason.
	var out bytes.Buffer
	if _, err := capture.DryRunRules(ctx, s.pool, capture.DryRunConfig{
		Candidate: capture.CandidateRule{Project: chmSlug, CriteriaType: "thread_key_prefix",
			Pattern: chmKey("C0CHMDRY"), Priority: 50},
		Show: "all", Out: &out,
	}); err != nil {
		t.Fatalf("DryRunRules: %v\n%s", err, out.String())
	}
	reasonOf := func(msg int64) string {
		re := regexp.MustCompile(`(?m)^msg ` + fmt.Sprint(msg) + `\s[\s\S]*?\n  reason:   ([^\n]*)`)
		m := re.FindStringSubmatch(out.String())
		if m == nil {
			t.Fatalf("dry-run output has no block for message %d:\n%s", msg, out.String())
		}
		return m[1]
	}
	if r := reasonOf(quiet); strings.Count(r, chmSuffix) != 1 {
		t.Errorf("dry run reason for the unmentioned channel message = %q; want the SWT-79 suffix once", r)
	}
	if r := reasonOf(loud); strings.Contains(r, chmSuffix) {
		t.Errorf("dry run reason for the mentioned channel message = %q; want no SWT-79 suffix", r)
	}
}

// ---- criteria 13/14: migration 0043 as Postgres applied it -------------------------

func TestSWT79_Migration0043_Integration_Applied(t *testing.T) {
	ctx := context.Background()
	s := newCHMSuite(t, ctx)

	var colDefault, nullable string
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(column_default,''), is_nullable FROM information_schema.columns
	                                 WHERE table_name='capture_decisions' AND column_name='channel_unmentioned'`).
		Scan(&colDefault, &nullable); err != nil {
		t.Fatalf("read the column: %v", err)
	}
	if colDefault != "false" || nullable != "NO" {
		t.Errorf("channel_unmentioned default=%q nullable=%s, want BOOLEAN NOT NULL DEFAULT false (D2: every writer "+
			"that does not name it records \"eligible, as today\")", colDefault, nullable)
	}
	var check string
	if err := s.pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
	                                 WHERE conname='capture_decisions_channel_unmentioned_is_attributed'`).Scan(&check); err != nil {
		t.Fatalf("the named CHECK capture_decisions_channel_unmentioned_is_attributed is missing: %v", err)
	}

	// D6: the a-millon row, every column named.
	var (
		name, execution, delivery, locality string
		client                              *string
		aiClassify, aiInquiry               bool
		promoteAfter                        *time.Time
		notifiers                           []string
	)
	if err := s.pool.QueryRow(ctx,
		`SELECT name, client, execution, delivery, ai_locality, ai_classify, ai_inquiry, inquiry_promote_after,
		        notifier_senders
		   FROM projects WHERE slug=$1`, chmAMillon).
		Scan(&name, &client, &execution, &delivery, &locality, &aiClassify, &aiInquiry, &promoteAfter, &notifiers); err != nil {
		t.Fatalf("project %q (D6, inserted by 0043): %v", chmAMillon, err)
	}
	for _, c := range []struct {
		ok   bool
		what string
	}{
		{client == nil, "client is NULL (load-bearing, 0016: task_get_next's p.client = $1 keeps worker consoles off it)"},
		{execution == "manual" && delivery == "dashboard", "execution/delivery manual/dashboard"},
		{locality == "any", "ai_locality 'any', EXPLICIT (the default local_only would make the drafts lane skip it)"},
		{!aiClassify, "ai_classify false (the personal lane is mail)"},
		{aiInquiry, "ai_inquiry true (the workload flag, 0024 precedent)"},
		{promoteAfter == nil, "inquiry_promote_after NULL (armed by hand at go-live, D7/C-D2; no test DB is armed by a migration)"},
		{len(notifiers) == 0, "notifier_senders '{}'"},
		{name == "#a-millon (Avviato general)", "name '#a-millon (Avviato general)'"},
	} {
		if !c.ok {
			t.Errorf("a-millon row: want %s", c.what)
		}
	}

	// Criterion 14: bulk is unchanged (0018 creates it on every DB).
	var bulkInquiry bool
	if err := s.pool.QueryRow(ctx, `SELECT ai_inquiry FROM projects WHERE slug='bulk'`).Scan(&bulkInquiry); err != nil {
		t.Fatalf("read bulk: %v", err)
	}
	if bulkInquiry {
		t.Errorf("bulk has ai_inquiry=true; SWT-79 must not change bulk (criterion 14)")
	}

	// V4 / criterion 13: the file applies twice cleanly. Re-apply it inside a
	// transaction that is rolled back, so the DB is unchanged either way.
	sqlBytes, err := os.ReadFile("../../migrations/0043_slack_channel_mentions.sql")
	if err != nil {
		t.Fatalf("read migrations/0043_slack_channel_mentions.sql: %v", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
		t.Errorf("0043 does not apply a SECOND time on a database that already has it: %v (criterion 13: "+
			"`applies twice cleanly`)", err)
	}
}

// ---- D8: the rule swap, and "never collaboratory" --------------------------------

// Rule 63 (#a-millon → bulk, priority 99) stays the winner while the new
// a-millon rule at the SAME priority is also enabled: equal priority resolves
// to the LOWER id (rules.go: priority DESC, id ASC), so `add` changes nothing
// yet. After 63 is disabled, #a-millon attributes to a-millon — never to the
// workspace catch-all's armed project (collaboratory) — and a ticket key in the
// channel still creates no collaboratory task (99 outranks rule 10 at 90).
//
// Needs the a-millon row from 0043 (the rule is added THROUGH capture_rule_add,
// which resolves the slug). The rule-63 stand-in is a suite-owned bulk-like
// project, so the real `bulk` is never touched.
func TestSWT79_RuleSwap_AMillonOrderingAndNeverCollaboratory(t *testing.T) {
	ctx := context.Background()
	s := newCHMSuite(t, ctx)
	var amillon int64
	if err := s.pool.QueryRow(ctx, `SELECT id FROM projects WHERE slug=$1`, chmAMillon).Scan(&amillon); err != nil {
		t.Fatalf("project %q is missing: %v — migration 0043 inserts it (D6); apply 0043", chmAMillon, err)
	}

	prefix := chmKey("C0CHMAMILLON")
	bulk := s.id(t, ctx, `INSERT INTO projects (name, slug, client, execution, delivery, ai_locality, ai_classify, ai_inquiry)
	                      VALUES ($1,$1,NULL,'manual','dashboard','local_only',false,false) RETURNING id`, chmBulkSlug)
	rule63 := s.id(t, ctx, `INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, priority, enabled, note)
	                        VALUES ($1,'thread_key_prefix',$2,NULL,99,true,$3) RETURNING id`, bulk, prefix, chmNote+" rule63")

	// Before the add: D8's dry run says the candidate LOSES every message to 63.
	early := s.slack(t, ctx, "C0CHMAMILLON", "public_channel", "Esteban", "buenos dias")
	earlyKey := s.slack(t, ctx, "C0CHMAMILLON", "public_channel", "Esteban", "CHM-5 se cayo")
	var out bytes.Buffer
	sum, err := capture.DryRunRules(ctx, s.pool, capture.DryRunConfig{
		Candidate: capture.CandidateRule{Project: chmAMillon, CriteriaType: "thread_key_prefix", Pattern: prefix, Priority: 99},
		Show:      "wins", Out: &out,
	})
	if err != nil {
		t.Fatalf("DryRunRules(a-millon candidate): %v\n%s", err, out.String())
	}
	if sum.CandidateWon != 0 || sum.CandidateLost != 2 {
		t.Errorf("dry run: candidate won %d, lost %d; want won 0, lost 2 — at equal priority rule 63's LOWER id wins, "+
			"so the add changes nothing until 63 is disabled (D8)\n%s", sum.CandidateWon, sum.CandidateLost, out.String())
	}

	// Step 1: add through the audited tool (humanOnly, opsctl).
	res := s.execute(t, ctx, "capture_rule_add", chmHuman, nil, fmt.Sprintf(
		`{"project":%q,"criteria_type":"thread_key_prefix","pattern":%q,"priority":99,"note":%q}`,
		chmAMillon, prefix, chmNote+" SWT-79 a-millon"))
	var added struct {
		RuleID int64 `json:"rule_id"`
	}
	if err := json.Unmarshal(res, &added); err != nil || added.RuleID <= rule63 {
		t.Fatalf("capture_rule_add result %s (err %v); want a rule id above rule 63's %d", res, err, rule63)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM capture_rules WHERE id=$1 AND project_id=$2 AND external_system IS NULL
	                          AND priority=99 AND enabled`, added.RuleID, amillon); n != 1 {
		t.Fatalf("rule %d is not an enabled, attribution-only priority-99 rule on a-millon (D8 step 1's shape)", added.RuleID)
	}
	s.pass(t, ctx, capture.RulesModeLive)
	for _, m := range []int64{early, earlyKey} {
		d := s.decision(t, ctx, m, capture.RulesModeLive)
		if d.project != chmBulkSlug || d.action != "attributed" {
			t.Errorf("message %d with both rules enabled: %v; want attributed to %s — rule 63 (lower id, same priority) "+
				"still wins (D8 step 1 changes nothing)", m, d, chmBulkSlug)
		}
		if !d.ambiguous {
			t.Errorf("message %d: ambiguous=false with two rules on two projects; D8 expects ambiguous=true in the "+
				"window between add and disable", m)
		}
	}

	// Step 2: disable 63.
	s.execute(t, ctx, "capture_rule_set_enabled", chmHuman, nil, fmt.Sprintf(`{"rule_id":%d,"enabled":false}`, rule63))
	for _, tool := range []string{"capture_rule_add", "capture_rule_set_enabled"} {
		if n := s.count(t, ctx, `SELECT count(*) FROM audit_events WHERE actor=$1 AND tool=$2 AND status='ok'`,
			chmHuman, tool); n != 1 {
			t.Errorf("%d ok audit_events rows for %s as %s, want 1 (invariant 3; criterion 15)", n, tool, chmHuman)
		}
	}

	chatter := s.slack(t, ctx, "C0CHMAMILLON", "public_channel", "Esteban", "alguien tiene el link?")
	mention := s.slack(t, ctx, "C0CHMAMILLON", "public_channel", "Esteban", "@Salvador puedes revisar el deploy?")
	ticket := s.slack(t, ctx, "C0CHMAMILLON", "public_channel", "Esteban", "CHM-6 otra vez caido")
	threadReply := s.slackKeyed(t, ctx, prefix+":p1758550000000100", "C0CHMAMILLON", "public_channel", "Esteban", "igual aqui")
	tasksBefore := s.count(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, s.project)
	s.pass(t, ctx, capture.RulesModeLive)

	for m, w := range map[int64]struct {
		flagged bool
		what    string
	}{
		chatter:     {true, "unmentioned chatter"},
		mention:     {false, "an @Salvador mention"},
		ticket:      {true, "a ticket key, unmentioned"},
		threadReply: {true, "a thread reply, unmentioned"},
	} {
		d := s.decision(t, ctx, m, capture.RulesModeLive)
		if d.project != chmAMillon || d.action != "attributed" {
			t.Errorf("%s (message %d) after 63 is disabled: %v; want attributed to %s — never to %s (collaboratory's "+
				"stand-in, reached by the workspace catch-all or the jira rule)", w.what, m, d, chmAMillon, chmSlug)
		}
		if d.flagged != w.flagged {
			t.Errorf("%s (message %d): channel_unmentioned = %v, want %v", w.what, m, d.flagged, w.flagged)
		}
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, s.project); n != tasksBefore {
		t.Errorf("tasks in %s went %d -> %d; a ticket key in #a-millon must never create a collaboratory task "+
			"(the a-millon rule at 99 outranks rule 10 at 90)", chmSlug, tasksBefore, n)
	}
	if n := s.count(t, ctx, `SELECT count(*) FROM external_refs WHERE system='jira' AND external_key IN ('CHM-5','CHM-6')`); n != 0 {
		t.Errorf("%d external_refs rows for the #a-millon ticket keys; attribution only", n)
	}

	// Usable alone: the mention reaches the inquiry lane under a-millon (ai_inquiry
	// true); the chatter, the ticket key and the reply do not.
	inbox := s.inquiryInbox(t, ctx)
	if !inbox[mention] {
		t.Errorf("the #a-millon @Salvador mention %d is NOT in classify's inquiry inbox; a-millon is ai_inquiry=true "+
			"and a mention is admitted (\"qwen decides\")", mention)
	}
	for _, m := range []int64{chatter, ticket, threadReply} {
		if inbox[m] {
			t.Errorf("unmentioned #a-millon message %d IS in classify's inquiry inbox (\"channels is only when they "+
				"mention me\")", m)
		}
	}
}

// Codex review: a Slack EDIT that adds the mention re-opens the lane. The sink
// re-ingests a changed message (ingested_at bumps, body_text is overwritten);
// the next pass clears the flag from the CURRENT text, so the edited message
// reaches the inquiry inbox. An edit that does not add a mention changes
// nothing. MUTATION: drop the recheck call from EvaluateRules → red.
func TestSWT79_EditAddingTheMentionClearsTheGate(t *testing.T) {
	ctx := context.Background()
	s := newCHMSuite(t, ctx)
	edited := s.slack(t, ctx, "C0CHMEDIT", "public_channel", "Rafael", "can someone check the deploy")
	still := s.slack(t, ctx, "C0CHMEDIT", "public_channel", "Rafael", "deploy is green")
	s.pass(t, ctx, capture.RulesModeLive)
	for _, m := range []int64{edited, still} {
		if d := s.decision(t, ctx, m, capture.RulesModeLive); !d.flagged {
			t.Fatalf("setup: message %d is not flagged unmentioned: %v", m, d)
		}
	}

	// The edits, as the sink applies them: new text, re-ingested after the decision.
	for m, body := range map[int64]string{edited: "@Salvador can you check the deploy", still: "deploy is green again"} {
		if _, err := s.pool.Exec(ctx, `UPDATE normalized_messages SET body_text=$2 WHERE id=$1`, m, body); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(ctx,
			`UPDATE raw_source_items SET ingested_at = now() + interval '1 second'
			  WHERE id = (SELECT raw_source_item_id FROM normalized_messages WHERE id=$1)`, m); err != nil {
			t.Fatal(err)
		}
	}
	s.pass(t, ctx, capture.RulesModeLive)

	if d := s.decision(t, ctx, edited, capture.RulesModeLive); d.flagged || !strings.Contains(d.reason, "edited to mention Salvador") {
		t.Errorf("an edit that added the mention left the gate shut: %v", d)
	}
	if d := s.decision(t, ctx, still, capture.RulesModeLive); !d.flagged {
		t.Errorf("an edit WITHOUT a mention cleared the gate: %v", d)
	}
	inbox := s.inquiryInbox(t, ctx)
	if !inbox[edited] {
		t.Errorf("the edited-in mention %d is not in the inquiry inbox", edited)
	}
	if inbox[still] {
		t.Errorf("the still-unmentioned message %d is in the inquiry inbox", still)
	}
}

// Codex review, round 2: the REVERSE edit. A thread reply that mentioned him
// (eligible) and is edited to remove the mention is flagged on the next pass,
// so it leaves the inbox before promote's thread rule could make a task of it.
func TestSWT79_EditRemovingTheMentionShutsTheGate(t *testing.T) {
	ctx := context.Background()
	s := newCHMSuite(t, ctx)
	m := s.slackKeyed(t, ctx, chmKey("C0CHMREV")+":p1758550000000100", "C0CHMREV", "public_channel", "Rafael",
		"@Salvador can you look")
	s.pass(t, ctx, capture.RulesModeLive)
	if d := s.decision(t, ctx, m, capture.RulesModeLive); d.flagged {
		t.Fatalf("setup: a mention is flagged: %v", d)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE normalized_messages SET body_text='never mind, sorted' WHERE id=$1`, m); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE raw_source_items SET ingested_at = now() + interval '1 second'
		  WHERE id = (SELECT raw_source_item_id FROM normalized_messages WHERE id=$1)`, m); err != nil {
		t.Fatal(err)
	}
	s.pass(t, ctx, capture.RulesModeLive)
	if d := s.decision(t, ctx, m, capture.RulesModeLive); !d.flagged || !strings.Contains(d.reason, "edited to remove") {
		t.Errorf("an edit that removed the mention left the message eligible: %v", d)
	}
	if s.inquiryInbox(t, ctx)[m] {
		t.Errorf("message %d is still in the inquiry inbox after its mention was edited away", m)
	}
}

// go-reviewer, round 2: the race. A pass read the old text, the sink wrote the
// edit, and the decision's created_at is AFTER the re-ingest — the recheck must
// still see it (no time comparison), because the edit is recent.
func TestSWT79_EditRacingTheDecisionIsStillRechecked(t *testing.T) {
	ctx := context.Background()
	s := newCHMSuite(t, ctx)
	m := s.slack(t, ctx, "C0CHMRACE", "public_channel", "Rafael", "anyone around?")
	s.pass(t, ctx, capture.RulesModeLive)
	// The edit landed BEFORE the decision's timestamp: body changed, ingested_at
	// set earlier than the decision's created_at (but within the hour).
	if _, err := s.pool.Exec(ctx, `UPDATE normalized_messages SET body_text='@Salvador anyone around?' WHERE id=$1`, m); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE raw_source_items SET ingested_at = (SELECT min(created_at) FROM capture_decisions WHERE message_id=$1) - interval '1 second'
		  WHERE id = (SELECT raw_source_item_id FROM normalized_messages WHERE id=$1)`, m); err != nil {
		t.Fatal(err)
	}
	s.pass(t, ctx, capture.RulesModeLive)
	if d := s.decision(t, ctx, m, capture.RulesModeLive); d.flagged {
		t.Errorf("an edit that raced the decision was never re-checked: %v", d)
	}
}

// go-reviewer, round 3: the recheck's cost bound. A message ingested more than
// an hour ago is not re-read, even if its text now differs — only a fresh
// (re-)ingest opens the window.
func TestSWT79_RecheckIgnoresMessagesIngestedOverAnHourAgo(t *testing.T) {
	ctx := context.Background()
	s := newCHMSuite(t, ctx)
	m := s.slack(t, ctx, "C0CHMOLD", "public_channel", "Rafael", "old chatter")
	s.pass(t, ctx, capture.RulesModeLive)
	if _, err := s.pool.Exec(ctx, `UPDATE normalized_messages SET body_text='@Salvador old chatter' WHERE id=$1`, m); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE raw_source_items SET ingested_at = now() - interval '2 hours'
		  WHERE id = (SELECT raw_source_item_id FROM normalized_messages WHERE id=$1)`, m); err != nil {
		t.Fatal(err)
	}
	s.pass(t, ctx, capture.RulesModeLive)
	if d := s.decision(t, ctx, m, capture.RulesModeLive); !d.flagged {
		t.Errorf("a message ingested 2h ago was re-checked; the recheck is bounded to the last hour: %v", d)
	}
}

// Codex round 3: the recheck updates the row the inboxes READ — the newest
// decision in ANY mode — not only the pass's own mode. A newer shadow
// `attributed` row over the live one, then an edit removing the mention, then
// a LIVE pass: the shadow row is flagged and the message leaves the inbox.
func TestSWT79_RecheckUpdatesTheNewestDecisionInAnyMode(t *testing.T) {
	ctx := context.Background()
	s := newCHMSuite(t, ctx)
	m := s.slack(t, ctx, "C0CHMMODE", "public_channel", "Rafael", "@Salvador thoughts?")
	s.pass(t, ctx, capture.RulesModeLive)
	s.pass(t, ctx, capture.RulesModeShadow)
	if d := s.decision(t, ctx, m, capture.RulesModeShadow); d.action != "attributed" || d.flagged {
		t.Fatalf("setup: newer shadow row = %v, want attributed and unflagged", d)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE normalized_messages SET body_text='thoughts?' WHERE id=$1`, m); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE raw_source_items SET ingested_at = now() WHERE id = (SELECT raw_source_item_id FROM normalized_messages WHERE id=$1)`, m); err != nil {
		t.Fatal(err)
	}
	s.pass(t, ctx, capture.RulesModeLive)
	if d := s.decision(t, ctx, m, capture.RulesModeShadow); !d.flagged {
		t.Errorf("the newest (shadow) decision was not re-flagged by a live pass: %v", d)
	}
	if s.inquiryInbox(t, ctx)[m] {
		t.Errorf("message %d is in the inquiry inbox after its mention was edited away", m)
	}
}
