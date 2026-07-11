// google-auth manages the provider='google' source accounts (SPEC
// 07-google-oauth-pollers): the Desktop-app loopback OAuth flow per account,
// refresh tokens pgcrypto-encrypted at rest. Trusted spine (writes
// source_accounts directly, like connectors do) — deliberately NOT an opsctl
// subcommand, which stays a pure executor client.
//
//	google-auth add <email> [--no-availability]
//	google-auth list
//
//	DATABASE_URL               ops db, required
//	OPS_TOKEN_KEY              pgcrypto key, required for add
//	GOOGLE_CLIENT_SECRET_FILE  default ~/.config/switchboard/google_client_secret.json
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: google-auth <add|list> [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "add":
		err = addCmd(os.Args[2:])
	case "list":
		err = listCmd()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "google-auth:", err)
		os.Exit(1)
	}
}

func addCmd(argv []string) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	noAvail := fs.Bool("no-availability", false, "exclude this account's calendar from availability")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: google-auth add <email> [--no-availability]")
	}
	email := strings.ToLower(fs.Arg(0))

	key := os.Getenv("OPS_TOKEN_KEY")
	if key == "" {
		return fmt.Errorf("OPS_TOKEN_KEY is not set (generate once: openssl rand -base64 32)")
	}
	secretFile := os.Getenv("GOOGLE_CLIENT_SECRET_FILE")
	if secretFile == "" {
		home, _ := os.UserHomeDir()
		secretFile = filepath.Join(home, ".config", "switchboard", "google_client_secret.json")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cfg, err := google.LoadOAuthConfig(secretFile, "")
	if err != nil {
		return err
	}
	tok, err := google.LoopbackFlow(ctx, cfg)
	if err != nil {
		return err
	}

	// Verify the authorized identity — five accounts in one browser is exactly
	// where the wrong one gets clicked.
	hc := &http.Client{Transport: &tokenTransport{token: tok.AccessToken}}
	gc := google.NewGmailClient(hc, "", email)
	got, err := gc.GetProfile(ctx)
	if err != nil {
		return fmt.Errorf("verify authorized identity: %w", err)
	}
	if !strings.EqualFold(got, email) {
		return fmt.Errorf("authorized account is %s, expected %s — nothing stored; retry and pick the right account", got, email)
	}

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	id, err := google.UpsertGoogleAccount(ctx, pool, email, tok.RefreshToken, key, google.ReadonlyScopes, !*noAvail)
	if err != nil {
		return err
	}
	fmt.Printf("authorized %s (account id %d, readonly scopes, send_enabled=false)\n", email, id)
	return nil
}

type tokenTransport struct{ token string }

func (t *tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(r)
}

func listCmd() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	rows, err := pool.Query(ctx,
		`SELECT id, account_email, scopes, send_enabled, calendar_in_availability,
		        COALESCE(sync_cursor::text,'{}')
		 FROM source_accounts WHERE provider='google' ORDER BY id`)
	if err != nil {
		return fmt.Errorf("select accounts: %w", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id int64
		var email, cursor string
		var scopes []string
		var sendEnabled, avail bool
		if err := rows.Scan(&id, &email, &scopes, &sendEnabled, &avail, &cursor); err != nil {
			return fmt.Errorf("scan account: %w", err)
		}
		n++
		fmt.Printf("%-4d %-40s send=%v availability=%v scopes=%d cursor=%s\n",
			id, email, sendEnabled, avail, len(scopes), cursor)
	}
	if n == 0 {
		fmt.Println("no google accounts (run google-auth add <email>)")
	}
	return rows.Err()
}
