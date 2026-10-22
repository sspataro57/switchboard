> Jira: SWT-109

# calendar-mcp-tools: live calendar reads over MCP; swb-first reservations (bookings and offer holds) pushed to Google eventually

**Status: FINAL.** Q1 (which calendar a booking is pushed to) was answered by
Salvador on 2026-10-21 and is folded in below (criteria 10, 11, 16, 30–33).
`docs/tickets/calendar-mcp-tools_OPEN_QUESTIONS.md` keeps the decision record.

**Amended 2026-10-21 (owner):** "I would like to be able to book times and swb
to keep them saved somewhere until they can sync to the actual calendar."
Booking is now a first-class verb for every session. Switchboard is the store
of record for a reservation, and pushing to Google is eventual. Offer holds are
one kind of reservation.

**Q1 answer (owner, 2026-10-21, verbatim):** "by default use sspataro@gmail.com
unless the meating request comes from avviation in that case try to sync to
handsoncconect."

## Amendments from implementation and go-reviewer (2026-10-22)

These supersede the criteria text they name.

- **Criterion 9, busy set:** the approved branch requires `push_next_attempt_at IS NOT NULL`. Only
  `book_time` and offer holds set it. Reason: `book_calendar_block` (SWT-28) approves its draft BEFORE its
  pre-flight, so counting every approved row made each legacy booking conflict with itself. The legacy
  pre-flight also excludes its own id.
- **Criterion 21 / "one create path":** `book_calendar_block` keeps SWT-28's synchronous `sendCalendarBlock`.
  The stage has its own reserve-and-send path (`calendar_push.go`). Both reach Google only through
  `calendarBooker` and both take `calendarBookingLock`. The stage's one-row request is `create_event` with
  the same field set.
- **Criterion 16, holds at send:** created in their own transaction right after the offer is marked
  `sent` (and on the recovered path). This is best-effort, with a task log on failure, plus a repair sweep in
  the stage. Rolling back a `sent` mark after the email left would be worse. `ensureHoldRows` locks the
  offer row, and `UNIQUE (hold_for_delivery_id, starts_at)` makes a duplicate set impossible.
- **Criterion 19, push inbox:** also excludes `confirmed_at IS NOT NULL`, `push_pending_reason='conflict'`
  (a conflict is terminal even for a row that keeps an id from an earlier attempt) and `ends_at <= now()`.
  Gate reasons (`account_not_write_enabled`, `target_mismatch`, `credit_budget`, awaiting verified absence)
  are re-checked every 30 minutes, not every sweep, to bound audit rows. The pre-flight excludes only the row
  itself, so two overlapping rows cannot pass in one batch. Each batch is capped at what is left of the
  channel's hourly limit.
- **Criterion 31, privacy guard:** the push checks BOTH `from_account_id` and `target_ref` against
  `reservationTarget`, and sends the resolved account email, never the stored `target_ref`. Deletes address
  the account email through `from_account_id`.
- **Criterion 22, deletes:** a failed delete backs off with `push_attempts` and `push_next_attempt_at`
  instead of spending a credit every sweep.
- **Criterion 4, agenda:** a pushed but unconfirmed reservation is shown once, as its Google event (the push
  records the event into the busy set at once), with state `pushed`.

Codex adversarial review (2026-10-22), applied:

- **Criteria 15–16, holds reserved BEFORE SMTP.** `send_delivery`'s gmail pre-flight re-checks an offer's
  windows AND inserts its holds in one transaction under `calendarBookingLock`, before phase 1. A definite
  failure (phase 1 refusal, MIME build, `SendRejectedError`) releases them (`not_needed`). An ambiguous
  failure keeps them until they expire. The unique index covers only live holds, so a re-sent offer can
  hold again.
- **Credit accounting is a durable counter.** The `ops_flags` row `calendar_write_invocations`
  (`{month, n}`) is incremented under a row lock before EVERY write invocation: stage creates and deletes,
  retries included, and the synchronous `book_calendar_block` path, which counts but is not refused. The
  stage refuses at `n >= CALENDAR_WRITE_MONTHLY_BUDGET`. This replaces counting distinct attempt stamps,
  which forgot overwritten retries.
- **The push claim locks the row.** Each eligible row is re-selected `FOR UPDATE` with the full inbox
  predicate inside the reserve transaction, so a cancel or R12 release that wins the race is honoured.
- **Stuck `sending` rows are recovered.** Past the 15-minute send lease a `sending` row with an id joins
  the push inbox (same id, only after verified absence) and the release inbox (delete; `gone` if it never
  landed).

Codex adversarial review, second pass (2026-10-22), applied:

