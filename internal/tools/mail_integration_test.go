//go:build integration

package tools_test

// Integration test for the read-only mail tools and the MCP actor gate (SPEC
// imap-mail-connector, acceptance criteria 16, 17, 19; OQ1 = option A).
// Build-tagged `integration` AND env-gated on DATABASE_URL. Every call goes
// through executor.Execute — the ONLY route to a handler (invariant 3) — with
// the REAL policy Matrix, so the human_only gate actually runs. NO network, no
// IMAP, no LLM: the corpus is seeded straight into normalized_messages.
//
//   DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//     go test -tags integration -run Mail ./internal/tools/
//
// GREENFIELD NOTE: internal/tools/mail.go and the humanActor "mcp:" prefix strip
// do not exist yet, so this fails at the first Execute — the expected failure
// mode. No new seam is imposed beyond criterion 16's tool contract.
//
// Cross-suite discipline (mutual-cleanup pact): the seeded corpus is
// provider='google' with account_email LIKE 'itest-mail-%' and its inbound
// message is visible to triage's GLOBAL pending filter; this file cleans its own
// corpus in FK order, before AND after, and every assertion is scoped to it.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	mailAcct       = "itest-mail-a@example.com"
	mailSlug       = "itest-mail-proj"
	mailThreadKey  = "gmail:itest-mail-a@example.com:<root-itest-mail@acme.example>"
	mailInboundMID = "<root-itest-mail@acme.example>"
	mailReplyMID   = "<sb-itest-mail-1@example.com>"
	mailOtherMID   = "<other-itest-mail@acme.example>"
	mailSlackMID   = "slack:TITESTMAIL:CITESTMAIL:p1"
	mailWorker     = "mcp:worker:itest-mail-worker" // autonomous identity
	mailHuman      = "mcp:manual:itest-mail-salvo"  // interactive session (OQ1 = A)
	mailSender     = "client@itest-mail.example"
	mailSubject    = "Staging login for the itest-mail walkthrough"
)

func mailExecutor(pool *pgxpool.Pool) *executor.Executor {
	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	return executor.New(reg, checker, audit.NewPGStore(pool))
}

func cleanupMailTools(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const scope = `(SELECT id FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-mail-%')`
	stmts := []string{
		`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE actor LIKE '%itest-mail%')`,
		`DELETE FROM audit_events WHERE actor LIKE '%itest-mail%'`,
		`DELETE FROM task_events WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + mailSlug + `'))`,
		`DELETE FROM approvals WHERE subject_type='delivery' AND subject_id IN (SELECT id FROM deliveries WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + mailSlug + `')))`,
		`DELETE FROM deliveries WHERE task_id IN (SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + mailSlug + `'))`,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + mailSlug + `')`,
		`DELETE FROM projects WHERE slug='` + mailSlug + `'`,
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + scope + `)`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + scope + `)`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'gmail:itest-mail-%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + scope,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + scope,
		`DELETE FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-mail-%'`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
}

type mailFixture struct {
	accountID int64
	threadID  int64
	taskID    int64
	draftedID int64
}

