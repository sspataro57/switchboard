package google_test

// WIRE-LEVEL tests for IMAPClientSource's authentication branch (SPEC
// docs/tickets/microsoft-oauth-mail_SPEC.md, acceptance criterion 4; the error
// half of criterion 3; the access-token half of criterion 6).
//
// The "network" is an in-process TLS listener on 127.0.0.1 speaking just enough
// IMAP4rev1, with a throwaway self-signed certificate — the sibling harness the
// SPEC names, smtp_test.go's fakeSMTP/fakeSMTPCert/clientConfig. NEVER a live
// IMAP server, never outlook.office365.com.
//
// WHY A WIRE SERVER AND NOT fake_imap_test.go: that fake implements the
// MailSource INTERFACE, so it sits ABOVE the credential decision and can never
// observe which SASL exchange happened. Criterion 4 is precisely about the bytes
// IMAPClientSource.connect puts on the socket, so the fake here is a SERVER.
//
// EXPECTED FAILURE MODE: **compile error** — google.IMAPClientSource has no
// AccessToken field yet ("unknown field AccessToken in struct literal"). Once the
// field exists but connect still calls Login unconditionally, these become
// ASSERTION failures ("no AUTHENTICATE XOAUTH2 line on the wire").
//
// IMPOSED SURFACE (imap.go; the SPEC says "IMAPClientSource gains a
// token-provider field" without naming it):
//
//	type IMAPClientSource struct {
//	    ...
//	    // AccessToken mints a fresh OAuth bearer token for XOAUTH2. nil means the
//	    // password path, byte for byte as today (criterion 12). Called per
//	    // connect, never cached across passes (D8).
//	    AccessToken func(ctx context.Context) (string, error)
//	}
//
// CAREFUL (SPEC "Files likely to touch"): imap.go is scanned by
// TestIMAPClientSource_UsesBodyPeekAndNoWriteVerbs for `expunge`, `.store(`,
// `.move(`, `.append(`. The auth branch must not introduce those substrings.

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

// The capability line Microsoft actually advertises before authentication,
// measured in the SPEC's "Source" section. LOGINDISABLED with AUTH=XOAUTH2 as
// the only mechanism is the entire reason this ticket exists.
const msxOutlookCaps = "IMAP4 IMAP4rev1 AUTH=XOAUTH2 LOGINDISABLED SASL-IR UIDPLUS MOVE ID UNSELECT CHILDREN IDLE NAMESPACE LITERAL+"

// What the three live Gmail mailboxes see: no LOGINDISABLED, no XOAUTH2.
const msxGmailCaps = "IMAP4rev1 UNSELECT IDLE NAMESPACE QUOTA ID XLIST CHILDREN X-GM-EXT-1 UIDPLUS COMPRESS=DEFLATE ENABLE MOVE CONDSTORE ESEARCH UTF8=ACCEPT LIST-EXTENDED LIST-STATUS LITERAL- SPECIAL-USE APPENDLIMIT=35651584 SASL-IR AUTH=PLAIN"

// ---- the in-process TLS IMAP server -------------------------------------------

type fakeIMAPServer struct {
	ln     net.Listener
	tlsCfg *tls.Config

	caps        string // advertised in the greeting and by CAPABILITY
	acceptToken bool   // AUTHENTICATE XOAUTH2 succeeds
	acceptLogin bool   // LOGIN succeeds
	// challengeJSON is the failure frame sent as a continuation before the tagged
	// NO, exactly as Outlook does. Sent base64-encoded.
	challengeJSON string

	mu    sync.Mutex
	lines []string // every line the client sent, in order
}

func newFakeIMAPServer(t *testing.T, caps string) *fakeIMAPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeIMAPServer{
		ln:     ln,
		tlsCfg: &tls.Config{Certificates: []tls.Certificate{fakeSMTPCert(t)}},
		caps:   caps,
	}
	t.Cleanup(func() { _ = ln.Close() })
	go s.serve()
	return s
}

