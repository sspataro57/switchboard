> Jira: SWT-16

# outbound-capture — task-log linkage for messages sent outside switchboard

No open questions arose; judgment calls are recorded under "Decisions made
unilaterally". Ready for `test-author`.

## Source

User request (Salvador, 2026-07-29):

> emails may still go out by gmail directly or another client. Switchboard is
> supposed to capture those too or we will lack context.

## Goal

When an outbound message that switchboard did not send is ingested and
normalized, append an informational `outbound_observed` event to the log of
every task that already corresponds on that thread (via its delivery rows), so
neither a worker nor the dashboard treats the task as untouched. Usable alone
means: Salvador replies to a client directly (Gmail app, Jira web UI, Slack on
a phone); the next connector pass logs the observation on the linked task's
event feed, visible on the existing `/tasks/{id}` dashboard page with no other
deployment or schema change — and re-running normalization (including `--all`)
never logs it twice.

## What already works (verified on main — this ticket does NOT re-specify it)

A message sent entirely outside switchboard is already captured for any
ingested mailbox/workspace/site:

1. **It lands raw and normalizes as outbound.** Gmail: direction is decided by
   the From address against ALL `provider='google'` account emails
   (`internal/connector/google/normalize.go:65-94`) — so "another mail client"
   sending from one of the five accounts is outbound too; the rule is
   address-based, not client-based. Slack: `author_id == own_user_id` from
   `OWN_USER_IDS`, failing closed when either is missing
   (`internal/connector/slackweb/normalize.go:63-80`). Jira: author accountId
   == the polling account (`internal/connector/jira/normalize.go`).
2. **It creates no spurious task.** Triage's pending filter is
   `WHERE m.direction = 'inbound'` (`internal/triage/store.go:34`); invariant 5
   holds for these without any delivery match.
3. **It reaches the model as context.** Triage thread context pulls both
   directions (`internal/triage/store.go:72-79`; `internal/triage/prompt.go:98`
   — "outbound is legitimate history") and the draft worker's thread window
   ignores direction (`internal/drafts/store.go:156-158`).

## The gap

Invariant 5's "attaches as task log" fires only through a delivery match:
gmail confirms by the pre-reserved Message-ID
(`internal/connector/google/sink.go` `confirmDelivery`, ~line 294), Slack by
exact `target_ref` + 120-char whitespace-normalized body prefix
(`internal/connector/slackweb/sink.go` `confirmDelivery`), jira and upwork by
their own post-hoc matchers. A message switchboard never sent has no
`deliveries` row, so nothing appends to any task's log. The task looks
untouched; a worker can redo work already done.

## The thread→task join (the crux, investigated)

There is exactly ONE stored, deterministic thread→task association in the
schema today, and it is `deliveries`:

- `tasks` has no thread/message column (`migrations/0001_initial.sql:114-133`).
- Triage is shadow mode — `triage.Store` has no task-write surface (enforced by
  reflection test), so no task was ever created FROM a message; attach-vs-create
  exists only inside `ai_extractions`.
- `external_refs.external_key` is agent-chosen free text via
  `link_external_ref` (`internal/tools/prci.go:45-59` — no format validation),
  or a PR number from the github path. It is not canonical against
  `thread_key` and cannot be joined reliably.
- The draft worker resolves a thread heuristically at draft time
  (`internal/drafts/store.go` `resolve`) and immediately persists the result
  INTO the delivery row (`thread_id` for gmail, `target_ref` otherwise) — so
  every heuristic association that exists is already materialized in
  `deliveries`.

Therefore: **a task is linked to a thread iff it has a `deliveries` row (any
status, including `drafted`) referencing that thread.** Per channel the key is:

| message channel | delivery channel | join |
|---|---|---|
| `gmail` | `gmail` | `d.thread_id = m.thread_id` (0006 column; `draft_delivery` requires it for gmail) |
| `slack` | `slack_reply` | `d.target_ref` = canonical URL derived from `thread_key` `slack:{ws}:{conv}[:{root}]` → `https://app.slack.com/client/{ws}/{conv}[/{root}]` (`slackweb.Target.CanonicalURL`, `internal/connector/slackweb/url.go:60`); match BOTH the thread-level and the conversation-level form (a delivery targeting the channel corresponds on the whole conversation) |
| `jira` | `jira_comment` | `d.target_ref = t.thread_key` (both are `jira:{site_host}:{KEY}` — `internal/tools/delivery.go:758`, `internal/connector/jira/normalize.go:64`) |

