> Jira: SWT-40

# inquiry-promote — deterministic attribution first, local-LLM routing after, then real asks become tasks

**STATUS: PROVISIONAL — two owner questions open** (`docs/tickets/inquiry-promote_OPEN_QUESTIONS.md`).
Q1 decides where one capture rule points; Q2 decides one Go constant's value. Everything else is
decided below, either by the owner (dated) or unilaterally (with rationale).

**Three parts, each usable alone, shipped in order A → C → B.** B's shadow period can start as soon
as A lands. Recommendation for tracking: Part B is its own SWT issue — it adds a classify lane, a
decision mode and a config table, and its go-live gate is independent of A and C.

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
  The noise was FYIs, people addressing each other (`@esteban …`, from Avviato channel
  `T0360B84U:C1C1TSLJH`), and stale moments.
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
    produce (Part B handles this).
- `capture_decisions.mode` CHECK is `('shadow','live')`, and it has an `(action='unmatched') =
  (project_id IS NULL)` CHECK (0015).
- Criteria types: `body_regex, sender, thread_key_prefix, thread_key_contains,
  source_slack_workspace, person`. `body_regex` matches `Subject + "\n" + BodyText` (`matchText`);
  `sender` is a case-insensitive substring of the raw From. A body_regex rule with no
  `external_system` is attribution only.
- Rule priorities per the capture-rules SPEC fixture table (confirm with `opsctl capture-rules
  list`, §V4f):
  - rule 8 (`T0HPR78RX`) → 10;
  - rule 9 (`T0360B84U` catch-all) → 1;
  - rule 10 (Treetop keys) → 90 (about 40 once `capture-rule-ticket-keys` lands);
  - rule 59 → 95;
  - the jira thread-key rules → 50;
  - the bulk sender rules → 5.
- **Classify lanes:** three lanes, two contracts, all local qwen on the z4 (4.5 s/verdict median
  for inquiry). `cmd/classify`'s router has NO hosted client. The residue lane's inbox is latest
  `unmatched`, with `ClassOf` → restricted. The residue and personal lanes share ONE contract,
  guarded by `LanePersonal.Contract == LaneResidue.Contract`.
- `internal/classify` may not contain the token `raw_source_items`
  (`TestClassifyPackage_FetchesNothingAndDecodesNoMIME`). A message's source account is reachable
  only through `raw_source_items.source_account_id`.
- **No self-reported confidence:** the structure scan bans `confidence` anywhere in the classify
  package, and the runbook ("Self-reported confidence is a CONSTANT") measured it constant for
  qwen.
- `classify.EvalResultThreshold = 120` and `classify.EvalIndicativeMarker` (`eval.go:516,521`)
  are the one spelling of "no ratio below 120 labels".
- `task_dismissals.reason_code ∈ {not_actionable, wrong_kind, duplicate, handled_elsewhere}`
  (0022). Since 0026 a task may carry several rows; `reopened_by_message_id` separates "overtaken
  by activity" from a human's plain reopen (SWT-36 D6). `classify eval` writes no store rows, and
  board-dismissals_SPEC's out-of-scope note requires re-checking that before dismissals become
  eval labels.
- `internal/promote` promotes only `worker_type='classify'` verdicts, and is barred from reaching
  `internal/provider` transitively (so it cannot import `internal/classify`). `task_get_next`
  routes only `assignee_type='claude'`. The drafts worker does not draft `slack_reply`. The image
  is distroless, so a CronJob cannot chain lanes with `&&`.
- Slack: a DM's key is `slack:{ws}:D…` (the leaf's own fallback, `slackconnector
  src/slack/channels.ts:20`; production keys like `slack:T0360B84U:D01EJRX6P45`). Mentions reach
  `body_text` as rendered display names, never as `<@U…>`. The conversation `type` exists only in
  `raw_json`.

---

## Part A — deterministic attribution, and re-pointing what is already decided

**Usable alone:** after A, a message whose subject names Collaboratory (or ReEngine) is attributed
to that project by a rule. The 55 already-`unmatched` Collaboratory-named mails point at
collaboratory for every reader, and the Avviato channel stops feeding collaboratory (if Q1 says so).
There is no code change, no model and no task creation.

**A-D1 — Name rules match the SUBJECT LINE only.** The pattern is
`(?i)\A[^\n]*\b<alias>\b`: `\A[^\n]*` pins the match to the first line of `matchText`, which is the
subject (for Slack it is the channel name).
- A subject naming the project is Salvador's own example ("it's telling the project right there").
- A body mention is the "Avviato mail that mentions Collaboratory in passing" case, or a
  signature, a quoted thread, a newsletter. Body-only mentions are left to Part B, where they are
  judged in context within a human-approved candidate set.

**A-D2 — Priority 3.** It sits below every source-specific rule: tickets (90/50/about 40),
rule 59 (95), workspaces (10), bulk senders (5). A stronger signal always wins, and marketing mail
that happens to name the project stays in bulk. It sits above the catch-all (1) and the channel
rule (2).

**A-D3 — Aliases are per-project DATA, seeded through `capture_rule_add`, never generated from
`projects.name`.** Project names like `personal`, `foundry`, `bulk` and `homelab` are ordinary
words; a generated rule for them would misattribute on day one.
- The recipe (runbook) is one attribution-only rule per project, alias list chosen per project.
- An alias is admitted only after the Go-regexp export (§V4h) shows its subject-line matches
  outside the project, by current attribution, are zero or explained.
- **Seeded by this ticket:** collaboratory `(?:ce)?collaboratory` (this covers the
  cecollaboratory.com product domain as written in subjects; `\b` does not fire inside
  "cecollaboratory") and reengine `re-?engine`, the other candidate on the mailbox (O2).
- Other projects only when a miss shows their name, via the same recipe.

