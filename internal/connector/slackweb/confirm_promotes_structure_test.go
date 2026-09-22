package slackweb

// slack-send-queue (SWT-76) criterion 27, the half no fixture can reach: the
// SHAPE of confirmDelivery's promotion. Two of SWT-71's four rules are
// invisible to a passing integration test —
//
//   - a `FOR SHARE OF t` added to the task read would still pass every
//     assertion in confirm_promotes_integration_test.go and would only ever bite
//     as a production deadlock, under concurrency, at 3 a.m.;
//   - a promotion split back into two pool.Exec calls is only visible when a
//     process dies between them.
//
// So they are pinned structurally, by parsing sink.go. ZERO I/O beyond reading
// one file.
//
// GREENFIELD NOTE: no new symbol is needed — this file compiles today and FAILS
// today, because confirmDelivery promotes with two unfenced s.pool.Exec calls
// (sink.go:465, :482), reads no `status` into its candidate list, and carries no
// lock-order comment. Expected red.
//
// THE LOCK-ORDER ARGUMENT, which is why the task status must be read WITHOUT a
// row lock (delivery.go:705-711, verbatim in its own words):
//
//	FOR SHARE, not FOR UPDATE (go-reviewer): SHARE still conflicts with
//	closeTransition's FOR UPDATE, so close-vs-send is serialised in both
//	orders, but it does NOT conflict with the FOR KEY SHARE a task_events
//	insert takes on its parent task. mark_delivery_sent/failed, the gmail
//	loop-closure sink and the Upwork reconciler lock a delivery THEN insert a
//	task event (delivery -> task); a FOR UPDATE here (task -> delivery) would
//	close a deadlock cycle with them.
//
// refuseClosedTask is the ONE place that takes task -> delivery, and it takes
// FOR SHARE precisely so the cycle stays OPEN. confirmDelivery is on the other
// side: delivery first, then the task, and the task status is read plainly.
// Adding a row lock there closes the cycle.
//
// MUTATIONS:
//	Split the promotion and its events into two pool.Exec calls -> TestConfirmDelivery_PromotesInOneTransaction
//	Add `FOR SHARE OF t` to the promotion's task read           -> TestConfirmDelivery_TakesNoRowLockOnTasks

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// confirmDeliverySource returns the body of confirmDelivery, from its
// declaration to the next top-level func.
func confirmDeliverySource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("sink.go")
	if err != nil {
		t.Fatalf("read sink.go: %v", err)
	}
	src := string(b)
	start := strings.Index(src, "func (s *PGSink) confirmDelivery(")
	if start < 0 {
		t.Fatalf("confirmDelivery is gone from sink.go; REWRITE this guard, never delete it")
	}
	rest := src[start+10:]
	end := strings.Index(rest, "\nfunc ")
	if end < 0 {
		return src[start:]
	}
	return src[start : start+10+end]
}

// SWT-71 rule 2: the promotion AND its events commit together. Today they are
// two independent s.pool.Exec calls; a crash between them loses the event, and
// with a queued send that event is the ONLY thing that moves the task.
func TestConfirmDelivery_PromotesInOneTransaction(t *testing.T) {
	body := confirmDeliverySource(t)

	begin := strings.Index(body, "s.pool.Begin(")
	if begin < 0 {
		t.Errorf("confirmDelivery never opens a transaction (s.pool.Begin). The promotion UPDATE and the " +
			"task_events INSERT(s) must commit together — a `sent` row with no delivery_sent event strands its " +
			"task at done_locally and there is no verb left to fix it (criterion 27). The sibling shape is " +
			"google/sink.go:588 and jira/sink.go:318.")
	} else if n := len(regexp.MustCompile(`s\.pool\.Exec\(`).FindAllString(body[begin:], -1)); n > 0 {
		// Once a tx is open the writes must go THROUGH it: a stray s.pool.Exec
		// after Begin is outside the transaction and reintroduces the split.
		t.Errorf("confirmDelivery still makes %d s.pool.Exec call(s) after opening a transaction; the "+
			"promotion and its events must run on the tx handle", n)
	}
	if !regexp.MustCompile(`INSERT INTO task_events`).MatchString(body) {
		t.Errorf("confirmDelivery writes no task_events row at all")
	}
}

// SWT-71 rule 1: validate the value that LANDS. The candidate SELECT reads
// `status`, and the promotion UPDATE is guarded on THAT value — never on a
// branch that can disagree with the row it writes. Without the guard a row that
// moved from `sending` to `sent` between the read and the write would still be
// handed the `sending` branch's delivery_sent.
func TestConfirmDelivery_ReadsStatusAndGuardsTheUpdateOnIt(t *testing.T) {
	body := confirmDeliverySource(t)

	if !regexp.MustCompile(`(?s)SELECT\s+id,\s*task_id,[^\x60]*status`).MatchString(body) &&
		!regexp.MustCompile(`(?s)SELECT[^\x60]*\bstatus\b[^\x60]*FROM deliveries`).MatchString(body) {
		t.Errorf("confirmDelivery's candidate SELECT does not read `status` alongside id/task_id/body. The "+
			"events branch on whether the row WAS `sending` or already `sent`, so the value must come from the "+
			"same read the decision is made on (criterion 27 / SWT-71 rule 1).\n\ngot:\n%s", body)
	}

	update := regexp.MustCompile(`(?s)UPDATE deliveries.*?\x60`).FindString(body)
	if update == "" {
		t.Fatalf("confirmDelivery has no UPDATE deliveries statement:\n%s", body)
	}
	if !regexp.MustCompile(`AND status\s*=\s*\$\d`).MatchString(update) {
		t.Errorf("the promotion UPDATE is not guarded `AND status=$n` with the value the SELECT read. Both "+
			"existing guards (sent_external_id IS NULL, confirmed_at IS NULL) say nothing about WHICH status "+
			"the row had, and that is exactly what decides whether delivery_sent is emitted.\n\ngot:\n%s", update)
	}
}

// SWT-71 rule 3: lock order delivery -> task, and the task status is read
// WITHOUT a row lock. See the argument quoted in this file's header.
func TestConfirmDelivery_TakesNoRowLockOnTasks(t *testing.T) {
	body := confirmDeliverySource(t)

	for _, banned := range []struct{ re, why string }{
		{`FOR SHARE OF t\b`, "a FOR SHARE on the task closes the deadlock cycle refuseClosedTask deliberately " +
			"leaves open: everything else here locks delivery THEN inserts a task_events row (which takes FOR " +
			"KEY SHARE on the parent task)"},
		{`FROM tasks[^\x60]*FOR UPDATE`, "a FOR UPDATE on the task is the same cycle, harder"},
		{`FOR UPDATE OF t\b`, "same"},
		{`FOR NO KEY UPDATE OF t\b`, "same"},
	} {
		if regexp.MustCompile(banned.re).MatchString(body) {
			t.Errorf("confirmDelivery matches /%s/ — %s (criterion 27; the argument is at "+
				"internal/tools/delivery.go:705-711)", banned.re, banned.why)
		}
	}

	// And the reasoning must be written down where the next person will change
	// it. A silent absence of a lock is indistinguishable from an oversight.
	lower := strings.ToLower(body)
	if !strings.Contains(lower, "lock order") && !strings.Contains(lower, "deadlock") {
		t.Errorf("confirmDelivery's promotion carries no comment naming the lock order / deadlock argument. " +
			"The NEXT reader's instinct is to lock the task row before reading its status; the comment at " +
			"delivery.go:705-711 is what stops them, and it has to be reachable from here (criterion 27)")
	}
}
