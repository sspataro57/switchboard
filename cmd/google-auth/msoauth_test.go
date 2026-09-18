package main

// Offline unit tests for `google-auth add-microsoft` (SPEC
// docs/tickets/microsoft-oauth-mail_SPEC.md, acceptance criteria 5, 6 and D12's
// usage fix). ZERO network, ZERO Postgres: the device/token endpoints are an
// httptest server reached through MS_OAUTH_AUTHORITY, the D4.2 live IMAP check
// is an injected function, and the ONE write is an injected store.
//
// THE POINT OF THE INJECTION. Criterion 5's load-bearing sentence is "A claim
// mismatch, a missing claim or a failed IMAP check stores NOTHING". That is an
// ORDERING property — verify, verify, then write — and an ordering property is
// only testable if the write is observable. A mislabelled row poisons
// ownEmailSet and every thread key for that mailbox, which is why
// add-app-password and add both verify before storing and why this one verifies
// twice.
//
// EXPECTED FAILURE MODE: **compile error** — cmd/google-auth/msoauth.go does not
// exist ("undefined: runAddMicrosoft", "undefined: addMicrosoftCmd"). The D12
// usage test is different: it compiles TODAY against the existing
// addAppPasswordCmd and fails as an ASSERTION, because the current message
// documents an argument order Go's flag package cannot parse.
//
// IMPOSED SURFACE (cmd/google-auth/msoauth.go; the SPEC names the file and the
// subcommand but no signature):
//
//	type addMicrosoftOpts struct {
//	    Email    string
//	    TokenKey string          // OPS_TOKEN_KEY; never printed
//	    Out      io.Writer       // where the verification URL + user code go
//	    // VerifyIMAP is D4.2: open IMAP with the fresh access token and require a
//	    // non-empty SelectFolders set (apppassword.go's check, same words).
//	    VerifyIMAP func(ctx context.Context, hosts google.MailHosts, email, accessToken string) error
//	    // Store is the ONLY write. It is called at most once, and only after both
//	    // identity checks have passed.
//	    Store func(ctx context.Context, email, refreshToken string, scopes []string, hosts google.MailHosts) (int64, error)
//	}
//
//	func runAddMicrosoft(ctx context.Context, o addMicrosoftOpts) error
//	func addMicrosoftCmd(argv []string) error   // flag parsing + wiring only

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

const (
	msaEmail    = "sspataro57@msn.com"
	msaClientID = "11111111-2222-3333-4444-555555555555"
	msaRefresh  = "0.AXoA-SENTINEL-REFRESH-TOKEN-DO-NOT-LEAK"
	msaAccess   = "ya29.SENTINEL-ACCESS-TOKEN"
	msaKey      = "itest-msoauth-cli-token-key"
)

// msaIDToken builds an unsigned JWT. D4 decodes without verifying the signature
// — acceptable ONLY because the token came straight from the token endpoint over
// TLS in this process, which is what the code comment must say and nothing
// stronger.
func msaIDToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc(payload) + ".sig"
}

