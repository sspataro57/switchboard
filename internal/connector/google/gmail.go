package google

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// GmailClient is the hand-rolled REST client (openai-adapter style): the four
// endpoints we call, baseURL injectable for the httptest fake. Identity is the
// userID path segment (the account email); auth rides on hc (oauth2 client in
// production).
type GmailClient struct {
	hc      *http.Client
	baseURL string
	userID  string
}

func NewGmailClient(hc *http.Client, baseURL, userID string) *GmailClient {
	if baseURL == "" {
		baseURL = "https://gmail.googleapis.com"
	}
	return &GmailClient{hc: hc, baseURL: strings.TrimRight(baseURL, "/"), userID: userID}
}

func (c *GmailClient) get(ctx context.Context, path string, query url.Values, out any) error {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("build request %s: %w", path, err)
	}
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
		return &apiError{Status: resp.StatusCode, Body: string(body), Path: path}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

// apiError carries the HTTP status for typed handling (calendar 410).
type apiError struct {
	Status int
	Body   string
	Path   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("google API %s: HTTP %d: %.200s", e.Path, e.Status, e.Body)
}

// GetProfile returns the authorized account's email (identity verification).
func (c *GmailClient) GetProfile(ctx context.Context) (string, error) {
	var out struct {
		EmailAddress string `json:"emailAddress"`
	}
	if err := c.get(ctx, "/gmail/v1/users/"+url.PathEscape(c.userID)+"/profile", nil, &out); err != nil {
		return "", err
	}
	return out.EmailAddress, nil
}

type gmailListItem struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
}

// ListMessages returns one page of message ids matching q.
func (c *GmailClient) ListMessages(ctx context.Context, q, pageToken string) (items []gmailListItem, nextPage string, err error) {
	query := url.Values{}
	if q != "" {
		query.Set("q", q)
	}
	if pageToken != "" {
		query.Set("pageToken", pageToken)
	}
	var out struct {
		Messages      []gmailListItem `json:"messages"`
		NextPageToken string          `json:"nextPageToken"`
	}
	if err := c.get(ctx, "/gmail/v1/users/"+url.PathEscape(c.userID)+"/messages", query, &out); err != nil {
		return nil, "", err
	}
	return out.Messages, out.NextPageToken, nil
}

// GetMessage fetches one message, format=full, returning the verbatim JSON.
func (c *GmailClient) GetMessage(ctx context.Context, id string) (json.RawMessage, error) {
	query := url.Values{"format": {"full"}}
	var out json.RawMessage
	if err := c.get(ctx, "/gmail/v1/users/"+url.PathEscape(c.userID)+"/messages/"+url.PathEscape(id), query, &out); err != nil {
		return nil, err
	}
	return out, nil
}
