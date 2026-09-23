# Handoff to the kube session — Slack auto tier (SWT-77) + task-page blank lines (#513)

Salvador's asks, 2026-09-22: "auto for every conversation — claude always asks approval so the double gate
is just annoying for slack", and "the task details looks very bad. lots of spaces" (`/tasks/496`).
Spec: `docs/tickets/slack-auto-tier_SPEC.md`; IK: "Slack replies on the auto tier (SWT-77)".

Image: `192.168.50.20:5000/switchboard:0.7.46`
(`sha256:bf6f5fc9461e9f4fc039c8cab0ceee4d0e28f38390899cdffeed9ed6018c49fb`), built from `main` at
`716dfe7`. It carries both changes; the running image is 0.7.45.

## 0. Already done outside the cluster

- The workstation's user-scope `ops` MCP (`ops-mcp-user`) is re-installed from `main` and registered with
  `SLACK_WEB_BRIDGE_URL` + `SLACK_WEB_BRIDGE_TOKEN_FILE` (runbook `ops-mcp-user-scope.md`, "The Slack
  bridge"). Sessions opened from now on can call `send_slack_reply`. 192.168.50.30 was offline and is still
  pending.
- No leaf change: the Mac mini's slackconnector is untouched.

## 1. No migration, no env var, no manifest change beyond the tag

`migrations/` is unchanged (newest is still 0042). No new route, port or config. Optional env on the
dashboard: `SLACK_SEND_DISPATCH_TIMEOUT` (Go duration, default 3m, clamped to 10m). Nothing needs to set it.

## 2. Roll ONE tag to every workload, in one apply

Keep the pins as usual (classify-promote `--lane personal`; pipelined
`PIPELINE_STAGES=gate,route,route_apply,inquiry,inquiry_promote`). Behaviour changes only in the dashboard:

- **`/tasks/{id}` source email:** whitespace-only lines are collapsed for display (#513). Indentation and
  content are kept. The stored `body_text` is untouched.
- **Slack Send (the human two-step) is stricter, never looser.** The shared send path now re-checks, in the
  transaction that marks a row `sending`, the kill switch, the hourly limit and an identical unresolved
  message to the same conversation. A send cut off by the dashboard's own 60s request timeout now leaves the
  row `sending` and UNSETTLED, so "Not in Slack" stays hidden and the task stays un-closable for the 15m
  send lease. That is deliberate: the mini can still click after we stop waiting.
- The orchestrator (R8) no longer records a `delivery_lifecycle` key for a task that is not
  `done_locally`/`delivered`/`closed`. That is the orchestratord workload, and it is pure logic.

**Check before replacing the dashboard pod** that no Slack send is in flight:
`SELECT id FROM deliveries WHERE channel='slack_reply' AND status='sending' AND send_settled_at IS NULL;`
It should return nothing, or only rows older than 15 minutes. Do not roll `connector-slackweb-watch` while
the bridge's `send_queue.waiting > 0` (SWT-76).

## 3. Post-roll check

- `/tasks/496`: the Bank of America alert reads as about a dozen lines with single blank lines between
  them, not screens of blank space.
- `/deliveries` renders. Delivery 61 (the SWT-77 smoke, task #506) is a `slack_reply` row created by
  `opsctl:salvo`.
- Optional, only with Salvador's go-ahead: one Slack reply approved and sent from the dashboard still goes
  out as before.

## 4. Rollback

Roll the image back to 0.7.45. Nothing is written by the new code that an old binary cannot read, and no
schema changed. Levers that need no roll, mildest first: `send_enabled=false` on the workspace's
`…@slack-web.local` source account, then `set_sending_frozen`, then a lower `OPS_SEND_HOURLY_LIMIT`.
