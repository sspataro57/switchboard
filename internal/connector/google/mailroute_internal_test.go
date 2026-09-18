package google

// Internal unit tests for the send router's auth_type decision (SPEC
// docs/tickets/microsoft-oauth-mail_SPEC.md, acceptance criterion 13, decision
// D6) and for the column list criterion 12 freezes.
//
// `package google`, not `google_test`: the routing decision must be a PURE
// function to be testable at all. After migration 0037 the auth_type CHECK makes
// an unknown value un-INSERTable, so "an unknown auth_type is refused instead of
// being routed to the OAuth sender" can never be reached from Postgres — it is
// reachable only here. (The refusal of a REAL xoauth2 row through
// MailSender.Send, which is the SPEC's mutation target, is the integration half:
// credential_integration_test.go.) Precedent for an internal test file:
// refetch_internal_test.go.
//
// EXPECTED FAILURE MODE: **compile error** — `undefined: routeSend`,
// `undefined: AuthTypeXOAuth2`.
//
// IMPOSED SURFACE (mailsender.go):
//
//	const AuthTypeXOAuth2 = "xoauth2"
//
//	// routeSend names the transport for an account's auth_type. Pure: no I/O.
//	// transport is "smtp" or "oauth"; every refusal is a *SendRejectedError,
//	// because nothing reached the network and send_delivery must release the
//	// reserved Message-ID rather than strand the row at 'failed'.
//	func routeSend(authType, accountEmail string) (transport string, err error)

import (
	"errors"
	"strings"
	"testing"
)

// ---- criterion 13: the three known values plus one unknown --------------------

func TestRouteSend_KnownAndUnknownAuthTypes(t *testing.T) {
	const acct = "sspataro57@msn.com"

	cases := []struct {
		name      string
		authType  string
		transport string
		// wantRefusal lists substrings the refusal must carry. Empty means the
		// call must succeed.
		wantRefusal []string
	}{
		{
			name:      "oauth keeps the Gmail API path",
			authType:  AuthTypeOAuth,
			transport: "oauth",
		},
		{
			name:      "app_password keeps SMTP",
			authType:  AuthTypeAppPassword,
			transport: "smtp",
		},
		{
			name:     "xoauth2 is refused BY NAME",
			authType: AuthTypeXOAuth2,
			// D6: an xoauth2 row falling into `default` hands a MICROSOFT refresh
			// token to the GMAIL-API sender. Fail-loud, but with a message that
			// sends the reader to the wrong place.
			wantRefusal: []string{acct, "xoauth2"},
		},
		{
			name:     "an unknown value is refused, not assumed to be oauth",
			authType: "banana",
			// The shape of the trap: a `default` branch that means "OAuth" turns
			// every future auth_type into a silent misroute.
			wantRefusal: []string{acct, "banana"},
		},
		{
			name:        "empty is refused too",
			authType:    "",
			wantRefusal: []string{acct},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			transport, err := routeSend(tc.authType, acct)

			if len(tc.wantRefusal) == 0 {
				if err != nil {
					t.Fatalf("routeSend(%q) = error %v, want transport %q", tc.authType, err, tc.transport)
				}
				if transport != tc.transport {
					t.Errorf("routeSend(%q) = %q, want %q", tc.authType, transport, tc.transport)
				}
				return
			}

			if err == nil {
				t.Fatalf("routeSend(%q) returned transport %q and no error; it must refuse", tc.authType, transport)
			}
			if transport != "" {
				t.Errorf("routeSend(%q) returned BOTH a refusal and transport %q", tc.authType, transport)
			}
			var rejected *SendRejectedError
			if !errors.As(err, &rejected) {
				t.Fatalf("routeSend(%q) error %v (%T) is not *SendRejectedError; nothing reached the "+
					"network, so send_delivery must be able to clear the reserved Message-ID and leave "+
					"failed->approved retry reachable", tc.authType, err, err)
			}
			for _, want := range tc.wantRefusal {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not name %q", err, want)
				}
			}
		})
	}
}

// D6 spells out what the xoauth2 refusal must SAY, because "account X is
// xoauth2, not app_password" (the existing SMTPSender wording) sends the reader
// looking for a password that was never supposed to exist.
func TestRouteSend_XOAuth2RefusalNamesTheMissingTransport(t *testing.T) {
	_, err := routeSend(AuthTypeXOAuth2, "sspataro57@msn.com")
	if err == nil {
		t.Fatalf("routeSend accepted an xoauth2 account")
	}
	got := strings.ToLower(err.Error())
	if !strings.Contains(got, "smtp") || !strings.Contains(got, "xoauth2") {
		t.Errorf("refusal %q does not name the missing transport (D6: \"no SMTP XOAUTH2 transport is "+
			"wired\"). Out of scope for this ticket is SENDING from the mailbox; the message is what tells "+
			"the next reader that this is a deliberate absence, not a bug", err)
	}
}

// ---- criterion 12: accountSelect is NOT changed -------------------------------

// The SPEC says it in those words. It is the column list every account read
// shares, so a new field here silently changes what ListIMAPAccounts,
// ListCalendarCredentialedAccounts and the refetch selection all return for the
// three live Gmail mailboxes.
func TestAccountSelect_ColumnListIsUnchanged(t *testing.T) {
	const want = `SELECT id, account_email, COALESCE(calendar_in_availability,false),
       COALESCE(auth_type,'oauth'), COALESCE(imap_host,''), COALESCE(imap_port,0),
       COALESCE(smtp_host,''), COALESCE(smtp_port,0)
  FROM source_accounts WHERE provider='google'`

	if accountSelect != want {
		t.Errorf("accountSelect changed.\n got: %s\nwant: %s\n\nCriterion 12 freezes it: this ticket adds "+
			"no column and reads no new one. scanAccounts scans positionally, so an added column silently "+
			"shifts every field for every caller", accountSelect, want)
	}
	// The predicate the SPEC's D3 table is built on: six production sites spell
	// provider='google', and the msn row joins them rather than adding a seventh
	// meaning.
	if !strings.Contains(accountSelect, "provider='google'") {
		t.Errorf("accountSelect no longer scopes to provider='google'")
	}
}
