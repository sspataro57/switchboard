> Jira: SWT-19

# upwork-room-identity — key Upwork threads on the room, not the client

**Both open questions answered 2026-08-26 and folded in**
(`docs/tickets/upwork-room-identity_OPEN_QUESTIONS.md` holds the data verbatim).
Q1 came back **(b)** and inverted criterion 10; Q2 came back **(a)** and fixed the
numbers in the verification protocol. This SPEC is no longer provisional.

**Read §4 before writing the commit message.** Q1's answer means this ticket is
NOT "room matching" for the half of the data the matcher sees, and saying
otherwise — in the commit, the code comment, or IK — would repeat, one ticket
later, the exact overstatement SWT-18's review just corrected.

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

Make `normalized_threads.thread_key` carry the Upwork room
(`communications.upwork_room_id`) where the source supplies one, give the key one
canonical spelling that every writer and reader shares, re-key the existing
corpus by re-normalizing from raw, and make the delivery matcher tighten on room
identity **when both sides know the room** without losing the confirmations it
makes today.

**Usable alone** means: with only the already-deployed `connector-upworkcrm`
CronJob (no draft worker, no triage, no orchestrator, no dashboard change), one
run re-keys the corpus and the client that has two Upwork rooms
(`e2ef9b65-9813-4d79-ac10-0e1813f788ff`) becomes two threads instead of one —
observable in `psql` and on the existing `/tasks` surfaces — while every message
the source gave no room for stays exactly where it is today. Nothing else in the
system has to move for that to be true or useful.

## Established facts — do not re-derive

Taken as given from the SWT-18 review, from the Q1/Q2 runs against pg-main on
2026-08-26, and from reading `main` at `526233b`.

1. `thread_key` is `Provider + ":" + client_id + ":" + channel`
   (`internal/connector/upworkcrm/normalize.go:99`) and `channel` is the constant
   `'upwork'`: 1,650 source rows, 26 clients, one distinct value. Every
   `normalized_threads` key in the ops db ends `:upwork`. **One thread == one
   client. Room identity is not operating anywhere today.**
2. SWT-18's `target_ref = thread_key` equality in `confirmUpworkDelivery`
   therefore selects exactly the candidate set the old client-wide `LIKE` did.
   The thing preventing a wrong-row bind today is the **multi-match refusal**,
   not the predicate. SWT-18 is correct and merged; it is not reopened here.
3. `communications` has 1,650 rows. **296 carry `upwork_room_id`; 1,354 (82%) are
   NULL.** 11 distinct room ids. One client already has 2 rooms.
4. **(Q1) The room-id gap is a DIRECTION gap, not an era gap, and it is not
   closing.** In the API era (since the first roomed row, 2026-07-21):
   inbound 213 rows / 212 roomed (99.5%); **outbound 188 rows / 84 roomed
   (44.7%)**. Last 7 days: inbound 8/8, **outbound 1/6**. Oldest roomed row
   2026-07-21, newest room-LESS row **2026-08-25** — the ranges overlap, so there
   is no cutover date. Not per-client either: the two highest-volume clients are
   internally mixed (165/107 and 146/118).
5. **(Q1, the consequence that shapes the design)** `confirmUpworkDelivery` runs
   for `direction == "outbound"` only (`sink.go:277`). Outbound is the half that
   lacks the room. So **a majority of our own sends will re-enter with no room
   id**, and any rule that requires a room on the observed message would leave
   most deliveries unconfirmable forever.
6. The ops db holds 26 upwork threads carrying 2,441 messages.
7. **(Q2)** Of those 2,441 raw communications rows: 1,626 have the
   `upwork_room_id` key present, **296 non-null (matching the source exactly)**,
   and **815 lack the key entirely**, with a date range ending 2026-07-11 —
   history ingested before the column existed, whose source rows the CRM has
   since replaced. `--full` cannot refresh them. There is no hidden population:
   296 in the source, 296 in ops raw.
8. Production has **ZERO `upwork_chat` deliveries**, and has never had one.
9. Ingestion moved from browser-extension scraping to API access; source
   `external_id` is now `story_<hex>` after the 2026-07-14 backfill.

### Facts established by reading the code

10. **The room is already in the ops db.** `PGSource.ListCommunications` selects
    `to_jsonb(m)` (`internal/connector/upworkcrm/source.go:67`), so
    `raw_source_items.raw_json` carries every column of the source row, including
    `upwork_room_id`, for any row ingested after the column existed (fact 7's
    1,626). Re-keying is therefore a re-normalize from raw (invariant 1's
    promise), not a data migration and not a re-scrape.
11. **`rawCommunication` does not parse the column.** `normalize.go:44-54` has no
    `UpworkRoomID` field; `NormalizeCommunication` never sees it. `rawClient`
    *does* (`normalize.go:41`) and already emits an `upwork_room` person identity
    from `clients.upwork_room_id` — so the ops db has a client-level room notion
    already, and it is NOT the same thing as the per-message room.
12. **`thread_key` has exactly two readers and two writers in Go**, all named
    below: written by `upsertMessage` via `NormalizeCommunication`; read by
    `confirmUpworkDelivery` (`sink.go:414`) and by
    `internal/drafts/store.go:126-137`. Nothing else parses it. `internal/capture`
    has no upwork channel at all.
