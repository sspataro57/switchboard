package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"
	googleoauth "golang.org/x/oauth2/google"
)

// ReadonlyScopes is exactly the step-7 scope set. Step 8's re-consent extends
// this constant (gmail.send, calendar.events) and re-runs `google-auth add`.
var ReadonlyScopes = []string{
	"https://www.googleapis.com/auth/gmail.readonly",
	"https://www.googleapis.com/auth/calendar.readonly",
}

// LoadOAuthConfig reads the Desktop-app client secret file ("installed" JSON
// shape) and builds the oauth2 config for the loopback flow.
func LoadOAuthConfig(path string, redirectURL string) (*oauth2.Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read client secret file: %w", err)
	}
	var f struct {
		Installed struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
			AuthURI      string `json:"auth_uri"`
			TokenURI     string `json:"token_uri"`
		} `json:"installed"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse client secret file: %w", err)
	}
	if f.Installed.ClientID == "" {
		return nil, fmt.Errorf("client secret file has no installed.client_id (need a Desktop-app client)")
	}
	return &oauth2.Config{
		ClientID:     f.Installed.ClientID,
		ClientSecret: f.Installed.ClientSecret,
		Endpoint:     oauth2.Endpoint{AuthURL: f.Installed.AuthURI, TokenURL: f.Installed.TokenURI},
		Scopes:       ReadonlyScopes,
		RedirectURL:  redirectURL,
	}, nil
}

// LoopbackFlow runs the Desktop-app authorization: listens on a loopback
// port, prints the auth URL, waits for the code, exchanges it. Returns the
// token (refresh token included on first consent).
func LoopbackFlow(ctx context.Context, cfg *oauth2.Config) (*oauth2.Token, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen loopback: %w", err)
	}
	defer ln.Close()
	cfg.RedirectURL = fmt.Sprintf("http://%s/callback", ln.Addr().String())

	state := fmt.Sprintf("sb-%d", time.Now().UnixNano())
	authURL := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)
	fmt.Printf("Open this URL in your browser and authorize:\n\n  %s\n\n", authURL)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		if s := r.URL.Query().Get("state"); s != state {
			errCh <- fmt.Errorf("oauth state mismatch")
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			errCh <- fmt.Errorf("callback carried no code: %s", r.URL.RawQuery)
			http.Error(w, "no code", http.StatusBadRequest)
			return
		}
		fmt.Fprintln(w, "Authorized. You can close this tab.")
		codeCh <- code
	})}
	go srv.Serve(ln)
	defer srv.Close()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("authorization wait: %w", ctx.Err())
	case err := <-errCh:
		return nil, err
	case code := <-codeCh:
		tok, err := cfg.Exchange(ctx, code)
		if err != nil {
			return nil, fmt.Errorf("exchange code: %w", err)
		}
		if tok.RefreshToken == "" {
			return nil, fmt.Errorf("no refresh token returned (revoke prior consent and retry)")
		}
		return tok, nil
	}
}

// UpsertGoogleAccount writes exactly one provider='google' source_accounts
// row: refresh token pgcrypto-encrypted with key, scopes, send_enabled=false.
func UpsertGoogleAccount(ctx context.Context, pool *pgxpool.Pool,
	email, refreshToken, key string, scopes []string, calendarInAvailability bool) (int64, error) {
	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts
		   (provider, account_email, refresh_token_encrypted, scopes, send_enabled, calendar_in_availability)
		 VALUES ('google', $1, pgp_sym_encrypt($2, $3), $4, false, $5)
		 ON CONFLICT (provider, account_email) DO UPDATE SET
		   refresh_token_encrypted = pgp_sym_encrypt($2, $3),
		   scopes = $4,
		   calendar_in_availability = $5
		 RETURNING id`,
		email, refreshToken, key, scopes, calendarInAvailability).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert google account %s: %w", email, err)
	}
	return id, nil
}

// DecryptRefreshToken reads the pgcrypto-decrypted refresh token. The token
// must never be logged; callers hold it only to build a TokenSource.
func DecryptRefreshToken(ctx context.Context, pool *pgxpool.Pool, accountID int64, key string) (string, error) {
	var token string
	err := pool.QueryRow(ctx,
		`SELECT pgp_sym_decrypt(refresh_token_encrypted, $2) FROM source_accounts WHERE id=$1`,
		accountID, key).Scan(&token)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("account %d not found", accountID)
	}
	if err != nil {
		return "", fmt.Errorf("decrypt refresh token for account %d: %w", accountID, err)
	}
	return token, nil
}

// TokenClient builds the per-account authorized *http.Client from the stored
// refresh token. If Google rotates the refresh token it is re-encrypted and
// saved (persistence hook around the TokenSource).
func TokenClient(ctx context.Context, pool *pgxpool.Pool, cfg *oauth2.Config, acct Account, key string) (*http.Client, error) {
	refresh, err := DecryptRefreshToken(ctx, pool, acct.ID, key)
	if err != nil {
		return nil, err
	}
	base := cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: refresh})
	ts := &persistingTokenSource{
		ctx: ctx, pool: pool, base: base,
		accountID: acct.ID, key: key, lastRefresh: refresh,
		scopes: cfg.Scopes, availability: acct.CalendarInAvailability, email: acct.Email,
	}
	return oauth2.NewClient(ctx, ts), nil
}

type persistingTokenSource struct {
	ctx          context.Context
	pool         *pgxpool.Pool
	base         oauth2.TokenSource
	accountID    int64
	key          string
	lastRefresh  string
	scopes       []string
	availability bool
	email        string
}

func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.base.Token()
	if err != nil {
		return nil, err
	}
	if tok.RefreshToken != "" && tok.RefreshToken != p.lastRefresh {
		if _, err := UpsertGoogleAccount(p.ctx, p.pool, p.email, tok.RefreshToken, p.key, p.scopes, p.availability); err == nil {
			p.lastRefresh = tok.RefreshToken
		}
	}
	return tok, nil
}

// GoogleEndpoint is the production oauth2 endpoint (var for reference).
var GoogleEndpoint = googleoauth.Endpoint