func (s *fakeIMAPServer) hostPort(t *testing.T) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(s.ln.Addr().String())
	if err != nil {
		t.Fatalf("split listener addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	return host, port
}

func (s *fakeIMAPServer) record(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, line)
}

func (s *fakeIMAPServer) wire() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

func (s *fakeIMAPServer) wireText() string { return strings.Join(s.wire(), "\n") }

// wireHasPrefix reports whether any received line's COMMAND (after the tag)
// starts with prefix, case-insensitively.
func (s *fakeIMAPServer) wireHasPrefix(prefix string) bool {
	for _, l := range s.wire() {
		if strings.HasPrefix(strings.ToUpper(commandOf(l)), strings.ToUpper(prefix)) {
			return true
		}
	}
	return false
}

// commandOf strips the IMAP tag from a client line.
func commandOf(line string) string {
	parts := strings.SplitN(strings.TrimSpace(line), " ", 2)
	if len(parts) != 2 {
		return ""
	}
	return parts[1]
}

func (s *fakeIMAPServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeIMAPServer) handle(rawConn net.Conn) {
	defer rawConn.Close()
	_ = rawConn.SetDeadline(time.Now().Add(20 * time.Second))
	conn := tls.Server(rawConn, s.tlsCfg)
	if err := conn.Handshake(); err != nil {
		return
	}
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	write := func(lines ...string) {
		for _, l := range lines {
			_, _ = w.WriteString(l + "\r\n")
		}
		_ = w.Flush()
	}

	write("* OK [CAPABILITY " + s.caps + "] fake imap ready")

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		s.record(line)

		tag := "*"
		if f := strings.SplitN(line, " ", 2); len(f) == 2 {
			tag = f[0]
		}
		cmd := commandOf(line)
		upper := strings.ToUpper(cmd)

		switch {
		case upper == "CAPABILITY":
			write("* CAPABILITY "+s.caps, tag+" OK CAPABILITY completed")

		case strings.HasPrefix(upper, "LOGIN"):
			if s.acceptLogin {
				write(tag + " OK LOGIN completed")
			} else {
				write(tag + " NO [AUTHENTICATIONFAILED] LOGIN failed")
			}

		case strings.HasPrefix(upper, "AUTHENTICATE"):
			// With SASL-IR advertised the client sends the initial response
			// inline; without it, it waits for an empty continuation first.
			if len(strings.Fields(cmd)) < 3 {
				write("+ ")
				ir, err := r.ReadString('\n')
				if err != nil {
					return
				}
				s.record("(ir) " + strings.TrimRight(ir, "\r\n"))
			}
			if s.acceptToken {
				write(tag + " OK AUTHENTICATE completed")
				break
			}
			write("+ " + base64.StdEncoding.EncodeToString([]byte(s.challengeJSON)))
			reply, err := r.ReadString('\n')
			if err != nil {
				return
			}
			// Recorded with a marker so the assertions can tell an empty line
			// (criterion 3) from a "*" cancellation.
			s.record("(challenge-reply) " + strings.TrimRight(reply, "\r\n"))
			write(tag + " NO AUTHENTICATE failed.")

		case strings.HasPrefix(upper, "LIST"):
			write(
				`* LIST (\HasNoChildren) "/" "INBOX"`,
				`* LIST (\Sent \HasNoChildren) "/" "Sent Items"`,
				tag+" OK LIST completed")

		case strings.HasPrefix(upper, "EXAMINE"), strings.HasPrefix(upper, "SELECT"):
			write(
				`* FLAGS (\Answered \Flagged \Deleted \Seen \Draft)`,
				"* 0 EXISTS",
				"* 0 RECENT",
				"* OK [UIDVALIDITY 42] UIDs valid",
				"* OK [UIDNEXT 1] Predicted next UID",
				tag+" OK [READ-ONLY] EXAMINE completed")

		case upper == "NOOP":
			write(tag + " OK NOOP completed")

		case upper == "LOGOUT":
			write("* BYE logging out", tag+" OK LOGOUT completed")
			return

		default:
			write(tag + " BAD unknown command")
		}
	}
}

