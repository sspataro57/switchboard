> Jira: SWT-9

# 09-jira-github-connectors — Jira connector (poll in, gated comments out) + GitHub PR/CI events

## Source

Build-order step 9, CLAUDE.md:

> 9. Jira connector (poll in, gated comments out). GitHub webhooks (review_pr
>    tasks, CI events driving pr_open→merged states).

## Goal

Ship both external work surfaces: a Jira poller that ingests client issues/comments
raw-first into the one funnel and a live `jira_comment` delivery channel (the first
`channel_not_live` graduation), plus a GitHub event path — HMAC-verified webhook
receiver AND a gh-token REST poller feeding one shared event mapper — whose
`pr_opened`/`ci_*`/`pr_merged` task_events drive the `pr_open → awaiting_ci →
awaiting_merge → done_locally` machine via new pure orchestrator rules.

**Usable alone means:** today, with zero deploy work — (a) `cmd/connectors/jira`
polls sspataro.atlassian.net (project CRM) and its issues/comments appear as
normalized threads/messages that shadow triage automatically sees; (b) Salvador
drafts a Jira comment on a task, approves it in the dashboard, `send_delivery`
posts it for real, and the next poll confirms it (loop closure by comment id);
(c) a real PR on sspataro57/switchboard, linked to a task via
`link_external_ref`, walks the task through pr_open → awaiting_ci →
awaiting_merge → done_locally driven by the poller — no public endpoint needed.
The webhook receiver is code-complete and tested against HMAC-signed fake
payloads; public exposure is documented operator/deploy work.

## Acceptance criteria

1. Migration `0007_jira_github.sql` applies on fresh and current
   schemas: `external_refs` gains `created_at`, a `UNIQUE (task_id, system,
   external_key)` constraint, and an index on `(system, external_key)`. No other
   DDL.
2. **Jira onboarding** (`cmd/jira-auth`, google-auth idiom): `jira-auth add
   --site <base-url> --email <auth-email> --projects KEY[,KEY]` reads the API
   token from `JIRA_API_TOKEN` (never argv), verifies identity via
   `GET /rest/api/2/myself` before storing, and upserts a `source_accounts` row:
   `provider='jira'`, `account_email=<auth-email>`, `domain_default=<base-url>`,
   `scopes=<project keys>`, `refresh_token_encrypted =
   pgp_sym_encrypt(token, OPS_TOKEN_KEY)`, `send_enabled=false`. `jira-auth list`
   shows configured sites. The token never appears in plaintext in the db
   (integration test pins it, oauth_integration_test idiom).
3. **Jira ingest (raw-first, invariant 1):** `cmd/connectors/jira` per
   provider='jira' account: searches `GET /rest/api/2/search/jql` with
   `project IN (<scopes>) AND updated >= "<cursor>" ORDER BY updated ASC`
   (paginated via nextPageToken), fetches each hit's full issue JSON
   (`GET /rest/api/2/issue/{key}` incl. the `comment` field; if
   `comment.total > len(comments)` it pages `/issue/{key}/comment`), and writes
   raw_source_items BEFORE any normalization: one item per issue
   (`external_id=issue:{KEY}`, raw = issue JSON minus the comments array) and
   one item per comment (`external_id=comment:{KEY}:{commentId}`, raw = the
   comment JSON) — per-comment items keep the shipped 1 raw item : 1 message
   invariant of `normalized_messages_raw_item_idx`. Content-hash
   insert/update/unchanged decision and `sync_runs` bookkeeping reuse the
   upworkcrm `Sink`/`upsertRaw` pattern; hash change resets `normalized_at`.
   Cursor `sync_cursor={"jira_updated_at": <max fields.updated seen>}` advances
   only on success, re-read with a trailing overlap (default 1h — JQL `updated`
   has minute granularity and evaluates in the API user's profile timezone;
   overlap + idempotent upserts absorb both).
