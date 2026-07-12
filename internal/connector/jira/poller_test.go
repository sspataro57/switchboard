package jira_test

// Offline poller unit tests (SPEC 09-jira-github-connectors, criterion 3). The
// raw-first upsert DECISION, the JQL scoping/order, the nextPageToken
// pagination, the once-per-run /myself, the comment total>inline paging, and the
// cursor-on-success-only rule live in Ingest (not the sink), so they are
// genuinely exercised here against the httptest fakeJira + an in-memory fake
// Sink. ZERO real network, ZERO Postgres.
//
// GREENFIELD NOTE: package internal/connector/jira does not exist yet; this file
// compile-FAILs under `go test ./...` until it is implemented — the expected
// failure mode. Imposed exported surface (the SPEC's client.go + ingest.go +
// sink.go); for greenfield code the SPEC's contract IS the signature:
//
//   const DefaultOverlap = time.Hour // JQL `updated` has minute granularity
//
//   type Account struct {
//       ID          int64
//       Email       string   // basic-auth user (account_email)
//       SiteBaseURL string   // domain_default, e.g. https://sspataro.atlassian.net
//       Projects    []string // scopes — project keys to poll (mandatory, Decision 2)
//   }
//
//   // Cursor lives in source_accounts.sync_cursor. own_account_id is cached from
//   // /myself once per run (criterion 4).
//   type Cursor struct {
//       JiraUpdatedAt string `json:"jira_updated_at"`
//       OwnAccountID  string `json:"own_account_id"`
//   }
//
//   type Stats struct {
//       IssuesListed, IssuesFetched, CommentsFetched   int
//       RawInserted, RawUpdated, RawUnchanged          int
//       Normalized, SuspectedMerges                    int
//   }
//
//   type Config struct { Full, All bool; Overlap time.Duration; Now time.Time }
//
//   // Sink is the ops-db side of the raw-first phase (upworkcrm/google shape).
//   type Sink interface {
//       Cursor(ctx context.Context, accountID int64) (Cursor, error)
//       SaveCursor(ctx context.Context, accountID int64, c Cursor) error
//       StartRun(ctx context.Context, accountID int64) (runID int64, err error)
//       FinishRun(ctx context.Context, runID int64, status string, stats Stats, errMsg string) error
//       RawHash(ctx context.Context, accountID int64, externalID string) (hash string, exists bool, err error)
//       InsertRaw(ctx context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) error
//       UpdateRaw(ctx context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) error
//   }
//
//   func NewClient(hc *http.Client, baseURL, email, token string) *Client
//   func Ingest(ctx context.Context, c *Client, sink Sink, acct Account, cfg Config) (Stats, error)

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/jira"
)

// ---- fake Sink ----------------------------------------------------------------

type jrawWrite struct {
	externalID string
	hash       string
	raw        json.RawMessage
}

type jiraFakeSink struct {
	accountID int64
	cursor    jira.Cursor
	stored    map[string]string // externalID -> hash

	inserts      []jrawWrite
	updates      []jrawWrite
	savedCursors []jira.Cursor
	runs         []string
	finishStatus string
}

func newJiraFakeSink() *jiraFakeSink {
	return &jiraFakeSink{accountID: 7, stored: map[string]string{}}
}

func (s *jiraFakeSink) Cursor(_ context.Context, _ int64) (jira.Cursor, error) { return s.cursor, nil }

func (s *jiraFakeSink) SaveCursor(_ context.Context, _ int64, c jira.Cursor) error {
	s.savedCursors = append(s.savedCursors, c)
	s.cursor = c
	return nil
}

func (s *jiraFakeSink) StartRun(_ context.Context, _ int64) (int64, error) {
	s.runs = append(s.runs, "start")
	return int64(len(s.runs)), nil
}

func (s *jiraFakeSink) FinishRun(_ context.Context, _ int64, status string, _ jira.Stats, _ string) error {
	s.runs = append(s.runs, "finish:"+status)
	s.finishStatus = status
	return nil
}

func (s *jiraFakeSink) RawHash(_ context.Context, _ int64, externalID string) (string, bool, error) {
	h, ok := s.stored[externalID]
	return h, ok, nil
}

func (s *jiraFakeSink) InsertRaw(_ context.Context, _ int64, externalID string, raw json.RawMessage, hash string) error {
	s.inserts = append(s.inserts, jrawWrite{externalID: externalID, hash: hash, raw: raw})
	s.stored[externalID] = hash
	return nil
}

func (s *jiraFakeSink) UpdateRaw(_ context.Context, _ int64, externalID string, raw json.RawMessage, hash string) error {
	s.updates = append(s.updates, jrawWrite{externalID: externalID, hash: hash, raw: raw})
	s.stored[externalID] = hash
	return nil
}

func jwrote(recs []jrawWrite, id string) *jrawWrite {
	for i := range recs {
		if recs[i].externalID == id {
			return &recs[i]
		}
	}
	return nil
}

