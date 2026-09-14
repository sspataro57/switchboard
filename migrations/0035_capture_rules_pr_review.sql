-- 0035 treetop-pr-review-tasks (SWT-54, docs/tickets/treetop-pr-review-tasks_SPEC.md).
--
-- Deploy order: apply BEFORE any image built with this file runs. loadRules selects
-- capture_rules.pr_review / .exclude_pr_authors on every capture pass, so a new image
-- on a db without 0035 fails capture for every connector. Old images never name the
-- columns and are unaffected (docs/runbooks/HANDOFF-kube-treetop-pr-review-tasks.md).
--
-- pr_review: the rule's matches are GitHub PR notification mail. On the create branch
-- capture reads authorship from the stored raw headers (X-GitHub-Reason / -Sender /
-- -Recipient) and falls through to the next rule when the PR is his own or its author
-- is excluded. exclude_pr_authors: logins treated like his own (his other accounts,
-- or bots): an entry equals a login case-insensitively, or '*'+suffix matches a suffix.
--
-- The pr_review CHECK is FAIL-CLOSED on a NULL external_system (SPEC amendment
-- 2026-09-14): `external_system = 'github'` alone is NULL for a NULL system, the AND
-- is then NULL, `false OR NULL` is NULL, and a CHECK passes on NULL, so an
-- attribution-only pr_review rule would have been stored. The explicit IS NOT NULL
-- makes that row FALSE and refused.
--
-- Rules are armed by capture_rule_add (the executor), never by a migration.
ALTER TABLE capture_rules
  ADD COLUMN pr_review          BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN exclude_pr_authors TEXT[]  NOT NULL DEFAULT '{}',
  ADD CONSTRAINT capture_rules_pr_review_github
    CHECK (NOT pr_review OR (external_system IS NOT NULL AND external_system = 'github'
                             AND key_regex IS NOT NULL AND NOT revive)),
  ADD CONSTRAINT capture_rules_exclude_needs_pr_review
    CHECK (cardinality(exclude_pr_authors) = 0 OR pr_review);
