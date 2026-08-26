> Jira: SWT-19

# Open questions — upwork-room-identity

**BOTH ANSWERED 2026-08-26 by the main session (which has psql).** Q1 is **(b)** —
the answer that inverts criterion 10 — and Q2 is **(a)**. Data and reasoning under
each heading below; the original text is preserved beneath the answers.

Both are data questions, not design preferences. **This session had no shell**, so
the `psql` runs below were not executed here; the SPEC's defaults are placed
deliberately and are marked Q1-dependent where the answer moves them. Running the
queries is cheaper than reasoning about them — that is the whole lesson SWT-18's
review left.

---

## Q1. ANSWERED — **(b)**. Outbound rows systematically lack a room id, and the gap is NOT closing.

Run 2026-08-26 against `upwork_crm` on pg-main.

**The load-bearing number, in the API era (since the first roomed row, 2026-07-21):**

```
direction | rows | roomed | pct
inbound   |  213 |    212 | 99.5
outbound  |  188 |     84 | 44.7
```

**Last 7 days**, which is the trend rather than the average:

```
inbound   |    8 |      8
outbound  |    6 |      1
```

**The boundaries OVERLAP, so there is no era to point at:**

```
oldest roomed row     2026-07-21 05:52:47+00
newest room-LESS row  2026-08-25 15:25:52+00   <- yesterday
```

Not per-client either — the two highest-volume clients are internally mixed
(165 rows / 107 roomed, and 146 / 118).

**Therefore (b).** `confirmUpworkDelivery` sees ONLY outbound rows, and more than
half of those carry no room id even now — five of the last six. Criterion 10 as
specced (an unroomed message may not claim a roomed delivery) would leave the
majority of roomed deliveries unconfirmable forever, with no reconciler
originally in scope to surface it. That is the regression-dressed-as-precision
this question was written to catch.

**So the rule must be symmetric**, as this question's own (b) branch prescribes:
an unroomed outbound message MAY claim a roomed delivery of the same client,
subject to the same `textmatch.NormalizedPrefix` comparison and the same
multi-match refusal. Criterion 10 inverts. Criterion 11 (two rooms, roomed
message) is unaffected and remains the case where room identity genuinely pays.

**Write it down as what it is:** for the outbound half — the only half the
matcher sees — this remains **client-wide scoping** most of the time, with room
identity as a tightening that applies when the source happened to supply a room.
Do not describe this ticket as "room matching" in the commit, the code comment,
or IK. That overstatement is exactly what SWT-18's review caught, and repeating
it one ticket later would be worse than the original.

**Open sub-question for the SPEC, not blocking:** *why* outbound rows lack the
room id while inbound rows have it 99.5% of the time is not established. It
smells like the CRM's own send/observe path writing rows without it. If that
path is fixable at the source, the honest fix may be there rather than here —
worth one look before building the asymmetric-rule machinery.

### Original text of Q1 (preserved for the reasoning trail)

### ~~Do room-less rows correlate with age and direction, or is the gap permanent?~~

This decides whether the mixed-key shape is a transitional gap that closes itself
or the permanent shape of the data — and, concretely, whether the matcher's
asymmetric candidate rule (SPEC §4, criteria 9-10) is a migration aid or a
forever rule.

Against the source db (`psql -h 192.168.50.49 -U ops -d upwork_crm` — separate
`~/.pgpass` line):

```sql
-- age: is the room id an API-era artifact?
SELECT date_trunc('month', communicated_at) AS m,
       count(*) AS n,
       count(upwork_room_id) AS roomed
  FROM communications GROUP BY 1 ORDER BY 1;

-- direction: do OUR sends carry a room?  This is the load-bearing half.
SELECT direction, count(*) AS n, count(upwork_room_id) AS roomed
  FROM communications GROUP BY 1;

-- client: is the gap per-client or per-era?
SELECT client_id, count(*) AS n, count(upwork_room_id) AS roomed,
       count(DISTINCT upwork_room_id) AS rooms
  FROM communications GROUP BY 1 ORDER BY roomed DESC;

-- and the sharpest single number: the newest room-less row.
SELECT max(communicated_at) FROM communications WHERE upwork_room_id IS NULL;
SELECT min(communicated_at) FROM communications WHERE upwork_room_id IS NOT NULL;
```

**The decision the answer drives.** Outbound rows are the only ones
`confirmUpworkDelivery` ever sees:

**(a) Recent rows carry a room in BOTH directions** (expected: the two `min`/`max`
above do not overlap, and outbound `roomed` ≈ outbound `n` for the API era) ⇒ keep
the SPEC as written. The asymmetric rule (a roomed message may also claim a
delivery targeted at the client's unroomed key; an unroomed message may not claim
a roomed delivery) is a **transition aid** that stops mattering once every
delivery is drafted against a roomed thread. Criteria 9-10 stand. Write in the
runbook that the unroomed key is legacy-only and should stop accreting.

