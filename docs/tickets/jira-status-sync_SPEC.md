> Jira: SWT-32

# jira-status-sync — a ticket reconciler: closed or not-mine drops the task, reopened or assigned-to-me brings it back

**Q1 ANSWERED** (Salvador, 2026-09-09; see
`docs/tickets/jira-status-sync_OPEN_QUESTIONS.md`). He proposed and chose a third
shape, now locked: **candidate-driven lookup** — capture keeps creating reengine
tasks from Slack and email exactly as today, and the reconciler fetches ONLY the
handful of Avviato issues that already have a task. No project-wide poll, no JQL,
no scope sweep, no first-tick flood. This document carries the answer; nothing is
provisional.

## Source

Ad-hoc, from Salvador, verbatim (two messages, one reconciler):

> so on collaboratory there are a number of taks that are matching closed
> tickets. If the ticket is closed the task should drop from switchboard of
> course repopen should resurface the task

> also reengine tickets for them to show as tasks they must be assigned not me

Clarified: a `reengine` ticket becomes — and stays — a task **only** when its Jira
assignee is Salvador. Tickets assigned to someone else, and unassigned tickets,
stay off the board. The board today already carries reengine tasks for tickets
that are not his (among the 7 seeded on 2026-09-09), so the one-time
reconciliation has to clean those up too.

Not a build-order step. It is maintenance on capture rules, which went LIVE on
2026-09-09 and seeded 40 tasks in one pass — 29 on `collaboratory`, 7 on
`reengine` — from Jira-keyed messages over a 30-day window. Capture links each
task to its ticket through `external_refs` (`link_external_ref`, system `jira`,
`external_key` = the ticket key). **Nothing has ever read the ticket back**, so a
task raised by a notification about `WEB-1234` stays on the board forever,
including when `WEB-1234` was already Done — or was never Salvador's — the day the
task was created.

## Goal

One deterministic pass that decides, per `jira` `external_refs` row, whether the
ticket still *warrants a task*, and moves the task through the executor to match:
close it when it does not, reopen the one it closed when it does again.

Warranting is a function of two facts read from a stored raw snapshot of the
issue:

- **status** — the ticket's `statusCategory` is not `done`; and
- **assignee** — for projects with the gate armed, the ticket is assigned to the
  polling account itself.

The snapshot comes from one of two places, and that is the ticket's second half:

- **already-polled sites** (`treetopllc.jira.com`, collaboratory) — the issue is
  already in `raw_source_items`, put there by the existing JQL poller. Nothing
  changes for them.
- **lookup-only sites** (`avviato.atlassian.net`, reengine) — **the task is the
  candidate**. The reconciler takes the ticket keys that already have tasks, GETs
  exactly those issues by key, stores each snapshot raw-first, and reads it back.
  No JQL, no project scan, and those raw rows are never normalized, so the funnel
  is unchanged.

Close/reopen is ONE mechanism over both facts and both sources: "assigned away
from me" and "moved to Done" are the same event as far as the board is concerned,
and so are their inverses.

