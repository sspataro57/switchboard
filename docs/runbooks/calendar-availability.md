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
