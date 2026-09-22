//go:build integration

package capture_test

// comms-inbox (SWT-74, docs/tickets/comms-inbox_SPEC.md) against a real
// database — the capture half, end to end:
//
//	criterion 42 — an ARMED rule's task_log attach creates its OWN task, marked
//	               with its message, and the TICKET task keeps its log line, gains
//	               the ids-only pointer and does NOT move to INCOMING;
//	criterion 43 — COLUMN-FED: `UPDATE capture_rules SET comm_task = false` and
//	               the very next message behaves exactly as SWT-72 ships;
//	criterion 44 — the three noise senders, one pass: Anonymous (JIRA), a
//	               projects.notifier_senders sender and a blank sender;
//	criterion 45 — channel-blind: the same armed rule over Slack and a
//	               jira-channel message;
//	criterion 10 — a SHADOW pass creates nothing and says "would";
//	criterion 11 — a REPLAY creates nothing (the live claim is spent);
//	criterion 16 — a CLOSED target is untouched (SWT-45/SWT-53 keep it);
//	criterion 41 — a Done ticket still closes its ticket task after a pass that
//	               created a comm task from its mail (surfaced_at untouched).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_comms?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run CaptureComm ./internal/capture/
//
// Reuses rules_revive_integration_test.go's crvSuite wholesale (its wholesale
// capture_decisions cleanup, its three accounts, its rule and message helpers)
// and rules_activity_integration_test.go's craSeedTask / craActivity.
//
// "TEST THE COLUMN, NOT THE FIXTURE" (IK): the rule is armed by WRITING
// capture_rules.comm_task, never by a Go value, and criterion 43 MUTATES that
// column between two passes with nothing else changing. Replacing `r.comm_task`
// in loadRules with a literal `false` turns criterion 42 red; the fixture
// supplies neither value.
//
// GREENFIELD NOTE — EXPECTED RED: migration 0040 is not applied, so
// ccRequire0040 fails every test here with one sentence; after that,
// RulesStats has no CommTasks field, so the package's integration binary does
// not compile.
//
// SPEC AMENDMENT (2026-09-22 12:20, swb #491, on main — the SPEC predates it):
// capture's actionTask branch ALSO calls task_mark_activity on a task it
// CREATES (after provenance, before task_mark_surfaced; excluded for a
// pr_review rule's create). Consequences encoded below:
//   - D8's RULE stands and is asserted (TheFirstMessageAboutANewKeyGetsNoComm):
//     a first message about a NEW key creates the TICKET task and NO comm task;
//   - D8's SENTENCE "a capture-created ticket task still lands in QUEUE" is
//     STALE: the created task is marked with its own message and lands in
//     INCOMING. That test asserts the CURRENT behaviour (activity_at SET on the
//     created ticket task), not the SPEC's stale line;
//   - D3 stays consistent with it: on an ARMED rule's task_log the target gets
//     the log line and the ids-only pointer and NO activity mark, while the COMM
//     task gets the mark. Seeded ticket tasks therefore have their seed-pass
//     activity CLEARED by craSeedTask before anything is asserted.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - loadRules selects a literal false -> ArmedRuleCreatesItsOwnIncomingRow, ColumnFed.
//   - keep markRuleActivity on the TARGET -> ArmedRuleCreatesItsOwnIncomingRow (the ticket's activity_at).
//   - drop markRuleActivity on the COMM -> the same (the comm's activity_at).
//   - drop setRuleProvenance on the COMM -> the same (source_thread_id NULL).
//   - call link_external_ref for the comm -> the second pass files onto the comm instead of the ticket.
//   - drop clause 2 / ownJiraEdit / notifier / blankSender -> ClosedTargetsAreUntouched, TheNoiseSenders.
//   - key the comm path on the channel or the rule kind -> SlackAndJiraToo.
//   - create a comm on the actionTask branch -> TheFirstMessageAboutANewKeyGetsNoComm.
//
// CROSS-SUITE DISCIPLINE: EvaluateRules' pending set is GLOBAL, so crvCleanup
// deletes capture_decisions WHOLESALE at start and end. Run against an ISOLATED
// database (IK 2026-09-12: the compose `ops` db is shared by every worktree).

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/capture"
)

const (
	ccJose      = "José Garcia <jose.g@avviato.example>"
	ccAnonymous = "Anonymous (JIRA)"
	ccKatie     = "Katie Evans (JIRA) <jira@caprev.jira.com>"
	ccNotifier  = "Jira <noreply@caprev.jira.com>"
)

