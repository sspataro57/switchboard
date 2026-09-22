package capture

// comms-inbox (SWT-74, docs/tickets/comms-inbox_SPEC.md), the PURE half of the
// capture side: criterion 3 (anonymousJiraActor, built from the existing Jira
// From parse and introducing NO new literal), criterion 4 (commTask's five
// clauses, first cause wins, a distinct reason each), criterion 13
// (ruleTaskBody's new relatedTaskID parameter: 0 is byte-identical to today)
// and criterion 14 (commTaskTitle's fallback chain). ZERO I/O.
//
// The SPEC's dated amendment is at the bottom of this header; read it before
// changing any ordering assertion.
//
// ---- IMPOSED SURFACE (SPEC D2, D4 and criteria 2-4, 13, 14) -------------------
//
//	// internal/capture/comm.go (new, pure: no context, no pgx, no pool, no
//	// connector import, no os.Getenv, no time.Now, no regexp — resurface.go's shape)
//	type commInput struct {
//	    armed       bool   // the winning rule's capture_rules.comm_task
//	    status      string // the linked task's tasks.status, from taskForExternalRef
//	    blankSender bool   // blankSender(sender)             — resurface.go's spelling
//	    notifier    bool   // notifierSender(sender, winner.notifiers) — resurface.go's spelling
//	    ownJiraEdit bool   // anonymousJiraActor(sender)      — ownaction.go's spelling
//	}
//	// commTask is true iff armed AND the task is not closed AND the sender is
//	// not blank AND not a notifier AND not Jira's Anonymous placeholder. The
//	// string is the reason fragment capture_decisions.reason records; FIRST
//	// CAUSE WINS, in D2's order, and every false outcome names its own cause.
//	func commTask(in commInput) (bool, string)
//
//	// internal/capture/ownaction.go — beside its siblings, the EXISTING parse,
//	// no new string literal:
//	func anonymousJiraActor(from string) bool {
//	    name, ok := jiraNotificationActor(from)
//	    return ok && sameJiraActor(name, jiraAnonymousActor)
//	}
//
//	// internal/capture/rules_store.go
//	func ruleTaskBody(pm pendingMessage, winner storedRule, system, key string, relatedTaskID int64) string
//	  // relatedTaskID == 0 -> byte-identical to today; N -> `related_task: N` as
//	  // the LAST key/value line, before the blank line and the preview (D4).
//	func commTaskTitle(pm pendingMessage, winner storedRule, key string) string
//	  // `{sender}: {subject else first line}` through
//	  // textmatch.NormalizedPrefix(…, rulesTitleLen); the label falls back to the
//	  // project NAME, then the slug, then the key; never empty, never a dangling
//	  // separator (ruleTaskTitle's criterion 4). The SPEC fixes the BEHAVIOUR,
//	  // not the Go spelling: this is the smallest call carrying every input
//	  // criterion 14 names, all of which the call site already holds
//	  // (the rules_title_test.go precedent).
//
// GREENFIELD NOTE — EXPECTED RED: comm.go, commInput, commTask,
// anonymousJiraActor and commTaskTitle do not exist, and ruleTaskBody takes
// four parameters, so this file compile-FAILS the whole internal/capture unit
// binary. That is the intended initial state.
//
// SPEC AMENDMENT THE TESTS BELOW ENCODE (2026-09-22 12:20, swb #491, on main —
// the SPEC predates it): capture's actionTask branch now ALSO marks activity on
// a task it CREATES. D8's rule itself stands (a first message about a NEW key
// gets the ticket task and NO comm task), but D8's sentence "a capture-created
// ticket task still lands in QUEUE" is STALE — it lands in INCOMING, marked
// with its own message. Nothing in this file's pure functions changes; the
// note is here because commTask's clause 1 is what keeps the create branch out
// of the comm path.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - drop clause 2 (closed task) from commTask -> FirstCauseWins.
//   - drop the ownJiraEdit / notifier / blankSender clause -> the same.
//   - reorder the clauses -> FirstCauseWins (the reason is the assertion).
//   - anonymousJiraActor bites on a named actor -> AnonymousJiraActor.
//   - ruleTaskBody emits `related_task: 0` -> RelatedTaskLineOnlyWhenSet.
//   - commTaskTitle leaves a dangling separator -> TitleNeverDangles.

