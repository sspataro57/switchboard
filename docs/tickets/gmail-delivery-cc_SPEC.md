> Jira: SWT-69

# gmail-delivery-cc — Cc on a gmail delivery

## Source

Ad-hoc, not a build-order step. Salvador, 2026-09-21, verbatim:

> "need to be able to add cc to emails on swb I need claude to always add katie
> when writing to rochested and it's not allowed"

Scope, when offered a standing auto-Cc rule inside switchboard (same day):

> "I don't want you to add it here just allow claude to do it"

Known-address rule, same day:

> "any valid address is fine, I approve every email anyway"

swb task #443.

## Goal

Give a `gmail` delivery an optional, explicit list of Cc addresses — set at
`draft_delivery`, editable at `update_delivery` while the row is `drafted`,
shown on the dashboard beside From/To before approval, and emitted as an RFC 5322
`Cc:` header whose addresses are also envelope recipients at send.

**Usable alone.** After this merges (and 0038 is applied to prod and
`ops-mcp-user` is reinstalled), a session in the collaboratory repo can say
"reply to Rochester, Cc Katie", draft the reply with
`draft_delivery {channel:"gmail", …, cc:["kevans@cecollaboratory.com"]}`,
Salvador sees `Cc: kevans@cecollaboratory.com` on the delivery card, approves,
and the mail leaves with Katie on it. Nothing else in the build order is needed
for that to work end to end.

**What this ticket is NOT.** There is no standing rule, no per-project default
Cc, no auto-Cc anywhere. Switchboard never adds an address by itself. The
"always Cc Katie on Rochester mail" habit lives in the collaboratory session's
own instructions, outside this repo. (The one exception is redraft inheritance —
D9 — which carries forward a Cc a human already approved the presence of, and
never invents one.)

## Decisions made unilaterally (with rationale)

**D1 — Ability only.** A `cc` argument, a column, a header, a dashboard field.
No rule anywhere in this repo decides that an address *should* be on a message.
Owner's words: "I don't want you to add it here just allow claude to do it."

