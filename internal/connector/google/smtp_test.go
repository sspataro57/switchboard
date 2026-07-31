package google_test

// Offline unit tests for the SMTP transport and its error classifier (SPEC
// imap-mail-connector, acceptance criteria 12 and 14; invariants 4 and 6).
// The "network" is an in-process fake SMTP listener on 127.0.0.1 speaking
// STARTTLS with a throwaway self-signed certificate — NEVER a real mail
// server, never a live send.
//
// Criterion 14 is the load-bearing part of invariant 4:
//   * a protocol response code (textproto.Error, any 4xx/5xx — the server
//     SPOKE and refused) => wrapped in *google.SendRejectedError, so
//     send_delivery clears the reserved Message-ID and failed->approved retry
//     stays reachable;
//   * an I/O error with no response code (dial failure, timeout, reset
//     mid-DATA) => stays untyped, the row goes failed with sent_external_id
//     KEPT, blocking any automatic resend.
//
// GREENFIELD NOTE: smtp.go does not exist yet; this file compile-FAILs under
// `go test ./...` until it does — the expected failure mode. Imposed surface
// (the SPEC pins the SEAM — tools.GmailSender — and leaves the transport shape
// open; this is the minimal split that keeps the classifier unit-testable with
// no Postgres, since SMTPSender.Send itself must resolve + decrypt an account
// row):
//
//   // SMTPConfig is the resolved submission endpoint for ONE message.
//   type SMTPConfig struct {
//       Host        string
//       Port        int
//       Username    string
//       Password    string      // never logged, never in an error string
//       ImplicitTLS bool        // 465; false => STARTTLS on 587 (the default)
//       TLSConfig   *tls.Config // nil => production default (verified)
//   }
//
//   // SubmitSMTP submits rawMIME UNCHANGED (invariant 6: no X-Mailer, no
//   // User-Agent, no X-Switchboard-* — nothing that fingerprints the sender).
//   // The envelope recipient is parsed from the built MIME's To header.
//   func SubmitSMTP(ctx context.Context, cfg SMTPConfig, from string, rawMIME []byte) error
//
//   // SMTPSender is the app-password transport implementing tools.GmailSender
//   // (no new tool, no new policy rule, no signature change — criterion 12).
//   type SMTPSender struct { Pool *pgxpool.Pool; TokenKey string; TLSConfig *tls.Config }
//   // MailSender routes per from-account auth_type (criterion 13).
//   type MailSender struct {
//       Pool *pgxpool.Pool; TokenKey string; OAuthCfg *oauth2.Config
//       BaseURL string; Bridge *CommandBridge; TLSConfig *tls.Config
//   }

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/tools"
)

// ---- seam assertions (criteria 12, 13) ---------------------------------------

// No signature change: both the transport and the router satisfy the EXISTING
// tools.GmailSender seam, whose only caller is the send_delivery handler
// (invariant 4's single gate).
var (
	_ tools.GmailSender = (*google.SMTPSender)(nil)
	_ tools.GmailSender = (*google.MailSender)(nil)
)

// ---- in-process fake SMTP server ---------------------------------------------

type fakeSMTP struct {
	ln     net.Listener
	tlsCfg *tls.Config

	rcptReply      string // default "250 2.1.5 Ok"
	dataReply      string // reply after the terminating "." — default "250 2.0.0 Ok: queued"
	dropDuringData bool   // reset the connection right after "354"

	mu       sync.Mutex
	mailFrom string
	rcptTo   []string
	data     []byte
	authSeen bool
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeSMTP{
		ln:        ln,
		tlsCfg:    &tls.Config{Certificates: []tls.Certificate{fakeSMTPCert(t)}},
		rcptReply: "250 2.1.5 Ok",
		dataReply: "250 2.0.0 Ok: queued as FAKE1",
	}
	t.Cleanup(func() { _ = ln.Close() })
	go s.serve()
	return s
}

// clientConfig is what the test injects where production uses the default,
// host-verified TLS config. InsecureSkipVerify keeps the fixture to one
// throwaway cert; certificate verification is not what these tests pin.
func (s *fakeSMTP) clientConfig() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true}
}

