package jira_test

// Unit tests for the deterministic raw -> canonical normalization (SPEC
// 09-jira-github-connectors, criterion 4; invariant 7 discipline transfer:
// normalize is a PURE function of the raw row — zero network, no LLM). Input is
// a raw_source_items.raw_json row exactly as the poller stored it (issue JSON
// minus the comments array; and each comment JSON separately, criterion 3 /
// Decision 4). Re-normalize-from-raw-alone (criterion 4) requires this purity.
//
// GREENFIELD NOTE: package internal/connector/jira does not exist yet; this file
// (and the whole jira_test package) compile-FAILs under `go test ./...` until it
// is implemented — the expected failure mode. For greenfield code the SPEC's
// contract IS the signature. Imposed exported surface (the SPEC's normalize.go):
//
//   const ( Provider = "jira"; Channel = "jira" )
//
//   type NormalizedThread struct {
//       ThreadKey    string   // jira:{site_host}:{KEY}
//       Subject      string   // issue summary
//       Participants []string // reporter/assignee accountIds
//   }
//
//   type NormalizedMessage struct {
//       ThreadKey         string    // jira:{site_host}:{KEY}
//       ExternalMessageID string    // jira:{site_host}:issue:{KEY} | jira:{site_host}:comment:{id}
//       Direction         string    // inbound|outbound
//       SentAt            time.Time
//       Subject           string
//       Sender            string
//       BodyText          string
//       Channel           string    // "jira"
//       AuthorAccountID   string    // for people/person_identities (provider='jira')
//   }
//
//   // Pure mappers over one raw row. Direction is outbound iff the author's
//   // accountId equals the polling account's own accountId (criterion 4).
//   func NormalizeIssue(raw json.RawMessage, siteHost, ownAccountID string) (NormalizedThread, NormalizedMessage, error)
//   // issueKey comes from the raw item external_id (comment:{KEY}:{id}), passed in.
//   func NormalizeComment(raw json.RawMessage, siteHost, issueKey, ownAccountID string) (NormalizedMessage, error)

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/jira"
)

// jiraTime parses a Jira v2 timestamp the same way the normalizer must.
func jiraTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse("2006-01-02T15:04:05.000-0700", s)
	if err != nil {
		t.Fatalf("parse jira time %q: %v", s, err)
	}
	return ts.UTC()
}

const (
	nrmKey       = "CRM-1"
	nrmCreated   = "2026-07-01T10:00:00.000+0000"
	nrmCommentID = "10001"
	nrmCommentAt = "2026-07-01T11:30:00.000+0000"
)

func issueRaw(reporter string) json.RawMessage {
	// Exactly what the poller stores for an issue item: issue JSON WITHOUT the
	// comments array (Decision 4). fields.comment may keep a count but no bodies.
	return json.RawMessage(`{
		"key": "` + nrmKey + `",
		"fields": {
			"summary": "Login is broken",
			"description": "The staging login page returns 500.",
			"created": "` + nrmCreated + `",
			"updated": "2026-07-02T09:00:00.000+0000",
			"reporter": {"accountId": "` + reporter + `", "displayName": "Reporter Person"},
			"assignee": {"accountId": "` + fakeOwnAcc + `", "displayName": "Bot"},
			"comment": {"total": 1, "startAt": 0, "maxResults": 0, "comments": []}
		}
	}`)
}

func commentRaw(author string) json.RawMessage {
	return json.RawMessage(`{
		"id": "` + nrmCommentID + `",
		"author": {"accountId": "` + author + `", "displayName": "Commenter"},
		"body": "pushed a fix, please retest",
		"created": "` + nrmCommentAt + `",
		"updated": "` + nrmCommentAt + `"
	}`)
}

// Issue -> thread + description message. Keys pinned by the SPEC:
// thread_key jira:{host}:{KEY}; description external_message_id
// jira:{host}:issue:{KEY}; channel 'jira'; sent_at = created; sender = reporter.
func TestNormalizeIssue_ThreadAndDescription(t *testing.T) {
	th, msg, err := jira.NormalizeIssue(issueRaw(fakeOtherAcc), fakeSiteHost, fakeOwnAcc)
	if err != nil {
		t.Fatalf("NormalizeIssue: %v", err)
	}

	wantThreadKey := "jira:" + fakeSiteHost + ":" + nrmKey
	if th.ThreadKey != wantThreadKey {
		t.Errorf("thread.ThreadKey = %q, want %q", th.ThreadKey, wantThreadKey)
	}
	if th.Subject != "Login is broken" {
		t.Errorf("thread.Subject = %q, want the issue summary", th.Subject)
	}
	if msg.ThreadKey != wantThreadKey {
		t.Errorf("msg.ThreadKey = %q, want %q", msg.ThreadKey, wantThreadKey)
	}
	if want := "jira:" + fakeSiteHost + ":issue:" + nrmKey; msg.ExternalMessageID != want {
		t.Errorf("description external_message_id = %q, want %q", msg.ExternalMessageID, want)
	}
	if msg.Channel != jira.Channel || jira.Channel != "jira" {
		t.Errorf("channel = %q, want jira", msg.Channel)
	}
	if !msg.SentAt.Equal(jiraTime(t, nrmCreated)) {
		t.Errorf("description SentAt = %v, want issue created %v", msg.SentAt, jiraTime(t, nrmCreated))
	}
	if msg.BodyText != "The staging login page returns 500." {
		t.Errorf("description body = %q, want the issue description", msg.BodyText)
	}
	if msg.AuthorAccountID != fakeOtherAcc {
		t.Errorf("description AuthorAccountID = %q, want the reporter %q", msg.AuthorAccountID, fakeOtherAcc)
	}
	if msg.Sender == "" {
		t.Errorf("description Sender must be non-empty (reporter)")
	}
}

