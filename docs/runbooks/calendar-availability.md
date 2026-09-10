# Calendar & availability (SWT-24)

`propose_slots` (executor tool, reachable via `opsctl call`) proposes free
slots from the merged busy set of every google calendar marked
`calendar_in_availability`. Since SWT-24 it **fails closed**: it refuses —
a named error, never `{"slots":[]}` — whenever it cannot prove it holds
current calendar data. Before SWT-24 an empty `normalized_events` table read
as "free all week"; that fail-open is gone on both axes (freshness and
window horizon).

## Reading a refusal

```
$ DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/opsctl call --tool propose_slots \
    --args '{"duration_minutes":30}'
opsctl: tool propose_slots: calendar not ready: sspataro@gmail.com last successful
calendar sync never; salvador@org.example last successful calendar sync
2026-08-29T04:11:07Z (older than max sync age 1h0m0s)
```

- **`never`** — no successful calendar sync has ever finished for that
  account. Either consent has not been run (see below) or the calendar phase
  has never executed.
- **a timestamp `(older than max sync age …)`** — the poller stopped. Check
  the connector CronJob/watch logs and `sync_runs` for `status='error'`
  rows with `stats->>'phase'='calendar'`.
- **`no google calendar is in availability scope`** — every
  `provider='google'` row has `calendar_in_availability=false`. An empty
  scope refuses on purpose: "nothing to consult" is not "you are free".
- **`window … is not fully inside the synced horizon`** — the request went
  beyond `[now-30d, now+90d]` (`google.CalendarWindowPast/Future`), the only
  span the connector ever fetches. Nothing was ever synced there, so an
  answer would be fabricated.

The same reason lands verbatim in `audit_events.error` with
`status='error'` — every refusal is audited exactly like an answer.

Readiness is the freshness of the **sync**, never the count of events: a
genuinely empty week with a fresh `status='ok'`, `stats->>'phase'='calendar'`
`sync_runs` row answers normally.

`AVAIL_MAX_SYNC_AGE` (Go duration, default `1h`) is the freshness window,
read only by the tool wiring. Keep it at more than twice the calendar poll
period. An unparseable value (e.g. a bare `720`) is an error, not a silent
fallback.

## The one human step: consent (per account)

Google CalDAV rejects app passwords (measured 2026-08-31: valid and garbage
passwords both return HTTP 401), so calendar read is OAuth-only. The consent
asks for **`calendar.readonly` and nothing else** — never the restricted
Gmail scopes that migration 0014 abandoned OAuth to avoid.

One-time GCP setup: a project with the Calendar API enabled, an OAuth
consent screen carrying only the `calendar.readonly` scope, and a
**Desktop-app** OAuth client whose JSON lives at
`~/.config/switchboard/google_client_secret.json`
(`GOOGLE_CLIENT_SECRET_FILE` overrides).

Then, per account, at a browser:

```
DATABASE_URL="$OPS_DATABASE_URL" OPS_TOKEN_KEY=… \
  go run ./cmd/google-auth add-calendar <email>
```

It verifies the authorized identity against the primary calendar's id (which
is the account address) and stores nothing on a mismatch. It touches only
`refresh_token_encrypted`, `scopes` and `calendar_in_availability`:
`auth_type` stays `app_password` and the app password survives, so the row
becomes dual-auth — IMAP/SMTP for mail, OAuth for calendar — and the live
mail path is unchanged.

One caveat: `add` and `add-calendar` each overwrite the row's
`refresh_token_encrypted` and `scopes` wholesale, and calendar-token rotation
re-persists the calendar-only scope set. Do not run both on the same account
expecting both consents to survive — the last one wins. (Production mail is
IMAP and never reads `scopes`, so mail is unaffected either way.)

## Verifying a first sync

```
DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/connectors/google --calendar-only --full
psql "$OPS_DATABASE_URL" -c "SELECT source_account_id, status, finished_at
   FROM sync_runs WHERE stats->>'phase'='calendar' ORDER BY id DESC LIMIT 5"
psql "$OPS_DATABASE_URL" -c "SELECT count(*) FROM normalized_events"
```

then re-run the `opsctl call` above and watch it answer with real busy time.
`--calendar-only` runs the calendar phase plus normalize and nothing else —
no mail ingest, no outbound observation, no capture-rules pass — so it is
safe to run beside the resident watch loop (per-account advisory locks, and
the calendar pass writes only its own `calendar_sync_token` cursor key).
In imap mode the plain one-shot pass also runs the calendar phase after
mail, for every account with a refresh token and the calendar scope; with
zero credentialed accounts it prints one line and exits 0.

Keeping it fresh is a scheduling question for the kube repo (a CronJob
calling `--calendar-only` at least every ~25 minutes for the default 1h
freshness window). Until that exists, `propose_slots` refuses honestly.

## The Pipedream transport (SWT-27) — the default going forward

