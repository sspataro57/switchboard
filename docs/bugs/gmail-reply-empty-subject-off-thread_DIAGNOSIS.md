# Diagnosis — gmail-reply-empty-subject-off-thread (SWT-61)

Status: **root cause confirmed** (reproduced locally on df7b894 and traced end to end).
Prod read-only checks done inside `BEGIN READ ONLY … ROLLBACK`. Nothing was sent, nothing written.
Client address, subject and body text appear nowhere below; subjects are described by length and
md5 fingerprint only.

## Root cause

`deliveries.subject` is optional at every layer of the gmail path, and the send path reads **only**
that column. `validateDraftDelivery` (`internal/tools/delivery.go:123-214`) requires a subject for
`calendar` (line 151-153) and for no other channel; a gmail draft needs only `body` and `thread_id`
(178-183). `draftDelivery`'s INSERT stores `NULLIF($5,'')` (delivery.go:486-495), so an absent
subject becomes SQL NULL. `approve_delivery` binds subject+body into a hash but never requires the
subject to say anything (delivery.go:849-881; `DeliveryContentHash` at 795-797 defines a NULL
subject as `""`), and the dashboard's `approveAction` checks only that a hash was posted
(`internal/dashboard/server.go:249-267`). `sendDelivery` phase 1 then copies the NULL to `""` into
`google.OutboundMessage.Subject` (delivery.go:1192-1199) — even though, thirty lines earlier, it
called `ResolveGmailRoute` (delivery.go:1160), which **does** resolve the thread's subject into
`GmailRoute.Subject` (delivery.go:1273-1298). That field has exactly one consumer and it is the
dashboard (`internal/dashboard/server.go:160-162`); the send path never reads it. Finally
`google.BuildOutboundMIME` writes the `Subject:` header only when the string is non-empty
(`internal/connector/google/send.go:71-73`) — the one conditional header in a builder that hard-
requires From, To and Message-ID (send.go:57-62). The result is a syntactically valid RFC 5322
message with **no Subject header at all** (Subject is optional in the RFC, so neither the builder,
SMTP submission nor the re-ingest complained), which is what reached the client as a standalone,
subject-less email.

## Evidence

- `internal/tools/delivery.go:151-153` — the only channel whose draft validator requires a subject
  is `calendar` ("it becomes the event summary"). gmail has no such rule.
- `internal/tools/delivery.go:178-183` — gmail's draft rules: non-empty `body`, and `thread_id`
  required. Subject is not mentioned.
- `internal/tools/delivery.go:486-495` — `INSERT … subject … NULLIF($5,'')`: omitted or empty
  subject lands as NULL. Prod #36 is NULL, not `''`, which matches this path exactly.
- `internal/tools/delivery.go:685-703` — `validateUpdateDelivery`: a present *body* must be
  non-blank (SWT-44 review), and the comment states plainly that `subject: ""` "stays legal: it
  clears the subject, as it always has". Nothing ever re-checks it.
- `internal/tools/delivery.go:751-757` — the UPDATE touches subject only when the caller sent the
  key, so the prod `update_delivery` (audit 1855, keys `body, delivery_id, require_channel,
  require_own_draft, worker_id`) left the NULL in place.
- `internal/tools/delivery.go:849-881` + `795-797` — approve hashes `subject + "\x00" + body` and
  compares; an empty subject hashes fine and approves fine.
- `internal/tools/delivery.go:1160-1199` — send phase 1 resolves the route (which carries
  `Subject`) and then builds the message from `d.subject` only. **The value needed to fix the
  message was already in memory and was discarded.**
- `internal/connector/google/send.go:71-73` — `if msg.Subject != "" { … }`. Compare 57-62, where an
  empty From/To/MessageID is a hard error.
- `internal/dashboard/server.go:160-162` + `internal/dashboard/templates/deliveries.html:65,79` —
  the dashboard's "Goes to" cell shows `Thread: {{.ThreadSubject}}` (the *thread's* subject, from
  the route), while the delivery's own subject on a drafted row appears only as the `value=` of a
  text input. A reviewer sees a plausible thread subject next to an empty input and nothing marks
  the row as subject-less.
