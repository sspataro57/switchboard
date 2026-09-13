//go:build integration

package classify_test

// SWT-40 Part B criterion B2 against a real database: the ROUTE inbox, one
// fixture per clause, each mutation named inline.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_isob?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run ClassifyRoute ./internal/classify/
//
// USE AN ISOLATED DATABASE (IK "Test infrastructure", 2026-09-12). Build-tagged
// `integration`, env-gated on DATABASE_URL, FATAL on 192.168.50.49.
//
// TEST THE COLUMN, NOT THE FIXTURE (IK SWT-21 #6). Every clause below turns on a
// value Postgres produces: nm.direction, the EXISTENCE of a mode='live'
// 'unmatched' decision, the LATEST decision in any mode, the account's
// source_account_projects rows (reached through raw_source_items.source_account_id,
// B-D9's carve-out), and the worker_type of an existing extraction.
//
// ---- IMPOSED SURFACE (beyond route_test.go's) --------------------------------
//
//	PGStore.PendingMessages(ctx, Config{Lane: LaneRoute, Since: …}) is the route
//	inbox (B2):
//	  - nm.direction = 'inbound';
//	  - EXISTS a mode='live' action='unmatched' decision for the message;
//	  - the LATEST decision (ORDER BY id DESC, ANY mode) is 'unmatched';
//	  - the receiving account (raw_source_items.source_account_id) has >= 1
//	    source_account_projects row — the personal gmail account is the zero-row
//	    control;
//	  - NOT EXISTS an extraction under worker_type 'classify_route';
//	  - sent_at within Since (Run refuses Since <= 0, route_test.go).
//	A route verdict is CURRENT only if it was recorded (ai_runs.created_at) at
//	or after the account's source_accounts.route_after (SPEC amendment
//	2026-09-13, resolving V6.5's "arm → backfill → confirm the 115"):
//	  - route_after NULL (unarmed): ANY route verdict excludes the message, so
//	    shadow classifies each message once, and verdicts accumulate before
//	    arming (B-D7): an unarmed account's unverdicted messages ARE in the inbox;
//	  - route_after set: a message whose route verdicts ALL predate it is back
//	    in the inbox; one verdict at or after it (>=) excludes it. One fresh
//	    verdict is all a message ever needs.
//	B7 is unchanged: route_apply never applies a pre-arming verdict.
//	Rows carry Attribution = AttrUnmatched, ProjectID 0, SourceAccountID and
//	the account's Candidates (ProjectID, Slug, Name, Client, Description,
//	IsDefault).
//
// GREENFIELD NOTE — EXPECTED RED: classify.LaneRoute and PendingMessage's new
// fields do not exist (compile failure first); past that, source_account_projects
// does not exist until migrations/0032_route_tier.sql is applied (rtiRequire0032).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/provider"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	rtiProvider  = "itest-route-src"
	rtiModel     = "itest-route-model"
	rtiThreadPfx = "gmail:itest-route:"
	rtiWindow    = 720 * time.Hour
)

type rtiSuite struct {
	pool            *pgxpool.Pool
	armed, personal int64 // source accounts
	collab, beta    int64 // projects
	seq             int
}

