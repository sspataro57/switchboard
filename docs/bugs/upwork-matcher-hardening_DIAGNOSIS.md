> Jira: SWT-18

# Diagnosis — upwork-matcher-hardening

Reproduction re-run at `526233b` before any conclusion below:

```
--- FAIL: TestUpwork_Repro_SWT18_Defect1_RawTextComparison (0.03s)
    sent_external_id is NULL ... confirmed_at is NULL ... delivery_confirmed task_events = 0, want 1
--- FAIL: TestUpwork_Repro_SWT18_Defect2_NoAttemptTimeFloor (0.02s)
    delivery 21 (room-b, sent 13:00 — 3h AFTER the message at 10:00) was stamped
    sent_external_id="upwork-room-msg-itest-umh-202"
    delivery 20 (room chat, the one that produced the message) has sent_external_id NULL
```

## Root cause

`(*PGSink).confirmUpworkDelivery` (`internal/connector/upworkcrm/sink.go:285-322`)
selects the delivery to stamp with a single `UPDATE ... WHERE id = (SELECT ...)`
whose candidate predicate is `left(body, 120) = left($4, 120)` over
`target_ref LIKE 'upwork_crm:{client}:%'`, ordered `sent_at DESC LIMIT 1`. Two
independent defects live in that one subquery.

**Defect 1** is that the comparison is a *raw byte/character* comparison in SQL.
The stored body is what `draft_delivery` persisted; `$4` is what the Upwork UI
handed back after a human copy/paste and a browser round trip. Any whitespace
the round trip alters — an NBSP where a space was typed, a collapsed blank-line
run, a stripped trailing space — makes the two 120-character windows unequal
while the message is identical, and the match then fails *permanently*, because
nothing ever retries a comparison that is already exact. The correct spelling is
the one the other three matchers use: normalize both sides in Go with
`textmatch.NormalizedPrefix(body, 120)` and compare the results
(`google/sink.go:543`, `jira/sink.go:285`, `slackweb/sink.go:304` via its local
alias at that line). It cannot be re-spelled in SQL — `internal/textmatch`'s own
package doc records why (POSIX `\s` does not cover the unicode spaces Go's
`strings.Fields` does), and the repro's fixture is built on exactly that NBSP.
Restructuring is therefore forced: the single UPDATE must become
select-candidates → filter in Go → guarded stamp, which is precisely what SWT-16
did to jira (`jira/sink.go:284-340`).

**Defect 2** is that there is no lower time bound on the candidate at all. The
scope is client-wide (`LIKE 'upwork_crm:{client}:%'` deliberately spans every
room that client has) and the tiebreak is `sent_at DESC` — newest wins. A
delivery sent *after* a message already existed therefore outranks the delivery
that actually produced it. Content matching alone cannot tell two identical
sends apart; the siblings all carry a floor (`jira/sink.go:319`,
`slackweb/sink.go:235`, `google/sink.go:562-563`). **But the sibling spelling
must not be copied verbatim here** — see "The floor's spelling" below, which is
the single most important finding in this diagnosis.

## Evidence

- `internal/connector/upworkcrm/sink.go:285-322` — the matcher, quoted above.
  Verified by reading, not from the report.
- `internal/connector/upworkcrm/sink.go:294-297` — a dead local:
  ```go
  prefix := nm.BodyText
  if len(prefix) > upworkMatchPrefixLen {
      prefix = prefix[:upworkMatchPrefixLen]
  }
  ```
  `prefix` is never passed to the query (the args at `:311` are
  `nm.ExternalMessageID, clientPrefix, upworkMatchPrefixLen, nm.BodyText`). It
  compiles because `len(prefix)` counts as a read. It is also byte-truncating,
  which would cut a multi-byte rune in half if it were ever used. This is the
  fossil of an abandoned Go-side prefix — evidence the author started down the
  right path and replaced it with SQL `left()`.
- `internal/textmatch/prefix.go:1-16` — the package doc explicitly forbids the
  SQL spelling and names NBSP as the reason. The repro's fixture uses NBSP.
- `internal/tools/delivery.go:633` — `mark_delivery_sent` writes
  `status='sent', sent_at=now(), updated_at=now()`. It does **not** write
  `send_attempted_at`.
- `internal/policy/matrix.go:120-125` — `send_delivery` on `upwork_chat` is
  denied (`channel_assisted`); only `mark_delivery_sent` is allowed. So the two
  code paths that *do* write `send_attempted_at` (`delivery.go:513` gmail,
  `delivery.go:941` slack) are unreachable for `upwork_chat`.
- `internal/tools/delivery.go:141` — `prefill_delivery` refuses anything that is
  not `slack_reply`, so there is no upwork prefill that could stamp an attempt
  instant either.
- `migrations/0012_slack_send_attempts.sql:36-40` — the one-time backfill
  `SET send_attempted_at = sent_at WHERE sent_at IS NOT NULL`. Any `upwork_chat`
  row marked sent *after* 0012 was applied has `send_attempted_at` NULL forever.

### git history — was this a call site missed, or a spec never implemented?

