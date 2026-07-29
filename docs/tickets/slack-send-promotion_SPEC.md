> Jira: SWT-12

# slack-send-promotion — promote slack_reply from assisted to approve

All four open questions ANSWERED 2026-07-29 and folded in below; see
`slack-send-promotion_OPEN_QUESTIONS.md` for the reasoning and the rejected
alternatives. Ready for `test-author`.

Answers in brief: (Q1) MCP-list `mark_delivery_sent` only; (Q2) flag after 3
completed export passes; (Q3) new `deliveries.approval_source` column, and the
manual path goes `drafted → sent` with no approval; (Q4) recording is exempt
from the kill switch, and a record written during a freeze is logged.

## Source

User request (Salvador, 2026-07-26 / 2026-07-29):

> Promote the `slack_reply` delivery channel from the assisted tier to approve:
> switchboard sends the message through the Slack Web connector after approval,
> instead of prefilling the composer and waiting for a human to click send inside
> Slack. The assisted flow requires remote-desktopping into the Mac mini to press
> send, which makes it unusable in practice.

This is the policy matrix's anticipated path — "autonomy is EARNED per category,
promoted manually". It is that promotion, not a redesign.

## Goal

Give `send_delivery` a Slack branch that clicks Send through the Slack Web
connector's bridge after switchboard approval, with post-hoc export-based
confirmation standing in for a reservable external id, while the sibling
connector's own token-gated MCP send remains a distinct, differently-gated
manual path that switchboard records rather than decides. Usable alone means:
Salvador drafts a `slack_reply`, approves it in the dashboard (or via opsctl),
clicks Send, and the message appears in the Avviato channel with no remote
desktop session — and a later connector run stamps `sent_external_id` on the row.

## The two publish paths (the central decision)

There are TWO ways a Slack message goes out. They have DIFFERENT approval
authorities. They are not one flow with a variation.

### 1. Automated path — the delivery row is the gate

Switchboard drafts to `deliveries` → `approve_delivery` (human, in switchboard)
→ `send_delivery` → bridge `send` op → browser clicks Send. The delivery row IS
the approval of record; `policy_decisions` carries the gating decision. The
bridge's `send` operation deliberately bypasses the leaf's local approval token
(via the leaf's existing `SLACK_CONNECTOR_UNATTENDED_SEND` knob) — two approval
systems for one action means neither is the authority.

### 2. Manual path — the leaf token is the gate

Salvador instructs an interactive Claude session; Claude calls the SIBLING
connector's own MCP (`slack_prepare_send` → `slack_send_reply` in
`~/projects/personal/slackconnector`), gated by that repo's short-lived local
approval token. Switchboard writes the `deliveries` row AFTERWARDS, marked
sent. The row is a RECORD, not a decision. Switchboard policy never gates the
send itself (it already happened when the row is written).

The split is legible in the row's fields, not just prose:
`deliveries.approval_source` holds `'switchboard'` or `'leaf_token'` (Q3a).

The manual path records via `mark_delivery_sent` on a `drafted` row, going
`drafted → sent` with NO `approvals` row written (Q3b) — no switchboard
approval happened, and routing the record through `approve_delivery` would ask
the policy engine to rule on a message already sitting in the channel, which it
can refuse.

The governing principle for this path, in Salvador's words: **the kill switch is
for switchboard.** It governs what switchboard itself puts in front of a client.
A send made by another route was never switchboard's to prevent, so recording it
is exempt from the freeze — and a record written while frozen is logged
distinctly (Q4).

## Acceptance criteria

Leaf (sibling repo `~/projects/personal/slackconnector`):

1. `handleBridgeRequest` (`src/switchboard/http-bridge.ts`) gains a `POST /send`
   route: bearer-token auth like `/export` and `/draft`; body
   `{target_url, text}`; 403 `writes_disabled` unless
   `SLACK_CONNECTOR_ENABLE_WRITES=true`; additionally 403 unless
   `SLACK_CONNECTOR_UNATTENDED_SEND=true`. Success returns the adapter's
   `SendResult` `{drafted:false, target_url, sent:true}`.