- **Holds are released only while no send has claimed the offer.** A losing concurrent send (phase 1 refused
  on the winner's Message-ID) never frees the winner's holds. A definite rejection releases them after its
  own `failed` write.
- **The legacy synchronous path refuses swb-first reservations.** `book_calendar_block` and `send_delivery`
  refuse a calendar row with `push_next_attempt_at`, a release request or a hold link. Only the stage pushes
  those, with its release, target and budget checks.
- **Budget admission happens after the pre-flight, in the reserve transaction**, so a pass whose rows all drop
  out (stale, conflict) counts nothing. A single-row `create_event` counts 2, because its client retries
  once on a transport failure. The batch calls make exactly one POST (no implicit retry) and count 1.
  `calendar_stale` re-checks every 30 minutes.

Codex adversarial review, third pass (2026-10-22), applied:

- **Hold cleanup takes the offer row lock** (the lock `ensureHoldRows` takes when a retry reserves) and releases
  only while the offer is still `failed` with no Message-ID. A re-approved retry keeps its holds. A phase-1
  refusal leaves holds to expire (accepted residual: up to 72h of an unsent offer's windows held).
- **The legacy path counts its write inside the transaction that marks the row `sending`.** If counting fails,
  nothing is reserved or sent.

## Source

Ad-hoc, not a build-order step. Observed 2026-10-21; scope confirmed by Salvador
the same day:

> Claude sessions outside the switchboard repo cannot read Salvador's live
> calendar. The ops MCP exposes only book_calendar_block; propose_slots is an
> executor tool reachable only via opsctl. The user-level `calendar` skill reads
> .ics exports last refreshed 2026-08-03. A session (task #2328, delivery 236,
> reply to Grady) drafted meeting windows from that snapshot and offered Tue 27
> Oct 14:00–16:00 ET, overlapping a live "SE/Collab Weekly Standup Meeting - FALL
> 2026" at 15:00–15:30 that switchboard's own sync had.

Owner decisions, verbatim:
- "Hold offered windows": "When a session offers times to someone (like Grady),
  it puts temporary holds on your calendar for those windows so nothing else
  gets offered or booked there. The holds come off when they pick a time or
  after a few days."
- "I would like to be able to book times and swb to keep them saved somewhere
  until they can sync to the actual calendar."
- Q1, quoted above.

Builds on the policy-matrix row "Calendar own blocks | auto (always via
availability service propose_slots)" and on SWT-24, SWT-27 and SWT-28.

## Goal

Every session can (1) read Salvador's live, fail-closed calendar and propose
free slots, and (2) book a time. A booking becomes a switchboard reservation at
once and counts as busy everywhere switchboard checks. A pipelined stage pushes
it to Google whenever it can, to the calendar the task's project dictates, and
never loses it.

**Usable alone:** from a session in any repo (user-scope `ops-mcp-user`):
- `calendar_list_events` shows this week in ET, including switchboard
  reservations marked "not yet on Google" with the reason.
- `book_time` saves a booking that `propose_slots` avoids immediately.
- A non-Avviato booking lands on sspataro@gmail.com within one sweep.
- An Avviato booking waits as pending ("account not write-enabled") for
  salvador@handsonconnect.org, and goes out once writes are turned on there.
- A sent gmail offer with `offered_windows` reserves its windows the same way.
  `release_offer_holds` and the 72h expiry clear them.
- `cancel_reservation` removes a booking.
- The `calendar` skill points sessions here, never at the .ics snapshot.

## Premises, verified before writing

1. `propose_slots` is registered (`internal/tools/createtask.go:76`) but not in
   `agentTools` (`internal/mcpserver/schemas.go`). Neither profile lists it.
2. `availability.LoadBusy` (`internal/availability/store.go:38`) is the single
   database-backed path to free/busy. `internal/availability/callsites_test.go`
   fails any file other than `store.go` or `internal/connector/google/sink.go`
   that names `normalized_events`, and it bans an exported `func LoadEvents(`.
3. `normalized_events` already has `title`, `status`, `transparency` and
   `all_day` (`migrations/0005_google_connector.sql:5-9`). All-day dates are
   parsed as UTC midnight (`normalize.go:274-275`).
4. `proposeSlots` already formats slots in `cfg.Location`; the +01:00 output
   came from a stale installed binary. `availabilityConfig` still falls back to
   UTC silently when `AVAIL_TZ` is bad (`proposeslots.go:138-141`).
5. **Structure tests pin which binaries may arm a sender:**
   `cmd/ops-mcp-user/main_structure_test.go` allows only `SetSlackSender`, and
   `cmd/orchestratord/structure_test.go` and
   `cmd/connectors/google/structure_test.go` allow no `tools.Set*`. The booker
   is wired only in `cmd/ops-mcp`, `cmd/dashboard` and `cmd/opsctl`. Swb-first
   booking (write the row now, push later from a process allowed to) is what
   makes booking possible from every session.
6. `cmd/pipelined` hosts resident stage loops (5 min sweep, Postgres as queue of
   record). It arms no seam today, and no structure test forbids one.
7. `channel='calendar'` deliveries already model own blocks: drafted → approved
   → sending → sent, then `confirmed_at` by observation. Unconfirmed SENT rows
   are busy (`loadReservations`, `store.go:80`); APPROVED rows are not.
   `RecordOwnCalendarEvent` writes a sent block into `normalized_events` at
   once, and unconfirmed rows are fenced from supersede (`sink.go:613-617`).
   Neither path knows about deletion.
8. `sendCalendarBlock` (`delivery_calendar.go:144`) is the single path to the
   write route. Its overlap refusal is an untyped `fmt.Errorf`. Every failure
   leaves the row `failed` with `sent_external_id` kept and no reopen (SWT-28).
   The workflow has resolved 409 → `events.get` since v16 (2026-09-07).
9. `toolChannel` (`internal/policy/pgloader.go:19`) pins a send-shaped verb's
   policy channel by tool name.
10. Pipedream gives 100 credits a month at about 1 per invocation. Reads take
    ~60 (`0 11,17 * * *`), which leaves about 40 for writes. Once the cap is
    spent, every call returns `HTTP 400 "Error in workflow"` (17 bytes) for the
    rest of the month.
11. R8 skips `channel == "calendar"` (`rules.go:283`).
12. Postgres `now()` is constant within a transaction, so rows stamped in one
    batch share a timestamp.
13. **The schema has no "Avviato" marker.** `projects.client` is the console
    routing key, and `projects.send_from_account` (`0001_initial.sql:109`) is
    unverified for these rows. The demo-mode allowlist is an `ops_flags` value
    that deliberately excludes llamasite-sec (IK "Demo mode"). IK also records
    a landmine: the `salvador@handsonconnect.org` mailbox "mixes in Foundry
    mail". A mailbox- or sender-domain rule would therefore route Foundry work
    to the Avviato calendar. Salvador's recorded rule (memory
    "avviato-day-job-no-freelance-disclosure", 2026-10-05/08) names the Avviato
    projects: **collaboratory, a-millon, reengine, llamasite-sec**.
    llamasite-sec is project 14 and owns the Avviato Slack workspace
    T0360B84U and `@avviato.com` mail (IK swb 1438). The other three slugs were
    not verified against the live table here; criterion 33 does that at deploy.