`git log -L285,322:internal/connector/upworkcrm/sink.go` returns exactly one
commit: **`64e4545`, 2026-07-11, "08-draft-deliveries" (SWT-8)**. The matcher was
born there and has not been touched since. It is the **oldest of the four**
matchers — it predates slackweb's (SWT-13, 2026-07-26), google's
(`680875f`, SWT-11, 2026-07-31) and `internal/textmatch` itself (SWT-16,
2026-07-31).

That reframes defect 1 completely. `docs/tickets/08-draft-deliveries_SPEC.md`,
the spec this code shipped against, says at line 82:

> whitespace-normalized body prefix (first 120 chars) matches the delivery body

and again at line 393, as decision 8:

> **Upwork confirmation = 120-char whitespace-normalized body-prefix match**

**The SPEC asked for whitespace normalization and the implementation did not do
it.** Defect 1 is not a convention that arrived later and upwork failed to pick
up — it is the original implementation failing to implement its own written
requirement, on day one, five weeks before the rule was named as a landmine.

It survived because the test shipped alongside it made the distinction
invisible: `loopclosure_integration_test.go:42` defines one constant `uclBody`
and seeds it on **both** sides of the comparison, byte-identical. A raw
comparison and a normalized comparison pass that fixture identically.

Defect 2 has a different history. `send_attempted_at` did not exist until
migration 0012 (SWT-12, 2026-07-29), eighteen days after this matcher shipped,
so the floor could not have been written here originally.

Was upwork *considered and skipped* later? Partly, and the record is precise.
`docs/tickets/outbound-capture_SPEC.md:403-404` lists as prior art to read:

> `internal/connector/upworkcrm/sink.go` `confirmUpworkDelivery` — thread_key
> prefix scoping (and the reason upwork is out of scope).

and lines 289-293 / 451-452 exclude `upwork_chat` from **capture** on structural
grounds (nullable CRM `external_id`, source-defined channel values). `f7a93e4`'s
commit message closes with "Deferred: upwork_chat and GitHub capture." So SWT-16
opened this file, read this function, and made an explicit, reasoned decision —
about *capture coverage*. It never asked whether the matcher it was reading had
the defect it was, in that same commit, fixing in jira. The exclusion note
created the impression upwork had been dealt with.

**Conclusion for scope:** this is not "one more call site." Defect 1 is a
five-week-old spec/implementation divergence that a same-body test hid and that a
subsequent reader mistook for "already considered." Defect 2 is a convention that
was never mechanically checkable. Both point at the same gap: nothing in the repo
can tell you a matcher lacks the rule. That is argued in "The structural
question" below.

## The floor's spelling — why the obvious fix would be a no-op

This is the finding that changes the fix, and it contradicts the shape the repro
implies.

The repro's defect-2 fixture seeds `send_attempted_at` explicitly:

```go
INSERT INTO deliveries (..., sent_at, send_attempted_at) VALUES (...,$4,$4)
```

**Production `upwork_chat` rows never have that column set.** `send_delivery` is
policy-denied for the channel (`matrix.go:120-125`), `prefill_delivery` refuses
the channel (`delivery.go:141`), and `mark_delivery_sent` — the *only* verb that
moves an upwork row to `sent` — does not write it (`delivery.go:633`). Only
0012's one-shot backfill ever populated it, for rows already sent at that
instant.

Therefore, dropping in the sibling clause verbatim —

```sql
AND (send_attempted_at IS NULL OR send_attempted_at - interval '2 minutes' <= $2)
```

— would make the repro's defect-2 test go green and would change **nothing** in
production, because every real row takes the `send_attempted_at IS NULL` branch
and passes. It is a no-op dressed as a fix. (The repro is not wrong; its fixture
just seeds a column production doesn't. The mutation proof it contains proves the
clause selects correctly *on that fixture*, which is what it claims and no more.)

slackweb's own code says this out loud, at `slackweb/sink.go:210-219`:

> The floor applies only where the instant is actually known. A switchboard
> dispatch records `send_attempted_at` ... An assisted row has no attempt
> instant ... Those rows keep the SWT-13 behaviour, so the historical-collision
> risk remains **for the assisted tier alone**.

slackweb could accept that residue because its assisted rows are the minority
path. `upwork_chat` is assisted **in its entirety** — there is no non-assisted
upwork row, ever.

The only instant an upwork row actually carries is `sent_at`, written by
`mark_delivery_sent`. google is the sibling whose spelling generalizes:
`COALESCE(d.send_attempted_at, d.sent_at)` (`google/sink.go:562-563`). That is
the shape to adopt.

**But the two-minute allowance is wrong for this channel, in the opposite
direction from the usual concern, and this must be resolved before the fix is
written.** For gmail/jira/slack the allowance absorbs clock skew between Postgres
`now()` and the provider clock — seconds. For upwork, `sent_at` is the instant a
*human* clicked "mark sent" on the dashboard, which is legitimately minutes or
hours after they actually pasted the message into the Upwork UI. The provider's
`communicated_at` is the real send instant. So `sent_at` is systematically
*later* than the message, by a human-shaped interval, and a 2-minute floor would
turn every "sent it, marked it later" case into a **permanent refusal** — the
exact failure mode the IK entry warns the floor must not create, reintroduced by
the fix meant to prevent it. On the repro's fixture it happens to work only
because the fixture sets `sent_at == message sent_at`.

