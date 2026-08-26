package textmatch

// Structural enforcement of the ONE-SPELLING rule, added by SWT-18.
//
// Plain unit test on purpose: no build tag, no database, so it runs under
// `go test ./...` and on every CI pass.
//
// The rule (IK "Exact text comparison across a provider round trip"): a post-hoc
// delivery matcher recognizes our own message by its opening text, and that
// comparison must go through textmatch.NormalizedPrefix — not left(body,N) in
// SQL, not a local strings.Fields. When two spellings drift NOTHING errors: a
// delivery silently stops being recognizable as our own and stays unclaimable
// with sent_external_id NULL forever.
//
// Why a test rather than a shared helper: the four matchers agree on exactly one
// thing (this comparison, already extracted here) and genuinely differ in join
// key, status set, time bound and multi-match policy. A helper would need six
// knobs and would bury the per-channel reasoning in arguments. A source scan
// costs twenty lines and catches the likely future mistake — a fifth connector.
//
// This check would have FAILED on 2026-07-31, the day SWT-16 fixed jira and left
// upworkcrm comparing raw bytes, which is the whole test of whether an
// enforcement mechanism earns its weight. It stayed broken for five weeks
// (SWT-18) because nothing in the repo could tell you a matcher lacked the rule.
//
// Deliberately NOT mechanized here: the attempt-time floor. It is spelled three
// different ways across three connectors and upwork is moving to exact-room
// matching instead of a fourth, so "the file mentions send_attempted_at" would
// have PASSED the very code SWT-18 fixed — a check that certifies a no-op is
// worse than no check. That half stays IK convention.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A connector sink confirms a delivery post-hoc when it WRITES the confirmation:
// sent_external_id (jira, slackweb, upworkcrm) or confirmed_at alone (google,
// which must never overwrite the id it reserved at send time).
var confirmationStamp = regexp.MustCompile(`(?i)\bSET\s+(sent_external_id|confirmed_at)\s*=`)

func TestConnectorSinksThatStampConfirmationsUseNormalizedPrefix(t *testing.T) {
	sinks, err := filepath.Glob(filepath.Join("..", "connector", "*", "sink.go"))
	if err != nil {
		t.Fatalf("glob connector sinks: %v", err)
	}

	stampers := 0
	for _, path := range sinks {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		body := string(src)
		if !confirmationStamp.MatchString(body) {
			continue
		}
		stampers++
		if !strings.Contains(body, "textmatch.NormalizedPrefix") {
			t.Errorf("%s stamps a delivery confirmation but never calls textmatch.NormalizedPrefix — "+
				"a post-hoc matcher that compares raw bytes (or re-spells the normalization in SQL, where "+
				"POSIX \\s does not cover the unicode spaces Go's strings.Fields does) fails PERMANENTLY on a "+
				"whitespace-only difference the provider round trip introduced, leaving sent_external_id NULL "+
				"and the row unclaimable. See IK \"Exact text comparison across a provider round trip\"", path)
		}
	}

	// Without this the scan silently passes if the sinks move, the SQL is
	// re-spelled, or the glob stops matching — the failure mode of every
	// source-scanning test.
	if stampers < 3 {
		t.Errorf("found only %d connector sink(s) stamping a delivery confirmation in %v; expected at least 3 "+
			"(jira, slackweb, upworkcrm — google stamps confirmed_at only). The scan has probably stopped "+
			"matching: check the glob and confirmationStamp against the current SQL", stampers, sinks)
	}
}
