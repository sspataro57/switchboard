> Jira: SWT-21

# provider-locality — personal content may only be processed by a local provider

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
   mode predicate** (`internal/triage/store.go:50-67`). Consequence, and it is
   the lever this ticket pulls first: a **shadow** attribution already removes a
   message from triage's inbox. A `personal` rule needs no live capture pass to
   take effect.
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
5. **A leak path exists that message-level attribution alone does not close.**
   Triage assembles thread context from `normalized_messages` by `thread_id`
   (`store.go:103-108`) with no reference to the neighbours' capture decisions. An
   `unmatched` message whose thread contains a `personal`-attributed sibling
   carries that sibling's body to the hosted API. The same is true of the drafts
   worker's thread context.

Measured context supplied with the ticket and taken as given (do not re-derive;
re-measure at verification time rather than pinning literals — the corpus is
live): capture claims 33,048 of 49,416 messages, leaving triage 16,369 that are
disproportionately personal (Bank of America ~803, a mortgage servicer,
CareCredit, an HSA, a medical biller). 11 `ai_runs` exist, all 2026-07-12, and no
`ai_extractions` from any financial sender. **Nothing has leaked yet. This ticket
exists to keep that true.**

## Goal

Make "which provider may see this content" a **total, pure, fail-closed function
of the content's capture attribution and the adapter's destination**, enforced at
the provider seam rather than at a call site, with refusals recorded; and seed the
`personal` project plus attribution-only capture rules so financial/personal mail
leaves the hosted-triage inbox immediately.

**Usable alone:** after this ticket, with no local model anywhere and no
classifier written, (a) ~800+ Bank of America messages and their siblings are
attributed to `personal` and are no longer in triage's inbox, (b) any code path
that tries to send personal content to a non-local endpoint refuses and records
the refusal, and (c) the seam the local classifier will plug into exists and is
tested. The classifier ticket adds a worker, not plumbing.

## Design shape (the three decisions this SPEC makes)

**A. Locality is derived from the destination, not from a type name or a config
flag.** Every `provider.Client` must describe the endpoint it will POST to;
`provider.LocalityOf` classifies that endpoint. This is deliberate and it is the
answer to "what cannot be forged or forgotten": the likely local stack is
llama.cpp's **OpenAI-compatible** server, so the same `provider.OpenAI` adapter
will serve both lanes with a different base URL. A locality declared by adapter
type would therefore be a lie in both directions. Keying on the destination also
means the exact configuration change the ticket fears — repointing a worker at a
hosted API — is the change that trips the guard.

**B. The class of a request is the most restrictive class of every piece of
content in it.** Not just the message being triaged: its thread context too
(premise 5).

**C. Unavailability is a first-class outcome, not an error.** Absent local
client, present-but-not-local endpoint, and present-but-unreachable all produce
the same `Skip` decision, recorded distinctly from `ok` and from `error`.

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
5. `provider.ClassOf(state AttributionState, projectLocalOnly bool) Class` maps
   the three capture states, and the mapping is pinned by test:
   - `AttributionUnseen` (no `capture_decisions` row) → `ClassRestricted`
   - `AttributionUnmatched` (latest action `unmatched`) → `ClassGeneral`
   - `AttributionProject` → `ClassRestricted` iff the project's `ai_locality` is
     `local_only`, else `ClassGeneral`
   - any other/zero state → `ClassRestricted`
   The unseen/unmatched split is SWT-17 §8's three-state model applied here:
   unmatched is a *decision* by the deterministic engine that the message belongs
   to no project; unseen means the engine has not looked, and guessing on its
   behalf is exactly the trap that SPEC named.
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
   accordingly and tested with `httptest` (closed listener / `httptest` server
   whose handler sleeps past a 1ms client deadline) — no live provider, ever.
9. Migration `0016_provider_locality.sql` adds
   `projects.ai_locality TEXT NOT NULL CHECK (ai_locality IN ('local_only','any'))`
   and names every existing project row explicitly rather than relying on the
   default. Default value for NEW rows: see OPEN QUESTION 1.
10. A `personal` project exists with `ai_locality='local_only'`, `client` NULL,
    `execution='manual'`, `delivery='dashboard'`. Where it is created (migration
    vs. operator command) is OPEN QUESTION 2.
