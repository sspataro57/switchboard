package main

// ---- route-candidates (SWT-40 Part B, B8) --------------------------------------
//
// `opsctl route-candidates add|remove|list`. The two writes are executor tool
// calls (route_candidate_add / route_candidate_remove, humanOnly, audited as
// opsctl:$USER) reached through the same path as create-task; `list` is a read.
// A candidate row authorises the routing tier to move a mailbox's unmatched mail
// into that project (B-D1), so "who changed it and when" is an audit row.
//
// Arming is deliberately NOT a verb here: an account starts routing only when a
// human sets source_accounts.route_after by hand, after the shadow reads and the
// eval gate (B-D7). See docs/runbooks/local-classifier.md, "Routing lane".

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/sspataro57/switchboard/internal/store"
)

const routeCandidatesListTimeout = 30 * time.Second

const routeProviderUsage = "the account's source_accounts.provider (e.g. google); needed when the address exists " +
	"under several providers"

// routeProviderFlag adds --provider to the flag set. It returns a function that
// sets payload["provider"] only when --provider was passed, and refuses an
// explicitly empty one rather than dropping it (a dropped provider would fall
// back to the tool's single-match rule, hiding the caller's typo).
func routeProviderFlag(fs *flag.FlagSet) func(payload map[string]any) error {
	provider := fs.String("provider", "", routeProviderUsage)
	return func(payload map[string]any) error {
		set := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "provider" {
				set = true
			}
		})
		if !set {
			return nil
		}
		if strings.TrimSpace(*provider) == "" {
			return fmt.Errorf("--provider, when given, must be non-empty")
		}
		payload["provider"] = strings.TrimSpace(*provider)
		return nil
	}
}

// parseRouteCandidateAdd builds the route_candidate_add call. It checks only
// presence; the TOOL refuses an unknown or ambiguous account (an address under
// several providers needs --provider), an unknown project, an empty
// description, a listed project and a second default.
func parseRouteCandidateAdd(argv []string) (string, json.RawMessage, error) {
	fs := flag.NewFlagSet("route-candidates add", flag.ContinueOnError)
	account := fs.String("account", "", "the receiving account's account_email (required)")
	project := fs.String("project", "", "project slug (required)")
	description := fs.String("description", "", "what this project covers, shown to the model on the candidate's line (required)")
	isDefault := fs.Bool("default", false, "the account's default: where a message lands when the model makes no grounded choice (at most one)")
	withProvider := routeProviderFlag(fs)
	if err := fs.Parse(argv); err != nil {
		return "", nil, err
	}
	if *account == "" || *project == "" || *description == "" {
		return "", nil, fmt.Errorf("--account, --project and --description are required")
	}
	payload := map[string]any{"account_email": *account, "project": *project, "description": *description}
	if *isDefault {
		payload["is_default"] = true
	}
	if err := withProvider(payload); err != nil {
		return "", nil, err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", nil, fmt.Errorf("marshal args: %w", err)
	}
	return "route_candidate_add", raw, nil
}

// parseRouteCandidateRemove builds the route_candidate_remove call. Removing a
// candidate that does not exist is the tool's error, never a silent no-op.
func parseRouteCandidateRemove(argv []string) (string, json.RawMessage, error) {
	fs := flag.NewFlagSet("route-candidates remove", flag.ContinueOnError)
	account := fs.String("account", "", "the receiving account's account_email (required)")
	project := fs.String("project", "", "project slug (required)")
	withProvider := routeProviderFlag(fs)
	if err := fs.Parse(argv); err != nil {
		return "", nil, err
	}
	if *account == "" || *project == "" {
		return "", nil, fmt.Errorf("--account and --project are required")
	}
	payload := map[string]any{"account_email": *account, "project": *project}
	if err := withProvider(payload); err != nil {
		return "", nil, err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", nil, fmt.Errorf("marshal args: %w", err)
	}
	return "route_candidate_remove", raw, nil
}

// runRouteCandidatesList prints every account's candidate set, numbered in the
// order the prompt numbers them, with the account's arming state: SHADOW
// (route_after unset: verdicts only, nothing written) or armed since when.
func runRouteCandidatesList(argv []string) error {
	fs := flag.NewFlagSet("route-candidates list", flag.ContinueOnError)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), routeCandidatesListTimeout)
	defer cancel()
	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	rows, err := pool.Query(ctx, `
		SELECT sa.id, sa.provider, sa.account_email, sa.route_after, p.slug, sap.is_default, sap.description
		  FROM source_account_projects sap
		  JOIN source_accounts sa ON sa.id = sap.source_account_id
		  JOIN projects p ON p.id = sap.project_id
		 ORDER BY sa.account_email, sa.id, sap.id`)
	if err != nil {
		return fmt.Errorf("select route candidates: %w", err)
	}
	defer rows.Close()

	var lastAccount int64
	n, index := 0, 0
	for rows.Next() {
		var accountID int64
		var provider, email, slug, description string
		var armedAt *time.Time
		var isDefault bool
		if err := rows.Scan(&accountID, &provider, &email, &armedAt, &slug, &isDefault, &description); err != nil {
			return fmt.Errorf("scan route candidate: %w", err)
		}
		if accountID != lastAccount {
			state := "SHADOW (route_after unset: verdicts only, nothing is routed)"
			if armedAt != nil {
				state = "armed since " + armedAt.UTC().Format(time.RFC3339)
			}
			fmt.Printf("%s (%s, account %d) — %s\n", email, provider, accountID, state)
			lastAccount, index = accountID, 0
		}
		index++
		n++
		def := ""
		if isDefault {
			def = " [default]"
		}
		fmt.Printf("  %d. %s%s: %s\n", index, slug, def, description)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate route candidates: %w", err)
	}
	if n == 0 {
		fmt.Println("no route candidates")
	}
	return nil
}
