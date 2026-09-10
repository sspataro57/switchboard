# task-list-mcp — open questions

**ALL THREE ANSWERED 2026-09-10 and folded into the SPEC, which is not provisional.**
This file records what was asked, what was decided, and why.
- Q1/Q2, Salvador, verbatim: "privacy is not important. filter so no junk and wasted
  tockens filtering". The second half became a design driver for the whole response
  shape (SPEC L15, criterion 7), not just Q2's answer.
- Q3, Salvador, verbatim: "List them anyway".

---

## Q1 — Should a worker console be limited to its own client's projects?

**Background.**
- A worker's identity is `mcp:{client}` (or `mcp:{client}.{sub}`), because opsworker
  sets `OPS_WORKER_ID` to the bare client. It is not `mcp:worker:{client}`.
- Nothing on the MCP surface is scoped by client today: `task_get_next` accepts any
  client, and `task_context` returns any task by id.
- The worker's claude process also inherits `DATABASE_URL` and runs with
  `--dangerously-skip-permissions`.

The options were:
- **(a)** no scoping;
- **(b)** a client scope that opsworker writes into ops-mcp's environment and the
  server injects into the args. That is keyed on server configuration rather than
  the actor prefix, but it only prevents accidents.

**ANSWERED 2026-09-10 — (a), no scoping for any actor.** "privacy is not important."

**Rationale and consequences folded into the SPEC:**
- Every caller sees whatever project it names, the same as `task_get_next` and
  `task_context` (SPEC L2a). The adapter injects nothing beyond `worker_id`
  (criterion 20).
- (b) is recorded as "not done, and why" in one line. Privacy is not a goal, and a
  scope on one reader in three, while the worker holds a DB credential, would have
  looked like a boundary without being one (the SWT-19 go-live-gate lesson).
- *Superseded by Q3:* this answer originally kept a `local_only` refusal, on the
  ground that locality is not privacy. Q3 removed that refusal. Q1's own answer
  stands.
- INSTITUTIONAL_KNOWLEDGE gains a dated standing fact at deliver: cross-client
  privacy is not a goal, and the MCP surface is unscoped by client.

---

## Q2 — By default, should `task_list` hide `delivered` tasks as well as `closed`?

**Background.** The brief said to mirror the board, and that the board hides closed
and delivered. It actually hides only `closed` (`internal/dashboard/board.go:72`).
Nothing moves `delivered → closed` automatically, so delivered work stays on the
board until someone closes it by hand.

**ANSWERED 2026-09-10 — (b), hide both `closed` and `delivered`.** "filter so no junk
and wasted tockens filtering."

**Rationale and consequences folded into the SPEC:**
- The default set is named `in_play`: every status except `closed` and
  `delivered`. It is spelled once, as `t.status NOT IN ('closed','delivered')`, in a
  const shared by `task_list` and `project_list`'s count (SPEC L4, criterion 6).
- **It differs from the board on purpose, and the SPEC says so plainly.** The board
  is a human view. `task_list` output lands in a model context, where every
  delivered row is tokens spent on work that needs nothing more.
  - The board's default is not changed.
  - The two predicates are deliberately not shared, and neither is
    `internal/promote`'s look-alike.
- An explicit `status` still returns `closed` or `delivered` when a caller means it
  (criteria 3 and 9).
- `project_list`'s per-project count is named `in_play`, because it follows the
  same predicate (criterion 16).
- **Decided alongside, not asked:**
  - `holding` stays IN the default (SPEC L4a). A holding task is a review decision
    owed by Salvador, which is in play for him, and it is counted separately.
  - `done_locally` stays for the same reason: its delivery is still pending.
- **Decided alongside under the same "no junk" rule** (SPEC L6, L9, L11, L15):
  - rows dropped `plan_order` and `created_at`;
  - `subproject`/`parent_id` and `project_list`'s `client` are omitted when empty;
  - the default limit went from 50 to 25;
  - the exact key sets are pinned by a unit test (criterion 7).

---

## Q3 — What should `task_list` do for `local_only` projects?

**What prompted it.** Verification step 0 on production found **six** `local_only`
projects, not the two the migrations create: `bulk`, `homelab`, `personal`,
`foundry`, `saka` and `town-ai`.
- The last four have clients, and saka and town-ai each have a ready task.
- 0016's column default is `'local_only'` (fail-closed), and every project created
  by hand since took it.
- The SPEC then refused `local_only` projects for every caller (old L3). That would
  have turned the tool off in most of Salvador's repos. The SPEC's own step 5
  example, `homelab`, was one of them.

**The question as put:** "task_list only returns task titles, status and priority,
never message bodies. What should task_list do for local-only projects?"

**ANSWERED 2026-09-10 — list them anyway.** "List them anyway". `local_only`
projects list normally, for every actor.

**Rationale and consequences folded into the SPEC:**
- **Ground (SPEC L3):**
  - `task_list` rows carry `title, status, priority, assignee_type`, plus
    `subproject`/`parent_id` when set, and never a body. The body is the content
    the locality rule exists to keep off hosted models.
  - The rule itself is unchanged where it is enforced today: classify, drafts and
    triage.
  - The owner decided it.
- **`task_list` and `project_list` no longer read `projects.ai_locality`.** The
  refusal, its handler step and its column-mutation test are gone.
- Criterion 12 flips to `TestTaskList_Integration_LocalOnlyListsForEveryActor`:
  - a `local_only` project lists for all nine actor shapes, identically in shape to
    an `any` project;
  - no row carries a `body` key;
  - the seeded body text appears nowhere in the response.
- Criteria 7 and 12 are now the load-bearing guard: the body-free row shape is what
  makes this decision acceptable, so a later field addition cannot quietly
  reintroduce it.
- `project_list` drops its `local_only` key (SPEC L11). Its only job was to stop a
  session memorising a slug `task_list` would refuse; with nothing refused, it is
  noise under the "no junk" rule. The row is `{slug,name,in_play}` plus `client`
  when set (criteria 7 and 16).
- **The honest residual, recorded in SPEC Future work rather than hidden:**
  - `task_context` still returns any task's BODY by id, whatever its locality. That
    is pre-existing.
  - A promoted `personal` task's TITLE may be derived from private mail, and it now
    reaches a hosted model through `task_list`.
- **Lesson for INSTITUTIONAL_KNOWLEDGE:** "measure the table, not the migration".
  The SPEC's first draft counted `local_only` projects from 0016/0018 and got two.
  A fail-closed column default describes the rows the migration wrote, not the rows
  operators wrote since.
- Separate operator follow-up, not this ticket: whether foundry, saka, town-ai and
  homelab should really be `local_only` for the classify, drafts and triage lanes.

---

All three questions were answered by Salvador on 2026-09-10, relayed by the
coordinator. No questions remain open; the SPEC is ready for `test-author`.
