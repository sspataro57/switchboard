> Jira: SWT-21

# provider-locality — personal content may only be processed by a local provider

**Status: FINAL.** The four open questions were answered 2026-08-28 and folded in
(see "What the answers changed", bottom). `provider-locality_OPEN_QUESTIONS.md`
is retained as the record of the reasoning, not as pending work.

## Source

Not a build-order step. Ad-hoc, from Salvador (2026-08-28):

> A message attributed to the `personal` project may only be processed by a
> provider that runs locally, enforced in code and failing closed, so that a
> configuration change cannot silently send personal financial mail to a hosted
> API.

Plus the follow-up constraint, same day:

> The local model is a 4B at LOW PRIORITY on a separate box. Slowness and
> unavailability are the NORMAL case. When no local provider is available for a
> personal message, the message is SKIPPED and left for the next pass — not
> deferred to a hosted provider, not retried against a different adapter, not
> dropped from the corpus. The skip must be distinguishable in the record from
> "the model looked and found nothing". A timeout is normal operation, not an
> error. Absent, undeclared and unreachable land in the same place.

And the answer to Q2, which widened the boundary from "personal" to "everything
the deterministic rules could not place" (Salvador, 2026-08-28): *"go with the
deviation, triage waits for the classifier."*

## Premises, verified against this repo before writing (not assumed)

1. **Shadow mode does not skip the model call.** `triage.Run`
   (`internal/triage/triage.go:126`) calls `client.Complete` unconditionally at
   line 144 for every pending message. Shadow means "extract everything, create
   nothing" — `triage.Store` simply has no task-write method. The full,
   untruncated body goes in the user prompt: `internal/triage/prompt.go:127-128`
   renders `m.BodyText` verbatim, and up to 10 prior thread messages truncated to
   400 chars each (line 122). So today, every message in triage's inbox and its
   thread neighbours are sent to `api.openai.com`.
2. **Triage's inbox is `capture_decisions` latest-action `unmatched`, with no
   mode predicate** (`internal/triage/store.go:50-67`). Consequence: a **shadow**
   attribution already removes a message from triage's inbox. A `personal` rule
   needs no live capture pass to take effect.
3. **Attribution-only is already a first-class rule shape.** A rule with no
   `external_system` yields `action='attributed'` and creates nothing, in shadow
   AND in live mode (`internal/capture/rules_store.go:536-543`). So "a `personal`
   project that collects mail and never manufactures tasks" needs no new engine
   behaviour.
4. **The provider seam is one method.** `provider.Client` is
   `Complete(ctx, Request) (Response, error)` (`internal/provider/provider.go:33`);
   `cmd/triage/main.go:84`, `cmd/drafts/main.go:63` and `cmd/planimport/main.go:108`
   each construct `provider.NewOpenAI` and inject it. Nothing in the seam
   describes where the bytes go.
5. **Thread context is a leak path that message-level attribution does not
   close.** Triage assembles thread context by `thread_id` alone
   (`store.go:103-108`) with no reference to the neighbours' capture decisions,
   and the drafts worker does the same for its own prompt. Under the final class
   mapping this is no longer load-bearing for triage (every triage request is
   restricted anyway) but it IS load-bearing for drafts — see criterion 14's
   honesty note.

Measured context supplied with the ticket, taken as given (do not re-derive;
re-measure at verification time rather than pinning literals — the corpus is
live): capture claims ~33,048 of ~49,416 messages, leaving triage ~16,369 that
are disproportionately personal (Bank of America ~803, a mortgage servicer,
CareCredit, an HSA, a medical biller). 11 `ai_runs` exist, all 2026-07-12, and no
`ai_extractions` from any financial sender. **Nothing has leaked yet. This ticket
exists to keep that true.**

**The restricted lane is not a degraded lane.** `docs/tickets/local-classifier-spike.md`
(2026-08-28) measured qwen3:8b at **0.90 recall against 0.80 for the best hosted
model**, and gpt-5.4-mini / gpt-5.6 each missed **3 of 5** HOA violation notices
that the local model caught. This SPEC cites that only to justify the scope of the
restriction; model choice belongs to SWT-22.

## Goal

Make "which provider may see this content" a **total, pure, fail-closed function
of the content's capture attribution and the adapter's destination**, enforced at
the provider seam rather than at a call site, with refusals recorded; and seed the
`personal` project plus attribution-only capture rules so financial/personal mail
is attributed rather than left in the residue.

**Usable alone:** after this ticket, with no local model anywhere and no
classifier written:

1. No captured message content reaches a hosted API from any worker, at all — not
   the personal messages, not the unmatched residue, not their thread neighbours.
   Triage makes zero network calls and records why.
2. The financial/personal senders are attributed to a `personal` project and are
   out of the residue, so the follow-on classifier's inbox is the residue proper.
3. The seam the local classifier plugs into exists, is pure, and is tested with no
   model and no database. SWT-22 adds a worker, not plumbing.

## Design shape (the four decisions this SPEC makes)

**A. Locality is derived from the destination, not from a type name or a config
flag.** Every `provider.Client` must describe the endpoint it will POST to;
`provider.LocalityOf` classifies that endpoint. This is the answer to "what cannot
be forged or forgotten": the measured local stack is `ollama serve` with an
OpenAI-compatible `/v1` route (spike, "Operational facts"), so the same
`provider.OpenAI` adapter will serve both lanes with a different base URL. A
locality declared by adapter type would therefore be a lie in both directions.
Keying on the destination also means the exact configuration change the ticket
fears — repointing a worker at a hosted API — is the change that trips the guard.
It needs no special case for the measured deployment: `127.0.0.1:11434` and a
`192.168.50.x` LAN address both classify local under criterion 2.

**B. Only a positive, non-restricted project attribution is general.** Unseen and
unmatched are both RESTRICTED. This is the accepted deviation from the first
draft, and it is the difference between a boundary that holds and one that is only
as good as the sender list: a personal message that no rule happens to match is
`unmatched`, and under the original mapping that made it hosted-eligible. **Rule
completeness must not be load-bearing for a security property.**

