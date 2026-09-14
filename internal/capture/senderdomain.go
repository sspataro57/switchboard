package capture

// SWT-23 criterion 2: the sender DOMAIN is parsed in GO, never in SQL.
//
// The reason is a silent failure: split_part(sender,'@',2) on `Name <a@b.com>`
// yields `b.com>` — with the closing angle bracket — a domain that matches no
// capture rule (KindSender is a substring of the raw From header), groups into
// its own leaderboard row, and reports a coverage number that is wrong in the
// direction that makes rules look useless. This repo's standing rule is that a
// format Go owns is never taken apart in SQL; rulesreport.go already says it
// for thread keys, and the From header is no exception.

import (
	"net/mail"
	"strings"
)

// senderDomain returns the lower-cased host of the From header's address, or
// the RAW STRING when no address can be found.
//
// The fallback is load-bearing, not a shrug: a sender with no `@` at all is
// never gmail — google writes the raw From header, which always carries an
// address (connector/google/rfc822.go, normalize.go), while slackweb writes
// message.Author and upworkcrm the CRM's sender column, both DISPLAY NAMES. So
// a bare name is a Slack or Upwork WORK conversation sitting unmatched
// (measured 2026-08-31: 1,287 of the residue, all channel='upwork'), and it
// must stay visible as itself rather than collapsing into one anonymous "(no
// address)" bucket or an empty key.
func senderDomain(sender string) string {
	s := strings.TrimSpace(sender)
	if s == "" {
		return ""
	}
	if addr := senderAddress(s); addr != "" {
		return strings.ToLower(addr[strings.LastIndexByte(addr, '@')+1:])
	}
	// No address at all: the raw string IS the key.
	return s
}

// senderAddress returns the addr-spec of the From header, or "" when the sender
// carries no address (a Slack or Upwork display name, an empty string). It is
// the ONE net/mail spelling in this package: senderDomain takes the host of it,
// and notifierSender (chat-on-closed-task CC4) compares the whole address.
//
// A non-empty result always holds an '@' followed by a non-empty host, and never
// an angle bracket or a space (the split_part trap above). Its case is kept as
// written; callers fold it.
func senderAddress(sender string) string {
	s := strings.TrimSpace(sender)
	if s == "" {
		return ""
	}
	if addr, err := mail.ParseAddress(s); err == nil {
		if at := strings.LastIndexByte(addr.Address, '@'); at >= 0 && at+1 < len(addr.Address) &&
			!strings.ContainsAny(addr.Address, "<> \t") {
			return addr.Address
		}
	}
	// Unparseable but addressed (multiple addresses, broken quoting): take the
	// token around the last '@', cut at the angle brackets, quotes, commas and
	// whitespace the split_part trap would otherwise keep.
	at := strings.LastIndexByte(s, '@')
	if at < 0 {
		return ""
	}
	// Leading whitespace after the '@' is trimmed BEFORE the cut, as the
	// pre-refactor senderDomain did: `foo@ bar.com` keeps host bar.com rather
	// than cutting to an empty host and falling back to the raw string.
	host := strings.TrimLeft(s[at+1:], " \t")
	if i := strings.IndexAny(host, " \t<>,\""); i >= 0 {
		host = host[:i]
	}
	if host == "" {
		return ""
	}
	local := s[:at]
	if i := strings.LastIndexAny(local, " \t<>,\""); i >= 0 {
		local = local[i+1:]
	}
	return local + "@" + host
}