2. The send is implemented through the EXISTING approval machinery —
   `prepareSend` with `autoApprove` (already keyed to `unattendedSend`, already
   logs a loud warn) followed by `sendReply` with the returned token — not a new
   click path that skips the destination/text binding checks
   (`APPROVAL_MISMATCH` stays enforced).
3. `dist/cli/switchboard-bridge.js` gains the same `send` operation
   (stdin `{target_url, text}`, stdout `SendResult`) behind the same two env
   gates, so the command-bridge transport can send too.
4. Drafting is untouched: the `draft` op and both Go bridges' `Draft` methods
   still reject any result claiming `sent:true`. `send` is its OWN method and
   route, not a relaxation of `Draft`. The "there must never be a send here"
   comments in `http-bridge.ts` and `internal/connector/slackweb/http_bridge.go`
   are revised, deliberately, in this diff.
5. The leaf MCP server's `slack_send_reply` behavior is unchanged. The README
   states that `SLACK_CONNECTOR_UNATTENDED_SEND` is per-process: set it in the
   bridge-server's environment ONLY, never in the MCP server's — setting it
   there silently removes the manual path's token gate (see Named consequences).

Switchboard — send path:

6. `internal/tools/delivery.go` gains a `SlackSender` seam
   (`Send(ctx, targetURL, text string) error` + `SetSlackSender`), mirroring
   `GmailSender`/`JiraSender`. Both `slackweb.CommandBridge` and
   `slackweb.HTTPBridge` implement it; the Go side rejects any send result that
   is not exactly `{drafted:false, sent:true}`.
7. `send_delivery` routes `channel='slack_reply'` to a `sendSlackReply` branch:
   in one tx it locks the row `FOR UPDATE`, refuses if `sent_external_id` is
   present (invariant 4 — never resend), requires `status='approved'`, parses
   `target_ref` with `slackweb.ParseTargetURL`, resolves the workspace's
   synthetic `source_accounts` row (`provider='slack_web'`,
   `account_email='{workspace_id}@slack-web.local'`) and refuses unless
   `send_enabled=true`, then commits `status='sending'` BEFORE the bridge call.
   `google.ScrubAIAttribution` runs on the body before the bridge call (jira
   precedent).
8. On bridge success: `status='sent'`, `sent_at=now()`, `sent_external_id`
   stays NULL (there is no id to record — the export stamps it later), and a
   `delivery_sent` task_event is emitted. No code path writes a fabricated
   `sent_external_id` at send time.
9. On bridge failure the two classes behave differently and a test covers each:
   - HTTP 4xx from the bridge (unauthorized, writes/unattended disabled,
     validation) — the request never reached the click: `status='failed'` +
     `error`, `sent_external_id` NULL, so the existing failed→approved retry
     path is reachable.
   - HTTP 5xx, transport error, or context timeout — the click MAY have landed:
     the row STAYS `'sending'` with `error` recorded. It is not re-approvable
     (`approve_delivery` only accepts drafted/failed) and nothing retries it
     automatically, ever. Resolution is criterion 11/12 or the export matcher.
10. Loop closure widens: `confirmDelivery` in
    `internal/connector/slackweb/sink.go` matches rows with
    `status IN ('sending','sent')` (today `'sent'` only), and on a prefix match
    stamps `sent_external_id` + `confirmed_at`, promotes a `'sending'` row to
    `'sent'` (setting `sent_at` if null), and emits `delivery_confirmed` — so a
    crash between the click and the `'sent'` write self-heals on the next
    export. The existing guards stay: `WHERE sent_external_id IS NULL`,
    RowsAffected check, never overwrite an existing `sent_external_id`.
