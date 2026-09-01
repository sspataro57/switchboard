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
