> Jira: SWT-43

# delivery-deny — Deny and Redo for drafted deliveries

No open questions arose; every ambiguity is resolved under "Decisions made
unilaterally" below. Two of those decisions go against the literal brief and
are flagged: the status is spelled `rejected`, not `denied` (D1), and a
failed `jira_comment` row cannot be rejected (D4).

## Source

Ad-hoc, from Salvador, 2026-09-12, verbatim:

> deliveries has an approve button but not a deny one

Asked what deny should do, he chose "Two buttons":

- **Deny**: the draft is marked denied, never sent, and no new draft is written.
- **Redo**: the draft is denied, and the drafts worker writes a fresh one on its
  next run, taking his reason into account.

This is not a build-order step. It completes step 8's approve/edit/send surface
(`08-draft-deliveries`) with the missing negative verdict.

## Goal

Add one executor verb, `reject_delivery {delivery_id, note?, redraft?}`. It
moves an unsent delivery to a new terminal status, `rejected`, and records the
verdict as labelled data. With `redraft` set it also makes exactly that row stop
blocking the drafts worker, which then re-drafts with the rejected body and
Salvador's note in its prompt.

**Usable alone means:** with nothing else deployed except the migration and a
rebuilt dashboard, Salvador opens `/deliveries`, types an optional note on a
drafted row and clicks **Deny** or **Redo**. The row shows `rejected` and can
never be approved, sent, booked, prefilled or marked sent. After a **Redo**, the
next `cmd/drafts` run writes a new drafted row for the same task whose prompt
contains the rejected text and the note. After a **Deny**, no drafts run writes
anything for that task. Nothing outbound happens at any point.

## Decisions made unilaterally (with rationale)

- **D1: the status is `rejected`, the tool is `reject_delivery`, and the
  buttons say "Deny" and "Redo".** Two reasons for not using `denied`:
  - CLAUDE.md says table and tool names are the vocabulary and must not gain
    synonyms. The human negative verdict is already spelled `rejected` in
    `approvals.status` (0001:266), `plan_imports.status` (0008) and
    `reject_plan_import`.
  - `deny` is already the **policy** verb in this repo: `Decision:"deny"`,
    `audit_events.status='denied'`, and `deliveries.policy_result` sits on the
    same row. "status=denied" next to a policy result would read as "the policy
    matrix denied this send", which is a different fact.

  Salvador's own words stay on the buttons. This is a one-word change before
  test-author starts, if he prefers `denied`.
- **D2: one verb with a `redraft` flag, not two tools.** Deny and Redo share
  everything that matters: the deniable-status rule, the lock order, the
  approvals row and the event. Two tools would spell that refusal twice. The
  audit row still tells them apart, because `audit_events.args` carries
  `redraft`.
- **D3: the redo intent is a column on the rejected row,
  `deliveries.redraft_requested_at`.** The drafts worker's `NOT EXISTS` becomes
  "no delivery for the parent other than a rejected row with a redraft
  requested". The alternatives were weaker:
  - A separate marker table would add a second read that must agree with the
    first.
  - A status such as `rejected_redraft` would split one verdict across two
    status values that every reader would have to list.

  With the column, a plain Deny keeps blocking exactly as today, and a Redo row
  stops blocking. The next draft is a new row, so it blocks again. That is the
  loop bound: one human click per re-draft (criterion 27).
- **D4: the rejectable set is `drafted`, `approved`, and `failed` with
  `sent_external_id IS NULL AND confirmed_at IS NULL`, EXCEPT `jira_comment`.**
  This narrows the brief's "same retry set approve accepts" by one channel.
  - `sendJiraComment` writes `failed` with a NULL id for EVERY send error. The
    comment may have landed (delivery.go:1120-1127), and the Jira prefix
    matcher still claims `failed` rows (`jira/sink.go:341`).
  - Rejecting such a row removes it from the matcher's candidate set. A comment
    that did land would then be reported as an `outbound_observed` hand-send.
    That is a false record, and it is permanent.
  - A gmail `failed`+NULL row is different: it only arises from
    `SendRejectedError`, a definite refusal (delivery.go:797-802).

  Refusing is the reversible choice, since widening later is one line. It
  follows the repo rule that a verb must not take a row a matcher might still
  legitimately claim. `sending` and `sent` are never rejectable.
- **D5: rejecting touches no task.** The work task stays `done_locally`, and R3's
  `Deliver #N` task (created with `assignee_type: human`, rules.go:258, so no
  console claims it) stays open in Salvador's lane.
  - After a plain Deny, that open Deliver task is the honest reminder that the
    work is done but undelivered. He finishes with the existing verbs:
    `task_close`/`task_dismiss` on the Deliver task, or `task_mark_delivered` on
    the parent if he sent something by hand.
  - Closing it automatically would make a later Redo impossible, because drafts
    only picks `ready`/`holding` Deliver tasks.
  - It would also repeat the "failing a delivery R8 already processed" landmine's
    shape: delivery state inferred into task state.