I am not going to guess the right constant. Options and their costs are in "Open
questions"; the fix must pick one deliberately, and the regression test must pin
the human-latency case (mark-sent recorded well after the message) alongside the
wrong-room case.

## Why the reproduction fails

**Defect 1.** The stored body carries U+00A0 at character 81, a trailing double
space and a three-newline run; the observed body carries a plain space, no
trailing spaces and one blank line. `left(body,120)` in Postgres counts
characters, so the two windows are correctly *aligned* — they simply are not
equal, byte for byte, because of the NBSP. The subquery at `sink.go:302` returns
no row, `QueryRow(...).Scan` yields `pgx.ErrNoRows`, and `:313-315` swallows that
as "no match" and returns nil. Nothing is stamped, no event is written, and
because the next poll will run the same exact comparison against the same two
unchanged strings, the row is unclaimable forever. Observed: `sent_external_id`
NULL, `confirmed_at` NULL, `delivery_confirmed` count 0.

**Defect 2.** Both deliveries are `upwork_chat`, `status='sent'`,
`sent_external_id IS NULL`, `confirmed_at IS NULL`, both match
`target_ref LIKE 'upwork_crm:{client}:%'` (the LIKE spans rooms `chat` and
`room-b` alike), and both carry byte-identical bodies so both survive the raw
comparison. Nothing else discriminates, so `ORDER BY sent_at DESC LIMIT 1` picks
the 13:00 row over the 10:00 row. Delivery 21 is stamped with the id of a message
that existed at 10:00. Delivery 20 — the one that produced it — is then locked
out for good by the partial unique index
`deliveries_sent_external_idx (channel, sent_external_id) WHERE sent_external_id
IS NOT NULL`: the id it needs is already taken.

Note the two defects compound in the direction that matters: **fixing defect 1
alone makes defect 2 more likely to fire**, because whitespace normalization
widens the set of bodies that compare equal, which widens the candidate set the
unguarded `ORDER BY sent_at DESC` chooses from. They must ship together.

## Invariant implicated

**Invariant 5 — own-message loop closure**, directly. "our sends re-enter via
ingestion; normalizer matches by external id to the delivery row and attaches as
task log." Defect 1 is that match silently failing; defect 2 is it succeeding
against the wrong row. Invariant 4's idempotency (`sent_external_id` set once,
never resend while present) is implicated as collateral: on a wrong bind, a real
send's id is recorded against a row that did not produce it and the correct row's
id is lost.

The fix must restore the gate, not patch the symptom: the matcher must identify
our own message the one way the repo has settled on (normalized prefix) and must
carry a lower time bound that is *real for this channel*, rather than a clause
that is syntactically present and semantically inert.

## Proposed fix scope

**ALL ITEMS DONE 2026-08-26** — see "Resolution" at the end of this file.

- [x] `internal/connector/upworkcrm/sink.go:285-322` — restructure
      `confirmUpworkDelivery` into the SWT-16 jira shape: `BEGIN`; select
      candidates with `SELECT id, task_id, COALESCE(body,'')` (body is nullable
      per 0001 — scanning NULL into a string errors and would fail the whole
      normalize run) `... FOR UPDATE`; filter in Go with
      `textmatch.NormalizedPrefix(body, upworkMatchPrefixLen) == want`; stamp
      inside the same transaction with `sent_external_id IS NULL AND confirmed_at
      IS NULL` restated on the UPDATE and a RowsAffected check before emitting
      `delivery_confirmed`. The current outer clause is a bare
      `WHERE id = (subquery)` with no guards — tightening it is not a new
      exposure.
- [x] Same function — add the empty-prefix refusal the other three carry:
      `if want == "" { return nil }`. Today's guard (`nm.BodyText == ""`) misses
      a whitespace-only body, which after normalization would match any candidate
      that also normalizes to empty, claiming a delivery on no evidence.
- [x] Same function — delete the dead byte-truncating `prefix` local
      (`sink.go:294-297`).
- [x] Same function — **replace the client-wide scope with exact room matching:**
      `target_ref = $2` where `$2` is the message's `thread_key`, instead of
      `target_ref LIKE 'upwork_crm:{client}:%'`. **NO time bound is added.**
      This supersedes the "add a lower time bound" item that stood here before —
      see "Superseded: the era assumption" below. In short: a clock bound cannot
      work on the assisted tier (`send_attempted_at` is always NULL; `sent_at` is
      when a human clicked, unboundedly later), and room identity resolves the
      defect-2 fixture on its own with no clock reasoning at all.
- [x] Decide multi-match policy explicitly rather than inheriting
      `ORDER BY sent_at DESC LIMIT 1` by accident. google and slackweb refuse on
      ambiguity; jira takes the newest and documents that as a deliberate
      carry-over. Upwork's client-wide room scope plus reusable status-line
      templates makes ambiguity *more* likely here than anywhere else, which
      argues for refusing — but that changes which deliveries get confirmed and
      needs its own line of reasoning in the fix, not a silent choice.