11. Unconfirmed-send reconciliation: after normalize, the connector run flags —
    never retries — any `slack_reply` delivery with
    `status IN ('sending','sent')`, `sent_external_id IS NULL`,
    `confirmed_at IS NULL` whose workspace has completed ≥ N successful export
    passes since the send attempt. **Q2 ANSWERED: N = 3 completed successful
    export passes**, env-tunable as `SLACK_UNCONFIRMED_FLAG_PASSES`, counted
    from `sync_runs` rows for that workspace's account with `status='ok'` that
    STARTED after `sent_at` (an export already in flight when the send happened
    must not count). Wall time was rejected: it flags spuriously whenever the
    mini is off or the CronJob is suspended — which is today's state — turning
    "the poller didn't run" into an alarm that reads "the send may have failed".
    Flagging = set a note in `error` (guarded so it fires once) + emit a
    `delivery_unconfirmed` task_event. Deterministic SQL, no LLM, connector-side
    (orchestrator untouched).
12. Human resolution verbs exist for the flagged/ambiguous states:
    - `mark_delivery_sent` for `slack_reply` accepts `status='sending'` as well
      as `'approved'` (Salvador looked at Slack and the message is there);
      `upwork_chat` behavior is unchanged.
    - New spine-facing, human-only, not-MCP-listed `mark_delivery_failed`:
      `sending`→`failed` for `slack_reply` rows with `sent_external_id IS NULL`
      and `confirmed_at IS NULL` (the message is verifiably NOT in Slack),
      re-opening the failed→approved retry path. Registered through the
      executor like every other tool.
13. Policy (`internal/policy/matrix.go`): the `slack_reply` branch drops the
    `channel_assisted` denial of `send_delivery`. New shape: hourly `rate_limit`
    for both `send_delivery` and `mark_delivery_sent`, kill switch for
    `send_delivery` ONLY, then allow. `mark_delivery_failed` joins `humanOnly`
    (it is not send-shaped — it moves a row AWAY from the world).
    `prefill_delivery` keeps its human-only allow (the assisted verb survives
    as a fallback). Workers still cannot reach any of these (`humanOnly`
    denies actor `mcp:{worker}`); unit tests in `matrix_test.go` cover the new
    branch including the worker-denied cases.

    **Q4 ANSWERED — the kill switch is for switchboard.** It governs what
    switchboard itself puts in front of a client, so `send_delivery` stays
    freeze-gated and `mark_delivery_sent` does not: a send made by another route
    was never switchboard's to prevent, and refusing to record it only makes the
    database disagree with a message that is provably in the channel.

    IMPLEMENTATION TRAP, do NOT simply delete `mark_delivery_sent` from
    `sendShaped`: `snapshotGated` is defined as `sendShaped` (`matrix.go:34`)
    and `Decide` returns `allow`/`matrix-human` for anything not in it BEFORE
    the channel switch (`matrix.go:60`), so that one-line removal would also
    silently drop the tool's hourly rate limit and its entire channel branch —
    widening the tool while appearing to narrow it. Split the concepts instead:
    keep a `snapshotGated` map holding BOTH tools (rate limit and channel logic
    still run for both) and consult `snap.SendingFrozen` only for
    `send_delivery`. `matrix_test.go` must pin all four corners: frozen+send =
    deny/`kill_switch`, frozen+record = allow, over-limit+record =
    deny/`rate_limit`, worker+record = deny/`human_only`.
14. Manual-path rows are queryably distinct from automated-path rows via
    `deliveries.approval_source` (criterion 18), and the SWT-13 canonicalization
    rule holds: any code writing `target_ref` stores `Target.CanonicalURL()`.
15. Wiring (closes the known dead-code gap): `cmd/opsctl/main.go` and
    `cmd/dashboard/main.go` build the Slack bridge the way
    `cmd/connectors/slackweb/main.go` already does — prefer
    `SLACK_WEB_BRIDGE_URL` + `slackweb.TokenFromEnv()` (HTTPBridge), fall back
    to `SLACK_WEB_BRIDGE_SCRIPT` (CommandBridge) — and pass it to BOTH
    `SetSlackDrafter` and `SetSlackSender`. A URL-only cluster/workstation
    deployment can now draft AND send; `HTTPBridge.Draft` stops being dead code.