11. Capture rules attribute the personal/financial senders to `personal` as
    **attribution only** — `external_system` NULL, so `action='attributed'`, no
    task, no `external_refs` row, in either mode. An integration test seeds
    equivalent rules over fixture messages and asserts: `action='attributed'`,
    `project_id = personal`, zero rows added to `tasks`, zero to `external_refs`.
12. A message attributed to `personal` is **not in triage's inbox**: an
    integration test writes a shadow `attributed` decision and asserts
    `PendingMessages` does not return it. (This is existing behaviour of
    `store.go:50-67`; the test pins it because this ticket now depends on it as a
    safety property rather than as an optimisation.)
13. `triage.AssembleContext` returns, per message, a tri-state attribution
    (`unseen` / `unmatched` / project + `ai_locality`) **for the message and for
    every thread message it includes in the prompt**. The existing project lookup
    (`store.go:171-175`, latest decision then its project) is extended, not
    forked: "latest" keeps meaning the newest row, full stop.
14. `triage.Run` computes the request class as
    `provider.MostRestrictive(class(message), class(each thread message)…)` and
    routes through `provider.Router`. `Run`'s signature takes `*provider.Router`,
    **not** `provider.Client` — so a fallback would require constructing a second
    router, which is a visible act rather than a forgotten one.
15. On `DecideSkip`, triage: makes no provider call of any kind; writes **no**
    `ai_extractions` row (so the message stays in the pending filter and is
    retried next pass — "left for the next pass" is a consequence of the existing
    filter, not new bookkeeping); records the refusal per criterion 17; does not
    increment the error counter; and does not cause a non-zero exit.
16. A restricted call that fails with `provider.ErrUnavailable` (including a
    deadline exceeded) takes the same skip path as criterion 15 — never an error
    path, never a second adapter. A test drives this with a fake local client that
    returns `ErrUnavailable`, and asserts the general fake recorded zero calls.
17. **Refusals are recorded, and the record is bounded.**
    - Pass-level: when the local lane is not `AvailReady`, the pass records ONE
      `ai_runs` row with `worker_type='triage'`, `provider='local'`, `status='skipped'`,
      and `input` = `{reason, class:"restricted", skipped_count:N, message_ids:[≤100]}`.
      One row per pass, not per message — the SWT-17 amplification landmine
      (49,415 messages × every pass) is the reason.
    - Per-message: a message-specific skip (unreachable mid-pass, unclassified
      error) records one `ai_runs` row with the message id and its reason. Bounded
      by how far the pass got.
    - `status='skipped'` is a value that `status='ok'` and `status='error'` never
      take, and a skip NEVER writes an `ai_extractions` row. So "the model looked
      and found nothing" (an extraction with `actionable=false`) and "no permitted
      provider looked" are structurally distinct rows, per the repo's rule that a
      poller-did-not-run alarm must not share a channel with a poller-found-nothing
      alarm.
18. `triage report` prints a `skipped:` line, broken down by reason
    (`no_local_provider`, `local_endpoint_not_private`, `local_unreachable`,
    `unclassified_error`), reading `ai_runs` where `status='skipped'`. Today's
    report joins only `status='ok'` rows (`report.go:20`), so skips would
    otherwise be invisible — which is the failure mode, not the fix.
19. `cmd/triage` builds the Router: general lane from `OPENAI_API_KEY` as today,
    local lane from `OPS_LOCAL_PROVIDER_URL` (+ `OPS_LOCAL_MODEL`) when set. If
    `OPS_LOCAL_PROVIDER_URL` is set to a non-local endpoint, the local lane is
    **absent** (not "local"), the process logs the refusal at startup, and the
    reason recorded on skips is `local_endpoint_not_private`. Unset →
    `AvailAbsent`. Neither case is a startup error: the general lane still works
    for `ClassGeneral`.
20. Availability is probed, not assumed. A local client may implement
    `provider.Prober` (`Probe(ctx) error`); the Router probes it ONCE per pass
    with a short deadline. A local client that does not implement `Prober`, or
    whose probe fails, is `AvailUnreachable` — "declares itself local but is
    unreachable is not a permitted processor right now", including the case where
    the declaration is all there is.
21. **No worker mints its own client.** A structural test (plain unit test, no
    build tag, no db — the `internal/textmatch/callsites_test.go` shape) scans
    `internal/triage`, `internal/drafts`, `internal/planimport` and `internal/capture`
    and fails any file mentioning an adapter constructor (`provider.NewOpenAI`,
    or any `provider.New*`) or a bare `provider.Client` parameter in a `Run`
    signature. Adapters are constructed in `cmd/` only.