**C. The class of a request is the most restrictive class of every piece of
content in it** — the message and its thread context (premise 5).

**D. Unavailability is a first-class outcome, not an error.** Absent local client,
present-but-not-local endpoint, and present-but-unreachable all produce the same
`Skip` decision, recorded distinctly from `ok` and from `error`. An unclassified
error skips the *message*; the *pass* raises only on the pattern (criterion 17).

## Ordering — this ticket GATES triage on SWT-22, not the reverse

A future reader will assume the usual direction. It is inverted here, deliberately.

Triage's entire inbox is `action='unmatched'` (premise 2), and under decision B
unmatched is RESTRICTED. Therefore **after this ticket, triage cannot process a
single message until a local adapter exists** (SWT-22). Every pass will route
every message to `DecideSkip` and record one aggregate refusal.

That consequence is accepted, and it costs nothing today: triage last ran
2026-07-12, is shadow-only, and creates nothing. What it buys is that the
boundary does not depend on anyone having written the right capture rule.

Dependencies, stated so the board matches reality:

- SWT-21 (this) ships the seam, the `personal` project and the attribution rules.
  It does not need a local model to be correct, only to be *useful for triage*.
- SWT-22 ships the local adapter + classifier and cites the spike's model choice.
  It is what restores triage to a working state.
- Neither ticket flips `CAPTURE_RULES_MODE` or takes triage live. Both are
  separate, later, and ordered (capture live before triage live — see
  `docs/runbooks/capture-rules.md`).

The hosted lane is NOT dead after this ticket: `drafts` (for tasks in projects
with `ai_locality='any'`) and `planimport` still use it. What it stops seeing is
captured message content that nothing has positively cleared.

## Acceptance criteria

Each is testable. Criteria 1-8 are pure unit tests (no db, no model, no network).

1. `provider.Client` gains `Describe() Descriptor` where
   `Descriptor{Name, Endpoint string}`. Adding it to the interface (not an
   optional side-interface) is deliberate: a new adapter cannot forget to declare,
   because the compiler refuses. Existing adapters and every test fake implement it.
2. `provider.LocalityOf(Descriptor) Locality` is pure and total, with
   `LocalityRemote` as the zero value. It returns `LocalityLocal` **only** when
   the endpoint parses as an `http`/`https` URL whose host is a loopback or
   private-range IP literal (127.0.0.0/8, ::1, 10/8, 172.16/12, 192.168/16,
   fc00::/7, 169.254/16) or is exactly `localhost`. Empty endpoint, unparseable
   endpoint, hostname requiring DNS, public IP, non-HTTP scheme → `LocalityRemote`.
   No DNS lookup is performed (a resolution at check time is both I/O and a
   TOCTOU).
3. `provider.Class` has exactly two values and the **zero value is the restrictive
   one**: `ClassRestricted Class = ""`, `ClassGeneral Class = "general"`. The
   predicate is `LocalOnly(c) == (c != ClassGeneral)` — so a forgotten field, a
   typo, and an unrecognised future value all land in restricted. A test pins all
   three.
4. `provider.MostRestrictive(...Class) Class` folds a request's content classes;
   the fold over an EMPTY set returns `ClassRestricted`.
5. `provider.ClassOf(state AttributionState, projectLocalOnly bool) Class` is
   total, and returns `ClassGeneral` **only** for a positive project attribution
   whose project is not local-only. Pinned by test:
   - `AttributionUnseen` (no `capture_decisions` row) → `ClassRestricted`
   - `AttributionUnmatched` (latest action `unmatched`) → **`ClassRestricted`**
   - `AttributionProject` → `ClassGeneral` iff the project's `ai_locality='any'`,
     else `ClassRestricted`
   - any other/zero state → `ClassRestricted`

   The unseen/unmatched distinction from SWT-17 §8 is **preserved in the type**
   even though both map to restricted today, because they are different facts
   (the engine has not looked / the engine looked and placed nothing), they are
   reported separately (criterion 18), and collapsing them into one state would
   destroy the distinction the next ticket needs. A test asserts both states exist
   and both map to restricted — not one state named "not general".
6. `provider.Decide(class Class, local Availability) Decision` is pure, total and
   exhaustive over `Availability ∈ {AvailAbsent, AvailNotLocal, AvailUnreachable,
   AvailReady}`, returning `Decision ∈ {DecideSkip, DecideLocal, DecideGeneral}`.
   A test table asserts **every** `(ClassRestricted, availability != AvailReady)`
   pair yields `DecideSkip` — never `DecideGeneral`. The zero `Availability` is
   `AvailAbsent`.
7. `provider.Router` is the only way a worker obtains a client. It is constructed
   from a general client and an optional local client, and exposes exactly one
   resolution method, `Route(ctx, class) (Client, Decision, Reason)`. **There is
   no method that hands a remote client to a restricted class**, and a unit test
   builds a Router whose local slot is nil, routes `ClassRestricted`, and asserts
   the general fake recorded **zero** `Complete` calls.
8. Transport failures are typed. Adapters wrap connection/timeout failures with
   `provider.ErrUnavailable` (`errors.Is`-checkable); HTTP-status, schema and
   parse failures are NOT wrapped. `provider.OpenAI.Complete` is updated
   accordingly and tested with `httptest` (closed listener / a handler that sleeps
   past a 1ms client deadline) — no live provider, ever.

9. Migration `0016_provider_locality.sql` adds
   `projects.ai_locality TEXT NOT NULL DEFAULT 'local_only' CHECK (ai_locality IN
   ('local_only','any'))`. **Fail closed on new rows**: a project created later
   without thinking about this column is restricted, which stalls (recoverable
   with one UPDATE, visible in the skipped lane) rather than leaks (irreversible).
   **Statement order is load-bearing and must be exactly:** ALTER (existing rows
   take the `local_only` default) → `UPDATE projects SET ai_locality='any'`
   (existing rows named explicitly; they predate the boundary and their traffic is
   client work) → INSERT the `personal` row with `local_only`. Reversing the last
   two silently makes `personal` general, and nothing would fail.