- **D6: a plain-rejected row can be upgraded to Redo; a Redo cannot be
  withdrawn.** A mis-clicked Deny otherwise has no recovery short of psql,
  because nothing re-drafts. The reverse is refused: once a redraft is
  requested, the drafts worker may already have written the new row, so
  "withdraw" would be a lie. He denies the new draft instead.
- **D7: Redo requires the work task to be `done_locally`, checked under the
  task lock.**
  - `draft_delivery` refuses any other status when the drafts worker sends
    `expect_task_status: "done_locally"` (delivery.go:438-440). A redraft on any
    other status therefore can never produce a draft. The flag would be absent
    because it is impossible, not because it is pending.
  - This does not re-spell the drafts queue predicate (the `Deliver #%` child
    test). The only residual is a Redo whose Deliver task was closed by hand.
    It stays visibly "redraft requested" with nothing drafting it (see Future
    work).
- **D8: no reason-code enum; one optional free-text note.** Salvador dislikes
  labelling asks. The Deny/Redo choice is already the one-bit label ("unwanted"
  vs "wrong words").
  - The `task_dismissals` enum was justified by a consumer, the precision
    queries. Nothing would consume a delivery code set today.
  - The note is typed (`deliveries.rejection_note`), and it is what the redraft
    prompt consumes.
- **D9: `reject_delivery` is humanOnly and NOT MCP-listed.** It joins
  `mark_delivery_failed` and `prefill_delivery` in `spineTools`. Delivery verdicts
  on a worker's own words belong to the human, and the brief puts MCP profile
  changes out of scope.
  - Note the asymmetry this leaves: `approve_delivery`/`send_delivery` ARE
    MCP-listed (schemas.go:131, 136).
  - An interactive `ops` session can therefore approve but not reject. Recorded
    under Future work, not fixed here.
- **D10: a rejection emits a `delivery_rejected` task event; no orchestrator
  rule reacts to it.** The work task's log (`/tasks/{id}`) then shows the
  verdict and note, the same way `delivery_failed` is informational
  (delivery.go:1400).
  - `Evaluate`'s default branch already returns nil for it (rules.go:132). A
    unit test pins that, so that nobody later routes it into R8.
  - Note that `task_context` returns recent event payloads to a worker console
    resumed on the task. The note is therefore Salvador's own text inside a
    possible future worker prompt. It is his instruction, not third-party text,
    so this is accepted.

## Acceptance criteria

### Migration

1. `migrations/0028_delivery_rejection.sql` does four things:
   - drops and re-adds `deliveries_status_check` with the five existing values
     plus `rejected`;
   - adds `rejection_note TEXT` and `redraft_requested_at TIMESTAMPTZ`;
   - adds `deliveries_rejected_unsent_check`:
     `CHECK (status <> 'rejected' OR (sent_external_id IS NULL AND confirmed_at IS NULL))`;
   - adds `deliveries_rejection_fields_check`:
     `CHECK ((redraft_requested_at IS NULL AND rejection_note IS NULL) OR status = 'rejected')`.

   A unit structure test (`TestMigration0028_...`, the 0027 guard's shape)
   asserts all four clauses and that every one of the five old status values
   survives.
2. The migration ledger in `internal/classify/structure_test.go` learns 28, and
   nothing above 0028 exists on this branch.
3. **Collision rule.** SWT-40 (inquiry-promote, `ticket-inquiry-promote`) names
   0027/0028/0029 in its SPEC, and 0027 is already taken on main by
   `0027_sync_runs_partial.sql`. Whichever of the two branches merges SECOND
   renumbers its files, its per-ticket guard test and the ledger line. Both
   branches edit the same ledger `if` line, so expect a textual conflict there
   regardless.

### Tool: `reject_delivery`

4. It is registered in `tools.Register` (`internal/tools/createtask.go`, beside
   `approve_delivery`) and listed in `allToolNames`.
5. `validateRejectDelivery` refuses:
   - `{}` and a missing or zero `delivery_id`;
   - a `note` over 2,000 runes (it travels into a model prompt);
   - a non-boolean `redraft`.

   `redraft` defaults to false. A whitespace-only note is stored as NULL.
6. It is in `policy.humanOnly` and in neither `sendShaped`, `freezeGated` nor
   `snapshotGated`. `TestDecide_RejectDelivery_FullActorCorpus` runs it over
   `mcpVerbsCorpus` (matrix_mcpverbs_test.go:61): human shapes allow, every
   other shape denies with `human_only`. A second test shows that a frozen,
   over-limit snapshot still allows it, because it moves a row away from the
   world (the `mark_delivery_failed` argument).
7. It is absent from `internal/mcpserver/schemas.go` and added to `spineTools`
   in `internal/mcpserver/adapter_test.go`, with a comment naming D9. The
   existing profile loops then prove the read and user profiles refuse it.
8. **Lock order is task → delivery.** The handler first SHARE-locks the
   delivery's task row (the `refuseClosedTask` query shape, factored into a
   helper that `refuseClosedTask` also calls, so `send_guard_structure_test.go`
   stays green). Then it takes `FOR UPDATE` on the delivery. A plain reject on a
   **closed** task is allowed: cleaning up a stale draft is exactly what closed
   work needs.
