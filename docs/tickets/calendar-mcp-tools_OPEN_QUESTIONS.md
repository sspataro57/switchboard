# calendar-mcp-tools: open questions

**ANSWERED by Salvador on 2026-10-21 and folded into
`docs/tickets/calendar-mcp-tools_SPEC.md`, which is now FINAL.** This file is
kept as the decision record.

---

## Q1: Which Google calendar should a booking be pushed to by default?

Every time a session books (or an offer reserves windows), switchboard saves it
straight away and later copies it to one of your Google calendars. Only
sspataro@gmail.com can be written to today; salvador@handsonconnect.org is not
write-enabled. Under the save-first design that no longer blocks anything: a
booking aimed at a calendar that can't be written yet just waits in switchboard,
still counted as busy, until writes are turned on.

**Answer (Salvador, 2026-10-21, verbatim):** "by default use sspataro@gmail.com
unless the meating request comes from avviation in that case try to sync to
handsoncconect."

**Folded in as:**
- The default target is sspataro@gmail.com (`source_accounts.calendar_default`).
- "Comes from Avviato" is decided by the TASK'S PROJECT, not by a model or a
  mailbox. `projects.calendar_account_id` is set to salvador@handsonconnect.org
  for collaboratory, a-millon, reengine and llamasite-sec (your recorded list
  of Avviato work), and the runbook verifies it at deploy.
- A mailbox rule was rejected because the handsonconnect mailbox also carries
  Foundry mail.
- An Avviato booking is never moved to sspataro@gmail.com: it waits as
  "account not write-enabled" until you turn writes on there.
- A non-Avviato booking can never be pushed to handsonconnect: a push-time
  re-check refuses it.
- Google only ever sees "Busy" or "Hold". Session labels stay in switchboard.

See criteria 10, 16 and 30–33.

---

Answer by editing the entries. Say 'questions answered' and I'll fold them into
the SPEC. *(Done on 2026-10-21. Nothing is pending.)*
