package google

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The send half of SWT-11: one tools.GmailSender that speaks SMTP, and one that
// picks a transport per account.
//
// Neither is a new tool and neither has a policy rule. send_delivery is still the
// only caller, it still refuses anything not 'approved', and it still reserves
// sent_external_id before the network call — nothing about invariant 4 moves
// because the transport changed.

// Auth types stored in source_accounts.auth_type.
const (
	AuthTypeOAuth       = "oauth"
	AuthTypeAppPassword = "app_password"
)

// SMTPSender submits through an account's own SMTP endpoint using its app
// password, decrypted per send and never retained.
type SMTPSender struct {
	Pool     *pgxpool.Pool
	TokenKey string
}

// Send implements tools.GmailSender.
//
// threadID is ignored: IMAP/SMTP has no Gmail thread id, and threading travels
// in the In-Reply-To/References headers BuildOutboundMIME already wrote. It is
// accepted only to satisfy the shared interface.
//
// The return value is the Message-ID the builder reserved, echoed back so the
// seam's contract holds; send_delivery discards it because it committed that id
// before calling.
func (s *SMTPSender) Send(ctx context.Context, fromUserID string, rawMIME []byte, _ string) (string, error) {
	acct, err := loadMailAccount(ctx, s.Pool, fromUserID)
	if err != nil {
		return "", err
	}
	// These are DEFINITE non-sends: nothing reached the network, so they are
	// rejections rather than ambiguous outcomes. Returning them untyped would make
	// send_delivery keep the reserved Message-ID, leaving a misconfigured account's
	// delivery stuck at 'failed' and unretriable without hand-written SQL.
	if acct.AuthType != AuthTypeAppPassword {
		return "", &SendRejectedError{Body: fmt.Sprintf("account %s is %s, not app_password", fromUserID, acct.AuthType)}
	}
	password, err := DecryptAppPassword(ctx, s.Pool, acct.ID, s.TokenKey)
	if err != nil {
		return "", &SendRejectedError{Body: err.Error()}
	}
	if password == "" {
		return "", &SendRejectedError{Body: fmt.Sprintf("account %s has no app password stored", fromUserID)}
	}

	hosts := acct.Hosts.WithDefaults()
	cfg := SMTPConfig{
		Host:     hosts.SMTPHost,
		Port:     hosts.SMTPPort,
		Username: acct.Email,
		Password: password,
		// 465 is implicit TLS by convention; everything else negotiates STARTTLS.
		ImplicitTLS: hosts.SMTPPort == 465,
	}
	if err := SubmitSMTP(ctx, cfg, acct.Email, rawMIME); err != nil {
		return "", err
	}
	return messageIDFromMIME(rawMIME), nil
}

// MailSender routes to the right transport for the sending account (criterion 13).
//
// The decision is the account's stored auth_type, never configuration or a
// guess: a mailbox onboarded with an app password sends over SMTP, an OAuth
// mailbox keeps the existing Gmail-API path byte for byte. Both can coexist
// during a migration, which is the point.
type MailSender struct {
	Pool     *pgxpool.Pool
	TokenKey string
	// SMTP handles app_password accounts; OAuth handles the rest. Either may be
	// nil, in which case an account needing it is an error rather than a silent
	// non-send.
	SMTP  *SMTPSender
	OAuth interface {
		Send(ctx context.Context, fromUserID string, rawMIME []byte, threadID string) (string, error)
	}
}

// Send implements tools.GmailSender.
func (m *MailSender) Send(ctx context.Context, fromUserID string, rawMIME []byte, threadID string) (string, error) {
	acct, err := loadMailAccount(ctx, m.Pool, fromUserID)
	if err != nil {
		// Unresolvable sending account: nothing was submitted, so the reservation
		// must be released rather than stranded.
		return "", &SendRejectedError{Body: err.Error()}
	}
	switch acct.AuthType {
	case AuthTypeAppPassword:
		if m.SMTP == nil {
			return "", &SendRejectedError{Body: fmt.Sprintf("account %s is app_password but no SMTP sender is configured", fromUserID)}
		}
		return m.SMTP.Send(ctx, fromUserID, rawMIME, threadID)
	default:
		if m.OAuth == nil {
			return "", &SendRejectedError{Body: fmt.Sprintf("account %s is %s but no OAuth sender is configured", fromUserID, acct.AuthType)}
		}
		return m.OAuth.Send(ctx, fromUserID, rawMIME, threadID)
	}
}