**(b) Outbound rows systematically lack a room id** (the API sync fills it for
inbound only, or the CRM's own outbox writes rows without it) ⇒ the SPEC's §4 is
wrong in the direction that matters most: every reply we send would come back
unroomed, so criterion 10's refusal would make roomed deliveries unconfirmable
forever, and this ticket would ship a regression dressed as precision. In that
case the honest rule is the symmetric one — an unroomed outbound message is
allowed to claim a roomed delivery of the same client, subject to the same
normalized-prefix comparison and the same multi-match refusal — which is
client-wide scoping for the outbound half and should be written down as such
rather than described as room matching. Criterion 10 inverts; criterion 11 (two
rooms, roomed message) is unaffected.

**Answer shape wanted:** (a) or (b), plus the two boundary timestamps if (a), so
the runbook can state when the legacy key stopped accreting.

---

## Q2. ANSWERED — **(a)**. It is superseded pre-backfill history. Leave it.

Run 2026-08-26 against the ops db:

```
raw upwork communications rows      2441
  ... with the upwork_room_id KEY present (any value)   1626
  ... with upwork_room_id NON-NULL                       296   <- matches the source exactly
  ... missing the key ENTIRELY                           815

date range of the rows missing the key:
  2026-05-09  →  2026-07-11
```

The 815 key-less rows stop dead at 2026-07-11, which is when the column started
being ingested (the CRM's story-id ground-truth backfill is dated 2026-07-14 in
the SWT-18 diagnosis). So they are exactly what this question's (a) branch
describes: history ingested before the column existed, whose source rows have
since been replaced. `--full` cannot refresh them and they normalize unroomed
onto the legacy key, which is what that key is for.

The roomed count agrees end to end — 296 in the source, 296 in ops raw — so the
re-key has no hidden population.

**Consequence for the verification protocol:** expect **2,441 messages unchanged
in total** after the re-key, with only the 296-row roomed subset moving to new
thread keys. Say so in the runbook so a reader does not read "most rows did not
move" as a failed migration.

### Original text of Q2 (preserved for the reasoning trail)

### ~~Does the ops corpus's ~800-row excess over the source matter for the re-key?~~

The ops db has 2,441 upwork messages; the source has 1,650 communications rows.
Roughly 800 ops-side raw items therefore have no live source row — most plausibly
the pre-2026-07-14 history the CRM replaced with story-id ground truth. Those raw
rows are frozen: `--full` will not refresh them (the source row is gone), so
whatever room id they lack they lack forever and they normalize unroomed.

That is design-neutral — the SPEC's fallback handles them — but it changes the
numbers step 6 of the verification protocol should expect, and it may indicate
raw rows that should not be in the corpus at all.

```sql
-- ops db
SELECT count(*) FROM raw_source_items r JOIN source_accounts a ON a.id=r.source_account_id
 WHERE a.provider='upwork_crm' AND r.external_id LIKE 'communications:%';
SELECT count(*) FILTER (WHERE r.raw_json ? 'upwork_room_id') AS has_key,
       count(*) FILTER (WHERE r.raw_json->>'upwork_room_id' IS NOT NULL) AS roomed,
       count(*)
  FROM raw_source_items r JOIN source_accounts a ON a.id=r.source_account_id
 WHERE a.provider='upwork_crm' AND r.external_id LIKE 'communications:%';
SELECT min(r.raw_json->>'communicated_at'), max(r.raw_json->>'communicated_at')
  FROM raw_source_items r JOIN source_accounts a ON a.id=r.source_account_id
 WHERE a.provider='upwork_crm' AND r.external_id LIKE 'communications:%'
   AND NOT (r.raw_json ? 'upwork_room_id');
```

`raw_json ? 'upwork_room_id'` (key present at all, possibly null) vs
`->> IS NOT NULL` (key present and non-null) is the distinction that separates
"ingested before the column existed" from "the source really has NULL there".

**(a) The excess is superseded pre-backfill history** ⇒ leave it. It normalizes
unroomed onto the legacy thread and is exactly what that thread is for; expect
2,441 unchanged after the re-key and say so in the runbook.

**(b) The excess is something else** (duplicate raw items, a second ingest path,
rows the source still has that the count above missed) ⇒ establish what before
running `--full --all` on production, because a re-key is the wrong moment to
discover an unexplained 800 rows.

---

Answer by editing the entries. Say "questions answered" and I'll fold them into
the SPEC.
