//go:build integration

package tools_test

// Blast radius of the SWT-61 fix (bug gmail-reply-empty-subject-off-thread):
// the new subject rule is GMAIL ONLY, and it cannot be undone after the draft.
//
//  1. update_delivery must not be able to blank a gmail subject (RED today:
//     validateUpdateDelivery says out loud that `subject: ""` "stays legal: it
//     clears the subject, as it always has", and the UPDATE stores NULLIF($3,'')
//     — so a session could re-open exactly the hole the draft-time fill closes,
//     on a drafted row that is one click from approval).
//  2. Other channels are untouched (GREEN today, must stay green). jira_comment
//     and slack_reply never read subject on their send paths (delivery.go:1557,
//     1651) and upwork_chat has no direct send path at all; a subject rule there
//     would be ceremony, untested against any real send. A fix that refuses
//     their subject-less drafts is too wide.
//
// Reuses the SWT-8 lifecycle fixture (seedDeliveryFixture / cleanupDeliveryData
// / draftGmail / deliveryExecutor, delivery_lifecycle_integration_test.go).
// NOTHING IS SENT: these tests stop at draft/edit. Every string is a placeholder.
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_gmailsubject?sslmode=disable' \
//	  go test -tags integration -count=1 -run TestRegression_SWT61 -v ./internal/tools/

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
)

const (
	gsubchSlackTarget = "https://app.slack.com/client/TITEST/CITEST/p1750000000000000"
	gsubchJiraSite    = "placeholder.atlassian.net"
	gsubchJiraTarget  = "jira:" + gsubchJiraSite + ":PLACEHOLDER-1"
	gsubchJiraAcct    = "itest-swt61-jira@example.com"
)

// seedJiraAccount gives draft_delivery's jira_comment branch the account it
// resolves From from (target_ref's site_host must match a provider='jira'
// account's domain_default — never caller-chosen). Nothing is ever sent through
// it: these tests stop at draft. Returns a cleanup to run AFTER the deliveries
// that reference it are gone.
func seedJiraAccount(t *testing.T, ctx context.Context, pool *pgxpool.Pool) func() {
	t.Helper()
	drop := func() {
		if _, err := pool.Exec(ctx, `DELETE FROM source_accounts WHERE account_email=$1`, gsubchJiraAcct); err != nil {
			t.Fatalf("cleanup jira account: %v", err)
		}
	}
	drop() // leftovers from a prior run first: the suite is rerunnable
	if _, err := pool.Exec(ctx,
		`INSERT INTO source_accounts (provider, account_email, domain_default, send_enabled)
		 VALUES ('jira', $1, $2, false)`, gsubchJiraAcct, "https://"+gsubchJiraSite); err != nil {
		t.Fatalf("seed jira account: %v", err)
	}
	return drop
}

// ---- 1. a gmail subject cannot be blanked after the draft ---------------------

