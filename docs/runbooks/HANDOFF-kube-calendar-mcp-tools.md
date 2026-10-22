# Handoff to the kube session: calendar for every session, swb-first bookings (SWT-109, swb 2348)

Image: `192.168.50.20:5000/switchboard:0.7.70` (`sha256:0a1b8b49115e03d9a94fc1672bf573ccc71249c95fca9189bc616d19f0d5ea51`), built from
`main` at `672f3d3`. Spec: `docs/tickets/calendar-mcp-tools_SPEC.md`; runbook:
`calendar-availability.md`, "Reservations"; IK: "Calendar for every session".

## 1. Migration 0049 FIRST

Check that prod is at 0048 (`SELECT max(version) FROM schema_migrations;`), then apply 0049 with the usual
migrate job. Verify:

```sql
SELECT slug, calendar_account_id FROM projects ORDER BY slug;          -- exactly collaboratory, a-millon, reengine, llamasite-sec set
SELECT id, account_email FROM source_accounts WHERE calendar_default;  -- one row: google sspataro@gmail.com
```

If a slug is missing, tell Salvador. Do not guess.

## 2. Roll ONE tag to ALL workloads in ONE apply

Keep every pin as it is. The only manifest change is on `deployment/pipelined`:

- `PIPELINE_STAGES` gains `,calendar_push`:
  `PIPELINE_STAGES=gate,route,route_apply,inquiry,inquiry_promote,calendar_push`.
- pipelined now needs the Pipedream workflow credential: `PIPEDREAM_CALENDAR_URL` from
  `secret/switchboard-pipedream`, and the token mounted read-only at
  `/etc/switchboard/pipedream/calendar_token` with `PIPEDREAM_CALENDAR_TOKEN_FILE` pointing at it. Do this
  exactly as on the dashboard and connector-gcal. With `calendar_push` listed and these missing, pipelined
  refuses to start, on purpose.
- Optional; leave unset for the defaults: `OFFER_HOLD_TTL` (72h), `CALENDAR_WRITE_MONTHLY_BUDGET` (35).

No other manifest change: no new route, port or Secret.

## 3. Check

- `kubectl logs deploy/pipelined` shows `stage started stage=calendar_push`, and later
  `calendar_push: push pass` lines only when something is due.
- On the dashboard `/deliveries`, any reservation row shows `booking:` or `hold for offer`.

Tell the switchboard session when done; it runs the live smoke test.

## Rollback

Roll back to 0.7.69 and drop `calendar_push` from `PIPELINE_STAGES`. 0049 is additive (nullable or
defaulted columns), so 0.7.69 runs on it unchanged.