9. The transition table, from the row's state as read under the lock:

   | row state | redraft=false | redraft=true |
   |---|---|---|
   | `drafted` / `approved` | → `rejected` | → `rejected` + redraft |
   | `failed`, id NULL, confirmed NULL, channel ≠ `jira_comment` | → `rejected` | → `rejected` + redraft |
   | `failed` `jira_comment` | refuse (D4) | refuse (D4) |
   | `approved` `jira_comment` with `error` set (a failed send approved for retry; review fix) | refuse (D4) | refuse (D4) |
   | `failed` with id or `confirmed_at` | refuse ("may have been sent") | refuse |
   | `sending` / `sent` | refuse | refuse |
   | `rejected`, no redraft | no-op success | upgrade: set `redraft_requested_at`, replace note if one is given |
   | `rejected`, redraft requested | refuse (D6: "deny the new draft instead") | no-op success |

   Every refusal names the delivery id, its status and channel, and the reason.
10. Any transition that sets `redraft_requested_at` refuses unless the task
    status read in criterion 8 is `done_locally` (D7). The refusal message says
    why: a draft can only be written for `done_locally` work.
11. **Atomicity.** A real transition (the first two rows of the table) writes
    all four of the following in ONE transaction:
    - `status='rejected'`, `rejection_note`, `redraft_requested_at`
      (`now()` or NULL), `updated_at`;
    - an `approvals` row `('delivery', id, 'rejected', actor, now())`, the
      `approveDelivery` idiom;
    - a `delivery_rejected` task event on `deliveries.task_id` with payload
      `{delivery_id, channel, redraft, note}`.

    The upgrade writes the column update and one `delivery_rejected` event with
    `redraft:true`, but no second approvals row. A no-op writes nothing.
12. The result is `{delivery_id, status:"rejected", redraft:bool, changed:bool}`.

### Send-path refusals (invariant 4)

13. A `rejected` row is refused by every verb that moves a delivery toward the
    world or edits its words. Each refusal is proven by an integration test that
    seeds a `rejected` row on the relevant channel and asserts the tool errors
    AND the row is byte-unchanged (status, `sent_external_id`,
    `send_attempted_at`):
    - `approve_delivery`
    - `update_delivery`
    - `send_delivery`: gmail, `jira_comment`, `slack_reply`, calendar
    - `book_calendar_block`
    - `prefill_delivery`
    - `mark_delivery_sent`: `upwork_chat`, `slack_reply`, including
      `leaf_gated:true`
    - `mark_delivery_failed`

    Every one of these already refuses through its status allowlist, so no send
    code changes. The tests make that allowlist load-bearing.
14. **Sequential race coverage.**
    - Reject then send: the send refuses.
    - Send phase 1 committed (`sending`) then reject: the reject refuses.
    - Approve then reject: the reject succeeds, and a following `send_delivery`
      refuses.
15. **Connector matchers never stamp a rejected row.** For each of the four
    post-hoc body matchers there is an integration test:
    - google `confirmDeliveryByBodyPrefix`
    - jira `matchByBodyPrefix`
    - slackweb `confirmDelivery`
    - upworkcrm `confirmUpworkDelivery`

    Each test seeds a `rejected` row on the matcher's own scope (same account,
    thread, target or client) whose body has the SAME normalized 120-char prefix
    as the ingested outbound message. The two bodies must differ in raw
    whitespace, per the IK rule "a matcher test whose two bodies are the same
    string tests nothing". The test then asserts the row keeps
    `status='rejected'`, `sent_external_id IS NULL` and `confirmed_at IS NULL`,
    and that no `delivery_confirmed` event is written.
16. Both reconcilers leave `rejected` rows alone: `slackweb/reconcile.go` and
    `upworkcrm/reconcile.go` select only `sending`/`sent`. Their tests assert no
    `error` marker is appended to a rejected row.
17. `deliveries_rejected_unsent_check` is the backstop. An integration test
    shows that a direct `UPDATE deliveries SET confirmed_at=now()` or
    `SET sent_external_id='x'` on a rejected row fails with a check violation.

### Loop closure (invariant 5): a hand-sent copy of a rejected draft

18. **Defined behaviour.** If Salvador sends the rejected words (or anything
    resembling them) by hand, then:
    - the rejected row is NEVER stamped (criterion 15);
    - capture's `hasUnconfirmedClaimant` (observe.go:287) does not treat a
      rejected row as a pending claimant, so the observation is not deferred;
    - `linkedTasks` (observe.go:316, no status filter) attaches one
      `outbound_observed` event to the task.

    `rejected` means "switchboard did not and will not send this row". It is a
    claim about switchboard's action, and `outbound_observed` carries the
    evidence of what the world saw. Recording "delivered after all" uses the
    existing `task_mark_delivered` on the work task; `mark_delivery_sent` refuses
    a rejected row (criterion 13). An integration test in
    `internal/capture` seeds a rejected gmail or slack row plus a matching
    outbound message and asserts exactly one `outbound_observed` event on the
    task, with the row unchanged.

