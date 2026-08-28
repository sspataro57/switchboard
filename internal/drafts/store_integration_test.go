//go:build integration

package drafts_test

// The integration suite internal/drafts never had (worker_test.go:8-10: "the SQL
// halves — Deliver-task queue, channel/thread resolution, advisory lock — belong
// to the integration suite", which was never written). SWT-17 §9 requires it,
// because migration 0015 drops projects.client_person_id and reworks every
// resolution path in PGStore.DeliverTasks — a package with no live deployment
// (cmd/drafts is not running) and, until this file, no coverage at all.
//
// Acceptance criteria 22, 23, 24, 25, 26, plus the SWT-19 behaviour §9's
// re-validation says the rework must PRESERVE.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run DraftsStore ./internal/drafts/
//
// GREENFIELD NOTE: fails today. store.go resolves gmail threads through
// person_identities and upwork clients through projects.client_person_id, so every
// case below that seeds NO people row resolves nothing — which is the point:
// criterion 23 says "with no people or person_identities row anywhere in the
// fixture". Expected red state.
//
// Cross-suite discipline: this suite owns projects slugged itest-dstore-%, the
// source account itest-dstore@gmail.example.test, the four upwork client uuids
// below and the thread_key prefixes gmail:itest-dstore:, upwork_crm:{those
// uuids}:, jira:dstore.jira.com:, slack:TDSTORE:. It deletes exactly those, in FK
// order, at start and end. It deliberately creates NO people / person_identities
// rows — see criterion 26.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/drafts"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	dsAccount = "itest-dstore@gmail.example.test"

	// Real-shaped upwork identifiers: uuid client ids and `room_<hex>` rooms, the
	// identifier space `communications.upwork_room_id` / `send_room_id` actually
	// draw from.
	dsClientLegacyOnly = "dddd1111-0000-0000-0000-00000000ds01"
	dsClientOneRoom    = "dddd2222-0000-0000-0000-00000000ds02"
	dsClientTwoRooms   = "dddd3333-0000-0000-0000-00000000ds03"
	dsRoomA            = "room_a1b2c3d4e5"
	dsRoomB            = "room_f6e5d4c3b2"

	dsGmailThread = "gmail:itest-dstore:18f0dstore01"
	dsJiraThread  = "jira:dstore.jira.com:WEB-4242"
	dsSlackThread = "slack:TDSTORE:C0DSTORE"
)

func dsLegacyKey(client string) string       { return "upwork_crm:" + client + ":upwork" }
func dsRoomedKey(client, room string) string { return "upwork_crm:" + client + ":room:" + room }
func dsSlug(name string) string              { return "itest-dstore-" + name }

func dsCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const projs = `(SELECT id FROM projects WHERE slug LIKE 'itest-dstore-%')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	threads := []string{
		dsGmailThread, dsJiraThread, dsSlackThread,
		dsLegacyKey(dsClientLegacyOnly),
		dsLegacyKey(dsClientOneRoom), dsRoomedKey(dsClientOneRoom, dsRoomA),
		dsLegacyKey(dsClientTwoRooms), dsRoomedKey(dsClientTwoRooms, dsRoomA), dsRoomedKey(dsClientTwoRooms, dsRoomB),
	}
	stmts := []struct {
		sql  string
		args []any
	}{
		// capture_decisions arrives with migration 0015 and FKs tasks; guarded by
		// to_regclass so this suite still runs against a db that predates it.
		{`DO $$ BEGIN IF to_regclass('public.capture_decisions') IS NOT NULL THEN
		    DELETE FROM capture_decisions WHERE task_id IN ` + tasksOf + `; END IF; END $$;`, nil},
		// Also by PROJECT (SWT-21): attributeThread writes decisions with a
		// project_id and no task_id, and capture_decisions.project_id has no ON
		// DELETE CASCADE — so without this the `DELETE FROM projects` below fails
		// on a foreign key and takes the whole suite's cleanup with it. The
		// message_id side does cascade from normalized_messages; the project side
		// does not, and that asymmetry is exactly the kind of thing a cleanup
		// pact gets wrong once.
		{`DO $$ BEGIN IF to_regclass('public.capture_decisions') IS NOT NULL THEN
		    DELETE FROM capture_decisions WHERE project_id IN ` + projs + `; END IF; END $$;`, nil},
		{`DELETE FROM external_refs WHERE task_id IN ` + tasksOf, nil},
		{`DELETE FROM task_events WHERE task_id IN ` + tasksOf, nil},
		{`DELETE FROM task_claims WHERE task_id IN ` + tasksOf, nil},
		{`DELETE FROM deliveries WHERE task_id IN ` + tasksOf, nil},
		{`DELETE FROM tasks WHERE project_id IN ` + projs, nil},
		{`DELETE FROM projects WHERE slug LIKE 'itest-dstore-%'`, nil},
		{`DELETE FROM normalized_messages WHERE thread_id IN (SELECT id FROM normalized_threads WHERE thread_key = ANY($1))`, []any{threads}},
		{`DELETE FROM normalized_threads WHERE thread_key = ANY($1)`, []any{threads}},
		{`DELETE FROM raw_source_items WHERE source_account_id IN (SELECT id FROM source_accounts WHERE account_email=$1)`, []any{dsAccount}},
		{`DELETE FROM sync_runs WHERE source_account_id IN (SELECT id FROM source_accounts WHERE account_email=$1)`, []any{dsAccount}},
		{`DELETE FROM source_accounts WHERE account_email=$1`, []any{dsAccount}},
	}
	for _, st := range stmts {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

type dsFixture struct {
	pool    *pgxpool.Pool
	account int64
}

func newDSFixture(t *testing.T, ctx context.Context) *dsFixture {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use the compose db on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	dsCleanup(t, ctx, pool)
	t.Cleanup(func() { dsCleanup(t, ctx, pool) })

	f := &dsFixture{pool: pool}
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, scopes, send_enabled, calendar_in_availability)
		 VALUES ('google',$1,'{}',true,false)
		 ON CONFLICT (provider, account_email) DO UPDATE SET send_enabled=true RETURNING id`, dsAccount).Scan(&f.account); err != nil {
		t.Fatalf("seed source account: %v", err)
	}
	return f
}

func (f *dsFixture) ins(t *testing.T, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := f.pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", q, err)
	}
	return id
}

// thread seeds a normalized_thread plus one inbound message on it (a thread with
// no messages is the shape the SWT-19 re-key leaves behind, and it must not win
// an ordering — so every thread this suite intends to be pickable has traffic).
func (f *dsFixture) thread(t *testing.T, ctx context.Context, key string, minsAgo int) int64 {
	t.Helper()
	id := f.ins(t, ctx,
		`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,'itest-dstore','[]') RETURNING id`, key)
	raw := f.ins(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,'{}',$3, now()) RETURNING id`, f.account, "itest-dstore-"+key, "itest-dstore-h-"+key)
	f.ins(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1,$2,'inbound',$3, now() - make_interval(mins => $4),'itest-dstore body','itest-dstore',
		         'client@itest-dstore.example','gmail') RETURNING id`,
		raw, id, "itest-dstore-msg-"+key, minsAgo)
	return id
}

type projectSpec struct {
	name         string // suffix after itest-dstore-
	client       string
	policies     string // jsonb literal, "" = '{}'
	withSendFrom bool
	refSystem    string // "" = no external_refs row
	refKey       string
	refOnDeliver bool // attach the ref to the Deliver task instead of its parent
	localOnly    bool // projects.ai_locality = 'local_only' (SWT-21)
}

