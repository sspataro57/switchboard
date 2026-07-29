# Switchboard Institutional Knowledge

Single source of truth for landmines, conventions, and known issues in this repo.
All agents in `.claude/agents/` read this file at session start instead of duplicating
its contents in their prompts. The spec itself lives in `CLAUDE.md` — this file holds
what the spec can't: things learned the hard way, environment facts, and infra quirks.

**When you update this file:** agents pick up changes on their next session. No need
to edit individual agent prompts unless the change is structural (a new category, not
a new item in an existing category).

---

## Known landmines (verified bites)

### Inherited ANTHROPIC_API_KEY starves worker sessions
**Location:** `internal/worker/loop.go` (CmdRunner env), bit 2026-07-11
The claude subprocess inherits the wrapper's environment; a stray
`ANTHROPIC_API_KEY` silently overrides the claude.ai subscription login and
runs fail with exit 1 + `is_error: "Credit balance is too low"`. Run opsworker
with `env -u ANTHROPIC_API_KEY` (or a deliberately configured key). Related:
`claude -p` exits NON-ZERO on is_error runs but still emits a valid result
envelope on stdout — the wrapper salvages it (session_id must be recorded even
for failed runs, or resume breaks).

### needs_feedback flips mid-run
**Location:** task lifecycle, bit 2026-07-11 (test race)
`request_feedback` sets the task to needs_feedback DURING the claude run —
polling status is not "the run ended". The session task_event lands only after
the run's envelope is parsed; don't read "parked but no session event" as a
loss until the wrapper logs park.

---

## The seven invariants (review checklist form)

These are normative in `CLAUDE.md` ("Non-negotiable invariants"); this is the
diff-review phrasing. Every reviewed diff gets checked against each:

1. **Raw-first** — connector code writes `raw_source_items` (raw JSON + content_hash)
   before any normalize/extract step. A connector that parses provider JSON straight
   into normalized tables is a violation even if it "also" saves raw.
2. **One funnel** — no new task-like tables. If a diff adds a table that holds
   "things to act on," it should be rows in `tasks` with a filter, not a sibling table.
3. **Everything through the executor** — any new tool/handler goes
   validate → policy check → audit start → handler → audit complete. Grep for handlers
   invoked outside the executor path. No raw_sql / raw_api tools exposed to agents.
4. **Nothing external without a delivery row** — any code that sends (SMTP/Gmail API,
   Jira comment, calendar invite, gh review) must be reachable only from a `deliveries`
   row in an approved state, and must be idempotent on `sent_external_id`.
5. **Own-message loop closure** — normalizer changes must keep the external-id match
   to delivery rows; our own sends must never re-triage into new tasks.
6. **Stealth attribution** — adapters strip `Co-Authored-By` trailers, set commit
   author, keep drafts in Salvador's terse register. Applies to product output, not
   just this repo's commits.
7. **Orchestrator purity** — the orchestrator never imports a provider adapter or
   calls an LLM. Rules are pure functions of (event, task, policy), unit-testable
   with no network. Every decision writes an audit row.

---

## Architectural conventions

- **Queue claims:** Postgres `FOR UPDATE SKIP LOCKED` — same pattern as jobagent
  (`~/GolandProjects/job-agent`). Read that implementation before writing claim code;
  don't invent a second claim idiom.
- **Dashboard:** Go + HTMX, following the rag-svc pattern (`~/GolandProjects/rag-scv`).
  Copy its handler/template structure rather than designing fresh.
- **Provider adapters:** LLM vendor details (model ids, API shapes, keys) live in
  adapters ONLY. Worker contract is prompt + JSON schema in, structured result out.
  A vendor import outside an adapter package is a flag.
- **Migrations:** forward-only, numbered. No `down` migrations, no editing an
  already-applied migration.
- **Vocabulary:** table/tool names in CLAUDE.md's schema section are the vocabulary —
  reuse, don't invent synonyms (it's `deliveries`, not `outbound_messages`).
- **Error handling:** wrap with context — `fmt.Errorf("doing X: %w", err)`. Flag bare
  `return err` in new code.
- **Context propagation:** functions doing I/O take `context.Context` first. New
  goroutines respect cancellation.

---

## Environment facts

