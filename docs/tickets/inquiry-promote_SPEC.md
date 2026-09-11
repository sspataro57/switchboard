> Jira: SWT-40

# inquiry-promote — deterministic attribution first, local-LLM routing after, real asks become tasks, all downstream of capture event-driven

**STATUS: FINAL — ready for `test-author`.** Both owner questions were answered 2026-09-11 and are
folded in as O4 (#a-millon → bulk) and O7 (Holding first). The record is in
`docs/tickets/inquiry-promote_OPEN_QUESTIONS.md`. Every remaining call is decided below, either by
the owner (dated) or unilaterally (with rationale).

**Five parts, each usable alone. Ship order: A → E → D → C → B.**

| part | what | code? | needs |
|---|---|---|---|
| **A** | deterministic attribution: project-name rules, #a-millon → bulk, re-pointing already-decided messages | no (rules are data) | — |
| **E** | the event-driven pipeline: MQTT wake-up contract, the `pipelined` consumer host, capture publishes `captured` | yes | MQTT broker (exists) |
| **D** | capture-time Jira assignee gate for gated projects (reengine) | yes, migration | E (its consumer runs in `pipelined`) |
| **C** | inquiry lane runs as a pipeline stage; real asks promote into tasks | yes, migration | E |
| **B** | local-LLM routing tier for what rules leave unmatched | yes, migration | E; shadow period |

**Recommended tracking:** B, D and E are each their own SWT issue. Each has its own go-live gate.
**Precondition ticket, not folded in:** deploying orchestratord (see "Dependencies").
**Follow-up ticket:** converting the existing downstream CronJobs (see "Future work").

## Source

Ad-hoc. Salvador, verbatim, 2026-09-11:

> "oks but those should go to qwen if there are questions or inquiries they should generate a
> task. qwen can quality to see if they are questions or inquiries no?"
>
> "also an email on salvador@handonconnect.org titled Activities Integration – Request and
> Response Validation is also a question for me and didn't create a task"
>
> "do you see the problem right? The email says Questions About Collaboratory Activities
> Integration it's telling the project right there"
>
> "I like the deterministic routing but after fails llvm routing should take turn"
>
> "basically that email salvador@handsonconnect.org is either collaboratory or reengine"
>
> "default to collaboratory is fine"
>
> "those are HOC/LLamasite not my projects. not even ReEngine" (on #a-millon, `C1C1TSLJH`)
>
> "the LHH tickets need a trip to jira to see if they are assigned to me. if they are then
> reengine if not ignore"
>
> "that is ok but I don't like all that being cronjobs everything if waiting cron jobs. All that
> dance should be a Q on mqtt"
>
> "the only thing on cron shoul be capture. after that should dance through orchestration and
> mqtt"
>
> "A is ok" (Q2: Holding for about two weeks, then Ready)

(The mailbox is `salvador@handsonconnect.org`, per `docs/runbooks/imap-mail-connector.md`.)

Coordinator's prod evidence (read-only, 2026-09-11):

- **Miss A** — byeluri Slack DM (normalized 158815), attributed to collaboratory by rule 8. There
  is no inquiry verdict because the lane is not scheduled. Filed by hand as task #110.
- **Miss B** — asunda45 DM, never ingested. That is bug `slackweb-collab-export-stale`, a dependency
  of this ticket.
- **Miss C** — a University of Rochester thread on the handsonconnect mailbox, 12 messages, all
  `unmatched`.
- **Inbound mail on the handsonconnect mailbox, by latest live decision**: collaboratory 487
  task_log / 29 task / 10 attributed; reengine 3 task / 1 task_log; **115 unmatched** (newest
  2026-09-10).
- **Messages naming `collaboratory`**: on that mailbox 152 task_log / **55 unmatched** /
  5 attributed. The 55 come from rochester.edu (20, all spellings), hostedscan.com 9,
  cecollaboratory.com 6, github.com 6, fullstory 4, handsonconnect.org 4, avviato.com 3,
  asu.edu 3.
- **Shadow inquiry quality** (16 of 50 flagged): the genuine asks were Katie ×2 and asunda45 ×3.
  The noise was FYIs, people addressing each other (`@esteban …`, from #a-millon,
  `T0360B84U:C1C1TSLJH`), and stale moments.
- **#a-millon (`C1C1TSLJH`)** carries HOC3/HOC4/LlamaSite deploys, Salesforce incidents and LHH
  ticket links. Its latest live decisions: collaboratory `attributed` via rule 9 (89), and
  reengine `task` via rule 1 (4).
- **Reengine's LHH/LHHSF tasks.** All 7 (65, 72, 75, 76, 90, 93, 94) had
  `assigned_to_self=false`. Capture created each one (rule 1 or 2), and the reconciler closed them
  about 24h later (`not_assigned` or `ticket_done`, acted 2026-09-10 14:22).
  - `source_accounts` 8309 is `jira_lookup` for `https://avviato.atlassian.net`, scopes
    `{LHH,LHHSF}`, created 2026-09-10.
  - `projects.ticket_assignee_gate` is true for reengine and false for collaboratory.
- **Every pipeline stage today is a CronJob:**
  - connectors: upworkcrm 5m, google 10m, jira 15m, slackweb 30m, gcal twice a day;
  - classify-personal 30m, classify-residue daily, classify-promote hourly at :20.

  A Slack ask can wait about 30+30+60 minutes. MQTT carries only the fleet contract.
- **orchestratord is NOT running in prod, and has not been since the step-05 smoke.**
  - `orchestrator_cursor.last_event_id = 75`, updated 2026-07-12 01:01Z. The last
    `actor='orchestrator'` audit row is from the same minute (17 rows ever).
  - 866 `task_events` sit past the cursor, dated 2026-09-07..11: log 794, status_changed 69,
    delivery_sent 1, delivery_confirmed 1, priority_changed 1.
  - There is no Deployment for it in `kube/switchboard`, and fleetd and hooksd are not deployed
    either.
  - No R-rule has run in prod: 3 tasks sit in `done_locally` and 9 in `blocked`.
- **`task_dismissals` on prod**: not_actionable 18, handled_elsewhere 3, wrong_kind 2
  (2026-09-09..11). Nothing trains on them today.

## Owner decisions (answered, not open)

- **O1 (2026-09-11) — tier order.** Deterministic capture rules decide first, unchanged. A
  local-LLM routing step acts only on what they leave `unmatched`.
- **O2 (2026-09-11) — the handsonconnect mailbox belongs to one of two projects.** Mail arriving on
  `salvador@handsonconnect.org` is collaboratory or reengine. The LLM tier chooses only between
  those two for that mailbox, never from the open project list.
- **O3 (2026-09-11) — default to collaboratory.** When the LLM tier does not confidently pick
  reengine for mail on that mailbox, the message routes to collaboratory ("default to
  collaboratory is fine"). "Confidently" is defined deterministically in B-D4, because a
  self-reported confidence cannot be used.
- **O4 (2026-09-11, was Q1) — #a-millon is nobody's project.** It goes to `bulk`, ignored. The one
  exception is an LHH ticket link there, which still reaches rule 1 and its Jira check (O5):
  "he cares whether the TICKET is his, not which channel mentioned it."
- **O5 (2026-09-11) — LHH tickets are checked against Jira BEFORE a task exists.** Assigned to him
  → reengine task (create or append), as today. Not assigned → no task and no task_log.
- **O6 (2026-09-11) — only capture stays on cron.** Everything downstream is event-driven: MQTT
  wake-ups over Postgres queues of record, with the orchestrator deciding at the task boundary.
- **O7 (2026-09-11, was Q2) — promoted question-tasks start in Holding.** "A is ok."
  - `inquiryCreateStatus = "holding"`: promoted inquiry tasks land in the Holding column
    (`classify_promotions.action='review'`), where Salvador works or dismisses them.
  - **The signal is `classify promote --outcomes`**, the dismissal-based readout (C-D12).
  - After about two weeks, when that readout satisfies him, **one line flips new ones to `ready`**:
    `inquiryCreateStatus = "ready"` (action `task`), a one-line diff plus its test. Tasks already
    created stay where they are.
  - It is not a column or an env var, for SWT-30 D2's reason: the autonomy argument lives in code,
    where a typo cannot widen it unreviewed.

## What exists today (verified in code, 2026-09-11)

- **A new rule never re-decides a live-decided message.** `capture.pendingMessages` excludes every
  message that already carries a row of the pass's mode (`rules_store.go:486-489`). The partial
  index `capture_decisions_live_uniq (message_id) WHERE mode='live'` allows one live row per
  message, forever. `--all` is refused in live mode (`RulesConfig.normalize`).
  `--live --since 8760h` reaches only messages that NEVER got a live decision. **So the 55/115 live
  `unmatched` mails cannot be picked up by the live engine, whatever rule is added.**
- **Every "latest decision" reader ignores `mode`.** They all read
  `ORDER BY cd.id DESC LIMIT 1`, about 15 sites: `classify/store.go` (every inbox and loader),
  `promote/store.go:236`, `triage/store.go:66,184,239`, `drafts/store.go:287,299`,
  `capture/attribution.go:55` and `capture/rulesreport.go:38`.
  - Consequence one: a shadow `--all` pass writes newer rows that these readers follow. That is the
    documented "run after changing rules" step, and it is how a new attribution-only rule
    re-points already-decided messages today.
  - Consequence two: an unguarded shadow pass would overwrite any attribution the rules did not
    produce. Parts B and D handle this.
- `capture_decisions` CHECKs: `mode IN ('shadow','live')`,
  `action IN ('unmatched','attributed','task','task_log')`, and
  `(action='unmatched') = (project_id IS NULL)` (0015).
- **Criteria types:** `body_regex, sender, thread_key_prefix, thread_key_contains,
  source_slack_workspace, person`.
  - `body_regex` matches `Subject + "\n" + BodyText` (`matchText`).
  - `sender` is a case-insensitive substring of the raw From.
  - A rule with no `external_system` is attribution only.
  - **`thread_key_prefix` is a case-SENSITIVE `strings.HasPrefix`.**
- Slack thread keys are `slack:{ws}:{conv}` for an unthreaded message and
  `slack:{ws}:{conv}:{root}` for a threaded one (`slackweb/normalize.go`: `channelThreadKey` plus
  the `ThreadRootID` append). The workspace id keeps its exported case (production keys like
  `slack:T0360B84U:D01EJRX6P45`).
- **Rule priorities** per the capture-rules SPEC fixture table (confirm with `opsctl
  capture-rules list`, §V4f):

  | rule | priority |
  |---|---|
  | rules 1-2 (LHH `body_regex` and `jira@avviato.atlassian.net` sender → reengine, task-creating, `external_system=jira`) | 100 |
  | rule 59 (Foundry mail → foundry) | 95 |
  | rule 10 (Treetop keys → collaboratory) | 90 (about 40 once `capture-rule-ticket-keys` lands) |
  | the treetop jira thread-key rules | 50 |
  | rule 8 (`T0HPR78RX`) | 10 |
  | the bulk sender rules | 5 |
  | rule 9 (`T0360B84U` catch-all) | 1 |
- **The Jira assignee check exists, after the fact only.** It lives in `internal/ticketstatus`,
  inside connector-jira every 15 min:
  - `RouteLookup` routes a key to the `jira_lookup` account whose scopes claim its prefix, fails
    closed, and refuses an ambiguous route.
  - `jira.LookupIssues` GETs the issue and stores it raw-first under the shared `IssueRawID`, the
    same row the poller uses. It records `own_account_id` from `/myself` and writes a `sync_runs`
    row. A per-key failure leaves no snapshot.
  - `loadSnapshots` re-reads the stored raw. Freshness is `DefaultLookupTTL = 1h`
    (`TICKET_LOOKUP_TTL`).
  - `Decide` is pure; `warranted = category≠done ∧ ¬delivered ∧ (¬gate ∨ assignee=own)`, and
    evidence gaps are `unreadable`, never a verdict.
  - It only ever sees tasks that already exist, through their `external_refs`.
- **The orchestrator:**
  - It drains `task_events` past a cursor. NOTIFY is a wake-up only and the drain is the sole
    delivery path.
  - It evaluates pure rules and applies them through the executor or `Publisher.PublishCommand`
    (fleet `cmd` topics).
  - **`task_events.task_id` is NOT NULL** (0001), so a message that has no task cannot produce an
    orchestrator event.
  - `create_task` writes no creation event (IK SWT-38).
- **The MQTT contract** (`internal/fleet/contract.go`):
  - `ops/workers/{id}/status` is retained QoS 1 with a 60 s heartbeat and an LWT of
    `{"state":"dead"}`;
  - `ops/workers/{id}/cmd` is not retained;
  - publishing is a strict `Marshal` and consuming a lenient `Parse`;
  - spine clients use `fleet.NewSpineClient(ctx, broker, distinctID)`, and the landmine is that a
    duplicate client id kicks the other connection off the broker.
- **Classify:** three lanes and two contracts, local qwen on the z4 (4.5 s/verdict inquiry
  median), the shared lock `0x5157_0022`, and no hosted client in `cmd/classify`.
  - `internal/classify` may not contain `raw_source_items`.
  - `confidence` is banned by a structure scan and measured constant for qwen.
  - `classify.EvalResultThreshold = 120` and `EvalIndicativeMarker` are the one spelling of "no
    ratio below 120".
- **Promote** is barred from reaching `internal/provider` transitively (so it cannot import
  classify). `task_get_next` routes only `assignee_type='claude'`. The drafts worker does not draft
  `slack_reply`. The image is distroless.
- `task_dismissals.reason_code ∈ {not_actionable, wrong_kind, duplicate, handled_elsewhere}`.
  Since 0026 a task may carry several rows (SWT-36 D6).
- Slack DMs: keys `slack:{ws}:D…` (the leaf's own fallback, `slackconnector
  src/slack/channels.ts:20`). Mentions reach `body_text` as display names, never `<@U…>`. The
  conversation `type` exists only in `raw_json`.

---

## Part A — deterministic attribution, and re-pointing what is already decided

**Usable alone:** after A, a message whose subject names Collaboratory (or ReEngine) is attributed
to that project by a rule. The 55 already-`unmatched` Collaboratory-named mails point at
collaboratory for every reader, and #a-millon points at `bulk` except its LHH links, which keep
going to rule 1. There is no code change and no model. Until D ships, an LHH link still creates a
reengine task that the reconciler closes later, which is today's behaviour.

**A-D1 — Name rules match the SUBJECT LINE only**, with the pattern `(?i)\A[^\n]*\b<alias>\b`.
`\A[^\n]*` pins the match to the first line of `matchText`, which is the subject (for Slack, the
channel name).
- Salvador's example is a subject ("it's telling the project right there").
- A body mention is the "Avviato mail that mentions Collaboratory in passing" case, and those are
  left to Part B.

**A-D2 — Priority 3.** It sits below every source-specific rule: #a-millon (99), LHH (100),
rule 59 (95), tickets (90/50/about 40), workspaces (10), bulk senders (5). It sits above the
catch-all (1). A stronger signal always wins, and marketing mail naming the project stays in bulk.

**A-D3 — Aliases are per-project DATA through `capture_rule_add`, never generated from
`projects.name`** (`personal`, `foundry`, `bulk`, `homelab` are ordinary words).
- An alias is admitted only after the Go-regexp export (§V4h) shows its out-of-project
  subject-line matches are zero or explained.
- **Seeded:** collaboratory `(?:ce)?collaboratory` (`\b` does not fire inside
  "cecollaboratory") and reengine `re-?engine`.
- Other projects are added only when a miss shows their name.

**A-D4 — `cecollaboratory.com` sender rule** (priority 3, collaboratory, attribution only). It is
added only after `opsctl capture-rules report --domain cecollaboratory.com` has been read; that
read is the runbook's Phase-0 blocker.

**A-D5 — #a-millon → `bulk` (O4), BELOW rules 1-2 and ABOVE everything else that can match Slack.**
- **The rule:** `thread_key_prefix` `slack:T0360B84U:C1C1TSLJH` → `bulk`, attribution only,
  **priority 99**.
- **Why below 100:** an LHH link in a-millon must still reach rule 1 and, once D ships, its Jira
  check (O4/O5). The ticket decides, not the channel.
- **Why 99 and not merely above 1:** it must also outrank rule 10 (Treetop keys, 90) and rule 59
  (95). Otherwise a WEB/API/OPS mention in a HOC channel would become a collaboratory task, and O4
  says everything but LHH there is ignored.
  - Rule 59 and rule 2 match mail, never a slack key, so outranking them changes nothing for
    their traffic.
  - The capture-rule-ticket-keys replacement mention rule (about 40) stays below 99.
- **Spelled in exact case:** `thread_key_prefix` is case-sensitive and the stored key keeps
  `T0360B84U`. One prefix covers the unthreaded key and every `…:C1C1TSLJH:{root}` thread key.
  §V4e checks that no longer conversation id shares the prefix.
- Tasks 72/75/93/94 stay closed with their `external_refs`.
  - Future LHH links in a-millon are judged by D.
  - A non-LHH a-millon message is `attributed`→bulk, never `task_log`, so capture's SWT-36 reopen
    never fires for it.

**A-D6 — Re-pointing already-decided messages uses the existing shadow `--all` pass**, once, after
the rules land: `opsctl capture-rules run --since 720h --all`. It is the documented
post-rule-change step, and the latest-decision readers already follow it.
- It writes a newer shadow row per inbound message in the window. A shadow row cannot create or
  log a task, so it re-points attribution only. That works for attribution-only rules, NOT for
  task-creating ones.
- **After D ships**, the same pass writes a shadow `held` for gated matches: a shadow row, never
  resolved (D resolves `mode='live'` holds only).

**Acceptance criteria — Part A**
- A1. Before any rule is added, the candidate patterns run through Go `regexp` over an export of
  subject+body+account+current attribution (§V4h). Postgres reads `\b` as a backspace, hence Go.
  The runbook records matches per alias. Required: the Rochester "Questions About Collaboratory …"
  messages match, and an alias with unexplained out-of-project matches is not added.
- A2. The rules are added with `opsctl capture-rules add` only (executor, humanOnly, audited),
  without `--external-system`. `opsctl capture-rules list` shows the name rules at 3 and #a-millon
  at 99, with notes naming this ticket.
- A3. After the §A-D6 pass:
  - the collaboratory-named handsonconnect mails have a latest `attributed` → collaboratory
    decision;
  - every `slack:T0360B84U:C1C1TSLJH…` message has a latest `attributed` → bulk decision, EXCEPT
    messages matching `LHH-[0-9]+`, which rule 1 still decides. The shadow report shows them
    `ambiguous=true`, both rules matched, rule 1 winning;
  - tasks 72/75/93/94 are unchanged.

  Before/after counts go into `docs/runbooks/capture-rules.md`.
- A4. `docs/runbooks/capture-rules.md` gains three sections: "Project-name rules", "Re-pointing
  already-decided messages", and "#a-millon → bulk" (O4, why 99, LHH still reaching rule 1, the
  case-sensitive prefix).

---

## Part E — the event-driven pipeline (only capture stays on cron)

**Usable alone:** after E, every connector's capture pass publishes a `captured` wake-up, and a
long-running `pipelined` Deployment consumes it. Its stages are D, C and B as they land, each on
its own queue of record in Postgres, each with a sweep fallback and a heartbeat. No new CronJob
exists. With E alone and no stage enabled, `pipelined` heartbeats and logs wake-ups, and that is
the smoke.

**E-D1 — Postgres stays the queue of record; MQTT is a wake-up only.** This is the orchestrator's
own NOTIFY discipline (`engine.go`), applied between stages.
- Every stage's work is a SQL inbox that already exists or is added by its part: held decisions,
  unmatched on armed accounts, classify inboxes, promotion inboxes.
- A payload never carries work. A lost MQTT message costs latency up to one sweep, never work; a
  duplicate costs one empty inbox query.
- Mosquitto has no ack or redelivery work-queue semantics, which is why this matters (CLAUDE.md:
  Postgres is the queue of record).

**E-D2 — Message-level boundaries are DIRECT MQTT publishes. The task-level boundary is the
orchestrator's.** The argument against routing message-level stages through the orchestrator:
- **`task_events.task_id` is NOT NULL.** A captured, routed or classified message has no task, so it
  cannot be an orchestrator event. Inventing placeholder tasks breaks invariant 2. A second,
  message-level event table plus a second LISTEN loop in orchestratord would be building a second
  orchestrator.
- **There is nothing to decide per event.** The next stage after `captured` is fixed by the stage
  graph, not by the event's content: each stage's own inbox filter selects its rows. The graph is
  still a pure function, `pipeline.Subscribers(event) []Stage`, a static table unit-tested in
  `internal/pipeline`, in the orchestrator's `rules.go` discipline (invariant 7). It is just not
  worth a round-trip through `task_events`.
- **orchestratord is not running.** Coupling these stages to it would make all of SWT-40 dead
  until that precondition lands. The message-level half works without it.
- **Where the orchestrator IS the next step:** the moment a stage creates or changes a TASK
  (promotion, the Jira gate's task creation, SWT-36 reopens), the executor writes `task_events`
  and the orchestrator's existing R-rules own that task's lifecycle (R3 Deliver task, R4/R5
  dependencies, R8 delivery).
  - That half needs orchestratord, as every task in the system does.
  - The pure-rule decider for anything task-shaped stays the orchestrator. No SWT-40 stage decides
    task lifecycle.

**E-D3 — The contract, `internal/pipeline/contract.go`**, modelled on `internal/fleet/contract.go`
(topic builders, a strict `Marshal`, a lenient `Parse`, vocabulary constants).
- **Topic:** `ops/pipeline/{event}`. **QoS 1, NOT retained.** A retained wake-up would re-fire on
  every reconnect, and retained state is global on the production broker (IK).
- **Payload:** `{"event": "captured", "source": "slackweb", "counts": {"unmatched": 1, "attributed":
  3, "held": 0}, "max_id": 123456, "ts": "…"}`. `counts` and `max_id` are diagnostic only.
  Consumers re-query their inbox and never trust ids (the upworkcrm "never branch on a stats
  payload" landmine).
- **Event vocabulary:**

  | event | published by | consumed by (SWT-40) |
  |---|---|---|
  | `captured` | every connector main, after its capture pass commits ≥1 decision | D gate (held), B route classify (unmatched on armed accounts), C inquiry classify (attributed on `ai_inquiry` projects) |
  | `gated` | D, after resolving ≥1 held decision | C inquiry classify (a gate outcome can attribute) |
  | `route_classified` | B's route lane, after writing ≥1 verdict | B route apply |
  | `routed` | B route apply, after writing ≥1 route row | C inquiry classify |
  | `inquiry_classified` | C's inquiry lane, after writing ≥1 verdict | C inquiry promote |
  | `promoted` | C promote, after ≥1 task created or attached | none: the task boundary is the orchestrator's (E-D2); published for observability and the follow-up |

  The follow-up conversion ticket subscribes classify-personal and classify-residue to `captured`
  and personal promotion to a new `personal_classified`, reusing this contract unchanged.
- **A publish never fails its stage.** It happens after the commit; a failure is logged and the
  sweep covers it. Connector mains need `MQTT_BROKER` in their env. They use a spine client with a
  distinct id per connector (`switchboard-capture-{connector}`), connected only for the publish.
  With the env unset, the publish is skipped with one log line (fail-open for LATENCY only, never
  for correctness).

**E-D4 — `cmd/pipelined`: one binary, one Deployment, one goroutine per enabled stage.**
- Each stage has its own MQTT spine client (id `switchboard-pipeline-{stage}`) so that it owns its
  own LWT.
- **Loop per stage:** subscribe to its upstream events → a coalescing wake channel (buffer 1, so a
  burst is one pending run) → run passes until the inbox drains (a pass is `--limit`-bounded, and
  the stage repeats while a pass processes its limit) → wait.
- **The sweep fallback:** a timer fires the same pass every `PipelineSweep = 5m`. Empty-inbox
  queries are cheap. Five minutes also bounds how late C's 1h grace can release.
- **Stages are enabled by config** (`PIPELINE_STAGES=gate,inquiry,route`), so each part goes live
  by adding its name, and removing it disables it.
- **Health:** each stage publishes the fleet heartbeat on
  `ops/workers/pipeline.{stage}/status` (retained, QoS 1, every `fleet.HeartbeatInterval`) with
  state `idle` or `working`, and an LWT of `dead`. The dotted id makes fleetd mirror them as client
  `pipeline` once fleetd is deployed. Until then `mosquitto_sub -t 'ops/workers/pipeline.+/status'`
  is the view. A stage that errors logs, stays up and retries on the next wake or sweep. It never
  crash-loops on a DB error.
- **GPU serialization:** both local-model stages (C's inquiry lane, B's route lane) take the
  shared `classify.AdvisoryLockKey` (`0x5157_0022`) per pass, exactly as `classify run` does. That
  serializes them with each other and with the classify-personal and classify-residue CronJobs
  that remain until the follow-up. A lost lock is not an error here: the stage retries after 30 s,
  then on the next sweep. ollama on the z4 answers one model at a time; this is the right
  behaviour, not a bottleneck to engineer around.
- **Non-GPU locks:** D's gate and B's route apply write `capture_decisions`, so they take
  capture's `RulesAdvisoryLockKey` (`0x5157_0015`). That serializes them with connector capture
  passes, closing the race where a shadow pass reads a message just before a route or gate row
  lands and buries it. C's promotion takes `0x5157_0021`, as today.

**E-D5 — The CronJob table after SWT-40.** No new CronJob. The classify-inquiry and classify-route
CronJobs of an earlier draft are withdrawn.

| workload | kind | change |
|---|---|---|
| connector-* | CronJob (stays) | image bump + `MQTT_BROKER` env |
| classify-personal, classify-residue | CronJob (stays until the follow-up) | image bump |
| classify-promote | CronJob (stays until the follow-up) | image bump. Its command pins `--lane personal`, so the inquiry lane is never run by cron |
| **pipelined** | **Deployment (new)**, replicas 1, strategy `Recreate` | env `DATABASE_URL`, `MQTT_BROKER`, `OPS_LOCAL_PROVIDER_URL`/`OPS_LOCAL_MODEL` (the IP literal), `OPS_TOKEN_KEY` (D's Jira lookup credential), `PIPELINE_STAGES` |

The manifests belong to the kube session; `docs/runbooks/HANDOFF-kube-inquiry-promote.md` lists
exactly the rows above.

**Acceptance criteria — Part E**
- E1. `internal/pipeline` defines the topic builder, the event vocabulary constants, `Wake`
  (strict `Marshal` refusing unknown events, lenient `Parse`) and `Subscribers(event) []Stage`.
  - Pure table tests cover every row of E-D3.
  - A structure test asserts no `retain=true` publish of an `ops/pipeline/` topic anywhere in the
    repo.
- E2. Connector mains publish `captured` after their capture pass iff it committed ≥1 decision.
  - An integration test on the local compose broker (`MQTT_BROKER=tcp://localhost:1884`, IK)
    covers: one message on commit, none on an empty pass, and a broker-down pass that still exits
    0 with the decision rows intact.
  - Mutation: publish before the commit → the test that reads the inbox on wake goes red.
- E3. `pipelined` stage-loop tests:
  - one wake runs one pass;
  - ten wakes during a pass coalesce to one follow-up pass;
  - with no wake, the sweep runs the pass;
  - a lost advisory lock retries without error.

  The stage functions are injected, so the loop is tested without a DB.
- E4. Heartbeats: per stage, retained status every 60 s, `working` during a pass, and an LWT of
  `dead` on kill. The test clears its retained topics afterwards (IK landmine).
- E5. `classify promote` gains `--lane personal|inquiry` and defaults to `personal`, so the
  CronJob's behaviour is byte-identical. `pipelined` calls the inquiry lane's function directly.
- E6. The runbook gains a `docs/runbooks/pipeline.md` covering the topic table, the "wake-up,
  never work" rule, how to watch it with mosquitto_sub, the sweep interval, how to enable a stage,
  and what a dead heartbeat means. An IK entry covers the contract and the E-D2 reasoning (why not
  `task_events`).

---

## Part D — capture-time Jira assignee gate (reengine's LHH tickets)

**Usable alone:** after D, a message whose winning rule would create or log a Jira-keyed task in
a project with `ticket_assignee_gate` produces no task at capture time. The ticket is looked up
first, within minutes: assigned to him → task (create or append), exactly as today; not his →
attribution only, no task, no log. Jira unreachable → no task, retried on the next sweep. The
after-the-fact reconciler stays as the backstop.

**D-D1 — Capture does not call Jira. It records a new action, `held`, and a pipeline stage
resolves it.**
- Capture runs inside EVERY connector main. An LHH link arrives through slackweb and google
  mains, as well as jira's, and the lookup credential (`OPS_TOKEN_KEY` + the stored token) exists
  only in connector-jira today. A capture-time HTTP call would spread that secret to every
  connector, or silently skip the check where it is absent.
- **The generic trigger:** the winning rule has `external_system='jira'`, a key was derived, and
  the rule's project has `ticket_assignee_gate`. Then capture's action is `held` instead of
  `task`/`task_log`, with project, external_system and key recorded and no task. The gate column
  is loaded with the rules (`loadRules` joins projects already). Only reengine turns it on;
  collaboratory's rule 10 and rules 3-5 are gate-off and unchanged.
- `held` names a project, so the `(action='unmatched') = (project_id IS NULL)` CHECK holds. Every
  latest-decision reader filters `attributed` or `unmatched` positively, so a held message is
  invisible to the classify, triage and promote inboxes. That is correct: its fate is pending.
- Shadow mode writes `held` too (it decides everything, acts on nothing). Only `mode='live'` holds
  are resolved.

**D-D2 — The resolution is a second `capture_decisions` row, `mode='gate'`**, the same argument
as B-D5.
- The live claim is spent by the `held` row, and `held` acted on nothing, so the gate row is the
  message's one ACTION. "One live action per message" still holds. The live index is untouched,
  so this is not a breaking migration.
- Partial unique `(message_id) WHERE mode='gate'` gives one resolution per message, forever. The
  row is inserted BEFORE the executor calls (claim-before-act) and completed with `task_id`
  (`recordDecisionTask`'s shape).
- **Its action:** `task` | `task_log` (assigned, ticket warranted) or `attributed` (not his, done,
  delivered or expired). The schema pins `mode='gate'` ⇒ `action IN ('task','task_log',
  'attributed')`, with `matched_rule_id`, `external_system` and `external_key` NOT NULL.
- **Shadow-overwrite guard:** capture's `pendingMessages` excludes messages carrying a `gate` or
  `route` row in every mode.

**D-D3 — ONE spelling of "is this ticket warranted", shared with the reconciler.**
- `ticketstatus.Decide`'s predicate is extracted into a pure exported `Warranted(obs Observation)
  (warranted bool, dropReason string, readable bool)`. `Decide` calls it, and its decisions stay
  byte-identical (its test suite is unchanged).
- The snapshot half of `ticketstatus.Run` is extracted into an exported
  `EnsureSnapshots(ctx, pool, keys, cfg) (map[key]snapshot, Stats)`. It covers routing,
  TTL-freshness, `LookupIssues`, then re-reading the stored raw. Both the reconciler and the gate
  call it.
- The gate's decision is `capture.DecideGate(obs, readable, existingRef) GateDecision`, pure:

  | observation | outcome |
  |---|---|
  | unreadable (no snapshot, unrouted, ambiguous route, no own id) | **stay held**, counted `pending_lookup` |
  | warranted, ref exists | `task_log` on the ref's task (plus SWT-36's guarded reopen if dismissed, as capture does today) |
  | warranted, no ref | `task` (create + link + provenance, capture's exact helpers) |
  | not warranted | `attributed`, reason names the drop (`not_assigned` / `ticket_done` / `ticket_delivered`) |
  | still unreadable after `GateMaxAge` | `attributed`, reason `gate_unverified_expired`: **fail closed, no task** |
- The gate uses the SAME predicate as the reconciler, done and delivered included, so it never
  creates a task the reconciler would close 15 minutes later.

**D-D4 — Where it lives: `internal/capture/gate.go`**, as the `gate` stage in `pipelined`.
- capture owns `capture_decisions` and already owns the task helpers (`createRuleTask`,
  `linkRuleRef`, `setRuleProvenance`, `appendRuleLog`, `reopenRuleTask`), which it calls as actor
  `capture:gate` through the executor (invariant 3).
- It imports `internal/ticketstatus` for `EnsureSnapshots` and `Warranted`, never the jira HTTP
  client directly. The pure `rules.go` stays I/O-free, and its structure test is unchanged.
- `pipelined` holds the lookup `ClientFactory`, the same token-decrypting closure
  `cmd/connectors/jira` builds (moved to a shared helper). That is the only place the gate touches
  a credential.
- **Inbox:** live `held` rows with no `gate` row, whose message is ≤ `GateMaxAge` old, oldest
  first, `--limit`-bounded.
- **Lock:** capture's `0x5157_0015` (E-D4). Each held message's ref is re-queried under the lock,
  so two held mentions of one new ticket create ONE task, and the second logs onto it
  (`external_refs` unique is the backstop).

**D-D5 — Caching and rate.**
- **The cache IS the stored raw snapshot**, the reconciler's own, keyed by `IssueRawID`. Its
  freshness is `ticketstatus.LookupTTL()` (1h). A ticket mentioned 50 times in an hour costs ONE
  GET, and the gate and the reconciler share every fetch.
- **Per pass:** at most `GateMaxKeysPerPass = 50` distinct keys fetched. The rest stay held for the
  next pass or sweep. One `/myself` per account per pass (`LookupIssues` already does that).
- **Retry rate = the sweep (5 min)** for keys whose fetch failed. Nothing retries faster, and there
  are no retries within a pass: `LookupIssues` counts a per-key failure and moves on.
- **`GateMaxAge = 72h`.** After that a still-unreadable hold resolves to `attributed`
  (`gate_unverified_expired`) and stops costing GETs. That is visible in the report, and it is the
  fail-closed direction the owner asked for.

**D-D6 — Later assignment.**
- A resolution is final for its message. A ticket assigned to him later gets its task from the
  **next mention**. The one that always exists is the Jira notification mail for the assignment
  itself: `jira@avviato.atlassian.net`, rule 2, which holds and then resolves `task`.
- The reconciler cannot create tasks (it only sees existing `external_refs`) and stays exactly
  what it is: the after-the-fact **backstop**. It closes a gate-created task when the ticket is
  reassigned away or finished (`not_assigned`/`ticket_done`), and reopens it per SWT-32 when the
  ticket comes back. The gate does not replace it.

**Acceptance criteria — Part D**
- D1. `decideMessage` returns `held` for a jira-system match with a key on a gated project, for
  both the would-be `task` and would-be `task_log`.
  - Gate-off projects are byte-identical; the collaboratory rule 10 fixture is the control.
  - An unkeyed match stays `attributed` (today's behaviour).
  - Mutation: read the gate from a constant instead of `projects.ticket_assignee_gate` → the
    integration test seeded through the column goes red. That is the SWT-21 "test the column" rule.
- D2. `ticketstatus.Warranted` is extracted, and `Decide`'s existing table tests pass unmodified.
  `EnsureSnapshots` is extracted, and the reconciler's integration suite passes unmodified.
- D3. `capture.DecideGate` is pure, with a table test per D-D3 row, including an expired
  unreadable hold, a done ticket and a delivered-status ticket.
- D4. Gate integration test on a fake Jira server, which is the jira package's existing test
  pattern:
  - an assigned key → one task, a ref and provenance, actor `capture:gate`, audit rows;
  - two held mentions of one new key → one task and one log;
  - an unassigned key → `attributed` and no task;
  - the server down → still held, a `pending_lookup` counter, no row;
  - `GateMaxAge` passed → `attributed` with `gate_unverified_expired`;
  - an existing ref on a dismissed task → a log plus a guarded reopen;
  - run-twice → nothing new.
- D5. Rate: 60 distinct held keys → ≤50 GETs in one pass; a key fetched within the TTL is not
  re-fetched by the gate or by the next reconciler pass (a shared-snapshot integration test).
- D6. The existing direct-write scan still holds (`internal/capture` writes only its own log).
  `rulesreport` renders `held`, gate resolutions and `pending_lookup` on their own lines.
- D7. The runbook gains "Capture-time assignee gate" in `docs/runbooks/ticket-status-sync.md`:
  the trigger, `held`, the four outcomes, TTL, rate, expiry, later assignment and the backstop. An
  IK entry covers `held`/`gate` and why capture never calls Jira.

---

## Part B — the local-LLM routing tier (after rules fail)

**Usable alone:** after B is armed for the handsonconnect mailbox, mail there that the rules left
`unmatched` is attributed to collaboratory or reengine within minutes of capture. The step is
recorded as a typed decision row (thread / single / model / default). Before arming it runs in
shadow: verdicts only, visible in `classify report --lane route`.

**B-D1 — Closed candidate sets only**, configured per source account in a new config table,
`source_account_projects` (source_account_id, project_id, is_default, description).
- It is written only through new humanOnly executor tools. A Slack workspace is a source account
  too, so per-workspace comes free.
- **Only accounts with candidate rows are routed at all.** The human-written row is the
  AUTHORISATION to move a message into a project whose `ai_locality` may be wider than its origin
  (IK SWT-21). Open-set routing is Future work: it would put about 14k residue messages in front of
  the GPU with nothing to measure against.
- Seed: handsonconnect → {collaboratory (default), reengine} (O2, O3).

**B-D2 — Four steps, the first two deterministic.** For each inbound message on an armed account
whose live decision is `unmatched` and whose latest decision is still `unmatched`:
1. **thread**: other messages on its thread carry latest decisions attributing to exactly ONE
   candidate → that project.
2. **single**: the account has exactly one candidate → it.
3. **model**: a stored `classify_route` verdict chose a candidate and passed grounding → that
   project.
4. **default**: no confident choice, and the account has a default → the default (O3). With no
   default → stay `unmatched`.

No verdict yet → `pending_verdict`: a missing verdict never falls to the default. Steps 1-2 cost
no GPU; they are why the latest Rochester message follows its thread once Part A has attributed the
rest.

**B-D3 — A fourth classify lane, `route` (`worker_type='classify_route'`), with its own contract,
kept separate from residue and inquiry.**
- Residue's contract is pinned equal to personal's (SWT-23), so folding routing into it breaks that
  guard.
- The inquiry lane needs the project context an unmatched message lacks.
- The lane-count and contract-count guards in `lane_test.go` are REWRITTEN to "four lanes, three
  contracts", never deleted.

**B-D4 — Output contract, no confidence:** `{project_index: integer|null, evidence: string,
reason: string}`.
- `project_index` is 1-based into the numbered candidate list (slug, name, client, the row's
  `description`), resolved in Go the way `ResolveLink` resolves a link.
- **The grounding gate replaces confidence:** a choice counts only if `evidence` is a
  whitespace-collapsed, case-folded substring of the sender, subject or body. Otherwise → step 4.
- For the handsonconnect mailbox, reengine is chosen only on quoted evidence, and everything else
  lands on collaboratory. That is O3, made deterministic.

**B-D5 — The applied decision is a `capture_decisions` row with `mode='route'`.**
- Why not `live`: a second live row per message is impossible under
  `capture_decisions_live_uniq`, and changing that partial index breaks every deployed binary's
  `ON CONFLICT … WHERE mode='live'` (the SWT-36 landmine).
- Why not a side table: every latest-decision reader follows this table already.
- The schema pins `mode='route'` ⇒ `action='attributed'`, `matched_rule_id IS NULL`, `task_id IS
  NULL`, `route_step` NOT NULL, and `ai_extraction_id` NOT NULL iff `route_step='model'`.
- Partial unique `(message_id) WHERE mode='route'` gives one route per message, forever.
- The shadow-overwrite guard is shared with D (D-D2).
- Accepted residual: a rule added after routing does not re-point a routed message.

**B-D6 — Application is `internal/capture/route.go`**: a pure `DecideRoute(facts, candidates,
verdict)` and a driver that writes rows directly (capture's own log) and calls no tool. It runs as
the `route_apply` stage in `pipelined`, woken by `route_classified`, under capture's lock
`0x5157_0015` (E-D4).

**B-D7 — Shadow → go-live** (the SWT-17/30 precedent).
- Verdicts accumulate in `ai_extractions`.
- Arming is per account: `UPDATE source_accounts SET route_after = now() WHERE account_email =
  '…'`. It is forward-only on the verdict clock for step 3; steps 1-2 apply inside the pass window.
- **Go-live gate: an eval against the rules tier's own answers.** Take ≥120 messages on the mailbox
  that RULES attributed (526 exist), hide the answer and score agreement per project.
  - This label set is deterministic output, not Salvador's judgement, and is marked
    `stratum:rules`. It is biased easy.
  - Every disagreement is read by hand before arming.
  - Salvador only skims the dry-run list of the real routes for the 115.

**B-D8 — Locality.** The lane is local-only (router `general=nil`). Unmatched messages are
`ClassRestricted` through `ClassOf`. The prompt carries only the message and the candidate rows.

**B-D9 — The classify store's account join is a named carve-out.**
`TestClassifyPackage_FetchesNothingAndDecodesNoMIME` is AMENDED:
- `raw_source_items` may appear only in one named constant that selects `source_account_id`;
- `raw_json` stays banned.

**Acceptance criteria — Part B**
- B1. `classify.LaneRoute` (`route`, `classify_route`, `route-v1`) with `RouteContract`. The schema
  has no `confidence`, `url` or `link*`. One bilingual prompt with no sender or client literals.
  The guards are rewritten.
- B2. The route inbox, integration-tested, with one fixture per clause and each mutation red:
  - inbound;
  - EXISTS a live `unmatched` decision;
  - the latest is `unmatched`;
  - the account has candidate rows;
  - no `classify_route` extraction;
  - `--since` required.

  The personal gmail account is the zero-row control.
- B3. `ResolveCandidate` covers null, 0, out of range and a valid index. `Grounded` fails a
  paraphrase and passes a verbatim span with different spacing.
- B4. `capture.DecideRoute` is pure, with a table test per step: one-project thread, two-project
  thread, single, grounded model, ungrounded with a default, ungrounded without a default, and no
  verdict.
- B5. One `mode='route'` row per applied message, with
  `ON CONFLICT (message_id) WHERE mode='route' DO NOTHING`, the predicate RESTATED. No task and no
  executor call. Run-twice writes nothing.
- B6. After a route row:
  - the inquiry inbox and promotion inboxes see the message;
  - the residue and triage inboxes do not;
  - a later shadow `--all` capture pass writes nothing for it.

  Mutation: drop the exclusion in `pendingMessages`.
- B7. Arming: `route_after` NULL → nothing is written. A step-3 verdict recorded before
  `route_after` is not applied.
- B8. `route_candidate_add {account_email, project, description, is_default?}` and
  `route_candidate_remove`: humanOnly, off MCP, audited.
  - `add` refuses an unknown account, an unknown project, an empty description, and a second
    default.
  - `opsctl route-candidates add|remove|list`.
  - The test enumerates the IK's actor shapes.
- B9. `classify report --lane route` breaks down by account and step, including `pending_verdict`
  and `ungrounded`. `rulesreport` gives route rows their own line. `/funnel` reads the lane
  through the lane loop.
- B10. `classify eval --lane route --labels docs/evals/route-from-rules.jsonl` prints a multi-class
  count table and honours `EvalResultThreshold`. The label file is produced by a documented query,
  carries no content, and has its labels validated against `projects.slug`.
- B11. The stages: `route` (GPU, woken by `captured`, lock `0x5157_0022`) and `route_apply` (woken
  by `route_classified`, lock `0x5157_0015`), publishing `route_classified` and `routed` (E-D3).
  The runbook gains a "Routing lane" section, and the IK an entry on the `route` mode.

---

## Part C — the inquiry lane as a pipeline stage; real asks promote into tasks

**Usable alone:** once enabled and armed, a Slack DM, or mail attributed by either tier, asking
Salvador something appears in the collaboratory board's **Holding column** (O7) as a human task. It
lands as soon as the 1h grace has passed after the connector's capture, bounded by the 5-minute
sweep; there is no cron hop. The task names the asker and the ask and carries `source_thread_id`. A
second ask on that thread logs onto it, a dismissed task follows SWT-36, and an ask answered within
the hour produces nothing. Nothing is sent and no console can claim these tasks. Every task's fate
is a precision label with no labelling ask.

**C-D1 — Two pipeline stages, reusing the existing code.**
- `inquiry` (GPU): `classify.Run` with `LaneInquiry`, `--since 72h`, woken by `captured`, `gated`
  and `routed`, under lock `0x5157_0022`. It publishes `inquiry_classified`.
- `inquiry_promote`: `promote.Run` for the inquiry lane, woken by `inquiry_classified` and the
  sweep (which is what releases grace-pending verdicts), under lock `0x5157_0021`, actor
  `promote:inquiry`. It publishes `promoted`.
- The classify-promote CronJob keeps the personal lane only (E5).

**C-D2 — Its own cutover, `projects.inquiry_promote_after`** (NULL = off, forward-only on the
verdict clock). SWT-33 D3's one-column-per-question rule.

**C-D3 — "Addressed to Salvador" is a deterministic spine rule**: addressed ⇔ `channel='gmail'`,
OR a 1:1 Slack DM (`slackweb.IsDirectMessageKey`), OR (`thread_scope='thread'` AND he posted on
that thread strictly before the ask).
- Not a model field: mentions are display names, and an identity in the prompt means
  `inquiry-v2`, a re-eval and a checkpoint bump.
- **Accepted recall cost:** a top-level `@Salvador` in a channel thread he is not in does not
  promote. (#a-millon no longer reaches collaboratory at all, per A-D5.)

**C-D4 — `slackweb.IsDirectMessageKey`**: the conversation segment starts with `D`, one spelling
beside `IsRootedThreadKey`, with group DMs excluded. **It ships only after §V4a shows it agrees
with the leaf's recorded `conversation.type='dm'` on every slack thread.**

**C-D5 — `ask_kind` whitelist `{question, request, decision, scheduling}`.** `fyi` asks nothing,
and excluding it also drops SWT-33's `fyi`+`needs_reply` contradictions.

**C-D6 — Two time bounds on `sent_at`.**
- Grace 1h, so the replied-since fold can fire first.
- Max age 72h (`InquiryMaxAge`), the second fence behind the verdict clock (SWT-30's backfill
  residual).
- The inquiry stage's `--since` is ≥ `InquiryMaxAge`.

**C-D7 — Any replied-since state blocks promotion.** The fold moves to a leaf package,
`internal/replyfold` (the join and column SQL, the prior-participation fragment, the states, the
scope rule), which classify and promote share. Ties read open (SWT-33 note 9). It is near-inert for
gmail.

**C-D8 — Gate first, then the existing `Decide`.** A gated verdict writes no row; stats and
`--dry-run` count it by reason. Passing verdicts go attach-open → attach+reopen dismissed →
create.
- **Create uses `inquiryCreateStatus`, a Go constant, initially `"holding"`** (O7): a `holding`
  task with `classify_promotions.action='review'`.
- The flip to `"ready"` (action `task`) is a one-line diff plus its test. Salvador makes it after
  about two weeks, on the `--outcomes` readout. It changes only tasks promoted after the deploy.

**C-D9 — Task shape.**
- Current attribution's project, `human`, priority 0.
- Title `{asker, else sender}: {ask}` via `textmatch.NormalizedPrefix(…,120)`.
- A deterministic body (ask_kind, asker, sender, channel, subject, sent_at, the ids, the thread
  identity, external_message_id, the reason).
- Provenance via `task_set_source_thread`. No permalink: it exists only in `raw_json`.

**C-D10 — A stored/current thread mismatch → gated `rethreaded`.**

**C-D11 — #110 gets provenance before arming** (`opsctl call --tool task_set_source_thread`).

**C-D12 — Dismissals are this lane's PIPELINE-precision label source, and O7's flip signal.** It
is a read-only readout, not an eval.
- **Unit:** a promoted inquiry task (a `task`/`review` promotion row with a `task_id` and a
  `classify_inquiry` extraction).
- **Outcome**, the pure `promote.InquiryOutcome(status, firstDismissal)`:

  | outcome | condition |
  |---|---|
  | **false positive** | FIRST dismissal `not_actionable` or `wrong_kind` |
  | **true positive** | closed with no dismissal, `delivered`, or first dismissal `handled_elsewhere` |
  | **mis-click** | first dismissal plain-reopened by a HUMAN actor (`policy.HumanActor`; SWT-36 D6) |
  | **excluded** | `duplicate`; still open; `attached` promotions |

  An activity-reopen does not undo the label.
- **It measures precision, never recall** (runbook criterion 32). It does not replace the committed
  model eval, and nothing enters `inquiry-needs-reply.jsonl` here. `classify eval` is untouched
  (board-dismissals_SPEC's caution).
- **Precedence:** the Holding column is where review happens; dismissals are what it records.
  There is no labelling ask.
- **Printing:** counts always, and a ratio only at ≥ `EvalResultThreshold` decided. The fold lives
  in `promote`; the threshold and marker are applied in `cmd/classify`.

**Acceptance criteria — Part C**
- C1. `classify promote --lane personal` (the default) is byte-identical to today.
- C2. The inquiry inbox, one query:
  - `classify_inquiry`, ok;
  - `needs_reply`;
  - inbound;
  - latest `action='attributed'` in any mode, including `route` and `gate` (a fixture each);
  - `ai_inquiry` AND `inquiry_promote_after` AND `r.created_at >= inquiry_promote_after`;
  - `sent_at >= now() - InquiryMaxAge`;
  - no promotion row;
  - oldest first.

  One fixture per clause, and cross-lane controls.
- C3. The pure `promote.InquiryGate(c, now)` with reasons `rethreaded`, `kind`, `stale`,
  `pending`, `answered`, `not_addressed`, with a table test.
- C4. Addressed integration tests (DM, top-level channel, rooted thread with an earlier post, a
  post only after → `answered`, group DM, gmail), plus two mutations (direction, strictness).
- C5. `IsDirectMessageKey` unit tests. The key-spelling scan extends to `promote` and `replyfold`.
- C6. `replyfold` imports only stdlib and `slackweb`. `classify.Summarize` output is
  byte-identical. `TestClassifySummary_OwnsTheQueriesAndTheFolds` is AMENDED.
- C7. `Decide`'s branches each have an inquiry-lane integration test (attach, reopen, Q3 new task,
  create).
- C8. Create order: `create_task` → `recordTask` → `task_set_source_thread` as `promote:inquiry`.
  Audit rows are asserted. Title and body are exact-text.
  - **`inquiryCreateStatus == "holding"` is pinned by a unit test.** The test's comment names O7
    and says the flip to `"ready"` is a deliberate one-line change that edits this assertion in the
    same diff.
  - An integration test asserts that a created inquiry task is `holding`, with promotion action
    `review`.
- C9. Claim-before-act; run-twice writes nothing; a personal-promoted message counts as `Lost`.
- C10. A gated verdict writes no row and calls no tool. A pending verdict promotes on the first
  pass or sweep after its grace (fixture).
- C11. The `inquiry` stats block with gated counts. `--dry-run`. `--max-age` is refused unless
  `--dry-run`.
- C12. Scans hold. Migration guard. Ledger. `CountersByLane` returns the `classify_inquiry` row
  (under `review` while O7's holding phase lasts).
- C13. The two stages are wired in `pipelined` (E3's injected-loop test, plus one end-to-end
  integration: a `captured` publish leads to a verdict, and the sweep then promotes after grace
  with the clock set via fixture `sent_at`). The handoff doc and runbooks are updated. The IK entry
  restates that `classify eval` writes no `ai_runs` and that `--max-age` is dry-run-only.
- C14. The dismissal readout: `InquiryOutcome` table test; the `InquiryOutcomes` SQL fold on the
  FIRST dismissal (mutation: latest → red); `classify promote --outcomes` with the threshold and
  marker. The runbook documents it as O7's flip signal: where to look, what the counts mean, and
  that the flip is `inquiryCreateStatus = "ready"`.

---

## Data model changes

Migrations are numbered in ship order: **0027 (D)**, **0028 (C)**, **0029 (B)**. Parts A and E
have none. The numbers are provisional: take the next free one at implementation time, because
`capture-rule-ticket-keys` may claim one first.

**`0027_capture_ticket_gate.sql` (Part D).** Confirm the inline constraint names against prod
first, as 0015 did: `capture_decisions_action_check` and `capture_decisions_mode_check`.
```sql
ALTER TABLE capture_decisions DROP CONSTRAINT capture_decisions_action_check;
ALTER TABLE capture_decisions ADD CONSTRAINT capture_decisions_action_check
  CHECK (action IN ('unmatched','attributed','task','task_log','held'));
ALTER TABLE capture_decisions DROP CONSTRAINT capture_decisions_mode_check;
ALTER TABLE capture_decisions ADD CONSTRAINT capture_decisions_mode_check
  CHECK (mode IN ('shadow','live','gate'));
ALTER TABLE capture_decisions ADD CONSTRAINT capture_decisions_gate_shape CHECK (
  mode <> 'gate' OR (action IN ('task','task_log','attributed')
                     AND matched_rule_id IS NOT NULL AND external_system IS NOT NULL
                     AND external_key IS NOT NULL));
ALTER TABLE capture_decisions ADD CONSTRAINT capture_decisions_held_is_not_gate CHECK (
  action <> 'held' OR mode IN ('shadow','live'));
-- One resolution per message, forever. PARTIAL: every ON CONFLICT restates WHERE mode = 'gate'.
CREATE UNIQUE INDEX capture_decisions_gate_uniq ON capture_decisions (message_id) WHERE mode = 'gate';
```
**Old binaries are unaffected.** They never write `held` or `gate`, and their conflict target
(`WHERE mode='live'`) is unchanged. But a deployed OLD capture binary keeps creating tasks for
gated projects until the connector images are bumped. That is today's behaviour, not a regression.

**`0028_inquiry_promotion.sql` (Part C).**
```sql
ALTER TABLE projects ADD COLUMN inquiry_promote_after TIMESTAMPTZ;  -- NULL = off; no default, no backfill
```
O7's Holding-first is a Go constant (`inquiryCreateStatus`), not a column. No schema change for
it.

**`0029_route_tier.sql` (Part B).**
```sql
ALTER TABLE capture_decisions DROP CONSTRAINT capture_decisions_mode_check;
ALTER TABLE capture_decisions ADD CONSTRAINT capture_decisions_mode_check
  CHECK (mode IN ('shadow','live','gate','route'));
ALTER TABLE capture_decisions
  ADD COLUMN route_step TEXT CHECK (route_step IN ('thread','single','model','default')),
  ADD COLUMN ai_extraction_id BIGINT REFERENCES ai_extractions(id),
  ADD CONSTRAINT capture_decisions_route_shape CHECK (
    (mode = 'route') = (route_step IS NOT NULL)
    AND (mode <> 'route' OR (action = 'attributed' AND matched_rule_id IS NULL AND task_id IS NULL))
    AND ((route_step = 'model') = (ai_extraction_id IS NOT NULL)));
CREATE UNIQUE INDEX capture_decisions_route_uniq ON capture_decisions (message_id) WHERE mode = 'route';

CREATE TABLE source_account_projects (           -- configuration, not work (capture_rules precedent)
  id                BIGSERIAL PRIMARY KEY,
  source_account_id BIGINT NOT NULL REFERENCES source_accounts(id),
  project_id        BIGINT NOT NULL REFERENCES projects(id),
  is_default        BOOLEAN NOT NULL DEFAULT false,
  description       TEXT NOT NULL,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (source_account_id, project_id)
);
CREATE UNIQUE INDEX source_account_projects_default_uniq
  ON source_account_projects (source_account_id) WHERE is_default;
ALTER TABLE source_accounts ADD COLUMN route_after TIMESTAMPTZ;  -- NULL = routing off
```
- No seeding anywhere. Config goes in through tools (0015's recorded reason).
- No new work tables. `held`, `gate` and `route` rows are decisions.
- C-D12 reads existing tables.
- If B ships before D, 0029's mode list omits `'gate'` and D adds it.

## API / MCP tool changes

- **New executor tools (B):** `route_candidate_add`, `route_candidate_remove`. Both humanOnly, off
  MCP. opsctl gets `route-candidates add|remove|list`.
- **Existing tools, unchanged, new callers:**
  - D: `create_task`, `link_external_ref`, `task_set_source_thread`, `task_append_log` and guarded
    `task_reopen`, as `capture:gate`;
  - C: `create_task` (`status: holding` under O7; `ready` after the flip), `task_append_log`,
    `task_set_source_thread` and guarded `task_reopen`, as `promote:inquiry`;
  - A: `capture_rule_add` via opsctl;
  - #110: `task_set_source_thread` via `opsctl call`.
- **No executor call** from route application, gate `attributed` resolutions, the outcome readout
  or any MQTT publish.
- **The Jira lookup is ingestion, not a tool.** It writes raw-first through `jira.LookupIssues`
  plus a `sync_runs` row, exactly as the reconciler's does. Auditability is that row, the stored
  snapshot and the `gate` decision row's reason.
- **CLI:**
  - `classify promote [--lane personal|inquiry] [--dry-run] [--limit N] [--max-age <dur>, dry-run
    only] [--outcomes [--since]]`;
  - `classify run|report|eval --lane route`;
  - `pipelined` (flags: `--stages`, `--sweep`);
  - `opsctl capture-rules gate [--dry-run]`, a one-shot gate pass for smoke tests and backfill.

## MQTT topics

| topic | retained | QoS | payload | publisher | subscriber |
|---|---|---|---|---|---|
| `ops/pipeline/captured` | no | 1 | `pipeline.Wake` | each connector main | pipelined: gate, route, inquiry |
| `ops/pipeline/gated` | no | 1 | `pipeline.Wake` | pipelined: gate | pipelined: inquiry |
| `ops/pipeline/route_classified` | no | 1 | `pipeline.Wake` | pipelined: route | pipelined: route_apply |
| `ops/pipeline/routed` | no | 1 | `pipeline.Wake` | pipelined: route_apply | pipelined: inquiry |
| `ops/pipeline/inquiry_classified` | no | 1 | `pipeline.Wake` | pipelined: inquiry | pipelined: inquiry_promote |
| `ops/pipeline/promoted` | no | 1 | `pipeline.Wake` | pipelined: inquiry_promote | none (observability; the follow-up) |
| `ops/workers/pipeline.{stage}/status` | **yes** | 1 | `fleet.Status` (`idle`/`working`), LWT `{"state":"dead"}` | each stage | fleetd mirror (when deployed), mosquitto_sub |

No work payloads and no commands. The orchestrator's `ops/workers/{id}/cmd` topics are untouched.

## Files likely to touch

- **Part A:** `docs/runbooks/capture-rules.md` only.
- **Part E:**
  - new `internal/pipeline/contract.go` (+ tests);
  - new `cmd/pipelined/main.go` (the stage loop, sweep, heartbeats; the loop in a testable
    `internal/pipeline/stage.go`);
  - `internal/fleet/client.go` (a generic `Publish(topic, qos, retained, payload)` if absent);
  - every `cmd/connectors/*/main.go` (publish after capture);
  - `docs/runbooks/pipeline.md`, `HANDOFF-kube-inquiry-promote.md`.
- **Part D:**
  - migration 0027;
  - `internal/capture/rules_store.go` (`held` in `decideMessage`, the gate column in
    `loadRules`, the `gate`/`route` exclusion in `pendingMessages`);
  - new `internal/capture/gate.go` (`DecideGate`, the driver, exported task helpers),
    `rulesreport.go`;
  - `internal/ticketstatus/decide.go` (extract `Warranted`), `store.go` (extract
    `EnsureSnapshots`);
  - a shared lookup-client-factory helper (from `cmd/connectors/jira`);
  - `cmd/opsctl/main.go` (`capture-rules gate`);
  - `docs/runbooks/ticket-status-sync.md`.
- **Part C:**
  - migration 0028;
  - `internal/replyfold/*`;
  - `internal/promote/inquiry.go` (gate, inbox, title/body, `inquiryCreateStatus`,
    `InquiryMaxAge`), `outcomes.go`, `store.go` (lane selector, stats, actors);
  - `cmd/classify/main.go`;
  - `internal/classify/summary.go`, `inquiry.go`, `summary_structure_test.go`;
  - `internal/connector/slackweb/normalize.go`, `threadscope_test.go`;
  - `docs/runbooks/local-classifier.md`.
- **Part B:**
  - migration 0029;
  - `internal/classify/route.go`, `lane.go`, `store.go`, `eval.go`, `summary.go`/`report.go`,
    `lane_test.go`, `structure_test.go`;
  - `internal/capture/route.go`;
  - `internal/tools/routecandidates.go` + `internal/policy`;
  - `cmd/opsctl/main.go`;
  - `docs/evals/route-from-rules.jsonl`.
- **All parts:** `internal/classify/structure_test.go` (the ledger), `.claude/INSTITUTIONAL_KNOWLEDGE.md`.

## In scope / Out of scope

**In scope:** Parts A–E as written.

**Out of scope (do not bundle):**
- **Deploying orchestratord**, and deciding the 866-event backlog. That is the precondition
  ticket (see Dependencies).
- **Converting the existing downstream CronJobs** (classify-personal, classify-residue, the
  classify-promote personal lane) to pipeline stages. That is the immediate follow-up, reusing
  E's contract.
- **The O7 flip itself** (`inquiryCreateStatus` → `"ready"`). It is Salvador's call after about two
  weeks of `--outcomes`, a separate one-line change.
- Deploying fleetd and hooksd.
- `slackweb-collab-export-stale` (Miss B).
- Rule 10 / Treetop keys, rule edits, enable/disable subcommands (`capture-rule-ticket-keys`).
  Rule 1 and rule 2 themselves are unchanged.
- Turning the gate on for collaboratory: its column stays false, and that is the
  capture-rule-ticket-keys territory.
- Tasks 72/75/93/94 and their refs.
- Other HOC/LlamaSite channels (the same recipe as A-D5, one rule each, once named).
- Open-set routing. Routing for Slack workspaces.
- A live re-decide path for task-creating rules on already-decided messages.
- A `source_account` rule criterion (B-D1 explains why).
- Dismissals into `classify eval`. Drafting and sending. Inquiry prompt changes. Auto-close on
  reply. Raising the inquiry labelled set. A holding→ready verb on the dashboard. The `thread_id`
  index.

## Invariants that apply

1. **Raw-first:** the Jira lookup stores each issue raw-first through `LookupIssues` before any
   decision reads it. The gate decides from the STORED snapshot, never an in-memory response
   (SWT-32 D19). B-D9's `source_account_id` join is the only other raw touch, and it has no
   `raw_json`.
2. **One funnel:**
   - tasks stay in `tasks`, and the Holding column is a filter, not a table;
   - `held`/`gate`/`route` are decision rows;
   - `source_account_projects` is configuration;
   - MQTT carries wake-ups, never work;
   - the queue of record for every stage is a SQL filter over existing tables.
3. **Everything through the executor:** every task write, whether by the gate (`capture:gate`) or
   by promotion (`promote:inquiry`), goes through the executor. Candidate config goes through
   humanOnly tools, and rules through `capture_rule_add`. The only direct writes are capture's own
   log and `classify_promotions`, and the existing direct-write scans still apply.
4. **Nothing external without a delivery row:** nothing sends. A Jira GET is a read.
5. **Own-message loop closure:** every inbox is inbound. `held`, `gate` and `route` all require a
   live decision, which an outbound message never has. Outbound rows are evidence only.
6. **Stealth attribution:** internal board text only.
7. **Orchestrator purity:** the orchestrator is unchanged and still calls no LLM.
   - The message-level stage graph is a pure table (`pipeline.Subscribers`).
   - Every stage decision is a pure, offline-tested function: `DecideGate`, `DecideRoute`,
     `InquiryGate`, `Decide`, `InquiryOutcome`, `ticketstatus.Warranted`.
   - The LLM stages are leaf consumers.
   - Task lifecycle after creation is the orchestrator's R-rules, via the `task_events` the
     executor writes.

## Sibling patterns to copy

- **Wake-up only, cursor/queue is truth:** `internal/orchestrator/engine.go` (`Listen` +
  `DrainOnce`).
- **The MQTT contract shape:** `internal/fleet/contract.go`. Spine clients:
  `fleet.NewSpineClient` (a distinct id per connection).
- **The Jira lookup:** `internal/ticketstatus/store.go` (`RouteLookup`, TTL, `LookupIssues`,
  read-back) and `decide.go`.
- **Capture actions:** `internal/capture/rules_store.go` (`createRuleTask`, `linkRuleRef`,
  `setRuleProvenance`, `appendRuleLog`, `reopenRuleTask`, `insertDecision`'s restated partial
  `ON CONFLICT`).
- **Lanes:** `classify/lane.go`, `inquiry.go`, `ResolveLink`. **Promotion:** `promote/store.go`,
  `CountersByLane`. **An autonomy constant pinned by a test:** `promote.whitelist` (SWT-30 D2),
  the model for `inquiryCreateStatus`.
- **CHECK widening:** 0015 step 3. **HumanOnly tools off MCP:** `capture_rule_add`.
- **Kube handoff:** `docs/runbooks/HANDOFF-kube-swt18.md`.
- **Queue claims:** advisory locks per stage. `FOR UPDATE SKIP LOCKED` is not needed while each
  stage is single-instance (replicas 1, `Recreate`). A second replica would need it, and that is
  noted in the runbook.

## Verification protocol

- **V1.** `go test ./...`.
- **V2.** `make integration` (`-p 1`, `itest-` fixtures, FK-ordered cleanup including
  `policy_decisions`/`audit_events` by actor `promote:%` and `capture:gate`). The MQTT tests use the
  compose broker (`tcp://localhost:1884`), never production, and clear their retained topics.
- **V3. Mutations** (apply, see red, revert, record):
  - Part C inbox clauses and addressing; `IsDirectMessageKey` on `G…`; first-vs-latest dismissal;
    `inquiryCreateStatus` changed without its test;
  - Part B's live-unmatched clause, the `pendingMessages` exclusion, grounding and the restated
    `ON CONFLICT`;
  - Part D's gate read from a constant, the gate row's restated `ON CONFLICT`, and the ref
    re-query dropped (two tasks for one key);
  - Part E's publish-before-commit.
- **V4. Prod read-only, before implementation** (`BEGIN READ ONLY … ROLLBACK`; nothing frozen in
  tests):
  - (a) Export `(thread_key, raw conversation.type)` for every slack thread and run
    `IsDirectMessageKey` in Go. Zero disagreements, ≥1 DM per workspace.
  - (b) The 16 flagged inquiry verdicts of 2026-09-10.
  - (c) Slack export freshness per workspace (Miss B).
  - (d) The handsonconnect mailbox breakdown and top senders.
  - (e) #a-millon keys: export `T0360B84U` thread keys and run `strings.HasPrefix` with the exact
    pattern in Go. Every match has conversation `C1C1TSLJH`, the count equals the channel's message
    count, and the case is `T0360B84U`. Also count its messages matching `LHH-[0-9]+`: the ones
    rule 1 keeps.
  - (f) `opsctl capture-rules list`: priorities of 1, 2, 8, 9, 10, 59, the bulk and jira rules.
    Required: no enabled Slack-matching rule sits between 99 and 100 except rules 1-2, and nothing
    above 100.
  - (g) `schema_migrations` max against `ls migrations/`.
  - (h) The A1 regexp export.
  - (i) `report --domain cecollaboratory.com`.
  - (j) Gate sizing:
    - over 30 days, how many live decisions came from `external_system='jira'` rules on gated
      projects, and how many distinct keys (the GET budget);
    - that account 8309's scopes are `{LHH,LHHSF}` and its stored token decrypts in the
      `pipelined` environment (one `EnsureSnapshots` dry call).
  - (k) Broker: `mosquitto_sub -h 192.168.50.45 -t 'ops/pipeline/#' -v` shows nothing before E
    (the topic space is free).
- **V5. Dry-runs before arming.**
  - D: `opsctl capture-rules gate --dry-run` over a shadow `held` set produced by
    `capture-rules run --since 720h --all` with the new code. Expected: the tickets behind tasks
    65/72/75/76/90/93/94 resolve `attributed (not_assigned|ticket_done)`, zero tasks.
  - C: set `inquiry_promote_after='2026-09-01'` while the old image is deployed, run
    `classify promote --lane inquiry --dry-run --max-age 720h`, read it against V4b, then reset to
    NULL. Every would-create line shows `status=holding` (O7).
  - B: the route lane shadow pass, the route eval, and a dry-run apply. Salvador skims the 115.
- **V6. Go-live, in ship order:**
  1. **A:** add the rules, run the §A-D6 pass, check A3.
  2. **E:**
     1. build the image, hand off (`pipelined` with `PIPELINE_STAGES=` empty, and `MQTT_BROKER` on
        the connectors);
     2. watch `ops/pipeline/captured` arrive after each connector tick, and
        `ops/workers/pipeline.+/status` heartbeats;
     3. kill the pod and see `dead`.
  3. **D:**
     1. apply 0027, bump the connector images (capture starts writing `held` for reengine), then
        enable `gate`;
     2. check that the next LHH mention yields a `held` then a `gate` row within one wake, with a
        task only if assigned;
     3. run the reconciler as usual (the backstop).
  4. **C:**
     1. apply 0028, then enable `inquiry` + `inquiry_promote`;
     2. run the one-shot `classify run --lane inquiry --since 336h` by hand for the backlog, and
        read `classify report`;
     3. set #110's provenance, then arm;
     4. check `/tasks?project=collaboratory&status=holding` (O7), the `/funnel` `classify_inquiry`
        row under `review`, `deliveries` unchanged and `classify promote --outcomes`;
     5. **about two weeks later**, Salvador reads `--outcomes` and, if satisfied, ships the one-line
        `inquiryCreateStatus = "ready"` change (out of scope here).
  5. **B:** apply 0029 → `route-candidates add` → enable `route` + `route_apply` in shadow (not
     armed) → the V5 reads → arm `route_after` → one `classify run --lane route --since 720h`
     backfill → confirm the 115.

  Historical asks older than 72h never become tasks. Salvador hand-files the Rochester thread if it
  is still open.
- **V7.** `/ticket-review` per part (go-reviewer + the adversarial pass: model output creates tasks
  and changes attribution; D creates tasks from a network read).

## Dependencies

- **PRECONDITION TICKET — deploy orchestratord** (not folded in). It must decide the backlog:
  replay from cursor 75, or re-seed at current `max(id)`. 05-orchestrator-loop_SPEC's first-deploy
  rule seeds at `max(id)`, and the 866 pending events include 69 `status_changed` and one
  `delivery_sent`, which R8 would act on.
  - **What in SWT-40 needs it:** only the lifecycle of the tasks SWT-40 creates, as for every task.
    That is R3 (Deliver task on `done_locally`), R4/R5 (dependencies), R8 (delivery), and R1
    (a feedback task and resume on `needs_feedback`). The 3 `done_locally` and 9 `blocked` tasks in
    prod show that gap exists today regardless of this ticket.
  - **What in SWT-40 works without it:** every message-level boundary (E-D2): `captured`, `gated`,
    `route_classified`, `routed`, `inquiry_classified`, `promoted`. Also the gate's and promoter's
    task creation, which go through the executor directly, not through orchestrator rules. So A–E
    all ship and run without orchestratord. What waits is only what happens to a promoted task
    after Salvador moves it.
- **`slackweb-collab-export-stale`** blocks Miss B only.
- **`capture-rule-ticket-keys`** (untracked in the main checkout). This ticket does not edit that
  file and does not touch rule 10 or key derivation.
  - **Overlap:** D's gate is generic over `external_system='jira'` + `ticket_assignee_gate`, the
    same shape as the Treetop rules that ticket owns. Collaboratory's gate stays OFF here, so rule
    10 and rules 3-5 behave exactly as today; turning it on is that ticket's or the owner's call.
  - **Shared ground:** `rules_store.go` (D's `held` path, their Q2(iii) `append_only` if chosen:
    both edit `decideMessage`, and the second to merge rebases), migration numbering, the ledger.
  - **Priorities:** the name rules (3) sit below their mention rule at either priority, and
    #a-millon (99) sits above it. #a-millon, a slack prefix, can never match the Foundry mail
    rule 59 exists for, so their "rule 59 still resolves" criterion is unaffected.
  - Run §A-D6's shadow pass after both rule sets, or run it twice.

## Future work

- **Immediate follow-up: convert classify-personal, classify-residue and the classify-promote
  personal lane to `pipelined` stages.** They subscribe to `captured` (and a new
  `personal_classified`) through E's contract unchanged, and delete their CronJobs. Only connector
  ingest + capture then stay on cron (O6).
- The O7 flip (`inquiryCreateStatus` → `"ready"`), after about two weeks of `--outcomes`.
- Moving the connectors from cron polling to push where the source allows (Slack leaf, IMAP IDLE
  already exists).
- `FOR UPDATE SKIP LOCKED` claims, if a stage ever needs >1 replica.
- `classify eval --labels-from-dismissals` (the ENRICHED stratum only, after the eval-persistence
  re-check).
- Open-set routing. A live re-decide path for task-creating rules. A model-side "addressed" signal
  if C-D3's recall cost shows.
- Auto-close / log-on-reply. Slack reply drafting from `source_thread_id`.
- Other HOC/LlamaSite channels.
- The `normalized_messages.thread_id` index.
- Outcome readouts per lane and step on `/funnel`, and a `wrong_project` dismissal code.
- Route correction when a later rule disagrees.
