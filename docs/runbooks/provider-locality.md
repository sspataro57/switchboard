# Runbook — the provider locality boundary (SWT-21)

Personal content may only be processed by a model running locally. This is
enforced in code, not configured per worker, and it fails closed.

## Read this before you look at a triage report

**An all-skipped triage report is the SUCCESS state right now.** Triage's entire
inbox is `action='unmatched'`, unmatched is restricted, and no local adapter
exists yet — so every message skips and `processed` is zero. That is the boundary
working exactly as designed, not an outage. Do not open an incident against it.

Triage becomes useful again when **SWT-22** ships the local classifier. The
dependency runs the opposite way from what a reader assumes: **SWT-21 gates
triage on SWT-22**, deliberately, because the alternative was a boundary that
was only as good as somebody's sender list.

## The one rule that no code can enforce

**A fallback to a hosted provider is NEVER correct.**

If the local lane is absent, misconfigured, or unreachable, restricted content is
**skipped and retried next pass**. It is never handed to the hosted provider
instead.

This sentence is the whole mitigation, and it is written here because nothing in
the code can stop the next contributor "fixing" a skip into a fallback. In a diff
that change looks like an availability improvement — try local, fall back on
error, fewer skips, more throughput. What it actually does is turn a slow local
model into a silent hosted one, on precisely the content that must never go
there, with no error anywhere.

`Router.Route` has no code path that hands restricted content to the general
client. Keep it that way.

## Configuration

| variable | lane | meaning |
|---|---|---|
| `OPS_LOCAL_PROVIDER_URL` | local | base URL of the local OpenAI-compatible server, e.g. `http://127.0.0.1:11434/v1` |
| `OPS_LOCAL_MODEL` | local | model name on that server, e.g. `qwen3:8b` |
| `OPS_LOCAL_API_KEY` | local | usually empty; local servers rarely check it |
| `OPENAI_API_KEY` | general | **no longer required to start** — a pass that never touches the hosted lane is now normal |
| `OPENAI_BASE_URL` | general | optional override |

**`OPS_LOCAL_PROVIDER_URL` is not just configuration — it is the evidence.**
Locality is derived from that URL, so pointing it at a hosted API does not
redirect the local lane; it makes the lane not-local, and restricted content
stops flowing. The misconfiguration you would most fear is the one that trips the
guard. Triage logs a warning at startup when the URL is not local.

Loopback and private LAN addresses both classify local, so `127.0.0.1:11434` and
`192.168.50.x` work with no special case. `http`/`https` only.

## Verifying the boundary

```bash
eval "$(grep '^export OPS_DATABASE_URL=' ~/.bashrc)"

# 1. the column exists and personal is the only local_only project
psql "$OPS_DATABASE_URL" -tAF' | ' -c \
  "SELECT slug, COALESCE(client,'(null)'), ai_locality FROM projects ORDER BY slug;"
```

Expect every pre-existing project to read `any` and `personal` to read
`local_only` with a **NULL client**. That NULL is load-bearing: `task_get_next`
excludes personal tasks through its existing `p.client = $1`, which is why this
ticket did not add a locality predicate there. Give the row a client name and
that reasoning silently becomes false.

```bash
# 2. what a pass decided, without running one
psql "$OPS_DATABASE_URL" -tAF' | ' -c \
  "SELECT status, count(*) FROM ai_runs WHERE worker_type='triage' GROUP BY 1;"

# 3. why it skipped
psql "$OPS_DATABASE_URL" -tAF' | ' -c \
  "SELECT input FROM ai_runs WHERE worker_type='triage' AND status='skipped'
    ORDER BY id DESC LIMIT 1;"
```

A skip record is **one aggregate row per pass**, not one per message — carrying
the availability reason, a breakdown by why each message was restricted
(`unseen`, `unmatched`, `project_local_only`, `thread_context`), the count, and a
capped sample of message ids. One row per message would be ~16,000 rows per pass
recording nothing the aggregate does not say better; that is SWT-17's
amplification lesson applied before it bit rather than after.

A `skipped` run writes **no `ai_extractions` row**. That is what makes "no
permitted provider looked" structurally different from "the model looked and
found nothing" — two facts that must never share an alarm.

