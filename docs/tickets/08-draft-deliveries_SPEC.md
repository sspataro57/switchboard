> Jira: SWT-8

# 08-draft-deliveries — Draft worker, deliveries lifecycle, dashboard slice, Gmail send, Upwork assisted tier

## Source

Build-order step 8, CLAUDE.md:

> 8. Draft worker + deliveries + dashboard approve/edit/send + Gmail send
>    adapter (threading headers, From inherited from thread — never
>    model-chosen) + Upwork assisted tier.

## Goal

Ship the full outbound path — GPT draft worker fills `deliveries` rows for Deliver
tasks, a minimal dashboard approves/edits/sends them through the executor, a Gmail
adapter sends with correct threading and a self-chosen Message-ID that the step-7
connector matches back for loop closure, and the Upwork assisted tier
(draft → copy → mark sent → scraper confirms) works today.

**Usable alone means:** task finishes locally → orchestrator's Deliver task exists →
draft worker produces a delivery draft → Salvador opens the dashboard, edits,
approves, sends (gmail) or copies + marks sent (upwork_chat) → the send is idempotent,
policy-gated, audited, and confirmed when it re-enters via ingestion. The Upwork
path is exercisable end-to-end immediately; the Gmail path is fully testable against
the fake and goes live the moment the OAuth runbook + send-scope re-consent is done —
no code change.

## Acceptance criteria

1. Migration `0006_deliveries.sql` applies on a fresh db and on the current schema:
   `deliveries` gains `subject`, `from_account_id`, `thread_id`, `created_by`,
   `error`, `sent_at`, `confirmed_at`; new table `ops_flags` exists.
2. `draft_delivery` (agent-facing, MCP-listed) creates a `drafted` deliveries row
   attached to a task, with `from_account_id` resolved from the thread's mailbox
   account (never caller-chosen for gmail), through the executor pipeline
   (validate → policy → audit → handler).
3. `update_delivery` edits subject/body only while `status='drafted'`;
   `approve_delivery` transitions drafted→approved and writes an `approvals` row
   (`subject_type='delivery'`, decided_by, decided_at). Approving `failed` rows is
   allowed only when `sent_external_id IS NULL` (retry path). Both are spine-facing
   (registered, NOT MCP-listed).
4. Policy matrix (`internal/policy/matrix.go`, pure core + pg snapshot loader):
   - kill switch on (`ops_flags` row `sending_frozen` = true) ⇒ `send_delivery` and
     `mark_delivery_sent` denied, rule `kill_switch`;
   - per-channel hourly rate limit (default 10, `OPS_SEND_HOURLY_LIMIT` override)
     ⇒ deny with rule `rate_limit` when reached;
   - `send_delivery` on channel `upwork_chat` denied (assisted tier — rule
     `channel_assisted`); channels `jira_comment`, `calendar`, `github_review`
     denied entirely (rule `channel_not_live`, steps 9+);
   - `approve_delivery`, `update_delivery`, `send_delivery`, `mark_delivery_sent`,
     `set_sending_frozen` denied to non-human actors (actor prefix not in
     `dashboard:` / `opsctl:` / `manual:`), rule `human_only`;
   - every verdict lands in `policy_decisions` with channel populated;
   - the matrix core is unit-testable with zero I/O (Decide(req, snapshot)).
5. `send_delivery` (gmail): requires `status='approved'`; sets `status='sending'`
   and generates + persists `sent_external_id`
   (`<sb-{delivery_id}-{rand}@{from-domain}>`) BEFORE the HTTP call; a fake Gmail
   endpoint (httptest, same idiom as `fake_google_test.go`) receives
   `POST /gmail/v1/users/me/messages/send` with `threadId` and a base64url raw
   RFC 2822 message asserting: `From` = the thread's account email, `To` = last
   inbound sender in the thread, `Subject`, `In-Reply-To` = last message's
   Message-ID, `References` = the thread's Message-ID chain in sent order,
   `Message-ID` = the persisted sent_external_id, and NO AI-attribution content
   (no `Co-Authored-By`, no "Generated with" lines — scrubbed). Success ⇒
   `status='sent'`, `sent_at` set, task_event `delivery_sent` on the delivery's task.
