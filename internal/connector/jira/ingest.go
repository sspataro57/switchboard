package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/chash"
)

// DefaultOverlap: JQL `updated` has minute granularity, one hour is generous.
const DefaultOverlap = time.Hour

// jiraTimeLayout is Jira's REST timestamp format.
const jiraTimeLayout = "2006-01-02T15:04:05.000-0700"

// Account is one provider='jira' source_accounts row.
type Account struct {
	ID          int64
	Email       string   // basic-auth user (account_email)
	SiteBaseURL string   // domain_default
	Projects    []string // scopes — mandatory project scoping
}

// Cursor lives in source_accounts.sync_cursor.
type Cursor struct {
	JiraUpdatedAt string `json:"jira_updated_at"`
	OwnAccountID  string `json:"own_account_id"`
}

type Stats struct {
	IssuesListed    int `json:"issues_listed"`
	IssuesFetched   int `json:"issues_fetched"`
	CommentsFetched int `json:"comments_fetched"`
	RawInserted     int `json:"raw_inserted"`
	RawUpdated      int `json:"raw_updated"`
	RawUnchanged    int `json:"raw_unchanged"`
	Normalized      int `json:"normalized"`
	SuspectedMerges int `json:"suspected_merges"`
}

type Config struct {
	Full    bool
	All     bool
	Overlap time.Duration
	Now     time.Time
}

func (c Config) now() time.Time {
	if c.Now.IsZero() {
		return time.Now()
	}
	return c.Now
}

// Sink is the ops-db side of the raw-first phase (upworkcrm/google shape).
type Sink interface {
	Cursor(ctx context.Context, accountID int64) (Cursor, error)
	SaveCursor(ctx context.Context, accountID int64, c Cursor) error
	StartRun(ctx context.Context, accountID int64) (runID int64, err error)
	FinishRun(ctx context.Context, runID int64, status string, stats Stats, errMsg string) error
	RawHash(ctx context.Context, accountID int64, externalID string) (hash string, exists bool, err error)
	InsertRaw(ctx context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) error
	UpdateRaw(ctx context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) error
}

// Ingest is the raw-first phase for one account: search updated issues in the
// scoped projects, store each issue (minus its comments array — comments are
// their own raw items) and each comment.
func Ingest(ctx context.Context, c *Client, sink Sink, acct Account, cfg Config) (Stats, error) {
	var stats Stats
	runID, err := sink.StartRun(ctx, acct.ID)
	if err != nil {
		return stats, fmt.Errorf("start jira run: %w", err)
	}
	fail := func(cause error) (Stats, error) {
		_ = sink.FinishRun(ctx, runID, "error", stats, cause.Error())
		return stats, cause
	}

	if len(acct.Projects) == 0 {
		return fail(fmt.Errorf("jira account %s has no project scoping (scopes) — an unscoped poll is refused", acct.Email))
	}

	cur, err := sink.Cursor(ctx, acct.ID)
	if err != nil {
		return fail(fmt.Errorf("read cursor: %w", err))
	}

	// /myself once per run, cached in the cursor.
	own, err := c.Myself(ctx)
	if err != nil {
		return fail(fmt.Errorf("fetch own accountId: %w", err))
	}
	cur.OwnAccountID = own

	jql := fmt.Sprintf("project IN (%s)", quoteList(acct.Projects))
	if !cfg.Full && cur.JiraUpdatedAt != "" {
		overlap := cfg.Overlap
		if overlap == 0 {
			overlap = DefaultOverlap
		}
		if ts, err := time.Parse(jiraTimeLayout, cur.JiraUpdatedAt); err == nil {
			// Relative-minutes form: naive JQL datetimes are interpreted in
			// the USER'S profile timezone (a UTC-formatted bound silently
			// missed everything — bit 2026-07-11); "-Nm" is TZ-independent.
			minutes := int(cfg.now().Sub(ts.Add(-overlap)).Minutes()) + 1
			if minutes < 1 {
				minutes = 1
			}
			jql += fmt.Sprintf(` AND updated >= "-%dm"`, minutes)
		}
	}
	jql += " ORDER BY updated ASC"

	maxUpdated := cur.JiraUpdatedAt
	pageToken := ""
	for {
		items, next, err := c.SearchJQL(ctx, jql, pageToken)
		if err != nil {
			return fail(fmt.Errorf("search issues: %w", err))
		}
		stats.IssuesListed += len(items)
		for _, it := range items {
			if err := ingestIssue(ctx, c, sink, acct, it.Key, &stats); err != nil {
				return fail(err)
			}
			if it.Fields.Updated > maxUpdated {
				maxUpdated = it.Fields.Updated
			}
		}
		if next == "" {
			break
		}
		pageToken = next
	}

	cur.JiraUpdatedAt = maxUpdated
	if err := sink.SaveCursor(ctx, acct.ID, cur); err != nil {
		return fail(fmt.Errorf("save cursor: %w", err))
	}
	if err := sink.FinishRun(ctx, runID, "ok", stats, ""); err != nil {
		return stats, fmt.Errorf("finish jira run: %w", err)
	}
	return stats, nil
}