// mailAccount is the send-side view of a source_accounts row.
type mailAccount struct {
	ID       int64
	Email    string
	AuthType string
	Hosts    MailHosts
}

// loadMailAccount resolves the sending mailbox. fromUserID is the account email
// (that is what splitGmailThreadKey hands send_delivery), and "me" is accepted
// only when exactly one google account exists, so it can never silently pick a
// mailbox on a multi-account deployment.
func loadMailAccount(ctx context.Context, pool *pgxpool.Pool, fromUserID string) (mailAccount, error) {
	if pool == nil {
		return mailAccount{}, errors.New("mail sender has no database pool")
	}
	var a mailAccount
	var imapHost, smtpHost *string
	var imapPort, smtpPort *int

	query := `SELECT id, account_email, COALESCE(auth_type,'oauth'),
	                 imap_host, imap_port, smtp_host, smtp_port
	            FROM source_accounts
	           WHERE provider='google' AND lower(account_email)=lower($1)`
	args := []any{fromUserID}
	if strings.TrimSpace(fromUserID) == "" || fromUserID == "me" {
		query = `SELECT id, account_email, COALESCE(auth_type,'oauth'),
		                imap_host, imap_port, smtp_host, smtp_port
		           FROM source_accounts WHERE provider='google'`
		args = nil
	}

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return mailAccount{}, fmt.Errorf("resolve sending account %q: %w", fromUserID, err)
	}
	defer rows.Close()
	found := 0
	for rows.Next() {
		found++
		if found > 1 {
			return mailAccount{}, fmt.Errorf("sending account %q is ambiguous across %d google accounts", fromUserID, found)
		}
		if err := rows.Scan(&a.ID, &a.Email, &a.AuthType, &imapHost, &imapPort, &smtpHost, &smtpPort); err != nil {
			return mailAccount{}, fmt.Errorf("scan sending account %q: %w", fromUserID, err)
		}
	}
	if err := rows.Err(); err != nil {
		return mailAccount{}, fmt.Errorf("iterate sending account %q: %w", fromUserID, err)
	}
	if found == 0 {
		return mailAccount{}, fmt.Errorf("no provider='google' account for %q", fromUserID)
	}
	if imapHost != nil {
		a.Hosts.IMAPHost = *imapHost
	}
	if imapPort != nil {
		a.Hosts.IMAPPort = *imapPort
	}
	if smtpHost != nil {
		a.Hosts.SMTPHost = *smtpHost
	}
	if smtpPort != nil {
		a.Hosts.SMTPPort = *smtpPort
	}
	return a, nil
}

// DecryptAppPassword returns the account's app password in clear, for immediate
// use. Callers must not store or log it — the whole reason it is encrypted at
// rest with OPS_TOKEN_KEY is that a database dump must not be a credential dump.
func DecryptAppPassword(ctx context.Context, pool *pgxpool.Pool, accountID int64, key string) (string, error) {
	var password *string
	err := pool.QueryRow(ctx,
		`SELECT pgp_sym_decrypt(app_password_encrypted, $2) FROM source_accounts
		  WHERE id=$1 AND app_password_encrypted IS NOT NULL`,
		accountID, key).Scan(&password)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("account %d has no app password stored", accountID)
	}
	if err != nil {
		// Deliberately does not echo the key or any decrypted fragment.
		return "", fmt.Errorf("decrypt app password for account %d: %w", accountID, err)
	}
	if password == nil {
		return "", fmt.Errorf("account %d has no app password stored", accountID)
	}
	return *password, nil
}

