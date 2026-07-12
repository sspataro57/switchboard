// jira-auth manages provider='jira' source accounts (SPEC
// 09-jira-github-connectors): site URL + basic-auth API token (pgcrypto at
// rest) + mandatory project scoping. Trusted spine, like google-auth.
//
//	jira-auth add <email> --site https://x.atlassian.net --projects KEY1,KEY2
//	jira-auth list
//
//	DATABASE_URL     ops db, required
//	OPS_TOKEN_KEY    pgcrypto key, required for add
//	JIRA_API_TOKEN   the API token to store, required for add
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/jira"
	"github.com/sspataro57/switchboard/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: jira-auth <add|list> [flags]")
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
		fmt.Fprintln(os.Stderr, "jira-auth:", err)
		os.Exit(1)
	}
}

func addCmd(argv []string) error {
	// Accept the email positionally BEFORE the flags (Go's flag parsing stops
	// at the first non-flag argument).
	email := ""
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		email, argv = strings.ToLower(argv[0]), argv[1:]
	}
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	site := fs.String("site", "", "Jira site base URL (required)")
	projects := fs.String("projects", "", "comma-separated project keys to poll (required — unscoped polls are refused)")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if email == "" && fs.NArg() == 1 {
		email = strings.ToLower(fs.Arg(0))
	}
	if email == "" || *site == "" || *projects == "" {
		return fmt.Errorf("usage: jira-auth add <email> --site URL --projects KEY1,KEY2")
	}
	key := os.Getenv("OPS_TOKEN_KEY")
	if key == "" {
		return fmt.Errorf("OPS_TOKEN_KEY is not set")
	}
	token := os.Getenv("JIRA_API_TOKEN")
	if token == "" {
		return fmt.Errorf("JIRA_API_TOKEN is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Verify the credentials before storing anything.
	c := jira.NewClient(http.DefaultClient, *site, email, token)
	own, err := c.Myself(ctx)
	if err != nil {
		return fmt.Errorf("verify credentials against %s: %w", *site, err)
	}

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	keys := []string{}
	for _, k := range strings.Split(*projects, ",") {
		if k = strings.TrimSpace(strings.ToUpper(k)); k != "" {
			keys = append(keys, k)
		}
	}

	var id int64
	err = pool.QueryRow(ctx,
		`INSERT INTO source_accounts
		   (provider, account_email, refresh_token_encrypted, scopes, send_enabled, domain_default)
		 VALUES ('jira', $1, pgp_sym_encrypt($2, $3), $4, false, $5)
		 ON CONFLICT (provider, account_email) DO UPDATE SET
		   refresh_token_encrypted = pgp_sym_encrypt($2, $3),
		   scopes = $4, domain_default = $5
		 RETURNING id`,
		email, token, key, keys, strings.TrimRight(*site, "/")).Scan(&id)
	if err != nil {
		return fmt.Errorf("upsert jira account: %w", err)
	}
	fmt.Printf("jira account %s stored (id %d, accountId %s, projects %v)\n", email, id, own, keys)
	return nil
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
		`SELECT id, account_email, COALESCE(domain_default,''), scopes,
		        COALESCE(sync_cursor::text,'{}')
		 FROM source_accounts WHERE provider='jira' ORDER BY id`)
	if err != nil {
		return fmt.Errorf("select accounts: %w", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id int64
		var email, site, cursor string
		var scopes []string
		if err := rows.Scan(&id, &email, &site, &scopes, &cursor); err != nil {
			return fmt.Errorf("scan account: %w", err)
		}
		n++
		fmt.Printf("%-4d %-35s %-40s projects=%v cursor=%s\n", id, email, site, scopes, cursor)
	}
	if n == 0 {
		fmt.Println("no jira accounts (run jira-auth add)")
	}
	return rows.Err()
}