import (
	"strings"
	"testing"
	"time"
)

// ---- criterion 3: anonymousJiraActor -------------------------------------------

// The table the criterion names, verbatim. `Anonymous (JIRA)` is Jira's
// placeholder for HIS OWN change (IK SWT-45: 182 of 208 such Treetop mails are
// field/status-edit notices), so it is the one From shape an armed rule must
// never turn into a comm. Everything else — a NAMED Jira actor, a person, a
// Slack display name, nothing at all — passes untouched.
func TestAnonymousJiraActor(t *testing.T) {
	for _, tc := range []struct {
		from string
		want bool
		why  string
	}{
		{"Anonymous (JIRA)", true, "the bare placeholder: Jira could not attribute the change, i.e. it was his"},
		{`"Anonymous (JIRA)" <jira@x>`, true, "the same placeholder inside a quoted display name with an address"},
		{"  anonymous   (JIRA)  ", true, "sameJiraActor folds case and whitespace runs, like namedJiraActor"},
		{"Katie Evans (JIRA)", false, "a NAMED actor is somebody else's comment — D10's accepted residual"},
		{"José Garcia <jose.g@avviato.com>", false, "a person writing directly: no (JIRA) shape at all"},
		{"Katie", false, "a Slack display name"},
		{"", false, "a blank sender is clause 3's job, not this one"},
		{"Anonymous", false, "no (JIRA) suffix: this is a parse of Jira's template, not a name blacklist"},
		{"jira@caprev.jira.com", false, "a bare address"},
	} {
		tc := tc
		t.Run(tc.from, func(t *testing.T) {
			if got := anonymousJiraActor(tc.from); got != tc.want {
				t.Errorf("anonymousJiraActor(%q) = %v, want %v — %s", tc.from, got, tc.want, tc.why)
			}
		})
	}
}

// Criterion 3's second half: it is built from the EXISTING parse. A second
// spelling of `(JIRA)` or `Anonymous` anywhere in the package is the SWT-13
// magic-literal landmine; the structural half of this is in
// comm_structure_test.go, and this is the behavioural proof that the two
// functions agree on every shape.
func TestAnonymousJiraActor_AgreesWithTheExistingParse(t *testing.T) {
	for _, from := range []string{
		"Anonymous (JIRA)", `"Anonymous (JIRA)" <jira@x>`, "Katie Evans (JIRA)", "Katie (QA) Evans (JIRA)",
		"José Garcia <jose.g@avviato.com>", "", "Jira <jira@x>",
	} {
		name, ok := jiraNotificationActor(from)
		want := ok && sameJiraActor(name, jiraAnonymousActor)
		if got := anonymousJiraActor(from); got != want {
			t.Errorf("anonymousJiraActor(%q) = %v but jiraNotificationActor/sameJiraActor say %v. Criterion 3: "+
				"it IS `ok && sameJiraActor(name, jiraAnonymousActor)` and introduces no new literal", from, got, want)
		}
	}
	// namedJiraActor is the sibling that excludes the placeholder; the two must
	// never both be true, or "whose change is this" has two answers.
	for _, from := range []string{"Anonymous (JIRA)", "Katie Evans (JIRA)", "Katie"} {
		if anonymousJiraActor(from) && namedJiraActor(from) != "" {
			t.Errorf("both anonymousJiraActor and namedJiraActor claim %q", from)
		}
	}
}

// ---- criterion 4: commTask's five clauses, first cause wins ---------------------

// The reason FRAGMENTS. The SPEC names the causes, not the exact sentences, so
// each row asserts the substring that identifies its clause plus the rule that
// every false outcome is distinguishable from every other (a smoke read of
// capture_decisions.reason has to tell them apart — resurfaces' contract).
var commClauseMarkers = []struct {
	clause int
	marker string
	why    string
}{
	{1, "comm rule", "D2 clause 1: `!armed` — the matched rule is not a comm rule"},
	{2, "closed", "D2 clause 2: SWT-45's revive and SWT-53's resurface own a closed task"},
	{3, "sender", "D2 clause 3: a blank sender fails CLOSED (notifierSender is an equality and can never match \"\")"},
	{4, "notifier", "D2 clause 4: the project's notifier list"},
	{5, "anonymous", "D2 clause 5: Jira's Anonymous (JIRA) placeholder — HIS OWN change (SWT-72 D10's residual)"},
}