16. Dashboard: `internal/dashboard/templates/deliveries.html` shows the Send
    button for approved `slack_reply` rows (today it is gmail-conditional) and
    keeps Mark-sent; flagged/errored rows surface the `error` text.
17. `go test ./...`, `go vet ./...`, `make integration` (Slack suite in the
    existing cleanup pact), and the leaf's `npm run check` all pass. Integration
    tests cover: two-phase send with a fake sender, ambiguous-failure row state,
    sending-row confirmation promotion, reconciler flagging, resolution verbs,
    and that a confirmed row is never re-sent or re-stamped.

From the answered open questions:

18. **Q3a — gate authority is recorded on the row.** Migration
    `0011_slack_send_promotion.sql` adds nullable `deliveries.approval_source
    TEXT`. `approve_delivery` writes `'switchboard'`; the manual-path record
    writes `'leaf_token'`. Existing rows backfill to `'switchboard'` (every
    delivery sent before this ticket passed `approve_delivery`). The value is
    written in the SAME tx as the status transition — a crash must never leave a
    row whose gate is unknown, which is the one thing the column exists to
    prevent.
19. **Q3b — the manual path skips the approval it never had.**
    `mark_delivery_sent` accepts a `drafted` `slack_reply` row when
    `approval_source='leaf_token'`, transitioning `drafted → sent` directly and
    writing NO `approvals` row. The automated path's `status='approved'`
    requirement in `sendSlackReply` is unchanged. Tests pin that a `drafted`
    row with `approval_source='switchboard'` (or NULL) is still refused — the
    new edge must not become a general bypass of approval.
20. **Q1 — `mark_delivery_sent` becomes MCP-listed; nothing else does.** Added to
    `agentTools`/`agentToolNames` (`internal/mcpserver/schemas.go`,
    `adapter.go`) so an interactive session can record a manual send in one
    call. `approve_delivery`, `send_delivery`, and `mark_delivery_failed` stay
    OFF the MCP surface — this session ingests Slack and email content, so the
    verbs that can put words in front of a client must not be one prompt
    injection away. A test pins that listing it does NOT make it worker-callable
    (`humanOnly` still denies `mcp:{worker}`).
21. **Q4 — freeze-time records are logged.** When `mark_delivery_sent` succeeds
    while `sending_frozen` is true, emit `delivery_recorded_during_freeze`
    (payload `{delivery_id, channel, approval_source}`) so "a message went out
    by another path while the kill switch was on" is a direct query rather than
    a reconstruction from timestamps. The orchestrator has no rule for this
    event; it is a human/dashboard signal.

## Data model changes

- No new tables. Statuses, `sent_external_id`, `sent_at`, `confirmed_at`,
  `error` all exist (0001 + 0006).
- Forward-only migration `migrations/0011_slack_send_promotion.sql` (number =
  next free at implementation time; 0010 is taken) adds one nullable column,
  `deliveries.approval_source TEXT`, and backfills existing rows to
  `'switchboard'`. Values: `'switchboard'` | `'leaf_token'`. No CHECK
  constraint — a third gate is plausible later (an auto tier), and the SWT-8
  convention is to validate delivery enums in the handler.
- New `task_events` vocabulary: `delivery_unconfirmed` (payload
  `{delivery_id, channel, passes}`) and `delivery_recorded_during_freeze`
  (payload `{delivery_id, channel, approval_source}`). Orchestrator has no rule
  for either — both are dashboard/human signals, and unknown event types are
  ignored by the drain.

## API / MCP tool changes

Leaf bridge (both transports):

- `send` operation: in `{target_url, text}` → out
  `{drafted:false, target_url, sent:true}`. Gates:
  `SLACK_CONNECTOR_ENABLE_WRITES=true` AND `SLACK_CONNECTOR_UNATTENDED_SEND=true`
  (HTTP: bearer token as well). No new information is returned — Slack Web
  exposes no message id at click time.

Switchboard executor:

- `send_delivery`: gains the `slack_reply` branch (criterion 7–9). Hooks in at
  the existing channel routing in `internal/tools/delivery.go` `sendDelivery`
  (the `jira_comment` special-case is the pattern). Executor path unchanged:
  validate → policy → audit start → handler → audit complete.
