# Codex Compatibility Instructions

Read `CLAUDE.md` before starting work.

`CLAUDE.md` is the authoritative description of the project workflow, architecture,
agent chain, validation requirements, and development rules. It was written for Claude
Code and may reference Claude Opus, Claude Fable, Claude Sonnet, Claude-specific agents,
commands, hooks, and tools.

Preserve the workflow, but translate Anthropic-specific mechanisms into Codex equivalents.

## Jira MCP for this repo

Where `CLAUDE.md` and `.claude/` refer to a Jira MCP server named `jira` (Claude Code loads
it from this repo's `.mcp.json`), Codex does not read `.mcp.json`. The equivalent server is
registered globally in `~/.codex/config.toml`. **For this repo, use the `jira-personal` server**
wherever the workflow says `jira` — it points at `sspataro.atlassian.net` (project SWT). Ignore the other
`jira-*` servers. The token is forwarded from the shell that launched `codex`; if the jira
tools are absent, that shell lacked the exported token — stop and report, never paste a
secret into config.

%b

## Model translation

Treat model names in `CLAUDE.md` (and in `.claude/agents/*.md` frontmatter) as capability
roles, not literal model invocation instructions.

### Claude Opus

When the workflow assigns a task to Claude Opus, use the strongest available GPT
reasoning configuration.

Opus responsibilities generally include:

* architecture;
* planning;
* difficult diagnosis;
* security analysis;
* design decisions;
* risk analysis;
* adversarial review;
* final verification.

Use high or xhigh reasoning for these stages.

### Claude Fable

When the workflow assigns a task to Claude Fable, use the strongest available Codex
coding model configured for sustained autonomous implementation.

Fable responsibilities generally include:

* executing approved implementation plans;
* large multi-file changes;
* migrations;
* complex refactors;
* writing and running tests;
* iterating on failures;
* completing long implementation sequences;
* maintaining task state across multiple stages.

Use high or xhigh reasoning and continue autonomously through the implementation and
validation loop.

### Claude Sonnet / Haiku

When the workflow assigns a task to Claude Sonnet or Haiku, the stage is mechanical
(ticket intake, verbatim capture, delivery writing, formatting). Use a standard
reasoning configuration; do not spend xhigh reasoning on these stages.

Do not attempt to invoke Anthropic models merely because `CLAUDE.md` names Opus, Fable,
or Sonnet.

## Chain translation

Translate a Claude workflow such as:

```text
Opus plans
→ Fable implements
→ Opus reviews
→ Fable fixes
→ Opus performs final acceptance
```

into:

```text
GPT reasoning pass plans
→ Codex implementation pass executes
→ fresh GPT reasoning pass reviews
→ Codex implementation pass fixes
→ fresh high-reasoning pass performs final acceptance
```

Use separate subagents or fresh context passes where available so that the reviewer does
not merely approve its own implementation.

## Claude agents and commands

When `CLAUDE.md` references Claude-specific agents, commands, skills, hooks, or
directories (`.claude/agents/`, `.claude/commands/`, `.claude/INSTITUTIONAL_KNOWLEDGE.md`):

1. Read the referenced file.
2. Identify its responsibility, inputs, expected output, and completion criteria.
3. Reproduce that behavior using Codex subagents, skills, shell commands, repository
   inspection, or direct execution.
4. Do not assume Claude-specific commands are executable in Codex.
5. Do not skip a workflow stage simply because its original implementation is
   Claude-specific.

## Workflow preservation

Maintain the order and intent defined in `CLAUDE.md`, including:

* repository discovery;
* architecture analysis;
* planning;
* implementation;
* tests and validation;
* independent review;
* remediation;
* final diff inspection;
* documentation or handoff updates.

Only substitute the model and execution mechanism.

## Before changing code

Before implementation:

1. Read `CLAUDE.md`.
2. Read all files referenced by the relevant workflow.
3. Determine which stages are Opus roles and which are Fable roles.
4. Translate those roles into Codex reasoning and implementation passes.
5. Inspect the current working tree and existing task state.
6. Briefly state the translated execution chain.

## Review independence

An Opus review stage must be treated as an independent review.

Use one of the following, in priority order:

1. a separate review subagent;
2. a fresh Codex session;
3. a fresh high-reasoning review pass that begins from the requirements and diff rather
   than the implementation rationale.

The reviewer must inspect the actual diff, tests, architecture constraints, and
acceptance criteria.

## Cross-model adversarial review

Some workflows in `CLAUDE.md` mandate an adversarial review pass in a **different model
family** than the one that wrote the code, with an explicit **no same-model fallback**
rule (in Claude Code this appears as `/codex:adversarial-review` — Codex is the
different model there).

When **you (Codex) are the implementer, the roles invert: the different model is
Claude.** A fresh Codex session or subagent does NOT satisfy this stage — it provides
independence of context, not of model, and "no same-model fallback" forbids it.

Run the pass by shelling out to Claude Code headless from the repository root, pinned
to **Opus**:

```bash
env -u ANTHROPIC_API_KEY claude -p --model opus "Adversarial review of the committed diff on this branch against <base>. \
Read CLAUDE.md and the repo's review command in .claude/commands/ for the review \
criteria and invariants. Try to REFUTE the implementation: correctness, invariant \
violations, missing tests, scope drift. Review only — change nothing. Report findings \
with file:line."
```

Rules:

* always invoke it as `env -u ANTHROPIC_API_KEY claude -p --model opus …` — stripping
  the API key forces the Claude Max/Pro **subscription** (the OAuth login) instead of
  billing `ANTHROPIC_API_KEY` per token; keep `--model opus`;
* run it from the repo root so it loads `CLAUDE.md` and the harness;
* it reviews the committed diff; never let it edit anything;
* feed its findings back into a Codex fix pass, then re-run until clean, exactly as the
  workflow's fix-loop describes;
* if the `claude` CLI is unavailable, **STOP and report the review stage as BLOCKED** —
  do not self-approve, do not substitute a same-model pass;
* only the designated different-model stage must cross model families — the in-house
  auditor stages defined in `.claude/agents/` may run as Codex passes.

## Conflicts

Apply instructions in this order:

1. Current user instructions.
2. `AGENTS.override.md`.
3. `AGENTS.md`.
4. Project and workflow intent in `CLAUDE.md`.
5. Anthropic-specific implementation details in `CLAUDE.md`.

Never let a Claude-specific model name override the intended project workflow.