// The full grid criterion 4 names: {armed, not armed} x {open, closed} x
// {plain, blank, notifier, anonymous-jira sender}, asserting the exact FIRST
// cause in each row. First cause wins is the whole contract: a reason that
// names the second cause is a reason a reader cannot act on.
func TestCommTask_FirstCauseWins(t *testing.T) {
	type senderShape struct {
		name                           string
		blank, notifier, anonymousJira bool
		clause                         int // the clause this shape trips, 0 = none
	}
	senders := []senderShape{
		{name: "plain person"},
		{name: "blank", blank: true, clause: 3},
		{name: "notifier", notifier: true, clause: 4},
		{name: "anonymous jira", anonymousJira: true, clause: 5},
	}
	statuses := []struct {
		status string
		clause int
	}{
		{"ready", 0}, {"in_progress", 0}, {"holding", 0}, {"blocked", 0}, {"delivered", 0},
		{"closed", 2},
	}
	trues := 0
	for _, armed := range []bool{false, true} {
		for _, st := range statuses {
			for _, s := range senders {
				in := commInput{armed: armed, status: st.status, blankSender: s.blank,
					notifier: s.notifier, ownJiraEdit: s.anonymousJira}
				// D2's order: 1 !armed, 2 closed, 3 blank, 4 notifier, 5 own jira edit.
				wantClause := 0
				switch {
				case !armed:
					wantClause = 1
				case st.clause != 0:
					wantClause = st.clause
				case s.clause != 0:
					wantClause = s.clause
				}
				got, reason := commTask(in)
				if wantClause == 0 {
					trues++
					if !got {
						t.Errorf("commTask(%+v) = false (%q), want TRUE: an armed rule, an OPEN task and a "+
							"person's sender is the whole ticket — the comm becomes its own INCOMING row", in, reason)
					}
					continue
				}
				if got {
					t.Errorf("commTask(%+v) = true, want false by clause %d", in, wantClause)
					continue
				}
				marker := commClauseMarkers[wantClause-1]
				if !strings.Contains(strings.ToLower(reason), marker.marker) {
					t.Errorf("commTask(%+v) = false with reason %q, want the FIRST cause to be clause %d "+
						"(a reason containing %q) — %s", in, reason, wantClause, marker.marker, marker.why)
				}
			}
		}
	}
	if trues == 0 {
		t.Fatalf("POSITIVE CONTROL FAILED: no input in the grid produced a comm task; the table cannot tell a " +
			"first-cause bug from a function that always returns false")
	}
}

// Every false outcome names its OWN cause: the five fragments are pairwise
// distinct. Without this, "first cause wins" is unobservable in production —
// capture_decisions.reason is the only record of why no comm task exists.
func TestCommTask_EveryClauseHasItsOwnReason(t *testing.T) {
	reasons := map[string]int{}
	for _, in := range []commInput{
		{armed: false, status: "ready"},
		{armed: true, status: "closed"},
		{armed: true, status: "ready", blankSender: true},
		{armed: true, status: "ready", notifier: true},
		{armed: true, status: "ready", ownJiraEdit: true},
	} {
		ok, reason := commTask(in)
		if ok {
			t.Fatalf("commTask(%+v) = true; this table is the FALSE outcomes", in)
		}
		if reason == "" {
			t.Errorf("commTask(%+v) returned an empty reason; every clause records its own fragment on "+
				"capture_decisions.reason", in)
		}
		reasons[reason]++
	}
	if len(reasons) != 5 {
		t.Errorf("the five clauses produced %d distinct reasons (%v), want 5 — a shared sentence makes the "+
			"cause unreadable in the decision row (resurfaces' contract, criterion 4)", len(reasons), reasons)
	}
}

