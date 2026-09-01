> Jira: SWT-20

# upwork-delivery-provenance — record the source thread, and give a delivery an indexed identity

**Status: FINAL.** The one open question (Q1, actor scope of the reopening) was
answered **(a) — every actor, including `drafts:gpt`** — on 2026-08-31; the
reasoning is recorded in `docs/tickets/upwork-delivery-provenance_OPEN_QUESTIONS.md`.
The SPEC below already assumed that answer; nothing else changed.

## Source

Not a build-order step. It is the pair of findings the SWT-19 adversarial review
deferred, quoted verbatim from `docs/tickets/upwork-room-identity_SPEC.md`
("Adversarial review (Codex, 2026-08-26)", lines 993-1012):

> **Deferred to SWT-20, with a mitigation shipped here.**
>
> 3. *Delivery targeting has no provenance for the originating room.* `DeliverTasks`
>    resolves a target from the task's CLIENT, so a task raised by room A can be
>    drafted into room B — and since this ticket a wrong-room delivery can never
>    confirm, surfacing only via the reconciler. The real fix is recording which
>    thread a task came from, which needs a schema change.
>    **Mitigation shipped:** when a client has MORE THAN ONE roomed thread and the
>    task carries no provenance, the draft worker now refuses to target it at all,
>    routing to the existing "unresolvable — tell the human" path. Refusing is
>    reversible; a wrong-room send is not. Two production clients are in this state.
> 4. *Every outbound message locks every unresolved Upwork delivery.* The matcher
>    selects all `sent`+unconfirmed upwork rows `FOR UPDATE` and filters in Go, and
>    flagged rows never leave that set, so the cost is O(outbound × unresolved)
>    once the tier is live. Zero rows today. The fix is persisting indexed
>    structured delivery identity (client, optional room) to shortlist before
>    locking — a schema change, and the same one finding 3 needs.
>
> Both deferred items are **blockers for enabling the Upwork tier**, not for
> merging this: they concern a channel with zero deliveries in production.

Two later deferrals from the same ticket fold into finding 3 and are in scope
here because they are the same missing fact:

- pass two: *"the existence check does not bind the target to the task's client"*
  — `draft_delivery` proves the `target_ref` names **some** ingested thread, not
  that it belongs to this task's conversation (SWT-19 SPEC lines 1049-1060).
- pass four: because no binding existed, `upwork_chat` drafting was **closed for
  every actor** (`internal/tools/delivery.go:279-281`). Reopening it is this
  ticket's job, "together with the binding that makes it safe."

## Goal

Record, on the task, the `normalized_threads` row a task came from; make
`upwork_chat` delivery targeting a function of that recorded thread instead of a
choice among the client's rooms; and persist the delivery's client identity as an
indexed column so the confirm matcher shortlists candidates before it locks
anything.

**Usable alone** means: with nothing else deployed, an `upwork_chat` delivery can
be created again (the pass-four closure lifts), it can only be aimed at the
conversation its task was raised in, and one `connector-upworkcrm` run confirms it
while locking only that client's unresolved rows. The two production multi-room
clients stop being undraftable — for every task whose source thread is roomed,
which is ~99% of API-era traffic (SWT-19 fact 5). No orchestrator change, no
dashboard change, no drafts-worker deploy is required for that to be true.

## Premises verified by reading the code at `15d7f2c`

Cited so nobody re-derives them, and because two of them contradict the issue text
as written — the code moved under it in SWT-21.

1. **Finding 3's first sentence is out of date. `DeliverTasks` no longer resolves
   from the client.** `internal/drafts/store.go:216-241` (`taskThread`) resolves
   the thread from the task's own `external_refs` row — parent task first, then the
   Deliver child — joining `nt.thread_key = er.external_key`. The person route and
   `projects.client_person_id` are gone (SWT-17). So provenance is already *read*;
   what is missing is a provenance that can be *trusted* and that always exists.
2. **`external_refs` cannot be that provenance, for three independent reasons.**
   (a) `link_external_ref` is **agent-facing and MCP-listed**
   (`internal/mcpserver/schemas.go:63`, `internal/mcpserver/adapter_test.go:88`),
   its `external_key` is free text (`internal/tools/prci.go:64`), so a worker can
   fabricate a link to any thread and thereby aim a delivery. That is precisely
   the exposure the pass-four closure was written for; reading the same table
   harder does not close it.
   (b) The join is on `thread_key`, a TEXT value this repo has already re-spelled
   once (SWT-19 re-keyed 433 threads) — the SWT-19 SPEC's own "Interaction with
   SWT-17" section flags refs written before a re-key as silently dangling.
   (c) `external_refs` is **UNIQUE `(system, external_key)`** (IK, SWT-17 entry),
   so a thread key can belong to exactly one task, forever. Two tasks from one
   conversation is a normal thing; one of them would carry no provenance.
3. **The only writer of `deliveries.target_ref` in non-test code is
   `draftDelivery`** (`internal/tools/delivery.go:314-320`). `update_delivery`
   touches subject and body only (`delivery.go:370-375`); `approve/send/mark_sent`
   never write it. So a column derived from `target_ref` has exactly one write site
   to keep honest — and every other `INSERT INTO deliveries` in the repo is a test
   fixture (23 of them; `git grep "INSERT INTO deliveries"`).
4. **`upwork_chat` drafting is closed for every actor**
   (`internal/tools/delivery.go:279-281`), so nothing in this ticket can regress a
   live path: there is no live path.
