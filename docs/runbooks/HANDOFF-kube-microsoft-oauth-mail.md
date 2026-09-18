# Handoff to the kube session — microsoft-oauth-mail (SWT-66)

Salvador's personal mailbox `sspataro57@msn.com` cannot be read with a password:
`outlook.office365.com:993` advertises `LOGINDISABLED` with `AUTH=XOAUTH2` as its
only mechanism, before authentication is attempted. This ticket teaches the
existing IMAP connector a second credential kind — an OAuth refresh token used to
mint XOAUTH2 bearer tokens. Spec: `docs/tickets/microsoft-oauth-mail_SPEC.md`.

Image: **to be built at delivery** (`192.168.50.20:5000/switchboard:<tag>`); the
tag and digest go here before this hand-off is acted on.

## 1. Migration 0037 must be applied FIRST

`migrations/0037_microsoft_oauth_mail.sql` widens `source_accounts.auth_type` to
allow `xoauth2` and adds `source_accounts_xoauth2_token_present`. No new table,
no new column, no data change. Merging it is not applying it.

A binary carrying this code against a pre-0037 schema still runs: the three Gmail
mailboxes are unaffected, and nothing writes an `xoauth2` row until someone
onboards one, which would then fail on the old CHECK.

## 2. One new env var, on TWO workloads

`MS_OAUTH_CLIENT_ID` — the Azure Application (client) ID. **It is not a secret**
(the app is a public client; no client secret exists, and none is stored), so a
ConfigMap value or a plain `env:` entry is fine.

It is needed by every workload that resolves an IMAP credential:

| workload | why |
|---|---|
| the mail connector **CronJob** | `runIMAPIngest` resolves each account's credential per pass |
| the **watch Deployment** | `idleOnce` does the same per IDLE cycle |

`opsctl mail refetch` also needs it, but that runs from the operator's shell, not
in-cluster.

Optional: `MS_OAUTH_AUTHORITY` overrides the authority base. Leave it unset. The
default is `https://login.microsoftonline.com/consumers` — personal Microsoft
accounts only, which is what makes a work account unable to sign in at all.

Everything else is unchanged: no new port, route, volume or secret, and no
manifest change beyond the image tag and this variable.

## 3. THE ORDER MATTERS

Apply 0037 → roll the image with `MS_OAUTH_CLIENT_ID` → **only then** onboard the
mailbox with `google-auth add-microsoft`. Onboarding first means every in-cluster
pass writes one `sync_runs` error row per pass for that mailbox, saying
`MS_OAUTH_CLIENT_ID is not set`, until the deploy lands.

Nothing about the three Gmail app-password mailboxes changes at any point.

## 4. Post-roll check

The MSN mailbox will not exist yet, so check the Gmail mailboxes are undisturbed:

- a connector pass still writes `ok` `sync_runs` rows with `stats->>'phase'='imap'`
  for the existing accounts;
- `opsctl mail refetch --dry-run --from '@' --limit 1` still resolves an account.

After the mailbox is onboarded, the first pass should produce `raw_source_items`
for it with `raw_json->>'source' = 'imap'`, and its own sent mail should normalize
`direction='outbound'`.

## 5. Rollback

Roll the deployment back to the previous tag. The `xoauth2` row is inert to older
binaries: `ListAppPasswordAccounts` in the previous image selects only
`auth_type='app_password'`, so the mailbox is simply not read, and no Gmail
mailbox is affected. Migration 0037 does not need reverting — it only widens a
CHECK — and reverting it would fail anyway while an `xoauth2` row exists.

## 6. What this mailbox can do

Read only, by three independent gates: the `SMTP.Send` scope is never requested,
the stored row has `send_enabled=false`, and the send router refuses an `xoauth2`
account by name. A reply attempt from it is supposed to fail.