// project seeds a project, a done_locally work task, its Deliver child, and
// optionally the external_refs row §9 site B resolves the thread from.
func (f *dsFixture) project(t *testing.T, ctx context.Context, spec projectSpec) (deliverID, parentID, projectID int64) {
	t.Helper()
	policies := spec.policies
	if policies == "" {
		policies = "{}"
	}
	slug := dsSlug(spec.name)
	var sendFrom any
	if spec.withSendFrom {
		sendFrom = f.account
	}
	locality := "any"
	if spec.localOnly {
		locality = "local_only"
	}
	projectID = f.ins(t, ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, send_from_account, policies, ai_locality)
		 VALUES ($1,$2,$3,'manual','dashboard','/tmp/itest-dstore',$4,$5::jsonb,$6) RETURNING id`,
		"itest-dstore "+spec.name, slug, spec.client, sendFrom, policies, locality)
	parentID = f.ins(t, ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'itest-dstore work','claude','done_locally') RETURNING id`, projectID)
	// The title must literally start "Deliver #" — that prefix IS the queue
	// predicate. Built in Go, not with ||$2::text, which makes Postgres deduce
	// two types for one parameter.
	deliverID = f.ins(t, ctx,
		`INSERT INTO tasks (project_id, parent_id, title, assignee_type, status)
		 VALUES ($1,$2,$3,'claude','ready') RETURNING id`, projectID, parentID, fmt.Sprintf("Deliver #%d", parentID))

	if spec.refSystem != "" {
		owner := parentID
		if spec.refOnDeliver {
			owner = deliverID
		}
		f.ins(t, ctx,
			`INSERT INTO external_refs (task_id, system, external_key, external_url, direction)
			 VALUES ($1,$2,$3,NULL,'in') RETURNING id`, owner, spec.refSystem, spec.refKey)
	}
	return deliverID, parentID, projectID
}