Tasks with no delivery on the thread — manually created tasks, plan-import
tasks — have no machine-readable thread association at all, so no linkage is
possible without guessing, which this ticket refuses to do. That is the honest
coverage limit: capture completes the loop for every stored link that exists;
widening it is triage-live's job (attach), not a heuristic here.

## Mechanism

A shared post-normalize pass, `internal/capture` (new package), in the exact
mold of `slackweb.ReconcileUnconfirmed` (`internal/connector/slackweb/
reconcile.go`): deterministic SQL + Go, no LLM, no provider imports, invoked by
each connector's main after `Normalize` so the pass's own confirmations are
already stamped. It:

1. selects outbound normalized messages for the connector's message channel,
   `sent_at` within a horizon, whose `external_message_id` is claimed by no
   delivery (`NOT EXISTS (SELECT 1 FROM deliveries d WHERE
   d.channel=$deliveryChannel AND d.sent_external_id = m.external_message_id)`
   — switchboard's own sends always end up matched: gmail reserves
   `<sb-{id}-...>` pre-send, slack/jira/upwork get stamped by their matchers);
2. defers (skips this pass, retries next) any message whose join key still has
   a potential unconfirmed claimant: a delivery with `status IN
   ('sending','sent') AND sent_external_id IS NULL AND confirmed_at IS NULL`
   within the same horizon — that message may be switchboard's own send that
   the matcher has not stamped or has refused to guess about (multi-match,
   skew floor), and claiming "sent outside switchboard" while an unresolved
   switchboard send to the same destination exists would be a lie;
3. resolves DISTINCT `task_id`s via the per-channel join above;
4. appends one `outbound_observed` task_event per (task, message), guarded by
   `INSERT ... SELECT ... WHERE NOT EXISTS` on
   `(task_id, event_type='outbound_observed', payload->>'message_id')` — the
   dedup key is `normalized_messages.id` (stable across re-normalization;
   `external_message_id` can be absent for some providers).

Purely informational: no task status change, no delivery mutation, no
`sent_external_id` invention, no orchestrator rule (the drain ignores unknown
event types — SWT-12 precedent). If a status effect is ever wanted, it belongs
in the orchestrator as a function of (event, task, policy); not this ticket.

## Acceptance criteria

1. New package `internal/capture` exposing
   `ObserveOutbound(ctx, pool, Channel) (int, error)` where `Channel` binds
   {message channel, delivery channel, join strategy} for `gmail`, `slack`,
   `jira`. Deterministic SQL + Go only; no LLM calls, no provider adapter
   imports (unit-testable with a db and nothing else). Exception: the slack
   join derives canonical URLs from `thread_key` — reuse
   `slackweb.Target.CanonicalURL` or an equivalent string builder covered by a
   test pinning equality with `CanonicalURL` output, so the SWT-13
   canonicalization landmine (exact-string match, silent forever-miss) cannot
   recur in a second spelling.
2. Detection matches the mechanism above: outbound direction, channel-scoped,
   horizon-bounded, unclaimed external id. A message whose
   `external_message_id` is NULL/empty is treated as unclaimed (switchboard
   sends always carry ids).
3. Deferral: a pending unconfirmed claimant on the join key (status IN
   `('sending','sent')`, `sent_external_id IS NULL`, `confirmed_at IS NULL`,
   `COALESCE(sent_at, send_attempted_at, updated_at)` within the horizon)
   suppresses capture for messages on that key this pass. Integration test:
   a slack outbound message co-existing with an in-flight `sending` row to the
   same target is not captured; after `mark_delivery_failed` settles the row,
   the next `ObserveOutbound` run captures it.
4. Task resolution per the join table above; ALL distinct linked tasks receive
   the event (no status filter, no "most recent delivery wins" guess).
