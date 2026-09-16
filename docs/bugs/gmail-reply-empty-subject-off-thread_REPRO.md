# Reproduction — gmail-reply-empty-subject-off-thread (SWT-61)

## Status
Confirmed. The missing Subject reproduces on the real send path. For the "off-thread" part, the
sent message itself was read: it did carry In-Reply-To and References (see Notes).

## Send path #36 actually took (read-only on prod, 2026-09-15)
- `audit_events` 1864: `send_delivery`, actor `dashboard:salvo`, status ok, 19:30:23 to 19:30:24 UTC.
  Preceded by 1848 `draft_delivery` (actor `mcp:manual:salvo`), 1855 `update_delivery` (body only)
  and 1863 `approve_delivery` (with `expect_content_hash`).
- Sending account `from_account_id` 1009: provider google, `auth_type = app_password`, SMTP host set,
  port 587, no OAuth refresh token, scopes `{}`.
- So the path was: dashboard, then `send_delivery`, then `tools.sendDelivery`
  (`internal/tools/delivery.go:1073`), which renders with `google.BuildOutboundMIME`
  (delivery.go:1220, defined in `internal/connector/google/send.go`). It then hands the bytes to
  `google.MailSender`, which routes app_password to `SMTPSender` and on to `SubmitSMTP`
  (`mailsender.go:72`, bytes passed unchanged). It did NOT go through the Gmail API. The builder
  named in the report (`send.go`) is on this SMTP path; the transport only carries its output.

## Trigger
1. A gmail thread with a non-empty `normalized_threads.subject` and at least one inbound message.
2. `draft_delivery {task_id, channel:"gmail", thread_id, body}` with **no `subject` key**
   (prod audit 1848's argument keys: body, channel, require_channel, require_thread_in_task_project,
   task_id, thread_id, worker_id). The row lands with `deliveries.subject IS NULL`.
3. `approve_delivery`, then `send_delivery`.

## Observed behavior
Prod `deliveries` 36: channel gmail, `subject IS NULL` (NULL, not the empty string), thread_id 159886,
target_ref NULL, status sent, sent_external_id `<sb-36-1789500623280005435@handsonconnect.org>`,
policy_result `{}`, approval_source switchboard, sent_at 19:30:24 UTC, confirmed_at 19:40:29 UTC.

The sent copy was re-ingested (own-message loop closure worked): `raw_source_items` 79030, account
1009, `imap:[Gmail]/Sent Mail:5:615`, normalized_messages 297954 (outbound, thread 159886, subject
length 0). Its stored RFC822 headers:
- Header names: Return-Path, Received, From, To, Date, Message-ID, In-Reply-To, References,
  MIME-Version, Content-Type. **There is no Subject header.**
- Message-ID: `<sb-36-...@handsonconnect.org>`.
- In-Reply-To: the Message-ID of the thread's latest inbound message (`<CLIENT-MSG-C>`).
- References: `<CLIENT-MSG-A> <sb-35-...@handsonconnect.org> <CLIENT-MSG-C>`, which is every
  message on the thread in order.
- To: one address, `<CLIENT-ADDRESS>` (redacted).

For comparison, the earlier reply on the same thread, sb-35 (raw 78274), has a Subject header plus
In-Reply-To and References.

The local reproduction renders the same header set: no Subject, correct In-Reply-To, full References.

## Expected behavior
A gmail reply on an existing thread carries a non-empty `Subject: Re: <thread subject>` (no doubled
"Re:"), and keeps In-Reply-To set to the latest inbound message and References covering the thread.

## Reproduction location
`internal/tools/gmail_reply_subject_repro_integration_test.go`,
`TestGmailReply_EmptySubject_SendsReOnThread` (build tag `integration`).

The test drives draft, approve and send through `executor.Execute` with the real policy Matrix, on a
seeded app_password account and a 3-message thread (inbound A, our earlier reply B, latest inbound C;
placeholder data). The network is faked at the `tools.GmailSender` seam with the existing
`fakeGmailSender`, which captures the raw bytes that SMTPSender would submit. Nothing is sent.

It runs against an isolated database (already created and migrated to 0036):

```
psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c 'CREATE DATABASE ops_gmailsubject'   # once
make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_gmailsubject?sslmode=disable'
DATABASE_URL='postgres://ops:ops@localhost:5433/ops_gmailsubject?sslmode=disable' \
  go test -tags integration -count=1 -run TestGmailReply_EmptySubject -v ./internal/tools/
```

Exact failing output on df7b894:

```
=== RUN   TestGmailReply_EmptySubject_SendsReOnThread
    gmail_reply_subject_repro_integration_test.go:161: drafted delivery 1: subject IS NULL = true (prod #36: true)
    gmail_reply_subject_repro_integration_test.go:179: rendered header names: [Content-Type Date From In-Reply-To Message-Id Mime-Version References To]
    gmail_reply_subject_repro_integration_test.go:180: Subject present=false value=""
    gmail_reply_subject_repro_integration_test.go:181: In-Reply-To="<inbound-c-itest-gsub@itest-gsub.example>"
    gmail_reply_subject_repro_integration_test.go:182: References="<inbound-a-itest-gsub@itest-gsub.example> <sb-1-111-itest-gsub@example.com> <inbound-c-itest-gsub@itest-gsub.example>"
    gmail_reply_subject_repro_integration_test.go:186: Subject = "" (header present: false), want "Re: Question about the program"
--- FAIL: TestGmailReply_EmptySubject_SendsReOnThread (0.06s)
FAIL
FAIL	github.com/sspataro57/switchboard/internal/tools	0.062s
FAIL
```

Only the Subject assertion fails. The In-Reply-To and References assertions pass, which matches prod.
The test cleans up after itself; a second run fails identically.

## Environment
- git rev-parse HEAD: `df7b89444f0e75911174d51e8be38cf7074e665b` (branch fix-gmail-reply-empty-subject)
- go1.25.0 linux/amd64; compose Postgres at localhost:5433, database `ops_gmailsubject` (0036).
- Prod `schema_migrations` max 0036. Prod was read only inside `BEGIN READ ONLY ... ROLLBACK`.
- No broker, SMTP server or Gmail API was touched; nothing was sent.

## Notes
Observations only.
- `deliveries.subject` for #36 is NULL. No call in its history (draft 1848, update 1855) ever passed
  a subject.
- Thread 159886 has 4 messages: inbound (10 Sep), outbound sb-35 (12 Sep, subject starts with "Re:"),
  inbound (15 Sep 15:33, the latest), and outbound sb-36 (15 Sep 19:30, no subject). The latest
  inbound's subject differs from `normalized_threads.subject`: it does not start with "Re:" and has
  a different length. That matters for which subject "Re: <thread subject>" should mean.
- "Standalone, not on the thread" is what the recipient saw. Switchboard's side of it is recorded
  above: correct In-Reply-To and References, no Subject. How the recipient's mail client groups a
  subject-less reply cannot be observed from here.
- Placeholders: `<CLIENT-ADDRESS>`, `<CLIENT-MSG-A>`, `<CLIENT-MSG-C>` stand in for the client's
  address and Message-IDs; no client subject or body text is recorded.
