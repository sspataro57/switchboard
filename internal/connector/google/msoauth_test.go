package google_test

// Offline unit tests for the Microsoft OAuth plumbing (SPEC
// docs/tickets/microsoft-oauth-mail_SPEC.md, acceptance criteria 1, 5 and 6;
// decisions D1, D2, D4, D8, D10).
//
// Every endpoint is an httptest server: no login.microsoftonline.com, no
// Postgres, no clock the test does not control. That is criterion 1 and
// invariant 7 — the claim reader is a pure function and the flow takes an
// injectable authority.
//
// EXPECTED FAILURE MODE: **compile error**. internal/connector/google/msoauth.go
// does not exist, so these fail with "undefined: google.MicrosoftOAuthConfig"
// (and friends) until it does.
//
// IMPOSED SURFACE — the SPEC names msoauth.go's contents ("endpoint construction
// from MS_OAUTH_CLIENT_ID/MS_OAUTH_AUTHORITY, MicrosoftScopes, the device flow,
// the id_token claim reader, the per-pass token source with the rotation hook,
// SaveRefreshToken") but only spells one signature. These are the ones this file
// pins; rename freely, but each seam must exist somewhere or the criterion it
// carries cannot be tested offline:
//
//	const DefaultMicrosoftAuthority = "https://login.microsoftonline.com/consumers"
//	var MicrosoftScopes = []string{
//	    "https://outlook.office.com/IMAP.AccessAsUser.All",
//	    "offline_access", "openid", "email",
//	}
//
//	// MicrosoftOAuthConfig reads MS_OAUTH_CLIENT_ID (required) and
//	// MS_OAUTH_AUTHORITY (optional override of the authority base). Env, not
//	// columns: the CAL_SOURCE/MAIL_SOURCE precedent (D10).
//	func MicrosoftOAuthConfig() (*oauth2.Config, error)
//
//	// DeviceFlow runs RFC 8628 against cfg's endpoints, printing the verification
//	// URL and the user code to out.
//	//
//	// DELIBERATE DEVIATION from the SPEC's sketch `DeviceFlow(ctx, cfg)`: the
//	// writer is a parameter so criterion 5's "prints the verification URL and user
//	// code" is assertable without hijacking os.Stdout. Production passes os.Stderr.
//	func DeviceFlow(ctx context.Context, cfg *oauth2.Config, out io.Writer) (*oauth2.Token, error)
//
//	// IDTokenIdentity decodes the id_token's middle segment (base64url) WITHOUT
//	// verifying the signature and returns preferred_username, or email. claims
//	// lists the claim names found, for the refusal message (D4.1).
//	func IDTokenIdentity(idToken string) (identity string, claims []string, err error)
//
//	// MintAccessToken redeems a refresh token. rotated carries the NEW refresh
//	// token when the provider rotated it (Microsoft always does for personal
//	// accounts) and is "" otherwise. No database: the persistence decision is
//	// OpenIMAPSource's, through SaveRefreshToken (D8).
//	func MintAccessToken(ctx context.Context, cfg *oauth2.Config, refreshToken string) (accessToken, rotated string, err error)

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

const (
	msxClientID = "11111111-2222-3333-4444-555555555555"

	// The sentinel criterion 6 greps for. It must appear in NO rendered error,
	// NO printed line and NO sync_runs payload.
	msxRefreshSentinel = "0.AXoA-SENTINEL-REFRESH-TOKEN-DO-NOT-LEAK"
)

// ---- fake device/token endpoints ----------------------------------------------

// msxIDToken builds an unsigned JWT with the given claims. The signature segment
// is junk on purpose: D4 says the decode does NOT verify it, and a test that fed
// a real signature would quietly imply otherwise.
func msxIDToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc(header) + "." + enc(payload) + ".not-a-real-signature"
}

// msxAuthority is a fake Microsoft authority: the device-code endpoint plus a
// token endpoint driven by a scripted sequence of replies.
type msxAuthority struct {
	srv *httptest.Server

	mu          sync.Mutex
	tokenCalls  int
	deviceCalls int
	lastForm    map[string][]string

	// replies is consumed one per token request; the last entry repeats.
	replies []msxTokenReply
	// interval is what the device response advertises, in seconds.
	interval int
}

