> Jira: SWT-65

# task-detail-source-message — the task detail page shows the message the task came from

**STATUS: DECIDED.** No open questions; every choice below is recorded with its rationale under
"Decisions made unilaterally". No `_OPEN_QUESTIONS.md` file accompanies this SPEC.

**Evidence status.** Every code fact below was read in the worktree
`/home/salvo/projects/personal/wt/taskdetail` (branch `ticket-task-detail-source-message`, from
`main` at 9180f16). The production counts quoted under Decision 1 were measured by the coordinating
session against the live `ops` db on 2026-09-17 and are used as SHAPE evidence only. They are never
asserted as literals in a test — that corpus is live (SWT-19's rule).

## Source

Ad-hoc, from Salvador, 2026-09-17, verbatim:

> when I click on somethin like this http://switchboard.home.arpa/tasks/225 after what is already
> there I want to see the full message. there is just not enough info there

Tracked as swb task 226. He reads this page on a tablet at about 1000px CSS width, and on the
desktop through `kubectl -n ops port-forward svc/dashboard 8085:80`.

## The problem, from the live system

`GET /tasks/{id}` (`s.showTask`, `internal/dashboard/board.go:488`, template
`internal/dashboard/templates/task.html`) renders `tasks.body` as the whole story of the task. For a
mail-derived task that body is the promoter's deterministic extract card — `promote.taskBody`,
`internal/promote/store.go:604` — which is, by construction, a POINTER:

```
kind / sender / subject / sent_at / normalized_message_id / ai_extraction_id / verdict [/ link]
```

Two live examples:

- **Task 225** (project 6 `personal`, `source_thread_id` 319411), "Debit card used - $100.00 on
  September 16, 2026": the card names Bank of America alerts as the sender and carries a
  `click.ealerts.bankofamerica.com` tracking `link`. It does not say which merchant or which card,
  because those words are in `normalized_messages.body_text` of message 317070, which the page never
  reads.
- **Task 220** (project 4 `collaboratory`, `source_thread_id` 144501): the card carries ONE of Mike
  Gruszewski's two questions. The second ("can custom fields be created on an object through the
  API?") exists only in the mail body. A reader working from the card answers half the mail.

Capture-created tasks are better off but still short: `internal/capture/rules_store.go:1722` appends
`textmatch.NormalizedPrefix(rulesPreview(msg), rulesPreviewLen)` — 400 runes, whitespace-collapsed —
under the same header block.

**The card being a pointer is correct and this ticket does not change it.** Invariant 1 puts the mail
in `raw_source_items` / `normalized_messages`; the task points at it. This ticket makes the dashboard
FOLLOW the pointer at render time. Nothing is copied into `tasks.body`, and no schema changes.

## Goal

On `GET /tasks/{id}`, when the task can be linked to a normalized message, render that message's full
stored text — plus the rest of its conversation, collapsed, and its attachment manifest — as escaped,
inert, never-auto-linked text below the existing card, and render the page exactly as it does today
when no message can be linked.

**Usable alone:** one dashboard image roll, no migration, no new route, no new env var. After it,
opening `/tasks/225` answers "which merchant, which card" and `/tasks/220` shows both of Mike's
questions, without leaving the page.

## Decisions made unilaterally

### D1 — how the page finds the message

Measured on production, 2026-09-17, over the **51 open tasks**:

| link | open tasks carrying it |
|---|---|
| `tasks.source_thread_id` | 18 |
| a `classify_promotions` row with `task_id` = the task | 7 |
| a `capture_decisions` row with `task_id` = the task | 11 |
| `tasks.surfaced_by_message_id` | 0 |
| **no link at all** | **33** |

18 + 7 + 11 = 36 against a complement of 18, so **the three usable links overlap** and a precedence is
required, not a `COALESCE` of independent columns.

**Precedence — first branch that resolves wins:**

1. **`classify_promotions`**: the lowest-id row with `task_id = $1` **and `action IN ('task','review')`**
   → its `normalized_message_id`. This is the most precise claim in the database: the row exists
   *because* that message made that task (`promote.claim` / `promote.recordTask`,
   `internal/promote/store.go:459,479`). The `action` filter is load-bearing — `action='attached'`
   rows also carry `task_id` (`classify_promotions_action_check` allows `task`/`review`/`attached`)
   and name a LATER message that joined an existing task, not the one that raised it.
2. **`capture_decisions`**: the lowest-id row with `task_id = $1` **and `action = 'task'`** → its
   `message_id`. Same reasoning against `action='task_log'`, which is capture's later-message append
   (`rules_store.go:1515`). Mode is not filtered: a shadow pass creates nothing, so
   `task_id IS NOT NULL` already implies the live decision.
3. **`tasks.source_thread_id`**: the thread's **latest inbound** message. A thread is not a message,
   so this branch picks one, and it picks the one a reply would answer — the same predicate and order
   `tools.latestInboundMessage` uses (`internal/tools/delivery.go:1405`:
   `direction='inbound' ORDER BY sent_at DESC, id DESC`). See D8 for how the two are kept to one
   spelling.
4. **Nothing resolves** → see D2.

**When more than one branch qualifies**, branch 1 wins over 2 wins over 3, and the OTHER candidates do
not appear anywhere on the page. Rationale: 1 and 2 are statements about *this message caused this
task*; 3 is a statement about a conversation. A weaker claim never displaces a stronger one, and
showing two "source" blocks would make the page answer a question he did not ask.

**Which thread is shown** is always the RESOLVED MESSAGE's own `normalized_messages.thread_id`, never
`tasks.source_thread_id`, even when both are set and disagree. One rule, no reconciliation: the
section is "this message and its conversation".

**Messages that `capture_decisions` names with `action='task_log'`** appear only if they happen to sit
on the resolved message's thread (in which case the thread block already shows them). They are not
gathered separately. Future work names the alternative.

**`tasks.surfaced_by_message_id` is deliberately NOT in the precedence.** Two reasons, both
disqualifying on their own:
- It is a *revive* marker, not a provenance pointer. Migration 0030 (SWT-45 Jira activity revive, and
  SWT-36's dismissal revive) writes the message that **resurfaced a closed task**. Answering "what is
  this task about" with it would name the message that reopened the task, a different claim.
- 0 of the 51 open tasks carry it, so as a primary link it is inert on today's board — the SWT-21
  landmine ("a guard whose column no query selected") in the shape of a display.

### D2 — the no-link branch is the COMMON case, and it renders exactly as today

33 of 51 open tasks — hand-made (`create_task` from a session or opsctl) and plan-derived — have no
link at all. When branch 4 is reached the page must be **byte-identical to today's**: no heading, no
`<details>`, no "(none)", no muted "no source message" line, no error, no extra `<h2>`. The section is
a single `{{if .SourceMessage}}` block and nothing outside it changes.

This is not an edge case to be handled; it is the majority rendering, and its integration test carries
the same weight as the one with a message (criteria 16 and 17).

### D3 — one message in full, the rest of the thread collapsed

- **The source message renders in full, verbatim, inline** — `normalized_messages.body_text` as
  stored, no quote detection, no CSS clipping, no per-paragraph fold. Cap: `sourceBodyCap` = **262144
  characters** (`left(body_text, $n)`), and when the stored body is longer the page prints one
  explicit marker line naming the character counts. Message 315655's 33,465 characters render whole.
- **Every other message on the thread** renders in its own CLOSED `<details>`, under one
  `Thread (N more)` heading. Summary line: direction, sender, `sent_at`, then the first 120 runes of
  the body via `textmatch.NormalizedPrefix` (the ONE truncation spelling, SWT-16). Body inside, cut at
  `tools.MailThreadBodyCap` (8 KiB, D8) with the same marker.
- **Ordering:** ascending `sent_at NULLS LAST, id` — byte-identical to `mailReadThread`'s
  (`internal/tools/mail.go:201`). Count cap `tools.MailThreadMaxMessages` (50), also
  `mail_read_thread`'s, with a marker when the thread is longer. The source message is excluded from
  this list by id and never shown twice.
- **`<details>` is this UI's idiom already** — the board's Advanced filter and per-row `actions`
  popups (SWT-57). No `<details>` is rendered `open`.
- **`task.html` has no `<script>` today and must still have none.** The whole section is
  server-rendered markup; nothing here needs JS. (The board's one inline script lives in `tasks.html`
  and postpones auto-refresh while a `<details>` is open — the detail page has no auto-refresh at
  all, so there is no interaction to preserve.)

**Why the source body is not folded or stripped at all:** his complaint is "not enough info there".
Any fold I choose can hide the one line that matters, and D5 records that in at least one real message
the second question sits BELOW a long quoted block. A long page is scrollable; a hidden question is a
half-answered email. Only the 256 KiB safety cap can drop text, and it says so when it does.

### D4 — untrusted text: escaped, inert, never auto-linked

These bodies are other people's mail, arriving from IMAP, Slack, Upwork and Jira. Non-negotiable for
this section:

- Every value reaches the page through `html/template`'s contextual escaping. `template.HTML`,
  `template.HTMLAttr` and any `safeHTML` funcmap entry are banned on the whole path, pinned by a new
  structure test in the shape of `TestTasksTemplate_NoBranchNoHTMXNoRawHTML`
  (`board_layout_structure_test.go:758`) and `TestDashboard_NoRawHTMLOnTheSessionPath`
  (`board_lights_structure_test.go:520`).
- **No auto-linking of URLs.** A URL in a body is inert text he selects and opens deliberately. Task
  225's card already carries a `click.ealerts.bankofamerica.com` tracking link; turning bank-alert
  URLs into one-tap anchors on his own dashboard is a phishing surface he did not ask for, and
  linkifying arbitrary text is where the next injection lands.
- **No remote images, no HTML-mail rendering.** Only `body_text` is read. The `text/html` part is
  never fetched, and `img src` is never extracted anywhere in this system (SWT-25).
- **No attribute takes a body-derived value**, so there is no `href`, `src`, `title` or `on*` sink to
  escape out of. The section's markup is delimited by
  `<!-- source-message: begin -->` / `<!-- source-message: end -->` so a structure test can slice it
  and assert the absence of `href`, `src`, `<img`, `<iframe`, `<script` and `on…=` inside it
  (criterion 12).

### D5 — quoted chain: collapsed by position, never stripped

No quote stripper. Not for `>` lines, not for `On <date> <someone> wrote:`, not for
`-----Original Message-----`. The source message's body is verbatim.

Weighed and rejected: a stripper is a heuristic over other people's mail clients, and its failure mode
is deleting the sentence the task exists for. Mike Gruszewski's second question sits below a long
quoted block in at least one message on thread 144501 — exactly the text a naive
"cut at the first quote marker" rule removes. Collapsing is reversible in one tap; stripping is
invisible.

The quoted chain is still *managed*, by position rather than by parsing: the bulk of a thread's
repetition lives in the OTHER messages, and those are the ones behind `<details>` (D3).

### D6 — attachments: names, sizes, availability, and the refetch hint. Never content.

The section lists the source message's attachment manifest when the message is gmail:

| column | source |
|---|---|
| filename | `google.Attachment.Filename` |
| type | `.ContentType` |
| size | `.SizeBytes`, suffixed `(encoded)` when `.SizeIsEncoded` |
| stored | `.Available`, else `.UnavailableReason` verbatim |

Plus, when `google.SourceInfo.Truncated` or `SourceInfo.UnavailableReason` is set, one muted line
carrying that reason and naming the repair: `opsctl mail refetch` (SWT-64). That reason is how the
"not stored: message was over the … cap" rows explain themselves today, and since SWT-64 they have a
fix worth naming at the point of frustration.

**No content, no download link, no byte is served.** The dashboard never calls
`google.ReadAttachment`, never writes the attachment cache, and adds no route. Reading an attachment
stays `mail_read_attachment` over MCP.

Gmail only: `google.ListAttachments` walks the IMAP `rfc822_b64` envelope
(`internal/connector/google/attachments.go:93`), and a slack / upwork / jira `raw_json` has no such
envelope. Non-gmail source messages render no attachment sub-section at all (not an empty one).

### D7 — any channel, not gmail only; the heading names the channel

Four connectors write `normalized_messages`, each stamping `channel`: `gmail`
(`google/sink.go:271`), `slack` (`slackweb/sink.go:241`, `slackweb.Channel`), `upwork`
(`upworkcrm/sink.go:256`) and `jira` (`jira/sink.go:231`). Every column this section reads —
`sender`, `subject`, `sent_at`, `direction`, `body_text`, `thread_id` — is written by all four. So the
section renders **any** normalized message; restricting it to gmail would hide exactly the Slack task
he is most likely to be short of context on.

The heading comes from one pure Go function, `sourceMessageHeading(channel string, viaThread bool)`:

| channel | branch 1 or 2 | branch 3 (via `source_thread_id`) |
|---|---|---|
| `gmail` | `Source email` | `Latest email on the source thread` |
| `slack` | `Source Slack message` | `Latest Slack message on the source thread` |
| `upwork` | `Source Upwork message` | `Latest Upwork message on the source thread` |
| `jira` | `Source Jira comment` | `Latest Jira comment on the source thread` |
| anything else / empty | `Source message` | `Latest message on the source thread` |

The branch-3 wording is not cosmetic: that branch picks a message the database never claimed raised
the task, and the heading must not overclaim. **The template never branches on the channel** — same
discipline as "the light is Go, not template" (SWT-52).

### D8 — no second spelling of an existing rule

Three constants/fragments move rather than get re-typed:

- `internal/tools/mail.go:32-33` `mailThreadMaxMessages` / `mailThreadBodyCap` are **exported** as
  `tools.MailThreadMaxMessages` / `tools.MailThreadBodyCap`; `mailReadThread` uses the exported names.
  The dashboard uses them for the collapsed thread block, so the MCP thread read and the dashboard
  thread read cut at the same width by construction.
- A new exported order fragment `tools.LatestInboundOrder = "sent_at DESC, id DESC"`, used by
  `latestInboundMessage` (`delivery.go:1411`) AND by the dashboard's branch-3 subquery. This is the
  `tools.TaskQueueOrder` precedent exactly (an exported SQL order fragment the dashboard shares with
  the spine, SWT-52/57).
- `sourceBodyCap` (262144) stays a dashboard constant and is deliberately NOT collapsed into
  `MailThreadBodyCap`. Same shape as SWT-64's two cap numbers: one bounds a model's context window,
  the other bounds one human's page. Its doc comment must say so, or the next session will "unify"
  them.

### D9 — privacy: the SWT-21 locality gate does not apply here, and must not be added

The mail tools (`mail_list_attachments`, `mail_read_attachment`) run every message through
`mailClassJudge` (`internal/tools/mailattach.go:256`) and refuse or count-only anything that is not
`ClassGeneral`. **That gate exists to keep client mail away from a HOSTED MODEL.** It is a boundary on
where text may TRAVEL, not on who may read it.

This page is Salvador's own dashboard: behind `s.auth.Require`, reachable only by
`kubectl -n ops port-forward` (the Ingress block in `kube/switchboard/dashboard.yaml` is commented out
while `OIDC_ISSUER` is unset), rendering his own mailboxes to his own eyes. He may read his own mail.
Applying `mailClassJudge` here would blank out `personal` and every `local_only` project — the exact
tasks that prompted the request (225 is `personal`).

**Nothing in this ticket exposes mail to a model or to any other reader.** No MCP tool is added or
widened; no tool schema, profile or pin changes; no LLM is called; the page writes nothing; the new
`internal/tools` helper (D10) is ungated by design and its callers are pinned to `internal/dashboard`
by a structure test (criterion 14) precisely so that this note cannot be quietly undone.

Say it here so nobody "fixes" it later: **the absence of a locality check on this page is the
decision, not an oversight.**

### D10 — where the attachment walk lives

`internal/dashboard` imports no connector package today (only `internal/tools` and
`internal/orchestrator`), and it should stay that way. A new exported helper in `internal/tools`,
which already imports `internal/connector/google`, is the seam — the `tools.ResolveGmailRoute`
precedent:

```
func MailAttachmentsForRawItem(ctx context.Context, pool *pgxpool.Pool, rawItemID int64)
        ([]google.Attachment, google.SourceInfo, error)
```

Its doc comment states that it performs NO locality judgement and is for the dashboard only.
Criterion 14 pins the caller set.

## Acceptance criteria

Numbered; each testable. "The section" means the markup between the two
`<!-- source-message: … -->` comments in `task.html`.

**Resolution**

1. For a task with a `classify_promotions` row (`task_id` = the task, `action` in `task`/`review`),
   the section shows that row's `normalized_message_id`, even when the task also has a
   `capture_decisions` row and a `source_thread_id` naming different messages.
2. A `classify_promotions` row with `action='attached'` never resolves the source message; a task
   whose only promotion row is `attached` falls through to the next branch.
3. For a task with no promotion but a `capture_decisions` row with `action='task'`, the section shows
   that row's `message_id`. An `action='task_log'` row never resolves it.
4. For a task with neither, but `source_thread_id` set, the section shows the thread's latest
   **inbound** message by `tools.LatestInboundOrder`; an outbound message on that thread is never
   chosen, and a later inbound one supersedes an earlier one.
5. The thread rendered is always the resolved message's own `thread_id`. For a task whose
   `source_thread_id` differs from the resolved message's thread, the collapsed block lists the
   resolved message's siblings.
6. `tasks.surfaced_by_message_id` is read by nothing on this path (structure: the identifier does not
   appear in `showTask`'s reachable source).

**Rendering**

7. The source message renders `sender`, `subject`, `sent_at`, `direction`, and its `body_text` in
   full, inline, verbatim — no quote stripping, no "show more" on the body itself.
8. A body longer than `sourceBodyCap` renders the first `sourceBodyCap` characters plus exactly one
   marker line naming the shown and stored character counts.
9. Every other message on the thread renders inside a CLOSED `<details>`, ordered
   `sent_at NULLS LAST, id` ascending, summary = direction, sender, `sent_at`, and the first 120 runes
   via `textmatch.NormalizedPrefix`; body cut at `tools.MailThreadBodyCap` with the same marker shape.
   No `<details … open` anywhere in `task.html`.
10. The source message never appears twice; a thread longer than `tools.MailThreadMaxMessages` renders
    the cap's worth plus a marker naming the true count.
11. A body containing `<script>alert(1)</script>`, `<img src=x onerror=alert(1)>` and a bare
    `https://click.ealerts.bankofamerica.com/f/a/…` URL renders with all of it escaped as text: the
    response contains `&lt;script&gt;`, contains the URL as text, and the section contains no `<a `,
    no `<img`, no `<script` and no `onerror`.
12. The section's markup contains no `href`, `src`, `<img`, `<iframe`, `<script` or `on…=` attribute
    at all, and `task.html` as a whole still contains zero occurrences of `<script`.
13. `template.HTML`, `template.HTMLAttr` and `safeHTML` appear nowhere on the path
    (`board.go`, the new `sourcemessage.go`, `task.html`).
14. `tools.MailAttachmentsForRawItem`'s only non-test callers under `internal/` are in
    `internal/dashboard` (structure scan), and no MCP-listed tool reaches it.

**The no-link case**

15. For a task with none of the three links, `GET /tasks/{id}` returns 200 and its body is
    byte-identical to the same page rendered by the pre-change binary: no heading, no marker comments,
    no empty `<details>`, no "(none)".
16. The same holds for a task whose only link is a `classify_promotions` row with `task_id` NULL, or a
    `source_thread_id` whose thread has no inbound message — an unresolvable link is the no-link case,
    not an error.
17. `GET /tasks/{id}` for a nonexistent id still 404s (`http.NotFound`, unchanged).

**Channel and attachments**

18. `sourceMessageHeading` returns the D7 table's ten strings; an unknown or empty channel yields the
    generic pair, never a panic or an empty heading.
19. A gmail source message with attachments lists filename, content type, size (with `(encoded)` when
    `SizeIsEncoded`) and stored yes/no — and no bytes, no download link, no new route.
20. A gmail source message whose raw row is truncated shows `SourceInfo`'s reason verbatim plus the
    `opsctl mail refetch` hint.
21. A slack (or upwork or jira) source message renders the body and thread normally and renders NO
    attachment sub-section — not an empty one.

**Cost and scope**

22. `showTask` adds at most **three** statements: one resolution-and-load (always), one thread read
    (skipped when the message has no `thread_id`), one `raw_json` read (gmail with a raw item only).
    The no-link case adds exactly one.
23. No migration, no new route, no new env var, no change to any MCP tool, schema, profile or pin, and
    no write of any kind on this path.

## Data model changes

**None.** No migration. Every column read exists: `tasks.source_thread_id` (0019),
`classify_promotions` (0021), `capture_decisions` (0015), `normalized_messages` /
`normalized_threads` / `raw_source_items` (0001, 0002, 0017).

## API / MCP tool changes

**None.** No MCP tool is added, widened, re-pinned or re-profiled. No executor tool is registered.

Invariant 3 is satisfied by absence, not by routing: this ticket adds **no tool call and no write**.
`showTask` is a read-only handler and stays one — the same standing already documented for
`listSources` / `showFunnel` (`server.go:61-68`). The dashboard's only executor calls remain
`dismissTaskAction`, `closeTaskAction`, the plan actions and the delivery actions, all untouched.

## MQTT topics

None touched.

## Files likely to touch

| Path | Change |
|---|---|
| `internal/dashboard/sourcemessage.go` | NEW. `sourceMessage` / `sourceThreadMessage` structs, the pure `sourceMessageHeading`, `(*Server).loadSourceMessage`, `sourceBodyCap`. |
| `internal/dashboard/board.go` | `taskDetail` gains `SourceMessage *sourceMessage`; `showTask` calls `loadSourceMessage` after the existing reads. |
| `internal/dashboard/templates/task.html` | The `{{if .SourceMessage}}` block between the two marker comments, after `{{if .Body}}` and before `{{if .Children}}`; CSS for the summary line only. |
| `internal/tools/mail.go` | `mailThreadMaxMessages` / `mailThreadBodyCap` → `MailThreadMaxMessages` / `MailThreadBodyCap`. |
| `internal/tools/delivery.go` | `latestInboundMessage` uses the new `LatestInboundOrder` const. |
| `internal/tools/mailattach.go` | `MailAttachmentsForRawItem` (D10). |
| `internal/dashboard/task_detail_structure_test.go` | NEW (criteria 6, 12, 13, 14). |
| `internal/dashboard/task_detail_test.go` | NEW: `sourceMessageHeading` table (criterion 18). |
| `internal/dashboard/task_detail_integration_test.go` | NEW, `//go:build integration` (criteria 1–5, 7–11, 15–17, 19–22). |

Not touched: `boardQuery`, `TaskExportRow`, the CSV/JSON export goldens, `lights.go`, `sections.go`,
`tasks.html`, `server.go` (no route), `auth.go`, everything outside `internal/dashboard` and the three
`internal/tools` files above, and the kube manifests.

## In scope / Out of scope

**In scope:** resolution precedence; the source message rendered in full; the collapsed thread; the
attachment manifest; the channel-aware heading; the byte-identical no-link render; the three shared
spellings of D8.

**Out of scope — named because they are adjacent and tempting:**

- **Changing what the promoter or capture writes into `tasks.body`.** The card stays a pointer
  (invariant 1). Nothing is copied at write time.
- **Rendering HTML mail.** `body_text` only. The `text/html` part is not parsed, not sanitized, not
  displayed.
- **A quote stripper** (D5), and any "smart summary" of a thread. No model is called on this page,
  ever.
- **Attachment download / preview / a `/tasks/{id}/attachments/{n}` route.** Names and sizes only
  (D6).
- **A reply box, a draft button, or any delivery verb on the detail page.** Drafting stays
  `draft_delivery` over MCP and approval stays `/deliveries` (invariant 4, SWT-44).
- **Any board (`/tasks`) change** — sections, lights, filters, auto-refresh, the INCOMING section
  (SWT-59) are all untouched.
- **A locality or redaction gate on this page** (D9), and any change to the MCP mail tools' gate.
- **`tasks.surfaced_by_message_id` as a "resurfaced by" line** (D1, Future work).
- **Backfilling `source_thread_id`** onto the 33 unlinked tasks. 0019 says outright that no backfill
  is possible: nothing recorded which message raised them.

## Invariants that apply

1. **Raw-first** — nothing is ingested. The page READS `raw_source_items.raw_json` (gmail only, for
   the attachment manifest) and `normalized_messages`, both already written by the connectors. It
   writes to neither and normalizes nothing. This ticket is only possible *because* the raw row
   exists: the attachment names come from the stored RFC822 bytes, not from a re-fetch.
2. **One funnel** — no table, no status, no queue. The link is derived at render time from rows that
   already exist; `tasks` stays the one funnel and nothing sprouts a sibling "task_messages" table.
3. **Everything through the executor** — no tool call is added. The page performs no write and no
   side effect, so there is no handler to route. The dashboard's existing executor verbs are
   untouched. The reviewer's check here is negative: grep the diff for `ex.Execute` /
   `executeTask` and find nothing new.
4. **Nothing external without a delivery row** — nothing is sent, and `deliveries` is read only by the
   pre-existing Deliveries table on this page, unchanged. Explicitly: no reply affordance is added
   (out of scope above), so no path from this page can produce outbound text.
5. **Own-message loop closure** — untouched, and consumed in the right direction: an OUTBOUND message
   on the thread (our own send, re-entered through ingestion and matched to its delivery row) is shown
   in the collapsed thread as context, and is never chosen as the SOURCE message by branch 3, whose
   predicate is `direction='inbound'`. Reading our own sends back is what makes thread 144501 legible;
   it must not make one of them look like the thing that raised the task.
6. **Stealth attribution** — nothing client-visible is produced. The page is port-forward-only and
   renders stored text to Salvador. No body is rewritten, so `ScrubAIAttribution` has no role here.
7. **Orchestrator purity** — the orchestrator is untouched, no provider adapter is imported by the
   dashboard (D10 keeps the google import inside `internal/tools`), and no LLM is called.
   `sourceMessageHeading` is pure and unit-tested with no db.

## Sibling patterns to copy

- **A read-only detail section fed by its own statement:** `reopenMarkers` and `boardLightFacts`
  (`internal/dashboard/board.go`) — a separate read rather than widening a pinned query.
- **A pure Go function the template never second-guesses:** `lightFor` / `lights.go` and
  `boardSectionOf` / `sections.go` (SWT-52, SWT-57). `sourceMessageHeading` is the same shape, and its
  test is the same table shape as `sections_test.go`'s `factCombos`.
- **An exported SQL fragment shared between spine and dashboard:** `tools.TaskQueueOrder`, used by
  `boardLightFacts` (`board.go:409`). `tools.LatestInboundOrder` copies it exactly.
- **Thread reading — ordering, caps, column list:** `mailReadThread` (`internal/tools/mail.go:180`).
  Copy the `ORDER BY` and the two caps verbatim (D8), not approximately.
- **The attachment manifest's fields and vocabulary:** `mailAttachListed` / `listedFor`
  (`internal/tools/mailattach.go:406`) and `google.Attachment`
  (`internal/connector/google/attachments.go:30`). Reuse the field names in the table headers so the
  dashboard and `mail_list_attachments` describe the same file the same way.
- **Structure-scan a template by slicing a delimited block:** the `<details>`/`actions` and
  `VerbFormsByteUnchanged` scans in `board_layout_structure_test.go`.
- **Integration harness:** `dashGuard`, `dashPool`, `newDashServer`, `get`, `snippet`
  (`dashboard_integration_test.go:55-250`), `bdInsID` (`board_dismiss_integration_test.go:151`).
- **Mail fixtures with a real RFC822 envelope:** `maEnvelope` and `seedMailAttach`
  (`internal/tools/mailattach_integration_test.go:205,355`).

## Test plan

### A. Structure (`internal/dashboard/task_detail_structure_test.go`, plain unit, no db)

- `TestTaskDetail_NoRawHTML` — `board.go`, `sourcemessage.go` and `task.html` contain none of
  `template.HTML(`, `template.HTMLAttr(`, `template.HTML `, `safeHTML` (criterion 13).
- `TestTaskTemplate_SourceSectionIsInert` — read `templates/task.html` from `templateFS`, slice
  between the two marker comments (fail loudly if either marker is missing: positive control), assert
  the slice contains none of `href`, `src`, `<img`, `<iframe`, `<script`, `onerror`, `onclick`,
  `on…=`; assert the whole file contains zero `<script` and zero `<details … open` (criterion 12).
- `TestTaskTemplate_ThreadIsCollapsed` — the slice contains `<details>` and `<summary` (criterion 9).
- `TestShowTask_DoesNotReadSurfacedBy` — `surfaced_by_message_id` appears nowhere in `board.go` /
  `sourcemessage.go` (criterion 6).
- `TestMailAttachmentsForRawItem_CallersArePinned` — scan non-test `.go` files under `internal/` for
  `MailAttachmentsForRawItem`; the only files naming it are `internal/tools/mailattach.go` (the
  definition) and files under `internal/dashboard/` (criterion 14). Positive control: fail if zero
  call sites are found.
- `TestMailThreadCaps_OneSpelling` — the literals `50` / `8 * 1024` for these caps and the string
  `sent_at DESC, id DESC` appear only at their const declarations under `internal/` (D8).

### B. Unit (`internal/dashboard/task_detail_test.go`)

- `TestSourceMessageHeading_Table` — all ten D7 rows plus `("", true/false)` and `("mastodon", …)`
  (criterion 18). No db, no pgx import.

### C. Integration (`internal/dashboard/task_detail_integration_test.go`, `//go:build integration`)

Slug `itest-taskdetail-proj`; FK-ordered, rerunnable cleanup that deletes `policy_decisions` and
`audit_events` **by `task_id` first** (the SWT-37 landmine), then `classify_promotions`,
`capture_decisions`, `task_events`, `tasks`, `normalized_messages`, `normalized_threads`,
`raw_source_items`, `source_accounts`, `projects`. Guarded by `dashGuard` (refuses 192.168.50.49) and
run against an isolated database.

Seed, in one project:

| Task | Link seeded | Asserts |
|---|---|---|
| T1 | gmail thread T; promotion `action='task'` → M2; ALSO a `capture_decisions action='task_log'` → M3 and `source_thread_id`=T | criteria 1, 3, 5, 7, 9, 10, 11, 19 |
| T2 | the same thread; promotion `action='attached'` only, plus `capture_decisions action='task'` → M1 | criterion 2, 3 |
| T3 | `source_thread_id` = T only; T's messages are M1 (inbound, older), M2 (inbound, newer), MO (outbound, newest) | criterion 4 — resolves M2, never MO |
| T4 | slack thread S, promotion `action='task'` → S1 | criteria 18 (rendered), 21 |
| T5 | **nothing** | criteria 15, 22 |
| T6 | promotion row with `task_id` NULL elsewhere + a `source_thread_id` pointing at an outbound-only thread | criterion 16 |
| T7 | gmail, raw row seeded truncated with a `parts` manifest (`maEnvelope(..., truncated=true, parts)`) | criterion 20 |

M2's `body_text` is built to carry, in one string: a quoted chain (`> …` lines and an
`On 9 Sep 2026, Salvador wrote:` line) with a distinctive sentence **below** it (asserted present —
the D5 regression), the literal `<script>alert(1)</script>`, an `<img src=x onerror=alert(1)>`, and a
`https://click.ealerts.bankofamerica.com/f/a/TRACKINGTOKEN` URL. A separate fixture message exceeds
`sourceBodyCap` for criterion 8. Unusual runes, if any are used, are built with `string(rune(0x…))` —
never pasted and never written as an escape a tool might decode (the Write-tool landmine).

Assertions worth calling out:

- **Criterion 15 is a byte comparison, not a substring check.** Capture T5's rendered page, and assert
  it equals the page rendered with the section forcibly disabled (compare against a golden captured in
  the same run by rendering a task the code path cannot resolve — or, simpler and stronger: assert the
  T5 body contains neither marker comment, no `<details`, and that its `<h2>` set is exactly the
  pre-change set).
- **Criterion 22 counts statements**, in the shape of
  `TestBoardRefresh_Integration_NoExtraQueries`: read `pg_stat_statements` deltas, or wrap the pool —
  whichever that test already does — and assert 1 added statement for T5 and ≤3 for T1.

### D. Smoke — 1000×700 headless Chromium, BOTH ways

Existing pattern: run the dashboard locally against the isolated db (`go run ./cmd/dashboard`, :8085,
`/dev/login?user=salvo`), or post-roll `kubectl -n ops port-forward svc/dashboard 8085:80`; then
Chromium headless at the tablet viewport with a screenshot kept for the delivery summary:

```
chromium --headless --disable-gpu --window-size=1000,700 \
  --screenshot=/tmp/taskdetail-with.png  'http://localhost:8085/tasks/<T1>'
chromium --headless --disable-gpu --window-size=1000,700 \
  --screenshot=/tmp/taskdetail-none.png 'http://localhost:8085/tasks/<T5>'
```

(The session cookie must be carried; if the headless run cannot, use Chrome DevTools' device toolbar
at 1000×700 in the logged-in profile, as SWT-57 and SWT-59 did, and keep the screenshots.)

Check on the WITH shot: the card is still first, the source heading reads `Source email`, the body is
readable without horizontal scroll (`white-space: pre-wrap` already set on `pre`), the tracking URL is
plain text and not tappable, the thread `<details>` are closed, and the attachment table shows names
and sizes only. On the NONE shot: the page ends at Events exactly as today, with no stray heading.

Finally, after the prod roll, open **task 225** and **task 220** on the tablet: 225 must name the
merchant and the card; 220 must show both of Mike's questions.

### E. Mutations that must turn a test red

`go test -overlay` does NOT reach the structure tests — they read source from disk. Perform these as
REAL edits inside a throwaway copy of the worktree
(`tar -C /home/salvo/projects/personal/wt -cf - taskdetail | tar -C /tmp/claude-1000/.../mut -xf -`),
run, then delete the copy. Never mutate the worktree in place.

| # | Mutation | Must fail |
|---|---|---|
| 1 | Resolution branch 1 selects a literal message id instead of `cp.normalized_message_id` | criterion 1 (COLUMN-level) |
| 2 | Replace `m.body_text` in the load statement with a literal `''` | criteria 7, 11 (COLUMN-level) |
| 3 | Replace `m.channel` in the load statement with the literal `'gmail'` | criteria 18, 21 (COLUMN-level) |
| 4 | Replace `m.direction` with the literal `'inbound'` | criteria 4, 9 (COLUMN-level) |
| 5 | Drop `action IN ('task','review')` from branch 1 | criterion 2 |
| 6 | Drop `action = 'task'` from branch 2 | criterion 3 |
| 7 | Swap branches 1 and 2 | criterion 1 |
| 8 | Branch 3 orders `sent_at ASC` / drops `direction='inbound'` | criterion 4 |
| 9 | Use `tasks.source_thread_id` for the thread block instead of the message's `thread_id` | criterion 5 |
| 10 | Render the body with `template.HTML` | criteria 11, 13 |
| 11 | Wrap bare URLs in `<a href="…">` | criteria 11, 12 |
| 12 | Render the section heading unconditionally (no `{{if .SourceMessage}}`) | criteria 15, 16 |
| 13 | Render the thread messages inline instead of in `<details>` | criterion 9 (structure) |
| 14 | Render `<details open>` | criteria 9, 12 |
| 15 | Call `MailAttachmentsForRawItem` for a non-gmail message | criterion 21 |
| 16 | Add a `mailClassJudge` gate to the load path | criteria 1, 7 (a `personal` fixture goes blank) |
| 17 | Add `surfaced_by_message_id` as branch 0 | criterion 6 |
| 18 | Raise `sourceBodyCap` / lower it to `MailThreadBodyCap` | criterion 8, D8 one-spelling test |

Mutation 2 is the SWT-21(6) pin: the guard's input must come from the COLUMN, and only a test that
makes Postgres produce it can prove that. All four column-level mutations (1–4) are integration
mutations for that reason.

## Verification protocol

Run in order. Do not commit before step 4 passes. Capture each command's exit status separately from
any pipe (`gate-commits-on-test-exit-status`).

**0. Read-only prod pre-checks** (`psql -h 192.168.50.49 -U ops -d ops`, inside
`BEGIN READ ONLY; … ROLLBACK;`). NOT run by the spec session. Paste results into the delivery summary;
assert none of them as literals in a test.

- **0a.** Re-measure D1's table over currently-open tasks. **Gate:** if the no-link group has fallen
  below about half, say so — it does not change the code, but criterion 15's weight rests on it.
- **0b.** Overlap: tasks carrying two or more of the three links, and, among them, how often branch 1
  and branch 2 name DIFFERENT messages, and how often the resolved message's `thread_id` differs from
  `tasks.source_thread_id`. **Gate:** if branch-1/branch-2 disagreement is common (say >20% of the
  overlap), record the ids — the precedence is still right, but the delivery summary should name what
  it is hiding.
- **0c.** `SELECT channel, count(*) FROM normalized_messages nm JOIN classify_promotions cp ON
  cp.normalized_message_id = nm.id GROUP BY 1` — confirm the channel set is within D7's table. An
  unexpected channel means the generic heading fires; not a stop, but record it.
- **0d.** Body size on the messages that would be resolved for today's open tasks:
  `max(length(body_text))` and the 95th percentile. **Gate:** if the max exceeds `sourceBodyCap`,
  confirm the truncation marker wording reads sensibly at that size.
- **0e.** `SELECT count(*) FROM classify_promotions WHERE task_id IS NULL` — the NULL-safety surface
  criterion 16 covers.

**1. Unit:** `go test ./...`. The SWT-48 `TestAttributionTrend_*` flake (20:00–24:00 EDT) is
pre-existing; if it fires, re-run with `TZ=UTC`.

**2. Integration, on an ISOLATED database** (never prod, never the shared compose `ops` — the
2026-09-12 landmine):

```
psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c "CREATE DATABASE ops_taskdetail"
make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_taskdetail?sslmode=disable'
DATABASE_URL='postgres://ops:ops@localhost:5433/ops_taskdetail?sslmode=disable' \
  go test -tags integration -p 1 -count=1 ./internal/dashboard/ ./internal/tools/
```

Run twice (rerunnable), then `go test -tags integration -p 1 ./...` once against the same URL.

**3. Mutations:** every row of section E goes red in the throwaway copy; the copy is deleted.

**4. Smoke:** section D, both shots, against `ops_taskdetail`. Drop the database afterwards.

**5. Review:** `/ticket-review task-detail-source-message`. The go-reviewer's attention goes to
invariant 3 (negative: no new write), the D9 privacy note (it must survive the diff intact), and the
escaping pins.

**6. Deploy — dashboard image only, no migration.**
- Build and push `192.168.50.20:5000/switchboard:<tag>` from here.
- **Manifests are not ours to edit.** Write `docs/runbooks/HANDOFF-kube-task-detail-source-message.md`
  naming the tag; the kube-c7 session owns `kube/switchboard/dashboard.yaml` and rolls it.
- Only `deployment/dashboard` needs the bump. No ordering constraint, no env var, no schema
  dependency: every column read has existed since 0021, and a pre-change binary on the same schema is
  equally fine.
- **Post-roll:** `kubectl -n ops port-forward svc/dashboard 8085:80`, then `/tasks/225` and
  `/tasks/220` on the tablet (section D's final check), plus one hand-made task to confirm the no-link
  render.

**7. Rollback:** roll `deployment/dashboard` back to the previous tag. Nothing is written and no
schema changed, so rollback is instant and lossless; URLs are unchanged and no data needs repair.

## Future work (not this ticket)

- **All attached messages, not just the thread.** `capture_decisions` with `action='task_log'` can
  name messages on other threads; a second collapsed list would show them (D1).
- **A "resurfaced by" line** when `tasks.surfaced_by_message_id` is set — a distinct, honestly-labelled
  claim, not a source pointer (D1).
- **Newest-first thread paging** for threads over `MailThreadMaxMessages`; today the cap keeps the
  oldest 50, matching `mail_read_thread` (D3).
- **A per-message attachment manifest** for the collapsed thread messages, if he ever wants it — one
  `raw_json` read per message is the cost that kept it out.
- **Bidi/control-character presentation.** U+202E and friends in a stored body can visually reorder
  the rendered line. Not stripped here: it is his own mail, the effect is cosmetic, and rewriting a
  stored body would misrepresent what was received. A `unicode-bidi` isolation on the `<pre>` is the
  cheap fix if it ever bites.
- **A Content-Security-Policy header** on the dashboard (`img-src 'none'`, `script-src 'self'
  'unsafe-inline'` for the board's one script) — belt and braces for every page, not just this one.
- **A reply affordance** from the detail page (`draft_delivery` prefilled). Deliberately out of scope:
  it crosses into invariant 4's delivery path and deserves its own ticket.