func seedMailCorpus(t *testing.T, ctx context.Context, pool *pgxpool.Pool) mailFixture {
	t.Helper()
	var fx mailFixture
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, send_enabled) VALUES ('google',$1,false) RETURNING id`,
		mailAcct).Scan(&fx.accountID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,$2,'[]') RETURNING id`,
		mailThreadKey, mailSubject).Scan(&fx.threadID); err != nil {
		t.Fatalf("seed thread: %v", err)
	}

	seedMsg := func(extID, channel, direction, subject, sender, body string, at time.Time, threadID *int64) {
		t.Helper()
		var rawID int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
			 VALUES ($1,$2,'{"source":"imap"}',$3, now()) RETURNING id`,
			fx.accountID, "imap:INBOX:1:"+extID, "hash-"+extID).Scan(&rawID); err != nil {
			t.Fatalf("seed raw %s: %v", extID, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO normalized_messages
			   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			rawID, threadID, direction, extID, at, body, subject, sender, channel); err != nil {
			t.Fatalf("seed message %s: %v", extID, err)
		}
	}

	base := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	seedMsg(mailInboundMID, "gmail", "inbound", mailSubject, mailSender,
		"The staging login is broken again — can you look today?", base, &fx.threadID)
	seedMsg(mailReplyMID, "gmail", "outbound", "Re: "+mailSubject, mailAcct,
		strings.Repeat("Pushed a fix to staging; details follow. ", 400), base.Add(time.Hour), &fx.threadID)
	seedMsg(mailOtherMID, "gmail", "inbound", "Invoice 2026-07 for the staging login work", "billing@itest-mail.example",
		"Invoice attached for the staging login work.", base.Add(48*time.Hour), nil)
	// A non-gmail channel carrying the same words: mail_search is channel='gmail' only.
	seedMsg(mailSlackMID, "slack", "inbound", "staging login on slack", "someone@itest-mail.example",
		"staging login chatter that mail_search must not return", base.Add(72*time.Hour), nil)

	projID := seedProject(t, ctx, pool, mailSlug, "itest-mail-client")
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'itest-mail work','claude','done_locally') RETURNING id`, projID).Scan(&fx.taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, status, body, subject, from_account_id, thread_id)
		 VALUES ($1,'gmail','drafted','Draft reply body','Re: staging login',$2,$3) RETURNING id`,
		fx.taskID, fx.accountID, fx.threadID).Scan(&fx.draftedID); err != nil {
		t.Fatalf("seed drafted delivery: %v", err)
	}
	return fx
}

type mailSearchResult struct {
	Messages []struct {
		MessageID string `json:"message_id"`
		ThreadID  *int64 `json:"thread_id"`
		ThreadKey string `json:"thread_key"`
		Subject   string `json:"subject"`
		Sender    string `json:"sender"`
		SentAt    string `json:"sent_at"`
		Direction string `json:"direction"`
		Snippet   string `json:"snippet"`
	} `json:"messages"`
	Truncated bool `json:"truncated"`
}

func TestMailTools_Integration_SearchReadThreadAndActorGate(t *testing.T) {
	pool := newToolsPool(t, context.Background())
	defer pool.Close()
	guardRealDB(t)
	ctx := context.Background()

	cleanupMailTools(t, ctx, pool)
	defer cleanupMailTools(t, ctx, pool)
	fx := seedMailCorpus(t, ctx, pool)

	ex := mailExecutor(pool)
	tasksBefore := mailScanInt(t, ctx, pool, `SELECT count(*) FROM tasks`)
	deliveriesBefore := mailScanInt(t, ctx, pool, `SELECT count(*) FROM deliveries`)

	// ---- criterion 16: mail_search --------------------------------------------

	t.Run("query matches subject/sender/body, gmail only", func(t *testing.T) {
		res, err := ex.Execute(ctx, executor.Call{
			Tool: "mail_search", Actor: mailWorker, Args: []byte(`{"query":"staging login"}`)})
		if err != nil {
			t.Fatalf("mail_search: %v", err)
		}
		var out mailSearchResult
		if err := json.Unmarshal(res.Output, &out); err != nil {
			t.Fatalf("mail_search output %s: %v", res.Output, err)
		}
		got := map[string]bool{}
		for _, m := range out.Messages {
			got[m.MessageID] = true
			if len(m.Snippet) > 300 {
				t.Errorf("snippet for %s is %d chars, want <= 300", m.MessageID, len(m.Snippet))
			}
		}
		for _, want := range []string{mailInboundMID, mailReplyMID, mailOtherMID} {
			if !got[want] {
				t.Errorf("mail_search did not return %s (results: %v)", want, got)
			}
		}
		if got[mailSlackMID] {
			t.Errorf("mail_search returned a slack-channel message; it is channel='gmail' only")
		}
		// sent_at DESC.
		for i := 1; i < len(out.Messages); i++ {
			if out.Messages[i-1].SentAt < out.Messages[i].SentAt {
				t.Errorf("results are not ordered sent_at DESC: %s before %s",
					out.Messages[i-1].SentAt, out.Messages[i].SentAt)
			}
		}
	})

	t.Run("from and thread_key selectors", func(t *testing.T) {
		res, err := ex.Execute(ctx, executor.Call{
			Tool: "mail_search", Actor: mailWorker, Args: []byte(`{"from":"` + mailSender + `"}`)})
		if err != nil {
			t.Fatalf("mail_search(from): %v", err)
		}
		var out mailSearchResult
		if err := json.Unmarshal(res.Output, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(out.Messages) != 1 || out.Messages[0].MessageID != mailInboundMID {
			t.Errorf("mail_search(from=%s) = %+v, want just the inbound message", mailSender, out.Messages)
		}

		res, err = ex.Execute(ctx, executor.Call{
			Tool: "mail_search", Actor: mailWorker, Args: []byte(`{"thread_key":"` + mailThreadKey + `"}`)})
		if err != nil {
			t.Fatalf("mail_search(thread_key): %v", err)
		}
		out = mailSearchResult{}
		if err := json.Unmarshal(res.Output, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(out.Messages) != 2 {
			t.Errorf("mail_search(thread_key) returned %d messages, want the 2 in the thread", len(out.Messages))
		}
	})

	t.Run("limit bounds the result set", func(t *testing.T) {
		res, err := ex.Execute(ctx, executor.Call{
			Tool: "mail_search", Actor: mailWorker, Args: []byte(`{"query":"staging login","limit":1}`)})
		if err != nil {
			t.Fatalf("mail_search(limit): %v", err)
		}
		var out mailSearchResult
		if err := json.Unmarshal(res.Output, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(out.Messages) != 1 {
			t.Fatalf("mail_search(limit=1) returned %d messages", len(out.Messages))
		}
		if out.Messages[0].MessageID != mailOtherMID {
			t.Errorf("limit=1 returned %s, want the newest match %s", out.Messages[0].MessageID, mailOtherMID)
		}
		if !out.Truncated {
			t.Errorf("truncated = false though more matches existed than the limit")
		}
	})

	// ---- criterion 16: mail_read_thread ---------------------------------------

	t.Run("read thread by key and by id", func(t *testing.T) {
		var out struct {
			ThreadKey string `json:"thread_key"`
			Subject   string `json:"subject"`
			Messages  []struct {
				MessageID string `json:"message_id"`
				Sender    string `json:"sender"`
				SentAt    string `json:"sent_at"`
				Direction string `json:"direction"`
				BodyText  string `json:"body_text"`
			} `json:"messages"`
		}
		res, err := ex.Execute(ctx, executor.Call{
			Tool: "mail_read_thread", Actor: mailWorker, Args: []byte(`{"thread_key":"` + mailThreadKey + `"}`)})
		if err != nil {
			t.Fatalf("mail_read_thread(thread_key): %v", err)
		}
		if err := json.Unmarshal(res.Output, &out); err != nil {
			t.Fatalf("mail_read_thread output %s: %v", res.Output, err)
		}
		if out.ThreadKey != mailThreadKey {
			t.Errorf("thread_key = %q, want %q", out.ThreadKey, mailThreadKey)
		}
		if len(out.Messages) != 2 {
			t.Fatalf("thread messages = %d, want 2", len(out.Messages))
		}
		if out.Messages[0].MessageID != mailInboundMID || out.Messages[1].MessageID != mailReplyMID {
			t.Errorf("messages are not ordered sent_at ASC: %s then %s",
				out.Messages[0].MessageID, out.Messages[1].MessageID)
		}
		if out.Messages[0].Direction != "inbound" || out.Messages[1].Direction != "outbound" {
			t.Errorf("directions = %s/%s, want inbound/outbound",
				out.Messages[0].Direction, out.Messages[1].Direction)
		}
		if len(out.Messages[1].BodyText) > 8*1024 {
			t.Errorf("body_text is %d bytes, want capped at 8 KiB", len(out.Messages[1].BodyText))
		}

		args, _ := json.Marshal(map[string]any{"thread_id": fx.threadID})
		if _, err := ex.Execute(ctx, executor.Call{Tool: "mail_read_thread", Actor: mailWorker, Args: args}); err != nil {
			t.Fatalf("mail_read_thread(thread_id): %v", err)
		}
	})

	// ---- invariant 3: the audit rows ------------------------------------------

	t.Run("every call left an ok audit row", func(t *testing.T) {
		for _, tool := range []string{"mail_search", "mail_read_thread"} {
			if got := mailScanInt(t, ctx, pool,
				`SELECT count(*) FROM audit_events WHERE tool=$1 AND actor=$2 AND status='ok'`, tool, mailWorker); got < 1 {
				t.Errorf("no ok audit_events row for %s (invariant 3)", tool)
			}
		}
	})

	// ---- criterion 17: the actor gate -----------------------------------------

	t.Run("worker identity is denied on approve_delivery and send_delivery", func(t *testing.T) {
		for _, tool := range []string{"approve_delivery", "send_delivery"} {
			tool := tool
			t.Run(tool, func(t *testing.T) {
				args, _ := json.Marshal(map[string]any{"delivery_id": fx.draftedID})
				_, err := ex.Execute(ctx, executor.Call{Tool: tool, Actor: mailWorker, Args: args})
				if err == nil {
					t.Fatalf("%s by %q succeeded; an autonomous worker must never approve or send", tool, mailWorker)
				}
				if !strings.Contains(err.Error(), "human_only") {
					t.Errorf("%s denial = %v, want rule human_only", tool, err)
				}
				if got := mailScanInt(t, ctx, pool,
					`SELECT count(*) FROM audit_events WHERE tool=$1 AND actor=$2 AND status='denied'`, tool, mailWorker); got < 1 {
					t.Errorf("no denied audit_events row for %s (the denial must be audited)", tool)
				}
				if got := mailScanInt(t, ctx, pool,
					`SELECT count(*) FROM policy_decisions p JOIN audit_events a ON a.id=p.audit_event_id
					 WHERE a.actor=$1 AND p.tool=$2 AND p.decision='deny' AND p.rule='human_only'`, mailWorker, tool); got < 1 {
					t.Errorf("no policy_decisions deny/human_only row for %s", tool)
				}
				// The audit row keeps the FULL, unmodified actor (OQ1 = A).
				if got := mailScanInt(t, ctx, pool,
					`SELECT count(*) FROM audit_events WHERE tool=$1 AND actor=$2`, tool, mailWorker); got < 1 {
					t.Errorf("the audit row does not carry the full actor string %q", mailWorker)
				}
				// Still drafted: nothing moved.
				if got := mailScanInt(t, ctx, pool,
					`SELECT count(*) FROM deliveries WHERE id=$1 AND status='drafted'`, fx.draftedID); got != 1 {
					t.Errorf("delivery %d left 'drafted' on a denied call", fx.draftedID)
				}
			})
		}
	})

	t.Run("the interactive session passes the gate", func(t *testing.T) {
		args, _ := json.Marshal(map[string]any{"delivery_id": fx.draftedID})
		if _, err := ex.Execute(ctx, executor.Call{Tool: "approve_delivery", Actor: mailHuman, Args: args}); err != nil {
			t.Fatalf("approve_delivery by %q: %v (OQ1 = A: one optional mcp: transport prefix is stripped)", mailHuman, err)
		}
		if got := mailScanInt(t, ctx, pool,
			`SELECT count(*) FROM deliveries WHERE id=$1 AND status='approved'`, fx.draftedID); got != 1 {
			t.Errorf("delivery %d is not approved after a human MCP call", fx.draftedID)
		}
		if got := mailScanInt(t, ctx, pool,
			`SELECT count(*) FROM audit_events WHERE tool='approve_delivery' AND actor=$1 AND status='ok'`, mailHuman); got != 1 {
			t.Errorf("the ok audit row does not carry the full actor %q", mailHuman)
		}
	})

	// ---- criterion 19: the mail tools create nothing ---------------------------

	if got := mailScanInt(t, ctx, pool, `SELECT count(*) FROM tasks`); got != tasksBefore {
		t.Errorf("tasks changed: before=%d after=%d (mail_search/mail_read_thread create zero tasks)", tasksBefore, got)
	}
	if got := mailScanInt(t, ctx, pool, `SELECT count(*) FROM deliveries`); got != deliveriesBefore {
		t.Errorf("deliveries changed: before=%d after=%d", deliveriesBefore, got)
	}
}

func mailScanInt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}
