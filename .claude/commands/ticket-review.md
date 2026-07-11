---
description: Pre-commit review — runs go-reviewer (invariants + conventions), then optionally /codex:adversarial-review
argument-hint: [ticket ID, e.g. 04-mcp-task-tools]
allowed-tools: Read, Bash(git status:*, git diff:*, git log:*)
---

Run the pre-commit review for ticket $1.

## Step 1: Verify state

- `git status` — there should be changes to review (staged/unstaged or commits on a
  step branch).
- `docs/tickets/$1_SPEC.md` (or `docs/bugs/$1_DIAGNOSIS.md`) exists — that's what we
  review against. If not, stop.
- `go test ./...` was run by the user or this session and passes — if unknown, say so
  and run it via the main thread before reviewing.

## Step 2: Run go-reviewer

Invoke `go-reviewer`. It produces the structured report: scope, the seven invariants,
schema/migrations, conventions, tests, hygiene.

If the recommendation is "Address findings, then re-review" — surface the findings
and stop. The user fixes, then re-runs `/ticket-review`.

## Step 3: Adversarial pass (optional, ask)

If go-reviewer says ready, ask the user whether to run `/codex:adversarial-review`
for a cross-vendor pass. Worth it for executor/policy/delivery code (the safety
spine); usually skippable for dashboard templates or scaffolding. Do not run it
without asking — this is an internal tool and the second pass is a cost/benefit call.

## Step 4: Synthesize

Print:
1. go-reviewer's recommendation (one line) and top findings.
2. Codex findings by severity, if run.
3. Overall: "Ready for /ticket-deliver" / "Address findings, then re-run" /
   "Reviews disagree on X — human judgment needed".

Stop after step 4.
