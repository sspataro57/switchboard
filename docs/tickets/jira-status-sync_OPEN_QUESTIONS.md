> Jira: SWT-32
> SPEC: docs/tickets/jira-status-sync_SPEC.md

# jira-status-sync — open questions

**STATUS: ANSWERED** (Salvador, 2026-09-09). Q1's answer is folded into the SPEC;
that document is authoritative and is no longer provisional. This file is the
record of the decision and of the alternatives it beat.

---

## Q1. Reengine's assignee is unknowable without polling Avviato, which you refused

**The bind.** The assignee gate compares `fields.assignee.accountId` in a stored raw
issue against the polling account's own `accountId`. That works for `collaboratory`
because the connector polls `treetopllc.jira.com` — those tasks exist *because*
`jira:treetopllc.jira.com:WEB-*` threads exist. It cannot work for `reengine`: those
tasks come from `body_regex LHH-[0-9]+` and `sender jira@avviato.atlassian.net`
matching **Slack messages and notification emails**, and there is no
`source_accounts` row for `avviato.atlassian.net`, so there is no LHH issue in
`raw_source_items` and never has been. The recorded reason, from the capture-rules
SPEC: *"I don't want to poll avviato, it is not my main thing; that work is manual
anyway so slack or email capturing those jira tickets as jobs on reengine for human
resolution is ok."*

So "reengine tickets must be assigned to me" needs a source for "assigned to whom",
and the only structural one is a fetch.

### The three options offered

**(a) Register an Avviato jira account, normal ingest.** `jira-auth add … --projects
LHH`, polled by JQL like Treetop. Full status + assignee, no new code paths. Cost:
every LHH issue in scope becomes an inbound `normalized_messages` row (description +
every comment), and the priority-100 `LHH-[0-9]+` capture rule matches all of them —
so capture creates a task for *every* LHH ticket, not just the ones someone emailed
about. The reconciler then closes the ones that are not his in the same tick, so the
board self-cleans, but the corpus, the funnel counters and `capture_decisions` all
grow by the whole project.

**(b) Register an Avviato account in a status-only mode: raw-first, never
normalized.** Ingest issues into `raw_source_items` and exclude the account from
`jira.Normalize`, so no messages, no threads, no capture matches. Keeps the refusal
intact in the sense it was meant. Cost: a per-account exclusion mechanism plus the
token — and it still polls the whole project.

**(c) Don't fetch anything. Clean the current reengine tasks by hand, once, and
accept that reengine has no recurring gate.**

**Not offered, and why:** parsing the assignee out of the Jira notification email
body. It is a template Atlassian owns and can change without telling us, it is the
exact fragility the runbook already warns about for the GitHub Message-ID rules, and
a wrong parse does not fail loudly — it silently drops a task off the board.

### Answer (Salvador, 2026-09-09): a fourth shape — CANDIDATE-DRIVEN LOOKUP

Not (a), not (b), not (c). Salvador proposed the shape himself and it is now locked:

> capture keeps creating reengine tasks from Slack/email exactly as today — **the
> task IS the candidate**. The reconciler takes the ticket keys that actually have
> tasks (via `external_refs`, system `jira`), fetches ONLY those issues from
> `avviato.atlassian.net` by key, stores each snapshot raw-first before reading it,
> keeps those rows out of the normalizer, and applies the same `warranted`
> predicate.

It is (b)'s never-normalized property without (b)'s poll. Consequences, all now in
the SPEC:

- **No project-wide poll, no JQL, no scope sweep, no first-tick flood.** Request
  volume is bounded by the number of jira-keyed TASKS (tens), not by the size of the
  LHH project. Option (a)'s flood warning is now a *rejected alternative* in
  "Out of scope", not a caveat on the mechanism.
- **The funnel is untouched**: no thread, no message, no capture decision, no triage
  inbox row.
- **Invariant 1 is mechanical, not a formality** (D19): the fetched issue is written
  to `raw_source_items` through the same `upsertRaw` the poller uses, and the
  decision reads the STORED row, never the HTTP response — so every close is
  reproducible from raw bytes with the network unplugged.
- **The never-normalized mechanism is a distinct provider value, `jira_lookup`, not
  a flag** (D17). Every existing jira query already filters `provider='jira'`
  (`ListAccounts`, `accountMeta`, `pendingRaw`), so a lookup account is invisible to
  all three by construction: forgetting a clause makes it *invisible*, where a
  boolean flag would make a forgotten clause a poll or a funnel flood, silently.
  `provider` has no CHECK constraint, so this costs no DDL.
- **Routing is by ticket-key prefix against the account's existing mandatory
  `scopes`** (D18) — `LHH-123` goes to the account declaring `LHH`. This preserves
  09-jira-github-connectors' unscoped-poll refusal verbatim and, crucially, does NOT
  route on `external_refs.external_url`, which `link_external_ref` writes as
  agent-facing free text and which could otherwise aim our token at a chosen host.
- **Identity anchor for Avviato**: `/myself` under the new token, cached in that
  account's `sync_cursor.own_account_id` by the existing merge-write (`sync_cursor ||
  $2::jsonb`), exactly as `Ingest` does for the polled sites.
- **Freshness is a TTL, not a cursor** (D20, default 1h, `--force` to bypass): there
  is nothing to page, and a cursor on a candidate-driven fetch would be state whose
  only job is to be wrong after a re-assignment.
- **Treetop/collaboratory is unchanged**: its snapshots are already in
  `raw_source_items`, so the status half makes no network call at all.

**The limit Salvador accepted, explicitly:** an Avviato ticket assigned to him that
never produced a Slack message or an email never becomes a task, because nothing ever
made it a candidate. That is the same trade as his SWT-17 stance — the notification
is still what surfaces work; this ticket only decides whether surfaced work stays
surfaced.

**Prerequisite this creates:** an Avviato API token, created by Salvador and stored
with `jira-auth add --lookup-only <email> --site https://avviato.atlassian.net
--projects LHH` (verification protocol step 0b). Until it exists, the status half
runs normally and reengine refs simply count `unpolled`.

---

Answer by editing the entries. Say "questions answered" and I'll fold them into the
SPEC.
