package google

// Microsoft sign-in for the IMAP path (SWT-66).
//
// outlook.office365.com:993 advertises `LOGINDISABLED` with `AUTH=XOAUTH2` as
// its only mechanism, so a password — app password included — cannot
// authenticate. This file is everything needed to hold a Microsoft credential
// and turn it into a bearer token: the endpoint/scope configuration, the device
// flow that obtains a refresh token once, the id_token claim reader that proves
// WHICH mailbox consented, and the per-pass mint that exchanges the stored
// refresh token for a short-lived access token.
//
// Every endpoint is injectable so the tests run offline against httptest.
//
// SECRETS: no refresh token, access token or pgcrypto key may appear in an error
// string. These errors reach stdout, logs and sync_runs rows.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"
)

// DefaultMicrosoftAuthority is the `consumers` authority: PERSONAL Microsoft
// accounts only (Q2).
//
// Not `/common` and not `/organizations`. With consumers, a work or school
// account cannot complete the flow at all — the wrong-account mistake is refused
// by the identity provider rather than caught afterwards by the claim check,
// which stays as the second net.
const DefaultMicrosoftAuthority = "https://login.microsoftonline.com/consumers"

// MicrosoftScopes is D2's scope set, and deliberately no more.
//
//   - IMAP.AccessAsUser.All is the minimum to read mail over IMAP.
//   - offline_access is what makes a refresh token come back; without it the
//     account dies in an hour.
//   - openid and email are reserved scopes, so they combine with a resource
//     scope in one request, and they make the token response carry an id_token,
//     which is how onboarding verifies the authorized identity.
//
// SMTP.Send is absent on purpose: sending from this mailbox is out of scope, and
// consent is the gate. Adding it later is incremental consent — extend this list
// and re-run the subcommand.
var MicrosoftScopes = []string{
	"https://outlook.office.com/IMAP.AccessAsUser.All",
	"offline_access",
	"openid",
	"email",
}

