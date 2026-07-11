> Jira: SWT-6

# 06-gpt-triage — GPT triage worker, SHADOW MODE

## Source

Build-order step 6, quoted from CLAUDE.md:

> 6. GPT triage (port CRM triage prompt; add attach-vs-create via
>    find_related_tasks, per-field confidence). Run SHADOW MODE first —
>    extract everything, create nothing, diff for days before going live.

Constrained by: Stack — "LLM providers: per-worker provider config, never
global. GPT for triage/draft workers (free tokens, strict json_schema output).
Provider details live in adapters only — worker contract is prompt + JSON
schema in, structured result out." Workers — "GPT workers (triage, drafts,
coordinator-assist) are plain queue consumers calling provider adapters. Triage
emits per-field confidence; below threshold → human-review lane, never a live
task." Invariants 2, 3, 5, 7. Scope rule: established clients only (the
step-2 grant already enforces prospects-stay-CRM-side mechanically).

## Goal

Ship a one-shot GPT triage worker that walks the un-triaged inbound
`normalized_messages` corpus, calls an OpenAI provider adapter with a
rubric-style prompt + strict JSON schema (actionable / kind / title / body /
priority / attach-vs-create against deterministic `find_related_tasks`
candidates, every field with its own confidence 0–1), records each call as
`ai_runs` + `ai_extractions`, and creates **nothing** — plus a deterministic
report command that shows what WOULD have been created or attached.

**Usable alone means:** after `go run ./cmd/triage run` against the real ops
db, every inbound established-client message carries a structured
interpretation in `ai_extractions`, and `go run ./cmd/triage report` gives
Salvador a would-create / would-attach / below-threshold diff he can eyeball
for days. New messages ingested by the step-2 connector get triaged on the
next run (cron-cadence, idempotent). Going live — actually minting `holding`
tasks and the human-review lane — is explicitly the next slice.

## Shadow mode is the whole step