func rtiRequire0032(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var present bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('source_account_projects') IS NOT NULL`).Scan(&present); err != nil {
		t.Fatalf("probe source_account_projects: %v", err)
	}
	if !present {
		t.Fatalf("source_account_projects does not exist: migrations/0032_route_tier.sql (SWT-40 Part B) is not " +
			"applied to this database")
	}
}

func rtiCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE provider = '` + rtiProvider + `')`
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`
	const projs = `(SELECT id FROM projects WHERE slug LIKE 'itest-route-%')`
	for _, q := range []string{
		`DELETE FROM ai_extractions WHERE ai_run_id IN (SELECT id FROM ai_runs WHERE model = '` + rtiModel + `')`,
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM ai_runs WHERE model = '` + rtiModel + `'`,
		`DELETE FROM capture_decisions WHERE message_id IN
		   (SELECT id FROM normalized_messages WHERE raw_source_item_id IN ` + raws + `)`,
		`DELETE FROM capture_decisions WHERE project_id IN ` + projs,
		`DELETE FROM source_account_projects WHERE source_account_id IN ` + accts + ` OR project_id IN ` + projs,
		`DELETE FROM projects WHERE slug LIKE 'itest-route-%'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE thread_key LIKE '` + rtiThreadPfx + `%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider = '` + rtiProvider + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func newRTISuite(t *testing.T, ctx context.Context) *rtiSuite {
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
	rtiRequire0032(t, ctx, pool)
	rtiCleanup(t, ctx, pool)
	t.Cleanup(func() { rtiCleanup(t, context.Background(), pool) })

	s := &rtiSuite{pool: pool}
	s.armed = s.id(t, ctx, `INSERT INTO source_accounts (provider, account_email, send_enabled)
	                        VALUES ($1,'itest-route-hoc@example.test',false) RETURNING id`, rtiProvider)
	s.personal = s.id(t, ctx, `INSERT INTO source_accounts (provider, account_email, send_enabled)
	                           VALUES ($1,'itest-route-personal@example.test',false) RETURNING id`, rtiProvider)
	project := func(slug string) int64 {
		return s.id(t, ctx, `INSERT INTO projects (name, slug, client, execution, delivery, ai_locality, ai_classify, ai_inquiry)
		                     VALUES ($1,$1,'itest-route-client','manual','dashboard','any',false,false) RETURNING id`, slug)
	}
	s.collab = project("itest-route-collab")
	s.beta = project("itest-route-beta")
	// Seeded directly: test-only. Production rows go in through route_candidate_add.
	s.exec(t, ctx, `INSERT INTO source_account_projects (source_account_id, project_id, is_default, description)
	                VALUES ($1,$2,true,'itest route collab description'), ($1,$3,false,'itest route beta description')`,
		s.armed, s.collab, s.beta)
	return s
}

func (s *rtiSuite) id(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", q, err)
	}
	return id
}

func (s *rtiSuite) exec(t *testing.T, ctx context.Context, q string, args ...any) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// msg seeds one message on its own gmail thread.
func (s *rtiSuite) msg(t *testing.T, ctx context.Context, acct int64, direction string, sentAgo time.Duration) (int64, int64) {
	t.Helper()
	s.seq++
	label := fmt.Sprintf("m%d", s.seq)
	raw := s.id(t, ctx, `INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
	                     VALUES ($1,$2,'{}',$3,now()) RETURNING id`, acct, "itest-route-"+label, "itest-route-h-"+label)
	th := s.id(t, ctx, `INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,$2,'[]') RETURNING id`,
		rtiThreadPfx+label, "itest-route "+label)
	m := s.id(t, ctx, `INSERT INTO normalized_messages
	                     (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
	                   VALUES ($1,$2,$3,$4, now() - make_interval(secs => $5), $6, $7, 'Pat Doe <pat@univ.example.test>', 'gmail')
	                   RETURNING id`,
		raw, th, direction, "<itest-route-"+label+"@example.test>", sentAgo.Seconds(),
		"itest route body "+label, "itest route subject "+label)
	return m, raw
}

func (s *rtiSuite) dec(t *testing.T, ctx context.Context, msg int64, mode, action string, project int64) {
	t.Helper()
	var p any
	if project != 0 {
		p = project
	}
	s.exec(t, ctx, `INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
	                VALUES ($1,$2,$3,$4,'itest-route')`, msg, mode, action, p)
}