// MicrosoftOAuthConfig builds the OAuth config from the environment.
//
// MS_OAUTH_CLIENT_ID is the Azure Application (client) ID. It is required and
// not a secret: the app is a PUBLIC client, so no secret is issued and none is
// stored — ClientSecret stays empty by construction.
//
// MS_OAUTH_AUTHORITY overrides the authority base for tests and for a future
// audience change; empty means DefaultMicrosoftAuthority.
func MicrosoftOAuthConfig() (*oauth2.Config, error) {
	clientID := strings.TrimSpace(os.Getenv("MS_OAUTH_CLIENT_ID"))
	if clientID == "" {
		return nil, errors.New("MS_OAUTH_CLIENT_ID is not set: it is the Azure Application (client) ID of the " +
			"public client registration, and no Microsoft mailbox can authenticate without it")
	}
	authority := strings.TrimRight(strings.TrimSpace(os.Getenv("MS_OAUTH_AUTHORITY")), "/")
	if authority == "" {
		authority = DefaultMicrosoftAuthority
	}
	return &oauth2.Config{
		ClientID: clientID,
		Scopes:   append([]string(nil), MicrosoftScopes...),
		Endpoint: oauth2.Endpoint{
			AuthURL:       authority + "/oauth2/v2.0/authorize",
			DeviceAuthURL: authority + "/oauth2/v2.0/devicecode",
			TokenURL:      authority + "/oauth2/v2.0/token",
			// Pinned, not probed. Left unset, x/oauth2 discovers the style by
			// RETRYING a failed token request with the other one, which doubles
			// every failed poll: a declined sign-in would hit the token endpoint
			// twice and read as a flaky provider. A public client sends its
			// client_id in the form body, so this is also simply correct.
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}, nil
}

// DeviceFlow runs the device code flow and returns the resulting token.
//
// Device code rather than the loopback flow `google-auth add` uses (D1): no
// redirect URI to register, no PKCE plumbing, and the browser step happens on
// whatever device the operator likes while this binary polls — which is what
// makes it work over SSH and from the cluster.
//
// The instructions go to out rather than stdout so the caller can capture them.
func DeviceFlow(ctx context.Context, cfg *oauth2.Config, out io.Writer) (*oauth2.Token, error) {
	if cfg == nil {
		return nil, errors.New("device flow: no OAuth config")
	}
	resp, err := cfg.DeviceAuth(ctx)
	if err != nil {
		return nil, fmt.Errorf("device authorization request: %w", err)
	}
	uri := resp.VerificationURIComplete
	if uri == "" {
		uri = resp.VerificationURI
	}
	fmt.Fprintf(out, "Open %s and enter the code %s\n", resp.VerificationURI, resp.UserCode)
	if resp.VerificationURIComplete != "" && resp.VerificationURIComplete != resp.VerificationURI {
		fmt.Fprintf(out, "Or open %s, which carries the code already\n", uri)
	}
	fmt.Fprintf(out, "Waiting for you to finish signing in ...\n")

	// DeviceAccessToken honours authorization_pending and slow_down itself, and
	// returns on access_denied or expired_token.
	tok, err := cfg.DeviceAccessToken(ctx, resp)
	if err != nil {
		return nil, fmt.Errorf("device access token: %w", err)
	}
	return tok, nil
}

// IDTokenIdentity returns the mailbox the id_token was issued for, plus the
// claim names it carried.
//
// It reads `preferred_username`, else `email`. It does NOT read `upn`: that is
// what a work account carries, and Q2 says a work account must not get through.
//
// The signature is NOT verified, and that is acceptable ONLY because the token
// came straight from the token endpoint over TLS inside this process and is used
// for one thing: confirming the operator signed in as the mailbox they named.
// It is never accepted from a caller, a header, or storage. Do not reuse this
// function anywhere that is not true.
//
// The claim list is returned so a refusal can print what WAS there, which is the
// difference between "you signed in with the wrong account" and silence.
func IDTokenIdentity(idToken string) (string, []string, error) {
	parts := strings.Split(strings.TrimSpace(idToken), ".")
	if len(parts) != 3 {
		return "", nil, fmt.Errorf("id_token is not a JWT (%d segments, want 3)", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", nil, fmt.Errorf("id_token payload is not base64url: %w", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", nil, fmt.Errorf("id_token payload is not JSON: %w", err)
	}
	names := make([]string, 0, len(claims))
	for k := range claims {
		names = append(names, k)
	}
	sort.Strings(names)

	for _, key := range []string{"preferred_username", "email"} {
		if v, ok := claims[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v), names, nil
		}
	}
	return "", names, fmt.Errorf("id_token carries neither preferred_username nor email; claims present: %s",
		strings.Join(names, ", "))
}

// MintAccessToken exchanges a stored refresh token for an access token, and
// returns the refresh token to store next.
//
// Microsoft ROTATES the refresh token on redemption for personal accounts, so
// the second return value is load-bearing: dropping it kills the mailbox when
// the old token ages out. It is empty when the provider returned no new one, and
// the caller must then keep what it had.
//
// Access tokens are never persisted, by anyone.
func MintAccessToken(ctx context.Context, cfg *oauth2.Config, refreshToken string) (string, string, error) {
	if cfg == nil {
		return "", "", errors.New("mint access token: no OAuth config")
	}
	if strings.TrimSpace(refreshToken) == "" {
		return "", "", errors.New("mint access token: no stored refresh token for this account")
	}
	tok, err := cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken}).Token()
	if err != nil {
		// %s, not %w, on purpose: the wrapped *oauth2.RetrieveError carries the
		// raw response body, which is the one place a provider could echo the
		// credential back. Scrubbing means errors.As cannot reach it downstream;
		// the provider's error CODE (invalid_grant) survives in the text, which
		// is what the operator and D9's error row actually need.
		return "", "", fmt.Errorf("refresh token rejected: %s", scrubSecrets(err.Error(), refreshToken))
	}
	if tok.AccessToken == "" {
		return "", "", errors.New("token endpoint returned no access token")
	}
	rotated := ""
	if tok.RefreshToken != "" && tok.RefreshToken != refreshToken {
		rotated = tok.RefreshToken
	}
	return tok.AccessToken, rotated, nil
}

// scrubSecrets removes credential material from a provider error before it
// becomes a log line or a sync_runs row. x/oauth2's RetrieveError carries the
// raw response body, and a provider is free to echo whatever it likes.
func scrubSecrets(msg string, secrets ...string) string {
	for _, s := range secrets {
		if len(s) >= 6 {
			msg = strings.ReplaceAll(msg, s, "<redacted>")
		}
	}
	return msg
}

