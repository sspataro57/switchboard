> Jira: SWT-18

# upwork-matcher-hardening — the Upwork post-hoc matcher never got either of the two fixes the other three matchers did

## Report (verbatim, Salvador, 2026-08-20)

> the only thing that claude already does is to identify the thread. Wen claude
> wants to post to upwork it verifies it's posting to the correct thread by
> marching a previous message (when sobody has several chat rooms)

That description is accurate, and checking the mechanism it describes is what
surfaced this bug. The observation is the report; the findings below are mine
(assistant, same session) and are marked as such.

## Observed defect (verified by reading `main` at `526233b`)

`confirmUpworkDelivery` (`internal/connector/upworkcrm/sink.go:285-322`) is the
assisted-tier post-hoc matcher: it takes an observed outbound Upwork message and
binds it to the `deliveries` row that produced it. It is one of four such
matchers. The other three were hardened against two documented landmines. This
one was not — it is missing BOTH.

|                    | `textmatch.NormalizedPrefix` | attempt-time floor |
|--------------------|------------------------------|--------------------|
| `google/sink.go`   | yes (`:543`)                 | yes (`:562-563`)   |
| `jira/sink.go`     | yes (`:285`)                 | yes (`:319`)       |
| `slackweb/sink.go` | yes (`:304`)                 | yes (`:235`)       |
| `upworkcrm/sink.go`| **no**                       | **no**             |

The query as written:

```sql
UPDATE deliveries SET sent_external_id=$1, confirmed_at=now(), updated_at=now()
 WHERE id = (
   SELECT id FROM deliveries
   WHERE channel='upwork_chat' AND status='sent'
     AND sent_external_id IS NULL AND confirmed_at IS NULL
     AND target_ref LIKE $2                    -- upwork_crm:{client}:%  (ANY room)
     AND left(body, $3) = left($4, $3)         -- raw, unnormalized
   ORDER BY sent_at DESC LIMIT 1)              -- no send_attempted_at floor
 RETURNING id, task_id
```

### Defect 1 — raw text comparison across a provider round trip

`left(body, 120) = left($4, 120)` compares the text we stored against the text
Upwork handed back, byte for byte. This is the SWT-16 landmine verbatim.
`.claude/INSTITUTIONAL_KNOWLEDGE.md` records the fix as living in
`internal/textmatch` and explicitly warns against re-spelling it in SQL, because
Postgres's POSIX `\s` does not cover the unicode spaces Go's `strings.Fields`
does — so an NBSP alone makes the two disagree, silently.

The IK entry names the call sites the fix reached: "slackweb's matcher, jira's
matcher, and capture's preview." `upworkcrm` is absent from that list. `grep -rn
textmatch internal/connector/` confirms it: google, jira and slackweb import it;
upworkcrm does not.

Upwork is the worst channel to leave unnormalized. It is the **assisted** tier —
the text round-trips through a browser UI and a human copy/paste, not an API. It
is strictly more likely to come back reformatted than Jira's API round trip,
which is the one that actually bit.

### Defect 2 — no attempt-time floor, over a scope that spans rooms

The other three matchers carry
`AND (send_attempted_at IS NULL OR send_attempted_at - interval '2 minutes' <= $2)`.
This one has no floor at all — only `ORDER BY sent_at DESC LIMIT 1`.

IK: "A post-hoc matcher without an attempt-time floor binds the wrong send."
slackweb got the floor in SWT-12; jira did not get it until an adversarial pass
went looking. Upwork never got it.

Upwork is more exposed than either, and this is the part Salvador's observation
points at directly: `target_ref LIKE 'upwork_crm:{client}:%'` is scoped to the
CLIENT and deliberately matches **any channel suffix** — every chat room that
client has. Combined with newest-first ordering and no floor, a delivery
re-approved and re-sent AFTER a message already exists is still a candidate, and
being newer it WINS over the row that actually produced that message.

## Consequence

On a miss:

- `sent_external_id` stays NULL **forever** — nothing retries a comparison that
  is already exact, so the row is permanently unclaimable.
- ~~Since SWT-16, capture sees an outbound message no delivery claims and logs
  `outbound_observed` — a false record that switchboard's own message was sent
  by hand.~~ **CORRECTED 2026-08-20 — this claim was wrong.** It was inferred
  from the SWT-16 IK entry and never verified. `internal/capture/observe.go:86-99`
  defines exactly three channels (Gmail, Jira, Slack); `grep -rn upwork
  internal/capture/` returns zero hits; and `cmd/connectors/upworkcrm/main.go`
  runs `Normalize` and stops, never calling `capture.ObserveOutbound`. No
  `outbound_observed` event can be written for an upwork message by any path
  today. The reproduction's zero count is correct production behaviour, not an
  artifact of the test running `Normalize` alone. This consequence becomes real
  only if upwork capture ships (SWT-16's own future work).

On a wrong bind (defect 2): a real send is recorded against the wrong Upwork
message, the correct row stays unclaimable, and the retry's own external id is
lost.

## Scope

Not part of SWT-17 (capture-rules) and must not be folded into it. This is a
pre-existing defect in shipped code.

## Reproduction hypothesis (to be proven, not assumed)

1. **Defect 1**: a delivery body whose first 120 characters contain an NBSP, a
   run of blank lines, or a trailing space that the provider round trip
   normalizes away — the match fails where `textmatch.NormalizedPrefix` would
   have succeeded.
2. **Defect 2**: two `upwork_chat` deliveries to the SAME client in DIFFERENT
   rooms with the same opening 120 characters, the second approved and sent
   after the first message already exists — the newer row is bound to the older
   message.

Both are SQL-level and provable against the ops schema without Upwork.