- `mark_delivery_sent`: accepts `slack_reply` in `sending` as well as
  `approved` (criterion 12), and in `drafted` when
  `approval_source='leaf_token'` (criterion 19). **Becomes MCP-listed**
  (criterion 20) — the only delivery verb besides `draft_delivery` on the agent
  surface. No longer freeze-gated (criterion 13); still human-only and still
  rate-limited.
- `mark_delivery_failed` (NEW, spine-facing, human-only, not MCP-listed):
  `{delivery_id}` → `{delivery_id, status:"failed"}` (criterion 12). Reached
  via `opsctl call` / dashboard.
- `draft_delivery`, `prefill_delivery`: no schema changes.
- `approve_delivery`: writes `approval_source='switchboard'` (criterion 18);
  stays OFF the MCP surface, dashboard and `opsctl call` only.

## MQTT topics

None. The connector stays a one-shot poller; the bridge is HTTP/subprocess.

## Files likely to touch

Switchboard:

- `internal/policy/matrix.go`, `internal/policy/matrix_test.go`
- `internal/tools/delivery.go` (SlackSender seam, `sendSlackReply`,
  `markDeliverySent` extension, `mark_delivery_failed`),
  `internal/tools/createtask.go` (registration), delivery integration tests
- `internal/connector/slackweb/bridge.go`, `http_bridge.go` (Send methods +
  comment revision), `sink.go` (confirm widening + reconciler),
  `integration_test.go`, `bridge_test.go`, `http_bridge_test.go`
- `cmd/connectors/slackweb/main.go` (reconciler invocation / N env)
- `cmd/opsctl/main.go`, `cmd/dashboard/main.go` (bridge wiring, criterion 15)
- `internal/dashboard/templates/deliveries.html`
- `migrations/0011_slack_send_promotion.sql` (`approval_source` + backfill)
- `internal/mcpserver/schemas.go`, `internal/mcpserver/adapter.go`
  (MCP-list `mark_delivery_sent`, criterion 20)
- `.claude/INSTITUTIONAL_KNOWLEDGE.md`, `README.md`

Leaf (`~/projects/personal/slackconnector`):

- `src/switchboard/http-bridge.ts` (`BridgeOperations.send`, `/send` route),
  `src/cli/bridge-server.ts`, `src/cli/switchboard-bridge.ts`,
  `src/slack/slack-web-adapter.ts` (only if a small internal helper is needed —
  `prepareSend`/`sendReply` already exist), tests, `README.md`

## In scope

- The bridge `send` operation on both transports (leaf halves included).
- The `send_delivery` Slack branch, seam, policy promotion, and wiring.
- Post-hoc confirmation hardening: sending-row promotion, reconciler flag,
  resolution verbs.
- Dashboard Send button for `slack_reply`.
- Recording the manual (leaf-token) path distinctly, via `approval_source`.

## Out of scope

- Promoting any other channel. `upwork_chat` stays assisted; email draft-only
  friction is SWT-14/15's territory (gmail bridge), not this ticket.
- An `auto` tier for Slack (send without approval) — that is a later earned
  promotion.
- DM enablement (`SLACK_CONNECTOR_ALLOW_DMS` stays false; smoke uses an
  allowlisted channel).
- Adding `OWN_USER_IDS` for Collaboratory/`T0HPR78RX` or widening the export —
  operational config, not code (but see Named consequences).
- Triage, orchestrator rules, task lifecycle changes.
- Deploying the dashboard or opsctl anywhere new.

## Named consequences

State these plainly; they are deliberate trades, not oversights.

1. **The manual path's audit trail records occurrence, not permission.** A
   message that really reached a client channel has no gating decision in
   `policy_decisions` — the gate was the leaf's token, outside switchboard.
   Invariant 4 is preserved in letter (a row always exists) but the row's
   meaning differs between paths, which is why `approval_source` is mandatory,
   not cosmetic: without it the two kinds of row are indistinguishable, and the
   safe-looking assumption — that everything in `deliveries` passed switchboard
   policy — would be false.
