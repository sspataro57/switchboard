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

## Capture-time assignee gate (SWT-40 Part D)

The reconciler above only ever sees tasks that already exist. On a gated
project that meant every LHH mention created a task first and the reconciler
closed it about a day later (`not_assigned` or `ticket_done`). The gate checks
the ticket BEFORE a task exists: assigned to him and still open means a task,
exactly as before; otherwise there is no task and no log line.

**The trigger.** The winning capture rule has `external_system='jira'`, a
ticket key was derived, and the rule's project has `ticket_assignee_gate`
(the column, loaded with the rules). Capture then records action **`held`**
instead of `task`/`task_log`. The row names the project, rule and key, and
nothing is created. Shadow mode writes `held` too. Gate-off projects
(collaboratory) are unchanged, and so is an unkeyed match, which stays
`attributed`.

**Why capture never calls Jira.** Capture runs inside every connector main,
and an LHH link arrives through slackweb and google as well as jira. The lookup
credential (`OPS_TOKEN_KEY` plus the stored token) exists only where the lookup
runs. A capture-time HTTP call would spread that secret to every connector, or
silently skip the check where it is absent. So `held` is resolved later by the
`gate` stage in `pipelined` (`docs/runbooks/pipeline.md`). It is woken by
`captured`, with the 5 min sweep as the fallback, and it takes capture's
advisory lock.

**The resolution** is a second `capture_decisions` row with `mode='gate'`, one
per message forever (`capture_decisions_gate_uniq`). It is claimed before any
executor call. It uses the reconciler's own stored snapshot and its own
predicate (`ticketstatus.EnsureSnapshots` and `ticketstatus.Warranted`), so the
gate never creates a task this reconciler would close 15 minutes later. There
are four outcomes:

| ticket | gate row | writes |
|---|---|---|
| assigned to own account, open, no task yet | `task` | create_task + link_external_ref + task_set_source_thread, actor `capture:gate` |
| same, and the key already has a task | `task_log` | task_append_log; plus the guarded task_reopen if a human dismissed that task (SWT-36) |
| not his, unassigned, done or a delivered status | `attributed`, reason `not_assigned` / the done reason / `ticket_delivered` | nothing |
| still unreadable after 72h | `attributed`, reason `gate_unverified_expired` | nothing (fail closed) |

An **unreadable** hold writes nothing and stays held (`pending_lookup`). That
covers Jira unreachable, no credential, a per-key fetch failure, or a snapshot
older than the message (below). It is retried on the next wake or sweep. A hold
whose key is over this pass's budget is left untouched too, counted
`budget_skipped`.

**Freshness.** A stored snapshot decides a hold only if it was verified at or
after the message was first seen (`normalized_messages.created_at`, which no
upsert rewrites). "Verified" is the snapshot's `ingested_at`, or the start of
this pass's successful GET when the ticket came back unchanged (an unchanged
refetch leaves `ingested_at` alone). So a key whose snapshot predates a held
message is fetched whatever the TTL. This is the D-D6 case: the gate saw the
ticket unassigned, the ticket was then assigned to him, and the assignment mail
arrives inside the hour. If that fetch fails, the older snapshot is no verdict:
the hold stays pending and, if Jira stays down, expires fail-closed. Keys
routed to no lookup account (poller-only projects) are never force-fetched, so
their holds resolve only when the poller stores a newer copy of the ticket.
Today every gated project (reengine) is lookup-routed.

**Fetch, then lock.** The gate fetches BEFORE it takes capture's advisory lock
(fetching is idempotent raw-first ingestion), then locks, re-reads the inbox and
decides from the stored snapshots. Every connector's capture pass takes the same
lock, so none of them waits on Jira. A pass that finds the lock busy skips before
any GET. Token-built Jira clients time out after 30 s per request.

**Cache and rate.** The cache IS the stored raw snapshot, the same row the
reconciler reads. `TICKET_LOOKUP_TTL` (1h) applies to keys whose snapshot is
newer than every held message naming them. A burst of mentions costs one GET: the
pass fetches once for the key's newest hold. The gate and the reconciler share
every fetch. A pass looks up at most **50 distinct keys**; holds on the rest are
left untouched for the next pass or sweep (`budget_skipped`). `/myself` is called
once per lookup account per pass. There are no in-pass retries: the retry rate
is the sweep.

**Expiry.** `GateMaxAge = 72h`, measured from when the HOLD was written (the
held row's `created_at`), not the message's `sent_at`, so a message captured
late still gets its full window. After that, a hold that is still unreadable
resolves `attributed (gate_unverified_expired)` and stops costing GETs. That is
the fail-closed direction: no task. Without `OPS_TOKEN_KEY` on pipelined, fresh
holds pile up as `pending_lookup` and every one of them expires this way. Check
the key first if the report shows only expiries.

**Turning a project's gate off while holds are pending.** The hold rows stay;
the gate stage still resolves them, but it reads `ticket_assignee_gate` from the
column on every pass, so they resolve with the gate OFF: a warranted-by-status
ticket becomes a task (or a log) WITHOUT the assignee check, just as capture
would have done with the gate off. If that is not what you want, let them
resolve before switching the gate off, or dry-run first (below) to see them.

**Later assignment.** A resolution is final for its message. A ticket assigned
to him later gets its task from the next mention, and there always is one: the
Jira assignment notification mail itself (the `jira@avviato.atlassian.net`
rule), which holds and then resolves `task`.

**The backstop.** The reconciler is unchanged. It closes a gate-created task
when the ticket is reassigned away or finished, and reopens it when the ticket
comes back.

**Reading it.** `opsctl capture-rules report` prints a `GATE` section: the
`held` count, `pending_lookup`, and one line per resolution (`gate task
warranted`, `gate attributed not_assigned`, …). Its crash-artifact WARNING line
counts `task` decisions with no `task_id` in live AND gate rows: a pass that
died between the gate's claim and `create_task`. pipelined logs a `gate pass`
line with the same counters on every pass, plus `budget_skipped`.

**Running it by hand.** `opsctl capture-rules gate` runs one gate pass, exactly
as the pipelined stage does (needs `OPS_TOKEN_KEY` to fetch). `opsctl
capture-rules gate --dry-run` decides every live hold from the STORED snapshots
only. It fetches nothing, takes no lock and writes nothing, and it prints one
line per hold: `message=<id> key=<key> outcome=<task | task_log |
attributed:<reason> | pending_lookup | budget_skipped>`. Add `--shadow` to read
the latest shadow `held` rows instead (the V5 preview before capture goes
live). A dry run never fetches, so a hold whose stored snapshot is older than
its message reads `pending_lookup` there even when a live pass would fetch and
resolve it. By hand:

```sql
-- holds still waiting, oldest first
SELECT h.message_id, h.external_key, m.sent_at
  FROM capture_decisions h JOIN normalized_messages m ON m.id = h.message_id
 WHERE h.mode = 'live' AND h.action = 'held'
   AND NOT EXISTS (SELECT 1 FROM capture_decisions g WHERE g.message_id = h.message_id AND g.mode = 'gate')
 ORDER BY m.sent_at;
```

Any `ON CONFLICT` against `capture_decisions` must restate its partial
predicate (`WHERE mode = 'live'` / `WHERE mode = 'gate'`). A structural test
scans for this.
