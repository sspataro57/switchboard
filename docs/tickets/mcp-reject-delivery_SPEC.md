# mcp-reject-delivery — let sessions drop stale deliveries over MCP (swb 4565)

Status: PENDING-SYNC (the personal Jira site is deactivated; see IK "Jira build tracker").

## Why

Salvador, 2026-11-22: "we need to allow claude to discharge stale comms on swb". A session
that drafts a reply, then drafts a better one (or sends through another task), leaves the
first row `drafted` or `approved`. An `approved` stale row can still be sent by mistake.
Interactive sessions could approve (full profile) but not reject: SWT-43 D9 kept
`reject_delivery` off MCP and recorded the asymmetry as Future work ("MCP symmetry"). The
live case: deliveries 489 (approved, superseded by sent 491 on the same thread), 493 and 494
(drafted on closed task 4555). Those three were rejected by hand through opsctl on
2026-11-22; this ticket gives sessions the verb.

## Decisions

- **D1: list `reject_delivery` on BOTH MCP profiles** (ops-mcp full and ops-mcp-user). The
  stale rows come from sessions in any repo (489/493/494 were created by `mcp:manual:salvo`).
  The entry is a slice of `agentTools`, one spelling.
- **D2: the policy gate is unchanged.** `reject_delivery` stays `humanOnly` (rule
  human_only): `mcp:manual:salvo` passes, every worker console shape (`mcp:{client}`,
  `mcp:{client}.{sub}`) is denied. Already pinned by
  `TestDecide_RejectDelivery_FullActorCorpus`. Not send-shaped: it moves a row away from the
  world.
- **D3: discard only — `redraft` is refused for every MCP caller** by an adapter guard
  (`rejectRedraftArgs`, in CallTool before the executor, fail-closed on a decode error).
  Redo wakes the GPT drafts worker; that stays a dashboard click (SWT-43's loop bound: one
  human click per re-draft). The schema omits `redraft` and `expect_content_hash` (a schema
  is not an argument boundary, F7, hence the guard). `expect_content_hash` passed anyway is
  harmless and forwarded.
- **D4: the handler is unchanged.** It already refuses `sending`/`sent`, a failed
  `jira_comment` (D4 of SWT-43), and a failed row with a sent id; it allows a closed task
  ("cleaning up a stale draft is exactly what closed work needs"); a replay is a no-op success.
- **D5: the words.** Instructions gain `"swb drop <delivery id>"`. The write-tools paragraph
  gains a third unasked exception: a session may drop a delivery IT drafted earlier in this
  conversation once that draft has been replaced by a newer one or its words went out
  another way, and must say so. Any other drop needs Salvador's ask.

- **D6: a session-written note may reach the drafts prompt (accepted).** A dashboard Redo
  with an empty reason keeps the stored `rejection_note`, which an MCP discard may have
  written; drafts.renderUser then quotes it as "his reason". Accepted rather than blocked:
  the rejected body quoted beside it is already session-written (`draft_delivery`), both
  sit between neutralised markers as data, the note is capped at 2000 runes, and the
  dashboard shows the stored reason before the Redo click. Comments and IK that called the
  note "his own text" are amended.
- **D7 (accepted risk, runbook item 5):** a prompt-injected session can drop any unsent
  delivery, an approved client reply included. Nothing notifies; the Deliver task stays
  open on the board. Same "prompt rule, not a boundary" stance as task_reopen.

## Changes

- `internal/mcpserver/schemas.go`: `reject_delivery` entry `{delivery_id int, note string}`,
  required `[delivery_id]`.
- `internal/mcpserver/adapter.go`: `userProfileTools` += `reject_delivery`; CallTool guard
  `rejectRedraftArgs`.
- `internal/mcpserver/serve.go`: the verb line and the exception.
- Tests: `reject_mcp_test.go` (surface, schema, guard, words); counts 37 → 38 (full) and
  25 → 26 (user) amended where pinned; `spineTools` loses `reject_delivery` (amended comment).
- Docs: IK (SWT-43 entry amended), `docs/runbooks/ops-mcp-user-scope.md`.

## Acceptance

1. Full profile lists 38 tools, user 26, read 3; `reject_delivery` on full and user, entries
   byte-identical.
2. Schema properties exactly `delivery_id` (integer), `note` (string); required
   `[delivery_id]`; no `redraft`, `worker_id`, `expect_content_hash`.
3. `redraft: true` (and a non-decodable args object) is refused before the executor on both
   profiles; `redraft: false` / absent forwards as `mcp:<worker id>`.
4. Through the production matrix, `mcp:manual:salvo` is allowed and `mcp:foundry` is denied
   human_only.
5. Instructions carry the verb line and the exception.
6. Live: from an interactive session, drop a throwaway drafted delivery; it reads `rejected`
   with an `approvals` row and a `delivery_rejected` event; a worker-shaped call is refused.

## Future work

- A read listing a task's live deliveries for sessions (today ids come from the session's own
  `draft_delivery` results, Salvador, or `task_context`).