type msxTokenReply struct {
	status int
	body   map[string]any
}

func newMSXAuthority(t *testing.T, interval int, replies ...msxTokenReply) *msxAuthority {
	t.Helper()
	a := &msxAuthority{replies: replies, interval: interval}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/v2.0/devicecode", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.deviceCalls++
		a.mu.Unlock()
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":      "DEV-CODE-XYZ",
			"user_code":        "HJKL-MNPQ",
			"verification_uri": "https://microsoft.com/devicelogin",
			"expires_in":       300,
			"interval":         a.interval,
			"message":          "To sign in, use a web browser to open the page …",
		})
	})
	mux.HandleFunc("/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		a.mu.Lock()
		a.tokenCalls++
		idx := a.tokenCalls - 1
		if idx >= len(a.replies) {
			idx = len(a.replies) - 1
		}
		reply := a.replies[idx]
		a.lastForm = r.PostForm
		a.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.status)
		_ = json.NewEncoder(w).Encode(reply.body)
	})
	a.srv = httptest.NewServer(mux)
	t.Cleanup(a.srv.Close)
	return a
}

func (a *msxAuthority) counts() (device, token int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.deviceCalls, a.tokenCalls
}

func (a *msxAuthority) form() map[string][]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastForm
}

// msxConfig points MicrosoftOAuthConfig at the fake authority through the very
// env vars production uses (D10) — so this also pins that the override exists.
func msxConfig(t *testing.T, authority string) *oauth2.Config {
	t.Helper()
	t.Setenv("MS_OAUTH_CLIENT_ID", msxClientID)
	t.Setenv("MS_OAUTH_AUTHORITY", authority)
	cfg, err := google.MicrosoftOAuthConfig()
	if err != nil {
		t.Fatalf("MicrosoftOAuthConfig: %v", err)
	}
	return cfg
}

// ---- D1/D2/D10 + Q2: the config ------------------------------------------------

func TestMicrosoftOAuthConfig_ScopesAudienceAndEndpoints(t *testing.T) {
	t.Setenv("MS_OAUTH_CLIENT_ID", msxClientID)
	t.Setenv("MS_OAUTH_AUTHORITY", "")

	cfg, err := google.MicrosoftOAuthConfig()
	if err != nil {
		t.Fatalf("MicrosoftOAuthConfig: %v", err)
	}
	if cfg.ClientID != msxClientID {
		t.Errorf("ClientID = %q, want the MS_OAUTH_CLIENT_ID value", cfg.ClientID)
	}
	if cfg.ClientSecret != "" {
		t.Errorf("ClientSecret = %q, want empty — D1: a public client is issued no secret and stores none", cfg.ClientSecret)
	}
	if cfg.Endpoint.DeviceAuthURL == "" {
		t.Errorf("no DeviceAuthURL; x/oauth2 refuses the device flow without one (D1)")
	}
	// Q2: personal Microsoft accounts ONLY. A work account must be refused at the
	// identity provider, not by the claim check afterwards.
	for _, url := range []string{cfg.Endpoint.DeviceAuthURL, cfg.Endpoint.TokenURL} {
		if !strings.HasPrefix(url, google.DefaultMicrosoftAuthority) {
			t.Errorf("endpoint %q is not under %q (Q2 chose the consumers audience; /common or /organizations "+
				"would let a work account complete the flow)", url, google.DefaultMicrosoftAuthority)
		}
	}
	if strings.Contains(cfg.Endpoint.TokenURL, "/common") || strings.Contains(cfg.Endpoint.TokenURL, "/organizations") {
		t.Errorf("token endpoint %q names a multi-tenant audience", cfg.Endpoint.TokenURL)
	}

	// D2: the exact scope set, and nothing that could send or read a calendar.
	want := map[string]bool{
		"https://outlook.office.com/IMAP.AccessAsUser.All": true,
		"offline_access": true,
		"openid":         true,
		"email":          true,
	}
	got := map[string]bool{}
	for _, s := range cfg.Scopes {
		got[s] = true
	}
	for s := range want {
		if !got[s] {
			t.Errorf("scope %q is not requested; without offline_access the account dies in an hour and "+
				"without openid/email there is no id_token for D4's claim check", s)
		}
	}
	for s := range got {
		if !want[s] {
			t.Errorf("unexpected scope %q. D2 requests four and no more — SMTP.Send in particular is "+
				"deliberately absent (out of scope: sending from this mailbox)", s)
		}
	}
	for _, forbidden := range []string{"SMTP.Send", "Mail.Send", "calendar"} {
		for _, s := range cfg.Scopes {
			if strings.Contains(strings.ToLower(s), strings.ToLower(forbidden)) {
				t.Errorf("scope %q contains %q — out of scope for this ticket, and consent is the gate", s, forbidden)
			}
		}
	}
}