// msxTokenSource builds a source authenticating with a bearer token.
func msxTokenSource(t *testing.T, srv *fakeIMAPServer, username, token string) *google.IMAPClientSource {
	t.Helper()
	host, port := srv.hostPort(t)
	return &google.IMAPClientSource{
		Host:      host,
		Port:      port,
		Username:  username,
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
		AccessToken: func(context.Context) (string, error) {
			return token, nil
		},
	}
}

// msxPasswordSource builds today's source, unchanged (criterion 12).
func msxPasswordSource(t *testing.T, srv *fakeIMAPServer, username, password string) *google.IMAPClientSource {
	t.Helper()
	host, port := srv.hostPort(t)
	src := google.NewIMAPClientSource(
		google.MailHosts{IMAPHost: host, IMAPPort: port}, username, password)
	src.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	return src
}

// ---- criterion 4: the token path against an XOAUTH2-only server ---------------

func TestIMAPClientSource_TokenPathSendsTheXOAUTH2InitialResponseInline(t *testing.T) {
	srv := newFakeIMAPServer(t, msxOutlookCaps)
	srv.acceptToken = true

	src := msxTokenSource(t, srv, msxUser, msxToken)
	defer func() { _ = src.Close() }()

	folders, err := src.Folders(context.Background())
	if err != nil {
		t.Fatalf("Folders over XOAUTH2: %v\nwire:\n%s", err, srv.wireText())
	}
	if len(folders) == 0 {
		t.Fatalf("no folders returned; the connection authenticated but LIST produced nothing")
	}
	sent := false
	for _, f := range folders {
		if f.Sent {
			sent = true
		}
	}
	if !sent {
		t.Errorf("no folder carries the RFC 6154 \\Sent marker — D11/invariant 5: Sent is the "+
			"own-message loop-closure surface, and SelectFolders picks it by that flag. folders=%v", folders)
	}

	// The load-bearing assertion: the exact criterion-2 initial response, inline
	// on the AUTHENTICATE command (the server advertises SASL-IR).
	want := "AUTHENTICATE XOAUTH2 " + msxWantIR
	if !srv.wireHasPrefix(want) {
		t.Errorf("the wire never carried %q.\nwire:\n%s", want, srv.wireText())
	}
	if srv.wireHasPrefix("LOGIN") {
		t.Errorf("a LOGIN command was sent on the token path.\nwire:\n%s", srv.wireText())
	}
}

func TestIMAPClientSource_PasswordPathIsRefusedByAnXOAuth2OnlyServer(t *testing.T) {
	const password = "msx-app-password-sentinel"
	srv := newFakeIMAPServer(t, msxOutlookCaps)
	srv.acceptLogin = true // even a server that WOULD accept it must never be asked

	src := msxPasswordSource(t, srv, msxUser, password)
	defer func() { _ = src.Close() }()

	_, err := src.Folders(context.Background())
	if err == nil {
		t.Fatalf("Folders succeeded with a password against a LOGINDISABLED server; " +
			"this is the exact state add-app-password hit on sspataro57@msn.com")
	}
	if strings.Contains(err.Error(), password) {
		t.Errorf("the app password is interpolated into the error (%v) — it reaches logs and sync_runs.stats "+
			"(criterion 6)", err)
	}
	if strings.Contains(srv.wireText(), password) {
		t.Errorf("the app password reached the wire of a LOGINDISABLED server.\nwire:\n%s", srv.wireText())
	}
}

// The regression fence for the three live Gmail mailboxes: with no token
// provider, nothing about the password exchange changes (criterion 12).
func TestIMAPClientSource_PasswordPathUnchangedOnAnOrdinaryServer(t *testing.T) {
	srv := newFakeIMAPServer(t, msxGmailCaps)
	srv.acceptLogin = true

	src := msxPasswordSource(t, srv, "itest-gmail@example.com", "abcd efgh ijkl mnop")
	defer func() { _ = src.Close() }()

	if _, err := src.Folders(context.Background()); err != nil {
		t.Fatalf("Folders on the unchanged app-password path: %v\nwire:\n%s", err, srv.wireText())
	}
	if !srv.wireHasPrefix("LOGIN") {
		t.Errorf("no LOGIN command was sent; the app-password path must be byte-identical to today.\nwire:\n%s", srv.wireText())
	}
	if srv.wireHasPrefix("AUTHENTICATE") {
		t.Errorf("an AUTHENTICATE command was sent for an account with no token provider.\nwire:\n%s", srv.wireText())
	}
}