22. `internal/drafts`: `DeliverTasks` also selects `p.ai_locality`, and `drafts.Run`
    routes through the same Router with the class folded over the project and every
    thread message included in the prompt. Same skip semantics (criterion 15),
    logged onto the Deliver task with the existing `task_append_log` /
    `draft_skip` shape (`drafts.go:126-134`), not as an error.
    **State honestly in the code comment:** this path is ARMED BUT INERT today —
    `personal` is attribution-only, so it has no tasks and therefore no Deliver
    tasks. It is here because a hand-created personal task is one `create_task`
    away, not because it fires now.
23. `internal/planimport`: passes `provider.ClassGeneral` explicitly at its single
    call site, with a comment stating why (the input is a plan file a human named
    on the CLI, not captured message content). Explicit, because criterion 3's zero
    value would otherwise silently restrict it.
24. `tools.getNext` (`internal/tools/getnext.go:48-57`) excludes tasks whose
    project has `ai_locality='local_only'`. **Same honesty label as criterion 22
    and stronger:** this predicate's discriminating column is a constant in
    production (every project is `any` except `personal`, which has no tasks and
    whose `client` is NULL, so the existing `p.client = $1` already excludes it).
    It is a no-op today by two independent mechanisms. Its test must say so and
    must fabricate the state deliberately — per the SWT-18 lesson, a test that
    proves a constant-column predicate works is proving its own fixture.
25. `docs/runbooks/provider-locality.md` records: the `personal` sender list and
    the exact `opsctl capture-rules add` commands used; the env contract
    (`OPS_LOCAL_PROVIDER_URL`, `OPS_LOCAL_MODEL`); what a skip looks like in
    `triage report`; and the sentence that a fallback to a hosted provider is
    never correct. A structural test asserts the runbook exists and contains the
    no-fallback sentence (the `TestRunbook_DocumentsCaptureBeforeTriage` shape,
    `internal/capture/rules_structure_test.go:125`) — the rule is prose-shaped and
    the next contributor's instinct is to "fix" the skip into a fallback.

## Data model changes

Migration **`0016_provider_locality.sql`** (highest existing is
`0015_capture_rules.sql`; production `schema_migrations` must be at 0015 before
this ships — see the five-migrations-behind entry in institutional knowledge).

```sql
ALTER TABLE projects ADD COLUMN ai_locality TEXT NOT NULL DEFAULT <see Q1>
  CHECK (ai_locality IN ('local_only','any'));
UPDATE projects SET ai_locality = 'any';                 -- name existing rows explicitly
INSERT INTO projects (name, slug, ai_locality) VALUES    -- subject to Q2
  ('Personal', 'personal', 'local_only') ON CONFLICT (slug) DO NOTHING;
```

Notes:

- `ai_locality` is a CHECK-constrained column, **not** a key in `projects.policies`
  jsonb. A jsonb key is absent-by-default and absent is the exact state this
  ticket must treat as unsafe; a NOT NULL CHECK column cannot be absent.
- No new tables. Refusals live in `ai_runs` (`status='skipped'`), which is the
  established "a worker considered this" log and already carries
  `provider`/`model`/`status`/`input`. It is deliberately **not**
  `policy_decisions`: that table answers "was this TOOL CALL permitted", is written
  only by `audit.PGStore.RecordPolicy` inside `Executor.Execute`, and a triage
  provider refusal is not a tool call. Same reasoning `0015` wrote down for
  `capture_decisions`.
- No change to `capture_rules` / `capture_decisions` schemas. The personal rules
  are ROWS in the existing table.
- Seeded-row safety check performed: every integration cleanup deletes projects
  by slug or slug-LIKE (27 call sites, all scoped), so a migration-seeded
  `personal` row is not collateral damage.

## API / MCP tool changes

No new executor tools and no new MCP tools. This ticket adds no capability an
agent can call; it removes one from the workers.

Touched via the executor path (invariant 3), unchanged shapes:

- `create_task` / `link_external_ref` / `task_append_log` — the capture engine's
  existing calls. The personal rules have `external_system` NULL, so in live mode
  they reach **none** of them; the decision row is written by
  `internal/capture`'s own append-only log, exactly as today.
