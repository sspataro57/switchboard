package capture

// SWT-23 criterion 2 — the sender DOMAIN is parsed in GO, never in SQL.
//
// This is the pure half of the residue census (criteria 1-4). A rule is written
// at domain granularity — `capture.KindSender` is a case-insensitive SUBSTRING
// of the raw From header (rules.go:205-206), so `linkedin.com` is a legitimate
// one-line rule — while `reportUnmatchedSenders` (rulesreport.go:257-286) groups
// by the whole `Name <addr>` string. `LinkedIn <messages-noreply@…>` and
// `LinkedIn Job Alerts <jobalerts-noreply@…>` are two rows there and must be ONE
// row in the new section.
//
// IMPOSED SURFACE (the SPEC's "Internal Go surface added" block):
//
//	func senderDomain(sender string) string   // net/mail parse, pure, unit-tested
//
// Unexported, so this file is in package `capture` — the same choice
// rules_test.go and rules_structure_test.go already made. ZERO I/O.
//
// GREENFIELD NOTE: senderDomain does not exist yet, so this file compile-FAILS.
// Expected red.

import (
	"strings"
	"testing"
)

// THE REASON THIS TEST EXISTS, in one line, because the failure it prevents is
// silent: `split_part(sender,'@',2)` on `Name <a@b.com>` yields `b.com>` — a
// domain that matches no rule, groups into its own leaderboard row, and reports
// a coverage number that is wrong in the direction that makes rules look
// useless. This repo's standing rule is that a format Go owns is never taken
// apart in SQL (rulesreport.go:288-295 says it for thread keys; upwork thread
// keys have a structural test for it since SWT-19).
func TestSenderDomain_ParsesTheFromHeaderInGo(t *testing.T) {
	cases := []struct {
		name   string
		sender string
		want   string
		why    string
	}{
		{
			name:   "display name and angle-addr",
			sender: "LinkedIn <messages-noreply@linkedin.com>",
			want:   "linkedin.com",
			why: "THE SPLIT_PART TRAP. split_part(sender,'@',2) returns `linkedin.com>` here — with the " +
				"closing angle bracket — which matches no rule anyone would write and silently splits one " +
				"domain into two leaderboard rows. This is the single case the whole criterion exists for",
		},
		{
			name:   "bare address, no display name",
			sender: "messages-noreply@linkedin.com",
			want:   "linkedin.com",
			why:    "the ordinary shape; the parse must not require angle brackets",
		},
		{
			name:   "quoted display name containing a comma",
			sender: `"Vazquez, Gil" <gil@sspataro.com>`,
			want:   "sspataro.com",
			why: "a comma inside a quoted phrase is not an address separator. Anything that splits on ',' " +
				"before parsing gets `\"Vazquez` and drops the address entirely",
		},
		{
			name:   "RFC 2047 encoded display name",
			sender: "=?UTF-8?Q?Caf=C3=A9_Nextdoor?= <digest@nextdoor.com>",
			want:   "nextdoor.com",
			why: "net/mail.ParseAddress decodes encoded-words; a hand-rolled scanner would either keep the " +
				"=?UTF-8?…?= run or choke on it, and nextdoor is 726 of the residue",
		},
		{
			name:   "the host is lower-cased",
			sender: "Motorola <NEWS@Motorola.COM>",
			want:   "motorola.com",
			why: "domains are case-insensitive and a rule is written in lower case. Two casings of one " +
				"domain are two rows, and each one looks too small to be worth a rule",
		},
		{
			name:   "a bare display name falls back to the raw string",
			sender: "gil vazquez",
			want:   "gil vazquez",
			why: "PREMISE 6: a sender with no `@` is not gmail. google writes the raw From header " +
				"(connector/google/rfc822.go:134, normalize.go:121), which always carries an address; " +
				"slackweb writes message.Author (slackweb/normalize.go:88) and upworkcrm the CRM's sender " +
				"column (upworkcrm/normalize.go:148) — both DISPLAY NAMES. So these rows are WORK " +
				"conversations sitting unmatched, and they must stay visible as themselves rather than " +
				"collapsing into one '(none)' bucket or being dropped",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := senderDomain(tc.sender)
			if got != tc.want {
				t.Errorf("senderDomain(%q) = %q, want %q\n%s", tc.sender, got, tc.want, tc.why)
			}
		})
	}
}

// The trap, asserted directly rather than only implied by the table above, so a
// reader of a failure sees the mechanism and not just a mismatched string.
func TestSenderDomain_NeverReturnsAnAngleBracket(t *testing.T) {
	for _, sender := range []string{
		"LinkedIn <messages-noreply@linkedin.com>",
		"Indeed <alerts@indeed.com>",
		"USPS Informed Delivery <USPSInformeddelivery@email.informeddelivery.usps.com>",
	} {
		got := senderDomain(sender)
		for _, bad := range []string{"<", ">", " "} {
			if len(got) > 0 && strings.Contains(got, bad) {
				t.Errorf("senderDomain(%q) = %q, which contains %q. That is the shape "+
					"split_part(sender,'@',2) produces, and it is why criterion 2 says the domain is parsed "+
					"in GO: a domain with a trailing '>' matches no capture rule, and nothing errors — the "+
					"census just reports a coverage number that is wrong", sender, got, bad)
			}
		}
	}
}

// A domain that is empty for a sender that HAS one would make the census read
// "the residue has no top domains", which looks like a clean inbox rather than a
// broken parse.
func TestSenderDomain_IsNeverEmptyForANonEmptySender(t *testing.T) {
	for _, sender := range []string{
		"LinkedIn <messages-noreply@linkedin.com>",
		"gil vazquez",
		"news@medium.com",
	} {
		if senderDomain(sender) == "" {
			t.Errorf("senderDomain(%q) = \"\"; the fold falls back to the RAW STRING when the parse fails, "+
				"never to an empty key — an empty key groups every unparseable sender into one anonymous "+
				"row and hides the 1,287 address-less messages the census exists to name", sender)
		}
	}
}