### Drafts worker: Redo

19. `drafts.DeliverTasks`' blocking clause becomes:
    `NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.task_id = t.parent_id AND NOT (d.status = 'rejected' AND d.redraft_requested_at IS NOT NULL))`.
    Status is spelled out rather than trusting the CHECK alone, for the reader.
20. `DeliverTasks` also selects, through a `LEFT JOIN LATERAL` on the parent's
    newest redraft-requested rejected row (`ORDER BY d.id DESC LIMIT 1`), three
    new `DeliverTask` fields: `RedraftOf int64`, `RejectedBody string` and
    `RejectionNote string`.
21. **Integration test, column-fed (IK landmine 6).** A `done_locally` parent
    with an open `Deliver #N` child:
    - (a) plain-rejected row only → not listed;
    - (b) redraft-rejected row only → listed, with `RejectionNote` equal to the
      stored note and `RejectedBody` equal to the stored body;
    - (c) redraft-rejected row plus a newer `drafted` row → not listed;
    - (d) two redraft-rejected rows → listed once, carrying the NEWER one's note.

    Mutations that must go red:
    - drop the new `AND NOT (...)` (so any delivery blocks again) → (b) goes
      unlisted;
    - replace it with `AND d.status <> 'rejected'` → (a) goes listed;
    - replace `d.rejection_note` in the SELECT with `''` → (b) goes red;
    - drop the LATERAL's `ORDER BY` → (d) goes red.
22. `renderUser` appends a section only when `RedraftOf != 0`. It gives the
    rejected draft (truncated to 600 bytes with the existing `truncate`) and
    "His reason:" followed by the note, or "(no reason given)". It instructs the
    model to write a new message that addresses the reason and not to repeat the
    rejected draft. The closing "Draft the message..." line stays last.
    `PromptVersion` becomes `drafts-v2`. `ai_runs.input` gains
    `redraft_of_delivery_id` (0 when absent).
23. Unit test (`worker_test.go`, fake store): a task with
    `RedraftOf/RejectedBody/RejectionNote` set produces a user prompt containing
    both strings, and a task without them contains neither the section header
    nor "His reason". `SystemPrompt` and `DraftSchema` are byte-unchanged; the
    existing schema test keeps passing.
24. The new row is created exactly as today: `draft_delivery` with
    `expect_task_status: "done_locally"` and `Actor = "drafts:gpt"`. It carries
    no link back to the rejected row; the link is `ai_runs.input` only (see
    Future work).
    - Its channel is whatever the drafts worker resolves today (gmail or
      `upwork_chat`), not necessarily the rejected row's channel.
    - A Redo on a worker-authored `slack_reply` or `jira_comment` row therefore
      produces a drafts-resolved row, or a `draft_skip` log on the Deliver task
      when nothing resolves. Both outcomes are visible.
25. **Locality (SWT-21) is unchanged.** The class fold uses the same inputs, and
    the rejected body and note add no attribution. The body was produced from
    this same task's context. The note is Salvador's instruction to the drafter
    and is accepted as such. This is stated in a code comment beside the new
    prompt section.
26. After a redraft-driven draft is written, a second `drafts.Run` in the same
    test drafts nothing for that task (the new `drafted` row blocks).
27. **Loop bound.** Nothing in this ticket writes `redraft_requested_at` except
    `reject_delivery`, and that verb is humanOnly. A structural test scans
    `internal/` (non-test Go) for the literal `redraft_requested_at` and fails on
    any writer outside `internal/tools`. Readers in `internal/drafts` and
    `internal/dashboard` are allowed by name.

### Orchestrator

28. `TestEvaluate_DeliveryRejectedFiresNothing` (rules_test.go, the
    `TestEvaluate_CaptureEventsFireNothing` shape) keeps the positive control and
    checks `delivery_rejected` with `redraft` true and false; both must return
    zero actions. Mutation: add a `case "delivery_rejected":` routed to
    `ruleDeliveryLifecycle` → red. `internal/orchestrator` is not edited.

### Dashboard

29. Route `POST /deliveries/{id}/reject` on the auth-required mux. A new handler
    (the `actionEdit` shape) parses the form and builds the args with
    `json.Marshal`: `delivery_id` from the path, `note` from the `note` field,
    and `redraft` true iff the submitted `redraft` value is `"true"`. It then
    calls `s.execute(..., "reject_delivery", ...)` with actor
    `dashboard:{user}`, which 303s to `/deliveries` with the flash. The handler
    runs no SQL of its own.
