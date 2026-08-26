# Runbook — re-keying Upwork threads onto rooms (SWT-19)

One-shot. Re-normalizes the existing Upwork corpus so threads carry the room the
source named, where it named one. **No migration, no schema change, nothing to
roll back on the database side.**

## What this actually does, stated precisely

Thread keys gain a second shape:

```
roomed    upwork_crm:{client_id}:room:{room_id}
unroomed  upwork_crm:{client_id}:{channel}      <- byte-identical to today's key
```

A message is roomed **iff the source row named a room**, in either
`communications.upwork_room_id` (observed in) or `communications.send_room_id`
(dispatched to). Never inferred from the client, never from sibling messages.

The resulting delivery-matching scope is **room-scoped for API-era traffic in
both directions, client-wide for pre-2026-07-21 history**. Do not describe it as
"room matching" — SWT-18 made that overstatement and it was wrong on production
data. 576 outbound rows have no room in either column and stay on one legacy
thread per client.

## Before

Nothing to prepare. The room columns are already in `raw_source_items.raw_json`
(the source query selects `to_jsonb(m)`), so this reads no new data.

Baseline — run these and keep the output:

```bash
eval "$(grep '^export OPS_DATABASE_URL=' ~/.bashrc)"

# corpus size and how many rows SHOULD end up roomed, computed independently
# of the normalizer. This is the number to check against afterwards.
psql "$OPS_DATABASE_URL" -tAF' | ' -c "
SELECT count(*) AS total,
       count(*) FILTER (WHERE COALESCE(NULLIF(r.raw_json->>'upwork_room_id',''),
                                       NULLIF(r.raw_json->>'send_room_id','')) IS NOT NULL) AS should_be_roomed
  FROM raw_source_items r JOIN source_accounts a ON a.id=r.source_account_id
 WHERE a.provider='upwork_crm' AND r.external_id LIKE 'communications:%';"

# current thread count (all legacy, all ending :upwork)
psql "$OPS_DATABASE_URL" -tAc \
  "SELECT count(*) FROM normalized_threads WHERE thread_key LIKE 'upwork_crm:%'"
```

**Do not write these numbers down as expected constants.** The corpus is live —
the connector ingests every 15 minutes, and it moved from 2,441/432 to 2,442/433
in a single afternoon. Re-measure at verification time; that is the whole point
of the query above.

## Run

```bash
eval "$(grep '^export OPS_DATABASE_URL=' ~/.bashrc)"
export UPWORK_CRM_DATABASE_URL='...'   # ops role, /upwork_crm, read-only option
DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/connectors/upworkcrm --full --all
```

`--full` re-reads every source communication and refreshes `raw_source_items`
where the content hash changed, which also resets `normalized_at` — that is what
picks up a room backfilled into an already-ingested row. `--all` then
re-normalizes every raw row.

Idempotent: a second run changes nothing.

## After — verify

```bash
psql "$OPS_DATABASE_URL" -tAF' | ' -c "
WITH expected AS (
  SELECT count(*) FILTER (WHERE COALESCE(NULLIF(r.raw_json->>'upwork_room_id',''),
                                         NULLIF(r.raw_json->>'send_room_id','')) IS NOT NULL) AS roomed,
         count(*) AS total
    FROM raw_source_items r JOIN source_accounts a ON a.id=r.source_account_id
   WHERE a.provider='upwork_crm' AND r.external_id LIKE 'communications:%'),
actual AS (
  SELECT count(*) FILTER (WHERE t.thread_key LIKE 'upwork\\_crm:%:room:%') AS roomed,
         count(*) AS total
    FROM normalized_messages m JOIN normalized_threads t ON t.id=m.thread_id
   WHERE t.thread_key LIKE 'upwork\\_crm:%')
SELECT e.roomed AS expected_roomed, a.roomed AS actual_roomed,
       e.total  AS expected_total,  a.total  AS actual_total
  FROM expected e, actual a;"
```

**`actual_roomed` must EQUAL `expected_roomed`.** They are computed
independently — one from the raw JSON, one from the keys the normalizer wrote —
so equality means the two-column read worked. A one-column implementation lands
noticeably low here (it sees only `upwork_room_id`, roughly two thirds short on
the outbound side) while producing perfectly well-formed keys and no errors,
which is exactly why this check exists and why it is an equality rather than a
range.

Then the observable outcome of the whole ticket — clients whose several Upwork
chat rooms stop being one thread:

```bash
psql "$OPS_DATABASE_URL" -tAF' | ' -c "
SELECT split_part(thread_key,':',2) AS client, count(*) AS roomed_threads
  FROM normalized_threads WHERE thread_key LIKE 'upwork\\_crm:%:room:%'
 GROUP BY 1 HAVING count(*) > 1 ORDER BY 2 DESC;"
```

As of 2026-08-26 that is two clients — one with 3 rooms, one with 2 — but read it
from the query, not from this line.

### What to expect that looks alarming and is not

- **Most rows do not move.** Roughly 2,009 of 2,441 stay on byte-identical legacy
  keys. That is the design, not a failed migration: they are pre-2026-07-21
  history the source never gave a room for.
- **Empty threads are left behind.** A client whose messages all carry rooms
  leaves an inert `upwork_crm:{client}:upwork` row. Deleting them would need a
  migration for no benefit. Leave them.
- **815 rows can never be roomed.** Their source rows no longer exist, so
  `--full` cannot refresh them. That is what the legacy key is for.
- **`--all` re-runs the delivery matcher for every outbound message** (~1,141).
  With zero `upwork_chat` deliveries in production this is provably a no-op —
  and it is the reason to do the re-key now rather than after the Upwork tier
  goes live.

## Rollback

There is no schema change, so rollback is a code rollback: deploy the previous
image tag and run `--full --all` again. The old normalizer rebuilds the old keys
from the same raw rows, and the roomed threads are left empty and inert. Nothing
is lost either direction, because the keys are derived from raw data that never
changes.

## Deploy

**Image is built and pushed — do not rebuild:**

```
192.168.50.20:5000/switchboard:0.5.0
digest sha256:12a4e2373996ffd22a18b7a1774597d7d79db4b1853e6743843a52731f06cbb7
built from main b695019 (clean tree)
```

The cluster needs a tag bump on `connector-upworkcrm` (kube repo, kube session's
work). **This one is not optional the way SWT-18's was** — until it lands, new
Upwork messages keep getting legacy keys, so the seam below keeps widening.

**Bump the image first, then re-key** — but for a narrower reason than "the old
image undoes it", which is what an earlier draft of this runbook claimed and is
wrong. Checked rather than assumed:

- The in-cluster CronJob runs **incremental** — no `--full`, no `--all` — and
  `pendingRaw` then selects only rows with `normalized_at IS NULL`. Re-keyed rows
  have it set, so an old-image run **does not touch them**. The re-key does not
  revert wholesale.
- What an old-image run *does* do is write **legacy keys for anything it
  normalizes**: newly ingested messages, and any row the CRM edits — because
  `UpdateRaw` resets `normalized_at` to NULL when the content hash changes
  (`sink.go:121`).

So running the re-key before the bump is not destructive, it just leaves a
widening seam of new messages on legacy keys until the bump lands, and each of
those is a message whose room the system knows and is not using. Bumping first
avoids the seam entirely. If the order does get reversed, the fix is simply to
run `--full --all` again after the bump.