// ingestIssue stores the issue raw (minus comments — one raw item per comment
// keeps 1 raw : 1 message) and every comment raw.
func ingestIssue(ctx context.Context, c *Client, sink Sink, acct Account, key string, stats *Stats) error {
	raw, err := c.GetIssue(ctx, key)
	if err != nil {
		return fmt.Errorf("get issue %s: %w", key, err)
	}
	stats.IssuesFetched++

	issueOnly, inlineComments, total, err := splitIssueComments(raw)
	if err != nil {
		return fmt.Errorf("split issue %s: %w", key, err)
	}
	if err := upsertRaw(ctx, sink, acct.ID, "issue:"+key, issueOnly, stats); err != nil {
		return err
	}

	comments := inlineComments
	if total > len(inlineComments) {
		// page the remainder
		startAt := 0
		comments = nil
		for {
			page, tot, err := c.GetComments(ctx, key, startAt)
			if err != nil {
				return fmt.Errorf("get comments %s: %w", key, err)
			}
			comments = append(comments, page...)
			startAt += len(page)
			if startAt >= tot || len(page) == 0 {
				break
			}
		}
	}
	for _, cm := range comments {
		var meta struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(cm, &meta); err != nil || meta.ID == "" {
			return fmt.Errorf("comment on %s missing id: %.100s", key, cm)
		}
		stats.CommentsFetched++
		if err := upsertRaw(ctx, sink, acct.ID, "comment:"+key+":"+meta.ID, cm, stats); err != nil {
			return err
		}
	}
	return nil
}

// splitIssueComments strips fields.comment.comments from the issue JSON and
// returns the inline comments + the reported total.
func splitIssueComments(raw json.RawMessage) (issueOnly json.RawMessage, comments []json.RawMessage, total int, err error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, nil, 0, fmt.Errorf("parse issue: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(doc["fields"], &fields); err != nil {
		return nil, nil, 0, fmt.Errorf("parse issue fields: %w", err)
	}
	if rawComment, ok := fields["comment"]; ok {
		var cblock struct {
			Total    int               `json:"total"`
			Comments []json.RawMessage `json:"comments"`
		}
		if err := json.Unmarshal(rawComment, &cblock); err == nil {
			comments = cblock.Comments
			total = cblock.Total
		}
		delete(fields, "comment")
	}
	fieldsOnly, err := json.Marshal(fields)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("re-marshal fields: %w", err)
	}
	doc["fields"] = fieldsOnly
	issueOnly, err = json.Marshal(doc)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("re-marshal issue: %w", err)
	}
	return issueOnly, comments, total, nil
}

func upsertRaw(ctx context.Context, sink Sink, accountID int64, externalID string, raw json.RawMessage, stats *Stats) error {
	h, err := chash.ContentHash(raw)
	if err != nil {
		return fmt.Errorf("hash %s: %w", externalID, err)
	}
	stored, exists, err := sink.RawHash(ctx, accountID, externalID)
	if err != nil {
		return fmt.Errorf("read stored hash for %s: %w", externalID, err)
	}
	switch {
	case !exists:
		if err := sink.InsertRaw(ctx, accountID, externalID, raw, h); err != nil {
			return fmt.Errorf("insert raw %s: %w", externalID, err)
		}
		stats.RawInserted++
	case stored == h:
		stats.RawUnchanged++
	default:
		if err := sink.UpdateRaw(ctx, accountID, externalID, raw, h); err != nil {
			return fmt.Errorf("update raw %s: %w", externalID, err)
		}
		stats.RawUpdated++
	}
	return nil
}

func quoteList(keys []string) string {
	quoted := make([]string, len(keys))
	for i, k := range keys {
		quoted[i] = `"` + k + `"`
	}
	return strings.Join(quoted, ", ")
}
