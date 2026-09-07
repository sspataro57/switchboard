> Jira: SWT-28

# calendar-booking — make "book an own calendar block" a first-class, policy-gated delivery

**Status: FINAL.** Both open questions were answered by Salvador on 2026-09-07
and are folded in below. `docs/tickets/calendar-booking_OPEN_QUESTIONS.md`
records them with the reasoning and stays as the decision record.

- **Q1 = auto tier, now.** The `calendar` channel ships at the tier CLAUDE.md's
  matrix states, via a new agent-callable `book_calendar_block(delivery_id)`
  executor verb — kill-switch- and rate-limit-gated, MCP-listed, NOT human-only
  (criteria 14–17, 18, 24). `policy.humanOnly` is not widened; the shared human
  gate is untouched. Salvador chose this knowing the prompt-injection exposure
  the question named, and the conflict / freshness / horizon refusal
  (criterion 19) is the backstop that makes it acceptable: the worst an injected
  call can do is put a block on Salvador's own calendar, at a time that is
  provably free, on an account a human explicitly write-enabled, at most ten per
  hour, all audited.
- **Q2 = R8 ignores `channel == "calendar"`.** A booking never advances a task's
  lifecycle and never burns the `delivery_lifecycle` dedup key (criterion 29).

## Source

Not a build-order step. Ad-hoc, agreed with Salvador 2026-09-07:

> Switchboard can propose free slots (`propose_slots`, SWT-24) but nothing can
> WRITE to a Google calendar. Build the booking path: "book an own calendar
> block" as a first-class, policy-gated outbound delivery.

The build-order text this descends from is step 7's availability service and the
policy-matrix row that gives it weight (CLAUDE.md, "Policy matrix"):

> | Calendar own blocks | auto (always via availability service propose_slots) |
> | Calendar invites w/ others | approve |

This ticket implements the FIRST row only, at the tier it states. "Own block" =
an event on one of our own Google calendars with **no other attendees**.
Anything with attendees is the second row and is out of scope (§Out of scope).

## Goal

One sentence: extend the deployed `switchboard-calendar` Pipedream workflow with
a backward-compatible `create_event` request shape, and make `channel='calendar'`
a live, auto-tier delivery channel whose booking verb runs through the executor
and the policy matrix, with the Google event id chosen by switchboard before the
network call so a retry cannot double-book.

**Usable alone** means: after this ticket, from a workstation shell, Salvador can
run `propose_slots`, then `draft_delivery` + `book_calendar_block` through
`opsctl`, and a real 15-minute block appears on `sspataro@gmail.com` — and the
next `*/20` read poll confirms that delivery row while `propose_slots` stops
offering the slot. The same two calls are reachable by a Claude Code worker over
MCP with no human in the loop. No dashboard work and no worker changes are
required to exercise it.

## Premises, verified before writing

Read on this branch at the paths and lines given.

1. **`calendar` is ALREADY a legal `deliveries.channel` value** —
   `migrations/0001_initial.sql:194` and the re-stated CHECK at
   `migrations/0009_slack_web_connector.sql:7-8`. No migration is needed to
   admit the value; what it lacks is columns, a policy branch and a send path.
2. **The policy matrix denies it today** — `internal/policy/matrix.go:159-162`,
   default branch, rule `channel_not_live`. So a send on a calendar row is
   refused before any handler runs; that is the gate this ticket opens.
3. **`propose_slots` is read-only and fails closed** —
   `internal/tools/proposeslots.go:100-110` calls `availability.LoadBusy`, which
   refuses on a stale sync, an empty scope, or a window outside
   `[now-30d, now+90d]` (`internal/availability/store.go:38-64`). Its doc comment
   already anticipates this ticket: *"step 8's write path consumes this existing
   audited surface"* (`proposeslots.go:20-21`).
4. **`LoadBusy` is the ONE database-backed door to free/busy and that is
   mechanically enforced** — `internal/availability/callsites_test.go` fails any
   file under `internal/` or `cmd/` other than `internal/availability/store.go`
   and `internal/connector/google/sink.go` that names `normalized_events` in SQL.
   Any conflict check this ticket adds must therefore go through `LoadBusy`, and
   any `normalized_events` write must stay inside `sink.go`.
5. **The Pipedream read client already holds the endpoint, the token, the
   construction-time validation and the "endpoint withheld" error discipline** —
   `internal/connector/google/pipedream.go:100-199`. Transport errors are
   CLASSIFIED, never `%w`-wrapped, because `*url.Error` embeds the URL and the
   URL is the token's neighbour (`pipedream.go:15-19`).
6. **Gmail's send already has exactly the idempotency shape this ticket needs**:
   a SELF-CHOSEN external id (`<sb-{deliveryID}-{unixnano}@domain>`) committed to
   the row together with `status='sending'` BEFORE the network call —
   `internal/tools/delivery.go:589` and `:606-611`. Jira's differs only because
   Jira assigns the id post-call.
7. **`deliveries_sent_external_idx` is UNIQUE on `(channel, sent_external_id)`
   where not null** — `migrations/0001_initial.sql:204-205`. A self-chosen
   calendar id is therefore structurally unique within the channel.
8. **Loop closure for gmail is an exact external-id match that stamps
   `confirmed_at` and emits `delivery_confirmed`** —
   `internal/connector/google/sink.go:312-343`. Note it has **no channel
   predicate** (`WHERE sent_external_id=$1`), which constrains the calendar id
   spelling (criterion 6).
9. **The calendar normalize branch dispatches on the `calendar:` external-id
   prefix** — `internal/connector/google/normalize.go:329-336`
   (`NormalizeCalendarEvent` → `sink.upsertEvent` → `markNormalized`). The prefix
   is spelled inline in three places: `ingest.go:312`, `pipedream_ingest.go:270`,
   `bridge_ingest.go:262`.
10. **The Pipedream poll is a windowed REPLACEMENT** — every poll supersedes any
    in-window raw item not present in the snapshot
    (`pipedream_ingest.go:296`). A raw row this ticket writes at send time is
    inside the window and inside the next snapshot, so it is re-upserted (same
    `content_hash` short-circuit) rather than superseded.
11. **The live workflow is `switchboard-calendar` v14 in project
    stealth-fun-natural, three Google accounts connected, HTTP trigger with a
    custom response and a Bearer shared secret** —
    `docs/runbooks/calendar-availability.md:102-147`. Its kube CronJob
    `connector-gcal` runs `*/20` with `CAL_SOURCE=pipedream`
    (same runbook, :149-162).
12. **The highest applied migration is 0019** (`ls migrations/`). Next free
    number is **0020**.
