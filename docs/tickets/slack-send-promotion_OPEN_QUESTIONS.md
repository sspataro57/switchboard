> Jira: SWT-12

# slack-send-promotion — open questions

The SPEC is provisional on these four. Q1–Q4 markers in the SPEC point back here.

## Q1 — Automated-path approval surface

`humanActor` now strips one `mcp:` prefix, so policy would PASS
`approve_delivery` / `mark_delivery_sent` / `mark_delivery_failed` for an
interactive session (`mcp:manual:salvo`) — but the mcpserver adapter refuses
any tool not in its hardcoded agent allowlist, so today those verbs are
reachable only via dashboard and `opsctl call`.

**MCP-list the human delivery verbs (approve_delivery, mark_delivery_sent,
mark_delivery_failed) so interactive sessions drive the whole lifecycle in one
place OR keep them dashboard/opsctl-only?** Listing is convenient and policy
still denies workers (`mcp:{worker}` fails humanOnly), but it puts
approve-and-send one prompt-injection away from any process holding the manual
MCP identity; unlisted, the blast radius stays exactly where it is and the
manual path records via `opsctl call` (interactive Claude can run it in Bash).

**Answer (Salvador, 2026-07-29): MCP-list `mark_delivery_sent` ONLY.**
`approve_delivery` and `mark_delivery_failed` stay dashboard/opsctl-only.

Grounds: the manual path only ever needs to RECORD a send the leaf already
made — it never needs to approve or to send. Listing just the recording verb
makes that path one call, while the two verbs that can put words in front of a
client stay off the MCP surface entirely. This session ingests Slack and email
content, so "one prompt injection away from approve-and-send" is a live threat,
not a theoretical one; recording an already-sent message is the one verb where
an injected call can do no external damage.

Consequence to implement: add `mark_delivery_sent` to `agentTools` /
`agentToolNames` in `internal/mcpserver/schemas.go` + `adapter.go`. Policy still
denies workers — `humanOnly` refuses `mcp:{worker}` — so listing it does NOT
make it worker-callable, and a test must pin that.

## Q2 — N before an unconfirmed send is flagged