10. **Test-fixture consequence of criterion 9, stated because it will otherwise
    read as a broken guard:** every integration test that inserts a project and
    expects the general lane must set `ai_locality='any'` explicitly. The affected
    suites insert projects at 21 sites (`internal/drafts/store_integration_test.go`,
    `internal/tools/*`, `internal/dashboard/*`, connector suites). A fixture that
    forgets it produces a *skip*, not a failure message about locality — so this
    criterion requires the drafts skip log to name the reason (criterion 15).
11. A `personal` project exists with `ai_locality='local_only'`, `client` NULL,
    `execution='manual'`, `delivery='dashboard'`, seeded by migration 0016 with
    `ON CONFLICT (slug) DO NOTHING`. **The migration seeds the PROJECT ONLY.**
    Verified safe: all 27 integration cleanups delete projects by slug or
    slug-LIKE, so a seeded row is not collateral damage.
12. **Capture rules are NOT seeded by the migration** — SWT-17's precedent holds
    unchanged (`migrations/0015_capture_rules.sql:29-32`: routing is configuration
    with an enabled flag and an audit trail; seeding puts production routing into
    every test database and makes a rule edit a new migration). They are added
    with `opsctl capture-rules add` and recorded in the runbook. Decision B is
    what makes this safe: with unmatched restricted, a missing rule costs
    attribution quality, not a leak.
13. The personal rules attribute to `personal` as **attribution only** —
    `external_system` NULL, so `action='attributed'`, no task, no `external_refs`
    row, in either mode. The sender list comes from **measurement, reviewed by
    hand** — the characterised unmatched corpus (financial ~1,510, job alerts
    ~1,857, newsletters ~1,557, dev/infra ~814, brand marketing the largest
    share), not from memory and not invented here. An integration test seeds
    equivalent rules over fixture messages and asserts `action='attributed'`,
    `project_id = personal`, zero rows added to `tasks`, zero to `external_refs`.
14. A message attributed to `personal` is **not in triage's inbox**: an
    integration test writes a shadow `attributed` decision and asserts
    `PendingMessages` does not return it. Existing behaviour of `store.go:50-67`;
    pinned because this ticket depends on it. **Honesty label:** after decision B
    this is a volume property, not the safety property. It keeps the classifier's
    inbox clean; it is not what stops a leak.
15. `triage.AssembleContext` returns, per message, a tri-state attribution
    (`unseen` / `unmatched` / project + `ai_locality`) **for the message and for
    every thread message it includes in the prompt**. The existing project lookup
    (`store.go:171-175`, latest decision then its project) is extended, not
    forked: "latest" keeps meaning the newest row, full stop.
16. `triage.Run` computes the request class as
    `provider.MostRestrictive(class(message), class(each thread message)…)` and
    routes through `provider.Router`. `Run`'s signature takes `*provider.Router`,
    **not** `provider.Client` — so a fallback would require constructing a second
    router, which is a visible act rather than a forgotten one.
    **Honesty label, required in the code comment:** in TRIAGE the fold cannot
    change the outcome today, because the triaged message is `unmatched` by
    construction and therefore already restricted. The fold is load-bearing in
    DRAFTS (criterion 22), where a project-attributed task can have an unseen or
    personal thread neighbour, and it is what would stop the thread hole if a
    later ticket ever makes unmatched general again. It is one spelling of one
    rule in both workers on purpose; a triage-only shortcut would be a second
    spelling that could drift.
17. On `DecideSkip`, triage: makes no provider call of any kind; writes **no**
    `ai_extractions` row (so the message stays in the pending filter and is
    retried next pass — a consequence of the existing filter, not new
    bookkeeping); records the refusal per criterion 19; does not increment the
    error counter; and does not cause a non-zero exit.
18. Restricted-lane failure handling, in three tiers:
    - `provider.ErrUnavailable` (including deadline exceeded) → same skip path as
      criterion 17, reason `local_unreachable`. Never an error, never a second
      adapter. A test drives this with a fake local client returning
      `ErrUnavailable` and asserts the general fake recorded zero calls.
    - Any other error (HTTP 4xx/5xx, malformed JSON, schema violation) → the
      MESSAGE is skipped with reason `unclassified_error`, recorded per message.
      It does not fail the run by itself.
    - The PASS raises (returns a non-nil error, non-zero exit) when
      `unclassified_error` count **exceeds half the restricted-lane attempts in
      that pass, with a floor of 20 attempts**. `local_unreachable` skips do NOT
      count toward it — those are normal operation for a 4B at low priority.
      **The threshold is a guess, not a derived number** (a broken adapter fails
      on everything; a malformed message fails alone — the shape is right, the
      50%/20 is a starting point that wants tuning against a real failure). It is
      a named constant with that sentence in its doc comment, and the pass logs
      the ratio it saw whether or not it raised. This is the same separation the
      reconcilers make between "the poller did not run" and "the poller found
      nothing".
19. **Refusals are recorded, and the record is bounded.**
    - Pass-level: when the local lane is not `AvailReady`, the pass records ONE
      `ai_runs` row with `worker_type='triage'`, `provider='local'`,
      `status='skipped'`, and `input` =
      `{avail_reason, class_reasons:{unseen:N,unmatched:M,project_local_only:K},
      skipped_count:N, message_ids:[≤100]}`. One row per pass, not per message —
      the SWT-17 amplification landmine (49,415 messages × every pass) is the
      reason, and after decision B the restricted population is the WHOLE inbox,
      so this is no longer a theoretical bound.
    - Per-message: a message-specific skip (unreachable mid-pass,
      `unclassified_error`) records one `ai_runs` row with the message id and its
      reason. Bounded by how far the pass got.
    - `status='skipped'` is a value `ok` and `error` never take, and a skip NEVER
      writes an `ai_extractions` row. So "the model looked and found nothing" (an
      extraction with `actionable=false`) and "no permitted provider looked" are
      structurally distinct rows.
