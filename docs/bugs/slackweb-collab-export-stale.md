> Jira: SWT-39

# slackweb-collab-export-stale — bug report (receipt)

## Report, verbatim (Salvador, 2026-09-11)

> I don't think this was captured as task A screenshot was saved as 'Screenshot_20260911_094927.png' to '/home/salvo/Pictures/Screenshots'.  or this A screenshot was saved as 'Screenshot_20260911_094958.png' to '/home/salvo/Pictures/Screenshots'.  should have landed in collaboratory

Screenshot_20260911_094927.png is a Slack DM transcribed as data:

> asunda45 4:49 PM — Hey Salvador, it is now ready for review and merge. I have also validated the changes with Jose, just to be safe.
> github.com/treetopllc/gonoble/pull/3872
> (banner: asunda45 is from Arizona State University)

Screenshot_20260911_094958.png is a Slack DM transcribed as data:

> byeluri Yesterday (2) — For next week, I'll be working through the Jira tickets assigned to me. Please let me know if …

## Scope of this bug

Only the asunda45 message. Nothing ingested it: it is not in `raw_source_items`.

The byeluri message was ingested (raw 77766), and capture rule 8 attributed it to collaboratory as attribution only. That is by design and belongs to ticket `inquiry-promote`, not to this bug. It was filed by hand as task #110.

## Evidence at report time (prod, read-only, 2026-09-11 ~13:45 UTC)

- Workspace Collaboratory/LlamaSite `T0HPR78RX` is `source_accounts.id = 542`, account `t0hpr78rx@slack-web.local`.
- **Stuck export.** Every slackweb `sync_runs` row for account 542 since at least 2026-09-11 11:00Z (ids 34510, 34540, 34568, 34598, 34626, 34656) reports `status=ok`, `conversations_seen=7`, `messages_seen=166` and `raw_inserted=0`, with `raw_updated` 26–27 on every run.
- **The other workspace is fine.** Avviato (539) sees 16 conversations and ~1225 messages per run, and its counts change.
- **The newest insert for account 542** is raw 77766 (byeluri DM `D0B6FV6HFSR`), ingested 2026-09-10 22:35:28Z.
- **The asunda45 DM** `D0AUD86LKGA` holds 494 messages. The newest message timestamp is 2026-09-10T17:25:24.597Z (raw 77683), last ingested 17:36:36Z. The 4:49 PM EDT message (≈20:49Z) is absent.
- **The Jira bot DM** `D023E7XSSGG`: the newest message is 2026-09-10T20:32:33Z.
- **The latest job log** is `connector-slackweb-29818890`. Ingest across both workspaces: `conversations_seen=23`, `messages_seen=1391`, `raw_inserted=0`, `raw_updated=29`.
- **The leaf** is `~/projects/personal/slackconnector` (TypeScript). It runs on the Mac mini (192.168.50.130) through the bridge. Its `src/switchboard/export.ts` mentions a virtualized sidebar.

## Expected

A new message in a Collaboratory conversation lands in `raw_source_items` on the next export, with `raw_inserted > 0`.