// attributeThread gives every message on a thread a capture_decisions row
// pointing at a project (SWT-21).
//
// Needed because the locality fold classifies the thread NEIGHBOURS whose bodies
// travel in the prompt, and a message with no decision row is AttrUnseen —
// restricted. Without this, EVERY fixture thread folds to restricted and any
// "it skipped" assertion passes for a reason that has nothing to do with what it
// claims to test.
//
// It is also the production shape: the capture pass attributes a client's thread
// long before a Deliver task exists for it.
func (f *dsFixture) attributeThread(t *testing.T, ctx context.Context, threadID, projectID int64) {
	t.Helper()
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
		 SELECT id, 'live', 'attributed', $2, 'itest-dstore: fixture attribution'
		   FROM normalized_messages WHERE thread_id = $1`, threadID, projectID); err != nil {
		t.Fatalf("attribute thread %d: %v", threadID, err)
	}
}

// deliverTask pulls one Deliver task out of the global queue by id.
func deliverTask(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) drafts.DeliverTask {
	t.Helper()
	tasks, err := drafts.NewStore(pool).DeliverTasks(ctx, drafts.Config{})
	if err != nil {
		t.Fatalf("DeliverTasks: %v", err)
	}
	for i := range tasks {
		if tasks[i].DeliverTaskID == id {
			return tasks[i]
		}
	}
	t.Fatalf("Deliver task %d not returned by DeliverTasks; the fixture never reaches the queue "+
		"(title LIKE 'Deliver #%%', status ready/holding, no delivery on the parent)", id)
	return drafts.DeliverTask{}
}

// ---- criterion 23: gmail, resolved from an external_refs row, no people row ----

func TestDraftsStore_Integration_GmailThreadFromExternalRef(t *testing.T) {
	ctx := context.Background()
	f := newDSFixture(t, ctx)

	threadID := f.thread(t, ctx, dsGmailThread, 30)
	deliverID, _, _ := f.project(t, ctx, projectSpec{
		name: "gmail", client: "Gmail Client", withSendFrom: true,
		refSystem: "gmail", refKey: dsGmailThread,
	})

	// Criterion 23 verbatim: no people, no person_identities anywhere.
	assertNoPeopleFixture(t, ctx, f.pool)

	dt := deliverTask(t, ctx, f.pool, deliverID)
	if dt.Channel != "gmail" {
		t.Errorf("Channel = %q, want gmail", dt.Channel)
	}
	if dt.ThreadID == nil || *dt.ThreadID != threadID {
		t.Errorf("ThreadID = %v, want %d — the thread is resolved from the task's external_refs row "+
			"(§9 site B/C), not from a person's mail identity", dt.ThreadID, threadID)
	}
	if len(dt.Thread) == 0 {
		t.Errorf("Thread context is empty; the resolved thread must still load its recent messages")
	}
}

// Site B resolves on the PARENT first and then on the Deliver task itself. The
// engine links refs to the work task, but a ref linked by an agent may sit on
// either.
func TestDraftsStore_Integration_RefOnTheDeliverTaskAlsoResolves(t *testing.T) {
	ctx := context.Background()
	f := newDSFixture(t, ctx)

	threadID := f.thread(t, ctx, dsGmailThread, 30)
	deliverID, _, _ := f.project(t, ctx, projectSpec{
		name: "gmail-on-deliver", client: "Gmail Client", withSendFrom: true,
		refSystem: "gmail", refKey: dsGmailThread, refOnDeliver: true,
	})

	dt := deliverTask(t, ctx, f.pool, deliverID)
	if dt.ThreadID == nil || *dt.ThreadID != threadID {
		t.Errorf("ThreadID = %v, want %d: §9 site B tries the parent, THEN the Deliver task itself",
			dt.ThreadID, threadID)
	}
}

// ---- criterion 24: upwork, target_ref is the thread_key exactly ----------------

func TestDraftsStore_Integration_UpworkTargetIsTheThreadKey(t *testing.T) {
	ctx := context.Background()
	f := newDSFixture(t, ctx)

	key := dsLegacyKey(dsClientLegacyOnly)
	f.thread(t, ctx, key, 30)
	deliverID, _, _ := f.project(t, ctx, projectSpec{
		name: "upwork", client: "Upwork Client",
		refSystem: "upwork_crm", refKey: key,
	})
	assertNoPeopleFixture(t, ctx, f.pool)

	dt := deliverTask(t, ctx, f.pool, deliverID)
	if dt.Channel != "upwork_chat" {
		t.Errorf("Channel = %q, want upwork_chat — derived from the resolved thread_key's PREFIX (§9 site D), "+
			"not from normalized_messages.channel, which is CRM-supplied free text", dt.Channel)
	}
	if dt.TargetRef != key {
		t.Errorf("TargetRef = %q, want exactly %q. An exact-string match is what confirms the delivery later; "+
			"a non-canonical spelling is permanently unconfirmable with no error anywhere (SWT-13)", dt.TargetRef, key)
	}
	if dt.ThreadID != nil {
		t.Logf("note: ThreadID is also set (%d) for an upwork target; the delivery keys on target_ref", *dt.ThreadID)
	}
}

// ---- SWT-19 must survive the rework: roomed preference ------------------------

// SWT-19 rewrote upwork target resolution inside this very function: prefer a
// ROOMED thread over the client's legacy one, deliberately rather than by
// `ORDER BY id DESC`. The reason is downstream: the matcher's room scoping only
// tightens anything if deliveries are TARGETED at roomed threads where one exists.
// A rework that resolves "the thread the ref names" and stops would target the
// legacy key again, keep every new delivery client-wide, and make SWT-19 a no-op
// that still passes its own unit tests — the SWT-18 mistake, one layer up.
//
// The ref here names the LEGACY key on purpose: that is what a capture rule
// writes for a client whose ingested history is pre-2026-07-21, and what
// `clientIDFromThreadKey` would give back. The roomed thread is newer and carries
// the recent traffic.
func TestDraftsStore_Integration_UpworkPrefersTheRoomedThread(t *testing.T) {
	ctx := context.Background()
	f := newDSFixture(t, ctx)

	legacy := dsLegacyKey(dsClientOneRoom)
	roomed := dsRoomedKey(dsClientOneRoom, dsRoomA)
	if legacy == roomed {
		t.Fatalf("fixture invalid: the two keys are the same string (%q)", legacy)
	}
	legacyThread := f.thread(t, ctx, legacy, 600) // old traffic
	roomedThread := f.thread(t, ctx, roomed, 20)  // current traffic
	if legacyThread == roomedThread {
		t.Fatalf("fixture invalid: both keys resolved to one thread row")
	}
	if !(legacyThread < roomedThread) {
		t.Fatalf("fixture invalid: the legacy thread (%d) must be created BEFORE the roomed one (%d), so "+
			"'prefer roomed' cannot coincide with 'newest row wins'", legacyThread, roomedThread)
	}

	deliverID, _, _ := f.project(t, ctx, projectSpec{
		name: "upwork-roomed", client: "Roomed Client",
		refSystem: "upwork_crm", refKey: legacy,
	})

	dt := deliverTask(t, ctx, f.pool, deliverID)
	if dt.Channel != "upwork_chat" {
		t.Fatalf("Channel = %q, want upwork_chat", dt.Channel)
	}
	if dt.TargetRef != roomed {
		t.Errorf("TargetRef = %q, want the ROOMED key %q. SWT-19's roomed-thread preference must SURVIVE the "+
			"§9 rework: a reply aimed at the legacy key is client-wide, and since SWT-19 the room scoping is "+
			"the thing that stops a wrong-room bind", dt.TargetRef, roomed)
	}
}

// ---- SWT-19 must survive the rework: the multi-room REFUSAL --------------------

// The refusal is SWT-20's shipped mitigation. Two production clients have several
// rooms (3 and 2). Picking the most recent is a GUESS, and since SWT-19 a guess is
// expensive in both directions: the reply may land in the wrong conversation, and
// the delivery can then NEVER confirm, because a room mismatch excludes — the miss
// surfaces only via the reconciler, ~45 minutes later. An empty Channel routes to
// the worker's existing "unresolvable — tell the human on the Deliver task" path,
// which is reversible and audited; a wrong-room send is neither.
//
// The ref names the legacy key, so nothing in the task says which room the work
// came from. (If the rework instead resolves a ref that names ONE room, that ref
// IS the task-level provenance SWT-19 asked for and targeting it is correct — this
// test does not forbid that; it forbids guessing when no room is named.)
func TestDraftsStore_Integration_UpworkRefusesWhenTheClientHasSeveralRooms(t *testing.T) {
	ctx := context.Background()
	f := newDSFixture(t, ctx)

	legacy := dsLegacyKey(dsClientTwoRooms)
	roomA := dsRoomedKey(dsClientTwoRooms, dsRoomA)
	roomB := dsRoomedKey(dsClientTwoRooms, dsRoomB)
	if roomA == roomB {
		t.Fatalf("fixture invalid: the two rooms are the same key (%q)", roomA)
	}
	f.thread(t, ctx, legacy, 900)
	f.thread(t, ctx, roomA, 40)
	f.thread(t, ctx, roomB, 10) // the most recent — the one a guess would pick

	deliverID, _, _ := f.project(t, ctx, projectSpec{
		name: "upwork-ambiguous", client: "Multi Room Client",
		refSystem: "upwork_crm", refKey: legacy,
	})

	dt := deliverTask(t, ctx, f.pool, deliverID)
	if dt.Channel != "" || dt.TargetRef != "" {
		t.Errorf("resolved (Channel %q, TargetRef %q) for a client with TWO rooms and no room named on the task; "+
			"want a refusal (Channel=\"\", TargetRef=\"\"). Removing the refusal silently reopens a wrong-room "+
			"send, which can never confirm and surfaces only via the reconciler ~45 minutes later",
			dt.Channel, dt.TargetRef)
	}
}

// ---- criterion 25: precedence and the negative cases ---------------------------

func TestDraftsStore_Integration_ChannelPrecedenceAndUnresolvables(t *testing.T) {
	ctx := context.Background()
	f := newDSFixture(t, ctx)

	upworkKey := dsLegacyKey(dsClientLegacyOnly)
	f.thread(t, ctx, upworkKey, 30)
	f.thread(t, ctx, dsJiraThread, 30)
	f.thread(t, ctx, dsSlackThread, 30)

	t.Run("policies.delivery_channel overrides the thread-derived channel", func(t *testing.T) {
		deliverID, _, _ := f.project(t, ctx, projectSpec{
			name: "override", client: "Override Client",
			policies:  `{"delivery_channel":"gmail"}`,
			refSystem: "upwork_crm", refKey: upworkKey,
		})
		dt := deliverTask(t, ctx, f.pool, deliverID)
		if dt.Channel != "gmail" {
			t.Errorf("Channel = %q, want gmail: explicit project config still wins over the thread prefix", dt.Channel)
		}
	})

	t.Run("no resolvable thread but a mail identity yields gmail with no thread", func(t *testing.T) {
		deliverID, _, _ := f.project(t, ctx, projectSpec{
			name: "nothread-mail", client: "Mail Client", withSendFrom: true,
		})
		dt := deliverTask(t, ctx, f.pool, deliverID)
		if dt.Channel != "gmail" || dt.ThreadID != nil {
			t.Errorf("(Channel %q, ThreadID %v), want (gmail, nil): the project has a mail identity but no "+
				"thread — the existing unresolvable state the worker already skips", dt.Channel, dt.ThreadID)
		}
	})

	t.Run("no resolvable thread and no mail identity yields nothing", func(t *testing.T) {
		deliverID, _, _ := f.project(t, ctx, projectSpec{name: "nothread-bare", client: "Bare Client"})
		dt := deliverTask(t, ctx, f.pool, deliverID)
		if dt.Channel != "" {
			t.Errorf("Channel = %q, want \"\" (unresolvable). Site E deliberately removed the synthesized "+
				"upwork target: drafting into a conversation that does not exist is outside the policy matrix's "+
				"'existing threads only, ≤2 touches'", dt.Channel)
		}
		if dt.TargetRef != "" {
			t.Errorf("TargetRef = %q, want empty: nothing may synthesize a target from a client id", dt.TargetRef)
		}
	})

	for _, tc := range []struct{ name, refSystem, refKey string }{
		{"jira", "jira", "WEB-4242"},
		{"slack", "slack", dsSlackThread},
	} {
		t.Run("a "+tc.name+" thread yields no channel", func(t *testing.T) {
			deliverID, _, _ := f.project(t, ctx, projectSpec{
				name: "thread-" + tc.name, client: tc.name + " Client",
				refSystem: tc.refSystem, refKey: tc.refKey,
			})
			dt := deliverTask(t, ctx, f.pool, deliverID)
			if dt.Channel != "" {
				t.Errorf("Channel = %q, want \"\": expanding the draft worker into jira_comment/slack_reply is "+
					"step-9 work and deliberately NOT bundled here", dt.Channel)
			}
		})
	}
}

// ---- criterion 26: ClientName without the people join --------------------------

func TestDraftsStore_Integration_ClientNameFallsBackToProjectName(t *testing.T) {
	ctx := context.Background()
	f := newDSFixture(t, ctx)

	t.Run("empty client falls back to the project name", func(t *testing.T) {
		// projects.client is '' (not NULL) on the real rows, which is why the
		// replacement is COALESCE(NULLIF(p.client,''), p.name) — a plain COALESCE
		// would keep returning the empty string.
		deliverID, _, _ := f.project(t, ctx, projectSpec{name: "noclient", client: ""})
		dt := deliverTask(t, ctx, f.pool, deliverID)
		if dt.ClientName != "itest-dstore noclient" {
			t.Errorf("ClientName = %q, want the project name %q — with client_person_id gone there is no "+
				"people.display_name to fall back to, and an empty ClientName renders as \"—\" in the prompt",
				dt.ClientName, "itest-dstore noclient")
		}
	})

	t.Run("a non-empty client still wins", func(t *testing.T) {
		deliverID, _, _ := f.project(t, ctx, projectSpec{name: "withclient", client: "Acme Corp"})
		dt := deliverTask(t, ctx, f.pool, deliverID)
		if dt.ClientName != "Acme Corp" {
			t.Errorf("ClientName = %q, want %q", dt.ClientName, "Acme Corp")
		}
	})
}

// assertNoPeopleFixture pins criterion 23's "with no people or person_identities
// row anywhere in the fixture". If a future edit adds one, the case stops proving
// that resolution is person-free.
func assertNoPeopleFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM person_identities pi
		  WHERE pi.value = ANY($1)`,
		[]string{dsClientLegacyOnly, dsClientOneRoom, dsClientTwoRooms}).Scan(&n); err != nil {
		t.Fatalf("count fixture identities: %v", err)
	}
	if n != 0 {
		t.Fatalf("fixture invalid: %d person_identities rows exist for this suite's clients. Criterion 23/24 "+
			"require the resolution to work with NO person anywhere — otherwise the old lookup could still be "+
			"what answers", n)
	}
}
