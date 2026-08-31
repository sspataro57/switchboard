> Jira: SWT-25
> Jira description is SPLIT: this SPEC is ~47k characters and Jira's
> description field caps at 32,767. The issue carries everything up to
> "In scope / Out of scope"; the remainder is a comment on the issue. A
> re-sync must split at an h2 boundary again, not truncate. This file is the
> authoritative copy.

# link-preservation — anchors extracted at normalize time, offered to the model as a closed numbered set

**Status: FINAL.** No open questions arose; the design was settled on the Jira
issue before this SPEC and is carried forward verbatim. The two places where this
SPEC had to choose something the design did not name are in "Decisions made
unilaterally" — argue them there, not by reopening the design.

## Source

Not a build-order step. Ad-hoc, descoped out of SWT-22 on 2026-08-29 after
measuring where links actually live.

> Salvador, 2026-08-28: "what we need to do is preserve the links on the
> summaries".

The design, settled and not to be reopened:

> Extract anchors DETERMINISTICALLY in the application at NORMALIZE time. Offer
> the model a numbered, closed set of anchor TEXTS. The model returns
> `link_index` — integer or null — and the application resolves the index back to
> a URL. Out-of-range index rejected. The model must never author a URL;
> returning an index makes that structural. Spine/leaves applied literally.

Extraction rules, measured over 400 personal messages:

> - Anchors only (`<a href>`). NEVER `img src` — the only "link" in a Pines First
>   Notice is an img pointing at `/wf/open`, a SendGrid open-tracking pixel;
>   following img src puts a tracking beacon on a task.
> - Drop by URL: unsubscribe, opt-out, privacy, terms, preferences, `/wf/open`,
>   asset/CDN hosts.
> - Drop by ANCHOR TEXT too (the URL filter alone leaves "Unsubscribe",
>   "Privacy", "Terms of Use", "View in Browser", "here"). Drop EMPTY anchor text
>   (image wrappers).
> - Result: median 2 candidates/message (down from median 4, mean 12
>   unfiltered); 288 of 400 land at 1–3. Survivors are the real calls to action
>   (VIEW DETAILS, VIEW ACCOUNT, the portal.pinespropertymanagement.com link).
> - Cap at 8 so marketing mail cannot flood the prompt.

Predecessor: `docs/tickets/local-classifier_SPEC.md` (SWT-22, delivered),
criterion 19b, which states why this could not ship there.

## Premises, verified against this repo before writing