// messageIDFromMIME reads back the Message-ID BuildOutboundMIME wrote.
func messageIDFromMIME(rawMIME []byte) string {
	const key = "message-id:"
	for _, line := range strings.Split(string(rawMIME), "\r\n") {
		if line == "" {
			break // end of headers
		}
		if len(line) >= len(key) && strings.EqualFold(line[:len(key)], key) {
			return strings.TrimSpace(line[len(key):])
		}
	}
	return ""
}

// accountSelect is the column list every account read shares, so a new field is
// added in one place rather than drifting between callers.
const accountSelect = `SELECT id, account_email, COALESCE(calendar_in_availability,false),
       COALESCE(auth_type,'oauth'), COALESCE(imap_host,''), COALESCE(imap_port,0),
       COALESCE(smtp_host,''), COALESCE(smtp_port,0)
  FROM source_accounts WHERE provider='google'`

func scanAccounts(rows pgx.Rows) ([]Account, error) {
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.Email, &a.CalendarInAvailability,
			&a.AuthType, &a.IMAPHost, &a.IMAPPort, &a.SMTPHost, &a.SMTPPort); err != nil {
			return nil, fmt.Errorf("scan google account: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate google accounts: %w", err)
	}
	return out, nil
}

// ListAppPasswordAccounts returns the accounts authenticating with an app
// password, optionally narrowed to one email.
//
// Scoped to auth_type='app_password' on purpose: an OAuth row has no password to
// decrypt, and handing it to the IMAP source would produce a login failure that
// reads as a credential problem rather than a configuration one.
func ListAppPasswordAccounts(ctx context.Context, pool *pgxpool.Pool, onlyEmail string) ([]Account, error) {
	query := accountSelect + ` AND auth_type='app_password'`
	var args []any
	if strings.TrimSpace(onlyEmail) != "" {
		query += ` AND lower(account_email)=lower($1)`
		args = append(args, onlyEmail)
	}
	rows, err := pool.Query(ctx, query+` ORDER BY account_email`, args...)
	if err != nil {
		return nil, fmt.Errorf("list app-password accounts: %w", err)
	}
	return scanAccounts(rows)
}

// UpsertAppPasswordAccount stores (or re-keys) an app-password mailbox.
//
// The password is encrypted SQL-side with pgcrypto so the plaintext never leaves
// this process and the key never reaches the database — the same idiom as the
// OAuth refresh token. refresh_token_encrypted is deliberately untouched: an
// account may hold both while a deployment migrates from OAuth to IMAP, and
// clearing it here would make the change one-way for no reason.
//
// send_enabled is set false on INSERT only. A freshly onboarded mailbox must not
// be able to send until someone says so, but re-running this command to rotate a
// password must not silently revoke a mailbox that was already sending.
func UpsertAppPasswordAccount(ctx context.Context, pool *pgxpool.Pool, email, password, tokenKey string, hosts MailHosts, calendarInAvailability bool) (int64, error) {
	h := hosts.WithDefaults()
	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts
		   (provider, account_email, auth_type, app_password_encrypted, scopes,
		    send_enabled, calendar_in_availability, imap_host, imap_port, smtp_host, smtp_port)
		 VALUES ('google', $1, 'app_password', pgp_sym_encrypt($2, $3), '{}',
		         false, $4, $5, $6, $7, $8)
		 ON CONFLICT (provider, account_email) DO UPDATE SET
		   auth_type              = 'app_password',
		   app_password_encrypted = pgp_sym_encrypt($2, $3),
		   calendar_in_availability = EXCLUDED.calendar_in_availability,
		   imap_host = EXCLUDED.imap_host, imap_port = EXCLUDED.imap_port,
		   smtp_host = EXCLUDED.smtp_host, smtp_port = EXCLUDED.smtp_port
		 RETURNING id`,
		email, password, tokenKey, calendarInAvailability,
		h.IMAPHost, h.IMAPPort, h.SMTPHost, h.SMTPPort).Scan(&id)
	if err != nil {
		// The password is never interpolated into this message.
		return 0, fmt.Errorf("store app-password account %s: %w", email, err)
	}
	return id, nil
}