5. Event shape: `event_type='outbound_observed'`, payload
   `{message_id, external_message_id, channel, thread_key, sent_at, sender,
   body_preview}` where `body_preview` is the 120-char whitespace-normalized
   prefix (reuse the `normalizedPrefix` idea from
   `internal/connector/slackweb/sink.go:295`).
6. Idempotence: re-running the connector, `--normalize-only`, and `--all`
   replays append no duplicate event for the same (task, message). Integration
   test runs `ObserveOutbound` twice and asserts event count unchanged —
   same class of guard as `confirmDelivery`'s `sent_external_id IS NULL` +
   RowsAffected check and `ReconcileUnconfirmed`'s fire-once marker.
7. No writes outside `task_events`: tasks, deliveries, normalized tables are
   untouched by the pass. Orchestrator code untouched; no rule added for
   `outbound_observed`.
8. An unmatched outbound message on a thread with NO delivery-linked task
   produces no event and no error — it is already context in
   `normalized_messages` (triage/drafts read it; verified above) and nothing
   more is knowable. Explicit test.
9. Wiring: `cmd/connectors/google/main.go`, `cmd/connectors/slackweb/main.go`,
   `cmd/connectors/jira/main.go` call `ObserveOutbound` after `Normalize`
   (for slackweb: alongside the existing `ReconcileUnconfirmed` call,
   `cmd/connectors/slackweb/main.go:63-71`) and print a stats line in the
   existing `printStats` style. Errors from the pass fail the run loudly (same
   treatment as reconcile), never silently.
10. Horizon: env `OUTBOUND_OBSERVE_HORIZON` (Go duration, default `720h`);
    unparseable or non-positive values fall back to the default — same
    defensive shape and rationale as `slackweb.UnconfirmedFlagPasses`
    (`internal/connector/slackweb/reconcile.go:27-37`). The horizon is what
    prevents a first-deploy flood of stale observations onto old tasks and
    bounds the per-pass scan.
11. Late linkage works: a delivery drafted AFTER the direct send, on the same
    thread, gets the observation on the next pass (this is why the scan is
    horizon-bounded rather than "newly normalized only"). Integration test:
    outbound message normalized first, `draft_delivery` on the thread second,
    `ObserveOutbound` third → event present.
12. `go vet ./...`, `go test ./...`, and `make integration` pass; new
    integration tests join the serialized cleanup pact (clean own fixtures in
    FK order, test-owned slugs).

## Data model changes

None. No migration (next free number would be 0013; it stays free).
`task_events.event_type` is unconstrained TEXT; new vocabulary entry:
`outbound_observed` (payload above). Record it in
INSTITUTIONAL_KNOWLEDGE.md's vocabulary notes at deliver time.

## API / MCP tool changes

None. This is not a tool: it is a connector-side deterministic pass with the
same standing as `confirmDelivery` and `ReconcileUnconfirmed` (trusted spine
code, attributable to a `sync_runs`-audited connector run), not an
agent-reachable surface — so the executor path is not in play (see invariant 3
below). No dashboard changes: `/tasks/{id}` already renders the last 50
task_events generically (`internal/dashboard/board.go:243-245`).

## MQTT topics

None.

## Files likely to touch

- `internal/capture/observe.go` (new), `observe_test.go` (new),
  `observe_integration_test.go` (new, build-tagged `integration`)