- [x] Regression tests (test-author converts the repro): the two repro cases,
      **plus** a third that the repro does not cover and that guards the fix
      against its own new failure mode — a delivery whose `send_attempted_at` is
      NULL and whose `sent_at` is well after the message (the realistic
      mark-sent-later case) must still confirm. Under exact-room matching this
      case passes by construction, which is precisely the argument for it: the
      fix has no clock-shaped failure mode to guard.
- [x] Regression test for the room discrimination itself: two deliveries to the
      same client, same body, different rooms — the one whose `target_ref`
      equals the message's `thread_key` confirms; the other is untouched
      regardless of `sent_at` ordering.
- [x] `.claude/INSTITUTIONAL_KNOWLEDGE.md` — extend the two existing landmine
      entries to name all four call sites, and record the assisted-tier
      no-op trap (drafted below, and I have added it — see "Landmine matched").

## Superseded: the era assumption behind this matcher

**Added 2026-08-20 after Salvador supplied the missing context**, which changes
the fix rather than merely shaping it:

> this design is from when we were scraping upwork messages using an extension.
> we now have api access so it's streamlined now

Verified against `~/projects/personal/upwork/upworkApiConnector`:

- **Ingestion is API-based and LIVE** (flow 2: GraphQL message sync, per-room
  cursors). Confirmed in the data — `upwork_crm.communications.external_id` is
  now `story_<hex>`, real Upwork story ids, after the 2026-07-14 backfill that
  "replaced all CRM history with story-id ground truth".
- **Sending is NOT.** The connector lists flow 3 (reply send) under *Next*; its
  design is a planned MQTT topic `crm/prospects/send` → GraphQL `sendMessage`;
  and its open questions still list **"sendMessage mutation availability"** as
  unresolved — the granted scope may not permit it. The CRM has an outbox
  (`is_draft`, `send_requested_at`, `send_room_id`; 133 rows with a send
  requested), but sends do not go out over the API yet.

`confirmUpworkDelivery` exists because in the scraping era there was no way to
learn the id of a message we had just sent, so it guessed by matching body text
after the fact. **That premise is already half dead and will be fully dead when
flow 3 lands**: an API send returns a story id, so `sent_external_id` is set at
send time and invariant 4 is satisfied directly, with no matching. The matcher
becomes a redundant safety net.

Consequences for this fix, and they are the reason the scope above changed:

1. **Do not invest in assisted-tier machinery.** Keep the change minimal.
2. **Exact-room matching survives the transition; a clock heuristic does not.**
   Room identity is exactly what the API hands over — `docs/GRAPHQL_NOTES.md`
   records that the URL room id equals the API room id, and the CRM already
   stores `send_room_id` / `upwork_room_id`. When flow 3 ships, an exact-room
   matcher degrades gracefully into redundancy. A `sent_at`-based allowance
   would simply be wrong.
3. **Open question 1 is CLOSED** by this: option (c). Open question 3
   (multi-match policy) is materially reduced — exact-room scope removes the
   main source of ambiguity, since two deliveries to the same client in the same
   room with the same 120 normalized characters is a far narrower case than
   across all of that client's rooms.
4. **The structural test recommendation is narrowed.** The normalization scan
   still earns its place. Do NOT mechanize the floor: it is spelled three ways
   already, Upwork is now moving to exact-room matching rather than a fourth
   spelling, and the rule is becoming obsolete. IK convention is the right home.

### Related staleness, NOT in this bug's scope

`internal/policy/matrix.go:120-125` hard-codes Upwork as assisted — it denies
`send_delivery` and allows only `mark_delivery_sent`. That is correct today and
becomes wrong the day flow 3 ships. Recorded here so it is found deliberately
rather than by a delivery that cannot go out. Autonomy is earned per category
(CLAUDE.md), so promoting it is a manual decision either way.

## The structural question — recommendation

**Recommendation: do not build a shared production helper. Do add one
source-scanning structural test for the normalization half, and name all four
call sites in the IK entries. Accept that the floor half stays convention.**

Reasoning, in order.

*Against a shared helper.* The four matchers agree on exactly one thing — "compare
the normalized 120-char prefix" — and that is already extracted into
`internal/textmatch`. Everything else genuinely differs: the join key
(`thread_id` for gmail, `target_ref=` for jira, `target_ref = ANY(...)` for slack,
`target_ref LIKE` for upwork), the status set (`('sending','sent','failed')` vs
`('sending','sent')` vs `('sent')`), the floor column (`send_attempted_at`, or
`COALESCE` with `sent_at`, or — for upwork — a column no code path writes), the
multi-match policy (refuse vs newest-wins), and whether the stamp overwrites
`sent_external_id` (gmail must never). A helper covering that needs six knobs,
and the per-channel *reasoning* — which is currently 20-line comments that are the
best documentation in this repo — would migrate into knob arguments and stop
being legible. That is a worse codebase, not a safer one.