20. `triage report` prints a `skipped:` line broken down by `avail_reason`
    (`no_local_provider`, `local_endpoint_not_private`, `local_unreachable`,
    `unclassified_error`) and by `class_reason` (`unseen`, `unmatched`,
    `project_local_only`), reading `ai_runs` where `status='skipped'`. Today's
    report joins only `status='ok'` rows (`report.go:20`), so skips would
    otherwise be invisible — which is the failure mode, not the fix. After this
    ticket the report is the ONLY place the operator learns that triage is idle by
    design rather than broken, so it must say so in words, not only in counts.
21. `cmd/triage` builds the Router: general lane from `OPENAI_API_KEY` as today,
    local lane from `OPS_LOCAL_PROVIDER_URL` (+ `OPS_LOCAL_MODEL`) when set. If
    `OPS_LOCAL_PROVIDER_URL` is set to a non-local endpoint, the local lane is
    **absent** (not "local"), the process logs the refusal at startup, and the
    reason recorded on skips is `local_endpoint_not_private`. Unset →
    `AvailAbsent`. Neither case is a startup error.
    **Change from today:** `OPENAI_API_KEY` must no longer be a hard requirement
    for `triage run` (`cmd/triage/main.go:56-59`), because after this ticket a
    triage pass that never touches the general lane is the normal case. Missing
    key → the general lane is absent; that is only fatal if something routes to it.
22. Availability is probed, not assumed. A local client may implement
    `provider.Prober` (`Probe(ctx) error`); the Router probes it ONCE per pass
    with a short deadline. A local client that does not implement `Prober`, or
    whose probe fails, is `AvailUnreachable` — "declares itself local but is
    unreachable is not a permitted processor right now", including the case where
    the declaration is all there is.
23. **No worker mints its own client.** A structural test (plain unit test, no
    build tag, no db — the `internal/textmatch/callsites_test.go` shape) scans
    `internal/triage`, `internal/drafts`, `internal/planimport` and
    `internal/capture` and fails any file mentioning an adapter constructor
    (`provider.NewOpenAI`, or any `provider.New*`) or a bare `provider.Client`
    parameter in a `Run`/`Propose` signature. Adapters are constructed in `cmd/`
    only. Covering all three worker packages is the point: SWT-18's scan covered
    three of four matchers and the fourth rotted silently for five weeks.
24. `internal/drafts`: `DeliverTasks` also selects `p.ai_locality`, `drafts.Run`
    takes `*provider.Router`, and the class is folded over the project and every
    thread message included in the prompt. Same skip semantics as criterion 17,
    logged onto the Deliver task through the existing `task_append_log` /
    `draft_skip` path (`drafts.go:126-134`), naming the reason — not as an error.
    **Honesty label required in the comment:** the `personal`-project half of this
    is ARMED BUT INERT (personal is attribution-only, so it has no tasks and no
    Deliver tasks). What is NOT inert is criterion 10's fixture consequence and
    the thread fold, both of which will fire on ordinary client projects.
25. `internal/planimport`: `Propose` takes `*provider.Router` and passes
    `provider.ClassGeneral` explicitly at its single call site, with a comment
    stating why (the input is a plan file a human named on the CLI, not captured
    message content). Explicit, because criterion 3's zero value would otherwise
    silently restrict it and plan import would stop working with no local model.
26. `docs/runbooks/provider-locality.md` records: the measured `personal` sender
    list and the exact `opsctl capture-rules add` commands used; the env contract
    (`OPS_LOCAL_PROVIDER_URL`, `OPS_LOCAL_MODEL`); what a skip looks like in
    `triage report`; **that triage is expected to skip everything until SWT-22
    lands, so an all-skipped report is the success state, not an outage**; and the
    sentence that a fallback to a hosted provider is never correct. A structural
    test asserts the runbook exists and contains the no-fallback sentence and the
    triage-gated-on-SWT-22 sentence (the `TestRunbook_DocumentsCaptureBeforeTriage`
    shape, `internal/capture/rules_structure_test.go:125`) — both rules are
    prose-shaped, and the next contributor's instincts are to "fix" the skip into
    a fallback and to "fix" the idle triage into a hosted call.

## Data model changes

Migration **`0016_provider_locality.sql`** (highest existing is
`0015_capture_rules.sql`; production `schema_migrations` must be at 0015 before
this ships — see the five-migrations-behind entry in institutional knowledge).

```sql
ALTER TABLE projects ADD COLUMN ai_locality TEXT NOT NULL DEFAULT 'local_only'
  CHECK (ai_locality IN ('local_only','any'));
UPDATE projects SET ai_locality = 'any';                 -- existing rows, named explicitly
INSERT INTO projects (name, slug, ai_locality) VALUES
  ('Personal', 'personal', 'local_only') ON CONFLICT (slug) DO NOTHING;
```

Notes:

- **The three statements are order-dependent** (criterion 9). The UPDATE has no
  WHERE clause on purpose: it means "every project that existed before the
  boundary", and the `personal` INSERT that follows is the only row that keeps the
  restrictive default. A WHERE clause naming slugs would rot the first time a
  project is renamed.
- The default is `local_only` — fail closed. A leak is irreversible; a stall is one
  UPDATE and shows up in the skipped lane. Same reasoning as SWT-19's choice of
  refusal over a guess.
- `ai_locality` is a CHECK-constrained column, **not** a key in `projects.policies`
  jsonb. A jsonb key is absent-by-default, and absent is the exact state this
  ticket must treat as unsafe; a NOT NULL CHECK column cannot be absent.
- No new tables. Refusals live in `ai_runs` (`status='skipped'`), the established
  "a worker considered this" log, which already carries
  `provider`/`model`/`status`/`input`. Deliberately **not** `policy_decisions`:
  that table answers "was this TOOL CALL permitted", is written only by
  `audit.PGStore.RecordPolicy` inside `Executor.Execute`, and a provider refusal
  is not a tool call. Same reasoning `0015` wrote down for `capture_decisions`.