// msaAuthority serves the device-code and token endpoints. One poll, immediate
// success: the pending/slow_down behaviour is pinned in the package's own
// msoauth_test.go, and paying its wall-clock cost twice buys nothing.
func msaAuthority(t *testing.T, idToken string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/v2.0/devicecode", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":      "DEV-CODE-XYZ",
			"user_code":        "HJKL-MNPQ",
			"verification_uri": "https://microsoft.com/devicelogin",
			"expires_in":       300,
			"interval":         1,
		})
	})
	mux.HandleFunc("/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token_type":    "Bearer",
			"expires_in":    3600,
			"scope":         strings.Join(google.MicrosoftScopes, " "),
			"access_token":  msaAccess,
			"refresh_token": msaRefresh,
			"id_token":      idToken,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// msaRun drives the subcommand with recorded side effects.
type msaRun struct {
	out    bytes.Buffer
	steps  []string // the ORDER of the observable effects
	stored struct {
		called       bool
		email        string
		refreshToken string
		scopes       []string
		hosts        google.MailHosts
	}
	verifiedToken string
	verifyErr     error
}

func (r *msaRun) opts() addMicrosoftOpts {
	return addMicrosoftOpts{
		Email:    msaEmail,
		TokenKey: msaKey,
		Out:      &r.out,
		VerifyIMAP: func(_ context.Context, hosts google.MailHosts, email, accessToken string) error {
			r.steps = append(r.steps, "verify-imap")
			r.verifiedToken = accessToken
			return r.verifyErr
		},
		Store: func(_ context.Context, email, refreshToken string, scopes []string, hosts google.MailHosts) (int64, error) {
			r.steps = append(r.steps, "store")
			r.stored.called = true
			r.stored.email = email
			r.stored.refreshToken = refreshToken
			r.stored.scopes = scopes
			r.stored.hosts = hosts
			return 42, nil
		},
	}
}

func msaCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// ---- criterion 5: the happy path -----------------------------------------------

func TestRunAddMicrosoft_VerifiesTwiceThenStoresOneRow(t *testing.T) {
	srv := msaAuthority(t, msaIDToken(t, map[string]any{"preferred_username": msaEmail, "sub": "abc"}))
	t.Setenv("MS_OAUTH_CLIENT_ID", msaClientID)
	t.Setenv("MS_OAUTH_AUTHORITY", srv.URL)

	ctx, cancel := msaCtx(t)
	defer cancel()

	r := &msaRun{}
	if err := runAddMicrosoft(ctx, r.opts()); err != nil {
		t.Fatalf("runAddMicrosoft: %v", err)
	}

	printed := r.out.String()
	for _, want := range []string{"https://microsoft.com/devicelogin", "HJKL-MNPQ"} {
		if !strings.Contains(printed, want) {
			t.Errorf("the subcommand printed %q, which does not contain %q (criterion 5)", printed, want)
		}
	}

	// D4: the LIVE check runs before anything is stored, with the fresh access
	// token — not with the refresh token, which IMAP cannot use.
	if got := strings.Join(r.steps, ","); got != "verify-imap,store" {
		t.Errorf("effect order = %q, want verify-imap,store. Storing before the live check is how a mailbox "+
			"that cannot actually be opened ends up in the ingest set (D4.2)", got)
	}
	if r.verifiedToken != msaAccess {
		t.Errorf("the IMAP check was given %q, want the ACCESS token", r.verifiedToken)
	}
	if !r.stored.called {
		t.Fatalf("nothing was stored on the happy path")
	}
	if !strings.EqualFold(r.stored.email, msaEmail) {
		t.Errorf("stored email = %q, want %q", r.stored.email, msaEmail)
	}
	if r.stored.refreshToken != msaRefresh {
		t.Errorf("stored refresh token = %q, want the one the token endpoint returned", r.stored.refreshToken)
	}
	if len(r.stored.scopes) != len(google.MicrosoftScopes) {
		t.Errorf("stored scopes = %v, want %v", r.stored.scopes, google.MicrosoftScopes)
	}
	h := r.stored.hosts.WithDefaults()
	if h.IMAPHost != "outlook.office365.com" || h.IMAPPort != 993 {
		t.Errorf("stored IMAP endpoint = %s:%d, want outlook.office365.com:993 (D3) — the Gmail defaults "+
			"would silently point this mailbox at imap.gmail.com", h.IMAPHost, h.IMAPPort)
	}
	if h.SMTPHost != "smtp-mail.outlook.com" || h.SMTPPort != 587 {
		t.Errorf("stored SMTP endpoint = %s:%d, want smtp-mail.outlook.com:587 (D3)", h.SMTPHost, h.SMTPPort)
	}

	// Criterion 6: none of this reaches the terminal.
	for _, secret := range []string{msaRefresh, msaAccess, msaKey} {
		if strings.Contains(printed, secret) {
			t.Errorf("a secret reached stdout: %q", printed)
		}
	}
}

// ---- criterion 5: three ways to store NOTHING ----------------------------------

func TestRunAddMicrosoft_RefusalsStoreNothing(t *testing.T) {
	cases := []struct {
		name        string
		claims      map[string]any
		verifyErr   error
		wantInError []string
		wantSteps   string
	}{
		{
			name:   "the wrong account signed in",
			claims: map[string]any{"preferred_username": "someone.else@outlook.com", "sub": "abc"},
			// Picking the wrong account in a browser is the realistic mistake, and
			// a mislabelled row poisons the own-address set and every thread key.
			wantInError: []string{"someone.else@outlook.com", msaEmail},
			wantSteps:   "", // the claim check precedes the IMAP check
		},
		{
			name:        "neither preferred_username nor email is present",
			claims:      map[string]any{"sub": "abc", "aud": msaClientID},
			wantInError: []string{"sub"},
			wantSteps:   "",
		},
		{
			name:        "the mailbox cannot be opened",
			claims:      map[string]any{"preferred_username": msaEmail},
			verifyErr:   errIMAPCheckFailed,
			wantInError: []string{msaEmail},
			wantSteps:   "verify-imap",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			srv := msaAuthority(t, msaIDToken(t, tc.claims))
			t.Setenv("MS_OAUTH_CLIENT_ID", msaClientID)
			t.Setenv("MS_OAUTH_AUTHORITY", srv.URL)

			ctx, cancel := msaCtx(t)
			defer cancel()

			r := &msaRun{verifyErr: tc.verifyErr}
			err := runAddMicrosoft(ctx, r.opts())
			if err == nil {
				t.Fatalf("runAddMicrosoft succeeded for %q", tc.name)
			}
			if r.stored.called {
				t.Errorf("a row was stored despite %q. Criterion 5: a claim mismatch, a missing claim or a "+
					"failed IMAP check stores NOTHING — the row is what makes every in-cluster pass try to "+
					"resolve a credential for this mailbox", tc.name)
			}
			if got := strings.Join(r.steps, ","); got != tc.wantSteps {
				t.Errorf("effects = %q, want %q", got, tc.wantSteps)
			}
			for _, want := range tc.wantInError {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q — the operator has to know WHICH account "+
						"answered, or the retry is a guess", err, want)
				}
			}
			// Criterion 6, on the failure paths specifically.
			for _, secret := range []string{msaRefresh, msaAccess, msaKey} {
				if strings.Contains(err.Error(), secret) || strings.Contains(r.out.String(), secret) {
					t.Errorf("a secret leaked on the %q path: err=%v out=%q", tc.name, err, r.out.String())
				}
			}
		})
	}
}

