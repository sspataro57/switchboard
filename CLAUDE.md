# ops — personal agentic work operating system

Meta-task system. Captures work signals from all sources (Upwork CRM, Gmail ×5,
Calendars, Jira, GitHub), reduces everything to canonical tasks in one funnel,
routes to queues consumed by Claude Code workers or Salvador, and emits
policy-gated outbound communications (email, Jira comments, Upwork replies,
calendar bookings, PR verdicts).

Core principle: **agency at the leaves, determinism at the spine.**
AI proposes structured interpretations and actions. The application stores,
validates, gates, audits, and executes them. The orchestrator NEVER calls an LLM.

## Stack (decided — do not relitigate)

- **Go** for everything in the spine: ops-svc, orchestrator, connectors, MCP
  server, worker wrapper. No LangGraph, no Temporal, no workflow engine.
  Orchestration is a Postgres event loop + cron ticker.
- **Postgres** (`ops` db on pg-main, CNPG, `pg-main-rw.cnpg.svc:5432`).
  Queue of record. Claims via `FOR UPDATE SKIP LOCKED` (same pattern as jobagent).
- **MQTT** (Mosquitto, `192.168.50.45:1883`, WS `:9001`) — fleet nervous system.
  Worker heartbeats + command topics. LWT for dead-console detection.
- **Dashboard**: Go + HTMX (rag-svc pattern), Keycloak OIDC in front,
  nginx ingress. The dashboard IS the board — there is NO OpenProject in this
  system. Jira is a connector (client-facing), never internal state.
- **LLM providers**: per-worker `provider` config, never global.
  GPT for triage/draft workers (free tokens, strict json_schema output).
  Claude Code for execution workers. Provider details live in adapters only —
  worker contract is prompt + JSON schema in, structured result out.
- Deploy: `ops` namespace on the home k8s cluster, images to 192.168.50.20:5000.

## Non-negotiable invariants

1. **Raw-first**: every captured item lands in `raw_source_items` (raw provider
   JSON + content_hash) BEFORE normalization or AI extraction. Reprocessing must
   always be possible.
2. **One funnel**: every source normalizes into canonical objects
   (Message/Thread/Event/Document/Person) and every actionable thing becomes a
   row in ONE `tasks` table. Queues are filters, not tables.
3. **Everything through the executor**: every tool call (MCP, dashboard,
   internal) goes validate → policy check → audit start → handler → audit
   complete. No side doors. No raw_sql / raw_api tools exposed to agents.
4. **Nothing external without a delivery row**: outbound comms exist only as
   `deliveries` that passed the policy matrix (auto / dashboard-approved /
   console-initiated). Idempotent sends: `sent_external_id` set once, never
   resend while present.
5. **Own-message loop closure**: our sends re-enter via ingestion; normalizer
   matches by external id to the delivery row and attaches as task log —
   never re-triaged into new tasks.
6. **Stealth attribution**: no Claude bylines anywhere client-visible. Adapters
   enforce it (strip Co-Authored-By trailers, set commit author, drafts in
   Salvador's terse register).
7. **Orchestrator is pure**: rules are functions of (event, task, policy).
   Unit-testable without any model. Every decision writes an audit row.

## Core schema (starting point — extend, don't fork)

```
source_accounts   (provider, account_email, refresh_token_encrypted [pgcrypto],
                   scopes, domain_default, send_enabled, calendar_in_availability)
sync_runs, raw_source_items (source_account_id, external_id, raw_json,
                   content_hash, ingested_at, normalized_at)
normalized_messages / _threads / _events / _documents
people, person_identities (identity resolution across emails/upwork/jira ids;
                   suspected merges → dashboard approval, never auto-merge)
projects          (client, execution: auto|manual, delivery: auto|dashboard|console,
                   repo_path, send_from_account, policies jsonb)
tasks             (project, subproject, assignee_type: human|claude, worker_type,
                   status, autonomy, parent_id, plan ordering, priority,
                   execution override per-task)
task_dependencies, task_events (LISTEN/NOTIFY feed for orchestrator),
task_claims, worker_heartbeats, feedback_requests
external_refs     (task_id, system: jira|github|upwork_crm, external_key,
                   external_url, sync_cursor, direction)
deliveries        (task_id, channel: gmail|jira_comment|upwork_chat|calendar|
                   github_review, target_ref, body, status: drafted→approved→
                   sending→sent|failed, policy_result, sent_external_id)
decisions         (project-scoped, written by coordinator, injected into every
                   task context)
ai_runs, ai_extractions, policy_decisions, approvals, audit_events
content_chunks, embeddings
```

Task status machine:
```
holding → ready → claimed → in_progress → needs_feedback ⇄ in_progress
in_progress → pr_open → awaiting_ci → awaiting_merge → done_locally
   (red CI ×2 → back to ready with logs appended; same task, never a new one)
done_locally → delivered → closed
blocked ⇄ ready (dependency resolution by orchestrator)
```

## Workers

- **Execution workers** = Claude Code headless loops. Wrapper (~150 lines) per
  client console: heartbeat(idle) → `get_next_task(client[, subproject])` →
  claim → `claude -p "$(context)" --output-format json
  --dangerously-skip-permissions` → capture session_id → done | park.
  Resume after feedback: `claude --resume $SESSION_ID -p "Feedback on #N: ..."`.
- **Loop rules (bake into worker prompt)**: never choose your own work; claim
  exactly one task; never ask in the console — call
  `request_feedback(task_id, question)` and end turn; cross-boundary
  questions on multi-console projects → `create_task(subproject=main,
  type=coordination)`, never decide unilaterally; repo actions (git/gh in your
  worktree) are free, but words on client-visible surfaces (Jira comments, PR
  review text on client repos, emails) go through MCP delivery tools only.
- **Heartbeats**: retained MQTT on `ops/workers/{client}/status`
  {state: idle|working|needs_feedback|manual, task}. LWT publishes
  {state: dead}. Commands on `ops/workers/{client}/cmd` (resume, pause, dispatch).
- **GPT workers** (triage, drafts, coordinator-assist) are plain queue
  consumers calling provider adapters. Triage emits per-field confidence;
  below threshold → human-review lane, never a live task. Log every dashboard
  correction as labeled data.
- **Manual mode**: no loop running; tasks pile in ready. Interactive session
  uses same MCP (.mcp.json) — `/task N` claims + dumps context; finish with
  mark_done_local. Session hook publishes state=manual.
- **Multi-console projects**: main = coordinator worker (decomposition,
  integration, decisions via `record_decision`); subprojects scoped by
  `get_next_task(subproject=X)`. Coordination tasks default approve_first.

## Policy matrix (initial — autonomy is EARNED per category by
approval-without-edit rate, promoted manually)

