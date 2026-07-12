// Package jira is the client-facing Jira connector (SPEC
// 09-jira-github-connectors): raw-first issue/comment polling into the one
// funnel, plus the gated comment send adapter. The personal SWT build tracker
// is NOT a target — accounts are scoped to explicit project keys.
package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Client is the hand-rolled Jira Cloud REST v2 client (v2 keeps bodies plain
// text both directions; the legacy /search endpoint is gone on Cloud — the
// new /search/jql carries nextPageToken pagination).
type Client struct {
	hc      *http.Client
	baseURL string
	email   string
	token   string
}

func NewClient(hc *http.Client, baseURL, email, token string) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{hc: hc, baseURL: strings.TrimRight(baseURL, "/"), email: email, token: token}
}

func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("build request %s: %w", path, err)
	}
	req.SetBasicAuth(c.email, c.token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jira %s: HTTP %d: %.300s", path, resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

// Myself returns the polling account's own accountId (direction rule).
func (c *Client) Myself(ctx context.Context) (string, error) {
	var out struct {
		AccountID string `json:"accountId"`
	}
	if err := c.get(ctx, "/rest/api/2/myself", nil, &out); err != nil {
		return "", err
	}
	return out.AccountID, nil
}

type searchItem struct {
	Key    string `json:"key"`
	Fields struct {
		Updated string `json:"updated"`
	} `json:"fields"`
}

// SearchJQL returns one page of issue keys matching jql.
func (c *Client) SearchJQL(ctx context.Context, jql, pageToken string) (items []searchItem, nextPage string, err error) {
	q := url.Values{"jql": {jql}, "fields": {"updated"}}
	if pageToken != "" {
		q.Set("nextPageToken", pageToken)
	}
	var out struct {
		Issues        []searchItem `json:"issues"`
		NextPageToken string       `json:"nextPageToken"`
	}
	if err := c.get(ctx, "/rest/api/2/search/jql", q, &out); err != nil {
		return nil, "", err
	}
	return out.Issues, out.NextPageToken, nil
}

// GetIssue fetches the full issue JSON (fields.comment embedded up to the
// server's inline limit).
func (c *Client) GetIssue(ctx context.Context, key string) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.get(ctx, "/rest/api/2/issue/"+url.PathEscape(key), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetComments pages the issue's comments from startAt.
func (c *Client) GetComments(ctx context.Context, key string, startAt int) (comments []json.RawMessage, total int, err error) {
	q := url.Values{"startAt": {fmt.Sprintf("%d", startAt)}}
	var out struct {
		Total    int               `json:"total"`
		Comments []json.RawMessage `json:"comments"`
	}
	if err := c.get(ctx, "/rest/api/2/issue/"+url.PathEscape(key)+"/comment", q, &out); err != nil {
		return nil, 0, err
	}
	return out.Comments, out.Total, nil
}