Line references are against the branch this SPEC was written on. Counts that came
from the production db are quoted from the SWT-22 record and are **not** re-derived
here — re-measure them at verification time; the corpus is live and a frozen
literal cries wolf every day a message arrives (SWT-19's lesson).

1. **The normalizer really does drop hrefs, and here is the line.**
   `internal/connector/google/rfc822.go:309` defines `htmlTagRE =
   regexp.MustCompile("(?s)<[^>]*>")` and `stripHTML` (317-332) applies it, so
   `<a href="https://…">VIEW DETAILS</a>` becomes `VIEW DETAILS`. The anchor text
   survives; the href does not. `img src` never survives either, which is why the
   SendGrid pixel is invisible in `body_text` today and must stay invisible.
2. **The HTML part is often not even looked at.** `extractBodyText`
   (rfc822.go:220-231) takes `walkForText`'s `(plain, html)` pair and strips the
   HTML **only when plain is empty** (227-229). So for a `multipart/alternative`
   message the whole HTML part — every anchor in it — is discarded before
   `stripHTML` is ever reached. Link extraction must read the part that
   `extractBodyText` throws away, not the text that survived.
3. **There are TWO live mappers, chosen by the raw item's external-id prefix, not
   by `MAIL_SOURCE`.** `Normalize` (normalize.go:260-301) dispatches
   `gmail:` → `NormalizeGmailMessage` (67), `imap:` → `NormalizeRFC822`
   (rfc822.go:62), `calendar:` → events. `MAIL_SOURCE` is read only in
   `cmd/connectors/google/main.go:94`, inside `if !normalizeOnly` — it selects an
   INGEST path and has no effect on normalize at all. Production is `imap`
   (CLAUDE.md build-order step 7); the bridge path writes gmail-API-shaped raw and
   is normalized by `NormalizeGmailMessage` (`bridge_ingest_test.go:411`).
4. **Re-normalization is the backfill, it already exists, and it is idempotent by
   design.** `cmd/connectors/google --normalize-only --all` (main.go:37-38,
   74, 91) skips ingest entirely and needs no `OPS_TOKEN_KEY`; `pendingRaw(all=true)`
   (sink.go:183-194) returns every non-superseded google raw row;
   `upsertMessage` writes `ON CONFLICT (raw_source_item_id) DO UPDATE`
   (sink.go:263-267). **Message ids are therefore stable across a re-normalize** —
   nothing is deleted and re-inserted — so `capture_decisions` (0015, `ON DELETE
   CASCADE` on `message_id`), `ai_extractions`, and the eval labels in
   `docs/evals/personal-actionability.jsonl` all survive it.
   The replay paths were written for this: sink.go:593-595 says so about
   `confirmed_at IS NULL`, and `confirmDeliveryByBodyPrefix` carries the SWT-16
   attempt-time floor (sink.go:562-563).
5. **`body_text` is the input to a post-hoc identity matcher, so changing it is
   not a cosmetic risk.** `confirmDeliveryByBodyPrefix` (sink.go:542-543) computes
   `textmatch.NormalizedPrefix(nm.BodyText, mailMatchPrefixLen)` and compares it
   to the stored delivery body. Institutional knowledge: **google has no
   reconciler**, so a refusal is SILENT — the delivery sits unconfirmed and
   nothing surfaces it. Whatever this ticket does to the extractor, `body_text`
   must come out byte-identical.
6. **The classifier reads a structured field or nothing.** `classify.PendingMessage`
   (`internal/classify/classify.go:79-100`) is filled by one query,
   `inboxSelect` (`internal/classify/store.go:71-75`), shared by `PendingMessages`
   and `MessagesByID` — so a column added there reaches both `run` and `eval` with
   no second spelling. `renderUser` (classify.go:403-411) is likewise shared by
   `Run` and `Eval` (eval.go:126), which is what makes re-running the SWT-22 eval
   a real delta measurement rather than a different experiment.
7. **The four-field contract is mechanically pinned in three places**, and all
   three fail the moment `link_index` is added — which is the point:
   `internal/classify/prompt.go:15` ("the output contract: four fields, nothing
   else"), `VerdictSchema` (33-46) with `additionalProperties:false`, and
   `internal/classify/structure_test.go:183-250`
   `TestSchema_MatchesTheOutputContract`, which asserts the exact property set and
   errors on `schema has an extra property`. Plus
   `docs/tickets/local-classifier_SPEC.md` criterion 18.
8. **SWT-22 also pinned "no migration in this ticket".**
   `internal/classify/structure_test.go:588-618` `TestNoMigrationWasAdded` fails on
   any `migrations/00NN_*.sql` with `NN > 16`. It is correct for SWT-22 and wrong
   for this one; it must be REPLACED, not deleted (criterion 26).
9. **Migration numbering.** `migrations/` runs 0001…0016, highest
   `0016_provider_locality.sql`; this ticket's is **0017**. The runner keys on
   `schema_migrations.version` with no checksum, so an edited applied file is
   skipped silently — forward-only, no exceptions.
10. **The jsonb-on-a-normalized-row shape already exists in this schema.**
    `normalized_events.attendees JSONB NOT NULL DEFAULT '[]'`
    (`migrations/0001_initial.sql:73`) and `normalized_threads.participants` are
    the precedent for "an ordered list of small structured things belonging to one
    normalized row".
11. **`golang.org/x/net` is already in the module graph** with a full `h1:` hash
    in `go.sum:45`, so `golang.org/x/net/html` costs a move from the indirect to
    the direct require block and no new module.
12. **Nothing in `internal/classify` imports a connector package** today, and
    nothing should start (see criterion 14's seam decision).

## Goal

Make the URL a personal message points at survive normalization and reach the
classifier's verdict, without the model ever being in a position to author one:
the normalizer extracts anchors from raw at normalize time into
`normalized_messages.links`, the prompt offers anchor TEXTS by number, and the
application resolves the number back to a URL.

**Usable alone.** After this ticket, with no new hardware and no cluster change:

1. `classify run` emits verdicts that carry the link a person would open —
   `ai_extractions.fields.link_url` — and `classify report` prints it, so a
   flagged HOA or bank notice is actionable from the report instead of sending the
   reader back to the mailbox.
2. The link is a fact about the message, not about the run: it lives in
   `normalized_messages.links`, extracted deterministically from raw, and is
   available to every future consumer (triage, drafts, the dashboard) without
   another MIME decoder.
3. The historical corpus is covered, not just new mail: one operator command
   (`--normalize-only --all`) backfills every already-normalized message from raw,
   which is exactly the reprocessability invariant 1 exists to guarantee.
4. The SWT-22 eval can be re-run on the same 280 labels for a before/after
   number, since the labels, the ids and the prompt path are unchanged apart from
   the candidate list.

## Acceptance criteria

Numbered and testable. 1-10 and 14-24 are unit tests with no db and no network;
11-13 and the seam test are integration.

### The extractor

1. New file `internal/connector/google/links.go` exposing a PURE function
   `ExtractLinks(htmlBody string) []Link` and `type Link struct{ Text, URL string }`.
   Pure means: no `context`, no I/O, no clock, no randomness — same input, same
   output, forever. A structural test in the shape of
   `internal/capture/rules_structure_test.go`'s `TestRulesGo_IsPure` asserts the
   file imports nothing from `net/http`, `net`, `os`, `database/sql` or pgx, with
   a control that fails if the file is missing so the scan cannot pass vacuously.
2. **Anchors only.** `<a href=…>` and nothing else — never `img src`, never
   `link`, `iframe`, `form action`, `area href`. A fixture in the shape of the
   Pines First Notice (a single `<img src="…/wf/open">` and no anchors) yields
   ZERO candidates, and the failure message says why: that img is a SendGrid
   open-tracking pixel, and following it would put a tracking beacon on a task.
3. Scheme allowlist: `http` and `https` only, compared case-insensitively after
   trimming whitespace and decoding HTML entities. `mailto:`, `tel:`,
   `javascript:`, `data:`, a bare fragment (`#…`) and any host-less relative URL
   are dropped. A URL longer than 2048 bytes is dropped (a `data:` payload that
   sneaks past the scheme check would otherwise be stored in a row and rendered
   into a prompt).
4. **Drop by URL.** One package-level list, case-insensitive substring match,
   with the measurement recorded above it: `unsubscribe`, `opt-out`, `optout`,
   `/wf/open`, `privacy`, `terms`, `preferences`, `list-manage`, plus the asset /
   CDN hosts. A table test names one survivor and one victim per entry.
5. **Drop by ANCHOR TEXT.** EMPTY text is always dropped (image wrappers). Then
   an exact match — after lowercasing, collapsing whitespace and trimming
   surrounding punctuation — against a list carrying at least: `unsubscribe`,
   `privacy`, `privacy policy`, `terms`, `terms of use`, `view in browser`,
   `view this email in your browser`, `here`, `click here`, `manage preferences`,
   `email preferences`, `opt out`, and the Spanish spellings
   (`cancelar suscripción`, `darse de baja`, `aviso de privacidad`, `términos`,
   `ver en el navegador`) — 51 of the 1,609 personal messages are Spanish
   (`internal/classify/prompt.go:62-64`), and a filter that is English-only is a
   filter that silently does half its job on them.
   **EXACT match, not substring**, and the test says why: "pay your bill here"
   must survive while a bare "here" must not.
   The comparison is spelled LOCALLY and deliberately not with
   `textmatch.NormalizedPrefix`. Reason, in the code comment: `internal/textmatch`
   is the one spelling of the delivery-identity comparison, and coupling a content
   filter to it means a future change to how our own sends are recognised silently
   changes which links a model is offered.
6. Dedup by URL — first occurrence keeps its text, later duplicates are dropped
   (the image wrapper and the text link under it are one link, and offering it
   twice both wastes an index and makes the list read as two options). Then cap at
   **8**, keeping document order. A 30-anchor fixture returns 8.
7. **Document order is the index basis and it is stable.** A determinism test in
   the shape of `TestNormalizeRFC822_Deterministic` (`rfc822_test.go:416`) runs the
   extractor twice and requires an identical slice. Nothing sorts, nothing ranks,
   nothing scores — the position a model is shown is the position in the message.

### Wiring into the normalizer

8. `NormalizeRFC822` fills `NormalizedMessage.Links` from the message's
   **text/html part, independently of which part won `body_text`** (premise 2).
   The test that pins it is a `multipart/alternative` fixture with BOTH a
   text/plain body and an HTML alternative carrying two anchors: `BodyText` is the
   plain text AND `Links` has two entries. Without this the common case — a
   templated notice that ships plain text and HTML — silently yields no links.
9. `NormalizeGmailMessage` fills `Links` the same way, via a `firstTextHTML`
   sibling of `firstTextPlain` (`normalize.go:133-145`). Not the production path
   today (premise 3), and wired anyway: the bridge and gmail_api raws are
   normalized by this mapper, and leaving it empty makes a future `MAIL_SOURCE`
   flip a silent, error-free loss of every link.
10. **`body_text` comes out byte-identical.** A golden test over the existing
    `rfc822_test.go` fixtures asserts every `BodyText` is unchanged by the
    refactor, and its failure message carries premise 5: `body_text` is the input
    to `confirmDeliveryByBodyPrefix`, google has no reconciler, and a mismatch
    there is a delivery that can never be confirmed with no error anywhere.
    Corollary, in scope terms: **`stripHTML` is not rewritten in this ticket** —
    not "improved", not moved onto the tokenizer.
11. `upsertMessage` (`sink.go:258-271`) writes `links` in the SAME statement as
    the body, in both the INSERT and the `DO UPDATE` list. One statement, so a
    re-normalize never leaves a row with a fresh body and stale links, and
    `--normalize-only --all` refreshes links with no new code path.

### Storage

12. `migrations/0017_normalized_message_links.sql`:
    `ALTER TABLE normalized_messages ADD COLUMN links JSONB NOT NULL DEFAULT
    '[]'::jsonb` plus `CHECK (jsonb_typeof(links) = 'array')`. Forward-only, no
    down. The element shape is `{"text": "...", "url": "..."}` and **the array
    POSITION is the identity** — there is no id, no ordinal column, and nothing may
    reorder the array after it is written.
13. An integration test proves the round trip through Postgres: normalize a raw
    item with the real normalizer, read the row back, assert the array, then
    re-normalize the same raw item and assert the value is unchanged and no
    duplicate row appeared. Rows written by the upwork / jira / slackweb
    normalizers keep `[]` and nothing breaks — asserted, because the CHECK plus
    NOT NULL is exactly the sort of column that turns another connector's insert
    into a runtime error.

### The classifier

14. `classify.PendingMessage` gains `Links []Link` with a classify-local `Link`
    type. **`internal/classify` does not import the connector package** — the
    contract between them is the COLUMN, not a Go type. That is a real seam and it
    gets a real test rather than a shared struct: one integration test normalizes a
    raw item through `google.Normalize` and reads it back through
    `classify.PGStore.PendingMessages`, asserting texts and URLs survive. Two
    structs agreeing by inspection is how two spellings of one fact drift.
15. `inboxSelect` (`store.go:71-75`) gains `COALESCE(nm.links, '[]')`. One
    constant, so `PendingMessages` and `MessagesByID` cannot disagree and `eval`
    scores what `run` classifies.
16. **The column test, not the fixture test.** An integration test seeds a
    `local_only`-attributed message WITH links via Postgres and asserts the
    rendered prompt carries the anchor texts. **Mutate `inboxSelect` to a literal
    `'[]'` and it must go red**; if it stays green the test is proving its own
    fixture. (This repo's seventh landmine, and the one the reviewer will look for
    first.)
17. `renderUser` (`classify.go:403-411`) renders a numbered list of anchor
    **TEXTS ONLY**, 1-based, **after** the body — the body is truncated at 4000
    characters (405-408), so a list placed before it could be eaten by a long
    marketing mail — and renders NOTHING at all when there are no candidates. A
    test asserts the rendered prompt contains none of the candidate URLs.
    Note honestly in the test comment what this does and does not prove: a
    text/plain body may legitimately contain URLs of its own, so the prompt is not
    URL-free in general. The structural guarantee is criteria 18-20, not this.
18. `VerdictSchema` gains `link_index`, `{"type": ["integer", "null"]}`,
    listed in `required`, with `additionalProperties:false` unchanged. **This is
    now a FIVE-field contract**: `{actionable, kind, title, reason, link_index}`.
    `TestSchema_MatchesTheOutputContract` is updated in the same change
    (criterion 26), not left contradicting the code.
19. **There is no URL-typed field anywhere in the output contract**, and a
    structural test proves it rather than a reviewer noticing: no schema property
    whose name matches `(?i)(url|href|link_url|uri)` exists, `link_index` is the
    only new property, and its declared type contains only `integer` and `null`.
    Failure message: the model must never author a URL, and returning an INDEX is
    what makes that structural — a string field named `link` would put the
    guarantee back into prompt discipline, where it does not survive a model
    update.
20. Resolution happens in the application: a pure
    `ResolveLink(links []Link, idx *int) (Link, LinkStatus)` with
    `LinkStatus ∈ {none_offered, not_chosen, resolved, rejected}`. **1-based**;
    `nil` and an absent field are both `not_chosen` (or `none_offered` when the
    list is empty); anything outside `1..len(links)` — `0`, `-3`, `len+1`, `9999`,
    or any index at all when the list is empty — is `rejected` and yields no URL.
    A table test covers all of those. Out-of-range is never an error, never a
    skip, and never fails the message.
21. **Four states are recorded and never collapsed**, on
    `ai_extractions.fields`:

    | situation | fields written |
    |---|---|
    | no anchor survived the filter | `link_candidates: 0`; no `link_url` |
    | candidates offered, model chose none | `link_candidates: N`, `link_index: null`; no `link_url` |
    | model chose k | `link_candidates: N`, `link_index: k`, `link_url`, `link_text` |
    | model chose out of range | `link_candidates: N`, `link_index_rejected: <value>`; no `link_url` |

    **"No candidates" is the COMMON case and must never be an error.** The two HOA
    First Notices have no usable link at all — only the tracking pixel — and
    `link_index: null` is ordinary output. A unit test runs a message with zero
    candidates end to end through `classifyAll` and asserts: verdict recorded,
    `status='ok'`, one extraction, no skip row, no raise.
    The four states exist for the same reason criterion 17 of SWT-22 exists: a
    counter that cannot tell "nothing to offer" from "the model declined" from
    "the model answered nonsense" is an alarm nobody can read.
22. `Stats` gains `Linked` and `LinkRejected`. `classify report` prints the
    resolved URL on each flagged line and one counts line covering the four states
    of criterion 21. A report test over seeded `ai_extractions.fields` asserts a
    flagged row with a `link_url` prints it and a flagged row without one prints a
    placeholder rather than an empty column.
23. The prompt gains ONE short paragraph, in `SystemPrompt`: the numbered list is
    the complete set of links available; answer with the number of the one a
    person would open to act on this message; answer `null` when none of them is
    that, or when no list is shown; never invent a number. The existing
    `TestPrompt_StatesTheObjectiveAndTheAttachmentCeiling` gains an assertion that
    the prompt mentions the numbered list and `null`.
    The attachment clause (criterion 19 of SWT-22) stays exactly as it is —
    "defer to the attachment" and "defer to the portal" are different shapes and
    the second one is now answerable.
24. **Nothing fetches a link, anywhere, ever.** No HTTP request is added to the
    diff outside the existing ollama adapter. The structural scan of criterion 1
    covers the extractor; a second assertion covers `internal/classify` (it has no
    HTTP import today and must not grow one).

### The eval, the contradiction, and the docs

25. Re-run the SWT-22 eval **unchanged** — same 280 labels, same file, same
    command — and record the result as a SECOND ROW in the table at
    `docs/runbooks/local-classifier.md:140-142`, with the date, alongside the
    2026-08-30 `0.83 / 0.58` baseline. The note under it names what moved for the
    two content-behind-a-link false negatives the runbook already lists — 27871
    (a doctor's-office portal message, content behind login) and 84710 (a portal
    notice built from an unfilled template) — and says plainly if they did not
    move. Expected drift exclusions: **zero**, because re-normalization upserts
    (premise 4) so ids and subjects are stable; any drift printed by the harness is
    a finding to investigate, not a nuisance to re-hash.
    `docs/evals/personal-actionability.jsonl` is NOT edited: no new labels, no
    new keys — `TestLabelsFile_IsIdsAndLabelsOnly` (structure_test.go:443) still
    passes untouched.
26. **The contradictory statements are fixed in the SAME change** (the Jira issue
    warns explicitly that they must not coexist). All five:
    - `internal/classify/prompt.go:15` — "four fields, nothing else" → five, with
      a one-line note that `link_index` is an index into `normalized_messages.links`
      and never a URL.
    - `internal/classify/structure_test.go:203-213` — the `want` map and the
      "criterion 18's contract is {actionable, kind, title, reason}" message.
    - `docs/tickets/local-classifier_SPEC.md` criterion 18 — amended in place with
      a dated pointer to SWT-25.
    - `docs/tickets/local-classifier_SPEC.md` criterion 19b — "DESCOPED to SWT-25"
      gains "DELIVERED by SWT-25 (date)". The rest of 19b stays: it is the record
      of WHY, and it is still true.
    - `internal/classify/structure_test.go:588` `TestNoMigrationWasAdded` —
      **replaced, not deleted**, by a test that pins 0017 as the highest migration
      and requires `0017_normalized_message_links.sql` to exist and to add a
      `links` column to `normalized_messages`. A guard that becomes wrong is
      rewritten to the new truth; deleting it is how the next ticket adds a
      migration nobody notices.
      SWT-22's Jira description is re-synced after the SPEC edit (it carries the
      SPEC; see the description-sync recipe in institutional knowledge).
27. `docs/runbooks/local-classifier.md` gains a **Links** section: where links
    come from (normalize time, raw → the `links` column) and why not from
    `body_text`; why the model returns an index and never a URL; that
    `link_index: null` is ORDINARY and the HOA First Notices have no link at all;
    that `img src` is never extracted and never followed, naming the `/wf/open`
    pixel; the backfill command and that it is idempotent; and how to add a
    drop-list entry (edit the list in `links.go`, re-run `--normalize-only --all`,
    re-run the eval). A structural test in the `TestRunbook_LocalClassifier` shape
    (structure_test.go:513) asserts the section names `link_index`, `img`,
    `normalize-only`, and the never-author-a-URL rule.
28. `.claude/INSTITUTIONAL_KNOWLEDGE.md` gains a short **Link preservation
    (SWT-25)** entry: the column and its position-is-identity rule; the index
    contract; the img-src refusal and why; that `--normalize-only --all` is the
    backfill and is idempotent because the upsert keys on `raw_source_item_id`;
    and the standing rule that `body_text` must never change without checking
    `confirmDeliveryByBodyPrefix`.

## Data model changes

**One migration: `migrations/0017_normalized_message_links.sql`.**

```sql
ALTER TABLE normalized_messages
  ADD COLUMN links JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE normalized_messages
  ADD CONSTRAINT normalized_messages_links_is_array
  CHECK (jsonb_typeof(links) = 'array');
```

- Vocabulary: this extends `normalized_messages`, the canonical object named in
  CLAUDE.md's schema section. No new table name is invented.
- `NOT NULL DEFAULT` on an existing table is catalog-only on PG 11+ — no table
  rewrite, and the corpus is ~49k rows anyway.
- Element shape `{"text": "...", "url": "..."}`, **array position is the
  identity**. Nothing may reorder it after write; the whole array is rewritten by
  the normalizer's upsert on every re-normalize.

**Why a jsonb column and not a `normalized_message_links` table** (the shape
decision the ticket asked for, made rather than deferred):

- **Re-normalization is a whole-value rewrite.** `upsertMessage` is three
  separate statements on the pool with no enclosing transaction
  (`sink.go:247-271`); a child table would need DELETE-then-INSERT, which either
  opens a window where a message has stale or duplicated links, or forces a
  transaction refactor of the upsert path — a change to the code that also
  performs delivery loop closure. One more `SET links = ...` in the existing
  `DO UPDATE` costs nothing and is atomic with the body it belongs to.
- **Ordering is the contract.** A jsonb array has intrinsic order; a table needs
  an ordinal column plus an `ORDER BY` that every reader must remember. The index
  the model is offered is the position — a reader who forgets the ORDER BY
  resolves the wrong URL, silently.
- **Precedent in this schema.** `normalized_events.attendees JSONB NOT NULL
  DEFAULT '[]'` (0001:73) is the same shape for the same reason.
- **Nothing needs to query links relationally.** No join, no filter, no
  aggregate; the classifier reads them in the same row read as the body — drafts'
  one-query pattern, which SWT-21's deviations name as the better one.
- **Cascade hygiene.** 19 test suites clear fixtures with `DELETE FROM
  normalized_messages` (0015:99-105). A child table is one more `ON DELETE
  CASCADE` to get right; a column is none.

Cost, stated honestly: no uniqueness or index on url, so "which messages link to
this host" is a `jsonb_array_elements` scan. Nothing today asks that, and adding
a GIN index later is a forward migration, not a redesign.

## API / MCP tool changes

**None.** No executor tool, no MCP tool, no new agent-facing capability. The
classifier still creates nothing, so it calls nothing through the executor
(invariant 3 has no surface here).

Recorded for the ticket that takes `classify` live, unchanged from SWT-22 and now
with one more clause: when a flagged message becomes a task, it goes through the
existing `create_task` executor tool with a `classify:` actor — and the link it
carries is `ai_extractions.fields.link_url`, a value the application resolved from
its own extraction table, never a string a model produced.

Internal Go surface added (not tools):

```go
// internal/connector/google
type Link struct{ Text, URL string }
func ExtractLinks(htmlBody string) []Link

// internal/classify
type Link struct{ Text, URL string }          // scanned from the column, not imported
type LinkStatus string                        // none_offered|not_chosen|resolved|rejected
func ResolveLink(links []Link, idx *int) (Link, LinkStatus)
```

## MQTT topics

None. No heartbeat, command topic or LWT is touched. `classify` and the google
connector are one-shot CLI passes.

## Files likely to touch

New:
- `migrations/0017_normalized_message_links.sql`
- `internal/connector/google/links.go` (pure extractor + the two drop lists)
- `internal/connector/google/links_test.go` (criteria 1-7)
- `internal/connector/google/links_integration_test.go` (criterion 13)
- `internal/classify/links.go` (`Link`, `LinkStatus`, `ResolveLink`)
- `internal/classify/links_test.go` (criteria 19-21)

Modified:
- `internal/connector/google/rfc822.go` (`NormalizeRFC822` fills `Links`;
  `extractBodyText` must hand back the HTML part it currently discards —
  `stripHTML` itself unchanged)
- `internal/connector/google/normalize.go` (`NormalizedMessage.Links`;
  `firstTextHTML` beside `firstTextPlain`; `NormalizeGmailMessage` fills it)
- `internal/connector/google/sink.go` (`upsertMessage` INSERT + `DO UPDATE`)
- `internal/connector/google/rfc822_test.go`, `normalize_test.go` (criteria 8-10,
  including the byte-identical `body_text` golden)
- `internal/classify/classify.go` (`PendingMessage.Links`, `renderUser`'s
  candidate list, resolution in `classifyAll`, `Stats`)
- `internal/classify/store.go` (`inboxSelect` + the scan)
- `internal/classify/prompt.go` (`link_index` in `VerdictSchema`, the prompt
  paragraph, the "four fields" comment)
- `internal/classify/report.go` (the URL column and the four-state counts)
- `internal/classify/structure_test.go` (criteria 18/19/26 — the schema contract
  and the replaced migration guard)
- `internal/classify/store_integration_test.go` (criterion 16, with the mutation)
- `internal/classify/worker_test.go` (the zero-candidate end-to-end case)
- `go.mod` (`golang.org/x/net` indirect → direct)
- `docs/runbooks/local-classifier.md` (criteria 25, 27)
- `docs/tickets/local-classifier_SPEC.md` (criterion 26)
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` (criterion 28)

Deliberately NOT touched: `internal/connector/{upworkcrm,jira,slackweb}/*` (their
messages keep `links='[]'`), `internal/triage/*`, `internal/drafts/*`,
`internal/capture/*`, `internal/orchestrator/*`, `internal/provider/*`
(the ollama adapter already forwards `Schema` as `format`; a union type is a
schema change, not an adapter change), `internal/textmatch/*`, and
`docs/evals/personal-actionability.jsonl`.

## In scope / Out of scope

**In scope**
- The deterministic anchor extractor and its two drop lists.
- Both google mappers wired to it; `body_text` provably unchanged.
- Migration 0017 and the upsert that writes it.
- `link_index` in the verdict contract, the numbered candidate list in the prompt,
  application-side resolution, out-of-range rejection, the four recorded states.
- The historical backfill, as an operator step using the flags that already exist.
- Re-running the existing eval and recording the delta.
- Runbook, institutional-knowledge entry, and the SWT-22 amendments.

**Out of scope — including what it is tempting to bundle**
- **Taking `classify` live.** Still shadow. Flagged → `create_task` through the
  executor is a later ticket, and having a URL to put on the task does not make
  this that ticket.
- **Fetching, resolving or unwrapping any URL.** No HEAD request to expand a
  tracking redirect, no title fetch, no screenshot. The moment this code makes a
  request it becomes a beacon-follower, and the whole reason `img src` is excluded
  is that we do not do that.
- **Rewriting `stripHTML` on the tokenizer.** It would be tidier and it would
  change `body_text`, which is the input to a silent post-hoc matcher (premise 5).
  If it is ever worth doing it is its own ticket with its own before/after over
  the corpus.
- **Bare URLs in text/plain bodies.** Not extracted; see "Decisions made
  unilaterally".
- **Attachment / PDF extraction.** Unchanged from SWT-22: the Pines fine amount
  lives in a PDF nothing reads, and a link is not a substitute for it.
- **Re-classifying messages already classified.** The inbox filter keys on
  `NOT EXISTS ai_extractions … worker_type='classify'` (store.go:66-69), so
  messages classified before this ticket keep their link-free verdicts. A
  re-classify sweep is a different piece of work; SWT-22 only ever ran `--limit 5`
  plus an eval, and `eval` writes nothing.
- **A dashboard rendering of links.** The report is the surface this ticket ships.
- **Links for non-google connectors.** Upwork/Jira/Slack messages keep `[]`.
  Their bodies are not HTML mail and nothing has asked.
- **SWT-23** (the ~14,500 `unmatched` residue) and the sender-context column.
  Both still theirs.

## Invariants that apply

1. **Raw-first.** This is the invariant that MOVED the work here, and its concrete
   demand is: the anchors are extracted **from `raw_source_items.raw_json`
   (`rfc822_b64`) at normalize time**, in `NormalizeRFC822` /
   `NormalizeGmailMessage`, which are pure functions of the raw row — so the whole
   corpus is reproducible with `--normalize-only --all` and no mailbox. The
   alternative SWT-22 rejected (the classifier parsing raw MIME itself) is a
   second decoder beside the normalizer and stays rejected: **nothing in
   `internal/classify` may read `raw_source_items` or decode MIME.** A reviewer
   should grep for exactly that.
2. **One funnel.** No new table and nothing task-like: `links` is a column on the
   canonical `normalized_messages` row, and the verdict stays an `ai_runs` +
   `ai_extractions` pair discriminated by `worker_type='classify'`. Shadow mode
   remains structural — `classify.Store` gains no write method beyond what it has
   (criterion 15 adds a SELECT column, nothing else), so
   `TestShadow_StoreHasNoTaskWriteMethod` still holds.
3. **Everything through the executor.** No handler and no tool is added, so
   nothing bypasses `Executor.Execute` because nothing is executed. Stated for the
   live ticket: the link reaches a task only through `create_task`, carrying a URL
   the application resolved.
4. **Nothing external without a delivery row.** Nothing sends and nothing is
   fetched. SWT-21's sharper reading still applies to the model call; this ticket
   adds one more edge that could have crossed the line and does not: a URL in the
   corpus is never dereferenced, so no outbound request exists to need a delivery
   row (criterion 24).
5. **Own-message loop closure.** The concrete demand on THIS diff is criterion 10:
   `body_text` must be byte-identical, because `confirmDeliveryByBodyPrefix`
   (sink.go:542-543) identifies our own sends by its whitespace-normalized 120-char
   prefix, google has no reconciler, and a broken match is a permanently
   unconfirmable delivery plus a false `outbound_observed`. Outbound messages get
   a `links` array too; that is inert, and it must not be described as a guard.
   The `--all` backfill re-runs the loop-closure path, which is idempotent by
   construction (`confirmed_at IS NULL`, sink.go:593-595) — verified before the
   backfill, not assumed.
6. **Stealth attribution.** Nothing client-visible is written. `link_url` and
   `link_text` are internal.
7. **Orchestrator purity.** Untouched; it learns nothing about links. No LLM call
   moves, and the new extractor is a pure function with no model in it — the
   deterministic half of "agency at the leaves, determinism at the spine", which
   is the entire architecture of this ticket.

Landmine classes this ticket walks past:

- **A predicate whose input comes from a column, tested with a fixture.** The
  candidate list is fed by `nm.links`; a unit test that supplies `Links` proves
  its own fixture. Criterion 16 puts the regression in the integration suite and
  names the mutation that must turn it red.
- **An alarm that cannot tell "nothing" from "broken".** Criterion 21's four
  states, and criterion 22 making them visible. "No candidates" is the common case
  and must never look like a failure.
- **A comment that states the opposite of its code.** Criterion 26 fixes five
  statements that this change makes false. The four-field sentence is exactly the
  shape SWT-21 shipped twice.
- **A guard that becomes wrong and gets deleted.** `TestNoMigrationWasAdded` is
  rewritten to the new truth, not removed.
- **A filter list that reads as complete and is English-only.** Criterion 5's
  Spanish spellings, because the corpus has 51 Spanish messages and a half-working
  filter shows up as noise, not as an error.

## Sibling patterns to copy

- **Pure-mapper shape and its tests:** `internal/connector/google/rfc822.go`'s
  header comment (why purity is what makes `--normalize-only --all` possible) and
  `rfc822_test.go`'s table tests — `TestNormalizeRFC822_BodyDecoding` (251) for
  the fixture builder, `TestNormalizeRFC822_Deterministic` (416) for criterion 7.
- **Purity scan with a control:** `internal/capture/rules_structure_test.go`
  `TestRulesGo_IsPure`, and `internal/provider/callsites_test.go` for the
  "positive control or the scan proves nothing" rule.
- **Schema + contract scans:** `internal/classify/structure_test.go:183-250` —
  criterion 18/19 extend that test rather than adding a second one beside it.
- **Store-shape:** `internal/classify/store.go`'s one-query fold (`inboxSelect`
  shared by both readers) and `internal/drafts/store.go`, which SWT-21's
  Deviation 9 names as the better pattern.
- **Integration-suite hygiene:** `make integration` runs `-p 1` because the triage
  and connector suites cross-pollute on one compose db. The new
  `links_integration_test.go` joins that mutual-cleanup pact: clean its own rows
  first, in FK order, scoped by a test-owned account email / slug.
- **Migration style:** `migrations/0015_capture_rules.sql` for how a CHECK
  constraint is commented (say what it makes structural and what it prevents), and
  `0016_provider_locality.sql` for the ALTER-plus-comment shape.
- **HTML handling that already exists here:** `stripHTML` (rfc822.go:317) — read
  it before writing the extractor, and then leave it alone (criterion 10).

## Verification protocol

Every command is meant to be run, not reasoned about.

```bash
eval "$(grep '^export OPS_DATABASE_URL=' ~/.bashrc)"
export OPS_LOCAL_PROVIDER_URL=http://127.0.0.1:11434
export OPS_LOCAL_MODEL=qwen3:8b
```

1. `go test ./...` — the extractor tables, the img-src refusal, the
   `multipart/alternative` case, the determinism test, the byte-identical
   `body_text` golden, `ResolveLink`'s table, the schema scans, and the
   zero-candidate end-to-end. No db, no network, no model.
   If `go build` wants to fetch `golang.org/x/net` and the box is offline, that is
   the contingency named under "Decisions made unilaterally" — record which
   extractor shipped.
2. `make db-up && make migrate && make integration` — the Postgres round trip
   (criterion 13) and the classify column test (criterion 16).
   **Mutate to confirm they bite:** replace `COALESCE(nm.links,'[]')` in
   `inboxSelect` with the literal `'[]'` and watch criterion 16 go red; if it stays
   green you tested your fixture.
3. **Migration state, before and after.**
   ```bash
   psql "$OPS_DATABASE_URL" -tAc "SELECT max(version) FROM schema_migrations"
   ```
   reads `0016` before, `0017` after `go run ./cmd/tools/migrate`. `ls migrations/`
   shows exactly one new file. Remember: merging a migration is not applying it.
4. **Freeze `body_text` on PRODUCTION data before backfilling.** This is the
   check that fixtures cannot make for you (premise 5):
   ```bash
   psql "$OPS_DATABASE_URL" -tAc \
     "SELECT md5(string_agg(md5(coalesce(body_text,'')), '' ORDER BY id))
        FROM normalized_messages WHERE channel='gmail';"
   ```
   Record it. Run the backfill (step 5). Run it again. **The two digests must be
   identical.** If they are not, stop — every unconfirmed gmail delivery's matcher
   input just moved and google has no reconciler to tell you.
5. **The backfill.**
   ```bash
   DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/connectors/google --normalize-only --all
   psql "$OPS_DATABASE_URL" -tAF' | ' -c "
     SELECT count(*) FILTER (WHERE jsonb_array_length(links) > 0) AS with_links,
            count(*)                                             AS total,
            round(avg(jsonb_array_length(links)), 2)             AS mean,
            max(jsonb_array_length(links))                       AS max
       FROM normalized_messages WHERE channel='gmail';"
   ```
   `max` must be ≤ 8 (criterion 6). Compare `with_links` and the median against the
   measured expectation (median 2 per message over the personal population) —
   re-measure rather than trusting this SPEC's literals.
   Then confirm nothing else moved: `SELECT count(*) FROM normalized_messages;`
   and `SELECT count(*) FROM capture_decisions;` unchanged before/after, and
   `SELECT count(*) FROM deliveries WHERE confirmed_at IS NOT NULL;` unchanged —
   the replay must confirm nothing new.
6. **The headline smoke: a verdict that carries a link.**
   ```bash
   DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/classify run --limit 20
   psql "$OPS_DATABASE_URL" -tAF' | ' -c "
     SELECT e.fields->>'link_candidates', e.fields->>'link_index',
            e.fields->>'link_url', e.fields->>'title'
       FROM ai_extractions e JOIN ai_runs r ON r.id=e.ai_run_id
      WHERE r.worker_type='classify' AND r.status='ok'
      ORDER BY e.id DESC LIMIT 20;"
   ```
   Expect a mix: rows with `link_candidates=0` and no url (the common case), rows
   with an index and a url, and — read them — a url that is plausibly the CTA of
   that message, not an unsubscribe link.
7. **The null case is not an error.** Find a message with zero candidates (an HOA
   First Notice is the intended one) and confirm it produced `status='ok'`, one
   extraction, no `status='skipped'` row and no non-zero exit.
8. **The out-of-range case is visible.** Force it — a fake lane in the unit suite
   returning `link_index: 99` — and confirm `link_index_rejected` lands in the
   fields and `classify report` prints it. Nothing about a nonsense index may fail
   a pass.
9. **`classify report --since 24h`** shows the URL on flagged lines and the
   four-state counts, and still prints the no-fallback note
   (`TestReports_ShareTheNoFallbackNote` must still pass).
10. **The eval, same labels, new number.**
    ```bash
    DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/classify eval \
      --labels docs/evals/personal-actionability.jsonl
    ```
    Expect `label drift: 0 excluded`. Record n / recall / precision / median
    latency / date as a second row in the runbook table, and say what happened to
    27871 and 84710. **A drop in recall is a result, not a failure to hide** — the
    candidate list is a prompt change and prompt changes have measured costs in
    this corpus (SWT-22's near-miss clause).
11. **The negative smoke still refuses.**
    `OPS_LOCAL_PROVIDER_URL=https://api.openai.com go run ./cmd/classify run --limit 5`
    → startup refusal, `avail_reason='local_endpoint_not_private'`, zero
    extractions, zero hosted calls. Links change nothing about the boundary.
12. `go test ./...` once more after the doc edits — criteria 26 and 27's
    structural tests read the files on disk.

## Decisions made unilaterally (argue if wrong)

- **A jsonb column, not a child table.** Argued at length under "Data model
  changes"; the deciding reason is that re-normalization is a whole-value rewrite
  and the existing upsert is not transactional.
- **Bare URLs in `text/plain` bodies are NOT candidates.** The design says
  "anchors only", and the measurement that produced a median of 2 candidates is a
  measurement of anchors: both drop lists lean on anchor TEXT, which a bare URL
  does not have, so plain-text marketing footers would arrive unfiltered and flood
  the list. The cost is bounded and visible: for a plain-text message the URL is
  already inline in `body_text`, so the reader is not blind — only the structured
  `link_url` is absent. Future work if it turns out to matter.
- **`golang.org/x/net/html`'s tokenizer, not a regexp.** Real mail has `>` inside
  quoted attributes and attributes split across lines; a regexp misparse yields a
  *wrong URL*, which is the class of defect you cannot see in a report. The module
  is already in the graph with a full hash (premise 11), so the cost is a go.mod
  line. Contingency if the module zip is not in the local cache and the box is
  offline: ship the regexp extractor behind the same pure `ExtractLinks` signature
  and record the swap in the runbook — the interface, the tests and the storage
  are unaffected.
- **1-based indices.** The list is 1-based where the model reads it and 1-based
  in `link_index`; `ResolveLink` is the ONE place that converts. `plan_order` is
  already 1-based sibling position in this repo, and an off-by-one that silently
  resolves the neighbouring URL is worse than a rejected index.
- **`{"type": ["integer","null"]}` plus permissive application-side handling.**
  A missing field, a JSON `null`, and any out-of-range integer are all handled —
  `not_chosen` / `none_offered` for the first two, `rejected` for the third — so
  the contract survives whatever a schema-constrained decoder does with a union
  type. One rejection path, no second contract invented at implementation time.
- **Exact anchor-text matching against the drop list, not substring.** "here"
  goes, "pay your bill here" stays. The measured cost is that a message whose only
  CTA is the bare word "here" yields no candidate — acceptable, because the null
  case is ordinary and a wrong link is worse than no link.
- **The drop lists live in Go, not in a table.** They are extraction rules, not
  routing rules: they run at normalize time, they are exercised by unit tests, and
  a change to them is re-runnable over the whole corpus with one command. A
  `link_filter_rules` table would be capture_rules' vocabulary borrowed for
  something with no operator workflow behind it.
- **Both mappers wired, not just the IMAP one.** Production is `imap`; wiring
  only that path makes a future `MAIL_SOURCE` flip a silent loss with no error.
  Ten lines of `firstTextHTML` buys that away.

## Future work (not this ticket)

- **Bare URLs from `text/plain`**, if the personal corpus turns out to carry
  plain-text-only actionable mail. It needs its own filter design, since the
  anchor-text drop list does not apply.
- **A re-classify sweep** for messages whose verdict predates this change — a
  `--reclassify` flag or a `prompt_version` predicate on the inbox filter. Cheap
  once someone wants it; not needed while only a handful of extractions exist.
- **Link rendering on the dashboard**, alongside the classifier lane that is
  already listed as future work in SWT-22.
- **Following a link to read what is behind it.** Named here only to be refused
  in the right place: it is a fetch, and it needs a deliberate design (what
  credentials, what audit row, what rate limit) rather than an incremental patch
  to a normalizer.
- **A GIN index on `links`** if anything ever needs "which messages point at this
  host".
