package jira

import "strings"

// IssueRawID is the ONE spelling of the raw_source_items.external_id an issue
// snapshot is stored under (SWT-32 criterion 5) — by the poller (ingestIssue)
// and by the candidate-driven lookup alike, so both halves of the reconciler
// read the SAME rows and a key can never grow two snapshots under two ids.
func IssueRawID(key string) string {
	return "issue:" + key
}

// CommentRawIDPrefix is the ONE spelling of the prefix every comment of key is
// stored under (`comment:{KEY}:{id}`): ingestIssue writes it, and capture's
// own-action guard (SWT-45) asks whether any of them still awaits normalization.
func CommentRawIDPrefix(key string) string {
	return "comment:" + key + ":"
}

// ParseIssueRawID is IssueRawID's inverse: ok is false for a comment raw id
// (`comment:{KEY}:{id}`), for a bare issue key, and for anything else. The
// normalizer's issue/comment switch is this pair's only reader; a HasPrefix in
// one file and a helper in another is exactly the drift the pair exists to stop.
func ParseIssueRawID(externalID string) (key string, ok bool) {
	key, found := strings.CutPrefix(externalID, "issue:")
	if !found || key == "" {
		return "", false
	}
	return key, true
}
