> Jira: SWT-17

# capture-rules — deterministic project assignment for normalized messages

**The four original open questions are answered and folded in** (Salvador,
2026-08-20). `docs/tickets/capture-rules_OPEN_QUESTIONS.md` holds the answers
verbatim.

**A fifth question (Q5) arose while implementing Q4 and is now also answered.**
Dropping the column killed the no-thread Upwork delivery fallback
(`internal/drafts/store.go:130-133`), whose client uuid was reachable only via
`client_person_id`. Confirmed **(a) — let it die** (Salvador, 2026-08-20), with
the reasoning sharpened: the branch aims at first-contact drafting, which the
policy matrix forbids ("existing threads only, ≤2 touches"), so it could only
ever fire out of policy. Verified empirically — 26 upwork identities, 0 without a
thread; the branch has never been reachable. §9 site E stands as written.

All questions are closed. Ready for `test-author`.

Two answers changed the shape of this ticket:

- **Q3 — unmatched IS triage's inbox.** Triage live consumes
  `action='unmatched'` decisions; deterministically routed messages are never
  re-triaged. A worker therefore reads `capture_decisions`, which needs an
  explicit invariant-2 argument rather than an emergent one. Design §8.
- **Q4 — `projects.client_person_id` is DROPPED in 0015.** Project selection
  moves wholly into `capture_rules`; `internal/drafts/store.go` is reworked to
  resolve the delivery target from the task's project + thread. This pulls
  step-8 delivery targeting into the ticket, against a worker with no live
  coverage — so the tests ARE the safety net. Design §9.

## Source

Ad-hoc request (Salvador, 2026-08-20), his routing rules in his words:

> Collaboratory = the ASU interns, plus Treetop Jira WEB and API projects, plus
> the collaboratory-www/gonoble repos (gonoble is Collaboratory's API).
> ReEngine = tickets on the Avviato Jira (project LHH on avviato.atlassian.net)
> or emails saying "Recommendation Engine". Both are Treetop work.

and, on polling Avviato:

> I don't want to poll avviato, it is not my main thing; that work is manual
> anyway so slack or email capturing those jira tickets as jobs on reengine for
> human resolution is ok.

Not a build-order step. Sits between step 2/9 (connectors, shipped) and step 6
(GPT triage, shipped in shadow): it is the deterministic half of "which project
does this belong to", which today does not exist.

## Goal

A DB-stored, priority-ordered rule engine — a pure function of
(message, rules) → project + optional external key — that runs as a
post-normalize pass inside the connector mains, records every evaluation, and
(in live mode only) creates ONE task per external ticket through the executor,
appending later notifications about the same ticket as task log events. It
becomes the single source of project assignment: `projects.client_person_id`
is dropped, and triage reads the engine's decisions both for the project and for
its own inbox.

**Usable alone** means: with only the four connector CronJobs deployed (no
triage, no orchestrator, no ops-mcp, no worker), a Slack notification from the
Jira app saying `LHH-23637` produces one `ready`, `assignee_type='human'` task
on the `reengine` project, visible on the already-deployed dashboard at
`/tasks` and `/tasks/{id}`, with its `external_refs` row rendered by the
existing task-detail handler — and the five follow-up notifications about
LHH-23637 land as log events on that same task instead of five more tasks.

Before that, in shadow mode (the default), the same pass records what it WOULD
have done for every message in the corpus and creates nothing.

## Verified current state

Established by reading the code on `main` and by the facts handed to this SPEC.
Line references are to `main` at `526233b`.

1. **The only project-assignment logic in the repo is 14 lines and is inert.**
   `internal/triage/store.go:113-126` resolves
   `SELECT id, slug FROM projects WHERE client_person_id=$1 ORDER BY id LIMIT 1`
   from `mc.PersonID`, which comes from `normalized_threads.participants[0]`
   (`store.go:97-110`). `participants` is `'[]'` for 16,959 of 16,985 threads —
   the google, jira and slackweb sinks hardcode it (`google/sink.go:249`,
   `jira/sink.go:195`, `slackweb/sink.go:139,152`), only `upworkcrm/sink.go:231`
   populates it — and `projects.client_person_id` is NULL on all four project
   rows. So the lookup resolves nothing, for anything.
2. **Nothing writes `projects.client_person_id`.** Migration
   `0004_gpt_triage.sql:6` creates it; `internal/triage/store.go` and
   `internal/drafts/store.go` read it; only test files insert it. Three test
   files reference it and must change with the drop:
   `internal/triage/integration_test.go:151-153,219-223,295` (which asserts the
   column EXISTS as a "migration 0004 artifact") and
   `internal/connector/upworkcrm/integration_test.go:83-86` (whose orphan-people
   cleanup subqueries the column, so it would fail with "column does not exist"
   rather than a test assertion).
3. **`external_refs` has 0 rows.** It has a writer in code —
   `link_external_ref` (`internal/tools/prci.go:45-59`), reached by
   `github.PGTaskResolver.Resolve` (`internal/connector/github/store.go:144-153`)
   and by agents over MCP — but neither has ever fired in production. This
   ticket makes the engine the first *systematic* writer.
4. **`capture` is the established shape for this kind of pass.**
   `internal/capture/observe.go:11-17`: "a post-normalize deterministic pass in
   the mold of `slackweb.ReconcileUnconfirmed`: SQL and Go, no LLM, no provider
   adapters, run by each connector's main after Normalize."
   `cmd/connectors/jira/main.go:79`, `cmd/connectors/google/main.go:145`,
   `cmd/connectors/google/watch.go:138` and `cmd/connectors/slackweb/main.go:77`
   already call it. `cmd/connectors/upworkcrm/main.go` does not.
5. **The connectors are the only switchboard code running.** Deployed: 4
   connector CronJobs + the dashboard. Not deployed: orchestratord, opsworker,
   triage, drafts, ops-mcp, fleetd. Anything that must produce visible effect
   today has to ship inside a connector main.
6. **`tasks.project_id` is `NOT NULL`** (`0001_initial.sql:116`) — no task can
   exist without a resolved project. That is why this engine is a prerequisite
   for triage going live, not a nicety.
7. **`cmd/connectors/github/main.go:61-67` is the precedent for a connector
   holding an executor**: registry → `tools.Register` → `policy.NewMatrix` →
   `executor.New`, actor `"ghpoll:github"`. No other connector main builds one.
8. **Highest migration is `0014_imap_mail.sql`.** Next is 0015.
9. **`normalized_messages.sender` is the raw From header** for gmail
   (`google/normalize.go:111` — `headers["from"]`, i.e. `Name <addr>`), so
   sender matching is substring, not equality.
10. **`internal/drafts` has NO integration coverage.** `worker_test.go` is the
    only test file and runs against a fake Store — its own header says "the SQL
    halves (Deliver-task queue, channel/thread resolution, advisory lock) belong
    to the integration suite" (`worker_test.go:8-10`), and that suite was never
    written. So `PGStore.DeliverTasks`/`resolve` — precisely the code Q4
    reworks — currently has zero automated coverage and no deployment
    (`cmd/drafts` is not running) to catch a regression.
11. **`thread_key` prefixes are connector-constructed and stable**, which makes
    them a sound basis for deriving a delivery channel: `gmail:{account}:{id}`
    (`google/normalize.go:106`), `upwork_crm:{client uuid}:{channel}`
    (`upworkcrm/normalize.go:99`, with `clientIDFromThreadKey` at
    `upworkcrm/sink.go:327` already extracting the uuid from it),
    `jira:{site_host}:{KEY}`, `slack:{ws}:{conv}[:{root}]`. By contrast
    `normalized_messages.channel` for upwork is CRM-supplied free text
    (`upworkcrm/normalize.go:106` copies `m.Channel`), so it is NOT a reliable
    channel discriminator.

## Vocabulary

"Route"/"routing" is NOT repo vocabulary — it appears in CLAUDE.md's prose
("routes to queues") and nowhere in the schema or package names. The closest
precedent is the `capture` package, and this pass has exactly its shape
(post-normalize, deterministic, run by connector mains). So:

- package: `internal/capture` (extended, not forked)
- tables: `capture_rules`, `capture_decisions`
- tools: `capture_rule_add`, `capture_rule_set_enabled`
- CLI: `opsctl capture-rules <list|add|run|report>`
- env: `CAPTURE_RULES_MODE`, `CAPTURE_RULES_SINCE`

Existing vocabulary is reused unchanged everywhere else: `tasks`,
`external_refs`, `projects`, `task_events`, `normalized_messages`,
`normalized_threads`, `source_accounts`.

## Design

### 1. Rules are typed data, one row per criterion

```sql
CREATE TABLE capture_rules (
  id              BIGSERIAL PRIMARY KEY,
  project_id      BIGINT NOT NULL REFERENCES projects(id),
  subproject      TEXT,
  criteria_type   TEXT NOT NULL CHECK (criteria_type IN
                    ('body_regex','sender','thread_key_prefix','thread_key_contains',
                     'source_slack_workspace','person')),
  pattern         TEXT NOT NULL,
  external_system TEXT CHECK (external_system IN
                    ('jira','github','upwork_crm','slack','gmail')),
  key_regex       TEXT,
  url_template    TEXT,
  priority        INTEGER NOT NULL DEFAULT 0,
  enabled         BOOLEAN NOT NULL DEFAULT true,
  note            TEXT,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (project_id, criteria_type, pattern)
);
CREATE INDEX capture_rules_eval_idx ON capture_rules (priority DESC, id) WHERE enabled;
```

Criteria semantics (all evaluated in Go, against one loaded message):

| criteria_type | matches when | matched against |
|---|---|---|
| `body_regex` | `regexp.MatchString(pattern, subject+"\n"+body_text)` | `normalized_messages.subject`, `.body_text` |
| `sender` | case-insensitive substring of the raw From/author string | `normalized_messages.sender` |
| `thread_key_prefix` | `strings.HasPrefix(thread_key, pattern)` | `normalized_threads.thread_key` |
| `thread_key_contains` | `strings.Contains(thread_key, pattern)` | `normalized_threads.thread_key` |
| `source_slack_workspace` | message's source account is `provider='slack_web'` and `account_email = pattern + "@slack-web.local"` | `raw_source_items.source_account_id → source_accounts` |
| `person` | `pattern` (a `people.id` as text) appears in the thread's `participants` array | `normalized_threads.participants` |

`body_regex` and `key_regex` are Go `regexp` (RE2 — no catastrophic
backtracking). Patterns are compiled and REJECTED at rule-insert time, not at
run time: a bad regex discovered inside a CronJob is a silent routing outage.
Case-insensitivity is spelled by the rule author as `(?i)`.

`source_slack_workspace` is deliberately resolved through
`source_accounts.account_email` (the `{workspace_id}@slack-web.local` synthetic
identity from SWT-13) rather than by prefix-matching `slack:{ws}:` on the
thread_key. Both work today; the account link survives a thread_key format
change, and a rule that silently stops matching after a format change is the
SWT-13 landmine in a third costume.

`person` is the criteria type that subsumes the dropped
`projects.client_person_id`: one rule row does the job the special-case column
did. It matches nothing until `participants` is populated for the non-upwork
sinks (out of scope, below) — which is exactly as much as the column achieves
today, so nothing is lost in the meantime.

### 2. Evaluation is a pure function; first match wins

```go
// internal/capture/rules.go — no DB, no network, no LLM.
func Evaluate(msg Message, rules []Rule) Match
```

Rules arrive pre-sorted `ORDER BY priority DESC, id ASC` and are scanned in that
order. The FIRST match sets `Match.Rule` / `Match.ProjectID`; scanning continues
so that `Match.AllRuleIDs` records every rule that matched, and
`Match.Ambiguous` is true when two matched rules name DIFFERENT projects.
Ambiguity is recorded and reported, never used to change the outcome — total and
reproducible beats clever.

Priority is load-bearing in two distinct ways, and both need pinning tests:

- **Across rules for one project:** `treetopllc/collaboratory-www` is a
  Collaboratory repo, but a message in that thread mentioning `LHH-23637` routes
  to ReEngine because `body_regex LHH-[0-9]+` sits at priority 100 and the repo
  rule at 50.
- **Across two projects inside ONE Slack workspace** (Q2): the Treetop workspace
  `T0360B84U` carries a priority-1 catch-all to `collaboratory`, while ReEngine
  tickets are claimed out of the same workspace by the priority-100 LHH rule.
  The catch-all only ever sees what the specific rules did not claim. A single
  workspace therefore feeds two projects, and the ONLY thing keeping them apart
  is the priority ordering — so a message in `T0360B84U` containing `LHH-23637`
  must resolve to `reengine`, not `collaboratory`, with `ambiguous=true`
  (criterion 19).

### 3. The same match yields the dedup key

A matching rule with `external_system` set also produces an external key:

- `key_regex` is applied to the same text the criterion matched (`subject+body`
  for `body_regex`/`sender`, the `thread_key` for the thread_key/workspace/person
  types); the first capture group, or the whole match if the regex has no group,
  is the `external_key`.
- `key_regex` NULL and `criteria_type='body_regex'` ⇒ `pattern` is reused as the
  key regex (the common case: `LHH-[0-9]+` is both the selector and the key).
- `key_regex` NULL otherwise ⇒ the key is the `thread_key` verbatim.
- `url_template` builds `external_refs.external_url` by substituting `{key}`
  once, e.g. `https://avviato.atlassian.net/browse/{key}`.
- `external_system` NULL ⇒ **project attribution only, no task**.

That last case is a scope boundary, not an oversight: turning arbitrary chatter
into tasks is triage's job (step 6, with confidence and a human-review lane).
A deterministic rule that created one task per Slack thread in a 40k-message
workspace would manufacture exactly the backlog this ticket exists to avoid —
and with Q2 answered, `T0360B84U` IS such a rule, so this is load-bearing.

Dedup then reduces to a lookup nothing else in the system does yet:

```sql
SELECT task_id FROM external_refs
 WHERE system = $1 AND external_key = $2
 ORDER BY created_at DESC, id DESC LIMIT 1
```

— the same query shape as `github.PGTaskResolver.Resolve`
(`internal/connector/github/store.go:116-119`). Hit ⇒ append a log to that task.
Miss ⇒ create the task, then link the ref.

`external_refs.system`'s CHECK currently allows only `jira|github|upwork_crm`
(`0001_initial.sql:181`); 0015 extends it with `slack` and `gmail` so a
thread-keyed dedup ref is expressible in the existing vocabulary instead of a
new table. The drop/add of a CHECK constraint is safe here for the same reason
0009's was: migrate runs each file in one transaction.

Writing thread_keys into `external_refs.external_key` verbatim also closes the
gap `internal/capture/observe.go:66-72` documents ("external_refs keys are
agent-chosen free text that cannot be matched against a thread_key"). For rows
this engine writes, they can be — and §9 depends on it.

### 4. Every evaluation writes a row

```sql
CREATE TABLE capture_decisions (
  id                 BIGSERIAL PRIMARY KEY,
  message_id         BIGINT NOT NULL REFERENCES normalized_messages(id),
  raw_source_item_id BIGINT REFERENCES raw_source_items(id),
  mode               TEXT NOT NULL CHECK (mode IN ('shadow','live')),
  matched_rule_id    BIGINT REFERENCES capture_rules(id),
  project_id         BIGINT REFERENCES projects(id),
  matched_rule_ids   BIGINT[] NOT NULL DEFAULT '{}',
  ambiguous          BOOLEAN NOT NULL DEFAULT false,
  action             TEXT NOT NULL CHECK (action IN
                       ('unmatched','attributed','task','task_log')),
  external_system    TEXT,
  external_key       TEXT,
  task_id            BIGINT REFERENCES tasks(id),
  reason             TEXT,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- One live action per message, FOREVER. Structural, not advisory.
CREATE UNIQUE INDEX capture_decisions_live_uniq
  ON capture_decisions (message_id) WHERE mode = 'live';
CREATE INDEX capture_decisions_message_idx ON capture_decisions (message_id, id DESC);
CREATE INDEX capture_decisions_project_idx ON capture_decisions (project_id, created_at);
```

`action` records the DECISION; `mode` records whether it was applied. So a
shadow row saying `action='task'` and a live row saying `action='task'` are
directly diffable, which is the entire point of shadow mode.
`action='unmatched'` is the only action with `project_id IS NULL`, and §8 keys
triage's inbox on exactly that.

Why not `policy_decisions`: that table is `(audit_event_id, tool, channel,
decision IN ('allow','deny','needs_approval'), rule)` (`0001_initial.sql:250-260`)
— it answers "was this TOOL CALL permitted", is written only by
`audit.PGStore.RecordPolicy` from inside `Executor.Execute`
(`internal/executor/executor.go:74`), and has no column for a project, a
message, or a matched rule. Overloading it would corrupt the one place a
reviewer looks to answer "what did the policy matrix decide", and would force a
fake `decision` value on an evaluation that is not a permission question.
`capture_decisions` parallels `policy_decisions` in naming, the way
`policy_decisions` already parallels `decisions`.

Writing `capture_decisions` directly (not through the executor) follows the
triage precedent: `triage` writes `ai_runs`/`ai_extractions` with its own store.
Tool-action tables — `tasks`, `external_refs`, `task_events` — are never written
directly here; see invariant 3 below.

### 5. Where it runs, and once at a time

`capture.EvaluateRules(ctx, pool, ex, cfg)` is called by every connector main
immediately after `capture.ObserveOutbound` (and in
`cmd/connectors/google/watch.go` after the same call). Unlike `ObserveOutbound`,
it is channel-agnostic — it reads `normalized_messages` regardless of channel —
so it does not need per-connector configuration, and any one connector pass
covers messages ingested by all of them.

Four CronJobs can overlap, so the pass takes `pg_try_advisory_lock` with key
`0x5157_0015` (the established convention: orchestrator `0x51570005`, triage
`0x51570006`). Failure to acquire is a clean no-op returning 0, never an error —
a connector run must not fail because another connector happened to be running.

`cmd/connectors/upworkcrm/main.go` gets the call too; it currently calls nothing
from `capture` (there is no `capture.Channel` for upwork), and this pass needs
none.

### 6. Mode and horizon (the anti-flood rules)

- `CAPTURE_RULES_MODE` ∈ `shadow` (default) | `live`. Per-process, set in the
  CronJob manifest — which lives in the kube repo, so going live is a handoff.
- Pending filter, both modes: inbound messages with **no `mode='live'` decision
  row**, `ORDER BY sent_at, id`.
- `CAPTURE_RULES_SINCE` (Go duration) bounds `sent_at`. Default in live mode is
  `720h`, matching `capture.DefaultObserveHorizon` and parsed with the same
  defensive shape as `capture.ObserveHorizon` (unparseable or non-positive falls
  back to the default; `"720"` is not a Go duration and must not become 720ns).
  Default in shadow mode is 0 = unbounded, because the whole corpus is what you
  want to diff.
- `--all` (re-evaluate messages that already have decision rows) is
  **shadow-only** and is refused in live mode: `task_append_log`
  (`internal/tools/appendlog.go`) has no dedup of its own, so a live replay would
  double-append. The live dedup is `capture_decisions_live_uniq`, and the
  refusal is what keeps it meaningful.
- Consequence, stated so it is chosen rather than discovered: once the mode
  flips to live, only messages inside the horizon are acted on. Three years of
  history stays un-tasked unless someone deliberately runs a bounded backfill
  (`opsctl capture-rules run --live --since 8760h`).

### 7. Task shape

For `action='task'`, through the executor:

1. `create_task` with `project` = the rule's project slug, `subproject` = the
   rule's subproject, `assignee_type='human'`, `title` and `body` derived
   deterministically, `priority` = 0. `create_task` already inserts
   `status='ready'` (`internal/tools/createtask.go:144`) — which is what
   ReEngine wants, since it is `execution=manual` and human-resolved by design.
2. `link_external_ref` with `system`/`external_key`/`external_url` from the
   match.

Title: `"{external_key} — {subject-or-first-line}"`, truncated with
`textmatch.NormalizedPrefix` (the ONE spelling of whitespace-collapsed,
rune-safe truncation — SWT-16) to 120 runes. Body: the matched message's
channel, thread_key, sender, sent_at, `normalized_messages.id` and the matched
rule id, plus the body preview. No model, no invention: every field is copied
from stored data.

For `action='task_log'`, `task_append_log` with a message naming the message id,
channel, sender and preview, so the six LHH-23637 notifications read as one task
with five appended events.

### 8. Triage's two reads of `capture_decisions` (Q3)

Triage now reads this table twice, for two different purposes. Both live in
`internal/triage/store.go`; spelling them out together is deliberate, because
they key on the same rows and could otherwise drift apart.

**(a) The project source.** `AssembleContext`'s project lookup
(`store.go:113-126`) is REPLACED — not fronted — by the latest decision for the
message:

```sql
SELECT DISTINCT ON (cd.message_id) p.id, p.slug
  FROM capture_decisions cd JOIN projects p ON p.id = cd.project_id
 WHERE cd.message_id = $1 AND cd.project_id IS NOT NULL
 ORDER BY cd.message_id, cd.id DESC
```

The `client_person_id` fallback disappears with the column (§9). `mc.PersonID`
and `mc.PersonName` stay — they come from `participants`, not from the dropped
column, and the prompt still uses them.

**(b) The inbox.** Triage's pending filter (`store.go:29-48`) gains a second
condition. Three states must be kept distinct, and conflating the first two is
the trap this section exists to prevent:

| state | meaning | triage does |
|---|---|---|
| no `capture_decisions` row at all | the engine has not evaluated this message yet | **SKIP** — not yet classifiable |
| latest decision `action='unmatched'` (`project_id IS NULL`) | evaluated; no rule covers it | **CONSUME** — this is the inbox |
| latest decision `action` ∈ `attributed`/`task`/`task_log` | deterministically routed | **NEVER** re-triage |

A message with no decision row is NOT unmatched; it is unseen. If a triage run
outruns the capture pass — trivially possible, since the pass hitchhikes on
connector CronJobs — treating unseen as unroutable would hand every fresh
message to the model before the deterministic rules ever looked at it, quietly
inverting the whole design. The filter is therefore an EXISTS on an `unmatched`
latest decision, never a `NOT EXISTS` on decisions generally.

**Go-live ordering constraint (write it in the runbook):** capture must be live
BEFORE triage goes live. While capture is in shadow, a matched message has a
decision row with a `project_id` and no task — capture did not create it
(shadow), and triage will not (routed). Such messages fall through both. That is
harmless while triage is also shadow (today), and becomes a silent gap the day
triage goes live first. The two mode flips are ordered, not independent.

Consequence worth stating: once this lands, triage stops extracting for
deterministically routed messages, in both modes. That saves tokens and is the
point — but it also means the triage shadow diff no longer covers those
messages. Their diff surface is the capture report (§ Verification), not
`triage report`.

### 9. Dropping `projects.client_person_id`: the drafts rework (Q4)

0015 drops the column. Every read site is replaced below; none is hand-waved,
because `internal/drafts` has no integration coverage and no deployment (fact
10) to catch a mistake.

**Site A — client display name.** `DeliverTasks` selects
`COALESCE(pe.display_name, p.client, '')` via `LEFT JOIN people pe ON
pe.id = p.client_person_id` (`store.go:28,37`).
→ `COALESCE(NULLIF(p.client,''), p.name)`; the `people` join is deleted.
`NULLIF` because `projects.client` is `''` (not NULL) on the real rows, and
`p.name` is `NOT NULL` — so `ClientName` is now never empty, where today an
unmapped project renders as `orDash` "—" in the prompt
(`internal/drafts/drafts.go:214`). Strictly better, no new dependency.

**Site B — the task→thread resolution** (new, and the foundation for C and D).
One helper replaces both person-based queries:

```sql
SELECT nt.id, nt.thread_key
  FROM external_refs er
  JOIN normalized_threads nt
    ON nt.thread_key = er.external_key
    OR (er.system = 'jira' AND nt.thread_key LIKE 'jira:%:' || er.external_key)
 WHERE er.task_id = $1
 ORDER BY er.created_at DESC, er.id DESC, nt.id DESC
 LIMIT 1
```

tried on the PARENT task first (R3 creates the Deliver task as a child of the
work task, and the engine links the ref to the work task), then on the Deliver
task itself. The `OR` arm covers jira refs, whose `external_key` is a ticket key
rather than a thread_key; every other system this engine writes stores the
thread_key verbatim (§3). Deterministic, total, and volume-safe — it runs once
per Deliver task, not per message.

**Site C — gmail thread resolution** (`store.go:87-102`). Both queries go: the
`person_identities` join with the `split_part(replace(replace(...)))` sender
parsing, and its ILIKE fallback. Replaced by Site B's helper. This is a
substantial simplification — that sender-parsing expression is a canonicalization
landmine of the SWT-13 family waiting to happen — and it makes the resolved
thread the same object for every channel.

**Site D — channel selection** (`store.go:70-80`). New precedence:

1. `p.policies->>'delivery_channel'` when non-empty — explicit config still
   wins, unchanged.
2. Otherwise derive from the resolved thread's `thread_key` PREFIX (fact 11):
   `gmail:` → `gmail`, `upwork_crm:` → `upwork_chat`.
3. Otherwise `p.send_from_account IS NOT NULL` → `gmail` (project has a mail
   identity but no thread was found; yields the existing "gmail with
   ThreadID==nil" unresolvable state the worker already handles by skipping).
4. Otherwise `""` (unresolvable).

`hasUpworkIdentity` (`store.go:142-148`) is deleted. Note the precedence change:
today `send_from_account` beats the upwork branch; now the actual conversation
beats both, which is the correct answer for a reply and is also what
`draft_delivery` requires (a gmail draft without a thread is rejected —
`internal/capture/observe.go:79-80`, "draft_delivery requires it"). A thread on
`jira:` or `slack:` yields `""` unless `delivery_channel` says otherwise:
expanding the draft worker into `jira_comment`/`slack_reply` is step-9 work and
is deliberately NOT bundled here.

**Site E — the upwork client UUID** (`store.go:114-137`). With a thread, the
target is the thread_key and the UUID was never needed —
`dt.TargetRef = threadKey` already. The UUID was needed only for the NO-thread
fallback that synthesizes `upwork_crm:{uuid}:upwork` for a client with no
ingested conversation. **That fallback is removed**, deliberately: the policy
matrix already restricts Upwork to "existing threads only, ≤2 touches"
(CLAUDE.md), so drafting into a conversation that does not exist was already
outside policy, and without a person id there is no honest way to reach the
uuid. A Deliver task with no resolvable upwork thread now yields `Channel=""`
and the worker skips it — the same outcome as any other unresolvable task, and
one the worker already implements. (Should a client-level upwork target ever be
wanted again, the honest route is an `external_refs` row with
`system='upwork_crm'`, which the engine can write from a rule — not a person
lookup.)

**Site F — triage.** `store.go:113-126`'s fallback query goes; see §8(a).

**Coverage is the safety net.** A new `internal/drafts/store_integration_test.go`
(build tag `integration`) is REQUIRED by this ticket — the suite
`worker_test.go:8-10` says should exist and never did. Criteria 22-25 specify
it. Without a running `cmd/drafts` there is nothing else that would notice a
regression, and that trade was accepted explicitly when Q4 was answered.

## Acceptance criteria

1. Migration `0015_capture_rules.sql` creates `capture_rules` and
   `capture_decisions` as above, extends `external_refs.system`'s CHECK with
   `slack` and `gmail`, and drops `projects.client_person_id`. Forward-only;
   `make migrate` on a fresh db succeeds and `schema_migrations` reaches 0015.
2. `capture.Evaluate(msg, rules)` is a pure Go function: no `pgxpool`, no
   `context`, no provider import in its file. Its unit tests run under
   `go test ./...` with no database and no network.
3. Given the fixture rules below, a message whose body contains `LHH-23637` and
   whose thread_key contains `treetopllc/collaboratory-www` resolves to
   `reengine`, not `collaboratory`, and the decision row's `matched_rule_ids`
   contains both rule ids with `ambiguous=true`.
4. Evaluation order is `priority DESC, id ASC` and first match wins; a test
   pins that reordering two equal-priority rules by id changes the winner.
5. A rule inserted with an uncompilable `pattern` or `key_regex` is REFUSED at
   insert time with an error naming the offending field; no row is written.
6. Every evaluated message produces exactly one `capture_decisions` row per run,
   including `action='unmatched'` when nothing matched.
7. Shadow mode (default) creates zero rows in `tasks`, `external_refs`,
   `task_events` and `deliveries`. An integration test asserts the counts of all
   four are unchanged across a shadow run over a fixture corpus.
8. In live mode, six inbound messages carrying `LHH-23637` produce exactly ONE
   `tasks` row (project `reengine`, `assignee_type='human'`, `status='ready'`),
   exactly ONE `external_refs` row
   (`system='jira'`, `external_key='LHH-23637'`,
   `external_url='https://avviato.atlassian.net/browse/LHH-23637'`), and FIVE
   `task_events` rows of type `log` on that task.
9. Re-running the live pass over the same six messages creates nothing and
   appends nothing (`capture_decisions_live_uniq` holds; the run reports 0).
10. `--all` in live mode exits with an error and performs no writes.
11. Outbound messages are never evaluated: the pending filter is
    `direction='inbound'`, mirroring `internal/triage/store.go:34`. A test
    inserts an outbound message matching a rule and asserts no decision row and
    no task.
12. A matched rule with `external_system` NULL yields `action='attributed'`:
    a decision row with a `project_id` and no task, in live mode.
13. Four concurrent passes serialize: the advisory lock is taken and a pass that
    cannot acquire it returns `(0, nil)` and prints a one-line skip, never an
    error exit.
14. `opsctl capture-rules report [--since]` prints, from `capture_decisions`
    only: per-project matched counts; DISTINCT `(external_system, external_key)`
    proposed-task counts (so 15 messages over 5 tickets reads as "5 tasks, 15
    messages"); the ambiguous list; and the top unmatched senders and thread_key
    prefixes by volume.
15. `capture_rule_add` and `capture_rule_set_enabled` are registered on the
    executor, are `humanOnly` in `internal/policy/matrix.go`, and are NOT listed
    in `internal/mcpserver/schemas.go`. A test asserts an agent-shaped actor
    (`mcp:opsworker-x`) is denied.
16. Every `tasks` / `external_refs` / `task_events` write made by this package
    goes through `executor.Execute`; a test greps or reflects that
    `internal/capture` contains no `INSERT INTO tasks`, `INSERT INTO
    external_refs`, or `INSERT INTO task_events`.
17. **(Q3)** Triage's project comes from the latest `capture_decisions` row with
    a non-null `project_id` and from nowhere else; the `client_person_id` query
    is gone. `internal/triage/integration_test.go` is updated accordingly —
    including its criterion-2 assertion that the column EXISTS
    (`integration_test.go:219-223`), which this ticket invalidates.
18. `go test ./...` and `make integration` are green.
19. **(Q2)** A message in Slack workspace `T0360B84U` whose body contains
    `LHH-23637` resolves to `reengine` with `ambiguous=true` and both rule ids in
    `matched_rule_ids`; a message in the same workspace matching no specific rule
    resolves to `collaboratory` with `action='attributed'` and creates no task,
    in live mode. One workspace, two projects, separated only by priority.
20. **(Q3)** Triage's pending filter distinguishes the three states of §8(b): a
    message with NO decision row is skipped; a message whose latest decision is
    `action='unmatched'` is consumed; a message whose latest decision names a
    project is never consumed. All three are asserted in the triage integration
    suite.
21. **(Q3)** The go-live ordering constraint is documented in
    `docs/runbooks/capture-rules.md`: capture live before triage live, with the
    fall-through-both reason stated.
22. **(Q4)** `internal/drafts/store_integration_test.go` exists (build tag
    `integration`, skipping without `DATABASE_URL`) and covers `DeliverTasks`
    end to end against Postgres. This suite is new — none existed.
23. **(Q4)** A Deliver task whose parent has an `external_refs` row keyed on a
    `gmail:` thread_key resolves `Channel='gmail'` with `ThreadID` set to that
    thread, with no `people` or `person_identities` row anywhere in the fixture.
24. **(Q4)** The same shape for upwork: a parent with an `external_refs` row
    keyed on an `upwork_crm:{uuid}:upwork` thread_key resolves
    `Channel='upwork_chat'` and `TargetRef` = that thread_key exactly.
25. **(Q4)** Precedence and negative cases: `policies->>'delivery_channel'`
    overrides the thread-derived channel; a task with no resolvable thread and a
    project with `send_from_account` set yields `Channel='gmail'`,
    `ThreadID=nil`; a task with no resolvable thread and no `send_from_account`
    yields `Channel=''`; a thread on a `jira:`/`slack:` key yields `Channel=''`.
26. **(Q4)** `ClientName` falls back to `projects.name` when `client` is empty,
    and no query in `internal/drafts` references `client_person_id`,
    `people`, or `person_identities`.
27. **(Q4)** `internal/connector/upworkcrm/integration_test.go:83-86`'s cleanup
    no longer references the dropped column (it would fail with "column does not
    exist", not an assertion), and that suite still passes under `-p 1`.

### Fixture rules (the acceptance data)

Seeded by runbook, not by migration (see "Decisions made unilaterally"):

| project | criteria_type | pattern | ext system | key_regex | url_template | prio |
|---|---|---|---|---|---|---|
| reengine | body_regex | `LHH-[0-9]+` | jira | (= pattern) | `https://avviato.atlassian.net/browse/{key}` | 100 |
| reengine | sender | `jira@avviato.atlassian.net` | jira | `[A-Z]+-[0-9]+` | `https://avviato.atlassian.net/browse/{key}` | 100 |
| collaboratory | thread_key_prefix | `jira:treetopllc.jira.com:WEB-` | jira | `[A-Z]+-[0-9]+$` | (per site) | 50 |
| collaboratory | thread_key_prefix | `jira:treetopllc.jira.com:API-` | jira | `[A-Z]+-[0-9]+$` | (per site) | 50 |
| collaboratory | thread_key_prefix | `jira:treetopllc.jira.com:OPS-` | jira | `[A-Z]+-[0-9]+$` | (per site) | 50 |
| collaboratory | thread_key_contains | `treetopllc/collaboratory-www` | NULL | — | — | 50 |
| collaboratory | thread_key_contains | `treetopllc/gonoble` | NULL | — | — | 50 |
| collaboratory | source_slack_workspace | `T0HPR78RX` | NULL | — | — | 10 |
| collaboratory | source_slack_workspace | `T0360B84U` | NULL | — | — | 1 |

The last row is Q2's answer: the Treetop workspace (40,452 messages, 59% of the
corpus) attributes to Collaboratory at priority 1, attribution only, no task. It
sits below every specific rule on purpose — the priority-100 LHH rules claim
ReEngine tickets out of the same workspace first, and the Jira/repo rules claim
what is theirs, so the catch-all only ever sees the remainder. Criterion 19
pins that ordering; it is the one place where two projects share one source and
nothing but `priority DESC` separates them.

The "emails saying Recommendation Engine" half of Salvador's ReEngine rule is
expressible as `body_regex (?i)recommendation engine` → reengine,
`external_system` NULL (no ticket key in prose). It is deliberately NOT in the
fixture set: it is attribution-only data a human adds after the shadow report
shows how often it fires and on what.

## Data model changes

Migration **0015_capture_rules.sql**:
- `CREATE TABLE capture_rules` (+ `capture_rules_eval_idx`, UNIQUE
  `(project_id, criteria_type, pattern)`).
- `CREATE TABLE capture_decisions` (+ `capture_decisions_live_uniq` partial
  unique, `capture_decisions_message_idx`, `capture_decisions_project_idx`).
- `ALTER TABLE external_refs DROP CONSTRAINT external_refs_system_check` /
  re-add with `('jira','github','upwork_crm','slack','gmail')`.
- `ALTER TABLE projects DROP COLUMN client_person_id` (Q4). Forward-only: there
  is no down migration and the column is not recreated. Data loss is nil (NULL
  on all four rows); code loss is the drafts rework in §9, which must land in
  the same commit or `internal/drafts` fails to compile against the schema.

**Landmine to respect:** `capture_decisions_live_uniq` is a PARTIAL unique
index. Any `ON CONFLICT` against it MUST restate the predicate
(`ON CONFLICT (message_id) WHERE mode='live' DO NOTHING`), exactly as
`task_events_outbound_observed_uniq` requires — arbiter inference matches a
partial index only when the predicate is repeated, and omitting it raises "no
unique or exclusion constraint matching the ON CONFLICT specification" at
runtime, inside a CronJob.

**Second landmine, from the drop:** a dropped column does not fail a query at
compile time. Two integration suites reference `client_person_id` in SQL
(`internal/triage/integration_test.go`,
`internal/connector/upworkcrm/integration_test.go:83-86`) and will fail at
RUNTIME with "column does not exist" — the upwork one inside cleanup, where a
failure is easy to misread as the cross-pollution pact breaking. Fix both in the
same change; criteria 17 and 27.

## API / MCP tool changes

Two NEW executor tools, registered in `internal/tools/createtask.go`'s `Register`
list, handled in a new `internal/tools/capturerules.go`:

- `capture_rule_add` — args `{project, criteria_type, pattern,
  subproject?, external_system?, key_regex?, url_template?, priority?, note?}`
  → `{rule_id}`. Validate: project slug resolves; criteria_type in the enum;
  pattern non-empty; `regexp.Compile` on `pattern` when
  `criteria_type='body_regex'` and on `key_regex` when present;
  `url_template` if present contains `{key}`; `external_system` in the extended
  `external_refs.system` enum.
- `capture_rule_set_enabled` — args `{rule_id, enabled}` → `{updated}`.

Both go through the standard path (`Executor.Execute`: registry lookup →
validate → policy check → audit start → policy record → handler → audit
complete), both are added to `policy.humanOnly` so only `dashboard:` / `opsctl:`
/ `manual:` actors may change routing, and neither is MCP-listed — an agent
must not be able to redirect the funnel.

**No new tool is needed for the pass itself.** It reuses `create_task`,
`link_external_ref` and `task_append_log`, all of which fall through
`policy.Matrix` to `policy.NewStatic` (they are neither `humanOnly` nor
`sendShaped`), so a connector-shaped actor is allowed — same as the github
poller's `"ghpoll:github"`. Actor spelling for this pass:
`"capture:{connector}"` (e.g. `capture:slackweb`).

Existing MCP surface is unchanged; `internal/mcpserver/schemas.go` is not
touched. `drafts.DeliverTask`'s exported shape is unchanged by §9 — only the SQL
behind it changes — so `internal/drafts/worker_test.go`'s imposed surface
(`worker_test.go:29-40`) still holds.

## MQTT topics

None. This pass publishes and subscribes to nothing. `fleet.NewSpineClient` is
not used; no heartbeat, no command topic, no LWT.

## SPEC re-validated 2026-08-28, before implementation

This SPEC was written on 2026-08-20. SWT-18 and SWT-19 have since landed and both
touched files it depends on, so its assumptions were re-checked against `main`
rather than trusted. **The design holds unchanged.** Three checks:

- **§9's rework targets still exist.** `internal/drafts/store.go` still carries
  `client_person_id`, `hasUpworkIdentity` and the `people`/`person_identities`
  joins that sites A-E describe. SWT-19 rewrote the *upwork target resolution*
  inside the same function (roomed-thread preference, and a refusal to guess when
  a client has several rooms), so the rework must PRESERVE that behaviour rather
  than reverting it — the multi-room refusal is SWT-20's shipped mitigation and
  removing it silently reopens a wrong-room send.
- **Migration 0015 is still free.** Highest applied and highest in the repo is
  `0014_imap_mail.sql`; SWT-19 added none, exactly as it predicted.
- **The fixture rules are unaffected by SWT-19's key change.** They match on
  `jira:` and github thread-key prefixes, not upwork ones, so the new
  `upwork_crm:{client}:room:{id}` shape does not touch them. A `thread_key_prefix`
  rule on `upwork_crm:` would still match either shape, which is the property the
  prefix design was chosen for.

**Two files must be added to "Files likely to touch" — neither existed when this
SPEC was written**, and both reference the column §9 drops:

- `internal/connector/upworkcrm/matcherhardening_regression_integration_test.go`
  (SWT-18) — its cleanup subqueries `projects.client_person_id`.
- `internal/drafts/store_upwork_room_integration_test.go` (SWT-19) — same cleanup
  subquery, plus an `INSERT` that SETS the column.

A third hit, `internal/tools/delivery_lifecycle_integration_test.go`, is a comment
only and needs nothing. `internal/tools/delivery.go` likewise mentions the column
in prose explaining why SWT-20's binding waits for it — no code dependency.

Dropping a column is exactly the change where a stale file list bites: the
migration succeeds, and the failure surfaces later as a test that cannot clean up
after itself.

## Files likely to touch

New:
- `migrations/0015_capture_rules.sql`
- `internal/capture/rules.go` — `Rule`, `Message`, `Match`, `Evaluate` (pure)
- `internal/capture/rules_store.go` — rule load, pending-message query, decision
  writes, advisory lock, the `EvaluateRules` driver
- `internal/capture/rules_test.go` — pure unit tests (no db)
- `internal/capture/rules_integration_test.go` — build tag `integration`
- `internal/capture/rulesreport.go` — the report, in the mold of
  `triage.Report`
- `internal/tools/capturerules.go` — the two executor tools
- `internal/drafts/store_integration_test.go` — **new suite** (Q4, criteria
  22-25); none existed for this package
- `docs/runbooks/capture-rules.md` — seeding the fixture rules, shadow routine,
  go-live checklist including the capture-before-triage ordering (criterion 21)

Modified:
- `internal/tools/createtask.go` — two entries in the `Register` list
- `internal/policy/matrix.go` — two entries in `humanOnly`
- `cmd/opsctl/main.go` — `capture-rules` subcommand (list/add/run/report)
- `cmd/connectors/jira/main.go`, `cmd/connectors/google/main.go`,
  `cmd/connectors/google/watch.go`, `cmd/connectors/slackweb/main.go` — call
  after the existing `capture.ObserveOutbound`; these mains currently build no
  executor, so they gain the four-line
  `registry → tools.Register → policy.NewMatrix → executor.New` block copied
  from `cmd/connectors/github/main.go:61-64`
- `cmd/connectors/upworkcrm/main.go` — same, first `capture` call in that main
- `internal/triage/store.go` — the decision-only project lookup (§8a) and the
  three-state inbox filter (§8b)
- `internal/drafts/store.go` — the §9 rework: sites A-E, `hasUpworkIdentity`
  deleted, `people`/`person_identities` joins gone
- `internal/triage/integration_test.go` — drops the `client_person_id` insert
  and the column-exists assertion; adds the §8(b) three-state cases
- `internal/connector/upworkcrm/integration_test.go` — cleanup no longer
  subqueries the dropped column
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` — a "Capture rules contract" section at
  delivery, including the capture-before-triage ordering and the dropped column

## In scope / Out of scope

**In scope:** the two tables; the pure evaluator and its six criteria types; the
post-normalize driver with advisory lock, mode and horizon; external-key
derivation and `external_refs`-based dedup; task creation / log append through
existing executor tools; the two rule-management tools; the shadow report;
dropping `projects.client_person_id` with the triage and drafts reworks it
forces (§8, §9) and the drafts integration suite that covers them; the runbook.

**Out of scope — named because they are the tempting bundles:**

- **The `participants` fix across three sinks.** `google/sink.go:249`,
  `jira/sink.go:195` and `slackweb/sink.go:139,152` hardcode `'[]'`. That is
  real, related, and separate: it is identity resolution (people /
  person_identities), it changes normalization for 16,959 threads, and it needs
  its own re-normalization plan. The `person` criteria type is specified here so
  the engine is ready for it, and will simply match nothing until it lands.
- **The rest of step-8 delivery targeting.** §9 removes the draft worker's
  dependency on a person id and nothing else. It does NOT add channels
  (`jira_comment`, `slack_reply` stay out of the draft worker), does not touch
  `draft_delivery`, the policy matrix, or the send path, and does not change
  `drafts.DeliverTask`'s exported shape.
- **Polling avviato.atlassian.net.** Explicitly refused by Salvador. No
  `source_accounts` row, no `jira-auth add`, no JQL. The LHH signal arrives as
  Slack (`slack:T0360B84U:D01EJRX6P45`, 1,502 messages, current) and
  occasionally as mail from `jira@avviato.atlassian.net`. The three mail-sourced
  ones normalize poorly (HTML notification mail reducing to image URLs) — Slack
  is the reliable carrier and the rules must not depend on the mail path.
- **Triage going live** (step 6's create-path). This ticket defines triage's
  project source and its inbox; it does not add triage's `create_task` call, and
  the ordering constraint in §8 says capture must go live first regardless.
- **Any dashboard page for rules or decisions.** The existing `/tasks` and
  `/tasks/{id}` (which already renders `external_refs`,
  `internal/dashboard/board.go:276-278`) are the verification surface. A
  `/capture` page is future work.
- **Orchestrator rules on these tasks.** Nothing new fires; orchestratord is not
  even deployed.
- **`raw_source_items` writes.** This pass ingests nothing.

## Invariants that apply

1. **Raw-first.** This pass writes no raw items and creates nothing that did not
   come from a row already stored raw. It runs strictly AFTER `Normalize` in
   each connector main, reads only `normalized_messages` /
   `normalized_threads` / `source_accounts`, and every `capture_decisions` row
   carries `raw_source_item_id` alongside `message_id` so a decision is
   traceable back to the provider JSON and re-derivable after a
   re-normalization.
2. **One funnel.** Every actionable thing this engine produces is a row in the
   ONE `tasks` table, with `project_id` set from the rule. Neither new table is
   task-like: `capture_rules` is configuration (no status, no assignee, nothing
   is ever "worked"), `capture_decisions` is an append-only evaluation log (no
   status column, no claim, no lifecycle, no transition).

   **Q3 makes triage read `capture_decisions` looking for work, so the argument
   has to be explicit.** It does not become a queue, for three reasons a
   reviewer can check:
   - The queue is still `normalized_messages`. `capture_decisions` is a
     PREDICATE over it, exactly as `ai_extractions` already is in triage's
     existing filter (`internal/triage/store.go:35-38`, a `NOT EXISTS` over a
     non-task table). Nobody calls `ai_extractions` a queue, and this is the
     same shape with the polarity flipped.
   - A decision row is a FACT about a message ("the rules did/did not cover
     it"), not an intent to act. Nothing claims it, nothing transitions it,
     nothing closes it, and two workers reading the same row do not conflict —
     `FOR UPDATE SKIP LOCKED` appears nowhere near it.
   - The falsification test: delete every `capture_decisions` row and you lose
     the dedup guard and the evaluation history, and every message gets
     re-evaluated. You lose no WORK — because no work item ever lived there.
     Work items are `tasks` rows, exclusively.
3. **Everything through the executor.** The pass owns an `*executor.Executor`
   and reaches `tasks`, `external_refs` and `task_events` ONLY via
   `create_task`, `link_external_ref` and `task_append_log`. Direct SQL in
   `internal/capture` is limited to reads plus `INSERT INTO capture_decisions`
   (the engine's own log, the `ai_runs`/`ai_extractions` precedent). Rule
   mutations are themselves tool calls, so "who changed the routing and when" is
   answerable from `audit_events`. Criterion 16 pins this mechanically. No
   raw_sql surface is added; the two new tools take typed args.
4. **Nothing external without a delivery row.** This engine sends nothing and
   must never create or mutate a `deliveries` row. It is inbound-only, and its
   product is internal tasks. Criterion 7 asserts the `deliveries` count is
   unchanged. §9 touches the code that RESOLVES a delivery target, but not the
   gate: `draft_delivery` and the policy matrix are untouched, and removing the
   synthesized upwork target (site E) narrows what can be drafted, never widens
   it — in the direction the matrix already pointed ("existing threads only").
5. **Own-message loop closure.** The pending filter is `direction='inbound'`,
   the same guard triage uses (`internal/triage/store.go:34`), so a message
   switchboard itself sent — which re-enters through ingestion and normalizes
   outbound — can never be re-captured into a new task. Criterion 11 tests it
   directly. This is the single most important line in the pass: without it, a
   Jira comment switchboard posts that quotes `LHH-23637` would spawn a task
   about itself.
6. **Stealth attribution.** Nothing here is client-visible; task titles and
   bodies are internal. They are copied verbatim from stored provider data with
   `textmatch.NormalizedPrefix` truncation — no generated prose, no model, so
   there is nothing to attribute.
7. **Orchestrator purity (applied as engine purity).** `Evaluate` is a pure
   function of (message, rules), unit-testable with zero network and zero
   database — criterion 2 makes that structural by keeping it in a file with no
   `pgxpool` import. And "every decision writes an audit row" is honoured twice:
   `capture_decisions` for the routing decision (including `unmatched`), and
   `audit_events` + `policy_decisions` for each executor call it makes.

## Sibling patterns to copy

- **The pass itself:** `internal/capture/observe.go` — package doc, horizon env
  parsing (`ObserveHorizon`, lines 46-56), candidate struct, "printed
  unconditionally so a silent pass and a pass that did not run look different"
  logging (`cmd/connectors/jira/main.go:83-85`).
- **Executor from a connector + dedup-by-external_refs:**
  `internal/connector/github/store.go:98-154` (`PGTaskResolver`) and
  `cmd/connectors/github/main.go:61-67`. The `Dispatcher` interface + actor
  string shape is exactly what `EvaluateRules` should take, so it is testable
  with a fake dispatcher.
- **Advisory lock + single-instance:** `triage.AdvisoryLockKey`
  (`internal/triage/store.go:14`) and `PGStore.TryLock`, used from
  `cmd/triage/main.go:75-82`. Note the difference: triage ERRORS when the lock
  is held; this pass must NOT, because it is a hitchhiker on a connector run.
- **Shadow-mode discipline and reporting:** `internal/triage/triage.go:1-7`
  (package doc stating shadow is structural) and `triage.Report` /
  `cmd/triage/main.go:93-111`.
- **Queue-as-filter `NOT EXISTS` pending query:**
  `internal/triage/store.go:29-48` — and the model for the invariant-2 argument
  about `capture_decisions`.
- **Partial-unique `ON CONFLICT` with restated predicate:**
  `internal/capture/observe.go:261-267`.
- **Truncation:** `internal/textmatch` — one spelling only; do not re-implement
  in SQL (Postgres POSIX `\s` and Go `strings.Fields` disagree on NBSP).
- **Migrations:** `migrations/0009_slack_web_connector.sql` for the
  CHECK-constraint drop/add-in-one-transaction precedent.
- **Integration suite shape for §9:** `internal/tools/mail_integration_test.go`
  (executor + Postgres, `DATABASE_URL`-gated) and
  `internal/connector/upworkcrm/loopclosure_integration_test.go` (fixture
  cleanup in FK order, scoped by a test-owned key).

## Verification protocol

Before commit:

1. `go test ./...` — pure evaluator tests, tool validation tests, the policy
   `humanOnly` test, the MCP-not-listed test.
2. `make integration` — `db-up` + `migrate` + `go test -tags integration -p 1
   -count=1 ./...`. New integration suites must join the mutual-cleanup pact
   (INSTITUTIONAL_KNOWLEDGE, "integration suites cross-pollute"): clean your own
   fixtures first, in FK order, scoped by a test-owned slug — and note the FK
   order now includes `capture_decisions` → `tasks` and `capture_rules` →
   `projects`, so a test that deletes projects must delete rules first. The new
   drafts suite creates tasks and external_refs and must do the same.
3. Migration check against production BEFORE any image push:
   ```
   psql -h 192.168.50.49 -U ops -d ops -tAc "SELECT max(version) FROM schema_migrations"
   ls migrations/
   ```
   Merging 0015 is not applying it; the drift landmine (five migrations behind,
   bit 2026-07-31) came from exactly this gap. 0015 DROPS a column, so the
   ordering is stricter than usual: apply the migration and deploy the image
   carrying the reworked `internal/drafts` together. An old image against the new
   schema would fail on `SELECT ... p.client_person_id` — though only for
   `cmd/drafts`, which is not deployed, which is precisely why the drop is
   survivable today.

Manual smoke (workstation, against the real `ops` db — shadow only, creates
nothing):

4. Seed the fixture rules:
   `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/opsctl capture-rules add --project reengine --type body_regex --pattern 'LHH-[0-9]+' --external-system jira --url-template 'https://avviato.atlassian.net/browse/{key}' --priority 100`
   (×9, per the runbook), then `opsctl capture-rules list`.
5. `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/opsctl capture-rules run
   --since 2160h` — expect a JSON stats line and a nonzero `decisions` count.
6. `... capture-rules report --since 2160h` — expect, for the last 90 days:
   ReEngine ≈ 5 distinct LHH keys over ≈ 15 messages (the fact that grounds
   criterion 14), a large `collaboratory` attributed count from the `T0360B84U`
   catch-all, and a much smaller unmatched list than before that rule existed.
   Sanity-check the ambiguous list: it should contain LHH mentions inside
   Collaboratory threads and little else.
7. Confirm nothing was created:
   ```
   psql ... -tAc "SELECT count(*) FROM tasks"          -- unchanged
   psql ... -tAc "SELECT count(*) FROM external_refs"  -- still 0
   psql ... -tAc "SELECT mode, action, count(*) FROM capture_decisions GROUP BY 1,2"
   ```
8. Live smoke, deliberately narrow, only after the shadow diff has been read for
   a few days: `CAPTURE_RULES_MODE=live ... capture-rules run --since 168h
   --limit 20`, then `kubectl -n ops port-forward svc/dashboard 8085:80` and open
   `/tasks?project=reengine` and the task detail page to see the `external_refs`
   row and the appended logs. Re-run the same command and assert it reports 0
   (criterion 9) — that is the "usable alone" check.

## Decisions made unilaterally

Salvador's four answers settled the big questions. These are the judgment calls
made INSIDE those answers, where implementing the answer required choosing
something he was not asked about.

1. **§9 site D — the channel is derived from the thread_key PREFIX, not from
   `normalized_messages.channel`.** For upwork the `channel` column is
   CRM-supplied free text (`upworkcrm/normalize.go:106`), while the thread_key
   prefix is constructed by the connector to a documented format (fact 11).
   Matching on the column would work today and rot the first time the CRM
   changes a label, silently.
2. **§9 site D — the resolved thread now beats `send_from_account`.** Today
   `send_from_account` wins and then the thread lookup fails, which is why the
   only reachable production outcome is "gmail with no thread" (i.e. a delivery
   `draft_delivery` would reject). Putting the actual conversation first is what
   makes the rework meaningful; the old ordering is preserved as the fallback.
3. **§9 site E — the synthesized upwork target for a client with no thread is
   REMOVED rather than reconstructed.** The uuid is only reachable through a
   person once the column is gone, and drafting into a conversation that does not
   exist is already outside the policy matrix's "existing threads only, ≤2
   touches". Narrowing beats inventing a new lookup path. This is the one place
   where Q4's answer costs a real (if unused and out-of-policy) capability, so it
   is called out rather than buried.
4. **§8 — the go-live ordering constraint** (capture live before triage live) is
   imposed by this SPEC rather than left to whoever flips the flags. Q3's answer
   creates the fall-through-both window; nobody asked for it, and the cheapest
   place to close it is a documented ordering plus criterion 21.
5. **Fixture rules are seeded by runbook, not by a data migration.** Migrations
   are forward-only and unchecksummed; putting Salvador's routing table in one
   means every test database also gets production routing rules, which collides
   with the integration cross-pollution pact and makes a rule edit a new
   migration. Rules are configuration with an `enabled` flag and an audit trail
   via `capture_rule_add`; the runbook is their source of truth. Same shape as
   the existing "Client→project mapping recipe (manual, per client)".
6. **Keyless rules attribute but do not create tasks.** A rule with
   `external_system` NULL records the project and stops. With Q2 answered this is
   load-bearing: `T0360B84U` covers 59% of the corpus, and a task per thread
   there would manufacture exactly the backlog shadow mode exists to prevent.
7. **`--all` is shadow-only in live mode.** `task_append_log` has no dedup;
   allowing a live replay would double-append silently.
8. **The pass runs in ALL five connector mains, serialized by an advisory
   lock**, rather than in one designated connector. It is channel-agnostic, and
   binding it to one connector would mean Slack-sourced LHH tickets waiting on
   the Jira CronJob's schedule — and would break silently the day that connector
   is suspended (`connector-slackweb` already has been).

## Future work (not this ticket)

- `/capture` dashboard page: rules table, recent decisions, unmatched-volume
  leaderboard, one-click enable/disable through the two tools.
- Fill `normalized_threads.participants` in the google/jira/slackweb sinks; the
  `person` criteria type then becomes live with no code change here.
- Promote high-confidence unmatched clusters into proposed rules (the shadow
  report already computes the input).
- Let capture rules set `worker_type` / `autonomy` on created tasks, once an
  execution worker runs against Collaboratory.
- Split the Treetop workspace catch-all by conversation once the shadow report
  shows which channels inside `T0360B84U` are ReEngine-only — the priority-1
  rule is a floor, not a final answer.
- Extend the draft worker to `jira_comment` / `slack_reply` targets, now that
  §9's thread resolution is channel-agnostic and already resolves those threads.