4. **Jira normalize (one funnel, invariant 2):** reads ONLY raw_source_items.
   Per issue item: upsert one `normalized_threads` row
   (`thread_key=jira:{site_host}:{KEY}`, subject = summary, participants =
   reporter/assignee) and one `normalized_messages` row for the description
   (`external_message_id=jira:{site_host}:issue:{KEY}`, sent_at = issue
   created, sender = reporter, `channel='jira'`). Per comment item: one message
   (`external_message_id=jira:{site_host}:comment:{id}`, sent_at = comment
   created, sender = author). Direction: `outbound` iff the author's
   `accountId` equals the polling account's own accountId (from `/myself`,
   fetched once per run and cached in the cursor JSON), else `inbound`.
   Authors upsert `people`/`person_identities` (`provider='jira'`,
   `value=accountId`) via the upworkcrm `ReconcileIdentities` no-auto-merge
   idiom (suspected merges surfaced in stats). Re-normalize from raw alone is
   idempotent (rerun test).
5. **Shadow triage sees Jira for free:** an inbound Jira comment normalized by
   criterion 4 appears in triage's PendingMessages (no triage code change —
   integration test pins one jira-channel message flowing into the shadow lane).
6. **jira_comment goes live (invariant 4):** `draft_delivery` accepts
   `channel='jira_comment'` requiring `target_ref` in thread_key form
   `jira:{site_host}:{issueKey}` (or `thread_id` pointing at a jira thread, from
   which target_ref is derived); `from_account_id` is resolved server-side by
   matching site_host against provider='jira' accounts — never caller-chosen.
   The policy matrix allows `send_delivery` on channel `jira_comment`
   (kill switch + shared per-channel hourly rate limit still apply);
   `channel_not_live` shrinks to `calendar` + `github_review`. Sends require
   `status='approved'` and `send_enabled=true` on the resolved account.
   **Every jira_comment still requires human approval this step** — see
   Decision 1 (the matrix's "auto" tier for progress comments is the documented
   promotion path, not shipped now).
7. **Jira send adapter:** a `JiraSender` seam beside `GmailSender` in
   `internal/tools/delivery.go`; real implementation
   `internal/connector/jira/send.go` posts
   `POST /rest/api/2/issue/{key}/comment` `{"body": <plain text>}` (v2 — no
   ADF) with basic auth (email + pgp_sym_decrypted token), injectable baseURL,
   body scrubbed via `google.ScrubAIAttribution` (belt; draft-time scrub
   remains). Because Jira assigns the comment id AFTER the call (unlike gmail's
   self-chosen Message-ID), the idempotency shape adapts: in-tx commit
   `status='sending'` BEFORE the network call (a `sending` row without
   sent_external_id refuses re-send); on 2xx set
   `sent_external_id = jira:{site_host}:comment:{id}` + `status='sent'` +
   task_event `delivery_sent` (orchestrator R8 then runs unchanged); definite
   rejection ⇒ `failed` (re-approvable); ambiguous transport failure ⇒ `failed`
   with a logged operator warning to check the issue before re-approving.
8. **Jira loop closure (invariant 5):** the jira sink's outbound-message upsert
   confirms the matching delivery by
   `external_message_id = sent_external_id` (google `confirmDelivery` idiom:
   first match only, `confirmed_at IS NULL` guard, `delivery_confirmed`
   task_event, never a new task). Belt for the ambiguous-failure hole: an
   outbound jira comment that matches NO sent_external_id is prefix-matched
   (120-char whitespace-normalized, upworkcrm matcher idiom) against `failed`
   jira deliveries on the same issue with `sent_external_id IS NULL`; a match
   fills sent_external_id post-hoc + confirms — mechanically blocking a
   duplicate re-send.
9. **link_external_ref** (agent-facing, MCP-listed):
   `{task_id, system: jira|github, external_key, external_url?}` upserts an
   `external_refs` row (idempotent on the new unique constraint). Executor path
   like every tool. The worker prompt gains two lines: name PR branches
   `task-{id}-{slug}` and call link_external_ref
   (`external_key={owner}/{repo}#{n}`) after opening a PR.
