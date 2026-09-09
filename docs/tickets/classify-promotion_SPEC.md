> Jira: SWT-30

# classify-promotion — promote personal classify verdicts into tasks

**STATUS: ANSWERED** (Salvador, 2026-09-09; see
`docs/tickets/classify-promotion_OPEN_QUESTIONS.md`). Q1 = holding tasks and
Q2 = verdict-clock-only confirm the assumptions this SPEC was written against.
Q3 flipped: the attach lookup considers only OPEN tasks — a thread whose task
is already `closed`/`delivered` gets a NEW task (criterion 9).

## Source

Ad-hoc, from Salvador ("allow personal project board to fillup"), verbatim:

> Promote personal classify verdicts into tasks so the personal board fills up.
> Forward-only (no backfill of historical flags): from a cutover timestamp,
> actionable verdicts from the personal lane create canonical tasks rows. Kind
> whitelist for auto-creation (payment_due, deadline); every other flagged kind
> lands in a human-review lane, never a live task. Attach-before-create: dedup
> so a re-classified or follow-up message attaches to the existing task instead
> of creating a duplicate. Shadow mode stays the default for every other lane;
> residue lane (classify_residue) explicitly excluded. Deterministic promotion —
> the promoter never calls an LLM, it reads stored ai_extractions verdicts only.

Not a build-order step. It is the deliberate, one-lane exit from shadow that
CLAUDE.md build-order step 6 describes in the abstract ("Run SHADOW MODE first";
"below threshold → human-review lane, never a live task"), applied to the local
classify lane instead of GPT triage.

## Goal

A deterministic promoter that turns stored `ai_extractions` verdicts from the
personal classify lane (`ai_runs.worker_type='classify'`) into `tasks` rows for
project `personal`, forward-only from a stored per-project cutover, whitelisted
by verdict kind, deduped by conversation, and creating everything through the
executor.

**Usable alone means:** after one `classify promote` run against the real ops
db, the `/tasks?project=personal` board carries a task for every whitelisted
actionable verdict recorded since the cutover, non-whitelisted flags are
visible in the review lane, and a second run creates nothing. No other lane,
project or worker changes behaviour, and nothing outbound happens at any point.

## Decisions made unilaterally (with rationale)

- **D1 — the cutover is a `projects` COLUMN, not a `policies` jsonb key.**
  `projects.classify_promote_after TIMESTAMPTZ` (NULL = promotion off for that
  project, the fail-closed side). Repo precedent is explicit and recent: 0016
  made `ai_locality` a column and 0018 made `ai_classify` one, and
  `internal/classify/structure_test.go:947` records the reason in the form the
  next session will read — "*not a key in projects.policies … an untyped
  predicate over jsonb is exactly the thing this repo keeps paying for*".
  Per-project rather than an `ops_flags` row because promotion eligibility is a
  property of the project, exactly like `ai_classify`: it composes with the
  existing lane predicates and a future second local-classify project needs no
  code change. `ops_flags` stays what it is — the global send kill switch.
- **D2 — the kind whitelist is a Go constant in the promoter package, not
  configuration.** `{payment_due, deadline}`, a subset of the closed enum in
  `internal/classify/prompt.go` (`payment_due | deadline | appointment |
  action_required | informational`). The whitelist IS the argument this ticket
  is making about autonomy; as a DB row a typo widens auto-creation with no
  review, and promotion stops being a pure function of stored rows (invariant
  7). Widening it later is a one-line diff plus a test.
- **D3 — new package `internal/promote`, driven by a new `classify promote`
  subcommand.** A separate package is what makes "the promoter never calls an
  LLM" *structural*: `internal/promote` must not import
  `internal/provider`, and a structure test asserts it. Putting it in
  `internal/classify` would make that assertion impossible — that package
  imports `provider` by construction.