- **Postgres:** `ops` db on pg-main (CNPG), `pg-main-rw.cnpg.svc:5432` in-cluster.
  The CNPG image already ships **pgvector** (confirmed 2026-07-11; `vector` was
  already in the template db) — local test Postgres must match
  (`pgvector/pgvector` image, not stock `postgres`).
  **Local access (established 2026-07-11):** no port-forward needed — the
  `pg-main-rw-lb` LoadBalancer exposes it at `192.168.50.49:5432`
  (namespace `cnpg`). Role `ops` owns database `ops`; its password lives in
  `~/.pgpass` (psql just works: `psql -h 192.168.50.49 -U ops -d ops`) and as
  `OPS_DATABASE_URL` in `~/.bashrc` (same non-interactive caveat as
  JIRA_TOKEN_PERSONAL — grep/eval it, don't source). Superuser creds:
  k8s secret `cnpg/pg-main-superuser`. The `ops` role can NOT
  `CREATE EXTENSION` — pgcrypto/vector were pre-created by postgres on the
  `ops` db; a future migration needing a new extension must be preceded by a
  superuser `CREATE EXTENSION` (record it here when it happens).
- **MQTT:** Mosquitto at `192.168.50.45:1883` (WebSocket `:9001`). Debug with
  `mosquitto_sub -h 192.168.50.45 -t 'ops/#' -v`. Heartbeats on
  `ops/workers/{worker_id}/status` (retained, QoS 1), commands on
  `ops/workers/{worker_id}/cmd` (NOT retained). `worker_id` == client for
  single-console; dotted `{client}.{subproject}` for multi-console (one topic
  level; mirror derives client as prefix before first `.`). The contract lives
  in `internal/fleet` — payload types, topic builders, 60s cadence constant.
  Retained-message gotcha: tests/smokes MUST clear their retained messages
  (`mosquitto_pub -r -n -t <topic>`) — retained state is global on the
  production broker. fleetd (cmd/fleetd) mirrors status → worker_heartbeats.
- **Deploy:** `ops` namespace on the home k8s cluster (created 2026-07-26);
  images pushed to `192.168.50.20:5000` (insecure local registry).
  Manifests live in the sibling **kube** repo (`~/projects/personal/kube/
  switchboard/`), not here — that repo is the cluster's source of truth.
  One image `switchboard:<tag>` carries every connector binary plus
  `/migrations`; the CronJob picks the entrypoint (`command:
  [/usr/local/bin/jira]`). Pin the tag — CronJobs have no rollout semantics,
  so `:latest` makes "which code ran" unanswerable.
  Live as of 2026-07-26: CronJobs `connector-upworkcrm` + `connector-jira`
  (*/15); `connector-google` exists but is SUSPENDED pending OAuth. Secrets
  `switchboard-db` / `-upwork-crm` / `-token-key` / `-google` are out-of-band.
  **Landmine (bit 2026-07-26):** the upwork_crm DSN contains `&` (the
  `options=-c default_transaction_read_only=on` param). Sourcing it from a
  `KEY=value` file leaves the variable UNSET — bash reads `&` as a background
  operator. Build such secrets with `--from-file`, never `--from-literal` via
  a sourced env file.
  Nothing else of switchboard is deployed: no orchestrator, dashboard, triage,
  drafts, fleetd, or hooksd in-cluster yet.
- **Upwork CRM (connector source, wired 2026-07-11):** db `upwork_crm` on pg-main.
  The `ops` role has SELECT on exactly `clients` + `communications` (granted as
  postgres: `GRANT CONNECT ON DATABASE upwork_crm TO ops; GRANT USAGE ON SCHEMA
  public TO ops; GRANT SELECT ON clients, communications TO ops;`) — the
  narrow grant also mechanically enforces "prospects stay CRM-side".
  Connector source DSN: `UPWORK_CRM_DATABASE_URL` = ops role against
  `/upwork_crm` with `options=-c default_transaction_read_only=on` (set it in
  the shell when running `cmd/connectors/upworkcrm`; not stored in ~/.bashrc —
  derive from the ops password in ~/.pgpass). GOTCHA: ~/.pgpass lines are
  per-database — the `ops:ops` line does NOT cover `upwork_crm`; a separate
  `192.168.50.49:5432:upwork_crm:ops:<pw>` line exists. A psql "hang" here is
  usually an invisible password prompt, not a lock. Known topics: `crm/leads/triage`
  (CRM → leadTriage, `{lead_id, reason, trace_id}`) and `crm/leads/approved`
  (leadTriage → proposalWriter, `{lead_id, score, status, ai_notes, trace_id}`;
  NOT fired on rejection). Lead status contract: 0=new, 1=rejected, 2=AI-approved
  (score ≥ 7). Pipeline repos: crm (`~/WebstormProjects/crm`), upwork-scrap
  (Mac mini; clone at `~/WebstormProjects/upwork-scrap`), leadTriage +
  proposalWriter (`~/PycharmProjects/`).

---

## Orchestrator contract (shipped in SWT-5)

- NOTIFY on `task_events` is a WAKE-UP only; the cursor drain
  (`orchestrator_cursor`, seeded at max event id so first deploy never replays
  history) is the sole delivery path. Missed/duplicate NOTIFYs are harmless.
- Dedup idiom: `orchestrated` task_events (written via `record_orchestration`)
  are the replay-dedup keys — rules check them in Facts before firing.
- Claim-expiry sweep EXEMPTS `needs_feedback` (parked ≠ crashed; expiring
  would orphan the resume).
- Single instance via `pg_try_advisory_lock` key `0x51570005`.
- Spine transition tools (`task_block`/`task_unblock`/`task_close` on
  already-target statuses) are idempotent no-op successes so replays never
  stall the drain; `task_close` refuses only active work.
- **Landmine:** `fleet.NewMirrorClient` hardcodes client id `switchboard-fleetd`
  — a second connection with that id kicks fleetd off the broker. Spine
  services use `fleet.NewSpineClient(ctx, broker, distinctID)`.
- Morning brief: env `ORCH_BRIEF_PROJECT` (unset = disabled), `ORCH_BRIEF_HOUR`
  (default 7). Deterministic SQL + Go template; never an LLM.

## Plan import + full board (shipped in SWT-10)

- One-way funnel: `planimport propose --project <slug> --file <path>` (raw-first
  under synthetic `provider='plan'` account `plans@local`,
  `external_id=plan:{slug}:{sha256}`; live gpt-5-mini parse, `PLAN_MODEL` env)
  → dashboard `/plans/{id}` approve/reject → `planimport apply --id N`
  (single-tx tree insert via `apply_plan_import`) → file replaced by a stub.
- The stub marker is `<!-- switchboard:imported plan_import={id} ... -->` on
  the FIRST line; propose refuses stubs. Hash-mismatched/missing files skip
  the stub write with a warning (tasks stand; never clobber unreviewed edits).
- ZERO tasks exist before approval — proposals live in ai_extractions
  (`worker_type='plan_import'`, invisible to triage's pending filter) plus a
  `plan_imports` gate row (0008; partial unique on (project_id, content_hash)
  WHERE status <> 'rejected' — one live proposal per content; re-propose only
  after reject).
- `plan_order` = 1-based sibling array position, assigned by Go in
  `planimport.Validate` — never model-chosen. Apply emits `child_created` /
  `dependency_added` / `plan_imported` (roots) events; R4 blocks dependents on
  the next drain — no orchestrator changes.
- Policy: `approve/reject/apply_plan_import` are humanOnly
  (dashboard:/opsctl:/manual:); `propose_plan_import` static fallthrough. None
  is MCP-listed — agents' verb for discovered work stays create_child_task.
- Dashboard: `/tasks` board (queues = query-param filters; closed hidden
  unless `?status=closed`), `/tasks/{id}` detail, `/briefs` (title predicate
  `Morning brief %` — R7's own dedup key), `/plans`, `/export/tasks.csv|json`
  (pinned header, id ASC). `GET /` now redirects to `/tasks`.
- Real smoke done 2026-07-11: plan_import 1 (switchboard follow-ups, 12 tasks
  #9-#20 on the real board — the operator-pending backlog itself); roots
  9/14/17 ready, 9 dependents R4-blocked; `~/plans/switchboard-followups.md`
  is now a stub.

## Jira + GitHub connectors (shipped in SWT-9)

- Jira accounts: `jira-auth add <email> --site URL --projects KEY1,KEY2`
  (JIRA_API_TOKEN + OPS_TOKEN_KEY env; project scoping MANDATORY — unscoped
  polls are refused so the SWT build tracker never enters the product funnel).
  Real account registered 2026-07-11: sspataro.atlassian.net scoped to CRM.
- **Landmine (bit 2026-07-11): JQL naive datetimes are interpreted in the
  USER'S profile timezone** — a UTC-formatted `updated >= "YYYY-MM-DD HH:MM"`
  bound silently matched nothing. Use the relative form `updated >= "-Nm"`
  (TZ-independent), as the connector now does.
- Raw ids: `issue:{KEY}` (stored minus the comments array) + `comment:{KEY}:{id}`;
  messages channel 'jira', thread_key `jira:{site_host}:{KEY}`; own comments
  (author == polling accountId) are outbound → invisible to triage.
- jira_comment channel is LIVE (matrix: rate-limited allow; all comments start
  at approve — the auto tier for progress comments is the earned-promotion
  path). sent_external_id = `jira:{site_host}:comment:{id}` (id assigned
  post-call; ambiguous failures recovered by the poller's prefix matcher).
- GitHub: `cmd/hooksd` (HMAC receiver, raw-first on delivery:{guid}; PUBLIC
  EXPOSURE PENDING deploy) + `cmd/connectors/github --repos owner/repo`
  (gh-token poller, same tools). PR↔task linking: external_refs
  (`link_external_ref`, agent-facing) or the `task-{N}-*` branch fallback.
- Orchestrator R9-R11: pr_opened→pr_open, ci started→awaiting_ci,
  ci_passed→awaiting_merge, pr_merged→done_locally (emits done_local so R3
  chains), pr_closed→ready+log, red CI ×2→ready with logs (same task).
- New task_events vocabulary: pr_opened/pr_merged/pr_closed, ci_started/
  ci_passed/ci_failed. New spine tools: record_pr_event, record_ci_event,
  task_pr_transition; agent-facing: link_external_ref.

## Slack send promotion (SWT-12) — contract + landmines

`slack_reply` is an **approve**-tier channel: switchboard clicks Send through the
connector's bridge after `approve_delivery`. Verified 2026-07-29 (switchboard half).

- **Nothing sends until the leaf ships.** The `/send` route and `send` CLI op do
  not exist in `sspataro57/slackconnector` yet. A leaf 404 is a 4xx, which is a
  DEFINITE rejection, so the row lands in `failed` and is re-approvable — safe, but
  not exercisable end to end.
- **`SLACK_CONNECTOR_UNATTENDED_SEND` is per-process.** Set it in the
  bridge-server's launchd environment ONLY. Setting it in the leaf MCP server's
  environment silently removes the manual path's human token gate. Two separate
  launchd environments on the mini; they are not the same knob.
- **Per-workspace go-live is `source_accounts.send_enabled`**, gmail's convention.
  `EnsureAccount` inserts `false` and never updates it, so a newly ingested
  workspace is safely off: `UPDATE source_accounts SET send_enabled=true` per
  workspace, by hand.
- **Confirmation only works where export works.** Export fails closed for any
  allowed workspace with no `OWN_USER_IDS` entry; `T0HPR78RX`
  (Collaboratory/LlamaSite) has none, so the bridge is narrowed to Avviato. A send
  into an unexported workspace stays unconfirmed forever and always ends flagged.
  `connector-slackweb` is also currently SUSPENDED.
- **A browser click reserves no message id.** Hence the whole shape:
  `send_attempted_at` commits before the click, `sent_external_id` stays NULL on
  success, and the next export stamps it by matching a 120-char body prefix.
  `'sending'` is TERMINAL until the matcher or a human moves it — nothing retries,
  because a retry of a click that did land is a double-post into a client channel.
- **`sending` means two different things and the columns tell them apart.**
  `send_attempted_at IS NOT NULL AND send_settled_at IS NULL` is IN FLIGHT;
  settled is ambiguous. `mark_delivery_failed` refuses an unsettled attempt younger
  than 15 minutes (`sendAttemptLease`) — that refusal is what stops a human
  reopening a live call for a second send.
- **`approval_source` says which authority let a row out**: `'switchboard'`
  (policy gated it) or `'leaf_token'` (the connector's own token did; switchboard
  only recorded it). `send_delivery` requires `'switchboard'`.
- **The kill switch is for switchboard.** `send_delivery` is freeze-gated;
  `mark_delivery_sent` is not, because recording a send made elsewhere cannot be
  prevented by freezing — only hidden. Freeze-time records emit
  `delivery_recorded_during_freeze`.
- **`sync_runs.started_at` for slackweb is the export's START**, passed into
  `StartRun` explicitly. It used to default to `now()` at insert, which was the
  export's END, because `Ingest` exports before creating the run row. The
  reconciler counts passes that could have OBSERVED a message, so this matters.
- **Over MCP, `mark_delivery_sent` permits exactly one transition**: resolving a
  `slack_reply` row already in `'sending'`. Everything else (approved, or drafted
  via `leaf_gated`) is dashboard/`opsctl` only, because `delivery_sent` drives R8
  and an injected call could otherwise fabricate a completed delivery. The durable
  fix is a leaf-produced receipt; it needs the `/send` route first.
- **`policy.MCPTransportPrefix` is the one definition of `"mcp:"`** —
  `humanActor` strips it, `executor.ViaMCP` tests it. Do not re-litter the literal.

---

## Slack Web connector (shipped in SWT-13)

- Leaf is the sibling TS repo (`~/projects/personal/slackconnector`), driven as a
  one-shot subprocess: `node dist/cli/switchboard-bridge.js export|draft`.
  `SLACK_WEB_BRIDGE_SCRIPT` must be an ABSOLUTE path to a regular file; no shell.
  Runs where the logged-in Chromium is — the **Mac mini**, never the cluster.
- Vocabulary: `provider='slack_web'`, synthetic account
  `{workspace_id}@slack-web.local`; raw ids `conversation:{id}` and
  `message:{conv}:{msg}`; `thread_key = slack:{ws}:{conv}[:{root_msg}]`;
  normalized channel `slack`; delivery channel `slack_reply` (migration 0009
  extends the CHECK — the drop/add is safe only because migrate runs each file
  in one transaction).
- Direction FAILS CLOSED: export requires `SLACK_CONNECTOR_OWN_USER_IDS` (member
  id per workspace). Missing `author_id`/`own_user_id` errors rather than
  guessing — no display-name matching, ever.
- Assisted tier: `prefill_delivery` (human-only, spine-facing, deliberately NOT
  MCP-listed) types an approved body into the composer; `send_delivery` stays
  denied by `channel_assisted`; a human sends and `mark_delivery_sent` records
  it. `CommandBridge.Draft` refuses any bridge result claiming `sent` — that's
  the Go-side backstop, now covered by `bridge_test.go` (stub script via
  `/bin/sh` as "node", no Node or browser needed).
- **Landmine (found in review, fixed 2026-07-26): non-canonical `target_ref`
  silently kills loop closure.** `ParseTargetURL` accepts a trailing slash;
  `confirmDelivery` matches `target_ref` by EXACT string against a trimmed
  value. `draft_delivery` now stores `Target.CanonicalURL()`, never the
  caller's spelling. Any new code writing `target_ref` must canonicalize too —
  the failure mode is a delivery that can never be confirmed, with no error.
- Loop closure = exact destination + whitespace-normalized 120-char body prefix
  (`slackMatchPrefixLen`), guarded by `WHERE sent_external_id IS NULL` plus a
  RowsAffected check so `--all` replays never double-emit `delivery_confirmed`.
- **Accepted risks (recorded, not bugs):** the global kill switch does NOT gate
  `prefill_delivery` (it isn't `sendShaped`), so a freeze still permits typing
  into a live composer — matches SPEC criterion 11; add it to `snapshotGated` if
  that changes. And opsctl's 30s deadline covers the whole browser prefill; on
  expiry node is killed mid-typing and a partial composer draft may remain,
  which a retry refuses to overwrite (clear it by hand).

## Delivery contract (shipped in SWT-8)

- Lifecycle tools: `draft_delivery` (agent-facing, THE route for client-visible
  words; gmail From resolved server-side from the thread — never caller-chosen),
  spine-facing `update_delivery`/`approve_delivery`/`send_delivery`/
  `mark_delivery_sent`/`task_mark_delivered`/`set_sending_frozen`.
- Policy matrix (internal/policy Matrix wrapping the static list): rules
  `kill_switch` (ops_flags row sending_frozen), `rate_limit` (10/channel/hour,
  `OPS_SEND_HOURLY_LIMIT`), `channel_assisted` (upwork_chat send denied —
  copy/prefill + mark_delivery_sent), `channel_not_live` (jira/calendar/github),
  `human_only` (delivery mutations need dashboard:/opsctl:/manual: actors).
- Invariant-4 idempotency: send_delivery commits `sending` + self-chosen
  `<sb-{id}-...>` Message-ID BEFORE the network call; a present
  sent_external_id refuses resend forever.
- Loop closure (invariant 5): gmail — connector's upsertMessage confirms the
  delivery by Message-ID (`confirmed_at` + `delivery_confirmed` event);
  upwork assisted — post-hoc 120-char body-prefix match fills sent_external_id.
- Orchestrator R8: `delivery_sent` → parent done_locally→delivered + Deliver
  task closed (`delivery_lifecycle` dedup record).
- task_events vocabulary additions: `delivery_sent`, `delivery_confirmed`.
- Dashboard slice: `cmd/dashboard` (:8085, `/deliveries`), dev-login when
  OIDC_ISSUER unset (`GET /dev/login`); actions all through the executor.
- Draft worker: `cmd/drafts run` (DRAFTS_MODEL default gpt-5-mini) over R3
  Deliver tasks; model contract strictly {subject, body}.
- GO-LIVE PENDING: gmail sends need the SWT-7 OAuth runbook + re-consent with
  `google.Scopes` (now includes gmail.send) + manual
  `UPDATE source_accounts SET send_enabled=true` per allowed account.

## Google connector (shipped in SWT-7 — code complete, OAuth PENDING)

- **Operator runbook (Salvador, once — the only manual part):**
  1. GCP console: create project `switchboard`, enable Gmail API + Google
     Calendar API.
  2. OAuth consent screen: External, app `switchboard`, the 5 account emails
     as test users, scopes gmail.readonly + calendar.readonly, then PUBLISH TO
     IN PRODUCTION (staying in Testing expires refresh tokens after 7 days).
  3. Credentials → OAuth client ID → Desktop app → download JSON to
     `~/.config/switchboard/google_client_secret.json` (chmod 600).
  4. `openssl rand -base64 32` → `export OPS_TOKEN_KEY=...` in ~/.bashrc.
  5. Per account ×5: `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/google-auth
     add <email>` (browser opens; identity verified via getProfile — a
     mismatch aborts). `google-auth list` to confirm.
  6. `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/connectors/google` — then
     cron it at 5-15 min. Re-run = incremental.
- Cursors: `sync_cursor = {"gmail_internal_date_ms": N, "calendar_sync_token": "..."}`.
- Cross-account Message-ID dedup: partial unique index (0005) — raw is NOT
  deduped (per-account, invariant 1); normalize-time skip, losers stamped.
- Direction rule: outbound iff From ∈ any provider='google' account email.
- Availability: `propose_slots` executor tool (opsctl call), env
  `AVAIL_TZ` (default Europe/Rome) / `AVAIL_WORK_START|END|DAYS`.
- Step 8 re-consent: extend `google.ReadonlyScopes` with send/write scopes and
  re-run google-auth add per account.

## Triage contract (shipped in SWT-6, SHADOW MODE)

- `OPENAI_API_KEY` lives in `~/.bashrc` — same non-interactive early-exit
  caveat as JIRA_TOKEN_PERSONAL: `eval "$(grep '^export OPENAI_API_KEY=' ~/.bashrc)"`.
- `TRIAGE_MODEL` default `gpt-5-mini`; advisory-lock key `0x51570006`.
- Client→project mapping recipe (manual, per client):
  `UPDATE projects SET client_person_id = (SELECT person_id FROM
  person_identities WHERE provider='upwork_crm' AND value='<client uuid>')
  WHERE slug='<slug>';` — unmapped people show in the report's UNMAPPED lane.
- Shadow is structural: `triage.Store` has no task-write method (reflection
  test enforces); going live ADDS the executor create_task call.
- Routine until live: connector sync → `triage run` → `triage report`; diff
  for days; going-live is gated on the diff, not the ticket.
- **Landmine (bit 2026-07-11): integration suites cross-pollute** — the
  triage pending filter and the connector's global count assertions share one
  compose db, so `make integration` runs `go test -p 1` (serialized) and the
  two suites neutralize each other's fixtures in cleanup. New integration
  suites with global-count assertions must join that mutual-cleanup pact.

## Task lifecycle contract (shipped in SWT-4)

- task_events event-type vocabulary: `claimed`, `status_changed`, `log`,
  `session` (payload carries session_id/is_error/num_turns/cost_usd — the
  resume pointer; latest wins), `feedback_requested`, `feedback_answered`,
  `done_local`, `child_created`, `released`. The NOTIFY trigger is step 5's.
- Fleet `resume` cmd args schema (pinned): `{"task_id": N, "feedback_request_id": M}`.
- `OPS_WORKER_ID` injection rule: ops-mcp force-overwrites any model-supplied
  `worker_id` from its env — identity is never model-chosen. The wrapper sets
  it when spawning claude; interactive sessions use `manual:salvo` (.mcp.json).
- Spine-facing tools (`task_release`, `answer_feedback`) are registered on the
  executor but NOT MCP-listed; reach them via `opsctl call` / `opsctl
  answer-feedback [--resume]`.
- Wrapper testing trick: `CLAUDE_BIN` env points the wrapper at a stub script
  emitting a canned result envelope.

## Test infrastructure

- **Unit tests:** `go test ./...`. Orchestrator rules and the policy matrix must be
  testable with zero network (invariant 7 exists partly for this).
- **Integration tests:** against a local Postgres (dockerized). `make db-up`
  starts it (`docker-compose.yml`, image `pgvector/pgvector:pg17`, host port
  **5433**, user/pass/db all `ops`); `make migrate` applies migrations to it;
  `make integration` does db-up + migrate + `go test -tags integration ./...`.
  Integration tests are build-tagged `integration` AND skip when `DATABASE_URL`
  is unset. Local URL: `postgres://ops:ops@localhost:5433/ops?sslmode=disable`.
  Compose also runs Mosquitto on host port **1884** (`docker/mosquitto.conf` —
  2.x needs `allow_anonymous true`); fleet integration tests additionally gate
  on `MQTT_BROKER` (local: `tcp://localhost:1884`). Never point tests at the
  production broker.
- **Provider adapters in tests:** never call live LLMs from tests. Adapters get a fake
  implementing the same interface.
- **Integration tests must be rerunnable against a persistent db** (bit 2026-07-11:
  the executor integration test passed on a fresh db, failed on rerun — cleanup
  `DELETE FROM projects` hit the tasks FK from its own prior run, and a
  `count(*)==1` assertion drifted). Clean up your own leftovers first, in FK
  order (children before parents), scoped by a test-owned actor/slug.
- _Known infra issues: none yet — record flakes and races here the first time they bite._

---

## Process conventions

- **Auto-commit is authorized** (Salvador, 2026-07-11: "commit automatically,
  don't ask — this is internal"). After /ticket-deliver's checks pass, commit on
  the ticket branch, merge to main, push, and move the Jira issue to Done.
  Never `Co-Authored-By` / AI references in commits (stealth rule still binds).
  This supersedes the old "no auto-commit" line here and in CLAUDE.md.
- **Diagnose before changing** — reproduction-first for bugs (`/bug-start`).
- **Never** `Co-Authored-By: Claude` trailers (also enforced via `.claude/settings.json`).
- Branches (once the repo has remotes/PR flow): `ticket-NN-short-kebab` for build-order
  steps, `bug-short-kebab` for bugs.
- Specs live in `docs/tickets/`, bug artifacts in `docs/bugs/`.

## Jira build tracker

Planning is local (SPECs in `docs/tickets/`); **tracking of record is Salvador's
personal Jira**: https://sspataro.atlassian.net, project **SWT** ("switchboard").
Verified 2026-07-11. (The same site also has a `CRM` project — not ours.)

- Access: the **jira MCP** (`jira` server in this repo's `.mcp.json` — `uvx
  mcp-atlassian`, token auth as `sspataro@gmail.com` via `${JIRA_TOKEN_PERSONAL}`;
  the env var must be set in the shell that launches the session). Tool names vary
  by version — search/create/transition/comment on issues; discover with ToolSearch.
- Fallback only: `JIRA_TOKEN_PERSONAL` env var exists (API token, basic auth as
  `sspataro@gmail.com`) — exported in `~/.bashrc`, but `.bashrc` early-exits for
  non-interactive shells, so `source ~/.bashrc` yields an EMPTY token there (and
  Jira answers unauthenticated searches with 200 + zero issues — looks like an
  empty board, isn't). Working pattern:
  `eval "$(grep '^export JIRA_TOKEN_PERSONAL=' ~/.bashrc)"`.
  Prefer the MCP; don't build curl wrappers.
- Every build ticket/bug gets a mirrored SWT issue (summary `{ID}: <goal>`); the
  local artifact records it as `> Jira: SWT-N` on its first line. `PENDING-SYNC`
  means the MCP wasn't available — the next command retries.
- **Specs live in Jira too** (Salvador, 2026-07-11): the issue description carries
  the FULL SPEC (markdown → Jira wiki markup; PUT via `/rest/api/2/issue/{key}` —
  v2 takes wiki text, v3 needs ADF). Sync at /ticket-start, re-sync whenever the
  SPEC changes, and at /ticket-deliver. Local files remain the working copies.
- **A description caps at 32,767 characters.** Bit 2026-07-29: the
  slack-send-promotion SPEC reached 37,778 after two review rounds and the v2 PUT
  returned `400 {"errors":{"description":"The entered text is too long..."}}`. The
  "full SPEC lives in the issue description" convention has a ceiling, so a long
  SPEC syncs as a section-aligned prefix with a pointer line naming the repo file
  as authoritative. Check `len(spec)` before the PUT rather than discovering it
  from a 400.
- **Comments mangle underscored identifiers — descriptions don't.** Verified
  2026-07-29. The jira MCP's `jira_add_comment`/`jira_edit_comment` convert
  Markdown to ADF, and *paired* underscores become emphasis: `sent_external_id`
  stores as `sent*external_id`, `mark_delivery_sent` as `mark*delivery*sent`. A
  lone underscore survives (`TestMatrix_MailToolsFallThroughForWorkers` is
  intact). Backticks are worse — inline code spans become line breaks — and
  backslash escapes are worse still (the backslash is kept AND the underscore
  still converts). Fenced blocks don't help either. No known escape works.
  Consequence: put anything identifier-dense in the **description** and keep
  comments to prose. Do NOT "fix" a mangled comment by rewriting a good
  description through the same converter — that risks corrupting correct
  content to tidy incorrect content.
- **The mangling is on the MCP's read side too — trust the REST GET, not the
  MCP's echo.** Verified 2026-07-29 by syncing the 22,124-char
  slack-send-promotion SPEC into SWT-12. `jira_transition_issue` echoed the
  description back full of `sent*external*id` and `slack\_reply`, but a direct
  v2 GET confirmed storage was byte-identical to the file on disk. So a session
  that reads a SPEC through `jira_get_issue` sees corrupted identifiers that are
  NOT corrupted in Jira — do not "repair" them. The MCP is fine for status,
  transitions, search, and prose comments; for description read/write use the
  v2 REST endpoint.
- **Working description sync** (bypasses the converter entirely, exact):
  `eval "$(grep '^export JIRA_TOKEN_PERSONAL=' ~/.bashrc)"`, then PUT
  `{"fields":{"description": <file contents>}}` to
  `https://sspataro.atlassian.net/rest/api/2/issue/{KEY}` with basic auth
  (`sspataro@gmail.com` + token). Pipe the SPEC straight from the file rather
  than retyping it — v2 stores the markdown raw, renders it imperfectly, and
  keeps every underscore and backtick. Read it back and assert equality with
  the file; that check is cheap and has already caught one silent difference.
- Sync points: `/ticket-start` & `/bug-start` create + move to In Progress;
  `/ticket-deliver` comments results and moves toward review — **Done only after
  Salvador actually commits**, never before.
- This tracker is fine to write to automatically (it's Salvador's own board and the
  whole point is tracking). Terse register, no AI references in summaries/comments.
- **Do not conflate with the product's Jira connector.** The product ingests
  client-facing Jira (treetopllc etc.) as a *connector* per CLAUDE.md — the personal
  board is only for building switchboard itself. The meta-tasks (`tasks` table)
  follow the product design; they do not sync here.

---

## How agents should use this file

- `spec-writer`: invariants + conventions + environment — apply to the SPEC's
  "invariants that apply" and "files likely to touch" sections.
- `test-author`: test infrastructure section; invariant 7 for orchestrator tests.
- `go-reviewer`: all sections — this file plus CLAUDE.md is the review checklist.
- `bug-reproducer`: environment facts + test infra — pick a reproduction surface
  that avoids known infra issues.
- `bug-diagnoser`: landmines first — they're the cheapest hypotheses.

---

## Update protocol

When you discover a new landmine, fix a known one, or change a convention:
1. Update this file.
2. Mention "I updated INSTITUTIONAL_KNOWLEDGE.md" so the next session re-reads it.
3. Don't touch agent prompts unless the change is structural.
