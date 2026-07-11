---
description: Start a switchboard build-order step (or ad-hoc feature) — generates SPEC via spec-writer, creates + tracks the mirrored Jira issue (SWT project)
argument-hint: [step number 1-10, or a short slug for ad-hoc work]
allowed-tools: Read, Write, Grep, Glob, Bash(git status:*, git checkout:*, git branch:*)
---

Start work on switchboard build-order step $1.

## Step 1: Resolve the step

- If $1 is a number: read the matching entry in CLAUDE.md's "Build order" section.
  That entry is the source text. Derive `{ID}` as zero-padded number + kebab slug
  (e.g. `04-mcp-task-tools`).
- If $1 is a slug: this is ad-hoc work; ask the user for a one-paragraph description
  if one wasn't given in the same message. `{ID}` = the slug.

Check `docs/tickets/` — if a SPEC for this ID already exists, report its state and ask
whether to regenerate or resume instead of silently overwriting.

## Step 2: Branch (only if the repo uses branches)

If the repo has commits and a remote/PR flow, create `ticket-{ID}` from the default
branch. If it's still pre-git or trunk-only (Salvador commits directly), skip and say so.

## Step 3: Generate SPEC

Invoke `spec-writer` with the step text / description. It produces:
- `docs/tickets/{ID}_SPEC.md`
- `docs/tickets/{ID}_OPEN_QUESTIONS.md` (only if ambiguity exists)

## Step 4: Sync to Jira (tracking of record)

Planning stays local; tracking lives in the personal Jira build tracker — project
**SWT** on sspataro.atlassian.net, via the Atlassian MCP (see INSTITUTIONAL_KNOWLEDGE.md
"Jira build tracker"). Use whichever tool names the MCP exposes (search, create issue,
transition — commonly `searchJiraIssuesUsingJql`, `createJiraIssue`,
`transitionJiraIssue`):

1. Search SWT for an existing issue whose summary starts with `{ID}`
   (JQL: `project = SWT AND summary ~ "{ID}"`). Reuse it if found.
2. Otherwise create one: summary `{ID}: <one-line goal>`, issue type Task,
   description = the SPEC's Goal + acceptance criteria plus the repo path of the full
   SPEC (keep the Jira body short; the local SPEC is the document).
3. Transition the issue to In Progress.
4. Record the key on the first line of the SPEC: `> Jira: SWT-N` — later commands
   find the issue through this line.

If the Atlassian MCP is not connected/authenticated in this session, say so (the user
authenticates with `/mcp`), write `> Jira: PENDING-SYNC` in the SPEC instead, and
continue — the next command retries.

## Step 5: Report

Print:
1. SPEC location and one-line summary of the goal.
2. Jira issue key + status (or PENDING-SYNC).
3. OPEN_QUESTIONS location if present — user answers in that file and says
   "questions answered".
4. Which invariants the SPEC flagged as applying.
5. Next step: invoke `test-author` (or resolve open questions first).

Stop after step 5. Do not start implementing.
