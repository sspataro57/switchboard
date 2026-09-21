# Handoff to the kube session — inquiry-reads-quoted-history (SWT-70)

The inquiry lane (does this client message need a reply from Salvador?) judged a client reply by
the conversation quoted under it and missed a real ask; no task was created. The prompt now cuts
quoted reply history before the model sees a message. Bug docs:
`docs/bugs/inquiry-reads-quoted-history*.md`.

Image: **`192.168.50.20:5000/switchboard:0.7.36`**
(`sha256:bf1765400b1bd7eee5a84a46736e32a6c6caeaed937eb8bc5600ba1198522283`), built from `main`
at `6ff5a05`. It supersedes 0.7.35 and carries nothing else new.

## 1. No migration, no env var, no manifest change beyond the tag

Newest migration is still 0038 (already applied). The change is prompt rendering compiled into the
binary.

## 2. Roll the tag

Behaviour changes only where the inquiry stage runs: **`deployment/pipelined`**
(`PIPELINE_STAGES=gate,route,route_apply,inquiry,inquiry_promote`). Roll the same tag to all 11
workloads as usual, keeping the pins (classify-promote `--lane personal`; pipelined
`PIPELINE_STAGES=…`; `MS_OAUTH_CLIENT_ID` on connector-google).

Nothing is re-classified by the roll: a message that already has an inquiry verdict keeps it. The
fix applies to messages judged from the roll onwards.

## 3. Post-roll check

After the next inbound message in an inquiry-armed project (collaboratory):

```sql
SELECT r.input->>'prompt_version', count(*), max(r.created_at)
  FROM ai_runs r
 WHERE r.worker_type = 'classify_inquiry' AND r.created_at > now() - interval '6 hours'
 GROUP BY 1;
-- inquiry-v2 for everything after the roll
```

## 4. Rollback

Set the tag back to 0.7.35. Verdicts written under v2 stay valid and are stamped `inquiry-v2`.
