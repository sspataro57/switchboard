package jira

// The candidate-driven lookup (SWT-32 criterion 9, D16): GET exactly the keys
// the reconciler hands over — the keys come from external_refs, never from the
// provider — and store each snapshot raw-first through the SAME upsertRaw the
// poller uses, so the lookup's snapshot and the poller's snapshot are the same
// row for the same issue. No search, no pagination, no watermark: there is
// nothing to page, and a cursor on a candidate-driven fetch would be state
// whose only job is to be wrong after a re-assignment (D20).
//
// Comments are deliberately NOT stored: a lookup snapshot exists to answer two
// questions — statusCategory and assignee — and comment rows under a
// jira_lookup account would be rows nothing ever reads (they are never
// normalized, D17).

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// LookupIssues GETs each key by name and stores it raw-first under the shared
// issue id. The caller (the reconciler) has already routed each key to this
// account by declared prefix; the check here is the same refusal at the place
// that actually holds the token, so a future caller that skips the router
// cannot widen it (D18's safety property, 09-jira-github-connectors' verbatim).
func LookupIssues(ctx context.Context, c *Client, sink Sink, acct Account, keys []string, cfg Config) (Stats, error) {
	var stats Stats
	runID, err := sink.StartRun(ctx, acct.ID)
	if err != nil {
		return stats, fmt.Errorf("start jira lookup run: %w", err)
	}
	fail := func(cause error) (Stats, error) {
		_ = sink.FinishRun(ctx, runID, "error", stats, cause.Error())
		return stats, cause
	}

	if len(acct.Projects) == 0 {
		return fail(fmt.Errorf("jira lookup account %s has no project scoping (scopes) — an unscoped lookup is refused", acct.Email))
	}
	declared := map[string]bool{}
	for _, p := range acct.Projects {
		declared[p] = true
	}

	// /myself once per PASS, not once per key: it is the identity of the site.
	// Merged into the cursor with the existing SaveCursor (a jsonb merge, so
	// nothing else in the cursor is clobbered) — and no watermark is written.
	cur, err := sink.Cursor(ctx, acct.ID)
	if err != nil {
		return fail(fmt.Errorf("read cursor: %w", err))
	}
	own, err := c.Myself(ctx)
	if err != nil {
		return fail(fmt.Errorf("fetch own accountId: %w", err))
	}
	cur.OwnAccountID = own
	if err := sink.SaveCursor(ctx, acct.ID, cur); err != nil {
		return fail(fmt.Errorf("save cursor: %w", err))
	}

	for _, key := range keys {
		prefix, _, found := strings.Cut(key, "-")
		if !found || !declared[prefix] {
			return fail(fmt.Errorf("key %s is outside account %s's declared prefixes %v — the refusal "+
				"comes before the token is spent", key, acct.Email, acct.Projects))
		}
		raw, err := c.GetIssue(ctx, key)
		if err != nil {
			// Per-key resilience: a deleted ticket, a permission gap or a
			// transient 404 must not abort the whole account's fetch — the
			// reconciler treats the missing snapshot as unpolled and the
			// previous snapshot, if any, still decides (criterion 32). Loud,
			// per key, so the log says which issue could not be read.
			slog.Warn("jira lookup: fetch failed; the stored snapshot, if any, still decides",
				"key", key, "account", acct.Email, "err", err)
			continue
		}
		issueOnly, _, _, err := splitIssueComments(raw)
		if err != nil {
			return fail(fmt.Errorf("split issue %s: %w", key, err))
		}
		stats.IssuesFetched++
		if err := upsertRaw(ctx, sink, acct.ID, IssueRawID(key), issueOnly, &stats); err != nil {
			return fail(err)
		}
	}

	if err := sink.FinishRun(ctx, runID, "ok", stats, ""); err != nil {
		return stats, fmt.Errorf("finish jira lookup run: %w", err)
	}
	return stats, nil
}
