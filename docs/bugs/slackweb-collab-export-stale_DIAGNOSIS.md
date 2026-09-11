> Jira: SWT-39

# Diagnosis — slackweb-collab-export-stale (the recurring lateness)

Scope, set by Salvador: "diagnose, the recurring one". This covers 542's lateness in
general. The overnight freeze (runs 33807–34656) is treated as one instance of it.

## Root cause

**The export only reads the conversations the leaf can scrape from the Slack UI
during each run.** For Collaboratory that scrape returns 6 to 8 of the 38
conversations switchboard knows about. Which ones it returns does not follow message
activity. Neither repo notices the shortfall.

- **How the set is chosen (leaf).** `exportSlackForSwitchboard`
  (`slackconnector/src/switchboard/export.ts:150-152`) reads exactly what
  `SlackWebAdapter.listChannels` returns. That call runs `listConversationsOnPage`
  (`src/slack/slack-web-adapter.ts:1029-1126`), which scrapes two places:
  - the Home sidebar;
  - the `/dms` rail view (`collectDmConversations`, `:974-1027`, added by `ecabc02`,
    2026-07-30).
- **Why Collaboratory depends on the DMs view.** Collaboratory's Home sidebar holds
  one starred channel and no DMs (`ecabc02`'s commit message). So nearly all of its
  coverage comes from what the DMs view's virtual list happens to render.
- **No other way in.**
  - Switchboard posts `/export` with no body (`internal/connector/slackweb/http_bridge.go:123`).
  - The leaf's `/export` takes no input (`src/switchboard/http-bridge.ts:166`,
    `src/cli/bridge-server.ts:88-115`).
  - There is no cursor, no list of known conversations, and no unread or Activity
    signal.
- **Nothing flags the gap.**
  - The export carries no enumerated or expected count.
  - An enumerated conversation whose read fails only produces a `warn` in the mini's
    log (`export.ts:175-178`, `bridge-server.ts:106-111`).
  - `Ingest` (`internal/connector/slackweb/ingest.go:58-85`) counts whatever subset
    arrives and calls `FinishRun(..., "ok", ...)`.

The result: a message lands only when its conversation happens to be in a run's
scraped set. That is either the one or two variable slots of a narrow run, or a rare
wide run. This is "late, not lost", and it is why late batches arrive with wide runs.

## Evidence

### Proven by prod data (read-only, 2026-09-11)

**Coverage is 6–8 of 38.**
- `raw_source_items` holds 38 `conversation:*` rows for 542.
- Since 2026-09-09, 88 of 90 runs of 542 saw 6, 7 or 8 conversations.
- The two exceptions are run 32234 (34 conversations) and run 32615 (33).
- Avviato (539) has the same shape: 45 known conversations, 16 per run on 61 runs, 13–17
  on 26 runs, and 42–43 on 3 runs (32398, 32431, 33957).

**Most active conversations only ever land in wide runs.**
- Since 2026-09-09, 20 conversations of 542 had any message first-seen.
- 16 of them had messages first-seen only in wide runs 32234 or 32615. Katie
  (`D04F7LXRB8B`) is one: 593 stored messages, newest 2026-09-09 17:33Z, first seen in
  32234.
- Only four conversations ever received anything in a narrow run: `D023E7XSSGG` (Jira),
  `D0AUD86LKGA` (asunda45), `D0B6FV6HFSR` (byeluri) and `C03J2KTN1PD`
  (rd-asu-collaboratory).

**Wide runs recover messages that hundreds of `ok` runs never saw, in both workspaces.**
- Run 32615 (542) inserted 11 messages, sent 2023-05 to 2026-07. Each had been missed by
  347 `ok` runs.
- Runs 32431 and 33957 (539) inserted messages from 2021–2023, missed by 344 and 395 runs.
- Run 32234 (542) inserted 1,876 messages, 40 of them sent since 2026-09-03.
- So Avviato's "late by at most 1 run" in the repro only holds for conversations inside
  its standing set. Its active ones (`D01EJRX6P45`, `DSAV4HJ2F`, `C1C1TSLJH`) all landed
  in 16–17-conversation runs.
- The repro metric cannot see a message that was never ingested.

