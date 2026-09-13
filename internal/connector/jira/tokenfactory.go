package jira

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// LookupRequestTimeout bounds one HTTP request (connect, headers and body)
// made by a token-built client (SWT-40 review fix 3). http.DefaultClient has no
// timeout, so one hung Jira socket held a pass until its context deadline.
const LookupRequestTimeout = 30 * time.Second

// tokenHTTPClient is shared by every token-built client: one connection pool,
// one timeout.
var tokenHTTPClient = &http.Client{Timeout: LookupRequestTimeout}

// TokenClientFactory is the token-decrypting ClientFactory: it decrypts the
// account's stored API token with key (OPS_TOKEN_KEY) and builds a client on the
// account's site. One spelling, shared by cmd/connectors/jira (poller and
// reconciler), cmd/pipelined (the capture-time gate) and opsctl.
//
// It returns NIL when key is empty: no credential. Callers treat a nil factory
// as "skip the lookup, loudly" (SWT-32 D21), never as an error.
func TokenClientFactory(pool *pgxpool.Pool, key string) ClientFactory {
	if key == "" {
		return nil
	}
	return func(ctx context.Context, acct Account) (*Client, error) {
		var token string
		if err := pool.QueryRow(ctx,
			`SELECT pgp_sym_decrypt(refresh_token_encrypted, $2) FROM source_accounts WHERE id=$1`,
			acct.ID, key).Scan(&token); err != nil {
			return nil, fmt.Errorf("decrypt token for %s: %w", acct.Email, err)
		}
		return newTokenClient(acct, token), nil
	}
}

// newTokenClient builds the client on the account's site with the timeout-bound
// HTTP client.
func newTokenClient(acct Account, token string) *Client {
	return NewClient(tokenHTTPClient, acct.SiteBaseURL, acct.Email, token)
}