*For a structural test, specifically.* The repo already accepts this idiom: IK
records that triage's shadow mode is enforced by a reflection test
("`triage.Store` has no task-write method (reflection test enforces)"). The same
move works here and is mechanical: walk `internal/connector/*/sink.go`, and for
any file that stamps `sent_external_id=` from observed content, assert the same
file references `textmatch.NormalizedPrefix`. It is roughly twenty lines, needs no
database, and **it would have failed on 2026-07-31, the day SWT-16 landed** —
which is the whole test of whether an enforcement mechanism is worth its weight.
It also catches the more likely future mistake (a fifth connector) rather than
the one that already happened.

*Why the floor half stays convention.* A source scan cannot check it honestly.
The correct floor is spelled three different ways across three connectors, and
this bug proves a *fourth* is needed, so "mentions `send_attempted_at`" would have
passed the very code that is broken — worse than no check, because it would
certify a no-op. That half belongs in the IK entry, naming all four sites
explicitly, so the next reader of any one of them sees the other three.

*Cost check.* One meta-test plus two IK edits, against four call sites. Not
over-engineering: the marginal cost is one test file, and the demonstrated cost of
the missing check is this ticket.

## Correction to the bug report — the `outbound_observed` consequence does not apply

The report's Consequence section states:

> Since SWT-16, capture sees an outbound message no delivery claims and logs
> `outbound_observed` — a false record that switchboard's own message was sent by
> hand.

**That is wrong for upwork, and it should be struck from the report rather than
softened.** Traced at the site:

- `internal/capture/observe.go:86-99` defines exactly three `Channel` values:
  `Gmail`, `Jira`, `Slack`. There is no upwork channel, and `ObserveOutbound`
  refuses an unconfigured one (`observe.go:155-157`,
  `"capture: channel is not configured"`).
- `grep -rn upwork internal/capture/` returns **zero** matches.
- `capture.ObserveOutbound` is called from `cmd/connectors/jira/main.go:79`,
  `cmd/connectors/slackweb/main.go:77`, `cmd/connectors/google/main.go:145` and
  `cmd/connectors/google/watch.go:138`. `cmd/connectors/upworkcrm/main.go` does
  not call it — it runs `upworkcrm.Normalize` at `:67` and stops.
- The exclusion was deliberate and is on the record:
  `docs/tickets/outbound-capture_SPEC.md:289-293` and `:451-452`, plus f7a93e4's
  "Deferred: upwork_chat and GitHub capture."

So no `outbound_observed` event can be written for an upwork message today, by
any path. The repro's observation (0 such events, and the test asserts 0) is not
an artifact of running `Normalize` in isolation — it is the correct production
behavior. The report's inference from the SWT-16 IK entry was reasonable and does
not survive contact with the code; the IK sentence is scoped to jira, where
capture *is* wired.

The consequence becomes real the day upwork capture ships — it is listed in
SWT-16's own Future work (`outbound-capture_SPEC.md:463`, "upwork_chat capture
once the CRM guarantees non-null external ids"). Worth recording as a
precondition on that future ticket; not a reason to widen this one.

The report's **first** consequence stands unchanged and is the whole cost today:
`sent_external_id` NULL forever, the row permanently unclaimable, and — on a
wrong bind — a real send recorded against the wrong message with the correct row
locked out by the partial unique index.

## Blast radius — queries to run (I did not connect to production)

Per instruction I did not touch pg-main. Run these as
`psql -h 192.168.50.49 -U ops -d ops`. Read them in order; **Q0 may make the rest
moot.**

**Q0 — is the matcher even reachable?** `confirmUpworkDelivery` returns
immediately when `nm.ExternalMessageID == ""` (`sink.go:287`), and SWT-16 recorded
that the CRM's `external_id` is nullable. Against the *source* db
(`psql -h 192.168.50.49 -U ops -d upwork_crm` — note the separate `~/.pgpass`
line):

```sql
SELECT direction, count(*) AS n, count(external_id) AS with_external_id
  FROM communications GROUP BY direction;
```

*If `with_external_id` is 0 for outbound:* the matcher has never run in
production. Zero live damage, zero data repair; this is a purely latent defect
that activates the day the CRM starts populating the column. *If it is non-zero:*
continue.

**Q1 — total upwork delivery surface.**

```sql
SELECT status, count(*) AS n, count(sent_external_id) AS with_ext,
       count(confirmed_at) AS confirmed
  FROM deliveries WHERE channel='upwork_chat' GROUP BY status ORDER BY status;
```

*Zero rows:* no live damage at all; scope is fix + tests, no repair step.
*Rows with `status='sent'` and `with_ext` < `n`:* candidates for Q2.