**A-D4 — `cecollaboratory.com` sender rule** (`sender`, priority 3, collaboratory, attribution
only). It is added ONLY after `opsctl capture-rules report --domain cecollaboratory.com` has been
run and read; the runbook's Phase-0 rule makes that read a blocker.

**A-D5 — The Avviato channel rule.** Attribution-only, `thread_key_prefix`
`slack:T0360B84U:C1C1TSLJH`, priority 2: above rule 9's catch-all (1), below everything else,
including the Treetop mention rule at either of its priorities. Its destination is **Q1**. Prefix
caveat: it would also match a longer id starting with `C1C1TSLJH`; §V4e checks that no such id
exists.

**A-D6 — Re-pointing already-decided messages uses the existing shadow `--all` pass**, run once
after the rules land: `opsctl capture-rules run --since 720h --all`. It is the documented
post-rule-change step and the mechanism the latest-decision readers already follow, so no new
code is needed.
- **Stated plainly:** it writes a newer shadow row for every inbound message in the window, under
  the rule set as of now. A shadow row cannot create or log a task, so it re-points attribution
  only. That is exactly what the 55 need, and it is why it works for attribution-only rules and
  NOT for task-creating ones.
- A live re-decide path for task-creating rules is Future work.

**Acceptance criteria — Part A**
- A1. Before any rule is added, the candidate patterns are run through Go `regexp` over an export
  of subject+body+account+current attribution (§V4h; the `capture-rule-ticket-keys` recipe,
  because Postgres reads `\b` as a backspace). The runbook records per alias: matches by current
  project, by account and by sender domain. Required: the Rochester thread's
  "Questions About Collaboratory …" messages match, and an alias with unexplained out-of-project
  matches is not added.
- A2. The rules are added with `opsctl capture-rules add` only (executor, humanOnly, audited), all
  without `--external-system`. `opsctl capture-rules list` shows them at priorities 3 and 2 with
  notes naming this ticket.
- A3. After the §A-D6 pass, a read-only query shows that the collaboratory-named messages on the
  handsonconnect mailbox have a latest `attributed` → collaboratory decision, and that `C1C1TSLJH`
  messages point at the Q1 destination. Before/after counts go into
  `docs/runbooks/capture-rules.md`.
- A4. `docs/runbooks/capture-rules.md` gains three sections:
  - "Project-name rules": the recipe, the subject-line pattern shape, priority 3 and the
    measurement gate;
  - "Re-pointing already-decided messages": the live-unmatched fact above, and what the shadow
    `--all` pass does and cannot do;
  - the Avviato channel rule.

---

## Part B — the local-LLM routing tier (after rules fail)

**Usable alone:** after B is armed for the handsonconnect mailbox, any mail there that the rules
left `unmatched` is attributed to collaboratory or reengine within about two hours. The step is
recorded as a typed decision row naming how it was chosen: thread, single, model or default.
Nothing else in the system changes. Before arming, the lane runs in shadow: verdicts only, visible
in `classify report --lane route`.

**B-D1 — Closed candidate sets only, configured per source account in data.** A new config table,
`source_account_projects` (source_account_id, project_id, is_default, description), is written
only through new humanOnly executor tools. A Slack workspace is a source account too
(`{ws}@slack-web.local`), so per-workspace comes free.
- **Only accounts with candidate rows are routed at all.** Open-set routing over the whole residue
  is Future work.
- The human-written candidate row is the AUTHORISATION that lets a message move into a project
  whose `ai_locality` may be wider than its origin (IK SWT-21 boundary). A model choosing from
  every project would widen exposure by model output.
- Open-set routing would also put about 14k residue messages in front of the GPU with nothing to
  measure precision against.
- Seed: handsonconnect → {collaboratory (default), reengine} (O2, O3).

**B-D2 — Four steps, the first two deterministic.** For each inbound message on an armed account
whose live decision is `unmatched` and whose latest decision is still `unmatched`:
1. **thread**: other messages on its thread carry latest decisions attributing to exactly ONE
   candidate project → that project.
2. **single**: the account has exactly one candidate → it.
3. **model**: a stored `classify_route` verdict chose a candidate and passed the grounding gate
   (B-D4) → that project.
4. **default**: the verdict chose nothing, or failed grounding, and the account has a default →
   the default (O3). With no default → stay `unmatched`.

No verdict yet → skip this pass (`pending_verdict`); a missing verdict never falls to the default.
Steps 1-2 cost no GPU and are why the latest Rochester message (whose subject lacks the name)
follows its thread once Part A has attributed the rest.

**B-D3 — A fourth classify lane, `route` (`worker_type='classify_route'`), with its own contract,
kept SEPARATE from residue and inquiry.**
- The code supports one question per contract per lane. Residue's contract is pinned equal to
  personal's (SWT-23 comparability), so folding routing into it breaks that guard.
- The inquiry lane needs a project and thread context that an unmatched message does not yet have.
  One combined pass would also make neither eval separable.
- Cost of separation: GPU on the route-armed accounts' unmatched mail only. That is about
  115 backlog plus a few per day at about 5 s each.
- The lane-count and contract-count guards in `internal/classify/lane_test.go` are REWRITTEN to
  "four lanes, three contracts", with the argument in the comment, never deleted.

**B-D4 — Output contract, no confidence:** `{project_index: integer|null, evidence: string,
reason: string}`, all required, `additionalProperties:false`.
- The prompt shows the account's candidates as a numbered list (slug, name, client, the candidate
  row's `description`). `project_index` is 1-based into that list, or null for "none of these".
  It is resolved in Go, `ResolveLink`'s pattern: the model never authors a slug.
- **Grounding gate (the precision gate that replaces confidence):** the choice counts only if
  `evidence` is a whitespace-collapsed, case-folded substring of the message's sender, subject or
  body. Any other answer (null, out of range, ungrounded) is "no confident choice" → step 4.