5. **The matcher's candidate set is unscoped.**
   `internal/connector/upworkcrm/sink.go:420-425` selects every `upwork_chat` row
   at `status='sent'` with `sent_external_id IS NULL AND confirmed_at IS NULL`,
   across all clients, `ORDER BY id DESC FOR UPDATE`, and filters with
   `ParseThreadKey` + `SameConversation` + `textmatch.NormalizedPrefix` in Go
   (`sink.go:431-461`). The comment at `sink.go:410-416` already states the cost
   and that the reconciler never removes a row from the set.
6. **`SameConversation` requires client equality**
   (`internal/connector/upworkcrm/threadkey.go:159-167`): `message.ClientID !=
   delivery.ClientID` returns false before any room logic. This is the property
   that makes a client-scoped SQL shortlist provably equivalent to the full scan.
7. **The one-spelling rule is mechanically enforced.**
   `internal/connector/upworkcrm/keyspelling_test.go` fails any file under
   `internal/` where a raw string contains `upwork_crm:` together with `LIKE`,
   `split_part` or `||`, with two exemptions by exact path. Anything this ticket
   adds to SQL must therefore compare **extracted columns**, never key text.
8. **`drafts` calls `draft_delivery` with the PARENT (work) task id**
   (`internal/drafts/drafts.go:249-261`), and the delivery row hangs off the work
   task (`store.go:71`, `NOT EXISTS ... d.task_id = t.parent_id`). Provenance
   lookup must therefore work from the work task, and from a Deliver child if a
   human calls it there.
9. **`upworkTarget` is the code the mitigation lives in**
   (`internal/drafts/store.go:260-363`): it parses the ref, returns it when roomed,
   and otherwise runs a `ClientThreadPrefix` LIKE over the client's threads,
   ordered by `max(m.sent_at)`, preferring a roomed thread and **refusing when more
   than one roomed thread exists** (`store.go:335-351`). That whole second half is
   what this ticket deletes.
10. **`humanActor` is unexported** (`internal/policy/matrix.go:73-89`) and is the
    one definition of the human prefixes (`dashboard:`, `opsctl:`, `manual:`, with
    `policy.MCPTransportPrefix` stripped once). A handler that needs the same
    question must call the same function, not restate the prefixes.
11. **The capture engine has the thread available already**: its pending query
    joins `normalized_threads` (`internal/capture/rules_store.go:421`) but selects
    no thread id, and `pendingMessage` (`rules_store.go:180-185`) carries none.
    Task creation is `createRuleTask` → `create_task`, followed by a separate
    `linkRuleRef` → `link_external_ref` (`rules_store.go:712-766`).
12. **Highest migration is `0018_bulk_project_and_classify_flag.sql`.** This
    ticket's is **0019**.

## Production facts

This SPEC session had no shell, so nothing below was measured today. Two are
carried from SWT-19's verification (2026-08-27) and must be re-checked before
implementation; the third is a rule, not a number.

- `deliveries` where `channel='upwork_chat'`: **0**, and there has never been one.
  Re-check: `SELECT count(*) FROM deliveries WHERE channel='upwork_chat';` — the
  migration's CHECK constraint (below) assumes this. If it is ever non-zero on
  pg-main, stop and re-spec the backfill.
