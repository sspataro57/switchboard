---
description: Start a switchboard bug — capture the report, sync a Jira Bug issue (SWT project), run reproduction-first flow
argument-hint: [short-kebab-bug-slug]
allowed-tools: Read, Write, Bash(git status:*, git checkout:*, git branch:*)
---

Start bug investigation `$1`.

## Step 1: Capture the report

Write `docs/bugs/$1.md` with the bug report VERBATIM from the user's message (plus
any pasted logs/payloads). Do not paraphrase or interpret — this file is the receipt.
If the report is too thin to act on (no observed behavior, no trigger), list what's
missing and stop.

## Step 2: Sync to Jira

Same pattern as `/ticket-start` step 4, but issue type **Bug**: via the Atlassian
MCP, reuse-or-create an SWT issue with summary `$1: <one-line symptom>`, transition
to In Progress, and record `> Jira: SWT-N` at the top of `docs/bugs/$1.md`. If the
MCP is not connected in this session, write `> Jira: PENDING-SYNC` and continue.

## Step 3: Environment check

Confirm from `.claude/INSTITUTIONAL_KNOWLEDGE.md` what the reproduction will need
(ops db access, MQTT broker, a running service). If a needed piece is down or its
connection details are still "TBD", stop and tell the user what to bring up first.

## Step 4: Run reproduction

Invoke `bug-reproducer` with `$1`. It will:
- Pick the surface (pure Go test / curl / SQL / MQTT transcript)
- Write a failing reproduction artifact
- Produce `docs/bugs/$1_REPRO.md`
- Refuse to investigate cause until reproduction fails as described

## Step 5: Report

Print:
1. Reproduction status (Confirmed / Trivial / Cannot reproduce / Needs more info)
2. Jira issue key (or PENDING-SYNC)
3. Artifact location + how to run it
4. Next step: if Confirmed, invoke `bug-diagnoser`. If Cannot reproduce, gather more
   info from the user (and note it on the Jira issue).

Stop after step 5. Do NOT proceed to diagnosis without explicit user authorization
(diagnose-before-changing is the standing rule; reproduction and diagnosis are
separate gates).