// ---- criterion 3 (the error half) + criterion 6 --------------------------------

func TestIMAPClientSource_AuthFailureErrorCarriesTheServerChallenge(t *testing.T) {
	const sentinelToken = "ya29.SENTINEL-ACCESS-TOKEN-MUST-NOT-LEAK"
	srv := newFakeIMAPServer(t, msxOutlookCaps)
	srv.acceptToken = false
	srv.challengeJSON = `{"status":"401","schemes":"Bearer","scope":"https://outlook.office.com/IMAP.AccessAsUser.All"}`

	src := msxTokenSource(t, srv, msxUser, sentinelToken)
	defer func() { _ = src.Close() }()

	_, err := src.Folders(context.Background())
	if err == nil {
		t.Fatalf("Folders succeeded although the server refused the token")
	}

	// The exchange must have COMPLETED: an empty reply to the challenge, not a
	// "*" cancellation (criterion 3).
	var replied, sawReply bool
	for _, l := range srv.wire() {
		if strings.HasPrefix(l, "(challenge-reply) ") {
			sawReply = true
			replied = strings.TrimSpace(strings.TrimPrefix(l, "(challenge-reply) ")) == ""
		}
	}
	if !sawReply {
		t.Fatalf("the client never answered the server's challenge frame.\nwire:\n%s", srv.wireText())
	}
	if !replied {
		t.Errorf("the client answered the challenge with something other than an empty line "+
			"(a \"*\" cancels the command, so the tagged NO and its cause never arrive).\nwire:\n%s", srv.wireText())
	}

	for _, want := range []string{"401", "IMAP.AccessAsUser.All"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("connect's error %q does not name %q from the server's challenge. Criterion 3: "+
				"a failure whose message is only \"AUTHENTICATE failed\" does not satisfy it — it cannot "+
				"distinguish a revoked token from a missing scope, which are different operator actions", err, want)
		}
	}
	if strings.Contains(err.Error(), sentinelToken) {
		t.Errorf("the access token leaked into the error string (criterion 6): %v", err)
	}
	if !strings.Contains(err.Error(), msxUser) {
		t.Errorf("connect's error %q does not name the account; D9's sync_runs message is built from it", err)
	}
}

// A credential that cannot be minted must fail with the CAUSE, not with a
// confusing protocol error: D9 builds the sync_runs message out of this string,
// and "imap dial failed" would send an operator looking at the network when the
// answer is "re-run google-auth add-microsoft".
//
// (The no-leak half of criterion 6 for the refresh token lives in msoauth_test.go,
// where a real sentinel token is actually fed through the mint path; asserting it
// here would only prove that a string this test invented is absent.)
func TestIMAPClientSource_TokenProviderErrorSurfacesTheCause(t *testing.T) {
	srv := newFakeIMAPServer(t, msxOutlookCaps)
	srv.acceptToken = true

	host, port := srv.hostPort(t)
	src := &google.IMAPClientSource{
		Host: host, Port: port, Username: msxUser,
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
		AccessToken: func(context.Context) (string, error) {
			// What a revoked consent looks like at mint time. The wrapper must
			// not echo the refresh token it was holding.
			return "", fmt.Errorf("refresh token rejected (invalid_grant)")
		},
	}
	defer func() { _ = src.Close() }()

	_, err := src.Folders(context.Background())
	if err == nil {
		t.Fatalf("Folders succeeded although no access token could be minted")
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("error %q does not carry the cause class; D9's sync_runs message is built from it", err)
	}
}