// Direction (criterion 4): outbound iff the author's accountId is the polling
// account's own accountId; else inbound. Proven on both the description
// (reporter-authored) and a comment.
func TestNormalizeIssue_DirectionByOwnAccount(t *testing.T) {
	// Reporter is us -> outbound.
	_, own, err := jira.NormalizeIssue(issueRaw(fakeOwnAcc), fakeSiteHost, fakeOwnAcc)
	if err != nil {
		t.Fatalf("NormalizeIssue(own): %v", err)
	}
	if own.Direction != "outbound" {
		t.Errorf("own-reported issue direction = %q, want outbound", own.Direction)
	}
	// Reporter is a client -> inbound.
	_, ext, err := jira.NormalizeIssue(issueRaw(fakeOtherAcc), fakeSiteHost, fakeOwnAcc)
	if err != nil {
		t.Fatalf("NormalizeIssue(other): %v", err)
	}
	if ext.Direction != "inbound" {
		t.Errorf("client-reported issue direction = %q, want inbound", ext.Direction)
	}
}

// Comment -> message. external_message_id jira:{host}:comment:{id} (the exact
// form the send adapter uses for sent_external_id — loop closure is straight
// equality, criterion 7/8); thread_key jira:{host}:{KEY} from the passed issueKey.
func TestNormalizeComment_MessageFields(t *testing.T) {
	msg, err := jira.NormalizeComment(commentRaw(fakeOtherAcc), fakeSiteHost, nrmKey, fakeOwnAcc)
	if err != nil {
		t.Fatalf("NormalizeComment: %v", err)
	}
	if want := "jira:" + fakeSiteHost + ":comment:" + nrmCommentID; msg.ExternalMessageID != want {
		t.Errorf("comment external_message_id = %q, want %q", msg.ExternalMessageID, want)
	}
	if want := "jira:" + fakeSiteHost + ":" + nrmKey; msg.ThreadKey != want {
		t.Errorf("comment thread_key = %q, want %q", msg.ThreadKey, want)
	}
	if msg.Direction != "inbound" {
		t.Errorf("client comment direction = %q, want inbound", msg.Direction)
	}
	if !msg.SentAt.Equal(jiraTime(t, nrmCommentAt)) {
		t.Errorf("comment SentAt = %v, want %v", msg.SentAt, jiraTime(t, nrmCommentAt))
	}
	if msg.BodyText != "pushed a fix, please retest" {
		t.Errorf("comment body = %q, want the comment body", msg.BodyText)
	}
	if msg.Channel != "jira" {
		t.Errorf("comment channel = %q, want jira", msg.Channel)
	}
	if msg.AuthorAccountID != fakeOtherAcc {
		t.Errorf("comment AuthorAccountID = %q, want %q", msg.AuthorAccountID, fakeOtherAcc)
	}
}

// Our own comment (author == own accountId) is outbound — invisible to triage's
// inbound-only filter, never re-triaged (invariant 5).
func TestNormalizeComment_OwnCommentOutbound(t *testing.T) {
	msg, err := jira.NormalizeComment(commentRaw(fakeOwnAcc), fakeSiteHost, nrmKey, fakeOwnAcc)
	if err != nil {
		t.Fatalf("NormalizeComment(own): %v", err)
	}
	if msg.Direction != "outbound" {
		t.Errorf("own comment direction = %q, want outbound (loop-closure invisibility)", msg.Direction)
	}
}

// Determinism (invariant 7): the same raw row normalizes identically on repeat.
func TestNormalizeComment_Deterministic(t *testing.T) {
	a, err := jira.NormalizeComment(commentRaw(fakeOtherAcc), fakeSiteHost, nrmKey, fakeOwnAcc)
	if err != nil {
		t.Fatalf("NormalizeComment (a): %v", err)
	}
	b, err := jira.NormalizeComment(commentRaw(fakeOtherAcc), fakeSiteHost, nrmKey, fakeOwnAcc)
	if err != nil {
		t.Fatalf("NormalizeComment (b): %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("normalization not deterministic:\n a=%+v\n b=%+v", a, b)
	}
}