// The true reason is a reason too: a live pass records that a comm task was
// requested, a shadow pass that one would be created (criterion 7 wires the
// mode words; here we only require that the TRUE outcome says something).
func TestCommTask_TheTrueOutcomeCarriesAReason(t *testing.T) {
	ok, reason := commTask(commInput{armed: true, status: "ready"})
	if !ok {
		t.Fatalf("commTask(armed, open, plain sender) = false (%q)", reason)
	}
	if strings.TrimSpace(reason) == "" {
		t.Errorf("commTask's TRUE outcome carries no reason fragment; the decision row has to say why a comm " +
			"task exists, not only why one does not")
	}
}

// ---- criterion 13: ruleTaskBody's relatedTaskID ----------------------------------

// commBodyMsg is the José case's shape: a direct mail quoting a jira key.
func commBodyPending() pendingMessage {
	return pendingMessage{
		msg: Message{
			ID: 8801, ThreadKey: "gmail:jose@avviato.example:<m1@mail>", Sender: "José Garcia <jose.g@avviato.com>",
			Subject:           "WEB-10469 blocks the import",
			BodyText:          "Hi Salvador,\nthe importer still rejects the file",
			ExternalMessageID: "<m1@mail>",
		},
		channel: "gmail",
		sentAt:  time.Date(2026, 9, 22, 8, 0, 0, 0, time.UTC),
	}
}

func commBodyRule() storedRule {
	return storedRule{
		rule:        Rule{ID: 75, Project: "collaboratory", Kind: "body_regex", Pattern: `WEB-[0-9]+`, Enabled: true},
		projectID:   9002,
		extSystem:   "jira",
		projectName: "Collaboratory",
	}
}

// With 0 the output is BYTE-IDENTICAL to today: `related_task` does not appear
// at all, and every existing body (every ticket task capture has ever created)
// stays what it was. CC6's "one line, only when set".
func TestRuleTaskBody_RelatedTaskLineOnlyWhenSet(t *testing.T) {
	pm, winner := commBodyPending(), commBodyRule()
	zero := ruleTaskBody(pm, winner, "jira", "WEB-10469", 0)
	if strings.Contains(zero, "related_task") {
		t.Errorf("ruleTaskBody(..., 0) mentions related_task:\n%s\nCriterion 13: with 0 the output is "+
			"BYTE-IDENTICAL to today, so every existing body is unchanged", zero)
	}
	if strings.Contains(zero, "related_task: 0") {
		t.Errorf("ruleTaskBody emits `related_task: 0` — the SPEC's own mutation row (criterion 13)")
	}

	with := ruleTaskBody(pm, winner, "jira", "WEB-10469", 452)
	if !strings.Contains(with, "related_task: 452\n") {
		t.Fatalf("ruleTaskBody(..., 452) carries no `related_task: 452` line:\n%s", with)
	}
	// It is the LAST key/value line: after message_id / external_message_id and
	// BEFORE the blank line and the preview. D4: what follows the blank line is
	// free text, and a `key: value` after a 400-character preview is unreadable.
	// The key block sits between the header sentence's blank line and the
	// preview's (ruleTaskBody's existing shape; located that way since the
	// implementation, 2026-09-22).
	keys := with
	if i := strings.Index(with, "\n\n"); i >= 0 {
		rest := with[i+2:]
		if j := strings.Index(rest, "\n\n"); j >= 0 {
			keys = rest[:j]
		} else {
			keys = rest
		}
	}
	lines := strings.Split(strings.TrimRight(keys, "\n"), "\n")
	last := lines[len(lines)-1]
	if last != "related_task: 452" {
		t.Errorf("the last key/value line before the blank line is %q, want `related_task: 452`. D4: the line "+
			"goes WITH the other keys, never after the preview:\n%s", last, with)
	}
	// And the ONLY difference from the zero body is that one line: D11's key
	// name, one vocabulary for both paths.
	if got := strings.Replace(with, "related_task: 452\n", "", 1); got != zero {
		t.Errorf("ruleTaskBody(..., 452) differs from ruleTaskBody(..., 0) by more than the one line.\n"+
			"with-minus-line: %q\n           zero: %q", got, zero)
	}
}

// ---- criterion 14: commTaskTitle ---------------------------------------------------