13. **`drafts` currently prefers the newest thread by accident.**
    `store.go:126-129` is `WHERE thread_key LIKE 'upwork_crm:'||$1||':%' ORDER BY
    id DESC LIMIT 1`. After re-keying, roomed threads have higher ids than the
    legacy client thread, so it happens to prefer a roomed thread. Accidental
    correctness is not correctness; criterion 14 makes it deliberate.
14. **The matcher is one-shot per message.** `confirmUpworkDelivery` is reachable
    only from `upsertMessage`, which runs only for raw items returned by
    `pendingRaw` (`sink.go:140-146`). A message normalized *before* its delivery
    reaches `status='sent'` is never re-examined, so on the assisted tier — where
    a human clicks "mark sent" minutes or hours after pasting — a real fraction of
    confirmations can never fire. This is pre-existing, is NOT fixed here (see Out
    of scope), and is the strongest argument for the reconciler in §6.
15. **`task_events.event_type` has no CHECK** (`0001_initial.sql:145`), so a new
    `delivery_unconfirmed` event for `upwork_chat` needs no migration. slackweb
    already writes that type.
16. **The highest migration is `0014_imap_mail.sql`.** `docs/tickets/
    capture-rules_SPEC.md` (SWT-17, unimplemented) claims 0015. **This ticket
    needs no migration at all** (§5), so the collision is moot for SWT-19; see
    "Migration numbering" below.

## Design

### 1. The key shape

```
roomed    upwork_crm:{client_id}:room:{room_id}     (new)
unroomed  upwork_crm:{client_id}:{channel}          (unchanged — today's key)
```

A message with a non-empty `upwork_room_id` gets the roomed form. A message
without one keeps **byte-identical** today's key. That single choice is what
makes the whole ticket cheap: 2,145 of the 2,441 ops messages (fact 7: the 815
key-less plus the 1,330 key-present-but-null) need no re-key, no migration and no
cleanup, and the 26 existing thread rows keep their identity, their
`participants` and their FK edges. Only the 296-row roomed subset moves.

The `room:` tag exists so that "is this key roomed?" is a **parse, not a guess
about the third segment's contents**. The alternative — `upwork_crm:{client}:
{room_id}` with the legacy `:upwork` as fallback — makes the two forms
distinguishable only by comparing the third segment against a magic literal,
which is the SWT-13 canonicalisation landmine in yet another costume. Room ids
may contain anything; the tag cannot be confused with a channel value because
the unroomed form has three segments and the roomed form has four.

### 2. One spelling, in Go, in the connector package

New in `internal/connector/upworkcrm/threadkey.go`:

```go
func ThreadKey(clientID, roomID, channel string) string
func ParseThreadKey(key string) (ThreadRef, error)   // ThreadRef{ClientID, RoomID, Channel, Roomed}
```

`ParseThreadKey` splits with `strings.SplitN(key, ":", 4)`: prefix must be
`upwork_crm`, `client_id` non-empty, and either (4 segments, third == `room`,
remainder non-empty → roomed, room id keeps any embedded colons) or (3 segments,
third non-empty → unroomed with that channel). Anything else is an error.

Every producer and consumer goes through these two functions — `normalize.go`,
`sink.go`'s matcher, `drafts/store.go`, and `draft_delivery`'s validator. **Do
not re-spell the format in SQL**, for the same reason `textmatch` may not be
re-spelled in SQL: two spellings of one canonicalisation is the failure mode this
repo has already paid for four times (SWT-13, SWT-16, SWT-18, and the
`LIKE`-vs-`=` scope in this very function). §4 leans on this hard — the matcher's
scoping now happens entirely in Go precisely so no SQL has to know the format.

### 3. The fallback is a real thread, not a bucket rename — and it is permanent

The unroomed key is **the same key it is today**, which means those rows keep
exactly the behaviour they have now: one thread per client, holding both the
scraping-era corpus and — per fact 4 — the 55% of current outbound traffic the
source gives no room for.

**Mixed keying is accepted and is the PERMANENT shape**, not a transition. Q1
settled that: the newest room-less row is from yesterday, five of the last six
outbound rows have no room, and the boundaries overlap so there is no era to wait
out. The hard rule that stops mixed keying becoming ambiguity is: *a thread is
roomed if and only if the source row named a room.* No inference, no inheritance.
Specifically **rejected**: filling a message's room from `clients.upwork_room_id`
(fact 11) or from the client's other messages. For 25 of 26 clients that
inference would usually be right and would be unfalsifiable; for the client with
two rooms it would silently merge two conversations into one, and that client is
the entire reason this ticket exists.

**Consequence, stated rather than discovered — conversation context fragments.**
Because inbound is 99.5% roomed and outbound only 44.7%, a live client's
conversation now splits across a roomed thread (all their messages, some of ours)
and the legacy thread (the rest of ours). `drafts.loadThreadContext` reads the
last 6 messages of ONE thread (`store.go:150-179`), so a draft prompt could see
the client's questions without our previous replies. That is a draft-quality
issue, not an invariant breach — every upwork delivery is assisted tier,
human-reviewed before a human pastes it — and nothing consumes this context today
(zero deliveries, `cmd/drafts` not deployed). It is **out of scope** here because
SWT-17 §9 rewrites context assembly wholesale; it is in Future work, and it must
be named in SWT-17 when that is implemented.

### 4. What mixed keying does to the matcher — read this before naming the commit

SWT-18's single equality does not survive Q1's answer. If the observed outbound
message must carry a room to match, then per fact 5 the majority of deliveries
become unconfirmable forever — a regression dressed as precision, and silent,
which is the worst combination this repo has.

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

**Say what this is.** For the outbound half — the only half the matcher ever sees
— this is **client-wide scoping most of the time**, with room identity as an
opportunistic tightening that applies in the 44.7% of cases where the source
supplied a room on our side too. It is strictly tighter than today and strictly
looser than the SPEC's first draft, and it is the honest reading of the data. The
code comment, the commit message, the runbook and the IK entry must all describe
it this way. **Do not call this ticket "room matching."** SWT-18 called its change
that and was wrong on production data; repeating the overstatement one ticket
after the correction would be worse than the original error, because this time
the data is in hand.

**Implementation shape: scope in Go, not in SQL.** The candidate query keeps only
channel/status/NULL guards —

```sql
SELECT id, task_id, COALESCE(target_ref,''), COALESCE(body,'') FROM deliveries
 WHERE channel='upwork_chat' AND status='sent'
   AND sent_external_id IS NULL AND confirmed_at IS NULL
 ORDER BY id DESC FOR UPDATE
