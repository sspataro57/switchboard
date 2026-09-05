package google

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ListCalendarCredentialedAccounts returns every provider='google' account
// that can actually read a calendar: a non-NULL refresh_token_encrypted AND
// calendar.readonly in scopes.
//
// CREDENTIAL-gated, never auth_type-gated (SWT-24, "credential-gated, not
// auth_type-gated"): auth_type names the MAIL path (mailsender.go,
// ListAppPasswordAccounts) and must keep saying 'app_password' after a
// calendar consent, or mail breaks. A row holding both an app password and a
// refresh token is legitimately dual-auth — IMAP/SMTP for mail, OAuth for
// calendar — and is exactly the shape the consent flow produces.
func ListCalendarCredentialedAccounts(ctx context.Context, pool *pgxpool.Pool, onlyEmail string) ([]Account, error) {
	query := accountSelect + `
   AND refresh_token_encrypted IS NOT NULL
   AND $1 = ANY(scopes)`
	args := []any{CalendarScopes[0]}
	if strings.TrimSpace(onlyEmail) != "" {
		query += ` AND lower(account_email)=lower($2)`
		args = append(args, onlyEmail)
	}
	rows, err := pool.Query(ctx, query+` ORDER BY account_email`, args...)
	if err != nil {
		return nil, fmt.Errorf("list calendar-credentialed accounts: %w", err)
	}
	return scanAccounts(rows)
}

// ListAvailabilityScopeAccounts returns provider='google' AND
// calendar_in_availability rows — the SAME set availability.LoadBusy demands
// freshness for, so the polled set and the demanded set are one set by
// construction and the operator has ONE lever that moves both sides together.
//
// Deliberately NOT credential-gated (contrast ListCalendarCredentialedAccounts
// above): under the Pipedream transport (SWT-27) the Google credential lives
// at Pipedream and our rows keep neither a refresh token nor scopes. The
// equality of this predicate with the readiness scope is proven by an
// integration test (pipedream_integration_test.go, criterion 16), not by a
// shared constant — the provider half of the predicate lives inside
// accountSelect's WHERE, and a half-restated "shared" predicate is this repo's
// recurring defect.
func ListAvailabilityScopeAccounts(ctx context.Context, pool *pgxpool.Pool, onlyEmail string) ([]Account, error) {
	query := accountSelect + `
   AND calendar_in_availability`
	var args []any
	if strings.TrimSpace(onlyEmail) != "" {
		query += ` AND lower(account_email)=lower($1)`
		args = append(args, onlyEmail)
	}
	rows, err := pool.Query(ctx, query+` ORDER BY account_email`, args...)
	if err != nil {
		return nil, fmt.Errorf("list availability-scope accounts: %w", err)
	}
	return scanAccounts(rows)
}
