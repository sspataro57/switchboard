# chat-on-closed-task — open questions

## Q1. Avviato DMs that name a Treetop-style ticket key

Capture rule 10 attributes any message containing `WEB-n`, `API-n` or `OPS-n` to **collaboratory**,
whichever Slack workspace it came from. So Jose Garcia's Avviato DMs (09-09, 09-10) were logged onto
collaboratory's closed tasks 56 and 57.

With this ticket, a message like that would come back as a **collaboratory Holding task**, just like a
Treetop DM. But an Avviato DM that names no key is not put on any board today, because Avviato's project
is not inquiry-checked.

**Should an Avviato DM that names a WEB/API/OPS key resurface in collaboratory's Holding column?**

- **Yes (recommended).** Follow the attribution capture already made. A human message showing up in
  the wrong project's Holding column is better than it vanishing, and a wrong one is one Dismiss click
  (which also records a label). It needs no workspace special-case in code. Fixing where rule 10 sends
  Avviato messages stays with capture-rule-ticket-keys.
- **No.** Only Treetop-workspace messages resurface. That needs a workspace-to-project check the code
  does not have today, which means more scope.

Answer: **Yes, follow the attribution.** The owner answered on 2026-09-14 with "Yes, follow the
filing (Recommended)": an Avviato DM that names a WEB/API/OPS key resurfaces in collaboratory's
Holding column. Rule 10's cross-workspace attribution stays with capture-rule-ticket-keys.

---

Answer by editing the entries. Say 'questions answered' and I'll fold them into the SPEC.
