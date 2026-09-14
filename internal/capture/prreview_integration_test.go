//go:build integration

package capture_test

// SWT-54 (docs/tickets/treetop-pr-review-tasks_SPEC.md) against a real database,
// the capture path end to end: criteria 1 (the CHECKs), 2 (the tool stores both
// columns), 4 (both columns are COLUMN-fed), 5, 6, 8 (fall-through a/b/c), 9 (the
// raw-header mutation), 10, 11, 12, 13, 14 (OQ-2 = a), 15 (OQ-1 = b), 16 and 18.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_isopr?sslmode=disable TZ=UTC \
//	  go test -tags integration -p 1 -count=1 -run 'CapturePRReview|CaptureDryRun' ./internal/capture/
//
// Build-tagged `integration` AND env-gated on DATABASE_URL, with a FATAL guard on
// 192.168.50.49. NO LLM, NO network, no GitHub token: the only input is stored mail.
//
// "TEST THE COLUMN, NOT THE FIXTURE" (memory: test-the-column-not-the-fixture):
//   - the PR-review rule is seeded THROUGH capture_rule_add on the executor, and the
//     stored pr_review / exclude_pr_authors columns are read back;
//   - every authorship fact lives ONLY in a real raw_source_items IMAP envelope
//     (rfc822_b64 carrying X-GitHub-Reason / -Sender / -Recipient). No header is
//     handed to capture through a Go struct; the normalized columns carry none;
//   - tasks, refs and provenance are created by a live pass, closed/dismissed
//     through task_close / task_dismiss on the real-matrix executor.
//
// MUTATIONS these tests are built to catch (named inline):
//   - loadRules selecting a literal instead of r.pr_review or r.exclude_pr_authors
//     (TestCapturePRReview_Integration_BothRuleColumnsAreColumnFed);
//   - authorship read from anything but the stored raw row: flipping ONE stored
//     X-GitHub-Reason from author to comment must turn "no task" into "task"
//     (TestCapturePRReview_Integration_AuthorshipIsReadFromTheStoredRawRow);
//   - the opening looked up only inside the pending message's own thread
//     (TestCapturePRReview_Integration_TheOpeningIsFoundByMessageIDAcrossAccounts);
//   - an `own` verdict that does not fall through (8a/8b/8c).
//
// ---- IMPOSED SURFACE ----------------------------------------------------------
//
//	type RulesStats struct { ...; PRAuthorSkipped, PRClosed int }
//	  PRAuthorSkipped: create-branch messages whose verdict was own/excluded and
//	  that fell through — counted in BOTH modes (the decision is mode-free, like
//	  Deferred). PRClosed: review tasks task_close answered closed — live only.
//	decision reasons:
//	  fall-through  begins  "rule R skipped: PR {canonical key} authored by him ("
//	  undetermined  contains "author undetermined"
//	  row 5         contains "author not named"
//	  other         contains the author login
//	  shadow notice contains "would close"
//	  notice-create contains "PR already merged/closed; no review task"
//	task_close {task_id, reason} as the pass actor, reason beginning
//	  "PR #N merged on GitHub" / "PR #N closed on GitHub".
//
// RED TODAY: migration 0035 is absent (prrRequire0035 fails every test first),
// and RulesStats has neither counter, so this file does not compile.
//
// CROSS-SUITE DISCIPLINE: EvaluateRules' pending set is GLOBAL, so this suite
// deletes capture_decisions WHOLESALE at start and end (the rules_integration /
// rules_reopen / rules_revive precedent). Run it against a private database
// (ops_isopr) — IK "the compose Postgres is SHARED". It owns projects
// itest-prr-*, google accounts itest-prr-{a,b}@gmail.example.test, threads
// gmail:itest-prr-%, and audit rows by task plus its two actors.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	prrCollab   = "itest-prr-collab"
	prrReengine = "itest-prr-reengine"
	prrAcctA    = "itest-prr-a@gmail.example.test"
	prrAcctB    = "itest-prr-b@gmail.example.test" // Step 0f: TWO receiving accounts
	prrActor    = "capture:itest-prr"
	prrHuman    = "opsctl:itest-prr"
	prrLogin    = "sspataro57"                 // Step 0b: his login, the only X-GitHub-Recipient seen
	prrWWW      = "treetopllc/itest-prr-www"   // the rule-6 fixture names this repo
	prrThird    = "treetopllc/itest-prr-third" // no attribution rule names this one
	// The seeded rule, verbatim (D2).
	prrPattern  = `<treetopllc/`
	prrKeyRegex = `<(treetopllc/[A-Za-z0-9._-]+/pull/[0-9]+)@github\.com>$`
	// Rule 10 and rule 1 in miniature, with prefixes no other suite uses.
	prrRule10 = `(PRW|PRA)-[0-9]+`
	prrLHH    = `PRLHH-[0-9]+`
)

func prrKey(repo string, n int) string { return fmt.Sprintf("%s#%d", repo, n) }

func prrURL(repo string, n int) string { return fmt.Sprintf("https://github.com/%s/pull/%d", repo, n) }

// ---- harness --------------------------------------------------------------------

type prrSuite struct {
	pool                           *pgxpool.Pool
	ex                             *executor.Executor
	collab, reengine               int64
	acctA, acctB                   int64
	prRule, rule6, rule10, ruleLHH int64
	seq                            int
}

type prrOpts struct {
	exclude  []string // exclude_pr_authors on the PR rule; the SEEDED rule's is empty (OQ-1 = b)
	noPRRule bool     // the dry-run suite: the candidate is NOT inserted
}

func prrRequire0035(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name='capture_rules' AND column_name IN ('pr_review','exclude_pr_authors')`).Scan(&n); err != nil {
		t.Fatalf("probe 0035's columns: %v", err)
	}
	if n != 2 {
		t.Fatalf("capture_rules has %d of the 2 columns migration 0035 adds (pr_review, exclude_pr_authors); apply "+
			"migrations/0035_capture_rules_pr_review.sql (`make migrate LOCAL_DB_URL=...ops_isopr...`)", n)
	}
}

func newPRRSuite(t *testing.T, ctx context.Context, o prrOpts) *prrSuite {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db (cleanup deletes capture_decisions " +
			"wholesale); use a private compose database on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	prrRequire0035(t, ctx, pool)
	prrCleanup(t, ctx, pool)
	t.Cleanup(func() { prrCleanup(t, ctx, pool) })

	// The REAL matrix (queueMatrixExecutor pattern): task_close is mcpHumanOnly,
	// which gates only MCP-prefixed non-humans, so capture:* must pass it.
	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	s := &prrSuite{pool: pool, ex: executor.New(reg, checker, audit.NewPGStore(pool))}

	proj := func(slug string) int64 {
		return s.id(t, ctx,
			`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
			 VALUES ($1,$1,'itest-prr-client','manual','dashboard','/tmp/itest-prr','any') RETURNING id`, slug)
	}
	s.collab = proj(prrCollab)
	s.reengine = proj(prrReengine)

	// Rules 6, 10 and 1 at their production priorities (50 / 90 / 100).
	s.rule6 = s.id(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, priority, enabled, note)
		 VALUES ($1,'thread_key_contains',$2,50,true,'itest-prr rule 6') RETURNING id`, s.collab, prrWWW)
	s.rule10 = s.id(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, priority, enabled, note)
		 VALUES ($1,'body_regex',$2,'jira',90,true,'itest-prr rule 10') RETURNING id`, s.collab, prrRule10)
	s.ruleLHH = s.id(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, priority, enabled, note)
		 VALUES ($1,'body_regex',$2,'jira',100,true,'itest-prr rule 1') RETURNING id`, s.reengine, prrLHH)
	if !o.noPRRule {
		s.prRule = s.addPRRule(t, ctx, o.exclude)
	}

	acct := func(email string) int64 {
		return s.id(t, ctx,
			`INSERT INTO source_accounts (provider, account_email, scopes, send_enabled, calendar_in_availability)
			 VALUES ('google',$1,'{}',false,false) RETURNING id`, email)
	}
	s.acctA = acct(prrAcctA)
	s.acctB = acct(prrAcctB)
	return s
}