14. The highest migration on main is `0048_tasks_demo_hidden.sql`, so the next
    is **0049**.

## Decisions made unilaterally

- **A reservation IS a `deliveries` row with `channel='calendar'`; no new
  table.** Booked in swb = `approved`; pushed = `sent`; on Google =
  `confirmed_at`. Reservations always hang off a task (`task_id NOT NULL`,
  invariant 2). The calendar channel already carries the interval, the account,
  the self-chosen id, loop closure, the supersede fence and the R8 skip. The
  delivery row IS the reservation, so invariant 4 holds literally.
- **Policy tier: AUTO, no approval gate** ("Calendar own blocks: auto").
  `book_time` writes the row `approved`, with `approval_source='switchboard'`
  and an `approvals` row naming the actor. It is not sendShaped (nothing leaves
  the system), so the kill switch stops PUSHING, not booking. The brakes on
  booking are:
  - the conflict refusal;
  - a cap of 10 reservations created per hour;
  - no attendees;
  - audit.
- **The calendar is chosen deterministically, from the TASK'S PROJECT, never
  by the caller (Q1).** A new column, `projects.calendar_account_id`, names a
  project's own push calendar. Migration 0049 sets it to the google
  `salvador@handsonconnect.org` account for the four Avviato slugs (premise
  13). Every other project leaves it NULL and pushes to the single
  `source_accounts.calendar_default` account, which the migration sets to the
  google `sspataro@gmail.com`.
  - *Beat:* a sender/mailbox/domain rule. The handsonconnect mailbox carries
    Foundry mail (premise 13), so it would route other-client work to the
    Avviato calendar.
  - *Beat:* the demo-mode allowlist, which is a display filter that omits
    llamasite-sec.
  - *Beat:* hard-coded slugs in Go, which cannot be corrected without a deploy.

  `book_time` has NO `account` argument, so no session can aim a booking at a
  calendar.
- **No silent fallback.** An Avviato reservation targets handsonconnect even
  while that account is not `calendar_write_enabled`. It waits as pending
  (`account_not_write_enabled`), busy in swb, and is never re-pointed to
  sspataro@gmail.com.
- **Neutral text on Google, always.** The pushed summary is server-side fixed:
  `Busy` for a booking, `Hold` for an offer hold. The description is fixed too.
  The session's `label` is stored only in switchboard
  (`deliveries.reservation_label`), shown in the events read and on the
  dashboard, and never sent to Google. This is the Avviato rule (no other-client
  or task names on a calendar colleagues see), applied to every calendar for
  simplicity, and invariant 6.
- **A known conflict refuses a booking. A stale calendar does not:** the
  booking is saved with `booking_verified=false`. The push re-verifies with the
  full fail-closed `LoadBusy` before writing. Offer windows stay strictly
  fail-closed at draft and at send, because they are promises to someone. The
  unverified path checks only switchboard's own reservations
  (`LoadReservations`) and never reads `normalized_events` without readiness.
- **One push stage, `calendar_push`, in `cmd/pipelined`.** It does every Google
  write for reservations (batch create, then batch delete) as actor
  `pipeline:calendar_push`. `book_calendar_block` (full profile, synchronous)
  is unchanged.
- **Creates and deletes are both batched.** `sendCalendarBlock` becomes
  `sendCalendarBlocks(ids)`, still the single path. With one id it sends the
  existing `create_event` request byte for byte; with more it sends
  `create_events`.
- **Never lose one, without breaking invariant 4.** A row's event id is minted
  once and never replaced. A `failed` row that holds an id is retried with that
  SAME id, and only after a verified read snapshot of its account, fetched
  after `send_attempted_at`, did not contain it. Google answers a duplicate id
  with 409, so a retry cannot double-book. This deliberately amends SWT-28's
  "never reopened"; codex review must confirm it.
- **The credit guard counts our own columns.** Batch rows share
  `send_attempted_at` or `release_attempted_at`, so the month's (UTC) write
  invocations are `COUNT(DISTINCT send_attempted_at) + COUNT(DISTINCT
  release_attempted_at)` over calendar rows. At or above
  `CALENDAR_WRITE_MONTHLY_BUDGET` (default 35), due rows are marked
  `credit_budget` and nothing is called. A 400 with the "Error in workflow"
  signature marks them `credit_cap`. Either way they stay saved and busy.
- **Offer holds are created at offer SEND** (in the gmail finalize transaction
  and on the SWT-71 recovered path), from `offered_windows`. Drafted offers
  reserve nothing.
