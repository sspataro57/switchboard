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

- **`cmd/classify` reads `DATABASE_URL`, not `OPS_DATABASE_URL`.** The shell
  exports only `OPS_DATABASE_URL`; run it as
  `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/classify ...` (same pattern as
  the google cmds). Bit on 2026-08-30: the first eval run exited 1 with
  `classify: connect: DATABASE_URL is not set` after 0 messages.

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
Recovery needs a compensating transition (reopen the work and its Deliver task) —
FUTURE work: SWT-20 shipped provenance and the shortlist and explicitly deferred
it (its "Future work" section; it needs the R8 lifecycle analysis).

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

### The landmine's 6th and 7th instances, both inside SWT-21 itself
**Location:** `internal/drafts/store.go`, found by review, twice, in one ticket.

**(6) A guard whose column no query selected.** `drafts.Run` was wired to the
locality boundary and `internal/drafts/locality_skip_test.go` asserted that a
`local_only` project's Deliver task is skipped. It passed from the day it was
written while the guard was INERT: `DeliverTasks` never selected
`p.ai_locality`, so nothing outside test fixtures ever wrote
`DeliverTask.ProjectLocalOnly`. Every real task folded to `ClassGeneral` and
would have reached the hosted model.
**The unit test could not have caught it, by construction — the unit test is the
thing supplying the value.** Only a test that makes POSTGRES produce it can. That
is the rule now: for any predicate whose input comes from a column, the
regression test belongs in the integration suite, and it must fail when the
column is dropped from the SELECT. Mutate the SELECT to a literal and watch it go
red; if it stays green you have tested your fixture.

**(7) Fixing it introduced the mirror image: a value that is a constant in
production for a STRUCTURAL reason.** The neighbour fold classified thread
messages by their `capture_decisions` row — but `internal/capture/rules_store.go`
filters `direction = 'inbound'` (that line IS invariant 5), so an **outbound
message can never have a decision, on any pass, in any mode**. Measured: 21,194
outbound messages, ZERO decisions, and 1,043 of 18,089 threads already carrying
one. So `unseen` on an outbound message does not mean "not classified yet", it
means "restricted forever" — and since a Deliver task exists to REPLY on a
thread, the first send re-entering through ingestion (invariant 5 again) would
have blocked that thread permanently, with no error and no remedy.
**Rule: before treating "absent row" as "not yet known", check whether some other
invariant guarantees the row can never exist.** Absent-because-pending and
absent-because-impossible need different handling, and the second one is
invisible in every fixture that only contains the first.

**Both were invisible for the same reason: the fixtures were shaped like the
assertion, not like production.** Every thread fixture in the repo contained
inbound messages only. When a fold or a filter reads a column, seed the fixture
from a `SELECT ... GROUP BY` of the real table first.

**Also from this ticket: a comment can be a defect.** Two comments shipped
stating the OPPOSITE of their code — the `Prober` doc ("treated as ready" when
`router.probe` returns `AvailUnreachable`) and the outbound-exclusion rationale
(claimed our sends passed the delivery policy gate: 1 delivery row against 21,194
outbound messages; `direction='outbound'` only means the From address is one of
the five own accounts). In a boundary file, the comment is what the next session
trusts. Verify the claim in a comment the same way you verify a predicate.

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

### slackweb `status='ok'` and `conversations_seen` are not coverage
**Location:** leaf `slackconnector/src/slack/slack-web-adapter.ts`
`collectDmConversations` / `listConversationsOnPage`, plus
`internal/connector/slackweb/ingest.go:58-85`. Bit 2026-09-10/11 (SWT-39,
`docs/bugs/slackweb-collab-export-stale_DIAGNOSIS.md`).

**Which conversations get exported.** The leaf exports only the conversations it
can scrape from the Slack UI in that run: the Home sidebar plus the `/dms` view's
virtual list. switchboard sends no cursor or conversation list (`/export` has an
empty body) and marks every run `ok` whatever subset arrives.

**How much that is.** Measured 2026-09-11:
- Collaboratory (542): 6–8 of 38 known conversations per run.
- Avviato (539): 16 of 45.
- Occasional "wide" runs (33–43 conversations, 12–16 min) sweep in messages
  that 340+ `ok` runs never saw, in BOTH workspaces.

**What that means.** A message lands only when its conversation happens to be
scraped. That is why messages arrive hours or days late while the runs read
`ok`, `raw_inserted=0`.

**Consequences:**
- A zero-insert streak is not evidence that nothing happened.
- `ReconcileUnconfirmed` counting `ok` runs as "passes that could have observed"
  a send is false for any conversation outside the scraped set.
- The bridge logs at `info`. Enumeration lines are `debug` and carry counts
  only, so the mini log cannot tell you which conversations a run covered.

**Rule:** before reasoning from slackweb `sync_runs`, check which conversations
the run actually exported. Last-written `raw_source_items` rows per run window
plus `messages_seen` arithmetic is the only record until coverage telemetry
ships.

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


### Residue lane (SWT-23)

- `classify` has TWO lanes: `--lane personal` (worker_type `classify`) and
  `--lane residue` (worker_type `classify_residue`). The worker_type values
  differ because both inbox filters key their NOT EXISTS on it — one shared
  value would make a message classified by one lane permanently invisible to
  the other after a capture rule claims it.
- The residue inbox is latest-decision `unmatched`, which CANNOT join
  `projects`: 0015's CHECK makes `(action='unmatched') = (project_id IS NULL)`
  a schema fact. Every query the personal lane uses inner-joins projects and
  therefore returns exactly zero residue rows without erroring.
- `projects.ai_classify` (0018): a WORKLOAD flag — "mail attributed here gets
  an actionability verdict from the personal lane". ai_locality remains the
  boundary; the personal filter keeps BOTH clauses. `bulk` is local_only +
  ai_classify=false. Fixtures that INSERT projects without naming ai_classify
  get false and their suites start SKIPPING, not failing (0016's ai_locality
  trap, again).
- COST: the measured per-message median is **7.2 s** through the real prompt
  path (runbook table). NEVER size a pass with the 0.25 s warm benchmark — the
  two differ by 25-29x, and the wrong one turned a 29.5-GPU-hour sweep into a
  "60-minute" estimate. `--since` is required on residue runs for this reason.
- A bare-name sender (no `@`) is never gmail: google writes the raw From
  header (connector/google/rfc822.go, normalize.go), while slackweb writes
  message.Author and upworkcrm the CRM sender column — both display names, so
  those rows are Slack/Upwork WORK sitting unmatched (measured 2026-08-31:
  1,287, all channel='upwork').

### Inquiry lane (SWT-33)

- `classify` has THREE lanes: `personal` (worker_type `classify`), `residue`
  (`classify_residue`), `inquiry` (`classify_inquiry`), and TWO output
  contracts — actionability, shared by personal and residue (the guard is
  `LanePersonal.Contract == LaneResidue.Contract`), and needs-reply for inquiry.
  Distinct worker_types because every inbox keys its NOT EXISTS on it.
- Three project columns, three questions: `ai_locality` is the BOUNDARY (where a
  message may go); `ai_classify` (0018) opts mail into the personal lane's
  actionability question; `ai_inquiry` (0024) opts a project's inbound client
  conversation into the inquiry lane. Only `collaboratory` is armed.
- The inquiry lane's routed class is PINNED to `ClassRestricted`
  (`classify.routedClass`). Without it the lane is a silent no-op:
  collaboratory is `ai_locality='any'`, so ClassOf returns `ClassGeneral`,
  cmd/classify's router has a nil general client, and every message skips as
  `no_general_provider` in a pass that exits 0 and reads as an empty inbox. Its
  filter has NO ai_locality clause for the same reason (it would return zero
  rows). Never add a hosted client to make skips go away.