// errIMAPCheckFailed stands in for what apppassword.go's check returns when the
// mailbox lists no INBOX/Sent folder.
var errIMAPCheckFailed = &msaError{"IMAP login succeeded but no INBOX/Sent folder was found"}

type msaError struct{ s string }

func (e *msaError) Error() string { return e.s }

// ---- D12: the usage strings document an order `flag` can actually parse -------

func TestUsageStringsPutFlagsBeforeThePositionalArgument(t *testing.T) {
	// Go's flag package STOPS parsing at the first non-flag argument, so
	// `add-app-password <email> --imap-host H` silently produces a usage error.
	// Salvador hit exactly this. The message is the sentence that misled him.
	cases := []struct {
		name string
		run  func() error
	}{
		{"add-app-password", func() error { return addAppPasswordCmd(nil) }},
		{"add-microsoft", func() error { return addMicrosoftCmd(nil) }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatalf("%s with no arguments returned no usage error", tc.name)
			}
			usage := err.Error()
			if !strings.Contains(usage, "usage:") {
				t.Fatalf("%s: %q is not a usage message", tc.name, usage)
			}
			flagAt := strings.Index(usage, "[--")
			emailAt := strings.Index(usage, "<email>")
			if emailAt < 0 {
				t.Fatalf("%s usage %q does not show the positional <email>", tc.name, usage)
			}
			if flagAt < 0 {
				t.Skipf("%s takes no flags, so there is no order to document", tc.name)
			}
			if emailAt < flagAt {
				t.Errorf("%s usage documents `<email>` BEFORE the flags: %q. Go's flag package stops "+
					"parsing at the first non-flag argument, so the documented order silently produces a "+
					"usage error (D12)", tc.name, usage)
			}
		})
	}
}
