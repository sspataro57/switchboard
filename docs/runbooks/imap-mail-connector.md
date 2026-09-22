# Runbook — IMAP mail connector (SWT-11)

Mail ingestion and sending over IMAP/SMTP with per-mailbox Google app passwords.
Replaces the OAuth path for mail: an Internal OAuth app covers only sspataro.com,
External + restricted Gmail scopes needs Google verification plus a CASA
assessment, and a third-party Workspace admin can block the client id anyway.

## Mailboxes in scope

| mailbox | notes |
|---|---|
| `salvador@handsonconnect.org` | Workspace org Salvador does not administer |
| `sspataro@gmail.com` | personal Gmail |
| `developer@sspataro.com` | app password already exists in `upwork/upwork-api-connector-secrets` |

## 1. Store the app passwords as cluster secrets

The passwords live in a k8s secret; the database holds an encrypted copy that the
connector and the send path read. Generate one app password per mailbox at
https://myaccount.google.com/apppasswords (requires 2FA on the account).

```bash
kubectl -n ops create secret generic mail-app-passwords \
  --from-literal=handsonconnect='xxxx xxxx xxxx xxxx' \
  --from-literal=sspataro='xxxx xxxx xxxx xxxx' \
  --from-literal=developer='xxxx xxxx xxxx xxxx'
```

Build it with `--from-file` if any value contains `&` — see the landmine in
INSTITUTIONAL_KNOWLEDGE about `KEY=value` files and background operators.

## 2. Onboard each mailbox

`add-app-password` reads the secret from **stdin only** (argv is world-readable
through `ps`, env leaks through `/proc/*/environ`), verifies it with a real IMAP
LOGIN + LIST **before** storing, and writes it `pgp_sym_encrypt`'d under
`OPS_TOKEN_KEY`. A wrong password therefore fails while you are watching, not at
every future pass.

```bash
kubectl -n ops get secret mail-app-passwords -o jsonpath='{.data.sspataro}' \
  | base64 -d \
  | google-auth add-app-password sspataro@gmail.com
```

Run it from anywhere that reaches both Gmail and the ops db. The image ships the
binary, so a one-shot Job works if the workstation cannot:

```bash
kubectl -n ops run mail-onboard --rm -i --restart=Never \
  --image=192.168.50.20:5000/switchboard:0.2.0 \
  --env=DATABASE_URL=... --env=OPS_TOKEN_KEY=... \
  --command -- /usr/local/bin/google-auth add-app-password sspataro@gmail.com
```

Verify: `google-auth list` shows `auth=app_password` per row. `send_enabled` is
**false** on insert — a freshly onboarded mailbox cannot send until you enable it
deliberately. Re-running to rotate a password does not revoke a mailbox that was
already sending.

```sql
UPDATE source_accounts SET send_enabled=true
 WHERE provider='google' AND account_email='sspataro@gmail.com';
```

## 3. Run ingestion

`MAIL_SOURCE` is an explicit selector. Unset preserves today's behaviour exactly
(bridge if `GMAIL_CONNECTOR_BRIDGE` is set, else the direct Gmail API); an
unknown value is an error, never a silent fallback.

```bash
MAIL_SOURCE=imap DATABASE_URL=... OPS_TOKEN_KEY=... google
```

Knobs, all with defensive defaults (an unparseable value falls back rather than
producing a zero):

| env | default | effect |
|---|---|---|
| `MAIL_MAX_MESSAGE_BYTES` | 1 MiB (prod target **25 MiB**, pending the kube roll — `HANDOFF-kube-mail-refetch.md`) | above it, headers + text are fetched and attachments are skipped. Applied at FETCH time, so raising it repairs nothing already stored — see "Recovering attachments on an over-cap message" |
| `MAIL_FOLDERS` | discover | comma-separated override; otherwise INBOX + the `\Sent` mailbox |
| `MAIL_IDLE_REFRESH` | 25m | IDLE re-issue interval (RFC 2177 caps at 29m) |
| `MAIL_RECONCILE_INTERVAL` | 10m | full sweep in `--watch` |
| `MAIL_PASS_TIMEOUT` | 10m | `--watch` only: bound on every pass (SWT-73 D6); a pass that exceeds it is cancelled, logged and counted, the loop continues |
| `MAIL_WATCH_HEALTH_ADDR` | `:8092` | `--watch` only: `GET /healthz` listen address (SWT-73 D7) |
| `OUTBOUND_OBSERVE_HORIZON` | 720h | SWT-16 capture window |

First pass is bounded by `SEARCH SINCE --backfill` (90d default). A 106,930-message
mailbox is never fetched whole; messages are fetched in batches of 50 so the pod
does not hold thousands of bodies at once.