2. **A browser click has no reservable external id.** Unlike SMTP's
   pre-reserved Message-ID, a crash after the click and before the `'sent'`
   write leaves a `'sending'` row that switchboard cannot classify. The design
   answer is: commit `'sending'` pre-click; the next export confirms via the
   120-char prefix matcher; after N passes with no match, flag for a human.
   NEVER auto-retry — a retry is a possible double-post into a client channel.
   `'sending'` is therefore terminal until the matcher or a human
   (`mark_delivery_sent` / `mark_delivery_failed`) moves it.
3. **`SLACK_CONNECTOR_UNATTENDED_SEND` is a per-process trust decision.** Set in
   the bridge-server's environment it means "switchboard gates sends"; set in
   the leaf MCP server's environment it would silently remove the manual path's
   human token gate. The two processes have separate launchd environments on
   the Mac mini; the leaf README must say so explicitly.
4. **The kill switch now genuinely stops Slack sends, and stops nothing else.**
   `send_delivery` is freeze-gated, so the panic button really does halt
   switchboard's Slack traffic — that is new; under the assisted tier the freeze
   never touched the human's click. But `mark_delivery_sent` is deliberately NOT
   freeze-gated (Q4): the kill switch is for switchboard, and a send made
   through the leaf's own token was never switchboard's to prevent. Refusing to
   record it would not un-send anything; it would only leave a message that is
   provably in a client channel with no row saying so.

   The accepted cost: during a freeze, rows can still reach `'sent'`. Anyone
   reading "frozen" as "nothing moves" will be wrong. That is why criterion 21
   logs `delivery_recorded_during_freeze` — the exemption must be visible in
   review rather than inferred from timestamps, and a burst of those events
   during a freeze is itself a signal worth seeing.
5. **Confirmation only works where export works.** Export fails closed for any
   allowed workspace without an `OWN_USER_IDS` entry; `T0HPR78RX` has none, so
   the bridge is effectively narrowed to Avviato. A send into an unexported
   workspace would stay unconfirmed forever and always end flagged. Not a bug —
   an operational precondition to record in INSTITUTIONAL_KNOWLEDGE.
6. **Existing accepted risk unchanged:** the freeze still does not gate
   `prefill_delivery` (not sendShaped), per SWT-13's recorded decision.
7. **A deliberate reversal of a written guarantee.** Both `http-bridge.ts` and
   `http_bridge.go` currently document that a send route must never exist. This
   ticket revises that on purpose; the diff must update the comments so the
   code never claims a guarantee it no longer provides.

## Invariants that apply

- **3 — everything through the executor:** the Slack branch of `send_delivery`
  and the new `mark_delivery_failed` are executor-registered handlers behind
  validate → policy → audit; no direct bridge calls from dashboard or opsctl
  code. The reconciler lives in the connector (trusted, `sync_runs`-audited),
  same standing as `confirmDelivery` today.
- **4 — nothing external without a delivery row:** the bridge sender is
  reachable only from `sendSlackReply` on an approved row; a present
  `sent_external_id` refuses resend forever; `'sending'` commits pre-click;
  the manual path still yields a row (as record — consequence 1); the
  confirmation stamp is guarded against overwrites.
- **5 — own-message loop closure:** our sends re-enter via export (direction
  outbound by `OWN_USER_IDS`, never re-triaged); the matcher now also closes
  the crash window by promoting `'sending'` rows.
- **6 — stealth attribution:** `ScrubAIAttribution` runs at draft time (exists)
  and again in `sendSlackReply` before the bridge call (jira precedent).
- **7 — orchestrator purity:** untouched. `delivery_unconfirmed` gets no rule;
  reconciliation is deterministic SQL in the connector.
- **1/2 — raw-first, one funnel:** no changes to ingestion or task shape; no
  new task-like tables (the flag is an event + error note, not a table).

## Sibling patterns to copy