- **The agenda read fails closed, with no override.**
- **The offer-hold TTL is 72h** (`OFFER_HOLD_TTL`; a bad value is an error).
  Plain bookings never expire.
- **Holds are for gmail offers only.** Slack offers are Future work (now cheap).
- **The skill source moves into the repo** (`skills/calendar/SKILL.md`,
  installed by `make install-skill`).

## Acceptance criteria

### A. Reads over MCP

1. `calendar_list_events {window_start?, window_end?}` (RFC3339, or
   `YYYY-MM-DD` read in `AVAIL_TZ`) is a read-only executor tool, not
   humanOnly. The default window is today 00:00 to +7 days. It refuses an empty,
   inverted or longer-than-31-day window.
2. Its SQL is `LoadAgenda` in `internal/availability/store.go`. It runs the SAME
   horizon and readiness code as `LoadBusy` (shared, not copied) before any
   read. A new scan in `callsites_test.go` fails if `LoadAgenda` reaches its
   queries without that call.
3. Refusals are `LoadBusy`'s, verbatim. Integration: when an in-scope account's
   last ok calendar run is at `now() - 25h`, the call errors and returns no
   events.
4. The response is `{timezone, window, max_sync_age, accounts:[{email,
   last_sync, age, write_enabled}], events:[…], truncated}`.
   - Every event has `title, start, end, all_day, account, busy, on_google`.
   - Google events from in-scope accounts exclude cancelled and superseded rows.
     `busy` is false only for transparent events.
   - A live reservation adds `reservation:{delivery_id, task_id, label, kind:
     booking|hold, state: pending|pushed|confirmed, pending_reason?, verified,
     hold?:{offer_delivery_id, expires_at, kept}}`.
   - A reservation already observed on Google appears once, as the Google event
     carrying the `reservation` object.
   - At most 300 events, ordered by start.
5. Every timestamp is in `AVAIL_TZ`. All-day events show `YYYY-MM-DD` dates.
   Unit-tested on the renderer.
6. **Incident regression** (integration): a 15:00–15:30 New York event on a
   Tuesday is listed at 15:00 local, and `propose_slots` over 14:00–16:00 never
   overlaps it.
7. `propose_slots` gains a `"timezone"` field and explicit
   `.In(cfg.Location)` formatting. `availabilityConfig` errors on a bad
   `AVAIL_TZ`. Unit-tested.
8. Both reads are added to `agentTools`, the full profile and
   `userProfileTools`, with no pins. The allowlist and count tests are updated.

### B. Reservations (swb-first)

9. **Busy set.** `loadReservations` returns every `channel='calendar'` row on an
   in-scope account where `released_at IS NULL`, `push_pending_reason IS
   DISTINCT FROM 'conflict'`, and one of:
   - `status='approved'` and `release_requested_at IS NULL`;
   - `status IN ('sending','sent','failed')` and `sent_external_id IS NOT NULL`
     and `confirmed_at IS NULL`. A pushed row that is release-requested stays
     busy until deleted.

   `Request` gains `ExcludeDeliveryIDs`. An exported
   `LoadReservations(ctx, pool, from, to)` returns this set without touching
   `normalized_events`. Integration: right after `book_time`, with no booker
   wired, `propose_slots` and `calendar_list_events` treat the slot as busy.
10. **`book_time {task_id, start, end, label}`** is a NEW executor tool on BOTH
    profiles. It is not humanOnly and not sendShaped, and it has **no account
    argument**.
    - Validation: RFC3339, `end > start`, at most 12h, inside the horizon. The
      label is 1–120 characters, scrubbed by `ScrubAIAttribution` and stored
      in `reservation_label`. The task exists and is not closed.
    - The target is resolved by criterion 30 and must be `provider='google' AND
      calendar_in_availability`. Write-enabled is NOT required.
    - Conflicts are checked under the SWT-28 booking advisory lock. If the
      calendar is fresh, `LoadBusy` overlap refuses and `booking_verified` is
      true. On `NotReadyError`, only `LoadReservations` is checked (overlap
      refuses) and `booking_verified` is false. Any other error refuses.
    - It inserts the row `approved`, with `approval_source='switchboard'`,
      subject `Busy`, a fixed description, `push_next_attempt_at=now()`, and an
      `approvals` row naming the actor, in one transaction.
    - It returns `{delivery_id, state:"pending", calendar, verified}`.
    - It refuses when 10 or more calendar rows were created in the last hour.
11. On the user profile, `book_time` carries the pin
    `require_assignee_type:"human"` (the `create_task` / `task_append_log`
    pattern). The full profile has no pin.
12. **`cancel_reservation {delivery_id}`** is NEW, on both profiles, DB-only,
    not humanOnly, and for calendar rows only.
    - It refuses kept holds and rows already released.
    - A row never pushed (`approved`, or `failed` with no id) gets
      `release_requested_at=released_at=now()` and `release_reason='cancelled'`,
      with no external call.
    - A pushed row is marked release-requested, and the stage deletes it.
    - It is idempotent and writes one `reservation_cancelled` task_event.

### C. Offer holds (one kind of reservation)

13. `draft_delivery` gains `offered_windows: [{start,end}]`: gmail only, 1–3
    windows, each at most 4h, non-overlapping, inside the horizon. It is
    STRICTLY fail-closed at draft (`LoadBusy` must answer, with no overlap).
    Windows are stored on the offer row in UTC, sorted. `update_delivery`
    accepts `offered_windows` while the offer is drafted; `[]` clears them.
    The user-profile pins are unchanged.