| channel/action                         | initial     |
|----------------------------------------|-------------|
| Jira progress comments                 | auto        |
| Jira final/status-bearing              | approve     |
| Email client-facing                    | approve     |
| Upwork replies                         | assisted (draft → copy/prefill → scraper confirms sent) |
| Upwork initial follow-up               | approve, existing threads only, ≤2 touches |
| Calendar own blocks                    | auto (always via availability service propose_slots) |
| Calendar invites w/ others             | approve     |
| GitHub: branch/draft-PR own repos      | auto        |
| GitHub: PR open on client repos        | draft PR    |
| GitHub: review comments                | approve → promote |
| GitHub: approve/request-changes        | approve     |
| merge_when_green                       | per-repo flag; default manual sweep |

Global kill switch freezes all `sending` transitions. Per-channel rate limits.
Scope: established clients only — unknown senders tagged prospect, stay CRM-side.

## Build order (each step ships something usable alone)

1. Schema + migrations; executor + audit skeleton.
2. Upwork CRM connector (poll upwork_crm tables on pg-main OR extend its MQTT
   topics) → raw → normalize.
3. MQTT heartbeat contract + minimal fleet view.
4. MCP task tools (task_get_next, claim, context, append_log,
   request_feedback, mark_done_local, create_child) + wrapper + ONE Claude loop
   against one real client. Manual task creation is fine here.
5. Orchestrator event loop: dependencies, lifecycle rules (done_locally →
   delivery task; needs_feedback → feedback task + resume cmd), claim
   expiry, cron templates (morning brief).
6. GPT triage (port CRM triage prompt; add attach-vs-create via
   find_related_tasks, per-field confidence). Run SHADOW MODE first —
   extract everything, create nothing, diff for days before going live.
7. Google OAuth (one project, Desktop-app client, loopback flow, publish to
   In Production to avoid 7-day token expiry; test users = the 5 accounts;
   readonly scopes only). Gmail + Calendar pollers (5–15 min). Message-ID
   dedup across accounts. Availability service (free/busy merge +
   propose_slots — deterministic, no LLM).
8. Draft worker + deliveries + dashboard approve/edit/send + Gmail send
   adapter (threading headers, From inherited from thread — never
   model-chosen) + Upwork assisted tier.
9. Jira connector (poll in, gated comments out). GitHub webhooks (review_pr
   tasks, CI events driving pr_open→merged states).
10. Plan import (one-way: .md → task tree via LLM-assisted parse + dashboard
    approval; file replaced by stub; new discovered work =
    create_child_task, never plan-file edits). Dashboard full board, briefs,
    exports.

## Working discipline (applies to sessions building THIS repo)

- Diagnose before changing. Auto-commit authorized: once /ticket-deliver passes,
  commit, merge to main, push (no AI references in commits — ever).
- Migrations forward-only, numbered.
- Table/tool names above are the vocabulary — reuse, don't invent synonyms.
- When ambiguous: small, reversible, audited beats clever.

## Harness

Session workflow lives in `.claude/`. Read `.claude/INSTITUTIONAL_KNOWLEDGE.md` at
session start — landmines, environment facts (DB/MQTT/registry), and conventions
accumulate there, not here.

Vocabulary split: **tickets** belong to the projects switchboard manages — Jira
tickets, GitHub issues, whatever arrives through connectors and is referenced in
`external_refs`. **Tasks** are switchboard's own canonical work items to route (the
`tasks` table above). A connector ingests a client's *ticket*; triage may create a
*task* from it. (This repo's build workflow also uses "ticket" for its own work
items — `/ticket-start` below.)

- `/ticket-start <N|slug>` — spec a build-order step (spec-writer → SPEC + open
  questions) and create/track the mirrored Jira issue
- then `test-author` writes failing tests → implement on the main thread
- `/ticket-review <ID>` — go-reviewer checks the diff against the seven invariants;
  optional codex adversarial pass for executor/policy/delivery code
- `/ticket-deliver <ID>` — full tests + "usable alone" smoke check + commit-ready
  summary (the commit itself is Salvador's) + Jira comment/transition
- `/bug-start <slug>` — reproduction-first bug flow (bug-reproducer → bug-diagnoser);
  no source reading until a reproduction fails as described

Planning is local — specs in `docs/tickets/`, bug artifacts in `docs/bugs/` — but
**tracking of record is the personal Jira** (sspataro.atlassian.net, project **SWT**,
via the Atlassian MCP in `.mcp.json`; see INSTITUTIONAL_KNOWLEDGE.md "Jira build
tracker"). That board tracks the BUILD only; it is unrelated to the product's Jira
connector for managed projects.