**Usable alone means:** with nothing new deployed and no dashboard change, one
hand-run of `opsctl ticket-status sync` against the real ops db makes the
`collaboratory` tasks whose tickets are already Done leave `/tasks` (the board
hides `closed` by default — `boardQuery`'s "closed hidden by default" line), and
— with `reengine`'s gate armed and an Avviato token stored — makes the reengine
tasks whose tickets are assigned to someone else leave it too. Every other task is
untouched, and each close leaves an `audit_events` + `task_events` trail. Reopening
a ticket, or assigning one back to Salvador, puts its task back on the board on the
next run.

**The limit Salvador accepted, stated plainly:** an Avviato ticket assigned to him
that never produced a Slack message or an email never becomes a task, because
nothing ever made it a candidate. That is the same trade as his SWT-17 stance
("slack or email capturing those jira tickets as jobs on reengine for human
resolution is ok") — the notification is still what surfaces work; this ticket only
decides whether a surfaced ticket stays surfaced.

## Preconditions

- **SWT-31 (`ticket-board-dismissals`) merges first.** It owns migration
  `0022_task_dismissals.sql` and the shared transition helper in
  `internal/tools/close.go`; this ticket takes **0023**, reuses that helper for
  `task_reopen`, and queries `task_dismissals`. If SWT-31 lands after this one,
  the migration number and the dismissal clause both need revisiting.
- The migration ledger guard in `internal/classify/structure_test.go`
  (`if n > 17 && n != 18 && n != 19 && n != 20 && n != 21`) must learn **22 and
  23**. A conflict there when merging SWT-31 is expected and is a one-line
  resolution — rewrite the guard to the new truth, never delete it.
- **Salvador must supply an Avviato API token** before the reengine half can be
  verified (verification protocol step 0b). The status half needs nothing new.

## What the investigation established (read this before disagreeing with a decision)

1. **Both facts are in the stored raw, and one is already parsed.**
   `jira.Client.GetIssue` fetches `/rest/api/2/issue/{key}` with **no `fields`
   parameter**, so the stored issue carries every navigable field. `ingestIssue`
   strips only `fields.comment` (`splitIssueComments`) and stores the rest as
   `raw_source_items.external_id = 'issue:'+KEY`. Proof that `assignee` survives
   that: `NormalizeIssue`'s `rawIssue` struct already reads
   `fields.assignee.accountId` into the thread's participants
   (`normalize.go:71-73`). `fields.status` sits in the same object and is simply
   never read today.
2. **"Me" is already stored, per site.** `Ingest` calls `/myself` once per run and
   caches the result in the account's cursor (`cur.OwnAccountID`,
   `ingest.go:93-97`); `accountMeta` reads it back as
   `sync_cursor->>'own_account_id'`, and `NormalizeIssue` uses it for the direction
   rule ("outbound iff the reporter is the polling account itself"). That is the
   identity anchor for the assignee gate — an accountId, not an email or a display
   name, per source account rather than a global constant. `jira-auth add` already
   calls `Myself` to verify credentials, but prints the id rather than storing it.
3. **`GetIssue` by key is already the fetch primitive.** `ingestIssue` calls it per
   key; the lookup path needs no new HTTP code, only a different way of choosing
   the keys. `upsertRaw` + `chash.ContentHash` is likewise the existing raw-first
   write, hash-short-circuited on unchanged bytes.
4. **`source_accounts.provider` has NO CHECK constraint** (0001:13) and the table
   is `UNIQUE (provider, account_email)`. A new provider value needs no migration
   and does not collide with the same email under `jira`.
5. **Every jira query is already scoped by `provider='jira'`** — `ListAccounts`
   (the JQL poller's account list), `accountMeta`, and `pendingRaw` (the
   normalizer's inbox). This is what makes D17 fail-closed: a lookup account under
   a different provider value is invisible to all three by construction, so it is
   never JQL-polled and its raw rows are never normalized, without editing any of
   them. `jira-auth list` shares the same filter, which is the one query that must
   be widened or the new account looks like it does not exist.
6. **Only the LATEST snapshot per issue exists.** `raw_source_items` is
   `UNIQUE (source_account_id, external_id)` and `upsertRaw` UPDATEs in place on a
   hash change (`UpdateRaw` also resets `normalized_at`). No status or assignee
   history exists or ever will from this table — which is why the pass needs its
   own state row rather than a diff over raw items.
7. **The polled half refreshes itself.** The JQL cursor is `updated >= "-Nm"`, and
   both a status transition and a re-assignment bump `updated`, so a Treetop change
   is observed on the next `*/15` tick. The lookup half has no cursor at all — it
   re-GETs its candidates, bounded by a TTL (D20).
8. **`external_refs` is `UNIQUE (system, external_key)`** (0015). One ticket, one
   task, enforced by the database — the ref→task mapping this pass walks is 1:1 and
   needs no tie-break. It is also what bounds the lookup: the candidate set can
   never exceed the number of jira-keyed tasks.
9. **`external_refs.sync_cursor` and `.direction` have no writers anywhere**
   (grepped: every `sync_cursor` hit is `source_accounts`). Empty columns from
   0001, not a state store this pass may quietly adopt (D3).
10. **`closed` is terminal only by tool refusal, not by schema.** 0001's CHECK
    lists `closed` with no transition constraint; `closeTask` is the only thing
    making it one-way, because no verb moves a task out of `closed`. `task_reopen`
    is named in SWT-31's Future work as "deliberately unbuilt until something needs
    it". This is that something.
11. **`task_close` refuses `claimed | in_progress | needs_feedback`** ("never close
    work out from under a holder") and is an idempotent no-op on `closed`. The pass
    must handle the refusal rather than treat it as an error.
12. **The orchestrator is NOT deployed** (institutional knowledge: "Still not
    deployed: orchestrator, triage, drafts, fleetd, hooksd"). A design routing
    ticket state through a new orchestrator rule would ship nothing usable — D1.
13. **Capture cannot make this decision at creation time.** `decideMessage` sees a
    normalized MESSAGE and its rule match; it never loads the issue, and its live
    claim per message is spent forever (`capture_decisions_live_uniq`), so a
    message suppressed at capture time is spent — a ticket later assigned to
    Salvador could never produce a task. Hence D13.

## Decisions made unilaterally (with rationale)

**D1 — a deterministic pass in the `capture` / `promote` mould, NOT an orchestrator
rule and NOT a bare connector-sink hook.** The orchestrator only reacts to
`task_events`, so a rule would still need something to observe Jira and emit the
event — two hops for one fact — and `orchestratord` is not deployed, so the
recurring path would be dead on arrival. A sink hook inside
`internal/connector/jira` is wrong for the opposite reason: the pass must reach
`tasks` and `task_events`, which is executor territory (invariant 3), while the
connector sink writes only its own raw/normalized rows. The established shape for
"deterministic post-normalize pass that acts through the executor" is
`internal/capture` (`EvaluateRules`) and `internal/promote` (`Run`): its own
package, its own advisory lock, its own typed state, called from a connector main
**and** from `opsctl`.

**D2 — the status discriminator is `fields.status.statusCategory.key == "done"`,
never a list of status names.** Jira's status *names* are per-project workflow
configuration (`Done`, `Closed`, `Resolved`, `Won't Do`, anything an admin types).
`statusCategory` is a Jira-level structure with exactly three keys — `new`,
`indeterminate`, `done` — and every custom status maps into one. A name list is
this repo's recurring magic-literal defect in a fresh costume: it would pass every
fixture, then silently stop closing tasks the day Treetop renames a column. The
name IS stored, as a diagnostic column, and **nothing branches on it**.

**D3 — a typed state table, `ticket_status_syncs`, one row per
`external_refs.id`.** Three alternatives rejected:
- *No state, act purely on divergence.* Fails on the return path: the pass would
  resurrect any closed task whose ticket happens to be open and assigned, including
  tasks a human dismissed and tasks R8 closed. Coming back must be conditioned on
  "this pass closed it", a fact only this pass can record.
- *`external_refs.sync_cursor`.* An untyped TEXT column with no writer, whose
  CLAUDE.md meaning is a per-ref sync position. It cannot hold the facts that
  matter (did we close it, why, from which status) without becoming a packed
  string — the untyped-predicate anti-pattern 0015 and 0021 argue against at
  length.
- *Inferring "we closed it" from `task_events.payload->>'reason'`.* Explicitly
  banned: SWT-31 criterion 20 forbids reading a jsonb payload for a label, and the
  four post-hoc-matcher landmines are all the same mistake.
The row is UPSERTed in place (current state), not appended: the *history* of every
close and reopen is already append-only and typed in `task_events`
(`status_changed`) and `audit_events`.

**D4 — a human dismissal outranks a reconciler echo: a dismissed task never
resurfaces.** If the task carries a `task_dismissals` row (SWT-31), a ticket that
becomes warranted again appends ONE log line to the closed task and changes
nothing. The four dismissal reasons (`not_actionable | wrong_kind | duplicate |
handled_elsewhere`) are all statements about whether the task should exist, not
about timing, so none is invalidated by a status or assignee flip; and resurrecting
a dismissed task would destroy the meaning of the dismissal as training data. The
check is NOT redundant with D3's precondition: the pass can close a task itself and
Salvador can *then* record a dismissal label on the already-closed row — SWT-31
criterion 14 permits exactly that — and only this clause stops the next reopen from
undoing it.

**D5 — the dismissal check lives in the PASS, not in `task_reopen`.** A human must
be able to undo a mis-click; SWT-31 names re-open as that remedy. So the tool stays
general and the pass is the thing that refuses to override a human.

**D6 — `task_reopen` restores the status the task had when this pass closed it**
(`closed_from_status`), falling back to `ready`. It is one column we are storing
anyway, and more honest than flattening a `delivered` task to `ready`. The allowed
target set is exactly the set `task_close` accepts as a *source*
(`holding | ready | blocked | done_locally | delivered`), spelled ONCE in
`close.go` and shared by both verbs — a second list is how the two drift.

**D7 — `task_reopen` is spine-facing but NOT `humanOnly`.** The pass calls it as
`ticketstatus:jira`, exactly as the orchestrator calls `task_close` as
`orchestrator`; gating on a human actor would make the feature impossible. The gate
that matters is the MCP surface: an agent that could reopen tasks could resurrect
its own closed work, so the tool is absent from `internal/mcpserver/schemas.go` and
asserted absent in `spineTools`. Per the institutional rule, this SPEC does **not**
claim an actor-prefix check is a security boundary — the transport is the boundary,
as for `task_set_source_thread` and the capture-rule tools.

**D8 — the pass runs AFTER `capture.EvaluateRules` in the jira connector main.**
Order is load-bearing in a small, pleasant way: a notification about a ticket that
is already Done, or already someone else's, creates the task in capture's half of
the tick and this pass closes it in the same tick, so the state Salvador is
complaining about never persists for 15 minutes. The reverse order would create it
and leave it until the next run.

**D9 — no shadow mode flag, `--dry-run` instead.** Capture, triage and classify
shipped shadow-first because their inputs are heuristics or a model. This pass's
inputs are two fields the provider itself owns: there is nothing to review for
accuracy. `--dry-run` (promote's shape) is the review, the hand-run reconciliation
is the smoke, and the recurring path only starts when the kube session re-pins the
connector image — a separate, deliberate act.

**D10 — the pass never touches deliveries.** A closed task keeps any `deliveries`
row it has. This copies SWT-31's stance verbatim, for the reason the "failing a
delivery that R8 already processed" landmine gives: delivery state is not to be
inferred from task state.

**D11 — the assignee gate is a typed `projects` column, default OFF.**
`projects.ticket_assignee_gate BOOLEAN NOT NULL DEFAULT false`, armed by a hand-run
`UPDATE` per project and recorded in the runbook. Three reasons:
- a column, not a `policies` jsonb key — 0016 (`ai_locality`) and 0018
  (`ai_classify`) set this precedent and both migrations say why: an untyped
  predicate over jsonb is the thing this repo keeps paying for;
- **default false is the fail-closed side here**: not gating leaves today's
  behaviour (status only), while gating by accident silently empties a client's
  board. Same asymmetry `ai_classify` used, same polarity;
- per project, because Salvador asked for it on `reengine` and said nothing about
  `collaboratory` — where he IS the person the tickets are raised for. Arming
  collaboratory later is one `UPDATE`, not a code change.

**D12 — "me" is the polling account's own accountId, per source account.** The
comparison is `fields.assignee.accountId == source_accounts.sync_cursor->>'own_account_id'`
for the account that stored that raw item (fact 2). Not an email, not a display
name, not a constant in code and not a new configuration column: `accountId` is
Jira's stable identifier, display names change and are not unique (the slackweb
landmine: "no display-name matching, ever"), and per-account means the same code is
correct on Avviato and Treetop with different Atlassian identities. It reuses the
value `NormalizeIssue` already trusts, so "who are we on this site" has ONE
spelling. **Fail-safe:** an empty `own_account_id` makes the gate unevaluable and
the ref is counted `unreadable` — never dropped. A task must not vanish because we
could not identify ourselves.

**D13 — the reconciler decides presence; capture keeps creating.** Capture fires on
messages and does not know the assignee at decision time, and its live claim per
message is spent forever (fact 13). So a not-mine ticket still produces a task,
which this pass closes — within the same connector tick, per D8. The cost is a task
that exists for seconds, visible in `task_events`; the alternative cost is a ticket
assigned to Salvador next week that can never become a task because its notification
was suppressed. That trade is not close. **This is also what makes the Q1 answer
work: the task IS the candidate.**

**D14 — unassigned counts as not-mine.** Explicit in Salvador's clarification. Jira
serialises an unassigned issue as `"assignee": null` — the key is PRESENT with a
null value. That is a positive fact and is acted on. A `fields` object with NO
`assignee` key at all is a different thing — evidence missing, not evidence of
absence — and is counted `unreadable` with no action. (Absent-because-impossible
versus absent-because-pending, the recorded landmine; here the two are literally
distinguishable in the JSON and the reader must distinguish them.)

**D15 — one predicate, two facts.** `warranted = statusCategory != 'done' AND (gate
off OR assignee == own)`. Close/reopen keys on `warranted` alone; `drop_reason`
(`ticket_done | not_assigned`) records WHICH fact did it, for the report and for a
later "why did this leave the board" query. Two separate rules with two state
machines would drift the moment a ticket is both closed and reassigned.

**D16 — the lookup is CANDIDATE-DRIVEN: the key set comes from `external_refs`,
never from the provider.** (Q1's answer.) The reconciler already loads every
`system='jira'` ref; the keys with no local snapshot and a lookup account claiming
their prefix are GETed one by one. Consequences worth stating, because they are the
whole reason this shape was chosen over a poll:
- the request volume is bounded by the number of jira-keyed TASKS (tens), not by
  the size of a Jira project;
- no JQL, no cursor, no pagination, no `sync_cursor.jira_updated_at` for lookup
  accounts;
- **no funnel change at all**: no thread, no message, no capture decision, no
  triage inbox row — because the raw rows are never normalized (D17);
- and the accepted limit above: an unnotified ticket is never a candidate.

**D17 — lookup accounts are a distinct provider value, `jira_lookup`, not a flag
column on `provider='jira'`.** This is the "keep them out of the normalizer"
mechanism, and the choice is about which failure mode you get when someone forgets
a clause. Every existing jira query filters `provider='jira'` (fact 5), so a
`jira_lookup` row is **invisible to all of them by construction**: never JQL-polled
(`ListAccounts`), never normalized (`pendingRaw`), never given a site identity by
the normalizer (`accountMeta`). A boolean flag would require adding a clause to
each of those three, and a forgotten one means a project-wide poll or a funnel
flood — silently. With a distinct provider, a forgotten clause means the account is
*invisible*, which is the safe direction and is loudly visible as "the lookup found
nothing". Vocabulary check: this extends an existing column's value set rather than
inventing a table, and the repo already uses `provider` for synthetic ingestion
shapes (`plan` for plan imports, `slack_web` for per-workspace synthetic accounts).
`provider` has no CHECK constraint, so this costs no migration statement (fact 4).
Cost, stated: anything that means "all Jira accounts" must now name both values —
today that is exactly one query, `jira-auth list` (criterion 12).

**D18 — a lookup account is routed by ticket-key PREFIX, using the existing
mandatory `scopes`.** `jira-auth add --lookup-only <email> --site
https://avviato.atlassian.net --projects LHH` stores `scopes={LHH}`, and the
reconciler sends `LHH-123` to the account that declares `LHH`. Three reasons:
- it preserves 09-jira-github-connectors' mandatory-scoping safety property
  verbatim ("an unscoped poll is refused so the SWT build tracker never enters the
  product funnel") — an account can only ever fetch keys whose prefix it declares,
  so no ref can make us GET an arbitrary issue;
- the alternative — routing on `external_refs.external_url`'s host — trusts a
  column `link_external_ref` writes as **agent-facing free text**, which would let a
  crafted ref point our token at a host of the caller's choosing. That is the
  SWT-20 argument against external_refs as a provenance store, in a sharper form;
- it needs no new column.
Two lookup accounts claiming one prefix is an **ambiguity refusal**, counted, never
guessed. A prefix no account claims is `unpolled` — exactly today's behaviour.

**D19 — raw-first is a WRITE-THEN-READ-BACK, not a parse of the response.**
(Invariant 1, mechanically.) The fetched issue goes through the existing
`upsertRaw` (content hash, insert or update) into `raw_source_items` under the
lookup account, and the decision then reads the **stored row**, not the HTTP
response in memory. So every close this pass performs is reproducible from
`raw_source_items` alone, and a re-run with the network unplugged reaches the same
verdict from the same bytes. A pass that decided from the response and stored a
copy afterwards would satisfy the letter of invariant 1 and lose the property it
exists for.

**D20 — the lookup has a freshness TTL, default 1 hour, and no cursor.** A key
whose stored snapshot was ingested within `TICKET_LOOKUP_TTL` is not re-fetched.
Without it, a `*/15` CronJob re-GETs every candidate four times an hour forever —
fine at 36 tasks, careless at 500. Read with the defensive shape capture's horizon
uses (unparseable or non-positive → the default; "3600" is not a Go duration).
`--force` on the CLI bypasses it for the smoke. Deliberately NOT a cursor: there is
nothing to page and no watermark to keep, and a cursor on a candidate-driven fetch
would be a stateful thing whose only job is to be wrong after a re-assignment.

**D21 — no `OPS_TOKEN_KEY` means the lookup is SKIPPED LOUDLY, not silently.** The
status half needs no token (Treetop's raw is already stored), so the pass still
does useful work; but it prints a line naming the lookup accounts it could not use,
and their refs are counted `unpolled`. An empty result and a disabled credential
must never look the same in a log.

## Acceptance criteria

### Data model

1. `migrations/0023_ticket_status_sync.sql` creates `ticket_status_syncs` with the
   columns in "Data model changes" below: FKs to `external_refs` and `tasks` both
   `ON DELETE CASCADE`, CHECKs on `status_category`, `last_action` and
   `drop_reason`, a **total** `UNIQUE (external_ref_id)`, and no other index.
2. The same migration adds `projects.ticket_assignee_gate BOOLEAN NOT NULL DEFAULT
   false` and performs **no** `UPDATE` arming it (D11 — arming is an operator act,
   recorded in the runbook, exactly as `classify_promote_after` is). No `DROP`, no
   down migration, no index on the new column.
3. The migration contains **no change to `source_accounts`**: `provider` has no
   CHECK, so `jira_lookup` needs no DDL (D17, fact 4). A test asserts the migration
   does not `ALTER TABLE source_accounts`.
4. `internal/classify/structure_test.go`'s ledger accepts 22 and 23 and nothing
   above 23; the test still fails for any unowned migration number.

### Reading the ticket

5. `jira.IssueRawID(key)` is the ONE spelling of the raw `external_id` for an
   issue, with `jira.ParseIssueRawID` as its inverse. `ingest.go` (`"issue:"+key`)
   and `normalize.go` (`strings.HasPrefix(it.externalID, "issue:")`) are converted
   to use them; a unit test round-trips the pair. *Deliberately not mechanized by a
   source scan:* the literal `issue:` is too generic for a repo-wide ban to
   discriminate (unlike `upwork_crm:` or `calendar:`), and a guard that cannot match
   only its target is worse than none.
6. `jira.IssueFacts(raw json.RawMessage) (Facts, error)` returns
   `{StatusCategory, StatusName string, StatusKnown bool, Assignee string,
   AssigneeKnown bool}` reading `fields.status.statusCategory.key`,
   `fields.status.name` and `fields.assignee.accountId` and nothing else. Unit
   tests: `done`, `indeterminate` and `new` fixtures; missing `fields.status` →
   `StatusKnown=false`; a `status` with no `statusCategory` → `StatusKnown=false`;
   and a status **named** `Closed` whose category is `indeterminate` returns
   `indeterminate` (D2 — the name is never consulted).
7. Assignee cases, each a unit test (D14): `"assignee": {"accountId":"5b1…"}` →
   `Assignee="5b1…", AssigneeKnown=true`; `"assignee": null` → `Assignee="",
   AssigneeKnown=true` (a positive statement of unassignment); `fields` with no
   `assignee` key → `AssigneeKnown=false`. Distinguishing the last two means probing
   `fields` as `map[string]json.RawMessage`, not unmarshalling into a struct with a
   pointer field.
8. A structural test asserts that neither `internal/ticketstatus` nor the new jira
   reader contains a status-name list, an email address, or a display-name
   comparison.

### The candidate-driven lookup

9. `jira.LookupIssues(ctx, c *Client, sink Sink, acct Account, keys []string, cfg)
   (Stats, error)` GETs each key with the existing `GetIssue`, splits comments off
   with the existing `splitIssueComments`, and writes through the existing
   `upsertRaw` — so the raw-first write for this ticket happens at exactly ONE
   place, the same one `ingestIssue` uses. It issues **no JQL**: a structural test
   asserts `SearchJQL` is not referenced from the lookup path.
10. The key set is the reconciler's candidate list (D16). Unit test with a fake
    client: given three refs of which one already has a fresh stored snapshot, the
    lookup fetches exactly the other two, in key order, and no others.
11. **Write-then-read-back (D19):** the decision reads the row from
    `raw_source_items`, never the in-memory response. Integration test: run the pass
    with a fake client that returns a `done` issue, assert the task closed AND the
    raw row exists with a matching `content_hash`; then re-run with a client that
    **errors on every call** and assert the same verdict is reached from the stored
    row (TTL not expired) with zero HTTP calls.
12. Lookup accounts are `provider='jira_lookup'` (D17). Tests assert, against a real
    schema: `jira.ListAccounts` does not return one, `pendingRaw` does not select
    its raw rows (they keep `normalized_at IS NULL` after a full `Normalize` run),
    and no `normalized_messages` / `normalized_threads` / `capture_decisions` row is
    created for them. `jira-auth list` DOES show them, labelled with their role —
    the one query widened to both values (fact 5, and the "a page that looks empty
    may be the wrong page" landmine).
13. `jira-auth add --lookup-only <email> --site URL --projects LHH` stores
    `provider='jira_lookup'` with `scopes={LHH}`, `send_enabled=false`, the token
    pgcrypto-encrypted, after verifying it with `Myself` — the existing add path,
    one flag. `--projects` stays REQUIRED (D18): a lookup account with no declared
    prefixes is refused, with a message saying it could otherwise fetch any issue on
    the site.
14. Routing is by key prefix against `scopes` (D18). Unit tests: `LHH-123` → the
    account declaring `LHH`; `WEB-1` with no claiming account → `unpolled`, no HTTP
    call; two accounts declaring `LHH` → `ambiguous`, no HTTP call, nothing acted
    on; a key whose prefix is not a legal Jira project key is `unpolled`, never a
    fetch.
15. **A ref never causes a fetch outside its account's declared prefixes.** A
    structural/unit test drives a ref whose `external_url` names a different host
    and asserts the fetch still goes to the account chosen by prefix — the
    `external_url` column is never read for routing (D18's SSRF argument).
16. The lookup calls `Myself` once per lookup account per pass and merges the result
    into `sync_cursor.own_account_id` with the existing `SaveCursor` (which is a
    `sync_cursor || $2::jsonb` merge, so nothing else in the cursor is clobbered —
    the SWT-24 whole-blob landmine). Integration test asserts the column is
    populated after one pass and that an unrelated pre-existing cursor key survives.
17. TTL (D20): a candidate whose stored raw was ingested within `TICKET_LOOKUP_TTL`
    (default 1h) is not re-fetched; `--force` bypasses it. Unit tests for the
    defensive duration reader: unset → default, `"3600"` → default, `"0s"` →
    default, `"5m"` → 5m.
18. Missing `OPS_TOKEN_KEY` (D21): the pass still reconciles from stored raw, prints
    a line naming each lookup account it skipped, counts their refs `unpolled`, and
    exits 0. Test asserts the line is printed — a skip that is only visible as a
    zero counter is not enough.
19. One `sync_runs` row per lookup account per pass, `status` ok/error, with the
    fetch counters in `stats`. Nothing branches on that payload (the upworkcrm
    two-rows landmine), and the runbook says so.

### The pure decision

20. `ticketstatus.Decide(obs Observation, state *State) Decision` is a pure function
    of its arguments: a structure test scans its file for `pgx`, `context`,
    `os.Getenv`, `net` and `jira` (the client), in the shape of
    `internal/capture/rules_structure_test.go`.
21. `warranted` is computed exactly as D15 spells it, in ONE place. Unit table over
    the cross-product `{done, indeterminate, new} × {mine, other, unassigned} ×
    {gate on, gate off}` — 18 rows, each naming the expected `warranted` and
    `drop_reason`. With the gate OFF the assignee is irrelevant in all six rows;
    with it ON, `done` yields `drop_reason='ticket_done'` even when the ticket is
    Salvador's (status precedence).
22. **not warranted + task open** (`holding|ready|blocked|done_locally|delivered`) →
    `close`, recording `closed_from_status` = the task's current status and
    `drop_reason`.
23. **not warranted + task already `closed` + no state row** → `none`, and the state
    row is written with `last_action='none'`. The pass must never claim a close it
    did not make — this is what stops it reopening a human's dismissal later.
24. **not warranted + task `claimed|in_progress|needs_feedback`** →
    `refused_active`: exactly ONE `task_append_log` naming the ticket, its status and
    its assignee, no status change, state recorded. A second pass over the same
    unchanged observation performs zero executor calls (no log spam every 15
    minutes).
25. **warranted + task not `closed`** → `none`.
26. **warranted + task `closed` + `state.last_action != 'closed'`** → `none`.
    Integration case: a task closed by `task_dismiss` before this pass ever ran is
    never reopened.
27. **warranted + task `closed` + `state.last_action == 'closed'` + no
    `task_dismissals` row** → `reopen` to `closed_from_status`, falling back to
    `ready` when it is absent or outside the allowed set. This fires for BOTH causes:
    a ticket that left `done`, and a ticket assigned back to Salvador.
28. **warranted + task `closed` + `state.last_action == 'closed'` + a
    `task_dismissals` row** → `suppressed_dismissed`: ONE `task_append_log` on the
    closed task, no status change, recorded once and never repeated (D4).
29. Flapping is symmetric in both dimensions: close → reopen → close over one state
    row, driven once by status and once by assignment, is a unit test.

### Scoping and refusals

30. The candidate set is `external_refs WHERE system='jira'` joined to `tasks` and
    `projects`, resolved against raw issues under `provider IN ('jira','jira_lookup')`
    accounts. A ref with no stored snapshot and no claiming lookup account is counted
    `unpolled` and **nothing happens to it**.
31. Two stored raw issues for one key (two accounts) → the key is **refused**,
    counted `ambiguous`, nothing acted on. (The multi-match refusal precedent:
    refusing is reversible; a wrong close is a task that vanishes off the board with
    no explanation.)
32. `unreadable` — and therefore NO action in either direction — covers all evidence
    gaps: `StatusKnown=false`; gate ON with `AssigneeKnown=false`; gate ON with an
    empty `own_account_id` for the storing account (D12's fail-safe); and a lookup
    fetch that failed (the previous snapshot, if any, is still used; a fetch failure
    never becomes a verdict). Each is a separate integration case and each is
    counted.
33. **`ticket_assignee_gate` is read from the COLUMN and the regression test lives in
    the integration suite**: mutating the `projects` SELECT to a literal `false` must
    turn a test red (institutional landmine 6 — "for any predicate whose input comes
    from a column, the regression test belongs in the integration suite"). A unit
    test cannot catch this, by construction; it is the thing supplying the value.
34. No SQL in this ticket concatenates the raw id: ids are computed in Go via
    `jira.IssueRawID` and bound as one array parameter (`= ANY($1)`), the way
    `ClientThreadPrefix` is passed. Structural test asserts no `'issue:' ||` or
    `format('issue:%s')` in `internal/`.

### The tool

35. `task_reopen` is registered in `tools.Register` beside `task_close`.
    `validateReopen` rejects `{}`, a missing/zero `task_id`, an empty `reason`, and
    any `status` outside the allowed set (by name, both ways).
36. `task_reopen` refuses any task whose status is not `closed`; an already-open task
    is an **idempotent success** returning `reopened:false` with no event written
    (the spine convention for replays).
37. `closeTask` and `reopenTask` share ONE unexported transition helper and ONE
    allowed-status list in `internal/tools/close.go`. Dropping the list from one verb
    must fail a test.
38. One transaction: the `tasks.status` UPDATE and the `status_changed`
    `{from:'closed', to:X, reason}` event commit together. No new payload key.
39. `task_reopen` is absent from `internal/mcpserver/schemas.go` and present in
    `spineTools` in `internal/mcpserver/adapter_test.go` — asserted deliberately, not
    by omission (the `task_set_source_thread` precedent). It is NOT in
    `policy.humanOnly`; a unit test pins that the repo's actor shapes (`dashboard:`,
    `opsctl:`, `manual:`, `mcp:manual:`, `mcp:worker:`, `worker:`, `drafts:gpt`,
    `orchestrator`, `capture:jira`, `promote:classify`, `ticketstatus:jira`) all fall
    through the static allow-list, so nothing here pretends an actor prefix is a
    gate.

### The driver

40. Every write to `tasks` / `task_events` goes through the executor as actor
    `ticketstatus:jira`, with `executor.Call.TaskID` set so `audit_events.task_id` is
    non-NULL. A structural test asserts `internal/ticketstatus` contains no
    `UPDATE tasks`, `INSERT INTO tasks` or `INSERT INTO task_events`; its only direct
    writes are `ticket_status_syncs` (raw writes belong to `internal/connector/jira`,
    criterion 9).
41. The pass takes an advisory lock on its own key (see "Concurrency"). Contention is
    **log-and-skip returning zero stats and no error** (capture's policy — this is a
    hitchhiker on a connector run), and the skip prints a line so a silent no-op and
    a real empty pass are never the same log.
42. `--dry-run` takes the same decisions over the same rows, performs **no writes of
    any kind** — no state rows, and **no fetches**, so a dry run cannot even mutate
    `raw_source_items`. It prints one line per decision including `warranted`,
    `drop_reason`, the assignee comparison, and `would_fetch` for candidates whose
    snapshot is missing or stale.
43. Counters print unconditionally, zeros included: `considered`, `closed` (split
    `ticket_done` / `not_assigned`), `reopened`, `refused_active`,
    `suppressed_dismissed`, `converged`, `unpolled`, `ambiguous`, `unreadable`,
    `fetched`, `fetch_skipped_ttl`, `fetch_failed`.
44. **Idempotence:** running the pass twice over an unchanged world performs zero
    executor calls on the second run (and, within the TTL, zero fetches). Integration
    test asserts the `audit_events` count for actor `ticketstatus:jira` is unchanged.

### Wiring

45. `cmd/connectors/jira/main.go` runs the pass AFTER `capture.EvaluateRules` (D8,
    with the reason in a comment); its counters print before its error is returned,
    matching the capture block directly above it.
46. `opsctl ticket-status sync [--dry-run] [--force] [--limit N]` and
    `opsctl ticket-status report` exist, following the `capture-rules` subcommand
    shape in `cmd/opsctl/main.go`. `report` is read-only: current
    `ticket_status_syncs` state joined to tasks and projects (including each
    project's gate), plus the `unpolled` refs and which lookup account, if any, would
    claim each.
47. `docs/runbooks/ticket-status-sync.md` documents: what the pass does; the
    `statusCategory` discriminator and why not names; the assignee gate (what arms
    it, what "me" means, that unassigned counts as not-mine); the candidate-driven
    lookup, `jira_lookup`, and the accepted limit that an unnotified ticket is never
    a candidate; the TTL and `--force`; the dismissal suppression; and the one-off
    reconciliation command.

## Data model changes

**`migrations/0023_ticket_status_sync.sql`** — the only migration this ticket adds.

```sql
-- The assignee gate: per project, default OFF (0016/0018 precedent — a typed
-- column, never a policies jsonb key; fail-closed means "keep today's
-- behaviour", because gating by accident silently empties a client's board).
-- Armed by hand, per project, per the runbook. No backfill and no UPDATE here.
ALTER TABLE projects ADD COLUMN ticket_assignee_gate BOOLEAN NOT NULL DEFAULT false;

CREATE TABLE ticket_status_syncs (
  id                  BIGSERIAL PRIMARY KEY,
  external_ref_id     BIGINT NOT NULL REFERENCES external_refs(id) ON DELETE CASCADE,
  task_id             BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  -- Jira's three statusCategory keys. The NAME is diagnostic only; nothing
  -- branches on it.
  status_category     TEXT NOT NULL CHECK (status_category IN ('new','indeterminate','done')),
  status_name         TEXT,
  -- The ticket's assignee accountId as last observed, and whether it matched the
  -- storing account's own_account_id. Diagnostic + the report's join; the
  -- DECISION is recomputed every pass from the stored raw, never read back here.
  assignee_account_id TEXT,
  assigned_to_self    BOOLEAN,
  -- What this pass last DID. 'closed' is the only value that authorises a later
  -- reopen: it is how the pass knows the close was its own and not a human's
  -- dismissal, R8's delivery lifecycle, or a hand-run task_close.
  last_action         TEXT NOT NULL CHECK (last_action IN
                        ('none','closed','reopened','refused_active','suppressed_dismissed')),
  -- WHICH fact dropped it. NULL unless the task was dropped.
  drop_reason         TEXT CHECK (drop_reason IN ('ticket_done','not_assigned')),
  -- The status the task held when this pass closed it, so a reopen restores it
  -- instead of flattening everything to ready. NULL unless last_action='closed'.
  closed_from_status  TEXT,
  reason              TEXT,
  observed_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  acted_at            TIMESTAMPTZ
);

CREATE UNIQUE INDEX ticket_status_syncs_ref_uniq ON ticket_status_syncs (external_ref_id);
```

- **No `source_accounts` DDL.** `provider` has no CHECK constraint (0001:13) and the
  table is `UNIQUE (provider, account_email)`, so `jira_lookup` is a value, not a
  schema change — and the same email may exist under both providers without
  colliding (D17).
- **One row per ref, UPSERTed** (`ON CONFLICT (external_ref_id) DO UPDATE`). The
  index is total, so the conflict target needs no restated predicate — unlike
  `capture_decisions_live_uniq` and `task_events_outbound_observed_uniq`, both
  partial, whose callers must repeat the WHERE clause or hit a runtime error.
- **This is state, not a log.** The history of every close and reopen is already
  append-only and typed in `task_events` (`status_changed`) and `audit_events`.
- **`assigned_to_self` is recorded, never read as an input.** Every pass recomputes
  the comparison from the stored raw and the account's `own_account_id`. A cached
  boolean feeding the decision would go stale the day the polling identity changes,
  silently.
- **ON DELETE CASCADE on both FKs**, for `capture_decisions`' recorded reason:
  integration suites clear fixtures by deleting `tasks`, and a cleanup failure reads
  like the cross-pollution pact breaking rather than like a new FK.
- **No other index, deliberately.** One row per jira external ref — tens today — and
  every read either scans the table whole or reaches it by `external_ref_id`. An
  index nothing uses is a permanent claim some query needs it (0016's and 0018's
  recorded argument).

New **data** (not schema), created by an operator, recorded in the runbook: one
`source_accounts` row with `provider='jira_lookup'`,
`domain_default='https://avviato.atlassian.net'`, `scopes={LHH}`, written by
`jira-auth add --lookup-only`.

`tasks`, `task_events`, `external_refs`, `capture_rules`, `capture_decisions`,
`task_dismissals` and the normalized tables are untouched. `raw_source_items` gains
rows (the lookup snapshots) and no columns.

## API / MCP tool changes

**New: `task_reopen`** — `{task_id: int, status?: string, reason: string}` →
`{task_id, status, reopened: true|false}`.

Executor path (invariant 3), hooked in exactly where its sibling is:

- registered in `tools.Register`'s table (`internal/tools/createtask.go`), beside
  `task_close`;
- `Validate` = `validateReopen` (`internal/tools/close.go`);
- policy: **not** added to `humanOnly` and not `sendShaped`/`snapshotGated` —
  nothing leaves the system, so neither the kill switch nor the rate limit has a
  claim on it; it falls through `policy.NewMatrix` to the static allow-list, as
  `task_close` does (D7);
- audit: callers pass `executor.Call.TaskID`, so audit start/complete carry the task;
- **NOT** in `internal/mcpserver/schemas.go`; asserted in `spineTools`.

**Modified: none.** `task_close` keeps its exact signature and behaviour; only its
transaction body is shared (SWT-31 already moves it into a helper — extend that
helper rather than adding a second one).

**Used unchanged:** `task_append_log` (the refused-active and suppressed-dismissed
lines), `task_close`.

**New CLI:** `opsctl ticket-status sync|report`, and `jira-auth add --lookup-only`.
No dashboard route, no HTTP surface, no JSON API.

**Provider surface touched:** `GET /rest/api/2/issue/{key}` and
`GET /rest/api/2/myself` on the lookup site, both through the existing
`jira.Client`. No write endpoint is called, ever.

## MQTT topics

None. Nothing published, nothing subscribed, no LWT.

Note for the reviewer: a close and a reopen each write a `status_changed`
task_event, which NOTIFYs the orchestrator's drain. That is deliberate and needs no
orchestrator change — `Evaluate`'s `status_changed` case runs `ruleUnblockDependents`
for `to == "closed"` (and for `to == "delivered"`, which a restore can produce) and
returns nil for everything else, including `to == "ready"`. See "Invariants that
apply" §7 for the named consequence.

## Concurrency

One advisory lock, taken on a dedicated connection with an explicit unlock before
the connection is released (a session-level lock outlives a returned pooled
connection — `capture.tryRulesLock` and `promote.tryLock` are the two spellings to
copy). It also covers the lookup, so two overlapping passes cannot both GET the same
key.

The key follows the established convention — the same four hex digits as this
ticket's migration number — and its freeness is checked mechanically by the
collision scan in `internal/classify/structure_test.go`, which walks `internal/` and
fails on any duplicate. If it collides, take the next free number and say why in a
comment beside it. Prior keys (orchestrator, triage, capture, classify, promote,
calendar booking) are referred to here in prose only; do not restate any of their
literals in this package.

## Files likely to touch

- `migrations/0023_ticket_status_sync.sql` (new)
- `internal/connector/jira/rawid.go` (new) — `IssueRawID` / `ParseIssueRawID`; call
  sites in `internal/connector/jira/ingest.go` (`"issue:"+key`) and
  `internal/connector/jira/normalize.go` (the `strings.HasPrefix` switch)
- `internal/connector/jira/facts.go` (new) — `IssueFacts`, pure
- `internal/connector/jira/lookup.go` (new) — `LookupIssues`, the candidate-driven
  fetch; reuses `GetIssue`, `splitIssueComments`, `upsertRaw`, `StartRun`/`FinishRun`
- `internal/connector/jira/sink.go` — `ListLookupAccounts` (provider `jira_lookup`);
  `ListAccounts`, `accountMeta` and `pendingRaw` are deliberately UNCHANGED
- `internal/connector/jira/facts_test.go`, `lookup_test.go` (new)
- `internal/ticketstatus/decide.go` (new) — `Observation`, `State`, `Decision`,
  `Decide`, the `warranted` predicate; zero I/O
- `internal/ticketstatus/store.go` (new) — advisory lock, the candidate query
  (`external_refs` × `tasks` × `projects` × `raw_source_items` × `source_accounts` ×
  `task_dismissals` × `ticket_status_syncs`), prefix routing, the TTL, the injected
  lookup dependency, the state UPSERT, the executor calls
- `internal/ticketstatus/decide_test.go`, `structure_test.go`,
  `store_integration_test.go` (new; the integration suite joins the existing
  mutual-cleanup pact — `go test -p 1`, clean up in FK order under a test-owned slug
  prefix)
- `internal/tools/close.go` — `task_reopen` (`reopenArgs`, `validateReopen`,
  `reopenTask`) on SWT-31's shared transition helper
- `internal/tools/createtask.go` — the `Register` table
- `internal/tools/tools_unit_test.go` — `allToolNames` + `toolsUnderTest`
- `internal/mcpserver/adapter_test.go` — `spineTools`
- `internal/policy/` — a test only; no matrix change
- `cmd/jira-auth/main.go` — `--lookup-only` on `add`; `list` widened to both provider
  values with a role column
- `cmd/connectors/jira/main.go` — the pass, after the capture block, with the token
  factory it already builds
- `cmd/opsctl/main.go` — the `ticket-status` subcommand (usage line at :33, the
  switch at :42-63)
- `internal/classify/structure_test.go` — the migration ledger
- `docs/runbooks/ticket-status-sync.md` (new)

## In scope / Out of scope

**In scope:** migration 0023 (the state table and the `projects` gate column); the
jira raw-id, issue-facts and candidate-driven lookup readers; the `jira_lookup`
account shape and `jira-auth --lookup-only`; the pure `warranted` decision and its
driver; `task_reopen`; the connector-main and `opsctl` entry points; the one-time
reconciliation of the live board; the runbook.

**Out of scope — named because they are the tempting bundles:**

- **A project-wide poll of Avviato** (`jira-auth add` without `--lookup-only`,
  a JQL cursor, `scopes` as a poll scope). This is the alternative Q1 rejected: it
  would turn every LHH issue into an inbound message and, through the priority-100
  `LHH-[0-9]+` capture rule, into a task — the whole project arriving as a board
  flood that the reconciler would then close a second later. The candidate-driven
  lookup exists precisely so that never happens; do not "simplify" it into a poll.
- **Normalizing lookup snapshots**, or giving `jira_lookup` a thread key. Those raw
  rows exist to be read by this pass and nothing else.
- **Gating creation inside capture** on assignee or status (D13), and any change to
  capture rules, `capture_decisions`, or the rule set.
- **Arming `collaboratory`'s gate.** One `UPDATE`, an operator act, not code.
- **Commenting on the ticket, transitioning it, re-assigning it, or any other
  outbound Jira action.** This pass reads. `jira_comment` sends stay behind
  `deliveries` and the policy matrix (invariant 4).
- **GitHub issue/PR state** driving the same reconciliation. `external_refs.system`
  has five values; this ticket handles `jira` only.
- **A dashboard control for reopen**, or surfacing `ticket_status_syncs` on `/tasks`
  or `/funnel`. The board hides `closed` already; that IS the "drops from
  switchboard" Salvador asked for.
- **SWT-31's dismissal work** (titles, `task_dismiss`, the board form). This ticket
  consumes `task_dismissals` and changes nothing about it.
- **Any other presence rule** — priority, age, watchers, sprint, labels.
- **Re-titling, re-prioritising or re-assigning tasks**, and deleting or archiving
  anything. Closed is closed; nothing is removed.
- **Touching `promote`, `classify`, `triage`, the orchestrator, or any connector's
  normalize path.**

## Invariants that apply

1. **Raw-first — and this ticket now WRITES, so the demand is concrete.** The
   lookup's raw write happens in ONE place: `jira.LookupIssues` → the existing
   `upsertRaw` (content hash, insert-or-update) into `raw_source_items` under the
   `jira_lookup` account, using the same `IssueRawID(key)` external id as the poller.
   It happens **before any field is read**, and the decision then reads the STORED
   row, not the HTTP response (D19) — so every close is reproducible from
   `raw_source_items` alone and a re-run reaches the same verdict from the same
   bytes. Nothing is extracted, normalized or decided before that write. For the
   already-polled half the write happened months ago, which is why the status half
   needs no network at all.
2. **One funnel — `ticket_status_syncs` must not become a second tasks table.** It
   carries no title, no worker assignee, no claim, no priority, and nothing ever
   "works" a row; the items stay rows in `tasks`, and the board lane stays a FILTER
   (`?status=closed`), never a table. The migration comment says this out loud, as
   0015, 0021 and 0022 do. The lookup's raw rows add no second funnel either: they
   are deliberately never normalized, so they produce no Message/Thread and no task.
3. **Everything through the executor — concretely, for THIS ticket:** every status
   change is a `task_close` / `task_reopen` call and every log line a
   `task_append_log` call, all with actor `ticketstatus:jira` and `Call.TaskID` set;
   `internal/ticketstatus` contains no SQL against `tasks` or `task_events`
   (criterion 40 makes that structural); its only direct write is the pass's own
   `ticket_status_syncs` row — the `capture_decisions` / `classify_promotions`
   precedent.
4. **Nothing external without a delivery row — nothing outbound exists here.** The
   lookup makes two READ calls (`GET issue`, `GET myself`) and no write call of any
   kind; no `deliveries` row is created, read or mutated and no send adapter is
   imported. A closed task keeps any open delivery it has (D10): delivery state is
   not to be inferred from task state, per the "failing a delivery R8 already
   processed" landmine.
5. **Own-message loop closure — untouched.** The pass reads no messages, so
   capture's `direction='inbound'` filter and jira's `confirmDelivery` /
   `matchByBodyPrefix` path are not modified and cannot be widened. The lookup
   cannot interfere with them either: it writes only `issue:` raws under a provider
   the normalizer does not select, so no `normalized_messages` row and no delivery
   confirmation can arise from it. Named consequence, unchanged and correct: a later
   notification about a dropped ticket still resolves through `external_refs` and
   appends a task log to the closed task. A log on a closed task is a record, not a
   resurrection.
6. **Stealth attribution — nothing client-visible is produced.** Close and reopen
   reasons are stored prose composed from the ticket key, its status and the assignee
   comparison; no model authors anything and nothing this pass writes ever leaves
   switchboard.
7. **Orchestrator purity — no rule is added and no rule is modified.** The
   orchestrator sees a close exactly as it sees `task_close` today
   (`status_changed → closed` runs `ruleUnblockDependents`) and sees a reopen as a
   `status_changed` whose `to` is neither `delivered` nor `closed`, which returns
   nil. **The backwards-transition check the landmine demands, done:** the forward
   transition (`close`) fires only `ruleUnblockDependents`, and nothing re-blocks a
   dependent that was unblocked — accepted and named, and inert today because capture
   creates no `task_dependencies`. R8's `delivery_lifecycle` dedup record is written
   on the *parent* work task while the task it closes is the *child* Deliver task, so
   a marker check on the reopened task would not even fire; the real guard is D3's
   precondition — the pass reopens only what it recorded closing, and a task R8
   closed is recorded `last_action='none'` on first observation. Every action writes
   an audit row through the executor; every no-op writes the state row, so "why did
   nothing happen to this task" is answerable from the database.

## Sibling patterns to copy

- **Driver shape** (advisory lock on a dedicated connection, per-row decision,
  executor calls, counters printed unconditionally):
  `internal/capture/rules_store.go` `EvaluateRules` / `tryRulesLock`, and
  `internal/promote/store.go` `Run` / `tryLock`. Copy capture's log-and-skip lock
  policy, not promote's error policy, and say why in the comment.
- **Pure decision split**: `internal/promote/promote.go` `Decide` (values in,
  `Decision` out, no I/O) with `store.go` holding every query — plus
  `internal/capture/rules.go`'s header comment for why the split is enforced
  structurally.
- **Raw-first fetch + hash short-circuit**: `internal/connector/jira/ingest.go`
  `ingestIssue` / `upsertRaw`. `LookupIssues` is that function with the key set
  supplied instead of searched — reuse it rather than writing a second raw writer.
- **Injected client factory with a decrypted token**:
  `cmd/connectors/jira/main.go`'s `factory` closure (`pgp_sym_decrypt` per account).
  The lookup dependency is injected into `ticketstatus.Run` the same way, so
  `internal/ticketstatus` never handles a token and the integration suite passes a
  fake.
- **Observation sweep as the evidence**: `internal/connector/google/sink.go`
  `ConfirmObservedCalendarDeliveries`, called per verified snapshot from
  `pipedream_ingest.go` — the poll's observation is the fact, not a hook on
  re-normalization.
- **A cursor field written by merge, never as a blob**: `google/sink.go`'s
  `SaveCursorField` rationale and jira's `sync_cursor || $2::jsonb` (criterion 16).
- **`external_refs` lookup**: `internal/capture/rules_store.go` `taskForExternalRef`
  and `internal/connector/github/store.go` `PGTaskResolver.Resolve`.
- **Identity from the polling account**: `jira.Ingest`'s `/myself` call and
  `NormalizeIssue`'s direction rule (D12).
- **A typed, hand-armed project column**: `projects.classify_promote_after` (0021)
  and `projects.ai_classify` (0018) — the migration comments, the fail-closed
  default, and the "armed by a hand-run UPDATE, documented in the runbook"
  convention.
- **Multi-match refusal**: `internal/connector/slackweb/sink.go` `confirmDelivery`
  and `upworkcrm/sink.go` `confirmUpworkDelivery` — refuse rather than guess, and
  make the refusal visible (criterion 43's counters do that here).
- **Shared transition + typed row**: `internal/tools/close.go` `closeTask` and
  SWT-31's `dismissTask`.
- **Bind the key, never build it in SQL**:
  `internal/connector/upworkcrm/threadkey.go` and its `keyspelling_test.go`.
- **CLI subcommand**: `cmd/opsctl/main.go`'s `capture-rules` block
  (`list|add|run|report`) and `cmd/classify/main.go`'s `promoteCmd` for the
  `--dry-run` + JSON counters shape.
- **Column-fed predicate proof**: `internal/drafts`' locality regression
  (institutional landmine 6) — criterion 33 is that test for
  `ticket_assignee_gate`.
- **`FOR UPDATE SKIP LOCKED`**: deliberately NOT used. Single-instance pass over tens
  of rows serialized by an advisory lock, not a work queue; the only row lock is
  `closeTask`'s existing `SELECT ... FOR UPDATE` on one task.

## Verification protocol

Run in this order; do not commit before step 5 passes.

**0a. Blocking pre-check — prove both discriminators exist in the stored raw, and
see which sites have accounts.** Record the output in the delivery summary:

```bash
psql -h 192.168.50.49 -U ops -d ops -c "
SELECT id, provider, account_email, domain_default, scopes,
       sync_cursor->>'own_account_id' AS own_account_id
  FROM source_accounts WHERE provider LIKE 'jira%' ORDER BY id;"

psql -h 192.168.50.49 -U ops -d ops -c "
SELECT ri.external_id,
       ri.raw_json #>> '{fields,status,statusCategory,key}' AS category,
       ri.raw_json #>> '{fields,status,name}'               AS name,
       ri.raw_json #>> '{fields,assignee,accountId}'        AS assignee,
       (ri.raw_json #> '{fields}') ? 'assignee'             AS assignee_key_present,
       ri.ingested_at
  FROM raw_source_items ri
  JOIN source_accounts sa ON sa.id = ri.source_account_id
 WHERE sa.provider='jira' AND ri.external_id LIKE 'issue:%'
 ORDER BY ri.ingested_at DESC LIMIT 20;"
```

If `category` is NULL on every row, STOP: the fix is in ingestion (widen what
`GetIssue` requests), not a status-name list, and this SPEC needs amending. If
`assignee_key_present` is false everywhere, criterion 7's distinction must be
re-derived from real data before implementing D14.

Then measure the actual work — do **not** copy the counts into a test as literals
(the corpus is live; assert against a computation, not a frozen number):

```bash
psql -h 192.168.50.49 -U ops -d ops -c "
SELECT p.slug, t.status,
       ri.raw_json #>> '{fields,status,statusCategory,key}' AS category,
       ri.raw_json #>> '{fields,assignee,accountId}'        AS assignee,
       count(*)
  FROM external_refs er
  JOIN tasks t    ON t.id = er.task_id
  JOIN projects p ON p.id = t.project_id
  LEFT JOIN raw_source_items ri ON ri.external_id = 'issue:' || er.external_key
 WHERE er.system = 'jira'
 GROUP BY 1,2,3,4 ORDER BY 1,2,3;"
```

(That `||` is an operator's ad-hoc query; criterion 34 forbids it in code.) Expect
`collaboratory` rows with a category and `reengine` rows with NULLs — the latter are
the lookup's candidate set, and their count is the number of GETs the first Avviato
pass will make.

**0b. PREREQUISITE FROM SALVADOR — an Avviato API token.** The reengine half cannot
be verified without it. He creates it at
`https://id.atlassian.com/manage-profile/security/api-tokens` while logged in as his
Avviato identity, then:

```bash
export JIRA_API_TOKEN='<the token>'
eval "$(grep '^export OPS_TOKEN_KEY=' ~/.bashrc)"
DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/jira-auth add <his-avviato-email> \
  --site https://avviato.atlassian.net --projects LHH --lookup-only
DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/jira-auth list   # shows role=lookup
```

`add` verifies the credential with `/myself` before storing anything, so a bad token
fails here rather than mid-pass. **Until this exists, everything below except step 7
still runs** — reengine refs simply count `unpolled`.

**1. `go test ./...`** — the decision table (criteria 21-29), the issue-facts reader
including the three assignee shapes (6-7), the raw-id round trip (5), the lookup's
key selection, prefix routing, TTL and no-JQL assertions (9-10, 14-15, 17), tool
registration/validation/refusals (35-37), the spine-tool and actor assertions (39),
the structural scans (8, 20, 34, 40), and the migration ledger (4).

**2. `make integration`** — `db-up` + `migrate` (applies 0022 and 0023 to the compose
db on :5433) + `go test -tags integration ./...`. Covers write-then-read-back (11),
the `jira_lookup` isolation proofs (12), the cursor merge (16), the candidate query
and its refusals (30-32), the column-fed gate proof (33), the transaction and
idempotence behaviour (38, 44), and the dismissal suppression against a real
`task_dismissals` row (26, 28). The suite must be rerunnable: clean up in FK order
under a test-owned slug prefix and join the existing mutual-cleanup pact.

**3. Confirm the local db really migrated:**
`psql "postgres://ops:ops@localhost:5433/ops?sslmode=disable" -tAc "SELECT max(version) FROM schema_migrations"`
→ `0023`.

**4. Apply the migration to pg-main FIRST** (merging a migration is not applying it —
the five-migrations-behind incident):

```bash
psql -h 192.168.50.49 -U ops -d ops -tAc "SELECT max(version) FROM schema_migrations"   # expect 0022
DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/tools/migrate
psql -h 192.168.50.49 -U ops -d ops -tAc "SELECT max(version) FROM schema_migrations"   # expect 0023
```

**5. The "usable alone" smoke, on the real board — status half (no token needed).**

```bash
cd ~/projects/personal/switchboard
eval "$(grep '^export OPS_DATABASE_URL=' ~/.bashrc)"
alias opsctl='DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/opsctl'

opsctl ticket-status sync --dry-run     # read the plan line by line FIRST
opsctl ticket-status sync
opsctl ticket-status report
```

Check all five:
- the dry-run plan and the live counters agree, and `fetched` is 0 in the dry run
  (criterion 42: a dry run makes no HTTP calls);
- every task the pass closed has a `done` ticket:
  `SELECT task_id, status_category, status_name, drop_reason, last_action, closed_from_status FROM ticket_status_syncs ORDER BY task_id;`
- those tasks are gone from `/tasks?project=collaboratory` and present under
  `/tasks?project=collaboratory&status=closed`
  (`kubectl -n ops port-forward svc/dashboard 8085:80`);
- the audit trail exists:
  `SELECT actor, tool, task_id, created_at FROM audit_events WHERE actor='ticketstatus:jira' ORDER BY id DESC LIMIT 20;`
- a second `opsctl ticket-status sync` immediately after prints all-zero action
  counters and adds no `audit_events` rows (criterion 44 on real data).

**6. The return path, end to end.** Pick one ticket the pass just closed, reopen it in
the client's Jira (or transition it out of Done), then:

```bash
DATABASE_URL="$OPS_DATABASE_URL" OPS_TOKEN_KEY=... go run ./cmd/connectors/jira
opsctl ticket-status sync
```

The task must be back on `/tasks?project=collaboratory` in the status it held before,
with a `status_changed {from: closed, to: ...}` event on `/tasks/{id}`. Then dismiss
it (SWT-31's board control), close the ticket again, run the pass, reopen the ticket,
run the pass — and confirm it stays off the board with exactly one suppression log
line (D4).

**7. The assignee half + the lookup — needs step 0b.**

```bash
psql -h 192.168.50.49 -U ops -d ops -c \
  "UPDATE projects SET ticket_assignee_gate = true WHERE slug = 'reengine';"
opsctl ticket-status sync --dry-run      # expect would_fetch on the reengine refs
opsctl ticket-status sync --force        # first real fetch
opsctl ticket-status report
```

- `fetched` equals the reengine candidate count from step 0a — **no more**. Confirm
  no project-wide poll happened:
  `SELECT count(*) FROM raw_source_items ri JOIN source_accounts sa ON sa.id=ri.source_account_id WHERE sa.provider='jira_lookup';`
  must equal the candidate count, not the size of LHH;
- those raw rows are never normalized:
  `SELECT count(*) FROM raw_source_items ri JOIN source_accounts sa ON sa.id=ri.source_account_id WHERE sa.provider='jira_lookup' AND ri.normalized_at IS NOT NULL;`
  must be 0, now and after the next `connector-jira` run;
- no funnel movement: `normalized_messages`, `normalized_threads` and
  `capture_decisions` counts are unchanged across the pass;
- every reengine task still on the board has a ticket whose
  `fields.assignee.accountId` equals the lookup account's `own_account_id` (which the
  pass has now populated); the dropped ones show `drop_reason='not_assigned'`;
- re-assign one LHH ticket to Salvador in Jira, run `opsctl ticket-status sync
  --force`, and its task is back on the board;
- `collaboratory` counters are unchanged by arming reengine (the gate is per-project
  and read from the column — criterion 33's live counterpart);
- run once more WITHOUT `--force` inside the TTL and confirm `fetch_skipped_ttl`
  covers every candidate and `fetched` is 0.

**8. Recurring path.** The `*/15` behaviour only starts when the connector image is
rebuilt and `connector-jira`'s tag re-pinned — the kube session owns
`~/projects/personal/kube/switchboard/`. Until then the pinned image runs the old
code and the pass is hand-run only. That is expected, not a failure. Hand the
migration-applied fact, the new `jira_lookup` account and the new tag to that session
together; note that the CronJob already has `OPS_TOKEN_KEY`, so the lookup works
there with no manifest change beyond the tag.

## Open questions

None outstanding. Q1 (the Avviato source) was answered on 2026-09-09 with the
candidate-driven lookup and is folded in above; the record is in
`docs/tickets/jira-status-sync_OPEN_QUESTIONS.md`. Everything else was resolved
in-document — notably whether a human-dismissed task should resurface (D4, with the
non-redundancy argument spelled out), where the dismissal check belongs (D5), what
"me" is (D12), whether capture should gate creation (D13), the account-row shape
(D17) and the routing key (D18).

## Future work (not this ticket)

- A GitHub reader for the same reconciler (`external_refs.system='github'`): a closed
  issue or a merged PR dropping its task, using `internal/connector/github`'s stored
  payloads. The package boundary is drawn for it.
- The candidate-driven lookup generalised: any `external_refs` system whose provider
  can be fetched by key gets the same treatment, which is a cheaper shape than a
  poller for every small integration.
- Further presence facts behind the same `warranted` predicate — sprint, priority
  floor, label allow-list. Each is one term and one column; none is asked for yet.
- Surfacing `ticket_status_syncs` on `/funnel` as a read-only counter ("N tasks
  dropped by ticket state this week, split by reason; M refs with no snapshot").
- Feeding `drop_reason='not_assigned'` back into capture rule precision: a rule whose
  tasks are overwhelmingly other people's tickets is a rule to narrow — the same
  report SWT-31's `task_dismissals` join wants.
- Arming `collaboratory`'s assignee gate, once Salvador has seen how reengine's
  behaves.
- Revisiting the accepted limit (an unnotified ticket is never a candidate) if
  reengine ever becomes a main engagement: the honest fix then is a real poll, and it
  should be a deliberate ticket with the funnel consequences priced, not a quiet
  widening of this lookup.