- For the handsonconnect mailbox this means reengine is chosen only on quoted evidence, and
  everything else lands on collaboratory. That is O3, made deterministic.

**B-D5 — The applied decision is a `capture_decisions` row with a NEW mode, `'route'`.**
- Why a new mode and not `live`: a second live row per message is impossible under
  `capture_decisions_live_uniq`. Changing that partial index breaks every deployed binary's
  `ON CONFLICT (message_id) WHERE mode='live'` (the SWT-36 landmine).
- Why a row in this table and not a side table: every latest-decision reader follows
  `capture_decisions` already, so a side table would mean rewriting about 15 queries.
- The schema pins the shape: `mode='route'` ⇒ `action='attributed'`, `matched_rule_id IS NULL`,
  `task_id IS NULL`, `route_step` NOT NULL, and `ai_extraction_id` NOT NULL iff
  `route_step='model'`. A route row can never create or log a task, so "one live ACTION per
  message" is untouched.
- A partial unique `(message_id) WHERE mode='route'` makes it one route per message, forever.
- Old binaries never write `route` and their conflict target is unchanged, so the migration is
  not a breaking change.
- **The shadow-overwrite guard:** capture's `pendingMessages` excludes every message carrying a
  `route` row in EVERY mode, `--all` included. A later shadow pass therefore cannot bury a route
  attribution under an `unmatched` row. Rules still decide first, because the route tier only ever
  sees messages the LIVE rules left unmatched.
- Accepted residual: a rule added after a message was routed does not re-point it.

**B-D6 — Application lives in `internal/capture` (`route.go`), not in classify or promote.**
capture owns `capture_decisions`. The decision is a pure `DecideRoute(msg facts, candidates,
verdict) (project, step, reason)`, tested offline; the driver writes rows directly (capture's own
log, the precedent) and calls no tool. It runs as the first stage of `classify promote`: same
CronJob, same lock `0x5157_0021`, before either promotion lane.

**B-D7 — Shadow → go-live, the SWT-17/30 precedent.**
- Verdicts accumulate in shadow in `ai_extractions` (`classify.Store` gains no write).
- Arming is per account, by hand: `UPDATE source_accounts SET route_after = now() WHERE
  account_email = '…'`. NULL means off, and it is forward-only on the verdict clock for step 3.
  Steps 1-2 apply to any matching message inside the pass window.
