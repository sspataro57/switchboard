# Ticket status sync (SWT-32)

The reconciler that keeps the board honest about Jira: a task whose ticket no
longer warrants it — moved to a done status, or (on a gated project) assigned to
someone else — is closed and drops off the board; a ticket that warrants it
again brings the task back to the status it held. One predicate, two facts,
both directions:

```
warranted = statusCategory != 'done' AND (gate off OR assignee == own accountId)
```

It runs at the end of every `connector-jira` tick (after capture, on purpose:
a notification about an already-done or not-mine ticket creates the task and
loses it in the same tick), and by hand:

```
opsctl ticket-status sync --dry-run          # the plan; writes nothing, fetches nothing
opsctl ticket-status sync [--force] [--limit N]
opsctl ticket-status report                  # current state, gates, unpolled refs
```

## The status discriminator

`fields.status.statusCategory.key` — Jira's three-value structure (`new`,
`indeterminate`, `done`) that every custom workflow status maps into. NEVER a
list of status names: names are per-project configuration, and a name list
passes every fixture until the day a client renames a column. The name is
stored in `ticket_status_syncs.status_name` as a diagnostic; nothing branches
on it.

## The assignee gate

Per project, default OFF. Arm it by hand (the reengine ask — collaboratory
stays status-only unless armed):

```sql
UPDATE projects SET ticket_assignee_gate = true WHERE slug = 'reengine';
```

"Me" is the storing account's own accountId — `sync_cursor->>'own_account_id'`,
cached from /myself per source account — never an email or a display name.
**Unassigned counts as not-mine** (Jira sends `"assignee": null`, a positive
fact); a snapshot with NO assignee key at all is missing evidence and the ref
counts `unreadable`, touching nothing.

## The candidate-driven lookup

Reengine's tickets live on a site we do not poll. The task IS the candidate:
the reconciler takes the ticket keys that have tasks (via `external_refs`),
fetches ONLY those issues with a `jira_lookup` account, stores each snapshot
raw-first, and decides from the STORED row — reproducible with the network
unplugged. Create the account (needs the site API token in `JIRA_API_TOKEN`
and `OPS_TOKEN_KEY` set):

```
jira-auth add <email> --site https://avviato.atlassian.net --projects LHH --lookup-only
```

A `jira_lookup` account is invisible to the poller and the normalizer by
construction — no messages, no threads, no capture matches, no funnel change.
`jira-auth list` is the one place that shows it.

**The accepted limit, on purpose:** a ticket that never produced a Slack
message or an email never becomes a candidate, so it never becomes a task —
even if it is assigned to you. That is the shape Salvador chose over a
project-wide poll; do not "fix" a missing task by adding one.

Freshness: a snapshot younger than `TICKET_LOOKUP_TTL` (default 1h; a Go
duration — a bare number falls back to the default) is not re-fetched;
`--force` bypasses the TTL for a smoke. Missing `OPS_TOKEN_KEY` skips the
lookup half loudly and the status half still reconciles.

## Dismissals outrank the reconciler

A task with a `task_dismissals` row (SWT-31) never resurfaces: when its ticket
warrants a task again, the pass appends one log line to the closed task and
stops — recorded once, never repeated. Reopening a dismissal by hand is
deliberate: `opsctl call --tool task_reopen --args '{"task_id":N,"reason":"..."}'`.

## One-off reconciliation

The first hand-run is the reconciliation of the seeded tasks:

```
opsctl ticket-status sync --dry-run   # read the plan first, always
opsctl ticket-status sync
opsctl ticket-status report
```

State lives in `ticket_status_syncs` (one row per ref, UPSERTed);
`last_action='closed'` is the only thing that authorises a later reopen, which
is what keeps the pass from ever resurrecting a close a human made.
