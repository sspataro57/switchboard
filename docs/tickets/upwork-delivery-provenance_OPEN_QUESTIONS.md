> Jira: SWT-20

# upwork-delivery-provenance — open questions

One. Everything else in the SPEC is decided, including the two forks the issue
posed (provenance location, and whether a room column ships) — those are recorded
under "Decisions made unilaterally" with the reasoning, not here.

---

## Q1. When the closure lifts, does `drafts:gpt` get in immediately?

`draft_delivery` currently refuses `upwork_chat` for every actor
(`internal/tools/delivery.go:279-281`). This ticket replaces that with a
server-side binding: the target must be the task's recorded source thread (or,
for a human actor, another room of the same bound client). The binding holds for
every caller either way — the question is only who may create the row on day one.

**(a) Reopen for every actor, including `drafts:gpt`.** The drafts worker starts
producing `upwork_chat` drafts on the first run after deploy, for any Deliver task
whose parent carries upwork provenance. Nothing sends: `send_delivery` is still
denied by `channel_assisted`, `approve_delivery` and `mark_delivery_sent` are
human-only, and the worker is undeployed today — so the practical exposure is rows
appearing on `/deliveries` for approval, plus model spend. This is what the SPEC
assumes.

**(b) Reopen for `policy.HumanActor` only, and keep automated callers refused
behind a named constant until you have approved a handful by hand.** Then flipping
it is a one-line change with its own commit. Cost: the "usable alone" claim shrinks
to "a human can draft into the right room via opsctl", the drafts worker keeps
logging `draft_skip` on upwork tasks, and criterion 12's asymmetry becomes the
whole gate rather than a room-choice nuance — which makes the actor test load-
bearing in exactly the way the IK entry on actor prefixes warns about.

Answer: **(a) — reopen for every actor, including `drafts:gpt`.**
(Decided in-session 2026-08-31 under the standing autonomy directive; overrule by
reverting this edit.)

The trust boundary is the server-side binding, which holds identically for every
caller — an actor-prefix gate on top of it is a transport label doing policy work,
the exact landmine the IK entry warns about and that (b) would make load-bearing.
The policy matrix already places Upwork replies in the assisted tier, which
presumes GPT drafting with human send; nothing sends without a human either way
(`send_delivery` stays `channel_assisted`-denied, approve/mark are human-only),
and the drafts worker is undeployed, so day-one exposure is approval-queue rows
plus model spend. The earned-autonomy rule in the matrix gates SENDING tiers, not
draft creation — (b) would be guarding the wrong verb.

---

Answer by editing the entries. Say "questions answered" and I'll fold them into
the SPEC.
