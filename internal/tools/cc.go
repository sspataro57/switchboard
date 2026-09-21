package tools

// gmail-delivery-cc (SWT-69): the pure half of the Cc contract. A Cc is
// something a drafter asks for and Salvador approves; nothing in switchboard
// adds one by itself (D1), and any syntactically valid address is acceptable
// because every gmail delivery is read by him before it can leave (D2).

import (
	"fmt"
	"net/mail"
	"strings"
)

// MaxCcAddresses caps a delivery's Cc list (D5). More than ten carbon copies is
// a mailing list, a different feature with different policy. The schema CHECK
// deliveries_cc_shape_check is pinned equal to it by a test.
const MaxCcAddresses = 10

// maxCcAddressBytes is RFC 5321's path maximum.
const maxCcAddressBytes = 254

// NormalizeCc turns the caller's cc entries into what is STORED: the address
// only (a display name is model-chosen words on a client-visible header, so it
// is dropped), domain lower-cased, local part as given, deduped
// case-insensitively with the first spelling kept. It always returns a non-nil
// slice: the column is NOT NULL DEFAULT '{}', and a nil []string would encode as
// SQL NULL.
//
// Every refusal names the entry and the rule. The byte floor (printable ASCII,
// and none of the RFC 5322 specials that only a quoted local part can carry) is
// the header-injection floor: CR, LF, spaces and commas cannot survive it, which
// is what lets the MIME writer and the content hash join addresses with commas.
func NormalizeCc(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, raw := range in {
		if strings.TrimSpace(raw) == "" {
			return nil, fmt.Errorf("cc: an entry is empty; give one email address per entry")
		}
		if strings.ContainsAny(raw, "\r\n\x00") {
			return nil, fmt.Errorf("cc: %q contains a control character; give one plain email address per entry", raw)
		}
		addr, err := mail.ParseAddress(raw)
		if err != nil {
			return nil, fmt.Errorf("cc: %q is not one email address (%v); give one address per entry", raw, err)
		}
		a := addr.Address
		at := strings.LastIndex(a, "@")
		if at <= 0 || at == len(a)-1 {
			return nil, fmt.Errorf("cc: %q is not one email address", raw)
		}
		local, domain := a[:at], strings.ToLower(a[at+1:])
		a = local + "@" + domain
		for i := 0; i < len(a); i++ {
			if a[i] < 0x21 || a[i] > 0x7E {
				return nil, fmt.Errorf("cc: %q contains %q; only printable ASCII addresses can be sent (no spaces, "+
					"no accented or non-Latin characters)", a, badRune(a, i))
			}
		}
		if i := strings.IndexAny(local, `()<>[]:;@\,"`); i >= 0 {
			return nil, fmt.Errorf("cc: %q has %q in its local part; an address that needs quoting cannot be a Cc", a, local[i:i+1])
		}
		if len(a) > maxCcAddressBytes {
			return nil, fmt.Errorf("cc: an address is %d bytes long; the limit is 254", len(a))
		}
		key := strings.ToLower(a)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, a)
	}
	if len(out) > MaxCcAddresses {
		return nil, fmt.Errorf("cc: %d addresses; the limit is 10 (more than that is a mailing list, not a reply)", len(out))
	}
	return out, nil
}

// badRune is the whole rune at byte offset i, so a refusal shows "é" rather
// than half of it.
func badRune(s string, i int) string {
	for start := i; start >= 0; start-- {
		if r := []rune(s[start:]); len(r) > 0 && r[0] != '�' {
			return string(r[0])
		}
	}
	return s[i : i+1]
}

// ccAddressOf is the address part of a From/To value for the collision rule
// (D7). normalized_messages.sender holds the raw From header for google rows,
// so it is parsed first and compared raw-and-trimmed only when it does not
// parse.
func ccAddressOf(headerValue string) string {
	if addr, err := mail.ParseAddress(headerValue); err == nil {
		return strings.ToLower(addr.Address)
	}
	return strings.ToLower(strings.TrimSpace(headerValue))
}

// ccCollision names the first Cc that repeats the message's From or To, or "".
func ccCollision(cc []string, from, to string) (addr, field string) {
	f, t := ccAddressOf(from), ccAddressOf(to)
	for _, a := range cc {
		switch strings.ToLower(a) {
		case f:
			return a, "From"
		case t:
			return a, "To"
		}
	}
	return "", ""
}

// ccWithout drops every Cc that equals the send-time From or To (D7): dropping
// narrows the recipient set and cannot surprise anybody, whereas refusing would
// wedge an approved delivery over a duplicate. Always non-nil.
func ccWithout(cc []string, from, to string) []string {
	f, t := ccAddressOf(from), ccAddressOf(to)
	out := make([]string, 0, len(cc))
	for _, a := range cc {
		if l := strings.ToLower(a); l == f || l == t {
			continue
		}
		out = append(out, a)
	}
	return out
}