10. **GitHub webhook receiver** (`internal/connector/github` + `cmd/hooksd`):
    verifies `X-Hub-Signature-256` (HMAC-SHA256 over the raw body with
    `GITHUB_WEBHOOK_SECRET`, constant-time compare; bad/missing ⇒ 401, no
    processing). Every accepted delivery is stored raw-first under the
    synthetic provider='github' account (`account_email='github@webhooks'`,
    upworkcrm idiom), `external_id=delivery:{X-GitHub-Delivery}` — redelivery
    dedup for free. Handled events: `pull_request`
    (opened/reopened/closed), `workflow_run`
    (requested/in_progress/completed). Unknown events ⇒ 200, raw stored,
    ignored. Payload → intents is a pure `MapEvent` function; intents become
    executor calls (actor `hooksd:github`) to the new spine tools — hooksd
    never writes task_events or tasks directly.
11. **PR↔task matching:** by `external_refs (system='github',
    external_key='{owner}/{repo}#{n}')`, newest active link wins; fallback when
    no ref exists and the head branch matches `task-{N}-*` ⇒ hooksd/poller
    calls link_external_ref itself, then proceeds. No match at all ⇒ raw
    stored, debug log, no events (not ours to route).
12. **Spine event tools** (registered, NOT MCP-listed): `record_pr_event`
    `{task_id, action: opened|merged|closed, pr, url}` ⇒ task_events
    `pr_opened|pr_merged|pr_closed`; `record_ci_event` `{task_id, phase:
    started|completed, conclusion?, run_id, run_url, name?}` ⇒ `ci_started` /
    `ci_passed` / `ci_failed`. Both are idempotent on their external key
    (pr number + action / run_id + phase+conclusion, checked against existing
    task_events) so webhook + poller overlap never double-counts.
13. **GitHub poller** (`cmd/connectors/github`, the no-public-endpoint
    interim): for each open external_ref (system='github', task not
    delivered/closed), fetches PR state (`GET /repos/{o}/{r}/pulls/{n}`) and
    its head-SHA check runs (`GET /repos/{o}/{r}/commits/{sha}/check-runs`)
    with `GITHUB_TOKEN`, stores the fetched JSON raw-first
    (`external_id=pr:{owner}/{repo}#{n}`, hash-diffed), maps observed state
    through the SAME intent vocabulary as MapEvent, and emits the same
    idempotent tool calls. Cursor: the per-ref `external_refs.sync_cursor`
    (TEXT, already in 0001) holds the last observed state fingerprint.
14. **Orchestrator rules (pure, invariant 7):**
    - **R9 pr_lifecycle** — `pr_opened` ⇒ `task_pr_transition(to=pr_open)`
      (from ready/claimed/in_progress); `pr_merged` ⇒
      `task_pr_transition(to=done_locally, summary="PR merged: {url}")` — the
      handler emits a `done_local` task_event so R3 (Deliver task) chains
      unchanged; `pr_closed` (unmerged) ⇒ back to ready + `task_append_log`.
    - **R10 ci_lifecycle** — `ci_started` ⇒ awaiting_ci (from pr_open);
      `ci_passed` ⇒ awaiting_merge (from pr_open/awaiting_ci).
    - **R11 ci_failure** — on `ci_failed`, Facts expose the consecutive-failure
      streak (ci_failed events since the last ci_passed/pr_opened): streak 1 ⇒
      `task_append_log` with the run URL; streak ≥2 ⇒
      `task_pr_transition(to=ready)` + log — same task, never a new one
      (CLAUDE.md status machine).
    All three dedup via `record_orchestration` records (existing
    `orchestrated()` idiom); merge_when_green stays manual (out of scope).
15. **task_pr_transition** (spine tool, NOT MCP-listed): `{task_id, to:
    pr_open|awaiting_ci|awaiting_merge|ready|done_locally, reason/summary}`
    with the legality set pinned in "API changes" below; already-at-target ⇒
    idempotent no-op success (replay discipline, same as task_block).