// SaveRefreshToken persists a rotated refresh token and NOTHING else.
//
// One column on purpose. UpsertGoogleAccount would also write scopes and
// calendar_in_availability, so using it here would silently resurrect stale
// values on every rotation — the same whole-blob clobber that SaveCursorField
// exists to avoid.
func SaveRefreshToken(ctx context.Context, pool *pgxpool.Pool, accountID int64, refreshToken, key string) error {
	if strings.TrimSpace(refreshToken) == "" {
		return errors.New("save refresh token: refusing to store an empty token")
	}
	if key == "" {
		return errors.New("save refresh token: OPS_TOKEN_KEY is not set")
	}
	ct, err := pool.Exec(ctx,
		`UPDATE source_accounts SET refresh_token_encrypted = pgp_sym_encrypt($2, $3) WHERE id = $1`,
		accountID, refreshToken, key)
	if err != nil {
		return fmt.Errorf("store rotated refresh token for account %d: %w", accountID, err)
	}
	if ct.RowsAffected() != 1 {
		return fmt.Errorf("store rotated refresh token: account %d does not exist", accountID)
	}
	return nil
}

// UpsertXOAuth2Account stores (or re-keys) a Microsoft mailbox.
//
// provider stays 'google' (D3). On this deployment that value already means "a
// mailbox switchboard reads over IMAP and sends over SMTP, ingested by
// cmd/connectors/google" — every google row has a NULL refresh token and every
// raw item carries source:"imap". A 'microsoft' value would leave the address
// out of ownEmailSet, so the mailbox's own sends would normalize INBOUND and
// become triage candidates (invariant 5), and out of PendingRaw's join, so
// ingest would succeed while nothing normalized and no error appeared anywhere.
//
// calendar_in_availability is forced FALSE and is not a parameter: a
// provider='google' row with that flag and no calendar sync makes propose_slots
// refuse for EVERY account, forever (the SWT-24 readiness contract).
//
// send_enabled is set false on INSERT only, so re-running to rotate a credential
// cannot silently revoke a mailbox that was already sending.
func UpsertXOAuth2Account(ctx context.Context, pool *pgxpool.Pool, email, refreshToken, tokenKey string,
	scopes []string, hosts MailHosts) (int64, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return 0, errors.New("upsert xoauth2 account: refusing to store an empty refresh token")
	}
	if tokenKey == "" {
		return 0, errors.New("upsert xoauth2 account: OPS_TOKEN_KEY is not set")
	}
	// A personal Microsoft account can be registered against a gmail.com
	// address, so `add-microsoft sspataro57@gmail.com` would pass both identity
	// checks and then re-key a LIVE app-password mailbox to xoauth2 — orphaning
	// its password, and making every send from it refuse. Converting a mailbox
	// between credential kinds is a deliberate act, not a side effect of a typo.
	var existing string
	switch err := pool.QueryRow(ctx,
		`SELECT COALESCE(auth_type,'') FROM source_accounts WHERE provider='google' AND account_email=$1`,
		email).Scan(&existing); {
	case err == nil && existing != "" && existing != AuthTypeXOAuth2:
		return 0, fmt.Errorf("%s already exists as a %s mailbox; refusing to convert it to xoauth2. "+
			"Remove or re-onboard it deliberately if that is what you meant", email, existing)
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return 0, fmt.Errorf("check the existing account for %s: %w", email, err)
	}

	h := hosts.WithDefaults()
	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts
		   (provider, account_email, auth_type, refresh_token_encrypted, scopes,
		    send_enabled, calendar_in_availability, imap_host, imap_port, smtp_host, smtp_port)
		 VALUES ('google', $1, 'xoauth2', pgp_sym_encrypt($2, $3), $4,
		         false, false, $5, $6, $7, $8)
		 ON CONFLICT (provider, account_email) DO UPDATE SET
		   auth_type               = 'xoauth2',
		   refresh_token_encrypted = pgp_sym_encrypt($2, $3),
		   scopes                  = EXCLUDED.scopes,
		   calendar_in_availability = false,
		   imap_host = EXCLUDED.imap_host, imap_port = EXCLUDED.imap_port,
		   smtp_host = EXCLUDED.smtp_host, smtp_port = EXCLUDED.smtp_port
		 RETURNING id`,
		email, refreshToken, tokenKey, scopes,
		h.IMAPHost, h.IMAPPort, h.SMTPHost, h.SMTPPort).Scan(&id)
	if err != nil {
		// The token is never interpolated into this message.
		return 0, fmt.Errorf("store xoauth2 account %s: %w", email, err)
	}
	return id, nil
}