`CAL_SOURCE=pipedream` swaps the calendar TRANSPORT: a Pipedream workflow
holds the Google OAuth grants and manages refresh; the cluster keeps no
Google calendar credential (`--calendar-only` then needs neither
`OPS_TOKEN_KEY` nor a client secret file). Everything else — fail-closed
readiness, raw-first, the horizon, normalize — is unchanged; `CAL_SOURCE`
unset (or `oauth`) is byte-for-byte SWT-24 and is the way back with no code
change. The OAuth path stays in the tree, dormant.

**The workflow shape** (create once at pipedream.com):
- HTTP trigger with a **custom response**; require
  `Authorization: Bearer <shared secret>` (generate ≥32 chars; refuse others).
- Connect the three Google accounts (read scope where offered).
- On each request: parse `{schema_version, time_min, time_max, calendars[]}`;
  for each requested calendar call `calendar.events.list` with
  `singleEvents=true`, `timeMin`/`timeMax` from the request, paging to
  completion; respond
  `{schema_version:1, time_min, time_max (echoed VERBATIM — the connector
  refuses a mismatch), calendars:[{calendar_id, status:"ok",
  event_count:<len>, events:[...]}, …]}`; a per-calendar failure becomes
  `{calendar_id, status:"error", error:"…"}`.
- `curl` it once and time it — that timing is part of the human step.