func ccRequire0040(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE (table_name='capture_rules' AND column_name='comm_task')
		     OR (table_name='capture_decisions' AND column_name='comm_task_id')`).Scan(&n); err != nil {
		t.Fatalf("probe 0040's columns: %v", err)
	}
	if n != 2 {
		t.Fatalf("found %d of the 2 columns migration 0040 adds. Criterion 1: apply "+
			"migrations/%s (`make migrate LOCAL_DB_URL=...`). Merging a migration is not applying it, and "+
			"every capture pass built from this branch selects capture_rules.comm_task (Verification Step 5's "+
			"barrier)", n, "0040_capture_comm_tasks.sql")
	}
}

// ccCommRule seeds the José rule's shape THROUGH THE COLUMN criterion 43 is
// about: a body_regex rule on a CIC-NNNN key, external_system jira, armed (or
// not) for comms. Rule 75 in production is exactly this shape.
func ccCommRule(t *testing.T, ctx context.Context, s *crvSuite, armed bool) int64 {
	t.Helper()
	return s.id(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, url_template,
		                            priority, enabled, note, comm_task)
		 VALUES ($1,'body_regex',$2,'jira',$3,'https://caprev.jira.com/browse/{key}',90,true,'itest-caprev comm',$4)
		 RETURNING id`,
		s.project, `\bCIC-[0-9]+\b`, `\b(CIC-[0-9]+)\b`, armed)
}

// ccDecision is the message's latest decision row, with the two columns this
// ticket adds to it.
type ccDecision struct {
	action     string
	taskID     *int64
	commTaskID *int64
	reason     string
	resurface  bool
}

func ccDecisionOf(t *testing.T, ctx context.Context, s *crvSuite, msg int64, mode string) ccDecision {
	t.Helper()
	var d ccDecision
	var reason *string
	err := s.pool.QueryRow(ctx,
		`SELECT action, task_id, comm_task_id, reason, resurface FROM capture_decisions
		  WHERE message_id=$1 AND mode=$2 ORDER BY id DESC LIMIT 1`, msg, mode).
		Scan(&d.action, &d.taskID, &d.commTaskID, &reason, &d.resurface)
	if err != nil {
		t.Fatalf("read the %s decision for message %d: %v", mode, msg, err)
	}
	if reason != nil {
		d.reason = *reason
	}
	return d
}

type ccTask struct {
	project      string
	status       string
	assignee     string
	title        string
	body         string
	priority     int
	sourceThread *int64
	activityAt   *time.Time
	activityBy   *int64
	surfacedAt   *time.Time
}

func ccTaskRow(t *testing.T, ctx context.Context, s *crvSuite, id int64) ccTask {
	t.Helper()
	var r ccTask
	if err := s.pool.QueryRow(ctx,
		`SELECT p.slug, t.status, t.assignee_type, t.title, COALESCE(t.body,''), t.priority,
		        t.source_thread_id, t.activity_at, t.activity_by_message_id, t.surfaced_at
		   FROM tasks t JOIN projects p ON p.id = t.project_id WHERE t.id=$1`, id).
		Scan(&r.project, &r.status, &r.assignee, &r.title, &r.body, &r.priority,
			&r.sourceThread, &r.activityAt, &r.activityBy, &r.surfacedAt); err != nil {
		t.Fatalf("read task %d: %v", id, err)
	}
	return r
}

// ccLogs returns this task's `log` event messages, oldest first.
func ccLogs(t *testing.T, ctx context.Context, s *crvSuite, task int64) []string {
	t.Helper()
	rows, err := s.pool.Query(ctx,
		`SELECT COALESCE(payload->>'message','') FROM task_events
		  WHERE task_id=$1 AND event_type='log' ORDER BY id`, task)
	if err != nil {
		t.Fatalf("read log events of task %d: %v", task, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			t.Fatalf("scan log event: %v", err)
		}
		out = append(out, m)
	}
	return out
}

// ccCommMail is José's shape: a DIRECT mail quoting the key, no Jira in the path.
func ccCommMail(t *testing.T, ctx context.Context, s *crvSuite, sender, subject, body string) int64 {
	t.Helper()
	now := s.dbNow(t, ctx)
	s.seq++
	return s.msg(t, ctx, s.mail, crvMailPfx+"<comm-"+time.Now().Format("150405.000000000")+">", "gmail",
		"inbound", sender, subject, body, now, now)
}

// ---- criterion 42 -------------------------------------------------------------------