**Q2 — already-lost confirmations (defect 1's signature).**

```sql
SELECT id, task_id, target_ref, sent_at, send_attempted_at, confirmed_at,
       left(body, 60) AS body_head
  FROM deliveries
 WHERE channel='upwork_chat' AND status='sent' AND sent_external_id IS NULL
 ORDER BY sent_at;
```

*Interpretation:* a row sent in the last 15 minutes is normal — the connector
CronJob runs `*/15`, so it simply has not been polled yet. Anything older than an
hour whose outbound communication demonstrably exists in the CRM is a lost
confirmation. Expect `send_attempted_at` to be NULL on every row; if it is not,
the row predates migration 0012 and was backfilled, and I want to know that before
the floor is written.

**Q3 — wrong binds, unambiguous form (defect 2's signature).** A delivery stamped
with a message that is in a *different room* than the delivery targeted:

```sql
SELECT d.id, d.target_ref, d.sent_external_id, d.sent_at,
       t.thread_key, m.sent_at AS message_sent_at
  FROM deliveries d
  JOIN normalized_messages m ON m.external_message_id = d.sent_external_id
                            AND m.channel <> 'gmail'
  JOIN normalized_threads t ON t.id = m.thread_id
 WHERE d.channel='upwork_chat' AND d.sent_external_id IS NOT NULL
   AND t.thread_key <> d.target_ref;
```

*Any row returned is a confirmed wrong bind* and needs manual repair: clear
`sent_external_id`/`confirmed_at` on the wrongly-stamped row so the correct one
can claim the id on the next pass. Empty result: defect 2 has not fired.

**Q4 — the defect-2 precondition, to size future risk.** Pairs of upwork
deliveries to the same client sharing an opening:

```sql
SELECT split_part(target_ref, ':', 2) AS client,
       left(regexp_replace(COALESCE(body,''), '\s+', ' ', 'g'), 120) AS head,
       count(*) AS n, array_agg(id) AS ids
  FROM deliveries WHERE channel='upwork_chat'
 GROUP BY 1, 2 HAVING count(*) > 1;
```

*Caveat, and it is the SWT-16 caveat:* Postgres POSIX `\s` is **not** Go's
whitespace, so this grouping is a triage approximation only. Never take it as
equivalent to `textmatch.NormalizedPrefix`, and never let this spelling near the
matcher itself.

*Note on Q3's omitted second predicate:* I deliberately did **not** include a
`COALESCE(send_attempted_at, sent_at) > m.sent_at` clause. For upwork, `sent_at`
is the mark-sent instant and is legitimately hours after the message, so that
predicate produces false positives on every normal row — which is the same reason
the floor's constant is an open question rather than a copy.

## Open questions

**Q1 CLOSED, Q2 CLOSED, Q3 reduced — 2026-08-20.** See "Superseded: the era
assumption" above. Q1 is answered by option (c): exact room matching, no time
bound at all. Q2 (is production `target_ref` equal to the normalizer's
`thread_key`?) is answered from the code rather than the data — production has
ZERO `upwork_chat` deliveries, so there is nothing to sample, and the only
in-repo producer is `internal/drafts/store.go:137`, which assigns the
`thread_key` verbatim from `normalized_threads`. Two caveats recorded for the
implementer: `draft_delivery` validates only that an upwork `target_ref` is
non-empty (`delivery.go:107-108`) with no canonicalization, unlike `slack_reply`
which parses — so an agent over MCP could write a different spelling; and
`drafts/store.go:131`'s no-thread fallback writes a synthetic
`upwork_crm:{uuid}:upwork` that is NOT a real `thread_key` — which SWT-17
removes (its OPEN_QUESTIONS Q5, answered "let it die"). Exact-room matching and
that removal are mutually reinforcing.

The original text of Q1 is preserved below for the reasoning trail; it is no
longer a decision to make.

1. ~~**What lower bound is correct for the assisted tier, and with what
   allowance?**~~ (CLOSED — option (c), no bound.) Established: `send_attempted_at` is always NULL for
   `upwork_chat`, so the bound must come from `sent_at`, and `sent_at` is
   systematically *later* than the message by an unbounded human interval. Not
   established: which of these is right — (a) `COALESCE(send_attempted_at,
   sent_at)` with a generous allowance (hours) chosen from observed
   mark-sent latency; (b) an upper bound instead, refusing a message *newer* than
   the delivery's `sent_at` by more than a window, which is the direction the
   asymmetry actually points; (c) making the client-wide `target_ref LIKE` an
   exact `target_ref = thread_key` match, which resolves the repro's defect-2
   fixture on room identity alone and needs no clock reasoning. (c) is
   attractive and may be the real fix, but I have not established why the LIKE
   was chosen — `draft_delivery` takes `target_ref` from the caller for
   `upwork_chat` (`delivery.go:107-108`) with no canonicalization, unlike
   slackweb (SWT-13 landmine), so exact matching may have been avoided on purpose.
   Salvador's own report ("verifies it's posting to the correct thread by
   matching a previous message... when somebody has several chat rooms") suggests
   room identity is meaningful to him and worth asking about directly.
2. **Is `target_ref` for upwork deliveries in production actually equal to the
   normalizer's `thread_key`** (`upwork_crm:{client}:{channel}`,
   `normalize.go:99`)? Q3 above answers it. If callers have been writing a
   different spelling, option (c) is off the table and the SWT-13 landmine has a
   fourth instance.
3. **Multi-match: refuse or newest-wins?** Listed in scope as a decision to make,
   not a defect I have proven. I have not established how often two upwork
   deliveries to one client share 120 normalized characters; Q4 sizes it.

## Risk assessment

- **Restructuring the single UPDATE into select-then-stamp** widens the window
  between selection and stamp. Mitigated the way jira mitigated it: one
  transaction, `FOR UPDATE` on the candidates, guards restated on the UPDATE.
  It cannot deadlock against an in-flight upwork send because there is no
  automated upwork send path — `send_delivery` is policy-denied for the channel.
- **Normalization widens the candidate set.** More bodies will now compare equal.
  This is the intended fix for defect 1 and it strictly increases defect 2's
  exposure; that is why they cannot be split across two changes.
- **The floor is the risky half.** A bound that is too tight converts a silent
  miss into a *different* silent miss — a permanent refusal — and looks like a
  fix in the repro. This is the same failure shape as SWT-13's non-canonical
  `target_ref`: no error anywhere. The regression suite must pin the
  mark-sent-later case or the fix is not verified.
- **Shared code touched:** `internal/textmatch` gains a fourth caller and is not
  modified. Nothing else in `upworkcrm/sink.go` is on this path;
  `upsertMessage:274-279` calls the matcher only for `direction == "outbound"`,
  and triage's inbound-only filter is unaffected.
- **Cross-suite pollution:** the upworkcrm integration suite is in the mutual
  cleanup pact (IK "integration suites cross-pollute"); the regression tests must
  join it, as the repro already does.
- **Deploy precondition:** the in-cluster `connector-upworkcrm` CronJob runs a
  *pinned older image*, so a fix only takes effect after an image build and a tag
  bump in the kube repo — and IK's "merging a migration is not applying it"
  lesson applies to code too. No migration is needed for this fix.

## Landmine matched

Both existing entries: **"Exact text comparison across a provider round trip"**
(defect 1) and **"A post-hoc matcher without an attempt-time floor binds the
wrong send"** (defect 2). This is the third instance of the first and the third
of the second.

**One NEW landmine, and I have added it to
`.claude/INSTITUTIONAL_KNOWLEDGE.md`** — "The attempt-time floor is INERT on the
assisted tier": `send_attempted_at` is written only by `send_delivery`'s gmail and
slack paths, both of which are unreachable for `upwork_chat` (policy-denied), so
copying the sibling floor clause into an assisted-tier matcher produces a clause
that is always true and a test that goes green while production is unchanged. I
also extended the two existing entries to name all four matcher call sites, so
the next reader of any one of them sees the others.

I updated INSTITUTIONAL_KNOWLEDGE.md.

## Status

**Confirmed cause, both defects. Fix scope settled; ready for `test-author`.**

Fix is defect-1 normalization (`textmatch.NormalizedPrefix` on both sides) plus
exact-room matching (`target_ref = thread_key`), shipped together — normalization
alone widens the candidate set and makes defect 2 more likely to fire. No time
bound, no migration.

**Blast radius: ZERO, measured 2026-08-20 against pg-main** (the queries in the
section above were run by the main session, which has cluster access):

- `deliveries` in the whole production ops db: **1 row**, a `jira_comment` from
  the 2026-07-12 smoke test, with `sent_external_id` set.
- `upwork_chat` deliveries with `status='sent'` and `sent_external_id IS NULL`:
  **0**. There has never been an `upwork_chat` delivery in production.
- Q0's hypothesis (that the matcher might never have run because `external_id`
  is empty) is **wrong**: 1,141 outbound upwork messages carry a non-empty
  `external_message_id`, so `sink.go:287` never short-circuits. The matcher HAS
  executed ~1,141 times in production — it found no candidates every time,
  because the delivery side is empty.

