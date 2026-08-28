//go:build integration

package capture_test

// Integration tests for outbound-capture (SWT-16,
// docs/tickets/outbound-capture_SPEC.md). Build-tagged `integration` AND env-gated
// on DATABASE_URL. No Gmail, no Slack, no Jira, no LLM: the pass reads normalized
// tables and writes task_events, so every fixture is plain SQL.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -run Capture ./internal/capture/
//
// GREENFIELD NOTE: internal/capture does not exist yet, so this file compile-FAILs
// on capture.ObserveOutbound / capture.Gmail / capture.Slack / capture.Jira — the
// expected red state.
//
// IMPOSED surface (criterion 1 pins the function; the channel bindings are values
// of the Channel type it takes):
//
//	func ObserveOutbound(ctx context.Context, pool *pgxpool.Pool, ch Channel) (observed int, err error)
//	var Gmail, Slack, Jira Channel
//
// `observed` is asserted exactly only where "messages observed" and "events
// appended" coincide (one message, one linked task); the fan-out cases assert the
// task_events instead, because the SPEC does not say which of the two the counter
// reports.
//
// Cross-suite discipline (the serialized cleanup pact): this suite owns the project
// slug 'itest-capture-proj', the three synthetic accounts below, and the thread_key
// prefixes gmail:itest-capture:%, slack:TCAPTURE:%, jira:capture.local:% — and
// cleans exactly those in FK order, rerunnably, at start and end.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	capProjectSlug = "itest-capture-proj"

	capGoogleAccount = "itest-capture@gmail.example.test"
	capSlackAccount  = "tcapture@slack-web.local"
	capJiraAccount   = "itest-capture-jira@example.test"

	capGmailThreadPrefix = "gmail:itest-capture:"
	capSlackThreadPrefix = "slack:TCAPTURE:"
	capJiraThreadPrefix  = "jira:capture.local:"

	capSlackConversationRef = "https://app.slack.com/client/TCAPTURE/CCAPTURE"
)

var capAccountEmails = map[string]string{
	"google":    capGoogleAccount,
	"slack_web": capSlackAccount,
	"jira":      capJiraAccount,
}

// ---- harness --------------------------------------------------------------------

type captureSuite struct {
	pool      *pgxpool.Pool
	projectID int64
}

func newCaptureSuite(t *testing.T, ctx context.Context) *captureSuite {
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
	cleanupCapture(t, ctx, pool)
	t.Cleanup(func() { cleanupCapture(t, ctx, pool) })

	s := &captureSuite{pool: pool}
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,'itest-capture','manual','dashboard','/tmp/itest-capture', 'any') RETURNING id`,
		capProjectSlug).Scan(&s.projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return s
}

func cleanupCapture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE (provider, account_email) IN (
	                 ('google','` + capGoogleAccount + `'),
	                 ('slack_web','` + capSlackAccount + `'),
	                 ('jira','` + capJiraAccount + `')))`
	const tasks = `(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + capProjectSlug + `'))`
	queries := []string{
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`,
		`DELETE FROM task_events WHERE task_id IN ` + tasks,
		`DELETE FROM task_claims WHERE task_id IN ` + tasks,
		`DELETE FROM deliveries WHERE task_id IN ` + tasks,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + capProjectSlug + `')`,
		`DELETE FROM projects WHERE slug='` + capProjectSlug + `'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE '` + capGmailThreadPrefix + `%'
		    OR thread_key LIKE '` + capSlackThreadPrefix + `%' OR thread_key LIKE '` + capJiraThreadPrefix + `%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE (provider, account_email) IN (
		   ('google','` + capGoogleAccount + `'),
		   ('slack_web','` + capSlackAccount + `'),
		   ('jira','` + capJiraAccount + `'))`,
	}
	for _, query := range queries {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatalf("cleanup %q: %v", query, err)
		}
	}
}

func (s *captureSuite) accountID(t *testing.T, ctx context.Context, provider string) int64 {
	t.Helper()
	email, ok := capAccountEmails[provider]
	if !ok {
		t.Fatalf("no test account for provider %q", provider)
	}
	var id int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, scopes, send_enabled, calendar_in_availability)
		 VALUES ($1,$2,'{}',false,false)
		 ON CONFLICT (provider, account_email) DO UPDATE SET account_email=EXCLUDED.account_email
		 RETURNING id`, provider, email).Scan(&id); err != nil {
		t.Fatalf("seed %s source account: %v", provider, err)
	}
	return id
}

func (s *captureSuite) task(t *testing.T, ctx context.Context, title string) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,$2,'claude','in_progress') RETURNING id`, s.projectID, title).Scan(&id); err != nil {
		t.Fatalf("seed task %q: %v", title, err)
	}
	return id
}

// msgFixture is one normalized message plus the raw row invariant 1 requires
// behind it (raw_source_item_id is NOT NULL).
type msgFixture struct {
	provider  string // source_accounts.provider
	channel   string // normalized_messages.channel: gmail | slack | jira
	direction string // inbound | outbound
	threadKey string
	extID     any    // string, "" or nil (NULL) — nil/"" is "unclaimed" per criterion 2
	sentAgo   string // interval literal, e.g. "2 hours"
	body      string
	sender    string
	rawID     string // raw_source_items.external_id, unique per account
}