- Git history: the conditional Subject line is **original** to `64e4545` (08-draft-deliveries) —
  `git log -L 68,74:internal/connector/google/send.go` shows one commit. Not a regression.
  `GmailRoute.Subject` was added in `b36db2a` (SWT-44 review, 2026-09-12) *for the dashboard*, and
  the send path was never switched onto it.
- What changed is the **exposure**, in SWT-44. Before it, gmail drafts came from the drafts worker,
  whose model contract requires a subject (`internal/drafts/drafts.go:50` "reuse the thread's
  subject with Re: when replying", schema 54-64 `"required": ["subject","body"]`) and which always
  passes `args["subject"]` (drafts.go:281). After SWT-44 an interactive MCP session is the normal
  drafting path, and `subject` is absent from `draft_delivery`'s required list
  (`internal/mcpserver/schemas.go:66`: `"required":["task_id","channel","body"]`).
- Prod audit confirms the split: the only two gmail drafts ever created are `draft_delivery` 1392
  (`opsctl:salvo`, arg keys include `subject` → delivery #35, sent correctly, subject length 60 =
  `"Re: " + ` the 56-char thread subject) and 1848 (`mcp:manual:salvo`, arg keys `body, channel,
  require_channel, require_thread_in_task_project, task_id, thread_id, worker_id` — **no
  subject** → delivery #36).

## Why the reproduction fails

`TestGmailReply_EmptySubject_SendsReOnThread` drafts with no `subject` key, so the row is NULL
(the test logs `subject IS NULL = true`), approve passes, and send builds the MIME from `""`. The
builder's `if msg.Subject != ""` skips the header, so the captured bytes carry
`[Content-Type Date From In-Reply-To Message-Id Mime-Version References To]` and no Subject —
byte-for-byte the header set the re-ingested prod copy of `<sb-36-…>` has. The In-Reply-To and
References assertions pass because those come from `ResolveGmailRoute` and the references query,
neither of which depends on the subject. Only the Subject assertion fails, in prod and locally.

## Invariant implicated

**Invariant 4 — nothing external without a delivery row that passed the gate.** The row *is* the
description of the outbound message, and the gate let an incomplete one through: a gmail delivery
that cannot produce a well-formed reply was drafted, approved and sent. The fix must restore
completeness at the row, not paper over it in the transport — a fix only in the builder would leave
the other callers free to create the same row.

Invariant 5 is intact and worth noting: the sent copy re-entered and matched the delivery row
(`raw_source_items` 79030 → `normalized_messages` 297954, outbound, subject length 0). Loop closure
survived a subject-less message; it did not flag it.

## Which subject the reply should carry (question (c))

Recommendation: **the subject of the message being replied to — the thread's latest inbound message,
the same one `In-Reply-To` names — with exactly one `Re: ` prefix**; fall back to
`normalized_threads.subject` when that message's subject is empty; refuse when both are empty.

- `latestInboundMessage` (`internal/tools/delivery.go:1309-1320`) is already "the message a gmail
  reply on this thread answers" — To and In-Reply-To come from it. Taking the subject from anywhere
  else makes the subject disagree with the headers. (It does not currently SELECT `subject`; the fix
  adds that column to the one spelling.)
- The thread's stored subject goes stale by construction: `internal/connector/google/sink.go:248-251`
  upserts with `DO UPDATE SET subject = COALESCE(normalized_threads.subject, EXCLUDED.subject)` —
  **first writer wins, never updated**. When a correspondent renames a thread, the stored subject is
  still the first-ingested message's. That is exactly thread 159886: the stored subject (length 56,
  fp `d2880380`) is the 10 Sep inbound's; the 15 Sep inbound we replied to is length 49, fp
  `1ab4f4b5` — a different subject.
- Measured on prod (read-only, 2026-09-15): of 17,818 `gmail:` threads, 17,084 have an inbound
  message and **142 (0.83 %)** have a stored subject that differs from the latest inbound's. In a
  3,000-thread recent sample, 32 differ raw and **15 still differ after stripping `Re:`/`Fwd:`
  prefixes** — so about half the divergence is prefix noise and half is a genuine rename.
- **Doubling is real, not theoretical.** In the same corpus 247 latest-inbound subjects already
  start with a reply prefix, and 190 *stored thread* subjects do (the first ingested message was
  itself a reply) — a naive `"Re: " + subject` would double on hundreds of live threads. Zero of
  the sampled subjects used a localized prefix (`AW:`, `SV:`, `VS:`, `Antw:`, `Rif:`, `R:`), so
  localized handling is defensive rather than load-bearing — cheap enough to include in one regexp.
- **What the code already has:** `internal/capture/prreview.go:395-400` —
  `prSubjectReplyRe = regexp.MustCompile("^(?i:re:\\s*)+")`, repeated and case-insensitive. It is
  unexported, lives in `capture`, is used only to build PR-review task *titles*
  (`prReviewTitle`, prreview.go:408-425), and does **not** cover `Re[2]:` or localized forms. There
  is no reply-subject builder anywhere. Per the repo's one-spelling rule the new helper should be
  the single spelling and `prReviewTitle` should use its strip half.
- Shape of the helper: strip **reply** prefixes repeatedly (`re` with optional `[n]`, optional
  whitespace, colon; plus the localized set), leave `Fwd:` in place (a reply to a forward is
  conventionally `Re: Fwd: …`), trim, then prepend one `Re: `. Preserve the remaining text verbatim
  — it is client-visible. It must be idempotent.
- A non-empty `deliveries.subject` is **never** rewritten: if a human or the drafts worker chose
  words, they ship as-is.

## Where the fix belongs (question (b))

Considered, in the order the request lists them:

| layer | verdict |
|---|---|
| `google.BuildOutboundMIME` | Cannot construct the right subject — it receives an `OutboundMessage` and has no database. Correct as a **floor**: make `Subject` required exactly like From/To/MessageID (send.go:57-62). |
| `validateDraftDelivery` | Validators see args only (no pool), so it can refuse but cannot fill. Refusing every subject-less gmail draft pushes the choice of client-visible words onto the caller — including the model — which is the opposite of "determinism at the spine". Keep a refusal only for the case the handler cannot resolve. |
| the creator (MCP session / drafts worker / opsctl) | Fixing the caller fixes one caller. The drafts worker already always sends a subject; the session that created #36 did not. Invariant 4 says gate the row, not the caller. Schema/description wording is ergonomics, not the fix. |
| dashboard approval screen | Necessary (it is the review surface and today it hides the emptiness behind an input's `value=`), but bypassable: `approve_delivery` is reachable from opsctl and the full MCP profile. Not the primary. |

**Recommended primary: fill it in `draftDelivery`'s handler, in the gmail branch
(`internal/tools/delivery.go:420-447`, written by the INSERT at 486-495).** Reasons:

1. It is inside the executor, so it covers **every** caller — user-profile MCP, full-profile MCP,
   opsctl, dashboard, drafts worker — and no future caller can route around it (invariant 4 spirit).
2. The gmail branch already resolves the thread server-side to fix `from_account_id`; resolving the
   reply subject there is the same server-side resolution, in the same transaction, with the same
   "never caller-chosen" property that From has.
3. Filling at draft time makes the subject **visible on the dashboard before approval**, part of
   `DeliveryContentHash` (so Salvador's approve is bound to it), and editable with
   `update_delivery` — a fill at send time would put words on the wire that no one reviewed.

With three guards behind it, each small and each on a distinct path:

- `sendDelivery` phase 1 (delivery.go:1192-1199): **refuse** a gmail row whose subject is empty,
  naming the reason. Unreachable for rows created after the fix; it is the floor for rows created
  before it, and for anything that writes the table directly. Refuse rather than fill, so the
  approved words and the sent words can never differ. Costs nothing today (prod has zero drafted or
  approved empty-subject gmail rows).
- `google.BuildOutboundMIME`: require `Subject` like From/To/MessageID. Pure, testable, and the
  last line if a future caller builds a message another way (today only delivery.go:1196 does).
- Dashboard: render the delivery's own subject on drafted rows with an explicit `(no subject)`
  marker next to the existing `Thread:` line, so the two subjects are distinguishable at a glance.

Optionally, for the same reason migration 0020 added `deliveries_calendar_identity_check`: a
forward-only migration `CHECK (channel <> 'gmail' OR (subject IS NOT NULL AND btrim(subject) <> ''))`
would make the hole unrepresentable. Prod has exactly **one** violating gmail row (#36, sent), so
this needs a one-row backfill decision first — recorded as an open question, not as scope.

## Proposed fix scope

- [ ] New helper package (one spelling) — `StripReplyPrefix` / `ReplySubject`: repeated `re` with
      optional `[n]` and the localized set, `Fwd:` preserved, idempotent.
- [ ] `internal/tools/delivery.go` `latestInboundMessage` (1309-1320): also SELECT `subject`
      (the one spelling of "the message we answer" gains the field the fix needs).
- [ ] `internal/tools/delivery.go` `draftDelivery` gmail branch (420-447 / INSERT 486-495): when the
      caller passed no subject, store `ReplySubject(latest inbound subject, else thread subject)`;
      refuse with a clear message when both are empty.
- [ ] `internal/tools/delivery.go` `sendDelivery` phase 1 (1192-1199): refuse a gmail row with an
      empty subject.
- [ ] `internal/connector/google/send.go` `BuildOutboundMIME` (57-73): require `Subject`; write the
      header unconditionally.
- [ ] `internal/dashboard/templates/deliveries.html` (65, 76-87): show the delivery subject on
      drafted rows with a `(no subject)` marker.
- [ ] `internal/capture/prreview.go:395-400`: adopt the shared strip helper (one spelling).
- [ ] Regression tests (test-author converts the repro) — see below.

## Out of scope for this fix

- The To-drift gap (SWT-46): To is re-resolved at send while the subject would now be frozen at
  draft. That divergence is deliberate here; persisting the whole route at approval is SWT-46's job.
- The R8 dedup caveat recorded under SWT-44 (a first send on a `ready` task writing the
  delivery_lifecycle key) — SWT-47.
- A migration-level CHECK on `deliveries.subject` (needs a decision about the one existing sent row).
- Whether Gmail's grouping *specifically* requires a matching subject: not observable from here.
- The jira_comment row with a NULL subject (#1): subject is unused on that channel.

## Open questions

1. Latest-inbound subject vs stored thread subject. The diagnosis recommends latest inbound; the
   existing reproduction asserts `"Re: " + thread subject` and its fixture deliberately gives the
   latest inbound a *different* subject, so **accepting the recommendation changes that assertion**
   (`gmail_reply_subject_repro_integration_test.go:185-187`). Owner call.
2. Empty on both sides (29 gmail threads have an empty stored subject): refuse the draft (the
   recommendation) or fall back to a literal placeholder?
3. Localized prefixes: measured zero in the corpus. Include them in the regexp or keep it to
   `re`/`Re[n]`?
4. Move `prReviewTitle` onto the shared helper now (one-spelling rule) or in a follow-up? Widening
   the regexp changes some PR-review task titles — board text only, no client surface.
5. Backfill/CHECK for the existing sent row #36 — leave the historical NULL, or normalize it so a
   future constraint can be added?

## Read-only prod: how many deliveries are affected (question (e))

Queried 2026-09-15 inside `BEGIN READ ONLY … ROLLBACK`; `subject IS NULL OR btrim(subject) = ''`.

**Nothing is queued. No gmail delivery is currently `drafted`, `approved` or `sending` with an
empty subject, so nothing would go out wrong if approved right now.** There is nothing to stop.

gmail, by status:

| status | count | ids |
|---|---|---|
| drafted | 0 | — |
| approved | 0 | — |
| sending | 0 | — |
| sent | 1 | **36** (task 164, thread 159886, sent 2026-09-15 19:30:24 UTC) |

All channels: 2 rows total — **#36** (gmail, sent) and **#1** (jira_comment, sent 2026-07-12; that
channel's send path never reads `subject`, so it is cosmetic there).

For context, only two gmail deliveries exist at all: **#35** (sent 2026-09-12, subject present,
length 60 = `"Re: "` + the 56-char thread subject, drafted by `opsctl:salvo` *with* a subject) and
**#36**. The defect is 1 of 2 gmail sends ever made, and 1 of 1 made through the SWT-44 session path.

## Tests that should pin the fix (question (f))

New:

- **Convert the reproduction** (`internal/tools/gmail_reply_subject_repro_integration_test.go`) into
  the regression test: a Subject header is present and equals `Re: <subject of the latest inbound>`
  (see open question 1), In-Reply-To/References unchanged.
- Same suite, second case: a thread whose latest inbound subject **already** starts with `Re:` —
  the sent subject carries exactly one prefix.
- Same suite, third case: stored thread subject differs from the latest inbound's — the drafted
  subject follows the latest inbound (this is the prod shape, thread 159886).
- Same suite, fourth case: both subjects empty — `draft_delivery` refuses, naming the fix
  ("pass subject").
- Integration (not unit): draft with no subject → `deliveries.subject IS NOT NULL`. The value comes
  from a **column**, so per the institutional-knowledge rule the test must go red when the SELECT
  stops fetching the subject — mutate the SELECT to a literal and watch it fail before trusting it.
- Integration: force an approved gmail row's subject to NULL by SQL and call `send_delivery` — it
  refuses, proving the floor is reachable independently of the draft-time fill.
- Unit, helper: repeated `Re:`, uppercase `RE:`, `Re[2]:`, leading whitespace, localized prefixes,
  a subject that is only a prefix, `Fwd:` preserved, empty input, idempotency.
- Unit, `google.BuildOutboundMIME`: an empty Subject is an error (mirror the From/To/MessageID
  assertions in `send_test.go`).
- Dashboard structure test: a drafted gmail row renders the delivery's subject and a `(no subject)`
  marker, alongside the existing `TestDeliveriesTemplate_*` family.

Must stay green:

- `internal/connector/google/send_test.go`: `TestBuildOutboundMIME_HeadersAndThreading`,
  `TestBuildOutboundMIME_ScrubsAIAttribution`, `TestGmailSender_Send_PostsRawWithThreadID`,
  `TestGmailSender_Send_NonOKWrapsError`.
- `internal/tools`: `TestDelivery_Integration_FullLifecycle`;
  `TestApproveDelivery_Integration_ContentBound`,
  `TestDraftDelivery_Integration_ThreadMustBeFiledUnderTheTaskProject`,
  `TestUpdateDelivery_Integration_RequireChannel`, `TestResolveGmailRoute_Integration`;
  `TestDeliveryContentHash_IsSHA256OfSubjectNULBody`, `TestValidateApproveDelivery_ContentHashShape`,
  `TestValidateDraftDelivery_RequireChannelRefusesOtherChannels`,
  `TestValidateDraftDelivery_ThreadProjectPinNeedsAThread`,
  `TestValidateUpdateDelivery_RefusesEmptyBody`; the whole `reject_delivery` suite; the calendar
  suites (`delivery_calendar_validate_test.go`, `delivery_calendar_integration_test.go` — calendar's
  existing subject rule must not move); `delivery_slack_integration_test.go`,
  `delivery_upwork_target_test.go` (those channels must keep accepting an empty subject).
- `internal/dashboard`: `TestDashboard_Integration_DeliveryDestinationAndContentBoundApprove`,
  `TestDeliveriesTemplate_ApproveFormCarriesContentHash`, `TestApproveAction_PassesTheHashAsJSON`,
  `TestApproveAction_RefusesAPostWithoutTheHash`, and the Reject equivalents.
- `internal/mcpserver`: `user_drafts_test.go`, `profile_test.go`, `adapter_test.go`, `serve_test.go`,
  `runbook_test.go` — they pin the `draft_delivery` schema, its description and the profile pins, so
  any wording change there must be made in step with them.
- `internal/capture` PR-review title tests, if the shared strip helper lands there.

Watch the content-hash interaction: filling the subject at draft changes
`DeliveryContentHash(subject, body)` for gmail drafts. The hash is computed at render time so the
flow is unaffected, but any test that hard-codes a hash for a subject-less gmail draft must be
updated (check `TestApproveDelivery_Integration_ContentBound`).

## Other channels (question (d))

**gmail only.**

- `sendJiraComment` (`internal/tools/delivery.go:1557`) selects `task_id, body, status,
  sent_external_id, target_ref, from_account_id, send_enabled, account_email` — no subject.
- `sendSlackReply` (delivery.go:1651) selects `task_id, body, status, sent_external_id, target_ref,
  approval_source` — no subject.
- `upwork_chat` has no direct send path at all (assisted tier: `prefill_delivery` /
  `mark_delivery_sent`), and reads body + target_ref only.
- The drafts worker's contract says so out loud: "empty for chat channels"
  (`internal/drafts/drafts.go:50`).
- `calendar` is already closed: the validator requires a subject (delivery.go:151-153) and the send
  uses it as the event `Summary` (`internal/tools/delivery_calendar.go:294`), with migration 0020's
  CHECK covering the rest of that row's identity.

So an empty subject reaches an external surface on gmail alone. On the other channels subject is
unused metadata — a rule there would be ceremony, and it would be untested against any real send.

**Unthreaded gmail does not exist in this system, so no exception is needed.**
`validateDraftDelivery` refuses a gmail draft without `thread_id` ("From is resolved from the
thread", delivery.go:181-183), and `sendDelivery` refuses a gmail row whose `thread_id` is NULL
(delivery.go:1152-1154). Every gmail delivery is a reply on an ingested thread. A future
"new outbound mail" capability would need an explicit subject of its own; do not pre-carve an
allowance for a case the code currently refuses. The case that *does* need handling is a threaded
reply whose thread and latest inbound both have an empty subject (29 gmail threads have an empty
stored subject) — that is open question 2.

## Risk assessment

- **Client-visible words change.** The draft now carries a subject the spine chose. It is shown on
  the dashboard and editable before approval, so the human gate is unchanged; but the register of
  the subject is now deterministic rather than model-written, which is the intent (CLAUDE.md:
  "determinism at the spine").
- **`BuildOutboundMIME` becoming strict** turns a missing subject into a build error, and
  `sendDelivery` marks the row `failed` on a build error (delivery.go:1220-1224). That is a safe
  non-send, and with the draft-time fill it should be unreachable — but it is a behaviour change for
  any pre-existing approved row. Measured: zero such rows in prod.
- **The send-time refusal could strand an approved row** with no subject; an approved row cannot be
  edited (`update_delivery` requires `drafted`), so recovery is Deny/Redo. Again: zero such rows
  today, and the alternative (silently filling at send) breaks the approve-binds-the-words property
  SWT-44 built.
- **Shared code paths:** `latestInboundMessage` is also used by `refuseThreadOutsideTaskProject`
  (the same-project pin) — adding a selected column must not change its ordering or its
  `pgx.ErrNoRows` contract. `ScrubAIAttribution` still applies to the subject on the draft path
  (delivery.go:493); a resolved subject must go through the same scrub for consistency.
- **`prReviewTitle`** is a second consumer of any shared Re-stripping regexp; widening it changes
  some board titles (no client surface).
- **Subject frozen at draft vs To re-resolved at send** is a new asymmetry within one row. It is the
  conservative direction (approved words are what ship), but it should be recorded for SWT-46.

## Landmine matched

None existing. **New landmine — recorded in `.claude/INSTITUTIONAL_KNOWLEDGE.md`** ("The send path
had the subject and threw it away"): a resolver whose field has only a *display* consumer, plus a
transport that writes a field only `if != ""`. Both halves read as correct in isolation; together
they emit a client-visible message missing a field the system had already computed.