- **Go-live gate: an eval against the rules tier's own answers.** Take ≥120 messages on the mailbox
  that RULES attributed (plenty: 526), hide the answer, score agreement per project.
  - This label set is deterministic output, not Salvador's judgement. It is marked `stratum:
    rules` and is a sanity floor. It is biased easy: most of it is ticket-keyed notifications.
  - Every disagreement is read by hand before arming.
  - Salvador's only read is skimming the dry-run list of the real routes for the 115: no
    labelling.
- Under 120 scored, the eval prints counts and the existing marker, never a ratio.

**B-D8 — Locality.** The lane is local-only by construction (router `general=nil`). Unmatched
messages are `ClassRestricted` through `ClassOf`'s non-project branch, as in the residue lane, so
no pin is needed. The prompt carries only the message and the candidate rows, never thread
neighbours; thread inheritance is SQL (step 1).

**B-D9 — The classify store's account join is a named carve-out.** The route inbox must know the
message's source account, reachable only through `raw_source_items.source_account_id`.
`TestClassifyPackage_FetchesNothingAndDecodesNoMIME` is AMENDED:
- `raw_source_items` may appear only inside one named constant that selects `source_account_id`;
- `raw_json` stays banned everywhere.

This is SWT-29's carve-out-by-name pattern. It gives no decoder and no raw read.

**Acceptance criteria — Part B**
- B1. `classify.LaneRoute` (`Name:"route"`, `WorkerType:"classify_route"`, `PromptVersion:
  "route-v1"`) with `RouteContract` (B-D4). The schema has no `confidence`, `url` or `link*`
  field; the existing scans cover it. One prompt for all accounts, bilingual, no sender or client
  literals (the existing scans apply). Guards rewritten per B-D3.
- B2. Route inbox (integration-tested, one fixture per clause, each mutation red):
  - inbound;
  - EXISTS a `mode='live'` decision with `action='unmatched'`;
  - the latest decision is `unmatched`;
  - the source account has ≥1 `source_account_projects` row;
  - no `classify_route` extraction;
  - `--since` required on the message clock (D6 of SWT-33's shape).

  Accounts without candidate rows produce zero rows, and the personal gmail account is the
  control fixture.
- B3. Candidate resolution in Go: `ResolveCandidate(index, candidates)` covers null, 0, out of
  range and a valid index. The grounding check `Grounded(evidence, sender, subject, body)` uses the
  one whitespace-collapse spelling; a unit test proves a paraphrase fails and a verbatim span
  with different spacing passes.
- B4. `capture.DecideRoute` is pure, with a table test for each step:
  - thread with one candidate project → thread;
  - thread with two projects → falls through;
  - single candidate → single;
  - grounded model choice → model;
  - null, out of range or ungrounded with a default → default;
  - the same without a default → unmatched (no row);
  - no verdict → pending.
- B5. Application writes one `capture_decisions` row (`mode='route'`, `action='attributed'`,
  `project_id`, `route_step`, `ai_extraction_id` for model, and a reason naming the step and the
  evidence). It uses `ON CONFLICT (message_id) WHERE mode='route' DO NOTHING`, predicate RESTATED
  (IK partial-index landmine; a structural test scans `internal/` for it). It creates no task and
  calls no executor tool. A run-twice integration test writes nothing the second time.
- B6. After a route row, integration tests show:
  - the inquiry inbox and both promotion inboxes see the message as attributed;
  - the residue and triage inboxes no longer see it;
  - a subsequent shadow `--all` capture pass writes NO row for it.

  Mutation: drop the route exclusion from `pendingMessages` and the last assertion goes red.
- B7. Arming: `source_accounts.route_after` NULL → the application writes nothing, and the stats
  say so. A step-3 verdict recorded before `route_after` is not applied (two fixtures, one on each
  side of the cutover).
- B8. Config tools: `route_candidate_add {account_email, project, description, is_default?}` and
  `route_candidate_remove {account_email, project}`. Both are humanOnly, NOT in
  `internal/mcpserver/schemas.go`, and audited through the executor.
  - `add` refuses: an unknown account; an unknown project; an empty description; a second default
    for an account (partial unique index).
  - `opsctl route-candidates add|remove|list`.
  - The test enumerates the IK's actor shapes against `humanOnly`.
- B9. `classify report --lane route` breaks down by account and by step: would-route (shadow) or
  routed (live), including `pending_verdict` and `ungrounded`. `rulesreport` renders route rows on
  their own "routed by the LLM tier" line, never under a rule id. `/funnel` gets the lane through
  the existing lane loop, and `funnel.go` stays free of worker_type literals.
- B10. `classify eval --lane route --labels docs/evals/route-from-rules.jsonl` scores per-project
  agreement (a multi-class count table) and honours `EvalResultThreshold`. The label file is
  produced by a documented query (latest rule-made attribution on route-armed accounts; label =
  project slug; `stratum:rules`), carries no content, and its loader validates labels against
  `projects.slug`.
- B11. Runbooks gain a "Routing lane" section in `local-classifier.md` covering the tier order,
  candidates, the four steps, the grounding gate, why there is no confidence, arming, the eval gate
  and the shadow-overwrite guard. An IK entry covers the `route` mode (what it may and may not be),
  the pendingMessages exclusion and why a new mode instead of the live index.

---

## Part C — schedule the inquiry lane and promote real asks into tasks

**Usable alone:** once deployed and armed, a Slack DM, or mail attributed by either tier, asking
Salvador something appears on the collaboratory board within about 75 minutes of attribution as a
human task. The task names the asker and the ask and carries `source_thread_id`. A second ask on
that thread logs onto it, and a dismissed task follows SWT-36. An ask answered within the hour
produces nothing. Nothing is sent and no console can claim these tasks. Every promoted task's fate
(worked or dismissed) is readable as a precision label with no labelling ask.

**C-D1 — A second lane inside the existing promoter.** `classify promote` runs route application
(Part B, when present), then the personal lane unchanged, then the inquiry lane, under the same
lock. It reuses the claim table, `threadTask`, the SWT-36 reopen path, the funnel counters and the
existing CronJob. Actor `promote:inquiry`.

**C-D2 — Its own cutover, `projects.inquiry_promote_after`** (NULL = off, no default,
forward-only on the verdict clock). One column per question (SWT-33 D3); reusing
`classify_promote_after` would arm personal-lane promotion on any future `ai_classify` project.

**C-D3 — "Addressed to Salvador" is a deterministic spine rule, not a model field.**
- The rule: addressed ⇔ `channel='gmail'` (his mailbox), OR a 1:1 Slack DM
  (`slackweb.IsDirectMessageKey`), OR (`thread_scope='thread'` AND he posted on that thread
  strictly before the ask).
- Why not a model field: in a channel the model cannot know who the recipient is. Mentions are
  rendered display names, and adding an identity means `inquiry-v2`, a re-eval and a checkpoint
  bump.
- **Accepted recall cost:** a top-level `@Salvador` in a channel thread he is not in does not
  promote. That is the price of dropping `@esteban …`. Gmail's cost is list and CC traffic, which
  `needs_reply` and the review lane absorb.

**C-D4 — `slackweb.IsDirectMessageKey`**: the conversation segment of a slack key starts with `D`,
parsed like `IsRootedThreadKey`, one spelling. Group DMs are excluded on purpose. **It ships only
after §V4a proves it agrees with the leaf's recorded `conversation.type='dm'` on every slack
thread.** If they disagree, stop and carry the type into a normalized column instead.

**C-D5 — `ask_kind` whitelist `{question, request, decision, scheduling}`** (a Go constant). `fyi`
asks nothing, and excluding it also drops SWT-33's three `fyi`+`needs_reply` contradictions.

**C-D6 — Two time bounds on `sent_at`, on top of the verdict cutover.**
- **Grace, 1h:** without it, promotion runs before the replied-since fold can fire.
- **Max age, 72h (`InquiryMaxAge`):** the second fence behind the verdict clock. SWT-30's residual
  is that backfill runs and resumed connectors give old messages fresh verdicts.
- The scheduled inquiry run uses `--since 72h` so the classify window covers the promote window.

**C-D7 — Any replied-since state blocks promotion.** The fold moves to a leaf package,
`internal/replyfold`: the join and column SQL, the prior-participation fragment, the state
constants, the scope rule, and `inquiryState`. Classify and promote read ONE spelling. Tie
semantics are inherited: strictly later, and a tie reads open (SWT-33 note 9). The fold stays
near-inert for gmail, so a gmail ask effectively always passes; the review lane is what absorbs
that.

**C-D8 — Gate first, then the existing `Decide`.** A gated verdict writes no promotion row: `pending`
is not final, and the rest age out of the 72h SQL window. Stats and `--dry-run` print gated counts
by reason. Passing verdicts go through `Decide` unchanged (attach-open → attach+reopen dismissed →
create). Create uses `inquiryCreateStatus`: `holding`/`review` or `ready`/`task`, per **Q2**.

**C-D9 — Task shape.**
- Current attribution's project; `assignee_type='human'`; `priority=0`.
- Title `{asker, else sender}: {ask}` via `textmatch.NormalizedPrefix(…,120)`; fallbacks are the
  subject, then `inquiry: {ask_kind}`.
- Body: a deterministic block with ask_kind, asker, sender, channel, subject, sent_at, the ids,
  thread_id, thread_key, thread_scope, external_message_id (the Slack id, or the gmail Message-ID
  header, `google/normalize.go:86`) and the reason.
- Provenance via `task_set_source_thread` (SWT-20), never `external_refs`.
- **No permalink**: it exists only in `raw_json`. A reply is aimed later from `source_thread_id`.

**C-D10 — Stored/current thread mismatch → gated `rethreaded`** (fail closed, visible).

**C-D11 — #110 gets provenance before arming** (`opsctl call --tool task_set_source_thread`). Its
DM's first verdict then attaches instead of duplicating it.

**C-D12 — Dismissals are the label source for this lane's PIPELINE precision, and the flip gate
for Q2.** This adopts the coordinator's suggestion, as a read-only readout, not an eval.
- **The unit** is a promoted inquiry task: a `classify_promotions` row with action `task` or
  `review` and a `task_id`, whose extraction is `worker_type='classify_inquiry'`.
- **Its outcome** is a pure function, `promote.InquiryOutcome(status, firstDismissal)`:

  | outcome | condition |
  |---|---|
  | **false positive** | the task's FIRST dismissal is `not_actionable` or `wrong_kind` |
  | **true positive** | closed with no dismissal, `delivered`, or first dismissal `handled_elsewhere` (a real ask, answered elsewhere) |
  | **mis-click, not a label** | first dismissal later plain-reopened by a HUMAN actor (`reopened_by_message_id` NULL, `reopened_by` a human actor, decided by `policy.HumanActor`: never restate the prefixes); SWT-36 D6's table |
  | **excluded, counted apart** | `duplicate` (a dedup signal, not a precision label); still open (undecided); `attached` promotions (no task of their own) |

  An activity-reopen does NOT undo the label. The first dismissal was his judgement at the time
  (SWT-36 D6: "overtaken, not wrong").
- **What it measures, and what it cannot.** It measures the whole pipeline's precision: attribution
  + model + gate, on asks that became tasks. It can never measure recall, because an ask that was
  never promoted leaves no task to dismiss. Runbook criterion 32 still holds: a set built only from
  flagged output measures precision, never recall.
  - So this readout does NOT replace the committed model eval
    (`docs/evals/inquiry-needs-reply.jsonl`, uniform + enriched).
  - No dismissal row enters that file in this ticket. `classify eval` stays untouched and still
    writes no store rows (board-dismissals_SPEC's caution holds because nothing changes it).
- **Precedence against the review lane:** they are not competing. Q2's holding column is WHERE the
  review happens; dismissals are WHAT it records. Salvador flips `inquiryCreateStatus` to `ready`
  by reading this readout. The ticket contains no labelling ask of any kind.
- **Printing:** counts always. A precision ratio only once decided outcomes ≥
  `classify.EvalResultThreshold`; below it, `classify.EvalIndicativeMarker`. The fold lives in
  `internal/promote` (it owns its log's folds, the `CountersByLane` precedent). The threshold and
  marker are applied in `cmd/classify`, which may import both packages, so there is one spelling of
  each and promote still cannot reach provider.

**Acceptance criteria — Part C**
- C1. `classify promote` keeps personal-lane decisions, rows and calls byte-identical (the existing
  suites stay untouched and green). Its early "no cutover" return fires only when every cutover
  column is NULL, and names them all.
- C2. The inquiry inbox, one query:
  - `worker_type='classify_inquiry'`, ok;
  - `needs_reply='true'`;
  - `nm.direction='inbound'`;
  - latest decision `action='attributed'` (**any mode, including `route`**, asserted by a
    fixture);
  - project has `ai_inquiry` AND `inquiry_promote_after IS NOT NULL` AND
    `r.created_at >= inquiry_promote_after`;
  - `nm.sent_at >= now() - $InquiryMaxAge`;
  - no promotion row;
  - oldest first.

  One fixture per clause, each mutation red. Cross-lane controls both ways.
- C3. The pure `promote.InquiryGate(c, now) (ok, reason)` lives in the offline `promote_test.go`.
  Reasons in order: `rethreaded`, `kind`, `stale` (NULL `sent_at` counts as stale), `pending`,
  `answered`, `not_addressed`. There is a table test.
- C4. Addressed integration tests, seeded in Postgres:
  - a DM passes;
  - a top-level channel message → `not_addressed`;
  - a rooted thread with his earlier post passes;
  - the same thread with his post only after → `answered`;
  - a group DM → `not_addressed`;
  - gmail passes.

  Mutations: drop `direction='outbound'`; make the comparison non-strict.
- C5. `IsDirectMessageKey` unit tests: DM, rooted DM thread, channel, `G…`, non-slack. The
  `slackweb/threadscope_test.go` key-spelling scan extends to `internal/promote` and
  `internal/replyfold`.
- C6. `internal/replyfold` imports only stdlib and `internal/connector/slackweb`.
  `classify.Summarize` output is byte-identical (the golden report and summary/inquiry integration
  suites stay unchanged). Classify keeps its exported names as aliases.
  `TestClassifySummary_OwnsTheQueriesAndTheFolds` is AMENDED so its by-name carve-out follows the
  constants.
- C7. For the inquiry lane, `Decide`'s branches each have an integration test: attach+log; attach
  + guarded `task_reopen` as `promote:inquiry`; Q3 new task; create.
- C8. Create order: `create_task {project, title, body, assignee_type:"human", priority:0,
  status}` → `recordTask` → `task_set_source_thread`, all through `executor.Execute` as
  `promote:inquiry`. `audit_events` rows are asserted. The title and body have an exact-text unit
  test.
- C9. Claim-before-act, and run-twice writes nothing. A message already promoted by the personal
  lane counts as `Lost`: the total unique index gives one promotion per message across lanes.
- C10. Gated → no row, no tool call. A `pending` verdict promotes after its grace elapses.
- C11. Stats `inquiry` block with `gated` by reason. `--dry-run` prints per candidate and writes
  nothing. `--max-age <dur>` is **refused unless `--dry-run`**; it exists for reading the gate
  against history (§V5).
- C12. The existing direct-write, provider-reachability and key-spelling scans hold. A new
  `TestMigration0027_*` guard, and the classify ledger admits 27 (and 28 with Part B).
  `CountersByLane` returns a `classify_inquiry` row (integration).
- C13. `docs/runbooks/HANDOFF-kube-inquiry-promote.md` covers the CronJobs, scheduling and cost
  below, and states migration-before-image. The runbook's inquiry sections are updated. An IK
  "Inquiry promotion" entry restates that `classify eval` writes no `ai_runs` (the only thing
  keeping an eval out of either promoter's inbox) and that `--max-age` is dry-run-only.
- C14. **Dismissal readout (C-D12).**
  - `promote.InquiryOutcome` has a pure table test covering every row of C-D12's table, including
    an activity-reopened `not_actionable` (still a false positive), a human plain-reopen
    (mis-click), `handled_elsewhere` (true positive) and `duplicate` (excluded).
  - `promote.InquiryOutcomes(ctx, pool, since)` is the one SQL fold. It reads only
    `classify_promotions`, `ai_extractions`, `ai_runs`, `tasks` and `task_dismissals` (the FIRST
    row per task by `created_at, id`), and writes nothing. An integration test seeds each outcome
    through the real verbs (`task_dismiss`, `task_close`, guarded and plain `task_reopen`) and
    asserts the counts. Mutation: read the LATEST dismissal instead of the first, and the
    activity-reopen case goes red.
  - `classify promote --outcomes [--since <dur>]` prints the counts and applies
    `EvalResultThreshold`/`EvalIndicativeMarker` exactly as the eval does. A unit test shows no
    `\d\.\d\d` below 120 decided and a ratio at or above it.
  - The runbook documents the readout as the Q2 flip input.

---

## Data model changes

**`migrations/0027_inquiry_promotion.sql` (Part C).**
```sql
ALTER TABLE projects ADD COLUMN inquiry_promote_after TIMESTAMPTZ;  -- NULL = off; no default, no backfill
```

**`migrations/0028_route_tier.sql` (Part B).** Confirm the inline constraint names against prod
first, as 0015 did; the mode CHECK is `capture_decisions_mode_check`. It runs in one transaction.
```sql
ALTER TABLE capture_decisions DROP CONSTRAINT capture_decisions_mode_check;
ALTER TABLE capture_decisions ADD CONSTRAINT capture_decisions_mode_check
  CHECK (mode IN ('shadow','live','route'));