- `cmd/connectors/google/main.go`
- `cmd/connectors/slackweb/main.go`
- `cmd/connectors/jira/main.go`
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` (event vocabulary + any landmines found)

## In scope

- The `internal/capture` pass, its per-channel joins (gmail, slack, jira), the
  deferral guard, the dedup guard, the horizon env.
- Wiring into the three connector mains.
- Tests per the criteria.

## Out of scope

- **`deliveries` rows for externally-sent messages** — decided before this
  SPEC and carried: they were never switchboard deliveries; inventing rows
  corrupts the table's meaning. SWT-12's `approval_source` reasoning applies:
  never record a gate that never happened. Invariant 4 governs what
  switchboard sends; it does not oblige a row for what a human sent elsewhere.
- **The SWT-12 manual (leaf_token) path** — distinct on purpose: there Claude
  DID send through the leaf at Salvador's instruction, so a row IS written
  (`approval_source='leaf_token'`) and the slack matcher stamps it on the next
  export — such a message is claimed, so this ticket's pass never touches it.
- `upwork_chat` capture: the CRM's `external_id` is nullable and channel
  values are source-defined (`internal/connector/upworkcrm/normalize.go:99-107`),
  and the assisted tier makes every send a human send pending a prefix match —
  the matched/unmatched distinction is structurally blurry there. Revisit if
  wanted; not now.
- GitHub (no `normalized_messages` ingestion exists for it).
- Any task-status nudge or orchestrator rule (smaller, safer first cut —
  adopted as decided in the request).
- A stored task↔thread column (`tasks.thread_id` or similar) to widen
  coverage beyond delivery-linked tasks — that is triage-live attach
  territory (build-order step 6 going live), an adjacent step not to bundle.
- Fixing the operational preconditions below (config/ops, not code).
- The in-flight SWT-14/15 gmail/IMAP bridge work: this pass reads normalized
  tables only, so it inherits whatever those tickets ingest with no coupling.

## Operational preconditions (stated, not specced around)

Capture only happens where ingestion runs. As of 2026-07-29:

- `connector-slackweb` is SUSPENDED, and workspace `T0HPR78RX`
  (Collaboratory/LlamaSite) has no `OWN_USER_IDS` entry — slack export fails
  closed there, so slack capture is effectively Avviato-only and inert until
  the CronJob resumes.
- `connector-google` is SUSPENDED pending the SWT-7 OAuth runbook; gmail
  capture (the complaint that motivated this ticket) delivers nothing
  observable until Google ingestion runs.
- `connector-jira` and `connector-upworkcrm` run */15 in-cluster; jira is the
  one channel where this ticket is observable immediately — and only for
  issues that already carry a `jira_comment` delivery.

Neither suspension is code work in this ticket. The ticket is still "usable
alone" via the jira channel and the local-db smoke below.

## Named consequences

1. **Coverage equals correspondence.** Only tasks that already have a delivery
   row on the thread get the observation. A manually created task about an
   email thread has no stored link and gets nothing — the message remains
   thread context only. Honest limit, stated above; the fix is triage-live
   attach, not heuristics here.
2. **Deferral can outlast patience on wedged rows.** A slack delivery a human
   resolved with `mark_delivery_sent` keeps `sent_external_id` NULL until the
   matcher stamps it; while inside the horizon it suppresses capture on its
   conversation. The horizon bounds this: an old unresolved claimant stops
   deferring, and if the matcher later binds a captured message to it (a
   `--all` replay can), the task carries both `outbound_observed` and
   `delivery_confirmed` for one message — redundant, visible, and harmless,
   preferred over silently claiming a switchboard send was external.
3. **Drafted-delivery tasks get the signal too, deliberately.** A pending
   draft whose thread just received a direct reply is exactly the "may be
   moot" case; its task's log now says so. No automation acts on it.
4. **First deploy logs nothing older than the horizon.** Historical direct
   sends stay context-only; backfilling months of observations onto closed
   tasks would be noise, not context.

## Invariants that apply

- **1 — raw-first:** untouched; the pass reads normalized tables and writes
  task_events only. No ingestion changes.
- **2 — one funnel:** linkage, not a parallel store — no new tables, no
  task-like rows; the observation is an event on the existing `tasks` row.
- **3 — everything through the executor:** no new tool; the pass is
  connector-side spine code with the same standing as `confirmDelivery` /
  `ReconcileUnconfirmed` (both established precedents that write task_events
  from connector runs). Nothing here is reachable by an agent.
- **4 — nothing external without a delivery row:** nothing is sent; no
  delivery row is created or mutated, and no `sent_external_id` is ever
  invented (criterion 7).
- **5 — own-message loop closure:** strengthened, not weakened: the existing
  external-id/prefix matchers run first and are untouched; capture handles
  only what they provably did not claim, and the deferral guard keeps an
  ambiguous switchboard send from being mislabeled as external.
- **7 — orchestrator purity:** orchestrator untouched; `outbound_observed`
  has no rule; the drain ignores unknown event types. The pass itself is
  deterministic SQL — no model anywhere near it.
- 6 (stealth attribution) does not apply: nothing client-visible is produced.

## Sibling patterns to copy

- `internal/connector/slackweb/reconcile.go` `ReconcileUnconfirmed` — the
  whole shape: post-normalize deterministic pass, env override with defensive
  fallback, fire-once guard, task_events insert, connector-main wiring
  (`cmd/connectors/slackweb/main.go:63-71`).
- `internal/connector/slackweb/sink.go` `confirmDelivery` — exact-key match
  discipline, `normalizedPrefix`, RowsAffected/`IS NULL` idempotence guards,
  refusing to guess on multi-match.
- `internal/connector/google/sink.go` `confirmDelivery` (~line 294) — the
  Message-ID claim this pass's "unclaimed" test complements.
- `internal/connector/upworkcrm/sink.go` `confirmUpworkDelivery` — thread_key
  prefix scoping (and the reason upwork is out of scope).
- Orchestrator `orchestrated`-event dedup / R7's brief title predicate — the
  dedup-by-query idiom criterion 6 follows.
- Integration cleanup pact: `internal/orchestrator/integration_test.go:115`
  FK-ordered, slug-scoped cleanup.

## Verification protocol

1. `go vet ./... && go test ./...`.
2. `make db-up && make migrate && make integration` — new tests serialized in
   the pact.
3. Local-db smoke (jira channel, no cluster dependency):
   a. Against `postgres://ops:ops@localhost:5433/ops`: seed a project + task,
      a `drafted` `jira_comment` delivery with
      `target_ref='jira:smoke.local:SMK-1'`, a jira `source_accounts` row, and
      a raw `comment:SMK-1:{id}` item authored by the polling account (so it
      normalizes outbound) with `thread_key='jira:smoke.local:SMK-1'`.
   b. `DATABASE_URL=... go run ./cmd/connectors/jira --normalize-only` — verify
      exactly one `outbound_observed` event on the task
      (`psql: SELECT event_type, payload FROM task_events WHERE task_id=...`).
   c. Re-run with `--normalize-only --all` — verify the count is unchanged.
   d. Verify on the dashboard task detail page that the event renders.
   e. Delete smoke rows in FK order.