13. **R8 fires on ANY `delivery_sent` regardless of channel** —
    `internal/orchestrator/rules.go:114-115`, `:271-291`. A failed action is
    logged and the drain continues (`engine.go:110-141`), but the
    `record_orchestration` dedup key that follows is only suppressed after a
    failed `create_task` — so a booking against an active task would burn that
    task's `delivery_lifecycle` key. Q2 closes this (criterion 29).
14. **`policy.Decide`'s channel switch allows any `sendShaped` tool once the
    channel branch is reached** (`matrix.go:108-158`). A new send-shaped verb is
    therefore permitted on gmail/jira/slack rows unless something denies it by
    name — the hole criterion 15 closes.
15. **The policy snapshot resolves the channel from `delivery_id` in the call
    args** — `internal/policy/pgloader.go:33-38` via `deliveryIDArgs`. A verb
    whose only argument is `delivery_id` gets its channel snapshot for free.

## Decisions made unilaterally

Each names the alternative it beat. (The two decisions Salvador made are in the
status header and in `_OPEN_QUESTIONS.md`, not here.)

- **`sent_external_id` = `calendar:{google_event_id}`, not the bare event id.**
  `confirmDelivery` (premise 8) matches `sent_external_id` with no channel
  predicate, so a bare base32 id could in principle be claimed by a passing
  Message-ID match. The `calendar:` prefix is also the spelling already used for
  the raw item's `external_id` (premise 9), which makes loop closure a plain
  equality with no key surgery in SQL. Alternative — a bare id plus adding a
  channel predicate to `confirmDelivery` — touches a shipped matcher for no gain.
- **The event id is SWITCHBOARD's, chosen before the call.** Google's
  `events.insert` accepts a client-supplied `id` (base32hex: `[0-9a-v]`, 5–1024
  chars) and answers a duplicate with **409**, which is exactly the property
  "retry a timeout without double-booking" requires — and it is gmail's shape
  verbatim (premise 6), so the invariant-4 reasoning is already written and
  tested in this repo. Alternative — a `request_id` stored in Pipedream and
  checked there — puts the idempotency state in a third party we cannot query,
  cannot test, and cannot reason about from a `psql` prompt.