16. `go test ./...` green; `make integration` green including: jira ingest +
    normalize + loop-closure walk against an httptest fake Jira; a
    delivery walk drafted→approved→sent (fake Jira) →confirmed; hooksd
    HMAC + payload→events; a full PR lifecycle walk (link → pr_opened →
    ci_started → ci_failed ×2 → ready, then pr_opened → ci_passed →
    awaiting_merge → pr_merged → done_locally → R3 Deliver task). New suites
    join the mutual-cleanup pact (serialized `-p 1`, FK-ordered cleanup,
    test-owned slugs).

## Data model changes

Migration `migrations/0007_jira_github.sql` (forward-only):

```sql
-- external_refs gets its first writers (link_external_ref, hooksd fallback).
ALTER TABLE external_refs
  ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE external_refs
  ADD CONSTRAINT external_refs_task_system_key_uniq
  UNIQUE (task_id, system, external_key);
CREATE INDEX external_refs_system_key_idx ON external_refs (system, external_key);
```

Notes:
- No deliveries DDL — `target_ref`, `error`, `sent_at`, `confirmed_at` (0006) and
  the `jira_comment` channel CHECK (0001) already cover this step.
- No normalized_* DDL — per-comment raw items keep the 1:1
  `normalized_messages_raw_item_idx` upsert target; `channel` column (0002)
  takes value `jira`.
- New task_events vocabulary: `pr_opened`, `pr_merged`, `pr_closed`,
  `ci_started`, `ci_passed`, `ci_failed` (record in INSTITUTIONAL_KNOWLEDGE.md).
- Key formats pinned: thread_key `jira:{site_host}:{KEY}`; message ids
  `jira:{site_host}:issue:{KEY}` / `jira:{site_host}:comment:{id}` (the send
  adapter's sent_external_id uses the comment form — loop closure is straight
  equality); github external_key `{owner}/{repo}#{n}`.

## API / MCP tool changes

All tools register in `internal/tools.Register` (invariant 3). Executor path
unchanged.

**Agent-facing (added to `internal/mcpserver/schemas.go` agentTools):**

- `link_external_ref` — `{task_id, system: jira|github, external_key,
  external_url?}` → `{external_ref_id}`. Idempotent upsert. Internal
  bookkeeping, not client-visible words, so safe to expose; workers use it after
  opening PRs (worker prompt updated).
- `draft_delivery` — validation extended: `channel='jira_comment'` allowed with
  `target_ref` = `jira:{site_host}:{issueKey}` or a jira `thread_id`;
  from_account resolved server-side by site_host (mirror of the gmail mailbox
  resolution). Schema description updated.

**Spine-facing (registered, NOT MCP-listed; reachable via executor from hooksd /
poller / orchestrator / opsctl):**

- `record_pr_event` — `{task_id, action, pr, url}`; inserts the task_event,
  idempotent on (task, pr, action).
- `record_ci_event` — `{task_id, phase, conclusion?, run_id, run_url, name?}`;
  inserts `ci_started`/`ci_passed`/`ci_failed`, idempotent on
  (task, run_id, resulting event type).
- `task_pr_transition` — `{task_id, to, reason?, summary?}`. Legality:
  pr_open ← ready|claimed|in_progress; awaiting_ci ← pr_open;
  awaiting_merge ← pr_open|awaiting_ci; ready ← pr_open|awaiting_ci;
  done_locally ← pr_open|awaiting_ci|awaiting_merge. Same-status ⇒ no-op
  success. `to=done_locally` writes a `done_local` task_event (payload carries
  summary) so R3 chains; all others write `status_changed`.
- `send_delivery` — handler refactored from gmail-only to a channel switch;
  the jira branch per criterion 7. The `JiraSender` seam
  (`SetJiraSender`, cmd-level wiring with OPS_TOKEN_KEY, fake in tests)
  mirrors `SetGmailSender`.