func (s *rtiSuite) extraction(t *testing.T, ctx context.Context, raw int64, workerType string) {
	t.Helper()
	run := s.id(t, ctx, `INSERT INTO ai_runs (worker_type, provider, model, input, output, status)
	                     VALUES ($1,'itest',$2,'{}','{}','ok') RETURNING id`, workerType, rtiModel)
	s.exec(t, ctx, `INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields) VALUES ($1,$2,'{}')`, run, raw)
}

func (s *rtiSuite) inbox(t *testing.T, ctx context.Context) map[int64]classify.PendingMessage {
	t.Helper()
	got, err := classify.NewStore(s.pool).PendingMessages(ctx, classify.Config{Lane: classify.LaneRoute, Since: rtiWindow})
	if err != nil {
		t.Fatalf("route inbox: %v", err)
	}
	out := map[int64]classify.PendingMessage{}
	for _, m := range got {
		out[m.MessageID] = m
	}
	return out
}

func TestClassifyRouteInbox_Integration_OneFixturePerClause(t *testing.T) {
	ctx := context.Background()
	s := newRTISuite(t, ctx)
	h := time.Hour

	in := map[string]int64{}
	out := map[string]int64{}

	base, _ := s.msg(t, ctx, s.armed, "inbound", 2*h)
	s.dec(t, ctx, base, "live", "unmatched", 0)
	in["base: inbound, live unmatched, latest unmatched, candidate account, no route verdict"] = base

	residue, residueRaw := s.msg(t, ctx, s.armed, "inbound", 2*h)
	s.dec(t, ctx, residue, "live", "unmatched", 0)
	s.extraction(t, ctx, residueRaw, "classify_residue")
	// Mutation: NOT EXISTS over ANY worker_type → this one leaves the inbox.
	in["cross-lane control: a residue-lane verdict does not hide it"] = residue

	// Mutation: drop nm.direction='inbound'. (Synthetic: capture never decides an
	// outbound message — the row isolates the clause.)
	outbound, _ := s.msg(t, ctx, s.armed, "outbound", 2*h)
	s.dec(t, ctx, outbound, "live", "unmatched", 0)
	out["outbound message"] = outbound

	// Mutation: drop EXISTS live unmatched → both of these enter.
	shadowOnly, _ := s.msg(t, ctx, s.armed, "inbound", 2*h)
	s.dec(t, ctx, shadowOnly, "shadow", "unmatched", 0)
	out["no live decision at all (shadow unmatched only)"] = shadowOnly
	liveAttributed, _ := s.msg(t, ctx, s.armed, "inbound", 2*h)
	s.dec(t, ctx, liveAttributed, "live", "attributed", s.collab)
	s.dec(t, ctx, liveAttributed, "shadow", "unmatched", 0)
	out["live decision attributed, a later shadow row unmatched"] = liveAttributed

	// Mutation: read the latest decision with a mode predicate, or drop the
	// latest clause → these enter.
	repointed, _ := s.msg(t, ctx, s.armed, "inbound", 2*h)
	s.dec(t, ctx, repointed, "live", "unmatched", 0)
	s.dec(t, ctx, repointed, "shadow", "attributed", s.collab)
	out["latest decision attributed (a later shadow rule re-pointed it)"] = repointed
	routed, _ := s.msg(t, ctx, s.armed, "inbound", 2*h)
	s.dec(t, ctx, routed, "live", "unmatched", 0)
	s.exec(t, ctx, `INSERT INTO capture_decisions (message_id, mode, action, project_id, route_step, reason)
	                VALUES ($1,'route','attributed',$2,'default','itest-route')`, routed, s.collab)
	out["already routed (a mode='route' row is the latest)"] = routed

	// Mutation: drop the NOT EXISTS classify_route → this enters.
	classified, classifiedRaw := s.msg(t, ctx, s.armed, "inbound", 2*h)
	s.dec(t, ctx, classified, "live", "unmatched", 0)
	s.extraction(t, ctx, classifiedRaw, "classify_route")
	out["already has a classify_route verdict"] = classified

	// Mutation: drop the --since bound → this enters.
	old, _ := s.msg(t, ctx, s.armed, "inbound", rtiWindow+48*h)
	s.dec(t, ctx, old, "live", "unmatched", 0)
	out["sent before --since"] = old

	// Mutation: drop the candidate-rows clause → this enters. B-D1: only accounts
	// with candidate rows are routed at all.
	personal, _ := s.msg(t, ctx, s.personal, "inbound", 2*h)
	s.dec(t, ctx, personal, "live", "unmatched", 0)
	out["the personal account: no candidate rows (the zero-row control)"] = personal

	var armedAt *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT route_after FROM source_accounts WHERE id=$1`, s.armed).Scan(&armedAt); err != nil {
		t.Fatalf("read route_after: %v", err)
	}
	if armedAt != nil {
		t.Fatalf("fixture drift: the candidate account is armed (%v); this test asserts the inbox fills while UNARMED", armedAt)
	}

	got := s.inbox(t, ctx)
	for why, id := range in {
		if _, ok := got[id]; !ok {
			t.Errorf("%s (message %d): not in the route inbox", why, id)
		}
	}
	for why, id := range out {
		if _, ok := got[id]; ok {
			t.Errorf("%s (message %d): in the route inbox; B2 excludes it", why, id)
		}
	}

	m, ok := got[base]
	if !ok {
		t.Fatalf("base message missing; the row assertions below need it")
	}
	if m.SourceAccountID != s.armed {
		t.Errorf("SourceAccountID = %d, want %d — the value comes from raw_source_items.source_account_id "+
			"(B-D9's named join); a constant here routes every account through one candidate set", m.SourceAccountID, s.armed)
	}
	if m.Attribution != provider.AttrUnmatched || m.ProjectID != 0 {
		t.Errorf("Attribution = %v, ProjectID = %d; want AttrUnmatched and no project. B-D8: an unmatched message "+
			"is ClassRestricted through ClassOf", m.Attribution, m.ProjectID)
	}
	byProject := map[int64]classify.RouteCandidate{}
	for _, c := range m.Candidates {
		byProject[c.ProjectID] = c
	}
	if len(m.Candidates) != 2 || len(byProject) != 2 {
		t.Fatalf("Candidates = %+v, want the account's two rows", m.Candidates)
	}
	for id, want := range map[int64]classify.RouteCandidate{
		s.collab: {ProjectID: s.collab, Slug: "itest-route-collab", Name: "itest-route-collab", Client: "itest-route-client",
			Description: "itest route collab description", IsDefault: true},
		s.beta: {ProjectID: s.beta, Slug: "itest-route-beta", Name: "itest-route-beta", Client: "itest-route-client",
			Description: "itest route beta description"},
	} {
		if got := byProject[id]; got != want {
			t.Errorf("candidate %d = %+v, want %+v (slug, name, client and the row's description, B-D4)", id, got, want)
		}
	}

	// Arming never removes a message that has no verdict at all. (What arming
	// DOES change — a pre-arming verdict stops counting — is
	// TestClassifyRouteInbox_Integration_AVerdictIsCurrentOnlyFromArming.)
	s.exec(t, ctx, `UPDATE source_accounts SET route_after = now() WHERE id=$1`, s.armed)
	if _, ok := s.inbox(t, ctx)[base]; !ok {
		t.Errorf("arming the account removed the base message from the route inbox; arming never removes a message that has no verdict")
	}
}

// The NOT EXISTS clause must match what the lane itself WRITES: a verdict
// recorded by Run through PGStore leaves the inbox, and its fields carry the
// account the carve-out join read.
func TestClassifyRouteInbox_Integration_AVerdictLeavesTheInbox(t *testing.T) {
	ctx := context.Background()
	s := newRTISuite(t, ctx)
	base, _ := s.msg(t, ctx, s.armed, "inbound", time.Hour)
	s.dec(t, ctx, base, "live", "unmatched", 0)

	local := cfLocal()
	local.verdict = `{"project_index":1,"evidence":"itest route body","reason":"itest"}`
	st := classify.NewStore(s.pool)
	stats, err := classify.Run(ctx, st, provider.NewRouter(nil, local, time.Minute),
		classify.Config{Model: rtiModel, MaxTokens: 256, Lane: classify.LaneRoute, Since: rtiWindow})
	if err != nil {
		t.Fatalf("Run(route): %v", err)
	}
	if stats.Processed < 1 {
		t.Fatalf("stats = %+v, want the base message classified", stats)
	}
	if _, ok := s.inbox(t, ctx)[base]; ok {
		t.Errorf("the base message is still in the route inbox after its verdict; the NOT EXISTS does not key on " +
			"the worker_type the lane records (classify_route)")
	}
	var raw []byte
	if err := s.pool.QueryRow(ctx, `SELECT e.fields FROM ai_extractions e JOIN ai_runs r ON r.id = e.ai_run_id
	                                 JOIN normalized_messages nm ON nm.raw_source_item_id = e.raw_source_item_id
	                                WHERE nm.id = $1 AND r.worker_type = 'classify_route'`, base).Scan(&raw); err != nil {
		t.Fatalf("read the verdict: %v", err)
	}
	var f map[string]any
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("fields: %v", err)
	}
	if f["source_account_id"] != float64(s.armed) {
		t.Errorf("fields.source_account_id = %v, want %d", f["source_account_id"], s.armed)
	}
	if p, _ := f["project_id"].(float64); int64(p) != s.collab && int64(p) != s.beta {
		t.Errorf("fields.project_id = %v, want one of the account's candidates (%d, %d)", f["project_id"], s.collab, s.beta)
	}
	if g, _ := f["grounded"].(bool); !g {
		t.Errorf("fields.grounded = %v; the evidence is a verbatim span of the body", f["grounded"])
	}
}

// SPEC amendment (2026-09-13), resolving V6.5's backfill: on an ARMED account a
// route verdict is current only if it was recorded at or after route_after, so
// the shadow-period verdicts do not keep the backlog out of the post-arming
// `classify run --lane route --since 720h`. One fixture per case.
//
// Mutations that must turn this red:
//   - any route verdict excludes (today's clause, no route_after comparison)
//     → "armed, verdict before route_after" stays out;
//   - `>` instead of `>=` → "exactly at route_after" enters;
//   - a NULL route_after read as "everything predates it" → the unarmed case enters;
//   - "all verdicts predate" read as "any verdict predates" → "before and after" enters.
func TestClassifyRouteInbox_Integration_AVerdictIsCurrentOnlyFromArming(t *testing.T) {
	ctx := context.Background()
	s := newRTISuite(t, ctx) // NOTE: s.armed has candidates but route_after NULL (unarmed)

	armedAcct := s.id(t, ctx, `INSERT INTO source_accounts (provider, account_email, send_enabled)
	                           VALUES ($1,'itest-route-armed@example.test',false) RETURNING id`, rtiProvider)
	// Whole seconds, so "exactly at route_after" is an exact timestamptz equality.
	s.exec(t, ctx, `UPDATE source_accounts SET route_after = date_trunc('second', now()) - interval '1 day' WHERE id = $1`, armedAcct)
	s.exec(t, ctx, `INSERT INTO source_account_projects (source_account_id, project_id, is_default, description)
	                VALUES ($1,$2,true,'itest route collab description'), ($1,$3,false,'itest route beta description')`,
		armedAcct, s.collab, s.beta)

	message := func(acct int64) (int64, int64) {
		t.Helper()
		m, raw := s.msg(t, ctx, acct, "inbound", 2*time.Hour)
		s.dec(t, ctx, m, "live", "unmatched", 0)
		return m, raw
	}
	verdict := func(raw int64, createdAt string, args ...any) {
		t.Helper()
		q := `INSERT INTO ai_runs (worker_type, provider, model, input, output, status, created_at)
		      VALUES ('classify_route','itest','` + rtiModel + `','{}','{}','ok', ` + createdAt + `) RETURNING id`
		run := s.id(t, ctx, q, args...)
		s.exec(t, ctx, `INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields) VALUES ($1,$2,'{}')`, run, raw)
	}
	// relativeToArming records a route verdict at the armed account's route_after + offset.
	relativeToArming := func(raw int64, offset time.Duration) {
		t.Helper()
		verdict(raw, `(SELECT route_after FROM source_accounts WHERE id = $1) + make_interval(secs => $2)`,
			armedAcct, offset.Seconds())
	}

	in := map[string]int64{}
	out := map[string]int64{}

	before, beforeRaw := message(armedAcct)
	relativeToArming(beforeRaw, -time.Hour)
	in["armed, its only verdict recorded before route_after"] = before

	noVerdict, _ := message(armedAcct)
	in["armed, no verdict at all (control)"] = noVerdict

	after, afterRaw := message(armedAcct)
	relativeToArming(afterRaw, time.Hour)
	out["armed, verdict recorded after route_after"] = after

	exactly, exactlyRaw := message(armedAcct)
	relativeToArming(exactlyRaw, 0)
	out["armed, verdict recorded exactly at route_after (>=)"] = exactly

	both, bothRaw := message(armedAcct)
	relativeToArming(bothRaw, -time.Hour)
	relativeToArming(bothRaw, time.Hour)
	out["armed, verdicts both before and after route_after"] = both

	unarmed, unarmedRaw := message(s.armed)
	verdict(unarmedRaw, `now() - interval '30 days'`)
	out["unarmed (route_after NULL), an old verdict"] = unarmed

	var armedAt *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT route_after FROM source_accounts WHERE id=$1`, s.armed).Scan(&armedAt); err != nil {
		t.Fatalf("read route_after: %v", err)
	}
	if armedAt != nil {
		t.Fatalf("fixture drift: the unarmed candidate account has route_after %v", armedAt)
	}

	got := s.inbox(t, ctx)
	for why, id := range in {
		if _, ok := got[id]; !ok {
			t.Errorf("%s (message %d): not in the route inbox", why, id)
		}
	}
	for why, id := range out {
		if _, ok := got[id]; ok {
			t.Errorf("%s (message %d): in the route inbox; a current verdict excludes it", why, id)
		}
	}

	// The backfill: ONE fresh verdict is all a message needs. Run records it at
	// now() (>= route_after), and the message leaves the inbox again.
	local := cfLocal()
	local.verdict = `{"project_index":1,"evidence":"itest route body","reason":"itest"}`
	if _, err := classify.Run(ctx, classify.NewStore(s.pool), provider.NewRouter(nil, local, time.Minute),
		classify.Config{Model: rtiModel, MaxTokens: 256, Lane: classify.LaneRoute, Since: rtiWindow}); err != nil {
		t.Fatalf("Run(route) backfill: %v", err)
	}
	after2 := s.inbox(t, ctx)
	for why, id := range map[string]int64{"the re-classified pre-arming message": before, "the control": noVerdict} {
		if _, ok := after2[id]; ok {
			t.Errorf("%s (message %d) is still in the route inbox after the backfill wrote a fresh verdict; one "+
				"post-arming verdict must exclude it", why, id)
		}
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM ai_extractions e JOIN ai_runs r ON r.id = e.ai_run_id
	                                 WHERE r.worker_type = 'classify_route' AND e.raw_source_item_id = $1`, beforeRaw).Scan(&n); err != nil {
		t.Fatalf("count verdicts: %v", err)
	}
	if n != 2 {
		t.Errorf("the pre-arming message carries %d route verdicts after the backfill, want 2 (the stale one, kept, "+
			"and exactly one fresh one)", n)
	}
}
