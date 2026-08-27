> Jira: SWT-19

# upwork-room-identity — key Upwork threads on the room, not the client

**Both open questions answered 2026-08-26 and folded in; Q1 was then RE-answered
the same day and this SPEC revised again** (`docs/tickets/
upwork-room-identity_OPEN_QUESTIONS.md`, committed as `9532eb6`, holds the data
verbatim). Q1's final answer is **(a)**; the intermediate **(b)** was measured on
one of two room columns. Q2 is **(a)**. Not provisional, and every count in it is
measured.

**The correction that matters most, and the reason it is at the top:**
`communications` has **two** room columns — `upwork_room_id` (the room a message
was OBSERVED in) and `send_room_id` (the room a send was DISPATCHED to). They are
disjoint per row and share an identifier space. **The room source is
`COALESCE(upwork_room_id, send_room_id)`.** A normalizer reading `upwork_room_id`
alone would key 136 outbound messages onto the legacy thread *while believing it
had read the room* — this ticket's own bug, reintroduced inside its own fix, and
undetectable from the writes. Criterion 3 exists to stop exactly that, and
criterion 23's exact expected count is what makes a one-column implementation
fail rather than pass.

## Source

Not a build-order step. It is the follow-up the SWT-18 post-merge review named
explicitly (`docs/bugs/upwork-matcher-hardening_DIAGNOSIS.md`, "Post-merge review
correction (2026-08-26)"):

> **Follow-up (NOT this ticket):** key `thread_key` on `upwork_room_id`. It
> re-keys every existing upwork thread and touches dedup and `external_refs`, so
> it is its own ticket, and it should land before the Upwork tier goes live.

and, upstream of that, Salvador's original report — "verifies it's posting to the
correct thread by matching a previous message... when somebody has several chat
rooms".

## Goal

Make `normalized_threads.thread_key` carry the Upwork room where the source
supplies one — from either room column — give the key one canonical spelling that
every writer and reader shares, re-key the existing corpus by re-normalizing from
raw, and make the delivery matcher tighten on room identity when both sides know
the room without losing the confirmations it makes today.

**Usable alone** means: with only the already-deployed `connector-upworkcrm`
CronJob (no draft worker, no triage, no orchestrator, no dashboard change), one
run re-keys the corpus and the client that has two Upwork rooms
(`e2ef9b65-9813-4d79-ac10-0e1813f788ff`) becomes two threads instead of one —
observable in `psql` and on the existing `/tasks` surfaces — while every message
the source gave no room for stays exactly where it is today. Nothing else in the
system has to move for that to be true or useful.

## Established facts — do not re-derive

From the SWT-18 review, the Q1/Q2 runs against pg-main on 2026-08-26 (including
Q1's re-run and the follow-up counts), and reading `main` at `526233b`.

1. `thread_key` is `Provider + ":" + client_id + ":" + channel`
   (`internal/connector/upworkcrm/normalize.go:99`) and `channel` is the constant
   `'upwork'`: 1,650 source rows, 26 clients, one distinct value. Every
   `normalized_threads` key in the ops db ends `:upwork`. **One thread == one
   client. Room identity is not operating anywhere today.**
2. SWT-18's `target_ref = thread_key` equality in `confirmUpworkDelivery`
   therefore selects exactly the candidate set the old client-wide `LIKE` did.
   The thing preventing a wrong-row bind today is the **multi-match refusal**,
   not the predicate. SWT-18 is correct and merged; it is not reopened here.
3. **(Q1 re-run) There are TWO room columns**, disjoint per row (no row carries
   both) and in the same identifier space (`room_<hex>`; 6 values appear in
   both):

   | column | meaning | inbound | outbound | distinct |
   |---|---|---|---|---|
   | `upwork_room_id` | the room a message was OBSERVED in | 212 | 84 | 11 |
   | `send_room_id` | the room a send was DISPATCHED to | 0 | 136 | 9 |

   Whole-table, under `COALESCE`, over **14 distinct rooms** (11 + 9 − 6 shared):

   ```
   inbound    854 rows, 212 roomed, 642 unroomed
   outbound   796 rows, 220 roomed, 576 unroomed
   TOTAL     1650 rows, 432 roomed, 1218 unroomed
   ```
4. **`send_room_id` is written by the send path and nothing else.**
   `send_room_id IS NOT NULL` = 136, `send_requested_at IS NOT NULL` = 136, both
   = 136, disagreements = **0**. There is nothing broken in the CRM's send path;
   it has been recording the room the whole time.
5. **(Q1 = (a)) Roomed-ness is an API-era property in BOTH directions.** Since
   the first roomed row (2026-07-21), reading both columns: inbound 213 rows /
   212 roomed (**99.5%**), outbound 188 / 186 (**98.9%**). Only **2** API-era
   outbound rows carry no room at all. The 44.7% figure that appeared in an
   earlier draft of this SPEC was `upwork_room_id` alone and is wrong.
6. `confirmUpworkDelivery` runs for `direction == "outbound"` only
   (`sink.go:277`), so the outbound column is the one the matcher lives or dies
   on — which is precisely why reading only `upwork_room_id` would have been
   fatal and silent: it would see 84 of the 220 roomed outbound rows.
7. One client (`e2ef9b65-9813-4d79-ac10-0e1813f788ff`) already has 2 rooms by
   `upwork_room_id`. Whether more clients are multi-room once `send_room_id` is
   counted has NOT been measured; it does not change the design, only the number
   of threads the re-key creates (bounded by 14 rooms, fact 3).
8. The ops db holds 26 upwork threads carrying 2,441 messages.
9. **(Q2, plus the follow-up count)** Of those 2,441 raw communications rows:
   1,626 have the `upwork_room_id` key present, 296 non-null, and **815 lack that
   key entirely** with a date range ending 2026-07-11 — history ingested before
   the column existed, whose source rows the CRM has since replaced (`--full`
   cannot refresh them). Measured under `COALESCE(upwork_room_id, send_room_id)`,
   **432 of the 2,441 raw rows are roomed — matching the source's 432 exactly** —
   leaving **2,009 on legacy keys**. That is the whole of the re-key: 432 move,
   2,009 stay, 2,441 total unchanged.
10. Production has **ZERO `upwork_chat` deliveries**, and has never had one.
11. Ingestion moved from browser-extension scraping to API access; source
    `external_id` is now `story_<hex>` after the 2026-07-14 backfill.

### Facts established by reading the code

12. **Both room columns are already in the ops db.** `PGSource.ListCommunications`
    selects `to_jsonb(m)` (`internal/connector/upworkcrm/source.go:67`), so
    `raw_source_items.raw_json` carries every column of the source row for any row
    ingested after that column existed. Re-keying is therefore a re-normalize from
    raw (invariant 1's promise), not a data migration and not a re-scrape. Fact
    9's 432 == 432 agreement is the proof: nothing is missing on the ops side.
13. **`rawCommunication` parses NEITHER room column.** `normalize.go:44-54` has no
    `UpworkRoomID` and no `SendRoomID`; `NormalizeCommunication` never sees them.
    `rawClient` *does* have `UpworkRoomID` (`normalize.go:41`) and already emits an
    `upwork_room` person identity from `clients.upwork_room_id` — a client-level
    room notion that is NOT the same thing as either per-message column.
14. **`Ingest` skips `is_draft` rows** (`ingest.go:115-117`), and that is not a
    concern here: only **8 rows in the whole table are `is_draft`, and ZERO of
    them carry `send_room_id`**. The CRM's outbox is `send_requested_at` +
    `send_room_id`, a different population from `is_draft`. No roomed row is
    skipped at ingest — confirmed by fact 9's exact 432/432 agreement.
15. **`thread_key` has exactly two readers and two writers in Go**, all named
    below: written by `upsertMessage` via `NormalizeCommunication`; read by
    `confirmUpworkDelivery` (`sink.go:414`) and by
    `internal/drafts/store.go:126-137`. Nothing else parses it. `internal/capture`
    has no upwork channel at all.
16. **`drafts` currently prefers the newest thread by accident.**
    `store.go:126-129` is `WHERE thread_key LIKE 'upwork_crm:'||$1||':%' ORDER BY
    id DESC LIMIT 1`. After re-keying, roomed threads have higher ids than the
    legacy client thread, so it happens to prefer a roomed thread. Accidental
    correctness is not correctness; criterion 15 makes it deliberate.
17. **The matcher is one-shot per message.** `confirmUpworkDelivery` is reachable
    only from `upsertMessage`, which runs only for raw items returned by
    `pendingRaw` (`sink.go:140-146`). A message normalized *before* its delivery
    reaches `status='sent'` is never re-examined, so on the assisted tier — where
    a human clicks "mark sent" minutes or hours after pasting — a real fraction of
    confirmations can never fire. Pre-existing, NOT fixed here (see Out of scope),
    and the strongest argument for the reconciler in §6.
18. **`task_events.event_type` has no CHECK** (`0001_initial.sql:145`), so a new
    `delivery_unconfirmed` event for `upwork_chat` needs no migration. slackweb
    already writes that type.
19. **The highest migration is `0014_imap_mail.sql`.** `docs/tickets/
    capture-rules_SPEC.md` (SWT-17, unimplemented) claims 0015. **This ticket
    needs no migration at all** (§5), so the collision is moot for SWT-19.

## Design

### 1. The key shape

```
roomed    upwork_crm:{client_id}:room:{room_id}     (new)
unroomed  upwork_crm:{client_id}:{channel}          (unchanged — today's key)
```

A message with a room — from **either** column — gets the roomed form. A message
without one keeps **byte-identical** today's key, so the 2,009 legacy rows
(fact 9) need no re-key, no migration and no cleanup, and the 26 existing thread
rows keep their identity, their `participants` and their FK edges.

The `room:` tag exists so that "is this key roomed?" is a **parse, not a guess
about the third segment's contents**. The alternative — `upwork_crm:{client}:
{room_id}` with the legacy `:upwork` as fallback — makes the two forms
distinguishable only by comparing the third segment against a magic literal,
which is the SWT-13 canonicalisation landmine in yet another costume. Room ids may
contain anything; the tag cannot be confused with a channel value because the
unroomed form has three segments and the roomed form has four.

### 2. Reading the room: two columns, one function

```go
// internal/connector/upworkcrm/normalize.go
type rawCommunication struct {
    // ...
    UpworkRoomID *string `json:"upwork_room_id"` // observed in
    SendRoomID   *string `json:"send_room_id"`   // dispatched to
}
```

Room = `COALESCE(upwork_room_id, send_room_id)`, spelled once in Go, with
**observed winning** if both are ever set. They are disjoint today (fact 3), so
the precedence is a tiebreak for a case that does not exist — but it must be
written and tested rather than left to field order, and observed is the right
winner because it is ground truth about where the message actually is on Upwork,
which is what the matcher is trying to identify. Neither set ⇒ unroomed.

**This is criterion 3 and it is the highest-value test in the ticket.** A
normalizer that reads one column produces keys that look perfectly well-formed,
pass every unit test that fabricates a room, and quietly file 136 outbound
messages under the legacy key — 62% of the roomed outbound corpus (fact 3). That
is indistinguishable from correct behaviour at the write site: the same shape as
SWT-18's `RoomDiscrimination` test proving room scoping with channel values
production never emits.

New in `internal/connector/upworkcrm/threadkey.go`:

```go
func ThreadKey(clientID, roomID, channel string) string
func ParseThreadKey(key string) (ThreadRef, error)   // ThreadRef{ClientID, RoomID, Channel, Roomed}
```

`ParseThreadKey` splits with `strings.SplitN(key, ":", 4)`: prefix must be
`upwork_crm`, `client_id` non-empty, and either (4 segments, third == `room`,
remainder non-empty → roomed, room id keeps any embedded colons) or (3 segments,
third non-empty → unroomed with that channel). Anything else is an error.

Every producer and consumer goes through these — `normalize.go`, `sink.go`'s
matcher, `drafts/store.go`, and `draft_delivery`'s validator. **Do not re-spell
the format in SQL**, for the same reason `textmatch` may not be: two spellings of
one canonicalisation is the failure mode this repo has already paid for four
times. §4 leans on this hard — the matcher's scoping happens entirely in Go
precisely so no SQL has to know the format.

### 3. The fallback is a real thread, not a bucket rename

The unroomed key is **the same key it is today**, and it holds one population:
**pre-2026-07-21 history** — 1,218 source rows, 2,009 in the ops corpus once the
frozen pre-backfill 815 are counted — plus the 2 API-era outbound stragglers.
Q1's final answer means this is a **legacy boundary, not a permanent direction
gap**: new traffic is ~99% roomed in both directions, so the legacy key should
stop accreting almost entirely. Say that in the runbook — a room-less API-era row
is now an anomaly worth looking at, not the norm.

The hard rule that keeps mixed keying from becoming ambiguity: *a thread is roomed
if and only if the source row named a room, in one of the two columns.* No
inference, no inheritance. Specifically **rejected**: filling a message's room
from `clients.upwork_room_id` (fact 13) or from the client's other messages. For
25 of 26 clients that inference would usually be right and would be
unfalsifiable; for the multi-room client it would silently merge two
conversations, and that client is the entire reason this ticket exists.

**Consequence, corrected under the two-column numbers — thread splitting is a
history/live split, not an inbound/outbound split.** An earlier draft of this SPEC
warned that a client's conversation would fragment with their messages roomed and
ours legacy; that was an artifact of the one-column measurement and **does not
happen** (outbound is 220 roomed, not 84). What remains is that a client whose
conversation predates 2026-07-21 has their old history on the legacy thread and
their current conversation on a roomed thread. `drafts.loadThreadContext` reads
the last 6 messages of one thread (`store.go:150-179`), and for an active
conversation those 6 are all API-era and all in the roomed thread, both
directions. So the context a draft sees is intact; what it loses is deep history,
which that window never reached anyway. Accepted, not a Future-work item.

### 4. What mixed keying does to the matcher — read this before naming the commit

**The rule: a room MISMATCH is the only thing that excludes. An unknown room
excludes nothing.** For an outbound message M and an unconfirmed `upwork_chat`
delivery D, both keys parsed with `ParseThreadKey`:

| | D roomed | D unroomed |
|---|---|---|
| **M roomed** | candidate **iff same room** | candidate |
| **M unroomed** | candidate | candidate |

plus, always, `D.ClientID == M.ClientID`. A delivery whose `target_ref` does not
parse is never a candidate (and the reconciler in §6 flags it, which is how a
legacy or hand-written target surfaces instead of rotting).

The bottom-left cell — an unroomed message claiming a roomed delivery — is the one
that survived Q1's re-answer, and it is worth being precise about why it is still
needed even though outbound is 98.9% roomed in the API era: **576 outbound rows
are unroomed legacy history**, 2 API-era outbound rows carry no room, and `--all`
re-normalization replays the whole history through this matcher. Refusing that
cell would make those permanently unconfirmable, silently. It is a **legacy
tolerance**, not an accommodation of a broken send path — nothing is broken
(fact 4).

**Say what this is.** The honest description: **room-scoped for API-era traffic in
both directions, client-wide for pre-2026-07-21 history.** It is strictly tighter
than today. The code comment, the commit message, the runbook and the IK entry
must all describe it that way and cite facts 3-5. **Do not call this ticket "room
matching" flat** — SWT-18 called its change that and was wrong on production data,
and the scope here is still conditional on the source having supplied a room.
Criterion 21 makes this a review item.

**Implementation shape: scope in Go, not in SQL.** The candidate query keeps only
channel/status/NULL guards —

```sql
SELECT id, task_id, COALESCE(target_ref,''), COALESCE(body,'') FROM deliveries
 WHERE channel='upwork_chat' AND status='sent'
   AND sent_external_id IS NULL AND confirmed_at IS NULL
 ORDER BY id DESC FOR UPDATE
```

— and client/room scoping plus the `textmatch.NormalizedPrefix` comparison both
happen in Go. Forced, not stylistic: "any roomed key of this client" cannot be
expressed as an equality, and expressing it as a `LIKE` or a `split_part` would
put a second spelling of the key format in SQL. The set is bounded by *unconfirmed
assisted upwork deliveries*, which is 0 today; if it ever grows large that is
itself the alarm the reconciler raises. Two concurrent connector runs lock in the
same `id DESC` order, so they block rather than deadlock.

Everything else about the matcher is unchanged: normalized 120-char prefix,
`status='sent'`, the `IS NULL` guards restated on the UPDATE, `FOR UPDATE`, the
`RowsAffected` check before the `delivery_confirmed` event, the already-claimed
pre-check, and **no time bound**.

**The multi-match refusal STAYS, and the two-column finding is a fresh argument
FOR it.** The columns mean different things — dispatched-to versus observed-in —
and 6 outbound bodies already appear more than once for the same client. If the
CRM ever stores a dispatched message AND its later observation as two rows, both
key to the same room *correctly*, and the result is two same-body rows in one
thread: exactly the ambiguity the refusal exists to handle, arriving through a
door that room identity does not close. The trade is unchanged and still
asymmetric — two unconfirmed rows can be confirmed later or by a human; one wrong
stamp burns the external id under `deliveries_sent_external_idx` and locks the
correct row out permanently (invariant 4). What changes is that the refusal stops
being silent — §6.

### 5. Re-keying the existing corpus: no migration, one re-normalize

`normalized_messages.thread_id` follows the thread that `upsertMessage` resolves,
because the message upsert already does `ON CONFLICT (raw_source_item_id) DO
UPDATE SET thread_id=EXCLUDED.thread_id` (`sink.go:260-261`). So re-keying is:

```
UPWORK_CRM_DATABASE_URL=... DATABASE_URL="$OPS_DATABASE_URL" \
  go run ./cmd/connectors/upworkcrm --full --all
```

`--full` re-reads every source communication and updates `raw_source_items` where
the content hash changed (which also resets `normalized_at`), so a room id
backfilled into an already-ingested source row is picked up. `--all` then
re-normalizes every raw row, creating the roomed threads and re-pointing the
roomed messages into them. Idempotent: a second run changes nothing.

The expected outcome is **exact, not a range** (fact 9): of 2,441 ops messages,
**432 move to roomed keys and 2,009 stay on unchanged legacy keys**. A run that
produces fewer than 432 roomed is a one-column implementation, not a smaller
corpus — that is criterion 23's whole purpose.

Other consequences to expect, not to be surprised by:

- **Most rows still do not move**, and that is the designed outcome, not a failed
  migration. Write it in the runbook.
- **Old threads are not deleted.** A client whose messages all carry rooms leaves
  behind an empty `upwork_crm:{client}:upwork` thread. Empty threads are inert and
  deleting them would need a migration for no benefit. Leave them.
- **The 815 key-less rows are frozen** (fact 9): their source rows no longer
  exist, `--full` cannot refresh them, and they normalize unroomed forever. That
  is what the legacy key is for.
- **`--all` re-runs `confirmUpworkDelivery` for every outbound message** (~1,141
  of them). With zero `upwork_chat` deliveries in production this is provably a
  no-op today (fact 10) — and it is the reason to do the re-key NOW rather than
  after the Upwork tier goes live.

**Migration numbering.** SWT-19 adds no migration, so SWT-17 keeps 0015. If
implementation discovers a migration is unavoidable, take **0016** and say so in
the commit; do not renumber SWT-17's SPEC from this ticket. The number belongs to
whatever merges first, and the loser renumbers — an already-applied file is never
edited.

### 6. `deliveries.target_ref`: validate it now, while it is free

Two things are true at once and both point the same way:

- Production has zero `upwork_chat` deliveries, so canonicalising costs nothing
  today.
- A `target_ref` that `ParseThreadKey` cannot read is now **permanently
  unconfirmable** — §4 excludes it as a candidate — where the pre-SWT-18 `LIKE`
  was forgiving. And `draft_delivery` validates only that an upwork `target_ref`
  is non-empty (`internal/tools/delivery.go:107-108`), unlike `slack_reply` which
  parses (`delivery.go:110-114`). That is the SWT-13 landmine's fourth instance,
  named in IK and still open.

So: `validateDraftDelivery` gains, for `upwork_chat`, an
`upworkcrm.ParseThreadKey` call exactly parallel to the `slackweb.ParseTargetURL`
call two lines below it, and `draftDelivery` stores the parsed value's canonical
spelling rather than the caller's (the SWT-13 fix's shape). This runs inside the
executor's validate step — no new tool, no new handler, no policy change.

Both roomed and unroomed target refs are accepted: the legacy corpus is real, and
§4's bottom-left cell is what makes an unroomed target still confirmable.

**And upworkcrm gets a reconciler**, `internal/connector/upworkcrm/reconcile.go`,
a near-copy of `slackweb/reconcile.go` scoped to `channel='upwork_chat'` and
`status='sent'`. It flags — never retries, never invents a `sent_external_id` —
rows no run has confirmed after N completed successful `sync_runs` for the upwork
account, appending a note to `deliveries.error` (fire-once via the
`unconfirmedNote` marker) and writing one `delivery_unconfirmed` task event.

Why it belongs in THIS ticket: every failure mode around this matcher has the same
signature — a row at `status='sent'`, `sent_external_id` NULL, forever, with
nothing anywhere saying so. That covers a `target_ref` that no longer parses, a
refusal on ambiguity, and the pre-existing one-shot gap in fact 17. Shipping a
change to the matcher's scoping without the detector is shipping a silent failure
mode on purpose.

**Landmine specific to upwork's pass counting:** one `upworkcrm` invocation writes
**TWO** `sync_runs` rows — `Ingest` calls `StartRun` (`ingest.go:70`) and
`Normalize` calls it again (`normalize.go:148`) — and both finish `ok`. A
threshold copied from slackweb's 3 would fire after 1.5 CronJob invocations (~22
minutes). Count in the same unit slackweb does (completed `ok` runs) but set the
upwork default to **6** (= 3 invocations ≈ 45 minutes at `*/15`), and say in the
comment that 6 means 3 because of the double-run. Do not try to distinguish ingest
runs from normalize runs by their `stats` jsonb: both marshal the same `Stats`
struct, so the discriminating keys are present-and-zero in both — a predicate
whose discriminating column is a constant, which is the exact mistake SWT-18's
review caught.

## Pre-verified against production, 2026-08-26 (read-only dry run)

Before any test existed, the REAL normalizer (`NormalizeCommunication`, not a
reimplementation) was applied to every production raw communication row and the
keys it produced were parsed back with `ParseThreadKey`. Nothing was written.

```
total=2442  roomed=433  legacy=2009  normalize_failed=0  unparseable=0
distinct_threads=38  distinct_rooms=14  clients_with_rooms=11  multi_room_clients=2
```

Three things this establishes that no unit test can:

1. **Go and SQL agree exactly.** 433 roomed from the normalizer against 433 from
   the independent `COALESCE(NULLIF(...), NULLIF(...))` query over the same rows.
   That is criterion 23's property, demonstrated on real data before
   implementation was reviewed.
2. **Every key the normalizer produces parses.** `unparseable=0` — the producer
   and the consumer of the format agree over 2,442 real rows, including whatever
   punctuation real room ids and client uuids contain.
3. **No row fails to normalize.** `normalize_failed=0`, so the re-key cannot
   strand a message.

**Correcting fact 7, which this SPEC recorded as unmeasured.** It said one client
has two rooms and that whether more are multi-room once `send_room_id` is counted
"has NOT been measured". Measured now — **there are TWO multi-room clients**:

```
43431d4c-d34a-43f2-b49b-2dc70c52c096   3 rooms
e2ef9b65-9813-4d79-ac10-0e1813f788ff   2 rooms
```

So the "usable alone" check should name both, and the 3-room client is the better
demonstration. Note this does not change the design — it enlarges the evidence
that room identity is worth having, since the client with three rooms is
currently one undifferentiated thread.

## The corpus is LIVE — do not hardcode its counts (corrected 2026-08-26, during implementation)

Criterion 23 originally asserted `432 roomed / 2,009 legacy / 2,441 total` as
equalities, on my instruction, arguing that a range would let a one-column
implementation pass. **The argument was right and the form was wrong.**
`connector-upworkcrm` ingests every 15 minutes, and the corpus moved while the
ticket was being implemented:

```
2026-08-26 ~19:30 UTC   2,441 raw / 432 roomed
2026-08-26 ~21:30 UTC   2,442 raw / 433 roomed
```

A frozen literal therefore fails on any day a message arrives, which is every
day — an alarm that cries wolf, which this repo has an IK entry about.

**The rule instead: assert the normalizer's output against the same corpus
measured at verification time by an INDEPENDENT computation.** The count of
messages on `:room:` keys must EQUAL

```sql
SELECT count(*) FROM raw_source_items r
  JOIN source_accounts a ON a.id = r.source_account_id
 WHERE a.provider='upwork_crm' AND r.external_id LIKE 'communications:%'
   AND COALESCE(NULLIF(r.raw_json->>'upwork_room_id',''),
                NULLIF(r.raw_json->>'send_room_id','')) IS NOT NULL
```

and the legacy count must equal the total minus that. Still an equality, still
exact, and a one-column implementation still fails it — it would produce the
`upwork_room_id`-only count against a coalesce-derived expectation. Strictly
better than both a literal and a range, because the two sides are computed
independently rather than one being remembered.

`NULLIF` on both columns because the Go reader treats an empty string as absent,
identically to a missing key. Zero rows carry an empty-string room today, so both
spellings agree — the `NULLIF` keeps the SQL measuring the Go semantics rather
than drifting a row if that ever changes.

**Fixture-owned corpora keep exact literals.** This applies only to assertions
against production. Every count quoted elsewhere in this SPEC is "as of
2026-08-26" and is illustration, not an invariant.

## Acceptance criteria

1. `upworkcrm.ThreadKey` / `ParseThreadKey` exist in
   `internal/connector/upworkcrm/threadkey.go` and are pure (no `context`, no
   `pgxpool`); their unit tests run under `go test ./...` with no database.
   Round-trip property: for every roomed and unroomed input,
   `ParseThreadKey(ThreadKey(...))` returns the inputs.
2. `ParseThreadKey` rejects, with an error naming the offending part: a wrong
   provider prefix, an empty client id, a 3-segment key with an empty third
   segment, a 4-segment key whose third segment is not `room`, a 4-segment key
   with an empty room id, and fewer than 3 segments. A room id containing a colon
   survives the round trip intact.
3. **Both room columns are read.** `rawCommunication` parses `upwork_room_id` AND
   `send_room_id`, and the room is `COALESCE(upwork_room_id, send_room_id)`.
   Four cases, four assertions: only `upwork_room_id` → roomed on that value; only
   `send_room_id` → **roomed on that value** (the case a one-column implementation
   gets wrong, and it is 136 source rows / 62% of roomed outbound); both set →
   observed (`upwork_room_id`) wins and no error; neither → the byte-identical
   legacy key. Empty string is treated as absent for both columns, as is a missing
   JSON key.
4. Normalization stays deterministic and raw-only: the same raw row normalizes to
   the same output on repeated calls, and no code path in `Normalize` reads the
   source database.
5. An integration test over a fixture corpus containing one client with two rooms,
   rows roomed via each column, and room-less rows produces: one thread per
   (client, room) pair regardless of which column supplied the room, ONE thread for
   the room-less rows under the legacy key, and every message pointing at the right
   thread. Re-running with `--all` changes no counts.
6. Re-normalizing a corpus first normalized under the old key moves the roomed
   messages to new threads and leaves the room-less ones on the original thread row
   (same `normalized_threads.id`), with no orphaned `normalized_messages` and no
   thread deleted.
7. `validateDraftDelivery` refuses an `upwork_chat` `target_ref` that
   `ParseThreadKey` rejects, with the error naming the target; it accepts both the
   roomed and unroomed forms; and `draft_delivery` persists the canonical spelling
   rather than the caller's (a target with surrounding whitespace is stored trimmed
   and canonical).
8. The four existing fixtures that write a non-canonical upwork target
   (`internal/tools/delivery_lifecycle_integration_test.go:395`,
   `internal/tools/delivery_slack_integration_test.go:616` and `:757`,
   `internal/connector/slackweb/confirm_integration_test.go:452` — all spelled
   `upwork_crm:itest-*:chat`) are updated to canonical keys, and `make integration`
   is green. A test asserting the *old* spelling is now refused must exist, so the
   change is pinned rather than merely accommodated.
9. **A ROOMED outbound message confirms** (a) a delivery whose `target_ref` is that
   room's key, and (b) a delivery whose `target_ref` is the same client's unroomed
   key. Separate cases, separate assertions. Case (a) must be exercised with the
   room arriving via `send_room_id`, since that is the column 136 of the 220 roomed
   outbound rows actually carry.
10. **An UNROOMED outbound message DOES confirm** a delivery whose `target_ref` is
    a roomed key of the same client, when it is the only prefix match. The test
    comment must say why this tolerance exists and must cite the real numbers:
    API-era outbound is **98.9% roomed** (186 of 188) — the outbound path is
    healthy and records the room in `send_room_id` — so this cell covers the
    **576 unroomed outbound rows of pre-2026-07-21 history**, the 2 API-era
    stragglers, and `--all` replays of that history. It must NOT read as though
    the outbound path is broken; it was the earlier one-column measurement that
    was.
11. Two deliveries in DIFFERENT rooms of the same client with byte-identical
    bodies, and a **roomed** message: only the one whose `target_ref` names the
    message's room is stamped, in both `sent_at` orderings. This is SWT-18's
    `RoomDiscrimination` test **rebuilt on real room ids** rather than on channel
    values the source has never emitted (`chat`, `room-b`) — the old fixture must
    be deleted or re-based, not left as a passing proof of nothing. This is the one
    exclusion the rule makes and where room identity pays.
12. A delivery belonging to a DIFFERENT client is never a candidate, even with an
    identical body; and a delivery whose `target_ref` does not parse is never a
    candidate, in either message shape.
13. Two deliveries reachable from one message (same room, or one roomed and one
    unroomed for the same client) sharing a normalized 120-char prefix: neither is
    stamped, and the reconciler flags both once the pass threshold is reached.
14. **Same-body duplicates in one room refuse.** Two outbound messages with the
    same normalized prefix in the same room — the shape the CRM would produce if it
    ever stored a dispatched row and its later observation separately (fact 3's two
    columns; 6 outbound bodies already repeat per client) — do not produce two
    stamps: the first confirms, the second is refused by the already-claimed
    pre-check without failing the normalize run.
15. `internal/drafts/store.go` resolves the upwork target through
    `ParseThreadKey`, preferring a **roomed** thread for the client and falling
    back to the unroomed one, deliberately rather than by `ORDER BY id DESC`
    (fact 16). A test with both thread shapes present asserts the roomed key is
    chosen regardless of insertion order.
16. `upworkcrm.ReconcileUnconfirmed` flags an `upwork_chat` delivery that no run
    has confirmed after N completed `ok` sync runs started after its `sent_at`:
    appends the marker note to `error`, writes exactly one `delivery_unconfirmed`
    task event, changes no `status`, and writes no `sent_external_id`. Running it
    again flags nothing (fire-once on the marker).
17. The reconciler's pass default is 6 with a comment stating that upwork writes
    two `sync_runs` rows per invocation, and the env override
    (`UPWORK_UNCONFIRMED_FLAG_PASSES`) falls back to the default on unparseable or
    non-positive input, matching `slackweb.UnconfirmedFlagPasses`.
18. `cmd/connectors/upworkcrm/main.go` calls the reconciler after `Normalize` and
    prints its count unconditionally, so a pass that flagged nothing and a pass
    that did not run look different (`cmd/connectors/slackweb/main.go` shape).
19. `internal/textmatch/callsites_test.go` still passes: the matcher rewrite keeps
    `textmatch.NormalizedPrefix` on both sides.
20. No SQL anywhere constructs, parses, or pattern-matches an upwork thread key —
    including the matcher, whose client and room scoping is entirely in Go (§4). A
    structural test (the `callsites_test.go` idiom) greps `internal/` for
    `'upwork_crm:'` inside a string that also contains `LIKE`, `split_part` or
    `||`, and fails on a hit. Test files that clean fixtures by prefix are exempted
    explicitly by path, not by accident.
21. **Naming discipline, checked at review rather than by a test.** The
    `confirmUpworkDelivery` block comment, the commit message, the runbook and the
    IK entry describe the scoping as **room-scoped for API-era traffic in both
    directions, client-wide for pre-2026-07-21 history**, and cite facts 3-5
    including the two columns. No artifact calls it plain "room matching" or
    "exact room scoping". The reviewer is asked to check this specifically; it is
    the failure SWT-18 shipped.
22. `go test ./...` and `make integration` are green. The upworkcrm integration
    suite stays in the mutual-cleanup pact and its prefix cleanups
    (`integration_test.go:79,81`) still cover both key shapes.
23. **The production re-key's roomed count must EQUAL an independently computed
    expectation, measured at the same moment** — never a frozen literal.
    **(SUPERSEDES this criterion's original text, which named 432 / 2,009 /
    2,441; those were true on 2026-08-26 and the corpus moved to 2,442 / 433
    within the same afternoon. See "The corpus is LIVE" above.)** The check:

    ```sql
    -- expected, from the raw JSON
    COALESCE(NULLIF(raw_json->>'upwork_room_id',''), NULLIF(raw_json->>'send_room_id','')) IS NOT NULL
    -- actual, from the keys the normalizer wrote
    thread_key LIKE 'upwork\_crm:%:room:%'
    ```

    The two must be equal, and the legacy count must equal total minus roomed.
    A range would let a one-column implementation pass, which is precisely the
    bug criterion 3 exists to catch — such an implementation lands roughly two
    thirds short on the outbound side while producing well-formed keys and no
    errors. Also assert that clients with several rooms now occupy several
    roomed threads (two such clients as of 2026-08-26, with 3 and 2 rooms — read
    the count from the query, not from this line). Fixture-owned corpora keep
    exact literals; this rule is only for production. The runbook
    `docs/runbooks/upwork-room-rekey.md` carries the query verbatim. This is the
    "usable alone" check.

## Data model changes

**None. No migration.** Re-keying happens by re-normalizing from raw (§5);
`thread_key` is a `TEXT` column with a unique index and no format constraint;
`task_events.event_type` has no CHECK, so `delivery_unconfirmed` needs nothing
(fact 18); `deliveries.target_ref` is free text validated in Go.

Vocabulary unchanged: `normalized_threads`, `normalized_messages`,
`raw_source_items`, `deliveries`, `task_events`, `sync_runs`. No new table, no new
column, no synonym.

**Migration-number collision, flagged not resolved:** SWT-17 claims
`0015_capture_rules.sql`. SWT-19 claims nothing. If that changes, take 0016.

## API / MCP tool changes

No new tools, no changed request/response shapes, no MCP surface change.

One behaviour change inside an existing tool: **`draft_delivery`'s validate step**
(`internal/tools/delivery.go` `validateDraftDelivery`) now parses an `upwork_chat`
`target_ref` instead of only checking it is non-empty, and the handler stores the
canonical spelling. This sits at the *validate* stage of the executor path —
registry lookup → **validate** → policy check → audit start → handler → audit
complete (invariant 3) — exactly where `slack_reply`'s `ParseTargetURL` already
sits. A rejected target fails before any policy or audit write.

The reconciler writes `deliveries.error` and one `task_events` row directly, not
through the executor — deliberately identical to `slackweb.ReconcileUnconfirmed`
and to the four post-hoc matchers. It is connector-side observation of an external
fact, not a tool call, and it creates and mutates no delivery lifecycle state.

## MQTT topics

None. This ticket publishes and subscribes to nothing; no heartbeat, no command
topic, no LWT, no `fleet` client.

## Files likely to touch

New:
- `internal/connector/upworkcrm/threadkey.go` — `ThreadKey`, `ParseThreadKey`,
  `ThreadRef`
- `internal/connector/upworkcrm/threadkey_test.go` — pure, no db
- `internal/connector/upworkcrm/reconcile.go` — `ReconcileUnconfirmed`,
  `UnconfirmedFlagPasses`
- `internal/connector/upworkcrm/roomkey_integration_test.go` — the re-key and
  mixed-key cases (criteria 5, 6, 9-14)
- `docs/runbooks/upwork-room-rekey.md` — the `--full --all` procedure, the exact
  expected counts (432 / 2,009 / 2,441), the two room columns and what each means,
  the honest scoping description from criterion 21, and the deploy ordering

Modified:
- `internal/connector/upworkcrm/normalize.go` — `rawCommunication.UpworkRoomID`
  **and `.SendRoomID`**, the `COALESCE` in one place,
  `NormalizedMessage.RoomID`, the `ThreadKey` call at `:99`
- `internal/connector/upworkcrm/sink.go` — the candidate query and Go-side scoping
  in `confirmUpworkDelivery` (`:402-467`) and the block comment at `:287-355`,
  whose "exact thread_key matching" section and its correction paragraph are both
  replaced by §4's table and its honest naming
- `internal/connector/upworkcrm/normalize_test.go` — `:159` asserts the old key
  shape for a room-less row (still correct, keep it, plus roomed siblings for both
  columns)
- `internal/connector/upworkcrm/integration_test.go` — `:243-249` counts threads
  and asserts the client-keyed thread; prefix cleanups at `:79,81,337,338`
- `internal/connector/upworkcrm/matcherhardening_regression_integration_test.go` —
  SWT-18's `RoomDiscrimination` case re-based on real room ids (criterion 11)
- `internal/tools/delivery.go` — `validateDraftDelivery` (`:107-108`) and the
  `draftDelivery` handler's `target_ref` write
- `internal/drafts/store.go` — the upwork branch (`:110-137`), roomed-preferred
- `cmd/connectors/upworkcrm/main.go` — reconciler call + stats line
- `internal/tools/delivery_lifecycle_integration_test.go`,
  `internal/tools/delivery_slack_integration_test.go`,
  `internal/connector/slackweb/confirm_integration_test.go` — canonical fixtures
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` — replace the "'Exact room matching' on
  Upwork is not room matching" entry with what is true after this ticket: the key
  shape, **the two room columns and the COALESCE**, the API-era/legacy boundary as
  the reason unknown rooms are tolerated, and the general lesson (verify a claimed
  data path against the DATA) kept intact. The two-column near-miss is itself the
  best example of that lesson to date and belongs in the entry.

## In scope / Out of scope

**In scope:** the key format and its single Go spelling; reading BOTH room columns
in the normalizer; the mixed-key fallback and the matcher's mismatch-only
exclusion rule; re-keying by `--full --all` re-normalization; canonical
`target_ref` validation for `upwork_chat` in `draft_delivery`; roomed-preferred
target resolution in `internal/drafts`; the upwork reconciler; the runbook; the IK
correction.

**Out of scope — named because they are the tempting bundles:**

- **SWT-17 (capture-rules).** Same file (`internal/drafts/store.go`), adjacent
  problem, separately specced. See "Interaction with SWT-17". Do not implement §9
  of that SPEC here, do not drop `projects.client_person_id`, do not create
  `capture_rules`.
- **Reopening SWT-18.** Its fix is correct and merged. Normalized-prefix
  comparison, the empty-prefix refusal, the already-claimed pre-check, the
  transaction shape and the multi-match refusal are all kept. Only the candidate
  SCOPE changes, and it changes in both directions (tighter on room-vs-room,
  tolerant of unknown rooms).
- **The time-floor question.** Settled: no floor on this tier. Do not add one, do
  not "improve" it with `COALESCE(send_attempted_at, sent_at)`.
- **Anything in `internal/capture`.** No upwork channel there;
  `outbound_observed` for upwork remains deferred.
- **Fixing the one-shot matcher (fact 17).** A sweep that re-examines recent
  messages against newly-`sent` deliveries is a real gap and a real ticket. This
  ticket only makes it *visible*, via the reconciler. Do not turn the reconciler
  into a retrier — a retry on the assisted tier means a human double-posting.
- **Anything in the CRM repo.** Nothing there is broken (fact 4). Do not open a
  ticket against the send path.
- **`upwork_room` person identities from communications.** `NormalizeClient`
  emits one from `clients.upwork_room_id`; emitting one per message room would
  change identity resolution and could raise suspected merges.
- **Promoting `upwork_chat` off the assisted tier.** `internal/policy/matrix.go`
  `:120-125` stays exactly as it is.
- **Deleting empty legacy threads**, and any dashboard view of threads or rooms.

## Invariants that apply

1. **Raw-first.** Nothing here re-scrapes. Both room columns come out of
   `raw_source_items.raw_json` (fact 12), and re-keying is the "reprocessing must
   always be possible" clause being cashed in: the corpus is re-derived from
   stored raw with a changed pure function. `Normalize` must still read only
   `raw_source_items` (criterion 4). The `--full` re-ingest half goes through the
   unchanged `Ingest`, so raw is still written before anything normalizes.
2. **One funnel.** No new table, no task-like row. Threads multiply, tasks do not.
   Nothing creates a task or changes what triage sees; triage's project lookup
   does not read `thread_key`.
3. **Everything through the executor.** The only tool touched is `draft_delivery`,
   and the change lands in its **validate** step inside `Executor.Execute`; no
   handler is invoked from anywhere else and no new surface is exposed to agents.
   The normalizer and the reconciler are connector code writing their own
   observations — the established pattern for all four post-hoc matchers and for
   `slackweb.ReconcileUnconfirmed`.
4. **Nothing external without a delivery row.** Nothing here sends. The reconciler
   must never write `status`, `sent_external_id` or `sent_at` — it annotates and
   raises a signal (criterion 16). Idempotency is preserved: the matcher still
   stamps `sent_external_id` once, under restated `IS NULL` guards with a
   `RowsAffected` check, and the already-claimed pre-check still prevents a replay
   from failing the run on `deliveries_sent_external_idx` (criterion 14).
5. **Own-message loop closure.** The invariant this ticket is *about*. An outbound
   Upwork message must still find the `deliveries` row that produced it. That
   message carries its room in `send_room_id` for 136 of the 220 roomed outbound
   rows (fact 3) — which is exactly why criterion 3 is load-bearing: read one
   column and the write path still looks right while the match silently stops
   finding anything. §4's mismatch-only-excludes rule keeps the path reachable
   across the legacy boundary. The failure this invariant forbids is silent, so
   the reconciler (§6) is part of honouring it, not a bonus.
6. **Stealth attribution.** Nothing client-visible is generated or altered. The
   reconciler's note lands in `deliveries.error`, an internal column.
7. **Orchestrator purity.** The orchestrator is not touched and no rule fires on
   `delivery_unconfirmed` (slackweb's precedent). `ThreadKey`/`ParseThreadKey` and
   `NormalizeCommunication` are pure functions unit-testable with no network and
   no database. The matcher's scoping decision moves INTO Go partly for this
   reason: it becomes a testable function of (message, candidate) rather than a
   SQL predicate.

## Sibling patterns to copy

- **Target canonicalisation at draft time:** `slackweb.ParseTargetURL` +
  `Target.CanonicalURL()`, called from `validateDraftDelivery`
  (`internal/tools/delivery.go:110-114`) and stored by the handler. That is the
  SWT-13 fix; copy its shape exactly, including storing the parsed spelling.
- **The reconciler:** `internal/connector/slackweb/reconcile.go` — pass-counting
  rather than wall-clock, the `unconfirmedNote` marker as the fire-once guard, the
  `RowsAffected` race check before the event insert, and the env override's
  defensive parse. Read its header comment before copying; the
  `sync_runs.started_at` semantics differ.
- **Select candidates, filter in Go, stamp in one transaction:**
  `internal/connector/jira/sink.go:284-340` (the SWT-16 shape) and the current
  `upworkcrm/sink.go:402-467`. §4 keeps that skeleton and moves more of the
  predicate into the Go half.
- **One spelling of a canonicalisation, in Go, never in SQL:**
  `internal/textmatch/prefix.go` and its package doc.
- **Structural enforcement test:** `internal/textmatch/callsites_test.go` —
  criterion 20's source scan copies its shape.
- **Integration fixtures + mutual cleanup pact:**
  `internal/connector/upworkcrm/loopclosure_integration_test.go` and
  `matcherhardening_regression_integration_test.go`.

## Verification protocol

Before commit:

1. `go test ./...` — threadkey round-trip and rejection tests, the four-case room
   source test (criterion 3), the matcher's candidate-rule table as a pure unit
   test if the Go filter is extracted, the source-scan structural test, the
   reconciler's env-override test.
2. `make integration` — `db-up` + `migrate` + `go test -tags integration -p 1
   -count=1 ./...`. Serialized `-p 1` is mandatory (IK: integration suites
   cross-pollute); fixture cleanups must cover both key shapes.
3. `git grep -n "upwork_crm:" -- '*.go'` and read every hit: the only non-test
   string constructions of an upwork key must be in `threadkey.go`.
4. `git grep -n "upwork_room_id\|send_room_id" -- '*.go'` — both must appear, and
   only in `normalize.go` and its tests. One without the other is the bug.
5. Re-read the `confirmUpworkDelivery` comment and the commit message against
   criterion 21 before committing.

Manual smoke, against the real `ops` db (read-only first):

6. Baseline. These are measured values, so **assert equality, not a range** — a
   range would let a one-column implementation through:
   ```sql
   SELECT count(*)                                                              AS raw_rows,      -- 2441
          count(*) FILTER (WHERE r.raw_json ? 'upwork_room_id')                 AS upwork_present, -- 1626
          count(*) FILTER (WHERE COALESCE(r.raw_json->>'upwork_room_id',
                                          r.raw_json->>'send_room_id') IS NOT NULL) AS roomed,     -- 432
          count(DISTINCT COALESCE(r.raw_json->>'upwork_room_id',
                                  r.raw_json->>'send_room_id'))                 AS rooms
     FROM raw_source_items r JOIN source_accounts a ON a.id = r.source_account_id
    WHERE a.provider='upwork_crm' AND r.external_id LIKE 'communications:%';
   ```
   Expect **2,441 / 1,626 / 432**, plus the current 26 threads and 2,441 messages.
   Reading `upwork_room_id` alone gives 296 — if a run ever produces that, the
   normalizer is one-column.
7. Re-key: `UPWORK_CRM_DATABASE_URL=... DATABASE_URL="$OPS_DATABASE_URL" go run
   ./cmd/connectors/upworkcrm --full --all` (build the DSN with `--from-file`
   semantics; IK's `&` landmine).
8. After:
   ```sql
   SELECT thread_key, count(*) FROM normalized_threads t
     JOIN normalized_messages m ON m.thread_id = t.id
    WHERE t.thread_key LIKE 'upwork_crm:%' GROUP BY 1 ORDER BY 1;
   ```
   Expect exactly: **432** messages on `:room:` keys, **2,009** on unchanged
   legacy keys, **2,441** total; at most 14 roomed threads (fact 3); two distinct
   roomed threads for `e2ef9b65-9813-4d79-ac10-0e1813f788ff`; and the 26 legacy
   threads still present. "Most rows did not move" is the designed outcome
   (criterion 23).
9. Idempotency: run step 7 again; thread and message counts unchanged, and
   `sync_runs.stats` shows `raw_unchanged` covering the corpus.
10. Deliveries untouched: `SELECT count(*) FROM deliveries WHERE
    channel='upwork_chat';` — still 0, before and after.
11. **Deploy precondition:** the in-cluster `connector-upworkcrm` CronJob runs a
    pinned tag (currently `0.4.1`). The fix reaches production only after an image
    build and a tag bump in the kube repo, which is the kube session's work. No
    migration this time, so the "merging is not applying" hazard is only about the
    image.

## Interaction with SWT-17 (capture-rules) — ordering

**They must be ordered, and SWT-19 should go first.** Two reasons, one hard:

- **Hard.** SWT-17 §3 writes `thread_key` verbatim into
  `external_refs.external_key` and §9 site B joins
  `nt.thread_key = er.external_key`. If SWT-17 lands (and runs, even in shadow)
  before the re-key, every upwork ref it wrote names a thread key that no longer
  exists, and the join silently returns nothing. Re-keying first means those rows
  are born correct. Cost of the other order: a data fixup on a table whose whole
  point is being the dedup key.
- **Soft.** Both edit `internal/drafts/store.go`'s upwork branch. SWT-17 deletes
  it outright (§9 site E, "let it die"); SWT-19 changes it to prefer a roomed
  thread (criterion 15). If SWT-19 goes first, SWT-17 simply deletes a slightly
  different block — but it must then preserve the roomed-preference property in
  its `external_refs`-based resolution, because the matcher's tightening depends
  on deliveries being targeted at roomed threads where one exists. **Add that as a
  line in SWT-17 when it is implemented.** If SWT-17 goes first, criterion 15 has
  to be re-expressed against code that does not exist yet.

They do not conflict on migrations, because SWT-19 has none.

## Decisions made unilaterally

1. **The unroomed key is byte-identical to today's key**, rather than a new
   `:noroom` spelling. The 2,009 legacy rows need no re-key, no migration and no
   cleanup; the 26 existing thread rows keep their ids and FK edges; and the
   existing tests asserting the current key remain correct assertions about a case
   that still exists.
2. **The roomed key carries a `room:` tag** rather than putting the room id in the
   channel position — roomed-ness becomes a parse instead of a comparison against
   a magic literal (the SWT-13 lesson).
3. **Observed beats dispatched when both columns are set.** They are disjoint
   today, so this is a tiebreak for a case that does not exist; it is written and
   tested anyway because leaving it to field order is how the next reader learns
   the wrong rule. Observed wins because it is ground truth about where the message
   is on Upwork.
4. **No inference of a room from `clients.upwork_room_id` or from sibling
   messages** (§3): unfalsifiable where right, silently merges two conversations
   where wrong, on the one client this ticket exists for.
5. **The matcher's client/room scoping moves into Go**, leaving the SQL with
   channel/status/NULL guards only. Forced by "any roomed key of this client" being
   inexpressible as an equality, and by §2's one-spelling rule.
6. **The reconciler ships in this ticket** (§6), pass default 6 because upwork
   writes two `sync_runs` per invocation.
7. **`draft_delivery` accepts unroomed targets**, because the legacy corpus is
   real and §4's bottom-left cell keeps them confirmable.
8. **SWT-18's `RoomDiscrimination` test is re-based, not kept** — it proves room
   scoping using channel values the source has never emitted.
9. **Criterion 21 makes naming a review item.** It cannot be mechanized, it is the
   exact mistake this ticket was created to correct, and a SPEC that does not ask
   for it will get the same commit message SWT-18 wrote.
10. **Criterion 23 asserts exact counts rather than a range.** 432 roomed / 2,009
    legacy / 2,441 total are measured (fact 9), and a range would let the
    one-column bug pass the very check written to catch it.

## Open questions

**None outstanding, and no unmeasured numbers remain.** Q1 (answered (b),
re-answered **(a)** on the two-column finding) and Q2 (**(a)**) are both closed
with data in `docs/tickets/upwork-room-identity_OPEN_QUESTIONS.md`.

Two things closed after the first fold-in, recorded so they are not reopened:

- **Why outbound rows appeared to lack a room id — CLOSED, not deferred.** They
  never lacked it. It was in `send_room_id`, written by the send path with zero
  disagreements against `send_requested_at` (fact 4). Nothing to investigate or
  fix in the CRM.
- **The 576-vs-1,218 discrepancy an earlier draft flagged — CLOSED.** 576 is the
  outbound half; the whole-table unroomed count is 1,218 (fact 3). And the
  `is_draft` caveat that same draft raised is **empirically false and dropped**:
  8 `is_draft` rows exist in the whole table and none carries `send_room_id`
  (fact 14), which is why the ops-side roomed count matches the source's exactly.

## Future work (not this ticket)

- **Re-examine unconfirmed deliveries against already-normalized messages**
  (fact 17's gap): a bounded sweep, in the reconciler's spirit, that re-runs the
  prefix match for `status='sent'` rows whose `sent_at` postdates the messages in
  their room. Flag-only until it is proven, then promote.
- **`outbound_observed` for `upwork_chat`** — SWT-16's own deferred item, now more
  tractable because `external_id` is a real story id and the thread is a real room.
- **Room-level `external_refs`** (`system='upwork_crm'`, `external_key` = the
  roomed thread key) so a task can point at a conversation without a person lookup
  — the honest replacement for the client-level target SWT-17 removes.
- **`upwork_room` identities per message room**, and reconciling them against
  `clients.upwork_room_id`; a disagreement there is a suspected merge worth
  surfacing.
- **Watch for dispatched/observed duplicate rows.** If the CRM ever stores a
  dispatched message and its later observation as two `communications` rows, they
  key to the same room correctly but become two same-body rows in one thread.
  Criterion 14 pins the current behaviour (refuse, do not double-stamp); a
  deliberate dedup (same room + same story id, or a `send_requested_at`-aware
  merge) would be its own ticket.
- **Promote `upwork_chat` off the assisted tier** when the CRM's flow 3 (GraphQL
  `sendMessage`) ships: an API send returns a story id, `sent_external_id` is set
  at send time, and the post-hoc matcher becomes a redundant safety net. The room
  keying landed here is what makes that transition graceful, and `send_room_id`
  shows the CRM already tracks the destination room at dispatch.

---

## Adversarial review (Codex, 2026-08-26) — what was fixed and what was deferred

Four findings, three rated high. All four verified against the source before
acting; two were fixed in this ticket, two are deferred to SWT-20 with a
mitigation shipped here.

**Fixed here.**

1. *A parseable-but-nonexistent target is a client-wide confirmation wildcard.*
   Opened by this ticket: `SameConversation` treats an unroomed key as compatible
   with any room of the client (the legacy tolerance), and `draft_delivery`
   checked only SHAPE — so `upwork_crm:{real-client}:typo` could be marked sent
   and then confirmed by any outbound message from that client, burning its
   external id on a delivery naming no real conversation. `draft_delivery` now
   requires the target to name an ingested `normalized_threads` row. Legacy keys
   are real threads, so the tolerance costs nothing. Tested, and
   mutation-verified by disabling the check.
2. *The reconciler's alarm named two verbs that both rejected the row it flags.*
   `mark_delivery_sent` takes `approved` (plus two `slack_reply` edges) and the
   flagged row is `sent`; `mark_delivery_failed` took `slack_reply` only. The
   alarm was unactionable — worse than no alarm, because it trains the reader to
   ignore the channel. `mark_delivery_failed` now accepts `upwork_chat` stuck at
   `sent` (human-only, off MCP, still refusing any row carrying
   `sent_external_id` or `confirmed_at`), and the note names it and says plainly
   when to do nothing. No policy change: the verb is `humanOnly` and not
   channel-gated.

**Deferred to SWT-20, with a mitigation shipped here.**

3. *Delivery targeting has no provenance for the originating room.* `DeliverTasks`
   resolves a target from the task's CLIENT, so a task raised by room A can be
   drafted into room B — and since this ticket a wrong-room delivery can never
   confirm, surfacing only via the reconciler. The real fix is recording which
   thread a task came from, which needs a schema change.
   **Mitigation shipped:** when a client has MORE THAN ONE roomed thread and the
   task carries no provenance, the draft worker now refuses to target it at all,
   routing to the existing "unresolvable — tell the human" path. Refusing is
   reversible; a wrong-room send is not. Two production clients are in this state.
4. *Every outbound message locks every unresolved Upwork delivery.* The matcher
   selects all `sent`+unconfirmed upwork rows `FOR UPDATE` and filters in Go, and
   flagged rows never leave that set, so the cost is O(outbound × unresolved)
   once the tier is live. Zero rows today. The fix is persisting indexed
   structured delivery identity (client, optional room) to shortlist before
   locking — a schema change, and the same one finding 3 needs.

Both deferred items are **blockers for enabling the Upwork tier**, not for
merging this: they concern a channel with zero deliveries in production.

## Adversarial RE-review (Codex, 2026-08-27) — three of the four fixes were themselves defective

The re-review of the previous round's fixes returned `needs-attention` with four
more findings, three of them introduced BY those fixes. Each verified against the
source before acting; one turned out to need a nuance the reviewer had not stated.

**Reverted.** *Extending `mark_delivery_failed` to `upwork_chat` was worse than
the gap it filled.* An upwork row reaches `sent` via `mark_delivery_sent`, which
emits `delivery_sent`, which drives orchestrator **R8**: the work task flips to
`delivered`, its Deliver task is CLOSED, and an orchestration row is recorded so
R8 will not run again. `delivery_failed` has no orchestrator rule, so failing the
delivery afterwards leaves a real non-delivery permanently recorded as delivered
with its Deliver task shut — a database disagreeing with the world, silently,
which is the exact class of failure this ticket exists to remove. `slack_reply`
is unaffected because it wedges at `sending`, where `delivery_sent` never fired.
The verb is slack-only again, the test asserting the refusal is restored with
that history in it, and the reconciler's note names actions that exist and do not
corrupt state.

**Fixed.** *A new attempt did not re-arm the alarm.* The reconciler's fire-once
guard is a marker inside `deliveries.error`, and `mark_delivery_sent` did not
clear it — so a row that was flagged, failed, re-approved and re-sent kept the
old marker forever and could never be flagged again. The alarm would have been
permanently silent for precisely the delivery it had already caught once.
`error=NULL` on that transition, matching what `send_delivery`'s success paths
already did. (The reviewer's claim that *nothing* clears it was half right — a
sibling function does; this one was missed.)

**Fixed.** *The reconciler could annotate a row confirmed in the race window.*
Candidates are read before pass counting, so a concurrent normalize run could
confirm a row before the note was written, leaving a contradictory
"unconfirmed" alarm in the task history where a human reads it as evidence. The
full candidate predicate is now restated on the UPDATE, and the event is written
only when `RowsAffected` says the row was still unconfirmed.

**Deferred to SWT-20, with reasoning that decided it.** *The existence check does
not bind the target to the task's client.* Correct: `draft_delivery` proves the
`target_ref` names some ingested thread, not that it belongs to this task's
client, so an agent-facing call can still name another client's thread and
bypass the multi-room refusal in `drafts/store.go`. The reason it is not fixed
here: the only task→client binding available today runs through
`projects.client_person_id`, and **SWT-17 drops that column**. Building the check
on it would be building on something already scheduled for deletion, and SWT-20
introduces the provenance that replaces it. The existence check stays — it is a
strict improvement — and the reviewer's framing is adopted: **provenance is
load-bearing for delivery CORRECTNESS, not merely targeting quality**, which
makes SWT-20 a hard go-live gate rather than a nice-to-have.

Nothing here is reachable in production: the channel has never had a delivery.

## Adversarial pass THREE (Codex, 2026-08-27) — and why the passes stop here

Three fixed, one deferred. Notably, Codex independently searched for an
alternative to the deferred client binding and confirmed there is none that does
not depend on a column SWT-17 deletes — so that deferral is now corroborated
rather than asserted.

**Fixed.** *The reconciler race was narrowed, not closed.* The marker UPDATE and
the `delivery_unconfirmed` insert were separate statements, so they autocommitted
between: a normalize run could confirm the row in the gap (announcing
"unconfirmed" about a confirmed delivery), and if the event insert failed the
marker was already committed, suppressing every future attempt — an alarm that
silently loses itself. Both now run in ONE transaction with the row locked.

**Fixed.** *The re-arm destroyed diagnostics, and my comment about it was false.*
`error=NULL` wiped any prior sender failure along with the marker, justified by a
comment claiming the text survived in the audit trail. Codex checked: the
executor audit stores the caller's ARGUMENTS, not the row's prior state, and the
task events carry only ids. The re-arm now strips the marker substring alone and
leaves unrelated diagnostics intact.

**Fixed, and this was the pass's best idea.** *There was no ENFORCED go-live
gate.* The deferred client binding lived only as a note in SWT-20, so nothing
stopped the Upwork tier being switched on before provenance existed.
`draft_delivery` is MCP-listed and this session ingests client mail and chat, so
an injected call could name a real thread belonging to a DIFFERENT client for a
human to approve later. Agent-supplied `upwork_chat` drafts are now refused
outright; the dashboard and opsctl paths stay open, because a human choosing a
target deliberately is not the exposure. Mutation-tested. **The gate can no
longer be crossed by forgetting about it.**

**Deferred to SWT-20.** *A verified-but-unlinked row stays a matcher candidate
forever.* When the operator confirms the message IS in Upwork, there is no verb
to attach the external id, so the row remains in the unconfirmed candidate set
and a later same-prefix message could be claimed by it. Real, and it needs the
same evidence-backed confirmation verb as the recovery transition — the two
belong together in SWT-20 rather than as a third partial fix here.

### Why this is the last pass

Each round found real defects, and each round's findings sat further inside a
code path that **cannot execute**: `upwork_chat` has never had a delivery in
production, and it is now gated in code against being enabled. The remaining
findings all converge on one thing — the delivery-recovery and provenance work in
SWT-20 — rather than on the thread keying this ticket actually changed, which was
verified against production on 2026-08-27: 434 roomed of 2,443, matching an
independently computed expectation exactly, with both multi-room clients split.

Continuing to iterate would keep producing SWT-20-shaped findings about
unreachable code. The honest close is: SWT-19's own change is verified and live;
everything the reviewer keeps surfacing is the go-live gate, and that gate is now
enforced rather than documented.