func (s *captureSuite) message(t *testing.T, ctx context.Context, f msgFixture) (msgID, threadID int64) {
	t.Helper()
	accountID := s.accountID(t, ctx, f.provider)
	var rawID int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,'{}',$3, now()) RETURNING id`,
		accountID, f.rawID, "itest-capture-"+f.rawID).Scan(&rawID); err != nil {
		t.Fatalf("seed raw item %s: %v", f.rawID, err)
	}
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO normalized_threads (raw_source_item_id, thread_key, subject, participants)
		 VALUES ($1,$2,$3,'[]')
		 ON CONFLICT (thread_key) WHERE thread_key IS NOT NULL DO UPDATE SET subject=EXCLUDED.subject
		 RETURNING id`, rawID, f.threadKey, "itest capture "+f.channel).Scan(&threadID); err != nil {
		t.Fatalf("seed thread %s: %v", f.threadKey, err)
	}
	sentAt := "now()"
	if f.sentAgo != "" {
		sentAt = "now() - interval '" + f.sentAgo + "'"
	}
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		    body_text, subject, sender, channel)
		 VALUES ($1,$2,$3,$4,`+sentAt+`,$5,$6,$7,$8) RETURNING id`,
		rawID, threadID, f.direction, f.extID, f.body, "itest capture", f.sender, f.channel).Scan(&msgID); err != nil {
		t.Fatalf("seed %s message %s: %v", f.channel, f.rawID, err)
	}
	return msgID, threadID
}

// renormalize models `--all`: the connector upserts the same raw item again, which
// (ON CONFLICT (raw_source_item_id)) keeps normalized_messages.id — the dedup key
// criterion 6 pins.
func (s *captureSuite) renormalize(t *testing.T, ctx context.Context, msgID int64) {
	t.Helper()
	var again int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		    body_text, subject, sender, channel)
		 SELECT raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		        body_text, subject, sender, channel
		   FROM normalized_messages WHERE id=$1
		 ON CONFLICT (raw_source_item_id) DO UPDATE SET
		   body_text=EXCLUDED.body_text, sent_at=EXCLUDED.sent_at
		 RETURNING id`, msgID).Scan(&again); err != nil {
		t.Fatalf("re-normalize message %d: %v", msgID, err)
	}
	if again != msgID {
		t.Fatalf("re-normalization changed normalized_messages.id (%d -> %d); the dedup key would not be stable", msgID, again)
	}
}

// delFixture is one deliveries row. The join columns are the point: thread_id for
// gmail, target_ref for slack_reply/jira_comment.
type delFixture struct {
	taskID         int64
	channel        string
	targetRef      any
	threadID       any
	status         string
	sentExternalID any
	confirmed      bool
	sentAgo        string // "" = sent_at NULL
	attemptedAgo   string // "" = send_attempted_at NULL
	settled        bool
	body           string
}

func (s *captureSuite) delivery(t *testing.T, ctx context.Context, f delFixture) int64 {
	t.Helper()
	sentAt, attempted, settled, confirmed := "NULL", "NULL", "NULL", "NULL"
	if f.sentAgo != "" {
		sentAt = "now() - interval '" + f.sentAgo + "'"
	}
	if f.attemptedAgo != "" {
		attempted = "now() - interval '" + f.attemptedAgo + "'"
	}
	if f.settled {
		settled = "now()"
	}
	if f.confirmed {
		confirmed = "now()"
	}
	body := f.body
	if body == "" {
		body = "itest capture delivery body"
	}
	var id int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO deliveries
		   (task_id, channel, target_ref, thread_id, body, status, sent_external_id,
		    approval_source, sent_at, send_attempted_at, send_settled_at, confirmed_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,'switchboard',`+sentAt+`,`+attempted+`,`+settled+`,`+confirmed+`)
		 RETURNING id`,
		f.taskID, f.channel, f.targetRef, f.threadID, body, f.status, f.sentExternalID).Scan(&id); err != nil {
		t.Fatalf("seed %s delivery (apply migrations 0011 + 0012): %v", f.channel, err)
	}
	return id
}

func (s *captureSuite) eventCount(t *testing.T, ctx context.Context, taskID int64, eventType string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type=$2`, taskID, eventType).Scan(&n); err != nil {
		t.Fatalf("count %s events: %v", eventType, err)
	}
	return n
}

