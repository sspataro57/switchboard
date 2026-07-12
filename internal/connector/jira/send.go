package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

// CommentSender posts plain-text comments (REST v2 keeps bodies plain text).
// It implements the tools.JiraSender seam via AccountSender below.
type CommentSender struct {
	hc      *http.Client
	baseURL string
	email   string
	token   string
}

func NewCommentSender(hc *http.Client, baseURL, email, token string) *CommentSender {
	return &CommentSender{hc: hc, baseURL: strings.TrimRight(baseURL, "/"), email: email, token: token}
}

// Send posts the comment and returns Jira's assigned comment id.
func (s *CommentSender) Send(ctx context.Context, issueKey, body string) (string, error) {
	payload, err := json.Marshal(map[string]string{"body": google.ScrubAIAttribution(body)})
	if err != nil {
		return "", fmt.Errorf("marshal comment: %w", err)
	}
	u := s.baseURL + "/rest/api/2/issue/" + url.PathEscape(issueKey) + "/comment"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build comment request: %w", err)
	}
	req.SetBasicAuth(s.email, s.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("jira comment post: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read comment response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("jira comment HTTP %d: %.300s", resp.StatusCode, respBody)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("parse comment response: %w", err)
	}
	return out.ID, nil
}

// AccountSender implements the tools.JiraSender seam: it resolves the
// provider='jira' account by site host and posts with its credentials.
type AccountSender struct {
	Pool     *pgxpool.Pool
	TokenKey string
	BaseHC   *http.Client
}

func (a *AccountSender) Send(ctx context.Context, siteHost, issueKey, body string) (string, error) {
	var email, baseURL string
	var acctID int64
	err := a.Pool.QueryRow(ctx,
		`SELECT id, account_email, COALESCE(domain_default,'') FROM source_accounts
		 WHERE provider='jira' AND domain_default LIKE '%'||$1||'%' ORDER BY id LIMIT 1`,
		siteHost).Scan(&acctID, &email, &baseURL)
	if err != nil {
		return "", fmt.Errorf("resolve jira account for %s: %w", siteHost, err)
	}
	var token string
	if err := a.Pool.QueryRow(ctx,
		`SELECT pgp_sym_decrypt(refresh_token_encrypted, $2) FROM source_accounts WHERE id=$1`,
		acctID, a.TokenKey).Scan(&token); err != nil {
		return "", fmt.Errorf("decrypt jira token: %w", err)
	}
	hc := a.BaseHC
	if hc == nil {
		hc = http.DefaultClient
	}
	return NewCommentSender(hc, baseURL, email, token).Send(ctx, issueKey, body)
}