**The narrow set has a fixed core of 6 conversations plus 0–2 variable slots.**
Message counts add up exactly:
- 6-conversation runs return 47–50 messages.
- Run 34684 returned 365 = 47 + 118 (byeluri, all stored messages) + 200 (asunda45,
  capped by `SLACK_CONNECTOR_SYNC_CHANNEL_LIMIT` default 200, `bridge-server.ts:91-95`).
- Every frozen run returned 166 = 47 + 119, and run 33779 rewrote exactly 119 byeluri
  rows. **So the freeze's 7th conversation was byeluri, and asunda45 was simply not in
  the set.**
- The core always includes `C03MYC6VAJ1` and `C045FRJTYNN`. These are the two
  oldest-activity conversations of the 38: newest messages 2022-12-02 and 2022-10-06.

**The variable slot does not follow activity.** Rows last written per run on
2026-09-10 and 11 show who held it:

| run(s) | time (Z) | extra conversation(s) |
|---|---|---|
| 33394, 33422 | 16:00, 16:30 | Jira |
| 33459 | 17:00 | asunda45 (200 rows) |
| 33488 | 17:30 | Jira + asunda45 |
| 33546–33604 | 18:30–19:30 | none |
| 33633 | 20:00 | rd-asu-collaboratory (inserted two messages sent 00:01Z, ~20h late) |
| 33663 | 20:30 | Jira |
| — | 20:49 | *the asunda45 message is sent* |
| 33691 | 21:00 | none |
| 33721, 33749 | 21:30, 22:00 | rd-asu-collaboratory (newest message 00:04Z) |
| 33779–34656 | 22:30–13:30 | byeluri (31 runs) |
| 34684 | 14:00 | byeluri + asunda45 |

The table also dates the start of the delay: asunda45 left the set before its 20:49Z
message and did not return for 17 hours. That fits the repro's note that the message
was missed before the stats froze.

**Timing tracks coverage.** Narrow runs take 5m08s–6m50s for the whole two-workspace
export. Wide runs take 12m43s–16m07s. That measures the cost of full coverage today.

### Proven by code

**Leaf** (`~/projects/personal/slackconnector` @ `c938526`):
- `export.ts:150-152` iterates only `listChannels`' result. `:175-178`: a read failure
  is `onConversationError` plus `continue`, so the conversation silently leaves the
  output.
- `slack-web-adapter.ts:1106`: the DMs-view rows are unioned into enumeration;
  `:1112` applies policy. `ALLOWED_CHANNELS` is unset and `ALLOW_DMS=true` (launchd
  plist), so policy removes nothing.
- `collectDmConversations` (`:974-1027`):
  - `:986` takes the scroller from `firstVisible(page, slackSelectors.dmList)`.
    `firstVisible` (`:1938-1954`) returns the first visible match in selector order, and
    the last `dmList` fallback is the generic `[data-qa="slack_kit_scrollbar"]`
    (`selectors.ts:99-103`).
  - `:989-1014` extracts from the list's current position and only ever scrolls down
    (`scrollTop +=`, `:1002`). It stops after 3 unchanged positions and never scrolls to
    the top or restores the position.
  - Contrast the sidebar pass (`:1068`, `:1096-1104`), which records and restores its
    position. Contrast also the Activity collector, fixed for exactly this "start from an
    inherited scroll window" shape (`docs/BUG-read-back-staleness.md`,
    `scrollElementToTop` in `src/slack/scrolling.ts:37-42`, used at
    `slack-web-adapter.ts:1586`).
- The leaf's own `docs/HANDOFF.md:306-312` lists this as **open bug 1, "Enumeration is
  partial"**. It reads: "~13 of roughly thirty Collaboratory conversations", "scrollTop
  pinned at 1603 while the same 12 rows re-extracted every pass, so the container being
  scrolled is not the one that reveals more rows", and "exports read from enumeration,
  so coverage is likely incomplete".
- The mini logs at `SLACK_CONNECTOR_LOG_LEVEL=info` (launchd plist). The only
  enumeration lines, `Enumerated conversations from the DMs view` (`:1016`) and
  `Discovered Slack conversations` (`:1113-1124`), are `debug`, so the mini log has no
  record of enumeration. Even at debug they carry counts only: no IDs, no scroll
  position.

**Switchboard:**
- `http_bridge.go:122-135`: `Export` posts a nil body and trusts the result.
- `ingest.go:58-85`: `ConversationsSeen` counts returned conversations; the run is `ok`
  whatever the count.
- `types.go:14-34`: the export schema has no coverage fields.

