package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	Provider = "jira"
	Channel  = "jira"
)

// NormalizedThread is the canonical projection of one issue.
type NormalizedThread struct {
	ThreadKey    string   // jira:{site_host}:{KEY}
	Subject      string   // issue summary
	Participants []string // reporter/assignee accountIds
}

// NormalizedMessage is one issue description or comment.
type NormalizedMessage struct {
	ThreadKey         string
	ExternalMessageID string // jira:{site_host}:issue:{KEY} | jira:{site_host}:comment:{id}
	Direction         string
	SentAt            time.Time
	Subject           string
	Sender            string
	BodyText          string
	Channel           string
	AuthorAccountID   string
}

type jiraUser struct {
	AccountID   string `json:"accountId"`
	DisplayName string `json:"displayName"`
}

type rawIssue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary     string    `json:"summary"`
		Description string    `json:"description"`
		Created     string    `json:"created"`
		Updated     string    `json:"updated"`
		Reporter    *jiraUser `json:"reporter"`
		Assignee    *jiraUser `json:"assignee"`
	} `json:"fields"`
}

// NormalizeIssue is the pure mapper: issue → thread + description message.
// Direction is outbound iff the reporter is the polling account itself.
func NormalizeIssue(raw json.RawMessage, siteHost, ownAccountID string) (NormalizedThread, NormalizedMessage, error) {
	var iss rawIssue
	if err := json.Unmarshal(raw, &iss); err != nil {
		return NormalizedThread{}, NormalizedMessage{}, fmt.Errorf("parse raw issue: %w", err)
	}
	if iss.Key == "" {
		return NormalizedThread{}, NormalizedMessage{}, fmt.Errorf("raw issue has no key")
	}

	threadKey := "jira:" + siteHost + ":" + iss.Key
	th := NormalizedThread{ThreadKey: threadKey, Subject: iss.Fields.Summary}
	var reporterID, reporterName string
	if iss.Fields.Reporter != nil {
		reporterID, reporterName = iss.Fields.Reporter.AccountID, iss.Fields.Reporter.DisplayName
		th.Participants = append(th.Participants, reporterID)
	}
	if iss.Fields.Assignee != nil && (iss.Fields.Reporter == nil || iss.Fields.Assignee.AccountID != iss.Fields.Reporter.AccountID) {
		th.Participants = append(th.Participants, iss.Fields.Assignee.AccountID)
	}

	sentAt, _ := time.Parse(jiraTimeLayout, iss.Fields.Created)
	direction := "inbound"
	if reporterID != "" && reporterID == ownAccountID {
		direction = "outbound"
	}
	sender := reporterName
	if sender == "" {
		sender = reporterID
	}

	msg := NormalizedMessage{
		ThreadKey:         threadKey,
		ExternalMessageID: "jira:" + siteHost + ":issue:" + iss.Key,
		Direction:         direction,
		SentAt:            sentAt,
		Subject:           iss.Fields.Summary,
		Sender:            sender,
		BodyText:          iss.Fields.Description,
		Channel:           Channel,
		AuthorAccountID:   reporterID,
	}
	return th, msg, nil
}

type rawComment struct {
	ID      string    `json:"id"`
	Author  *jiraUser `json:"author"`
	Body    string    `json:"body"`
	Created string    `json:"created"`
}

// NormalizeComment is the pure mapper for one comment; issueKey comes from the
// raw item external_id (comment:{KEY}:{id}).
func NormalizeComment(raw json.RawMessage, siteHost, issueKey, ownAccountID string) (NormalizedMessage, error) {
	var cm rawComment
	if err := json.Unmarshal(raw, &cm); err != nil {
		return NormalizedMessage{}, fmt.Errorf("parse raw comment: %w", err)
	}
	if cm.ID == "" {
		return NormalizedMessage{}, fmt.Errorf("raw comment has no id")
	}

	sentAt, _ := time.Parse(jiraTimeLayout, cm.Created)
	var authorID, authorName string
	if cm.Author != nil {
		authorID, authorName = cm.Author.AccountID, cm.Author.DisplayName
	}
	direction := "inbound"
	if authorID != "" && authorID == ownAccountID {
		direction = "outbound"
	}
	sender := authorName
	if sender == "" {
		sender = authorID
	}

	return NormalizedMessage{
		ThreadKey:         "jira:" + siteHost + ":" + issueKey,
		ExternalMessageID: "jira:" + siteHost + ":comment:" + cm.ID,
		Direction:         direction,
		SentAt:            sentAt,
		Subject:           "",
		Sender:            sender,
		BodyText:          cm.Body,
		Channel:           Channel,
		AuthorAccountID:   authorID,
	}, nil
}

// SiteHost extracts the host from the account's SiteBaseURL.
func SiteHost(baseURL string) string {
	h := strings.TrimPrefix(strings.TrimPrefix(baseURL, "https://"), "http://")
	return strings.TrimSuffix(strings.Split(h, "/")[0], "/")
}

// Normalize is the second phase: raw issues/comments → threads + messages.
// Own comments (author == the polling account) are outbound — invisible to
// triage's inbound filter — and confirm matching deliveries (loop closure).
func Normalize(ctx context.Context, sink *PGSink, cfg Config) (Stats, error) {
	var stats Stats

	accounts, err := sink.accountMeta(ctx)
	if err != nil {
		return stats, fmt.Errorf("load account metadata: %w", err)
	}

	items, err := sink.pendingRaw(ctx, cfg.All)
	if err != nil {
		return stats, fmt.Errorf("list raw items: %w", err)
	}

	for _, it := range items {
		meta, ok := accounts[it.accountID]
		if !ok {
			return stats, fmt.Errorf("raw item %d references unknown jira account %d", it.id, it.accountID)
		}
		switch {
		case strings.HasPrefix(it.externalID, "issue:"):
			th, msg, err := NormalizeIssue(it.raw, meta.siteHost, meta.ownAccountID)
			if err != nil {
				return stats, fmt.Errorf("normalize %s: %w", it.externalID, err)
			}
			if err := sink.upsertThreadMessage(ctx, it.id, th, msg); err != nil {
				return stats, fmt.Errorf("apply %s: %w", it.externalID, err)
			}
		case strings.HasPrefix(it.externalID, "comment:"):
			parts := strings.SplitN(it.externalID, ":", 3)
			if len(parts) != 3 {
				return stats, fmt.Errorf("raw item %d has malformed comment id %q", it.id, it.externalID)
			}
			msg, err := NormalizeComment(it.raw, meta.siteHost, parts[1], meta.ownAccountID)
			if err != nil {
				return stats, fmt.Errorf("normalize %s: %w", it.externalID, err)
			}
			th := NormalizedThread{ThreadKey: msg.ThreadKey}
			if err := sink.upsertThreadMessage(ctx, it.id, th, msg); err != nil {
				return stats, fmt.Errorf("apply %s: %w", it.externalID, err)
			}
		default:
			return stats, fmt.Errorf("raw item %d has unknown external_id shape %q", it.id, it.externalID)
		}
		if err := sink.markNormalized(ctx, it.id); err != nil {
			return stats, fmt.Errorf("stamp normalized_at for %s: %w", it.externalID, err)
		}
		stats.Normalized++
	}
	return stats, nil
}
