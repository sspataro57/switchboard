package tools

// SWT-40 Part B route-candidate tools (docs/tickets/inquiry-promote_SPEC.md,
// B-D1, B8, "API / MCP tool changes"): route_candidate_add and
// route_candidate_remove are the ONLY writers of source_account_projects, the
// routing tier's closed candidate set per receiving account. So audit_events +
// policy_decisions answer "who authorised routing this mailbox into that
// project, and when" for every row.
//
// The same two deliberate omissions as capture_rule_add, for the same reason:
//
//   - Neither tool is listed in internal/mcpserver/schemas.go. A candidate row
//     is the AUTHORISATION to move a message into a project whose ai_locality
//     may be wider than its origin (IK SWT-21); an agent that could add one
//     could steer a mailbox's traffic into a project of its choosing.
//   - Both are in policy.humanOnly, so only dashboard:/opsctl:/manual: actors
//     pass. The MCP allowlist and the actor gate are independent defences.
//
// Arming (source_accounts.route_after) is NOT a tool: it is a hand-run UPDATE
// after the shadow reads and the eval gate (B-D7), and no code path sets it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---- route_candidate_add ------------------------------------------------------

type routeCandidateAddArgs struct {
	AccountEmail string `json:"account_email"` // source_accounts.account_email
	Project      string `json:"project"`       // projects.slug
	Description  string `json:"description"`   // reaches the prompt (B-D4)
	IsDefault    bool   `json:"is_default,omitempty"`
	// Provider narrows account_email to one source_accounts row when the address
	// exists under several providers (B8 amendment 2026-09-13). A pointer so an
	// explicit "" is refused rather than read as absent.
	Provider *string `json:"provider,omitempty"`
}

// parseRouteProvider trims an optional provider. Absent → "" (no narrowing);
// present but empty after trimming → refused. There is no hard-coded provider
// list: the database decides which providers exist for the address.
func parseRouteProvider(p *string) (string, error) {
	if p == nil {
		return "", nil
	}
	v := strings.TrimSpace(*p)
	if v == "" {
		return "", errors.New("provider, when given, must be non-empty (a source_accounts.provider, e.g. google)")
	}
	return v, nil
}

// parseRouteCandidateAdd applies every check that needs no database, so
// Validate and the handler share ONE spelling (the handler re-runs it: a
// handler never trusts that Validate ran). The description is trimmed: it is
// shown to the model on the candidate's numbered line, and a whitespace-only
// one would be a candidate the model cannot tell apart from the others.
func parseRouteCandidateAdd(args []byte) (routeCandidateAddArgs, error) {
	var a routeCandidateAddArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return a, fmt.Errorf("parse args: %w", err)
	}
	a.AccountEmail = strings.TrimSpace(a.AccountEmail)
	a.Project = strings.TrimSpace(a.Project)
	a.Description = strings.TrimSpace(a.Description)
	if a.AccountEmail == "" {
		return a, errors.New("missing account_email")
	}
	if a.Project == "" {
		return a, errors.New("missing project (a projects.slug)")
	}
	if a.Description == "" {
		return a, errors.New("missing description: it is what the model reads on the candidate's numbered line " +
			"(B-D4), so an empty one is refused")
	}
	provider, err := parseRouteProvider(a.Provider)
	if err != nil {
		return a, err
	}
	if a.Provider != nil {
		a.Provider = &provider
	}
	return a, nil
}