- Two clients have several roomed threads (3 and 2 rooms as of 2026-08-27). They
  are the clients the SWT-19 mitigation made undraftable. Read the count from the
  query, never from this line — the corpus is live (IK: "Do not assert production
  counts as frozen literals").
- Capture rules and triage are both in **shadow**, so the number of tasks carrying
  any usable provenance today is expected to be zero or near it. Re-check before
  claiming the smoke test exercises real rows:
  `SELECT count(*) FROM external_refs WHERE system='upwork_crm';`

**Backfill: none, and none is possible.** `tasks.source_thread_id` cannot be
reconstructed for tasks that already exist — nothing recorded which message raised
them. They stay NULL, which resolves to "no provenance", which refuses. That is the
safe state and it is the same state they are in today.

## Design

### 1. Provenance is a column on `tasks`, written by observers only

```sql
ALTER TABLE tasks ADD COLUMN source_thread_id BIGINT REFERENCES normalized_threads(id);
```

The fact being recorded is "which conversation raised this task". A FK to
`normalized_threads` says exactly that, survives a thread re-key (premise 2b), and
allows many tasks per thread (2c).

It is written by **`task_set_source_thread`**, a new spine tool: registered on the
executor, **not** in `internal/mcpserver/schemas.go`, exactly like
`capture_rule_add` / `capture_rule_set_enabled` (IK: "an agent must not be able to
redirect the funnel"). The structural protection is the MCP schema list, not an
actor-prefix test — the IK entry on `ViaMCP` is explicit that a prefix check is a
transport label, and the counter-example (`drafts:gpt`) is in this repo.

Semantics:

- `{task_id, thread_id}`; both must exist. The thread FK does the existence work.
- Writing the SAME thread id again is an idempotent success (replays are normal:
  the capture driver may re-run a pass).
- Writing a DIFFERENT thread id on a task that already has one is **refused**, with
  an error naming the current value. Provenance is a recorded observation, not a
  mutable pointer; silently re-aiming an existing task's future deliveries is the
  failure this ticket exists to prevent. Correcting a genuinely wrong value is a
  psql statement plus a note, not a verb.

Callers in this ticket: the capture engine (`rules_store.go`, after
`createRuleTask` and beside `linkRuleRef`, actor `capture:{connector}`), and
`opsctl call` for humans. `create_task` keeps its current argument shape — adding
`source_thread_id` there would put a forgeable provenance field on an MCP-listed,
agent-facing tool.

`external_refs` is unchanged and keeps its dedup job. This ticket adds a fact
alongside it; it does not fork the vocabulary.

### 2. One reader of provenance, in one place

```go
// internal/store/provenance.go
type SourceThread struct{ TaskID, ThreadID int64; ThreadKey string }
func TaskSourceThread(ctx context.Context, q Querier, taskID int64) (SourceThread, bool, error)
```

Resolves on the named task, then one level up through `parent_id` (premise 8 —
`draft_delivery` is called on the work task, a human may call it on the Deliver
child). Two lookups, bounded, the same coverage `drafts.taskThread` has today.

Both `internal/drafts/store.go` and `internal/tools/delivery.go` call it. Two
spellings of the provenance query is the drift this repo has paid for four times;
`internal/store` already exists for exactly this (`UnconfirmedNoteMarker`).

### 3. Targeting: the recorded thread, verbatim

`internal/drafts/store.go`:

- `resolve` reads provenance through `store.TaskSourceThread` for the upwork
  branch. **The `external_refs` join stays for gmail and jira** — it is their only
  route and nothing about delivery correctness on those channels changes here.
- `upworkTarget` collapses to: parse the provenance thread's key; return it. The
  `ClientThreadPrefix` query, the roomed preference, the `max(m.sent_at)` ordering
  and the multi-room refusal (`store.go:292-362`) are **deleted**.
  - roomed provenance → the roomed key. Exact room, no choice made.
  - unroomed (legacy) provenance → the legacy key. That is the truthful statement
    "this client's conversation, room not recorded by the source"; the human types
    the message, and `SameConversation`'s legacy tolerance keeps it confirmable.
  - no provenance → `""` → `dt.Channel = ""` → `drafts.go:143-154`'s existing
    "unresolvable — tell the human" log. Unchanged path, now the only fallback.

**"Never to the most recent room"** is satisfied structurally: after this change no
code path in `drafts` looks at any thread other than the recorded one.

### 4. `draft_delivery` reopens, bound server-side

The pass-four closure (`delivery.go:279-281`) is replaced by a binding, in the
handler (invariant 3: this is inside `Executor.Execute`, after validate and policy):

1. Resolve `store.TaskSourceThread(task_id)`. **No provenance → refuse, every
   actor.** The error says the task records no source conversation and names
   `task_set_source_thread`.
2. Provenance whose `thread_key` is not an upwork key → refuse (a gmail-raised task
   cannot be delivered into Upwork by naming a target).
3. Canonicalise the supplied `target_ref` with `ParseThreadKey` + `ThreadKey`
   (unchanged, `delivery.go:224-228`).
4. Bind:
   - `target_ref` == the provenance key → accept. This is the automated path; the
     drafts worker always produces exactly this after §3.
   - otherwise, the target must (i) parse, (ii) name an ingested
     `normalized_threads` row, (iii) have the **same client id** as the provenance
     — and the caller must be a human actor (`policy.HumanActor`, exported from the
     existing unexported `humanActor`, premise 10). This is finding 3's "explicit
     human choice": a human moving the reply into another room *of the same
     conversation partner*.
   - anything else → refuse.
5. The `EXISTS (thread_key=...)` check at `delivery.go:241-250` is subsumed: the
   accepted target is resolved to a `normalized_threads.id` and stored in
   `deliveries.thread_id`, so the FK proves existence.

The client binding — the thing pass two deferred — now holds for **every** actor.
The actor test in step 4 decides only who may *choose among rooms of the bound
client*, never whether the binding applies. Say that in the code comment; a reviewer
reading `HumanActor` in a delivery handler will otherwise (correctly) reach for the
IK entry on actor prefixes.

### 5. Delivery identity: one indexed column, written in Go

```sql
ALTER TABLE deliveries ADD COLUMN target_client_ref TEXT;

ALTER TABLE deliveries ADD CONSTRAINT deliveries_upwork_identity_check
  CHECK (channel <> 'upwork_chat' OR (target_client_ref IS NOT NULL AND thread_id IS NOT NULL));

CREATE INDEX deliveries_upwork_unconfirmed_idx ON deliveries (target_client_ref)
  WHERE channel = 'upwork_chat' AND status = 'sent'
    AND sent_external_id IS NULL AND confirmed_at IS NULL;
```

`target_client_ref` is filled by `draftDelivery` from `ParseThreadKey(target_ref).ClientID`
— **the same parse that produced the stored `target_ref`**, in Go, at the one write
site (premise 3). No SQL builds or dissects a key, so `keyspelling_test.go` stays
green and there is still one spelling.

The CHECK is what makes the shortlist safe rather than hopeful. A derived column
that some path forgets to write is this repo's most expensive recurring bug (IK:
"a guard whose column no query selected"; "test the column, not the fixture"). Here
the omission is caught by Postgres at INSERT, loudly, in every fixture and every
future write path, instead of silently shrinking a candidate set. It is free today:
zero `upwork_chat` rows exist (fact 1). It costs a fixture update in the six test
files that insert upwork deliveries by hand — that cost is the point.

`thread_id` for `upwork_chat` is the same column gmail already uses (0006); its
readers are channel-scoped (`delivery.go:499` inside the gmail branch,
`capture/observe.go:89` on the Gmail channel only), so setting it here adds a
FK-backed provenance record on the delivery without touching any of them. Verify
that claim with `git grep -n "d\.thread_id\|deliveries.*thread_id"` before writing.

**No `target_room_ref`.** Decided, against the issue text, with the reason stated
in "Decisions made unilaterally" (§D3): the only SQL predicate the shortlist may
carry is one *implied by* `SameConversation`, and client equality is the only such
predicate. A room column would have no reader, and the room is one `ParseThreadKey`
away in the Go half that actually decides.

### 6. The matcher: shortlist, then lock, then revalidate

`confirmUpworkDelivery` (`sink.go:397-461`) becomes three steps:

1. **Shortlist, no lock.** `SELECT id FROM deliveries WHERE channel='upwork_chat'
   AND status='sent' AND sent_external_id IS NULL AND confirmed_at IS NULL AND
   target_client_ref = $1` with `$1 = messageRef.ClientID`. Uses the partial index.
   Empty → return, having locked nothing. This is the common case once the tier is
   live: an outbound message for a client with no open deliveries.
2. **Lock the shortlist.** `... WHERE id = ANY($1) AND status='sent' AND
   sent_external_id IS NULL AND confirmed_at IS NULL ORDER BY id DESC FOR UPDATE`,
   inside the existing transaction. Descending id order is preserved so concurrent
   runs still block rather than deadlock (`sink.go:418-419`), which holds for
   overlapping subsets too.
3. **Revalidate in Go, unchanged.** `ParseThreadKey(target_ref)` →
   `SameConversation` → `textmatch.NormalizedPrefix` comparison → multi-match
   refusal → restated `IS NULL` guards on the UPDATE → `RowsAffected` check →
   `delivery_confirmed` event. Not one line of the decision moves into SQL.

**Why the shortlist cannot change the outcome:** step 3 is the same function over a
subset, and the subset is exactly the rows the function could have accepted —
`SameConversation` returns false whenever client ids differ (premise 6), and the
CHECK guarantees every `upwork_chat` row carries the client value that the same
parser produced from the same `target_ref` (§5). A stale or wrong column can
therefore only cause a MISS, never a wrong stamp — and a miss is what
`upworkcrm/reconcile.go` exists to surface. Pin the implication with a pure unit
test (criterion 16); it is the one property the whole optimisation rests on.

Flagged-but-unresolved rows still stay in their own client's set, deliberately: the
reconciler annotates, it never resolves, and such a row can legitimately confirm
later. What changes is that they no longer block every other client's connector run.

## Acceptance criteria

1. Migration `0019_delivery_provenance.sql` adds `tasks.source_thread_id` (FK to
   `normalized_threads`), `deliveries.target_client_ref`,
   `deliveries_upwork_identity_check` and `deliveries_upwork_unconfirmed_idx`, and
   applies cleanly to a database that already has 0001-0018. No backfill, no data
   statement, no edit to any earlier file.
2. `task_set_source_thread` is registered in `internal/tools/createtask.go`'s table
   and is **absent** from `internal/mcpserver/schemas.go`; the existing agent-facing
   tool lists (`internal/mcpserver/adapter_test.go:88`,
   `internal/tools/tools_unit_test.go:80`) are updated to assert its absence
   deliberately rather than by omission.
3. `task_set_source_thread` sets the column, is an idempotent success when re-run
   with the same thread id, and REFUSES a different thread id on a task that
   already carries one, with an error naming the existing value. Refusing a
   non-existent task or thread comes from the FK / an explicit lookup, not a panic.
4. `store.TaskSourceThread` resolves provenance from the named task, and from its
   parent when the named task has none; returns `found=false` (not an error) when
   neither has one. Unit-testable against a fixture; the integration test must make
   POSTGRES supply the value (IK: "test the column, not the fixture").
5. `internal/drafts/store.go` resolves an upwork target ONLY from
   `store.TaskSourceThread`. `upworkTarget`'s client-wide candidate query, its
   roomed preference and its multi-room refusal are deleted, and no query in the
   package filters threads by `ClientThreadPrefix` any more.
6. **The mitigation's cost is removed, and the test says so:** a task whose source
   thread is a ROOMED thread of a client that has three roomed threads resolves to
   that exact room's key. The same fixture under the pre-change code refused; the
   test comment must name SWT-19's mitigation so the reversal is deliberate.
7. A task whose source thread is a LEGACY (unroomed) thread resolves to that legacy
   key — not to any roomed thread of the client, in any ordering, including when a
   roomed thread has newer messages.
8. A task with NO provenance yields `Channel == ""` and reaches `drafts.go`'s
   "unresolvable — tell the human" log exactly as today; no delivery is created.
9. `draft_delivery` for `upwork_chat` is no longer closed. A call whose
   `target_ref` equals the task's provenance key succeeds and inserts a row with
   the canonical `target_ref`, `target_client_ref` set, and `thread_id` pointing at
   the provenance thread.
10. `draft_delivery` refuses `upwork_chat` when the task (and its parent) record no
    source thread, **for all six actor shapes** — `dashboard:`, `opsctl:`,
    `mcp:worker:`, `mcp:manual:`, `drafts:gpt`, bare `worker:` — reusing the actor
    list already pinned by SWT-19's closure test. The binding is not actor-keyed.
11. `draft_delivery` refuses a `target_ref` naming a thread of a DIFFERENT client
    than the provenance, for every actor including humans, even when that thread is
    ingested and the key is canonical. This is pass two's deferred finding; it must
    have its own test.
12. A `target_ref` naming a different ROOM of the SAME client is refused for
    `drafts:gpt` and for `mcp:*`, and accepted for `dashboard:`/`opsctl:`/`manual:`
    (finding 3's "explicit human choice"). The test comment states why this one
    distinction is actor-keyed and the client binding is not.
13. Every `upwork_chat` row is structurally forced to carry its identity:
    `INSERT INTO deliveries (task_id, channel, target_ref, body, status)` with
    `channel='upwork_chat'` and no `target_client_ref`/`thread_id` FAILS on
    `deliveries_upwork_identity_check`. A test asserts the constraint bites; the
    six fixture files listed below are updated to insert realistic rows.
14. The matcher shortlists before locking: the candidate `SELECT ... FOR UPDATE`
    is reached only with an explicit id set, and an outbound message for client A
    does not lock any delivery of client B. Demonstrated by a test that holds a
    lock on client B's unconfirmed row in another transaction and confirms client
    A's message without blocking (a bounded `statement_timeout` or context deadline
    makes the failure a red test rather than a hang).
15. Every SWT-19 matcher behaviour still holds, re-run unchanged over the new
    shape: roomed-confirms-roomed via `send_room_id`, roomed-confirms-unroomed,
    unroomed-confirms-roomed, different-room refusal in both `sent_at` orderings,
    different client never a candidate, unparseable `target_ref` never a candidate,
    two-candidate refusal, same-room duplicate refusal. The existing
    `roommatcher_integration_test.go` / `matcherhardening_regression_integration_test.go`
    cases are updated for the fixture shape, not weakened.
16. A pure unit test asserts `SameConversation(a, b) == false` whenever
    `a.ClientID != b.ClientID`, in both key shapes and both argument orders, with a
    comment naming it as the property the SQL shortlist is derived from. If this
    ever fails, the shortlist is unsound.
17. `keyspelling_test.go` still passes: no `LIKE`, `split_part` or `||` near
    `upwork_crm:` anywhere under `internal/`, including the new shortlist and the
    new provenance query. The `internal/drafts` code that previously needed
    `ClientThreadPrefix` no longer calls it (`ClientThreadPrefix` itself stays —
    it is still the safe bind-parameter builder).
18. `policy.HumanActor` is exported from `internal/policy/matrix.go` and
    `humanOnly`'s check and the new handler call the SAME function. A test asserts
    `HumanActor("mcp:manual:salvo")` is true and `HumanActor("drafts:gpt")` is
    false, so the exported form cannot drift from `Decide`'s gate.
19. The capture engine sets provenance: a live-mode `task` decision on an upwork
    message produces a task whose `source_thread_id` is the message's thread, via
    the executor. A failure to set it fails the pass loudly (the `linkRuleRef`
    shape, `rules_store.go:749-766`) rather than leaving a provenance-less task.
    Shadow mode still writes nothing — asserted by the existing shadow
    zero-writes test, extended to `tasks.source_thread_id`.
20. `internal/capture/rules_structure_test.go`'s ban still holds: the package
    contains no direct `INSERT INTO tasks` / `UPDATE tasks`; the provenance write
    goes through the executor.
21. `upworkcrm.ReconcileUnconfirmed` is unchanged in behaviour and still green,
    including its fire-once marker and the `mark_delivery_sent` re-arm.
22. `go test ./...` and `make integration` are green.
23. **"Usable alone" smoke, on the local integration db:** create a task, set its
    provenance to a roomed upwork thread, `draft_delivery` → `approve_delivery` →
    `mark_delivery_sent`, run the connector's normalize over an outbound message in
    that room, and observe `sent_external_id` + `confirmed_at` + one
    `delivery_confirmed` event — with a second client's unconfirmed row present
    throughout and never locked.

## Data model changes

Migration **`0019_delivery_provenance.sql`**, forward-only:

| table | change |
|---|---|
| `tasks` | `+ source_thread_id BIGINT REFERENCES normalized_threads(id)` — nullable; the conversation that raised the task |
| `deliveries` | `+ target_client_ref TEXT` — client identity extracted in Go from the parsed target key |
| `deliveries` | `+ CONSTRAINT deliveries_upwork_identity_check CHECK (channel <> 'upwork_chat' OR (target_client_ref IS NOT NULL AND thread_id IS NOT NULL))` |
| `deliveries` | `+ INDEX deliveries_upwork_unconfirmed_idx (target_client_ref) WHERE channel='upwork_chat' AND status='sent' AND sent_external_id IS NULL AND confirmed_at IS NULL` |

No new table (invariant 2). No new `external_refs.system` value. No change to
`deliveries.channel`'s CHECK, to `task_events.event_type`, or to the status machine.
Vocabulary reused throughout: `tasks`, `deliveries`, `normalized_threads`,
`external_refs`.

**Apply-time precondition:** `SELECT count(*) FROM deliveries WHERE
channel='upwork_chat'` must be 0 on pg-main, or the CHECK will fail. It has never
been non-zero. On a local integration db carrying fixture residue from earlier
runs, delete it by hand (`DELETE FROM deliveries WHERE channel='upwork_chat'`) —
that is test residue, not data. Do not soften the constraint to `NOT VALID` to get
past it; an unvalidated constraint is a guard that does not guard.

**Migrating is not applying** (IK): check
`SELECT max(version) FROM schema_migrations` against `ls migrations/` before any
image with this code reaches the cluster.

## API / MCP tool changes

**New — `task_set_source_thread` (spine-facing, NOT MCP-listed).**
Request `{"task_id": N, "thread_id": M}` → `{"task_id": N, "thread_id": M}`.
Registered in `internal/tools/createtask.go`'s table, so it runs the full executor
path: registry lookup → validate → policy check → audit start → handler → audit
complete. Policy: static fallthrough (the `propose_plan_import` shape) — it is not
delivery-gated and it is not human-only, because the capture engine
(`capture:{connector}`) is its main caller. Its protection is that no agent has a
transport to it (criterion 2).

**Changed — `draft_delivery` (agent-facing, MCP-listed).** Same request shape.
`upwork_chat` stops returning the blanket refusal and instead binds `target_ref` to
the task's provenance (§4). The bind happens in the HANDLER, not in `validate`,
because it needs the database; `validate` keeps its existing pure parse
(`delivery.go:129-131`). A refusal therefore lands after the policy check and is
audited, which is what we want for an attempted cross-client target.

**Unchanged:** `update_delivery`, `approve_delivery`, `send_delivery` (still
policy-denied for `upwork_chat` by `channel_assisted`), `mark_delivery_sent`,
`mark_delivery_failed` (still `slack_reply`-only — see the IK entry on R8),
`prefill_delivery`, `link_external_ref`, `create_task`, `create_child_task`.

**No policy matrix change.** `upwork_chat` stays on the assisted tier.

## MQTT topics

None. This ticket publishes and subscribes to nothing.

## Files likely to touch

New:
- `migrations/0019_delivery_provenance.sql`
- `internal/store/provenance.go` — `SourceThread`, `TaskSourceThread`, the querier seam
- `internal/tools/provenance.go` — `task_set_source_thread` validate + handler
- `internal/tools/delivery_upwork_binding_integration_test.go` — criteria 9-13
- `internal/connector/upworkcrm/shortlist_integration_test.go` — criterion 14
- `docs/runbooks/upwork-tier-golive.md` — the order of operations for enabling the
  tier (migrate → image → provenance present → watch), and what each refusal means

Modified:
- `internal/tools/delivery.go` — `draftDelivery`'s `upwork_chat` branch
  (`:219-282`, the closure and the EXISTS check), the INSERT at `:314-320`
- `internal/tools/createtask.go` — register the new tool (`:39-83`)
- `internal/policy/matrix.go` — export `HumanActor` (`:73-89`)
- `internal/drafts/store.go` — `resolve` (`:128-204`) and `upworkTarget`
  (`:260-363`); `taskThread` (`:216-241`) stays for gmail/jira
- `internal/connector/upworkcrm/sink.go` — `confirmUpworkDelivery` (`:397-461`) and
  the block comment at `:403-419`, whose "across all clients" paragraph is the
  thing this ticket falsifies
- `internal/capture/rules_store.go` — `pendingMessage` (`:180-185`) + the pending
  SELECT (`:414-421`) carry `m.thread_id`; a `setRuleProvenance` beside
  `linkRuleRef` (`:749-766`)
- `internal/mcpserver/adapter_test.go`, `internal/tools/tools_unit_test.go` — the
  agent-facing tool lists
- Fixtures inserting `channel='upwork_chat'` by hand (criterion 13):
  `internal/connector/upworkcrm/loopclosure_integration_test.go:107`,
  `roommatcher_integration_test.go:192`,
  `reconcile_integration_test.go:168,178,184`,
  `matcherhardening_regression_integration_test.go:314`,
  `internal/tools/delivery_lifecycle_integration_test.go` (the upwork case at
  `:392-425`), `internal/tools/delivery_upwork_target_integration_test.go`.
  Grep `git grep -n "upwork_chat" -- '*_test.go'` — do not trust this list to be
  complete.
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` — a SWT-20 entry: provenance is a task
  column written by observers (and WHY `external_refs` is not it: agent-writable,
  mutable key, unique per key), the CHECK as the reason the shortlist is sound, and
  the standing rule that any SQL predicate on delivery identity must be *implied
  by* `SameConversation` rather than a restatement of it.

## In scope / Out of scope

**In scope:** `tasks.source_thread_id` + `task_set_source_thread`; one shared
provenance reader; provenance-only upwork targeting in `drafts` (deleting the
SWT-19 mitigation and the client-wide scan); the `draft_delivery` binding and the
lifting of the pass-four closure; `deliveries.target_client_ref` + CHECK + partial
index + the shortlisted matcher; capture writing provenance for the tasks it
creates; the runbook and the IK entry.

**Out of scope — named because they are the tempting bundles:**

- **Reopening SWT-19's candidate rule.** `SameConversation`'s table (mismatch-only
  excludes) and the key format are correct and have exactly one spelling. This
  ticket changes which rows are *offered* to that rule, never the rule.
- **The multi-match refusal and the time-floor question.** Both settled; do not add
  a floor (IK: it is inert on the assisted tier), do not switch to newest-wins.
- **A recovery verb for a delivery that never landed.** The reconciler's note points
  at "raise a new task"; a compensating transition still needs the R8 analysis in
  the IK entry, and it is a separate ticket even though SWT-19's note names SWT-20.
  If it ships here it will be re-litigating orchestrator lifecycle inside a schema
  ticket.
- **A confirmation verb for a verified-but-unlinked row** (SWT-19 pass three's
  deferral). Related, and it wants evidence semantics of its own.
- **Promoting `upwork_chat` off the assisted tier**, and `send_delivery` for it.
  `internal/policy/matrix.go:120-125` stays exactly as it is.
- **Provenance for gmail/jira/slack targeting.** They keep the `external_refs`
  route. Migrating them is a follow-up with its own smoke surface.
- **`capture/observe.go`'s upwork channel** (`outbound_observed`), still deferred.
- **Any dashboard UI** for choosing a room. The human path is `opsctl call
  draft_delivery` today.
- **Taking capture rules or triage live.** Unrelated flip, ordered separately.

## Invariants that apply

1. **Raw-first.** Untouched: nothing here ingests or normalizes. The matcher change
   is downstream of `upsertMessage`, and `Normalize` still reads
   `raw_source_items` only (`keyspelling_test.go`'s `TestNormalizeReadsRawOnly`
   must stay green).
2. **One funnel.** No new table. Provenance is a column on `tasks`; delivery
   identity is a column on `deliveries`. Queues stay filters.
3. **Everything through the executor.** `task_set_source_thread` is a registry tool
   — validate → policy → audit start → handler → audit complete — and is the only
   writer of `tasks.source_thread_id`; capture reaches it the way it reaches
   `create_task`, keeping `rules_structure_test.go`'s ban intact. The
   `draft_delivery` binding is inside the existing handler; no side door, and no new
   agent-facing capability (the new tool is off the MCP list).
4. **Nothing external without a delivery row.** Nothing here sends. Idempotency is
   untouched: `sent_external_id` is still stamped once under restated `IS NULL`
   guards with a `RowsAffected` check, and the shortlist can only ever *reduce* the
   set the stamp is chosen from, never widen it (§6).
5. **Own-message loop closure.** This is the invariant the ticket serves twice: a
   delivery aimed at the room its task came from is a delivery the outbound message
   can confirm, and the shortlist is provably candidate-equivalent (premise 6 +
   the CHECK). The failure mode of a wrong shortlist is a MISS, which is silent —
   which is why `upworkcrm/reconcile.go` must keep working unchanged (criterion 21)
   and why criterion 16 pins the implication as a test rather than a comment.
6. **Stealth attribution.** Nothing client-visible is generated. Bodies still pass
   `google.ScrubAIAttribution` at the same write sites.
7. **Orchestrator purity.** The orchestrator is not touched; no new event type, no
   new rule. `store.TaskSourceThread`, `policy.HumanActor` and `SameConversation`
   are pure or trivially testable with no model and no network.

## Sibling patterns to copy

- **A spine tool that must not reach agents:** `capture_rule_add` /
  `capture_rule_set_enabled` (`internal/tools/capturerules.go`, registered in
  `createtask.go:81-82`, absent from `internal/mcpserver/schemas.go`) — copy the
  registration + the absence, including the tests that assert the absence.
- **A second executor call after `create_task`:**
  `internal/capture/rules_store.go:749-766` (`linkRuleRef`) — argument marshalling,
  `TaskID` on the `executor.Call`, and failing the pass loudly rather than
  continuing.
- **Server-side resolution of a caller-supplied field:** `draft_delivery`'s gmail
  branch (`internal/tools/delivery.go:283-310`, From from the thread) and its jira
  branch (`:190-208`). Upwork's binding is the same shape: the caller does not get
  to choose the destination identity.
- **Select candidates / decide in Go / stamp in one transaction:**
  `internal/connector/jira/sink.go:284-340` and the current
  `upworkcrm/sink.go:397-461`. Keep the skeleton; only the candidate SELECT moves.
- **One definition of a rule shared by two layers:** `internal/textmatch/prefix.go`
  and `internal/store/unconfirmed.go` (`UnconfirmedNoteMarker` after SWT-19's
  fourth pass). `store.TaskSourceThread` is the same idea for provenance.
- **Structural enforcement tests:** `internal/connector/upworkcrm/keyspelling_test.go`,
  `internal/textmatch/callsites_test.go`, `internal/capture/rules_structure_test.go`.

## Verification protocol

Before commit:

1. `go test ./...` — the `SameConversation` implication (16), `policy.HumanActor`
   (18), the provenance resolver's unit half (4), the tool-list assertions (2), and
   `keyspelling_test.go` (17).
2. `make integration` — `db-up` + `migrate` + `go test -tags integration -p 1
   -count=1 ./...`. `-p 1` is mandatory (IK: integration suites cross-pollute), and
   the new suites must join the mutual-cleanup pact and clean in FK order.
3. `git grep -n "ClientThreadPrefix" -- '*.go'` — after this ticket the only
   non-test callers should be gone from `internal/drafts`; if one remains, targeting
   is still choosing among threads.
4. `git grep -n "INSERT INTO deliveries" -- '*.go'` — exactly one non-test site,
   and it writes `target_client_ref` and `thread_id`.
5. Mutation check (IK: "test the column, not the fixture"): delete
   `target_client_ref` from the INSERT and confirm the failure is the CHECK at
   insert time, not a silently empty candidate set. Then drop the shortlist's
   `target_client_ref = $1` clause and confirm criterion 14 goes red — a shortlist
   test that passes with the predicate removed is testing nothing.
6. Re-read the `confirmUpworkDelivery` block comment: the paragraph at
   `sink.go:410-416` claiming the candidate set is "every upwork delivery never
   confirmed, EVER — across all clients" must be rewritten, not left as prose that
   contradicts the code (IK: "a comment can be a defect").

Manual smoke, local db first:

7. `make db-up && make migrate`, then
   `psql "$LOCAL" -c "\d deliveries"` and `\d tasks` — the two columns, the CHECK
   and the partial index are present.
8. Criterion 23's end-to-end run against the local db, with a second client's
   unconfirmed delivery present the whole time. `psql` afterwards:
   `SELECT id, channel, target_ref, target_client_ref, thread_id, sent_external_id,
   confirmed_at FROM deliveries ORDER BY id;`

Against production (read-only until the migration):

9. `psql "$OPS_DATABASE_URL" -c "SELECT count(*) FROM deliveries WHERE
   channel='upwork_chat'"` → expect 0 (the CHECK's precondition).
10. `psql "$OPS_DATABASE_URL" -c "SELECT max(version) FROM schema_migrations"`
    against `ls migrations/` before and after applying 0019.
11. Confirm the two multi-room clients by query (not by the number in this SPEC)
    and record what a provenance-carrying task for one of them would resolve to —
    read-only, since capture is in shadow and no such task exists yet.
12. Deploy note for the kube session: `connector-upworkcrm` runs a pinned tag; the
    matcher change reaches production only after an image build and tag bump in the
    kube repo, and **the migration must be applied first** — the new code's
    shortlist selects a column that does not exist until then.

## Decisions made unilaterally

- **D1. Provenance is `tasks.source_thread_id`, not an `external_refs` row.** The
  issue asked which; the answer is neither "drafts doesn't read them" nor "some
  paths don't write them" alone — it is that `external_refs` cannot be trusted for
  this job at all: `link_external_ref` is agent-facing free text (premise 2a), the
  join key is a mutable thread key (2b), and the unique constraint allows one task
  per conversation (2c). A delivery target derived from an agent-writable table is
  the exact exposure the pass-four closure was written for.
- **D2. `external_refs` keeps its dedup role and stays the gmail/jira route.** Not
  deleted, not migrated — that is a bigger blast radius for no gain in this ticket.
- **D3. No `target_room_ref` column.** The issue says "client and optional room".
  The room has no legitimate SQL reader: any shortlist predicate must be *implied
  by* `SameConversation`, and its room clause (`NOT (both roomed AND rooms differ)`)
  is the rule itself — spelling it in SQL is the second-spelling landmine this repo
  has paid for four times. Client equality is the only implied predicate, so the
  client is the only column that earns its place; the room is one `ParseThreadKey`
  away in the Go half that decides. A column nothing reads is this repo's other
  recurring bug. Reversible: adding it later is one nullable column when a reader
  exists (Future work).
- **D4. The CHECK constraint, rather than trusting the write site.** Zero rows make
  it free today and impossible to add later without a backfill. It converts "a
  fixture forgot the column" from a silently narrowed candidate set into an INSERT
  error, which is the only form of this guard that survives contact with this
  codebase's history.
- **D5. `deliveries.thread_id` is set for `upwork_chat`.** Existing vocabulary, an
  FK that subsumes the ad-hoc `EXISTS(thread_key)` check, and its readers are
  channel-scoped. Verify that last claim by grep before relying on it.
- **D6. Unroomed provenance targets the legacy thread rather than refusing.** It is
  the recorded conversation, `SameConversation`'s legacy tolerance keeps it
  confirmable, and the send is a human paste on the assisted tier. The residual
  risk (a legacy target confirmable by any room of that client) is pre-existing and
  is what the multi-match refusal covers.
- **D7. Changing an existing `source_thread_id` is refused for everyone.** An
  overwrite silently re-aims every future delivery of that task. A correction is
  psql plus a note.
- **D8. The client binding applies to every actor; only room CHOICE is
  human-gated.** The IK entry on actor prefixes forbids keying a *restriction* on
  the caller. Here the restriction (same client as provenance) is unconditional; the
  actor test only decides who may pick among rooms already inside that binding, and
  it uses `policy.HumanActor` — the gate `drafts:gpt` correctly fails — rather than
  `executor.ViaMCP`, which it correctly passes.

## Open questions

**None remaining.** Q1 (actor scope) was answered **(a)**: the reopening applies to
every actor from day one. The trust boundary is the server-side client binding of
D8, which is unconditional; an actor-prefix gate on draft CREATION would be a
transport label doing policy work (the IK landmine), and the policy matrix's
earned-autonomy rule gates sending tiers, not draft rows. Nothing sends without a
human: `send_delivery` stays `channel_assisted`-denied and approve/mark stay
human-only. Full reasoning in the OPEN_QUESTIONS file.

## Future work (not this ticket)

- **`target_room_ref`**, if a reader appears (a deliveries view that shows the
  room, or a per-room draft-already-exists guard).
- **The recovery verb** for a delivery that never landed, and the confirmation verb
  for a verified-but-unlinked row — SWT-19 passes three and four both point here,
  and both need the R8 lifecycle analysis in the IK entry, not a status flip.
- **Provenance for gmail/jira/slack targeting**, retiring the `thread_key ==
  external_key` join in `drafts.taskThread` once every creation path sets the
  column.
- **Provenance on `create_child_task`** by inheritance, if the two-level walk ever
  proves too shallow (it is not today: R3's Deliver task is a direct child).
- **`outbound_observed` for `upwork_chat`** — SWT-16's deferral, now cheaper
  because a delivery carries `thread_id` and `target_client_ref`.