- No change to `capture_rules` / `capture_decisions` schemas. The personal rules
  are ROWS in the existing table, added through the existing tool.

## API / MCP tool changes

No new executor tools and no new MCP tools. This ticket adds no capability an
agent can call; it removes one from the workers.

Touched via the executor path (invariant 3), unchanged shapes:

- `create_task` / `link_external_ref` / `task_append_log` — the capture engine's
  existing calls. The personal rules have `external_system` NULL, so in live mode
  they reach **none** of them; the decision row is written by `internal/capture`'s
  own append-only log, as today.
- `capture_rule_add` — used as-is to seed the personal rules. It is `humanOnly`
  and off the MCP surface, which is what keeps an agent from re-pointing the
  personal lane, and what gives the rules an audit row that a migration would not.
- `task_append_log` — drafts' existing skip log, one new reason string.

**`task_get_next` is NOT touched.** The first draft proposed an
`ai_locality='local_only'` exclusion there; it is dropped. It would be a predicate
whose discriminating column is constant in production (`personal` has no tasks,
and its `client` is NULL, so the existing `p.client = $1` at
`internal/tools/getnext.go:51` already excludes it) — the shape this repo has paid
for three times. It belongs to the ticket that gives `personal` tasks at all.

Provider seam (not an executor tool — internal Go API):

```go
type Descriptor struct{ Name, Endpoint string }
type Client interface {
    Complete(ctx context.Context, req Request) (Response, error)
    Describe() Descriptor
}
type Prober interface{ Probe(ctx context.Context) error }

func LocalityOf(Descriptor) Locality
func ClassOf(state AttributionState, projectLocalOnly bool) Class
func MostRestrictive(...Class) Class
func Decide(Class, Availability) Decision
type Router struct{ /* general, local */ }
func (r *Router) Route(ctx context.Context, c Class) (Client, Decision, Reason)
var ErrUnavailable = errors.New("provider: unavailable")
```

## MQTT topics

None. No worker heartbeat, command topic or LWT is touched.

## Files likely to touch

New:
- `migrations/0016_provider_locality.sql`
- `internal/provider/locality.go` — Descriptor, Locality, Class, AttributionState,
  Availability, Decision, `LocalityOf`, `ClassOf`, `MostRestrictive`, `Decide`,
  `ErrUnavailable`. **Pure**: no `context` on the decision functions, no pgx, no
  net/http, no `os.Getenv` — the `internal/capture/rules.go` discipline, with
  SWT-17 criterion 2 as the model.
- `internal/provider/router.go` — Router + probe.
- `internal/provider/locality_test.go`, `internal/provider/callsites_test.go`
- `internal/capture/attribution.go` — the SQL reader returning a per-message
  tri-state attribution + project locality. Returns capture's own struct, **not** a
  `provider` type: `internal/capture/rules.go` is scanned for an
  `internal/provider` import and the package should keep that property in spirit.
- `docs/runbooks/provider-locality.md`

Modified:
- `internal/provider/provider.go` (Client gains Describe), `openai.go`
  (Describe + `ErrUnavailable` wrapping), `openai_test.go`
- `internal/triage/triage.go` (Run takes `*provider.Router`; class fold; skip
  path; the `unclassified_error` ratio raise), `store.go` (attribution tri-state
  for message + thread), `report.go` (skipped lane), `worker_test.go`,
  `integration_test.go`, `capturedecisions_integration_test.go`
- `cmd/triage/main.go` (Router construction; `OPENAI_API_KEY` no longer required)
- `internal/drafts/drafts.go`, `internal/drafts/store.go`,
  `internal/drafts/worker_test.go`, `internal/drafts/store_integration_test.go`
  (fixture `ai_locality='any'`), `cmd/drafts/main.go`
- `internal/planimport/planimport.go`, `internal/planimport/worker_test.go`,
  `cmd/planimport/main.go`
- Integration suites that insert projects and exercise a model or delivery path —
  set `ai_locality='any'` (criterion 10)
- `docs/runbooks/capture-rules.md` (cross-reference the personal lane and the
  restricted-by-default residue)

## In scope / Out of scope

**In scope**
- The provider-locality decision functions, the Router, typed unavailability, the
  structural tests.
- The `personal` project, `projects.ai_locality`, migration 0016.
- Attribution-only capture rules for the measured financial/personal senders
  (rows added by `opsctl`, plus the runbook that records them).
- Triage's routing + skip + record + report lane.
- The interface change at all three worker call sites (triage, drafts,
  planimport), so the compiler enforces class declaration.

**Out of scope — explicitly, including what it is tempting to bundle**
- **The local classifier and the local adapter — that is SWT-22**, which this
  ticket gates triage on. Model choice, prompt, schema, thresholds, second-pass
  precision recovery: all there. This ticket ships the socket.
- **`task_get_next`'s locality predicate.** Dropped per Q4; see "API changes".
- **Hardware / serving stack**: the RX 570 (Polaris/gfx803 — ROCm dropped, Vulkan
  the only route), the box, its scheduling priority, its unit file. Nothing here
  names a model or a GPU.
- **Going live on capture rules** (`CAPTURE_RULES_MODE=live`) — a kube-repo change
  with its own ordering constraint. The personal rules work in shadow (premise 2).
- **Triage going live** (creating tasks). Unchanged: triage still creates nothing.
- **A tool to change `ai_locality`.** Set by migration and by psql. An executor
  tool (`project_set_ai_locality`, humanOnly) is future work.
- **Outbound**: personal deliveries, a personal kill switch, policy-matrix rows
  for a personal channel. `personal` produces no tasks and therefore no deliveries.
- **Retro-scrubbing `ai_runs.input`** for the 11 existing runs. They predate the
  financial corpus and carry no financial sender (given, and re-checkable).
