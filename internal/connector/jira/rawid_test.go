package jira_test

// Unit test for SWT-32 (docs/tickets/jira-status-sync_SPEC.md) criterion 5: the
// ONE spelling of an issue's raw external_id, and its inverse. ZERO I/O.
//
// GREENFIELD NOTE — EXPECTED RED. internal/connector/jira/rawid.go does not
// exist, so this file compile-FAILs the jira_test package with "undefined:
// jira.IssueRawID" / "undefined: jira.ParseIssueRawID".
//
// IMPOSED SURFACE (the SPEC names the two functions and their roles; the Go
// spelling of the inverse's second result is this file's, and it is the
// ParseThreadKey shape — `ok`, never a sentinel string):
//
//	// IssueRawID is the raw_source_items.external_id an issue snapshot is
//	// stored under, by the poller (ingest.go) and by the candidate-driven
//	// lookup alike — so both halves of the reconciler read the same rows.
//	func IssueRawID(key string) string
//	// ParseIssueRawID is its inverse: ok is false for a comment id, for a bare
//	// issue key, and for anything else.
//	func ParseIssueRawID(externalID string) (key string, ok bool)
//
// WHY THIS PAIR AT ALL, and why the SPEC deliberately does NOT mechanize it the
// way upworkcrm's thread key is mechanized: criterion 34 bans BUILDING this id
// inside a query (string concatenation, or format(), on the SQL side), which is
// checkable and is checked in internal/ticketstatus/structure_test.go — but a
// repo-wide ban on the literal prefix itself is not, because the string is too
// generic to discriminate, and "a guard that cannot match only its target is
// worse than none". So the guarantee here is a round trip plus the call-site
// check below, and nothing pretends to be more.

import (
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/connector/jira"
)

// The round trip, over the key shapes the corpus actually holds: the two live
// prefixes (WEB-* on treetopllc, LHH-* on avviato) and the numeric edges.
func TestIssueRawID_RoundTrips(t *testing.T) {
	for _, key := range []string{"WEB-1234", "LHH-1", "AB-999999", "SWT-32"} {
		key := key
		t.Run(key, func(t *testing.T) {
			id := jira.IssueRawID(key)
			if id == key {
				t.Fatalf("IssueRawID(%q) = %q — the raw id is namespaced so an issue and a comment "+
					"cannot collide in raw_source_items (UNIQUE (source_account_id, external_id))", key, id)
			}
			got, ok := jira.ParseIssueRawID(id)
			if !ok {
				t.Fatalf("ParseIssueRawID(IssueRawID(%q)) = _, false — the pair must be each other's "+
					"inverse or the normalizer's switch and the poller's write have drifted", key)
			}
			if got != key {
				t.Errorf("ParseIssueRawID(IssueRawID(%q)) = %q, want %q", key, got, key)
			}
		})
	}
}

// The inverse REFUSES everything that is not an issue snapshot. The comment case
// is the one that matters: `comment:{KEY}:{id}` shares the corpus with
// `issue:{KEY}`, normalize.go switches between them on exactly this
// distinction, and a parser that accepted a comment id would hand the
// reconciler a "snapshot" containing a comment body and no status at all.
func TestParseIssueRawID_RefusesEverythingElse(t *testing.T) {
	for _, tc := range []struct{ name, id string }{
		{"a comment raw id", "comment:WEB-1234:700701"},
		{"a bare issue key", "WEB-1234"},
		{"empty", ""},
		{"another connector's id", "calendar:abc123"},
		{"the prefix with no key", "issue:"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			key, ok := jira.ParseIssueRawID(tc.id)
			if ok {
				t.Errorf("ParseIssueRawID(%q) = %q, true — want ok=false. This inverse is what "+
					"normalize.go's issue/comment switch becomes; accepting a %s would make the "+
					"reconciler read a comment as an issue snapshot", tc.id, key, tc.name)
			}
		})
	}
}

// The call sites, criterion 5's second half: "ingest.go (`\"issue:\"+key`) and
// normalize.go (`strings.HasPrefix(it.externalID, \"issue:\")`) are converted to
// use them."
//
// A call-site check rather than a literal ban, for the reason the SPEC gives:
// banning `issue:` repo-wide cannot discriminate its target. Checking that the
// two named files CALL the pair is the honest half — it fails if the conversion
// is skipped, and it cannot be satisfied by a comment.
func TestIssueRawID_IsUsedByTheTwoCallSites(t *testing.T) {
	for _, tc := range []struct{ rel, want, why string }{
		{"ingest.go", "IssueRawID(", "the poller's raw write is where the id is MINTED; if it still " +
			"concatenates the literal, the lookup writing IssueRawID(key) could store a second row " +
			"under a different id for the same issue"},
		{"normalize.go", "ParseIssueRawID(", "the normalizer's issue/comment switch is the id's only " +
			"reader; a HasPrefix here and a helper there is exactly the drift this pair exists to stop"},
	} {
		tc := tc
		t.Run(tc.rel, func(t *testing.T) {
			src := readJiraSource(t, tc.rel)
			if !strings.Contains(src, tc.want) {
				t.Errorf("internal/connector/jira/%s does not call %s — criterion 5: %s",
					tc.rel, tc.want, tc.why)
			}
		})
	}
}