- **D4 — a promotion decision log table, `classify_promotions` (migration
  0021).** Same role and same reasoning as `capture_decisions`: an append-only
  log written directly by its own package (the `ai_runs`/`ai_extractions`
  precedent), carrying no status, no assignee and no claim, so it is not a
  second tasks table (invariant 2). `UNIQUE (normalized_message_id)` is the
  whole idempotency story and it is structural, not advisory.
- **D5 — the attach key is `tasks.source_thread_id`, not `external_refs`.**
  SWT-20 rejected `external_refs` as a provenance store for three reasons that
  all still hold here: `link_external_ref` is agent-facing free text, the join
  key is a mutable thread key, and `UNIQUE (system, external_key)` allows one
  task per conversation forever. `task_set_source_thread` is the sanctioned
  writer and `tasks.source_thread_id` the sanctioned column.
- **D6 — promoted tasks are `assignee_type='human'`, `priority=0`.** Verified:
  `internal/tools/getnext.go:51` filters `p.client = $1` and the `personal`
  project has `client IS NULL`, so a personal task can never be handed to a
  worker queue no matter what the assignee says. Human is the honest label.

## Acceptance criteria

1. `classify promote` exists as a subcommand of `cmd/classify` and takes
   `--dry-run` and `--limit N`. It takes **no** `--since` and no cutover flag:
   the bound is the stored `projects.classify_promote_after`, so "which
   verdicts were eligible" is answerable from the database after the fact.
2. The promoter's inbox is exactly: `ai_extractions` joined to `ai_runs` with
   `worker_type='classify'` AND `status='ok'`, `fields->>'actionable'='true'`,
   joined through `normalized_messages` to the message's LATEST
   `capture_decisions` row and its project, where that project has
   `ai_classify` AND `classify_promote_after IS NOT NULL` AND the verdict's
   `ai_runs.created_at >= classify_promote_after`, and no
   `classify_promotions` row exists for the message. Oldest verdict first.
3. **The residue lane cannot be promoted, for two independent reasons, and a
   test proves both.** (a) `worker_type='classify'` excludes
   `classify_residue` rows by name. (b) The project join excludes them
   structurally: a residue message's latest decision is `unmatched`, and
   0015's CHECK makes `(action='unmatched') = (project_id IS NULL)` a schema
   fact, so an inner join to `projects` returns zero residue rows without
   erroring (the SWT-23 lesson, restated). The integration fixture must contain
   a `classify_residue` verdict over an unmatched message and assert it
   produces no promotion row and no task.
4. Forward-only: a verdict whose `ai_runs.created_at` precedes the project's
   `classify_promote_after` is never promoted, and no promotion row is written
   for it (absence, not a 'skipped' row). Test: two verdicts, one either side
   of the cutover; exactly one promotes.
5. Promotion is OFF until a human sets the cutover. With
   `classify_promote_after IS NULL` on every project, a full run creates
   nothing, writes no promotion rows and exits 0 with a line saying no project
   has a cutover set. (Fail-closed, 0018's asymmetry: not promoting is a stall,
   promoting by accident fills a board with rows nobody chose.)
6. The decision itself is a **pure function** with no I/O — signature shaped
   like `promote.Decide(v Verdict, existing *ExistingTask) Decision` — and its
   unit test imports no pgx, no net and no provider (the
   `internal/orchestrator/rules_test.go` shape). Order of the rules, pinned by
   test: attach-before-create (to an OPEN task — Q3) wins over the whitelist;
   then whitelist → create;
   then → review.
7. Whitelisted kinds (`payment_due`, `deadline`) create a `tasks` row via the
   executor `create_task` with `status='ready'` — a live task on the personal
   board.
8. Every other flagged kind (`appointment`, `action_required`,
   `informational`, and any kind not in the whitelist) lands in the
   human-review lane and NEVER as a `ready` task. **Assumed answer to Q1:** the
   review lane is a `tasks` row with `status='holding'` in the same project —
   queues are filters (invariant 2), `/tasks?project=personal&status=holding`
   is the surface, and `internal/dashboard/board.go:46` already renders
   `holding` as the first column. This requires `create_task` to accept an
   optional `status` of exactly `ready|holding` (see "API changes").