6. Idempotency (invariant 4): a delivery whose `sent_external_id` is set and status
   is `sending`/`sent` can never be sent again — `send_delivery` refuses. On a
   definite API rejection (HTTP response received, non-2xx) ⇒ `status='failed'`,
   `error` set, `sent_external_id` cleared (nothing exists externally). On an
   ambiguous transport failure (timeout/conn error after the request may have been
   received) ⇒ `failed` with `sent_external_id` KEPT, blocking retry until operator
   inspection.
7. Loop closure gmail (invariant 5): normalizing an outbound raw gmail message whose
   Message-ID equals a delivery's `sent_external_id` sets `confirmed_at` and inserts
   task_event `delivery_confirmed` on the delivery's task; no new task is ever
   created from it (belt: triage's pending filter is inbound-only — an integration
   test pins both).
8. Upwork assisted tier: `mark_delivery_sent` transitions approved→sent with
   `sent_external_id` NULL + task_event `delivery_sent`; when the upworkcrm
   connector later normalizes an outbound communication for the same client whose
   whitespace-normalized body prefix (first 120 chars) matches the delivery body,
   it sets `sent_external_id` = the communication's external_id (post-hoc) +
   `confirmed_at` + task_event `delivery_confirmed`.
9. Draft worker (`internal/drafts` + `cmd/drafts`): finds Deliver tasks (created by
   orchestrator rule `delivery_task`) whose parent has no delivery row yet,
   assembles deterministic context (parent task + done_local summary + resolved
   channel + resolved thread's recent messages), calls the provider adapter
   (fake in tests) with a strict `{subject, body}` json_schema and a terse-register
   system prompt, records `ai_runs`, and creates the draft via the executor
   `draft_delivery` (actor `drafts:gpt`). From/To/recipient appear NOWHERE in the
   model schema. Unresolvable channel/thread ⇒ one `task_append_log` on the Deliver
   task ("draft manually") and skip thereafter. Single instance via advisory lock
   `0x51570007`.
10. Orchestrator R8: on task_event `delivery_sent` (task = the delivered work task),
    emit `task_mark_delivered(task)` + `task_close` of the R3 Deliver task (found
    via the `delivery_task` orchestration record) + `record_orchestration`
    (rule `delivery_lifecycle`, dedup on task). Pure rule test, no network.
    `delivery_confirmed` events fire no rule.
11. Dashboard (`cmd/dashboard`, Go + HTMX, rag-svc pattern): `/deliveries` lists by
    status; per-delivery view supports inline edit (drafted only), Approve, Send
    (gmail), Copy-to-clipboard + Mark sent (upwork_chat), and a global kill-switch
    toggle. All ACTIONS go through the executor tools; list/detail reads are direct
    SQL. With OIDC env unset it mounts `GET /dev/login` (rag-scv `devlogin.go`
    idiom) and runs locally without Keycloak; with OIDC env set it uses the
    Keycloak OIDC login (rag-scv `auth/oidc.go` idiom). Actor =
    `dashboard:{email}`.
12. `go test ./...` green; `make integration` green including a full lifecycle walk
    (draft → edit → approve → send(fake) → confirm) that joins the mutual-cleanup
    pact (see INSTITUTIONAL_KNOWLEDGE.md, integration cross-pollution landmine).

## Data model changes

Migration `migrations/0006_deliveries.sql` (forward-only):

```sql
ALTER TABLE deliveries
  ADD COLUMN subject         TEXT,
  ADD COLUMN from_account_id BIGINT REFERENCES source_accounts(id),
  ADD COLUMN thread_id       BIGINT REFERENCES normalized_threads(id),
  ADD COLUMN created_by      TEXT,
  ADD COLUMN error           TEXT,
  ADD COLUMN sent_at         TIMESTAMPTZ,
  ADD COLUMN confirmed_at    TIMESTAMPTZ;
CREATE INDEX deliveries_status_idx ON deliveries (status);

CREATE TABLE ops_flags (
  name       TEXT PRIMARY KEY,
  value      JSONB NOT NULL DEFAULT '{}',
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

Notes:
- No `approved_by/approved_at` columns — the existing `approvals` table is the
  vocabulary for that (`subject_type='delivery'`, `subject_id=delivery.id`);
  dashboard joins it.
- Kill switch = `ops_flags` row `('sending_frozen', '{"frozen": true}')`; absent row
  means not frozen. A new tiny table because no existing table holds global runtime
  flags; `decisions` is project-scoped and semantic, `projects.policies` is
  per-project.
- `target_ref` usage pinned per channel: gmail ⇒ redundant with `thread_id`
  (left NULL); upwork_chat ⇒ the `thread_key`
  (`upwork_crm:{client uuid}:{channel}`) so the confirmation matcher can derive the
  client without a join through the draft worker's context.
- Status CHECK unchanged (`drafted→approved→sending→sent|failed` values already in
  0001); transition legality is enforced in the tool handlers.
- New task_events vocabulary: `delivery_sent`, `delivery_confirmed` (record in
  INSTITUTIONAL_KNOWLEDGE.md's task-lifecycle section at delivery).

## API / MCP tool changes

All tools register in `internal/tools.Register` (invariant 3 — registry is the only
route to handlers). Executor path unchanged; the policy stage gets the matrix
checker (below).

**Agent-facing (added to `internal/mcpserver/schemas.go` agentTools):**

- `draft_delivery` — `{task_id, channel: gmail|upwork_chat, body, subject?,
  thread_id?, target_ref?}` → `{delivery_id}`. Creates a `drafted` row. For gmail,
  `from_account_id` is resolved server-side from the thread's `thread_key` mailbox
  segment (`gmail:{account_email}:{gmail_thread_id}`) — the caller cannot choose
  From. Body/subject are scrubbed of AI attribution at write time (shared scrub
  func, belt in the adapter). Records `created_by` = actor. This is THE route for
  client-visible words from workers (worker prompt rule already says so).

**Spine-facing (registered, NOT MCP-listed; reachable via dashboard + `opsctl call`):**

- `update_delivery` — `{delivery_id, subject?, body?}`; only while `drafted`
  (editing an approved draft would bypass the approval).
- `approve_delivery` — `{delivery_id}`; drafted→approved, plus failed→approved iff
  `sent_external_id IS NULL`; inserts `approvals` row with decided_by = actor.
  This call IS the policy-matrix "approve" for email client-facing.
- `send_delivery` — `{delivery_id}`; gmail only (matrix denies other channels).
  In-tx: lock row, require approved, refuse if `sent_external_id` present; compute
  headers from the thread; write `sending` + `sent_external_id`; commit; call the
  Gmail adapter; finalize sent|failed per criterion 6; emit `delivery_sent`
  task_event on success.
- `mark_delivery_sent` — `{delivery_id}`; upwork_chat only; approved→sent,
  `sent_at=now()`, `sent_external_id` NULL; emits `delivery_sent`.
- `task_mark_delivered` — `{task_id, reason?}`; done_locally→delivered; idempotent
  no-op success when already delivered/closed (orchestrator replay discipline, same
  as task_block/task_unblock).
- `set_sending_frozen` — `{frozen: bool}`; upserts the `ops_flags` row. A tool (not
  raw SQL) so freezing/unfreezing is audited.

**Policy upgrade** (`internal/policy`): `policy.Request` gains `Args
json.RawMessage` (executor passes `call.Args` through — one-line change in
`executor.Execute`). New `Matrix` checker wrapping the existing static allow-list:
non-delivery tools fall through to static; delivery tools are decided by a pure
`Decide(req, Snapshot)` core with a pg loader gathering
`Snapshot{SendingFrozen, SentLastHour map[channel]int, Channel, HourlyLimit}`
(orchestrator Facts pattern — purity lives in the core, I/O in the loader).
`cmd/opsctl`, `cmd/ops-mcp`, `cmd/orchestratord`, `cmd/dashboard`, `cmd/drafts` all
construct the Matrix checker.

**Draft worker** (`internal/drafts`, `cmd/drafts`): triage-shaped queue consumer
(reads direct, writes via executor). Channel resolution is deterministic config,
never model output: `projects.policies->>'delivery_channel'` wins; else `gmail` if
`send_from_account` set; else `upwork_chat` if `client_person_id` has an
`upwork_crm` identity; else unresolvable. Thread resolution: gmail ⇒ latest
`normalized_threads` in the send account's mailbox containing a message from any
email identity of the project's client person; upwork ⇒ latest
`upwork_crm:{client uuid}:%` thread. Model contract: prompt + strict
`{subject, body}` schema via `internal/provider` (OpenAI adapter reused, model
`DRAFTS_MODEL`, default `gpt-5-mini`); ai_runs recorded by the worker.

**Orchestrator**: R8 as in criterion 10 (`rules.go` + Facts additions in `facts.go`
to surface the `delivery_task` orchestration's created_task_id — the Orchestration
struct already carries CreatedTaskID). R3 unchanged.

**Gmail send adapter** (`internal/connector/google/send.go`, beside the existing
client plumbing): pure `BuildOutboundMIME(...)` (headers From/To/Subject/Date/
Message-ID/In-Reply-To/References, text/plain UTF-8, attribution scrub) +
`GmailSender{hc, baseURL}` posting `{raw, threadId}` to
`/gmail/v1/users/me/messages/send` (baseURL injectable, same as `NewGmailClient`).
Auth reuses `TokenClient` with the extended scope set. `oauth.go`: new `Scopes` var
= readonly set + `gmail.send` (ReadonlyScopes kept for reference; google-auth and
the connector switch to `Scopes`); operator re-consent runbook amended: re-run
`google-auth add` per account, then `UPDATE source_accounts SET send_enabled=true`
for accounts allowed to send (manual SQL, documented — no new flag).
`send_delivery` refuses accounts with `send_enabled=false`.

**Loop closure hooks (trusted spine writes, connector-side — same trust level as
their existing normalized-table writes):**
- `internal/connector/google/sink.go` `upsertMessage`: when direction=outbound and
  `deliveries.sent_external_id` matches the Message-ID ⇒ `confirmed_at=now()` +
  `delivery_confirmed` task_event (first match only, `confirmed_at IS NULL` guard).
- `internal/connector/upworkcrm` normalize/sink: outbound communications run the
  prefix matcher of criterion 8 against unconfirmed sent upwork deliveries; on
  match, set `sent_external_id` post-hoc (partial unique index makes double-claim
  impossible) + `confirmed_at` + task_event.

**Dashboard** (`internal/dashboard`, `cmd/dashboard`): stdlib `http.ServeMux` with
method patterns, embedded html/template + HTMX, rag-scv structure. Routes:
`GET /healthz`, `GET /deliveries[?status=]`, `GET /deliveries/{id}`,
`POST /deliveries/{id}/edit|approve|send|mark-sent`, `POST /deliveries/new`
(manual draft form → `draft_delivery`, the fallback when the worker can't resolve),
`POST /flags/sending-frozen`. OIDC env (`OIDC_ISSUER`, `OIDC_CLIENT_ID`,
`OIDC_CLIENT_SECRET`, cookie name) or stub mode with `GET /dev/login`. Copy button
is client-side clipboard JS on the delivery body. k8s/nginx packaging deferred —
runs on the workstation.

## MQTT topics

None. (Heartbeats/commands untouched; the draft worker is a cron-style consumer
like triage, not a fleet console.)

## Files likely to touch

- `migrations/0006_deliveries.sql` — new
- `internal/policy/policy.go` — add `Args` to Request
- `internal/policy/matrix.go`, `internal/policy/pg.go` — new (pure core + loader)
- `internal/policy/matrix_test.go` — new
- `internal/executor/executor.go` — pass `call.Args` into `policy.Request`
- `internal/tools/delivery.go` — new (draft/update/approve/send/mark_sent,
  task_mark_delivered, set_sending_frozen); wire names into
  `internal/tools/createtask.go` `Register`
- `internal/mcpserver/schemas.go` — add `draft_delivery`
- `internal/connector/google/oauth.go` — `Scopes` var (+ gmail.send)
- `internal/connector/google/send.go` (+ `send_test.go`) — new
- `internal/connector/google/sink.go` — gmail loop-closure hook in `upsertMessage`
- `internal/connector/upworkcrm/sink.go` / `normalize.go` — upwork confirmation
  matcher on outbound communications
- `internal/drafts/{drafts.go,store.go,prompt.go}` + tests — new
- `cmd/drafts/main.go` — new
- `internal/orchestrator/rules.go`, `facts.go` (+ engine/apply if action wiring
  needs it), `rules_test.go` — R8
- `internal/dashboard/{server.go,handlers.go,auth.go,devlogin.go,templates/*}` — new
- `cmd/dashboard/main.go` — new
- `cmd/opsctl/main.go`, `cmd/ops-mcp/main.go`, `cmd/orchestratord/main.go` — switch
  to the Matrix checker
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` — new event types, drafts advisory-lock key,
  send-scope re-consent status

## In scope / Out of scope

**In scope:** everything above — 0006, delivery tools, policy matrix slice for
deliveries (kill switch, rate limit, channel tiers, human-only actions), draft
worker, Gmail send adapter + scope var, both loop-closure hooks, orchestrator R8,
the single-page deliveries dashboard with dev-login.

**Out of scope (do not bundle):**
- **Jira comment sends and GitHub review/PR verdicts** — step 9. The matrix denies
  those channels (`channel_not_live`); the deny rule is this step's only touch.
- **Calendar invite writes** — policy says approve, but there is no consumer, no
  invite-drafting worker, and it needs `calendar.events` scope; deferring keeps the
  re-consent to gmail.send only. `propose_slots` (step 7) already covers own-block
  scheduling reads. Revisit alongside step 9 or on first real need.
- **Full dashboard board, briefs, exports** — step 10. This step ships exactly the
  `/deliveries` slice.
- **Triage going live / attach-to-task drafting from inbound messages** — the draft
  worker's queue here is Deliver tasks; inbound-reply drafting arrives when triage
  exits shadow mode (step 6 go-live), reusing `draft_delivery` unchanged.
- **Autonomy promotion machinery** (approval-without-edit rate tracking) — the data
  accrues in `approvals` + edit audit rows; the promotion report is future work.
- **k8s deploy of dashboard/drafts** — workstation processes for now.
- **Real Keycloak client provisioning** — OIDC wiring accepts env config; creating
  the Keycloak client is an operator action, not code.

## Invariants that apply

- **3 — everything through the executor:** every delivery mutation (draft, edit,
  approve, send, mark sent, freeze) is a registered tool; handlers stay unexported;
  the dashboard's POST handlers call `Executor.Execute`, never SQL-update
  deliveries. Reads (list/detail) are direct, consistent with existing practice.
- **4 — nothing external without a delivery row:** the ONLY call site of
  `GmailSender.Send` is the `send_delivery` handler, which requires an approved
  deliveries row; `sent_external_id` written before the network call, never resend
  while present; kill switch and rate limits gate the `sending` transition in the
  policy stage; `policy_result` on the row records the verdict.
- **5 — own-message loop closure:** the adapter sets its OWN Message-ID so the
  step-7 normalizer (which stores Message-ID verbatim as `external_message_id`)
  matches it to `sent_external_id`; matched sends attach `delivery_confirmed` to
  the task and are never re-triaged (triage filter is inbound-only; test pins it).
  Upwork closure matches post-hoc via the CRM communication external_id.
- **6 — stealth attribution:** scrub at draft-write AND in `BuildOutboundMIME`
  (strip Co-Authored-By / "Generated with" / model self-references); the draft
  prompt demands Salvador's terse register, no sign-offs; From is Salvador's
  account inherited from the thread — never model-chosen (it is not even in the
  model's schema).
- **7 — orchestrator purity:** R8 is a pure function of (event, facts); the matrix
  `Decide` core is likewise pure — snapshot loaders own the I/O; both unit-test
  with zero network.
- **1 / 2** apply only tangentially: no new ingestion (hooks live inside existing
  raw-first connectors) and no new task-like table — deliveries is outbound
  vocabulary from 0001, queues remain filters over `tasks`.

## Sibling patterns to copy

- **Dashboard:** rag-scv — `~/GolandProjects/rag-scv/internal/http/server.go`
  (ServeMux method patterns, stub-mode branching, `WithOIDC`),
  `internal/http/devlogin.go` (dev-login cookie stub),
  `internal/auth/oidc.go` (`NewOIDC`, `HandleLogin`/`HandleCallback`,
  `MiddlewareWithOIDC`).
- **Worker shape:** `internal/triage/{triage.go,store.go,prompt.go}` +
  `cmd/triage/main.go` — Config, Store interface, consecutive-error abort,
  advisory lock, ai_runs bookkeeping.
- **HTTP client + fake:** `internal/connector/google/gmail.go` (injectable baseURL)
  and `fake_google_test.go` / `poller_test.go` for the httptest fake idiom.
- **Pure-core + loader split:** `internal/orchestrator/{rules.go,facts.go}` — the
  matrix checker mirrors Evaluate/Facts.
- **Tool handler shape:** `internal/tools/donelocal.go` (inTx, insertTaskEvent,
  guarded status UPDATE with RowsAffected check).
- **Actor conventions:** `cmd/opsctl/main.go` `actor()` (`opsctl:{user}`),
  `.mcp.json` `manual:salvo` — the matrix's human-prefix set derives from these.

## Verification protocol

1. `go test ./...` — matrix unit tests (kill switch, rate limit, channel tiers,
   human-only), MIME builder tests (headers, scrub), R8 rule tests, drafts worker
   with fake provider, delivery handler unit tests.
2. `make integration` — lifecycle walk (draft → edit → approve → send against
   httptest fake → delivery_sent event → R8 transitions), idempotent-resend
   refusal, gmail loop-closure (inject an outbound raw message with the matching
   Message-ID, assert confirmed_at + no new task), upwork prefix-match
   confirmation. Serialized (`-p 1`) and cleanup-pact compliant.
3. Manual smoke (no OAuth needed):
   - `go run ./cmd/dashboard` with OIDC env unset; `curl -c j http://localhost:8080/dev/login`
     then browse `/deliveries`.
   - Seed a done_locally task (psql), run orchestratord tick → Deliver task; run
     `cmd/drafts` (fake provider via env or against a real key on one task);
     approve + mark-sent an upwork draft in the dashboard; flip the kill switch and
     assert send buttons are refused (denied audit row in `audit_events`).
   - `psql -h 192.168.50.49 -U ops -d ops -c "select id,channel,status,sent_external_id,confirmed_at from deliveries order by id desc limit 5"`.
4. Post-runbook (when Salvador authorizes Google): re-consent with the extended
   scopes, `send_enabled=true` on one account, send one real email to himself,
   run the google connector, confirm `confirmed_at` fills — this is the go-live
   check, not a commit gate.

## Decisions made unilaterally (rationale inline)

1. **Draft worker queue = R3 Deliver tasks, R3 unchanged.** Alternatives (new
   worker_type on tasks, drafting from inbound messages) either contort
   assignee_type semantics or belong to triage go-live. The worker selects Deliver
   tasks via the `delivery_task` orchestration records and dedups on
   "parent already has a delivery row"; advisory lock prevents races.
2. **`approvals` table over approved_by/at columns** — reuse the 0001 vocabulary,
   don't denormalize.
3. **Kill switch = `ops_flags` table + `set_sending_frozen` tool** — smallest
   audited global flag; env vars can't be flipped at runtime, `decisions` is
   project-scoped.
4. **Rate limit default 10/channel/hour**, counted from `deliveries.sent_at` —
   generous for a solo operator, cheap to query; env-overridable.
5. **Failed-send retry** = clear `sent_external_id` only on definite API rejection;
   ambiguous failures keep it and require operator inspection — the strictest
   honest reading of "never resend while present".
6. **References header = the thread's full Message-ID chain in sent order** (we
   store per-message Message-IDs, not References headers); In-Reply-To = last
   message in thread. Standard reply semantics, deterministic.
7. **To = last inbound sender in the thread** — we don't store Reply-To; this is
   the deterministic reply target, never model-chosen.
8. **Upwork confirmation = 120-char whitespace-normalized body-prefix match**,
   scoped to the delivery's client (target_ref thread_key) and unconfirmed sent
   deliveries — honest minimal; manual mark-sent already covers the human loop, the
   matcher only upgrades it to confirmed.
9. **Channel tier for calendar/jira/github = hard deny in the matrix now** — makes
   "not live yet" a policy fact instead of an unregistered-tool accident.
10. **`policy.Request` gains Args** — the matrix needs the delivery id to know the
    channel; passing the raw args keeps the executor generic and the loader owns
    parsing.
11. **Send adapter lives in `internal/connector/google`** — it reuses TokenClient,
    baseURL idiom, and the fake; a separate adapters package would duplicate OAuth
    plumbing for no isolation gain (the only caller is the send tool either way).
12. **gmail.send only in the re-consent** — calendar write scope deferred with the
    calendar-invite consumer (out of scope above).

No open questions — ambiguities were resolvable from CLAUDE.md, the shipped code,
and INSTITUTIONAL_KNOWLEDGE.md; the calls are documented above and reversible.

## Future work (not this step)

- Inbound-reply drafting when triage goes live (reuses `draft_delivery`).
- Approval-without-edit rate report → autonomy promotion (data already accrues).
- Calendar invite deliveries (`calendar` channel) + calendar.events scope.
- Dashboard k8s packaging behind Keycloak + nginx ingress (step 10 territory).