- `task_get_next` — handler query gains one predicate (criterion 24). Validation,
  policy and audit path unchanged; still reached only through
  `executor.Execute`.
- `capture_rule_add` — used as-is to seed the personal rules. It is `humanOnly`
  and off the MCP surface, which is what keeps an agent from re-pointing the
  personal lane.

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
- `internal/provider/locality.go` — Descriptor, Locality, Class, Availability,
  Decision, `LocalityOf`, `ClassOf`, `MostRestrictive`, `Decide`, `ErrUnavailable`.
  **Pure**: no `context` for the decision functions, no pgx, no net/http, no
  `os.Getenv` (the `internal/capture/rules.go` discipline, and criterion 2 of
  SWT-17 is the model to copy).
- `internal/provider/router.go` — Router + probe.
- `internal/provider/locality_test.go`, `internal/provider/callsites_test.go`
- `internal/capture/attribution.go` — the SQL reader returning a per-message
  tri-state attribution + project locality. Returns capture's own struct, **not**
  a `provider` type: `internal/capture/rules.go` is scanned for an
  `internal/provider` import and the package should keep that property in spirit.
- `docs/runbooks/provider-locality.md`

Modified:
- `internal/provider/provider.go` (Client gains Describe), `openai.go`
  (Describe + `ErrUnavailable` wrapping), `openai_test.go`
- `internal/triage/triage.go` (Run takes `*provider.Router`; class fold; skip
  path), `store.go` (attribution tri-state for message + thread), `report.go`
  (skipped lane), `worker_test.go`, `integration_test.go`,
  `capturedecisions_integration_test.go`
- `cmd/triage/main.go` (Router construction, `OPS_LOCAL_PROVIDER_URL`)
- `internal/drafts/drafts.go`, `internal/drafts/store.go`,
  `internal/drafts/worker_test.go`, `cmd/drafts/main.go`
- `internal/planimport/planimport.go`, `cmd/planimport/main.go`
- `internal/tools/getnext.go` + `internal/tools/getnext_ordering_integration_test.go`
- `docs/runbooks/capture-rules.md` (cross-reference the personal lane)

## In scope / Out of scope

**In scope**
- The provider-locality decision functions, the Router, the typed unavailability,
  and the structural tests.
- The `personal` project, `projects.ai_locality`, migration 0016.
- Attribution-only capture rules for the financial/personal senders (rows, plus
  the runbook that records them).
- Triage's routing + skip + record + report lane; drafts and planimport call-site
  declarations; `task_get_next` predicate.

**Out of scope — explicitly, including the things it is tempting to bundle**
- **The local classifier itself**: model choice, prompt, schema, the
  actionable/not-actionable decision, recall tuning. This ticket ships the socket,
  not the plug.
- **Hardware / serving stack**: the RX 570 (Polaris, gfx803 — ROCm is out,
  llama.cpp+Vulkan is the probable stack), the box, its scheduling priority, its
  systemd unit. Nothing here names a model or a GPU.
- **Going live on capture rules** (`CAPTURE_RULES_MODE=live`). The personal rules
  work in shadow — that is the point of premise 2 — and flipping the mode is a
  kube-repo change with its own ordering constraint.
- **Triage going live** (creating tasks). Unchanged: triage still creates nothing.
- **A tool to change `ai_locality`.** The column is set by migration and by psql.
  An executor tool for it (`project_set_ai_locality`, humanOnly) is future work.
- **Outbound**: personal deliveries, a personal kill switch, policy-matrix rows
  for a personal channel. `personal` produces no tasks and therefore no
  deliveries.
- **Retro-scrubbing `ai_runs.input`** for the 11 existing runs. They predate the
  financial corpus and carry no financial sender (given, and re-checkable).

## Invariants that apply

1. **Raw-first** — untouched. No connector changes; `raw_source_items` writes are
   not on this path. The personal rules read `normalized_messages`, which were
   raw-first when ingested.
2. **One funnel** — no new task-like table. `personal` is a row in `projects`;
   the refusals are rows in the existing `ai_runs`; the attributions are rows in
   the existing `capture_decisions`. If this SPEC had proposed a
   `restricted_messages` table it would be a violation — it does not.