// addPRRule seeds the D2 rule THROUGH capture_rule_add (criterion 2), then reads
// both columns back: a tool that parsed pr_review and dropped it would leave a
// plain github rule, and every test below would then be about the wrong rule.
func (s *prrSuite) addPRRule(t *testing.T, ctx context.Context, exclude []string) int64 {
	t.Helper()
	args := map[string]any{
		"project": prrCollab, "criteria_type": "thread_key_contains", "pattern": prrPattern,
		"external_system": "github", "key_regex": prrKeyRegex, "priority": 91, "pr_review": true,
		"note": "itest-prr: one review task per treetopllc PR he did not author",
	}
	if len(exclude) > 0 {
		args["exclude_pr_authors"] = exclude
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res, err := s.ex.Execute(ctx, executor.Call{Tool: "capture_rule_add", Actor: prrHuman, Args: raw})
	if err != nil {
		t.Fatalf("capture_rule_add refused the seeded PR-review rule %s: %v (criterion 2)", raw, err)
	}
	var out struct {
		RuleID int64 `json:"rule_id"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil || out.RuleID == 0 {
		t.Fatalf("capture_rule_add result %s: %v", res.Output, err)
	}
	var pr bool
	var stored []string
	if err := s.pool.QueryRow(ctx, `SELECT pr_review, exclude_pr_authors FROM capture_rules WHERE id=$1`,
		out.RuleID).Scan(&pr, &stored); err != nil {
		t.Fatalf("read back rule %d: %v", out.RuleID, err)
	}
	if !pr {
		t.Fatalf("rule %d stored pr_review=false; capture_rule_add must INSERT the column (criterion 2)", out.RuleID)
	}
	if strings.Join(stored, ",") != strings.Join(exclude, ",") {
		t.Fatalf("rule %d stored exclude_pr_authors=%v, want %v", out.RuleID, stored, exclude)
	}
	return out.RuleID
}

func prrCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE provider='google' AND account_email IN ('` + prrAcctA + `','` + prrAcctB + `'))`
	const projs = `(SELECT id FROM projects WHERE slug IN ('` + prrCollab + `','` + prrReengine + `'))`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	const actors = `('` + prrActor + `','` + prrHuman + `')`
	for _, q := range []string{
		`DELETE FROM capture_decisions`, // wholesale: see the header
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM external_refs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_claims WHERE task_id IN ` + tasksOf,
		`DELETE FROM deliveries WHERE task_id IN ` + tasksOf,
		// IK TRAP 2: capture:* audit and policy rows by task BEFORE the tasks; the
		// create_task audit row carries no task_id, so the actors sweep it.
		`DELETE FROM policy_decisions WHERE audit_event_id IN
		   (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor IN ` + actors + `)`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf + ` OR actor IN ` + actors,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM capture_rules WHERE project_id IN ` + projs,
		`DELETE FROM projects WHERE slug IN ('` + prrCollab + `','` + prrReengine + `')`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'gmail:itest-prr-%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider='google' AND account_email IN ('` + prrAcctA + `','` + prrAcctB + `')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func (s *prrSuite) id(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", q, err)
	}
	return id
}

func (s *prrSuite) n(t *testing.T, ctx context.Context, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

func (s *prrSuite) exec(t *testing.T, ctx context.Context, q string, args ...any) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func (s *prrSuite) call(t *testing.T, ctx context.Context, tool string, taskID int64, args string) {
	t.Helper()
	if _, err := s.ex.Execute(ctx, executor.Call{Tool: tool, Actor: prrHuman, Args: []byte(args), TaskID: &taskID}); err != nil {
		t.Fatalf("%s(%s) as %s: %v", tool, args, prrHuman, err)
	}
}

func (s *prrSuite) thread(t *testing.T, ctx context.Context, key string) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx, `SELECT id FROM normalized_threads WHERE thread_key=$1`, key).Scan(&id); err == nil {
		return id
	}
	return s.id(t, ctx, `INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,$1,'[]') RETURNING id`, key)
}

func (s *prrSuite) pass(t *testing.T, ctx context.Context, mode string) capture.RulesStats {
	t.Helper()
	st, err := capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{Mode: mode, Actor: prrActor})
	if err != nil {
		t.Fatalf("EvaluateRules(%s): %v", mode, err)
	}
	return st
}

// ---- stored GitHub notification mail ---------------------------------------------

// ghMail describes one GitHub notification as GitHub sends it. The X-GitHub-*
// values end up ONLY inside the stored RFC822 (raw_source_items.raw_json);
// normalized_messages gets exactly what NormalizeRFC822 would give it.
type ghMail struct {
	acctB     bool   // received by the second account
	repo      string // owner/repo
	kind      string // "pull" (default) | "issues" | "commit"
	pr        int
	opening   bool   // the Message-ID IS the thread root: GitHub's PR-opened notification
	reason    string // X-GitHub-Reason; "" = header absent (commit/push mail, 0a)
	sender    string // X-GitHub-Sender (also the From display name)
	recipient string // X-GitHub-Recipient
	subject   string
	body      string
	direction string // "" = inbound
	rawJSON   string // non-empty: store this raw_json verbatim (a gmail:-shaped row)
	minsAgo   int
}

type prrMsg struct {
	id, raw, thread int64
	mid, root       string
	uid             int
	spec            ghMail
}

func (m ghMail) root() string {
	kind := m.kind
	if kind == "" {
		kind = "pull"
	}
	return fmt.Sprintf("<%s/%s/%d@github.com>", m.repo, kind, m.pr)
}

func prrRFC822(m ghMail, mid, root string) string {
	from := m.sender
	if from == "" {
		from = "GitHub"
	}
	owner, name, _ := strings.Cut(m.repo, "/")
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s <notifications@github.com>\r\n", from)
	fmt.Fprintf(&b, "To: %s <%s@noreply.github.com>\r\n", m.repo, name)
	fmt.Fprintf(&b, "Subject: %s\r\n", m.subject)
	fmt.Fprintf(&b, "Message-ID: %s\r\n", mid)
	if mid != root {
		fmt.Fprintf(&b, "In-Reply-To: %s\r\nReferences: %s\r\n", root, root)
	}
	b.WriteString("Date: Mon, 14 Sep 2026 12:00:00 +0000\r\n")
	if m.reason != "" {
		fmt.Fprintf(&b, "X-GitHub-Reason: %s\r\n", m.reason)
	}
	if m.sender != "" {
		fmt.Fprintf(&b, "X-GitHub-Sender: %s\r\n", m.sender)
	}
	if m.recipient != "" {
		fmt.Fprintf(&b, "X-GitHub-Recipient: %s\r\n", m.recipient)
	}
	fmt.Fprintf(&b, "List-ID: %s <%s.%s.github.com>\r\n", m.repo, name, owner)
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n\r\n")
	b.WriteString(strings.ReplaceAll(m.body, "\n", "\r\n"))
	b.WriteString("\r\n")
	return b.String()
}

// prrEnvelope is the imapRawEnvelope shape buildIMAPEnvelope writes.
func prrEnvelope(t *testing.T, rfc string, uid int) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"source": "imap", "folder": "INBOX", "uidvalidity": 1, "uid": uid,
		"internaldate": "2026-09-14T12:00:01Z", "flags": []string{}, "size": len(rfc),
		"truncated": false, "rfc822_b64": base64.StdEncoding.EncodeToString([]byte(rfc)),
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return string(raw)
}

func (s *prrSuite) mail(t *testing.T, ctx context.Context, m ghMail) prrMsg {
	t.Helper()
	s.seq++
	kind := m.kind
	if kind == "" {
		kind = "pull"
	}
	root := m.root()
	mid := root
	if !m.opening {
		mid = fmt.Sprintf("<%s/%s/%d/c%d-itest-prr@github.com>", m.repo, kind, m.pr, s.seq)
	}
	acctID, acctEmail := s.acctA, prrAcctA
	if m.acctB {
		acctID, acctEmail = s.acctB, prrAcctB
	}
	thread := s.thread(t, ctx, "gmail:"+acctEmail+":"+root)
	uid := 90000 + s.seq
	raw := m.rawJSON
	if raw == "" {
		raw = prrEnvelope(t, prrRFC822(m, mid, root), uid)
	}
	tag := fmt.Sprintf("%d-%d", time.Now().UnixNano(), s.seq)
	rawID := s.id(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,$3::jsonb,$4, now()) RETURNING id`, acctID, "itest-prr-"+tag, raw, "itest-prr-h-"+tag)
	dir := m.direction
	if dir == "" {
		dir = "inbound"
	}
	from := m.sender
	if from == "" {
		from = "GitHub"
	}
	sender := from + " <notifications@github.com>"
	if dir == "outbound" {
		sender = "Salvador Spataro <" + acctEmail + ">"
	}
	msgID := s.id(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1,$2,$3,$4, now() - make_interval(mins => $5), $6,$7,$8,'gmail') RETURNING id`,
		rawID, thread, dir, mid, m.minsAgo, m.body, m.subject, sender)
	return prrMsg{id: msgID, raw: rawID, thread: thread, mid: mid, root: root, uid: uid, spec: m}
}

