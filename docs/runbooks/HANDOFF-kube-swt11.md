# Handoff → kube session: deploy SWT-11 (IMAP mail)

Written by the switchboard session, 2026-07-31. Everything in this repo is done
and merged; what remains is cluster state, which belongs to the kube session.

**Image is built and pushed — do not rebuild.**

```
192.168.50.20:5000/switchboard:0.3.0
digest sha256:04c3215f3dfe32633e491a103d23ff446f712b55880ac819eaa5471676ce21d8
built from main fc361c0
```

All eight entrypoints verified to run in that image: `google`, `jira`,
`upworkcrm`, `slackweb`, `github`, `dashboard`, `google-auth`, `migrate`.

## Preconditions (already true — stated so you can skip verifying)

- **pg-main is at migration 0014.** I applied 0010–0014 on 2026-07-31; production
  had drifted five behind (`schema_migrations` said 0009 while main was at 0014).
  Nothing had broken only because the CronJobs run a pinned older image.
  Confirm if you like: `psql -h 192.168.50.49 -U ops -d ops -tAc "SELECT max(version) FROM schema_migrations"` → `0014`.
- `deployment/dashboard` + `service/dashboard` already exist in `ops` (I created
  them from here on 2026-07-31 — that was me stepping into your repo; sorry).
  They run tag `0.2.0`, which predates the SWT-11 merge.

## What needs doing

### 1. Bump the dashboard to 0.3.0

`kube/switchboard/dashboard.yaml`, `image:` → `:0.3.0`. It gains `/sources`
improvements and the mail send router. `Recreate` strategy, one replica.

### 2. Un-suspend `connector-google` and point it at IMAP

It has been `SUSPEND=True` since the OAuth era. Two changes in
`kube/switchboard/connectors.yaml`:

- `suspend: false`
- add `MAIL_SOURCE=imap` to the container env

`MAIL_SOURCE` is an explicit selector on purpose: **unset preserves today's
behaviour exactly** (bridge if `GMAIL_CONNECTOR_BRIDGE` is set, else direct Gmail
API), and an unknown value is a hard error rather than a silent fallback. So the
env is what actually switches the path — the code will not infer it from the
database.

Existing env is already sufficient otherwise: `DATABASE_URL` and `OPS_TOKEN_KEY`
are both mounted, and `OPS_TOKEN_KEY` is what decrypts the app passwords.

Optional knobs, all defaulting sanely (an unparseable value falls back rather
than producing a zero):

| env | default | effect |
|---|---|---|
| `MAIL_MAX_MESSAGE_BYTES` | 1 MiB | above it, headers+text fetched, attachments skipped |
| `MAIL_FOLDERS` | discover | comma-separated override; else INBOX + the `\Sent` mailbox |

### 3. Bump the other three CronJobs to 0.3.0

`connector-jira`, `connector-upworkcrm`, `connector-slackweb` are on `0.1.0`.
They will keep working on the old tag, but they are now behind SWT-12, SWT-16 and
SWT-11 — and the database is already migrated past them, so the drift is the
wrong way round. Not urgent, but do not leave it indefinitely.

## Deliberately NOT in this handoff

- **Watch mode (`--watch`) is not deployed.** It works and is tested, but it
  would be switchboard's first long-running connector and wants its own decision
  about runtime shape. The `*/10` CronJob does the same work on a schedule. If
  you do deploy it later, note it needs `MAIL_IDLE_REFRESH` (25m default) and
  `MAIL_RECONCILE_INTERVAL` (10m), and it takes a per-account advisory lock so it
  will not race a CronJob that overlaps it.
- **No Ingress for the dashboard.** With `OIDC_ISSUER` unset it falls back to a
  dev-login stub that grants a session to anyone reaching `/dev/login` — on a
  surface that approves and sends client-visible messages. The Ingress block is
  written and commented out in `dashboard.yaml` beside the OIDC env it needs.
  Port-forward until OIDC is configured.

## Mailbox onboarding — not yours unless you want it

Nothing ingests until a `provider='google'` row exists with
`auth_type='app_password'`. That is a credential operation, not a cluster one;
full procedure in `docs/runbooks/imap-mail-connector.md`. Short version, once the
secret exists:

```bash
kubectl -n ops get secret mail-app-passwords -o jsonpath='{.data.sspataro}' \
  | base64 -d \
  | google-auth add-app-password sspataro@gmail.com
```

The `google-auth` binary ships in `0.3.0`, so a one-shot Job on this image works
if the workstation cannot reach both Gmail and the db.

## How to tell it worked

The dashboard's `/sources` page is the check — it is the only view that reads
`raw_source_items` / `normalized_messages` / `sync_runs`, so it distinguishes a
quiet board from a dead connector while triage is still in shadow mode.

```bash
kubectl -n ops port-forward svc/dashboard 8085:80
# then open /sources — the google account should appear with auth=app_password,
# a recent run, and non-zero Raw/Msgs. Before onboarding it will not appear at all.
```

SQL equivalents if you prefer:

```sql
-- did a pass run, and did it succeed
SELECT a.account_email, s.status, s.started_at, s.stats
  FROM sync_runs s JOIN source_accounts a ON a.id=s.source_account_id
 WHERE a.provider='google' ORDER BY s.started_at DESC LIMIT 5;

-- how much got truncated; if this is a large fraction, raise the cap and re-run --full
SELECT count(*) FILTER (WHERE (raw_json->>'truncated')::bool) AS truncated, count(*) AS total
  FROM raw_source_items WHERE raw_json->>'source'='imap';
```

## Rollback

Set the image back to `0.2.0` (dashboard) / `0.1.0` (connectors) and re-suspend
`connector-google`. **Migration 0014 is additive and stays** — it only adds
nullable columns plus a default, so old code ignores it. Do not try to reverse it;
migrations here are forward-only.
