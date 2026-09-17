# microsoft-oauth-mail — open questions

Two decisions the SPEC cannot take for you. Everything else is settled in
`microsoft-oauth-mail_SPEC.md` under "Decisions taken in this SPEC".

---

## Q1. Capture-rule priority for the MSN mailbox: does the mailbox win, or is it the fallback?

The routing rule is `--project personal --type thread_key_prefix --pattern
'gmail:sspataro57@msn.com:'`, attribution only (no `--external-system`, so no
task in any mode). The only free parameter is priority, and it decides what
happens when a client's mail arrives at that address.

The live ladder (runbook "Project-name rules"): ticket rules **90/50**, Slack
workspaces **10**, bulk senders **5**, project-name subject rules **3**, the
T0360B84U catch-all **1**.

**Answer A — HIGH (e.g. 95): the mailbox wins.** Everything arriving at
sspataro57@msn.com is `personal`, including a client who happens to have that
address. Clean mental model ("that inbox is my private life"), and no client mail
can be silently filed under a project by a rule you did not write for it. Cost: if
a client ever writes to that address, the message is filed `personal` and never
reaches the client's board — and `personal` is `ai_locality='local_only'`, so it
also never reaches a hosted model.

**Answer B — LOW (e.g. 4, above bulk, below project-name rules): `personal` is
the fallback.** A client-keyed rule (ticket rules, sender rules, project-name
subject rules) still wins on mail that arrives at the MSN address; anything the
client rules do not claim falls to `personal`. Cost: a bulk-sender rule at 5
outranks it, so newsletters to the MSN address file under `bulk`, not `personal`
(probably what you want); and a subject that merely NAMES a project (priority 3)
would... lose, which may surprise you later.

Note before answering: because of the global Message-ID dedup index, a mail
delivered to BOTH a Gmail mailbox and the MSN one keeps only one normalized row,
attached to whichever copy normalized first — effectively arbitrary. So under A,
a cross-delivered client mail is `personal` only about half the time, which is
worse than either pure outcome. `opsctl capture-rules try` prints the real corpus
answer for both priorities before anything is stored.

**Answer:**

---

## Q2. Azure supported account types: `consumers` only, or `common`?

You do the app registration, and the audience you pick becomes the authority URL
baked into `MS_OAUTH_AUTHORITY`'s default
(`https://login.microsoftonline.com/{audience}`).

**Answer A — "Personal Microsoft accounts only" (`consumers`).** Exactly matches
the one account in scope. A work/school account cannot sign in at all, so the
wrong-account mistake is impossible at the identity provider rather than caught by
switchboard's claim check. If you later add an Office 365 work mailbox, you create
a second registration or re-point the audience.

**Answer B — "Accounts in any organizational directory and personal Microsoft
accounts" (`common`).** One registration serves both kinds forever, so a future
client Office 365 mailbox needs no Azure work. Cost: a tenant admin can block or
require consent for the app, the wrong-account failure mode returns (a work
account can complete the device flow, and only switchboard's id_token claim check
stops it being stored), and the registration is a broader thing to leave lying
around than the job needs.

**Answer:**

---

Answer by editing the entries. Say "questions answered" and I'll fold them into
the SPEC.