## When the local box is down

Nothing to do. Skips are not errors, they do not count toward the
consecutive-error abort, and the run exits zero. The messages stay pending and
the next pass picks them up.

The pass raises only on a **pattern**: when unclassified errors exceed half a
pass's restricted-lane attempts, with a floor of 20 attempts. That threshold is a
guess and is labelled one in the code; tune it against a real failure rather than
in the abstract. A broken adapter fails on everything and trips it; one malformed
message does not.

## The personal sender list

Migration 0016 creates the `personal` project but seeds **no** capture rules.
Routing is configuration with an enabled flag and an audit trail (SWT-17), so
putting it in a migration would install production routing into every test
database and make a rule edit a new migration. **This section is therefore the
only record of what the boundary's input actually is.** Keep it in step with the
database.

Read this first, because it is the thing most likely to be misread: **the rules
are NOT what keeps personal mail local.** Unmatched is already restricted, so a
personal message no rule happens to match is kept local anyway. Rule
completeness is deliberately not load-bearing for the security property — if it
were, the boundary would only ever be as good as somebody's sender list. What
these rules buy is routing *quality*: the mail lands in a named project instead
of sitting in triage's residue, and its thread siblings classify correctly.

### Where the list came from

Measured against the real corpus, not guessed — the same discipline as the
classifier spike:

```bash
psql "$OPS_DATABASE_URL" -tAF' | ' -c "
WITH latest AS (
  SELECT DISTINCT ON (message_id) message_id, action
  FROM capture_decisions ORDER BY message_id, id DESC)
SELECT lower(substring(nm.sender from '@([A-Za-z0-9._-]+)')), count(*)
FROM normalized_messages nm
JOIN latest l ON l.message_id = nm.id AND l.action = 'unmatched'
GROUP BY 1 ORDER BY 2 DESC LIMIT 60;"
```

`DISTINCT ON (message_id) … ORDER BY id DESC` is not decoration. Shadow mode
writes a decision **per pass**, so a plain count over `capture_decisions`
triple-counts: the same query without it reported 49,493 unmatched messages
against a real figure of 16,066, and every per-sender count was inflated about
3×. Always reduce to the latest decision per message before counting anything in
this table.

### The commands

Run after 0016 is applied — `--project personal` fails until the project row
exists.

```bash
add() { go run ./cmd/opsctl capture-rules add --project personal --type sender \
          --priority 5 --pattern "$1" --note "$2"; }

# banking and credit
add bankofamerica.com        'personal: BofA alerts, statements, receipts'
add salliemae.com            'personal: student loan servicer'
add citi.com                 'personal: Citi card'
add '@chase.com'             'personal: Chase'
add wellsfargo.com           'personal: Wells Fargo'
add servicing.synchrony.com  'personal: Synchrony servicing'
add yourmortgageonline.com   'personal: mortgage servicer'
add billing.fpl.com          'personal: utility billing'

# HOA and municipal
add pinespropertymanagement.com 'personal: HOA manager - announcements AND fine notices'
add ppines.com                  'personal: HOA'
add cityofpembrokepines         'personal: municipal notices'

# health
add adapthealth          'personal: medical supplier billing'
add edelivery.uhc.com    'personal: health insurer'
add questdiagnostics.com 'personal: lab results and billing'
add walgreens.com        'personal: pharmacy'
add eclinicalmail.com    'personal: clinic portal'
add remindmemd.com       'personal: appointment reminders'
add e.petinsurance.com   'personal: pet insurance'
```

Then verify, in evaluation order, and re-run the shadow pass:

```bash
go run ./cmd/opsctl capture-rules list
go run ./cmd/opsctl capture-rules run          # shadow; add --live only when satisfied
go run ./cmd/opsctl capture-rules report
```

`capture-rules add` takes `--type` (not `--kind`), `--external-system` (not
`--source`), `--key-regex`, and `run`/`report` take `--live` and `--since`. Flag
names here were checked against `cmd/opsctl/main.go`; the first draft of this
runbook invented four flags from Go struct-field names and none of them existed.

### What the list covers, and what it does not

