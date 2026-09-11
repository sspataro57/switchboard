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

A task with an OPEN `task_dismissals` row (SWT-31; `reopened_at IS NULL`
since SWT-36) never resurfaces: when its ticket warrants a task again, the pass
appends one log line to the closed task and stops — recorded once, never
repeated. Reopening a dismissal by hand is deliberate:
`opsctl call --tool task_reopen --args '{"task_id":N,"reason":"..."}'`.

**D4 now means an OPEN dismissal (SWT-36 D9).** A dismissal that was overtaken
by new inbound activity (capture or promote reopened the task and stamped the
row), or undone by a human's plain reopen, no longer suppresses: the task is
ordinary again. If its ticket is Done or assigned away, this pass closes it in
the same jira tick (capture runs first), so it ends closed with a log line —
intended, the ticket's state outranks a comment — and because that close is
the pass's own, it reopens later if the ticket warrants it. A human
re-dismissal writes a new open row and re-arms the suppression.

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

The lookup writes one `sync_runs` row per account per pass with its fetch
counters in `stats` — diagnostic only: **nothing branches on that payload**
(the upworkcrm two-rows landmine), and nothing should start to.

## Delivered statuses: "QA means I delivered" (SWT-34)

A ticket can stop warranting a task without being *finished*. When Salvador
hands work back — a client's QA column — the ball is in someone else's court,
but `statusCategory` cannot say so: a QA column and a work-in-progress column
are BOTH `indeterminate`. So the reconciler gains a third, **per-project
configured** clause over the status NAME.

This does not overturn SWT-32's D2. `statuscategory` remains the discriminator
for *is this ticket finished* — Jira's own structure, in code. `not my turn` is
one team's workflow, so it lives in data an operator wrote:

```sql
-- arm (collaboratory; drops its TT-In QA tasks on the next pass)
UPDATE projects SET ticket_delivered_statuses = ARRAY['TT-In QA'] WHERE slug = 'collaboratory';

-- revert: the next pass puts those tasks back in the status they held
UPDATE projects SET ticket_delivered_statuses = '{}' WHERE slug = 'collaboratory';
```

Empty (`'{}'`, the default) is today's behaviour exactly, so every other project
is unaffected until armed. Matching is EXACT on a normalized form — lowercased,
unicode whitespace collapsed — never substring: a `QA` substring would also eat
a "QA Blocked" or "Needs QA Rework" column, where the ball IS in his court.
Adding a status is one array element.

`TT-In Review` is deliberately NOT armed, and this is settled rather than
pending (Salvador, 2026-09-10): **in review means CI is running on it — the work
is not ready**. It is not a hand-back, so its tasks stay on the board. Do not
add it to the array "for symmetry" with QA.

Because it is a clause in the same `warranted` predicate, **reopen comes free**:
a ticket leaving the delivered set is warranted again and the existing reopen
path restores the task to the status it held. The drop is recorded as
`drop_reason='ticket_delivered'`, which is also what `opsctl ticket-status
report` and the `closed_ticket_delivered` counter say. The report additionally
prints each project's armed set and whether the row's own status is a member —
an armed set that matches nothing otherwise looks identical to an unarmed one,
and a mis-typed entry is the likely failure.

**The gap, until `qa-question-resurface` ships.** Salvador also asked that a
*fresh question* resurface a dropped task. That half is deferred, because a
question is not a status change: bolting it onto `warranted` would let the very
next pass re-close the task, giving a board row that flaps every 15 minutes. It
needs its own recorded action plus a re-close suppression. In the meantime a
client question on a dropped ticket IS still recorded — capture appends it as a
`log` event on the closed task — it is simply surfaced nowhere. To read those by
hand:

```sql
SELECT te.task_id, te.created_at, te.payload->>'message'
  FROM task_events te
  JOIN ticket_status_syncs s ON s.task_id = te.task_id
 WHERE te.event_type = 'log' AND s.drop_reason = 'ticket_delivered'
   AND te.created_at > s.acted_at
 ORDER BY te.created_at DESC;
```