30. `deliveries.html`:
    - Rows in `drafted`, `approved` or `failed` get ONE inline
      `<form method="post" action="/deliveries/{{.ID}}/reject">` with a text
      input `name="note"` and two submit buttons,
      `<button name="redraft" value="false">Deny</button>` and
      `<button name="redraft" value="true">Redo</button>`.
    - A `rejected` row without a redraft gets the same form with the Redo button
      only.
    - A `rejected` row shows its note and, when set, "redraft requested" under
      the status.
    - `.status-rejected` gets a style.
    - The filter links gain `<a href="/deliveries?status=rejected">rejected</a>`.
    - No `onchange` anywhere.

    The jira-failed and id-bearing refusals surface as the flash. The template
    does not restate D4.
31. `listDeliveries` selects `COALESCE(d.rejection_note,'')` and
    `d.redraft_requested_at IS NOT NULL` into the new `deliveryRow` fields.
32. **New unit structure test** `internal/dashboard/deliveries_structure_test.go`.
    None pinned the delivery buttons before. It reads the embedded template and
    asserts:
    - the Approve form posts to `/deliveries/{{.ID}}/approve`;
    - the reject form posts to `/reject` and carries `name="note"`,
      `name="redraft" value="false"`, `name="redraft" value="true"`, the labels
      `Deny` and `Redo`, and the `status=rejected` filter link;
    - there are zero `onchange` occurrences.