14. The dashboard shows `offered_windows` on drafted and approved offers, and
    every reservation with its label, target calendar, state and pending reason.
15. **Send-time refusal.** In `sendDelivery`'s gmail branch, before phase 1,
    `LoadBusy` runs over each offered window. Stale or conflicting refuses, and
    the row stays `approved` with a NULL `sent_external_id`. Integration tests
    cover both cases.
16. **Holds are created at send.** In the transaction that marks the offer
    `sent`, and on the SWT-71 recovered path, `ensureHoldRows(tx, offerID)`
    inserts one reservation per window: `hold_for_delivery_id=<offer>`, the
    offer's task, the criterion 30 target from that task's project, subject
    `Hold`, the fixed description, `status='approved'`, an `approvals` row,
    `push_next_attempt_at=now()`, and `hold_expires_at = offer sent_at +
    OFFER_HOLD_TTL`. It never inserts a second set.
17. `release_offer_holds {delivery_id, keep_start?, reason?}` is on both
    profiles and DB-only.
    - `keep_start` (matched as an instant) sets `hold_kept_at`, and the kept
      hold becomes a plain booking with no expiry. A non-matching value
      refuses, listing the windows.
    - Other live holds are released with reason `picked_other` or
      `not_needed`. `expired` is refused over MCP.
    - Unpushed holds are released with no external call.
    - It is idempotent (`changed:0` on a repeat call).

### D. Pushing (pipelined stage `calendar_push`)

18. A `pipeline.Stage` `calendar_push` runs on the sweep only, with no upstream
    wake. Each pass calls `push_calendar_reservations {limit}` and then
    `apply_hold_releases {limit}`, each only when its inbox is non-empty, so a
    pass with nothing due spends no credit (integration test with a counting
    fake booker). Both tools:
    - are sendShaped and freezeGated, with channel pinned to `calendar` in
      `toolChannel`;
    - are not MCP-listed;
    - run as actor `pipeline:calendar_push`.
19. **The push inbox** is calendar rows with `push_next_attempt_at <= now()`,
    not release-requested, that are either `approved`, or `failed` with an id
    and retry-eligible (see Decisions). Legacy `drafted` rows are never pushed.

    Cheap DB checks run first and spend no credit. Each sets a pending reason
    and re-checks every pass:
    - `target_mismatch`: criterion 31's guard;
    - `account_not_write_enabled`: re-read from `source_accounts`;
    - `credit_budget`.

    Then `sendCalendarBlocks(eligibleIDs)` runs under the advisory lock:
    - A per-row `LoadBusy` pre-flight excludes the batch's ids.
      `NotReadyError` → `calendar_stale`.
    - `SlotConflictError` is TERMINAL: `failed`, no id, reason `conflict`,
      out of the busy set, with a `log` task_event.
    - Survivors are reserved (`sending`, id minted only if absent, one shared
      `send_attempted_at`) and sent in ONE call.

    A kill-switch denial leaves rows untouched; the agenda derives
    `sending_frozen` for them.
20. **Results.**
    - ok → `sent`, `RecordOwnCalendarEvent`, a `delivery_sent` event
      (`channel:"calendar"`), and the reason cleared.
    - Per-entry or transport error → `failed`, id kept, reason
      `transport_error` or `credit_cap` (the 400 "Error in workflow"
      signature), `push_attempts+1`, and `push_next_attempt_at = now() +
      min(5m·2^attempts, 6h)`.

    Only `conflict` and an explicit release take a reservation out of the busy
    set.
21. **The `create_events` client.**
    `PipedreamCalendarClient.CreateEvents(ctx, CreateEventsRequest)`:
    - request `{schema_version:1, action:"create_events", events:[{calendar_id,
      event_id, start, end, summary, description}]}`, with no attendees field;
    - response `results:[{calendar_id, event_id, status, created, event |
      error}]`;
    - each ok entry passes `verifyCreateEventEcho`; each requested id appears
      exactly once;
    - errors are classified, with no endpoint or token in them;
    - one retry, on a transport error only.

    With one row, `sendCalendarBlocks` sends the unchanged `create_event`
    request; a byte-identical test pins it.
22. **`apply_hold_releases`** covers every released or cancelled reservation.
    - Rows never pushed, or with `ends_at <= now()`, get `released_at=now()` with
      no call.
    - The rest go into ONE `DeleteEvents` call with a shared
      `release_attempted_at`. `deleted`/`gone` sets `released_at` and writes a
      `reservation_released` event; `error` waits for the next pass and logs.
    - It never writes `deliveries.error`.
23. **The `delete_events` client** has the same discipline. Before the network
    call it refuses any `event_id` not matching `^sb[0-9a-v]+t[0-9a-v]+$`. The
    workflow applies the same check with `sendUpdates=none`, and 404/410 map to
    `gone`.
24. **Released means free.** The `SupersedeAbsentCalendar` fence and
    `ConfirmObservedCalendarDeliveries` ignore rows with `released_at IS NOT
    NULL`. Integration covers it, with a mutation check: drop the predicate and
    the test goes red.
25. `cmd/pipelined` arms `tools.SetCalendarBooker` ONLY when `calendar_push` is
    enabled. A new structure test pins it as the single `Set*` call, behind the
    stage check.

### E. Expiry (orchestrator R12)

