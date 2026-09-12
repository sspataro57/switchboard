package tools

// SWT-44 review fixes (user-profile-drafts): the pure halves of the
// content-bound approval, the user profile's gmail-only pin and the
// empty-body refusal. ZERO network, ZERO Postgres — the validators are called
// directly (package tools), so an accepted case never reaches a handler that
// would dereference a nil pool.
//
// MUTATIONS THAT MUST TURN THIS FILE RED:
//   - drop the NUL separator from DeliveryContentHash → the boundary row.
//   - drop the require_channel check from validateDraftDelivery → every
//     refused-channel row.
//   - accept a whitespace-only body in validateUpdateDelivery → the empty-body rows.

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestDeliveryContentHash_IsSHA256OfSubjectNULBody(t *testing.T) {
	sum := sha256.Sum256([]byte("Re: invoice\x00Thanks, attached."))
	if got, want := DeliveryContentHash("Re: invoice", "Thanks, attached."), hex.EncodeToString(sum[:]); got != want {
		t.Errorf("DeliveryContentHash = %s, want sha256(subject + NUL + body) = %s", got, want)
	}
	// The separator is the point: without it, moving words across the
	// subject/body boundary would keep the hash and pass a changed draft.
	if DeliveryContentHash("ab", "c") == DeliveryContentHash("a", "bc") {
		t.Error("DeliveryContentHash(ab, c) == DeliveryContentHash(a, bc): subject and body must be separated")
	}
}

func TestValidateApproveDelivery_ContentHashShape(t *testing.T) {
	good := DeliveryContentHash("s", "b")
	for _, tc := range []struct {
		args string
		ok   bool
	}{
		{`{"delivery_id":7}`, true}, // omitted = today's behaviour (opsctl, full-profile MCP)
		{`{"delivery_id":7,"expect_content_hash":"` + good + `"}`, true},
		{`{"delivery_id":7,"expect_content_hash":"abc"}`, false},
		{`{"delivery_id":7,"expect_content_hash":"` + strings.ToUpper(good) + `"}`, false},
		{`{}`, false},
	} {
		err := validateApproveDelivery([]byte(tc.args))
		if tc.ok && err != nil {
			t.Errorf("validateApproveDelivery(%s) = %v, want nil", tc.args, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("validateApproveDelivery(%s) accepted it; a malformed hash would otherwise refuse as "+
				"\"changed since it was shown\", which is not what happened", tc.args)
		}
	}
}

// The user profile pins require_channel:"gmail" (SWT-44 fix 3). The validator
// REFUSES a differing channel; it never rewrites one.
func TestValidateDraftDelivery_RequireChannelRefusesOtherChannels(t *testing.T) {
	const slackTarget = "https://app.slack.com/client/TITEST/CITEST/p1750000000000000"
	refused := []string{
		`{"task_id":1,"channel":"slack_reply","body":"b","target_ref":"` + slackTarget + `","require_channel":"gmail"}`,
		`{"task_id":1,"channel":"calendar","body":"b","subject":"s","target_ref":"a@example.com",` +
			`"start":"2030-01-01T10:00:00Z","end":"2030-01-01T10:15:00Z","require_channel":"gmail"}`,
		`{"task_id":1,"channel":"jira_comment","body":"b","target_ref":"jira:x.atlassian.net:SWT-1","require_channel":"gmail"}`,
		`{"task_id":1,"channel":"upwork_chat","body":"b","target_ref":"upwork_crm:c1:upwork","require_channel":"gmail"}`,
	}
	for _, args := range refused {
		err := validateDraftDelivery([]byte(args))
		if err == nil {
			t.Errorf("validateDraftDelivery accepted %s under require_channel gmail", args)
			continue
		}
		if !strings.Contains(err.Error(), "gmail") {
			t.Errorf("refusal %q does not name the allowed channel (gmail); the model corrects itself from the message", err)
		}
	}
	if err := validateDraftDelivery([]byte(
		`{"task_id":1,"channel":"gmail","body":"b","thread_id":5,"require_channel":"gmail"}`)); err != nil {
		t.Errorf("a gmail draft under require_channel gmail was refused: %v", err)
	}
	// No pin (the full profile, the drafts worker): every channel as before.
	if err := validateDraftDelivery([]byte(
		`{"task_id":1,"channel":"slack_reply","body":"b","target_ref":"` + slackTarget + `"}`)); err != nil {
		t.Errorf("a slack_reply draft with no require_channel was refused: %v", err)
	}
}

// SWT-44 fix 5: a present body must say something. subject "" stays legal —
// it clears the subject, as before.
func TestValidateUpdateDelivery_RefusesEmptyBody(t *testing.T) {
	for _, args := range []string{
		`{"delivery_id":7,"body":""}`,
		`{"delivery_id":7,"body":"   \n\t "}`,
		`{"delivery_id":7,"subject":"s","body":" "}`,
		`{"delivery_id":7}`,
	} {
		if err := validateUpdateDelivery([]byte(args)); err == nil {
			t.Errorf("validateUpdateDelivery accepted %s", args)
		}
	}
	for _, args := range []string{
		`{"delivery_id":7,"subject":""}`,
		`{"delivery_id":7,"body":"fixed words"}`,
		`{"delivery_id":7,"subject":"Re: x","body":"y","require_own_draft":"true"}`,
	} {
		if err := validateUpdateDelivery([]byte(args)); err != nil {
			t.Errorf("validateUpdateDelivery(%s) = %v, want nil", args, err)
		}
	}
}