func (s *captureSuite) projectEventCount(t *testing.T, ctx context.Context, eventType string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM task_events WHERE event_type=$1
		   AND task_id IN (SELECT id FROM tasks WHERE project_id=$2)`, eventType, s.projectID).Scan(&n); err != nil {
		t.Fatalf("count project %s events: %v", eventType, err)
	}
	return n
}

type observedPayload struct {
	MessageID         int64  `json:"message_id"`
	ExternalMessageID string `json:"external_message_id"`
	Channel           string `json:"channel"`
	ThreadKey         string `json:"thread_key"`
	SentAt            string `json:"sent_at"`
	Sender            string `json:"sender"`
	BodyPreview       string `json:"body_preview"`
}

func (s *captureSuite) observedPayloads(t *testing.T, ctx context.Context, taskID int64) []observedPayload {
	t.Helper()
	rows, err := s.pool.Query(ctx,
		`SELECT payload::text FROM task_events
		  WHERE task_id=$1 AND event_type='outbound_observed' ORDER BY id`, taskID)
	if err != nil {
		t.Fatalf("read outbound_observed payloads: %v", err)
	}
	defer rows.Close()
	var out []observedPayload
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan payload: %v", err)
		}
		var p observedPayload
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatalf("unmarshal payload %s: %v", raw, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate payloads: %v", err)
	}
	return out
}

func (s *captureSuite) observe(t *testing.T, ctx context.Context, ch capture.Channel) int {
	t.Helper()
	n, err := capture.ObserveOutbound(ctx, s.pool, ch)
	if err != nil {
		t.Fatalf("ObserveOutbound: %v", err)
	}
	return n
}

// bodyPreview is criterion 5's rule, restated: whitespace-normalized, first 120
// runes (slackweb.normalizedPrefix with slackMatchPrefixLen).
func bodyPreview(body string) string {
	normalized := strings.Join(strings.Fields(body), " ")
	runes := []rune(normalized)
	if len(runes) > 120 {
		runes = runes[:120]
	}
	return string(runes)
}

// ---- gmail: detection, event shape, idempotence (criteria 2, 4, 5, 6) -----------

func TestCapture_Integration_GmailObservesADirectSendAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newCaptureSuite(t, ctx)
	taskID := s.task(t, ctx, "gmail thread with a delivery")

	const threadKey = capGmailThreadPrefix + "t-direct"
	const directID = "<direct-from-the-gmail-app@mail.example.test>"
	// Whitespace-noisy and longer than 120 chars: the payload must carry the
	// normalized 120-rune prefix, not the raw body.
	const body = "Sent this straight from the Gmail app on my phone,\n\n   so switchboard never had a delivery row for it — " +
		"but the task log must still say the client was answered."

	msgID, threadID := s.message(t, ctx, msgFixture{
		provider: "google", channel: "gmail", direction: "outbound", threadKey: threadKey,
		extID: directID, sentAgo: "2 hours", body: body, sender: "salvo@example.test",
		rawID: "gmail:direct:1",
	})
	// Same thread, INBOUND, also unclaimed: capture is outbound-only.
	s.message(t, ctx, msgFixture{
		provider: "google", channel: "gmail", direction: "inbound", threadKey: threadKey,
		extID: "<client-reply@client.example.test>", sentAgo: "3 hours",
		body: "the client's own message", sender: "client@client.example.test",
		rawID: "gmail:direct:inbound:1",
	})
	// The stored thread→task link: any status, including drafted (SPEC "the crux").
	s.delivery(t, ctx, delFixture{taskID: taskID, channel: "gmail", threadID: threadID, status: "drafted"})

	if got := s.observe(t, ctx, capture.Gmail); got != 1 {
		t.Fatalf("ObserveOutbound(Gmail) = %d, want 1 — one outbound gmail message inside the horizon whose "+
			"external_message_id no delivery claims, on a thread with a delivery-linked task", got)
	}
	payloads := s.observedPayloads(t, ctx, taskID)
	if len(payloads) != 1 {
		t.Fatalf("outbound_observed events = %d, want exactly 1 (the inbound message must never produce one)", len(payloads))
	}
	p := payloads[0]
	if p.MessageID != msgID {
		t.Errorf("payload message_id = %d, want normalized_messages.id %d — that id IS the dedup key (criterion 6)", p.MessageID, msgID)
	}
	if p.ExternalMessageID != directID {
		t.Errorf("payload external_message_id = %q, want %q", p.ExternalMessageID, directID)
	}
	if p.Channel != "gmail" {
		t.Errorf("payload channel = %q, want gmail", p.Channel)
	}
	if p.ThreadKey != threadKey {
		t.Errorf("payload thread_key = %q, want %q", p.ThreadKey, threadKey)
	}
	if p.SentAt == "" {
		t.Errorf("payload sent_at is empty; criterion 5 pins it (the log entry is worthless without when)")
	}
	if p.Sender != "salvo@example.test" {
		t.Errorf("payload sender = %q, want salvo@example.test", p.Sender)
	}
	if want := bodyPreview(body); p.BodyPreview != want {
		t.Errorf("payload body_preview = %q, want the whitespace-normalized 120-rune prefix %q", p.BodyPreview, want)
	}

	// Criterion 6: a second pass, and a --all re-normalization, add nothing.
	if got := s.observe(t, ctx, capture.Gmail); got != 0 {
		t.Errorf("second ObserveOutbound(Gmail) = %d, want 0", got)
	}
	s.renormalize(t, ctx, msgID)
	s.observe(t, ctx, capture.Gmail)
	if n := s.eventCount(t, ctx, taskID, "outbound_observed"); n != 1 {
		t.Errorf("outbound_observed events after a rerun and a --all replay = %d, want still exactly 1 — "+
			"otherwise every poll adds a line to the task log", n)
	}

	// Channel scoping: the jira and slack passes must not see a gmail message.
	if got := s.observe(t, ctx, capture.Jira); got != 0 {
		t.Errorf("ObserveOutbound(Jira) = %d over a gmail-only corpus, want 0", got)
	}
	if got := s.observe(t, ctx, capture.Slack); got != 0 {
		t.Errorf("ObserveOutbound(Slack) = %d over a gmail-only corpus, want 0", got)
	}
	if n := s.eventCount(t, ctx, taskID, "outbound_observed"); n != 1 {
		t.Errorf("outbound_observed events after the other channels' passes = %d, want still 1", n)
	}
}

// The property that keeps invariant 5 intact: switchboard's OWN send re-enters
// ingestion as an outbound message, and the gmail matcher already claimed its
// pre-reserved Message-ID. Capture must never relabel it "sent outside
// switchboard".
func TestCapture_Integration_GmailNeverCapturesASwitchboardSend(t *testing.T) {
	ctx := context.Background()
	s := newCaptureSuite(t, ctx)
	taskID := s.task(t, ctx, "gmail send switchboard made")

	const threadKey = capGmailThreadPrefix + "t-ours"
	const ourID = "<sb-4242-itest-capture@switchboard>"
	_, threadID := s.message(t, ctx, msgFixture{
		provider: "google", channel: "gmail", direction: "outbound", threadKey: threadKey,
		extID: ourID, sentAgo: "30 minutes", body: "the reply switchboard sent through the Gmail API",
		sender: "salvo@example.test", rawID: "gmail:ours:1",
	})
	s.delivery(t, ctx, delFixture{
		taskID: taskID, channel: "gmail", threadID: threadID, status: "sent",
		sentExternalID: ourID, sentAgo: "31 minutes", attemptedAgo: "31 minutes", settled: true, confirmed: true,
	})

	if got := s.observe(t, ctx, capture.Gmail); got != 0 {
		t.Fatalf("ObserveOutbound(Gmail) = %d for a message whose id is on a delivery's sent_external_id, want 0 — "+
			"capture handles only what the matchers provably did NOT claim (invariant 5)", got)
	}
	if n := s.eventCount(t, ctx, taskID, "outbound_observed"); n != 0 {
		t.Errorf("outbound_observed events = %d for switchboard's own send, want 0", n)
	}
}

// Criterion 7: the pass writes task_events and nothing else. md5 of the whole row
// catches any column, including updated_at.
func TestCapture_Integration_WritesNothingOutsideTaskEvents(t *testing.T) {
	ctx := context.Background()
	s := newCaptureSuite(t, ctx)
	taskID := s.task(t, ctx, "no side effects")

	msgID, threadID := s.message(t, ctx, msgFixture{
		provider: "google", channel: "gmail", direction: "outbound",
		threadKey: capGmailThreadPrefix + "t-immutable", extID: "<immutable@mail.example.test>",
		sentAgo: "1 hour", body: "a direct send", sender: "salvo@example.test", rawID: "gmail:immutable:1",
	})
	deliveryID := s.delivery(t, ctx, delFixture{taskID: taskID, channel: "gmail", threadID: threadID, status: "drafted"})

	digest := func(query string, args ...any) string {
		var out string
		if err := s.pool.QueryRow(ctx, query, args...).Scan(&out); err != nil {
			t.Fatalf("digest %q: %v", query, err)
		}
		return out
	}
	const (
		taskQ = `SELECT md5(t::text) FROM tasks t WHERE id=$1`
		delQ  = `SELECT md5(d::text) FROM deliveries d WHERE id=$1`
		msgQ  = `SELECT md5(m::text) FROM normalized_messages m WHERE id=$1`
		thrQ  = `SELECT md5(n::text) FROM normalized_threads n WHERE id=$1`
	)
	taskBefore, delBefore := digest(taskQ, taskID), digest(delQ, deliveryID)
	msgBefore, thrBefore := digest(msgQ, msgID), digest(thrQ, threadID)

	if got := s.observe(t, ctx, capture.Gmail); got != 1 {
		t.Fatalf("ObserveOutbound(Gmail) = %d, want 1", got)
	}

	if got := digest(taskQ, taskID); got != taskBefore {
		t.Errorf("the tasks row changed; capture is purely informational — no status change, no bump (criterion 7)")
	}
	if got := digest(delQ, deliveryID); got != delBefore {
		t.Errorf("the deliveries row changed; capture must never mutate a delivery or invent a sent_external_id " +
			"(criterion 7, invariant 4)")
	}
	if got := digest(msgQ, msgID); got != msgBefore {
		t.Errorf("the normalized_messages row changed; the pass reads normalized tables, it does not stamp them")
	}
	if got := digest(thrQ, threadID); got != thrBefore {
		t.Errorf("the normalized_threads row changed; the pass reads normalized tables")
	}
}

// Criterion 2: switchboard's sends always carry ids, so a message with NULL or
// empty external_message_id cannot be one of ours — it is unclaimed by definition.
func TestCapture_Integration_MissingExternalMessageIDCountsAsUnclaimed(t *testing.T) {
	ctx := context.Background()
	s := newCaptureSuite(t, ctx)
	taskID := s.task(t, ctx, "ids missing from the provider")

	const threadKey = capJiraThreadPrefix + "CAP-NULLID"
	_, _ = s.message(t, ctx, msgFixture{
		provider: "jira", channel: "jira", direction: "outbound", threadKey: threadKey,
		extID: nil, sentAgo: "1 hour", body: "no external id at all", sender: "Salvador",
		rawID: "jira:nullid:1",
	})
	_, _ = s.message(t, ctx, msgFixture{
		provider: "jira", channel: "jira", direction: "outbound", threadKey: threadKey,
		extID: "", sentAgo: "1 hour", body: "empty external id", sender: "Salvador",
		rawID: "jira:emptyid:1",
	})
	s.delivery(t, ctx, delFixture{taskID: taskID, channel: "jira_comment", targetRef: threadKey, status: "drafted"})

	if got := s.observe(t, ctx, capture.Jira); got != 2 {
		t.Fatalf("ObserveOutbound(Jira) = %d, want 2 — NULL and '' external ids are UNCLAIMED (criterion 2); a "+
			"`NOT EXISTS (... sent_external_id = m.external_message_id)` written without care makes NULL match "+
			"nothing and silently drops these", got)
	}
	if n := s.eventCount(t, ctx, taskID, "outbound_observed"); n != 2 {
		t.Errorf("outbound_observed events = %d, want 2 (one per message)", n)
	}
}

// Criterion 10: the horizon bounds the scan — this is what stops a first-deploy
// flood onto old tasks.
func TestCapture_Integration_HorizonBoundsTheScan(t *testing.T) {
	ctx := context.Background()
	s := newCaptureSuite(t, ctx)
	taskID := s.task(t, ctx, "horizon")

	const threadKey = capJiraThreadPrefix + "CAP-HORIZON"
	s.message(t, ctx, msgFixture{
		provider: "jira", channel: "jira", direction: "outbound", threadKey: threadKey,
		extID: "jira:capture.local:comment:770001", sentAgo: "3 hours",
		body: "commented in the Jira web UI three hours ago", sender: "Salvador", rawID: "jira:horizon:1",
	})
	s.delivery(t, ctx, delFixture{taskID: taskID, channel: "jira_comment", targetRef: threadKey, status: "drafted"})

	t.Setenv("OUTBOUND_OBSERVE_HORIZON", "1h")
	if got := s.observe(t, ctx, capture.Jira); got != 0 {
		t.Errorf("ObserveOutbound(Jira) = %d with OUTBOUND_OBSERVE_HORIZON=1h over a 3-hour-old message, want 0", got)
	}
	if n := s.eventCount(t, ctx, taskID, "outbound_observed"); n != 0 {
		t.Errorf("outbound_observed events = %d outside the horizon, want 0", n)
	}

	t.Setenv("OUTBOUND_OBSERVE_HORIZON", "720h")
	if got := s.observe(t, ctx, capture.Jira); got != 1 {
		t.Errorf("ObserveOutbound(Jira) = %d with the default horizon, want 1", got)
	}
	if n := s.eventCount(t, ctx, taskID, "outbound_observed"); n != 1 {
		t.Errorf("outbound_observed events = %d inside the horizon, want 1", n)
	}
}

// Criterion 8: no delivery on the thread means no machine-readable link, so there
// is nothing to log and nothing to fail about. The message stays thread context.
func TestCapture_Integration_NoLinkedTaskProducesNoEventAndNoError(t *testing.T) {
	ctx := context.Background()
	s := newCaptureSuite(t, ctx)
	// A task exists in the project, but nothing links it to the thread — the
	// manually-created-task case named as this ticket's honest coverage limit.
	taskID := s.task(t, ctx, "unlinked task about the same work")

	s.message(t, ctx, msgFixture{
		provider: "google", channel: "gmail", direction: "outbound",
		threadKey: capGmailThreadPrefix + "t-unlinked", extID: "<unlinked@mail.example.test>",
		sentAgo: "20 minutes", body: "a direct reply on a thread no delivery mentions",
		sender: "salvo@example.test", rawID: "gmail:unlinked:1",
	})

	got, err := capture.ObserveOutbound(ctx, s.pool, capture.Gmail)
	if err != nil {
		t.Fatalf("ObserveOutbound returned an error for an unlinkable message: %v — criterion 8 says no event and "+
			"NO error; erroring would fail the connector run on ordinary traffic", err)
	}
	if got != 0 {
		t.Errorf("ObserveOutbound(Gmail) = %d with no delivery-linked task, want 0", got)
	}
	if n := s.eventCount(t, ctx, taskID, "outbound_observed"); n != 0 {
		t.Errorf("outbound_observed events on an unlinked task = %d, want 0 — guessing a link is exactly what this "+
			"ticket refuses to do", n)
	}
}

// Criterion 11: the scan is horizon-bounded, not newly-normalized-only, so a link
// created AFTER the direct send still gets the observation on the next pass.
func TestCapture_Integration_LateLinkageIsCapturedOnTheNextPass(t *testing.T) {
	ctx := context.Background()
	s := newCaptureSuite(t, ctx)
	taskID := s.task(t, ctx, "delivery drafted after the direct send")

	_, threadID := s.message(t, ctx, msgFixture{
		provider: "google", channel: "gmail", direction: "outbound",
		threadKey: capGmailThreadPrefix + "t-late", extID: "<late-linkage@mail.example.test>",
		sentAgo: "4 hours", body: "answered from another mail client first", sender: "salvo@example.test",
		rawID: "gmail:late:1",
	})

	if got := s.observe(t, ctx, capture.Gmail); got != 0 {
		t.Fatalf("ObserveOutbound(Gmail) = %d before any delivery exists, want 0", got)
	}

	// The draft worker resolves the thread and persists it into the delivery row —
	// the link appears only now.
	s.delivery(t, ctx, delFixture{taskID: taskID, channel: "gmail", threadID: threadID, status: "drafted"})

	if got := s.observe(t, ctx, capture.Gmail); got != 1 {
		t.Fatalf("ObserveOutbound(Gmail) = %d after the delivery was drafted, want 1 — a "+
			"newly-normalized-only scan would miss every late link (criterion 11)", got)
	}
	if n := s.eventCount(t, ctx, taskID, "outbound_observed"); n != 1 {
		t.Errorf("outbound_observed events = %d, want 1", n)
	}
}

// ---- jira: the target_ref = thread_key join, fanned out (criteria 1, 4) ---------

func TestCapture_Integration_JiraJoinsOnTargetRefAndFansOutToEveryLinkedTask(t *testing.T) {
	ctx := context.Background()
	s := newCaptureSuite(t, ctx)
	first := s.task(t, ctx, "jira issue work")
	second := s.task(t, ctx, "second task on the same issue")
	unrelated := s.task(t, ctx, "task on another issue")

	const threadKey = capJiraThreadPrefix + "CAP-1"
	const commentID = "jira:capture.local:comment:990001"
	msgID, _ := s.message(t, ctx, msgFixture{
		provider: "jira", channel: "jira", direction: "outbound", threadKey: threadKey,
		extID: commentID, sentAgo: "45 minutes",
		body: "commented straight in the Jira web UI", sender: "Salvador", rawID: "jira:direct:1",
	})

	// Drafted and already-sent deliveries on the same issue: no status filter.
	s.delivery(t, ctx, delFixture{taskID: first, channel: "jira_comment", targetRef: threadKey, status: "drafted"})
	s.delivery(t, ctx, delFixture{
		taskID: second, channel: "jira_comment", targetRef: threadKey, status: "sent",
		sentExternalID: "jira:capture.local:comment:990000", sentAgo: "2 hours",
		attemptedAgo: "2 hours", settled: true, confirmed: true,
	})
	s.delivery(t, ctx, delFixture{
		taskID: unrelated, channel: "jira_comment",
		targetRef: capJiraThreadPrefix + "CAP-2", status: "drafted",
	})

	if got := s.observe(t, ctx, capture.Jira); got < 1 {
		t.Fatalf("ObserveOutbound(Jira) = %d, want at least 1", got)
	}
	for _, taskID := range []int64{first, second} {
		if n := s.eventCount(t, ctx, taskID, "outbound_observed"); n != 1 {
			t.Errorf("task %d outbound_observed events = %d, want 1 — ALL distinct linked tasks get the event; "+
				"no status filter, no 'most recent delivery wins' (criterion 4)", taskID, n)
		}
	}
	if n := s.eventCount(t, ctx, unrelated, "outbound_observed"); n != 0 {
		t.Errorf("a task linked to a DIFFERENT issue got %d events, want 0", n)
	}
	if p := s.observedPayloads(t, ctx, first); len(p) == 1 && p[0].ThreadKey != threadKey {
		t.Errorf("payload thread_key = %q, want %q", p[0].ThreadKey, threadKey)
	}

	// --all replay: the upsert keeps normalized_messages.id, so the dedup query
	// still matches and neither task gains a second line.
	s.renormalize(t, ctx, msgID)
	s.observe(t, ctx, capture.Jira)
	if n := s.projectEventCount(t, ctx, "outbound_observed"); n != 2 {
		t.Errorf("project outbound_observed events after a --all replay = %d, want still 2", n)
	}
}

func TestCapture_Integration_JiraNeverCapturesASwitchboardSend(t *testing.T) {
	ctx := context.Background()
	s := newCaptureSuite(t, ctx)
	taskID := s.task(t, ctx, "jira comment switchboard posted")

	const threadKey = capJiraThreadPrefix + "CAP-OURS"
	const commentID = "jira:capture.local:comment:990500"
	s.message(t, ctx, msgFixture{
		provider: "jira", channel: "jira", direction: "outbound", threadKey: threadKey,
		extID: commentID, sentAgo: "10 minutes", body: "progress comment switchboard posted",
		sender: "Salvador", rawID: "jira:ours:1",
	})
	s.delivery(t, ctx, delFixture{
		taskID: taskID, channel: "jira_comment", targetRef: threadKey, status: "sent",
		sentExternalID: commentID, sentAgo: "11 minutes", attemptedAgo: "11 minutes", settled: true, confirmed: true,
	})

	if got := s.observe(t, ctx, capture.Jira); got != 0 {
		t.Fatalf("ObserveOutbound(Jira) = %d for a comment already claimed by a delivery, want 0 (invariant 5)", got)
	}
	if n := s.eventCount(t, ctx, taskID, "outbound_observed"); n != 0 {
		t.Errorf("outbound_observed events = %d for switchboard's own comment, want 0", n)
	}
}

// ---- slack: canonical-URL join, both forms (criteria 1, 4) ----------------------

func TestCapture_Integration_SlackMatchesThreadAndConversationTargets(t *testing.T) {
	ctx := context.Background()
	s := newCaptureSuite(t, ctx)
	threadTask := s.task(t, ctx, "delivery targeting the thread")
	conversationTask := s.task(t, ctx, "delivery targeting the channel")
	otherTask := s.task(t, ctx, "delivery targeting another channel")

	const root = "p1780000000000001"
	const threadKey = capSlackThreadPrefix + "CCAPTURE:" + root
	threadRef := capSlackConversationRef + "/" + root

	s.message(t, ctx, msgFixture{
		provider: "slack_web", channel: "slack", direction: "outbound", threadKey: threadKey,
		extID: "slack:TCAPTURE:CCAPTURE:p1780000000000009", sentAgo: "15 minutes",
		body: "replied from Slack on my phone", sender: "Salvo", rawID: "message:CCAPTURE:p1780000000000009",
	})

	s.delivery(t, ctx, delFixture{taskID: threadTask, channel: "slack_reply", targetRef: threadRef, status: "drafted"})
	s.delivery(t, ctx, delFixture{
		taskID: conversationTask, channel: "slack_reply", targetRef: capSlackConversationRef, status: "drafted",
	})
	s.delivery(t, ctx, delFixture{
		taskID: otherTask, channel: "slack_reply",
		targetRef: "https://app.slack.com/client/TCAPTURE/CCAPOTHER", status: "drafted",
	})

	if got := s.observe(t, ctx, capture.Slack); got < 1 {
		t.Fatalf("ObserveOutbound(Slack) = %d, want at least 1 — the join derives %q and %q from thread_key %q "+
			"(slackweb.Target.CanonicalURL); an exact-string mismatch here is a permanent silent miss (SWT-13)",
			got, threadRef, capSlackConversationRef, threadKey)
	}
	if n := s.eventCount(t, ctx, threadTask, "outbound_observed"); n != 1 {
		t.Errorf("thread-level delivery's task got %d events, want 1 (target_ref %q)", n, threadRef)
	}
	if n := s.eventCount(t, ctx, conversationTask, "outbound_observed"); n != 1 {
		t.Errorf("conversation-level delivery's task got %d events, want 1 — a delivery targeting the CHANNEL "+
			"corresponds on the whole conversation, threads included (SPEC join table)", n)
	}
	if n := s.eventCount(t, ctx, otherTask, "outbound_observed"); n != 0 {
		t.Errorf("a delivery to a different conversation got %d events, want 0", n)
	}
}

// Criterion 3, the guard that keeps invariant 5 intact for a channel with no
// reservable message id: while an unconfirmed switchboard send to the same
// destination is pending, this message MAY be that send, so capture defers.
func TestCapture_Integration_SlackDefersWhileAnUnconfirmedClaimantIsPending(t *testing.T) {
	ctx := context.Background()
	s := newCaptureSuite(t, ctx)
	taskID := s.task(t, ctx, "slack conversation with an in-flight send")

	const threadKey = capSlackThreadPrefix + "CCAPDEFER"
	const conversationRef = "https://app.slack.com/client/TCAPTURE/CCAPDEFER"
	s.message(t, ctx, msgFixture{
		provider: "slack_web", channel: "slack", direction: "outbound", threadKey: threadKey,
		extID: "slack:TCAPTURE:CCAPDEFER:p1780000000000021", sentAgo: "1 minute",
		body: "a Slack message that may be switchboard's own click", sender: "Salvo",
		rawID: "message:CCAPDEFER:p1780000000000021",
	})
	// The link.
	s.delivery(t, ctx, delFixture{taskID: taskID, channel: "slack_reply", targetRef: conversationRef, status: "drafted"})
	// The claimant: clicked, never confirmed, still inside the horizon. A browser
	// click reserves no message id, so this row cannot say whether the message
	// above is its own.
	pending := s.delivery(t, ctx, delFixture{
		taskID: taskID, channel: "slack_reply", targetRef: conversationRef, status: "sending",
		attemptedAgo: "2 minutes",
	})

	if got := s.observe(t, ctx, capture.Slack); got != 0 {
		t.Fatalf("ObserveOutbound(Slack) = %d while an unconfirmed 'sending' delivery to the same target exists, "+
			"want 0 — that message may be switchboard's OWN send whose matcher has not stamped it yet, and "+
			"logging it as 'sent outside switchboard' would be a lie (criterion 3)", got)
	}
	if n := s.eventCount(t, ctx, taskID, "outbound_observed"); n != 0 {
		t.Errorf("outbound_observed events = %d while the claimant is pending, want 0", n)
	}

	// mark_delivery_failed settles the row (status leaves ('sending','sent')); the
	// deferral was for this pass only, so the next run captures.
	if _, err := s.pool.Exec(ctx,
		`UPDATE deliveries SET status='failed', send_settled_at=now(), error='verified by hand: never landed',
		        updated_at=now() WHERE id=$1`, pending); err != nil {
		t.Fatalf("settle the pending claimant: %v", err)
	}
	if got := s.observe(t, ctx, capture.Slack); got != 1 {
		t.Fatalf("ObserveOutbound(Slack) = %d once the claimant settled, want 1 — deferral skips a pass, it does "+
			"not drop the message (criterion 3)", got)
	}
	if n := s.eventCount(t, ctx, taskID, "outbound_observed"); n != 1 {
		t.Errorf("outbound_observed events after the claimant settled = %d, want 1", n)
	}
}

// Named consequence 2: deferral is horizon-bounded, so a wedged claimant cannot
// blind a conversation forever. This also pins the COALESCE order — the claimant's
// instant is sent_at, then send_attempted_at, then updated_at, and updated_at is
// now() on every write, so reading it first would defer forever.
func TestCapture_Integration_SlackDeferralIsHorizonBounded(t *testing.T) {
	ctx := context.Background()
	s := newCaptureSuite(t, ctx)
	taskID := s.task(t, ctx, "slack conversation with a wedged send")

	const threadKey = capSlackThreadPrefix + "CCAPWEDGE"
	const conversationRef = "https://app.slack.com/client/TCAPTURE/CCAPWEDGE"
	s.message(t, ctx, msgFixture{
		provider: "slack_web", channel: "slack", direction: "outbound", threadKey: threadKey,
		extID: "slack:TCAPTURE:CCAPWEDGE:p1780000000000031", sentAgo: "5 minutes",
		body: "a fresh direct Slack reply", sender: "Salvo", rawID: "message:CCAPWEDGE:p1780000000000031",
	})
	s.delivery(t, ctx, delFixture{taskID: taskID, channel: "slack_reply", targetRef: conversationRef, status: "drafted"})
	// Attempted 800 hours ago (past the 720h default), never confirmed, sent_at
	// NULL. updated_at is now(), which must NOT be what the horizon reads.
	s.delivery(t, ctx, delFixture{
		taskID: taskID, channel: "slack_reply", targetRef: conversationRef, status: "sending",
		attemptedAgo: "800 hours",
	})

	if got := s.observe(t, ctx, capture.Slack); got != 1 {
		t.Fatalf("ObserveOutbound(Slack) = %d with a claimant OLDER than the horizon, want 1 — an unresolvable "+
			"claimant must not blind capture on that conversation forever (named consequence 2), and the "+
			"claimant's instant is COALESCE(sent_at, send_attempted_at, updated_at), not updated_at first", got)
	}
	if n := s.eventCount(t, ctx, taskID, "outbound_observed"); n != 1 {
		t.Errorf("outbound_observed events = %d, want 1", n)
	}
}

func TestCapture_Integration_SlackNeverCapturesASwitchboardSend(t *testing.T) {
	ctx := context.Background()
	s := newCaptureSuite(t, ctx)
	taskID := s.task(t, ctx, "slack reply switchboard sent")

	const threadKey = capSlackThreadPrefix + "CCAPOURS"
	const conversationRef = "https://app.slack.com/client/TCAPTURE/CCAPOURS"
	const messageID = "slack:TCAPTURE:CCAPOURS:p1780000000000041"
	s.message(t, ctx, msgFixture{
		provider: "slack_web", channel: "slack", direction: "outbound", threadKey: threadKey,
		extID: messageID, sentAgo: "6 minutes", body: "the reply switchboard clicked Send on",
		sender: "Salvo", rawID: "message:CCAPOURS:p1780000000000041",
	})
	// The matcher already stamped it: claimed, so capture never touches it.
	s.delivery(t, ctx, delFixture{
		taskID: taskID, channel: "slack_reply", targetRef: conversationRef, status: "sent",
		sentExternalID: messageID, sentAgo: "6 minutes", attemptedAgo: "7 minutes", settled: true, confirmed: true,
		body: "the reply switchboard clicked Send on",
	})

	if got := s.observe(t, ctx, capture.Slack); got != 0 {
		t.Fatalf("ObserveOutbound(Slack) = %d for a message the slack matcher already claimed, want 0 — "+
			"double-logging switchboard's own send as external is the invariant-5 regression this guards", got)
	}
	if n := s.eventCount(t, ctx, taskID, "outbound_observed"); n != 0 {
		t.Errorf("outbound_observed events = %d, want 0", n)
	}
}

// ---- review follow-ups (SWT-16 go-reviewer) -------------------------------------

// Criterion 3 on the GMAIL join. Every other deferral test drives the target_ref
// branch; this one drives joinByThreadID, which had no coverage at all. A break
// toward "never defer" there is silent, and its consequence is precisely the
// invariant-5 mislabel the deferral guard exists to prevent: capture would report a
// message switchboard itself sent as sent by hand.
//
// Reachable in practice: a human resolving a wedged row with mark_delivery_sent
// leaves status='sent' with sent_external_id NULL until the matcher stamps it.
func TestCapture_Integration_GmailDefersWhileAnUnconfirmedClaimantIsPending(t *testing.T) {
	ctx := context.Background()
	s := newCaptureSuite(t, ctx)
	taskID := s.task(t, ctx, "gmail thread with an in-flight send")

	const threadKey = capGmailThreadPrefix + "t-defer"
	_, threadID := s.message(t, ctx, msgFixture{
		provider: "google", channel: "gmail", direction: "outbound", threadKey: threadKey,
		extID: "<direct-send-during-an-attempt@mail.example.test>", sentAgo: "2 minutes",
		body:   "a reply that may be switchboard's own send, unstamped",
		sender: "salvo@example.test", rawID: "gmail:defer:1",
	})
	// The link.
	s.delivery(t, ctx, delFixture{taskID: taskID, channel: "gmail", threadID: threadID, status: "drafted"})
	// The claimant: dispatched, never stamped, still inside the horizon.
	pending := s.delivery(t, ctx, delFixture{
		taskID: taskID, channel: "gmail", threadID: threadID, status: "sending",
		attemptedAgo: "3 minutes",
	})

	if got := s.observe(t, ctx, capture.Gmail); got != 0 {
		t.Fatalf("ObserveOutbound(Gmail) = %d while an unconfirmed 'sending' delivery on the same thread_id exists, "+
			"want 0 — the deferral guard must key on thread_id for gmail exactly as it keys on target_ref for slack "+
			"(criterion 3); without it capture logs switchboard's own send as external", got)
	}
	if n := s.eventCount(t, ctx, taskID, "outbound_observed"); n != 0 {
		t.Errorf("outbound_observed events = %d while the gmail claimant is pending, want 0", n)
	}

	// Settling the row releases the deferral: it skips a pass, it does not drop.
	if _, err := s.pool.Exec(ctx,
		`UPDATE deliveries SET status='failed', send_settled_at=now(), error='verified by hand: never landed',
		        updated_at=now() WHERE id=$1`, pending); err != nil {
		t.Fatalf("settle the pending gmail claimant: %v", err)
	}
	if got := s.observe(t, ctx, capture.Gmail); got != 1 {
		t.Fatalf("ObserveOutbound(Gmail) = %d once the claimant settled, want 1 — deferral skips a pass, it does not "+
			"drop the message (criterion 3)", got)
	}
	if n := s.eventCount(t, ctx, taskID, "outbound_observed"); n != 1 {
		t.Errorf("outbound_observed events after the gmail claimant settled = %d, want 1", n)
	}
}

// The unclaimed test is channel-scoped: a delivery on a DIFFERENT channel that
// happens to carry this id as its sent_external_id must not count as claiming it.
// Without the d.channel predicate the pass would silently under-report on any id
// namespace two channels share.
func TestCapture_Integration_AClaimOnAnotherChannelDoesNotCount(t *testing.T) {
	ctx := context.Background()
	s := newCaptureSuite(t, ctx)
	taskID := s.task(t, ctx, "jira issue whose id collides with another channel")

	const threadKey = capJiraThreadPrefix + "CAP-CHAN"
	const commentID = "jira:capture.local:comment:990001"
	s.message(t, ctx, msgFixture{
		provider: "jira", channel: "jira", direction: "outbound", threadKey: threadKey,
		extID: commentID, sentAgo: "20 minutes", body: "a comment left in the Jira web UI",
		sender: "acc-capture-own", rawID: "comment:CAP-CHAN:990001",
	})
	// The link.
	s.delivery(t, ctx, delFixture{taskID: taskID, channel: "jira_comment", targetRef: threadKey, status: "drafted"})
	// A confirmed delivery on a DIFFERENT channel carrying the same string. Only a
	// jira_comment delivery can claim a jira message.
	s.delivery(t, ctx, delFixture{
		taskID: taskID, channel: "slack_reply", targetRef: capSlackConversationRef, status: "sent",
		sentExternalID: commentID, sentAgo: "21 minutes", settled: true, confirmed: true,
	})

	if got := s.observe(t, ctx, capture.Jira); got != 1 {
		t.Fatalf("ObserveOutbound(Jira) = %d, want 1 — the claim test must be scoped to the channel's OWN delivery "+
			"channel (jira_comment); a slack_reply row carrying the same string claims nothing here", got)
	}
}

// Criterion 5 pins sent_at's shape, not merely its presence: the payload is read by
// humans on the dashboard and by anything later built on the event, so an
// unparseable or local-zone timestamp is a defect.
func TestCapture_Integration_SentAtIsRFC3339UTC(t *testing.T) {
	ctx := context.Background()
	s := newCaptureSuite(t, ctx)
	taskID := s.task(t, ctx, "payload timestamp shape")

	const threadKey = capJiraThreadPrefix + "CAP-TIME"
	s.message(t, ctx, msgFixture{
		provider: "jira", channel: "jira", direction: "outbound", threadKey: threadKey,
		extID: "jira:capture.local:comment:990002", sentAgo: "90 minutes",
		body: "a comment for the timestamp assertion", sender: "acc-capture-own",
		rawID: "comment:CAP-TIME:990002",
	})
	s.delivery(t, ctx, delFixture{taskID: taskID, channel: "jira_comment", targetRef: threadKey, status: "drafted"})

	if got := s.observe(t, ctx, capture.Jira); got != 1 {
		t.Fatalf("ObserveOutbound(Jira) = %d, want 1", got)
	}
	payloads := s.observedPayloads(t, ctx, taskID)
	if len(payloads) != 1 {
		t.Fatalf("payloads = %d, want 1", len(payloads))
	}
	parsed, err := time.Parse(time.RFC3339, payloads[0].SentAt)
	if err != nil {
		t.Fatalf("payload sent_at %q is not RFC3339: %v", payloads[0].SentAt, err)
	}
	if _, offset := parsed.Zone(); offset != 0 {
		t.Errorf("payload sent_at %q is not UTC (offset %ds); the event must not carry the connector host's zone",
			payloads[0].SentAt, offset)
	}
	if elapsed := time.Since(parsed); elapsed < 80*time.Minute || elapsed > 100*time.Minute {
		t.Errorf("payload sent_at %q is %v ago, want ~90 minutes — it must carry the MESSAGE's instant, not the pass's",
			payloads[0].SentAt, elapsed.Round(time.Minute))
	}
}

// A zero Channel is a programming error (a caller that forgot the binding), and it
// must fail loudly rather than scan the empty channel and report a confident zero.
func TestCapture_Integration_ZeroChannelIsAnError(t *testing.T) {
	ctx := context.Background()
	s := newCaptureSuite(t, ctx)

	n, err := capture.ObserveOutbound(ctx, s.pool, capture.Channel{})
	if err == nil {
		t.Fatalf("ObserveOutbound with a zero Channel returned (%d, nil), want an error — an unconfigured channel "+
			"must not read as 'nothing to observe'", n)
	}
	if n != 0 {
		t.Errorf("ObserveOutbound with a zero Channel reported %d observations alongside its error, want 0", n)
	}
}