9. Attach-before-create, OPEN tasks only (Q3 answer): when the message's
   thread already has a task that is NOT `closed`/`delivered`
   (`tasks.source_thread_id = nm.thread_id AND status NOT IN
   ('closed','delivered')`, oldest such task wins on ties), the promoter
   appends ONE `task_append_log` event through the executor and writes a
   promotion row with `action='attached'` and that `task_id` — no second task,
   no status change. When the thread's only tasks are `closed`/`delivered`,
   the verdict falls through to the whitelist rules and creates a NEW task
   (criteria 7-8): a thread yields at most one OPEN task, and a re-raised
   obligation is visible on the board instead of landing as a log line inside
   a closed task. `Decide` therefore takes the existing task's STATUS as an
   input and stays pure; the pinned rule order (criterion 6) is unchanged —
   attach-to-open wins over the whitelist. Test both branches: open task →
   attached, closed task → new task with a promotion row recording the closed
   task's id in `reason`.
10. Re-classification is a no-op: a second `ai_extractions` verdict for a
    message that already has a `classify_promotions` row produces nothing at
    all — no task, no log append, no second promotion row. (Distinct from 9:
    9 is a *different* message on the same thread, 10 is the *same* message.
    `task_append_log` has no dedup of its own — capture's recorded reason for
    refusing `--all` in live mode — so this must be a refusal, not an append.)
11. Idempotency is structural: `classify_promotions` has
    `UNIQUE (normalized_message_id)` and the insert uses `ON CONFLICT
    (normalized_message_id) DO NOTHING RETURNING id`; losing the race means the
    promoter does NOT act on that message. Running the promoter twice over the
    same inbox creates exactly the same rows as running it once — asserted by
    an integration test that runs it twice and compares counts.
12. Claim-before-act ordering, copied from `capture.EvaluateRules`: the
    `classify_promotions` row is inserted BEFORE the executor calls it
    describes, then updated with the resulting `task_id`. A crash between the
    two leaves a promotion row with `task_id IS NULL` (visible, diagnosable) —
    never a task nothing remembers, which a second run would duplicate.
13. Every task and every log append goes through `executor.Execute`
    (invariant 3) with actor `promote:classify` (the `capture:{connector}`
    shape). A structure test scans `internal/promote/*.go` and fails on
    `INSERT INTO tasks`, `INSERT INTO task_events`, `INSERT INTO deliveries`
    or `INSERT INTO external_refs` — the
    `internal/capture/rules_structure_test.go:72` test, retargeted.
14. The promoter never calls a model: `internal/promote` imports neither
    `internal/provider` nor any vendor SDK, asserted by a structure test that
    parses the package's imports (the `internal/classify/structure_test.go`
    `go/parser` shape). `--dry-run` and the real path read the same rows and
    take the same decisions; dry-run performs no writes of any kind (no
    promotion rows either) and prints one line per decision.
15. Task content is copied, never generated: title is the verdict's stored
    `fields->>'title'`, truncated with `textmatch.NormalizedPrefix(.., 120)`
    (the ONE spelling); body is a deterministic block carrying kind, sender,
    subject, sent_at, `normalized_message_id`, `ai_extraction_id`, the verdict
    reason, and `link_url` when the verdict resolved one. No second model call
    exists anywhere in the path.
16. The target project is the message's CURRENT attribution (the latest
    `capture_decisions` project), not the stored `fields->>'project_id'`. When
    the two differ — the rules re-attributed the message after it was
    classified — the promotion row's `reason` records both ids. Test with a
    fixture whose stored project_id no longer matches.