4. Real observation (when convenient, not gating): comment directly in the
   Jira web UI on a CRM issue that has a `jira_comment` delivery; after the
   next `connector-jira` CronJob pass, the linked task shows the event.

## Decisions made unilaterally

- **Shared package over per-connector duplication.** The detection and dedup
  SQL is channel-parameterized but identical in shape; three copies of it is
  how the canonicalization class of bug happens. `internal/capture` mirrors
  the reconciler's standing.
- **Event name `outbound_observed`.** Not `delivery_*` — there is no delivery,
  and reusing that prefix would imply one. "Observed" matches the reconciler's
  own language ("runs that could have OBSERVED a message").
- **All distinct linked tasks get the event, no status filter.** The event is
  informational history; picking "the most recent delivery's task" would be a
  guess, and filtering by status would hide the observation from exactly the
  task a human inspects. Multi-task threads are rare; mild noise beats a
  wrong single attribution.
- **Horizon default 720h.** Long enough that a suspended connector resuming
  after weeks still captures the backlog that matters; short enough to stop a
  first-deploy flood. Env-tunable; the value is an operational knob, not a
  contract.
- **Deferral is horizon-bounded** (consequence 2) rather than eternal: an
  unresolvable claimant must not permanently blind capture on a conversation.
- **upwork_chat excluded** (see Out of scope) — structural ambiguity, not
  laziness; recorded so it isn't re-litigated from scratch later.

## Future work (not this ticket)

- Triage-live attach (build-order 6 go-live) widening thread→task coverage
  beyond delivery-linked tasks — the real fix for consequence 1.
- A dashboard affordance on pending drafts whose thread has a newer
  `outbound_observed` ("possibly moot — review before approving").
- An orchestrator rule, if experience shows one is wanted (e.g., surface
  `outbound_observed` on `ready` tasks in the morning brief) — pure function
  of (event, task, policy), per invariant 7.
- upwork_chat capture once the CRM guarantees non-null external ids.