1,601 of the 16,066 currently-unmatched messages — **10%**. The pattern is a
case-insensitive substring of the raw `From` header, so a bare domain covers
every subdomain (`bankofamerica.com` picks up `ealerts.`, `emcom.`, `receipts.`,
`servicing.`, `clientfeedback.`). `@chase.com` keeps its `@` on purpose:
`chase.com` alone is a substring of `purchase.com`.

`--priority 5` puts every one of these **below** all client rules (10 through
100) so a personal rule can never claim a client message; it sits above only the
Treetop Slack catch-all at priority 1, which matches a different channel
entirely.

Deliberately excluded, and each will need a decision rather than a default:

- **`mhs.net`** — `recruiting.mhs.net` is job mail and `mhs.net` is a hospital
  system. One substring cannot separate them; splitting the rule needs a look at
  the actual traffic.
- **`aihealthstrategist.com`** (222) — a newsletter, not health data. The name
  matches a health regex; the content is marketing. This is the reason the list
  was built from measured senders and eyeballed, rather than from a keyword
  regex over sender addresses.
- The long tail below ~10 messages. Unmatched is restricted anyway, so a sender
  missing from this list is a routing gap, never a leak.

## Drafts skips too, and for a reason that surprises people

The boundary classifies the WHOLE request, not just its subject. `drafts` folds
the Deliver task's project together with every thread message whose body goes
into the prompt, and takes the most restrictive.

An **inbound** thread message with no `capture_decisions` row is `unseen`, and
unseen is restricted. So a Deliver task on an ordinary `any` project **still
skips** if any inbound message on its thread has not been through a capture pass
yet — mail that arrived since the last run, since the capture pass hitchhikes on
the connector CronJobs.

That is the designed behaviour, not a bug: the alternative is shipping a personal
sibling's body to a hosted API because nobody had classified it yet. It clears on
its own with the next capture pass. To clear it now, run one — in shadow, which
is what this system is still in:

```bash
go run ./cmd/opsctl capture-rules run
```

(`--live` is a separate decision and is out of scope for SWT-21; a shadow pass
writes the decisions this fold reads.)

**Outbound messages are excluded from the fold**, and that exclusion is
load-bearing rather than an optimisation. The capture engine filters
`direction = 'inbound'` — that line *is* invariant 5 — so an outbound message
will never carry a decision row on any pass in any mode. Folding one would read
"no decision" as "unclassified" when it means "not applicable", and would block
every thread the system has ever replied on, permanently, with no remedy.

What makes it safe is **inheritance**: an outbound message takes its
conversation's class, which is folded anyway from the task's own project and from
every inbound sibling. (Note that `direction='outbound'` means only "sent from
one of the five own accounts" — mostly mail typed in Gmail by hand. It does *not*
mean the content passed a delivery policy gate; there are 21,194 outbound
messages and 1 delivery row.)

**Residual, so nobody discovers it the hard way:** the outbound body is still
rendered into the prompt — it is excluded from the class fold, not from the
conversation. A hand-written reply that pastes personal material into a client
thread introduces content no inbound sibling holds and no project describes, and
it will reach the hosted lane. Dropping outbound from the prompt would break the
draft worker's job; folding it restricted breaks every replied-on thread. This is
an accepted gap.

The skip is logged onto the Deliver task as a `draft_skip` entry naming the
reason, so a task sitting idle says why on its own timeline.

## Adding a provider adapter

`Describe()` is on the `provider.Client` interface, so a new adapter that does
not declare where it sends **will not compile**. That is deliberate — a registry
can be missed and a config flag can be wrong, but the compiler cannot be talked
out of it.

If the adapter may serve the local lane it must also implement `Prober`. A local
client that cannot demonstrate reachability is treated as unreachable: a
declaration is not evidence, and this boundary does not accept declarations as
proof anywhere else either.

**Test fakes are the trap.** A fake that declares a local endpoint and skips
`Probe` makes its suite SKIP rather than exercise its subject — a green test that
tests nothing. The same shape bit the 23 project fixtures when `ai_locality`
defaulted to `local_only`. If a suite suddenly passes suspiciously fast, check
whether it is skipping.