// The auth style is PINNED, not discovered. With AuthStyleUnknown x/oauth2 tries
// the header style and, on any error, re-issues the WHOLE request with the
// params style — and MicrosoftOAuthConfig builds a fresh Config per call, so its
// style cache never warms and the doubling is permanent, not first-time-only. A
// declined sign-in would hit Microsoft twice and read as a flaky provider. It is
// also simply required: with an empty ClientSecret the header style sends
// `Authorization: Basic base64(clientid:)`, which Microsoft reads as a
// confidential-client request.
func TestMicrosoftOAuthConfig_AuthStyleIsPinnedToParams(t *testing.T) {
	t.Setenv("MS_OAUTH_CLIENT_ID", msxClientID)
	t.Setenv("MS_OAUTH_AUTHORITY", "")

	cfg, err := google.MicrosoftOAuthConfig()
	if err != nil {
		t.Fatalf("MicrosoftOAuthConfig: %v", err)
	}
	if cfg.Endpoint.AuthStyle != oauth2.AuthStyleInParams {
		t.Errorf("Endpoint.AuthStyle = %v, want AuthStyleInParams", cfg.Endpoint.AuthStyle)
	}
}

func TestMicrosoftOAuthConfig_MissingClientIDNamesTheVariable(t *testing.T) {
	t.Setenv("MS_OAUTH_CLIENT_ID", "")
	t.Setenv("MS_OAUTH_AUTHORITY", "")

	_, err := google.MicrosoftOAuthConfig()
	if err == nil {
		t.Fatalf("MicrosoftOAuthConfig returned no error with MS_OAUTH_CLIENT_ID unset; D10 says an absent " +
			"client id is a LOUD per-account failure, never a skip")
	}
	if !strings.Contains(err.Error(), "MS_OAUTH_CLIENT_ID") {
		t.Errorf("error %q does not name MS_OAUTH_CLIENT_ID; D9 requires the cause class in the sync_runs "+
			"message and this string is what it is built from", err)
	}
}

// ---- criterion 5: the device flow ---------------------------------------------