`--watch` stays resident: IMAP IDLE on INBOX per account plus the reconcile sweep.
Sent is covered by the sweep only — its latency affects delivery confirmation,
which no rule waits on. In production that is `deployment/connector-google-watch`
— see "Running it resident" below.

## Running it resident (SWT-73)

`connector-google-watch` is a 1-replica Deployment (`strategy: Recreate`) running
`google --watch`: one IDLE connection per mailbox on INBOX, a wake runs the same
pass the CronJob runs (ingest → normalize → outbound observation → capture rules
→ pipeline announce) scoped to that mailbox, and a reconcile sweep every
`MAIL_RECONCILE_INTERVAL` covers Sent and anything IDLE missed. Mail lands in
seconds; the `connector-google` CronJob stays on `0 */2 * * *` as a net, never
suspended. Spec: `docs/tickets/imap-idle-watch_SPEC.md`.

**Watch mode is IMAP-only by construction.** `--watch` branches before
`MAIL_SOURCE` is read, so `MAIL_SOURCE` is irrelevant on the Deployment (harmless,
but not what selects the path). The one-shot flags are not available in watch mode:
`--full`, `--overlap`, `--all`, `--normalize-only` and `--calendar-only` are
one-shot only — a Deployment cannot be asked for a full rescan. Run those as a
one-shot `google` invocation; the per-account lock keeps it from racing the
watcher (it counts `accounts_busy` and skips a mailbox the watcher is reading).

**Startup.** The watcher resolves the capture config once and prints one line:
`watch: mode=live horizon=720h reconcile=10m idle_refresh=25m pass_timeout=10m
accounts=4 health=:8092`. It **refuses to start** (exit 1) when
`CAPTURE_RULES_MODE=live` sits under a `CAPTURE_RULES_SINCE` below 2h — under
cron that is one red run, in a resident loop it is a pod that looks alive while
capturing nothing. It does NOT refuse `mode=shadow`; it prints it, so read that
line first when the watcher creates no tasks.

**The singleton.** The pod holds `lockkeys.MailWatch` (`0x5157_0010`) as a session
lock for its lifetime. A second replica (a node drain's overlap, a rolling
restart) does not crash: it logs `standing by`, answers `/healthz` 503
`standby: another mail watcher holds the lock`, retries every 15 s and takes over
when the holder goes away. **Standby is not an outage.** A LOST lock connection (a
CNPG switchover) is different: the loop exits non-zero on its next reconcile tick
and the kubelet restarts the pod. The key is watcher-versus-watcher only — the
CronJob is kept off a mailbox by the per-account locks, not by this key.

**`/healthz`** (`MAIL_WATCH_HEALTH_ADDR`, default `:8092`) answers 200 `ok` iff a
pass **completed** within 3 x `MAIL_RECONCILE_INTERVAL` and the lock connection
answers; else 503 with one line naming which condition failed. A pass that hit
`MAIL_PASS_TIMEOUT` does not count as completed. It deliberately ignores IDLE
state and mail volume: one mailbox in backoff must not restart the pod (a restart
cannot fix `invalid_grant` and would thrash the healthy mailboxes), and a quiet
mailbox is not a sick one. Do not add either to the verdict. There is no Service
and no Ingress; `kubectl -n ops port-forward deploy/connector-google-watch
8092:8092` when a human wants to see it.

**The `imap_idle` phase.** A mailbox whose IDLE fails writes one `sync_runs` row
(`stats->>'phase' = 'imap_idle'`, status `error`) per failure and backs off with
jitter (5s → 5m), leaving the other mailboxes listening. On the first successful
cycle after a failure it writes ONE `ok` row — the recovery marker — and nothing
per wake or per refresh. So on `/funnel`: an account that never failed has no
`imap_idle` phase at all; one that failed and recovered shows a fresh `last_ok`;
one that is still broken shows a stale-or-never `imap_idle` with its error. The
`imap` phase (per account, per pass) is the ingest itself, exactly as under cron.

**Connections.** Per mailbox: 1 resident IDLE connection plus 1 transient per pass
— at most 8 concurrent for four mailboxes, inside Gmail's 15 per account. The
MSN mailbox's credential is minted under the per-account lock in BOTH drivers,
and `idleOnce` makes exactly one connection per lock acquisition (a test pins
it): two processes redeeming the same rotating refresh token is how the loser
gets `invalid_grant`, and that alarm looks exactly like a revoked consent.

**Levers.** Scale the Deployment to 0 and put the CronJob back on `*/10 * * * *`:
today's behaviour returns within ten minutes, no data loss (same cursors, same
raw rows, same locks). `MAIL_PASS_TIMEOUT` and `MAIL_RECONCILE_INTERVAL` are env
only, no roll.

## 4. Verify

