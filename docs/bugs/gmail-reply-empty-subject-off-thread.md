> Jira: SWT-61

# gmail-reply-empty-subject-off-thread

## Report (verbatim, relayed 2026-09-15 by the collab session, collaboratory-www)

> Bug report from the collab session (collaboratory-www) for switchboard gmail deliveries. Delivery #36
> (task 164) was drafted with channel gmail, thread_id 159886 and an EMPTY subject. It was approved and
> sent 2026-09-15 19:30 UTC (sent_external_id <sb-36-1789500623280005435@handsonconnect.org>). Salvador
> reports it reached the customer as a standalone email with no subject, not as a reply on the thread.
>
> Likely cause: internal/connector/google/send.go:71 writes a Subject header only when msg.Subject !=
> "", and Gmail only attaches a message to a thread (threadId) when the subject matches.
> In-Reply-To/References alone didn't keep it on the thread.
>
> Suggested fix: when a gmail delivery has a thread_id, default an empty subject to "Re: <latest thread
> subject>" (don't double "Re:"), or refuse the draft/approval with a clear error. Either way, the
> dashboard should show the subject before approval. I've changed my own habit to always pass subject
> explicitly.

## Coordinator context (not part of the report)

- Task 164 is a university client's question (SWT-58). The reply went to that client, so this is a
  client-visible defect.
- Production sends go through SMTP (CLAUDE.md build step 7: `internal/connector/google/smtp.go`,
  `MAIL_SOURCE=imap`), not the Gmail API. The reproduction must establish which send path delivery #36
  actually took before any cause is accepted; the report's `send.go:71` is a lead, not a finding.
- Client message content must not be published outside: keep the client's address and message text
  out of committed docs.

## Expected

A gmail delivery drafted on an existing thread reaches the recipient as a reply on that thread, with a
subject.

## Observed (per the owner, relayed)

It arrived as a standalone email with no subject.
