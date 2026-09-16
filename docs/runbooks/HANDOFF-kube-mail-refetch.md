# Handoff to the kube session — raise the mail capture cap (SWT-64)

One env var on one CronJob. No image build is required for this change, and nothing else in
the SWT-64 ticket needs the cluster at all — the refetch tool is an `opsctl` subcommand that
runs on the workstation.

## The change

`connector-google` currently sets **no** `MAIL_MAX_MESSAGE_BYTES`, so it runs on the 1 MiB
default (`DefaultMaxMessageBytes`, `internal/connector/google/imap.go:43`). Set it to 25 MiB:

```
MAIL_MAX_MESSAGE_BYTES=26214400
```

Owner decision, 2026-09-16 ("25 mib on the cronjob").

Current env on that CronJob, for reference: `MQTT_BROKER`, `CAPTURE_RULES_MODE=live`,
`DATABASE_URL`, `OPS_TOKEN_KEY`, `MAIL_SOURCE=imap`.

## Why 25 MiB and not 100

The cap is applied at FETCH time (`src.Fetch(..., maxBytes)`), so it decides what is ever put
on the wire and stored. It exists to stop `raw_source_items` bloating across ~117k messages;
at 100 MiB a single message can outweigh thousands of ordinary ones, on an always-on pass over
a 106,930-message mailbox. 25 MiB clears Gmail's own attachment ceiling for practical
purposes, so ordinary client attachments land on the first pass while a pathological message
is still bounded.

The 100 MiB number the owner also asked for belongs to the hand-run tool
(`RefetchMaxMessageBytes`), where a human names a handful of messages and accepts their size.
Two numbers, two different bargains — they are deliberately not the same knob.

## What it does and does not fix

- **Going forward:** new over-25-MiB-free mail keeps its attachments on first ingest.
- **It does NOT repair already-stored rows.** The bytes of a message that was truncated under
  the 1 MiB cap were never transferred; raising the cap cannot recover them. That is what
  `opsctl mail refetch` is for, and it needs no cluster change.

## Verify after rolling

```bash
kubectl -n ops get cronjob connector-google -o jsonpath='{range .spec.jobTemplate.spec.template.spec.containers[0].env[*]}{.name}={.value}{"\n"}{end}' | grep MAIL_MAX
```

Then, after the next pass, a newly-arrived message with a large attachment should list its
parts as `available: true` via `mail_list_attachments`.

## Rollback

Remove the env var (back to the 1 MiB default) or lower the value. Nothing is migrated and
nothing is stored differently; only the size of what future passes fetch changes. Rows already
stored at the larger cap stay readable.

## Cosmetic follow-ons, recorded so they are not read as bugs

- `truncatedReason()` prints the READER's cap, not the cap in force when the row was captured
  (`internal/connector/google/attachments.go:76-83`) — the known SWT-42 gap.
- `docs/runbooks/imap-mail-connector.md` and `internal/dashboard/templates/sources.html` name
  "1 MiB" in prose.