func (s *fakeSMTP) hostPort(t *testing.T) (string, int) {
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

func (s *fakeSMTP) received() (from string, rcpt []string, data string, authed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mailFrom, append([]string(nil), s.rcptTo...), string(s.data), s.authSeen
}

func (s *fakeSMTP) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeSMTP) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	write := func(lines ...string) {
		for _, l := range lines {
			_, _ = w.WriteString(l + "\r\n")
		}
		_ = w.Flush()
	}

	write("220 fake.smtp ESMTP")
	tlsUp := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(cmd)

		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			if tlsUp {
				write("250-fake.smtp", "250 AUTH PLAIN")
			} else {
				write("250-fake.smtp", "250 STARTTLS")
			}

		case upper == "STARTTLS":
			write("220 2.0.0 Ready to start TLS")
			tlsConn := tls.Server(conn, s.tlsCfg)
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			conn = tlsConn
			_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
			r = bufio.NewReader(conn)
			w = bufio.NewWriter(conn)
			tlsUp = true

		case strings.HasPrefix(upper, "AUTH PLAIN"):
			s.mu.Lock()
			s.authSeen = true
			s.mu.Unlock()
			if fields := strings.Fields(cmd); len(fields) < 3 {
				write("334 ")
				if _, err := r.ReadString('\n'); err != nil {
					return
				}
			} else {
				_, _ = base64.StdEncoding.DecodeString(fields[2])
			}
			write("235 2.7.0 Accepted")

		case strings.HasPrefix(upper, "MAIL FROM:"):
			s.mu.Lock()
			s.mailFrom = angleAddr(cmd)
			s.mu.Unlock()
			write("250 2.1.0 Ok")

		case strings.HasPrefix(upper, "RCPT TO:"):
			s.mu.Lock()
			s.rcptTo = append(s.rcptTo, angleAddr(cmd))
			s.mu.Unlock()
			write(s.rcptReply)

		case upper == "DATA":
			write("354 End data with <CR><LF>.<CR><LF>")
			if s.dropDuringData {
				// Reset mid-DATA: an I/O error with NO response code. The send
				// MAY have gone through, so invariant 4 keeps the reservation.
				_ = conn.Close()
				return
			}
			var body strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" || l == ".\n" {
					break
				}
				body.WriteString(strings.TrimPrefix(l, ".")) // undo dot-stuffing
			}
			s.mu.Lock()
			s.data = []byte(body.String())
			s.mu.Unlock()
			write(s.dataReply)

		case upper == "QUIT":
			write("221 2.0.0 Bye")
			return

		case upper == "RSET", upper == "NOOP":
			write("250 2.0.0 Ok")

		default:
			write("500 5.5.2 Unrecognized command")
		}
	}
}

func angleAddr(cmd string) string {
	openIdx := strings.Index(cmd, "<")
	closeIdx := strings.Index(cmd, ">")
	if openIdx < 0 || closeIdx < openIdx {
		return strings.TrimSpace(cmd[strings.Index(cmd, ":")+1:])
	}
	return cmd[openIdx+1 : closeIdx]
}

func fakeSMTPCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func smtpConfigFor(t *testing.T, s *fakeSMTP) google.SMTPConfig {
	t.Helper()
	host, port := s.hostPort(t)
	return google.SMTPConfig{
		Host:      host,
		Port:      port,
		Username:  sendFrom,
		Password:  "app-password-never-logged",
		TLSConfig: s.clientConfig(),
	}
}

// ---- criterion 12 + invariant 6: bytes submitted unchanged --------------------

