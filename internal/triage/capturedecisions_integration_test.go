//go:build integration

package triage_test

// Integration tests for SWT-17 §8 — triage's two reads of capture_decisions
// (acceptance criteria 17 and 20). Build-tagged `integration` AND env-gated on
// DATABASE_URL. No LLM: nothing here calls triage.Run, only the store's pending
// filter and context assembly, which are the two functions §8 changes.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run TriageCaptureDecisions ./internal/triage/
//
// GREENFIELD NOTE: fails today because migration 0015 (capture_decisions) does not
// exist, and — once it does — because internal/triage/store.go still resolves the
// project from the dropped projects.client_person_id and its pending filter knows
// nothing about decisions. Expected red state.
//
// Cross-suite discipline: this file is in the same package as integration_test.go,
// whose TestTriage_Integration_ShadowMode asserts GLOBAL counts (provider calls =
// 2 over every pending inbound message in the db). This suite seeds a message that
// triage is SUPPOSED to consume, so leaving one row behind would break that test
// in a way that reads like a triage bug. It therefore cleans its own corpus in FK
// order at start AND end, scoped to provider 'itest-capinbox-src', project slug
// 'itest-capinbox-proj' and thread_key prefix 'itest-capinbox:'.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/triage"
)

const (
	cinboxProvider = "itest-capinbox-src"
	cinboxProject  = "itest-capinbox-proj"
	cinboxThread   = "itest-capinbox:thread"
)

type cinboxCorpus struct {
	pool      *pgxpool.Pool
	projectID int64
	personID  int64
	threadID  int64
	// the three states of §8(b)
	unseenID     int64 // no capture_decisions row at all
	unmatchedID  int64 // latest decision action='unmatched', project_id NULL
	attributedID int64 // latest decision names a project (shadow mode!)
	routedID     int64 // latest decision action='task' (live mode)
	routedTaskID int64
}