func TestCaptureComm_Integration_ArmedRuleCreatesItsOwnIncomingRow(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	ccRequire0040(t, ctx, s.pool)

	// The TICKET task, created the way production made it (create_task +
	// link_external_ref + task_set_source_thread through the executor), with the
	// create pass's own activity mark cleared: this test is about what the COMM
	// pass does, not what the seed did (swb #491 — see the header).
	ticket := craSeedTask(t, ctx, s, "CIC-7001")
	if at, _ := craActivity(t, ctx, s, ticket); at != nil {
		t.Fatalf("CONTROL: the seeded ticket task already carries activity_at=%v", at)
	}
	logsBefore := len(ccLogs(t, ctx, s, ticket))

	rule := ccCommRule(t, ctx, s, true)
	mail := ccCommMail(t, ctx, s, ccJose, "CIC-7001 blocks the import",
		"Hi Salvador,\nthe importer still rejects the file on CIC-7001. Can you look?")

	st := s.pass(t, ctx, capture.RulesModeLive)
	if st.CommTasks != 1 {
		t.Errorf("the pass reports CommTasks=%d, want 1 (criterion 9: the counter Step 5.4 watches for a day "+
			"after arming ONE rule)", st.CommTasks)
	}
	if st.Appended != 1 {
		t.Errorf("the pass reports Appended=%d, want 1: D3 step 1 is byte-identical — the words still belong in "+
			"the ticket's log, where a session working it reads them", st.Appended)
	}

	// The decision: ONE row, action task_log, task_id = the ticket, comm_task_id
	// = the new task. No new action value (every latest-decision reader, including
	// replyfold.InquiryEligibleLatestSQL, keys on the existing four).
	d := ccDecisionOf(t, ctx, s, mail, "live")
	if d.action != "task_log" {
		t.Errorf("the decision's action is %q, want task_log. Data model: \"a comm decision IS a task_log, and "+
			"inventing an action value would break every latest-decision reader\"", d.action)
	}
	if d.taskID == nil || *d.taskID != ticket {
		t.Errorf("the decision's task_id = %v, want the TICKET task %d", d.taskID, ticket)
	}
	if d.commTaskID == nil {
		t.Fatalf("the decision's comm_task_id is NULL after an ARMED rule's attach. Criterion 42 / D3 step 3: "+
			"the id is recorded on the decision BEFORE the follow-ups, because the live claim is spent and a "+
			"later failure must not lose the pointer to what was created. reason: %s", d.reason)
	}
	comm := *d.commTaskID
	if comm == ticket {
		t.Fatalf("comm_task_id equals the ticket task; the comm must be its OWN row (the whole ticket)")
	}
	if !strings.Contains(d.reason, "comm task requested") {
		t.Errorf("the live decision's reason is %q, want it to say \"comm task requested\" (criterion 7)", d.reason)
	}

	// The COMM task: D4's fields, and the mark that puts it in INCOMING.
	c := ccTaskRow(t, ctx, s, comm)
	if c.status != "ready" {
		t.Errorf("the comm task is %q, want ready. D4: holding would need a Requeue to become workable "+
			"(SWT-72's correction to D6)", c.status)
	}
	if c.assignee != "human" {
		t.Errorf("the comm task's assignee_type is %q, want human — it is HIS to answer, and no worker console "+
			"may claim it (D4)", c.assignee)
	}
	if c.project != crvSlug {
		t.Errorf("the comm task's project is %q, want the RULE's project %q (D4: even if the target task's "+
			"project differs)", c.project, crvSlug)
	}
	if c.priority != 0 {
		t.Errorf("the comm task's priority is %d, want 0 (ruleCreateTaskArgs' value; task_set_priority reorders it)",
			c.priority)
	}
	if !strings.HasPrefix(c.title, "José Garcia") {
		t.Errorf("the comm task's title is %q, want it to START with the sender. \"Usable alone\": the row at "+
			"the top of INCOMING reads `from José Garcia <jose.g@avviato.com>` (criterion 14)", c.title)
	}
	// The body's LAST key/value line is the pointer back to the ticket task.
	// The key block sits between the header sentence's blank line and the
	// preview's (ruleTaskBody's existing shape; located that way since the
	// implementation, 2026-09-22).
	keys := c.body
	if i := strings.Index(c.body, "\n\n"); i >= 0 {
		rest := c.body[i+2:]
		if j := strings.Index(rest, "\n\n"); j >= 0 {
			keys = rest[:j]
		} else {
			keys = rest
		}
	}
	lines := strings.Split(strings.TrimRight(keys, "\n"), "\n")
	if last, want := lines[len(lines)-1], "related_task: "+strconv.FormatInt(ticket, 10); last != want {
		t.Errorf("the comm task's last key line is %q, want %q (criteria 13, 42; D11's key name — one "+
			"vocabulary for both paths)\n\nbody:\n%s", last, want, c.body)
	}
	if c.sourceThread == nil {
		t.Errorf("the comm task's source_thread_id is NULL. D3 step 4: without it draft_delivery REFUSES the " +
			"task, and \"answer the question\" is the point (invariant 4: the thread is what later binds a " +
			"reply to the right conversation)")
	}
	if c.activityAt == nil || c.activityBy == nil || *c.activityBy != mail {
		t.Fatalf("the comm task's activity is %v / %v, want set and by message %d. D3 step 5: this, and ONLY "+
			"this, is what puts the comm in INCOMING — without it the row sits in QUEUE and the ticket is a no-op",
			c.activityAt, c.activityBy, mail)
	}
	if c.surfacedAt != nil {
		t.Errorf("the comm task carries surfaced_at; criterion 12: task_mark_surfaced is NOT called on the comm " +
			"path (that is SWT-45's machinery for an overriding create)")
	}

	// The comm task has NO external_refs row. Load-bearing: external_refs is
	// UNIQUE (system, external_key) and taskForExternalRef takes the NEWEST ref,
	// so a second row would hijack every future attach for CIC-7001.
	if n := s.n(t, ctx, `SELECT count(*) FROM external_refs WHERE task_id=$1`, comm); n != 0 {
		t.Errorf("the comm task carries %d external_refs row(s), want 0 (criterion 12 / D4)", n)
	}

	// The TICKET task: the full log line, the ids-only pointer, and NO mark.
	logs := ccLogs(t, ctx, s, ticket)
	if len(logs) != logsBefore+2 {
		t.Fatalf("the ticket task gained %d log event(s), want exactly 2 (appendRuleLog then the pointer): %v",
			len(logs)-logsBefore, logs[logsBefore:])
	}
	full, pointer := logs[len(logs)-2], logs[len(logs)-1]
	if !strings.Contains(full, "jira CIC-7001") || !strings.Contains(full, "José Garcia") {
		t.Errorf("the ticket task's first new log line is %q, want appendRuleLog's BYTE-IDENTICAL text (D3 step "+
			"1: the words belong in the ticket's log too; the cost that they exist twice is accepted)", full)
	}
	wantPointer := "capture: comm #" + strconv.FormatInt(comm, 10) + " created from this message (message " + strconv.FormatInt(mail, 10) + ")"
	if pointer != wantPointer {
		t.Errorf("the ticket task's pointer log is %q, want EXACTLY %q (criterion 15: ids only — no title, no "+
			"sender, no message text, which is what makes it safe on a claude task)", pointer, wantPointer)
	}
	if at, by := craActivity(t, ctx, s, ticket); at != nil || by != nil {
		t.Errorf("the TICKET task carries activity %v / %v after an ARMED rule's attach. D3: an armed comm "+
			"REPLACES SWT-72's mark on the target — \"a new row in INCOMING, 452 stays in QUEUE\"; surfacing "+
			"both would double the rows (D11's sentence, unchanged)", at, by)
	}
	if got := s.status(t, ctx, ticket); got == "closed" {
		t.Errorf("the ticket task is closed; nothing in the comm path closes it")
	}

	// Invariant 3: every write went through the executor as capture:{connector}.
	// create_task's audit row carries no task_id (the task does not exist when
	// the call starts); it is found by its ARGS instead — the body's related_task
	// line names the ticket (amended 2026-09-22 on implementation: the executor
	// stamps task_id only from Call.TaskID, and audit_events keeps no result).
	if n := s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE tool='create_task' AND actor=$1
	                      AND status='ok' AND args::text LIKE $2`, crvActor, "%related_task: "+strconv.FormatInt(ticket, 10)+"%"); n != 1 {
		t.Errorf("audit_events for the create_task that made comm %d = %d, want 1 (invariant 3)", comm, n)
	}
	for _, tc := range []struct {
		task int64
		tool string
		want int
	}{
		{comm, "task_set_source_thread", 1},
		{comm, "task_mark_activity", 1},
		{comm, "link_external_ref", 0},
		{comm, "task_mark_surfaced", 0},
		{ticket, "task_mark_activity", 0},
	} {
		if n := s.audits(t, ctx, tc.task, tc.tool, crvActor); n != tc.want {
			t.Errorf("audit_events for %s on task %d = %d, want %d (invariant 3: the comm is created, given its "+
				"thread and surfaced through the EXECUTOR; the only direct SQL is capture's own "+
				"capture_decisions log)", tc.tool, tc.task, n, tc.want)
		}
	}
	// The target's ONE log pointer is an executor call too (2 = appendRuleLog + the pointer).
	if n := s.audits(t, ctx, ticket, "task_append_log", crvActor); n != 2 {
		t.Errorf("task_append_log audit rows on the ticket task = %d, want 2 (the full line, then the pointer)", n)
	}
	_ = rule
}

// ---- criterion 43: COLUMN-FED, both directions ----------------------------------------

// IK "test the column, not the fixture": the ONLY thing that changes between
// the two halves is one UPDATE of capture_rules.comm_task. A `loadRules` that
// selected a literal would make both halves behave the same, and the fixture
// supplies neither value.
func TestCaptureComm_Integration_ColumnFed(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	ccRequire0040(t, ctx, s.pool)

	ticket := craSeedTask(t, ctx, s, "CIC-7010")
	rule := ccCommRule(t, ctx, s, true)

	first := ccCommMail(t, ctx, s, ccJose, "CIC-7010 is blocked", "CIC-7010 still fails on import")
	s.pass(t, ctx, capture.RulesModeLive)
	if d := ccDecisionOf(t, ctx, s, first, "live"); d.commTaskID == nil {
		t.Fatalf("ARMED: no comm task for the first message (reason %q)", d.reason)
	}
	if at, _ := craActivity(t, ctx, s, ticket); at != nil {
		t.Fatalf("ARMED: the ticket task was marked as well as the comm (criterion 42)")
	}

	// DISARM the rule — one UPDATE, no deploy, which is also the SPEC's rollback.
	s.exec(t, ctx, `UPDATE capture_rules SET comm_task = false WHERE id = $1`, rule)

	second := ccCommMail(t, ctx, s, ccJose, "CIC-7010 again", "one more note about CIC-7010")
	st := s.pass(t, ctx, capture.RulesModeLive)
	if st.CommTasks != 0 {
		t.Errorf("a DISARMED rule created %d comm task(s). Criterion 43/17: an un-armed rule behaves exactly as "+
			"SWT-72 ships — log line plus task_mark_activity on the TARGET", st.CommTasks)
	}
	d := ccDecisionOf(t, ctx, s, second, "live")
	if d.commTaskID != nil {
		t.Errorf("a DISARMED rule recorded comm_task_id=%v", d.commTaskID)
	}
	if !strings.Contains(strings.ToLower(d.reason), "comm rule") {
		t.Errorf("the disarmed decision's reason is %q; D2 clause 1 names its own cause (\"the matched rule is "+
			"not a comm rule\")", d.reason)
	}
	at, by := craActivity(t, ctx, s, ticket)
	if at == nil || by == nil || *by != second {
		t.Errorf("a DISARMED rule left the ticket task's activity at %v / %v, want set by message %d. Criterion "+
			"43: with comm_task=false the ticket task's activity_at MOVES instead (SWT-72's behaviour), and "+
			"that swap is the whole ticket", at, by, second)
	}
	// The second message still filed onto the TICKET task, not onto the comm:
	// proof that the comm path wrote no external_refs row (criterion 12's mutation).
	if d.taskID == nil || *d.taskID != ticket {
		t.Errorf("the second message filed onto task %v, want the TICKET task %d. A link_external_ref on the "+
			"comm path would hijack every future attach for this key onto the comm", d.taskID, ticket)
	}
}

// ---- criterion 44: the noise keeps filing silently ------------------------------------

func TestCaptureComm_Integration_TheNoiseSenders(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	ccRequire0040(t, ctx, s.pool)

	// The notifier list is a COLUMN too (projects.notifier_senders, SWT-53 CC4).
	s.exec(t, ctx, `UPDATE projects SET notifier_senders = $2 WHERE id = $1`,
		s.project, []string{"noreply@caprev.jira.com"})

	anon := craSeedTask(t, ctx, s, "CIC-7020")
	notif := craSeedTask(t, ctx, s, "CIC-7021")
	blank := craSeedTask(t, ctx, s, "CIC-7022")
	ccCommRule(t, ctx, s, true)

	// ONE pass, three messages (criterion 44).
	mAnon := ccCommMail(t, ctx, s, ccAnonymous, "(CIC-7020) Status changed", "status changed to In Review")
	mNotif := ccCommMail(t, ctx, s, ccNotifier, "(CIC-7021) updated", "CIC-7021 was updated")
	mBlank := ccCommMail(t, ctx, s, "", "(CIC-7022) automated", "CIC-7022 nightly report")
	st := s.pass(t, ctx, capture.RulesModeLive)
	if st.CommTasks != 0 {
		t.Errorf("the noise pass created %d comm task(s), want 0. D10/D2: a comm task for each of these is a "+
			"worse board than today's", st.CommTasks)
	}

	for _, tc := range []struct {
		name   string
		msg    int64
		task   int64
		marker string
		why    string
	}{
		{"anonymous jira", mAnon, anon, "anonymous", "D2 clause 5: `Anonymous (JIRA)` is Jira's placeholder for " +
			"HIS OWN change (IK SWT-45: 182 of 208 such Treetop mails are field/status-edit notices)"},
		{"notifier", mNotif, notif, "notifier", "D2 clause 4: the project's notifier list, matched by EQUALITY " +
			"(SWT-53 CC4), never a substring"},
		{"blank sender", mBlank, blank, "sender", "D2 clause 3: fails CLOSED — notifierSender is an equality " +
			"and can never match \"\", so with no identity at all the message only logs"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			d := ccDecisionOf(t, ctx, s, tc.msg, "live")
			if d.commTaskID != nil {
				t.Errorf("%s created comm task %d — %s", tc.name, *d.commTaskID, tc.why)
			}
			if !strings.Contains(strings.ToLower(d.reason), tc.marker) {
				t.Errorf("%s's decision reason is %q, want it to NAME its own cause (a reason containing %q) — "+
					"%s. Criterion 44: each decision's reason names its own cause, so a smoke read of "+
					"capture_decisions tells them apart", tc.name, d.reason, tc.marker, tc.why)
			}
			// Each target keeps SWT-72's mark: the noise is not invisible, it is
			// just not its own row.
			at, by := craActivity(t, ctx, s, tc.task)
			if at == nil || by == nil || *by != tc.msg {
				t.Errorf("%s left the target's activity at %v / %v, want set by message %d. Criterion 44: "+
					"\"each target keeps SWT-72's mark\" — the un-armed behaviour is what a refused comm falls "+
					"back to", tc.name, at, by, tc.msg)
			}
		})
	}
}

// ---- criterion 45: channel-blind -------------------------------------------------------

func TestCaptureComm_Integration_SlackAndJiraToo(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	ccRequire0040(t, ctx, s.pool)

	slackTicket := craSeedTask(t, ctx, s, "CIC-7030")
	jiraTicket := craSeedTask(t, ctx, s, "CIC-7031")
	ccCommRule(t, ctx, s, true)

	now := s.dbNow(t, ctx)
	slackMsg := s.slackMsg(t, ctx, "CIC-7030 — can you confirm the cutover date?", now, now)
	// A jira-CHANNEL message that is not the connector rule's thread shape, so
	// the armed rule wins: this is D10's second copy, and the board renders it
	// as `new comment`.
	jiraMsg := s.msg(t, ctx, s.jiraAcct, crvMailPfx+"<jira-comment-7031>", "jira", "inbound",
		ccKatie, "", "CIC-7031 — I left a comment", now, now)

	st := s.pass(t, ctx, capture.RulesModeLive)
	if st.CommTasks != 2 {
		t.Fatalf("the pass created %d comm task(s), want 2 (criterion 45: the comm path is channel-blind — "+
			"neither commTask nor the create path contains a channel literal)", st.CommTasks)
	}

	for _, tc := range []struct {
		name, channel string
		msg, ticket   int64
	}{
		{"slack", "slack", slackMsg, slackTicket},
		{"jira", "jira", jiraMsg, jiraTicket},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			d := ccDecisionOf(t, ctx, s, tc.msg, "live")
			if d.commTaskID == nil {
				t.Fatalf("no comm task for the %s message (reason %q)", tc.name, d.reason)
			}
			c := ccTaskRow(t, ctx, s, *d.commTaskID)
			if c.activityBy == nil || *c.activityBy != tc.msg {
				t.Fatalf("the %s comm task's activity_by_message_id = %v, want %d", tc.name, c.activityBy, tc.msg)
			}
			// The REMARK the board renders comes from the message's CHANNEL
			// through SWT-72's PK join — no stored discriminator, no new
			// incoming kind (D5). The render itself is criterion 42's board
			// half, in internal/dashboard.
			var channel string
			if err := s.pool.QueryRow(ctx, `SELECT nm.channel FROM tasks t
			      JOIN normalized_messages nm ON nm.id = t.activity_by_message_id WHERE t.id=$1`,
				*d.commTaskID).Scan(&channel); err != nil {
				t.Fatalf("read the comm task's activity channel: %v", err)
			}
			if channel != tc.channel {
				t.Errorf("the %s comm task's activity message has channel %q, want %q — the remark "+
					"(`new slack` / `new comment`) is computed from it at render time (D5)", tc.name, channel, tc.channel)
			}
			if at, _ := craActivity(t, ctx, s, tc.ticket); at != nil {
				t.Errorf("the %s ticket task was marked too (criterion 42/D3)", tc.name)
			}
		})
	}
}

// ---- criterion 10: shadow creates nothing ----------------------------------------------

func TestCaptureComm_Integration_ShadowCreatesNothing(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	ccRequire0040(t, ctx, s.pool)

	ticket := craSeedTask(t, ctx, s, "CIC-7040")
	ccCommRule(t, ctx, s, true)
	mail := ccCommMail(t, ctx, s, ccJose, "CIC-7040 question", "one question about CIC-7040")

	tasksBefore := s.n(t, ctx, `SELECT count(*) FROM tasks`)
	auditsBefore := s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE actor=$1`, crvActor)

	st := s.pass(t, ctx, capture.RulesModeShadow)
	if st.CommTasks != 0 {
		t.Errorf("a SHADOW pass reports CommTasks=%d, want 0: shadow extracts and records everything and "+
			"CREATES NOTHING (criterion 10)", st.CommTasks)
	}
	d := ccDecisionOf(t, ctx, s, mail, "shadow")
	if d.commTaskID != nil {
		t.Errorf("a SHADOW decision carries comm_task_id=%v, want NULL (criterion 10)", d.commTaskID)
	}
	if !strings.Contains(d.reason, "would create a comm task") {
		t.Errorf("the shadow decision's reason is %q, want it to say \"would create a comm task\" — that "+
			"sentence IS how Verification Step 0a's arming decision is made before anything is armed "+
			"(criteria 7, 10)", d.reason)
	}
	if n := s.n(t, ctx, `SELECT count(*) FROM tasks`); n != tasksBefore {
		t.Errorf("a shadow pass created %d task(s)", n-tasksBefore)
	}
	if n := s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE actor=$1`, crvActor); n != auditsBefore {
		t.Errorf("a shadow pass made %d executor call(s), want 0 (criterion 10: it calls no tool)", n-auditsBefore)
	}
	if at, _ := craActivity(t, ctx, s, ticket); at != nil {
		t.Errorf("a shadow pass marked the ticket task")
	}
}

// ---- criterion 11: a replay creates nothing --------------------------------------------

func TestCaptureComm_Integration_AReplayCreatesNothing(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	ccRequire0040(t, ctx, s.pool)

	craSeedTask(t, ctx, s, "CIC-7050")
	ccCommRule(t, ctx, s, true)
	mail := ccCommMail(t, ctx, s, ccJose, "CIC-7050 question", "a question about CIC-7050")

	if st := s.pass(t, ctx, capture.RulesModeLive); st.CommTasks != 1 {
		t.Fatalf("setup: the first live pass created %d comm task(s), want 1", st.CommTasks)
	}
	tasks := s.n(t, ctx, `SELECT count(*) FROM tasks`)
	events := s.n(t, ctx, `SELECT count(*) FROM task_events`)
	audits := s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE actor=$1`, crvActor)
	decisions := s.n(t, ctx, `SELECT count(*) FROM capture_decisions WHERE message_id=$1 AND mode='live'`, mail)

	for i := 0; i < 3; i++ {
		if st := s.pass(t, ctx, capture.RulesModeLive); st.CommTasks != 0 {
			t.Errorf("replay %d created %d comm task(s). Criterion 11: insertDecision returns inserted=false — "+
				"the partial unique index capture_decisions_live_uniq IS the claim, and one live action per "+
				"message is forever", i+1, st.CommTasks)
		}
	}
	if n := s.n(t, ctx, `SELECT count(*) FROM tasks`); n != tasks {
		t.Errorf("three replays created %d task(s), want 0", n-tasks)
	}
	if n := s.n(t, ctx, `SELECT count(*) FROM task_events`); n != events {
		t.Errorf("three replays wrote %d task_event(s), want 0 (task_append_log has NO dedup of its own)", n-events)
	}
	if n := s.n(t, ctx, `SELECT count(*) FROM audit_events WHERE actor=$1`, crvActor); n != audits {
		t.Errorf("three replays made %d executor call(s), want 0", n-audits)
	}
	if n := s.n(t, ctx, `SELECT count(*) FROM capture_decisions WHERE message_id=$1 AND mode='live'`, mail); n != decisions {
		t.Errorf("three replays wrote %d further live decision(s), want 0", n-decisions)
	}
}