ALTER TABLE capture_decisions
  ADD COLUMN route_step TEXT CHECK (route_step IN ('thread','single','model','default')),
  ADD COLUMN ai_extraction_id BIGINT REFERENCES ai_extractions(id),
  ADD CONSTRAINT capture_decisions_route_shape CHECK (
    (mode = 'route') = (route_step IS NOT NULL)
    AND (mode <> 'route' OR (action = 'attributed' AND matched_rule_id IS NULL AND task_id IS NULL))
    AND ((route_step = 'model') = (ai_extraction_id IS NOT NULL)));
-- One route per message, forever. PARTIAL: every ON CONFLICT must restate WHERE mode = 'route'.
CREATE UNIQUE INDEX capture_decisions_route_uniq ON capture_decisions (message_id) WHERE mode = 'route';

CREATE TABLE source_account_projects (           -- configuration, not work (capture_rules precedent)
  id                BIGSERIAL PRIMARY KEY,
  source_account_id BIGINT NOT NULL REFERENCES source_accounts(id),
  project_id        BIGINT NOT NULL REFERENCES projects(id),
  is_default        BOOLEAN NOT NULL DEFAULT false,
  description       TEXT NOT NULL,              -- what the model is told this project is, from this account
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (source_account_id, project_id)
);
CREATE UNIQUE INDEX source_account_projects_default_uniq
  ON source_account_projects (source_account_id) WHERE is_default;

