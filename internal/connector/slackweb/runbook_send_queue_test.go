package slackweb_test

// slack-send-queue (SWT-76) Part 7, criterion 33: the runbook gains a
// "What happens when the browser is busy" section.
//
// A test on prose earns its place the way google/runbook_microsoft_test.go and
// mcpserver/runbook_test.go do. Here it earns it twice over, because the
// feature's ONLY visible failure mode is a row that sits in `sending` looking
// broken while it is in fact healthy. Without the section, the next person to
// meet a queued row — Salvador at 7 a.m., or a session six months from now —
// reaches for the one action D6 forbids: fail it and re-approve, which
// double-posts into a client conversation if the click did land.
//
// EXPECTED FAILURE MODE: **assertion failures** — docs/runbooks/slack-web-connector.md
// exists (its last section is "Tests") and says nothing about 202, a queue, or
// what a queued row looks like, so every token below is missing today.
//
// Scope, deliberately: the six things criterion 33 enumerates and nothing else.
// A doc test that pins every sentence makes the doc unmaintainable.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestRunbook_DocumentsTheBusyBrowserOutcomes(t *testing.T) {
	const rel = "docs/runbooks/slack-web-connector.md"
	b, err := os.ReadFile(filepath.Join("..", "..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	doc := string(b)
	lower := strings.ToLower(doc)

	if !regexp.MustCompile(`(?i)^#+ .*browser is busy`).MatchString(doc) &&
		!strings.Contains(lower, "what happens when the browser is busy") {
		t.Errorf("%s has no \"What happens when the browser is busy\" section (criterion 33)", rel)
	}

	for _, want := range []struct{ tok, why string }{
		{"202", "the middle outcome: the leaf ACCEPTED the send and will click it in the next gap"},
		{"503", "the outcome that is still a definite refusal — queue full, or an estimate over the bound"},
		{"max_queue_ms", "the request field whose PRESENCE is the version gate (D12) and whose value is the " +
			"caller's lease-derived bound (D3)"},
		{"send_queued_at", "what a queued row looks like in SQL — the column a human greps for"},
		{"send_queue_job_id", "the job id that ties the row to a line in the mini's log"},
		{"queued on the bridge", "what a queued row looks like in the dashboard"},
		{"mark_delivery_sent", "the verb a human uses once they have LOOKED in Slack"},
		{"mark_delivery_failed", "the verb the lease refuses for 15 minutes, and why"},
		{"15 min", "the first horizon: the lease"},
		{"ReconcileUnconfirmed", "the second horizon: three rotation passes that read the conversation"},
	} {
		if !strings.Contains(doc, want.tok) && !strings.Contains(lower, strings.ToLower(want.tok)) {
			t.Errorf("%s never mentions %q — %s (criterion 33)", rel, want.tok, want.why)
		}
	}

	// The two facts that cost the most if they are absent, stated as rules.
	if !strings.Contains(lower, "never resend") && !strings.Contains(lower, "nothing ever resends") {
		t.Errorf("%s does not state the explicit rule that NOTHING ever resends. D6 refuses an automatic "+
			"timeout-to-`failed` precisely because `failed` is re-approvable (delivery.go:1102) and a resend "+
			"of a click that DID land is a double post into a client conversation (criterion 33)", rel)
	}
	if !strings.Contains(lower, "restart") {
		t.Errorf("%s does not say what a bridge restart means for a queued send. The leaf's queue is in memory "+
			"and dies with the process (D9) — a lost job becomes an unconfirmed delivery a human resolves, and "+
			"nothing replays it (criterion 33)", rel)
	}
}