- **PDF/attachment extraction** (the spike's Pines ceiling). Raw is preserved;
  this is future data work, not a boundary question.

## Invariants that apply

1. **Raw-first** — untouched. No connector changes; `raw_source_items` writes are
   not on this path. The personal rules read `normalized_messages`, which were
   raw-first when ingested.
2. **One funnel** — no new task-like table. `personal` is a row in `projects`;
   refusals are rows in the existing `ai_runs`; attributions are rows in the
   existing `capture_decisions`. A `restricted_messages` table would be a
   violation; this SPEC proposes none.
3. **Everything through the executor** — the only tool surface used is the
   existing `capture_rule_add` / `task_append_log`. No new tool, no handler
   invoked outside `Executor.Execute`, no raw_sql. The skip record is NOT
   smuggled through the executor: it is a worker's own bookkeeping row in
   `ai_runs`, the `ai_runs`/`ai_extractions`/`capture_decisions` precedent.
4. **Nothing external without a delivery row** — nothing here sends. Note the
   sharper reading this ticket adds: an outbound POST to a hosted LLM is not a
   `deliveries` channel and never will be, so it needs its own gate — criteria
   1-8 are that gate.
5. **Own-message loop closure** — untouched. Triage's and capture's
   `direction='inbound'` filters are unchanged; the personal rules match inbound
   mail only, so switchboard's own sends cannot be re-attributed.
6. **Stealth attribution** — untouched. Nothing here writes client-visible words.
7. **Orchestrator purity** — reinforced and generalised. The orchestrator never
   imports a provider adapter; this ticket makes the same property checkable for
   the *routing* decision: `LocalityOf`, `ClassOf`, `MostRestrictive` and `Decide`
   are pure functions in a file with no db, no network and no env read,
   unit-testable with zero infrastructure — the `internal/capture/rules.go` /
   `rules_structure_test.go` pattern, one ticket old and proven.

Landmine classes named in the brief, and what this SPEC does about each:

- **Actor-prefix checks are transport labels, not trust boundaries** (SWT-19's
  go-live gate). This gate keys on the destination address of the HTTP request and
  on the content's capture attribution — never on who is calling. No actor string
  appears in criteria 1-8. The counter-example that broke the last one
  (`drafts:gpt` calling the executor directly) has no analogue: drafts goes
  through the same Router.
- **A predicate whose discriminating column is constant is a no-op** (SWT-18).
  The `task_get_next` predicate that would have been exactly that shape is
  **dropped** rather than labelled. What remains is honestly labelled: criterion
  16's fold cannot change a triage outcome today, and criterion 24's
  personal-project half is inert. Neither is claimed as protection it does not
  provide. The spike found the same shape in the model itself (qwen3:8b returns
  0.95 confidence on everything it flags) — a reminder that this failure mode is
  not confined to SQL.
- **An inert mechanism that still looks alive.** The decision function is NOT
  inert after decision B: every triage pass will exercise `DecideSkip` on real
  messages from day one, and the report will show it.

## Sibling patterns to copy

- **Pure decision + structural test:** `internal/capture/rules.go` (its header
  comment explains why the file boundary IS the guarantee) and
  `internal/capture/rules_structure_test.go:37` `TestRulesGo_IsPure` — copy the
  banned-token scan for `internal/provider/locality.go`.
- **Call-site scan that cannot be forgotten:** `internal/textmatch/callsites_test.go`
  — plain unit test, no build tag, scans sibling sources and fails on omission.
  Criterion 23 is the same mechanism, and covers all three worker packages
  precisely because SWT-18's covered three of four.
- **Fake-provider worker tests:** `internal/triage/worker_test.go` (fake
  `provider.Client` + fake `Store`, no db, no network) and
  `internal/drafts/worker_test.go`. Criteria 7, 16, 17, 18 extend these; never a
  live LLM.
- **httptest adapter tests:** `internal/provider/openai_test.go:110` — criterion 8
  extends it with transport-failure cases.
- **Three-state attribution:** `internal/triage/store.go:50-67`'s comment is the
  canonical statement of unseen ≠ unmatched; criterion 5 lifts it into a type.
  Read it before writing `ClassOf`.
- **Runbook-as-tested-artifact:** `internal/capture/rules_structure_test.go:125`
  `TestRunbook_DocumentsCaptureBeforeTriage` — criterion 26 copies it.
- **Alarm separation:** `internal/connector/slackweb/reconcile.go` /
  `upworkcrm/reconcile.go` count completed passes so that "did not run" and "found
  nothing" cannot share an alarm — criterion 18's tiering is the same idea.

## Verification protocol

Before commit:

1. `go test ./...` — the pure locality table tests, the Router zero-hosted-calls
   test, the skip-path worker tests, the ratio-raise test, and the two structural
   scans. No db, no broker, no network.
2. `make integration` (`make db-up && make migrate` first; local URL
   `postgres://ops:ops@localhost:5433/ops?sslmode=disable`; runs `-p 1`, join the
   mutual-cleanup pact, clean up by slug in FK order). **Expect fixture churn from
   criterion 10** — a suite that starts skipping instead of drafting has a project
   fixture missing `ai_locality='any'`, not a broken guard.
3. **Migration state before any image ships:**
   `psql -h 192.168.50.49 -U ops -d ops -tAc "SELECT max(version) FROM schema_migrations"`
   must read `0015` before and `0016` after. Then verify the order-dependent
   statements landed right — this is the one that silently fails:
   `SELECT slug, ai_locality FROM projects ORDER BY id;`
   must show every pre-existing project `any` and `personal` `local_only`.
4. **Re-measure, do not trust the literals in this SPEC.** Read-only against
   production: the size of triage's inbox now; what the new personal rules claim
   (`opsctl capture-rules run --all` in **shadow**, then
   `opsctl capture-rules report --since 168h`); and, for criterion 16's fold, how
   many project-attributed threads contain an unseen or personal message — that
   is the fold's live value in DRAFTS. If it is zero, say so rather than claiming
   the fold protects something.
5. **The headline smoke — zero network.** With `OPS_LOCAL_PROVIDER_URL` unset,
   run `triage run --limit 5`. Expected: **no HTTP request leaves the process at
   all**, every message skipped, one `ai_runs` row with `status='skipped'` and
   `avail_reason='no_local_provider'`, and `ai_extractions` unchanged. Confirm:
   `SELECT status, provider, input->>'avail_reason', input->>'skipped_count' FROM ai_runs ORDER BY id DESC LIMIT 5;`
   and `SELECT count(*) FROM ai_extractions;` before/after.
6. **The negative smoke — a lie about locality is refused.** Set
   `OPS_LOCAL_PROVIDER_URL=https://api.openai.com/v1` and run `triage run --limit 5`.
   Expected: refusal at startup, `avail_reason='local_endpoint_not_private'`,
   zero hosted calls, one skipped row. This is the ticket's whole thesis in one
   command; it must be run, not reasoned about.
7. **The personal lane, end to end in shadow:** seed the rules per the runbook,
   `opsctl capture-rules run --all` (shadow), then confirm with `psql` that (a)
   the Bank of America messages have `action='attributed'` on `personal`, (b)
   `SELECT count(*) FROM tasks` and `FROM external_refs` are unchanged, and (c)
   those ids no longer appear in triage's pending filter.
8. **`triage report` must read as "idle by design".** Run it after step 5 and
   confirm a human reading it would not open an incident — criterion 20's words,
   not just its counts.
9. Drafts and plan import still work on the general lane: `planimport propose`
   against a scratch file, and a drafts unit run against a fixture project with
   `ai_locality='any'`. If either now skips, criterion 25 or 10 was missed.

## What the answers changed (2026-08-28)

Recorded so a reader of the first draft is not misled.

- **Q1 → `ai_locality` defaults to `local_only`** (was open). Criterion 9 now
  fixes the default and pins the migration's statement ORDER; criterion 10 is new
  and covers the test-fixture consequence.
- **Q2 → migration seeds the project only; rules come from a measured sender list
  via `opsctl`** (criteria 11-13), keeping SWT-17's precedent.
- **Q2's accepted deviation → an UNMATCHED message is RESTRICTED, not general.**
  This is the largest change. Criterion 5 is rewritten; the "Ordering" section is
  new and states that SWT-21 gates triage on SWT-22 rather than the reverse;
  criterion 14 is demoted from safety property to volume property; criterion 16
  gains an honesty label because the thread fold can no longer change a triage
  outcome; criterion 21 drops the hard `OPENAI_API_KEY` requirement; criterion 26
  requires the runbook to say that an all-skipped report is the success state.
- **Q3 → an unclassified error skips the message; the pass raises on the pattern**
  (criterion 18), at >50% of restricted-lane attempts with a floor of 20. The
  number is labelled a guess in the SPEC and must be labelled a guess in the
  constant's doc comment.
- **Q4 → split.** The interface change ships at all three worker call sites
  (criteria 23-25); the `task_get_next` predicate is DROPPED and moved to "Out of
  scope" with its reasoning.
- **Spike folded in** (`docs/tickets/local-classifier-spike.md`): the restricted
  lane is not a degraded lane (0.90 vs 0.80 recall; hosted missed 3 of 5 HOA
  violation notices), and the measured local deployment shape (ollama on loopback
  or LAN) classifies as local under criterion 2 with no special case.

## Decisions made unilaterally (argue if wrong)

- **Locality from the endpoint, not the adapter type.** llama.cpp/ollama serve an
  OpenAI-compatible API, so type-based declaration is wrong on the first real
  deployment.
- **`Describe()` on the `Client` interface rather than an optional side
  interface.** An optional interface can be forgotten silently; a required method
  fails at compile time. Cost: every test fake gains a one-line method.
- **The request's class folds over thread context, not just the triaged message**
  (premise 5), kept as ONE spelling in both workers even though it is redundant in
  triage today.
- **`AttributionUnseen` and `AttributionUnmatched` remain distinct states** even
  though both map to restricted, because they are different facts and are reported
  separately.
- **Skips live in `ai_runs`, not a new table or `policy_decisions`.** Precedent
  and vocabulary discipline.
- **One aggregate skip row per pass when the local lane is down**, rather than one
  per message. The SWT-17 amplification incident is the reason, and after decision
  B the restricted population is the entire inbox.

## Deviations from this SPEC, accepted during implementation (2026-08-28)

Recorded here rather than left as a silent difference between the SPEC and the
code. Each fails closed at least as hard as what the SPEC pinned.

**1. The enums are ints, not strings, and `Locality` gained a third value.**
The SPEC sketched `ClassRestricted Class = ""` / `ClassGeneral = "general"` and a
two-valued `Locality`. Shipped: `iota` ints with `LocalityUnknown` as the zero
value. Reason: an unparseable or empty endpoint is genuinely a third state, and
naming it stops it from having to masquerade as "remote". Every zero value still
fails closed (`LocalityUnknown`, `ClassRestricted`, `AvailAbsent`, `DecideSkip`).

**2. No `LocalOnly(c Class)` predicate.** `Decide` and `MostRestrictive` switch
on the PERMITTED case (`c != ClassGeneral` restricts), which is what makes an
unrecognised value fail closed. A `LocalOnly` helper would have been a second
place to get that polarity wrong.

**3. `Decision` is `{DecideSkip, DecideAllow}`, not
`{DecideSkip, DecideLocal, DecideGeneral}`.** `Decide` answers "may this be
processed at all"; `Router.Route` answers "by which client". Criterion 6's
"restricted content must never yield `DecideGeneral`" therefore moves up to the
router, where `TestRouter_RestrictedWithNoLocalClient_NeverTouchesGeneral`
asserts it with a call count rather than an enum comparison — a stronger check.

**4. `internal/capture/attribution.go` was not created.** The attribution read
lives in `internal/triage/store.go` and `internal/drafts/store.go`, in the SAME
query that resolves the project, so the class and the project cannot disagree.
It also keeps `internal/capture` free of a `provider` import.

**5. `probe` caches for a TTL rather than exactly once per pass** (criterion 22's
wording). A pass longer than the TTL re-probes. Safe direction: a local box that
came back mid-pass starts serving again instead of staying refused.

**6. `ai_runs.provider` now records the serving lane, not the constant
`"openai"`.** Not requested by the SPEC, but with two lanes the constant would
label every locally-processed row as hosted — the one question that column
answers, answered wrongly, in the audit trail. Applied to triage and drafts;
planimport keeps a constant because it is always `ClassGeneral`. Nothing reads
the column, so the 11 historical rows are unaffected.

**7. No index on `ai_locality`.** The SPEC did not ask for one; the first cut
added a partial index anyway and it was removed. Every read reaches `projects` by
primary key, and the table has tens of rows.

**8. `Route` returns `(nil, DecideSkip, no_general_provider)` when general
content has no hosted client.** Not in the SPEC. Since criterion 21 made
`OPENAI_API_KEY` optional, "no hosted client" is a supported triage
configuration, and the previous shape returned a nil client with `DecideAllow`
for every caller to dereference.

**9. `AssembleContext` classifies ALL inbound thread messages, not only the ones
the prompt renders.** Criterion 15 says "every thread message it includes in the
prompt", and the prompt query has `LIMIT 10` plus a `sent_at` bound. The class
fold reads a superset, which is the safe direction, at the cost of an unbounded
scan on a very long thread. `internal/drafts/store.go` does it the tighter way —
same query as the bodies — and is the better pattern to copy.

**10. Outbound thread messages are excluded from the fold.** Not in the SPEC, and
found only in re-review. The capture engine filters `direction = 'inbound'`
(invariant 5), so an outbound message can never carry a capture decision:
classifying one as `unseen` meant "restricted forever", and since a Deliver task
exists to reply on a thread, the first send re-entering through ingestion would
have blocked that thread permanently. Measured at the time: 21,194 outbound
messages, zero decisions, 1,043 of 18,089 threads already affected.

The ground is **structural unclassifiability plus inheritance**: an outbound
message takes its conversation's class, which drafts folds anyway from the task's
own project and from every inbound sibling. It is NOT that our sends passed the
delivery policy gate — the first version of this justification said that and it
is false. `direction='outbound'` is set when the From address is one of the five
own accounts, so it is mostly mail typed in Gmail by hand: 21,194 such messages
against 1 delivery row. The gate also answers disclosure to the recipient, which
is a different question from disclosure to a hosted API.

*Residual this does not cover*, named rather than left implicit: the outbound
body is still rendered into the prompt — excluded from the class fold, not from
the conversation. A hand-written reply that pastes personal material into a
client thread introduces content no inbound sibling holds and no project
attribution describes, and it reaches the hosted lane unclassified. Dropping
outbound from the prompt would break the draft worker's job; folding it
restricted breaks every replied-on thread. Accepted gap.

*Qualification to criterion 16*, which claims the fold "is what would stop the
thread hole if a later ticket ever makes unmatched general again": that holds for
inbound neighbours only. Drafts enforces the inheritance rule because it always
folds the Deliver task's own project; **triage does not** — it folds the focus
message and its inbound neighbours, and nothing there stands for the thread's own
class. Harmless today (triage's focus message is `unmatched`, hence restricted,
by construction), but a ticket widening that inbox must supply a thread-level
class before relying on the fold.

**11. `cmd/drafts` and `cmd/planimport` do NOT get a local lane.** They were
wired for one and it was reverted: criterion 21 names `cmd/triage`, and neither
worker implements criterion 18's tier 1, so an `ErrUnavailable` there would
become a hard error and a non-zero exit — a busy local box looking like an
outage. Their routers are built with a nil local lane, which is exactly
criterion 24: restricted content skips rather than being processed by anything.
SWT-22 adds the lane with the skip semantics that make it safe.

## Post-review correction: the drafts guard shipped inert (2026-08-28)

Worth recording because it is the fourth or fifth appearance of the same failure
in this repo. The first implementation wired `drafts.Run` to the boundary and
added `internal/drafts/locality_skip_test.go`, which passed — while
`DeliverTasks` never selected `p.ai_locality`. Nothing populated
`DeliverTask.ProjectLocalOnly` outside test fixtures, so in production every
Deliver task folded to `ClassGeneral` and reached the hosted lane, including on a
`local_only` project.

The unit test could not have caught it: the unit test is the thing supplying the
value. The general shape — **a predicate whose discriminating column is a
constant in production** — now has an integration test per instance, here
`internal/drafts/locality_store_integration_test.go`, which makes Postgres
produce the value instead of a fixture.

The fix then introduced its own version of the same failure, in the opposite
direction: with the column armed, outbound neighbours (structurally undecidable,
per invariant 5) restricted every replied-on thread forever. Also invisible to
every test, because every fixture thread contained inbound messages only. Both
are now covered by tests whose fixtures are shaped like production rather than
like the assertion.

## Future work (not this ticket)

- **SWT-22 — the local classifier and adapter.** Its inputs are recorded in
  `docs/tickets/local-classifier-spike.md`: recall-first on a rare class; sender
  is NOT the discriminator (the same BoA address sends ~800 notifications and the
  occasional payment-due notice); subject is (median 44 chars, 37% template
  repeats); accuracy is misleading (always answering "not actionable" scores
  99.8%); self-reported confidence is a constant and must not be a threshold; set
  `"think": false`; per-sender prompts are rules in a costume, so a missing
  context fact belongs in a column.
- An executor tool to change `projects.ai_locality` (humanOnly, audited).
- `task_get_next`'s locality predicate, with the ticket that gives `personal`
  tasks at all — where it will actually discriminate something.
- PDF/attachment extraction from `rfc822_b64` (the spike's Pines ceiling).
- A dashboard lane showing skipped-for-locality counts, once the dashboard grows
  an AI-runs view.
