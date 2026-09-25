# Handoff to the kube session: a notifier's late copy no longer reopens a task (SWT-91)

Collaboratory's dismissed tasks were coming back in same-second bursts every ~30 minutes. The cause was the Jira
app's Slack DMs, ingested about an hour late, once per workspace. Capture now flags notifier senders. For a
flagged message, task_reopen skips a Slack copy outright, and skips any other copy sent more than 20 min before
the put-down. People's messages are unchanged. Bug record: `docs/bugs/stale-copy-reopens.md`.

Image: `192.168.50.20:5000/switchboard:0.7.56`
(`sha256:b8d22f323204b3ee427f458bb3a572d0996f918cd3c303f2ae41318718e138c4`), built from `main` at `1f56ee4`.
It includes everything in 0.7.55.

## 0. Already done

Nothing. There is no migration and no config change.

## 1. Roll ONE tag to ALL 13 workloads in ONE apply

The behaviour change is in the capture passes (the connector and watcher workloads that run `EvaluateRules`) and
in the `task_reopen` handler (every binary that registers tools). Keep the pins, and do the SWT-76 check (bridge
`send_queue.waiting == 0`) before replacing the watcher pod. No env var, port or probe changes.

## 2. Check

- After the next Slack Jira-DM batch (~every 30 min), no collaboratory task reopens from a Slack message whose
  sender is `Jira`:

  ```sql
  SELECT t.id, e.created_at, e.payload->>'reason'
    FROM task_events e JOIN tasks t ON t.id = e.task_id
   WHERE e.event_type = 'status_changed' AND e.payload->>'from' = 'closed'
     AND e.created_at > '<rollout time>' AND e.payload->>'reason' LIKE '%slack message%';
  ```

  Expect no rows attributed to sender `Jira`.
- `audit_events` rows with `tool='task_reopen' AND args ? 'notifier_copy'` appear, with output
  `skipped: slack_notifier_copy`.

## 3. Rollback

Roll all 13 back to 0.7.55. Nothing to undo in the database.
