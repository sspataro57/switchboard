package jira

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
)

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
		return NewClient(http.DefaultClient, acct.SiteBaseURL, acct.Email, token), nil
	}
}