func TestCommTaskTitle_SenderThenSubjectElseFirstLine(t *testing.T) {
	winner := commBodyRule()
	pm := commBodyPending()
	got := commTaskTitle(pm, winner, "WEB-10469")
	if !strings.HasPrefix(got, "José Garcia <jose.g@avviato.com>: ") {
		t.Errorf("commTaskTitle = %q, want it to LEAD with the sender and a colon. D4/criterion 14: "+
			"`{sender}: {subject else first line}` — \"from José Garcia <jose.g@avviato.com>\" is how he "+
			"recognises the row at the top of INCOMING", got)
	}
	if !strings.Contains(got, "WEB-10469 blocks the import") {
		t.Errorf("commTaskTitle = %q, want the SUBJECT after the separator", got)
	}

	// No subject: the body's FIRST LINE, never the whole body.
	noSubject := pm
	noSubject.msg.Subject = ""
	got = commTaskTitle(noSubject, winner, "WEB-10469")
	if !strings.Contains(got, "Hi Salvador,") {
		t.Errorf("commTaskTitle with no subject = %q, want the body's first line (ruleTaskTitle's shape)", got)
	}
	if strings.Contains(got, "the importer still rejects") {
		t.Errorf("commTaskTitle with no subject = %q, want the FIRST LINE only", got)
	}
}

// The fallback chain, criterion 14: sender -> project NAME -> slug -> key. The
// title is never empty, and never a dangling separator.
func TestCommTaskTitle_FallbackChainAndNeverDangles(t *testing.T) {
	base := commBodyPending()
	base.msg.Sender = ""
	winner := commBodyRule()

	if got := commTaskTitle(base, winner, "WEB-10469"); !strings.HasPrefix(got, "Collaboratory") {
		t.Errorf("commTaskTitle with a blank sender = %q, want the project NAME as the label (criterion 14)", got)
	}
	noName := winner
	noName.projectName = ""
	if got := commTaskTitle(base, noName, "WEB-10469"); !strings.HasPrefix(got, "collaboratory") {
		t.Errorf("commTaskTitle with a blank sender and no project name = %q, want the SLUG (criterion 14)", got)
	}
	noProject := noName
	noProject.rule.Project = ""
	if got := commTaskTitle(base, noProject, "WEB-10469"); !strings.HasPrefix(got, "WEB-10469") {
		t.Errorf("commTaskTitle with nothing but the key = %q, want the KEY as the last fallback (criterion 14)", got)
	}

	// Nothing to say at all: a label and no dangling separator.
	empty := pendingMessage{msg: Message{ID: 1}}
	got := commTaskTitle(empty, noProject, "WEB-10469")
	if got == "" {
		t.Errorf("commTaskTitle returned an empty title; criterion 14: never empty")
	}
	if strings.HasSuffix(strings.TrimSpace(got), ":") {
		t.Errorf("commTaskTitle = %q, a DANGLING separator. ruleTaskTitle's criterion 4: the separator only "+
			"appears when there is something after it", got)
	}
}

// The ONE truncation spelling (SWT-16): textmatch.NormalizedPrefix at
// rulesTitleLen, so a 400-character Slack paragraph cannot become a board title.
func TestCommTaskTitle_TruncatesWithTheOneSpelling(t *testing.T) {
	pm := commBodyPending()
	pm.msg.Subject = strings.Repeat("blocked ", 60)
	got := commTaskTitle(pm, commBodyRule(), "WEB-10469")
	if n := len([]rune(got)); n > rulesTitleLen {
		t.Errorf("commTaskTitle is %d runes, want <= rulesTitleLen (%d) — textmatch.NormalizedPrefix is the ONE "+
			"spelling of whitespace-collapsed, rune-safe truncation (criterion 14)", n, rulesTitleLen)
	}
	pm.msg.Subject = "a  subject\twith   runs\nof whitespace"
	got = commTaskTitle(pm, commBodyRule(), "WEB-10469")
	if strings.Contains(got, "  ") || strings.Contains(got, "\t") || strings.Contains(got, "\n") {
		t.Errorf("commTaskTitle = %q, want whitespace COLLAPSED (NormalizedPrefix, not a manual cut)", got)
	}
}
