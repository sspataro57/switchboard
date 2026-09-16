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
| `OUTBOUND_OBSERVE_HORIZON` | 720h | SWT-16 capture window |

First pass is bounded by `SEARCH SINCE --backfill` (90d default). A 106,930-message
mailbox is never fetched whole; messages are fetched in batches of 50 so the pod
does not hold thousands of bodies at once.

`--watch` stays resident: IMAP IDLE on INBOX per account plus the reconcile sweep.
Sent is covered by the sweep only — its latency affects delivery confirmation,
which no rule waits on.

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