// NOTE ON RUNTIME: x/oauth2's DeviceAccessToken sleeps for the server-advertised
// interval and, per RFC 8628, adds FIVE seconds on slow_down. With interval=1
// that makes this test take roughly 1 + 1 + 6 seconds. The wall-clock cost buys
// the one behaviour a naive implementation gets wrong — treating any error reply
// as fatal — so it is paid deliberately rather than mocked away.
func TestDeviceFlow_PrintsTheCodeAndPollsThroughPendingAndSlowDown(t *testing.T) {
	auth := newMSXAuthority(t, 1,
		msxTokenReply{http.StatusBadRequest, map[string]any{"error": "authorization_pending"}},
		msxTokenReply{http.StatusBadRequest, map[string]any{"error": "slow_down"}},
		msxTokenReply{http.StatusOK, map[string]any{
			"token_type":    "Bearer",
			"scope":         strings.Join(google.MicrosoftScopes, " "),
			"expires_in":    3600,
			"access_token":  "ACCESS-TOKEN-1",
			"refresh_token": msxRefreshSentinel,
			"id_token":      msxIDToken(t, map[string]any{"preferred_username": "sspataro57@msn.com", "sub": "abc"}),
		}},
	)
	cfg := msxConfig(t, auth.srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var out bytes.Buffer
	tok, err := google.DeviceFlow(ctx, cfg, &out)
	if err != nil {
		t.Fatalf("DeviceFlow: %v (it must survive authorization_pending and slow_down — those are the "+
			"NORMAL replies while the human is still signing in)", err)
	}
	if tok.RefreshToken != msxRefreshSentinel {
		t.Errorf("RefreshToken = %q, want the one the token endpoint returned", tok.RefreshToken)
	}
	if tok.AccessToken != "ACCESS-TOKEN-1" {
		t.Errorf("AccessToken = %q, want ACCESS-TOKEN-1", tok.AccessToken)
	}
	if tok.Extra("id_token") == nil || tok.Extra("id_token") == "" {
		t.Errorf("the token carries no id_token; D4's claim check has nothing to read")
	}

	device, token := auth.counts()
	if device != 1 {
		t.Errorf("device-code requests = %d, want 1", device)
	}
	if token != 3 {
		t.Errorf("token requests = %d, want 3 (pending, slow_down, success). Fewer means an error reply "+
			"aborted the poll: the user would see the CLI give up while the browser step was still open", token)
	}

	printed := out.String()
	for _, want := range []string{"https://microsoft.com/devicelogin", "HJKL-MNPQ"} {
		if !strings.Contains(printed, want) {
			t.Errorf("the flow printed %q, which does not contain %q — without both the URL and the user "+
				"code there is nothing for Salvador to act on (criterion 5)", printed, want)
		}
	}
	// Criterion 6: the printed lines are terminal + log output.
	if strings.Contains(printed, msxRefreshSentinel) || strings.Contains(printed, "ACCESS-TOKEN-1") {
		t.Errorf("the device flow printed a token: %q", printed)
	}

	// D1: no PKCE, no secret, no redirect URI on the wire.
	form := auth.form()
	if form == nil {
		t.Fatalf("the token endpoint recorded no form")
	}
	for _, forbidden := range []string{"client_secret", "code_verifier", "redirect_uri"} {
		if v := form[forbidden]; len(v) > 0 && v[0] != "" {
			t.Errorf("the token request carried %s=%q; D1 chose device code precisely to avoid all three", forbidden, v[0])
		}
	}
	if got := form["grant_type"]; len(got) == 0 || !strings.Contains(got[0], "device_code") {
		t.Errorf("grant_type = %v, want the RFC 8628 device_code grant", got)
	}
}

func TestDeviceFlow_AccessDeniedStopsAndSaysSo(t *testing.T) {
	auth := newMSXAuthority(t, 1,
		msxTokenReply{http.StatusBadRequest, map[string]any{
			"error":             "access_denied",
			"error_description": "The user has declined to authorize the application",
		}},
	)
	cfg := msxConfig(t, auth.srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var out bytes.Buffer
	if _, err := google.DeviceFlow(ctx, cfg, &out); err == nil {
		t.Fatalf("DeviceFlow returned no error after access_denied; it would poll until the code expired")
	} else if !strings.Contains(err.Error(), "access_denied") {
		t.Errorf("error %q does not name access_denied", err)
	}
	if _, token := auth.counts(); token != 1 {
		t.Errorf("token requests = %d, want 1 — access_denied is terminal, not transient", token)
	}
}

// ---- D4.1: the id_token claim reader (pure) -----------------------------------

func TestIDTokenIdentity_ReadsPreferredUsernameThenEmail(t *testing.T) {
	cases := []struct {
		name   string
		claims map[string]any
		want   string
	}{
		{"preferred_username", map[string]any{"preferred_username": "sspataro57@msn.com", "sub": "x"}, "sspataro57@msn.com"},
		{"email only", map[string]any{"email": "sspataro57@msn.com", "sub": "x"}, "sspataro57@msn.com"},
		{"both agree", map[string]any{"preferred_username": "sspataro57@msn.com", "email": "sspataro57@msn.com"}, "sspataro57@msn.com"},
		{"mixed case is preserved as sent", map[string]any{"preferred_username": "SSpataro57@MSN.com"}, "SSpataro57@MSN.com"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, claims, err := google.IDTokenIdentity(msxIDToken(t, tc.claims))
			if err != nil {
				t.Fatalf("IDTokenIdentity: %v", err)
			}
			if !strings.EqualFold(got, tc.want) {
				t.Errorf("identity = %q, want %q (case-insensitively)", got, tc.want)
			}
			if len(claims) == 0 {
				t.Errorf("claims list is empty; D4 requires the refusal message to print which claims were found")
			}
		})
	}
}

func TestIDTokenIdentity_RefusesWhenNeitherClaimIsPresent(t *testing.T) {
	// A token with an identity-shaped claim we deliberately do NOT accept: `upn`
	// is what a work account carries, and Q2 says a work account must not get
	// through at all.
	id := msxIDToken(t, map[string]any{"sub": "abc123", "aud": msxClientID, "upn": "salvo@contoso.com"})

	got, claims, err := google.IDTokenIdentity(id)
	if err == nil {
		t.Fatalf("IDTokenIdentity returned %q with neither preferred_username nor email present; "+
			"a mislabelled row poisons ownEmailSet and every thread key (D4)", got)
	}
	found := strings.Join(claims, ",") + " " + err.Error()
	for _, want := range []string{"sub", "aud"} {
		if !strings.Contains(found, want) {
			t.Errorf("the refusal does not say which claims WERE found (%q); D4 requires it, because the "+
				"operator's next move depends on whether the consent simply lacked the email scope", found)
		}
	}
}

func TestIDTokenIdentity_RefusesMalformedTokens(t *testing.T) {
	cases := []struct{ name, token string }{
		{"empty", ""},
		{"one segment", "not-a-jwt"},
		{"two segments", "aaa.bbb"},
		{"payload is not base64url", "aaa.!!!!.ccc"},
		{"payload is not json", "aaa." + base64.RawURLEncoding.EncodeToString([]byte("nope")) + ".ccc"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got, _, err := google.IDTokenIdentity(tc.token); err == nil {
				t.Errorf("IDTokenIdentity(%q) = %q, want an error", tc.token, got)
			}
		})
	}
}

