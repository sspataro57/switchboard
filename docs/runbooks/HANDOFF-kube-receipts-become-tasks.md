# Handoff to the kube session — receipts-become-tasks (SWT-68)

The personal classifier turned payment receipts, autopay notices and refunds into board tasks
(19 of them, mostly PayPal mail in the MSN mailbox). Its prompt is now `classify-v2`: a bill or a
minimum payment is actionable unless the mail says it was already paid or authorized, or will be
charged automatically. Bug docs: `docs/bugs/receipts-become-tasks*.md`.

Image: **`192.168.50.20:5000/switchboard:0.7.34`**
(`sha256:8acc4142b5eda9d2871d61916a8c0cc87809a49b6330a73c7eaf06d02ae97921`), built from `main`
at `ce9e115`. It supersedes 0.7.33 and carries nothing else new.

## 1. No migration, no env var, no manifest change beyond the tag

The change is a prompt string compiled into the binary. Newest migration is still 0037.

## 2. Roll the tag

Only the **`classify-personal` CronJob** changes behaviour (it runs `classify run --lane personal`).
Roll the same tag to all 11 workloads as usual, keeping the pins (classify-promote
`--lane personal`; pipelined `PIPELINE_STAGES=…`; `MS_OAUTH_CLIENT_ID` on connector-google).

Nothing is re-classified by the roll: a message that already has a personal-lane verdict keeps it
(the inbox filter keys on the worker type, not the prompt version), so no old mail is re-read and
no task is duplicated. The fix applies to mail classified from the roll onwards.

## 3. Post-roll check

After the next `classify-personal` run that finds new mail:

```sql
SELECT r.input->>'prompt_version', count(*)
  FROM ai_runs r
 WHERE r.worker_type = 'classify' AND r.created_at > now() - interval '2 hours'
 GROUP BY 1;
-- classify-v2 only
```

## 4. Rollback

Roll `classify-personal` (or everything) back to 0.7.33. Verdicts written under v2 stay valid;
they are stamped `classify-v2`, so they can be told apart.