26. `loadTickFacts.ExpiredHoldOffers` lists offers owning a live hold with
    `hold_expires_at <= now`, not kept and not release-requested. The pure
    `ruleHoldExpiry` emits `release_offer_holds {delivery_id,
    reason:"expired"}` per offer, with no `record_orchestration`. Zero-I/O tests
    cover four cases: an expired hold fires; a kept hold, a plain booking and
    an empty list do not. `orchestratord` stays seam-free.

### F. Instructions and the skill

27. `mcpserver.Instructions` gains:
    - "what's on my calendar" → `calendar_list_events`;
    - "book/block X" → `book_time`;
    - "cancel that booking" → `cancel_reservation`;
    - before offering ANY time → `propose_slots` plus `offered_windows`, never
      any other source;
    - "they picked X" → `release_offer_holds` with `keep_start`;
    - a pending reservation is reported as "saved, not yet on Google
      (reason)", never "booked on your calendar".
28. `skills/calendar/SKILL.md` replaces `~/.claude/skills/calendar/SKILL.md`. It
    uses only these MCP verbs and retires the .ics script for every answer, with
    no fallback. `make install-skill` installs it. Install it on both
    workstations along with `go install ./cmd/ops-mcp-user`.
29. *(reserved: numbering kept stable for the test author)*

### G. Which calendar (Q1) and the Avviato privacy rule

30. **Target resolution, one unexported function `reservationTarget(ctx, tx,
    taskID)`**, used by `book_time`, `ensureHoldRows` and the push re-check:
    - if the task's project has `calendar_account_id`, that account;
    - otherwise the single `source_accounts` row with `calendar_default`.

    Either way the target must be `provider='google' AND
    calendar_in_availability`, or the call is refused by name. Unit tests over
    fixtures:
    - a task in a project whose `calendar_account_id` is the handsonconnect
      fixture resolves to handsonconnect;
    - a task in an unmapped project (a `foundry` fixture) resolves to the
      default;
    - no default row present refuses.
31. **Privacy guard, never pushed to the wrong calendar.** At push time the
    handler recomputes `reservationTarget` for the row's task. If it differs from
    the row's `from_account_id`, the row is held as `target_mismatch` and never
    sent, whatever the reason (a project re-mapped, or a hand-edited row).
    Integration test: forge a row on an unmapped-project task with
    `from_account_id` = the handsonconnect fixture. Ten passes with a counting
    fake booker make ZERO calls for it, and its reason is `target_mismatch`.
    Mutation: remove the re-check and the test goes red.
32. **No silent fallback, and neutral text.** Integration: a booking on a task
    in the handsonconnect-mapped project, with that account not write-enabled
    and the default account write-enabled, stays `approved` with reason
    `account_not_write_enabled` across passes and is never sent to the default.
    Flip handsonconnect to write-enabled, and the next pass pushes it there.

    A unit test on the request builder: for any label, including one that names
    a client or task, every `create_event` / `create_events` entry's summary is
    exactly `Busy` or `Hold`, and its description is the fixed text. The label
    never appears in any outbound byte.
33. **The data is seeded and verified.** Migration 0049:
    - sets `calendar_default=true` on `provider='google' AND
      lower(account_email)='sspataro@gmail.com'`;
    - sets `projects.calendar_account_id` = the `provider='google'`
      `salvador@handsonconnect.org` account for slugs `collaboratory`,
      `a-millon`, `reengine`, `llamasite-sec`.

    Accounts are always matched on provider AND email: IK says the same email
    exists across providers. The runbook's pre-deploy check is
    `SELECT slug, calendar_account_id FROM projects ORDER BY slug`: exactly those
    four are set, and any missing slug is reported to Salvador, never guessed.
    A new Avviato project is mapped by one UPDATE (runbook line).

## Data model changes

`migrations/0049_calendar_reservations.sql`, forward-only. Every new column is
nullable or defaulted.

| table.column | type | meaning |
|---|---|---|
| `projects.calendar_account_id` | BIGINT REFERENCES source_accounts(id) | this project's push calendar (NULL = default) |
| `source_accounts.calendar_default` | BOOLEAN NOT NULL DEFAULT false | the default push calendar; partial UNIQUE index `WHERE calendar_default` |
| `deliveries.offered_windows` | JSONB | gmail offers: `[{start,end}]` |
| `deliveries.hold_for_delivery_id` | BIGINT REFERENCES deliveries(id) | the offer a hold belongs to |
| `deliveries.hold_expires_at` | TIMESTAMPTZ | holds only |
| `deliveries.hold_kept_at` | TIMESTAMPTZ | the chosen hold |
| `deliveries.reservation_label` | TEXT | the session's label; swb-only, never pushed |
| `deliveries.booking_verified` | BOOLEAN | checked against a fresh calendar at booking |
| `deliveries.push_pending_reason` | TEXT | why it is not on Google yet |
| `deliveries.push_next_attempt_at` | TIMESTAMPTZ | backoff clock |
| `deliveries.push_attempts` | INT NOT NULL DEFAULT 0 | |
| `deliveries.release_requested_at` | TIMESTAMPTZ | cancel/release decided |
| `deliveries.release_reason` | TEXT | `picked_other`, `not_needed`, `expired`, `cancelled` |
| `deliveries.release_attempted_at` | TIMESTAMPTZ | shared per delete batch (credit count) |
| `deliveries.released_at` | TIMESTAMPTZ | off Google, or never needed there |