// Padded vs unpadded: Microsoft sends base64url with no padding. A decoder using
// the padded alphabet fails on two thirds of real tokens — and only in production.
func TestIDTokenIdentity_AcceptsUnpaddedBase64URL(t *testing.T) {
	// 'x' repeated until the payload length is NOT a multiple of 4 when encoded.
	claims := map[string]any{"preferred_username": "a@b.com", "nonce": strings.Repeat("x", 5)}
	id := msxIDToken(t, claims)
	if strings.Contains(id, "=") {
		t.Fatalf("the fixture itself is padded; it no longer tests what it claims: %q", id)
	}
	if _, _, err := google.IDTokenIdentity(id); err != nil {
		t.Errorf("IDTokenIdentity on an unpadded base64url payload: %v", err)
	}
}

// ---- D8 + criterion 6: minting, rotation and the no-leak rule ------------------

func TestMintAccessToken_ReturnsTheRotatedRefreshToken(t *testing.T) {
	const rotated = "0.AXoA-ROTATED-REFRESH-TOKEN"
	auth := newMSXAuthority(t, 1, msxTokenReply{http.StatusOK, map[string]any{
		"token_type":    "Bearer",
		"expires_in":    3600,
		"access_token":  "ACCESS-TOKEN-2",
		"refresh_token": rotated,
	}})
	cfg := msxConfig(t, auth.srv.URL)

	access, got, err := google.MintAccessToken(context.Background(), cfg, msxRefreshSentinel)
	if err != nil {
		t.Fatalf("MintAccessToken: %v", err)
	}
	if access != "ACCESS-TOKEN-2" {
		t.Errorf("access token = %q, want ACCESS-TOKEN-2", access)
	}
	if got != rotated {
		t.Errorf("rotated refresh token = %q, want %q. Microsoft rotates on redemption for personal "+
			"accounts; dropping the new one kills the mailbox when the old one ages out (D8)", got, rotated)
	}
	if form := auth.form(); len(form["refresh_token"]) == 0 || form["refresh_token"][0] != msxRefreshSentinel {
		t.Errorf("the stored refresh token was not the one redeemed: %v", form["refresh_token"])
	}
}