// ---- criterion 16: a CLOSED target is untouched ------------------------------------------

func TestCaptureComm_Integration_ClosedTargetsAreUntouched(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	ccRequire0040(t, ctx, s.pool)

	ticket, _ := s.closedTask(t, ctx, "CIC-7060")
	ccCommRule(t, ctx, s, true)
	mail := ccCommMail(t, ctx, s, ccJose, "CIC-7060 one more thing", "about CIC-7060, one more thing")

	st := s.pass(t, ctx, capture.RulesModeLive)
	if st.CommTasks != 0 {
		t.Errorf("an armed rule created %d comm task(s) for a CLOSED target. D2 clause 2: \"the task is closed; "+
			"SWT-45's revive and SWT-53's resurface own it\"", st.CommTasks)
	}
	d := ccDecisionOf(t, ctx, s, mail, "live")
	if d.commTaskID != nil {
		t.Errorf("comm_task_id = %v for a closed target", d.commTaskID)
	}
	if !strings.Contains(strings.ToLower(d.reason), "closed") {
		t.Errorf("the decision reason is %q, want it to name the CLOSED task as the cause (criterion 4)", d.reason)
	}
	// SWT-53's behaviour is untouched: the message still resurfaces for the
	// inquiry lane, exactly as it does today.
	if !d.resurface {
		t.Errorf("capture_decisions.resurface = false for a person's message on a closed task; criterion 16 / " +
			"the out-of-scope list: \"Closed tasks — SWT-45's revive and SWT-53's resurface keep them\"")
	}
	if got := s.status(t, ctx, ticket); got != "closed" {
		t.Errorf("the closed target moved to %q; a non-reviving armed rule changes nothing about it", got)
	}
}