```sql
-- per-account ingest health (or open the dashboard: /sources for stored
-- totals, /funnel for runs and freshness — SWT-29 moved the run columns there)
SELECT account_email, auth_type,
       (SELECT count(*) FROM raw_source_items r WHERE r.source_account_id=a.id) AS raw,
       (SELECT max(started_at) FROM sync_runs s WHERE s.source_account_id=a.id) AS last_run
  FROM source_accounts a WHERE provider='google';

-- how much was truncated: if this is a large fraction, raise the cap and re-run --full
SELECT count(*) FILTER (WHERE (raw_json->>'truncated')::bool) AS truncated,
       count(*) AS total
  FROM raw_source_items WHERE raw_json->>'source'='imap';
```

Reprocess without touching the network at all — this is why raw-first matters:

```bash
google --normalize-only --all
```

## Landmines

- **Never mark mail read.** Every fetch is `BODY.PEEK` and the mailbox is always
  SELECTed read-only. The `MailSource` interface has no mutating verb precisely so
  an implementation cannot acquire one; a test greps `imap.go` for such verbs, so
  do not name them even in comments.
- **A UIDVALIDITY change discards the folder's stored position** and re-runs the
  SINCE window. Old raw rows survive (uidvalidity is part of `external_id`) and
  Message-ID dedup collapses the duplicates. Expect a burst of `raw_unchanged`.
- **The cursor advances only after a complete pass.** A mid-pass failure re-fetches
  rather than skips. Errors leave `sync_runs.status='error'`.
- **Sent is not optional.** It is the own-message loop-closure surface: without it
  our own replies never re-enter, deliveries stay unconfirmed, and SWT-16's capture
  pass then reports them as sent by hand.
- **A definite non-send releases the reserved Message-ID; an ambiguous one keeps
  it.** A server that answered with a 4xx/5xx refused, so retry stays reachable. A
  dial failure or a connection dropped mid-DATA leaves the outcome unknown, so the
  row keeps `sent_external_id` and no automatic resend can happen. Resolve those by
  hand — a duplicate client-visible email is worse than a stuck row.
- **Two passes on one account cannot overlap**: each takes a per-account advisory
  lock and a second pass skips (counted as `accounts_busy`), so a stray CronJob
  cannot race the `--watch` Deployment's cursor.

## Attachments (SWT-42)

- **Where they live:** attachments are stored with the message. `raw_source_items.raw_json->>'rfc822_b64'` holds the whole RFC822 message, up to `MAIL_MAX_MESSAGE_BYTES` (1 MiB default).
- **Over the cap:** a larger message is captured `truncated: true`, with headers and one text part, and its `parts` manifest lists what was left behind. Those bytes were never transferred, so no reprocessing recovers them — only `opsctl mail refetch` does.
- **Reading them:** normalization keeps only body text. To read attachments, use the executor tools `mail_list_attachments` and `mail_read_attachment`, which are in both MCP profiles.
- **Who may see what:** both tools gate every caller by the SWT-21 locality rule. Only mail filed under a non-`local_only` project is shown. Unfiled mail is shown only on a mailbox with at least 20 filed messages, none of them local-only (owner decision O2).
- **Refusal wording:** a truncated part is reported as "not stored … capture cap", never as missing.
- **Finder bounds:** `from`/`subject` are literal substrings (`%` and `_` match themselves). One call examines at most 2,000 candidates and reads at most 64 MiB of stored mail; past either limit it returns `truncated: true`, and the fix is a narrower sender, subject or date window.

## Recovering attachments on an over-cap message (SWT-64)

The cap is applied at FETCH time. A message ingested while the cap was lower has no
attachment bytes anywhere in the database, and raising `MAIL_MAX_MESSAGE_BYTES` only helps
future mail. The incremental pass will never revisit it either: it searches
`FromUID = stored.UIDNext`, and the only built-in escapes re-run the whole backfill window
for every folder (106,930 messages on `sspataro@gmail.com`).

`opsctl mail refetch` re-fetches NAMED rows at 100 MiB and upserts them in place. It runs on
the workstation — no image, no manifest, no deploy.

```bash
# Always look first. Writes nothing, but does contact IMAP for the live UIDVALIDITY.
opsctl mail refetch --from '@example.com' --since 720h --limit 50 --dry-run

# Then the smallest possible live run.
opsctl mail refetch --raw-id 73094 --limit 1
```

`--limit` is required; one of `--from` or `--raw-id` is required; `--from` is a literal
substring, not a pattern.

**Read the refusals, they are the point.** `raw_json` is overwritten in place and there is no
version history, so the pass refuses anything it cannot prove is the same message:

| counter | meaning |
|---|---|
| `uidvalidity_changed` | the folder's generation rolled; that UID now names a different message. **Not recoverable this way** — the row's coordinates are stale. |
| `folder_not_selectable` | the row's folder is outside INBOX + `\Sent` (or `MAIL_FOLDERS`) |
| `envelope_mismatch` | `raw_json` disagrees with `external_id`; the row was not written by this connector |
| `gone` | the server returned no message for that UID (expunged) |
| `wrong_account` | the target belongs to a different account than the pass is running as |
| `row_vanished` | the row disappeared between selection and write; re-run the selection |
| `would_downgrade` | the refetch came back truncated but the stored row is COMPLETE — refused, because the write would destroy stored bytes |
| `would_shrink` | the refetch carries fewer bytes than the row already holds (both truncated, but the replacement is smaller) — refused for the same reason. Usually means `--max-bytes` is too low. |
| `still_truncated` | **the repair recovered nothing** — the message is over this pass's cap too, and a truncated capture WAS written (it was no smaller than the stored one). Every other counter reads as success, so check this one. |

Verify with the tool that reports the problem in the first place:

```bash
opsctl call --tool mail_list_attachments --args '{"raw_source_item_id":73094}'
```

Expect `available: true` on the parts. This works before re-normalization, because
`mail_list_attachments` reads `raw_json` directly.

**One cosmetic side effect.** A refetch writes a `sync_runs` row with phase `imap_refetch`.
The **`/funnel`** page judges health per phase (`funnelDisplayStaleAfter`, 3h), so that
account grows an `imap_refetch` row whose `last_ok` ages past the threshold and then reads
`stale` forever, with no further run to clear it. It reflects a one-off hand-run, not a
broken connector.

## A Microsoft mailbox (Outlook.com / MSN / Hotmail)

Outlook.com refuses password logins on IMAP. `outlook.office365.com:993`
advertises `LOGINDISABLED` with `AUTH=XOAUTH2` as its only mechanism, *before*
authentication is attempted, so an app password cannot work no matter how it is
generated — it is irrelevant for IMAP. A Microsoft mailbox authenticates with an
OAuth bearer token minted from a stored refresh token (`auth_type='xoauth2'`).

### 1. Register the app once (Salvador, in the Azure portal)

Entra ID → **App registrations** → New registration.

- **Supported account types: "Personal Microsoft accounts only"**. This is the
  `consumers` audience. A work or school account then cannot complete the flow
  at all, which is the cheapest possible guard against signing in as the wrong
  account.
- Authentication → **Allow public client flows** = Yes. That is the only setting
  the device flow needs; there is no redirect URI to register.
- API permissions → APIs my organization uses → *Office 365 Exchange Online* →
  Delegated → `IMAP.AccessAsUser.All`. `offline_access` is requested by the
  client itself and is what makes a refresh token come back; without it the
  mailbox stops authenticating within the hour.
- **No secret.** A public client is issued no client secret, switchboard stores
  none, and nobody should go looking for one.

What switchboard needs from this is one value: the **Application (client) ID**.
It is not a secret.

### 2. Roll it out IN THIS ORDER

Onboarding before the deploy means every in-cluster pass writes an error row for
that mailbox until the image catches up.

1. Apply migration **0037** (`auth_type` gains `xoauth2`). Merging a migration is
   not applying it.
2. Hand the kube session the image bump plus `MS_OAUTH_CLIENT_ID` on **both**
   workloads that resolve a credential: the connector **CronJob** and the watch
   **Deployment**. (`opsctl mail refetch` reads it from the operator's shell.)
   `MS_OAUTH_AUTHORITY` is optional and overrides the authority base; its default
   is `https://login.microsoftonline.com/consumers`.
3. Only then onboard the mailbox:

```
OPS_TOKEN_KEY=... DATABASE_URL=... google-auth add-microsoft sspataro57@msn.com
```

It prints a URL and a user code, waits while you sign in on any device, then
verifies twice before storing anything: the `id_token`'s claim must equal the
email you passed, and an IMAP `LIST` with the fresh access token must find a
usable folder set. A mismatch stores nothing.

### 3. What the mailbox can and cannot do

It is **read-only**, deliberately and in three independent ways: `SMTP.Send` is
never requested, the stored row has `send_enabled=false`, and `MailSender`
refuses an `xoauth2` account by name. A reply attempt from this mailbox is
supposed to fail — that is not a bug to fix in passing.

### 4. When it stops working

Microsoft rotates the refresh token every time it is redeemed, and switchboard
stores the new one. A consent that is revoked (Microsoft account → Privacy →
Apps and services), or one that ages out, shows up as `invalid_grant` in the
account's `sync_runs.error` row, one row per failing account per pass. Other
causes name themselves the same way: `MS_OAUTH_CLIENT_ID is not set`, or no
stored token at all.

Recovery is re-consent: run `google-auth add-microsoft <email>` again. It is an
upsert — the same row is re-keyed, `send_enabled` is not touched, and nothing
else about the mailbox changes.