- "Still open" is a READ-TIME fold in `classify.Summarize` (a later
  `direction='outbound'` message on the verdict's thread), never written back.
  `thread_scope` is `thread` | `conversation` | `none`; `conversation` (an
  unthreaded Slack channel or DM) is a weaker claim with its own counter.
  Measured 2026-09-10: 113 thread-exact Slack keys vs 83 conversation-level, one
  conversation-level key holding 9,704 messages. The rooted/unrooted rule has
  ONE spelling: `slackweb.IsRootedThreadKey`.
- The gmail half of the fold is near-inert for collaboratory: 1 outbound among
  447 messages on its gmail threads (30 days to 2026-09-10) — replies do not land
  on the same thread. A gmail `open` means little until that changes.
- The advisory lock `0x5157_0022` is SHARED by all three lanes and
  `classify run` exits 1 when it loses it: never schedule two lanes in the same
  minute — chain them in one command or stagger them.
- `classify eval` refuses to print a ratio below `classify.EvalResultThreshold`
  (120) SCORED labels, on EVERY lane — counts plus `EvalIndicativeMarker`,
  enforced inside `Eval` (a caller-set flag was bypassable). The measured
  lanes' own files (280, 874) sit above it. The labels are Salvador's
  judgement alone (runbook, "Labelling protocol").

### Queue read tools over MCP (SWT-35)

- `task_list(project, status?, assignee_type?, subproject?, limit?)` and
  `project_list` are read-only, MCP-listed executor tools (not humanOnly, not
  snapshotGated — `mail_search`'s shape). `task_list` orders by `taskQueueOrder`
  (getnext.go), the ONE spelling shared with `task_get_next`.
- Its default status set is `in_play` = everything except `closed` AND
  `delivered` — deliberately unlike the dashboard board, which hides only
  `closed` (rows in a model context cost tokens). `inPlayPredicate` is the one
  spelling, also behind `project_list`'s `in_play` count. Rows never carry a
  body; `task_context` is the per-task read.
- The MCP surface is NOT scoped by client: cross-client privacy is not a goal
  (Salvador, 2026-09-10), and by his decision the same day `local_only`
  projects are listed like any other. A worker console's actor is
  `mcp:{client}` / `mcp:{client}.{sub}` (opsworker sets OPS_WORKER_ID to the
  bare client) — `mcp:worker:{client}` exists only in tests.
- Production has SIX `ai_locality='local_only'` projects (bulk, homelab,
  personal, foundry, saka, town-ai) although the migrations set only two: the
  column's `local_only` default catches every hand-created project. Measure the
  table, never count from the migrations.
- **LANDMINE: omitting a variable from an MCP install does not unset it.** A
  stdio MCP server inherits the environment of the shell that launched `claude`
  (this repo's ops-mcp carries `OPS_TOKEN_KEY`, `JIRA_TOKEN_PERSONAL`,
  `OPENAI_API_KEY` in `/proc/<pid>/environ`; `.mcp.json` sets none), and
  `~/.bashrc` exports them. Never treat "we didn't pass the secret" as a
  boundary — gate in the binary.
- Hence a separate narrow binary. SWT-35 shipped it READ-ONLY as
  `cmd/ops-mcp-read`; SWT-37 RENAMED it `cmd/ops-mcp-user`
  (`mcpserver.NewWithProfile(…, ProfileUser)`) and it is no longer read-only:
  it lists/accepts `project_list`, `task_list`, `task_get_next` PLUS
  `task_dismiss`, `task_close`, `task_mark_delivered` (see "Task verbs over
  MCP") and, since SWT-38, `create_task`, `task_append_log`,
  `task_set_priority` (nine tools; see "Task capture over MCP") and, since
  SWT-42, `mail_list_attachments`, `mail_read_attachment` (eleven; see "Mail
  attachments over MCP") and, since SWT-44, `draft_delivery`, `update_delivery`
  (thirteen; see "Gmail drafting over MCP"). Its main
  still calls no `tools.Set*` seam, so NO sender is wired
  (connector code is linked via internal/tools but stays nil). Not an env
  setting on ops-mcp: that was tried and fails open (unset had to mean full for
  existing launchers). `ProfileRead` survives only as the fail-closed floor an
  unknown profile lands on. `task_context` is in neither narrow profile: the
  claim holder's fetch flips claimed → in_progress.
- Installed once at Claude Code USER scope as `ops` → `ops-mcp-user` (runbook
  `docs/runbooks/ops-mcp-user-scope.md`, which also carries the migration from
  the old `ops-mcp-read` registration): a `go install` binary from `main`,
  `OPS_WORKER_ID=manual:salvo`. Never install `ops-mcp` (full) at user scope.
  Re-run `go install ./cmd/ops-mcp-user` after any merge touching
  `cmd/ops-mcp-user`, `internal/mcpserver`, `internal/tools` or
  `internal/policy`, then open a NEW session. In this repo
  `.mcp.json`'s project-scope `ops` (full) shadows it. Installed 2026-09-10:
  `DATABASE_URL='${OPS_DATABASE_URL}'` DOES expand in a user-scope entry
  (Connected on first `claude mcp get ops`); no literal DSN needed.
- "swb" = switchboard (Salvador's shorthand, 2026-09-10): taught by
  `mcpserver.Instructions` (sent at initialize → session system prompt) and
  both queue-tool descriptions. "list swb projects" → project_list, "swb
  queue" → task_list. The server NAME stays `ops` (the precedence trick needs
  the same name as `.mcp.json`'s).
- **LANDMINE: `claude mcp get/list` lie about same-name precedence.** Inside
  this repo they show the user-scope `ops`, yet a session here loads
  `.mcp.json`'s full `ops` (26 tools since SWT-44; a session in `kube` gets 13).
  Verify precedence from inside a session, never from the CLI listing.
- `claude -p` from a shell uses `ANTHROPIC_API_KEY` (exported, no credit) over
  the claude.ai login: prefix `env -u ANTHROPIC_API_KEY` for smoke sessions.
- `task_list` reads resolve + page + counts in ONE `RepeatableRead, ReadOnly`
  transaction: a shared WHERE stops predicate drift, not drift in time.
- Residual (Future work): row TITLES — some derived from private mail — reach
  whatever model the calling session runs, including for `personal`; and in
  the full profile `task_context` still returns any task's body by id with no
  client or locality clause.

### Task verbs over MCP (SWT-37, mcp-task-verbs)

- **Owner decision, 2026-09-10:** `task_dismiss`, `task_close` and
  `task_mark_delivered` are callable from EVERY repo's Claude Code session
  (the user-scope `ops-mcp-user`) and from this repo's full `ops`. Accepted
  risk: untrusted text read in any session (mail, Slack, a web page) can tell
  it to dismiss/close/deliver any task in any project, and policy sees
  `mcp:manual:salvo`, a human. Nothing is sent and these three verbs touch no
  delivery (since SWT-44 the same sessions also draft gmail replies — see
  "Gmail drafting over MCP"); recovery is `task_reopen` (and, once SWT-36 ships, a dismissal reopens on
  the next inbound message routed to it). The Instructions' "only when Salvador asks" line is a
  prompt rule, not a boundary.
- The user profile SWT-37 shipped had six tools; SWT-38 made it NINE
  (`create_task`, `task_append_log`, `task_set_priority` — see "Task capture
  over MCP (SWT-38)"), and the Instructions' closing rule now covers "these
  write tools", not "these three".
- **`mcp_human_only` (policy rule, `mcpHumanOnly` map):** deny `task_close`
  / `task_mark_delivered` iff the actor carries the MCP prefix AND is not
  human. It is a TRANSPORT rule, not a trust boundary: it keeps worker
  consoles (`mcp:{client}`) off these two verbs and changes NOTHING for
  in-process callers (orchestrator R2/R8, `ticketstatus:jira`), whose
  decision stays `allow / static-default` byte for byte. It cannot be
  `humanOnly` — the spine calls both verbs. `task_dismiss` stays `humanOnly`
  (its only gate for workers now that it is MCP-listed). "Arrived over MCP"
  has ONE spelling: `policy.ViaMCPActor`, which `executor.ViaMCP` calls.
- **Pre-existing, not closed here:** a worker console runs with
  `--dangerously-skip-permissions` and inherits `DATABASE_URL`, so it can shell
  out to opsctl (`opsctl:$USER` counts as human) or psql. Every actor gate is
  bypassable by a worker; recorded under the SPEC's Future work.
- **Dismissal provenance:** `task_dismissals.dismissed_by` keeps the raw actor.
  `dashboard:…` = Salvador picked the code; `mcp:…` = a model mapped his words
  to a code. A precision/eval pass can split on `dismissed_by LIKE 'mcp:%'`.
- **MCP audit rows carry a NULL `task_id`** (the adapter sets only Tool, Actor,
  Args) — pre-existing.
- **LANDMINE (integration tests):** `audit_events.task_id` has no cascade, and
  the tools cleanup deletes audit rows only by `itest-mcp-tools-` actor. A test
  that writes `mcp:…` actors must delete its `policy_decisions` + `audit_events`
  by `task_id` BEFORE `cleanupToolsData`, or the next run's task delete fails
  the FK. And use `queueMatrixExecutor` (real matrix): `newExecutor` is
  static-only and allows every worker call.
- `closeTransition` refuses only `claimed`, `in_progress` and `needs_feedback`
  — so `task_close` also closes `pr_open` / `awaiting_ci` / `awaiting_merge`
  work, whatever the `openStatuses` comment in close.go implies.
- The full profile's `Instructions` (incl. the swb verb lines) reach worker
  consoles too; a worker acting on one costs a denied audit row.
- `drafts.DeliverTasks` drafts only for a parent still in `done_locally`
  (SWT-37 Q1 = b): a parent closed or marked delivered by hand leaves its
  `Deliver #N` child open but no longer drafted. That filter is only a READ
  before a model call, so the write re-checks under the task row lock:
  `draft_delivery` refuses a `closed` task for every caller, and refuses when
  the caller's `expect_task_status` (the drafts worker sends `done_locally`)
  no longer holds (Codex review).
- **Closed work never gets a delivery approved or sent** (`refuseClosedTask`,
  first in approve / every send / calendar book): lock order is task →
  delivery everywhere. `delivered` is deliberately NOT refused — R8 marks a
  task delivered after its FIRST send and a sibling delivery must still go
  out; a stale draft on a hand-delivered task stays behind human approval.
  `prefill_delivery` runs the same guard (it fills a real Slack composer).
  Known residual: a send already committed to `sending` still goes out if the
  task is hand-marked DELIVERED during its network call (needs a
  delivery-set model to fix; Future work in the SWT-37 SPEC).
- **`task_close` / `task_dismiss` refuse while a delivery's send is LIVE**
  (closeTransition): send phase 1 dispatches after its tx ends, so a close in
  that gap would let words reach a client for closed work. Live = `sending`,
  unsettled (`send_settled_at IS NULL`) and started within `sendAttemptLease`
  (15m; clock `COALESCE(send_attempted_at, updated_at)`, Jira stamps only
  `updated_at`). Past the lease a crashed gmail/Jira/calendar phase 1 (no
  settle path) stops blocking. The refusal carries "refusing to close active
  work" — the reconciler's non-fatal skip marker (`activeWorkRefusal`) — so a
  live send never aborts a reconciliation pass.
- `prefill_delivery` runs the Slack bridge call INSIDE its tx while holding the
  task SHARE lock, so a close/mark-delivered/draft on THAT task waits for the
  bridge (bounded by the caller's timeout: 60s dashboard, 30s opsctl). A wait
  on one task, not a deadlock.
- **Worker ids are validated at launch** (`worker.ValidateWorkerID`, called by
  `WriteMCPConfig`): an id that would read as human (`manual:`/`dashboard:`/
  `opsctl:`) or carries `mcp:` is refused. Before this, `opsworker --client
  manual:foo` passed every human gate as `mcp:manual:foo`.

### Task capture over MCP (SWT-38, mcp-task-capture)

- The user profile (`ops-mcp-user`, every other repo's session) gained
  `create_task`, `task_append_log` and the new `task_set_priority`: a session
  logs the work Salvador hands it as a swb task, writes progress on it, closes
  it with `task_close`, and reorders any task. No claim or run power; since
  SWT-44 it drafts gmail replies but never approves or sends one (see "Gmail
  drafting over MCP").
- **`assignee_type` is a ROUTING field, not "who types".** `human` = Salvador's
  lane (no worker console routes it — `getNext` selects only
  `assignee_type='claude'`); `claude` = the console queue for the project's
  client, where a running console would claim it. A session's own work is
  `human` + `ready` for its whole life (never claimed: `task_close` refuses
  claimed/in_progress).
- **Profile pins** (`mcpserver.userProfilePins`, set by `NewWithProfile` from
  the profile alone, no env input): the user profile force-sets
  `require_assignee_type:"human"` on `create_task` and `task_append_log`,
  AFTER `injectWorkerID`, by OVERWRITE. The field is hidden from every schema
  and only narrows; the full profile has no pins. Enforcement is in the
  executor path: `validateCreateTask` refuses a differing assignee (default
  human counts), and `appendLog` checks the task's assignee under a `FOR SHARE`
  lock in the same tx as the insert. `audit_events.args` carrying
  `require_assignee_type` is the user-scope marker — the actor
  (`mcp:manual:salvo`) cannot tell this repo's session from another repo's.
- **Why log is pinned too:** `task_context` returns the last 50 events'
  payloads, and a worker console feeds that into `claude -p
  --dangerously-skip-permissions`. A log line on a `claude` task is text inside
  a future worker prompt.
- **The priority scale 0..3** — normal, elevated, high, urgent; higher runs
  first (`taskQueueOrder`) — is spelled ONCE in `internal/tools/priority.go`
  (`PriorityMin`, `PriorityMax`, `PriorityLevels`). `create_task` and
  `task_set_priority` range-check against it; `create_child_task` and plan
  import do not yet (Future work). The MCP schema's min/max are literals pinned
  equal to the consts by `TestTaskSetPrioritySchema`.
- `task_set_priority` writes a `priority_changed {from,to,reason}` event (none
  when the value is unchanged — no-op success); it never touches status, a
  claim or `plan_order`. The orchestrator fires nothing on it or on `log`
  (pinned by `TestEvaluate_CaptureEventsFireNothing`).
- `task_set_priority` is `humanOnly` (rule `human_only`), NOT `mcpHumanOnly`:
  no spine caller writes priority after creation, so the orchestrator is
  refused too. A future rule that must set priority (triage escalation) has to
  MOVE it to `mcpHumanOnly` deliberately. `create_task` / `task_append_log`
  stay ungated in policy (`static-default`): the pin, not policy, is the gate.
- `create_task` writes NO creation `task_events` row, and the adapter passes no
  `Call.TaskID`: a session-created task's origin is only in `audit_events.args`
  (project + title), not on its own page.

### Mail attachments over MCP (SWT-42, mail-attachments)

- **Attachments ARE stored** — inside `raw_source_items.raw_json.rfc822_b64`
  (the whole RFC822 message, IMAP path) for any message up to the capture cap
  (`MAIL_MAX_MESSAGE_BYTES`, default 1 MiB). NormalizeRFC822 skips them, so
  `mail_read_thread` shows only the text body; a session that concludes "the
  attachment isn't stored" is wrong. Over the cap the row is `truncated` and
  keeps a `parts` manifest (names, types, ENCODED sizes, no bytes). gmail:-shaped
  rows (API/bridge) carry no bytes at all.
- `mail_list_attachments` (message ids, a thread, or a sender/subject finder) and
  `mail_read_attachment` (index | filename | part_id; text inline ≤100 KiB
  per page with `offset`, else `to_file` → `<UserCacheDir>/switchboard/attachments/<raw>/<idx>-<name>`,
  0600, swept after 7 days). Both are in BOTH profiles (owner decision O1) — the
  user profile was then eleven tools, full 25 (thirteen and 26 since SWT-44).
  Read-only, not humanOnly, not
  snapshotGated: audit row only, never the content. Part numbering is
  `pathString`, the same numbering `planOversizeFetch` writes into a manifest.
- **Before SWT-42 the mail tools had NO locality gate** — `mail_search` /
  `mail_read_thread` return local_only mail bodies to any caller. That residual
  is still open for those two; the attachment tools are gated in the HANDLER
  (every caller, every profile) by the SWT-21 rule: latest capture_decisions row
  per message, `ClassOf(state, local_only)`, outbound folded over its thread's
  inbound (`MostRestrictive`).
- **O2 clean-mailbox rule (owner, 2026-09-12):** unfiled inbound is general iff
  the RECEIVING raw row's `source_account_id` has ≥20 filed inbound messages and
  none filed local_only (latest decision per message), computed per call. Prod on
  the day: 1009 handsonconnect clean (566 filed / 0 local); 1003 and 1004 are
  not. SWT-40's routing supersedes it.
- Private refusals: an explicit id errors naming the reason (by raw id when the
  caller gave one — never the private Message-ID); the thread form and the
  finder classify from HEADERS first and only COUNT restricted matches: private
  mail is never loaded or MIME-walked. The finder is literal-substring
  (`likeEscape`), ≤2,000 candidates, ≤64 MiB of raw rows, `truncated` on
  either cap.
- The saved-file cache refuses a symlinked base and sweeps only through
  `os.Root`. Known gaps (Future work): the listed-part count is unbounded
  (bounded only by the 1 MiB row); `truncatedReason` prints the reader's cap,
  not the connector's; a truncated manifest omits the text/plain leaf kept as
  the body, so a named .txt on an HTML-only oversize message is not listed.
- Attachment content is untrusted third-party text; the Instructions line says
  read-as-data. Accepted risk as in SWT-37: a session that reads a malicious
  attachment still holds the write verbs — and, since SWT-44, gmail drafting
  (never approving or sending).

### Gmail drafting over MCP (SWT-44, user-profile-drafts)

- **The tools.** The user profile (`ops-mcp-user`, every other repo's session)
  lists `draft_delivery` and `update_delivery` (thirteen tools);
  `update_delivery` is also MCP-listed in the full profile (26). It stays
  `humanOnly`. A session writes a client email reply as a `drafted` delivery row
  and fixes its words; Salvador approves and sends on the dashboard.
- **Gmail only.** Pin `draft_delivery: {require_channel: "gmail"}`
  (`mcpserver.userProfilePins`, SWT-38 C4 pattern: OVERWRITE after
  `injectWorkerID`, hidden from every schema, only narrows).
  `validateDraftDelivery` REFUSES a differing channel, first, and never rewrites
  it. No Slack, Upwork, Jira or calendar row from a session that reads
  untrusted text.
- **"Own" drafts = the actor's, gmail only.** Pins `update_delivery:
  {require_own_draft: "true", require_channel: "gmail"}`: the handler locks the
  row (FOR UPDATE) and refuses unless `deliveries.created_by =
  executor.ActorFrom(ctx)` AND `channel = 'gmail'`. The actor is `mcp:` +
  OPS_WORKER_ID, and the user-scope install and this repo's full-profile `ops`
  both run as `manual:salvo` — so the pin means drafts created by the
  mcp:manual:salvo actor (any interactive session), gmail only; never the
  drafts worker's (`drafts:gpt`) or the dashboard's. It CANNOT tell one session
  from another; the channel pin (second review round) is what stops a
  user-scope session rewriting a full-profile slack_reply / jira_comment /
  upwork_chat / calendar draft. The full profile, dashboard and opsctl send no
  pin and edit any draft. A present body that is empty or whitespace (before or
  after the attribution scrub) is refused; `subject: ""` still clears the
  subject.
- **Same project (owner decision, Salvador 2026-09-12: "Same project").** Pin
  `draft_delivery: {require_thread_in_task_project: "true"}`: a user-profile
  gmail draft is allowed only on a thread already filed under the task's
  project. `refuseThreadOutsideTaskProject`, inside draftDelivery's tx after the
  task lock and before the insert, accepts (a) `thread_id =
  tasks.source_thread_id`, or (b) the thread's LATEST INBOUND message — the
  reply's recipient, picked by `latestInboundMessage`, the same helper
  `ResolveGmailRoute` uses (`ORDER BY sent_at DESC, id DESC`), so the rule and
  the send agree on the message — has a LATEST capture_decisions row (`ORDER BY
  id DESC LIMIT 1`, any mode) with `project_id = tasks.project_id`. The rule
  follows the recipient: an older message filed here does NOT qualify a thread
  whose newest inbound mail is filed elsewhere or unmatched. Outbound messages
  never count. Refusal: "thread N is not filed under this task's project
  (<slug>): its latest inbound message is filed elsewhere or not at all; ask
  Salvador to file it, or draft from the switchboard session" (not "the
  dashboard": it cannot file mail; not "a capture rule": a new rule does not
  re-file already-decided mail). Full profile and drafts worker: no pin,
  unchanged. Fixture note: `capture_decisions_live_uniq` allows ONE live row
  per message — a test that re-points a message writes the later decisions as
  shadow.
  - **What it guarantees.** A session can `create_task` in ANY project, so the
    rule means "the draft's thread is filed under the task's project" — it
    ties draft to thread, and is NOT a limit on which project a session can
    draft into.
  - **Shadow decisions count.** The latest decision in ANY mode, per the repo's
    latest-decision convention (classify/store.go, mailattach.go). That is
    also what lets a shadow `--all` re-filing pass qualify a thread.
  - **Accepted residual race (Codex).** The check and the insert are not
    serialized against a concurrent capture pass re-filing the thread (or a
    new inbound message landing). The window is one transaction; nothing sends
    without Salvador's dashboard approve; the dashboard shows From/To; and
    SWT-46 will persist the recipient at draft/approval.
- **Content-bound approval.** `approve_delivery` takes an optional
  `expect_content_hash` = `tools.DeliveryContentHash(subject, body)` (lowercase
  hex sha256 of subject, NUL, body; a NULL subject is `""`), compared under the
  delivery FOR UPDATE lock; a mismatch refuses ("changed since it was shown to
  you") and the row stays drafted. The dashboard renders the hash into the
  Approve form (hidden `content_hash`, computed in `listDeliveries`);
  `approveAction` passes it through, built with `json.Marshal`, and REFUSES a
  POST without one ("reload the page and review it again") before the executor
  — a stale pre-deploy page or a crafted POST cannot approve unbound. The tool
  keeps it optional for opsctl and full-profile MCP: it stays human-only, and
  the dashboard is the review surface. Not in any MCP schema.
- **The dashboard shows From/To before approval.** `tools.ResolveGmailRoute`
  is the ONE spelling of where a gmail send goes (From account, To = the
  thread's latest inbound sender, In-Reply-To, provider thread, thread subject):
  `send_delivery` phase 1 builds its message from it, and `listDeliveries` shows
  it on drafted/approved/failed gmail rows (a sent row is not recomputed: a
  newer inbound would misstate where it went). Other channels show
  `target_ref`; anything unresolvable reads `(unresolved)`. The session picks
  the thread, so the recipient is the SESSION's choice among ingested threads —
  bounded, on the user profile, by the same-project pin (the thread's latest
  inbound message must be filed under the task's project) — and the dashboard,
  not the resolution, is the check.
- **Known gap — the To can change before Send (NOT fixed here; SWT-46).** Send re-resolves To from the thread's latest inbound
  message AT SEND TIME (pre-existing behaviour), so the To shown can change if
  a new inbound message arrives before Send, and an approved row re-renders its
  To up to Send. The fix is persisting the route at approval; it folds into the
  cc/reply-all ticket, which must store explicit recipients anyway.
- **Approve and send stay off the profile.** A session must not approve its
  own client email: that is the human gate the policy matrix puts on
  client-facing mail (invariant 4), and these sessions read untrusted text
  (SWT-42 attachments). The user binary wires no sender anyway.
- **The actor path.** `ops-mcp-user`'s `OPS_WORKER_ID` must be `manual:*`
  (the runbook's `manual:salvo`). `draft_delivery` is not policy-gated, so any
  id drafts, but `update_delivery` is `humanOnly`: a worker-shaped id (`salvo`,
  `acme`) gets `human_only` and cannot fix even its own draft.
  `worker.ValidateWorkerID` guards opsworker's ids, not this binary's env.
- **Recovery for a planted draft:** Deny it on the dashboard (SWT-43; a plain
  Deny, not Redo), or edit it. Either way it never sends unapproved.
- **R8 caveat — recorded for a follow-up ticket (SWT-47),
  NOT fixed here.** R8 fires on
  `delivery_sent` for the delivery's TASK: `task_mark_delivered` refuses a task
  that is not `done_locally`, the engine logs that and continues, and the
  `record_orchestration` delivery_lifecycle dedup key lands anyway — so a LATER
  real delivery on that task is deduped into silence (the
  `internal/orchestrator/rules.go:275-282` hazard SWT-28 fenced for calendar
  only). Before SWT-44 drafts came from the drafts worker on done_locally
  parents; now a session drafting on its own `human`/`ready` work task is the
  COMMON path, and the first send of such a draft writes the dedup key while
  the task is still `ready`.

### The pipeline wake-ups (SWT-40 Part E, inquiry-promote)

- **Only capture is on cron.** Connector mains call `pipeline.AnnounceCaptured` after the error check on
  `capture.EvaluateRules`, which publishes `ops/pipeline/captured` iff the pass committed ≥1 decision.
  `cmd/pipelined` (one Deployment, replicas 1, Recreate) runs one `StageLoop` per stage named in
  `PIPELINE_STAGES`. Runbook: `docs/runbooks/pipeline.md`; kube rows: `docs/runbooks/HANDOFF-kube-inquiry-promote.md`.
- **A wake-up, never work (E-D1).** Postgres is the queue of record; a stage re-queries its own inbox
  whatever woke it. Lost wake = latency up to one 5 min sweep; duplicate = one empty query. Never branch
  on `Wake.Counts`/`MaxID`.
- **Why not `task_events` (E-D2):** `task_events.task_id` is NOT NULL, so a message with no task cannot be
  an orchestrator event, and a second message-level event table + LISTEN loop would be a second
  orchestrator. Message-level stages are direct MQTT; the task boundary stays the orchestrator's R-rules.
  The stage graph is the static `pipeline.Subscribers` table (invariant 7 discipline).
- **LANDMINE: `ops/pipeline/*` is QoS 1 and NEVER retained** (a retained wake re-fires on every reconnect;
  retained state is global on the prod broker). `structure_test.go` scans for it, lexically.
- **Client ids.** A stage uses `switchboard-pipeline-{stage}` with a dead LWT on
  `ops/workers/pipeline.{stage}/status`; the daemon `switchboard-pipelined` / `pipeline.daemon`. A
  connector announce uses `switchboard-capture-{connector}-{random}`: the google CronJob and its IMAP
  IDLE watcher can overlap, and a shared id kicks the other off the broker.
- **`dead` means "not running".** A clean DISCONNECT suppresses the will, so pipelined publishes the dead
  payload itself on shutdown (`fleet.Client.PublishDead`, the one deliberate path around
  `Status.Marshal`'s refusal), after every loop and heartbeat goroutine has returned.
- **Loop rules:** one catch-up pass at start; a burst of wakes coalesces to one run; a full pass repeats up
  to `MaxDrainPasses` (50); a lost advisory lock retries ONCE after 30 s, then waits for the sweep; a pass
  is cancelled after `PassTimeout` (15 min), and one that ignores it for 90 s more ends Run with
  `ErrPassWedged` (pipelined exits non-zero). A stage pass MUST honour ctx and count as processed only
  rows that left its inbox. Heartbeats publish from their own goroutine (a broker outage never stalls
  passes); the queue drops its OLDEST state when full.
- `fleet.newClient` now disconnects on a connect that timed out (a late CONNACK used to leave an unowned
  client reconnecting forever), and `PublishStatus` waits at most 10 s for its ack.
- **E5** (`classify promote --lane`) ships with Part C.

### The capture-time assignee gate (SWT-40 Part D, inquiry-promote)

- **Capture never calls Jira.** A jira-keyed match whose rule's project has `ticket_assignee_gate` (read
  from the column in `loadRules`) is recorded `held`: project, rule and key named, nothing created, in
  shadow and live alike. Capture runs in every connector main, and an LHH link arrives via slackweb and
  google too. The lookup credential (`OPS_TOKEN_KEY` plus the stored token) lives only in connector-jira
  and pipelined. A capture-time GET would spread the secret everywhere, or silently skip the check where
  it is absent. Runbook: `docs/runbooks/ticket-status-sync.md` "Capture-time assignee gate".
- **The resolution is a SECOND row, `mode='gate'`** (`capture.RunGate`, the pipelined `gate` stage,
  lock `0x5157_0015`). The live claim is spent by `held`, which acted on nothing, so the gate row is the
  message's one action. Actions: `task` / `task_log`, via capture's own helpers as `capture:gate`, or
  `attributed`, with the reconciler's drop reason or `gate_unverified_expired`. An unreadable hold
  writes NOTHING and stays held (`pending_lookup`). The gate row is the latest decision, so every
  `ORDER BY id DESC` reader follows it.
- **LANDMINE, the third partial unique index on `capture_decisions.message_id`.**
  `capture_decisions_gate_uniq ... WHERE mode='gate'` sits next to the live one, and every `ON CONFLICT`
  must restate its predicate. `gate_structure_test.go` scans `internal/` and `cmd/` for it.
- **`pendingMessages` excludes gate-resolved messages in EVERY mode, `--all` included.** Otherwise the
  documented shadow `--all` re-pointing pass writes newer rows that bury the resolution for every reader.
- **One predicate, one fetch path.** The gate calls `ticketstatus.Warranted` (extracted from `Decide`)
  and `ticketstatus.EnsureSnapshots` (extracted from `Run`), so it cannot create a task the reconciler
  closes 15 min later, and the two share the stored snapshot cache (TTL 1h). `gate.go` may not name
  `LookupIssues`, `RouteLookup`, the TTL reader or the delivered-status matcher. `ticket_delivered_statuses`
  may only be read in `ticketstatus/store.go` and opsctl (SWT-34 criterion 22), so the gate gets the set
  through `ticketstatus.DeliveredStatusesByProject`.
- **Rate:** at most 50 distinct keys looked up per pass; holds on other keys are skipped untouched and
  counted `GateStats.BudgetSkipped`. pipelined's processed count is `GateStats.Resolved`, never pending
  holds: a re-counted pending hold would make the stage loop re-run at once, faster than the sweep.
  Expiry is strictly after 72h from when the HOLD was written (the held row's `created_at`, not the
  message's `sent_at`), and the inbox includes expired holds so they resolve.
- **Freshness rule (review fix 1).** A stored snapshot decides a hold only if `Snapshot.VerifiedAt >=`
  the message's first-seen time (`normalized_messages.created_at`, which no upsert rewrites). The gate
  passes `ticketstatus.Config.MinFresh` (per key, the newest held message's first-seen time), which
  forces a fetch past the TTL; the reconciler passes nil, so its TTL logic is untouched. A failed
  forced fetch leaves the hold pending until expiry, never decided from the older snapshot.
  **LANDMINE: `ingested_at` does NOT move on an unchanged refetch** (`upsertRaw`'s hash short-circuit).
  So `VerifiedAt` is `ingested_at` OR the DB clock at the start of this call's successful GET (from
  `jira.Stats.FetchedKeys`, in-memory, `json:"-"`). Comparing `ingested_at` alone starves every mention of
  an unchanged ticket until it expires. Keys routed to no lookup account are never force-fetched, so
  their holds wait for the poller to store a newer copy. Mutations: drop `MinFresh` → the D-D6 test goes red;
  drop the `VerifiedAt` check → the Jira-down test goes red.
- **Fetch, then lock (review fix 3).** `RunGate` probes `0x5157_0015` (busy → `ErrGateLockHeld`, no GET),
  releases it, fetches via `EnsureSnapshots`, THEN takes the lock, re-reads the inbox and decides.
  Capture's lock is never held across Jira HTTP. Token-built clients (`jira.TokenClientFactory`) carry a
  30 s `http.Client` timeout, `LookupRequestTimeout`, which also covers the connector-jira poller.
- **One decide step (review fix 2).** `decideGateHolds` (budget, freshness, per-message ref re-query,
  `DecideGate`) is shared by `RunGate` and `DryRunGate`. `opsctl capture-rules gate --dry-run [--shadow]`
  prints `message=… key=… outcome=…` from the stored snapshots only (no fetch, no lock, no writes).
  `--shadow` reads the latest shadow `held` rows and is refused without `--dry-run`.
- **Claim, act, complete (review round 2, fix 1).** Gate `task` AND `task_log` rows are both claimed with
  `task_id` NULL before any executor call, and `recordDecisionTask` fills `task_id` only after the calls
  succeed (for `task_log`: `task_append_log`, then the guarded reopen). Migration 0029's
  `capture_decisions_gate_task_pin` therefore pins only `attributed ⇒ task_id NULL`. The report's
  "claimed with no task" WARNING counts live/gate `task` and gate `task_log` rows with NULL `task_id`: a
  pass that died after the claim. For a `task_log`, the log may or may not have landed; the reason text
  names the target task. Mutation: claim `task_log` with `task_id` set → the failing-append test
  (`gate_scope_integration_test.go`) goes red.
- **Tenant scope (review round 2, fix 2).** The gate sets `ticketstatus.Config.ScopeToRoute`. A stored row
  counts as a key's snapshot only if it came from the account `RouteLookup` routes the key to (by prefix
  scope). For a key no lookup account claims, it falls back to the one `provider='jira'` poller account
  storing it. An ambiguous route, or two storing pollers, gives no snapshot: pending, then fail-closed
  expiry. The reconciler leaves the flag false, so its Count > 1 ambiguity refusal is unchanged.
  `Snapshot.SourceAccountID` is the storing row's `source_account_id`. Mutation: drop the filter → the
  two-snapshot test goes pending and the lone-foreign test creates a task.
- **`VerifiedAt` for a key fetched in this call is the fetch START** (`clock_timestamp()` before the GET),
  unconditionally, and only for the row the fetching account stored (round 2, fix 3). It is never
  max(ingested_at, start): the response describes the ticket as of the request.
- **FOLLOW-UP (not fixed): `external_refs` dedup is not tenant-qualified (follow-up SWT-49) either.** `taskForExternalRef`
  and the unique key are `(system, external_key)`, so two Jira sites sharing a prefix would share one ref.
  This is latent today because the sites use different prefixes.
- **FOLLOW-UP (not fixed): unchanged-content verification lives only in memory.** `VerifiedAt` from a GET
  that returned unchanged content is not stored (`ingested_at` does not move). So a pass that fetches and
  then loses the capture lock (`ErrGateLockHeld` on the second take) re-fetches the same keys next pass,
  up to 50 GETs. Persisting a verified-at time per stored snapshot would fix it.
- **Gate turned off with holds pending** → they resolve with the gate off (the column is read every pass):
  tasks with no assignee check.
- **Deploy consequence:** until the connector images carrying Part D are deployed, OLD capture binaries
  keep creating tasks for gated projects. That is the old behaviour, not a regression. Order: 0029, then
  the connector bump, then `PIPELINE_STAGES=gate` plus `OPS_TOKEN_KEY` on pipelined. Without the key,
  every hold expires `gate_unverified_expired` (fail closed, no task). **LANDMINE: 0029 BEFORE any image
  built from main.** On a db without 0029, a new capture binary fails the action CHECK on the first gated
  match, and that stalls capture for every connector.
- The shared token-decrypting factory is `jira.TokenClientFactory(pool, key)`, which returns nil for an
  empty key. connector-jira, opsctl and pipelined all use it.
- `TestDecideGate_BodyIsPure` slices from `func DecideGate(` to the next `\nfunc `, so the next
  function's doc comment counts as "body". `DecideGate` is kept last in `gate.go` for that reason.

### Link preservation (SWT-25)

- `normalized_messages.links` (0017): JSONB array of `{"text","url"}`, written
  by the google normalizer from the raw text/html part. **Array position is the
  identity** — nothing may reorder it after write.
- The classifier's `link_index` is a 1-based index into that array, integer or
  null, resolved by `classify.ResolveLink` — the model never authors a URL, and
  the schema has no string field it could author one into.
- `img src` is never extracted and never followed: the only "link" in a Pines
  First Notice is the SendGrid `/wf/open` tracking pixel.
- Backfill = `go run ./cmd/connectors/google --normalize-only --all`.
  Idempotent: the upsert keys on `raw_source_item_id`, so message ids and the
  eval labels survive it.
- STANDING RULE: `body_text` must never change without checking
  `confirmDeliveryByBodyPrefix` — google has no reconciler, and a one-space
  shift leaves a delivery permanently unconfirmable with no error anywhere.

## Environment facts

- **ollama on the workstation is a systemd user service** (since 2026-08-31):
  `~/.config/systemd/user/ollama.service`, `OLLAMA_VULKAN=1` (ROCm crashes on
  this GPU), linger enabled so it survives logout. TEMPORARY until ollama moves
  in-cluster behind a MetalLB `192.168.50.0/24` address; then disable the unit.

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
  Still not deployed: triage, drafts, fleetd, hooksd. **orchestratord IS deployed**
  (SWT-41, 2026-09-12, image 0.7.9): Deployment `orchestratord` in `ops`
  (`kube/switchboard/orchestrator.yaml`, replicas 1, Recreate, liveness `/healthz`
  on :8091), started from cursor 1018 after `orchestrator_cursor_advance`.
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
  (`orchestrator_cursor`) is the sole delivery path. Missed/duplicate NOTIFYs
  are harmless.
- **LANDMINE (SWT-41): built is not deployed, and the Dockerfile build line is
  the deploy list.** orchestratord shipped in SWT-5 (2026-07-11), ran once as a
  `--once` smoke and then did not run for two months: its binary was not even in
  the image. R3 Deliver tasks, R8, R9–R11, dependency unblocking and claim expiry
  silently never happened in prod while 912 events queued. A daemon a ticket
  depends on must be in the `Dockerfile` build line (pinned by
  `cmd/orchestratord/dockerfile_test.go`), have a manifest, and have a health
  signal judged from OUTSIDE the process, or the ticket is not delivered.
- **The cursor row outlives its seed.** 0003 seeded it at max(id) ONCE, at apply
  time; any later first start drains from wherever the row is. Skipping is
  `orchestrator_cursor_advance` (humanOnly, off MCP, compare-and-set, refuses
  while an engine holds the lock, takes SHARE on task_events so no in-flight
  lower id is skipped). Downtime catch-up is the default and correct behaviour —
  never advance as a routine restart step (docs/runbooks/orchestrator.md).
- **Health = `pg_locks` (per database) + backlog age, not cursor age** — an idle
  system never moves the cursor. `/funnel` Orchestrator section, `/tasks` red
  line when not `ok`, `/healthz` for the kubelet. The engine re-checks its lock
  before every event (`DrainHooks.Guard`) and exits on loss; residual window is
  one event's actions (a DB fencing token is future work).
- **The orchestrator's package graph is pinned:** `internal/orchestrator` must not
  reach `internal/provider`, `internal/connector/*`, `internal/planimport` or
  `internal/tools`, even transitively (`deps_test.go`). Shared lock keys live in
  the import-free `internal/lockkeys`.
- Dedup idiom: `orchestrated` task_events (written via `record_orchestration`)
  are the replay-dedup keys — rules check them in Facts before firing.
- Claim-expiry sweep EXEMPTS `needs_feedback` (parked ≠ crashed; expiring
  would orphan the resume).
- Single instance via `pg_try_advisory_lock` key `0x51570005` (spelled once, in
  `internal/lockkeys`).
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
  **CORRECTED 2026-09-10 (SWT-33):** the export half is no longer true —
  `T0HPR78RX` now carries resolved direction, 2,155 inbound / 2,393 outbound
  rows, latest outbound 2026-09-09, so its `OWN_USER_IDS` entry exists and it
  exports. Load-bearing for the inquiry lane's replied-since fold, which rests
  entirely on `direction='outbound'` existing for this workspace. Whether the
  SEND narrowing and the SUSPENDED note are also stale was NOT re-checked.
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
  `update_delivery` (MCP-listed since SWT-44, still humanOnly; the user profile
  pins it to gmail drafts created by the mcp:manual:salvo actor, i.e. any
  interactive session — see "Gmail drafting over MCP"), and
  spine-facing `approve_delivery`/`send_delivery`/
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
- **`rejected` is a terminal delivery status (SWT-43, delivery-deny).** Written
  ONLY by `reject_delivery {delivery_id, note?, redraft?}` (humanOnly, off MCP;
  dashboard Deny/Redo). It means "switchboard did not and will not send this
  row". The rejectable set is `drafted`, `approved`, and `failed` with no sent
  id and no confirmation — **EXCEPT `jira_comment`**: `sendJiraComment` writes
  failed+NULL for every error, the comment may have landed, and the jira
  matcher still claims `failed` rows, so rejecting one would turn a landed
  comment into a false `outbound_observed` hand-send. **The same refusal covers
  an `approved` `jira_comment` with `error IS NOT NULL`** (SWT-43 review):
  approve accepts failed-without-id for a retry and does not clear `error`
  (only a successful send or `mark_delivery_sent` does), so a failed-then-
  approved Jira row is the same may-have-landed row. `sending`/`sent` never.
  The verdict is an `approvals` row `('delivery', id, 'rejected', actor)` plus a
  `delivery_rejected` task event that NO orchestrator rule reacts to (the
  Deliver task stays open; never infer delivery state into task state).
- **Deny/Redo are bound to the words shown, like Approve (SWT-44's hash).**
  Both reject forms carry the hidden `content_hash`
  (`tools.DeliveryContentHash`). `reject_delivery` takes an optional
  `expect_content_hash`, compared under the delivery FOR UPDATE lock; a mismatch
  refuses with "changed since it was shown to you; reload and review it again".
  The dashboard's `actionReject` refuses a POST without a hash before the
  executor, and opsctl may omit it.
- **`deliveries.redraft_requested_at` is the drafts unblock.** The drafts
  `NOT EXISTS` ignores exactly a rejected row with it set (Redo); a plain Deny
  keeps blocking; the new draft blocks again. That is the loop bound — one
  human click per re-draft — and it holds only while `reject_delivery` is the
  column's sole writer (`TestRedraftRequestedAt_OnlyInternalToolsWritesIt`).
  That scan covers `internal/` and `cmd/`, flags ANY assignment (the first cut
  listed right-hand sides and missed `= CASE ...`, the shape the Redo UPDATE
  itself uses), and has a probe, `TestRedraftWritePattern_Probe`.
  Redo needs the work task `done_locally` (the only status a draft lands on).
- **"Blocks a new draft" has ONE spelling: `tools.BlockingDeliverySQL`.** It is
  used by `drafts.DeliverTasks`' NOT EXISTS AND by `draft_delivery`'s re-check
  under the task FOR UPDATE whenever `expect_task_status` is set (the
  drafts-worker path). DeliverTasks is a read, not a claim, so without the
  re-check two drafts passes could each draft from one Redo, or from one first
  draft. The loser gets `tools.ErrDeliveryBlocksDraft`, which the worker counts
  as a skip. Plain callers (sessions, dashboard, opsctl) send no
  `expect_task_status` and still draft siblings.
- **The note reaches the redraft prompt as quoted data.** The rejected body and
  Salvador's note sit between `<<<BEGIN/END REJECTED DRAFT>>>` and
  `<<<BEGIN/END HIS REASON>>>`, and any marker copy inside them is broken up
  (`neutraliseMarkers`). They are framed as his feedback to address, not
  instructions. `SystemPrompt` is byte-unchanged. There is deliberately no
  authorization beyond humanOnly: the note is his own text, typed behind
  Keycloak.
- **Silent wait (Future work, beside D7's residual).** A Redo stays "redraft
  requested" with nothing drafting it, and nothing logged, whenever ANOTHER
  non-rejected delivery exists on the same parent task. Examples: a session
  drafted a sibling gmail reply, or an older drafted, approved or sent row sits
  beside the rejected one. Every delivery on the parent blocks the drafts queue
  except the Redo row itself, so the worker never lists the Deliver task. This
  is the same shape as D7's residual (a Redo whose Deliver task was closed by
  hand). Recovery today: deal with the sibling first (Deny it or send it), or
  draft by hand. Surfacing both cases on the dashboard is SPEC Future work.
- **Every new matcher's, reconciler's or send path's status set must exclude
  `rejected`.** Today they exclude it by allowlist, pinned by tests;
  `deliveries_rejected_unsent_check` is the schema backstop (a rejected row can
  never carry `sent_external_id` or `confirmed_at`). A hand-sent copy of a
  rejected draft is recorded as `outbound_observed` on the task, never as a
  stamp on the rejected row.

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

## Capture rules contract (shipped in SWT-17, SHADOW MODE)

Deterministic project assignment. A priority-ordered rule engine runs as a
post-normalize pass in every connector main, writes one `capture_decisions` row
per message, and in LIVE mode only creates one task per external ticket through
the executor. Runbook: `docs/runbooks/capture-rules.md`.

- Mode from `CAPTURE_RULES_MODE` (shadow default); advisory-lock key
  `0x51570015`. Shadow is real — it decides everything and creates nothing.
- **Capture runs BEFORE triage, and the ordering is load-bearing.** Triage's
  inbox is `action='unmatched'`; a routed message is never re-triaged. Reversed,
  triage spends model calls on messages the rules would have routed. The two
  mode flips are ordered, not independent: capture goes live first.
- **Three states, and conflating the first two is the trap.** No decision row =
  UNSEEN (skip, the engine has not looked yet). Latest decision `unmatched` =
  the inbox. Latest decision routed = never re-triage. Treating unseen as
  unmatched hands every fresh message to the model before the rules run.
- **The pending filter skips decisions OF THE SAME MODE, not just live ones.**
  Keyed on `cd.mode = <this pass's mode>`. It was `= 'live'`, which meant a
  shadow pass excluded nothing and re-evaluated the WHOLE inbound corpus every
  run — 49,415 messages and ~65 MB of body text per pass, four mains on `*/15`
  plus a watch loop on every IDLE wake. Do NOT "simplify" it to "any decision":
  that starves the shadow→live transition, because after a shadow period every
  message already carries a shadow row and the first live pass would decide
  nothing. Neither failure is visible at fixture scale.
- **`external_refs.system` has THREE spellings** — the CHECK in 0015,
  `captureExternalSystems` in `internal/tools/capturerules.go`, and
  `validateLinkExternalRef` in `internal/tools/prci.go`. Drift between them is
  not an error: capture creates the task, commits the decision, then fails the
  link, and the next message for that key creates a SECOND task while the live
  claim is already spent. Change all three together.
- `capture_decisions.message_id` is `ON DELETE CASCADE`. A decision without its
  message means nothing, and 19 test suites clear fixtures by deleting
  `normalized_messages` — without the cascade they fail inside cleanup, which
  reads like the cross-pollution pact breaking.
- **Triage can no longer produce an `attach` verdict from the live pass.** The
  pending filter yields only unmatched messages, unmatched carries no project,
  and candidates load only when a project resolved. An inert path that still
  looks alive — recorded so nobody debugs it as broken.
- Two tools, `capture_rule_add` / `capture_rule_set_enabled`, both `humanOnly`
  and both OFF the MCP surface: an agent must not be able to redirect the funnel.
- **`external_refs` is UNIQUE on `(system, external_key)`** — one ticket, one
  task, enforced by the database rather than assumed by the reader. Capture's
  dedup is a lookup in that table, and `link_external_ref` IS on the agent
  surface, so without the constraint a worker could claim a ticket for a task of
  its choosing and every later notification would append there, silently.
  Added at zero rows, when it was free. **Consequence: a ticket cannot be
  attached to two tasks.** No task-plus-follow-up on one key; that would need a
  deliberate design, not the absence of a constraint.

## Triage contract (shipped in SWT-6, SHADOW MODE)

- `OPENAI_API_KEY` lives in `~/.bashrc` — same non-interactive early-exit
  caveat as JIRA_TOKEN_PERSONAL: `eval "$(grep '^export OPENAI_API_KEY=' ~/.bashrc)"`.
- `TRIAGE_MODEL` default `gpt-5-mini`; advisory-lock key `0x51570006`.
- ~~Client→project mapping recipe~~ **DEAD since SWT-17.** It said
  `UPDATE projects SET client_person_id = ...`, and that column no longer exists
  (migration 0015 drops it) — the statement is now a runtime error, and it was
  the first thing a session looking for "how do I map a client to a project"
  would have found. **Project membership is a capture-rules question now:**
  `opsctl capture-rules add --project <slug> --type <kind> --pattern <pattern>`.
  Triage reads the resulting decision; it no longer maps anything itself.
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
- **LANDMINE (2026-09-12): the compose Postgres is SHARED by every worktree and agent.** Capture suites take capture's advisory lock 0x5157_0015 and delete `capture_decisions` wholesale, so two branches running integration tests at once corrupt each other ("another pass holds advisory lock", rows vanishing). Run a branch's integration suite in its own database: `psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c "CREATE DATABASE ops_<branch>"`, `make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_<branch>?sslmode=disable'`, then point `DATABASE_URL` at it. Advisory locks are per-database, so this isolates them too.
- **Known flake (SWT-48): `TestAttributionTrend_*` in internal/capture fail from 20:00 to 24:00 EDT** (local date != UTC date). They pass with `TZ=UTC`. Pre-existing on main; it is not a regression in whatever branch you are testing.
- **LANDMINE: an edited migration never reaches a DB that already applied it.** `cmd/tools/migrate` keys on `schema_migrations.version` with no checksum. Editing a numbered file in place is fine only if NO database (prod or the shared compose `ops`) has applied it yet; otherwise fix that DB by hand or rebuild it. 2026-09-12: 0029's task_id pin was edited after the compose `ops` DB had applied it and was patched by hand; prod never had the old version.

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

### Calendar & availability (SWT-24)

- **Google CalDAV v2 rejects app passwords** (measured 2026-08-31): a PROPFIND
  with a valid decrypted app password returns HTTP 401 byte-identical to a
  garbage-password control; `/user/` gives 405. Calendar read is OAuth-only —
  do not re-open CalDAV/ICS without new evidence. The consent
  (`google-auth add-calendar`) asks for `calendar.readonly` ONLY; the
  restricted Gmail scopes are what migration 0014 abandoned OAuth to avoid.
- **The readiness contract**: `availability.LoadBusy` is the ONE
  database-backed door to free/busy (`loadEvents` is unexported, and
  `internal/availability/callsites_test.go` bans a second `normalized_events`
  reader). READY = a `sync_runs` row with `status='ok'`,
  `stats->>'phase'='calendar'`, `finished_at >= now - AVAIL_MAX_SYNC_AGE`
  (default 1h) for EVERY `provider='google'` account with
  `calendar_in_availability`. Freshness of the SYNC, never the count of
  events — an empty week answers; an empty SCOPE refuses. The horizon refusal
  keys on `google.CalendarWindowPast/Future` — never re-spell those durations.
- **A google row can be dual-auth**: `auth_type='app_password'` names the MAIL
  path and survives `add-calendar`; calendar selection is credential-gated
  (`refresh_token_encrypted IS NOT NULL AND calendar.readonly = ANY(scopes)`,
  `ListCalendarCredentialedAccounts`). Gating calendar on `auth_type='oauth'`
  skips every production account forever.
- **The calendar phase writes ONE cursor key** (`SaveCursorField`,
  `calendar_sync_token`) because the resident watch loop moves `imap_folders`
  under it; a whole-blob `SaveCursor` rolls the IMAP position back and skipped
  mail is a delivery confirmation that never lands. The direct-path
  `IngestCalendar` clobbered exactly this until SWT-24.
- The phase runs only under `MAIL_SOURCE=imap` (`calendarPhaseRuns`) — bridge
  and gmail_api ingest calendar inline, and two passes race for the same
  cursor key. `--calendar-only` = calendar phase + normalize, nothing else.

### Delivery provenance & the upwork shortlist (SWT-20)

- **Provenance is a task column written by observers**:
  `tasks.source_thread_id` (FK to normalized_threads), written ONLY by
  `task_set_source_thread` — a spine tool registered on the executor and
  deliberately absent from `internal/mcpserver/schemas.go` (the
  `capture_rule_add` shape). It is NOT humanOnly: the capture engine is its
  main caller. `external_refs` was rejected as the provenance store for three
  reasons that keep coming back: `link_external_ref` is agent-facing free
  text, the join key is a mutable thread_key (re-keyed once already), and
  UNIQUE (system, external_key) allows one task per conversation forever.
- **The upwork target is the recorded conversation, verbatim.**
  `internal/drafts` resolves it through `store.TaskSourceThread` (the ONE
  reader; a second spelling of the provenance query is how drafts and
  draft_delivery come to disagree about a task's conversation). No provenance
  → the unresolvable-tell-the-human path. The SWT-19 multi-room refusal
  survives as that fallback, now firing for the stronger reason.
- **The draft_delivery binding is unconditional; only room CHOICE is
  actor-keyed.** Same client as provenance holds for every actor including
  drafts:gpt; picking a different room of the bound client needs
  `policy.HumanActor` (exported in SWT-20 — ONE definition, shared with
  Decide's human_only gate; never restate the prefixes; `mcp:manual:` is a
  human, one transport prefix stripped).
- **The shortlist rule**: any SQL predicate on delivery identity must be
  IMPLIED BY `SameConversation`, never a restatement of it. Client equality
  is the only such predicate (`deliveries.target_client_ref`, written in Go by
  the same ParseThreadKey call that produced target_ref); the room clause is
  the rule itself and stays in Go. `deliveries_upwork_identity_check` is what
  makes the shortlist sound — a row missing its identity is refused at INSERT
  instead of silently shrinking the candidate set. Fixture consequence: every
  upwork_chat test INSERT must carry target_client_ref + thread_id (thread
  seeded first), and cleanups must delete tasks BEFORE normalized_threads
  (tasks_source_thread_id_fkey is a new parent).

### The Pipedream calendar transport (SWT-27)

- `CAL_SOURCE=oauth|pipedream` (unset = oauth, byte-for-byte SWT-24; unknown
  value errors). ONE dispatch (`runCalendarPhase`) at both call sites. The
  transport is a deployment property — never a source_accounts column.
- **Selection is the availability scope** (`ListAvailabilityScopeAccounts`:
  provider='google' AND calendar_in_availability) so the polled set IS the
  demanded set — proven equal by an integration test over the columns, not a
  shared constant (the provider half lives inside accountSelect's WHERE; a
  half-restated "shared" predicate is the recurring defect).
- **Every poll is a full snapshot = a windowed REPLACEMENT** via
  SupersedeAbsentCalendar with the SAME bounds the request used. The window
  ECHO check is what keeps that from destroying data — a workflow answering a
  narrower window than asked would cancel everything outside it. A count
  mismatch on a CLAIMED entry taints the WHOLE poll (transit is shared) —
  blast radius: one miscounted calendar errors every account's run and, after
  AVAIL_MAX_SYNC_AGE, propose_slots refuses for everyone (fail-closed on
  purpose); a stranger calendar's bad count is ignored with the stranger.
  Status/absence/recurrence/parse failures fail only their account. Every event validates
  through NormalizeCalendarEvent BEFORE any raw write — one bad stored item
  stalls Normalize and with it mail, outbound observation and capture.
- **Empty verified snapshot** → ok run + stale events kept +
  calendar_empty_snapshot counter (never call the supersede with empty keep —
  the sink refuses by design). Over-busy, self-healing, deliberate.
- The Pipedream path writes **no cursor at all** and decrypts nothing
  (no OPS_TOKEN_KEY, no client secret). Secrets env-only
  (PIPEDREAM_CALENDAR_URL, *_TOKEN_FILE preferred); errors withhold the
  endpoint — the URL is the token's neighbour, and Go's *url.Error/*net
  errors embed it, so transport errors are CLASSIFIED, never %w-wrapped.
- stats->>'calendar_source' / calendar_empty_snapshot are DIAGNOSTIC ONLY —
  nothing may branch on a stats payload (the upworkcrm two-rows landmine).

### Calendar booking — the write route (SWT-28)

- The same workflow serves reads AND writes, branching on `action`: absent =
  the untouched read poll (its wire bytes are pinned byte-identical by a unit
  test), `create_event` = `events.insert` with a CLIENT-SUPPLIED id
  (`CalendarEventID`: base32hex `[0-9a-v]` — Google's id alphabet; anything
  else is rejected and reserves nothing). 409 → `events.get` + `created:false`
  is what makes the transport retry (exactly ONCE, only on a transport error,
  NEVER on any HTTP response) safe.
- `channel='calendar'` is the FIRST auto-tier channel: `book_calendar_block`
  approves + sends in one audited call and is deliberately NOT in
  `policy.humanOnly`. The gates that replace the human: `channel_mismatch`
  (denied by name on every other channel, BEFORE the channel switch — once a
  verb is sendShaped, any live branch allows it), the kill switch +
  10/h rate limit (it is sendShaped AND freezeGated), per-account
  `calendar_write_enabled` re-checked at SEND, and the pre-flight
  `availability.LoadBusy` refusal (verbatim propose_slots semantics).
- `calendar:{event_id}` has ONE spelling — `google.CalendarExternalID` — used
  by the send path and all three calendar ingest sites; a structural test
  bans the raw literal. The send-time raw row and the next poll's raw row
  must hash identically or the snapshot replacement supersedes our own block.
- R8 skips `channel=="calendar"` entirely (no mark, no close, and crucially
  no `record_orchestration` — firing it would burn the task's one-shot
  `delivery_lifecycle` dedup key and silently dedupe the task's later REAL
  delivery).
- A failed booking keeps `sent_external_id` (unlike gmail's definite-reject
  reopen): reopening would trust a human-edited third-party workflow's 409
  handling. Recovery = read poll or a new draft.
- The busy set learns about a booked block IMMEDIATELY
  (`PGSink.RecordOwnCalendarEvent`, best-effort) — without it propose_slots
  re-offers the just-booked slot for up to 20 minutes and the auto tier
  double-books itself.
- Latent trap (delta review F4): only the PIPEDREAM poll runs the
  observation sweep. Under CAL_SOURCE=bridge/oauth a booked block can never
  confirm (the send-time record short-circuits the content hash), so its
  reservation and supersede fence become PERMANENT for that account. Fine
  while the production calendar transport is pipedream; rolling the
  transport back with calendar bookings outstanding needs the operator
  confirmed_at stamp (runbook) or a sweep port first.
- **Landmine (found by the live smoke, 2026-09-07): a hook on Normalize never
  fires for our own writes.** The send-time record stamps `normalized_at`, so
  the next poll's content_hash short-circuit means Normalize never revisits
  the row — a confirm hook there is absent-because-impossible, with no error
  anywhere. Loop closure for self-written rows must key on the poll's
  OBSERVATION (`ConfirmObservedCalendarDeliveries`, called per verified
  snapshot), not on re-normalization. Same family as the two recorded
  absent-field traps: the quiet path is the one that never runs.

### The local classify lane runs on the z4 (2026-09-06)

`OPS_LOCAL_PROVIDER_URL=http://192.168.50.55:11434`, `OPS_LOCAL_MODEL=qwen3:8b`
(both in ~/.bashrc). The z4's Polaris card runs ollama under Vulkan, 100% GPU,
one model at a time (qwen3:8b holds 5.6 of 8 GB); measured through the classify
harness: median 4.1s/verdict vs 7.2s on the workstation's shared card. The
workstation's own ollama systemd user service (the one marked temporary) is
STOPPED AND DISABLED — do not resurrect it for a "quick run"; point at the z4.
Always the IP literal: the locality boundary does no DNS, a hostname is
LocalityRemote and every message gets skipped. Requests queue serially — fine
for CronJobs and evals, not for anything interactive.

Thinking measured 2026-09-07 (the A/B the eval harness exists for): qwen3:8b
with think:true, 2048 budget, 8k ctx scored **recall 0.57 / precision 0.67 /
31s median** on the personal labels vs 0.94 / 0.50 / ~10s at think:false —
the model deliberates itself OUT of flagging Rx, appointment and fraud
notices, and 4 of 280 messages exhausted the whole budget reasoning.
`classify eval --think` stays available for future models; for qwen3:8b the
question is CLOSED with data. Do not re-enable thinking without a new eval.

Update 2026-09-07: the RX 570 hard-crashes the node under sustained STOCK
power load (~3-minute windows; ground-bond and slot theories both dead —
the Z4 won't POST without the P400, so the layout is fixed). **The 90 W
power cap is the steady state** and has never failed (137 production
verdicts + real-shaped soaks). Cost: ~10 s/verdict on production messages
(9.3 s stock). If the endpoint connection-refuses mid-pass, timestamp it to
the kube session — that is hardware, not software, and stock power is not
to be re-enabled for a "quick run".

### Classify promotion (SWT-30)

The personal lane's exit from shadow. `internal/promote` reads stored
`ai_extractions` verdicts and creates tasks through the executor — never a
model call, and that is STRUCTURAL: the package cannot import
`internal/provider` (a transitive-reachability test in
`internal/promote/structure_test.go` walks the import graph, so importing
`internal/classify` doesn't dodge it either).

- **The cutover is a COLUMN**: `projects.classify_promote_after TIMESTAMPTZ`
  (0021), NULL = off, no default, no backfill — armed only by a hand-run
  UPDATE (see the runbook's Promotion section). Forward-only on the VERDICT
  clock (`ai_runs.created_at`), Q2's answer: lowering it does not backfill.
- **The residue lane is excluded twice**, and one bar is free:
  `worker_type='classify'` by name, plus the inner join to the attributed
  project — 0015's CHECK makes `(action='unmatched') = (project_id IS NULL)` a
  schema fact. Fixtures isolate EACH bar (residue-over-attributed, and
  personal-over-unmatched); dropping either goes red.
- **Promotion dedup key**: `classify_promotions.normalized_message_id` has a
  TOTAL unique index; the claim is `ON CONFLICT DO NOTHING RETURNING id`,
  inserted BEFORE the executor call (claim-before-act, capture's ordering). A
  row with `task_id IS NULL` is a crash artifact — visible, inert, never
  completed by a later pass.
- **`classify eval` writes NO ai_runs/ai_extractions rows** (verified
  2026-09-09: it calls `lane.Complete` directly and scores in memory). That is
  the ONLY reason an eval over the committed labelled set — all historical
  personal mail — cannot inject fresh-timestamped verdicts into the promoter's
  inbox. If `Eval` ever gains a store write, the cutover gains an eval-shaped
  hole. Say so in any ticket that touches eval persistence.
- **Boundary restated** (SWT-21's): promoted tasks carry restricted personal
  content into `tasks`. Safe today because drafts skips `local_only` projects
  and `task_get_next` filters `p.client = $1` while personal has `client IS
  NULL`. Any future reader of `tasks` without one of those clauses inherits a
  leak.
- **`create_task` grew `status` (`ready|holding`)** — deliberately NOT added to
  the MCP schema; a guard test pins that. The review lane is
  `/tasks?project=personal&status=holding`, a filter, not a table.
- Advisory lock `0x5157_0021`; losing it is an ERROR (classify's policy, not
  capture's log-and-skip) — a solo pass that silently no-ops looks like an
  empty inbox.

### Capture rules went LIVE (2026-09-09)

Salvador: "allow all the things from all projects to land as tasks." The flip
is `CAPTURE_RULES_MODE=live` on the connector CronJobs (kube session applied;
exactly lowercase "live" — anything else is silently shadow by design), live
horizon left at the 720h default. Seeded first with a one-off
`opsctl capture-rules run --live --since 720h` from the workstation:
4,846 considered, 3,130 matched, **40 tasks created**
(collaboratory 29 / reengine 7 / saka 2 / foundry 1 / town-ai 1), 656 log
appends. Live decisions are per-message claims (the partial unique index), so
connector ticks after the seed only act on NEW messages. Capture's lock policy
is log-and-skip on contention (benign, unlike classify-promote's error).

Same day: projects `foundry` (8), `town-ai` (9), `homelab` (10), `saka` (11 —
client Mario Cruz / Saka Technologies, TWO CRM client records so TWO
thread_key_prefix rules, 57+58) were created; upwork client_id-prefix capture
rules 55 (foundry/Lyle) and 56 (town-ai/Erica) route all their rooms, current
and future. pod-hut deliberately NOT created — engagement on long pause.

### Ticket-status reconciler is live (SWT-32, 2026-09-09)

`internal/ticketstatus` runs at the end of every connector-jira tick (after
capture, deliberately): a jira-keyed task closes when its ticket's
statusCategory is `done` OR (gated projects) the assignee is not the polling
account's own accountId; it reopens — to the status it held — when the ticket
warrants it again. `last_action='closed'` in `ticket_status_syncs` is the only
thing that authorises a reopen; human dismissals (task_dismissals) always
outrank the reconciler. Reengine's gate is ARMED. The Avviato lookup half
(`jira_lookup` provider, candidate-driven, prefix-routed) is dormant until an
API token is stored via `jira-auth add --lookup-only` — its refs show as
`unpolled` in `opsctl ticket-status report`, which is expected, not a bug.
One-off reconciliation ran 2026-09-09: 14 collaboratory tasks closed.
`task_reopen` exists now (spine, not humanOnly, off MCP); advisory key low
digits 0023. Landmine class confirmed twice this ticket: Jira has a FOURTH
statusCategory key (`undefined`) — readers must treat unknown keys as
evidence gaps, never pass them toward a three-value CHECK.

### Pipedream's free tier is 100 credits/MONTH (verified 2026-09-10)

Read off the billing page, after two cadence decisions had silently assumed a
DAILY budget: the free workspace allows **100 execution credits per month,
resetting on the 1st**, and the calendar workflow costs **~1 credit per
invocation** (its usage chart shows 21 credits on 2026-09-08 at hourly
cadence). A booking spends one too. So hourly polling is 7x the monthly cap and
`*/20` is 21x; **~3 invocations/day is the ceiling for reads and writes
combined**. Cadence is now `0 11,17 * * *` (twice daily, 07:00/13:00 EDT).

**The failure signature is a liar**: with the cap spent, EVERY request — even
an unauthenticated GET with no body — returns `HTTP 400 "Error in workflow"`,
17 bytes of text/html, no detail. That reads exactly like a broken workflow
step or a bad request envelope; it is neither. Check credits at
pipedream.com/settings/billing BEFORE debugging the workflow, and note the
cap's effect persists for the rest of the calendar month (2026-09-08 → Oct 1
here: 507 consecutive failed runs).

Standing consequence: `AVAIL_MAX_SYNC_AGE=150m` is far tighter than the polling
gap this budget forces, so availability will refuse nearly all day even once
credits return — the freshness gate and the cadence have to be decided
together. The durable fix is to take calendar READS off Pipedream entirely
(private iCal feed, or the Google Calendar API) and keep Pipedream for the rare
booking write.

### Dismissals reopen on new inbound activity (SWT-36)

A dismissed task (`status='closed'` AND an OPEN `task_dismissals` row) comes
back when a NEW inbound message reaches it through one of the two existing
attach paths — promote's `threadTask` (same thread + project) or capture's
`taskForExternalRef` (same external ref). The pass logs first, then calls the
GUARDED `task_reopen {task_id, dismissal_id, message_id, reason}`; the handler
decides under the tasks row lock and restores `closed_from_status` (else
`ready`). Three things to know before touching it:

- **Partial-index landmine, again.** 0026 replaced the TOTAL
  `task_dismissals_task_uniq` with the PARTIAL `task_dismissals_open_uniq
  (task_id) WHERE reopened_at IS NULL` — one OPEN dismissal per task, any
  number over time. Every `ON CONFLICT` against `task_dismissals` must restate
  `WHERE reopened_at IS NULL` or Postgres raises "no unique or exclusion
  constraint matching the ON CONFLICT specification" at runtime, on a human's
  Dismiss click (the capture_decisions_live_uniq / 0013 precedent; a
  structural test scans internal/ for it). Rows are never deleted: a reopen
  STAMPS `reopened_at/_by`, plus `reopened_by_message_id` when activity did it
  (NULL = a human's plain reopen, the only mis-click signal). Count rows, not
  tasks, when reading labels.
- **The clock is INGEST time.** Reopen iff `normalized_messages.created_at >
  task_dismissals.created_at`, strictly, compared in SQL inside the handler —
  never `sent_at`. Both are Postgres `now()` (one clock, no skew allowance),
  both survive re-normalization (sink upserts never touch `created_at`), and
  it catches the lag case (sent before the dismissal, ingested after). Cost:
  a long-suspended connector's backlog is stamped "now" and can reopen for
  old mail, bounded by capture's 720h live horizon. Callers pass IDS only; the
  handler also refuses a non-inbound `message_id` with an ERROR (invariant 5).
- **D3's scope.** Only a DISMISSAL reopens. Plain `task_close` (orchestrator,
  hand-run), R8's Deliver-task close, reconciler closes and `delivered` tasks
  keep their old behaviour (promote's Q3 fall-through; capture's silent log).
  The reconciler's D4 suppression reads OPEN dismissals only, so an
  activity-reopened task is ordinary to it again.
- **0026 needs a coordinated CUTOVER, not migrate-then-roll** (Codex review).
  Pre-SWT-36 code's `ON CONFLICT (task_id) DO NOTHING` cannot infer the
  partial index, so every OLD dismiss writer (dashboard, installed
  ops-mcp-user, opsctl, open `ops` sessions) errors after 0026; new code needs
  0026 first. Drain the old writers (dashboard to 0, close sessions), apply
  0026, deploy the new images and re-install ops-mcp-user/opsctl, then scale
  back up. SPEC Verification step 5 has the sequence. Generalises: dropping a
  total unique index that an old `ON CONFLICT (cols)` infers is a breaking
  change for the old binary — plan a drain or an expand/contract.
- **Reading dismissal labels:** "a human undid this" is `reopened_by` being a
  HUMAN actor (dashboard:/opsctl:/manual:), not merely
  `reopened_by_message_id IS NULL` — a reconciler reopen racing a fresh human
  dismissal can stamp it with a NULL message id (pre-existing, seconds wide).