No `tasks` rows are written. Not `ready`, not `holding`, none. No
`task_events`, no `deliveries`. The deliverable is the extraction corpus plus
the report. This is load-bearing: acceptance criterion 7 asserts it, tests
assert it, and the worker package contains no code path that inserts into
`tasks` (nothing to disable when going live — the live slice ADDS the
executor `create_task` call, it doesn't remove a guard).

## Acceptance criteria

1. `go build ./...` and `go test ./...` pass offline. No test anywhere calls
   a live LLM: the triage worker is tested against a fake
   `provider.Client`; the OpenAI adapter is tested against `httptest`
   (request shape: strict `json_schema` response_format, model from config,
   auth header; response parsing: content JSON, usage tokens, latency
   measured).
2. Migration `0004_gpt_triage.sql` applies cleanly on top of 0003 (twice;
   second run skips), locally and on the real ops db.
3. `triage run` selects exactly the un-triaged inbound messages: every
   `normalized_messages` row with `direction='inbound'` that has no
   `ai_extractions` row (via a triage `ai_runs` join) for its
   `raw_source_item_id`. Outbound rows are never triaged (invariant 5), but
   do appear in thread context. Messages are processed oldest-first by
   `sent_at`.
4. Each processed message produces one `ai_runs` row (`worker_type='triage'`,
   `provider='openai'`, `model`, `input` = rendered user prompt + prompt
   version + candidate ids + message/raw ids, `output` = verbatim model JSON,
   `status='ok'`, prompt/completion tokens, latency_ms) and one
   `ai_extractions` row (`ai_run_id`, `raw_source_item_id` = the message's
   raw item, `fields` = the parsed extraction with per-field confidences plus
   resolved context: normalized_message_id, thread_id, person_id, project_id
   or null, verdict `create|attach|none`, validation notes). Non-actionable
   messages get an extraction row too ("extract everything").
5. Per-field confidence is preserved verbatim: every schema field is
   `{value, confidence}` with confidence ∈ [0,1]; the worker clamps
   out-of-range values and records that it did so in `fields.validation`.
6. Attach-vs-create is candidate-constrained: `find_related_tasks` (a
   deterministic SQL function, no LLM) supplies up to 10 open tasks for the
   mapped project; the prompt lists them; if the model returns an
   `attach_to_task_id` not in the offered set, the worker nulls it and
   records the rejection in `fields.validation`. Empty candidate list ⇒
   attach_to_task_id must be null.
7. Shadow guarantee: `SELECT count(*) FROM tasks`, `task_events`, and
   `deliveries` are unchanged by any `triage run` or `triage report`.
   Asserted in the integration test and checked in the real smoke.
8. Idempotency: an immediate second `triage run` processes 0 messages and
   makes 0 provider calls. A per-message provider failure writes an
   `ai_runs` row with `status='error'` and NO extraction row, continues with
   the next message, and exits non-zero at the end; the failed message is
   retried on the next run. More than 5 consecutive provider errors aborts
   the run (provider down, stop burning the batch).
9. Single instance: concurrent `triage run` invocations are excluded via
   `pg_try_advisory_lock` (key `0x51570006`); the loser exits immediately
   with a clear message.
10. `triage report` renders, from `ai_extractions` alone (no LLM, no
    network beyond Postgres): summary counts (processed / actionable /
    would-create / would-attach / below-threshold / unmapped-project) and
    per-extraction lines (sent_at, person, project or UNMAPPED, verdict,
    title, min field confidence, attach target). `--threshold` (default from
    `TRIAGE_CONFIDENCE_THRESHOLD`, default 0.7) buckets extractions whose
    minimum decision-relevant confidence falls below it as
    "would-route-to-human-review". Threshold is report-only this step.
11. Provider isolation: OpenAI details (endpoint, auth, request/response
    shapes, model ids) appear ONLY in `internal/provider`. `grep -r openai`
    outside that package (and its config plumbing in `cmd/triage`) is clean.
    `internal/orchestrator` imports nothing new (invariant 7).
12. Real smoke: `triage run --limit 5` against the real corpus + real OpenAI
    lands 5 real extraction rows; `triage report` shows them; task counts
    unchanged.

## Data model changes

Migration: `migrations/0004_gpt_triage.sql` (forward-only). No new tables —
`ai_runs` / `ai_extractions` from 0001 are the vocabulary and fit as-is.

- `ALTER TABLE projects ADD COLUMN client_person_id BIGINT REFERENCES
  people(id)` — the missing message→project hop (see Decisions). Nullable;
  populated manually via psql (14 clients, a handful of projects). Unmapped
  people still get extractions with `project_id: null`; the report flags
  them, which is itself the signal to add the mapping.
- `CREATE INDEX ai_extractions_raw_item_idx ON
  ai_extractions (raw_source_item_id)` — the queue-filter probe
  (NOT EXISTS per message).
- `CREATE INDEX ai_runs_worker_type_idx ON ai_runs (worker_type)` —
  the filter joins extractions to triage runs to stay robust against future
  extraction writers (step 8 drafts, step 10 plan import).

No extension needed (deliberately: pg_trgm would require a superuser
`CREATE EXTENSION` on pg-main — see Decisions on find_related_tasks).

## The queue (filter, not table)

Per invariant 2, the triage queue is a filter over existing rows:

```sql
SELECT m.id, m.raw_source_item_id, m.thread_id, m.sent_at, ...
FROM normalized_messages m
WHERE m.direction = 'inbound'
  AND NOT EXISTS (
    SELECT 1 FROM ai_extractions e
    JOIN ai_runs r ON r.id = e.ai_run_id AND r.worker_type = 'triage'
    WHERE e.raw_source_item_id = m.raw_source_item_id)
ORDER BY m.sent_at
[LIMIT n]
```

No `FOR UPDATE SKIP LOCKED` claim machinery: the worker is a one-shot
single-instance process (advisory lock, criterion 9), so row-level claims
buy nothing. If triage ever becomes a concurrent daemon, the jobagent
SKIP LOCKED pattern is the upgrade path — noted, not built.

Note the interplay with the step-2 connector: a raw item whose content
changes gets `normalized_at` reset and re-normalized, but its old extraction
row still exists, so it is NOT re-triaged automatically. Acceptable for
shadow mode (the CRM rarely mutates history); `--all` style re-triage is
Future work.

## Provider adapter — `internal/provider`

The adapter boundary per CLAUDE.md: prompt + JSON schema in, structured
result out. It records NOTHING itself — the worker owns `ai_runs`.

```go
type Request struct {
    Model      string
    System     string
    User       string
    SchemaName string          // json_schema name, e.g. "triage_extraction"
    Schema     json.RawMessage // strict JSON Schema
    MaxTokens  int
}

type Response struct {
    Raw              json.RawMessage // the message content — schema-shaped JSON
    Model            string          // as reported by the API
    PromptTokens     int
    CompletionTokens int
    LatencyMS        int
}

type Client interface {
    Complete(ctx context.Context, req Request) (Response, error)
}
```

- **OpenAI implementation over net/http, no SDK.** One endpoint
  (`POST {base}/v1/chat/completions`), `response_format: {"type":
  "json_schema", "json_schema": {"name": ..., "strict": true, "schema":
  ...}}`. The official openai-go SDK would be a large dependency for one
  request shape; the adapter IS the isolation layer, so hand-rolling it is
  cheap and keeps `go.sum` small (repo convention so far: pgx, paho, mcp
  only). Refusals (`choices[0].message.refusal`) and non-2xx surface as
  wrapped errors with body excerpts; 429/5xx are not retried in the adapter
  (the worker's error bookkeeping + next cron run is the retry — same
  posture as the connector).
- **Fake** (`internal/provider/fake.go` or a testing subpackage): scripted
  `Client` returning canned `Response`s / errors, capturing requests for
  prompt-assembly assertions. Every non-adapter test uses it.
- Config: `OPENAI_API_KEY` (required at run time), `OPENAI_BASE_URL`
  (optional, default `https://api.openai.com/v1`), model passed per-request
  by the worker from `TRIAGE_MODEL`.
- **Default model `gpt-5-mini`** — current cheap tier with strict
  structured-output support; the ~800-message backfill costs pennies and
  it's the tier covered by the OpenAI complimentary-tokens program
  ("free tokens" in CLAUDE.md). It is a config default in `cmd/triage`
  (env `TRIAGE_MODEL`), never a constant inside the adapter or worker.

## Triage worker — `internal/triage`

A plain queue consumer (CLAUDE.md Workers section), structured like the
connector: pure mappers + a pg store + a `Run` orchestration function.

Per message, oldest-first:

1. **Assemble context** (deterministic SQL):
   - the message (sender, channel, subject, body, sent_at, direction);
   - up to 10 prior messages in the same thread by `sent_at` (both
     directions — outbound context is allowed, outbound *triage* is not);
   - person: `normalized_threads.participants` → `people.display_name`;
   - project: `people.id` → `projects.client_person_id` → slug (or
     UNMAPPED);
   - candidates: `find_related_tasks(project_id)` — see below. Unmapped
     person ⇒ empty candidate list.
2. **Call the adapter** with the triage system prompt + rendered user
   message + the strict extraction schema.
3. **Validate + clamp**: parse against the Go struct mirror of the schema;
   clamp confidences to [0,1]; null non-candidate `attach_to_task_id`;
   derive `verdict`: `attach` if attach_to_task_id set, else `create` if
   actionable.value, else `none`. Record every correction in
   `fields.validation`.
4. **Record**: insert `ai_runs`, then `ai_extractions` (FK ordering). Direct
   writes over the pool, connector-style — see Invariants §3.

### find_related_tasks

A deterministic SQL search feeding candidates INTO the prompt so the model
picks `attach_to_task_id` or null:

```sql
SELECT id, title, status, subproject, updated_at
FROM tasks
WHERE project_id = $1 AND status NOT IN ('closed')
ORDER BY updated_at DESC
LIMIT 10
```

Deliberately no pg_trgm / similarity ranking: the ops role cannot
`CREATE EXTENSION` (INSTITUTIONAL_KNOWLEDGE landmine), the open-task
population at 14-client scale fits entirely in 10 candidates, and semantic
matching against titles is exactly what the LLM in the loop is for. Recency
+ project scoping is the deterministic part; the model does the rest.
Implemented as an internal function this step, not an executor tool — no
agent calls it yet. Promoting it to a registered read-only tool happens in
the live slice if/when an agent-facing surface needs it (documented in
Future work).

### Prompt (port of the CRM triage craft, not its subject)

`internal/triage/prompt.go`: `const PromptVersion = "triage-v1"`, system
prompt, user template, and the JSON Schema literal.

Ported from `~/PycharmProjects/leadTriage/src/crm_lead_triage/rubric.py`:
the **structure** (signal families the model weighs, explicit
"OUTPUT CONTRACT" section restating the schema in prose), the **register**
(decisive, "err toward X" tie-breakers), and the **discipline** (prompt
text isolated in one file, editable without touching wiring). NOT ported:
lead scoring, 1–10 score, status coercion — this triage classifies
established-client messages, it does not score prospects.

System prompt content (test-pinned by substring, exact wording at
implementation):
- Role: triage assistant for a solo contract-engineering business; the
  input is one message from an EXISTING client thread, with recent thread
  context and a list of currently-open tasks for that client's project.
- ACTIONABLE signals: direct requests, bug reports, questions needing an
  answer, scheduling asks, deadline/scope changes, approvals that unblock
  work.
- NOT ACTIONABLE: acknowledgements, thanks, FYI, social pleasantries, our
  own commitments echoed back, automated notifications with no ask.
- ATTACH vs CREATE: attach when the message is progress/reply/detail on an
  offered candidate task; create when it's a distinct new ask; when torn,
  prefer attach — duplicate tasks cost more than a mis-attached log line.
  Only ids from the candidate list are valid.
- Confidence discipline: confidence reflects THIS field, not overall
  vibes; be decisive — 0.5 everywhere is a wasted verdict.
- Output contract: restate fields, kinds
  (`action_request|question|scheduling|status_update|fyi`), priority
  vocabulary (0 normal, 1 elevated, 2 high, 3 urgent — aligns with
  `tasks.priority` int for the live slice), title in Salvador's terse
  register (≤ 80 chars, imperative).

Extraction schema (strict: every property required, `additionalProperties:
false` at every level; each field is `{"value": ..., "confidence": number
0..1}`):

```json
{
  "actionable":        {"value": "boolean",            "confidence": 0.0},
  "kind":              {"value": "action_request|question|scheduling|status_update|fyi", "confidence": 0.0},
  "title":             {"value": "string",             "confidence": 0.0},
  "body":              {"value": "string",             "confidence": 0.0},
  "priority":          {"value": "integer 0..3",       "confidence": 0.0},
  "attach_to_task_id": {"value": "integer|null",       "confidence": 0.0},
  "summary":           "one-line decisive rationale (leadTriage notes style)"
}
```

### Binary — `cmd/triage`

One-shot, scheduling external (cron / manual), exactly like the connector.
Flat under `cmd/` like `fleetd`/`orchestratord`.

```
triage run    [--limit N] [--since 720h]   # N=0, since=0 → all pending
triage report [--threshold 0.7] [--since 720h]
```

`--limit` exists for the smoke (criterion 12) and for rationing the first
backfill; `--since` (on `sent_at`) lets Salvador skip ancient history if the
full backfill proves noisy. Defaults process everything pending — the
corpus is ~815 messages, one backfill is cheap, and a complete shadow
corpus is more diffable than a truncated one.

Env: `DATABASE_URL`, `OPENAI_API_KEY`, `TRIAGE_MODEL` (default
`gpt-5-mini`), `OPENAI_BASE_URL` (optional), `TRIAGE_CONFIDENCE_THRESHOLD`
(default 0.7; report bucketing only this step). Stdout: JSON stats per run
(processed, errors, actionable, attach/create/none counts), connector-style.

The report lives here and not in `opsctl` deliberately: `opsctl` is
documented as "a minimal CLI client of the executor — never writes tool
tables directly", and keeping it a pure executor client is worth more than
one fewer binary. The report is the triage worker reading its own
bookkeeping.

## API / MCP tool changes

**None.** No executor tools registered, no MCP surface, no change to
`create_task` (it stays `ready`-only; the `holding` insert path is the live
slice's problem — see Future work). Where this stands re invariant 3: the
triage worker is a trusted spine service writing its own bookkeeping
(`ai_runs`/`ai_extractions`) directly over the pool — the exact precedent
the step-2 connector set with `sync_runs`/`raw_source_items`, and the
audit posture matches (`ai_runs` IS the per-call audit trail: input,
output, status, tokens, latency). Agent-facing actions are what route
through the executor; this step has none. The moment triage goes live,
task creation goes through executor `create_task` — that call is the next
slice's centerpiece, not this one's.

## MQTT topics

None. GPT workers are plain queue consumers per CLAUDE.md; heartbeats are
the Claude-console contract (`ops/workers/{id}/status`) and a one-shot cron
binary has no live state worth a retained topic. If triage ever becomes a
daemon, it joins the fleet contract then.

## Files likely to touch

Existing (verified in repo):
- `migrations/` — new `0004_gpt_triage.sql` (migrator handles it with zero
  changes).
- `Makefile` — optional `triage-run` convenience target; `integration`
  already covers new tagged tests.
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` — record under Environment facts:
  `OPENAI_API_KEY` lives in `~/.bashrc` (same non-interactive early-exit
  caveat as `JIRA_TOKEN_PERSONAL` — `eval "$(grep '^export OPENAI_API_KEY='
  ~/.bashrc)"`), `TRIAGE_MODEL` default, advisory-lock key `0x51570006`,
  and the manual `projects.client_person_id` mapping recipe.

New:
- `internal/provider/provider.go` — `Client` interface, `Request`/`Response`.
- `internal/provider/openai.go` — net/http implementation, strict
  json_schema response_format.
- `internal/provider/fake.go` — scripted fake for all consumer tests.
- `internal/provider/openai_test.go` — httptest: request shape, auth,
  refusal/error paths, usage parsing.
- `internal/triage/prompt.go` — PromptVersion, system prompt, user
  template renderer, schema literal.
- `internal/triage/triage.go` — `Run(ctx, store, client, cfg)`: queue
  drain, validation/clamping, verdict derivation, consecutive-error abort.
- `internal/triage/store.go` — pg store: pending-message filter, thread
  context, person/project resolution, `FindRelatedTasks`, ai_runs +
  ai_extractions writes, advisory lock. Style: `internal/connector/
  upworkcrm/sink.go`.
- `internal/triage/report.go` — deterministic report over ai_extractions.
- `internal/triage/{triage,prompt,report}_test.go` — unit, fake provider,
  offline.
- `internal/triage/integration_test.go` — build tag `integration`,
  env-gated on `DATABASE_URL`, rerunnable (clean own leftovers, FK order:
  ai_extractions before ai_runs).
- `cmd/triage/main.go` — subcommand + flag parsing, env wiring, run-once.

## In scope

- Migration 0004 as specified.
- `internal/provider` with the OpenAI implementation + fake.
- The triage worker: queue filter, context assembly, find_related_tasks,
  prompt + strict schema, validation, ai_runs/ai_extractions recording,
  advisory-lock single instance, per-message error bookkeeping.
- `cmd/triage` with `run` and `report`.
- Manual one-time mapping of the real projects to client people
  (`UPDATE projects SET client_person_id = ...` via psql) as part of the
  verification protocol, recorded in INSTITUTIONAL_KNOWLEDGE.md.
- Full backfill over the real corpus + the days-long diffing routine
  (Salvador runs `triage run` on cron/manually after each connector sync,
  reads `triage report`).

## Out of scope (do not bundle)

- **Going live** — creating `tasks` rows (`holding` or otherwise), the
  human-review lane, threshold *enforcement*, the `create_task` status/lane
  parameter, and "log every dashboard correction as labeled data" (needs
  step 8's dashboard). This is the explicit next slice after the shadow
  diff proves out.
- **Step 7**: Gmail/Calendar ingestion — triage reads whatever is in
  `normalized_messages`; when step 7's pollers land, their messages flow
  through this same worker untouched.
- **Step 8**: draft worker, deliveries, dashboard approve/edit/send. No
  drafting here — triage classifies, it does not compose.
- **Step 5 extensions**: the orchestrator neither schedules nor consumes
  triage this step (extractions create no task_events).
- Attach-side effects: "attach" verdicts are recorded, not executed — no
  task_events log appending in shadow mode.
- Re-triage of content-mutated raw items (`--all` equivalent).
- Embeddings/similarity-based find_related_tasks.
- Identity-merge handling beyond what step 2 ships (suspected merges stay
  suspected).
- Any daemon-ization, MQTT presence, or k8s packaging.

## Invariants that apply

- **2. One funnel** — the sharp edge of this step. No new task-like tables:
  `ai_extractions` holds *interpretations*, and the triage queue is a
  filter over `normalized_messages` (NOT EXISTS probe), not a table. Shadow
  mode writes zero tasks; when live, every actionable extraction becomes a
  row in THE `tasks` table via `create_task` — never a parallel
  "triage_results to act on" structure. Review check: criterion 7's count
  assertions.
- **3. Everything through the executor** — no agent-facing surface is
  added, so nothing routes around the executor. The worker's direct
  `ai_runs`/`ai_extractions` writes follow the connector precedent (trusted
  spine service, own bookkeeping; `ai_runs` is the audit trail). No import
  of `internal/executor` from `internal/triage`. The live slice's task
  creation WILL go through executor `create_task`.
- **5. Own-message loop closure** — triage processes `direction='inbound'`
  only. Our own sends (outbound rows the connector ingests) are never
  re-triaged into new tasks — enforced here structurally by the queue
  filter, not just by step 8's delivery matching. Outbound rows still
  appear in thread context (they're legitimate conversation history).
- **7. Orchestrator is pure** — the orchestrator is untouched and still
  imports no provider adapter. Triage is a separate consumer binary;
  vendor details live only in `internal/provider` (criterion 11). The
  deterministic halves of triage (queue filter, context assembly,
  find_related_tasks, validation/clamping, verdict derivation, report) are
  pure functions / plain SQL, unit-testable offline with the fake provider
  — the same testability discipline invariant 7 exists to protect.
- **1. Raw-first (read side)** — triage consumes normalized rows and links
  every extraction to `raw_source_item_id`, so any extraction is traceable
  to (and reprocessable from) the raw provider JSON. It captures nothing
  new, so it owes no raw writes.
- (4, 6 have no surface: nothing external is sent, nothing client-visible
  is authored. Titles/bodies in extractions are internal; the terse-register
  prompt guidance is for future task quality, not attribution.)

## Sibling patterns to copy

- **One-shot worker shape, sink style, run stats**:
  `internal/connector/upworkcrm/{ingest,normalize,sink}.go` +
  `cmd/connectors/upworkcrm/main.go` in this repo — env wiring, phase
  functions over a pg store, JSON stats to stdout, non-zero exit on error.
- **Advisory-lock single instance**: `internal/orchestrator/engine.go`
  (`pg_try_advisory_lock`, key `0x51570005`) — copy the idiom, new key
  `0x51570006`.
- **Env-gated integration tests, rerunnable cleanup**:
  `internal/connector/upworkcrm/integration_test.go` and
  `internal/executor/integration_test.go` — build tag `integration`, skip
  without `DATABASE_URL`, delete own leftovers in FK order.
- **Prompt craft to port**: `~/PycharmProjects/leadTriage/src/
  crm_lead_triage/rubric.py` — signal-family rubric, decisive register,
  OUTPUT CONTRACT section, prompt isolated from wiring. `chain.py` is the
  cautionary tale its docstring tells: structured output is only guaranteed
  on native OpenAI — which is why the adapter pins native strict
  json_schema and a base-url override doesn't grow a parser fallback until
  a LAN endpoint actually needs one.
- **Pg store style**: `internal/audit/pg.go` — small struct over
  `*pgxpool.Pool`, wrapped errors, context-first.

## Verification protocol

1. `go test ./...` — green offline, no compose db, no network.
2. `make integration` — 0001–0004 onto compose Postgres, then: seed a mini
   corpus (person + identities + thread + inbound/outbound messages + a
   mapped project + 2 open tasks), run with the fake provider →
   assert criteria 3–8 (rows written, confidences preserved, candidate
   rejection nulled, outbound skipped, second run = 0 processed, task/
   task_events/deliveries counts unchanged, error path leaves retryable
   state).
3. Apply 0004 to the real ops db:
   `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/tools/migrate --dir
   migrations` (twice; second all-skips).
4. Map the real project(s):
   `psql -h 192.168.50.49 -U ops -d ops` →
   `UPDATE projects SET client_person_id = (SELECT person_id FROM
   person_identities WHERE provider='upwork_crm' AND value='<client
   uuid>') WHERE slug='<slug>';` — record the recipe in
   INSTITUTIONAL_KNOWLEDGE.md.
5. Real smoke ("usable alone"), with
   `eval "$(grep '^export OPENAI_API_KEY=' ~/.bashrc)"`:
   `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/triage run --limit 5`
   → stdout stats show 5 processed, 0 errors; then psql:
   - `SELECT status, model, prompt_tokens, latency_ms FROM ai_runs WHERE
     worker_type='triage' ORDER BY id DESC LIMIT 5;` → ok rows, plausible
     tokens/latency.
   - `SELECT fields->'actionable', fields->>'verdict' FROM ai_extractions
     ORDER BY id DESC LIMIT 5;` → sane structured verdicts with
     confidences.
   - `SELECT count(*) FROM tasks;` / `task_events` / `deliveries` —
     unchanged.
6. `go run ./cmd/triage run --limit 5` again → 0 new provider calls for
   those 5 (stats show 5 fewer pending, or 5 different messages processed —
   idempotency on already-extracted items).
7. `go run ./cmd/triage report` → readable would-create / would-attach /
   below-threshold / unmapped summary.
8. Full backfill (no --limit), then the multi-day routine: connector sync →
   `triage run` → `triage report` → Salvador diffs judgment vs reality.
   Going-live is gated on this diff looking right, not on this ticket.
9. Commit via `/ticket-deliver` after review.

## Decisions made unilaterally (rationale attached; flag in review if wrong)

- **Client→project mapping = nullable `projects.client_person_id` FK,
  populated manually.** The missing hop is exactly one edge (identity →
  person exists via step 2; person → project doesn't). A column beats a
  `project_identities` mapping table at 14-client scale, beats matching on
  the free-text `projects.client` column (fragile), and shadow mode makes a
  wrong mapping observable and costless. Constraint accepted: one project
  per client person for triage-default purposes; a multi-project client
  needs a mapping table or per-thread override — Future work, and the
  report's UNMAPPED lane will say so before it hurts.
- **Inbound-only triage.** Invariant 5 says our sends never re-triage into
  tasks; enforcing it structurally in the queue filter is cheaper and
  stronger than trusting a prompt. Cost: commitments Salvador makes in his
  own outbound ("I'll ship X Friday") aren't mined into tasks — Future
  work, distinct feature ("commitment extraction"), not triage.
- **Extraction row for every inbound message, actionable or not.** "Extract
  everything" verbatim from the build order; also makes the idempotency
  filter uniform (one probe, no actionability special case) and gives the
  shadow diff its denominator.
- **No claim machinery; advisory lock + sequential.** One-shot cron binary,
  single instance — `FOR UPDATE SKIP LOCKED` claims solve a concurrency
  this step doesn't have. The jobagent pattern is documented as the upgrade
  path if triage daemonizes.
- **find_related_tasks = recency + project scope, no pg_trgm.** Extension
  creation needs superuser on pg-main (recorded landmine); the open-task
  population per project fits in the 10-candidate window entirely, so
  similarity ranking would reorder a list the model sees in full anyway.
  Internal function, not an executor tool — no agent calls it this step.
- **Direct ai_runs/ai_extractions writes (no executor).** Connector
  precedent: trusted spine service, own bookkeeping, `ai_runs` is itself
  the per-call audit record (input/output/status/tokens/latency). Routing
  a non-agent batch worker's bookkeeping through executor audit would
  double-write the same trail.
- **net/http OpenAI adapter, no SDK.** One endpoint, one request shape;
  the adapter is the isolation boundary either way; keeps the dependency
  set small (pgx/paho/mcp today). Revisit if a second provider capability
  (streaming, files) actually lands.
- **Default model `gpt-5-mini`, env-configurable.** Cheap current tier with
  strict structured outputs, covered by the complimentary-token program
  CLAUDE.md's "free tokens" refers to; `TRIAGE_MODEL` env keeps it
  per-worker config, never global, never a buried constant.
- **Per-message error = record + continue + exit non-zero; 5 consecutive
  errors = abort.** Batch progress shouldn't die on one flaky call, but a
  dead provider shouldn't burn 800 attempts. No adapter-level retries: the
  idempotent filter makes the next cron run the retry, same posture as the
  connector.
- **Report in `cmd/triage`, not `opsctl`.** `opsctl` stays a pure executor
  client (its own doc comment says so); the report is the worker reading
  its own bookkeeping. One more binary is cheaper than blurring that line.
- **Threshold ships as config, enforces nothing.** CLAUDE.md's "below
  threshold → human-review lane" is a live-mode routing rule; in shadow
  there is no lane to route to. Shipping `TRIAGE_CONFIDENCE_THRESHOLD` now
  lets the report bucket by it so days of diffing calibrate the value
  before it gates anything.
- **`--since` filter offered but defaulting to full backfill.** A complete
  shadow corpus over real history is the best calibration set we'll ever
  get, and it costs pennies at the mini tier. The flag exists in case the
  old-message noise drowns the report.

## Future work (not this SPEC)

- **Going live**: `create_task` grows a status/lane parameter (`holding`
  for auto-lane, plus the human-review lane for below-threshold fields);
  triage calls the executor; attach verdicts append task_events logs;
  correction logging as labeled data once the dashboard (step 8) exists.
- Promote `find_related_tasks` to a registered read-only executor tool when
  an agent-facing consumer appears.
- Multi-project clients: mapping table or per-thread project override.
- Commitment extraction from Salvador's own outbound messages.
- Re-triage path for content-mutated raw items (`--all` equivalent) and a
  prompt-version-driven re-extraction sweep (PromptVersion is recorded per
  run precisely so this is possible).
- Embedding-based candidate retrieval feeding find_related_tasks once
  `content_chunks`/`embeddings` get their first writer.
- Daemon-ization + fleet presence + CronJob packaging when the first
  long-running deploy lands.

## Open questions

None — the two genuinely open design points (client→project mapping, queue
idiom) were decided unilaterally above with reversible defaults; shadow
mode itself is the safety net for both.