- **A new `source_accounts.calendar_write_enabled` column, not a reuse of
  `send_enabled`.** `send_enabled` is the MAIL go-live gate for a google row;
  reusing it would mean flipping calendar booking on also flips SMTP sending on
  for that mailbox, and vice versa. `calendar_in_availability` is the precedent:
  calendar-specific capability, its own column (IK: "a google row can be
  dual-auth" — `auth_type` names the mail path and says nothing about calendar).
  Under the auto tier this column carries more weight than it would have at the
  approve tier: it is the ONLY per-account consent an unattended booking has, so
  it defaults false and is flipped by hand, per account.
- **`book_calendar_block` approves AND sends in one call, rather than an
  auto-approve rule inside `approve_delivery`.** Two reasons. The audit trail
  stays legible — one tool call, one policy decision, one `approvals` row naming
  the actor — and `approve_delivery` keeps meaning "a human approved this",
  which three other channels depend on. Alternative — teaching
  `approve_delivery` to self-approve for calendar — would put an unattended path
  inside the verb every other channel's human gate runs through.
- **The auto verb is denied on every channel but calendar, in POLICY, by name.**
  Premise 14 means a new send-shaped tool is otherwise allowed on a gmail row the
  moment the gmail branch is reached, and the handler's own channel check would
  be the only thing between an agent and an unapproved client email. Two gates,
  the outer one pure and unit-testable (criterion 15).
- **No gate in this ticket keys on the actor prefix.** IK: "an actor-prefix check
  is a transport label, not a trust boundary" — and the counter-example named
  there (`drafts:gpt` calling the executor directly) is exactly the kind of
  caller this verb is for. What restricts `book_calendar_block` is the channel,
  the kill switch, the rate limit, `calendar_write_enabled`, and the `LoadBusy`
  refusal — none of which a caller can forge.
- **Booking is refused outside working hours only if it is BUSY, never for being
  out of hours.** Working hours shape what `propose_slots` PROPOSES; a 20:00 own
  block is a legitimate thing to book. The safety gates are freshness, horizon
  and overlap.
- **"The slot came from propose_slots" is enforced as a PROPERTY, not as
  provenance.** The send path re-derives the busy set through `LoadBusy` over
  `[start, end)` and refuses on overlap, on a stale calendar sync, and outside
  the horizon — the same three refusals `propose_slots` makes, fail-closed, at
  the last possible moment. Alternative — a signed proposal token minted by
  `propose_slots` and redeemed at booking — proves a slot was once free, which is
  strictly weaker than proving it is free now, and adds state for that weaker
  claim.
- **All write failures KEEP `sent_external_id`; none reopens the row.** Gmail
  clears it only on a definite `SendRejectedError` so `failed → approved` can
  retry. Here the reopen would need the workflow's 409 handling to be trustworthy
  for a resend to be safe, and that handling lives in a human-edited third-party
  workflow. Keeping the id costs nothing (it is derived from this delivery and
  worthless to any other row) and keeps invariant 4 literal. Consequence: the
  recovery for an ambiguous failure is the read poll (which confirms it if it
  landed) or a NEW draft, and that is documented in the runbook.
  Corollary: `google.SendRejectedError`'s definite/ambiguous distinction is
  deliberately NOT copied into the write client — with no reopen path there is
  nothing for the distinction to unlock.
- **The write is a METHOD on the existing `*PipedreamCalendarClient`, in a new
  file.** One endpoint, one token read, one constructor validation, one
  classified-error discipline. A second client type would duplicate all four and
  is precisely how two spellings of one rule come to disagree.

## Acceptance criteria

Numbered; each is testable.

### A. The Pipedream write route (transport)

1. `PipedreamCalendarRequest` gains an `Action string \`json:"action,omitempty"\``
   field; the read poll sends it EMPTY, so the bytes on the wire for a read are
   byte-identical to today's. A unit test marshals the read request and asserts
   the `action` key is absent.
2. A new write envelope exists in `internal/connector/google/pipedream_write.go`:
   request `{schema_version:1, action:"create_event", calendar_id, event_id,
   start, end, summary, description}` and response
   `{schema_version:1, action:"create_event", calendar_id, status, created,
   event}` / on failure `{schema_version:1, action:"create_event", calendar_id,
   status:"error", error}` — the same `calendar_id`/`status`/`error` keys the read
   entries use, at the top level because a write touches exactly one calendar.
   The request carries **no attendees field**, ever.
3. `(*PipedreamCalendarClient).CreateEvent(ctx, req) (CreateEventResponse, error)`
   POSTs to the same endpoint with the same Bearer header, the same
   `maxBytes+1` capped read, and the same status-error snippet. Its transport
   error is CLASSIFIED, never `%w`-wrapped, and mentions neither the token nor
   the endpoint — asserted by a unit test that points the client at an
   unreachable host and greps the error for the host and the token.
4. `CreateEvent` VERIFIES the response before returning success, refusing on
   any of: `schema_version != 1`; `action != "create_event"`;
   `status != "ok"`; `strings.EqualFold(calendar_id, req.CalendarID)` false;
   the returned event's `id` not byte-equal to the requested `event_id`; the
   returned event's start/end not equal to the requested ones **as instants**;
   the returned event carrying any `attendees`. Each refusal names which check
   failed.
5. The start/end echo is compared by parsing both sides to `time.Time` and using
   `Equal`, NOT by string comparison — Google re-serializes offsets and a byte
   comparison across a provider round trip is the repo's oldest recorded
   landmine (IK: "Exact text comparison across a provider round trip"). The unit
   test proves it with a response echoing `+02:00` for a request written in `Z`.
6. `google.CalendarEventID(deliveryID int64, nonce int64) string` produces
   `"sb" + strconv.FormatInt(deliveryID, 32) + "t" + strconv.FormatInt(nonce, 32)`.
   A unit test asserts every character of a table of outputs is in `[0-9a-v]`
   (Google's base32hex id alphabet) and the length is ≥ 5.
   `google.CalendarExternalID(eventID string) string` returns
   `"calendar:" + eventID` and is the ONE spelling: `ingest.go:312`,
   `pipedream_ingest.go:270` and `bridge_ingest.go:262` are changed to call it.
7. `CreateEvent` retries EXACTLY ONCE, and only on a transport error or context
   deadline — never on any HTTP response, however shaped. A retry is safe only
   because the id is ours and a duplicate insert answers 409; a non-2xx response
   proves the workflow was reached and must not be re-driven. A unit test drives
   a server that fails the first connection and succeeds the second, and a second
   test asserts a 500 produces one request, not two.

### B. Schema

8. Migration `migrations/0020_calendar_booking.sql` (forward-only, numbered):
   - `ALTER TABLE source_accounts ADD COLUMN calendar_write_enabled BOOLEAN NOT
     NULL DEFAULT false;`
   - `ALTER TABLE deliveries ADD COLUMN starts_at TIMESTAMPTZ, ADD COLUMN
     ends_at TIMESTAMPTZ;`
   - `ALTER TABLE deliveries ADD CONSTRAINT deliveries_calendar_identity_check
     CHECK (channel <> 'calendar' OR (starts_at IS NOT NULL AND ends_at IS NOT
     NULL AND ends_at > starts_at AND target_ref IS NOT NULL AND from_account_id
     IS NOT NULL));`
   The CHECK is the 0019 pattern and for the same reason: a calendar row missing
   its interval would not error, it would silently become a delivery nothing can
   book or confirm. Free at zero rows — the migration comment instructs verifying
   `SELECT count(*) FROM deliveries WHERE channel='calendar'` = 0 first (it is 0
   in production today; the channel has never been live).
9. No new table. No new index (the existing unique
   `(channel, sent_external_id)` serves the confirm lookup).

### C. draft_delivery gains the calendar channel

10. `draftDeliveryArgs` gains `Start string \`json:"start,omitempty"\`` and
    `End string \`json:"end,omitempty"\`` (RFC3339). `validateDraftDelivery`
    accepts `"calendar"` and, for it, requires: `target_ref` non-empty,
    `subject` non-empty (it becomes the event summary), `body` non-empty (it
    becomes the description — the recorded reason the block exists), `start` and
    `end` parseable RFC3339, `end` after `start`, and
    `end - start <= 12h`. The 12-hour cap is a fat-finger guard: a typo'd end
    DATE would blanket the calendar and make `propose_slots` refuse everything
    downstream.
11. `draftDelivery`'s calendar branch resolves the account SERVER-SIDE from
    `target_ref`, exactly as the jira and gmail branches resolve From: the row
    must be `provider='google'` AND `calendar_in_availability` AND
    `calendar_write_enabled`, matched case-insensitively on `account_email`. It
    stores `from_account_id` and stores `target_ref` as the **`account_email`
    read back from that row** — the database's spelling, never the caller's
    (the SWT-13 canonicalization rule; a non-canonical `target_ref` is how a
    delivery becomes permanently unconfirmable).
12. A `calendar` draft with no matching write-enabled in-scope account is
    refused by name, distinguishing the three causes (no such google account /
    not in availability scope / not write-enabled). Requiring
    `calendar_in_availability` is load-bearing: `LoadBusy` only reads in-scope
    calendars, so booking onto an out-of-scope one would be booking blind.
13. `starts_at`/`ends_at` are stored on the row. `subject` and `body` continue to
    pass through `google.ScrubAIAttribution` at insert (existing code,
    `delivery.go:332`) — that is invariant 6 for this channel, and the send
    handler reads the STORED values, never the caller's.

### D. Policy — the auto tier (Q1 = b)

14. `policy.Decide` gains a `case "calendar":` branch, rate-limited like
    gmail/jira (`OPS_SEND_HOURLY_LIMIT`, default 10), allowing both
    `send_delivery` and the new `book_calendar_block`. The channel no longer
    falls into `channel_not_live`. Pure-function unit tests cover: both verbs
    allowed within limit; `rate_limit` at the limit for each; `kill_switch` when
    frozen for each.
15. **`book_calendar_block` is DENIED on every channel other than `calendar`,
    in `Decide`, by name** — a guard evaluated before the channel switch, with
    its own rule string (`channel_mismatch`). Without it, premise 14 means the
    gmail branch would allow it and the handler's channel check would be the only
    thing between an agent and an unapproved client email. Unit tests pin a deny
    for each of `gmail`, `jira_comment`, `upwork_chat`, `slack_reply`, and for an
    empty channel (a delivery id that resolved to nothing).
16. **`book_calendar_block` is NOT in `policy.humanOnly`, and `humanOnly` is not
    otherwise changed.** A test pins that the verb is allowed for every actor
    shape that exists in this repo — `dashboard:`, `opsctl:`, `manual:`,
    `mcp:manual:salvo`, `mcp:worker:acme`, bare `worker:acme`, `drafts:gpt`,
    `capture:google` — because the recorded defect in this repo was a gate that
    keyed on the caller and a test that pinned only one shape.
17. **`book_calendar_block` is `sendShaped` and `freezeGated`**: it consumes the
    channel's hourly allowance and the global kill switch stops it. Its args are
    `{delivery_id}` only, so `pgloader` resolves the channel snapshot with no
    loader change (premise 15). `send_delivery` on a calendar row remains
    available and remains human-only — the explicit two-step for a human who
    wants to look first; both verbs route to the same send half (criterion 18).
    `mark_delivery_sent` and `mark_delivery_failed` stay refused for `calendar`:
    this channel has a real send path and a reservable id, so it has neither an
    assisted tier nor a click-may-have-landed window.

### E. The booking path

18. `book_calendar_block` is registered in `internal/tools/createtask.go`'s
    `Register` with `validateDeliveryIDOnly` (reused, no new arg shape). Its
    handler `bookCalendarBlock`, in one transaction: locks the row `FOR UPDATE`;
    refuses unless `channel='calendar'` (the inner of the two gates from
    criterion 15); refuses unless `status` is `drafted` or `approved`; sets
    `status='approved'`, `approval_source='switchboard'`; inserts an `approvals`
    row with `executor.ActorFrom(ctx)` so WHO booked is recorded even when it is
    a worker — then calls the same `sendCalendarBlock` the human path uses.
    `sendDelivery` routes `channel == "calendar"` to that same
    `sendCalendarBlock`. There is exactly ONE code path that talks to the write
    route.
19. **The pre-flight refusal.** Before any row mutation, `sendCalendarBlock`
    calls `availability.LoadBusy` with `WindowStart=starts_at`,
    `WindowEnd=ends_at`, `Now=time.Now()`, `MaxSyncAge` from the existing
    `availabilityConfig()`, and `HorizonPast/Future = google.CalendarWindowPast/
    Future` (the same injection `proposeSlots` does). It refuses, leaving the row
    **`approved` and otherwise untouched**, when: `LoadBusy` errors (stale sync,
    empty scope, outside horizon — the error propagates VERBATIM so
    `audit_events.error` reads exactly like a `propose_slots` refusal), or any
    returned interval overlaps `[starts_at, ends_at)`. The refusal is an
    executor error, so the audit row records it; nothing is marked failed,
    because a retry after the conflict clears must remain possible. This is the
    backstop the auto tier rests on, and it is the same refusal for a worker and
    for Salvador.
    **Amendment (2026-09-07, codex adversarial review — two high findings,
    both fixed):** (a) the pre-flight alone was an unlocked snapshot: two
    overlapping bookings could both see a free slot and both reserve. The
    pre-flight and the phase-1 reserve now run in ONE transaction under a
    global `pg_advisory_xact_lock` (allocation is serialized across the whole
    availability scope — busy is merged across calendars, so a per-account
    key would still double-book the human), and an UNCONFIRMED calendar
    delivery (sending / sent / failed-with-id, `confirmed_at IS NULL`) is
    itself LoadBusy-visible as a reservation (`availability.loadReservations`),
    lifted at confirmation when the observed event takes over. (b) a poll
    whose snapshot was fetched before a booking landed could supersede the
    block's send-time record — and identical bytes on the next snapshot would
    leave it cancelled forever; `SupersedeAbsentCalendar` now refuses to
    supersede a raw row matching an unconfirmed calendar delivery of the same
    account. Residual accepted risk: concurrent bookings can overshoot the
    hourly rate limit by the number in flight (the limit is a volume brake,
    not a quota); a block hand-deleted before its first observing poll
    stays reserved until an operator intervenes (over-busy direction); and
    the allocation transaction holds a pool connection plus the advisory
    lock while LoadBusy acquires a second connection, so >= MaxConns
    simultaneous bookings could stall — bounded by a 30-second deadline on
    the whole send (a clean timeout that leaves the row approved and
    retryable) rather than restructured (delta review F1, accepted).
20. **Phase 1 (tx), the gmail shape verbatim.** Lock the row `FOR UPDATE`;
    refuse if `sent_external_id` is present ("never resend (invariant 4)"); refuse
    unless `status='approved'`; refuse unless `approval_source='switchboard'`;
    re-read `calendar_write_enabled` from the joined account and refuse if false
    (the go-live gate must be checked at SEND, not only at draft — a draft can
    predate a revocation, and under the auto tier that column is the only
    per-account consent); then commit, in one statement, `status='sending'`,
    `sent_external_id = google.CalendarExternalID(google.CalendarEventID(
    deliveryID, time.Now().UnixNano()))`, `send_attempted_at=now()`.
21. **Phase 2, the network call**, bounded by a 30s context (opsctl's existing
    deadline shape). On any error: `status='failed'`, `error=<classified
    message>`, **`sent_external_id` kept** (Decisions). On success:
    `status='sent'`, `sent_at=now()`, `error=NULL`, then a `delivery_sent`
    task_event with `{delivery_id, channel:"calendar", sent_external_id}` —
    the same payload shape the gmail and jira branches emit. The `channel` key is
    load-bearing: criterion 29's rule reads it.
22. **The busy set learns about the block immediately.** After the row reaches
    `sent`, the handler stores the returned event resource through a new
    `func (s *PGSink) RecordOwnCalendarEvent(ctx context.Context, accountID
    int64, raw json.RawMessage) error` in package `google`, which: validates
    through `NormalizeCalendarEvent` (the same mapper `Normalize` runs), then
    `upsertRaw` under `CalendarExternalID(id)` (raw-first: provider JSON
    verbatim + `content_hash`), then `s.upsertEvent`, then `markNormalized`.
    All `normalized_events` SQL stays inside `sink.go`, so
    `availability/callsites_test.go` still passes. Without this the block is
    invisible to `propose_slots` for up to 20 minutes and switchboard can
    propose — and book — the same slot twice; under the auto tier that would be
    a self-inflicted double-booking loop, not a rare race.
23. The record in criterion 22 is BEST-EFFORT and never changes the delivery's
    status: on failure the handler emits a `log` task_event naming the delivery
    and the cause and returns `"busy_set_pending": true` in its result. The next
    read poll heals it. It must not touch `deliveries.error` — that column
    carries the reconcilers' fire-once markers and writing to it here would need
    a re-arm rule (IK: "an alarm whose fire-once marker is never cleared").
24. **Wiring.** `cmd/opsctl`, `cmd/dashboard` AND `cmd/ops-mcp` wire the booker
    via `tools.SetCalendarBooker` when `PIPEDREAM_CALENDAR_URL` is configured
    (the `NewDeliveryBridgeFromEnv` shape: a construction error is fatal, an
    absent configuration leaves the seam nil and booking refused by name).
    `cmd/ops-mcp` is new to this list and is required by the auto tier — without
    it every worker call would fail "no calendar booking adapter wired".
25. **MCP surface.** `book_calendar_block` is added to
    `internal/mcpserver/schemas.go` (input `{"delivery_id":{"type":"integer"}}`,
    required) and to the agent allowlist in
    `internal/mcpserver/adapter_test.go`, with a comment saying why it is
    agent-facing where `send_delivery` is not. `draft_delivery`'s schema gains
    `calendar` in the channel enum plus `start`/`end`, and the existing
    `TestListTools_ExactlyAgentAllowlist` must go green on the new exact list.

### F. Loop closure

26. `internal/connector/google/normalize.go`'s `calendar:` branch calls a new
    `sink.confirmCalendarDelivery(ctx, rawItemID, externalID)` after
    `upsertEvent`. It matches `channel='calendar' AND sent_external_id=$1 AND
    from_account_id = (SELECT source_account_id FROM raw_source_items WHERE
    id=$2) AND confirmed_at IS NULL`, stamps `confirmed_at=now()`, and emits one
    `delivery_confirmed` task_event with `{delivery_id, matched_event_id}` —
    the `confirmDelivery` shape (`sink.go:312-343`), including the
    `confirmed_at IS NULL` guard plus a `RowsAffected` check so a
    `--normalize-only --all` replay emits no second event.
    **Amendment (2026-09-07, found by the live smoke):** the normalize hook
    alone never fires for our own blocks — criterion 22's send-time record
    stamps `normalized_at`, so the next poll's `content_hash` short-circuit
    means Normalize never revisits the row. The poll OBSERVING the event id is
    the loop-closure evidence, so `CalendarSnapshotSink` gains
    `ConfirmObservedCalendarDeliveries(ctx, accountID, present)` and
    `RunPipedreamCalendar` calls it with each verified snapshot's `present`
    set (idempotent via `confirmed_at IS NULL`; account-scoped). The
    normalize hook stays for the changed-bytes case. Verified live: one
    `delivery_confirmed` after the first poll, none after the second.
27. Confirmation stamps `confirmed_at` ONLY — never a status promotion. A
    calendar row is already `sent`; the lifecycle transition belongs to the path
    that owns it (the comment at `sink.go:605-611` states the rule).
28. No new task is ever created from a re-ingested calendar event: the calendar
    branch of `Normalize` writes `normalized_events` and nothing else, capture
    rules and triage read `normalized_messages`, and this ticket adds no
    task-creating path. An integration test asserts `count(*) FROM tasks` is
    unchanged across a poll that re-ingests a booked block. The booked block IS
    in the busy set afterwards and `propose_slots` stops offering it — correct,
    not a regression, and asserted directly.

### G. Orchestrator (Q2 = b)

29. `ruleDeliveryLifecycle` (`internal/orchestrator/rules.go:274`) returns nil —
    no `task_mark_delivered`, no `task_close`, and **no `record_orchestration`**
    — when the event payload's `channel` is `"calendar"`, read with the existing
    `payloadStr` helper. A booking therefore never advances a task's lifecycle
    and never burns the task's `delivery_lifecycle` dedup key, so a later real
    delivery on the same task still fires R8 normally. Pure-rule tests in
    `internal/orchestrator/rules_r8_test.go` (zero I/O, invariant 7):
    a calendar `delivery_sent` yields zero actions; a gmail one is unchanged; and
    an event with NO `channel` key behaves exactly as today (the absent key must
    not become a silent skip — every historical `delivery_sent` payload carries
    the channel, but the rule must not depend on that).
    A task whose deliverable IS the booking is closed by its worker with
    `mark_done_local` / `task_close`, as any other locally-finished task is.

## Data model changes

`migrations/0020_calendar_booking.sql` — see criterion 8. Vocabulary is the
existing one: `deliveries`, `source_accounts`, `raw_source_items`,
`normalized_events`, `approvals`, `task_events`, `audit_events`,
`policy_decisions`. No new table, no synonym.

| table | column | type | note |
|---|---|---|---|
| `source_accounts` | `calendar_write_enabled` | BOOLEAN NOT NULL DEFAULT false | per-account go-live, flipped by hand (`send_enabled`'s convention); under the auto tier this is the only per-account consent |
| `deliveries` | `starts_at` | TIMESTAMPTZ | calendar rows only |
| `deliveries` | `ends_at` | TIMESTAMPTZ | calendar rows only |
| `deliveries` | — | CHECK `deliveries_calendar_identity_check` | a calendar row cannot exist without its interval, target and account |

Existing columns used unchanged: `channel='calendar'` (already legal),
`target_ref` (the calendar id = account email, canonicalized from the DB row),
`from_account_id`, `subject` (summary), `body` (description),
`sent_external_id` (`calendar:{event_id}`, set pre-network),
`send_attempted_at`, `sent_at`, `confirmed_at`, `approval_source`, `error`.

## API / MCP tool changes

Every one of these is registered in `internal/tools/createtask.go`'s `Register`
and therefore runs validate → policy → audit start → handler → audit complete
(invariant 3).

| tool | change | surface |
|---|---|---|
| `draft_delivery` | accepts `channel:"calendar"` + new `start`/`end` args; resolves the account server-side | agent-facing (MCP-listed) |
| `book_calendar_block` | **NEW** — approve + send a drafted calendar row in one audited call | agent-facing (MCP-listed), NOT human-only, sendShaped + freezeGated |
| `send_delivery` | new `calendar` branch → `sendCalendarBlock` | human-only (unchanged); the explicit two-step |
| `approve_delivery` | unchanged | human-only |
| `propose_slots` | unchanged | unchanged |

Request shape for the draft:

```json
{"task_id": 42, "channel": "calendar", "target_ref": "sspataro@gmail.com",
 "subject": "Focus block", "body": "reserved for SWT-28 review",
 "start": "2026-09-08T15:00:00+02:00", "end": "2026-09-08T15:15:00+02:00"}
```

Response: `{"delivery_id": N}` (unchanged shape).

`book_calendar_block` request: `{"delivery_id": N}`.
Response: `{"delivery_id": N, "status": "sent", "sent_external_id":
"calendar:sb1zt..."}`, plus `"busy_set_pending": true` when criterion 23's local
record failed.

## The Pipedream contract (HUMAN step, not code in this repo)

The workflow edit is done in a browser at pipedream.com on
`switchboard-calendar` (project stealth-fun-natural) and is documented in
`docs/runbooks/calendar-availability.md`. This repo owns only the Go client.

- **Backward compatibility is the first requirement.** The trigger step gains a
  branch on `action`: **absent or empty ⇒ the existing read path, unchanged**.
  The read path's code must not be edited at all. Verify by re-running one live
  read poll BEFORE touching anything else.
- On `action == "create_event"`: reject unless `calendar_id` is one of the three
  connected accounts (`status:"error"`); call `calendar.events.insert` on that
  account with `sendUpdates=none` and body
  `{id: <event_id>, summary, description, start:{dateTime:<start>},
  end:{dateTime:<end>}}` — **no `attendees` key, ever**, and no description
  footer of any kind (invariant 6).
- On HTTP **409** (duplicate id) call `events.get(event_id)` and return that
  event with `created:false`. This is what makes a retried timeout safe.
- Respond `{schema_version:1, action:"create_event", calendar_id, status:"ok",
  created:<bool>, event:<the Google Calendar v3 Event resource VERBATIM>}`.
  Verbatim matters twice: the connector's echo checks read `id`/`start`/`end`
  from it, and the resource is stored raw (criterion 22).
- Any failure: `{schema_version:1, action:"create_event", calendar_id,
  status:"error", error:"<message>"}` — the read path's per-calendar error shape.
- `curl` the write once by hand with a throwaway id and time it; that timing is
  part of the human step, as it was for the read.

Config is unchanged: `PIPEDREAM_CALENDAR_URL` + `PIPEDREAM_CALENDAR_TOKEN_FILE`
(preferred) / `PIPEDREAM_CALENDAR_TOKEN`. Env only, never a DB row, never
printed, never in an error string.

## MQTT topics

None. This ticket publishes and subscribes to nothing.

## Files likely to touch

Existing (paths verified):

- `internal/connector/google/pipedream.go` — add `Action` to
  `PipedreamCalendarRequest` (criterion 1).
- `internal/connector/google/pipedream_write.go` — **new**: write envelope,
  `CreateEvent`, echo verification, the single retry.
- `internal/connector/google/calendarids.go` — **new** (or fold into
  `pipedream_write.go`): `CalendarEventID`, `CalendarExternalID`.
- `internal/connector/google/ingest.go:312`,
  `internal/connector/google/pipedream_ingest.go:270`,
  `internal/connector/google/bridge_ingest.go:262` — call `CalendarExternalID`.
- `internal/connector/google/sink.go` — `RecordOwnCalendarEvent`,
  `confirmCalendarDelivery`.
- `internal/connector/google/normalize.go:329-336` — call
  `confirmCalendarDelivery`.
- `internal/tools/delivery.go` — `CalendarBooker` seam + `SetCalendarBooker`,
  `draftDeliveryArgs.Start/End`, `validateDraftDelivery`, `draftDelivery`
  calendar branch, `sendDelivery` routing, `sendCalendarBlock`,
  `bookCalendarBlock`.
- `internal/tools/createtask.go:39-89` — register `book_calendar_block`.
- `internal/policy/matrix.go` — the `case "calendar":` branch, the
  `channel_mismatch` guard, `sendShaped` + `freezeGated` entries.
- `internal/mcpserver/schemas.go`, `internal/mcpserver/adapter_test.go` —
  the new agent tool and the exact-allowlist test.
- `internal/orchestrator/rules.go:274`,
  `internal/orchestrator/rules_r8_test.go` — the calendar skip (criterion 29).
- `cmd/opsctl/main.go:147-171`, `cmd/dashboard/main.go:46-78`,
  `cmd/ops-mcp/main.go:50-66` — wire the booker.
- `internal/dashboard/server.go` + `internal/dashboard/templates/deliveries.html`
  — show `starts_at`/`ends_at` for calendar rows, so a human reviewing what was
  booked can see WHEN. Minimal: one extra column, rendered only when non-null.
  Under the auto tier this is the review surface, not an approval surface.
  *(Amended 2026-09-07: the template also gained a Book button on approved
  calendar rows, posting to the existing `/deliveries/{id}/send` route — the
  human two-step of criterion 17, still behind the human-only `send_delivery`
  gate. Review flagged it as beyond this note; kept deliberately.)*
- `migrations/0020_calendar_booking.sql` — **new**.
- `docs/runbooks/calendar-availability.md` — a new section, "Booking an own
  block (SWT-28)": the workflow edit, the `calendar_write_enabled` flip, reading
  a refusal, the ambiguous-failure recovery, and how to stop an unattended
  booking loop (the kill switch, `set_sending_frozen`).
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` — extend the SWT-27 Pipedream section
  with the write contract's invariants and the auto-tier gate list.

## In scope / Out of scope

**In scope**: the `create_event` request/response contract and its Go client;
`channel='calendar'` as a live **auto-tier** delivery channel end to end (draft →
policy → `book_calendar_block` → confirm), reachable by a worker over MCP; the
pre-send conflict/freshness/horizon refusal; migration 0020; the immediate local
record so the busy set is current; loop closure; the R8 calendar skip; the
runbook; a one-column dashboard change.

**Out of scope** — named because they are the adjacent things a session would be
tempted to bundle:

- **Calendar invites with attendees.** A different policy-matrix row (approve
  tier), a different consent surface in Pipedream (Google sends mail on behalf of
  the account), and a different failure model. The write request in this ticket
  has no attendees field precisely so that work cannot half-arrive. In
  particular: `book_calendar_block` must NOT grow an attendees argument later —
  that would silently promote the approve-tier row to auto.
- **Updating or deleting a booked event.** No `update_event`/`delete_event`
  action, no `cancel_delivery` verb. Undoing a booking is a browser click plus
  the next poll's snapshot replacement.
- **Any automatic BOOKER.** Nothing in this ticket creates a calendar delivery on
  its own: no orchestrator rule, no cron, no worker prompt change. The auto tier
  means a worker MAY book when it decides to; it does not mean anything books on
  a schedule. A "reserve focus time from the morning brief" rule is a separate
  ticket with its own risk argument.
- **Recurring blocks, all-day blocks, timezone-preference logic.** The workflow
  is queried with `singleEvents=true` and the ingest REFUSES any event carrying a
  `recurrence` rule (`pipedream_ingest.go:258-262`), so writing one would stall
  the next poll for that account.
- **The morning brief reading calendar titles**, and anything else in build-order
  step 10.
- **Kube manifests.** No new deployment; the existing `connector-gcal` CronJob is
  unchanged. If a secret or image bump is needed, that is the kube session's
  commit (IK: "Kube manifests belong to the kube session").

## Invariants that apply

1. **Raw-first.** Two writes, both raw-first. (a) The send handler stores the
   Google Event resource VERBATIM via `upsertRaw` inside
   `PGSink.RecordOwnCalendarEvent` — content hash first, normalize second, in
   that order, in that function. (b) The next `*/20` read poll upserts the same
   `calendar:{id}` external id with the same short-circuit
   (`pipedream_ingest.go:276-282`). Nothing in this ticket writes
   `normalized_events` without a raw row behind it.
2. **One funnel.** No new table and no task-like row. A booking is a
   `deliveries` row hanging off an existing `tasks` row; `deliveries.task_id` is
   NOT NULL and this ticket does not relax that.
3. **Everything through the executor.** `book_calendar_block` is a registered
   tool with `validateDeliveryIDOnly` → `policy.Decide` → audit start → handler →
   audit complete; there is no side door and no second entry point.
   `sendCalendarBlock` is unexported and reachable only from the two registered
   handlers; the `CalendarBooker` seam has exactly one caller. Under the auto
   tier this matters more, not less: an unattended booking still writes an
   `audit_events` row with its actor, args and outcome, and a
   `policy_decisions` row for the gate it passed.
4. **Nothing external without a delivery row.** The Pipedream write route is
   unreachable except from a `deliveries` row that is `approved` with
   `approval_source='switchboard'` — including on the auto path, which writes
   that approval (and an `approvals` row naming the actor) inside the same
   transaction rather than skipping it. `sent_external_id` is committed with
   `status='sending'` BEFORE the POST (criterion 20); a present
   `sent_external_id` refuses a resend forever; the unique index
   `(channel, sent_external_id)` makes that structural rather than hopeful; and
   no failure path clears it.
5. **Own-message loop closure.** `confirmCalendarDelivery` matches the
   re-ingested event to its delivery by exact external id scoped to
   `from_account_id`, stamps `confirmed_at` and attaches `delivery_confirmed` to
   the task. Our own block is never re-triaged — structurally, because the
   calendar branch of `Normalize` creates no messages and capture/triage read
   `normalized_messages` only (criterion 28 asserts it rather than assuming it).
6. **Stealth attribution.** `subject`/`body` are scrubbed by
   `google.ScrubAIAttribution` at draft time and the send reads the stored
   values, so the event summary and description cannot carry a byline. The
   workflow spec forbids a generated footer. Nothing about the event says
   switchboard except the id, which is not client-visible (own calendar, no
   attendees).
7. **Orchestrator purity.** The one orchestrator change (criterion 29) is a
   channel test inside the pure `ruleDeliveryLifecycle`, over the payload the
   event already carries: no I/O, no provider import, no clock read, unit-tested
   with zero network. `internal/orchestrator` gains no new dependency.

## Sibling patterns to copy

- **The write client**: `internal/connector/google/pipedream.go:100-199` — the
  constructor's validation, `PipedreamTokenFromEnv`, the `maxBytes+1`
  `LimitReader`, the 200-char status snippet, and above all the CLASSIFIED
  transport error. Read its header comment (`:15-19`) before writing a single
  error string.
- **The send handler**: `internal/tools/delivery.go:506-653` (gmail) for the
  two-phase shape and the pre-network id commit; `:888-967` (jira) for the
  compact single-target variant. Do NOT copy `sendSlackReply`'s fence and lease
  machinery — those exist because a browser click reserves no id, which is the
  opposite of this channel's situation.
- **The approve half of `book_calendar_block`**: `approveDelivery`
  (`delivery.go:420-464`) — the `approval_source='switchboard'` write in the same
  statement as the status transition, and the `approvals` insert carrying
  `executor.ActorFrom(ctx)`. Reuse the shape; do not reuse the human-only tool.
- **An agent-facing tool that is deliberately narrow**: `mark_delivery_sent`'s
  MCP narrowing comment (`delivery.go:680-698`) is the register to write
  `book_calendar_block`'s doc comment in — say what an injected call can and
  cannot do, in the handler, next to the check.
- **The migration**: `migrations/0019_delivery_provenance.sql` — the
  channel-scoped CHECK with the "free at zero rows, impossible later" comment.
- **Loop closure**: `internal/connector/google/sink.go:312-343`
  (`confirmDelivery`) — exact-id match, `confirmed_at IS NULL` guard, one event.
  The four post-hoc BODY matchers named in IK are NOT the pattern here: this
  channel matches on an id we chose, so there is no content comparison, no
  `textmatch.NormalizedPrefix`, and no attempt-time floor to get wrong.
- **The one door**: `internal/availability/store.go:38-64` — call `LoadBusy`,
  never write a second `normalized_events` query
  (`internal/availability/callsites_test.go` will fail the build if you do).
- **The pure rule + its test**: `internal/orchestrator/rules.go:274-291` and
  `rules_r8_test.go` — the existing R8 test file already documents its contract
  in a header comment; extend it in the same voice.
- **Fail-closed error text**: `docs/runbooks/calendar-availability.md:13-45` —
  a booking refusal should read like a `propose_slots` refusal, because it is
  the same refusal.

## Verification protocol

Before commit, in this order.

**1. Unit.**
```
go build ./... && go test ./...
```
Must include: the read request marshals without an `action` key; the echo
checks (each refusal reason, plus the `+02:00` vs `Z` instant comparison); the
id alphabet; the retry-once/no-retry-on-500 pair; the classified transport error
containing neither host nor token; `policy.Decide` for calendar (both verbs
allowed / `rate_limit` / `kill_switch`); the `channel_mismatch` deny for
`book_calendar_block` on each other channel; the eight-actor-shape allow list for
`book_calendar_block`; `validateDraftDelivery` for calendar (missing target_ref,
missing subject, bad RFC3339, end before start, over 12h); the R8 calendar skip
(zero actions, no `record_orchestration`), the gmail control, and the
absent-`channel` control.

**2. Integration** — compose db on **:5433**, never `192.168.50.49`.
```
make integration
```
Must include, with a fake `CalendarBooker` and no network:
- migration 0020 applied; the CHECK refuses a calendar row with a NULL
  `starts_at` (assert the constraint by name);
- `book_calendar_block` called with a NON-human actor (`mcp:worker:itest`)
  succeeds end to end — this is the auto tier's central claim and must be
  asserted with an actor a human gate would have refused;
- `book_calendar_block` on a `gmail` delivery is DENIED by policy with rule
  `channel_mismatch`, and the gmail row is untouched afterwards;
- a booking REFUSED because the in-scope calendar's last `ok`/`phase=calendar`
  `sync_runs` row is older than `AVAIL_MAX_SYNC_AGE` — and the row is still
  `approved` afterwards. Seed the freshness rows RELATIVE to `now()`
  (`now() - interval '90 minutes'`), never as literal timestamps that age out;
- a booking REFUSED because a seeded `normalized_events` row overlaps — and the
  regression test must fail if the guard's input column is dropped from the
  SELECT (IK: "test the column, not the fixture"; mutate `LoadBusy`'s query to a
  literal and watch it go red);
- a booking REFUSED because `calendar_write_enabled` is false at send time even
  though it was true at draft time (mutate the column between the two calls) —
  the go-live gate is the auto tier's per-account consent and must bite at send;
- a booking REFUSED while `sending_frozen` is set (kill switch reaches the auto
  verb);
- a successful booking: `status='sent'`, `sent_external_id` non-null and prefixed
  `calendar:`, an `approvals` row naming the actor, one `delivery_sent`
  task_event, a `raw_source_items` row with the matching `external_id` and a
  `normalized_events` row behind it, and `propose_slots` no longer offering that
  slot;
- re-running `book_calendar_block` on the same row refuses with the invariant-4
  message;
- a normalize pass over the same raw item stamps `confirmed_at` and emits ONE
  `delivery_confirmed`; a second pass emits none; `count(*) FROM tasks` unchanged;
- an orchestrator drain over the `delivery_sent` event leaves the task's status
  unchanged and writes NO `delivery_lifecycle` orchestration row (criterion 29 at
  the integration level, not just in the pure rule).
Clean up in FK order, scoped by a test-owned account email
(`itest-calendar-book-%`) — and join the mutual-cleanup pact if any assertion is
a global count (IK: integration suites cross-pollute, `go test -p 1`).

**3. Live smoke — the "usable alone" check.** This one DOES use production; the
`192.168.50.49` ban is on integration tests, not on a deliberate manual smoke.

```bash
eval "$(grep '^export OPS_DATABASE_URL=' ~/.bashrc)"
export PIPEDREAM_CALENDAR_URL=... PIPEDREAM_CALENDAR_TOKEN_FILE=...

# 0. Read path still works AFTER the workflow edit (the backward-compat check).
DATABASE_URL="$OPS_DATABASE_URL" CAL_SOURCE=pipedream \
  go run ./cmd/connectors/google --calendar-only
psql "$OPS_DATABASE_URL" -c "SELECT status, stats->>'calendar_source', finished_at \
  FROM sync_runs WHERE stats->>'phase'='calendar' ORDER BY id DESC LIMIT 3"

# 1. A slot, from the service that is supposed to choose it.
DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/opsctl call --tool propose_slots \
  --args '{"duration_minutes":15,"count":1}'

# 2. Go live for ONE account.
psql "$OPS_DATABASE_URL" -c "UPDATE source_accounts SET calendar_write_enabled=true \
  WHERE provider='google' AND account_email='sspataro@gmail.com'"

# 3. A task to hang it on, then draft and book — TWO calls, no approval step.
DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/opsctl call --tool create_task \
  --args '{"project":"<slug>","title":"calendar-booking smoke"}'
DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/opsctl call --tool draft_delivery \
  --args '{"task_id":N,"channel":"calendar","target_ref":"sspataro@gmail.com",
           "subject":"Focus block (switchboard smoke)",
           "body":"calendar-booking smoke; delete after verifying",
           "start":"<slot.start>","end":"<slot.end>"}'
DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/opsctl call --tool book_calendar_block \
  --args '{"delivery_id":D}'
```

Then assert, in order:

1. **The block is on the calendar.** Open Google Calendar for
   `sspataro@gmail.com` and see it at the proposed time, with no attendees.
2. `psql -c "SELECT status, approval_source, sent_external_id, starts_at,
   ends_at, confirmed_at FROM deliveries WHERE id=D"` → `sent`, `switchboard`,
   `calendar:sb…`, the interval, NULL confirmed_at. And
   `psql -c "SELECT decided_by FROM approvals WHERE subject_type='delivery' AND
   subject_id=D"` → the opsctl actor.
3. **`propose_slots` no longer offers that slot** — re-run step 1 IMMEDIATELY
   (before any poll). This is criterion 22: if it still offers it, the local
   record did not land.
4. **The kill switch works on the auto verb**: `set_sending_frozen {"frozen":true}`,
   draft a second block, `book_calendar_block` → denied with rule `kill_switch`;
   then unfreeze. This is the operator's stop button for an unattended booker and
   must be exercised once by hand, not only in a unit test.
5. **Loop closure**: run the read poll again, then
   `psql -c "SELECT confirmed_at FROM deliveries WHERE id=D"` → non-null, and
   `psql -c "SELECT event_type FROM task_events WHERE task_id=N ORDER BY id"` →
   `delivery_sent` then `delivery_confirmed`, exactly one each — and NO
   `status_changed` to `delivered` (criterion 29).
6. **Idempotency, by hand**: re-run `book_calendar_block` → refused with the
   invariant-4 message, and no second block appears.
7. **Audit**: `psql -c "SELECT tool, actor, status, error FROM audit_events WHERE
   tool IN ('draft_delivery','book_calendar_block') ORDER BY id DESC LIMIT 6"` →
   every call recorded with its actor, refusals carrying their reason.

**Cleanup** (do all four):
```bash
# a. Delete the event in the Google Calendar UI.
# b. Re-poll; the snapshot replacement supersedes the raw row and marks the
#    normalized event cancelled, so the slot frees itself.
DATABASE_URL="$OPS_DATABASE_URL" CAL_SOURCE=pipedream go run ./cmd/connectors/google --calendar-only
DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/opsctl call --tool propose_slots --args '{"duration_minutes":15,"count":1}'
# c. Turn the account back off until the ticket is delivered.
psql "$OPS_DATABASE_URL" -c "UPDATE source_accounts SET calendar_write_enabled=false \
  WHERE provider='google' AND account_email='sspataro@gmail.com'"
# d. Close the smoke task (leave the delivery row — it is the audit trail).
DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/opsctl call --tool task_close --args '{"task_id":N,"reason":"smoke"}'
```

**4. `/ticket-review`** — the go-reviewer pass against the seven invariants,
with the adversarial codex pass, which is not optional here: this diff adds an
agent-callable verb that writes to the outside world, and touches executor,
policy and delivery code (CLAUDE.md, Harness).

## Future work (NOT this ticket)

- Calendar invites with attendees (the approve-tier matrix row).
- `update_event` / `delete_event` actions and a cancel verb, which need a
  compensating lifecycle transition — the same gap SWT-20 recorded for
  `mark_delivery_failed`.
- A reconciler for calendar deliveries stuck `failed` with an id, in the shape of
  `slackweb/reconcile.go` — worth having only once a second failure is observed
  in the wild (google and jira still have none).
- A per-account daily booking budget, distinct from the per-channel hourly rate
  limit. The hourly limit is a global brake; if the auto tier turns out to want a
  per-calendar one, that is a policy change with its own ticket.
- A worker or cron that DECIDES to book (e.g. focus time from the morning brief).
  This ticket ships the capability; nothing yet exercises it unprompted.