// routeProvider is the trimmed provider after parsing, "" when absent.
func routeProvider(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func validateRouteCandidateAdd(args []byte) error {
	_, err := parseRouteCandidateAdd(args)
	return err
}

// routeCandidateAdd inserts one source_account_projects row. Refused: an
// unknown or ambiguous account, an unknown project, a project the account
// already lists, and a second default for one account. The partial unique
// index and the (account, project) UNIQUE are the backstops; the checks here
// exist so the error names the field rather than a constraint.
func routeCandidateAdd(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	a, err := parseRouteCandidateAdd(args)
	if err != nil {
		return nil, err
	}
	accountID, err := resolveRouteAccount(ctx, pool, a.AccountEmail, routeProvider(a.Provider))
	if err != nil {
		return nil, err
	}
	projectID, err := resolveRouteProject(ctx, pool, a.Project)
	if err != nil {
		return nil, err
	}

	var listed bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM source_account_projects WHERE source_account_id = $1 AND project_id = $2)`,
		accountID, projectID).Scan(&listed); err != nil {
		return nil, fmt.Errorf("check existing candidate: %w", err)
	}
	if listed {
		return nil, fmt.Errorf("account %s already lists project %q as a route candidate; remove it first to change "+
			"its description or default", a.AccountEmail, a.Project)
	}
	if a.IsDefault {
		var current string
		err := pool.QueryRow(ctx,
			`SELECT p.slug FROM source_account_projects sap JOIN projects p ON p.id = sap.project_id
			  WHERE sap.source_account_id = $1 AND sap.is_default`, accountID).Scan(&current)
		switch {
		case err == nil:
			return nil, fmt.Errorf("account %s already has a default route candidate (%q); an account has at most "+
				"one default (O3), so remove that one first", a.AccountEmail, current)
		case !errors.Is(err, pgx.ErrNoRows):
			return nil, fmt.Errorf("check existing default: %w", err)
		}
	}

	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_account_projects (source_account_id, project_id, is_default, description)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		accountID, projectID, a.IsDefault, a.Description).Scan(&id); err != nil {
		return nil, fmt.Errorf("insert route candidate (one row per account+project, at most one default per "+
			"account): %w", err)
	}
	return marshalResult(map[string]any{
		"candidate_id": id, "source_account_id": accountID, "project_id": projectID, "is_default": a.IsDefault,
	})
}

// ---- route_candidate_remove ---------------------------------------------------

type routeCandidateRemoveArgs struct {
	AccountEmail string  `json:"account_email"`
	Project      string  `json:"project"`
	Provider     *string `json:"provider,omitempty"` // as on add
}

func parseRouteCandidateRemove(args []byte) (routeCandidateRemoveArgs, error) {
	var a routeCandidateRemoveArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return a, fmt.Errorf("parse args: %w", err)
	}
	a.AccountEmail = strings.TrimSpace(a.AccountEmail)
	a.Project = strings.TrimSpace(a.Project)
	if a.AccountEmail == "" {
		return a, errors.New("missing account_email")
	}
	if a.Project == "" {
		return a, errors.New("missing project (a projects.slug)")
	}
	provider, err := parseRouteProvider(a.Provider)
	if err != nil {
		return a, err
	}
	if a.Provider != nil {
		a.Provider = &provider
	}
	return a, nil
}

func validateRouteCandidateRemove(args []byte) error {
	_, err := parseRouteCandidateRemove(args)
	return err
}

// routeCandidateRemove deletes one candidate row. A candidate that does not
// exist is an ERROR, never a silent no-op: this is routing configuration typed
// by a human, and a no-op on a mistyped slug is indistinguishable from a
// removal (capture_rule_set_enabled's argument).
//
// Route rows already written stay: they are decisions, and capture_decisions
// is an append-only log. Removing a candidate only stops FUTURE routing into
// that project.
func routeCandidateRemove(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	a, err := parseRouteCandidateRemove(args)
	if err != nil {
		return nil, err
	}
	accountID, err := resolveRouteAccount(ctx, pool, a.AccountEmail, routeProvider(a.Provider))
	if err != nil {
		return nil, err
	}
	projectID, err := resolveRouteProject(ctx, pool, a.Project)
	if err != nil {
		return nil, err
	}
	var id int64
	err = pool.QueryRow(ctx,
		`DELETE FROM source_account_projects WHERE source_account_id = $1 AND project_id = $2 RETURNING id`,
		accountID, projectID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("account %s does not list project %q as a route candidate", a.AccountEmail, a.Project)
	}
	if err != nil {
		return nil, fmt.Errorf("delete route candidate: %w", err)
	}
	return marshalResult(map[string]any{"removed": true, "candidate_id": id})
}

// ---- shared resolution --------------------------------------------------------

// resolveRouteAccount maps an account email (and an optional provider) to ONE
// source_accounts row. source_accounts is unique on (provider, account_email),
// so one address can name accounts under several providers (prod:
// salvador@handsonconnect.org is google, jira and jira_lookup).
//
//   - provider given: (lower(account_email), provider) must match exactly; no
//     match is refused, naming the providers that DO exist for the address. It
//     never falls back to the address's only account: a caller who named jira
//     must not get the google mailbox.
//   - provider absent: exactly one match proceeds; several are AMBIGUOUS and
//     refused rather than guessed (a candidate on the wrong account routes the
//     wrong mailbox), and the error says to pass provider.
//
// The email is matched case-insensitively (typed by a human); the provider
// exactly, and against the database's own values: no hard-coded list.
func resolveRouteAccount(ctx context.Context, pool *pgxpool.Pool, email, provider string) (int64, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, provider FROM source_accounts WHERE lower(account_email) = lower($1) ORDER BY provider, id`, email)
	if err != nil {
		return 0, fmt.Errorf("resolve account %q: %w", email, err)
	}
	defer rows.Close()
	var ids []int64
	var providers []string
	for rows.Next() {
		var id int64
		var provider string
		if err := rows.Scan(&id, &provider); err != nil {
			return 0, fmt.Errorf("scan account: %w", err)
		}
		ids = append(ids, id)
		providers = append(providers, provider)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate accounts: %w", err)
	}
	if len(ids) == 0 {
		return 0, fmt.Errorf("no source account has account_email %q", email)
	}
	if provider != "" {
		for i, p := range providers {
			if p == provider {
				return ids[i], nil
			}
		}
		return 0, fmt.Errorf("account_email %q has no source account under provider %q; it exists under: %s",
			email, provider, strings.Join(providers, ", "))
	}
	if len(ids) == 1 {
		return ids[0], nil
	}
	return 0, fmt.Errorf("account_email %q names %d source accounts; ambiguous, refused: pass provider: one of %s",
		email, len(ids), strings.Join(providers, ", "))
}

func resolveRouteProject(ctx context.Context, pool *pgxpool.Pool, slug string) (int64, error) {
	var id int64
	err := pool.QueryRow(ctx, `SELECT id FROM projects WHERE slug = $1`, slug).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("project %q not found", slug)
	}
	if err != nil {
		return 0, fmt.Errorf("resolve project %q: %w", slug, err)
	}
	return id, nil
}