func TestSubmitSMTP_SubmitsTheBuiltMIMEUnchanged(t *testing.T) {
	srv := newFakeSMTP(t)
	raw, err := google.BuildOutboundMIME(sampleOutbound("Thanks — pushed the fix to staging."))
	if err != nil {
		t.Fatalf("BuildOutboundMIME: %v", err)
	}

	if err := google.SubmitSMTP(context.Background(), smtpConfigFor(t, srv), sendFrom, raw); err != nil {
		t.Fatalf("SubmitSMTP: %v", err)
	}

	from, rcpt, data, authed := srv.received()
	if from != sendFrom {
		t.Errorf("MAIL FROM = %q, want %q", from, sendFrom)
	}
	if len(rcpt) != 1 || rcpt[0] != sendTo {
		t.Errorf("RCPT TO = %v, want [%s] (parsed from the built MIME's To header)", rcpt, sendTo)
	}
	if !authed {
		t.Errorf("no AUTH PLAIN was performed")
	}
	if strings.TrimRight(data, "\r\n") != strings.TrimRight(string(raw), "\r\n") {
		t.Errorf("DATA differs from BuildOutboundMIME's output (invariant 6: submit the bytes unchanged)\n got %q\nwant %q", data, raw)
	}
	// No automation fingerprint may be added on a client-visible surface.
	for _, forbidden := range []string{"X-Mailer", "User-Agent", "X-Switchboard", "X-Sb-"} {
		if strings.Contains(strings.ToLower(data), strings.ToLower(forbidden+":")) {
			t.Errorf("submitted message carries a %s header the builder did not write (invariant 6)", forbidden)
		}
	}
	if strings.Contains(data, "app-password-never-logged") {
		t.Errorf("the app password leaked into the submitted message")
	}
}

// ---- criterion 14: the error classifier ---------------------------------------

func TestSubmitSMTP_ServerRefusalIsSendRejected(t *testing.T) {
	raw, err := google.BuildOutboundMIME(sampleOutbound("body"))
	if err != nil {
		t.Fatalf("BuildOutboundMIME: %v", err)
	}

	cases := []struct {
		name    string
		prepare func(*fakeSMTP)
	}{
		{"550 at RCPT", func(s *fakeSMTP) { s.rcptReply = "550 5.1.1 <x>: Recipient address rejected" }},
		{"5xx at end-of-DATA", func(s *fakeSMTP) { s.dataReply = "554 5.7.1 Message rejected as spam" }},
		{"4xx transient refusal", func(s *fakeSMTP) { s.rcptReply = "451 4.3.0 Try again later" }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeSMTP(t)
			tc.prepare(srv)

			err := google.SubmitSMTP(context.Background(), smtpConfigFor(t, srv), sendFrom, raw)
			if err == nil {
				t.Fatalf("SubmitSMTP returned nil though the server refused")
			}
			var rejected *google.SendRejectedError
			if !errors.As(err, &rejected) {
				t.Fatalf("error %v (%T) is not *google.SendRejectedError; the reserved Message-ID would never be cleared and failed->approved retry becomes unreachable", err, err)
			}
		})
	}
}

func TestSubmitSMTP_ConnectionResetMidDATAStaysUntyped(t *testing.T) {
	srv := newFakeSMTP(t)
	srv.dropDuringData = true
	raw, err := google.BuildOutboundMIME(sampleOutbound("body"))
	if err != nil {
		t.Fatalf("BuildOutboundMIME: %v", err)
	}

	err = google.SubmitSMTP(context.Background(), smtpConfigFor(t, srv), sendFrom, raw)
	if err == nil {
		t.Fatalf("SubmitSMTP returned nil though the connection dropped mid-DATA")
	}
	var rejected *google.SendRejectedError
	if errors.As(err, &rejected) {
		t.Fatalf("an I/O error with no response code was classified as a definite rejection (%v); "+
			"the send MAY have gone through, so invariant 4 must KEEP sent_external_id", err)
	}
}

func TestSubmitSMTP_DialFailureStaysUntyped(t *testing.T) {
	// A listener that is closed immediately: nothing ever answers on the port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	_ = ln.Close()

	raw, err := google.BuildOutboundMIME(sampleOutbound("body"))
	if err != nil {
		t.Fatalf("BuildOutboundMIME: %v", err)
	}
	cfg := google.SMTPConfig{Host: host, Port: port, Username: sendFrom, Password: "pw", TLSConfig: &tls.Config{InsecureSkipVerify: true}}

	err = google.SubmitSMTP(context.Background(), cfg, sendFrom, raw)
	if err == nil {
		t.Fatalf("SubmitSMTP returned nil against a dead port")
	}
	var rejected *google.SendRejectedError
	if errors.As(err, &rejected) {
		t.Fatalf("a dial failure was classified as a definite rejection: %v", err)
	}
	if strings.Contains(err.Error(), "pw") && strings.Contains(err.Error(), "Password") {
		t.Errorf("the error string leaks credential material: %v", err)
	}
}