**Policy** (`internal/policy/matrix.go`): `Decide` gains a `jira_comment` case —
allow `send_delivery` under kill switch + rate limit (gmail shape);
`mark_delivery_sent` stays the assisted verb (harmless-allow like gmail).
`channel_not_live` now names only `calendar` and `github_review`. Static
fallback continues to allow the new spine tools for hooksd/poller actors;
`humanOnly` is untouched (approve/send still human-gated).

**Orchestrator** (`internal/orchestrator`): `Evaluate` switch gains the six new
event types → R9/R10/R11; `Facts` gains the CI failure streak and (for R9's
merged path) nothing new — done_local chaining reuses R3. Loader SQL in
`facts.go`; rules stay pure.

**Connectors:**
- `internal/connector/jira`: `client.go` (injectable baseURL, basic auth,
  search/jql pagination, issue + comment fetch, `/myself`), `ingest.go` +
  `sink.go` (upworkcrm Sink shape), `normalize.go` (raw-only reads, thread +
  message upserts, identity reconcile, loop-closure + prefix-match hooks),
  `send.go` (`JiraSender` impl + scrub belt).
- `internal/connector/github`: `receiver.go` (HMAC verify + raw store +
  dispatch), `mapevent.go` (pure payload→intent), `poll.go` (REST state read →
  same intents), `client.go` (injectable baseURL, token auth).

## MQTT topics

None. hooksd is an HTTP listener; the jira/github pollers are cron-style
one-shots like the other connectors. Heartbeat/command topics untouched.

## Files likely to touch

- `migrations/0007_jira_github.sql` — new
- `internal/connector/jira/{client.go,ingest.go,normalize.go,sink.go,send.go}`
  (+ unit/integration tests, httptest fake) — new
- `cmd/jira-auth/main.go`, `cmd/connectors/jira/main.go` — new
- `internal/connector/github/{receiver.go,mapevent.go,poll.go,client.go}`
  (+ tests, signed-payload fixtures) — new
- `cmd/hooksd/main.go`, `cmd/connectors/github/main.go` — new
- `internal/tools/delivery.go` — draft validation (+jira_comment), send channel
  switch, `JiraSender` seam
- `internal/tools/externalref.go` — new (`link_external_ref`)
- `internal/tools/ghevents.go` — new (`record_pr_event`, `record_ci_event`)
- `internal/tools/prtransition.go` — new (`task_pr_transition`)
- `internal/tools/createtask.go` — register the four new tools
- `internal/mcpserver/schemas.go` — `link_external_ref`; draft_delivery schema text
- `internal/policy/matrix.go` (+ `matrix_test.go`) — jira_comment live
- `internal/orchestrator/{rules.go,facts.go,rules_test.go}` — R9/R10/R11 + streak fact
- `internal/worker/loop.go` — two prompt lines (branch naming, link after PR);
  confirm exact prompt location before editing
- `internal/dashboard/server.go` + templates — Send action visible for
  jira_comment deliveries (handler already generic over `send_delivery`;
  verify, likely template-only)