func TestRegression_SWT61_UpdateDeliveryCannotBlankAGmailSubject(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()
	cleanupDeliveryData(t, ctx, pool)
	defer cleanupDeliveryData(t, ctx, pool)

	fx := seedDeliveryFixture(t, ctx, pool)
	ex := deliveryExecutor(pool)

	read := func(id int64) *string {
		t.Helper()
		var s *string
		if err := pool.QueryRow(ctx, `SELECT subject FROM deliveries WHERE id=$1`, id).Scan(&s); err != nil {
			t.Fatalf("read delivery %d subject: %v", id, err)
		}
		return s
	}

	for _, tc := range []struct{ name, subject string }{
		{"empty", `""`},
		{"blank", `"   "`},
		// Pins the ORDER of the check against the scrub: updateDelivery reads
		// the post-scrub subject, and ScrubAIAttribution drops any line
		// carrying an attribution marker (a subject is one line). Move the
		// check above the scrub assignment and this edit blanks the subject
		// again with the rest of the suite still green — the draft path's
		// equivalent hole, on the edit path.
		{"attribution only", `"generated with the placeholder tool"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := draftGmail(t, ctx, ex, fx.parentID, fx.threadID)
			before := read(id)
			_, err := ex.Execute(ctx, executor.Call{Tool: "update_delivery", Actor: delActor,
				Args: []byte(`{"delivery_id":` + itoa(id) + `,"subject":` + tc.subject + `}`)})
			if err == nil {
				t.Errorf("update_delivery blanked a gmail draft's subject (%s). SWT-61: the draft-time fill makes a "+
					"subject-less gmail row unrepresentable; an edit that clears it re-opens the hole on a row one "+
					"click from approval", tc.name)
			} else if !strings.Contains(strings.ToLower(err.Error()), "subject") {
				t.Errorf("refusal = %q, want it to name `subject`", err)
			}
			if got, want := gsubShow(read(id)), gsubShow(before); got != want {
				t.Errorf("gmail draft subject = %s after a refused edit, want it untouched (%s)", got, want)
			}
		})
	}

	// Control: a real edit still works — the rule is "never blank", not "never edit".
	t.Run("a real edit still works", func(t *testing.T) {
		id := draftGmail(t, ctx, ex, fx.parentID, fx.threadID)
		const edited = "Re: placeholder edited subject"
		callOK(t, ctx, ex, delActor, "update_delivery", `{"delivery_id":`+itoa(id)+`,"subject":"`+edited+`"}`)
		if got := read(id); got == nil || *got != edited {
			t.Errorf("subject = %s after an edit, want %q", gsubShow(got), edited)
		}
	})
}

// ---- 2. other channels are unaffected -----------------------------------------

// GREEN today and after the fix: the channels whose send paths never read
// subject keep accepting a subject-less draft.
func TestRegression_SWT61_OtherChannelsStillDraftWithoutASubject(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()

	// Registered BEFORE the cleanup defer so it runs AFTER it (LIFO): the
	// deliveries referencing this account must be gone first.
	defer seedJiraAccount(t, ctx, pool)()
	cleanupDeliveryData(t, ctx, pool)
	defer cleanupDeliveryData(t, ctx, pool)

	fx := seedDeliveryFixture(t, ctx, pool)
	ex := deliveryExecutor(pool)

	for _, tc := range []struct{ name, args string }{
		{"jira_comment", `{"task_id":` + itoa(fx.parentID) + `,"channel":"jira_comment","target_ref":"` + gsubchJiraTarget + `","body":"placeholder comment"}`},
		{"slack_reply", `{"task_id":` + itoa(fx.parentID) + `,"channel":"slack_reply","target_ref":"` + gsubchSlackTarget + `","body":"placeholder message"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ex.Execute(ctx, executor.Call{Tool: "draft_delivery", Actor: delActor, Args: []byte(tc.args)})
			if err != nil {
				t.Fatalf("draft_delivery(%s) without a subject was refused: %v — SWT-61 is gmail only. %s's send "+
					"path never selects subject; a rule there is ceremony, untested against any real send", tc.name, err, tc.name)
			}
			var d struct {
				DeliveryID int64 `json:"delivery_id"`
			}
			mustUnmarshal(t, out.Output, &d)
			var s *string
			if err := pool.QueryRow(ctx, `SELECT subject FROM deliveries WHERE id=$1`, d.DeliveryID).Scan(&s); err != nil {
				t.Fatalf("read subject: %v", err)
			}
			if s != nil {
				t.Errorf("%s draft subject = %q, want NULL: nothing fills a subject on a channel that never sends one", tc.name, *s)
			}
		})
	}

	// ...and clearing a subject stays legal off gmail (the new refusal is
	// channel-scoped, like update_delivery's require_channel pin).
	t.Run("blanking a slack_reply subject stays legal", func(t *testing.T) {
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO deliveries (task_id, channel, target_ref, body, subject, status, created_by)
			 VALUES ($1, 'slack_reply', $2, 'placeholder message', 'placeholder subject', 'drafted', $3)
			 RETURNING id`, fx.parentID, gsubchSlackTarget, delActor).Scan(&id); err != nil {
			t.Fatalf("seed slack draft: %v", err)
		}
		callOK(t, ctx, ex, delActor, "update_delivery", `{"delivery_id":`+itoa(id)+`,"subject":""}`)
		var s *string
		if err := pool.QueryRow(ctx, `SELECT subject FROM deliveries WHERE id=$1`, id).Scan(&s); err != nil {
			t.Fatalf("read subject: %v", err)
		}
		if s != nil {
			t.Errorf("slack_reply subject = %q after a clear, want NULL: SWT-61's rule is gmail-scoped", *s)
		}
	})
}
