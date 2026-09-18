package google

// The ONE place that turns a source_accounts row into a connected-ready IMAP
// source (D7).
//
// Before this ticket three callers each did the same three steps for themselves
// — list accounts, decrypt the app password, construct the source. Three copies
// times two credential kinds is six branches and a guaranteed divergence: the
// one-shot pass ingesting four mailboxes while the watcher still listens to
// three, with nothing to grep for. So the branch on credential kind lives here
// and nowhere else, and a structure test fails any other file that names both
// auth types in a credential decision.

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// OpenIMAPSource resolves acct's credential and returns a source ready to
// connect.
//
// app_password decrypts the stored password, exactly as before. xoauth2 hands
// the source a token minter instead: the access token is fetched when the
// connection is made, not here, because a resident watcher outlives the hour a
// Microsoft access token lasts.
//
// Rotation is persisted as a side effect of minting (D8). Microsoft issues a new
// refresh token when it redeems the old one; dropping it kills the mailbox when
// the old one ages out. A failure to persist does NOT fail the pass — the token
// in hand still works — but it is loud, because silently losing it means the
// mailbox dies days later for no visible reason.
func OpenIMAPSource(ctx context.Context, pool *pgxpool.Pool, acct Account, tokenKey string) (*IMAPClientSource, error) {
	switch acct.AuthType {
	case AuthTypeAppPassword:
		password, err := DecryptAppPassword(ctx, pool, acct.ID, tokenKey)
		if err != nil {
			return nil, fmt.Errorf("no credential for %s: %w", acct.Email, err)
		}
		return NewIMAPClientSource(acct.Hosts(), acct.Email, password), nil

	case AuthTypeXOAuth2:
		cfg, err := MicrosoftOAuthConfig()
		if err != nil {
			return nil, fmt.Errorf("no credential for %s: %w", acct.Email, err)
		}
		refresh, err := DecryptRefreshToken(ctx, pool, acct.ID, tokenKey)
		if err != nil {
			return nil, fmt.Errorf("no credential for %s: %w", acct.Email, err)
		}
		// Mint ONCE here, eagerly, before returning a source at all. The token is
		// then handed to the first connection and re-minted for later ones.
		//
		// Eager because this is what makes a revoked consent legible: deferring
		// the whole mint to connect time reports `invalid_grant` as whatever the
		// socket did next — typically a dial or LIST error — and the operator
		// reads a credential problem as a network one. Failing here also means
		// the caller's error run carries the provider's own code (D9).
		first, rotated, err := MintAccessToken(ctx, cfg, refresh)
		if err != nil {
			return nil, fmt.Errorf("no credential for %s: %w", acct.Email, err)
		}
		if rotated != "" {
			if err := SaveRefreshToken(ctx, pool, acct.ID, rotated, tokenKey); err != nil {
				return nil, fmt.Errorf("no credential for %s: the refresh token rotated but could not be "+
					"stored, so this mailbox would stop authenticating once the old one expires: %w",
					acct.Email, err)
			}
			refresh = rotated
		}

		src := NewIMAPClientSource(acct.Hosts(), acct.Email, "")
		src.AccessToken = func(ctx context.Context) (string, error) {
			// The eager token serves the first connection; a resident watcher
			// outlives the hour it lasts, so every later connection mints again.
			if first != "" {
				token := first
				first = ""
				return token, nil
			}
			access, rotated, err := MintAccessToken(ctx, cfg, refresh)
			if err != nil {
				// Names the account and carries the provider's own code
				// (invalid_grant for a revoked or expired token), which is what
				// separates "sign in again" from "fix the registration".
				return "", fmt.Errorf("mint access token for %s: %w", acct.Email, err)
			}
			if rotated != "" {
				if err := SaveRefreshToken(ctx, pool, acct.ID, rotated, tokenKey); err != nil {
					return "", fmt.Errorf("mint access token for %s: the refresh token rotated but could "+
						"not be stored, so this mailbox will stop authenticating once the old one expires: %w",
						acct.Email, err)
				}
				refresh = rotated
			}
			return access, nil
		}
		return src, nil

	default:
		// Includes 'oauth': a Gmail-API row has no password to decrypt and no
		// refresh token this path can use, and handing it to IMAP would produce
		// a login failure that reads as a credential problem rather than a
		// configuration one.
		return nil, fmt.Errorf("account %s has auth_type %q, which is not an IMAP credential",
			acct.Email, acct.AuthType)
	}
}
