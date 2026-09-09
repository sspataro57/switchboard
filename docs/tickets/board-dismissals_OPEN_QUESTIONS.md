> Jira: SWT-31

# board-dismissals — open questions

One question. Everything else in the SPEC is decided (see "Decisions made
unilaterally"); this one changes which column `loadRules` selects, so it is
cheaper to answer than to unpick later.

## Q1 — the human prefix on a thread-keyed task title

Verified in the code, since the request assumed otherwise: `storedRule`
(`internal/capture/rules_store.go:171`) carries `rule.Project` — the project
**slug** — plus `projectID`, `subproject`, `extSystem`, `urlTemplate`. It does
NOT carry `projects.name` or `projects.client`; `loadRules` selects `p.slug`
only. So "Saka" costs one more column in that SELECT (and the integration test
of criterion 3, which has to prove the column reaches the title).

The other candidate is already in hand and costs nothing: `pm.msg.Sender`, which
for upworkcrm is the CRM sender column — a display name ("Mario Cruz"), not an
address.

**Project name** — `Saka — Hi Salvador, I wanted to check in about…`
Your own example. Stable per project, groups visually, matches how you refer to
the engagement. Cost: it duplicates the board's existing `project` column
(`Project` is rendered on every row of `tasks.html`), so the title spends its
first 20 characters repeating the cell next to it, and it says nothing about
which of a client's rooms or people the message came from.

**Message sender** — `Mario Cruz — Hi Salvador, I wanted to check in about…`
Adds the one fact the board row does not already show, and the row still says
`saka` in the project column, so nothing is lost. Cost: display names come from
the CRM's sender column, so they are whatever the provider stored — for a
two-client project (saka has TWO CRM client records, rules 57+58) it may or may
not be the person you expect, and it is empty on some sources, which means a
fallback chain the project name does not need. Also: sender is already printed
in the task body (`ruleTaskBody` writes `sender:`), so this is a promotion of
existing information, not new information.

(A third form — `Saka / Mario Cruz — Hi Salvador,` — is available for the price
of both, and burns ~30 of the 120-rune budget before any content.)

---

**Answer (Salvador, 2026-09-09): message sender** — "Mario Cruz — Hi
Salvador,". The project is already its own board column; the sender is the fact
the row lacks. Fallback when the source stored no sender: project name (then
slug, then the key). Folded into SPEC criteria 2-4.