// colleague is a PR-opened notification from someone else (Step 0b's senders).
func (s *prrSuite) colleague(t *testing.T, ctx context.Context, repo string, pr int, sender, title string, minsAgo int) prrMsg {
	t.Helper()
	_, name, _ := strings.Cut(repo, "/")
	return s.mail(t, ctx, ghMail{repo: repo, pr: pr, opening: true, reason: "subscribed", sender: sender,
		recipient: prrLogin, subject: fmt.Sprintf("[%s] %s (PR #%d)", repo, title, pr),
		body: fmt.Sprintf("%s opened this pull request on %s.", sender, name), minsAgo: minsAgo})
}

// rewriteReason is criterion 9's MUTATION: it rewrites ONE stored raw row's
// X-GitHub-Reason and nothing else.
func (s *prrSuite) rewriteReason(t *testing.T, ctx context.Context, m prrMsg, reason string) {
	t.Helper()
	spec := m.spec
	spec.reason = reason
	s.exec(t, ctx, `UPDATE raw_source_items SET raw_json=$2::jsonb WHERE id=$1`, m.raw,
		prrEnvelope(t, prrRFC822(spec, m.mid, m.root), m.uid))
}

// seedBucket creates rule 10's bucket task (key "PRW") from a non-GitHub mail.
func (s *prrSuite) seedBucket(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	thread := s.thread(t, ctx, "gmail:"+prrAcctA+":<itest-prr-bucket@mail.example.test>")
	raw := s.id(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,'itest-prr-bucket','{}','itest-prr-h-bucket', now()) RETURNING id`, s.acctA)
	s.id(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1,$2,'inbound','<itest-prr-bucket@mail.example.test>', now() - interval '2 hours',
		         'staging is down again','PRW-1 staging down','Marco <marco@example.test>','gmail') RETURNING id`, raw, thread)
	s.pass(t, ctx, "live")
	var task int64
	if err := s.pool.QueryRow(ctx,
		`SELECT task_id FROM external_refs WHERE system='jira' AND external_key='PRW'`).Scan(&task); err != nil {
		t.Fatalf("fixture: rule 10's bucket task (key PRW) was not created: %v", err)
	}
	return task
}

type prrDecision struct {
	action                    string
	projectID, ruleID, taskID *int64
	ruleIDs                   []int64
	sys, key                  *string
	reason                    string
}

func (s *prrSuite) decision(t *testing.T, ctx context.Context, msg int64, mode string) (prrDecision, bool) {
	t.Helper()
	var d prrDecision
	var reason *string
	err := s.pool.QueryRow(ctx,
		`SELECT action, project_id, matched_rule_id, task_id, matched_rule_ids, external_system, external_key, reason
		   FROM capture_decisions WHERE message_id=$1 AND mode=$2 ORDER BY id DESC LIMIT 1`, msg, mode).
		Scan(&d.action, &d.projectID, &d.ruleID, &d.taskID, &d.ruleIDs, &d.sys, &d.key, &reason)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return d, false
		}
		t.Fatalf("read %s decision for message %d: %v", mode, msg, err)
	}
	if reason != nil {
		d.reason = *reason
	}
	return d, true
}

func (s *prrSuite) must(t *testing.T, ctx context.Context, m prrMsg, mode string) prrDecision {
	t.Helper()
	d, ok := s.decision(t, ctx, m.id, mode)
	if !ok {
		t.Fatalf("message %d (%s) has no %s decision", m.id, m.mid, mode)
	}
	return d
}

// ref returns the task linked to a github key, and how many refs carry it.
func (s *prrSuite) ref(t *testing.T, ctx context.Context, key string) (int64, string, int) {
	t.Helper()
	n := s.n(t, ctx, `SELECT count(*) FROM external_refs WHERE system='github' AND external_key=$1`, key)
	if n == 0 {
		return 0, "", 0
	}
	var task int64
	var url string
	if err := s.pool.QueryRow(ctx,
		`SELECT task_id, COALESCE(external_url,'') FROM external_refs WHERE system='github' AND external_key=$1
		  ORDER BY id LIMIT 1`, key).Scan(&task, &url); err != nil {
		t.Fatalf("read ref %s: %v", key, err)
	}
	return task, url, n
}

func (s *prrSuite) mustRef(t *testing.T, ctx context.Context, key string) int64 {
	t.Helper()
	task, _, n := s.ref(t, ctx, key)
	if n != 1 {
		t.Fatalf("external_refs rows for (github, %s) = %d, want exactly 1", key, n)
	}
	return task
}

func (s *prrSuite) noRef(t *testing.T, ctx context.Context, key, why string) {
	t.Helper()
	if _, _, n := s.ref(t, ctx, key); n != 0 {
		t.Errorf("(github, %s) has %d external_refs rows, want 0: %s", key, n, why)
	}
}

func (s *prrSuite) status(t *testing.T, ctx context.Context, task int64) string {
	t.Helper()
	var st string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, task).Scan(&st); err != nil {
		t.Fatalf("read task %d: %v", task, err)
	}
	return st
}

func (s *prrSuite) events(t *testing.T, ctx context.Context, task int64, eventType string) int {
	t.Helper()
	return s.n(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type=$2`, task, eventType)
}

func prrHas(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func prrSkipped(rule int64, key string) string {
	return fmt.Sprintf("rule %d skipped: PR %s authored by him (", rule, key)
}

// ---- criterion 1: the CHECKs and the defaults -------------------------------------

func TestCapturePRReview_Integration_Migration0035ChecksAndDefaults(t *testing.T) {
	ctx := context.Background()
	s := newPRRSuite(t, ctx, prrOpts{noPRRule: true})

	var pr bool
	var ex []string
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, priority, note)
		 VALUES ($1,'thread_key_contains','<itest-prr-default/',1,'itest-prr') RETURNING pr_review, exclude_pr_authors`,
		s.collab).Scan(&pr, &ex); err != nil {
		t.Fatalf("insert a rule naming neither column: %v", err)
	}
	if pr || len(ex) != 0 {
		t.Errorf("defaults = (pr_review %v, exclude_pr_authors %v), want (false, {}) — every existing rule is inert", pr, ex)
	}

	bad := []struct{ name, q, constraint string }{
		{"pr_review on a jira rule",
			`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, pr_review, note)
			 VALUES ($1,'thread_key_contains','<itest-prr-c1/','jira','x',true,'itest-prr')`, "capture_rules_pr_review_github"},
		{"pr_review with no key_regex",
			`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, pr_review, note)
			 VALUES ($1,'thread_key_contains','<itest-prr-c2/','github',true,'itest-prr')`, "capture_rules_pr_review_github"},
		// NOT here: pr_review on an ATTRIBUTION-ONLY rule (external_system NULL).
		// The SPEC's CHECK, verbatim, does not refuse it: NULL = 'github' is
		// NULL, the AND is NULL, `false OR NULL` is NULL, and a CHECK passes on
		// NULL. capture_rule_add refuses it (criterion 2, capturerules_prreview_test.go)
		// and loadRules would read it as attribution-only. Flagged to the main
		// thread rather than pinned either way.
		{"pr_review with revive",
			`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, revive, pr_review, note)
			 VALUES ($1,'thread_key_contains','<itest-prr-c4/','github','x',true,true,'itest-prr')`, "capture_rules_pr_review_github"},
		{"exclude_pr_authors without pr_review",
			`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, exclude_pr_authors, note)
			 VALUES ($1,'thread_key_contains','<itest-prr-c5/','github','x','{"*[bot]"}','itest-prr')`, "capture_rules_exclude_needs_pr_review"},
	}
	for _, c := range bad {
		_, err := s.pool.Exec(ctx, c.q, s.collab)
		if err == nil {
			t.Errorf("%s: the insert succeeded; want a violation of %s (criterion 1)", c.name, c.constraint)
		} else if !strings.Contains(err.Error(), c.constraint) {
			t.Errorf("%s: err = %v, want the named CHECK %s", c.name, err, c.constraint)
		}
	}
	for _, q := range []string{
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, pr_review, note)
		 VALUES ($1,'thread_key_contains','<itest-prr-ok1/','github','x',true,'itest-prr')`,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, pr_review, exclude_pr_authors, note)
		 VALUES ($1,'thread_key_contains','<itest-prr-ok2/','github','x',true,'{"*[bot]","sspataro-alt"}','itest-prr')`,
	} {
		if _, err := s.pool.Exec(ctx, q, s.collab); err != nil {
			t.Errorf("a legal pr_review rule was refused: %v\n%s", err, q)
		}
	}
}