CHECKs:
- `offered_windows` only on gmail rows, as an array.
- The hold, booking, push and release columns only on `channel='calendar'`.
- `hold_expires_at` and `hold_kept_at` only when `hold_for_delivery_id IS NOT
  NULL`.
- `push_pending_reason IN ('account_not_write_enabled', 'target_mismatch',
  'credit_budget', 'credit_cap', 'calendar_stale', 'transport_error',
  'conflict')`.
- `release_reason` within its set, and `(release_requested_at IS NULL) =
  (release_reason IS NULL)`.
- `released_at` implies `release_requested_at`.

Partial indexes:
- the push inbox, on `push_next_attempt_at`;
- `hold_for_delivery_id`.

The migration comment says to check that prod is at 0048 first.

## API / MCP tool changes

All registered in `tools.Register`: validate → policy → audit → handler → audit.

| tool | change | MCP | policy |
|---|---|---|---|
| `calendar_list_events` | NEW read | full + user | static allow |
| `propose_slots` | timezone field; bad AVAIL_TZ errors | full + user (newly listed) | static allow |
| `book_time` | NEW: a reservation in swb; no account argument | full + user (user pin: human task) | auto tier; not sendShaped; 10/h cap in the handler |
| `cancel_reservation` | NEW, DB-only | full + user | static allow |
| `draft_delivery` / `update_delivery` | `offered_windows` (gmail) | unchanged | unchanged |
| `send_delivery` | gmail pre-flight; holds created at send | unchanged | unchanged |
| `release_offer_holds` | NEW, DB-only | full + user | static allow; `expired` refused over MCP |
| `push_calendar_reservations` | NEW | off MCP | sendShaped + freezeGated, channel `calendar` |
| `apply_hold_releases` | NEW | off MCP | sendShaped + freezeGated, channel `calendar` |
| `book_calendar_block` | unchanged (via `sendCalendarBlocks` with one id) | full | unchanged |

## MQTT topics

The `calendar_push` stage gets the standard pipelined per-stage heartbeat and
dead LWT. No new topic and no wake event.

## Files likely to touch

- `migrations/0049_calendar_reservations.sql` (new)
- `internal/availability/store.go`: `LoadAgenda`, `loadReservations`,
  `LoadReservations`, `ExcludeDeliveryIDs`. Plus `callsites_test.go`.
- `internal/tools/proposeslots.go`; `internal/tools/calendar_events.go` (new)
- `internal/tools/reservations.go` (new): `book_time`, `cancel_reservation`,
  `push_calendar_reservations`, `apply_hold_releases`, `release_offer_holds`,
  `ensureHoldRows`, `reservationTarget`, backoff, the credit guard,
  `OFFER_HOLD_TTL`
- `internal/tools/delivery_calendar.go`: `sendCalendarBlocks`,
  `SlotConflictError`, `CalendarBooker.CreateEvents/DeleteEvents`, the
  failed-with-id retry, and the fixed summary/description builder
- `internal/tools/delivery.go`: `offered_windows`, the gmail pre-flight, and
  `ensureHoldRows` in finalize and in `finishConfirmedSend`
- `internal/tools/createtask.go`; `internal/policy/matrix.go`;
  `internal/policy/pgloader.go`
- `internal/connector/google/pipedream_write.go`: `CreateEvents` and
  `DeleteEvents`. `internal/connector/google/sink.go`: the `released_at`
  predicates.
- `internal/orchestrator/rules.go`, `facts.go`, and
  `rules_hold_expiry_test.go` (new)
- `internal/pipeline/contract.go` (+ its test); `cmd/pipelined/main.go`,
  `cmd/pipelined/calendarpush.go` (new); a structure test
- `internal/mcpserver/schemas.go`, `adapter.go`, `serve.go`, and their tests
- `internal/dashboard/server.go`, `templates/deliveries.html`
- `skills/calendar/SKILL.md` (new), `Makefile`
- Runbooks:
  - `calendar-availability.md`: reservations, the two workflow actions, the
    credit guard, the Avviato mapping and its verification query, and how to
    turn on handsonconnect writes;
  - `ops-mcp-user-scope.md` and `pipeline.md`;
  - a kube handoff note: `PIPELINE_STAGES` += `calendar_push`, the
    `switchboard-pipedream` secret, `OFFER_HOLD_TTL`,
    `CALENDAR_WRITE_MONTHLY_BUDGET`.
- `.claude/INSTITUTIONAL_KNOWLEDGE.md`

## Credits (revisited)

About 40 credits a month are left for writes, and the guard default is 35.
- One create invocation per sweep carries everything due; one delete invocation
  carries every release.
- A plain booking costs about 1 credit, plus 1 if it is cancelled after being
  pushed, and bookings in the same sweep share.
- An offer costs about 2 (one create for all its holds, at most one delete),
  down from ~3.5.
- That is roughly 17 offers, or ~35 plain bookings, a month, up from ~10
  offers.
- Avviato bookings spend nothing while handsonconnect is not write-enabled.
- When the budget is out, nothing is lost: rows wait as `credit_budget`.

## In scope / Out of scope

**In scope:**
- Both reads, and the timezone fix.
- `book_time` and `cancel_reservation`, with swb-first reservations and the
  busy set.
- Project-based calendar targeting, the privacy guard, and the default and
  Avviato seed.
- Offer windows, holds at send, release and expiry.
- The `calendar_push` stage: batched create and delete, backoff, the credit
  guard, and the same-id retry.
- Both workflow actions, the dashboard, the Instructions, and the skill on both
  workstations.

