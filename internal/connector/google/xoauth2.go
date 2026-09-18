package google

// XOAUTH2, the SASL mechanism Microsoft requires for IMAP.
//
// It lives here rather than coming from a dependency because neither the pinned
// go-sasl nor its newest release implements it — they ship PLAIN, LOGIN,
// EXTERNAL, ANONYMOUS and OAUTHBEARER. OAUTHBEARER is NOT a substitute:
// outlook.office365.com:993 advertises `AUTH=XOAUTH2` and nothing else.
//
// The mechanism is one initial client response and, on failure, one round of
// politeness. That is the whole protocol.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// XOAuth2Mechanism is the SASL mechanism name, as advertised in CAPABILITY.
const XOAuth2Mechanism = "XOAUTH2"

// xoauth2 implements sasl.Client (Start/Next) for Google's XOAUTH2 spelling,
// which Microsoft adopted verbatim.
type xoauth2 struct {
	username  string
	token     string
	challenge string
}

// XOAuth2Client returns a sasl.Client that authenticates username with an OAuth
// bearer token. It is a value, not a connection: build one per authentication.
func XOAuth2Client(username, token string) *xoauth2 {
	return &xoauth2{username: username, token: token}
}

// Start returns the mechanism and its initial response.
//
// The byte layout is exact and unforgiving:
//
//	user=<username> \x01 auth=Bearer <token> \x01 \x01
//
// TWO trailing 0x01 bytes, not one. A single terminator is the common mistake
// and Outlook answers it with a bare NO carrying no useful reason, which is a
// long way from the cause. The CRLF is go-imap's to write, never ours.
func (x *xoauth2) Start() (string, []byte, error) {
	if x.username == "" || x.token == "" {
		// Never render either value, here or anywhere below: this error reaches
		// logs and sync_runs rows.
		return "", nil, fmt.Errorf("xoauth2: username and token are both required")
	}
	ir := []byte("user=" + x.username + "\x01auth=Bearer " + x.token + "\x01\x01")
	return XOAuth2Mechanism, ir, nil
}

// Next answers the server's challenge with an EMPTY response.
//
// This looks like a no-op and is load-bearing. On failure the server does not
// reject outright: it sends a base64 JSON challenge describing why, and waits.
// The exchange only completes — and the tagged NO with its message only
// arrives — once the client sends an empty line. Returning an error here
// instead would abort the exchange and throw away the one diagnostic the server
// offered, leaving "AUTHENTICATE failed" as the whole story.
//
// The challenge is recorded so connect can put the server's own words into the
// error it returns.
func (x *xoauth2) Next(challenge []byte) ([]byte, error) {
	if len(challenge) > 0 {
		x.challenge = string(challenge)
	}
	return []byte{}, nil
}

// Challenge returns the server's failure challenge in its most useful form:
// the decoded JSON when it is JSON, else whatever arrived, else "".
//
// Microsoft sends {"status":"401","schemes":"bearer","scope":"..."}; the status
// and scope are what distinguish a revoked token from a missing scope, which is
// the difference between "sign in again" and "fix the app registration".
func (x *xoauth2) Challenge() string {
	raw := strings.TrimSpace(x.challenge)
	if raw == "" {
		return ""
	}
	// go-imap hands the mechanism the DECODED challenge, but a server that
	// double-encodes, or a caller that passes the wire bytes, should still read
	// sensibly rather than as base64 noise.
	if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil && json.Valid(decoded) {
		return string(decoded)
	}
	return raw
}