// ---- D8 (as amended 2026-09-22): a first message about a NEW key ------------------------

// D8's RULE: the comm path lives strictly in the found / task_log branch, so a
// first message about a NEW key gets the TICKET task and no comm task — a
// second row would duplicate it.
//
// D8's SENTENCE ("a capture-created ticket task still lands in QUEUE") is STALE
// since swb #491: the create branch now marks the new task with its own
// message, so it lands in INCOMING with the sender. This asserts the CURRENT
// behaviour, deliberately.
func TestCaptureComm_Integration_TheFirstMessageAboutANewKeyGetsNoComm(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	ccRequire0040(t, ctx, s.pool)

	ccCommRule(t, ctx, s, true)
	tasksBefore := s.n(t, ctx, `SELECT count(*) FROM tasks`)
	mail := ccCommMail(t, ctx, s, ccJose, "CIC-7070 — new request", "please look at CIC-7070 when you can")

	st := s.pass(t, ctx, capture.RulesModeLive)
	if st.CommTasks != 0 {
		t.Errorf("a first message about a NEW key created %d comm task(s), want 0. D8: the person's message "+
			"ALREADY becomes a task — the ticket task — and a second, comm task would duplicate it", st.CommTasks)
	}
	if st.TasksCreated != 1 {
		t.Fatalf("the pass created %d ticket task(s), want 1", st.TasksCreated)
	}
	if n := s.n(t, ctx, `SELECT count(*) FROM tasks`) - tasksBefore; n != 1 {
		t.Fatalf("the pass created %d task(s) in total, want exactly 1 (D8: never two rows for one first message)", n)
	}
	d := ccDecisionOf(t, ctx, s, mail, "live")
	if d.action != "task" {
		t.Fatalf("the decision's action is %q, want task", d.action)
	}
	if d.commTaskID != nil {
		t.Errorf("a `task` decision carries comm_task_id=%v; 0040's CHECK (comm_task_id IS NULL OR action = "+
			"'task_log') must refuse it in the data as well", d.commTaskID)
	}
	ticket := s.mustTask(t, ctx, "CIC-7070")
	// 2026-09-22 (swb #491), the amendment this file's header names: the
	// rule-CREATED task is marked with its own message, so it lands in INCOMING
	// rather than QUEUE. D8's stale sentence said otherwise; the behaviour on
	// main is this one.
	at, by := craActivity(t, ctx, s, ticket)
	if at == nil || by == nil || *by != mail {
		t.Errorf("the rule-CREATED ticket task's activity is %v / %v, want set by message %d. 2026-09-22 "+
			"(swb #491, \"lyle's emails are not landing in incoming\"): a task a rule creates from a person's "+
			"first message is marked with that message too. This test encodes the CURRENT behaviour; D8's "+
			"sentence \"a capture-created ticket task still lands in QUEUE\" predates it and is stale", at, by, mail)
	}
}

