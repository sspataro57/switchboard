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

_None yet — greenfield. Add entries here the first time something bites, in this shape:_

### <short name>
**Location:** `file.go:line`
What goes wrong, the symptom you'd observe, and what any new code touching this area
must do to avoid it.

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
- **Deploy:** `ops` namespace on the home k8s cluster; images pushed to
  `192.168.50.20:5000` (insecure local registry).
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

- Access: the **Atlassian MCP** (`atlassian` server in this repo's `.mcp.json`,
  official remote connector, OAuth). If tools are missing in a session, authenticate
  with `/mcp`. Tool names vary by version — search/create/transition/comment on
  issues; discover with ToolSearch.
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