3. **Everything through the executor** — the only tool surface touched is
   `task_get_next`'s handler query and the existing `capture_rule_add`. No new
   tool, no handler invoked outside `Executor.Execute`, no raw_sql. The skip
   record is NOT smuggled through the executor: it is a worker's own bookkeeping
   row in `ai_runs`, the `ai_runs`/`ai_extractions`/`capture_decisions` precedent.
4. **Nothing external without a delivery row** — nothing here sends. Note the
   sharper reading this ticket adds: an outbound POST to a hosted LLM is not a
   `deliveries` channel and never will be, so it needs its own gate, which is
   what criteria 1-8 are.
5. **Own-message loop closure** — untouched. Triage's `direction='inbound'`
   filter and capture's are unchanged; the personal rules match inbound mail
   only, so switchboard's own sends cannot be re-attributed.
6. **Stealth attribution** — untouched. Nothing here writes client-visible words.
7. **Orchestrator purity** — reinforced, and generalised. The orchestrator never
   imports a provider adapter; this ticket makes the same property checkable for
   the *routing* decision: `provider.LocalityOf`, `ClassOf`, `MostRestrictive`
   and `Decide` are pure functions in a file with no db, no network and no env
   read, unit-testable with zero infrastructure — the `internal/capture/rules.go`
   / `rules_structure_test.go` pattern, one ticket old and proven.

Additionally, the two landmine classes named in the brief:

- **Actor-prefix checks are transport labels, not trust boundaries** (SWT-19's
  go-live gate). This gate keys on *the destination address of the HTTP request*
  and *the content's capture attribution* — never on who is calling. There is no
  actor string anywhere in criteria 1-8. The counter-example that broke the last
  one (`drafts:gpt` calling the executor directly) has no analogue here: drafts
  goes through the same Router as triage.
- **A predicate whose discriminating column is constant is a no-op** (SWT-18).
  Criterion 24 is exactly that shape and says so out loud rather than claiming
  protection it does not provide. Criteria 22 (drafts) is armed-but-inert for a
  different reason and says so too. The criteria that are NOT inert are 11-18:
  the personal rules claim real messages today, and the class fold over thread
  context (premise 5) is a live path with a measurable population.

## Sibling patterns to copy

- **Pure decision + structural test:** `internal/capture/rules.go` (the header
  comment explains why the file boundary IS the guarantee) and
  `internal/capture/rules_structure_test.go:37` `TestRulesGo_IsPure` — copy the
  banned-token scan verbatim for `internal/provider/locality.go`, adding
  `provider.NewOpenAI` to the ban list for worker packages.
- **Call-site scan that cannot be forgotten:** `internal/textmatch/callsites_test.go`
  — plain unit test, no build tag, scans sibling source files and fails on the
  omission. Criterion 21's scan is the same mechanism.
- **Fake-provider worker tests:** `internal/triage/worker_test.go` (fake
  `provider.Client` + fake `Store`, no db, no network) and
  `internal/drafts/worker_test.go`. Criteria 7, 14, 15, 16 extend these; never a
  live LLM.
- **httptest adapter tests:** `internal/provider/openai_test.go:110` — criterion 8
  extends it with a transport-failure case.
- **Three-state attribution:** `internal/triage/store.go:50-67`'s comment is the
  canonical statement of unseen ≠ unmatched; criterion 5 is that model lifted to
  the class function. Read it before writing `ClassOf`.
- **Partial-index / ON CONFLICT discipline:** `migrations/0015_capture_rules.sql:135`
  — not needed here (0016 adds no partial index), but read it before adding one.
- **Runbook-as-tested-artifact:** `internal/capture/rules_structure_test.go:125`
  `TestRunbook_DocumentsCaptureBeforeTriage` — criterion 25 copies it.
- **Queue claims:** untouched; no new claim code. (`FOR UPDATE SKIP LOCKED` in
  `internal/tools/claim.go:51` if the getNext change ever grows a claim, which it
  should not.)

## Verification protocol

Before commit:

1. `go test ./...` — must include the pure locality table tests, the Router
   zero-hosted-calls test, the skip-path worker tests, and the two structural
   scans. These need no db, no broker and no network.
2. `make integration` (`make db-up && make migrate` first; local URL
   `postgres://ops:ops@localhost:5433/ops?sslmode=disable`; runs `-p 1`, join the
   mutual-cleanup pact, clean up by slug in FK order).