So: a real defect in shipped code, never yet reachable for harm, and it becomes
reachable the moment the draft worker and the Upwork tier go live — which is
also when a silently unclaimable delivery is most expensive, since the failure
is invisible by construction.

---

## Resolution (2026-08-26)

Implemented in `internal/connector/upworkcrm/sink.go`, exactly the scope settled
above and nothing more.

- `confirmUpworkDelivery` restructured into the SWT-16 jira shape: `BEGIN`,
  `SELECT id, task_id, COALESCE(body,'') ... FOR UPDATE`, compare
  `textmatch.NormalizedPrefix` in Go, stamp in the same transaction with the
  `IS NULL` guards restated and `RowsAffected` checked before the
  `delivery_confirmed` event is written.
- Scope narrowed from `target_ref LIKE 'upwork_crm:{client}:%'` to
  `target_ref = nm.ThreadKey`. **No time bound**, per the superseded-era
  reasoning above.
- Empty-prefix refusal added (`want == ""`), replacing the `nm.BodyText == ""`
  guard that a whitespace-only body walked past. Guards also refuse an empty
  `ThreadKey`.
- Dead byte-truncating `prefix` local removed, along with
  `clientIDFromThreadKey`, whose only caller was the `LIKE` scope.
- **Multi-match: REFUSE**, decided rather than inherited (the open item in the
  scope list). google and slackweb refuse; jira documents newest-wins as a
  carry-over. Refusing is the reversible half of the trade — two unconfirmed
  rows can still be confirmed by a later distinct message or resolved by a
  human, whereas one wrong stamp burns the external id under
  `deliveries_sent_external_idx` and locks the correct row out permanently
  (invariant 4).
- **One guard added beyond the listed scope:** a pre-check refusing a message
  whose `external_message_id` some `upwork_chat` delivery already claims. Not
  cosmetic — with two same-prefix rows in one room where the first is already
  confirmed, a replay would try to stamp an id the unique index holds and fail
  the WHOLE normalize run rather than skip one confirmation. slackweb carries
  the same guard for the same reason.

