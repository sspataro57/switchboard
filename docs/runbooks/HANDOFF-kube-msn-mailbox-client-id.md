# Handoff to the kube session — MSN mailbox client ID (SWT-66 follow-up)

No new image. The running `switchboard:0.7.32` already has the Microsoft
mailbox code and migration 0037 is applied. The one thing missing is the env
var from `HANDOFF-kube-microsoft-oauth-mail.md` §2, now that the Azure app exists.

## Change

On the **`connector-google` CronJob** only, add:

```yaml
- name: MS_OAUTH_CLIENT_ID
  value: eadf37a7-ddcf-47a7-8f8f-4caff8283b53
```

Not a secret: it is a public client (Azure app `switchboard-mail`, personal
Microsoft accounts only, no client secret exists). A plain `env:` entry or a
ConfigMap value is fine. No other workload needs it.

## Why now

`sspataro57@msn.com` is being onboarded as an `xoauth2` row in
`source_accounts`. Until this variable is set, every connector-google pass
writes one `sync_runs` error row for that mailbox (`MS_OAUTH_CLIENT_ID is not
set`). The three Gmail mailboxes are unaffected either way — the error is
per-account.

## Verify

```bash
kubectl -n ops get cronjob connector-google \
  -o jsonpath='{.spec.jobTemplate.spec.template.spec.containers[0].env[*].name}'
```

lists `MS_OAUTH_CLIENT_ID`. After the next pass, the latest `sync_runs` row for
the MSN account is `status='ok'`.

## Rollback

Remove the variable. The MSN mailbox then errors per pass again; nothing else
changes.