- Two-phase send, `sending` committed pre-network, ambiguous-vs-definite
  failure split: `internal/tools/delivery.go` `sendDelivery` (gmail) and
  `sendJiraComment` (id-post-call shape — the closer cousin).
- Post-hoc recovery that also promotes status and fills the id:
  `internal/connector/jira/sink.go` (~line 242) — exactly the shape criterion
  10 needs.
- Prefix matcher and RowsAffected/`sent_external_id IS NULL` guards:
  `internal/connector/slackweb/sink.go` `confirmDelivery`,
  `internal/connector/upworkcrm/sink.go`.
- Bridge transport selection: `cmd/connectors/slackweb/main.go` `newSource()`.
- Adapter seam + fake-in-tests: `SetJiraSender` usage in
  `internal/tools/delivery_jira_integration_test.go`.
- Leaf route auth/gating: `src/switchboard/http-bridge.ts` `/draft` handling
  (token first, then method, then writes gate).

## Verification protocol

1. Leaf: `npm run check` (route tests for `/send` gates: no token → 401, writes
   off → 403, unattended off → 403, happy path shape).
2. Switchboard: `go vet ./... && go test ./...`.
3. `make db-up && make migrate && make integration` — new integration tests in
   the serialized cleanup pact.
4. Real smoke (Salvador is a member of Avviato `T0360B84U`; the Mac mini work
   follows the repo runbook pattern, not ad-hoc SSH):
   a. On the mini: bridge-server env gains `SLACK_CONNECTOR_UNATTENDED_SEND=true`
      (bridge-server process ONLY); restart it.
   b. Workstation: `draft_delivery` a short body targeting an allowlisted
      Avviato channel Salvador controls; `approve_delivery`; `send_delivery`
      via dashboard button or `opsctl call` with `SLACK_WEB_BRIDGE_URL` +
      token set. Verify the message appears in Slack; row is `sent`,
      `sent_external_id` NULL.
   c. Run `cmd/connectors/slackweb`; verify `sent_external_id` + `confirmed_at`
      stamped and exactly one `delivery_confirmed` event; rerun with `--all`
      and verify no duplicate event.
   d. Kill switch check: freeze via dashboard, verify `send_delivery` on a
      second approved row is denied with rule `kill_switch`; unfreeze.
   e. Delete the test rows in FK order afterwards.

## Decisions made unilaterally

- **This ticket wires the HTTP bridge into the delivery seams** (criterion 15).
  Without it the promotion works only on the Mac mini itself, which is the
  exact configuration the promotion exists to escape. It also retires the
  `HTTPBridge.Draft` dead code.
- **The leaf send reuses `prepareSend(autoApprove)` + `sendReply`** rather than
  a new direct click path: the destination/text binding and
  `APPROVAL_MISMATCH` checks come for free, and `unattendedSend` already
  exists, documented and loud, for exactly this deployment shape.
- **`prefill_delivery` survives** as a human-only fallback verb. Removing it
  buys nothing and the assisted flow remains useful when the bridge-server is
  down.
- **Per-workspace send gate = `send_enabled` on the synthetic account**,
  mirroring gmail's go-live convention (`UPDATE source_accounts SET
  send_enabled=true` per workspace, manual). Today `EnsureAccount` inserts
  `send_enabled=false` and never updates it, so the default is safely off.
- **Failure classification by HTTP status** (4xx definite / 5xx-transport
  ambiguous): the Go client cannot see whether a 500 happened before or after
  the click, so 500 is conservatively ambiguous.
- **`mark_delivery_failed` is added** despite being new vocabulary: without it
  a verified-unsent stuck row is unrecoverable except by raw SQL, which would
  be a side door (invariant 3).

## Future work (not this ticket)

- Structured error codes on the bridge's 500 responses so pre-click leaf
  failures (`SLACK_LOGGED_OUT`, `SLACK_UI_CHANGED` before the click) can be
  classified definite and auto-retryable.
- `OWN_USER_IDS` for `T0HPR78RX` to widen export + confirmation to
  Collaboratory.
- Earned promotion of specific Slack targets to `auto` once the
  approval-without-edit rate justifies it.