**D2 — Any syntactically valid address is acceptable. No known-address check, no
allowlist.** Owner decision, 2026-09-21: *"any valid address is fine, I approve
every email anyway."* This rests on a fact in the policy matrix, not on trust in
the caller: client-facing email is **approve-first**, `approve_delivery` is
`humanOnly` and is listed on no MCP profile (IK, "Gmail drafting over MCP":
"Approve and send stay off the profile"), and the dashboard renders the row
before Salvador clicks. Every Cc is therefore read by a human before it can
leave. The alternative — a "known to switchboard" test — could only have
consulted `normalized_messages.sender` and `person_identities` (there are no
recipient columns on `normalized_messages`; see "What a known-address check
could have seen" below), which is a half-honest rule that would refuse a
correspondent who has only ever been Cc'd.
**If gmail ever becomes auto-send for some category, this decision must be
revisited before that ships** — see Future work.

**D3 — gmail only.** A non-empty `cc` on any other channel is refused by name in
the validator, before any other channel rule, the way `RequireChannel` is
(delivery.go:132-136). `upwork_chat`, `slack_reply`, `jira_comment` and
`calendar` have no concept of a carbon copy and their send paths would silently
drop it; silently dropping a recipient the caller asked for is worse than
refusing. A schema CHECK backstops it (D12).

**D4 — `deliveries.cc TEXT[] NOT NULL DEFAULT '{}'`, migration 0038.** TEXT[],
not jsonb: every "short list of short strings" in this schema is already TEXT[]
(`source_accounts.scopes` 0001:16, `projects.ticket_delivered_statuses`
0025:31, `projects.notifier_senders` 0034:38, `capture_rules.exclude_pr_authors`
0035:23); jsonb in this schema is for provider payloads and untyped blobs. TEXT[]
also gives `cardinality()` and `= ANY()` inside a CHECK (both immutable), and pgx
scans it straight into `[]string` with no marshal step. `NOT NULL DEFAULT '{}'`
so there is exactly ONE representation of "no Cc" — a nullable array would give
two (NULL and `{}`) and every reader would have to COALESCE, which is the class
of trap SWT-61's NULL subject came from.

**D5 — Normalization, in one pure function, address-only.** `cc` entries are
parsed with `net/mail.ParseAddress`. What is STORED is `addr.Address` only:
**a display name is dropped, not stored**. Reasons: (a) a display name is words
on a client-visible surface and would be model-chosen — the repo's standing rule
for gmail routing is "From inherited from thread — never model-chosen", and
invariant 6 is about exactly this kind of byline; (b) an address-only value needs
no RFC 2047 encoding and no quoting, so the MIME writer stays a `strings.Join`;
(c) one canonical shape makes the From/To comparison (D7) and the dedupe exact.
Further, on the value that LANDS (the IK rule from SWT-61 and SWT-64: validate
what lands, not what you were handed):
- the domain part is lower-cased; the **local part is left as given** (RFC 5321
  makes the local part case-sensitive to the receiving host, and this is someone
  else's mailbox, not one of ours);
- every byte of the stored address must be printable ASCII (0x21–0x7E) —
  no control characters, no spaces, no non-ASCII. There is no SMTPUTF8 support in
  `SubmitSMTP` and a non-ASCII header octet in a base64 raw MIME body is a defect
  in both transports. This is also the header-injection floor: CR/LF cannot
  survive it;
- length ≤ 254 bytes per address (RFC 5321 path maximum);
- dedupe is case-insensitive over the whole normalized address, **first
  occurrence wins** (keeps the caller's local-part spelling);
- **maximum 10 addresses.** Justification: this is a reply on a human
  conversation; more than ten carbon copies is a mailing list, which is a
  different feature with different policy. Ten also keeps the folded header far
  under the RFC 5322 998-octet line limit. The number is a named const beside the
  function, and the schema CHECK is pinned equal to it by a test.

**D6 — absent vs empty, stated once.**
- `draft_delivery`: `cc` absent, `null`, or `[]` → the row stores `{}`. There is
  no inheritance at draft time (D9 is the drafts worker's, not the executor's).
- `update_delivery`: `cc` **absent or `null` → unchanged**; `cc: []` → **clear**;
  `cc: [...]` → replace wholesale. Go shape `Cc *[]string`, so the three cases are
  distinguishable; `null` unmarshals to a nil pointer and therefore reads as
  absent, which is documented on the field.
- There is no add/remove verb. Replace-wholesale is the smaller surface and the
  caller always knows the current list (it is on the card and in the tool's
  error text).

**D7 — a Cc may never be the From or the To.** Checked in the handler, where
both are known: From is the resolved account email, To is
`latestInboundMessage(...).sender` — the same value `ResolveGmailRoute` returns
(delivery.go:1377-1384), so the rule and the send agree on who the To is.
Comparison is case-insensitive on the ADDRESS part: `normalized_messages.sender`
holds the raw `From` header for google rows (IK, residue-lane entry: "google
writes the raw From header"), so it is parsed with `net/mail.ParseAddress` first
and compared raw-and-trimmed only if it does not parse. Refusal names the
address and which field it collided with.
At **send** time the To is re-resolved (the known SWT-46 gap: a newer inbound
message can change it between approval and Send). If a stored Cc then equals the
send-time To or From, the send **drops that address from the Cc** rather than
refusing: dropping narrows the recipient set and can never surprise anybody,
whereas refusing would wedge an approved delivery over a cosmetic duplicate. What
was actually sent is recorded (D14).

**D8 — the content hash covers the Cc.** `tools.DeliveryContentHash` becomes
`DeliveryContentHash(subject, body string, cc []string) string`, hashing
`subject + NUL + body + NUL + strings.Join(cc, ",")`. Without this, a Cc added by
`update_delivery` between the page render and the Approve click would pass a
hash check designed to catch exactly that (SWT-44's content-bound approval), and
the claim "Salvador saw every Cc" would be false in precisely that window. A
comma is unambiguous as a separator because D5 forbids it inside a stored
address; the NUL keeps values from sliding across the boundary. No hash is
stored anywhere, so this is not a data migration — the only effect is that a
dashboard page rendered before the deploy refuses its Approve with the existing
"reload the page and review it again" message.

**D9 — a redraft inherits the rejected row's Cc, and the drafts worker is where
that happens.** Reject-with-redraft (SWT-43 "Redo") throws away the *words*; the
Cc is *routing* and Salvador's Redo note is about the text, so losing Katie on a
redraft would be a silent, invisible change of recipients. Implementation: the
LATERAL in `drafts.DeliverTasks` (store.go:87-90) — which already carries the
rejected row's `body` and `rejection_note` — also selects its `cc` into
`DeliverTask.RedraftCc`, and `drafts.Run` (drafts.go:280-286) passes
`args["cc"]` on the gmail branch when it is non-empty. This is deterministic Go;
the model contract stays `{subject, body}` and the model never sees or chooses a
recipient. It is deliberately NOT done inside `draftDelivery`: an executor-side
inheritance would make "absent cc" mean "maybe inherit" for every caller, and
would be switchboard adding a recipient on its own, which D1 forbids.
**This changes a pinned test:** `TestDrafts_Redraft_DraftsThroughTheSameCall`
(internal/drafts/worker_test.go:584-614) asserts the redraft's `draft_delivery`
args have exactly the first draft's key set. Its criterion is "the new row
carries no link back to the rejected row" — `cc` is not such a link (it is a
recipient list, indistinguishable from one the caller typed), so the test is
amended to allow `cc` as the one permitted extra key and to assert its value
equals the rejected row's Cc. The amendment must be in the same commit and must
keep failing for any other extra key.

**D10 — the dashboard's Cc box clears when emptied.** `actionEdit`
(server.go:325-339) forwards `body`/`subject` only when non-empty — the
pre-existing SWT-61 residual where clearing the subject box silently keeps the
old subject. For a recipient list that behaviour is worse than for words:
Salvador would delete Katie from the box, see the save succeed, and send to her
anyway. So `cc` is forwarded whenever the POST **contains the key**
(`r.PostForm.Has("cc")`), with an empty value meaning `cc: []` = clear. The
subject residual is NOT fixed here (out of scope) but is named in the code
comment so the asymmetry reads as deliberate.

**D11 — no policy change, no new tool.** The Cc rides on the gmail delivery row
and is therefore already behind `channel_assisted`/`human_only`/`kill_switch`
and the approve gate. Nothing in `internal/policy` learns the word "cc".

**D12 — two schema CHECKs, and per-address syntax stays in Go.** Migration 0038
adds:
- `deliveries_cc_gmail_check`: `CHECK (channel = 'gmail' OR cc = '{}')`
- `deliveries_cc_shape_check`: `CHECK (cardinality(cc) <= 10 AND NOT ('' = ANY(cc)))`

Both expressions are immutable and so legal in a CHECK. RFC 5322 address syntax
is deliberately NOT re-spelled as a Postgres regex: `net/mail` is the one parser
in this repo (`recipientsFromMIME` smtp.go:185 already uses it), and a second
spelling in SQL is the drift the "one spelling" rule exists to prevent. The
CHECKs are backstops against a direct writer, not the validation.

**D13 — the MIME writer emits `Cc:` after `To:`, folds it, and refuses control
characters.** `BuildOutboundMIME` gains the same kind of floor it gained for
Subject in SWT-61: an address containing any byte outside 0x21–0x7E is an error,
not a written header. Folding: addresses joined with `", "`, and when the
accumulated line would exceed 78 characters the writer breaks at a comma and
continues with CRLF + one space (RFC 5322 §2.2.3 FWS). **No Bcc, ever** — the
struct gains no Bcc field and the builder writes no Bcc header.

**D14 — what was sent is recorded.** `audit_events.args` already carries the
caller's `cc` for `draft_delivery` and `update_delivery` (the executor stores
`Call.Args` verbatim), so those two need no code to be audited — only a test that
pins it. `send_delivery`'s audit args are `{delivery_id}` and cannot carry it, so
the `delivery_sent` task event payload (delivery.go:1336-1338) gains
`"cc": [...]` when the sent Cc set is non-empty — the post-hoc record of who the
message actually went to after D7's send-time drop.

## What a known-address check could have seen (recorded, since D2 closes it)

`normalized_messages` has no recipient columns (id, raw_source_item_id,
thread_id, direction, external_message_id, sent_at, body_text, subject, sender,
channel, links — 0001 + 0017). The `To`/`Cc` of ingested mail exist only inside
`raw_source_items.raw_json.rfc822_b64`, which is unindexed and, above
`MAIL_MAX_MESSAGE_BYTES`, sometimes absent (SWT-64). So a cheap "known address"
rule could have consulted only `normalized_messages.sender` (senders we have
seen) and `person_identities`. Katie's `kevans@cecollaboratory.com` does appear
as a SENDER in collaboratory mail, so a sender-based rule would have admitted the
motivating case — but it would refuse anyone who has only ever been a recipient,
which is a rule that looks like a safety property and is really a coverage
artefact of the ingestion schema. D2 rejects it.

## Acceptance criteria

1. `migrations/0038_delivery_cc.sql` adds `deliveries.cc TEXT[] NOT NULL DEFAULT
   '{}'` plus `deliveries_cc_gmail_check` and `deliveries_cc_shape_check` as
   spelled in D12. Applying it twice is safe (idempotent guards in the style of
   0037).
2. `internal/classify/structure_test.go`'s migration ledger (line ~1210) names 38
   as this ticket's, and a per-ticket guard
   `TestMigration0038_Integration_DeliveryCcShape` asserts the column type, the
   NOT NULL default and both CHECK names against a real database.
3. A pure, exported normalizer in `internal/tools` (e.g. `NormalizeCc(in
   []string) ([]string, error)`) implements D5 in full: parse, address-only,
   domain lower-cased, local part preserved, printable-ASCII-only, ≤254 bytes,
   case-insensitive dedupe first-wins, ≤`MaxCcAddresses` (10), empty element
   refused. It is unit-testable with no database and no network.
4. `draft_delivery` accepts `cc: [string]`. On `channel != "gmail"` a non-empty
   `cc` is refused by name, before any other channel rule.
5. `draft_delivery` stores the normalized list on the new column in the same
   INSERT as body/subject (delivery.go:542-553). An absent, null or empty `cc`
   stores `{}`.
6. `draft_delivery` refuses a Cc equal (case-insensitive, address part) to the
   resolved From or to the thread's latest inbound sender, naming which.
7. `update_delivery` accepts `cc`: absent/null leaves it unchanged, `[]` clears
   it, a list replaces it. `cc` alone is now sufficient — the "nothing to
   update" refusal (delivery.go:749-751) accepts subject OR body OR cc.
8. `update_delivery` applies the same normalization, the same gmail-only rule
   (under the row lock, where the channel is read) and the same From/To
   comparison as `draft_delivery`, and still refuses any row whose status is not
   `drafted`.
9. A Cc cannot change after approval: `update_delivery` is drafted-only
   (delivery.go:795-797) and the dashboard edit form renders only for
   `drafted` rows (deliveries.html:83). A test asserts `update_delivery` with
   `cc` on an `approved` row is refused, and one asserts the approved row's `cc`
   is unchanged afterwards.
10. `tools.DeliveryContentHash` takes the cc (D8); every call site is updated
    (delivery.go approve + reject, dashboard server.go:264), and a test shows
    that adding a Cc after the page render makes an Approve carrying the old hash
    refuse with "changed since it was shown to you".
11. `send_delivery` phase 1 reads `d.cc` in the SAME `FOR UPDATE OF d` SELECT as
    body/subject/thread (delivery.go:1203-1209) and builds the outbound message
    from it inside that transaction — no second read, no read after the lock is
    released.
12. Send-time: any stored Cc equal to the re-resolved To or From is dropped, not
    refused (D7). A test pins both halves (dropped, and the rest still sent).
13. `google.OutboundMessage` gains `Cc []string`; `BuildOutboundMIME` writes
    `Cc: …` after `To:`, folds per D13, and returns an error for any address
    carrying a byte outside 0x21–0x7E. No Bcc field, no Bcc header.
14. The SMTP envelope includes the Cc addresses with NO change to
    `internal/connector/google/smtp.go`: `recipientsFromMIME` (smtp.go:173-200)
    already reads To/Cc/Bcc. A test builds a message with a Cc and asserts
    `recipientsFromMIME` returns To + Cc, deduped.
15. Idempotency is untouched: `sending` + the self-chosen `<sb-…>` Message-ID are
    still committed before the network call, and a row carrying
    `sent_external_id` still refuses forever (delivery.go:1216-1218). A test
    re-runs `send_delivery` on a sent row with a Cc and gets the invariant-4
    refusal.
16. The `delivery_sent` task event payload carries `"cc"` when the sent set is
    non-empty (D14), and `audit_events.args` carries `cc` for `draft_delivery`
    and `update_delivery` (pinned, no new code).
17. The MCP schemas for `draft_delivery` and `update_delivery`
    (internal/mcpserver/schemas.go:64-74) gain a `cc` array-of-string property
    with a description naming: gmail only, one address per entry, display names
    dropped, max 10, and (for `update_delivery`) `[]` clears. Because both tools
    are already in `userProfileTools`, this reaches the user profile with no
    profile change; a test asserts the `cc` property is present in the schema the
    **user** profile lists.
18. Neither profile pin changes. `draft_delivery` stays pinned to
    `require_channel: gmail` + `require_thread_in_task_project` and
    `update_delivery` to `require_own_draft` + `require_channel` for the user
    profile; tool counts stay 27 / 15 / 3 (`serve_test.go:53-58`).
19. The dashboard shows `Cc: …` in the destination cell of every gmail row —
    including `sent` rows, because unlike From/To this is a STORED fact, not a
    re-resolution (the "a sent row's route is not recomputed" rule,
    server.go:186-190, does not apply to it).
20. The dashboard edit form on a `drafted` gmail row carries a `cc` text input
    pre-filled with the current list; saving it with content sets the list,
    saving it empty CLEARS it (D10); an invalid address comes back as the
    handler's flash, not a silent no-op.
21. The drafts worker carries the rejected row's Cc into a redraft (D9), and
    `TestDrafts_Redraft_DraftsThroughTheSameCall` is amended to permit exactly
    `cc` as an extra key and to assert its value.
22. `docs/runbooks/ops-mcp-user-scope.md` records that `draft_delivery` /
    `update_delivery` now take `cc`, and the existing re-install rule (that file,
    lines 237-240: re-run `go install ./cmd/ops-mcp-user` after any merge
    touching `internal/mcpserver` / `internal/tools`) is called out in the
    handoff — on this workstation AND on 192.168.50.30.
23. A kube handoff doc `docs/runbooks/HANDOFF-kube-gmail-delivery-cc.md` names
    migration 0038 (apply to prod BEFORE the roll — merging is not applying), the
    image build, and the two binary re-installs.
24. `go test ./...` and `make integration` are green.

## Data model changes

`migrations/0038_delivery_cc.sql`:

- `deliveries.cc TEXT[] NOT NULL DEFAULT '{}'` — the explicit carbon-copy
  recipients of a gmail delivery, normalized (D5). Empty array = no Cc.
- `CONSTRAINT deliveries_cc_gmail_check CHECK (channel = 'gmail' OR cc = '{}')`
- `CONSTRAINT deliveries_cc_shape_check CHECK (cardinality(cc) <= 10 AND NOT ('' = ANY(cc)))`

No other table changes. No new table (invariant 2). Existing rows get `{}` from
the default, so the migration is a rewrite-free `ADD COLUMN … DEFAULT` on
Postgres 17 and needs no backfill.

## API / MCP tool changes

No new tools. Two existing executor tools gain one argument each; both already go
through the executor path (invariant 3) and keep their policy rules.

**`draft_delivery`** (handler `internal/tools/delivery.go:270`, validator `:124`,
registered on the executor with the rest of the SWT-8 set):

```jsonc
{ "task_id": 12, "channel": "gmail", "thread_id": 34,
  "subject": "Re: Rochester schedule", "body": "…",
  "cc": ["kevans@cecollaboratory.com"] }      // optional; gmail only
```

Hook points: syntax and count validation in `validateDraftDelivery` (pure, args
only); the gmail-only refusal there too, placed beside the `RequireChannel`
check; the From/To comparison and the write in `draftDelivery`, inside the
existing transaction, in the same INSERT.

**`update_delivery`** (handler `:765`, validator `:741`):

```jsonc
{ "delivery_id": 51, "cc": ["kevans@cecollaboratory.com"] }  // replace
{ "delivery_id": 51, "cc": [] }                              // clear
{ "delivery_id": 51, "body": "…" }                           // cc unchanged
```

Hook points: `updateDeliveryArgs.Cc *[]string`; the "nothing to update" rule
relaxed; the channel and status checks already run under the row `FOR UPDATE`, so
the gmail-only rule and the write go there; the UPDATE gains a
`cc = CASE WHEN $n THEN $n+1 ELSE cc END` arm matching the existing
subject/body shape (delivery.go:819-825).

**`approve_delivery` / `reject_delivery`**: no argument change; their
`expect_content_hash` now covers the Cc because `DeliveryContentHash` does (D8).

**Dashboard** (all through the executor, unchanged): `POST
/deliveries/{id}/edit` forwards `cc` per D10.

## MQTT topics

None. This ticket publishes nothing and subscribes to nothing.

## Files likely to touch

- `migrations/0038_delivery_cc.sql` (new)
- `internal/tools/cc.go` (new) — `NormalizeCc`, `MaxCcAddresses`, the From/To
  comparison helper
- `internal/tools/delivery.go` — `draftDeliveryArgs` (+`Cc []string`),
  `validateDraftDelivery`, `draftDelivery` INSERT, `updateDeliveryArgs`
  (+`Cc *[]string`), `validateUpdateDelivery`, `updateDelivery` UPDATE,
  `DeliveryContentHash` signature + its two call sites here, `sendDelivery`
  phase-1 SELECT / `OutboundMessage` construction / `delivery_sent` payload
- `internal/connector/google/send.go` — `OutboundMessage.Cc`,
  `BuildOutboundMIME` header + fold + control-character refusal
- `internal/connector/google/smtp.go` — **no change** (pinned by a test)
- `internal/mcpserver/schemas.go` — `draft_delivery`, `update_delivery` schemas
  and descriptions
- `internal/mcpserver/serve.go` — the Instructions line about client email
  replies (line 28) gains one clause: a session may pass `cc` when Salvador asks
  for one, and never on its own
- `internal/dashboard/server.go` — `deliveryRow` (+`Cc []string`, `CcLine
  string`), `listDeliveries` SELECT + hash call, `actionEdit`
- `internal/dashboard/templates/deliveries.html` — Cc in the destination cell,
  `cc` input in the edit form
- `internal/drafts/store.go` — the redraft LATERAL selects `d.cc` into
  `DeliverTask.RedraftCc`
- `internal/drafts/drafts.go` — the gmail branch passes `cc` on a redraft
- `internal/drafts/worker_test.go` — amend
  `TestDrafts_Redraft_DraftsThroughTheSameCall` (D9)
- `internal/classify/structure_test.go` — migration ledger gains 38
- `docs/runbooks/ops-mcp-user-scope.md`, new
  `docs/runbooks/HANDOFF-kube-gmail-delivery-cc.md`
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` — after delivery, an entry under the
  delivery contract

Test files are the test-author's; the pinned ones that WILL need amending are
`internal/drafts/worker_test.go` (D9) and every current caller of
`tools.DeliveryContentHash` (`internal/tools/userdrafts_test.go`,
`internal/tools/delivery_userdrafts_integration_test.go`,
`internal/tools/gmail_reply_subject_repro_integration_test.go`,
`internal/tools/reject_delivery_test.go`,
`internal/tools/reject_review_integration_test.go`,
`internal/dashboard/deliveries_integration_test.go`,
`internal/dashboard/deliveries_reject_integration_test.go`).

## In scope / out of scope

**In scope:** the `cc` argument on the two tools; the column and migration; pure
validation; the From/To collision rule; the content hash; the dashboard display
and edit; the MIME header and envelope; redraft inheritance; the audit/event
record; the runbook + handoff.

**Out of scope** (each is a thing a session might be tempted to bundle):

- **Bcc.** No field, no header, no argument. A blind copy on an approve-first
  channel is a recipient the review surface cannot show honestly.
- **Reply-all** (inferring a Cc from the thread's own `Cc`/`To` headers). It
  needs recipient columns on `normalized_messages`, which do not exist — that is
  a connector + normalizer ticket, and it would make switchboard choose
  recipients, which D1 forbids.
- **Changing how To is resolved**, and closing the SWT-46 "To can change before
  Send" gap by persisting the route at approval. Related, deliberately untouched;
  D7's send-time drop is the minimum this ticket needs from it.
- **Any standing or automatic Cc rule**: per-project defaults, a `projects`
  column, a capture rule, a prompt rule inside this repo.
- **Cc on any other channel** (upwork_chat, slack_reply, jira_comment,
  calendar).
- **Fixing `actionEdit`'s subject residual** (clearing the subject box silently
  keeps the old subject, IK/SWT-61 residuals). Named in a comment, not fixed.
- **An add/remove Cc verb**, a Cc address book, contact autocomplete.

## Invariants that apply

**1 — Raw-first.** No connector code changes. Our own Cc'd send re-enters through
ingestion exactly as today: the whole RFC822 lands in
`raw_source_items.raw_json.rfc822_b64` (Cc header included) before normalization.
Nothing in this ticket writes a normalized row from provider data.

**2 — One funnel.** A column on `deliveries`, not a `delivery_recipients` table.
No new task-like table; no new status.

**3 — Everything through the executor.** `cc` can reach the database by exactly
two paths — `draft_delivery` and `update_delivery` — and both are executor tools:
validate (`validateDraftDelivery` / `validateUpdateDelivery`) → policy check
(unchanged rules) → audit start (args, including `cc`, recorded) → handler →
audit complete. The dashboard's edit posts through `s.execute` → executor
(server.go:338), not straight to SQL. No new tool, no raw SQL surface, no side
door.

**4 — Nothing external without a delivery row.** The Cc is a field OF the
delivery row, so it is inside the gate rather than beside it. Concretely, four
things must be true in the code:
(a) `update_delivery` refuses any status but `drafted` under the row's `FOR
UPDATE` (delivery.go:785-797), so a Cc cannot change after approval;
(b) the approval is bound to the Cc via `DeliveryContentHash` (D8), so a Cc added
between render and click makes Approve refuse;
(c) `send_delivery` reads `cc` in phase 1's locked SELECT, in the same
transaction that commits `sending` and the reserved Message-ID — the send can
never read a Cc that a concurrent writer changed after the lock;
(d) idempotency is untouched: `sent_external_id` is still reserved before the
network call and a present id still refuses resend forever.

**5 — Own-message loop closure.** Unchanged, and must stay so: the gmail sink
confirms by Message-ID, not by recipients. If a Cc address happens to be one of
our own ingested mailboxes, the cross-account Message-ID dedup (0005's partial
unique index) handles the second copy exactly as it does today. Nothing in this
ticket touches a matcher, a `target_ref`, or `textmatch.NormalizedPrefix`.

**6 — Stealth attribution.** D5's address-only rule is the concrete demand: no
model-chosen display name reaches a client-visible header. Body and subject
scrubbing (`ScrubAIAttribution`) is unchanged, and the Cc list is not scrubbed —
it is not prose and has no lines to drop.

**7 — Orchestrator purity.** The orchestrator is not touched and imports nothing
new. R8 still fires on `delivery_sent`; the payload gains a field and NO rule
branches on it. A test asserting no orchestration decision differs with and
without a Cc is cheap and worth having.

## Sibling patterns to copy

- **The per-channel argument rule and its placement:** `validateDraftDelivery`'s
  `RequireChannel` refusal (delivery.go:132-136) — refuse FIRST, name the real
  reason, never rewrite the caller's value.
- **Optional-vs-absent on an update:** `updateDeliveryArgs.Subject *string` and
  the `CASE WHEN $2 THEN … ELSE subject END` UPDATE (delivery.go:819-825) is the
  exact shape `cc` copies.
- **Validate the value that LANDS:** SWT-61's post-scrub subject re-check
  (delivery.go:487-502) and SWT-64's byte-floor. For `cc` this means the stored,
  normalized address is what the ASCII/length/collision rules see — never the raw
  caller string.
- **A transport-level floor:** `BuildOutboundMIME`'s Subject refusal
  (send.go:63-75) is the model for the control-character refusal on Cc.
- **A schema backstop under a Go rule:** `deliveries_rejected_unsent_check` /
  `deliveries_rejection_fields_check` (0028:14-19).
- **A per-ticket migration guard test plus the ledger line:**
  `TestMigration0037_Integration_AppliesTwiceAndConstrainsAuthType` and
  `internal/classify/structure_test.go:1200-1213`.
- **Dashboard handler/template shape:** rag-svc's HTMX handlers as ever, but the
  local sibling is `actionEdit` + `deliveries.html`'s existing edit form.

## Mutations that must turn a test red

(For the test-author: each of these is a plausible half-implementation.)

1. Drop the `channel = 'gmail'` refusal in the validator → a `slack_reply` draft
   with a Cc must fail a test (and the CHECK must fail the INSERT).
2. Store the caller's raw string instead of `addr.Address` → a
   `"Katie <kevans@…>"` input must show a display name in the stored row and go
   red.
3. Remove the dedupe, or make it case-sensitive → `["a@x.com","A@X.com"]` stores
   two and goes red.
4. Raise/lower the count limit in Go without the CHECK (or vice versa) → a test
   pinning `MaxCcAddresses` against the constraint goes red.
5. Read `cc` in `sendDelivery` with a second query after the transaction → a test
   that changes the row between phase 1 and phase 2 sees the wrong Cc. (At
   minimum, a structural assertion that `cc` is in the phase-1 SELECT list; the
   IK rule from SWT-21 defect 6 applies — **mutate the SELECT to a literal `'{}'`
   and the integration test must go red**, otherwise the test is feeding itself
   the fixture.)
6. Leave `DeliveryContentHash` on (subject, body) → the "Cc added after render"
   approve test goes red.
7. Let `update_delivery` edit an `approved` row → red.
8. Treat `cc: []` as "unchanged" in `update_delivery` (or absent as "clear") →
   both directions must have a test.
9. Write the Cc header before `To:` or without folding → the MIME test goes red
   on byte comparison of the header block and on a 10-address fold case.
10. Drop the Cc from `recipientsFromMIME`'s envelope (i.e. someone "optimizes"
    smtp.go to track To separately) → the envelope test goes red.
11. Drop the redraft inheritance in `drafts.Run` → the amended redraft test goes
    red.
12. Remove the send-time From/To drop → a test with a Cc equal to the
    re-resolved To must show it dropped.

## Verification protocol

Before commit, in this order:

1. `go test ./...` — must be green, and must remain zero-network. The new
   normalizer, the MIME builder and the folding are all covered here.
2. `make integration` — `db-up` + migrate + `go test -tags integration -p 1
   -count=1 ./...`. `-p 1` is not optional: integration suites share the compose
   `ops` database and cross-pollute (IK, "integration suites cross-pollute"). Any
   new global-count assertion joins the mutual-cleanup pact; new suites clean up
   their own rows in FK order scoped by a test-owned actor/slug, and — per the
   SWT-37 landmine — any test writing `mcp:` actors deletes its
   `policy_decisions` + `audit_events` by `task_id` before the shared cleanup.
3. **Migration smoke on an ISOLATED scratch database** — never against the shared
   compose `ops` db while anything else is running, never against prod:

   ```bash
   docker compose exec -T db psql -U ops -d postgres -c 'CREATE DATABASE ops_cc_smoke'
   SCRATCH='postgres://ops:ops@localhost:5433/ops_cc_smoke?sslmode=disable'
   DATABASE_URL=$SCRATCH go run ./cmd/tools/migrate --dir migrations   # twice: second run is a no-op
   DATABASE_URL=$SCRATCH psql "$SCRATCH" -c '\d deliveries'            # column + both CHECKs present
   ```

4. **Manual smoke — draft a gmail delivery with a Cc and inspect the built MIME
   without sending**, against that same scratch db:
   - seed a project, a `human` task, a gmail `normalized_threads` row
     (`gmail:{account_email}:{tid}`) and one inbound `normalized_messages` row,
     plus the matching `source_accounts` row;
   - `DATABASE_URL=$SCRATCH opsctl call draft_delivery '{"task_id":…,
     "channel":"gmail","thread_id":…,"body":"test","cc":["kevans@cecollaboratory.com"]}'`;
   - `psql "$SCRATCH" -c "SELECT id, channel, cc FROM deliveries ORDER BY id DESC LIMIT 1"`
     → `{kevans@cecollaboratory.com}`;
   - `opsctl call update_delivery '{"delivery_id":…,"cc":[]}'` clears it, and
     re-setting it works;
   - refusals read sensibly: a Cc equal to the thread's inbound sender, a Cc on a
     `slack_reply` draft, an eleventh address, `"not an address"`.
   - **The MIME itself:** no gmail sender is wired to `opsctl`, so `send_delivery`
     refuses ("no gmail send adapter wired") — inspect the bytes through the
     integration test that injects a capturing fake sender
     (`go test -tags integration -run '…Cc…' ./internal/tools/`) and read the
     asserted `Cc:` line and the envelope list from `recipientsFromMIME`. Nothing
     in this protocol sends mail.
5. **Dashboard eyeball** (the "usable alone" check): run `cmd/dashboard` against
   the scratch db with `OIDC_ISSUER` unset (dev-login), open `/deliveries`, and
   confirm the drafted row's destination cell reads `From / To / Cc`, that the
   edit box is pre-filled, that emptying it clears the Cc, and that Approve on a
   stale tab (after an `opsctl update_delivery` changed the Cc) refuses with
   "changed since it was shown to you". Check the page TITLE if it looks empty —
   IK's login-redirect landmine.
6. `/ticket-review gmail-delivery-cc` (go-reviewer; this touches executor,
   delivery and send code, so take the codex adversarial pass too) before
   `/ticket-deliver`.

## Deliverables beyond the merge

1. **Migration 0038 applied to prod before rollout.** Merging is not applying
   (IK, imap runbook §234). Every live binary — dashboard, orchestratord,
   pipelined, connectors, `opsctl`, `ops-mcp-user` and every open `ops` session
   — selects `deliveries.*` in places; apply 0038 first, then roll images.
2. **Image build + kube handoff** —
   `docs/runbooks/HANDOFF-kube-gmail-delivery-cc.md`, in the shape of
   `HANDOFF-kube-gmail-reply-subject.md`. The kube session owns the manifests
   (memory: "Kube manifests belong to the kube session").
3. **Re-install the user-scope MCP binary on BOTH machines.** This ticket touches
   `internal/mcpserver` and `internal/tools`, which is exactly the re-install
   trigger in `docs/runbooks/ops-mcp-user-scope.md` (lines 237-240) and IK
   ("Re-run `go install ./cmd/ops-mcp-user` after any merge touching …"):

   ```bash
   cd ~/projects/personal/switchboard && git switch main && go install ./cmd/ops-mcp-user
   ```

   on this workstation **and** on 192.168.50.30 (IK, "Second workstation .30
   install"), then open NEW sessions — an open session keeps the old binary and
   its old schema, so it will not know `cc` exists. `make install-skill` is only
   needed if `skills/swb-status/` changes; it does not here.
4. **IK entry** after delivery, under "Delivery contract (shipped in SWT-8)": the
   Cc column, gmail-only, the normalization rule, the hash change, the redraft
   inheritance, and the fact that the no-known-address decision rests on
   approve-first.

## Open questions

None. The one genuine owner decision — whether a Cc address must already be
known to switchboard — was answered directly on 2026-09-21 ("any valid address is
fine, I approve every email anyway") and is recorded as D2. No
`_OPEN_QUESTIONS.md` file accompanies this SPEC.

## Future work

- **If gmail ever becomes auto-send for any category, revisit D2 first.** The
  whole no-known-address argument is that a human reads every Cc before it sends.
  An auto tier on this channel voids it, exactly the way SWT-59 voided SWT-30's
  containment argument without touching its code.
- **Reply-all**, which needs recipient columns on `normalized_messages` (a
  connector + normalizer ticket) and a deliberate answer to "who chooses the
  recipients".
- **Bcc**, if ever wanted, with an answer to how a review surface shows a blind
  copy honestly.
- **Persisting the route at approval** (SWT-46): the real fix for the To that can
  change between approval and Send. It would let D7's send-time drop become a
  refusal, or disappear entirely.
- **A per-project default Cc**, explicitly NOT wanted today; if it is ever
  wanted, it belongs in `projects.policies` with a dashboard-visible indication on
  the draft that switchboard added a recipient.
- **Fix `actionEdit`'s subject residual** so clearing the subject box surfaces
  SWT-61's refusal instead of silently keeping the old subject.
