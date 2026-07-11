package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"golang.org/x/oauth2"
)

// Auth is session auth for the dashboard: Keycloak OIDC (code flow) when
// OIDC_ISSUER is configured, else a local dev-login stub (rag-svc idiom).
// Sessions are in-memory (single-user workstation tool; step 10 revisits).
type Auth struct {
	oidc     *oauth2.Config
	userInfo string // userinfo endpoint
	dev      bool

	mu       sync.Mutex
	sessions map[string]string // cookie value -> user identity
}

const sessionCookie = "sb_session"

// NewAuth configures OIDC from env or falls back to dev mode.
func NewAuth(ctx context.Context, issuer, clientID, clientSecret, redirectURL string) (*Auth, error) {
	a := &Auth{sessions: map[string]string{}}
	if issuer == "" {
		a.dev = true
		return a, nil
	}

	var disc struct {
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
		UserinfoEndpoint      string `json:"userinfo_endpoint"`
	}
	resp, err := http.Get(issuer + "/.well-known/openid-configuration")
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery: %w", err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&disc); err != nil {
		return nil, fmt.Errorf("parse OIDC discovery: %w", err)
	}
	a.oidc = &oauth2.Config{
		ClientID: clientID, ClientSecret: clientSecret,
		Endpoint:    oauth2.Endpoint{AuthURL: disc.AuthorizationEndpoint, TokenURL: disc.TokenEndpoint},
		RedirectURL: redirectURL,
		Scopes:      []string{"openid", "email", "profile"},
	}
	a.userInfo = disc.UserinfoEndpoint
	return a, nil
}

// Routes mounts the auth endpoints.
func (a *Auth) Routes(mux *http.ServeMux) {
	if a.dev {
		mux.HandleFunc("GET /dev/login", func(w http.ResponseWriter, r *http.Request) {
			user := r.URL.Query().Get("user")
			if user == "" {
				user = "salvo"
			}
			a.setSession(w, user)
			http.Redirect(w, r, "/deliveries", http.StatusFound)
		})
		return
	}
	mux.HandleFunc("GET /auth/login", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, a.oidc.AuthCodeURL("sb-state"), http.StatusFound)
	})
	mux.HandleFunc("GET /auth/callback", func(w http.ResponseWriter, r *http.Request) {
		tok, err := a.oidc.Exchange(r.Context(), r.URL.Query().Get("code"))
		if err != nil {
			http.Error(w, "exchange failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		user, err := a.fetchUser(r.Context(), tok)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		a.setSession(w, user)
		http.Redirect(w, r, "/deliveries", http.StatusFound)
	})
}

func (a *Auth) fetchUser(ctx context.Context, tok *oauth2.Token) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.userInfo, nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("userinfo: %w", err)
	}
	defer resp.Body.Close()
	var ui struct {
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ui); err != nil {
		return "", fmt.Errorf("parse userinfo: %w", err)
	}
	if ui.Email != "" {
		return ui.Email, nil
	}
	return ui.PreferredUsername, nil
}

func (a *Auth) setSession(w http.ResponseWriter, user string) {
	buf := make([]byte, 24)
	_, _ = rand.Read(buf)
	val := hex.EncodeToString(buf)
	a.mu.Lock()
	a.sessions[val] = user
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: val, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

// User returns the session identity ("" when unauthenticated).
func (a *Auth) User(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[c.Value]
}

// Require gates a handler on a live session.
func (a *Auth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.User(r) == "" {
			if a.dev {
				http.Redirect(w, r, "/dev/login", http.StatusFound)
			} else {
				http.Redirect(w, r, "/auth/login", http.StatusFound)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}