func cleanupCaptureInbox(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN
	                (SELECT id FROM source_accounts WHERE provider='` + cinboxProvider + `'))`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + cinboxProject + `'))`
	stmts := []string{
		`DELETE FROM capture_decisions WHERE message_id IN
		   (SELECT id FROM normalized_messages WHERE raw_source_item_id IN ` + raws + `)`,
		`DELETE FROM capture_decisions WHERE task_id IN ` + tasksOf,
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM external_refs WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + cinboxProject + `')`,
		`DELETE FROM capture_rules WHERE project_id IN (SELECT id FROM projects WHERE slug='` + cinboxProject + `')`,
		`DELETE FROM projects WHERE slug='` + cinboxProject + `'`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'itest-capinbox:%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN
		   (SELECT id FROM source_accounts WHERE provider='` + cinboxProvider + `')`,
		`DELETE FROM person_identities WHERE person_id IN (SELECT id FROM people WHERE display_name LIKE 'itest-capinbox-%')`,
		`DELETE FROM people WHERE display_name LIKE 'itest-capinbox-%'`,
		`DELETE FROM sync_runs WHERE source_account_id IN
		   (SELECT id FROM source_accounts WHERE provider='` + cinboxProvider + `')`,
		`DELETE FROM source_accounts WHERE provider='` + cinboxProvider + `'`,
	}
	for _, q := range stmts {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func seedCaptureInbox(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *cinboxCorpus {
	t.Helper()
	c := &cinboxCorpus{pool: pool}

	ins := func(q string, args ...any) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("insert %q: %v", q, err)
		}
		return id
	}

	accountID := ins(`INSERT INTO source_accounts (provider, account_email, send_enabled)
	                  VALUES ($1, 'itest-capinbox@pg-main', false) RETURNING id`, cinboxProvider)
	c.personID = ins(`INSERT INTO people (display_name) VALUES ('itest-capinbox-Client') RETURNING id`)
	c.projectID = ins(`INSERT INTO projects (name, slug, client, execution, delivery, ai_locality)
	                   VALUES ($1,$1,'Capinbox','manual','dashboard', 'any') RETURNING id`, cinboxProject)
	// participants is what mc.PersonID still comes from after the column drop —
	// §8(a) keeps PersonID/PersonName, it only replaces the PROJECT lookup.
	c.threadID = ins(`INSERT INTO normalized_threads (thread_key, subject, participants)
	                  VALUES ($1,'capinbox',$2) RETURNING id`, cinboxThread,
		[]byte(fmt.Sprintf("[%d]", c.personID)))

	msg := func(label, body string, minsAgo int) int64 {
		raw := ins(`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		            VALUES ($1,$2,'{}',$3, now()) RETURNING id`,
			accountID, "itest-capinbox-"+label, "itest-capinbox-h-"+label)
		return ins(`INSERT INTO normalized_messages
		              (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		               body_text, subject, sender, channel)
		            VALUES ($1,$2,'inbound',$3, now() - make_interval(mins => $4), $5,'capinbox',
		                    'client@capinbox.example','slack') RETURNING id`,
			raw, c.threadID, "itest-capinbox-"+label, minsAgo, body)
	}

	c.unseenID = msg("unseen", "the capture pass has not looked at this one yet", 40)
	c.unmatchedID = msg("unmatched", "no rule covers this chatter", 30)
	c.attributedID = msg("attributed", "routed by a rule, attribution only", 20)
	c.routedID = msg("routed", "routed by a rule that made a task", 10)

	// A task for the routed message's decision to point at.
	c.routedTaskID = ins(`INSERT INTO tasks (project_id, title, assignee_type, status)
	                      VALUES ($1,'itest-capinbox routed','human','ready') RETURNING id`, c.projectID)

	// The decisions. Note the MODES differ deliberately: the attributed row is
	// SHADOW and the task row is LIVE. §8(b) keys on the ACTION, not the mode —
	// that is precisely the window the go-live ordering constraint (criterion 21)
	// exists for, so a filter that only excluded live rows would re-triage every
	// shadow-matched message and quietly invert the design.
	ins(`INSERT INTO capture_decisions (message_id, mode, action, project_id, matched_rule_ids)
	     VALUES ($1,'shadow','unmatched',NULL,'{}') RETURNING id`, c.unmatchedID)
	ins(`INSERT INTO capture_decisions (message_id, mode, action, project_id, matched_rule_ids)
	     VALUES ($1,'shadow','attributed',$2,'{}') RETURNING id`, c.attributedID, c.projectID)
	ins(`INSERT INTO capture_decisions (message_id, mode, action, project_id, task_id, external_system, external_key, matched_rule_ids)
	     VALUES ($1,'live','task',$2,$3,'jira','CAPINBOX-1','{}') RETURNING id`,
		c.routedID, c.projectID, c.routedTaskID)
	return c
}

func newCaptureInboxSuite(t *testing.T, ctx context.Context) *cinboxCorpus {
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
	cleanupCaptureInbox(t, ctx, pool)
	t.Cleanup(func() { cleanupCaptureInbox(t, ctx, pool) })
	return seedCaptureInbox(t, ctx, pool)
}

// ---- criterion 20 / §8(b): the three states of the inbox -----------------------

func TestTriageCaptureDecisions_Integration_ThreeStateInboxFilter(t *testing.T) {
	ctx := context.Background()
	c := newCaptureInboxSuite(t, ctx)

	pending, err := triage.NewStore(c.pool).PendingMessages(ctx, triage.Config{})
	if err != nil {
		t.Fatalf("PendingMessages: %v", err)
	}
	got := map[int64]bool{}
	for _, m := range pending {
		got[m.MessageID] = true
	}

	// State 2 — evaluated, no rule covered it. THIS is triage's inbox (Q3).
	if !got[c.unmatchedID] {
		t.Errorf("message %d (latest decision action='unmatched') is not pending; that decision row IS the "+
			"handoff — triage live consumes exactly these (§8b, criterion 20)", c.unmatchedID)
	}

	// State 1 — not evaluated yet. Unseen is NOT unmatched.
	if got[c.unseenID] {
		t.Errorf("message %d has NO capture_decisions row and was returned as pending. A message the engine has "+
			"not evaluated is UNSEEN, not unroutable: triage runs on its own schedule and the capture pass "+
			"hitchhikes on connector CronJobs, so treating unseen as unmatched hands every fresh message to the "+
			"model before the deterministic rules ever look at it. The filter is an EXISTS on an 'unmatched' "+
			"latest decision, never a NOT EXISTS on decisions generally", c.unseenID)
	}

	// State 3 — deterministically routed. Never re-triaged, in either mode.
	for _, id := range []int64{c.attributedID} {
		if got[id] {
			t.Errorf("message %d has a decision naming a project (action='attributed', mode SHADOW) and was "+
				"returned as pending. §8(b) keys on the ACTION: a shadow-matched message must not be re-triaged "+
				"either, which is why capture must go live BEFORE triage (criterion 21)", id)
		}
	}
	if got[c.routedID] {
		t.Errorf("message %d already produced a task through the deterministic engine and was returned as "+
			"pending; re-triaging it is how one ticket becomes two tasks", c.routedID)
	}
}

// The latest decision decides, not the first. A message can be re-evaluated in
// shadow (`--all`), so a stale row must not pin a message out of — or into — the
// inbox forever.
func TestTriageCaptureDecisions_Integration_LatestDecisionWins(t *testing.T) {
	ctx := context.Background()
	c := newCaptureInboxSuite(t, ctx)

	// A rule is added that covers what used to be unmatched: a NEWER decision row
	// attributes it. The message must leave the inbox.
	if _, err := c.pool.Exec(ctx,
		`INSERT INTO capture_decisions (message_id, mode, action, project_id, matched_rule_ids)
		 VALUES ($1,'shadow','attributed',$2,'{}')`, c.unmatchedID, c.projectID); err != nil {
		t.Fatalf("insert the later decision: %v", err)
	}
	pending, err := triage.NewStore(c.pool).PendingMessages(ctx, triage.Config{})
	if err != nil {
		t.Fatalf("PendingMessages: %v", err)
	}
	for _, m := range pending {
		if m.MessageID == c.unmatchedID {
			t.Fatalf("message %d is still pending after a NEWER decision attributed it to a project; the filter "+
				"must read the LATEST decision (ORDER BY id DESC), not any decision", c.unmatchedID)
		}
	}

	// And the mirror: a rule that is removed/disabled re-opens the inbox.
	if _, err := c.pool.Exec(ctx,
		`INSERT INTO capture_decisions (message_id, mode, action, project_id, matched_rule_ids)
		 VALUES ($1,'shadow','unmatched',NULL,'{}')`, c.attributedID); err != nil {
		t.Fatalf("insert the later unmatched decision: %v", err)
	}
	pending, err = triage.NewStore(c.pool).PendingMessages(ctx, triage.Config{})
	if err != nil {
		t.Fatalf("PendingMessages (after re-evaluation): %v", err)
	}
	found := false
	for _, m := range pending {
		if m.MessageID == c.attributedID {
			found = true
		}
	}
	if !found {
		t.Errorf("message %d was re-evaluated as unmatched and did not return to the inbox", c.attributedID)
	}
}

// ---- criterion 17 / §8(a): the project comes from the decision, and nowhere else

func TestTriageCaptureDecisions_Integration_ProjectComesFromTheDecision(t *testing.T) {
	ctx := context.Background()
	c := newCaptureInboxSuite(t, ctx)
	st := triage.NewStore(c.pool)

	// The routed message is not in the pending set (that is criterion 20's point),
	// so its context is assembled directly — the lookup is per message id.

	for _, tc := range []struct {
		name        string
		messageID   int64
		wantProject bool
	}{
		{"attributed message resolves its project from the decision", c.attributedID, true},
		{"task-routed message resolves its project from the decision", c.routedID, true},
		{"unmatched message resolves NO project", c.unmatchedID, false},
		{"unseen message resolves NO project", c.unseenID, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pm triage.PendingMessage
			if err := c.pool.QueryRow(ctx,
				`SELECT id, raw_source_item_id, COALESCE(thread_id,0), sent_at, COALESCE(sender,''),
				        COALESCE(subject,''), COALESCE(channel,''), COALESCE(body_text,''), direction
				   FROM normalized_messages WHERE id=$1`, tc.messageID).
				Scan(&pm.MessageID, &pm.RawSourceItemID, &pm.ThreadID, &pm.SentAt, &pm.Sender,
					&pm.Subject, &pm.Channel, &pm.BodyText, &pm.Direction); err != nil {
				t.Fatalf("load message %d: %v", tc.messageID, err)
			}

			mc, err := st.AssembleContext(ctx, pm)
			if err != nil {
				t.Fatalf("AssembleContext: %v", err)
			}
			if tc.wantProject {
				if mc.ProjectID == nil || *mc.ProjectID != c.projectID {
					t.Errorf("ProjectID = %v, want %d resolved from the latest capture_decisions row — that is "+
						"the ONLY project source now that projects.client_person_id is dropped (§8a)",
						mc.ProjectID, c.projectID)
				}
				if mc.ProjectSlug != cinboxProject {
					t.Errorf("ProjectSlug = %q, want %q", mc.ProjectSlug, cinboxProject)
				}
			} else if mc.ProjectID != nil {
				t.Errorf("ProjectID = %d for a message with no project-bearing decision; there is no fallback "+
					"lookup left to invent one", *mc.ProjectID)
			}

			// PersonID/PersonName survive the drop: they come from participants,
			// not from the dropped column, and the prompt still uses them.
			if mc.PersonID == nil || *mc.PersonID != c.personID {
				t.Errorf("PersonID = %v, want %d from normalized_threads.participants — §8(a) replaces the "+
					"PROJECT lookup only", mc.PersonID, c.personID)
			}
		})
	}
}

// The SUPERSEDED-ATTRIBUTION case: a message attributed by an earlier pass and
// re-evaluated as unmatched must resolve NO project (SWT-17, from the
// go-reviewer pass).
//
// This is the one fixture that tells the two candidate readings of "latest"
// apart, and without it the deviation is one refactor from being reverted.
//
//	the SPEC's §8a:  WHERE cd.project_id IS NOT NULL ORDER BY cd.id DESC
//	what shipped:    ORDER BY cd.id DESC LIMIT 1, then take its project
//
// Every other case in this suite gives a message exactly one decision row, so
// the SPEC's spelling passes all of them. Only here do they diverge: the SPEC's
// query skips PAST the newer unmatched row to find the older project-bearing
// one, and hands triage a project from the decision that superseded it.
//
// That is not academic. Disable a rule or narrow a pattern — the ordinary way a
// rule set is tuned — and every message it used to route becomes exactly this
// shape. The message is served to triage as inbox (the §8b filter takes the
// latest row unconditionally) AND carries a stale project, so triage extracts
// against a project the rules no longer say it belongs to. No error anywhere.
func TestTriageCaptureDecisions_Integration_SupersededAttributionResolvesNoProject(t *testing.T) {
	ctx := context.Background()
	c := newCaptureInboxSuite(t, ctx)
	st := triage.NewStore(c.pool)

	// The attributed message gains a LATER unmatched decision — the rule that
	// routed it has been disabled.
	var supersedingID int64
	if err := c.pool.QueryRow(ctx,
		`INSERT INTO capture_decisions (message_id, mode, action, project_id, matched_rule_ids)
		 VALUES ($1,'shadow','unmatched',NULL,'{}') RETURNING id`, c.attributedID).Scan(&supersedingID); err != nil {
		t.Fatalf("seed superseding decision: %v", err)
	}

	// Fixture guard: the new row must actually be the later one, or this test
	// proves nothing about ordering.
	var latestIsSuperseding bool
	if err := c.pool.QueryRow(ctx,
		`SELECT (SELECT id FROM capture_decisions WHERE message_id=$1 ORDER BY id DESC LIMIT 1) = $2`,
		c.attributedID, supersedingID).Scan(&latestIsSuperseding); err != nil {
		t.Fatalf("check ordering: %v", err)
	}
	if !latestIsSuperseding {
		t.Fatalf("fixture invalid: the superseding decision %d is not the latest for message %d",
			supersedingID, c.attributedID)
	}

	var pm triage.PendingMessage
	if err := c.pool.QueryRow(ctx,
		`SELECT id, raw_source_item_id, COALESCE(thread_id,0), sent_at, COALESCE(sender,''),
		        COALESCE(subject,''), COALESCE(channel,''), COALESCE(body_text,''), direction
		   FROM normalized_messages WHERE id=$1`, c.attributedID).
		Scan(&pm.MessageID, &pm.RawSourceItemID, &pm.ThreadID, &pm.SentAt, &pm.Sender,
			&pm.Subject, &pm.Channel, &pm.BodyText, &pm.Direction); err != nil {
		t.Fatalf("load message: %v", err)
	}

	mc, err := st.AssembleContext(ctx, pm)
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}
	if mc.ProjectID != nil {
		t.Errorf("ProjectID = %d for a message whose LATEST decision is unmatched. The project came from a "+
			"decision that has been superseded — the rule was disabled or narrowed, and this message is no "+
			"longer routed. §8b's filter serves it to triage as inbox, so triage would extract against a "+
			"project the rules no longer assign it, silently. Both reads must mean the newest row",
			*mc.ProjectID)
	}
}