// ---- criterion 41: the ticket-status reconciler is unaffected ----------------------------

func TestCaptureComm_Integration_ADoneTicketStillClosesItsTicketTask(t *testing.T) {
	ctx := context.Background()
	s := newCRVSuite(t, ctx)
	ccRequire0040(t, ctx, s.pool)

	s.snapshot(t, ctx, "CIC-7080", "indeterminate", "In Progress")
	ticket := craSeedTask(t, ctx, s, "CIC-7080")
	ccCommRule(t, ctx, s, true)
	mail := ccCommMail(t, ctx, s, ccJose, "CIC-7080 — one question", "a question about CIC-7080")
	if st := s.pass(t, ctx, capture.RulesModeLive); st.CommTasks != 1 {
		t.Fatalf("setup: the pass created %d comm task(s), want 1", st.CommTasks)
	}
	comm := *ccDecisionOf(t, ctx, s, mail, "live").commTaskID

	// The ticket goes Done in Jira.
	s.snapshot(t, ctx, "CIC-7080", "done", "Closed-ish")
	s.reconcile(t, ctx)

	if got := s.status(t, ctx, ticket); got != "closed" {
		t.Errorf("the ticket task is %q after its ticket went Done, want closed. Criterion 41: the comm path "+
			"must not hold it open — that is exactly the bite SWT-72 D1 avoided by NOT reusing surfaced_at", got)
	}
	if r := ccTaskRow(t, ctx, s, ticket); r.surfacedAt != nil {
		t.Errorf("the ticket task carries surfaced_at=%v; criterion 41: surfaced_at is UNTOUCHED by this ticket "+
			"(its reader is the reconciler's J11 hold)", r.surfacedAt)
	}
	if got := s.status(t, ctx, comm); got == "closed" {
		t.Errorf("the reconciler closed the COMM task; it carries no external_refs row and is not a ticket task " +
			"(criterion 12 / D4)")
	}
}