// ---- criteria 5, 6, 10, 11, 13 (first half), 18: a colleague's PR -----------------

func TestCapturePRReview_Integration_AColleaguesPRBecomesOneReviewTask(t *testing.T) {
	ctx := context.Background()
	s := newPRRSuite(t, ctx, prrOpts{})
	key := prrKey(prrWWW, 9101)

	open := s.colleague(t, ctx, prrWWW, 9101, "joseg-avviato", "Ranking widget", 30)
	// Criterion 18: his own emailed reply on the same PR thread is OUTBOUND. It
	// carries `author` on purpose: an outbound row must neither be decided nor
	// feed the verdict (D1 reads inbound mail only) — if it did, the colleague's
	// PR would read as his and create nothing.
	ours := s.mail(t, ctx, ghMail{repo: prrWWW, pr: 9101, reason: "author", sender: prrLogin, recipient: prrLogin,
		direction: "outbound", subject: "Re: [treetopllc/itest-prr-www] Ranking widget (PR #9101)", body: "looking now", minsAgo: 29})
	issue := s.mail(t, ctx, ghMail{repo: prrWWW, kind: "issues", pr: 77, reason: "subscribed", sender: "joseg-avviato",
		recipient: prrLogin, subject: "[treetopllc/itest-prr-www] Flaky ranking test (Issue #77)", body: "it flakes", minsAgo: 28})
	foundry := s.colleague(t, ctx, "Foundry-Underwriting/itest-prr-rave", 31, "joseg-avviato", "Rater", 27)
	tower := s.colleague(t, ctx, "tower987124/itest-prr-tool", 5, "joseg-avviato", "Tooling", 26)

	st := s.pass(t, ctx, "live")

	d := s.must(t, ctx, open, "live")
	if d.action != "task" || d.sys == nil || *d.sys != "github" || d.key == nil || *d.key != key {
		t.Fatalf("colleague PR decision = (%s, %v, %v), want (task, github, %s) — criteria 5/6: the canonical "+
			"key, never the path spelling", d.action, d.sys, d.key, key)
	}
	if d.ruleID == nil || *d.ruleID != s.prRule || !prrHas(d.ruleIDs, s.prRule) || !prrHas(d.ruleIDs, s.rule6) {
		t.Errorf("matched_rule_id %v / matched_rule_ids %v, want the PR rule %d winning over rule 6 %d, both recorded",
			d.ruleID, d.ruleIDs, s.prRule, s.rule6)
	}
	if !strings.Contains(d.reason, "joseg-avviato") {
		t.Errorf("reason %q does not name the author login (criterion 10: `other` names it when known)", d.reason)
	}

	task, url, _ := s.ref(t, ctx, key)
	if task == 0 {
		t.Fatalf("no external_refs (github, %s) after a live pass", key)
	}
	if url != prrURL(prrWWW, 9101) {
		t.Errorf("external_url = %q, want %q (github.PRURL)", url, prrURL(prrWWW, 9101))
	}
	if d.taskID == nil || *d.taskID != task {
		t.Errorf("decision task_id = %v, want %d", d.taskID, task)
	}
	var project, priority int64
	var assignee, status, title string
	var sourceThread *int64
	if err := s.pool.QueryRow(ctx,
		`SELECT project_id, assignee_type, status, priority, title, source_thread_id FROM tasks WHERE id=$1`, task).
		Scan(&project, &assignee, &status, &priority, &title, &sourceThread); err != nil {
		t.Fatalf("read task %d: %v", task, err)
	}
	if project != s.collab || assignee != "human" || status != "ready" || priority != 0 {
		t.Errorf("task = (project %d, %s, %s, priority %d), want (%d, human, ready, 0) — criterion 11 / D8: "+
			"ready, not holding", project, assignee, status, priority, s.collab)
	}
	if want := "Review PR #9101 — itest-prr-www: Ranking widget"; title != want {
		t.Errorf("title = %q, want %q (prReviewTitle)", title, want)
	}
	if sourceThread == nil || *sourceThread != open.thread {
		t.Errorf("source_thread_id = %v, want the PR thread %d (SWT-20 provenance)", sourceThread, open.thread)
	}
	// All via the executor, as the pass actor (invariant 3).
	if n := s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE actor=$1 AND tool='create_task' AND status='ok'
	                       AND args->>'title'=$2`, prrActor, title); n != 1 {
		t.Errorf("create_task audit rows for the review task = %d, want 1", n)
	}
	for _, tool := range []string{"link_external_ref", "task_set_source_thread"} {
		if n := s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE actor=$1 AND tool=$2 AND status='ok'
		                       AND (args->>'task_id')::bigint=$3`, prrActor, tool, task); n != 1 {
			t.Errorf("%s audit rows for task %d = %d, want 1", tool, task, n)
		}
	}
	if st.TasksCreated != 1 {
		t.Errorf("TasksCreated = %d, want 1", st.TasksCreated)
	}

	if _, ok := s.decision(t, ctx, ours.id, "live"); ok {
		t.Errorf("the OUTBOUND message got a decision row — criterion 18 / invariant 5")
	}
	if di := s.must(t, ctx, issue, "live"); di.action != "attributed" || di.ruleID == nil || *di.ruleID != s.prRule || di.key != nil {
		t.Errorf("issue thread decision = (%s, rule %v, key %v), want attributed by the PR rule %d with NO key "+
			"(criterion 5: /issues/ derives no key)", di.action, di.ruleID, di.key, s.prRule)
	}
	for _, m := range []prrMsg{foundry, tower} {
		if dm := s.must(t, ctx, m, "live"); dm.action != "unmatched" || prrHas(dm.ruleIDs, s.prRule) {
			t.Errorf("%s: decision (%s, rules %v), want unmatched and not matched by the PR rule", m.root, dm.action, dm.ruleIDs)
		}
	}

	// Criterion 13: a second and a third mail log onto the SAME task.
	c2 := s.mail(t, ctx, ghMail{repo: prrWWW, pr: 9101, reason: "comment", sender: "ananthsekar007", recipient: prrLogin,
		subject: "Re: [treetopllc/itest-prr-www] Ranking widget (PR #9101)", body: "@sspataro57 one nit below"})
	c3 := s.mail(t, ctx, ghMail{repo: prrWWW, pr: 9101, reason: "review_requested", sender: "joseg-avviato", recipient: prrLogin,
		subject: "Re: [treetopllc/itest-prr-www] Ranking widget (PR #9101)", body: "joseg-avviato requested your review"})
	logsBefore := s.events(t, ctx, task, "log")
	st2 := s.pass(t, ctx, "live")
	for _, m := range []prrMsg{c2, c3} {
		if dm := s.must(t, ctx, m, "live"); dm.action != "task_log" || dm.taskID == nil || *dm.taskID != task {
			t.Errorf("later mail %s: decision (%s, task %v), want task_log onto %d", m.mid, dm.action, dm.taskID, task)
		}
	}
	if got := s.events(t, ctx, task, "log") - logsBefore; got != 2 {
		t.Errorf("log events appended = %d, want 2", got)
	}
	s.mustRef(t, ctx, key)
	if n := s.n(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1 AND title LIKE 'Review PR #9101%'`, s.collab); n != 1 {
		t.Errorf("review tasks for PR 9101 = %d, want 1", n)
	}
	if st2.Appended != 2 || st2.TasksCreated != 0 {
		t.Errorf("second pass stats = %+v, want Appended 2, TasksCreated 0", st2)
	}
	// Criterion 6: no path-spelled github key anywhere.
	if n := s.n(t, ctx, `SELECT count(*) FROM capture_decisions WHERE external_system='github' AND external_key LIKE '%/pull/%'`); n != 0 {
		t.Errorf("%d capture_decisions carry a path-spelled github key; every github-keyed decision stores github.PRKey's", n)
	}
	if n := s.n(t, ctx, `SELECT count(*) FROM external_refs WHERE system='github' AND external_key LIKE '%/pull/%'`); n != 0 {
		t.Errorf("%d external_refs carry a path-spelled github key", n)
	}
}

// ---- criterion 8: his own PR falls through to exactly today's decision ------------

func TestCapturePRReview_Integration_HisOwnPRFallsThroughToTodaysDecision(t *testing.T) {
	ctx := context.Background()
	s := newPRRSuite(t, ctx, prrOpts{})
	bucket := s.seedBucket(t, ctx)

	// (a) a colleague comments on HIS PR: the mail itself says author.
	a := s.mail(t, ctx, ghMail{repo: prrWWW, pr: 9201, reason: "author", sender: "joseg-avviato", recipient: prrLogin,
		subject: "Re: [treetopllc/itest-prr-www] PRW-12 tidy the ranking (PR #9201)", body: "joseg-avviato approved these changes.",
		minsAgo: 20})
	// (b) keyless, collaboratory-www: the opening's sender IS its recipient (row 2);
	// its reason is deliberately not author, so only row 2 can call it his.
	bOpen := s.mail(t, ctx, ghMail{repo: prrWWW, pr: 9202, opening: true, reason: "subscribed", sender: prrLogin,
		recipient: prrLogin, subject: "[treetopllc/itest-prr-www] Tidy imports (PR #9202)", body: "opened", minsAgo: 19})
	bLater := s.mail(t, ctx, ghMail{repo: prrWWW, pr: 9202, reason: "comment", sender: "joseg-avviato", recipient: prrLogin,
		subject: "Re: [treetopllc/itest-prr-www] Tidy imports (PR #9202)", body: "nice", minsAgo: 18})
	// (c) a third treetopllc repo no attribution rule names.
	c := s.mail(t, ctx, ghMail{repo: prrThird, pr: 9203, reason: "author", sender: "ananthsekar007", recipient: prrLogin,
		subject: "Re: [treetopllc/itest-prr-third] Retry queue (PR #9203)", body: "lgtm", minsAgo: 17})

	st := s.pass(t, ctx, "live")

	da := s.must(t, ctx, a, "live")
	if da.action != "task_log" || da.taskID == nil || *da.taskID != bucket || da.ruleID == nil || *da.ruleID != s.rule10 ||
		da.key == nil || *da.key != "PRW" {
		t.Errorf("8(a): decision (%s, task %v, rule %v, key %v), want task_log onto rule 10's bucket task %d "+
			"(rule %d, key PRW) — exactly as without the new rule", da.action, da.taskID, da.ruleID, da.key, bucket, s.rule10)
	}
	for _, r := range []int64{s.prRule, s.rule10, s.rule6} {
		if !prrHas(da.ruleIDs, r) {
			t.Errorf("8(a): matched_rule_ids %v lacks rule %d — they come from the FULL evaluation", da.ruleIDs, r)
		}
	}
	if !strings.HasPrefix(da.reason, prrSkipped(s.prRule, prrKey(prrWWW, 9201))) {
		t.Errorf("8(a): reason %q does not begin %q", da.reason, prrSkipped(s.prRule, prrKey(prrWWW, 9201)))
	}
	for _, m := range []prrMsg{bOpen, bLater} {
		dm := s.must(t, ctx, m, "live")
		if dm.action != "attributed" || dm.ruleID == nil || *dm.ruleID != s.rule6 || dm.projectID == nil || *dm.projectID != s.collab {
			t.Errorf("8(b) %s: decision (%s, rule %v, project %v), want attributed by rule 6 (%d) to collaboratory",
				m.mid, dm.action, dm.ruleID, dm.projectID, s.rule6)
		}
		if !strings.HasPrefix(dm.reason, prrSkipped(s.prRule, prrKey(prrWWW, 9202))) {
			t.Errorf("8(b): reason %q does not begin %q", dm.reason, prrSkipped(s.prRule, prrKey(prrWWW, 9202)))
		}
	}
	dc := s.must(t, ctx, c, "live")
	if dc.action != "unmatched" || dc.projectID != nil || !prrHas(dc.ruleIDs, s.prRule) {
		t.Errorf("8(c): decision (%s, project %v, rules %v), want unmatched with the PR rule still in matched_rule_ids",
			dc.action, dc.projectID, dc.ruleIDs)
	}
	if !strings.HasPrefix(dc.reason, prrSkipped(s.prRule, prrKey(prrThird, 9203))) {
		t.Errorf("8(c): reason %q does not begin %q", dc.reason, prrSkipped(s.prRule, prrKey(prrThird, 9203)))
	}
	for _, k := range []string{prrKey(prrWWW, 9201), prrKey(prrWWW, 9202), prrKey(prrThird, 9203)} {
		s.noRef(t, ctx, k, "his own PR never gets a review task (invariant 5's J17 analogue)")
	}
	if st.PRAuthorSkipped != 4 {
		t.Errorf("PRAuthorSkipped = %d, want 4 (a, both b mails, c)", st.PRAuthorSkipped)
	}

	// "Exactly as without the new rule": disable it and decide the same mail in
	// SHADOW (--all). Action, task, rule, project and key must be identical.
	s.call(t, ctx, "capture_rule_set_enabled", 0, fmt.Sprintf(`{"rule_id":%d,"enabled":false}`, s.prRule))
	if _, err := capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{Mode: "shadow", Actor: prrActor, All: true}); err != nil {
		t.Fatalf("shadow --all without the PR rule: %v", err)
	}
	eq := func(a, b *int64) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
	eqs := func(a, b *string) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
	for _, m := range []prrMsg{a, bOpen, bLater, c} {
		live, shadow := s.must(t, ctx, m, "live"), s.must(t, ctx, m, "shadow")
		if live.action != shadow.action || !eq(live.taskID, shadow.taskID) || !eq(live.ruleID, shadow.ruleID) ||
			!eq(live.projectID, shadow.projectID) || !eqs(live.key, shadow.key) {
			t.Errorf("%s: with the PR rule (%s task %v rule %v) differs from without it (%s task %v rule %v) — "+
				"criterion 8: his own PR's mail gets EXACTLY today's decision", m.mid,
				live.action, live.taskID, live.ruleID, shadow.action, shadow.taskID, shadow.ruleID)
		}
	}
}

// ---- criterion 9: the verdict is read from the stored raw row ---------------------

func TestCapturePRReview_Integration_AuthorshipIsReadFromTheStoredRawRow(t *testing.T) {
	ctx := context.Background()
	s := newPRRSuite(t, ctx, prrOpts{})
	key := prrKey(prrThird, 9301)

	// No opening stored, so rows 2-4 cannot fire: m1's STORED header is the only
	// authorship evidence on the PR.
	m1 := s.mail(t, ctx, ghMail{repo: prrThird, pr: 9301, reason: "author", sender: "joseg-avviato", recipient: prrLogin,
		subject: "Re: [treetopllc/itest-prr-third] Batch export (PR #9301)", body: "first review", minsAgo: 20})
	m2 := s.mail(t, ctx, ghMail{repo: prrThird, pr: 9301, reason: "comment", sender: "joseg-avviato", recipient: prrLogin,
		subject: "Re: [treetopllc/itest-prr-third] Batch export (PR #9301)", body: "one more thing", minsAgo: 10})
	s.pass(t, ctx, "live")
	if _, _, n := s.ref(t, ctx, key); n != 0 {
		t.Fatalf("PR %s got a task although a stored mail says X-GitHub-Reason: author (row 1)", key)
	}
	if d := s.must(t, ctx, m2, "live"); !strings.HasPrefix(d.reason, prrSkipped(s.prRule, key)) {
		t.Errorf("m2 reason %q: the verdict for m2 must come from m1's stored header (a message on the same thread)", d.reason)
	}

	// MUTATION: flip ONE stored header author -> comment, then decide again.
	s.rewriteReason(t, ctx, m1, "comment")
	s.exec(t, ctx, `DELETE FROM capture_decisions WHERE message_id = ANY($1)`, []int64{m1.id, m2.id})
	s.pass(t, ctx, "live")
	task := s.mustRef(t, ctx, key)
	d1 := s.must(t, ctx, m1, "live")
	if d1.action != "task" || !strings.Contains(d1.reason, "author undetermined") {
		t.Errorf("after the flip, m1 decision = (%s, %q), want task with \"author undetermined\" — the stored raw "+
			"row is what decides, so flipping it must flip the outcome", d1.action, d1.reason)
	}
	if d2 := s.must(t, ctx, m2, "live"); d2.action != "task_log" || d2.taskID == nil || *d2.taskID != task {
		t.Errorf("after the flip, m2 decision = (%s, %v), want task_log onto %d", d2.action, d2.taskID, task)
	}
}

// ---- D1: the opening is found by exact Message-ID, across accounts ----------------

func TestCapturePRReview_Integration_TheOpeningIsFoundByMessageIDAcrossAccounts(t *testing.T) {
	ctx := context.Background()
	s := newPRRSuite(t, ctx, prrOpts{})

	// PR 9401: HIS opening landed in account B (thread gmail:B:<root>); a
	// colleague's comment landed in account A. The comment's own thread holds no
	// opening, so only the exact external_message_id lookup can see it's his.
	s.mail(t, ctx, ghMail{acctB: true, repo: prrWWW, pr: 9401, opening: true, reason: "subscribed", sender: prrLogin,
		recipient: prrLogin, subject: "[treetopllc/itest-prr-www] Own work (PR #9401)", body: "opened", minsAgo: 20})
	cA := s.mail(t, ctx, ghMail{repo: prrWWW, pr: 9401, reason: "comment", sender: "joseg-avviato", recipient: prrLogin,
		subject: "Re: [treetopllc/itest-prr-www] Own work (PR #9401)", body: "question", minsAgo: 10})
	s.pass(t, ctx, "live")
	s.noRef(t, ctx, prrKey(prrWWW, 9401), "the opening in account B names him; the lookup is by Message-ID, not by thread")
	if d := s.must(t, ctx, cA, "live"); d.action != "attributed" || !strings.HasPrefix(d.reason, prrSkipped(s.prRule, prrKey(prrWWW, 9401))) {
		t.Errorf("account A's comment on his PR: decision (%s, %q), want rule 6's attribution after the skip", d.action, d.reason)
	}
}

// ---- criterion 10: undetermined and row 5 ------------------------------------------

func TestCapturePRReview_Integration_UndeterminedAndReviewRequestedCreate(t *testing.T) {
	ctx := context.Background()
	s := newPRRSuite(t, ctx, prrOpts{})

	// collaboratory-www#3186's shape (0b): dependabot, subscribed, no opening stored.
	u := s.mail(t, ctx, ghMail{repo: prrThird, pr: 9451, reason: "subscribed", sender: "dependabot[bot]", recipient: prrLogin,
		subject: "Re: [treetopllc/itest-prr-third] Bump sanitize-html (PR #9451)", body: "rebased", minsAgo: 20})
	rr := s.mail(t, ctx, ghMail{repo: prrThird, pr: 9452, reason: "review_requested", sender: "joseg-avviato", recipient: prrLogin,
		subject: "Re: [treetopllc/itest-prr-third] Cache warmup (PR #9452)", body: "review please", minsAgo: 19})
	// A gmail:-shaped raw row ('{}'): contributes nothing, and never fails the pass.
	g := s.mail(t, ctx, ghMail{repo: prrThird, pr: 9453, rawJSON: `{}`, sender: "joseg-avviato",
		subject: "Re: [treetopllc/itest-prr-third] Retry budget (PR #9453)", body: "hm", minsAgo: 18})

	s.pass(t, ctx, "live")
	for _, c := range []struct {
		m    prrMsg
		n    int
		frag string
	}{{u, 9451, "author undetermined"}, {rr, 9452, "author not named"}, {g, 9453, "author undetermined"}} {
		d := s.must(t, ctx, c.m, "live")
		if d.action != "task" || !strings.Contains(d.reason, c.frag) {
			t.Errorf("PR %d: decision (%s, %q), want task with %q (criterion 10; D1 fail-open)", c.n, d.action, d.reason, c.frag)
		}
		s.mustRef(t, ctx, prrKey(prrThird, c.n))
	}
	var title string
	if err := s.pool.QueryRow(ctx, `SELECT title FROM tasks WHERE id=$1`, s.mustRef(t, ctx, prrKey(prrThird, 9451))).Scan(&title); err != nil {
		t.Fatalf("read title: %v", err)
	}
	if want := "Review PR #9451 — itest-prr-third: Bump sanitize-html"; title != want {
		t.Errorf("title = %q, want %q", title, want)
	}
}

// ---- criterion 12: a Jira key in a colleague's PR ----------------------------------

func TestCapturePRReview_Integration_AJiraKeyedPRGoesToTheReviewTaskUnlessLHH(t *testing.T) {
	ctx := context.Background()
	s := newPRRSuite(t, ctx, prrOpts{})
	bucket := s.seedBucket(t, ctx)
	bucketLogs := s.events(t, ctx, bucket, "log")

	w := s.colleague(t, ctx, prrWWW, 9461, "joseg-avviato", "PRW-1234 ranking widget", 10)
	l := s.colleague(t, ctx, prrWWW, 9462, "joseg-avviato", "PRLHH-77 recommendation fix", 9)
	s.pass(t, ctx, "live")

	dw := s.must(t, ctx, w, "live")
	if dw.action != "task" || dw.ruleID == nil || *dw.ruleID != s.prRule || !prrHas(dw.ruleIDs, s.rule10) {
		t.Errorf("PRW-keyed colleague PR: decision (%s, rule %v, rules %v), want the review task by the PR rule %d "+
			"(91 beats rule 10's 90), rule 10 recorded", dw.action, dw.ruleID, dw.ruleIDs, s.prRule)
	}
	var title string
	if err := s.pool.QueryRow(ctx, `SELECT title FROM tasks WHERE id=$1`, s.mustRef(t, ctx, prrKey(prrWWW, 9461))).Scan(&title); err != nil {
		t.Fatalf("read title: %v", err)
	}
	if want := "Review PR #9461 — itest-prr-www: PRW-1234 ranking widget"; title != want {
		t.Errorf("title = %q, want %q (the key stays visible)", title, want)
	}
	if got := s.events(t, ctx, bucket, "log"); got != bucketLogs {
		t.Errorf("rule 10's bucket task gained %d logs; the colleague's PR mail belongs to the review task", got-bucketLogs)
	}

	dl := s.must(t, ctx, l, "live")
	if dl.action != "task" || dl.ruleID == nil || *dl.ruleID != s.ruleLHH || dl.projectID == nil || *dl.projectID != s.reengine ||
		dl.key == nil || *dl.key != "PRLHH-77" || !prrHas(dl.ruleIDs, s.prRule) {
		t.Errorf("LHH-keyed PR: decision (%s, rule %v, project %v, key %v, rules %v), want rule 1 (%d, 100) on reengine "+
			"with key PRLHH-77", dl.action, dl.ruleID, dl.projectID, dl.key, dl.ruleIDs, s.ruleLHH)
	}
	s.noRef(t, ctx, prrKey(prrWWW, 9462), "an LHH-keyed Treetop PR is reengine work (D3)")
}

// ---- criterion 13: closed and dismissed review tasks -------------------------------

func TestCapturePRReview_Integration_LaterMailOnAClosedOrDismissedTask(t *testing.T) {
	ctx := context.Background()
	s := newPRRSuite(t, ctx, prrOpts{})

	s.colleague(t, ctx, prrWWW, 9471, "joseg-avviato", "Search facets", 30)
	s.colleague(t, ctx, prrWWW, 9472, "ananthsekar007", "Cron tidy", 29)
	s.pass(t, ctx, "live")
	done := s.mustRef(t, ctx, prrKey(prrWWW, 9471))
	dismissed := s.mustRef(t, ctx, prrKey(prrWWW, 9472))
	s.call(t, ctx, "task_close", done, fmt.Sprintf(`{"task_id":%d,"reason":"reviewed"}`, done))
	s.call(t, ctx, "task_dismiss", dismissed, fmt.Sprintf(`{"task_id":%d,"reason_code":"wrong_kind"}`, dismissed))
	doneLogs := s.events(t, ctx, done, "log")

	l1 := s.mail(t, ctx, ghMail{repo: prrWWW, pr: 9471, reason: "comment", sender: "joseg-avviato", recipient: prrLogin,
		subject: "Re: [treetopllc/itest-prr-www] Search facets (PR #9471)", body: "follow-up after your review"})
	l2 := s.mail(t, ctx, ghMail{repo: prrWWW, pr: 9472, reason: "comment", sender: "ananthsekar007", recipient: prrLogin,
		subject: "Re: [treetopllc/itest-prr-www] Cron tidy (PR #9472)", body: "actually it is a review"})
	st := s.pass(t, ctx, "live")

	if d := s.must(t, ctx, l1, "live"); d.action != "task_log" || d.taskID == nil || *d.taskID != done {
		t.Errorf("mail on a Done review task: decision (%s, %v), want task_log onto %d", d.action, d.taskID, done)
	}
	if got := s.status(t, ctx, done); got != "closed" {
		t.Errorf("Done review task status = %q, want closed — resurfacing is SWT-53's, not this ticket's (D4)", got)
	}
	if got := s.events(t, ctx, done, "log") - doneLogs; got != 1 {
		t.Errorf("logs on the Done task = +%d, want +1 (it lands in the history, it does not vanish)", got)
	}
	s.mustRef(t, ctx, prrKey(prrWWW, 9471))
	if n := s.n(t, ctx, `SELECT count(*) FROM tasks WHERE project_id=$1 AND title LIKE 'Review PR #9471%'`, s.collab); n != 1 {
		t.Errorf("review tasks for PR 9471 = %d, want 1", n)
	}

	if d := s.must(t, ctx, l2, "live"); d.action != "task_log" || d.taskID == nil || *d.taskID != dismissed {
		t.Errorf("mail on a dismissed review task: decision (%s, %v), want task_log onto %d", d.action, d.taskID, dismissed)
	}
	if got := s.status(t, ctx, dismissed); got == "closed" {
		t.Errorf("dismissed review task is still closed; SWT-36's guarded reopen must fire exactly as for any ref")
	}
	if n := s.n(t, ctx, `SELECT count(*) FROM task_dismissals WHERE task_id=$1 AND reopened_at IS NOT NULL`, dismissed); n != 1 {
		t.Errorf("reopened dismissals on task %d = %d, want 1", dismissed, n)
	}
	if st.Reopened != 1 {
		t.Errorf("Reopened = %d, want 1", st.Reopened)
	}
}

// ---- criterion 14 (OQ-2 = a): the merge/close notice closes the task ---------------

func TestCapturePRReview_Integration_AMergeOrCloseNoticeClosesTheReviewTask(t *testing.T) {
	ctx := context.Background()
	s := newPRRSuite(t, ctx, prrOpts{})
	for i, n := range []int{9501, 9502, 9503, 9504} {
		s.colleague(t, ctx, prrWWW, n, "joseg-avviato", fmt.Sprintf("Change %d", n), 60-i)
	}
	s.pass(t, ctx, "live")
	t1, t2, t3, t4 := s.mustRef(t, ctx, prrKey(prrWWW, 9501)), s.mustRef(t, ctx, prrKey(prrWWW, 9502)),
		s.mustRef(t, ctx, prrKey(prrWWW, 9503)), s.mustRef(t, ctx, prrKey(prrWWW, 9504))
	// t4 is active work: a session holds it.
	s.exec(t, ctx, `UPDATE tasks SET status='in_progress' WHERE id=$1`, t4)

	const footer = "\n\n—\nReply to this email directly, view it on GitHub."
	notice := func(n int, body string, mins int) prrMsg {
		return s.mail(t, ctx, ghMail{repo: prrWWW, pr: n, reason: "state_change", sender: "joseg-avviato", recipient: prrLogin,
			subject: fmt.Sprintf("Re: [treetopllc/itest-prr-www] Change %d (PR #%d)", n, n), body: body, minsAgo: mins})
	}
	logs := map[int64]int{}
	for _, id := range []int64{t1, t2, t3, t4} {
		logs[id] = s.events(t, ctx, id, "log")
	}
	n1 := notice(9501, "Merged #9501 into main."+footer, 9)
	n2 := notice(9502, "Closed #9502."+footer, 8)
	notice(9503, "Closed #9999."+footer, 7)                                   // another N
	notice(9503, "Thanks!\nMerged #9503 into main."+footer, 6)                // mid-body
	notice(9503, "This builds on the fix merged in #9503 earlier."+footer, 5) // 0d's prose
	n4 := notice(9504, "Merged #9504 into main."+footer, 4)
	n5 := notice(9505, "Closed #9505."+footer, 3) // the FIRST mail seen for PR 9505

	st, err := capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{Mode: "live", Actor: prrActor})
	if err != nil {
		t.Fatalf("EvaluateRules(live): %v — an activeWorkRefusal is a skip, never a pass failure (D5)", err)
	}

	for _, c := range []struct {
		task  int64
		msg   prrMsg
		n     int
		state string
	}{{t1, n1, 9501, "merged"}, {t2, n2, 9502, "closed"}} {
		if d := s.must(t, ctx, c.msg, "live"); d.action != "task_log" || d.taskID == nil || *d.taskID != c.task {
			t.Errorf("notice on PR %d: decision (%s, %v), want task_log onto %d (the close rides on it)", c.n, d.action, d.taskID, c.task)
		}
		if got := s.status(t, ctx, c.task); got != "closed" {
			t.Errorf("PR %d review task status = %q after its %s notice, want closed (OQ-2 = a)", c.n, got, c.state)
		}
		if got := s.events(t, ctx, c.task, "log") - logs[c.task]; got != 1 {
			t.Errorf("PR %d: logs +%d, want +1 (log FIRST, then close)", c.n, got)
		}
		want := fmt.Sprintf("PR #%d %s on GitHub%%", c.n, c.state)
		if n := s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE actor=$1 AND tool='task_close' AND status='ok'
		                       AND (args->>'task_id')::bigint=$2 AND args->>'reason' LIKE $3`, prrActor, c.task, want); n != 1 {
			t.Errorf("PR %d: task_close audit rows as %s with reason LIKE %q = %d, want 1", c.n, prrActor, want, n)
		}
		if n := s.n(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='status_changed'
		                       AND payload->>'to'='closed' AND payload->>'reason' LIKE $2`, c.task, want); n != 1 {
			t.Errorf("PR %d: status_changed events naming %q = %d, want 1", c.n, want, n)
		}
		if n := s.n(t, ctx, `SELECT count(*) FROM task_dismissals WHERE task_id=$1`, c.task); n != 0 {
			t.Errorf("PR %d: the close wrote a dismissal label; it counts as Done", c.n)
		}
	}
	if got := s.status(t, ctx, t3); got != "ready" {
		t.Errorf("PR 9503 status = %q, want ready: a different N, a mid-body notice and merged prose never close", got)
	}
	if got := s.events(t, ctx, t3, "log") - logs[t3]; got != 3 {
		t.Errorf("PR 9503 logs +%d, want +3", got)
	}
	if got := s.status(t, ctx, t4); got != "in_progress" {
		t.Errorf("active review task status = %q, want in_progress (close refuses active work; a skip)", got)
	}
	if d := s.must(t, ctx, n4, "live"); d.action != "task_log" {
		t.Errorf("notice on active work: action %q, want task_log", d.action)
	}
	if got := s.events(t, ctx, t4, "log") - logs[t4]; got != 1 {
		t.Errorf("active task logs +%d, want +1", got)
	}
	if st.PRClosed != 2 {
		t.Errorf("PRClosed = %d, want 2", st.PRClosed)
	}
	d5 := s.must(t, ctx, n5, "live")
	if d5.action != "attributed" || !strings.Contains(d5.reason, "PR already merged/closed; no review task") {
		t.Errorf("a notice that would create: decision (%s, %q), want attributed with \"PR already merged/closed; "+
			"no review task\"", d5.action, d5.reason)
	}
	s.noRef(t, ctx, prrKey(prrWWW, 9505), "a review of merged work is not work")
}

// ---- criterion 16 (+14's shadow half) -----------------------------------------------

func TestCapturePRReview_Integration_ShadowActsOnNothingAndDecidesLikeLive(t *testing.T) {
	ctx := context.Background()
	s := newPRRSuite(t, ctx, prrOpts{})
	s.colleague(t, ctx, prrWWW, 9601, "joseg-avviato", "Ranking v2", 60)
	s.pass(t, ctx, "live")
	task := s.mustRef(t, ctx, prrKey(prrWWW, 9601))

	notice := s.mail(t, ctx, ghMail{repo: prrWWW, pr: 9601, reason: "state_change", sender: "joseg-avviato", recipient: prrLogin,
		subject: "Re: [treetopllc/itest-prr-www] Ranking v2 (PR #9601)", body: "Merged #9601 into main.", minsAgo: 5})
	c := s.colleague(t, ctx, prrWWW, 9602, "ananthsekar007", "Export CSV", 4)
	own := s.mail(t, ctx, ghMail{repo: prrWWW, pr: 9603, reason: "author", sender: "joseg-avviato", recipient: prrLogin,
		subject: "Re: [treetopllc/itest-prr-www] His change (PR #9603)", body: "approved", minsAgo: 3})

	tables := []string{"tasks", "external_refs", "task_events", "audit_events", "task_dismissals"}
	before := map[string]int{}
	for _, tb := range tables {
		before[tb] = s.n(t, ctx, `SELECT count(*) FROM `+tb)
	}
	st := s.pass(t, ctx, "shadow")
	for _, tb := range tables {
		if got := s.n(t, ctx, `SELECT count(*) FROM `+tb); got != before[tb] {
			t.Errorf("%s changed across a SHADOW pass: %d -> %d (criterion 16: shadow creates, links, logs and "+
				"closes nothing)", tb, before[tb], got)
		}
	}
	if got := s.status(t, ctx, task); got != "ready" {
		t.Errorf("shadow closed the review task (status %q)", got)
	}
	if st.TasksCreated != 0 || st.Appended != 0 || st.PRClosed != 0 {
		t.Errorf("shadow stats = %+v, want TasksCreated, Appended and PRClosed all 0", st)
	}
	if st.PRAuthorSkipped != 1 {
		t.Errorf("shadow PRAuthorSkipped = %d, want 1 (the fall-through is a decision, counted in both modes)", st.PRAuthorSkipped)
	}
	if d := s.must(t, ctx, notice, "shadow"); d.action != "task_log" || !strings.Contains(d.reason, "would close") {
		t.Errorf("shadow notice decision (%s, %q), want task_log saying \"would close\"", d.action, d.reason)
	}

	shadow := map[int64]string{}
	for _, m := range []prrMsg{notice, c, own} {
		shadow[m.id] = s.must(t, ctx, m, "shadow").action
	}
	s.pass(t, ctx, "live")
	for _, m := range []prrMsg{notice, c, own} {
		if live := s.must(t, ctx, m, "live").action; live != shadow[m.id] {
			t.Errorf("%s: shadow action %q, live action %q — shadow records the same actions a live pass would",
				m.mid, shadow[m.id], live)
		}
	}
	if got := s.status(t, ctx, task); got != "closed" {
		t.Errorf("the live pass did not close the merged PR's task (status %q)", got)
	}
}

// ---- criterion 15 (OQ-1 = b): bot PRs ---------------------------------------------

func TestCapturePRReview_Integration_ABotPRGetsATaskUnderTheSeededRule(t *testing.T) {
	ctx := context.Background()
	s := newPRRSuite(t, ctx, prrOpts{}) // the seeded rule: exclude list EMPTY
	bot := s.colleague(t, ctx, prrWWW, 9701, "dependabot[bot]", "Bump sanitize-html from 2.11.0 to 2.12.1", 5)
	st := s.pass(t, ctx, "live")

	d := s.must(t, ctx, bot, "live")
	if d.action != "task" || !strings.Contains(d.reason, "dependabot[bot]") {
		t.Errorf("dependabot PR under the seeded rule: decision (%s, %q), want task naming dependabot[bot] — OQ-1 = (b)",
			d.action, d.reason)
	}
	var title string
	if err := s.pool.QueryRow(ctx, `SELECT title FROM tasks WHERE id=$1`, s.mustRef(t, ctx, prrKey(prrWWW, 9701))).Scan(&title); err != nil {
		t.Fatalf("read title: %v", err)
	}
	if want := "Review PR #9701 — itest-prr-www: Bump sanitize-html from 2.11.0 to 2.12.1"; title != want {
		t.Errorf("title = %q, want %q", title, want)
	}
	if st.PRAuthorSkipped != 0 {
		t.Errorf("PRAuthorSkipped = %d, want 0", st.PRAuthorSkipped)
	}
}

func TestCapturePRReview_Integration_AnExcludedAuthorFallsThrough(t *testing.T) {
	ctx := context.Background()
	s := newPRRSuite(t, ctx, prrOpts{exclude: []string{"*[bot]"}})
	bot := s.colleague(t, ctx, prrWWW, 9711, "dependabot[bot]", "Bump lodash", 6)
	colleague := s.colleague(t, ctx, prrWWW, 9712, "joseg-avviato", "Real work", 5)
	st := s.pass(t, ctx, "live")

	d := s.must(t, ctx, bot, "live")
	if d.action != "attributed" || d.ruleID == nil || *d.ruleID != s.rule6 {
		t.Errorf("excluded bot PR: decision (%s, rule %v), want rule 6's attribution (D1 row 3 falls through)", d.action, d.ruleID)
	}
	if prefix := fmt.Sprintf("rule %d skipped: PR %s", s.prRule, prrKey(prrWWW, 9711)); !strings.HasPrefix(d.reason, prefix) ||
		!strings.Contains(d.reason, "dependabot[bot]") {
		t.Errorf("reason %q, want it to begin %q and name dependabot[bot]", d.reason, prefix)
	}
	s.noRef(t, ctx, prrKey(prrWWW, 9711), "an excluded author creates no task")
	if dc := s.must(t, ctx, colleague, "live"); dc.action != "task" {
		t.Errorf("a colleague's PR under the same rule: action %q, want task (exclusion is per login)", dc.action)
	}
	if st.PRAuthorSkipped != 1 {
		t.Errorf("PRAuthorSkipped = %d, want 1", st.PRAuthorSkipped)
	}
}

// ---- criterion 4: both columns are COLUMN-fed ---------------------------------------

func TestCapturePRReview_Integration_BothRuleColumnsAreColumnFed(t *testing.T) {
	ctx := context.Background()
	s := newPRRSuite(t, ctx, prrOpts{exclude: []string{"*[bot]"}})

	// exclude_pr_authors. MUTATION: `'{}'::text[]` for r.exclude_pr_authors in
	// loadRules' SELECT turns this first assertion red.
	bot := s.colleague(t, ctx, prrThird, 9801, "dependabot[bot]", "Bump axios", 10)
	s.pass(t, ctx, "live")
	s.noRef(t, ctx, prrKey(prrThird, 9801), "the stored exclude list says *[bot]")
	s.exec(t, ctx, `UPDATE capture_rules SET exclude_pr_authors='{}' WHERE id=$1`, s.prRule)
	s.exec(t, ctx, `DELETE FROM capture_decisions WHERE message_id=$1`, bot.id)
	s.pass(t, ctx, "live")
	s.mustRef(t, ctx, prrKey(prrThird, 9801))

	// pr_review. MUTATION: `false` for r.pr_review turns this red (a plain github
	// rule gives his own PR a task).
	own := s.mail(t, ctx, ghMail{repo: prrThird, pr: 9802, reason: "author", sender: "joseg-avviato", recipient: prrLogin,
		subject: "Re: [treetopllc/itest-prr-third] Own change (PR #9802)", body: "approved"})
	s.pass(t, ctx, "live")
	s.noRef(t, ctx, prrKey(prrThird, 9802), "pr_review=true reads his own PR as his")
	s.exec(t, ctx, `UPDATE capture_rules SET pr_review=false WHERE id=$1`, s.prRule)
	s.exec(t, ctx, `DELETE FROM capture_decisions WHERE message_id=$1`, own.id)
	s.pass(t, ctx, "live")
	task := s.mustRef(t, ctx, prrKey(prrThird, 9802)) // criterion 6: canonical even for a plain github rule
	var title string
	if err := s.pool.QueryRow(ctx, `SELECT title FROM tasks WHERE id=$1`, task).Scan(&title); err != nil {
		t.Fatalf("read title: %v", err)
	}
	if !strings.HasPrefix(title, prrKey(prrThird, 9802)+" — ") || strings.HasPrefix(title, "Review PR") {
		t.Errorf("plain github rule title = %q, want ruleTaskTitle's `{key} — {subject}` (review titles apply to "+
			"pr_review rules only)", title)
	}
	if n := s.n(t, ctx, `SELECT count(*) FROM external_refs WHERE system='github' AND external_key LIKE '%/pull/%'`); n != 0 {
		t.Errorf("%d path-spelled github refs (criterion 6)", n)
	}
}