func TestMintAccessToken_FailurePathsNeverRenderTheSecret(t *testing.T) {
	cases := []struct {
		name     string
		reply    msxTokenReply
		wantCode string
	}{
		{"revoked consent", msxTokenReply{http.StatusBadRequest, map[string]any{
			"error":             "invalid_grant",
			"error_description": "AADSTS700082: The refresh token has expired due to inactivity.",
		}}, "invalid_grant"},
		{"wrong client id", msxTokenReply{http.StatusBadRequest, map[string]any{
			"error":             "unauthorized_client",
			"error_description": "AADSTS700016: Application not found in the directory.",
		}}, "unauthorized_client"},
		{"server error", msxTokenReply{http.StatusInternalServerError, map[string]any{
			"error": "temporarily_unavailable",
		}}, "temporarily_unavailable"},
		// The case scrubSecrets exists for. x/oauth2's RetrieveError carries the
		// raw response body, and a provider is free to echo the credential back
		// in it. Without a reply that actually does so, every leak assertion in
		// this file passes on a fixture that could never have leaked, and making
		// scrubSecrets the identity function would leave the suite green.
		{"the provider echoes the token back", msxTokenReply{http.StatusBadRequest, map[string]any{
			"error":             "invalid_grant",
			"error_description": "AADSTS70008: the refresh token " + msxRefreshSentinel + " has expired.",
		}}, "invalid_grant"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			auth := newMSXAuthority(t, 1, tc.reply)
			cfg := msxConfig(t, auth.srv.URL)

			access, rotated, err := google.MintAccessToken(context.Background(), cfg, msxRefreshSentinel)
			if err == nil {
				t.Fatalf("MintAccessToken returned no error for %s", tc.name)
			}
			if access != "" || rotated != "" {
				t.Errorf("a failed mint returned access=%q rotated=%q; both must be empty so nothing "+
					"downstream stores half a credential", access, rotated)
			}
			if !strings.Contains(err.Error(), tc.wantCode) {
				t.Errorf("error %q does not carry the cause class %q; D9's per-account sync_runs message "+
					"is built from it and \"re-run google-auth add-microsoft\" is the action it must imply",
					err, tc.wantCode)
			}
			// Criterion 6: this string reaches stdout, the connector log and
			// sync_runs.stats.
			if strings.Contains(err.Error(), msxRefreshSentinel) {
				t.Errorf("the refresh token is rendered in the error: %v", err)
			}
			// …and so is any %#v-style dump of the config.
			if strings.Contains(err.Error(), msxClientID) && strings.Contains(err.Error(), msxRefreshSentinel) {
				t.Errorf("the error dumps the whole oauth2 config: %v", err)
			}
		})
	}
}

// The sentinel must not survive into a wrapped error either — the realistic leak
// is fmt.Errorf("mint token for %s: %w", refreshToken, err) written in haste.
func TestMintAccessToken_NoSecretInAnyRenderedLayer(t *testing.T) {
	auth := newMSXAuthority(t, 1, msxTokenReply{http.StatusBadRequest, map[string]any{"error": "invalid_grant"}})
	cfg := msxConfig(t, auth.srv.URL)

	_, _, err := google.MintAccessToken(context.Background(), cfg, msxRefreshSentinel)
	if err == nil {
		t.Fatalf("expected an error")
	}
	for _, rendered := range []string{
		err.Error(),
		fmt.Sprintf("%v", err),
		fmt.Sprintf("%+v", err),
		fmt.Sprintf("%s", err),
	} {
		if strings.Contains(rendered, msxRefreshSentinel) {
			t.Errorf("the refresh token appears in a rendered error: %q", rendered)
		}
	}
}