33. **Integration test through the real handler** (joining
    `dashboard_integration_test.go`'s cleanup pact), POSTing to
    `/deliveries/{id}/reject`:
    - with `redraft=true` and a note containing `"`, `\` and a newline, it
      asserts the 303 and the stored `rejection_note` byte-equal to the input.
      This proves `json.Marshal`, not `Sprintf`;
    - with `redraft=false` it asserts `redraft_requested_at IS NULL`;
    - `GET /deliveries?status=rejected` lists both.
34. The task detail page (`board.go:332-342`, `task.html:67-72`) is unchanged; it
    already renders `status`, so a rejected delivery reads `rejected` there.

### Labelled data

35. The label query below runs verbatim in the integration suite over one
    Deny and one Redo, and returns both rows with the correct `redraft` bit and
    `decided_by = 'dashboard:…'`. Nothing reads the `delivery_rejected` payload
    for labels.

```sql
SELECT d.id, d.channel, d.created_by, d.body, d.rejection_note,
       (d.redraft_requested_at IS NOT NULL) AS redraft,
       a.decided_by, a.decided_at
  FROM deliveries d
  JOIN approvals a ON a.subject_type = 'delivery' AND a.subject_id = d.id
                  AND a.status = 'rejected'
 WHERE d.status = 'rejected'
 ORDER BY a.decided_at;
```

`approvals` now holds both verdicts per delivery. That is what the policy
matrix's "approval-without-edit rate" needs as a denominator; before this
ticket a rejected draft was simply never approved and therefore invisible.

## Data model changes

**`migrations/0028_delivery_rejection.sql`** is the only migration:

```sql
ALTER TABLE deliveries DROP CONSTRAINT deliveries_status_check;
ALTER TABLE deliveries ADD CONSTRAINT deliveries_status_check CHECK (status IN
  ('drafted','approved','sending','sent','failed','rejected'));

ALTER TABLE deliveries
  ADD COLUMN rejection_note       TEXT,
  ADD COLUMN redraft_requested_at TIMESTAMPTZ;

-- A rejected row was never sent by switchboard and never will be (invariant 4).
ALTER TABLE deliveries ADD CONSTRAINT deliveries_rejected_unsent_check
  CHECK (status <> 'rejected' OR (sent_external_id IS NULL AND confirmed_at IS NULL));

-- The rejection fields exist only on rejected rows.
ALTER TABLE deliveries ADD CONSTRAINT deliveries_rejection_fields_check
  CHECK ((redraft_requested_at IS NULL AND rejection_note IS NULL) OR status = 'rejected');
```

- `deliveries_status_check` is the name Postgres generated for 0001's inline
  CHECK. The 0009 precedent (`deliveries_channel_check`) relied on the same
  rule. Verify it on pg-main before applying (Verification step 5). The
  drop/add is safe because migrate runs each file in one transaction (IK, Slack
  Web connector entry).
- **No index.** `deliveries_status_idx` (0006) already serves `status` reads.
  The drafts `NOT EXISTS` is per parent task, and deliveries holds a handful of
  rows per task.
- **Deploy order is migrate-then-roll, with no cutover.** The change is
  additive. Old binaries never write `rejected`. An old drafts worker treats a
  rejected row as blocking, which is Deny's semantics. An old dashboard renders
  the status string with no buttons. The 0026 drain does not apply here
  (nothing drops an index that an old `ON CONFLICT` infers).
- Who rejected and when lives in `approvals` (`decided_by`, `decided_at`), not
  in duplicate delivery columns.

## API / MCP tool changes

**New: `reject_delivery`**, spine-facing:
`{delivery_id: int, note?: string, redraft?: bool}` →
`{delivery_id, status: "rejected", redraft, changed}`.

Its executor hook-in (invariant 3):
- validate: `validateRejectDelivery`;
- policy: `humanOnly` → `Decide` → `allow / matrix-human` for humans;
- audit start and complete: the executor;
- handler: `rejectDelivery` in `internal/tools/delivery.go` (next to
  `approveDelivery`).

It is reachable from the dashboard and from
`opsctl call --tool reject_delivery --args '{...}'`, and deliberately NOT over
MCP (D9).

**Modified behaviour, no signature changes:**
- `drafts.DeliverTasks` (blocking clause and LATERAL fields);
- `drafts.renderUser` and `PromptVersion`;
- dashboard `listDeliveries` and the template.

Existing tools change in two places, both from the review fixes: `draft_delivery` refuses on the drafts-worker path (`expect_task_status` set) when a blocking delivery exists for the task (`tools.BlockingDeliverySQL`, under the task lock), and approve_delivery validates `expect_content_hash` with the shared `checkContentHashShape`. Every send path's existing status allowlist is
what refuses `rejected` (criterion 13).

## MQTT topics

None.

## Files likely to touch

- `migrations/0028_delivery_rejection.sql`
- `internal/tools/delivery.go`: `rejectDeliveryArgs`, `validateRejectDelivery`,
  `rejectDelivery`, and a lock helper shared with `refuseClosedTask`.
- `internal/tools/createtask.go`: the `Register` table.
- `internal/tools/tools_unit_test.go`: `allToolNames`.
- `internal/tools/reject_delivery_test.go` (new, unit: validation) and
  `internal/tools/reject_delivery_integration_test.go` (new: criteria 8-14, 17,
  35).
- `internal/tools/reject_structure_test.go` (new): the 0028 guard (criterion 1)
  and the writer scan (criterion 27).
- `internal/policy/matrix.go`: `humanOnly`; tests in
  `internal/policy/matrix_mcpverbs_test.go` or a new
  `matrix_reject_test.go`.
- `internal/mcpserver/adapter_test.go`: `spineTools`.
- `internal/drafts/store.go`, `internal/drafts/drafts.go`,
  `internal/drafts/worker_test.go`, and
  `internal/drafts/store_integration_test.go` (or a new
  `redraft_integration_test.go` in the same cleanup pact).
- `internal/connector/{google,jira,slackweb,upworkcrm}/*_integration_test.go`:
  criterion 15, one test per matcher; criterion 16 in the two reconciler suites.
- `internal/capture/observe_integration_test.go` (or the existing observe
  suite): criterion 18.
- `internal/orchestrator/rules_test.go`: criterion 28.
- `internal/dashboard/server.go` (route and handler, `deliveryRow`,
  `listDeliveries`), `internal/dashboard/templates/deliveries.html`, new
  `internal/dashboard/deliveries_structure_test.go`, and
  `internal/dashboard/dashboard_integration_test.go`.
- `internal/classify/structure_test.go`: the ledger.
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` "Delivery contract": one entry covering
  the `rejected` status, D4's jira exclusion, the redraft column as the drafts
  unblock, and "every new matcher's status set must exclude `rejected`".

## In scope / Out of scope

**In scope:**
- migration 0028;
- `reject_delivery` (validate, policy, handler, event, approvals row);
- the Redo path in the drafts worker (query, fields, prompt);
- the dashboard Deny/Redo form, the filter link and the structure test;
- the refusal, matcher and loop-closure tests;
- the label query;
- the IK entry.

**Out of scope:**
- **Reply-all / cc** on gmail drafts.
- **MCP profile changes**, including listing `reject_delivery` on MCP or pulling
  `approve_delivery` off it (D9).
- A reason-code enum or per-channel reason sets (D8).
- Closing or retitling the Deliver task on Deny (D5).
- Deny/Redo buttons on `/tasks/{id}`.
- Recording `update_delivery` edits as labelled diffs.
- A `deliveries.redraft_of` / `ai_run_id` FK linking a redraft to the row it
  replaces.
- Multi-rejection history in the prompt (only the newest redraft-rejected row is
  used).
- Recovery for the `failed` `jira_comment` ambiguity (D4).
- The SWT-20 compensating transition for upwork.
- Anything in `internal/orchestrator`.
- Adjacent step-9/10 work, including the Jira connector's send path and plan
  import.

## Invariants that apply

1. **Raw-first:** not exercised. Nothing is ingested. The matcher tests seed
   through each sink's normal path, which is already raw-first.
2. **One funnel:** no new table. The verdict lives on the existing `deliveries`
   row plus an existing `approvals` row. The redraft marker is a column on the
   rejected row, not a queue. The drafts queue stays a filter over `tasks` and
   `deliveries`.
3. **Everything through the executor:**
   - the dashboard handler makes exactly one `s.ex.Execute` call and no SQL of
     its own for the action;
   - `approvals`, `deliveries` and `task_events` are written inside
     `rejectDelivery`'s transaction;
   - the drafts worker still writes only through `draft_delivery` and
     `task_append_log`.

   No `raw_sql` path is added, and `redraft_requested_at` has one writer
   (criterion 27).
4. **Nothing external without a delivery row:** a rejected row is outside every
   send path's allowlist (criterion 13). It is outside every matcher's and
   reconciler's candidate set (criteria 15-16). The schema forbids a sent id or
   a confirmation on it (criterion 17).

   Sending and reject serialize on the delivery row lock (criterion 14). The
   gmail belt re-evaluates `status IN (...)` after its `FOR UPDATE` wait, so a
   reject committed first drops the row; a belt stamp committed first makes the
   reject refuse on `confirmed_at`.

   Accepted residual: a `slack_reply` already prefilled into a composer is not
   cleared by a reject. Pressing Send by hand after that is a hand-send, and
   criterion 18 covers it.
5. **Own-message loop closure:** defined in criterion 18. A rejected row is
   never claimed, and a hand-sent copy becomes `outbound_observed` on the task.
   It is never re-triaged, because capture's `direction='inbound'` filter is
   untouched.
6. **Stealth attribution:** the redraft goes through `draft_delivery`, which
   scrubs body and subject exactly as today. The note is Salvador's own text,
   stored and fed to the drafter, never sent. `SystemPrompt`'s no-attribution
   rules are byte-unchanged (criterion 23).
7. **Orchestrator purity:** no rule is added. `delivery_rejected` falls through
   `Evaluate`'s default (criterion 28). The verdict writes executor audit rows.
   The orchestrator's package graph (`deps_test.go`) is untouched.

## Sibling patterns to copy

- **Verdict + approvals row:** `approveDelivery` (delivery.go:579-626) and
  `decidePlanImport` (planimport.go:149-187) for the `rejected` approvals write
  and the idempotent-replay rule.
- **Lock order and the closed-task read:** `refuseClosedTask`
  (delivery.go:480-497) and its FOR SHARE rationale.
- **Human-only, off-MCP delivery verb:** `markDeliveryFailed`
  (delivery.go:1322-1408) and its `humanOnly` comment in `matrix.go:69-75`.
- **Actor corpus:** `TestDecide_TaskDismiss_FullActorCorpus` over
  `mcpVerbsCorpus`.
- **"Fires nothing" guard with a positive control:**
  `TestEvaluate_CaptureEventsFireNothing`.
- **Column-fed integration proof:** `internal/drafts/locality_store_integration_test.go`
  (landmine 6).
- **Form with JSON-marshalled args:** `actionEdit` (server.go:168-182).
  **Template structure test:** `board_structure_test.go`.
- **Migration guard:** `internal/connector/slackweb/migration0027_structure_test.go`
  (DROP/ADD of a status CHECK, asserted by clause).
- **Queue claims (`FOR UPDATE SKIP LOCKED`):** not used here.

## Verification protocol

Run in order, and do not commit before step 6 passes.

1. `go test ./...` covers:
   - validation;
   - the policy corpus and the kill-switch test;
   - `spineTools` and the profile loops;
   - the 0028 guard and the writer scan;
   - the migration ledger;
   - the prompt tests;
   - the orchestrator fire-nothing test;
   - the deliveries template structure test;
   - `TestSendPaths_AllCallRefuseClosedTask` (must stay green after the lock
     helper refactor).
2. `make integration` (`db-up` + `migrate` + `go test -tags integration ./...`,
   serialized) covers criteria 8-18, 21, 26, 33 and 35. Suites must be
   rerunnable: clean up in FK order, and note that `approvals` has no FK to
   deliveries, so delete `approvals WHERE subject_type='delivery' AND subject_id
   IN (...)` explicitly. Delete `policy_decisions` and `audit_events` by task id
   before tasks (the IK SWT-37 landmine).
3. `psql "postgres://ops:ops@localhost:5433/ops?sslmode=disable" -tAc "SELECT max(version) FROM schema_migrations"`
   → `0028`.
4. **Mutation checks.** Each must turn the named test red, and the change is
   reverted afterwards:
   - remove `reject_delivery` from `humanOnly` → criterion 6;
   - add `case status == "rejected":` to `approveDelivery`'s switch →
     criterion 13;
   - add `'rejected'` to the status list of the jira matcher (`jira/sink.go:341`)
     and of the slackweb matcher (`slackweb/sink.go:312`) → criterion 15 for
     each;
   - drop `deliveries_rejected_unsent_check` from 0028 on a throwaway db →
     criterion 17;
   - the four drafts query mutations in criterion 21;
   - drop the `done_locally` check → criterion 10;
   - allow the D6 withdraw → criterion 9's table row;
   - route `delivery_rejected` to R8 → criterion 28;
   - replace `json.Marshal` with `fmt.Sprintf` in the handler → criterion 33.
5. **pg-main pre-flight (merging is not applying):**
   ```
   psql -h 192.168.50.49 -U ops -d ops -tAc "SELECT max(version) FROM schema_migrations"   # expect 0027
   psql -h 192.168.50.49 -U ops -d ops -tAc "SELECT conname FROM pg_constraint WHERE conrelid='deliveries'::regclass AND contype='c'"   # must list deliveries_status_check
   psql -h 192.168.50.49 -U ops -d ops -tAc "SELECT status, channel, count(*) FROM deliveries GROUP BY 1,2 ORDER BY 1,2"   # record: which rows the new buttons will appear on
   DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/tools/migrate
   ```
   Then re-run the first query and expect `0028`. If SWT-40 merged first, the
   expected numbers shift (criterion 3).
6. **Manual smoke** against the compose db: a local `cmd/dashboard` on :8085
   with dev-login. Seed the following through `opsctl call` (humans only):
   - a project with `ai_locality='local_only'` and a done_locally task with a
     `Deliver #N` child;
   - two drafted gmail deliveries on separate tasks via
     `opsctl call --tool draft_delivery`.

   Then:
   - Click **Deny** on the first with a note. It reads `rejected`, and
     Approve, Send and Deny disappear.
     `opsctl call --tool approve_delivery --args '{"delivery_id":N}'` errors.
   - Click **Redo** on the second with the note "shorter, no apology". Run
     `DATABASE_URL=... OPS_LOCAL_PROVIDER_URL=http://192.168.50.55:11434 go run ./cmd/drafts --limit 1`,
     which uses the z4 lane (IK: never a hosted client for a local_only
     fixture). A new drafted row appears for that task only, and its `ai_runs`
     row has `prompt_version = drafts-v2`, a `redraft_of_delivery_id`, and the
     note in `user_prompt`.
   - Run drafts again: nothing new.
   - Click **Redo** on the first (the upgrade): the next drafts run drafts it.
   - Run the label query: two rows.
   - Production is smoke-checked only after the kube session rolls a new
     dashboard image. Hand that off; do not edit `kube/` from here.

## Review fixes (go-reviewer, Codex), 2026-09-12

1. **The D4 hole.** A failed `jira_comment` approved for a retry, then
   rejected, slipped past D4. The refusal now also covers an `approved`
   `jira_comment` with `error IS NOT NULL`: `sendJiraComment` writes `error` on
   every failure, and approve does not clear it.
2. **The Redo race.** `draft_delivery` with `expect_task_status` (the drafts
   worker) re-checks, under the task lock, that no blocking delivery exists.
   The predicate is `tools.BlockingDeliverySQL`, one spelling shared with
   `drafts.DeliverTasks`. On a hit it refuses with `tools.ErrDeliveryBlocksDraft`,
   which the worker counts as a skip. This also closes the same race for first
   drafts.
3. **The note in the prompt.** The rejected body and the note are quoted as
   data between explicit markers, with marker copies neutralised, and framed as
   his feedback rather than instructions. `SystemPrompt` is unchanged. There is
   deliberately no authorization beyond humanOnly.
4. **The locality comment** now says the rejected body can come from a worker
   console or a user-scope session. It is still the task's own delivery, on
   the task's own thread, under the same-project and provenance rules.
5. **Deny/Redo bound to the words shown.** The reject forms carry the
   `content_hash`; `reject_delivery` takes an optional `expect_content_hash`;
   the dashboard route requires it (the `approveAction` shape).
6. **The writer scan** flags any assignment (CASE included), scans `cmd/`, and
   has a pattern probe.
7. **Docs.** `docs/runbooks/HANDOFF-kube-delivery-deny.md` (0028 before any
   roll). IK notes. The D7 refusal no longer says "Deny it instead" to an
   already denied row; it says the row "is already denied and stays that way".

## Future work (not this ticket)

- `deliveries.redraft_of` FK (or `ai_run_id`), so that the triple (rejected
  draft, note, accepted redraft) is one typed join rather than an
  `ai_runs.input` jsonb key. This is the most valuable training pair the system
  produces.
- A reason-code enum, once a report consumes it.
- Label `update_delivery` edits (a before/after diff) as the other half of
  approval-without-edit.
- Surface "redraft requested, but the Deliver task is closed or the parent moved
  on" (the D7 residual) on the dashboard. The same surface should cover the
  **silent wait**: a Redo stays "redraft requested", unlogged, whenever another
  non-rejected delivery exists on the same parent, because every delivery but
  the Redo row blocks the drafts queue (IK, Delivery contract).
- MCP symmetry: either list `reject_delivery` for human sessions or unlist
  `approve_delivery` (D9).
- A distinct `failed_definite` vs `failed_ambiguous` state for Jira sends. That
  would let D4's exclusion (and approve's risky Jira retry) key on evidence
  instead of on the channel.
- Deny/Redo from `/tasks/{id}`.
