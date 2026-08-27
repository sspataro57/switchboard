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

### Exact text comparison across a provider round trip
**Location:** `internal/connector/jira/sink.go` `matchByBodyPrefix`, found SWT-16
Post-hoc delivery matchers recognize our own message by its opening text. Jira's
compared `left(body,120)` RAW: the text we stored against the text Jira handed
back after re-serializing it. A provider may change line endings, trailing
spaces, or blank-line runs without changing the message, and any such change made
the match fail — **permanently**, because nothing retries a comparison that is
already exact, and the row then stays unclaimable with `sent_external_id` NULL
forever. Since SWT-16 that has a second cost: capture sees an outbound message no
delivery claims and logs `outbound_observed` — a false claim that switchboard's
own comment was sent by hand.
Fix: compare `textmatch.NormalizedPrefix` (whitespace-collapsed, rune-truncated).
**One spelling only** — the rule lives in `internal/textmatch`. Do NOT re-spell it
in SQL: Postgres's POSIX `\s` does not cover the unicode spaces Go's
`strings.Fields` does, so an NBSP alone makes the two disagree, silently. This is
the SWT-13 canonicalization landmine in a second costume.
**There are FOUR post-hoc body matchers; a fix to this rule must reach all four**
(naming them because SWT-16 named only three and upwork was missed for five
weeks): `google/sink.go` `confirmDeliveryByBodyPrefix`, `jira/sink.go`
`matchByBodyPrefix`, `slackweb/sink.go` `confirmDelivery`, `upworkcrm/sink.go`
`confirmUpworkDelivery` — plus capture's preview. Upwork's was the OLDEST and
shipped comparing raw bytes even though its own SPEC (08-draft-deliveries,
criteria 8) said "whitespace-normalized"; the test that shipped with it seeded
one constant on BOTH sides of the comparison, so raw and normalized passed
identically. **A matcher test whose two bodies are the same string tests
nothing.**
Since SWT-18 the rule is **mechanically enforced**: `internal/textmatch/
callsites_test.go` scans `internal/connector/*/sink.go` and fails any file that
stamps `sent_external_id`/`confirmed_at` without calling
`textmatch.NormalizedPrefix`. It is a plain unit test (no build tag, no db) and
would have failed on 2026-07-31, the day SWT-16 left upwork behind. Deliberately
NOT mechanized: the time floor — its correct spelling differs per channel, so a
source scan would certify the very no-op described below.