- `cmd/dashboard/main.go`, `cmd/opsctl/main.go` — wire `SetJiraSender`
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` — new event vocabulary, jira account
  config idiom, hooksd exposure status, GITHUB_WEBHOOK_SECRET/GITHUB_TOKEN env

## In scope / Out of scope

**In scope:** everything above — 0007, jira-auth + jira poller/normalizer, live
jira_comment channel + JiraSender + both loop-closure hooks, link_external_ref,
hooksd (HMAC receiver, code-complete + tested), github poller interim,
record_pr_event/record_ci_event/task_pr_transition, orchestrator R9–R11,
dashboard/template touch, worker-prompt lines.

**Out of scope (do not bundle):**
- **Client Jira onboarding** — operator work when credentials exist
  (`jira-auth add` per client site). Today only sspataro.atlassian.net is real.
- **Public exposure of hooksd** — deploy/operator work (ops namespace ingress or
  tunnel + webhook config with the secret on each repo). Documented, not built.
- **github_review deliveries** (PR review comments, approve/request-changes
  verdicts) — the channel stays `channel_not_live`; drafting/sending review
  words is future work. Workers opening draft PRs via gh in their worktree is
  already-free repo action, not a delivery.
- **merge_when_green automation** — per-repo flag, default manual sweep stands;
  this step only OBSERVES merges.
- **Auto tier for Jira progress comments** — Decision 1; promotion path
  documented, not shipped.
- **review_pr task auto-creation from incoming PRs** — needs triage-side
  routing decisions; today `create_task` + `link_external_ref` covers it
  manually. (CLAUDE.md names review_pr tasks; the event plumbing this step
  ships is the prerequisite — creation rules arrive with triage go-live or a
  later slice.)
- **Calendar channel**, step-10 dashboard board, autonomy promotion machinery.

## Invariants that apply

- **1 — raw-first:** the jira poller writes `raw_source_items` (issue + comment
  items) before normalize; hooksd stores every webhook delivery raw before
  mapping; the github poller stores fetched PR JSON raw before diffing. All
  three reprocessable from raw alone.
- **2 — one funnel:** jira issues/comments become normalized_threads/_messages
  (channel `jira`) — no jira-shaped side table; external_refs is linkage, not a
  task store; PR/CI state lives as task_events on existing `tasks` rows.
- **3 — everything through the executor:** link_external_ref, record_pr_event,
  record_ci_event, task_pr_transition are registered tools with unexported
  handlers; hooksd and the github poller mutate ONLY via Executor.Execute
  (actor `hooksd:github` / `github-poll:cron`); no raw_sql/raw_api exposure.
  (Connector sinks keep their existing trusted direct writes to
  raw/normalized tables, same trust level as upworkcrm/google.)
- **4 — nothing external without a delivery row:** the ONLY `JiraSender.Send`
  call site is the send_delivery handler, gated on an approved row +
  send_enabled account + policy matrix (kill switch, rate limit);
  `status='sending'` committed pre-network; a present sent_external_id refuses
  resend forever; the post-hoc prefix matcher closes the ambiguous-failure
  duplicate hole.
- **5 — own-message loop closure:** sent_external_id uses the poller's exact
  comment-id format; the jira sink confirms on outbound re-ingest
  (`confirmed_at` + `delivery_confirmed`, first-match guard) and own comments
  are direction=outbound, invisible to triage's inbound-only filter — never
  re-triaged.
- **6 — stealth attribution:** jira comment bodies scrubbed at draft time and
  again in the send adapter (`ScrubAIAttribution` belt); terse register is the
  draft worker's existing prompt contract; nothing in the payload names an AI.
- **7 — orchestrator purity:** R9/R10/R11 are pure (event, Facts, Config)
  functions — the CI streak is a Fact the loader computes in SQL; hooksd/poller
  translate provider payloads BEFORE the spine so no provider JSON ever reaches
  a rule; every decision writes audit + record_orchestration rows.

## Sibling patterns to copy

- **Connector shape:** `internal/connector/upworkcrm/{ingest.go,sink.go,
  normalize.go}` — Sink interface, upsertRaw hash decision, cursor-on-success,
  ReconcileIdentities (no auto-merge), normalize-from-raw-only.
- **HTTP client + fake:** `internal/connector/google/gmail.go` +
  `fake_google_test.go` — injectable baseURL, httptest fake idiom.
- **Token storage:** `internal/connector/google/oauth.go` +
  `cmd/google-auth/main.go` — pgp_sym_encrypt/decrypt with OPS_TOKEN_KEY,
  identity-verify-before-store, add/list CLI shape.
- **Send seam + idempotency:** `internal/tools/delivery.go` `GmailSender` /
  `sendDelivery` two-phase shape; `internal/connector/google/send.go` for the
  adapter layout and scrub belt.
- **Loop closure:** `internal/connector/google/sink.go` `confirmDelivery`
  (id equality) and the upworkcrm 120-char prefix matcher (post-hoc fill).
- **Pure rules + Facts loader:** `internal/orchestrator/{rules.go,facts.go}`,
  dedup via `orchestrated()`; idempotent no-op transitions in
  `internal/tools/close.go` / `donelocal.go`.
- **Queue-claim style spine daemons:** `cmd/orchestratord/main.go` for advisory
  locks and executor wiring; `cmd/fleetd` for a long-running spine process
  shape (hooksd is HTTP, not MQTT — do NOT reuse `fleet.NewMirrorClient`, and
  it needs no broker at all).

## Verification protocol

1. `go test ./...` — HMAC verify pos/neg, MapEvent table tests, R9/R10/R11 pure
   tests (incl. streak ×2 → ready), matrix jira_comment live +
   calendar/github_review still denied, jira client pagination/cursor/hash
   decisions, send adapter against httptest fake (auth header, plain-text body,
   scrub, sent_external_id format), tool idempotency (record_ci_event dup run,
   task_pr_transition same-status no-op).
2. `make integration` (serialized, cleanup-pact): jira raw→normalize→triage-sees
   walk; delivery walk draft→approve→send(fake)→re-ingest→confirmed + no new
   task; failed-send prefix-match post-hoc fill; hooksd signed-payload POST →
   raw stored → events → orchestrator drain walks the PR machine end to end.
3. Manual smoke (real, today):
   - `eval "$(grep '^export JIRA_TOKEN_PERSONAL=' ~/.bashrc)"`;
     `JIRA_API_TOKEN=$JIRA_TOKEN_PERSONAL go run ./cmd/jira-auth add
     --site https://sspataro.atlassian.net --email sspataro@gmail.com
     --projects CRM`; run `cmd/connectors/jira`; `psql -h 192.168.50.49 -U ops
     -d ops -c "select external_id from raw_source_items ... limit 5"` shows
     CRM issues/comments; rerun is incremental.
   - Comment out: create a scratch CRM issue, a scratch task +
     `draft_delivery` (jira_comment, target_ref jira:sspataro.atlassian.net:CRM-N),
     approve in the dashboard, send — comment visible in Jira; next poll sets
     `confirmed_at`. Delete the scratch issue/comment after.
   - GitHub: open a scratch draft PR on sspataro57/switchboard
     (`task-{N}-smoke` branch), `link_external_ref` via opsctl,
     `GITHUB_TOKEN=$(gh auth token) go run ./cmd/connectors/github` — task hits
     pr_open, then awaiting_ci/awaiting_merge as Actions run; close the PR
     unmerged → task back to ready. Clean up the branch/PR.
   - hooksd locally: `go run ./cmd/hooksd` + curl a fixture payload signed with
     the test secret → 202 and the same event path (proves the receiver without
     exposure).
4. Not a commit gate: real webhook exposure (ingress/tunnel + repo webhook
   config) is the go-live check for push-based delivery — poller covers until
   then.

## Decisions made unilaterally (rationale inline)

1. **All jira_comment sends require human approval this step**, despite the
   matrix table's initial "auto" for progress comments. The auto tier needs a
   machine-checkable progress-vs-final/status-bearing distinction (a delivery
   `kind`) that nothing produces yet; starting every category at the strictest
   tier is the "autonomy is EARNED" posture, strictly safer, and reversible —
   promotion is a `kind` field + one matrix branch when execution workers
   actually emit progress comments. Flagged here precisely because it deviates
   from the table's initial value.
2. **Jira account config squeezed into existing columns** — `domain_default` =
   site base URL, `scopes` = project keys to poll, token in
   `refresh_token_encrypted` (pgcrypto idiom). No new columns; "scopes" as
   what-we-read is semantically honest for Jira. Scoping by project keys is
   mandatory: an unscoped poll of sspataro.atlassian.net would ingest the SWT
   build tracker into the product funnel (the vocabulary split forbids that).
3. **REST v2 everywhere (search/jql, issue GET, comment POST)** — v2 represents
   rich text as plain strings both directions; v3 forces ADF for zero gain.
   The new `/search/jql` endpoint (nextPageToken) because the legacy `/search`
   is deprecated/removed on Cloud.
4. **Per-comment raw items** (`comment:{KEY}:{id}`) rather than one raw item
   per issue carrying all comments — preserves the shipped unique
   `normalized_messages_raw_item_idx` (1 raw : 1 message) that google/upworkcrm
   upsert against, gives per-comment change detection, and avoids a risky index
   migration. The issue item itself backs the thread + description message.
5. **Jira issues normalize to threads/messages; task linkage is external_refs;
   no auto task creation** — CLAUDE.md: "Jira is a connector, never internal
   state" and triage owns interpretation. Comments-as-messages puts Jira in the
   one funnel where shadow triage already looks; `link_external_ref` +
   manual `create_task` is the honest slice until triage goes live.
6. **hooksd is a separate binary**, not a dashboard mux route — the dashboard
   sits behind OIDC; webhooks need an unauthenticated-but-HMAC surface with its
   own exposure story. Separate blast radius, separate deploy.
7. **Webhook receiver primary + gh-token REST poller interim, one shared intent
   vocabulary** — no public endpoint exists, and the smoke must be real today;
   the poller reuses MapEvent's intent structs and the same idempotent spine
   tools, so webhook go-live is config, not code. Poller scope = open
   external_refs only (no repo scanning); per-ref `sync_cursor` fingerprint.
8. **hooksd/poller write NOTHING directly** — payloads map to spine tool calls
   (record_pr_event/record_ci_event, link fallback via link_external_ref);
   status transitions belong to orchestrator rules. Keeps the
   invariant-3 grep ("no task_events writes outside internal/tools") true and
   the rules pure/testable.
9. **CI signal = `workflow_run` (webhook) / check-runs on head SHA (poller)**,
   requested/in_progress ⇒ ci_started, completed ⇒ ci_passed|ci_failed —
   GitHub Actions is what these repos run; check_suite adds a second
   overlapping signal for nothing. Idempotency on run_id absorbs
   webhook/poller overlap.
10. **Jira idempotency shape: commit `sending` pre-call, external id post-call**
    — Jira assigns the comment id, so gmail's pre-chosen-id trick is
    impossible; `sending`-without-id refuses resend, and the prefix matcher
    (Decision 11) mechanically closes the ambiguous-failure duplicate window.
11. **Post-hoc prefix matcher for failed jira sends** — 20 lines reusing the
    shipped upwork matcher idiom, converts "operator must remember to check the
    issue" into a mechanical guard. Cheap, in scope.
12. **`external_ref`s uniqueness = (task_id, system, external_key)** with a
    non-unique (system, external_key) lookup index — a parent and child may
    legitimately reference the same issue; matching picks the newest active
    link.
13. **pr_closed (unmerged) ⇒ ready + log** — the work isn't done and isn't
    awaiting anything; ready re-enters the queue, the log says why. (blocked
    would need a dependency; needs_feedback would fake a feedback_request.)
14. **`ScrubAIAttribution` stays in the google package** — moving it to a
    neutral package is churn with no behavior change; noted as a candidate
    refactor if a third channel needs it after this step.

No open questions — every ambiguity was resolvable from CLAUDE.md, the shipped
code, and the verified environment facts; the deviations worth flagging are
Decisions 1 and 3 above, both documented and reversible.

## Future work (not this step)

- Delivery `kind` (progress|final) + matrix auto tier for Jira progress
  comments (Decision 1's promotion path).
- github_review channel go-live (review comments approve → promote;
  approve/request-changes) on the same seam pattern.
- review_pr task creation rules for incoming PRs on client repos.
- hooksd public exposure (ingress/tunnel + per-repo webhook config) and
  retiring the github poller to a catch-up role.
- merge_when_green per-repo automation over the awaiting_merge lane.
- Jira issue→task auto-attach when triage exits shadow mode
  (find_related_tasks over external_refs).