### Verification

Tests were proved red before the fix and green after, on the same fixtures:

```
before:  FAIL WhitespaceNormalizedMatch (sent_external_id NULL)
         FAIL RoomDiscrimination (both orderings — the wrong room was stamped)
         FAIL WhitespaceOnlyBodyClaimsNothing (claimed on no evidence)
after:   ok  5/5, including the two that pass by construction
         (MarkedSentLongAfterTheMessageStillConfirms, AlreadyStampedIsNotRestamped)
```

`internal/textmatch/callsites_test.go` failed naming `upworkcrm/sink.go` before
the fix and passes after. Full suite: `go test ./...` and `make integration`
both green, 22/22 packages, migrations 0001-0014 applied locally.

No migration. No production data touched — blast radius was measured at zero
before the change and the delivery side is still empty.

### Deploy note

The fix takes effect only after an image build and a tag bump in the kube repo
(`connector-upworkcrm` runs a pinned tag — currently `0.4.1`). Nothing is urgent:
there has never been an `upwork_chat` delivery in production, so the matcher has
nothing to bind either way until the draft worker and the Upwork tier go live.

---

## Post-merge review correction (2026-08-26) — "exact room matching" is not room matching

A `go-reviewer` pass after the merge found that the fix's defect-2 half rests on
a premise that does not hold on production data. **Verified independently
against the source db before accepting it**, which is what this section records.

`thread_key` is `upwork_crm:{client_id}:{communications.channel}`
(`normalize.go:99`), and `channel` is the **constant `'upwork'`**:

```
SELECT channel, count(*), count(DISTINCT client_id) FROM communications GROUP BY channel;
 upwork | 1650 | 26          -- one value, every row, every client

SELECT DISTINCT split_part(thread_key,':',3) FROM normalized_threads WHERE thread_key LIKE 'upwork_crm:%';
 upwork                       -- in the ops db too
```

So `target_ref = thread_key` is **client-scoped in production**, selecting
exactly the candidate set the old `LIKE 'upwork_crm:{client}:%'` did. The rooms
are real but the normalizer never reads them: `communications.upwork_room_id`
has 296 populated rows over 11 distinct rooms, and one client
(`e2ef9b65-9813-4d79-ac10-0e1813f788ff`) already has two — Salvador's reported
scenario is real data, not a hypothetical.

**What this does and does not change.**

- It does NOT make the shipped change wrong. The equality is still strictly
  tighter than the `LIKE`, and the outcome on ambiguity is refusal — the
  reversible direction. No invariant is violated and nothing needs reverting.
- It DOES mean the thing preventing a wrong-row bind today is the **multi-match
  refusal**, not room identity. Every artifact that said otherwise (commit
  message, `sink.go` comment, the IK paragraph, this file's Resolution section)
  overstated the mechanism, and the `RoomDiscrimination` regression test proves
  room scoping with channel values (`chat`, `room-b`) the source has never
  emitted. The code comment and IK are corrected; this section corrects the
  record here.
- It makes the "survives the flow-3 transition" argument in "Superseded: the era
  assumption" conditional rather than established: it lands only once
  `thread_key` is keyed on `upwork_room_id`.

**Follow-up (NOT this ticket):** key `thread_key` on `upwork_room_id`. It re-keys
every existing upwork thread and touches dedup and `external_refs`, so it is its
own ticket, and it should land before the Upwork tier goes live.

### Also from the review, closed here

- **Two coverage gaps, now closed.** The multi-match refusal and the
  already-claimed pre-check were both decided policy with nothing pinning them.
  Added `AmbiguousPrefixConfirmsNothing` and
  `ClaimedExternalIDSkipsWithoutFailingTheRun`, and mutation-tested both:
  reverting `matches > 1` to newest-wins fails the first; disabling the
  pre-check fails the second with
  `duplicate key value violates unique constraint "deliveries_sent_external_idx"`
  — confirming the pre-check's real cost is a **crashed normalize run**, not a
  missed confirmation.
- **Two factual errors in the IK paragraph this ticket added**, both corrected:
  google's matcher joins on `d.from_account_id`, not `thread_id`; slackweb uses
  `target_ref=$1`, not `target_ref = ANY(...)`. They were carried over from this
  file's own "structural question" section without being checked against the
  source.
- **Recorded, not fixed:** upworkcrm has no reconciler, so unlike slackweb an
  ambiguity refusal here is silent — two rows sit unconfirmed with nothing
  surfacing them. And with exact matching a non-canonical `target_ref` is now
  permanently unconfirmable where the `LIKE` was forgiving, while
  `draft_delivery` still validates only non-emptiness for upwork
  (`internal/tools/delivery.go:107-108`). Both are in IK.
- **Accepted as a four-matcher pattern question, not an SWT-18 defect:** the
  `delivery_confirmed` insert sits outside the transaction, so a crash between
  commit and insert loses the event. jira and slackweb have the identical hole,
  and the event drives no orchestrator rule, so what is lost is an audit row
  rather than a lifecycle transition.