### git blame
- `slack-web-adapter.ts:974-1027`: 51 of the 54 lines come from `ecabc02` (2026-07-30),
  3 from `c938526` (2026-09-09).
- The partial enumeration was known at `ecabc02` ("Incomplete: enumeration is stable but
  partial") and was never fixed. This is not a recent regression. `c938526` changed
  reads to reuse the enumerated conversation instead of re-resolving it. That stops
  losing enumerated conversations as `CHANNEL_NOT_FOUND`, but it does not widen what is
  enumerated.

## Why the reproduction fails

The repro counts `ok` runs that started after a message was sent and finished before it
was first normalized. Every such run exported a set that did not contain the message's
conversation:
- **raw 77849** (asunda45 20:49Z): absent from all 34 runs, 21:00Z to 13:30Z (table
  above). It landed when asunda45 re-entered the set in run 34684.
- **The 13 D0AUD86LKGA messages from 2026-09-08**: they waited for wide run 32234.
- **raw 77722** (C03J2KTN1PD, sent 00:01Z 2026-09-10): it waited for that channel's
  variable slot at 20:00Z, 39 runs later.

The freeze's constant stats have the same cause:
- The set held still: the core plus byeluri.
- None of those 7 had new messages, so `raw_inserted=0`.
- `raw_updated` 25–27 is per-run rewriting of `C03MYC6VAJ1` (22 rows) and `C045FRJTYNN`
  (3 rows) plus their conversation rows. That churn is separate (see Out of scope).

## Invariant implicated

**None of the seven is violated by the code path.** Raw-first holds for what arrives,
and nothing bypasses the executor or delivery rows. Two invariant-adjacent consequences
shape the fix:
- **Invariant 5 (own-message loop closure) depends on export coverage.** A send into a
  conversation outside the scraped set is never re-ingested, so it is never confirmed.
  `ReconcileUnconfirmed` (`internal/connector/slackweb/reconcile.go:108-122`) counts
  every `ok` run of the workspace as a pass that "could have observed" the send. That is
  false for an unscraped conversation. A reply sent to asunda45 during the freeze would
  have been flagged `delivery_unconfirmed` after 3 passes although it had landed.
- **The leaf's own rule is broken at the coverage level.** It says "a partial export
  must not look like a full one" (`bridge-server.ts:107`, `export.ts:66-70`). Missing
  coverage produces no signal at all; a failed read produces a mini-only `warn`.

The fix should restore that signal (restore the gate), not just widen one scrape.

## Proposed fix scope

**Primary repo: `~/projects/personal/slackconnector` (the leaf).** Secondary:
switchboard `internal/connector/slackweb`. Nothing is implemented. Order: A first,
because it is cheap and settles the open mechanism; then B; D alongside B; C needs
Salvador's decision.

**Leaf (slackconnector):**
- [ ] **A. Coverage telemetry.**
  - At `info`, per workspace: sidebar row count, DMs-view row count, which `dmList`
    selector matched, scroller `scrollTop/scrollHeight/clientHeight` at start and end,
    and the enumerated conversation IDs (`slack-web-adapter.ts:974-1126`).
  - Add optional per-workspace export fields `enumerated_conversation_ids[]` and
    `unreadable_conversations[{id, reason}]` (`export.ts` `SwitchboardExportWorkspace`,
    `:48-54`, and the `:175-178` catch).
  - These are additive. switchboard's plain `encoding/json` ignores unknown fields; no
    `DisallowUnknownFields` appears in `internal/connector/slackweb`.
- [ ] **B. Enumerate the DMs view from its top.** In `collectDmConversations`:
  - reset the real DM-list scroller to `scrollTop = 0` before the first extraction (the
    `scrollElementToTop` pattern);
  - confirm the scroller actually contains the `dmConversationLinks` rows rather than
    taking the generic `slack_kit_scrollbar` fallback blindly;
  - stop only when both the position and the extracted-ID set stop growing;
  - restore the position afterwards, like the sidebar pass does.
  - Unit test: a DOM fixture of a DMs list parked at `scrollTop > 0` must yield its top
    rows. This mirrors the Activity case in `tests/unit/scrolling.test.ts`.
- [ ] **C. (Decision for Salvador) Stop depending on enumeration alone.**
  - Option C1: union the scraped set with conversations switchboard already knows.
    Switchboard would send known conversation URLs in the `/export` body; the leaf would
    read them by URL through the existing pre-resolved path
    (`acceptResolvedConversation`, `slack-web-adapter.ts:1192-1210`). That path reads by
    URL, so it does not depend on the DMs view.
  - Option C2: use the Activity/Unreads view as the change signal.
  - Cost bound: a full two-workspace read ran 12–16 min on 2026-09-09 to 11 against a
    `*/30` schedule. Reading all 38 + 45 every run needs a budget, for example ordering
    by last activity plus a rotation.

**Switchboard (this repo):**
- [ ] **D. Record coverage and stop calling partial runs `ok`.**
  - `Ingest` (`ingest.go:58-85`) stores the leaf's enumerated and unreadable fields in
    `sync_runs.stats`.
  - It stores the list of conversation IDs actually exported.
  - It finishes the run as not-`ok` when conversations were unreadable (the exact status
    value is for the spec).
  - It flags a run whose exported set falls far below the account's known conversation
    count.
- [ ] **E. Count only runs that exported the target conversation.**
  `ReconcileUnconfirmed` (`reconcile.go:108-122`) should only count passes whose export
  included the delivery's target conversation. This needs D's per-run IDs.
- [ ] **F. Only if C1 is chosen.** `HTTPBridge.Export` (`http_bridge.go:122-123`) and the
  command bridge send the known-conversation list.
- [ ] **Regression tests** (test-author converts the repro):
  - Re-run `slackweb-collab-export-stale_repro.sql -v since=<deploy time>` once 542 has
    new messages.
  - Add the leaf unit test from B.
  - Add a Go ingest test: an export with unreadable conversations does not finish `ok`.
  - Per the "test the column" memory, add an integration test reading the stored stats.

## Out of scope for this fix

- **Per-run rewrite churn.** `C03MYC6VAJ1` (22 message rows) and `C045FRJTYNN` (3) plus
  their conversation rows change hash on every run. That explains the constant
  `raw_updated` 25–27, and it resets `normalized_at` on every run. Cause not examined.
- **Group DMs typed `public_channel`.** Rows from the DMs view with `C…` keys come back as
  `public_channel`, so `allowDms` does not govern them (leaf HANDOFF open bug 2). This is
  visible in 542's inventory: "alfredo, esteban, Katie" and similar are typed
  `public_channel`.
- **Hidden thread-reply loss.** `skipped_message_count` 61 on `C03J2KTN1PD`, and HANDOFF
  bugs 4–5 (dead thread-failure config, misleading exclusion log).
- **The `HTTPBridge.Send` 503 misclassification.** Already recorded in memory and in the
  HANDOFF.

## Open questions

These are listed, not guessed. The cause above stands without them. They decide the
mechanism inside the scrape, and so the exact shape of fix B.

1. **Was asunda45 not enumerated during the freeze, or enumerated but unreadable?**
   - The DB cannot tell: `conversations_seen` counts only returned conversations.
   - **Log:** `/Users/salvadorspataro/Library/Logs/slack-bridge-server.log` on the mini.
     Look for `Conversation enumerated but unreadable; excluded from this export` lines
     whose `conversation` field starts with `asunda45` and has no ` thread ` suffix
     (HANDOFF bug 5: thread failures reuse that message), from 2026-09-10T20:49Z to
     2026-09-11T13:40Z.
   - Zero lines means it was never enumerated, and fix B is right. Lines mean the read
     path failed, and the fix moves to `readChannel` for that DM.
   - The weak DB hint: frozen runs took 5m17s–7m51s, overlapping the 6-conversation
     runs. That does not rule out a fast failure.
2. **Why does the DMs view render this particular set?**
   - The hypothesis: it opens parked at an inherited scroll position (the core is the two
     oldest-activity conversations), and the downward-only loop, possibly on the wrong
     scroller, never reaches the top.
   - It matches the code and HANDOFF's "scrollTop pinned at 1603" telemetry, but it is
     unproven for this incident.
   - It is undecidable from existing logs (see LOG_LEVEL above). It needs fix A's
     telemetry, or a diagnostics screenshot of `/dms` in `T0HPR78RX` taken mid-export.
3. **What triggers a wide run?** The runs are 32234 and 32615 (542); 32398, 32431 and
   33957 (539). Wide runs never coincide across the two workspaces, which suggests
   per-workspace view state.
   - **Log:** the same bridge log. Look for `Slack bridge HTTP server listening`
     (process start), `Bridge job stopped making browser progress`, `Replaced the Slack
     tab with a fresh renderer`, `Authenticated export caller disconnected`, and
     `bridge request` lines with `path=/op` (interactive MCP use).
   - Windows: 2026-09-09 19:30–20:16Z, 2026-09-10 02:30–03:14Z, 2026-09-09
     23:00–2026-09-10 00:14Z, and 2026-09-11 01:00–01:43Z.
   - This decides whether a browser/bridge restart, or Salvador's own interactive Slack
     use through the bridge, is what resets the view.
4. **Did Salvador's own reading of the DM help it land?** asunda45 re-entered the set at
   14:00Z. Salvador was looking at that DM at about 13:49Z (the screenshot time, 09:49
   EDT). Whether reading a DM in another client changes the DMs view's rendered rows is
   unknown. Check the same log's `/op` lines 13:30–14:00Z.
5. **Which leaf revision runs on the mini?** The mini has no git repo; deploys are by
   rsync. The data fits `c938526` (no per-conversation re-resolution), but that is not
   proven. Grep the mini's `dist/slack/slack-web-adapter.js` for
   `acceptResolvedConversation`.
6. **`kubectl -n ops logs job/connector-slackweb-*` is not needed.** Those logs carry
   only totals and no per-conversation lines (repro, "Latest job logs").

## Risk assessment

- **Fix B** changes the one enumeration shared by the leaf's MCP tools (`listChannels`,
  `openChannel` / `resolveConversation`, `resolveTargetUrl` for draft and send), not just
  the export.
  - A full top-to-bottom walk adds DOM work while the tab is in the view the HANDOFF
    warns about: stranding the tab on `/dms` breaks later workspace lookups, so the
    `finally` return-to-Home must stay.
  - More enumerated conversations means more `page.goto` reloads per export, and so more
    tab recycles (`SLACK_CONNECTOR_TAB_RECYCLE_AFTER_RELOADS=2`) and a longer export.
    Wide runs today take 12–16 min. Approaching the `*/30` cadence risks overlapping
    sweeps (`maxSweepDepth: 0` refuses the second with a 503) and switchboard's HTTP
    timeout (HANDOFF notes a ~15 min client disconnect that kills the bridge).
  - Heavier reads also raise the renderer-stall risk that `c938526` addressed.