**Config**: `PIPEDREAM_CALENDAR_URL` + `PIPEDREAM_CALENDAR_TOKEN_FILE`
(preferred, mountable; `PIPEDREAM_CALENDAR_TOKEN` fallback). Env only —
never a DB row, never a flag, never printed; errors withhold the endpoint
(the URL is the token's neighbour). A misconfiguration fails before any
`sync_runs` row exists; a transport failure writes per-account `error` runs,
so an outage keeps `propose_slots` refusing by name.

**Semantics worth knowing**: every poll is a full snapshot applied as a
windowed replacement (deletions supersede on the next poll); a VERIFIED empty
snapshot finishes `ok`, keeps the stale events (over-busy — the direction
that cannot book over a meeting; self-heals on the first non-empty poll) and
counts `calendar_empty_snapshot`; `stats->>'calendar_source'` says which
transport produced a run — diagnostic only, nothing branches on it. Full
event objects (titles, attendees, locations) transit Pipedream and are
visible in its execution logs under its retention — chosen deliberately
(2026-09-05) to keep the morning-brief door open; the intervals-only
alternative remains a workflow edit away.

**Status**: the workflow went live 2026-09-07 — `switchboard-calendar` v14 in
project stealth-fun-natural, all three accounts connected, bad-token 401
verified. First E2E from the workstation ingested 173 events across the three
calendars and `propose_slots` answered for the first time.

**Kube side (DEPLOYED 2026-09-06, suspended until the secret exists)**:
CronJob `connector-gcal` in ops, image switchboard:0.7.0, args
`[--calendar-only]`, `CAL_SOURCE=pipedream`, schedule `*/20`,
`concurrencyPolicy: Forbid`. The secret it waits on is
**`secret/switchboard-pipedream`** with two keys: `PIPEDREAM_CALENDAR_URL`
(injected as the env var of the same name) and `calendar_token` (mounted
read-only at `/etc/switchboard/pipedream/calendar_token`, pointed at by
`PIPEDREAM_CALENDAR_TOKEN_FILE`). Create it with `--from-file` (per the
recorded landmine), then `kubectl -n ops patch cronjob connector-gcal -p
'{"spec":{"suspend":false}}'` — the poll period must stay under half
`AVAIL_MAX_SYNC_AGE` (1h default), and check Pipedream's free-tier invocation
allowance (~72/day at `*/20`) before scheduling.

**Quota incident (2026-09-08), and THE NUMBER that settles it (read off the
Pipedream billing page 2026-09-10)**: the free workspace allowance is
**100 execution credits per MONTH, resetting on the 1st** — not a daily
budget, which is what both earlier cadence decisions silently assumed. The
usage chart attributes **21 credits to `switchboard-calendar` on 2026-09-08
alone** at hourly cadence, so the real cost is **~1 credit per invocation**,
and a booking (`book_calendar_block`) spends one too.

Arithmetic that follows, and it is brutal: hourly = ~720 credits/month against
a 100 cap (7× over); `*/20` = ~2,160 (21× over). **The sustainable ceiling is
~3 polls per day**, total, for reads AND writes together. When the cap is
spent, every request — including an unauthenticated GET with no body — returns
`HTTP 400 "Error in workflow"` with no detail, which reads exactly like a
broken workflow; check the credits before debugging the steps. The cap was hit
2026-09-08 06:40Z and stayed hit for the rest of the month (507 failed runs).

Cadence as it now stands (2026-09-10): **`0 11,17 * * *`** — twice daily at
07:00/13:00 EDT, ~60 credits/month, leaving ~40 for bookings and retries. The
schedule is chosen for WHEN (just before the working day, and midday, local)
rather than how often, because at two polls a day each credit should land when
someone might actually call `propose_slots`; a 03:00 local poll refreshes
nothing anyone will use.

**Open consequence, deliberately NOT yet resolved**: `AVAIL_MAX_SYNC_AGE` is
still `150m`, which is far tighter than a 12-hour polling gap — so once credits
return, availability will REFUSE nearly all day. The value must move with the
cadence or the integration is up but mute. The tradeoff is Salvador's to make:
keep it tight (accurate, answers only just after a poll) or raise it to ~7h
(always answers, may offer a slot already filled by hand). The real fix is to
stop reading through Pipedream at all — a private iCal feed or the Google
Calendar API for the READS, keeping Pipedream only for the occasional booking
write, which fits inside 100 credits comfortably. The calendar sync age — judged with the SAME `AVAIL_MAX_SYNC_AGE`
and readiness predicate `propose_slots` uses — is visible on the dashboard's
`/funnel` page (SWT-29), alongside every other connector's freshness. Don't ALSO run the mail
one-shot with `CAL_SOURCE=pipedream` or invocations double; production mail
runs in the watch loop, which has no calendar phase.

## Booking an own block (SWT-28)

The write half of the Pipedream transport: `channel='calendar'` is a live
delivery channel at the **auto tier**. Two verbs, one send path:

- `book_calendar_block {delivery_id}` — agent-callable (MCP-listed, NOT
  human-only): approves a drafted calendar row and books it in one audited
  call. The gates are the policy matrix (`channel_mismatch` on any
  non-calendar row, the kill switch, the 10/hour channel limit), the
  per-account `calendar_write_enabled` column re-checked at SEND, and the
  pre-flight `LoadBusy` refusal (stale sync / empty scope / horizon / overlap
  — byte-identical to a `propose_slots` refusal, because it is the same one).
- `send_delivery` on a calendar row — the human two-step (draft → approve →
  send), unchanged and still human-only.

**The workflow side went live 2026-09-07 (v16)**: the trigger branches on
`action` — absent/empty is the untouched read poll; `action:"create_event"`
calls `events.insert` with the CLIENT-SUPPLIED id (`sendUpdates=none`, no
attendees, ever), resolves a 409 duplicate with `events.get` + `created:false`
(what makes a retried timeout safe), and echoes the created resource verbatim.
Measured write latency: **1.8 s**.

**Go-live is per account and by hand**:
```sql
UPDATE source_accounts SET calendar_write_enabled=true
 WHERE provider='google' AND account_email='<email>';
```
Default false everywhere — under the auto tier this column is the only
per-account consent an unattended booking has. Revoking it bites at the next
send, even for already-drafted rows.

**Reading a refusal**: a booking refusal in `audit_events.error` /
`deliveries.error` reads exactly like a `propose_slots` refusal (see "Reading
a refusal" above) — same fail-closed door, same wording. A `channel_mismatch`
policy deny means someone aimed `book_calendar_block` at a non-calendar row.

**A failed booking keeps its `sent_external_id`** — deliberately, unlike
gmail's definite-rejection path: the send may have landed, and reopening the
row would trust the third-party workflow's 409 handling. The row goes
`failed`; recovery is the next read poll (which confirms the block if it
landed) or a NEW draft. The delivery row is the audit trail; don't recycle it.
Note the interaction with reservations (next section): a failed-with-id row
RESERVES its interval, so a new draft for the SAME slot is refused with the
overlap message until a poll settles the row or you stamp it with the escape
below. An operator stamp stays distinguishable from a real observation in
the audit trail: a poll-confirmed delivery has a `delivery_confirmed`
task_event, a hand-stamped one has none.

**Concurrency (post-codex, 2026-09-07)**: slot allocation is serialized under
a global advisory lock and an UNCONFIRMED booking is itself part of the busy
set (a reservation on the `deliveries` row: sending, sent, or failed with an
id — lifted when a poll observes the event and stamps `confirmed_at`). A
stale poll can no longer supersede an unconfirmed block. Two residual quirks:
the hourly limit can overshoot by the number of bookings in flight, and a
block deleted by hand BEFORE any poll observed it keeps its slot reserved —
if that ever happens, stamp the row by hand:
`UPDATE deliveries SET confirmed_at=now() WHERE id=<D>` (it will then free at
the next poll like any deleted event). If the busy-set record fails after a
successful send, the handler emits a `log` task_event and returns
`busy_set_pending: true`; the reservation still holds the slot, and the next
poll heals the record.

**Stopping an unattended booker**: `opsctl call --tool set_sending_frozen
--args '{"frozen":true}'` — the kill switch is the ONE brake that reaches the
auto verb (it is not behind a human gate). Undoing a booked block is a
browser click in Google Calendar plus the next poll's snapshot replacement.

## If a Workspace admin blocks the consent

Two of the three mailboxes are Workspace orgs Salvador does not administer;
an admin there can block the client id for `calendar.readonly`. If consent
fails for one of them, decide **at consent time** and record the choice
here:

- **leave `calendar_in_availability=true`** on the blocked account →
  `propose_slots` stays refused for everyone until that calendar can be
  read. Honest and useless.
- **set it `false`** → answers come from a knowingly incomplete busy set,
  which can book over a meeting that lives only in that org's calendar.

Neither is right in the abstract; it depends on whether client meetings live
in the blocked calendar. The code behaves correctly under both.

> Decision record: *(none yet — consent not attempted as of 2026-08-31)*
