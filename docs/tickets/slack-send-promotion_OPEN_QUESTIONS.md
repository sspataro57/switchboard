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

Answer:

## Q2 — N before an unconfirmed send is flagged

No existing code implies a value — upwork_chat/jira have no reconciler; this is
the first. **Count completed successful export passes for the workspace since
the send attempt (proposed N=3, env `SLACK_UNCONFIRMED_FLAG_PASSES`) OR use
wall time since sent_at (e.g. 24h)?** Passes are the honest signal (a paused
poller can't false-flag) and are countable from `sync_runs`; wall time is
simpler SQL but flags spuriously when the mini is off. Also confirm N's value.

Answer:

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

Answer:

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

Answer:

---

Answer by editing the entries. Say "questions answered" and I'll fold them into
the SPEC.