```

— and client/room scoping plus the `textmatch.NormalizedPrefix` comparison both
happen in Go. This is forced, not stylistic: "any roomed key of this client"
cannot be expressed as an equality, and expressing it as a `LIKE` or a
`split_part` would put a second spelling of the key format in SQL — the precise
thing §2 exists to prevent. The set is bounded by *unconfirmed assisted upwork
deliveries*, which is 0 today and is meant to stay small; if it ever grows large
enough to matter, that is itself the alarm the reconciler raises. Two concurrent
connector runs lock in the same `id DESC` order, so they block rather than
deadlock.

Everything else about the matcher is unchanged: normalized 120-char prefix,
`status='sent'`, the `IS NULL` guards restated on the UPDATE, `FOR UPDATE`, the
`RowsAffected` check before the `delivery_confirmed` event, the already-claimed
pre-check, and **no time bound**.

**The multi-match refusal STAYS**, and after Q1 it is carrying more weight than
before, not less: with the outbound half effectively client-scoped, it is again
the primary defence against a wrong bind. The trade is unchanged and still
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

Consequences to expect and verify, not to be surprised by:

- **Only 296 of 2,441 messages move** (fact 7). The total is unchanged and most
  rows stay exactly where they are. **Write this in the runbook**: "most rows did
  not move" is the designed outcome, not a failed migration.
- **Old threads are not deleted.** A client whose messages all carry rooms would
  leave behind an empty `upwork_crm:{client}:upwork` thread. Per fact 4 that is
  unlikely for any active client, but empty threads are inert (nothing selects a
  thread by anything but key or message join) and deleting them would need a
  migration for no benefit. Leave them.
- **The 815 key-less rows are frozen** (fact 7): their source rows no longer
  exist, `--full` cannot refresh them, and they normalize unroomed forever. That
  is what the legacy key is for.
- **`--all` re-runs `confirmUpworkDelivery` for every outbound message** (~1,141
  of them). With zero `upwork_chat` deliveries in production this is provably a
  no-op today (fact 8) — and it is the reason to do the re-key NOW rather than
  after the Upwork tier goes live.

**Migration numbering.** SWT-19 adds no migration, so it does not take 0015 and
SWT-17 keeps it. If implementation discovers a migration is unavoidable, take
**0016** and say so in the commit; do not renumber SWT-17's SPEC from this
ticket. The general rule stands: the number belongs to whatever merges first, and
the loser renumbers — an already-applied file is never edited.

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
spelling rather than the caller's (the SWT-13 fix's shape: "`draft_delivery` now
stores `Target.CanonicalURL()`, never the caller's spelling"). This runs inside
the executor's validate step — no new tool, no new handler, no policy change.

Both roomed and unroomed target refs are accepted. Refusing unroomed targets was
considered and rejected before Q1 and is doubly wrong after it: with 55% of
outbound traffic room-less, unroomed targets are ordinary, not legacy.

**And upworkcrm gets a reconciler**, `internal/connector/upworkcrm/reconcile.go`,
a near-copy of `slackweb/reconcile.go` scoped to `channel='upwork_chat'` and
`status='sent'`. It flags — never retries, never invents a `sent_external_id` —
rows no run has confirmed after N completed successful `sync_runs` for the upwork
account, appending a note to `deliveries.error` (fire-once via the
`unconfirmedNote` marker) and writing one `delivery_unconfirmed` task event.

Why it belongs in THIS ticket rather than as future work: every failure mode
around this matcher has the same signature — a row at `status='sent'`,
`sent_external_id` NULL, forever, with nothing anywhere saying so. That covers a
`target_ref` that no longer parses, a refusal on ambiguity (which Q1 makes *more*
likely, since the outbound half is client-scoped again), and the pre-existing
one-shot gap in fact 14. Shipping a change to the matcher's scoping without the
detector is shipping a silent failure mode on purpose.

**Landmine specific to upwork's pass counting:** one `upworkcrm` invocation
writes **TWO** `sync_runs` rows — `Ingest` calls `StartRun` (`ingest.go:70`) and
`Normalize` calls it again (`normalize.go:148`) — and both finish `ok`. A
threshold copied from slackweb's 3 would therefore fire after 1.5 CronJob
invocations (~22 minutes). Count in the same unit slackweb does (completed `ok`
runs) but set the upwork default to **6** (= 3 invocations ≈ 45 minutes at
`*/15`), and say in the comment that 6 means 3 because of the double-run. Do not
try to distinguish ingest runs from normalize runs by their `stats` jsonb: both
marshal the same `Stats` struct, so the discriminating keys are present-and-zero
in both — a predicate whose discriminating column is a constant, which is the
exact mistake SWT-18's review caught.

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
3. `NormalizeCommunication` parses `upwork_room_id` from the raw row and produces
   `upwork_crm:{client}:room:{room}` when it is present and non-empty, and the
   byte-identical current key `upwork_crm:{client}:{channel}` when it is absent,
   NULL, or empty string. `NormalizedMessage` carries the room id as its own
   field (do not make callers re-parse the key). All three absent-forms are
   tested separately — fact 7 proves "key missing" and "key present but null" are
   both real populations in raw.
4. Normalization stays deterministic and raw-only: the same raw row normalizes to
   the same output on repeated calls, and no code path in `Normalize` reads the
   source database.
5. An integration test over a fixture corpus containing one client with two rooms
   plus room-less rows produces: one thread per (client, room) pair, ONE thread
   for the room-less rows under the legacy key, and every message pointing at the
   right thread. Re-running the normalize pass with `--all` changes no counts.
6. Re-normalizing a corpus that was first normalized under the old key moves the
   roomed messages to new threads and leaves the room-less ones on the original
   thread row (same `normalized_threads.id`), with no orphaned
   `normalized_messages` and no thread deleted.
7. `validateDraftDelivery` refuses an `upwork_chat` `target_ref` that
   `ParseThreadKey` rejects, with the error naming the target; it accepts both the
   roomed and unroomed forms; and `draft_delivery` persists the canonical spelling
   rather than the caller's (a target with surrounding whitespace is stored
   trimmed and canonical).
8. The four existing fixtures that write a non-canonical upwork target
   (`internal/tools/delivery_lifecycle_integration_test.go:395`,
   `internal/tools/delivery_slack_integration_test.go:616` and `:757`,
   `internal/connector/slackweb/confirm_integration_test.go:452` — all spelled
   `upwork_crm:itest-*:chat`) are updated to canonical keys, and `make integration`
   is green. A test asserting the *old* spelling is now refused must exist, so the
   change is pinned rather than merely accommodated.
9. **A ROOMED outbound message confirms** (a) a delivery whose `target_ref` is
   that room's key, and (b) a delivery whose `target_ref` is the same client's
   unroomed key. Both are separate cases with their own assertions.
10. **(Q1 = (b); this criterion is the inverse of the first draft's.)** An
    **UNROOMED outbound message DOES confirm** a delivery whose `target_ref` is a
    roomed key of the same client, when it is the only prefix match. The test
    comment must carry the reason — 44.7% of API-era outbound rows carry a room,
    1 of the last 6 — because a future reader will otherwise "tighten" this back
    into the bug it was written to prevent.
11. Two deliveries in DIFFERENT rooms of the same client with byte-identical
    bodies, and a **roomed** message: only the one whose `target_ref` names the
    message's room is stamped, in both `sent_at` orderings. This is SWT-18's
    `RoomDiscrimination` test **rebuilt on `upwork_room_id`** rather than on
    channel values the source has never emitted (`chat`, `room-b`) — the old
    fixture must be deleted or re-based, not left as a passing proof of nothing.
    This is the one case where room identity genuinely pays, and it is the only
    exclusion the rule makes.
12. A delivery belonging to a DIFFERENT client is never a candidate, even with an
    identical body; and a delivery whose `target_ref` does not parse is never a
    candidate, in either message shape.
13. Two deliveries reachable from one message (same room, or one roomed and one
    unroomed for the same client) sharing a normalized 120-char prefix: neither is
    stamped (multi-match refusal survives), and the reconciler flags both once the
    pass threshold is reached.
14. `internal/drafts/store.go` resolves the upwork target through
    `ParseThreadKey`, preferring a **roomed** thread for the client and falling
    back to the unroomed one, deliberately rather than by `ORDER BY id DESC`
    (fact 13). A test with both thread shapes present for one client asserts the
    roomed key is chosen regardless of insertion order.
15. `upworkcrm.ReconcileUnconfirmed` flags an `upwork_chat` delivery that no run
    has confirmed after N completed `ok` sync runs started after its `sent_at`:
    appends the marker note to `error`, writes exactly one `delivery_unconfirmed`
    task event, changes no `status`, and writes no `sent_external_id`. Running it
    again flags nothing (fire-once on the marker).
16. The reconciler's pass default is 6 with a comment stating that upwork writes
    two `sync_runs` rows per invocation, and the env override
    (`UPWORK_UNCONFIRMED_FLAG_PASSES`) falls back to the default on unparseable or
    non-positive input, matching `slackweb.UnconfirmedFlagPasses`.
17. `cmd/connectors/upworkcrm/main.go` calls the reconciler after `Normalize` and
    prints its count unconditionally, so a pass that flagged nothing and a pass
    that did not run look different (`cmd/connectors/slackweb/main.go` shape).
18. `internal/textmatch/callsites_test.go` still passes: the matcher rewrite keeps
    `textmatch.NormalizedPrefix` on both sides.
19. No SQL anywhere constructs, parses, or pattern-matches an upwork thread key —
    including the matcher, whose client and room scoping is entirely in Go (§4). A
    structural test (the `callsites_test.go` idiom) greps `internal/` for
    `'upwork_crm:'` inside a string that also contains `LIKE`, `split_part` or
    `||`, and fails on a hit. Test files that clean fixtures by prefix are
    exempted explicitly by path, not by accident.
20. **Naming discipline, checked at review rather than by a test.** The
    `confirmUpworkDelivery` block comment, the commit message, the runbook and the
    IK entry describe the scoping as *client-wide for the outbound half, tightened
    on room identity only when both sides carry a room*, and cite fact 4's
    numbers. No artifact of this ticket calls it "room matching" or "exact room
    scoping". The reviewer is asked to check this specifically; it is the failure
    SWT-18 shipped.
21. `go test ./...` and `make integration` are green. The upworkcrm integration
    suite stays in the mutual-cleanup pact and its prefix cleanups
    (`integration_test.go:79,81`) still cover both key shapes.
22. After the re-key run against production, the client
    `e2ef9b65-9813-4d79-ac10-0e1813f788ff` has two `normalized_threads` rows with
    distinct roomed keys, and the total upwork message count is unchanged
    (2,441 → 2,441) with exactly 296 messages on roomed keys. This is the "usable
    alone" check.

## Data model changes

**None. No migration.** Re-keying happens by re-normalizing from raw (§5);
`thread_key` is a `TEXT` column with a unique index and no format constraint;
`task_events.event_type` has no CHECK, so `delivery_unconfirmed` needs nothing
(fact 15); `deliveries.target_ref` is free text validated in Go.

The vocabulary is unchanged: `normalized_threads`, `normalized_messages`,
`raw_source_items`, `deliveries`, `task_events`, `sync_runs`. No new table, no
new column, no synonym.

**Migration-number collision, flagged not resolved:** SWT-17
(`docs/tickets/capture-rules_SPEC.md`) claims `0015_capture_rules.sql`. SWT-19
claims nothing. If that changes, take 0016.

## API / MCP tool changes

No new tools, no changed request/response shapes, no MCP surface change.

One behaviour change inside an existing tool: **`draft_delivery`'s validate step**
(`internal/tools/delivery.go` `validateDraftDelivery`) now parses an
`upwork_chat` `target_ref` instead of only checking it is non-empty, and the
handler stores the canonical spelling. This sits at the *validate* stage of the
executor path — registry lookup → **validate** → policy check → audit start →
handler → audit complete (invariant 3) — exactly where `slack_reply`'s
`ParseTargetURL` already sits. A rejected target fails before any policy or audit
write, same as every other validation error.

The reconciler writes `deliveries.error` and one `task_events` row directly, not
through the executor — deliberately identical to `slackweb.ReconcileUnconfirmed`
and to the four post-hoc matchers. It is connector-side observation of an
external fact, not a tool call, and it creates and mutates no delivery lifecycle
state (no status change, no `sent_external_id`, no send).

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
  mixed-key cases (criteria 5, 6, 9-13)
- `docs/runbooks/upwork-room-rekey.md` — the `--full --all` procedure, the
  before/after counts to assert (including "only 296 of 2,441 move"), the
  outbound-room gap and what it means for the matcher's scoping, and the deploy
  ordering

Modified:
- `internal/connector/upworkcrm/normalize.go` — `rawCommunication.UpworkRoomID`,
  `NormalizedMessage.RoomID`, the `ThreadKey` call at `:99`
- `internal/connector/upworkcrm/sink.go` — the candidate query and Go-side scoping
  in `confirmUpworkDelivery` (`:402-467`) and the block comment at `:287-355`,
  whose "exact thread_key matching" section and its correction paragraph are both
  replaced by §4's table and its honest naming
- `internal/connector/upworkcrm/normalize_test.go` — `:159` asserts the old key
  shape for a room-less row (that assertion is still correct and should stay,
  plus a roomed sibling)
- `internal/connector/upworkcrm/integration_test.go` — `:243-249` counts threads
  and asserts the client-keyed thread; prefix cleanups at `:79,81,337,338`
- `internal/connector/upworkcrm/matcherhardening_regression_integration_test.go` —
  SWT-18's `RoomDiscrimination` case is re-based on real room ids (criterion 11)
- `internal/tools/delivery.go` — `validateDraftDelivery` (`:107-108`) and the
  `draftDelivery` handler's `target_ref` write
- `internal/drafts/store.go` — the upwork branch (`:110-137`), roomed-preferred
- `cmd/connectors/upworkcrm/main.go` — reconciler call + stats line
- `internal/tools/delivery_lifecycle_integration_test.go`,
  `internal/tools/delivery_slack_integration_test.go`,
  `internal/connector/slackweb/confirm_integration_test.go` — canonical fixtures
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` — replace the "'Exact room matching' on
  Upwork is not room matching" entry with what is true after this ticket: the key
  shape, the direction gap (fact 4) as the reason the outbound half stays
  client-scoped, and the general lesson (verify a claimed data path against the
  DATA) kept intact