- **Fix C1** multiplies reads by design and needs the budget.
- **Fix D** changes `sync_runs.status` semantics for slackweb.
  - `ReconcileUnconfirmed` counts `status='ok'`, so fewer `ok` runs delays flags. That is
    correct, but the change is visible.
  - Anything else that reads slackweb `ok` runs as passes, including the repro's own
    `missed_runs`, changes meaning.
  - The dashboard `/sources` page may show more non-`ok` runs.
- **Fix E** needs stored per-run conversation IDs. That is a `stats` size increase of
  about 40 IDs per run per workspace, which is small.

## Landmine matched

None of the existing landmines. **New landmine recorded:** "slackweb `status='ok'` and
`conversations_seen` are not coverage", added to `.claude/INSTITUTIONAL_KNOWLEDGE.md`
under Known landmines. I updated INSTITUTIONAL_KNOWLEDGE.md.

## Fix — data model (switchboard half, 2026-09-11)

- **`migrations/0027_sync_runs_partial.sql`** widens `sync_runs_status_check` to
  `('running','ok','partial','error')`. It is additive and changes no rows. A Slack export whose leaf
  deferred or could not read in-scope conversations, or ran out of budget, finishes `partial`.
  **Deploy order:** apply 0027 to prod BEFORE any image that writes `partial`.
- **`sync_runs.stats`** (jsonb, no schema change) gains, per Slack workspace run:
  - `read`: the conversation ids actually read, written as `[]` when the run read nothing;
  - `deferred`;
  - `unreadable` (`{id,name,code,reason}`);
  - `coverage`: the leaf's counts.

  An old-leaf run carries none of these keys.
- **Readers changed:**
  - `ReconcileUnconfirmed` counts a pass only if it read the target conversation. Legacy runs
    without a `read` key keep counting, and `ok` and `partial` both count.
  - The `/funnel` connector-health row treats `partial` as a successful sync for freshness, and
    says `partial` when the latest run was partial.