func jacct() jira.Account {
	return jira.Account{ID: 7, Email: "sspataro@gmail.com", SiteBaseURL: "https://" + fakeSiteHost, Projects: []string{"CRM"}}
}

// oneIssue seeds a single fully-inlined issue with one comment.
func (f *fakeJira) seedBasic() {
	f.add(fakeIssue{
		key: "CRM-1", updated: "2026-07-02T09:00:00.000+0000", created: "2026-07-01T10:00:00.000+0000",
		summary: "Login broken", description: "500 on staging", reporter: fakeOtherAcc, assignee: fakeOwnAcc,
		comments: []fakeComment{{id: "10001", author: fakeOtherAcc, body: "still failing", created: "2026-07-01T11:00:00.000+0000"}},
	})
}

// ---- JQL scoping + order ------------------------------------------------------

func TestIngest_SearchJQLScopesProjectsAndOrders(t *testing.T) {
	ctx := context.Background()
	fj := newFakeJira()
	defer fj.close()
	fj.seedBasic()

	sink := newJiraFakeSink()
	acct := jacct()
	acct.Projects = []string{"CRM", "OPS"}

	c := jira.NewClient(http.DefaultClient, fj.url(), acct.Email, "tok")
	if _, err := jira.Ingest(ctx, c, sink, acct, jira.Config{Overlap: time.Hour, Now: time.Now()}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(fj.searchJQLs) == 0 {
		t.Fatalf("search/jql was never called")
	}
	jql := fj.searchJQLs[0]
	for _, want := range []string{"project IN", "CRM", "OPS", "ORDER BY updated ASC"} {
		if !strings.Contains(jql, want) {
			t.Errorf("JQL %q missing %q (project scoping + ascending updated order, criterion 3)", jql, want)
		}
	}
	// Fresh cursor: no `updated >=` bound (full initial pull); the once-per-run
	// /myself resolved the own accountId and it is cached in the saved cursor.
	if strings.Contains(jql, "updated >=") {
		t.Errorf("fresh-cursor JQL must not carry an `updated >=` bound; got %q", jql)
	}
	if fj.myselfCalls != 1 {
		t.Errorf("/myself calls = %d, want exactly 1 (fetched once per run, criterion 4)", fj.myselfCalls)
	}
	if got := lastJCursor(t, sink).OwnAccountID; got != fakeOwnAcc {
		t.Errorf("cursor own_account_id = %q, want %q (cached from /myself)", got, fakeOwnAcc)
	}
}

func TestIngest_IncrementalJQLCarriesUpdatedBound(t *testing.T) {
	ctx := context.Background()
	fj := newFakeJira()
	defer fj.close()
	fj.seedBasic()

	sink := newJiraFakeSink()
	sink.cursor = jira.Cursor{JiraUpdatedAt: "2026-07-01T00:00:00.000+0000", OwnAccountID: fakeOwnAcc}

	c := jira.NewClient(http.DefaultClient, fj.url(), jacct().Email, "tok")
	if _, err := jira.Ingest(ctx, c, sink, jacct(), jira.Config{Overlap: time.Hour, Now: time.Now()}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !strings.Contains(fj.searchJQLs[0], "updated >=") {
		t.Errorf("incremental JQL must carry an `updated >=` bound; got %q", fj.searchJQLs[0])
	}
}

// ---- pagination ---------------------------------------------------------------

func TestIngest_PaginationFollowsNextPageToken(t *testing.T) {
	ctx := context.Background()
	fj := newFakeJira()
	defer fj.close()
	fj.searchPageSize = 1
	for i := 1; i <= 3; i++ {
		fj.add(fakeIssue{
			key:      "CRM-" + itoaI(i),
			updated:  "2026-07-0" + itoaI(i) + "T09:00:00.000+0000",
			created:  "2026-07-0" + itoaI(i) + "T08:00:00.000+0000",
			summary:  "issue " + itoaI(i),
			reporter: fakeOtherAcc, assignee: fakeOwnAcc,
		})
	}

	sink := newJiraFakeSink()
	c := jira.NewClient(http.DefaultClient, fj.url(), jacct().Email, "tok")
	stats, err := jira.Ingest(ctx, c, sink, jacct(), jira.Config{Overlap: time.Hour, Now: time.Now()})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(fj.searchJQLs) != 3 {
		t.Errorf("search/jql called %d times, want 3 (page followed twice)", len(fj.searchJQLs))
	}
	if len(fj.issueGets) != 3 {
		t.Errorf("issue GETs = %d, want 3 (every listed hit fetched full)", len(fj.issueGets))
	}
	// 3 issue raw items (no comments on these).
	if stats.RawInserted != 3 {
		t.Errorf("RawInserted = %d, want 3 (all pages fetched, one raw per issue)", stats.RawInserted)
	}
}

// ---- raw-first: per-issue + per-comment items --------------------------------

func TestIngest_RawFirstPerIssueAndComment(t *testing.T) {
	ctx := context.Background()
	fj := newFakeJira()
	defer fj.close()
	fj.seedBasic()

	sink := newJiraFakeSink()
	c := jira.NewClient(http.DefaultClient, fj.url(), jacct().Email, "tok")
	if _, err := jira.Ingest(ctx, c, sink, jacct(), jira.Config{Overlap: time.Hour, Now: time.Now()}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// One item per issue, one per comment (criterion 3 external-id shapes).
	issue := jwrote(sink.inserts, "issue:CRM-1")
	if issue == nil {
		t.Fatalf("issue raw item issue:CRM-1 not inserted; inserts=%+v", sink.inserts)
	}
	if issue.hash == "" {
		t.Errorf("issue raw inserted with empty content_hash (raw-first requires a hash)")
	}
	// The issue raw must NOT carry the comment bodies (Decision 4: minus comments).
	if strings.Contains(string(issue.raw), "still failing") {
		t.Errorf("issue raw still contains a comment body — it must be stored minus the comments array:\n%s", issue.raw)
	}
	if c := jwrote(sink.inserts, "comment:CRM-1:10001"); c == nil {
		t.Errorf("comment raw item comment:CRM-1:10001 not inserted; inserts=%+v", sink.inserts)
	} else if !strings.Contains(string(c.raw), "still failing") {
		t.Errorf("comment raw missing the comment body:\n%s", c.raw)
	}

	// Cursor advanced to the max fields.updated seen, on success only.
	got := lastJCursor(t, sink)
	if got.JiraUpdatedAt != "2026-07-02T09:00:00.000+0000" {
		t.Errorf("cursor jira_updated_at = %q, want the max updated 2026-07-02T09:00:00.000+0000", got.JiraUpdatedAt)
	}
	if sink.finishStatus != "ok" {
		t.Errorf("run status = %q, want ok", sink.finishStatus)
	}
}

// ---- comment paging when total > inline --------------------------------------

func TestIngest_PagesCommentsWhenTotalExceedsInline(t *testing.T) {
	ctx := context.Background()
	fj := newFakeJira()
	defer fj.close()
	fj.add(fakeIssue{
		key: "CRM-9", updated: "2026-07-05T09:00:00.000+0000", created: "2026-07-05T08:00:00.000+0000",
		summary: "chatty", reporter: fakeOtherAcc, assignee: fakeOwnAcc,
		inlineLimit: 1, // issue GET embeds only 1 of 3 -> poller must fetch /comment
		comments: []fakeComment{
			{id: "1", author: fakeOtherAcc, body: "c-one", created: "2026-07-05T08:10:00.000+0000"},
			{id: "2", author: fakeOtherAcc, body: "c-two", created: "2026-07-05T08:20:00.000+0000"},
			{id: "3", author: fakeOwnAcc, body: "c-three", created: "2026-07-05T08:30:00.000+0000"},
		},
	})

	sink := newJiraFakeSink()
	c := jira.NewClient(http.DefaultClient, fj.url(), jacct().Email, "tok")
	if _, err := jira.Ingest(ctx, c, sink, jacct(), jira.Config{Overlap: time.Hour, Now: time.Now()}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(fj.commentGets) == 0 {
		t.Fatalf("issue with comment.total > inline must trigger a /issue/{key}/comment fetch")
	}
	for _, id := range []string{"comment:CRM-9:1", "comment:CRM-9:2", "comment:CRM-9:3"} {
		if jwrote(sink.inserts, id) == nil {
			t.Errorf("comment raw item %q not inserted (all comments must be paged in); inserts=%+v", id, sink.inserts)
		}
	}
}

// ---- error path: cursor not advanced -----------------------------------------

func TestIngest_ErrorLeavesCursorUnadvanced(t *testing.T) {
	ctx := context.Background()
	fj := newFakeJira()
	defer fj.close()
	fj.seedBasic()
	fj.forceIssueStatus["CRM-1"] = http.StatusInternalServerError

	sink := newJiraFakeSink()
	sink.cursor = jira.Cursor{JiraUpdatedAt: "2026-06-01T00:00:00.000+0000", OwnAccountID: fakeOwnAcc}

	c := jira.NewClient(http.DefaultClient, fj.url(), jacct().Email, "tok")
	_, err := jira.Ingest(ctx, c, sink, jacct(), jira.Config{Overlap: time.Hour, Now: time.Now()})
	if err == nil {
		t.Fatalf("Ingest: want a non-nil error when an issue GET returns 500")
	}
	if len(sink.savedCursors) != 0 {
		t.Errorf("cursor advanced on a failed run: %+v", sink.savedCursors)
	}
	if sink.finishStatus != "error" {
		t.Errorf("run status = %q, want error", sink.finishStatus)
	}
}

func lastJCursor(t *testing.T, s *jiraFakeSink) jira.Cursor {
	t.Helper()
	if len(s.savedCursors) == 0 {
		t.Fatalf("cursor never saved on success")
	}
	return s.savedCursors[len(s.savedCursors)-1]
}

func itoaI(i int) string { return string(rune('0' + i)) }