17. Single-instance: the pass takes `pg_try_advisory_lock` on key
    `0x5157_0021` (free — verified against every key in the repo: 0005
    orchestrator, 0006 triage, 0015 capture, 0022 classify, 0028 calendar
    booking) and releases it EXPLICITLY on a dedicated connection before
    returning it to the pool (`internal/classify/store.go` `TryLock`, whose
    comment explains the leak this avoids). Losing the lock is an error and a
    non-zero exit, as `classify run` does — this is a solo CronJob, not a
    hitchhiker on a connector pass.
18. `/funnel`'s classify block gains one promotion line per lane —
    created / attached / review / (dry-run) — read from `classify_promotions`
    over the same `?days=` window, as a fifth independently-degrading section
    (`funnel.go` `runSections`). The page stays a WINDOW, NOT A CONTROL: no
    POST route is added there.
19. Nothing outbound: no `deliveries` row is created or modified anywhere in
    this ticket, and the promote package has no method that could. Asserted by
    the same structure test as criterion 13.
20. `docs/runbooks/local-classifier.md` gains a "Promotion" section stating:
    the exact psql statement that sets the cutover, that it is forward-only and
    that lowering it does NOT backfill (nothing re-classifies an old message —
    the classify inbox's `NOT EXISTS` already excludes it), that the residue
    lane is excluded twice over, and the dry-run-before-live sequence.
21. `.claude/INSTITUTIONAL_KNOWLEDGE.md` gains an entry recording the cutover
    column, the double residue exclusion, the promotion dedup key, and the
    fact — verified in this session — that `classify eval` writes NO `ai_runs`
    or `ai_extractions` rows (it calls `lane.Complete` directly and scores in
    memory), which is the only reason an eval run cannot inject verdicts into
    the promoter's inbox. If `Eval` ever gains a store write, the promoter's
    inbox gains an eval-shaped hole.

## Data model changes

**Migration `0021_classify_promotion.sql`** (0020 is the current highest;
forward-only, numbered, one transaction).

1. `ALTER TABLE projects ADD COLUMN classify_promote_after TIMESTAMPTZ;`
   Nullable with NO default — NULL means "this project's verdicts never
   promote". Deliberately not backfilled for `personal`: the operator sets it
   in the psql statement the runbook prints, which is what makes the cutover a
   decision with a timestamp rather than a deploy side effect.

2. ```
   CREATE TABLE classify_promotions (
     id                    BIGSERIAL PRIMARY KEY,
     normalized_message_id BIGINT NOT NULL REFERENCES normalized_messages(id) ON DELETE CASCADE,
     raw_source_item_id    BIGINT REFERENCES raw_source_items(id),
     ai_extraction_id      BIGINT NOT NULL REFERENCES ai_extractions(id),
     project_id            BIGINT NOT NULL REFERENCES projects(id),
     kind                  TEXT NOT NULL,
     action                TEXT NOT NULL CHECK (action IN ('task','review','attached')),
     task_id               BIGINT REFERENCES tasks(id),
     reason                TEXT,
     created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
   );
   CREATE UNIQUE INDEX classify_promotions_message_uniq ON classify_promotions (normalized_message_id);
   CREATE INDEX classify_promotions_created_idx ON classify_promotions (created_at);
   ```
   `ON DELETE CASCADE` on `normalized_message_id` for capture_decisions'
   recorded reason: a promotion decision without its message means nothing, and
   19 integration suites clear fixtures by deleting `normalized_messages` —
   without the cascade they fail inside cleanup, which reads like the
   cross-pollution pact breaking rather than like a new FK.
   `UNIQUE` and not `UNIQUE ... WHERE`: this is a total unique index, so
   `ON CONFLICT (normalized_message_id) DO NOTHING` needs no predicate restated
   (unlike `capture_decisions_live_uniq` and
   `task_events_outbound_observed_uniq`, both partial).

No other schema change. `tasks`, `ai_runs`, `ai_extractions`,
`capture_decisions`, `external_refs` and `deliveries` are untouched.

## API / MCP tool changes

- **`create_task` gains an optional `status`**, accepting exactly `ready`
  (default, unchanged) or `holding`. This is the parameter
  `docs/tickets/06-gpt-triage_SPEC.md` reserved for the live slice ("create_task
  grows a status/lane parameter (`holding` for auto-lane, plus the human-review
  lane…)"). `validateCreateTask` rejects any other value by name. The MCP
  schema in `internal/mcpserver/schemas.go` is left ALONE: agents keep the
  ready-only description, and `holding` is strictly less privileged than the
  status they can already produce, so no new agent capability appears. Policy is
  unchanged — `create_task` stays a static-fallthrough tool.
  *Needed only under Q1 = holding-tasks.*
- **No new executor tools** otherwise, and no MCP surface change. The promoter
  is a spine service: it writes its own bookkeeping (`classify_promotions`)
  directly over the pool, exactly as `internal/capture` writes
  `capture_decisions` and `internal/classify` writes `ai_runs`, and reaches
  `tasks`/`task_events` only through `create_task`, `task_append_log` and
  `task_set_source_thread`.
- **`task_set_source_thread`** is called on every task the promoter creates,
  with the message's `thread_id` (skipped when the message has none —
  provenance is an observation, never an invention). This is what makes
  criterion 9's attach lookup work on the NEXT message of the thread; without
  it the promoter would create a duplicate task per follow-up and the dedup
  would be inert on production data.

## MQTT topics

None. The promoter publishes and subscribes to nothing.

## Files likely to touch

- `migrations/0021_classify_promotion.sql` (new)
- `internal/promote/promote.go` (new) — `Decide` (pure), `Run` (driver)
- `internal/promote/store.go` (new) — inbox query, promotion-row writes,
  `TryLock` (copy `internal/classify/store.go`'s explicit-unlock shape)
- `internal/promote/promote_test.go`, `store_integration_test.go`,
  `structure_test.go` (new)
- `cmd/classify/main.go` — `promote` subcommand, usage block, doc comment
- `internal/tools/createtask.go` — `status` arg (Q1-dependent)
- `internal/dashboard/funnel.go`, `internal/dashboard/templates/funnel.html`
  — the promotion line
- `docs/runbooks/local-classifier.md` — the Promotion section
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` — the new entry

Read before writing, in this order: `internal/capture/rules_store.go`
(`EvaluateRules` is the driver this one is a smaller copy of),
`internal/classify/store.go` (lock + inbox idiom),
`internal/tools/createtask.go`, `internal/tools/provenance.go`.

## In scope / Out of scope

**In scope:** the promoter package, the CLI subcommand, migration 0021, the
`create_task` status argument (Q1-dependent), the funnel counters, the runbook
and institutional-knowledge updates.

**Out of scope — do not bundle:**
- **Promoting any other lane.** The residue lane stays shadow forever until a
  separate ticket argues for it; `classify_residue` is excluded by name AND by
  the project join.
- **Taking GPT triage live** (build-order step 6's live slice). Triage's
  human-review lane, its confidence threshold and its attach-vs-create are a
  different worker with a different inbox; this ticket must not touch
  `internal/triage`.
- **Taking capture rules live** (`CAPTURE_RULES_MODE=live`). Promotion consumes
  capture's SHADOW attributions exactly as the classify lane already does;
  flipping capture is its own decision with its own diff.
- **Any outbound anything** — deliveries, drafts, calendar bookings, reminder
  emails off a `payment_due` task. Invariants 4 and 5 are untouched because
  nothing outbound exists in this path.
- **Orchestrator rules for promoted tasks** (auto-close, due dates, escalation,
  a `due_at` column). A promoted task is an ordinary `tasks` row and the
  existing lifecycle applies unchanged.
- **Backfilling historical flags.** Explicitly refused by the ticket.
- **Kube manifests / the promote CronJob.** The image build is here; the
  manifest belongs to the kube session (`~/projects/personal/kube/switchboard/`).
  This ticket ships a command that runs by hand.

## Invariants that apply

1. **Raw-first** — untouched, and depended upon: the promoter reads
   `ai_extractions.raw_source_item_id` and copies it onto the promotion row, so
   every promoted task is traceable back to the provider JSON that produced it.
   No new ingestion path exists here.
2. **One funnel** — the whole point of the ticket: flagged verdicts become rows
   in the ONE `tasks` table. `classify_promotions` is an append-only decision
   log with no status, assignee or claim — the `capture_decisions` precedent —
   and the review lane must be a FILTER over `tasks`
   (`?project=personal&status=holding`), never a second table of things to act
   on. If Q1 lands on a review surface with no task, that surface must still be
   a view over `classify_promotions`, not a work queue with its own states.
3. **Everything through the executor** — concretely for this step: the ONLY
   writes `internal/promote` performs directly are `INSERT INTO
   classify_promotions` and its `UPDATE ... SET task_id`. Task creation is
   `create_task`, the attach append is `task_append_log`, provenance is
   `task_set_source_thread`, all via `executor.Execute` with actor
   `promote:classify`. The structure test (criterion 13) is what keeps a "quick
   direct insert" from appearing later.
4. **Nothing external without a delivery row** — nothing external happens; the
   structure test bans `INSERT INTO deliveries` in this package so it stays
   true by construction.
5. **Own-message loop closure** — the promoter's inbox is verdicts over inbound
   messages only (the classify inbox already filters `direction='inbound'`, and
   the capture engine only ever decides inbound messages), so a send of ours
   re-entering through ingestion can never be promoted into a task.
6. **Stealth attribution** — not applicable in the client-visible sense (the
   personal project is `local_only` with `client IS NULL`), and worth stating
   because the promoted title is model-authored text: it is stored in `tasks`
   and rendered on the local dashboard only. Nothing in this path can put it in
   front of a client, because nothing in this path sends.
7. **Purity / every decision writes an audit row** — the promoter is the
   orchestrator's discipline applied to a worker: `Decide` is a pure function of
   (verdict, existing task, whitelist), unit-testable with no db and no model,
   and every decision leaves two records — the `classify_promotions` row and
   the `audit_events` rows the executor writes for `create_task` /
   `task_append_log` / `task_set_source_thread`. Note the one gap Q1 can open:
   a review decision that creates NO task writes a promotion row but no
   `audit_events` row.

Boundary note (SWT-21's ai_locality work, not a new invariant): promoted tasks
carry restricted personal content into `tasks`. That is safe today because
`internal/drafts/store.go` selects `p.ai_locality` and skips `local_only`
projects, and `task_get_next` filters `p.client = $1` while `personal.client IS
NULL`. Any future worker that reads `tasks` without one of those two clauses
inherits a leak — say so in the institutional-knowledge entry.

## Sibling patterns to copy

- **The driver**: `internal/capture/rules_store.go` `EvaluateRules` — decision
  row first, executor calls second, `recordDecisionTask` third; per-message
  loop; advisory lock; stats struct. This promoter is that function with a
  simpler decision.
- **The claim**: `insertDecision`'s `ON CONFLICT ... DO NOTHING RETURNING id` +
  `if !inserted { continue }`, and the comment explaining why the row is
  written before the action.
- **The lock**: `internal/classify/store.go` `TryLock` — dedicated connection,
  explicit `pg_advisory_unlock` on `context.Background()` before release.
- **The pure-rules test**: `internal/orchestrator/rules_test.go` (no pgx, no
  net, no provider import).
- **The structure tests**: `internal/capture/rules_structure_test.go:72`
  (direct-write ban) and `internal/classify/structure_test.go` (`go/parser`
  import scan, migration-count guard — write the 0021 equivalent in
  `internal/promote`, and do NOT delete
  `TestMigration0018_IsTheOnlyOneThisTicketAdds`; it globs `0018_*.sql`
  specifically and stays true).
- **The dashboard section**: `internal/dashboard/funnel.go` `runSections` +
  `funnelSection`; reads only, error line names its section.
- **Queue claims**: not needed here (single-instance advisory lock, no
  competing consumers), so `FOR UPDATE SKIP LOCKED` does NOT appear — the
  jobagent pattern is for worker claims on `tasks`.

## Verification protocol

1. `go test ./...` — unit suites including the new pure `Decide` tests and the
   structure tests.
2. `make integration` (db-up + migrate + `go test -tags integration ./...`,
   serialized `-p 1`). New integration suite must clean up its own fixtures in
   FK order (`classify_promotions` → `task_events` → `tasks` →
   `capture_decisions` → `normalized_messages` → `normalized_threads` →
   `ai_extractions` → `ai_runs` → `raw_source_items` → `source_accounts` →
   `projects`), scoped by a test-owned slug, and join the mutual-cleanup pact if
   it asserts any global count.
3. Migration check before anything against production:
   `psql -h 192.168.50.49 -U ops -d ops -tAc "SELECT max(version) FROM
   schema_migrations"` vs `ls migrations/` — merging a migration is not applying
   it.
4. **Smoke ("usable alone"), against the real ops db:**
   a. `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/classify run --lane
      personal --limit 5` — fresh verdicts, recorded now.
      (`cmd/classify` reads `DATABASE_URL`, not `OPS_DATABASE_URL`.)
   b. Set the cutover deliberately behind those verdicts:
      `psql … -c "UPDATE projects SET classify_promote_after = now() -
      interval '1 hour' WHERE slug='personal'"`.
   c. `go run ./cmd/classify promote --dry-run` — prints the plan, writes
      nothing. Confirm with
      `SELECT count(*) FROM classify_promotions` = 0.
   d. `go run ./cmd/classify promote` — whitelisted kinds appear as `ready`
      tasks under project `personal`, non-whitelisted flagged kinds in the
      review lane, `classify_promotions` carries one row per message.
   e. `go run ./cmd/classify promote` a SECOND time — output shows zero
      decisions; `SELECT count(*) FROM tasks WHERE project_id=6` and
      `count(*) FROM classify_promotions` are unchanged.
   f. Residue control:
      `SELECT count(*) FROM classify_promotions p JOIN ai_extractions e ON
      e.id=p.ai_extraction_id JOIN ai_runs r ON r.id=e.ai_run_id WHERE
      r.worker_type='classify_residue'` = 0.
   g. Board: `kubectl -n ops port-forward svc/dashboard 8085:80`, then
      `/tasks?project=personal` and `/tasks?project=personal&status=holding`,
      and `/funnel` for the promotion counters.
5. `/ticket-review classify-promotion` (go-reviewer against the seven
   invariants) before commit — the executor/task-creation surface makes the
   optional adversarial pass worth running too.

## Future work (not this SPEC)

- A second-opinion pass with a different model over flagged verdicts to trade
  precision back (measured precision at the cutover is ~0.50 on the personal
  labels) — `internal/classify/prompt.go` already records why a confidence
  threshold is not the answer.
- Promoting the residue lane, and whatever review surface that would need.
- Dashboard corrections as labelled data ("log every dashboard correction as
  labeled data") — dismissing a review-lane task is exactly a label, and
  nothing captures it today.
- Due dates / escalation for `payment_due` and `deadline` tasks (a `due_at`
  column plus an orchestrator rule).
- Attaching a follow-up that arrives on a NEW thread (a second dunning notice
  with no `References` header is its own thread and therefore its own task).
  Honest limit of D5, not a defect: thread-based dedup catches replies and
  re-classification, not a re-sent notice.
