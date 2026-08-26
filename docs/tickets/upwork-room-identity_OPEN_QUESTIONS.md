> Jira: SWT-19

# Open questions — upwork-room-identity

**BOTH ANSWERED 2026-08-26 by the main session (which has psql), and Q1 was then
RE-ANSWERED the same day.** Q1 is **(a)** — the first pass said (b) because it
counted `upwork_room_id` alone and there is a second room column,
`send_room_id`, carrying exactly the rows that looked missing. Q2 is **(a)**. Data and reasoning under
each heading below; the original text is preserved beneath the answers.

Both are data questions, not design preferences. **This session had no shell**, so
the `psql` runs below were not executed here; the SPEC's defaults are placed
deliberately and are marked Q1-dependent where the answer moves them. Running the
queries is cheaper than reasoning about them — that is the whole lesson SWT-18's
review left.

---

## Q1. RE-ANSWERED 2026-08-26 — **(a) after all. The first answer was measured on the wrong column.**

**Outbound rows do not lack the room id. They carry it in `send_room_id`.**

`communications` has TWO room columns, and the original Q1 query counted only one:

| column | meaning | inbound | outbound |
|---|---|---|---|
| `upwork_room_id` | the room a message was OBSERVED in | 212 | 84 |
| `send_room_id` | the room a send was DISPATCHED to | 0 | 136 |

They are **disjoint per row** (no row has both) and they are the **same identifier
space** — identical `room_<hex>` format, and 6 values appear in both columns
(11 distinct upwork values, 9 distinct send values).

`send_room_id` is written by exactly one path, and the correlation is perfect:

```
send_room_id IS NOT NULL        136
send_requested_at IS NOT NULL   136
both                            136
disagree                          0     <- zero
```

So the shape is simply: a message we DISPATCHED through the CRM records the room
it was sent to in `send_room_id`; a message OBSERVED coming back from Upwork
records the room it was seen in as `upwork_room_id`. Nothing is missing.

**Reading both columns, the number that drove the inversion disappears:**

```
API era (since 2026-07-21), COALESCE(upwork_room_id, send_room_id):
  inbound    213 rows, 212 roomed   99.5%
  outbound   188 rows, 186 roomed   98.9%     <- was reported as 44.7%
```

Only **2** API-era outbound rows have neither column set.

**What this changes in the SPEC.**

1. **The room source is `COALESCE(upwork_room_id, send_room_id)`, not
   `upwork_room_id`.** `rawCommunication` must parse both. This is the single
   most important correction — a normalizer reading one column would key 102 of
   188 recent outbound messages onto the legacy thread while believing it had
   read the room.
2. **Q1 reverts to branch (a).** Roomed-ness is an API-era property in BOTH
   directions, so §4's tolerance of unknown rooms is a **transition aid for
   pre-2026-07-21 history**, not a permanent accommodation of a broken outbound
   path. Criterion 10 still stands as written — an unroomed message must still be
   able to claim a roomed delivery, because 576 legacy rows and 2 recent ones
   have no room at all — but its justification changes from "outbound is
   systematically unroomed" to "legacy history is unroomed", and criterion 10's
   required test comment must cite 98.9%, not 44.7%.
3. **The Future-work sub-question is CLOSED.** There is nothing to fix in the
   CRM's send path; it was recording the room the whole time. Do not open a
   ticket against the CRM for this.
4. **Naming (criterion 20) survives and is reinforced**, for a better reason than
   before: with both columns read, the outbound half genuinely IS mostly
   room-scoped in the API era. The honest description is now "room-scoped for
   API-era traffic in both directions, client-wide for pre-2026-07-21 history".

**Worth one line in the SPEC as a hazard:** the two columns mean different things
(dispatched-to vs observed-in), and 6 outbound bodies already appear more than
once for the same client. If the CRM ever stores a dispatched message AND its
observation as two rows, they would key to the same room correctly — but they are
two rows in one thread with the same body, which is precisely the ambiguity the
matcher's multi-match refusal exists to handle. That is an argument FOR keeping
the refusal after rooms are real, not against it.

### Original (b) answer, preserved — it was measured on `upwork_room_id` alone

### ~~ANSWERED — (b). Outbound rows systematically lack a room id, and the gap is NOT closing.~~

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
