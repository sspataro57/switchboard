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
	if addr, err := mail.ParseAddress(s); err == nil {
		if at := strings.LastIndexByte(addr.Address, '@'); at >= 0 && at+1 < len(addr.Address) {
			return strings.ToLower(addr.Address[at+1:])
		}
	}
	// Unparseable but addressed (multiple addresses, broken quoting): take the
	// host after the last '@', trimmed of the angle-bracket the split_part trap
	// leaves behind.
	if at := strings.LastIndexByte(s, '@'); at >= 0 && at+1 < len(s) {
		host := strings.TrimRight(strings.TrimSpace(s[at+1:]), ">")
		if i := strings.IndexAny(host, " \t"); i >= 0 {
			host = host[:i]
		}
		if host != "" {
			return strings.ToLower(host)
		}
	}
	// No address at all: the raw string IS the key.
	return s
}
