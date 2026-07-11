---
description: Finalize a ticket — full test run, "usable alone" smoke check, commit-ready summary (Salvador commits), Jira comment + transition
argument-hint: [ticket ID, e.g. 04-mcp-task-tools]
allowed-tools: Read, Write, Bash(go test:*, go build:*, go vet:*, git status:*, git diff:*, git log:*)
---

Finalize ticket $1 for delivery. Prerequisites: implementation done, `/ticket-review` passed.

## Step 1: Verify state

- `docs/tickets/$1_SPEC.md` exists and `/ticket-review` produced a "ready" verdict for the
  current state of the diff (if code changed since the review, stop — re-review first).

## Step 2: Full test run

```bash
go vet ./...
go test ./...
```

If failures, stop and report the output. Do not proceed with failing tests.

## Step 3: "Usable alone" smoke check

CLAUDE.md's build order promises each step ships something usable alone. Run the
SPEC's "Verification protocol" — the concrete smoke check (curl the endpoint, psql
the table, mosquitto_sub the topic, run the migration against a scratch db). Report
actual observed output, not "should work".

## Step 4: Commit-ready summary

Salvador reviews and commits — never commit for him. Produce:

1. `git status` + `git diff --stat` snapshot.
2. A suggested commit message:
   ```
   $1: short imperative summary

   Body: what shipped, which acceptance criteria are met, what's deferred.
   ```
   No Co-Authored-By, no AI references.
3. Acceptance-criteria checklist from the SPEC with pass/fail per item.
4. Anything the reviewer flagged as accepted-risk or deferred.

## Step 5: Jira tracking update

Read the `> Jira:` line from the SPEC (an `SWT-N` key).
- If it says `PENDING-SYNC`, do the sync now (see `/ticket-start` step 4), then continue.
- Post a comment on the issue via the Atlassian MCP: test results, acceptance-criteria
  checklist, anything deferred. Terse register, no AI references.
- Transition the issue to the review/done-side status (inspect available transitions;
  prefer "In Review" if the board has one, else leave in progress and say so). Do NOT
  transition to Done — that happens when Salvador has actually committed; note on the
  issue (or tell him) that Done is pending his commit.

## Step 6: Knowledge capture

If this ticket surfaced a landmine, infra quirk, or new convention, update
`.claude/INSTITUTIONAL_KNOWLEDGE.md` now (and say you did). Also fill in any
"TBD" environment facts you established (local DB connection, compose target,
Jira project key).

Stop. The commit is Salvador's.