## In scope / Out of scope

**In scope:** the key format and its single Go spelling; parsing
`upwork_room_id` in the normalizer; the mixed-key fallback and the matcher's
mismatch-only exclusion rule; re-keying by `--full --all` re-normalization;
canonical `target_ref` validation for `upwork_chat` in `draft_delivery`;
roomed-preferred target resolution in `internal/drafts`; the upwork reconciler;
the runbook; the IK correction.

**Out of scope — named because they are the tempting bundles:**

- **SWT-17 (capture-rules).** Same file (`internal/drafts/store.go`), adjacent
  problem, separately specced. See "Interaction with SWT-17" below. Do not
  implement §9 of that SPEC here, do not drop `projects.client_person_id`, do not
  create `capture_rules`.
- **Reopening SWT-18.** Its fix is correct and merged. Normalized-prefix
  comparison, the empty-prefix refusal, the already-claimed pre-check, the
  transaction shape and the multi-match refusal are all kept as-is. Only the
  candidate SCOPE changes, and it changes in both directions (tighter on
  room-vs-room, looser on unknown rooms).
- **The time-floor question.** Settled: no floor on this tier. Do not add one, do
  not "improve" it with `COALESCE(send_attempted_at, sent_at)`.
- **Anything in `internal/capture`.** There is still no upwork channel there;
  `outbound_observed` for upwork remains deferred (SWT-16's own future work).
- **Fixing the one-shot matcher (fact 14).** A sweep that re-examines recent
  messages against newly-`sent` deliveries is a real gap and a real ticket. This
  ticket only makes it *visible*, via the reconciler. Do not turn the reconciler
  into a retrier — a retry on the assisted tier means a human double-posting.
- **Fixing the fragmented draft context (§3).** Context assembly belongs to
  SWT-17's drafts rework.
- **Chasing the missing outbound room id at the source.** See Future work: it may
  well be the honest fix, but it lives in the CRM repo, not here, and this
  ticket's rule is correct whether or not it lands.
- **`upwork_room` person identities from communications.** `NormalizeClient`
  emits one from `clients.upwork_room_id`; emitting one per message room would
  change identity resolution and could raise suspected merges. Separate concern,
  no benefit here.
- **Promoting `upwork_chat` off the assisted tier.** `internal/policy/matrix.go`
  `:120-125` stays exactly as it is. Autonomy is earned, and the CRM's send flow
  (flow 3) has not shipped.
- **Deleting empty legacy threads**, and any dashboard view of threads or rooms.

## Invariants that apply

1. **Raw-first.** Nothing here re-scrapes. The room id comes out of
   `raw_source_items.raw_json` (fact 10), and re-keying is exactly the
   "reprocessing must always be possible" clause being cashed in: the corpus is
   re-derived from stored raw with a changed pure function. `Normalize` must
   still read only `raw_source_items` (criterion 4). The `--full` re-ingest half
   goes through the unchanged `Ingest`, so raw is still written before anything
   normalizes.
2. **One funnel.** No new table, no task-like row. Threads multiply, tasks do
   not. Nothing in this ticket creates a task or changes what triage sees, and
   triage's project lookup does not read `thread_key`.
3. **Everything through the executor.** The only tool touched is
   `draft_delivery`, and the change lands in its **validate** step inside
   `Executor.Execute`; no handler is invoked from anywhere else and no new
   surface is exposed to agents. The normalizer and the reconciler are connector
   code writing their own observations, the established pattern for all four
   post-hoc matchers and for `slackweb.ReconcileUnconfirmed`.
4. **Nothing external without a delivery row.** Nothing here sends. The
   reconciler must never write `status`, `sent_external_id` or `sent_at` — it
   annotates and raises a signal, and criterion 15 pins that. Idempotency is
   preserved: the matcher still stamps `sent_external_id` once, under restated
   `IS NULL` guards with a `RowsAffected` check, and the already-claimed
   pre-check still prevents a replay from failing the run on
   `deliveries_sent_external_idx`.
5. **Own-message loop closure.** This is the invariant the ticket is *about*, and
   Q1 is the reason its rule came out looser than the first draft. Concretely:
   after the key change, an outbound Upwork message must still find the
   `deliveries` row that produced it, and per fact 5 that message usually carries
   no room. The write path is `upsertMessage` → `confirmUpworkDelivery` →
   `sent_external_id` + `confirmed_at` + `delivery_confirmed`; §4's
   mismatch-only-excludes rule is what keeps that path reachable across the
   roomed/unroomed boundary in both directions. The failure this invariant forbids
   is silent, so the reconciler (§6) is part of honouring it, not a bonus.
6. **Stealth attribution.** Nothing client-visible is generated or altered. The
   reconciler's note lands in `deliveries.error`, an internal column, and names no
   tooling.
7. **Orchestrator purity.** The orchestrator is not touched and no rule fires on
   `delivery_unconfirmed` (slackweb's precedent). `ThreadKey`/`ParseThreadKey` and
   `NormalizeCommunication` are pure functions unit-testable with no network and
   no database — the same discipline, applied to the connector. The matcher's
   scoping decision moves INTO Go partly for this reason: it becomes a testable
   function of (message, candidate) rather than a SQL predicate.

## Sibling patterns to copy

- **Target canonicalisation at draft time:** `slackweb.ParseTargetURL` +
  `Target.CanonicalURL()`, called from `validateDraftDelivery`
  (`internal/tools/delivery.go:110-114`) and stored by the handler. That is the
  SWT-13 fix; copy its shape exactly, including storing the parsed spelling.
- **The reconciler:** `internal/connector/slackweb/reconcile.go` — pass-counting
  rather than wall-clock (a suspended CronJob must not false-flag), the
  `unconfirmedNote` marker as the fire-once guard, the `RowsAffected` race check
  before the event insert, and the env override's defensive parse. Read its
  header comment before copying; the `sync_runs.started_at` semantics differ
  (slackweb passes the export start explicitly; upwork's default `now()` at insert
  is correct here because ingest starts its run first).
- **Select candidates, filter in Go, stamp in one transaction:**
  `internal/connector/jira/sink.go:284-340` (the SWT-16 shape) and the current
  `upworkcrm/sink.go:402-467`. §4 keeps that skeleton and only moves more of the
  predicate into the Go half.
- **One spelling of a canonicalisation, in Go, never in SQL:**
  `internal/textmatch/prefix.go` and its package doc.
- **Structural enforcement test:** `internal/textmatch/callsites_test.go` —
  criterion 19's source scan copies its shape (plain unit test, no build tag, no
  db, explicit exemption list).
- **Integration fixtures + mutual cleanup pact:**
  `internal/connector/upworkcrm/loopclosure_integration_test.go` and
  `matcherhardening_regression_integration_test.go`.

## Verification protocol

Before commit:

1. `go test ./...` — threadkey round-trip and rejection tests, normalizer key
   tests (all three absent-forms), the matcher's candidate-rule table as a pure
   unit test if the Go filter is extracted, the source-scan structural test, the
   reconciler's env-override test.
2. `make integration` — `db-up` + `migrate` + `go test -tags integration -p 1
   -count=1 ./...`. Serialized `-p 1` is mandatory (IK: integration suites
   cross-pollute); the fixture cleanups must cover both key shapes.
3. `git grep -n "upwork_crm:" -- '*.go'` and read every hit: after this ticket the
   only non-test string constructions of an upwork key must be in
   `threadkey.go`.
4. Re-read the `confirmUpworkDelivery` comment and the commit message against
   criterion 20 before committing.

Manual smoke, against the real `ops` db (read-only first):

5. Baseline, before the re-key — these are the numbers Q2 measured, so a
   disagreement means something changed since 2026-08-26 and the re-key waits:
   ```sql
   SELECT count(*) FROM normalized_threads WHERE thread_key LIKE 'upwork_crm:%';   -- 26
   SELECT count(*) FROM normalized_messages m JOIN normalized_threads t ON t.id=m.thread_id
    WHERE t.thread_key LIKE 'upwork_crm:%';                                        -- 2441
   SELECT count(*) FILTER (WHERE r.raw_json ? 'upwork_room_id')            AS has_key,   -- 1626
          count(*) FILTER (WHERE r.raw_json->>'upwork_room_id' IS NOT NULL) AS roomed,   -- 296
          count(*)                                                                        -- 2441
     FROM raw_source_items r JOIN source_accounts a ON a.id=r.source_account_id
    WHERE a.provider='upwork_crm' AND r.external_id LIKE 'communications:%';
   ```
6. Re-key: `UPWORK_CRM_DATABASE_URL=... DATABASE_URL="$OPS_DATABASE_URL" go run
   ./cmd/connectors/upworkcrm --full --all` (build the DSN with `--from-file`
   semantics; IK's `&` landmine).
7. After:
   ```sql
   SELECT thread_key, count(*) FROM normalized_threads t
     JOIN normalized_messages m ON m.thread_id=t.id
    WHERE t.thread_key LIKE 'upwork_crm:%' GROUP BY 1 ORDER BY 1;
   ```
   Expect: **total still 2,441**; exactly **296** on `:room:` keys; the remaining
   2,145 on the unchanged legacy keys; two distinct roomed threads for
   `e2ef9b65-9813-4d79-ac10-0e1813f788ff`; and the 26 legacy threads still
   present. "Most rows did not move" is the designed outcome (criterion 22).
8. Idempotency: run step 6 again; thread and message counts unchanged, and
   `sync_runs.stats` shows `raw_unchanged` covering the corpus.
9. Deliveries are untouched: `SELECT count(*) FROM deliveries WHERE
   channel='upwork_chat';` — still 0, before and after.
10. **Deploy precondition:** the in-cluster `connector-upworkcrm` CronJob runs a
    pinned tag (currently `0.4.1`). The fix reaches production only after an image
    build and a tag bump in the kube repo, which is the kube session's work. There
    is no migration this time, so the "merging is not applying" hazard is only
    about the image.

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
  thread (criterion 14). If SWT-19 goes first, SWT-17 simply deletes a slightly
  different block — but it must then preserve the roomed-preference property in
  its `external_refs`-based resolution, and it now also inherits §3's fragmented
  context problem. **Add both as lines in SWT-17 when it is implemented.** If
  SWT-17 goes first, criteria 14 and 3 have to be re-expressed against code that
  does not exist yet, which is the worse order.

They do not conflict on migrations, because SWT-19 has none.

## Decisions made unilaterally

1. **The unroomed key is byte-identical to today's key**, rather than a new
   `:noroom` spelling. Rationale: 2,145 of 2,441 messages need no re-key, no
   migration and no cleanup; the 26 existing thread rows keep their ids and FK
   edges; and the existing tests that assert the current key remain correct
   assertions about a case that not only still exists but keeps growing (fact 4).
2. **The roomed key carries a `room:` tag** rather than putting the room id in the
   channel position. It makes roomed-ness a parse instead of a comparison against
   a magic literal — the SWT-13 lesson — and removes the (small) collision class
   where a channel value equals a room id.
3. **No inference of a room from `clients.upwork_room_id` or from sibling
   messages.** Argued in §3: it is unfalsifiable where it is right and silently
   merges two conversations where it is wrong, on the one client this ticket
   exists for. Q1 raises the stakes — with 55% of outbound room-less, an inference
   rule would be firing constantly.
4. **The matcher's client/room scoping moves into Go**, leaving the SQL with
   channel/status/NULL guards only. Forced by "any roomed key of this client"
   being inexpressible as an equality, and by §2's one-spelling rule; the
   candidate set is bounded by unconfirmed assisted deliveries (0 today), and its
   growth is itself the alarm.
5. **The reconciler ships in this ticket** rather than as future work (§6). Pass
   default 6, because upwork writes two `sync_runs` per invocation.
6. **`draft_delivery` accepts unroomed targets.** Refusing them would make most
   drafts impossible given fact 4, and the mismatch-only rule is what pays for
   accepting them.
7. **SWT-18's `RoomDiscrimination` test is re-based, not kept.** It currently
   proves room scoping using channel values (`chat`, `room-b`) the source has
   never emitted — a passing test of nothing, which is precisely the finding this
   ticket comes from. Leaving it green beside a real one would preserve the
   confusion.
8. **Criterion 20 makes naming a review item.** It cannot be mechanized, it is the
   exact mistake this ticket was created to correct, and a SPEC that does not ask
   for it will get the same commit message SWT-18 wrote.

## Open questions

**None outstanding.** Both are answered with data in
`docs/tickets/upwork-room-identity_OPEN_QUESTIONS.md` (Q1 = (b), Q2 = (a)) and
folded in above. Q1's sub-question — *why* outbound rows lack the room id — is
non-blocking and sits in Future work.

## Future work (not this ticket)

- **Find out why outbound rows lack `upwork_room_id`** while inbound rows carry it
  99.5% of the time (Q1's sub-question). The likely culprit is the CRM's own
  send/observe path writing communications rows without the room, in
  `~/WebstormProjects/crm` or the upwork API connector — **worth one look before
  building this ticket**, because if it is a one-line fix at the source then the
  outbound half becomes roomed, criterion 10's looseness stops mattering, and the
  matcher genuinely becomes room-scoped. It does not block: §4's rule is correct
  either way and simply tightens on its own as the data improves. It is NOT in
  scope here because the fix would live in another repo.
- **Re-examine unconfirmed deliveries against already-normalized messages**
  (fact 14's gap): a bounded sweep, in the reconciler's spirit, that re-runs the
  prefix match for `status='sent'` rows whose `sent_at` postdates the messages in
  their room. Flag-only until it is proven, then promote.
- **Un-fragment the draft context** (§3): assemble upwork thread context across
  the client's roomed and unroomed threads rather than the single targeted one.
  Belongs with SWT-17's drafts rework, which owns that code.
- **`outbound_observed` for `upwork_chat`** — SWT-16's own deferred item, now more
  tractable because `external_id` is a real story id and the thread is a real
  room.
- **Room-level `external_refs`** (`system='upwork_crm'`, `external_key` = the
  roomed thread key) so a task can point at a conversation without a person
  lookup — the honest replacement for the client-level target SWT-17 removes.
- **`upwork_room` identities per message room**, and reconciling them against
  `clients.upwork_room_id`; a disagreement there is a suspected merge worth
  surfacing.
- **Promote `upwork_chat` off the assisted tier** when the CRM's flow 3 (GraphQL
  `sendMessage`) ships: an API send returns a story id, `sent_external_id` is set
  at send time, and the post-hoc matcher becomes redundant safety net. The room
  keying landed here is what makes that transition graceful —
  `docs/GRAPHQL_NOTES.md` records that the URL room id equals the API room id.