**Out of scope:**
- Invites with attendees.
- Editing or deleting other people's events (ids are shape-checked on both
  sides).
- Moving or renaming a reservation.
- Turning on writes for handsonconnect: that is a hand flip of
  `calendar_write_enabled`, plus the Pipedream account connection, when
  Salvador chooses to.
- A per-booking calendar choice, which is deliberately absent.
- Slack, Upwork and Jira offers.
- Auto-detecting the pick.
- Moving reads off Pipedream.
- Arming a booker in `ops-mcp-user`, `orchestratord` or the connector.
- Deleting the old .ics script or its timer.
- Kube manifests.
- Step 10 briefs.

## Invariants that apply

1. **Raw-first.** Pushed events are stored verbatim through
   `RecordOwnCalendarEvent`. An unpushed reservation is our own intent, held in
   `deliveries`, not a captured item. Deletions are learned by snapshot
   supersede.
2. **One funnel.** No new table. Reservations are `deliveries` rows on existing
   tasks; two configuration columns (`projects.calendar_account_id`,
   `source_accounts.calendar_default`) extend existing tables.
3. **Everything through the executor.** Every verb is registered, and the stage
   and R12 call through `executor.Execute`. There is one create path
   (`sendCalendarBlocks`) and one delete path (`apply_hold_releases`).
4. **Nothing external without a delivery row.** The reservation is the row. Its
   id is minted once, and the only retry reuses it after verified absence (the
   SWT-28 amendment, for codex). Pushes go only to `reservationTarget`,
   re-checked at send. Own blocks are the auto tier. The kill switch stops
   pushes and deletes.
5. **Own-message loop closure.** Confirmation is by observed event id. Released
   rows are excluded. Calendar ingest creates no tasks, and R8 skips calendar.
6. **Stealth attribution.** Google sees only `Busy`/`Hold` and fixed text; labels
   never leave switchboard. No attendees. This also enforces the Avviato
   disclosure rule.
7. **Orchestrator is pure.** R12 is a function of tick facts, a DB-only action.
   The process arms no seam.

## Sibling patterns to copy

- `LoadBusy` / `CalendarSyncStates`
- `bookCalendarBlock` (the approve shape and its `approvals` row)
- `toolChannel` / `send_slack_reply`
- `cmd/pipelined/inquiry.go` and `internal/pipeline/contract.go`
- `cmd/ops-mcp-user/main_structure_test.go`
- `ruleClaimExpiry` / `loadTickFacts`
- `pipedream_write.go`
- `migrations/0020_calendar_booking.sql`
- `userProfilePins` (`require_assignee_type`)
- `skills/swb-status` + `make install-skill`

## Verification protocol

1. **Unit:** `go build ./... && go test ./...`. This covers the renderer and the
   timezone fix; validation; the envelopes, echo checks and id shape; the
   byte-identical single-row wire; the neutral-text builder (criterion 32); the
   target resolution (criterion 30); the backoff and the credit arithmetic;
   policy for the two new sendShaped verbs; R12; the profile allowlists and the
   `book_time` pin; and the structure tests.
2. **Integration:** `make integration` on :5433, with a counting fake booker.
   - Criteria 3, 6, 9–13, 15–20, 22, 24 and 31–33.
   - The never-lose sequence: unwired booker → not write-enabled → budget out →
     transport error. Each leaves the row busy with its reason, and each later
     pass pushes it once the cause clears.
   - The same-id retry, only after verified absence.
   - The kill switch freezes pushes but not `book_time`.
   - Freshness seeded relative to `now()`. FK-ordered cleanup: holds before
     offers.
3. **Live smoke** (prod, deliberate):
   1. Run the migration check query (criterion 33).
   2. Pipedream: run a read poll, add `create_events` and `delete_events`, run
      the read poll again (unchanged). Note credits before and after.
   3. Open a new session in a non-switchboard repo after
      `go install ./cmd/ops-mcp-user`:
      - `calendar_list_events` shows the Tuesday standup at 15:00 ET.
      - `book_time` on a task in a non-Avviato project returns `pending`,
        `calendar: sspataro@gmail.com`, and `propose_slots` avoids it at once.
      - One sweep later it is on Google titled `Busy`, with the label nowhere
        in the event.
      - After the next read poll it is confirmed.
   4. `book_time` on a collaboratory task returns `calendar:
      salvador@handsonconnect.org` and stays pending
      (`account_not_write_enabled`) across sweeps. It is busy and visible, and
      sspataro@gmail.com is unchanged.
   5. `cancel_reservation` on both: the pushed one is deleted at the next sweep,
      the pending one is released with no call.
   6. A gmail offer with two windows, sent from the dashboard: two `Hold` rows at
      once, on Google after a sweep. Then `release_offer_holds keep_start`, and
      `opsctl ... reason:"expired"`.
   7. Check `audit_events`. Repeat the install and the skill copy on
      192.168.50.30.
4. **Review:** `/ticket-review` with the codex adversarial pass. It is
   mandatory: unattended external writes, policy changes, the SWT-28 retry
   amendment, and the privacy guard.

## Future work

- Slack offers with `offered_windows`.
- Detect the pick from an incoming invite.
- Move or rename a reservation (`update_event`).
- An alert task on a `conflict` after the offer went out.
- A slower push cadence for more batching.
- Take calendar reads off Pipedream.
- If Avviato work ever spans non-mapped projects, a project-level marker
  replaces the four-slug seed.