3. **Migration state before any image ships:**
   `psql -h 192.168.50.49 -U ops -d ops -tAc "SELECT max(version) FROM schema_migrations"`
   must read `0015` before, `0016` after applying.
4. **Re-measure, do not trust the literals in this SPEC.** Read-only against
   production:
   - the size of triage's inbox now (`capture_decisions` latest `unmatched`);
   - how many of those the new personal rules would claim (run
     `opsctl capture-rules run --all` in **shadow**, then
     `opsctl capture-rules report --since 168h`);
   - **the thread-sibling population** — how many still-unmatched messages share
     a `thread_id` with a `personal`-attributed message. That number is the live
     value of criterion 14's fold; if it is zero, say so rather than claiming the
     fold protects something.
5. **Negative smoke, the important one.** With `OPS_LOCAL_PROVIDER_URL` set to
   `https://api.openai.com/v1`, run `triage run --limit 5`. Expected: the pass
   refuses the local lane at startup with `local_endpoint_not_private`, every
   restricted message is skipped, ZERO hosted calls are made for restricted
   content, and one `ai_runs` row with `status='skipped'` exists. Confirm with
   `psql`:
   `SELECT status, provider, input->>'reason', input->>'skipped_count' FROM ai_runs ORDER BY id DESC LIMIT 5;`
6. **Positive-ish smoke without a model.** With `OPS_LOCAL_PROVIDER_URL` unset,
   `triage run --limit 5` then `triage report` — the report must print the
   `skipped:` line with reason `no_local_provider`, and `ai_extractions` must not
   have grown for those messages.
7. **The personal lane, end to end in shadow:** seed the rules per the runbook,
   `opsctl capture-rules run --all` (shadow), then confirm with `psql` that (a)
   the Bank of America messages have `action='attributed'` on `personal`, (b)
   `SELECT count(*) FROM tasks` and `FROM external_refs` are unchanged, and (c)
   those message ids no longer appear in triage's pending filter.
8. Do NOT run `triage run` unbounded against production during verification —
   `--limit` every invocation. The general lane still sends real bodies to
   OpenAI for `ClassGeneral` messages, and that is the behaviour this ticket
   narrows, not the behaviour it removes.

## Open questions

**Yes — see `docs/tickets/provider-locality_OPEN_QUESTIONS.md`. This SPEC is
provisional until they are answered**; questions 1, 2 and 4 change acceptance
criteria 9, 10-11 and 22-24 respectively.

## Decisions made unilaterally (argue if wrong)

- **Locality from the endpoint, not from the adapter type.** Rationale under
  "Design shape A": llama.cpp serves an OpenAI-compatible API, so type-based
  declaration is wrong on the first real deployment.
- **`Describe()` on the `Client` interface rather than an optional side
  interface.** An optional interface can be forgotten silently; a required method
  fails at compile time. Cost: every test fake gains a one-line method.
- **The request's class folds over thread context, not just the triaged message**
  (premise 5). Without it the guard has a hole the size of the thread.
- **`unmatched` → `ClassGeneral`.** Restricting the whole unmatched pile would
  stop triage entirely, and unmatched is a positive decision by the deterministic
  engine, not an absence. Unseen is the absence, and it is restricted.
- **Skips live in `ai_runs`, not a new table or `policy_decisions`.** Precedent
  and vocabulary discipline.
- **One aggregate skip row per pass when the local lane is down**, rather than one
  per message. The SWT-17 amplification incident is the reason.

## Future work (not this ticket)

- The local classifier: recall-first on a rare class (~32 of 16,369 messages
  carry a payment-due / statement-ready shape). Record for that ticket, because
  it is measured and easy to get wrong: **sender is not the discriminator** — the
  same Bank of America address sends ~800 notifications and the occasional
  payment-due notice. Subject is (median 44 chars, 37% template repeats). The
  error profile is asymmetric: a missed payment notice costs a late fee, a false
  positive costs dismissing a task, so **recall on a rare class is the goal and
  accuracy is a misleading metric** — always answering "not actionable" scores
  99.8%.
- An executor tool to change `projects.ai_locality` (humanOnly, audited).
- Extending locality classification to the Claude Code execution workers as a
  real gate rather than criterion 24's inert predicate — that needs a personal
  project that actually holds tasks, which needs the classifier first.
- A dashboard lane showing skipped-for-locality counts, once the dashboard grows
  an AI-runs view.