No existing code implies a value — upwork_chat/jira have no reconciler; this is
the first. **Count completed successful export passes for the workspace since
the send attempt (proposed N=3, env `SLACK_UNCONFIRMED_FLAG_PASSES`) OR use
wall time since sent_at (e.g. 24h)?** Passes are the honest signal (a paused
poller can't false-flag) and are countable from `sync_runs`; wall time is
simpler SQL but flags spuriously when the mini is off. Also confirm N's value.

**Answer (Salvador, 2026-07-29): completed successful export passes, N=3**,
env-tunable as `SLACK_UNCONFIRMED_FLAG_PASSES`.

Grounds: passes are the honest signal. A paused poller, a suspended CronJob, or
a mini that is simply off accumulates no passes and therefore cannot false-flag
— whereas wall time would flag "the poller didn't run" while claiming "the send
may have failed", which are different facts and must not share an alarm.

Counted from `sync_runs` for that workspace's account, `status='ok'`, started
after the send attempt. Rejected: wall time (spurious flags exactly in the
current state, CronJob suspended), and passes-OR-time (reintroduces the false
flag it was chosen to avoid).

Open sub-detail for implementation, not a new question: a send attempted while
an export is mid-flight must not count that in-flight run as one of the three —
count runs that STARTED after `sent_at`.

## Q3 — How the two gate authorities are told apart on the row

That they MUST be queryably distinct is settled (SPEC, Named consequence 1).
The mechanism: **(a) new nullable `deliveries.approval_source` column
('switchboard' set by approve_delivery, 'leaf_token' set by the manual-path
record; migration 0011) OR (b) a sentinel in the existing, currently-unwritten
`policy_result` jsonb (e.g. `{"gate":"leaf_token"}`)?** A column is indexable
and impossible to miss in queries; the jsonb avoids a migration but hides the
distinction in a field nothing populates today.

Related sub-choice, decide together: on the manual path, does the record flow
stay draft→approve→mark_sent (which writes an `approvals` row implying a
switchboard gate that never happened) or does `mark_delivery_sent` accept a
`drafted` slack_reply row when the Q3 marker says leaf-gated, skipping the
fake approval?

**Answer (Salvador, 2026-07-29): (a) a dedicated column, and (b) skip the fake
approval.**

**(a)** New nullable `deliveries.approval_source TEXT` in forward-only migration
`0011_slack_send_promotion.sql`: `'switchboard'` written by `approve_delivery`,
`'leaf_token'` written by the manual-path record. Rejected the `policy_result`
jsonb sentinel — nothing populates that column today, so the single fact that
says "policy never gated this" would hide in a blob no existing query reads. A
column is indexable and appears in every `SELECT *`, which is the point: an
auditor must not be able to overlook it.

**(b)** `mark_delivery_sent` accepts a `drafted` `slack_reply` row when
`approval_source='leaf_token'`, transitioning `drafted → sent` directly, and no
`approvals` row is written.

Rejected reusing draft→approve→mark_sent. Two reasons, the second being the
decisive one (Salvador pushed back on the first, correctly):

1. Weaker than it first appears: it writes an approval that never happened into
   the table an audit trusts. But `approval_source` already marks the delivery
   row, so the truth stays *recoverable* — just via a join, since the marker is
   on `deliveries` while the false entry would be in `approvals`. On its own
   this argues for a join, not a new edge.
2. Decisive: `approve_delivery` runs the policy engine, so this route asks
   policy to rule on a message that is ALREADY in the client channel — and it
   can refuse. Under a kill-switch freeze the approve step is denied and a
   demonstrably-sent message becomes unrecordable. Recording a past fact must
   not depend on a gate whose only meaning is "before the fact". This also
   keeps Q4's freeze surface to one tool instead of two.

Implementation notes: `approval_source` must be set at the same time as the
status transition, in the same tx, or a crash leaves a row whose gate is
unknown — which is the one thing this column exists to prevent. Backfill
existing rows to `'switchboard'`: every delivery sent before this ticket went
through `approve_delivery`. The automated path's existing
`status='approved'` requirement in `sendSlackReply` is unchanged.

## Q4 — Kill switch vs recording an already-happened send

`mark_delivery_sent` is `sendShaped`, so a freeze denies it. On the manual path
the message is already in the client channel when the tool runs — a freeze then
blocks writing the invariant-4 row for a real external message (and blocks
resolving a stuck automated `'sending'` row a human verified in Slack).
**Keep the freeze absolute (freeze = full stop, records included; accept the
temporary invariant-4 gap and write the row after unfreezing) OR exempt the
record/resolution uses of mark_delivery_sent from the kill switch (freeze stops
what switchboard can still prevent, never the recording of truth)?** The first
is simpler and matches SWT-13's criterion 11; the second matches what the
freeze can actually control.

**Answer (Salvador, 2026-07-29): recording is exempt from the freeze, and a
record written during a freeze is logged distinctly.**

Salvador's framing, which is the rule to implement against: *the kill switch is
for switchboard.* It governs what switchboard itself puts in front of a client.
A send that happened by another route — the leaf's own token gate — was never
switchboard's to prevent, so the freeze has no claim on it and the record must
still be written. That a message went out by another path WHILE the kill switch
was on is exactly the kind of thing to log loudly and be able to find later.

So:

- `send_delivery` STAYS freeze-gated. Switchboard's own sending is what the
  panic button is for; this is unchanged.
- `mark_delivery_sent` stops being freeze-gated. It transmits nothing.
- A record written while `sending_frozen` is true emits a distinct event
  (proposed `delivery_recorded_during_freeze`, payload
  `{delivery_id, channel, approval_source}`) so "sent by another path under the
  kill switch" is a direct query, not a timestamp reconstruction.

IMPLEMENTATION TRAP — do not just delete it from `sendShaped`. `snapshotGated`
is currently defined as `sendShaped` (`matrix.go:34`), and `Decide` returns
`allow`/`matrix-human` for anything not in `sendShaped` BEFORE reaching the
channel switch (`matrix.go:60`). Removing `mark_delivery_sent` from that one map
would therefore also drop its hourly `rate_limit` and its whole channel branch —
silently widening the tool rather than narrowing it.

Implement as a third category instead: a tool that IS snapshot-gated (so the
rate limit and channel logic still run) but is NOT freeze-gated. Concretely,
keep `snapshotGated` as its own map containing both tools, and check
`snap.SendingFrozen` only for `send_delivery`. `matrix_test.go` must pin all
four corners: frozen+send=deny, frozen+record=allow, over-limit+record=deny,
worker+record=deny.

---

All four questions are answered. Fold into the SPEC, drop the PROVISIONAL
marker, and re-sync the description to SWT-12.