ALTER TABLE source_accounts ADD COLUMN route_after TIMESTAMPTZ;  -- NULL = routing off for this account
```
- No seeding. The candidate rows go in through the tool, as capture_rules does (0015's recorded
  reason).
- The `(action='unmatched') = (project_id IS NULL)` CHECK holds for route rows automatically.
- Part A needs no migration.
- C-D12 reads existing tables only.
- Numbering: `capture-rule-ticket-keys` may claim a number first. Take the next free number at
  implementation time.

## API / MCP tool changes

- **New executor tools (Part B):** `route_candidate_add`, `route_candidate_remove`. Both humanOnly,
  off MCP, audited. opsctl gets `route-candidates add|remove|list`.
- **Existing tools called, unchanged:**
  - `create_task` (`status ready|holding`), `task_append_log`, `task_set_source_thread` and guarded
    `task_reopen`, as `promote:inquiry` (Part C);
  - `capture_rule_add`, via opsctl (Part A);
  - `task_set_source_thread`, via `opsctl call` (#110).
- **No executor call** from route application (capture's own log only) or from the outcome
  readout (read-only).
- **CLI:**
  - `classify run --lane route --since <dur>`;
  - `classify report|eval --lane route`;
  - `classify promote [--dry-run] [--limit N] [--max-age <dur>, dry-run only] [--outcomes
    [--since <dur>]]`.

## MQTT topics

None.

## Scheduling and cost (kube handoff; distroless means one lane per CronJob)

| CronJob | schedule | command | lock |
|---|---|---|---|
| classify-personal (existing) | `*/30 0-2,4-23` | unchanged | 0x5157_0022 |
| **classify-inquiry** (new, C) | `10,40 0-2,4-23 * * *` | `classify run --lane inquiry --since 72h --limit 100` | 0x5157_0022 |
| **classify-route** (new, B) | `50 0-2,4-23 * * *` | `classify run --lane route --since 72h --limit 100` | 0x5157_0022 |
| classify-residue (existing) | `5 3 * * *` | unchanged | 0x5157_0022 |
| classify-promote (existing) | `20 * * * *` | `classify promote`: image bump only | 0x5157_0021 |

- A lost shared lock is the documented benign exit, and overlap is free (NOT EXISTS dedup).
- `--since` must stay ≥ `InquiryMaxAge`.
- GPU: inquiry is about 34 msgs/day plus routed mail; route is the handsonconnect backlog (about
  115, run once by hand with `--since 720h`) plus a few per day. Both are minutes per day at
  4.5-10 s per verdict, well inside the 90 W budget.
- Lag, message to task: rules-attributed about 75 min; model-routed about 2-3 h.

## Files likely to touch

- **Part A:** no Go code. `docs/runbooks/capture-rules.md`.
- **Part B:**
  - migration 0028;
  - `internal/classify/route.go` (prompt, contract, `ResolveCandidate`, `Grounded`), `lane.go`,
    `store.go` (route inbox, the account-join carve-out), `eval.go` (multi-class count table),
    `summary.go`/`report.go` (the route block), `lane_test.go` and `structure_test.go` (rewritten
    guards);
  - `internal/capture/route.go` (`DecideRoute`, the driver), `rules_store.go` (the route exclusion
    in `pendingMessages`), `rulesreport.go`;
  - `internal/tools/routecandidates.go` plus `internal/policy` (`humanOnly` entries);
  - `cmd/opsctl/main.go`, `cmd/classify/main.go`;
  - `docs/evals/route-from-rules.jsonl`.
- **Part C:**
  - migration 0027;
  - `internal/replyfold/*`;
  - `internal/promote/inquiry.go` (gate, inbox, title/body, constants), `outcomes.go`
    (`InquiryOutcome`, `InquiryOutcomes`), `store.go` (`Run` stages, stats, actors),
    `promote_test.go`, `inquiry_integration_test.go`, `structure_test.go`;
  - `cmd/classify/main.go` (`--outcomes`, `--max-age`);
  - `internal/classify/summary.go`, `inquiry.go`, `summary_structure_test.go`;
  - `internal/connector/slackweb/normalize.go`, `threadscope_test.go`;
  - `docs/runbooks/HANDOFF-kube-inquiry-promote.md`, `local-classifier.md`;
  - `.claude/INSTITUTIONAL_KNOWLEDGE.md`.

## In scope / Out of scope

**In scope:** Parts A, B and C as written.

**Out of scope (do not bundle):**
- `slackweb-collab-export-stale` (Miss B).
- Rule 10 / Treetop ticket keys and any rule-edit or enable/disable subcommands (owned by
  `capture-rule-ticket-keys`).
- Open-set routing over accounts without candidate rows, and routing for Slack workspaces (their
  catch-alls leave nothing unmatched today).
- A live re-decide path for task-creating rules on already-decided messages.
- A `source_account` rule criterion. The candidate table expresses "this mailbox is collaboratory
  unless reengine" for the tier that can tell the two apart; a rules-tier catch-all would starve it
  (B-D1).
- Feeding dismissals into `classify eval` or the committed labelled set.
- Drafting or sending replies.
- Prompt changes to the inquiry lane.
- Auto-closing a promoted task on a later reply.
- The `a-millon`/HOC channel attribution.
- Raising the inquiry labelled set to 120.
- A dashboard holding→ready verb.
- An index on `normalized_messages.thread_id`.

## Invariants that apply

1. **Raw-first:** nothing ingests. The only raw touch is B-D9's named `source_account_id` join
   (no `raw_json`), plus §V4a's one-off measurement.
2. **One funnel:**
   - Tasks stay in `tasks`, and the review lane is a filter.
   - `source_account_projects` is configuration (no status, assignee or claim).
   - A route decision is a `capture_decisions` row, not a side table.
   - `classify_promotions` stays a log.
   - The outcome readout is a fold, not a table.
3. **Everything through the executor:** candidate config goes through the new humanOnly tools,
   rules through `capture_rule_add`, and task writes through the four spine tools. The only direct
   writes are capture's own log (route rows) and `classify_promotions`, and the existing
   direct-write scans still apply.
4. **Nothing external without a delivery row:** nothing sends.
5. **Own-message loop closure:** every inbox is inbound, and route application requires a live
   `unmatched` decision, which an outbound message can never have. Outbound rows serve only as
   evidence (replied-since, prior participation, thread inheritance never counts them: it reads
   decisions).
6. **Stealth attribution:** internal board text only.
7. **Orchestrator purity:** untouched. `DecideRoute`, `InquiryGate`, `Decide` and
   `InquiryOutcome` are pure and tested offline. Every applied decision leaves a typed row (route
   row, promotion row) plus executor audit rows. Gated and pending cases are reported in stats and
   dry-run output.

## Sibling patterns to copy

- The lane shape: `classify/lane.go` + `inquiry.go`. The index-not-a-string resolution:
  `classify.ResolveLink`.
- Promotion driver: `promote/store.go`. The fold seam: `promote.CountersByLane`. The pure gate
  beside a pure decide: `promote.Decide`, `orchestrator/rules_test.go`.
- HumanOnly config tools off MCP: `capture_rule_add` (`tools/capturerules.go`) + its policy
  entry.
- CHECK widening by drop/add: 0015 step 3. A partial-index `ON CONFLICT` with the predicate
  restated: `capture.insertDecision`.
- The Go-regexp corpus check: `capture-rule-ticket-keys_SPEC.md` "Already shipped".
- Kube handoff: `docs/runbooks/HANDOFF-kube-swt18.md`.
- Queue claims (`FOR UPDATE SKIP LOCKED`): not applicable. Every pass is advisory-lock
  single-instance.

## Verification protocol

- **V1.** `go test ./...`.
- **V2.** `make integration` (`-p 1`, `itest-` fixtures; FK-ordered cleanup including
  `policy_decisions`/`audit_events` by task and by actor `promote:%`). Every thread fixture seeds
  both directions; dismissals go through `task_dismiss`.
- **V3. Mutations** (apply, see red, revert, record):
  - Part C: each inquiry inbox clause; the prior-participation strictness and direction;
    `IsDirectMessageKey` accepting `G…`; first-versus-latest dismissal in the outcome fold.
  - Part B: the route inbox's live-unmatched clause; the `pendingMessages` route exclusion; the
    grounding check (accepting a paraphrase); the route `ON CONFLICT` predicate.
- **V4. Prod read-only, before implementation** (`BEGIN READ ONLY … ROLLBACK`; no count is frozen
  in a test):
  - (a) Export `(thread_key, raw_json->'conversation'->>'type')` for every slack thread and run
    `IsDirectMessageKey` over it in Go. Required: zero disagreements with `dm`, and ≥1 DM per
    workspace.
  - (b) The 16 flagged inquiry verdicts of 2026-09-10: channel, scope, thread_key, ask_kind.
  - (c) Slack export freshness per workspace (the Miss B dependency).
  - (d) The handsonconnect mailbox's latest-decision breakdown and top senders. This re-measures
    the 115 and sizes the route backlog.
  - (e) `C1C1TSLJH`: the channel name (`normalized_threads.subject`), top senders, attribution,
    and that no other key starts with the prefix.
  - (f) `opsctl capture-rules list`: priorities of 8, 9, 10, 59, the bulk rules and the jira
    rules.
  - (g) `schema_migrations` max against `ls migrations/`.
  - (h) Part A's regexp export (A1).
  - (i) `opsctl capture-rules report --domain cecollaboratory.com` (A-D4's blocker).
- **V5. Dry-runs before any arming.**
  - Part C: set `inquiry_promote_after='2026-09-01'` while the old image is still deployed (it never
    reads the column), run `classify promote --dry-run --max-age 720h`, and read it against V4b.
    Expected: the Katie and asunda45 asks pass or fail only `answered`/`pending`; `@esteban`,
    "this is the ticket" and `@channel` fail `not_addressed`/`kind`. A genuine ask failing
    `not_addressed` is C-D3's cost made visible and goes into review. Reset to NULL afterwards.
  - Part B: `classify run --lane route --since 720h`, `classify eval --lane route`, and
    `classify promote --dry-run` with `route_after` set in the same safe window. Salvador skims the
    route list for the 115.
- **V6. Go-live, in order:**
  1. **Part A:** add the rules (A2), then run the §A-D6 pass, then do A3's check.
  2. **Part C:**
     1. apply 0027, then build the image, then do the handoff;
     2. run `classify run --lane inquiry --since 336h` once by hand, then
        `classify report --lane inquiry --since 336h`;
     3. set #110's provenance, then arm;
     4. after the `:20` tick, check `/tasks?project=collaboratory&status=<Q2>`, the
        `classify_inquiry` row on `/funnel`, `deliveries` unchanged and
        `classify promote --outcomes` running and printing zero decided.
  3. **Part B:** apply 0028 → image → handoff (classify-route) → `route-candidates add` for the
     two handsonconnect rows → the shadow period with the V5 reads → arm `route_after` → one
     `classify run --lane route --since 720h` backfill. The next promote tick applies the routes.
     Confirm the 115's latest decisions.

  Historical asks older than 72h never become tasks. They show as `stale` in a
  `--dry-run --max-age 720h` pass. Salvador hand-files the Rochester thread if it is still open.
- **V7.** `/ticket-review inquiry-promote` (go-reviewer plus the adversarial pass: this diff lets
  model output create tasks and change attribution).

## Coordination with in-flight work

- **`capture-rule-ticket-keys`** (untracked in the main checkout). This ticket does not touch rule
  10, key derivation or enable/disable.
  - Shared ground: migration numbering and the ledger; `rules_store.go`, if their Q2(iii) lands;
    rule priorities (A-D2 and A-D5 sit below their mention rule at either priority).
  - Run §A-D6's shadow `--all` after both sets of rule changes, or run it twice. Their own
    verification runs one too, and that is safe for Part B rows because of B-D5's exclusion.
- **`slackweb-collab-export-stale`** blocks Miss B only.

## Future work

- `classify eval --labels-from-dismissals`, feeding promoted-and-dismissed messages into the
  inquiry eval as the ENRICHED stratum only. It can never supply the uniform half. It needs
  board-dismissals_SPEC's eval-persistence re-check first.
- Open-set routing (accounts without candidate rows), with its own labelled set.
- A live re-decide path so a task-creating rule can act on already-decided messages.
- A model-side "addressed" signal if C-D3's recall cost shows up: the recipient's per-workspace
  display name in the prompt as `inquiry-v2`, or mention ids once the leaf exports them.
- Auto-close or log-on-reply for promoted tasks.
- Slack reply drafting from `source_thread_id`.
- `a-millon`/HOC rules.
- The `normalized_messages.thread_id` index.
- Outcome readouts per promotion lane and per route step on `/funnel` (counts, never a ratio below
  120). A `wrong_project` dismissal code, to label Part B's misroutes.
- Route correction: re-pointing a routed message when a later rule disagrees.