**The four matchers agree on the comparison and on NOTHING else. Read the
sibling before copying it.** Scope: google joins on `d.from_account_id` (against
the raw item's `source_account_id`), jira on `target_ref=`, slackweb on
`target_ref=`, upwork on `upworkcrm.SameConversation` in Go (client + room, see
the SWT-19 entry above; was a client-wide `LIKE` until SWT-18 and a plain
`target_ref=` until SWT-19).
Status set: `('sending','sent','failed')` / `('sending','sent')` / `('sent')`.
Multi-match: google, slackweb and (since SWT-18) upwork REFUSE; jira keeps
newest-wins as a documented carry-over. Refusing is the reversible choice — two
unconfirmed rows can still be confirmed later, while one wrong stamp burns the
external id under `deliveries_sent_external_idx` and locks the correct row out
permanently. **Cost of refusing, worth knowing before you copy it:** a refusal is
invisible unless something flags it. slackweb has `slackweb/reconcile.go` and
upworkcrm has `upworkcrm/reconcile.go` (SWT-19; 6 passes, not 3 — see that
entry). **google and jira still have no reconciler**, so a refusal in those two
is SILENT: the rows sit unconfirmed and nothing surfaces them.

### An actor-prefix check is a transport label, not a trust boundary
**Location:** `internal/tools/delivery.go`, found by adversarial review 2026-08-27
`executor.ViaMCP(ctx)` tests whether the actor string starts with `mcp:`. That is
useful for "this TRANSPORT may only make this state transition" — its original
SWT-12 purpose — and useless as a security gate, because plenty of autonomous
callers are not on that transport. **The counter-example is in this repo:** the
drafts worker calls the executor directly as `drafts:gpt`, so a `ViaMCP` gate
does nothing for the one component that would act automatically. `opsctl` and any
direct caller are the same.
SWT-19 shipped a go-live gate built on it and recorded that the gate "cannot be
crossed by forgetting about it". It could. The fix was to close the channel for
EVERY actor, and the test pins six actor shapes rather than one — because the
defect was precisely that the check keyed on the caller.
**Rule: if a restriction exists to stop an untrusted or automated caller, do not
key it on the actor prefix.** Either deny the capability entirely until the real
binding exists, or gate on something unforgeable. And when writing a test for
such a gate, enumerate the actor shapes that exist in the repo — `dashboard:`,
`opsctl:`, `mcp:worker:`, `mcp:manual:`, `drafts:gpt`, bare `worker:` — because
one of them is usually the hole.

### Failing a delivery that R8 already processed corrupts the task lifecycle
**Location:** `internal/tools/delivery.go` `markDeliveryFailed`, found by
adversarial re-review 2026-08-27
`delivery_sent` drives orchestrator R8: the work task flips to `delivered`, its
Deliver task is CLOSED, and an orchestration row is recorded so R8 never runs
again for it. **`delivery_failed` has no orchestrator rule.** So flipping a row to
`failed` AFTER it reached `sent` leaves a real non-delivery permanently recorded
as delivered, with the Deliver task shut and the draft worker never picking it
up — no error anywhere.
This is why `mark_delivery_failed` is `slack_reply`-only. Slack wedges at
`sending`, where `delivery_sent` never fired and R8 never ran, so failing it
contradicts nothing. An `upwork_chat` row has no `sending` phase at all and is
therefore always past R8 by the time it is stuck. SWT-19 extended the verb to
upwork and had to revert it in the same ticket.
**Rule: before allowing a status transition backwards, check which orchestrator
rules the forward transition already fired.** A verb that "only moves a row away
from the world" is still unsafe if something else acted on the row getting there.
Recovery needs a compensating transition (reopen the work and its Deliver task),
which is SWT-20.

### An alarm whose fire-once marker is never cleared goes permanently silent
**Location:** `internal/connector/upworkcrm/reconcile.go` + `markDeliverySent`,
same review
Both reconcilers guard against re-flagging by writing a marker string into
`deliveries.error` and skipping rows that already contain it. That guard is only
correct if every path that starts a NEW attempt clears the column.
`send_delivery`'s success paths do (`error=NULL`); `mark_delivery_sent` did not —
so a row flagged, failed, re-approved and re-sent kept the marker forever and
could never be flagged again. The alarm was permanently silent for exactly the
delivery it had already caught once.
**Rule: a fire-once marker stored in mutable state needs a re-arm on every path
that creates a new attempt.** When adding such a path, grep for the marker.

### Upwork thread keys: one spelling, two shapes, and what the scope actually is (SWT-19)
**Location:** `internal/connector/upworkcrm/threadkey.go`
Since SWT-19 the key has two shapes and the difference is a PARSE, not a guess:
`upwork_crm:{client}:room:{room_id}` (roomed) and `upwork_crm:{client}:{channel}`
(unroomed, byte-identical to the pre-SWT-19 key). Segment COUNT separates them,
which no room id can forge — keying on the third segment's contents instead would
have been the SWT-13 magic-literal landmine again.
`ThreadKey` / `ParseThreadKey` / `SameConversation` / `ClientThreadPrefix` are the
ONLY spelling. **No SQL anywhere may build or pick apart this key** — a structural
test (`keyspelling_test.go`) fails any `LIKE`/`split_part`/`||` in the same string
as the provider literal anywhere under `internal/`, and it caught two real
instances during SWT-19's own implementation. **What it does and does not see:**
raw string literals are scanned wherever they appear — including a backtick-quoted
example inside a comment, which is how it caught both — while `//` comment LINES
are skipped deliberately, so prose may quote the old spelling to explain why it is
gone. A tab-indented code block in a comment is therefore invisible to it. Do not
describe it as catching every mention in a comment; it catches backticked ones. Filter by client with
`ClientThreadPrefix` passed as a BIND PARAMETER. The matcher's client and room
scoping is entirely in Go for this reason: "any roomed key of this client" is not
an equality.
**Describe the scope honestly, in these words:** *room-scoped for API-era traffic
in both directions, client-wide for pre-2026-07-21 history.* Not "room matching"
flat — SWT-18 called its change that and was wrong on production data, and this
one is still conditional on the source having supplied a room. 576 outbound rows
have no room in either column and all share one legacy thread per client; for
those, the multi-match refusal is the only thing between an ambiguous body and a
wrong bind.
**The re-key is a re-normalize, not a migration** (`--full --all`): `raw_json`
already carries both room columns, and only roomed rows move. Verified read-only
against production before review — the real normalizer over all 2,442 rows gave
433 roomed / 2,009 legacy, matching an independent SQL COALESCE exactly, with
zero normalize failures and zero keys its own parser could not read.
**Do not assert production counts as frozen literals.** That corpus is live —
it moved from 2,441/432 to 2,442/433 during one afternoon's work. Assert the
normalizer's output against the same corpus measured at verification time by an
independent computation; a literal cries wolf every day a message arrives.

### "Exact room matching" on Upwork is not room matching (SWT-18, corrected by review)
**Location:** `internal/connector/upworkcrm/normalize.go:99`, `sink.go`
SWT-18 replaced upwork's client-wide `target_ref LIKE` with `target_ref =
thread_key` and described it — in the commit, the code comment, the diagnosis and
this file — as scoping the match to one room. **It does not, on real data.**
`thread_key` is `upwork_crm:{client_id}:{communications.channel}` and `channel`
is the constant `'upwork'` for every row in the source db (1,650 rows, 26
clients, one distinct value; every `normalized_threads` key in ops ends `:upwork`).
So the equality selects exactly the candidate set the `LIKE` did, and the thing
actually preventing a wrong-row bind today is the MULTI-MATCH REFUSAL.
The real room id lives in `communications` — and in TWO columns, which the
normalizer did not read at the time. **CLOSED by SWT-19**, which reads both and
keys the thread on them; see the SWT-19 entry above for what the scope now is.
Left here because the LESSON outlived the defect.
**The general lesson, which is the same one three entries above:** a predicate
whose discriminating column is a constant in production is a no-op that passes
any test willing to fabricate values for it — SWT-18's `RoomDiscrimination` test
proves room scoping using `chat` and `room-b`, channel values the source has
never emitted. Verify a claimed data path against the DATA, not only the writes.
Related: with exact matching a non-canonical `target_ref` is permanently
unconfirmable where the `LIKE` was forgiving, and `draft_delivery` used to
validate only that an upwork `target_ref` was non-empty, with no
canonicalization — the SWT-13 landmine's fourth instance. **CLOSED by SWT-19**:
`validateDraftDelivery` now parses it with `upworkcrm.ParseThreadKey` and the
handler stores the canonical spelling, exactly as `slack_reply` does.
Note the review's hypothesis was WRONG in a useful way: it guessed the mismatch
came from `ScrubAIAttribution` running at send but not at store. It doesn't —
`draft_delivery` and `update_delivery` both store scrubbed bodies and the scrub is
idempotent, so stored body == sent body. Verify a claimed data path at every
write site before acting on it.

### A post-hoc matcher without an attempt-time floor binds the wrong send
**Location:** `internal/connector/jira/sink.go` `matchByBodyPrefix`, found SWT-16
slackweb got this floor in SWT-12 (see 0012's third defect); **jira never did**,
and the gap survived until an adversarial pass went looking. Without
`send_attempted_at - interval '2 minutes' <= message.SentAt`, a delivery
re-approved and re-sent AFTER a comment exists is still a match candidate — and
because the matcher takes the newest candidate, the fresh retry WINS over the row
that actually produced the comment. Result: `sent_external_id` records a real send
against the wrong external object, the retry's own comment id is lost forever, and
the correct row stays unclaimable. Verified by mutation: removing the floor makes
the in-flight retry get stamped with the older comment's id.
The two-minute allowance is not decoration — `send_attempted_at` is Postgres
`now()` while the message instant comes from the provider's clock, and a strict
comparison would turn a second of skew into a PERMANENT refusal.
**Rule: any post-hoc matcher that identifies our own message by CONTENT needs a
lower time bound.** Content matching alone cannot tell two identical sends apart.

### The attempt-time floor is INERT on the assisted tier
**Location:** `internal/tools/delivery.go` `markDeliverySent`, found SWT-18
`send_attempted_at` is written by exactly two places, both inside `send_delivery`
(gmail `delivery.go:513`, slack `delivery.go:941`), plus migration 0012's one-shot
backfill. `send_delivery` is policy-DENIED for `upwork_chat` (`matrix.go:120-125`,
`channel_assisted`) and `prefill_delivery` refuses any non-`slack_reply` channel,
so the only verb that moves an upwork row to `sent` is `mark_delivery_sent` — and
it writes `status`/`sent_at` only. **Every `upwork_chat` row created since 0012
has `send_attempted_at` NULL, forever.** Consequence: pasting the sibling clause
`(send_attempted_at IS NULL OR send_attempted_at - interval '2 minutes' <= $2)`
into an assisted-tier matcher yields a clause that is ALWAYS TRUE — a no-op that
turns a repro test green (the fixture seeds the column; production never does)
while production behaviour is unchanged. slackweb's `sink.go:210-219` says this
out loud for its own assisted rows; upwork is assisted in its entirety.
Use google's `COALESCE(send_attempted_at, sent_at)` spelling instead — but note
`sent_at` on an assisted row is the instant a HUMAN clicked "mark sent", which is
legitimately hours after the message, so the 2-minute skew allowance is wrong
here in the opposite direction and would create PERMANENT refusals.
**Rule: before adding a time floor, check that some code path actually writes the
column for that channel.** Verify a claimed data path at every write site.

### `communications` has TWO room columns; reading one is reading none
**Location:** `upwork_crm.communications`, found 2026-08-26 while investigating
SWT-19's Q1
`upwork_room_id` is the room a message was **observed** in; `send_room_id` is the
room a send was **dispatched** to. They are **disjoint per row** and the **same
identifier space** (`room_<hex>`; 6 values appear in both). `send_room_id` is
written by exactly one path — it agrees with `send_requested_at` on all 136 rows,
zero disagreements.
Consequence: outbound traffic looks unroomed if you count `upwork_room_id` alone,
because our own sends record the room in the sibling column. The correct source
is `COALESCE(upwork_room_id, send_room_id)`, and the difference is not marginal:

```
API-era outbound, upwork_room_id only    84/188   44.7%
API-era outbound, COALESCE both         186/188   98.9%
```

This measurement error was made, reported, and used to invert a SPEC's central
rule before being caught — so: **when a column looks empty for a subset of rows,
list the table's columns before concluding the data is missing.** `\d
communications` costs one command. It is the fourth costume of the same mistake
(see the three entries above: an inert time floor, a constant discriminator, and
a stats payload that cannot discriminate).
Note also that `~/WebstormProjects/crm` — recorded above as the CRM repo — **does
not exist on this workstation**, so the writer code cannot be read here and the
semantics above were established from the data alone.

### One upworkcrm invocation writes TWO sync_runs rows
**Location:** `internal/connector/upworkcrm/ingest.go:70` and
`normalize.go:148`, found while speccing SWT-19
`Ingest` and `Normalize` each call `StartRun`/`FinishRun`, and
`cmd/connectors/upworkcrm` runs both, so a single CronJob execution leaves TWO
rows with `status='ok'`. Verified against production: `sync_runs` for the upwork
account arrive in pairs, one pair per `*/15` tick.
Consequence: anything that counts completed passes as a proxy for "the poller has
looked since X" — the shape slackweb's reconciler uses, where the threshold is 3
passes — counts twice as fast here. A threshold copied across connectors fires
after 1.5 real runs instead of 3.
**Do NOT try to tell the two run kinds apart by their `stats` payload.** Both
marshal the same struct, so the discriminating keys are present-and-zero in the
run that did not populate them rather than absent — which is SWT-18's "the
discriminating column is a constant" mistake in a third costume. If the two kinds
must be distinguished, add a column that says so.

### A dashboard page that looks empty may be the wrong page
**Location:** `internal/dashboard/auth.go`, bit 2026-08-02 (fixed same day)
Login used to discard the requested path and always land on `/deliveries` — a
default from SWT-8, when that was the only page. So opening `/sources` without a
live session went `/sources` → `/dev/login` → `/deliveries`, and the near-empty
deliveries table read as "ingestion is broken" rather than "I am not on the page
I asked for". It cost real debugging time before anyone checked the browser tab
title.
Fixed: `Require()` carries the path through login (`?next=`), OIDC carries it in
the OAuth state parameter, and `safeNext` refuses anything that is not a single
in-app path — an absolute URL, a protocol-relative `//host`, a backslash or a
CR/LF. A login endpoint is the classic place an open redirect is exploited,
because the victim is mid-authentication and expecting to be sent somewhere.
**Rule: when a dashboard page looks empty, check the page title before checking
the data.**

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
  already-applied migration. The runner (`cmd/tools/migrate`) keys on
  `schema_migrations.version` ONLY — there is no checksum — so an edited
  already-applied file is skipped **silently** and the file diverges from the
  schema with no error anywhere. That invisibility is the whole reason for the
  rule. (Editing a migration that is still unmerged and applied only to a
  throwaway local db is fine, provided you make the local schema match by hand.)
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
  **The dashboard IS deployed since 2026-07-31** (`deployment/dashboard` +
  `service/dashboard`, ops namespace, image tag `0.2.0`, manifest
  `kube/switchboard/dashboard.yaml`) — switchboard's first long-running
  workload; everything else is still one-shot CronJobs. It is deliberately
  NOT exposed by an Ingress: with `OIDC_ISSUER` unset the dashboard falls back
  to a dev-login stub that hands a session to anyone who reaches `/dev/login`,
  and the dashboard performs approvals and sends. Reach it with
  `kubectl -n ops port-forward svc/dashboard 8085:80`; the Ingress block in the
  manifest is commented out until OIDC is configured.
  Still not deployed: orchestrator, triage, drafts, fleetd, hooksd.
- **The production db drifted five migrations behind main (bit 2026-07-31).**
  `schema_migrations` was at 0009 while main was at 0014: 0010 (calendar reset),
  0011/0012 (slack send promotion + attempts) and 0013 (task_events indexes) had
  all shipped in code and merged, but nothing had ever applied them to pg-main.
  Nothing broke only because the CronJobs run a pinned older image. **Deploying a
  new image without migrating first would have failed at runtime**, on columns
  the new code assumes. Applied 0010-0014 on 2026-07-31.
  Lesson: merging a migration is not applying it. There is no automatic migrate
  step in the deploy path — check
  `psql -h 192.168.50.49 -U ops -d ops -tAc "SELECT max(version) FROM schema_migrations"`
  against `ls migrations/` before pushing an image, or add a migrate Job to the
  rollout (the image already ships `/migrations` and the migrate binary for
  exactly this).
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
  `done_local`, `child_created`, `released`, `delivery_confirmed`,
  `outbound_observed`. The NOTIFY trigger is step 5's.
- `outbound_observed` (SWT-16) is informational ONLY: a message sent outside
  switchboard (Gmail app, Jira web UI, Slack on a phone) logged on every task
  that has a `deliveries` row on the same thread. Payload: `{message_id,
  external_message_id, channel, thread_key, sent_at, sender, body_preview}`;
  the dedup key is `payload->>'message_id'` (= `normalized_messages.id`, stable
  across re-normalization — `external_message_id` can be absent). That dedup is
  **structurally enforced**, not advisory: `task_events_outbound_observed_uniq`
  (0013) is a PARTIAL unique index on
  `(task_id, (payload->>'message_id')) WHERE event_type='outbound_observed'`.
  Any future writer of this event MUST repeat that predicate in its
  `ON CONFLICT (task_id, (payload->>'message_id')) WHERE
  event_type='outbound_observed' DO NOTHING` — arbiter inference matches a
  partial index only when the predicate is restated, and omitting it raises
  "no unique or exclusion constraint matching the ON CONFLICT specification" at
  runtime. Corollary: one observation per (task, message) FOREVER; a
  re-observe-after-edit feature would need a new key or a new event type.
  No status change, no delivery row, no orchestrator rule. Coverage equals correspondence:
  a task with no delivery on the thread has no stored thread↔task link and gets
  nothing — widening that is triage-live attach, not a heuristic.
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
- **Three historical commits DO carry the trailer, and stay that way by decision**
  (Salvador, 2026-07-31): `f0ab2cd` on `main` (the squash of PR #1, twice in one
  message), `7c4a5e7` on the dead `slackweb-http-bridge` branch, and `c2a5cde` on
  `runbook-cluster-split` (open PR #2). Do NOT "fix" these and do not propose it
  again. Reasons: `f0ab2cd` is 16 commits back, so a rewrite re-SHAs it and every
  commit after it and needs a force-push to eight remote refs including an open
  PR — and it still would not remove the trailer, because GitHub keeps PR #1's
  page showing the original merge commit and message regardless of what `main`
  says. Only deleting the PR removes that, at the cost of its review history.
  A partial clean for that price is not worth it. The forward rule is unchanged:
  never write a new one.
- **Four commits carry a `Claude-Session:` trailer, and they stay too** (Salvador,
  2026-08-26): `c83202f`, `b522208`, `bb6b0f7` and `ae37ec5` — the SWT-17 SPEC,
  the SWT-18 fix, its merge, and the SWT-18 deploy handoff. Same class of
  violation as the three above (CLAUDE.md: "no AI references in commits — ever")
  and caught by the go-reviewer pass, not by a hook. Decision: leave them and
  record it. They are only five commits deep with no open PR, so a rewrite is
  cheaper here than the `f0ab2cd` case — but rewriting re-SHAs the merge commit
  and the two SHAs already quoted in `docs/runbooks/HANDOFF-kube-swt18.md` and in
  the SWT-18 Jira comments, and buys nothing an honest record does not.
  **Nothing appends this trailer** — no hook, nothing in `.claude/settings.json`;
  it was typed into the `git commit` messages by the session. Forward rule
  unchanged, and it is worth stating in the form that would have caught it: the
  ban covers ANY AI reference, not just `Co-Authored-By`. Check the message you
  are about to write, not just the trailer block.
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
