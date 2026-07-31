package google

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// SMTP submission for the app-password path (criteria 12-14).
//
// This is the ONLY write verb this connector has against any mail server. It is
// reachable exclusively from send_delivery, which refuses anything that is not
// an approved delivery row and reserves sent_external_id before the network call
// (invariant 4). Nothing here decides whether to send; it only carries bytes.

// smtpDefaultTimeout bounds a submission when the caller supplied no deadline.
// Generous enough for a large attachment on a slow link, short enough that a
// stalled server cannot wedge a long-running process.
const smtpDefaultTimeout = 2 * time.Minute

// SMTPConfig is one submission endpoint plus its credentials.
type SMTPConfig struct {
	Host string
	Port int
	// Username is the mailbox; Password is the Google app password, decrypted
	// from source_accounts immediately before use and never logged.
	Username string
	Password string
	// ImplicitTLS selects port-465 semantics (TLS from the first byte). False
	// means STARTTLS on 587, which is what Gmail submission uses.
	ImplicitTLS bool
	// TLSConfig overrides the default; tests inject a throwaway CA. Production
	// leaves it nil and gets the system roots with ServerName = Host.
	TLSConfig *tls.Config
}

func (c SMTPConfig) addr() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

func (c SMTPConfig) tlsConfig() *tls.Config {
	if c.TLSConfig != nil {
		return c.TLSConfig
	}
	return &tls.Config{ServerName: c.Host}
}

// SubmitSMTP sends rawMIME verbatim.
//
// "Verbatim" is load-bearing for invariant 6: BuildOutboundMIME already wrote
// every header, including the reserved Message-ID that send_delivery committed
// before this call and the In-Reply-To/References that carry threading. Adding
// an X-Mailer or rewriting anything here would put an automation fingerprint on
// a client-visible surface.
//
// Error classification is criterion 14 and it is the whole reason this function
// does not just return err:
//
//   - The server SPOKE and refused (a 4xx/5xx response code) — the message
//     definitely did not land, so the error is wrapped in SendRejectedError and
//     send_delivery clears the reserved Message-ID, keeping failed→approved retry
//     reachable.
//   - Anything else (dial failure, timeout, connection reset mid-DATA) leaves the
//     outcome UNKNOWN. It stays untyped, so send_delivery keeps sent_external_id
//     and no automatic resend can happen. A duplicate client-visible email is
//     worse than a stuck row a human can resolve.
func SubmitSMTP(ctx context.Context, cfg SMTPConfig, from string, rawMIME []byte) error {
	to, err := recipientsFromMIME(rawMIME)
	if err != nil {
		// Not a transport failure: the message we built is unusable, and the
		// server never saw it. Definite non-delivery, so it is a rejection.
		return &SendRejectedError{Body: err.Error()}
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", cfg.addr())
	if err != nil {
		return fmt.Errorf("smtp dial %s: %w", cfg.addr(), err)
	}
	// A deadline on the connection itself: ctx bounded only the dial, so without
	// this a server that accepts the socket and then stalls would hang the caller
	// indefinitely — in watch mode, the whole loop.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(smtpDefaultTimeout))
	}
	if cfg.ImplicitTLS {
		conn = tls.Client(conn, cfg.tlsConfig())
	}

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		_ = conn.Close()
		return classifySMTP(fmt.Errorf("smtp greeting from %s: %w", cfg.addr(), err), err)
	}
	defer func() { _ = client.Close() }()

	if !cfg.ImplicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(cfg.tlsConfig()); err != nil {
				return classifySMTP(fmt.Errorf("smtp starttls: %w", err), err)
			}
		}
	}

	// AUTH only over TLS. Without this guard a downgraded or misconfigured
	// endpoint would take the app password in the clear.
	if cfg.Password != "" {
		if _, isTLS := client.TLSConnectionState(); !isTLS {
			// A definite non-send: we refused before authenticating, so nothing
			// was submitted and the reservation should be released for retry
			// once the endpoint is fixed.
			return &SendRejectedError{Body: fmt.Sprintf("smtp refusing to authenticate to %s without TLS", cfg.addr())}
		}
		auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
		if err := client.Auth(auth); err != nil {
			// Wrapped WITHOUT the password: the error text reaches deliveries.error
			// and the logs.
			return classifySMTP(fmt.Errorf("smtp auth as %s: %w", cfg.Username, err), err)
		}
	}

	if err := client.Mail(from); err != nil {
		return classifySMTP(fmt.Errorf("smtp MAIL FROM %s: %w", from, err), err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return classifySMTP(fmt.Errorf("smtp RCPT TO %s: %w", rcpt, err), err)
		}
	}

	w, err := client.Data()
	if err != nil {
		return classifySMTP(fmt.Errorf("smtp DATA: %w", err), err)
	}
	if _, err := w.Write(rawMIME); err != nil {
		return fmt.Errorf("smtp write message: %w", err)
	}
	// Close() is where the server's end-of-DATA verdict arrives — a 5xx here
	// (spam refusal) is a definite rejection, a dropped connection is not.
	if err := w.Close(); err != nil {
		return classifySMTP(fmt.Errorf("smtp end of DATA: %w", err), err)
	}
	if err := client.Quit(); err != nil {
		// The message is already accepted at this point; a failure to say QUIT
		// politely is not a send failure and must not be reported as one.
		return nil
	}
	return nil
}

// classifySMTP wraps err as a definite rejection only when the server answered
// with a protocol response code. See SubmitSMTP's doc for why the distinction is
// the difference between a retryable row and a possible duplicate send.
func classifySMTP(wrapped, cause error) error {
	var proto *textproto.Error
	if errors.As(cause, &proto) {
		return &SendRejectedError{Body: wrapped.Error()}
	}
	return wrapped
}

// recipientsFromMIME reads the envelope recipients out of the message we built.
// To/Cc/Bcc are parsed rather than tracked separately so that the envelope can
// never disagree with the headers the recipient sees.
func recipientsFromMIME(rawMIME []byte) ([]string, error) {
	msg, err := mail.ReadMessage(strings.NewReader(string(rawMIME)))
	if err != nil {
		return nil, fmt.Errorf("parse built MIME: %w", err)
	}
	var out []string
	seen := map[string]bool{}
	for _, header := range []string{"To", "Cc", "Bcc"} {
		v := msg.Header.Get(header)
		if strings.TrimSpace(v) == "" {
			continue
		}
		addrs, err := mail.ParseAddressList(v)
		if err != nil {
			return nil, fmt.Errorf("parse %s header: %w", header, err)
		}
		for _, a := range addrs {
			if !seen[a.Address] {
				seen[a.Address] = true
				out = append(out, a.Address)
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("built MIME carries no recipient")
	}
	return out, nil
}
