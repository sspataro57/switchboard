// jira-auth manages provider='jira' source accounts (SPEC
// 09-jira-github-connectors): site URL + basic-auth API token (pgcrypto at
// rest) + mandatory project scoping. Trusted spine, like google-auth.
//
//	jira-auth add <email> --site https://x.atlassian.net --projects KEY1,KEY2 [--lookup-only]
//	jira-auth list
//
// --lookup-only (SWT-32) stores the row as provider='jira_lookup': a
// credential the ticket-status reconciler fetches candidate issues with, and
// nothing else — never JQL-polled, never normalized, invisible to every
// provider='jira' query by construction. --projects stays REQUIRED for it: the
// declared prefixes are the only keys the token may ever fetch.
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
	lookupOnly := fs.Bool("lookup-only", false, "store as provider='jira_lookup': fetch-by-key only, never polled, never normalized (SWT-32)")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if email == "" && fs.NArg() == 1 {
		email = strings.ToLower(fs.Arg(0))
	}
	if email == "" || *site == "" || *projects == "" {
		if *lookupOnly && *projects == "" {
			// The scoping IS the boundary (SWT-32 D18): the declared prefixes are
			// the only keys the reconciler may route to this token.
			return fmt.Errorf("a lookup-only account with no --projects could fetch any issue on the " +
				"site; declare the project prefixes it may fetch (e.g. --projects LHH)")
		}
		return fmt.Errorf("usage: jira-auth add <email> --site URL --projects KEY1,KEY2 [--lookup-only]")
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

	provider := "jira"
	if *lookupOnly {
		provider = "jira_lookup"
	}
	var id int64
	err = pool.QueryRow(ctx,
		`INSERT INTO source_accounts
		   (provider, account_email, refresh_token_encrypted, scopes, send_enabled, domain_default)
		 VALUES ($6, $1, pgp_sym_encrypt($2, $3), $4, false, $5)
		 ON CONFLICT (provider, account_email) DO UPDATE SET
		   refresh_token_encrypted = pgp_sym_encrypt($2, $3),
		   scopes = $4, domain_default = $5
		 RETURNING id`,
		email, token, key, keys, strings.TrimRight(*site, "/"), provider).Scan(&id)
	if err != nil {
		return fmt.Errorf("upsert jira account: %w", err)
	}
	fmt.Printf("%s account %s stored (id %d, accountId %s, projects %v)\n", provider, email, id, own, keys)
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

	// Both provider values, deliberately (SWT-32 criterion 12): a jira_lookup
	// account is invisible to every polling/normalizing query by construction,
	// so this list is the ONE place an operator can confirm it exists at all.
	rows, err := pool.Query(ctx,
		`SELECT id, provider, account_email, COALESCE(domain_default,''), scopes,
		        COALESCE(sync_cursor::text,'{}')
		 FROM source_accounts WHERE provider IN ('jira','jira_lookup') ORDER BY id`)
	if err != nil {
		return fmt.Errorf("select accounts: %w", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id int64
		var provider, email, site, cursor string
		var scopes []string
		if err := rows.Scan(&id, &provider, &email, &site, &scopes, &cursor); err != nil {
			return fmt.Errorf("scan account: %w", err)
		}
		n++
		role := "poll"
		if provider == "jira_lookup" {
			role = "lookup"
		}
		fmt.Printf("%-4d %-7s %-35s %-40s projects=%v cursor=%s\n", id, role, email, site, scopes, cursor)
	}
	if n == 0 {
		fmt.Println("no jira accounts (run jira-auth add)")
	}
	return rows.Err()
}
