# Reproduction — inquiry-reads-quoted-history (SWT-70, swb #448)

> **Fixed (2026-09-21).** `internal/classify/inquiry_quotedhistory_repro_test.go` was red when this was written and is green now: it stays as the regression test. See `inquiry-reads-quoted-history_DIAGNOSIS.md`.


## Status
**Confirmed**, on three independent surfaces:

1. **Model-free, deterministic**: a Go test at the prompt-building seam fails today — the quoted
   reply chain is handed to the model as part of the message it is asked to judge, unfenced, and a
   second copy of it rides in the flattened thread-context line.
2. **Model-in-the-loop, stable**: replaying the STORED prompt for message 385428 against the same
   local model reproduces the stored verdict **5 times out of 5**, with a `reason` string byte-identical
   to the one in `ai_extractions` (254 chars, exact match). Not a one-off.
3. **The verdict flips when the quote is removed**: the same prompt with the target body cut at its
   first reply separator gives `needs_reply=true`, `ask_kind=request` **5 out of 5**; cutting the
   quoted tails off the context lines as well gives the same, 5 out of 5.

Reported symptom and reproduced symptom match: a reply whose new text delivers requested material is
judged `needs_reply=false` / `ask_kind="fyi"` with a reason describing the previous, quoted message.

## Trigger
1. An inbound gmail message on a project with `projects.ai_inquiry` (only `collaboratory` is armed),
   attributed `live | attributed` by capture, so it enters the inquiry inbox.
2. The message is an Outlook-style reply: a short NEW paragraph, then `-----Original Message-----`,
   then the quoted chain (here two levels deep) plus the sender's legal disclaimer.
3. At least one prior thread message is loaded as context, and that prior message ALSO carries its own
   quoted copy of the chain.
4. `classify run --lane inquiry` renders the prompt with `renderInquiryUser` and asks qwen3:8b.

Production instance: `normalized_messages.id = 385428`, thread 327208, `raw_source_items` 96917,
project `collaboratory` (id 4), sent 2026-09-21 13:57:02Z, sender `ur.rochester.edu`.

## The evidence

### The capture decision (correct)
`capture_decisions` 404008, 14:00:23Z: `mode=live`, `action=attributed`, `project_id=4`,
`matched_rule_ids={60}`, reason "rule 60 (body_regex) attributes to collaboratory; no external_system,
so attribution only". `ambiguous=false`, `resurface=false`, `ai_extraction_id` NULL.

### The run
`ai_runs` **12721**, `worker_type=classify_inquiry`, provider `ollama`, model `qwen3:8b`, `status=ok`,
14:01:08Z, latency 14,683 ms. `input` keys: `prompt_version=inquiry-v1`, `normalized_message_id=385428`,
`raw_source_item_id=96917`, `thread_id=327208`, `project_id=4`, `project_slug=collaboratory`,
`user_prompt`.

### Structure of the stored `user_prompt` (4,656 chars, 89 lines, CRLF inside the body)
Client text — quoted here only in short fragments.

| region | lines | chars | content |
|---|---|---|---|
| context header | 1 | 64 | `Earlier in this conversation, oldest first (prior context only):` |
| context msg 1 (`me: `) | 2 | 606 | our 2026-09-17 reply, whitespace-collapsed onto ONE line |
| context msg 2 (`them: `) | 3 | 608 | the client's 2026-09-17 ack, whitespace-collapsed onto ONE line |
| blank | 4 | 0 | |
| decide marker | 5 | 25 | `The message to decide on:` |
| headers | 6–8 | 183 | `From:` / `Subject:` / `Date: 2026-09-21T13:57:02Z` |
| **new text** | 10–23 | **203** | greeting, one apology line, then the deliverable: two labelled groups (`Course`, `Class`) with 4 bullet items naming field names and SQL types, then `Thanks` |
| **quoted history** | 25–88 | **2,910** | `-----Original Message-----` at line 25 and again at line 35 (3 occurrences of the separator in the prompt overall), full `From:/Sent:/To:/Subject:` header blocks under each, the client's earlier one-liner, our full earlier reply, signature, then a ~505-char `LEGAL DISCLAIMER` block |

Precisely:

- **Where the new text sits**: immediately after the 3-line header block, lines 10–23, **203 chars —
  6.4% of the 3,191-char body**. It is the material the previous message promised: an apology for the
  delay and the explicit list of fields needed. (The spread SQL below reports 218 chars for the same
  region: it measures `body_text`, which carries CRLF line endings; 203 is the same text counted after
  newline normalisation.)
- **Where the quoted history starts**: line 25, char offset 1,668 of the prompt (body starts at 1,465). Everything from there
  to the end — **2,910 chars, 88% of the message-to-decide-on section (3,296 chars)** — is quoted
  history and boilerplate. There is **no fence, no marker, no instruction** separating it from the new
  text; it is plain body text in the same block.
- **How the two context messages are presented**: one line each, `me: ` / `them: ` prefix,
  `strings.Fields`-collapsed to single spaces, hard-truncated at 600 chars plus `" …"` — both hit the
  cap exactly (606 = 4 + 600 + 2; 608 = 6 + 600 + 2). The `them:` line is the client's 2026-09-17
  message, which is itself an Outlook reply, so its collapsed 600 chars are: ~45 chars of its own new
  text (the `will send the custom fields … tomorrow` line the verdict ends up describing), then
  `-----Original Message-----`, then `From:`/`Sent:`/`To:`/`Subject:` and the first ~500 chars of our
  own earlier reply. The quoted chain therefore appears **twice** in the prompt.
- **Truncation**: only the two context lines are truncated (at `inquiryContextBodyMax = 600`). The
  target body is NOT truncated — `renderMessage`'s 4,000-char cap was not reached by a 3,191-char body,
  so the entire quoted chain and the disclaimer went to the model verbatim. No `…(truncated)` marker
  appears in the prompt.

### The stored verdict (`ai_extractions` 12617)
```
needs_reply=false  ask_kind="fyi"  asker=""  ask=""
reason="This message is a follow-up from the recipient ([the client contact]) confirming that they
        will send the custom fields along tomorrow. It does not ask for a reply, nor does it
        request any action from the recipient. It is a confirmation of a prior commitment."
```
Bookkeeping on the same row: `thread_scope=thread`, `context_messages=2`, `project_slug=collaboratory`,
`thread_key=gmail:salvador@handsonconnect.org:<…@getmailspring.com>`, and the message's own
`external_message_id`. The reason describes a commitment that exists **only** in the quoted region and
in the truncated `them:` context line; the 203 chars of new text are not mentioned.

### Downstream
`classify_promotions`: **no row** for 385428. No task. Nothing in the board's incoming section.
(Task #447 was created by hand.)

## Observed behavior

### Model-free (the deterministic artifact)
`go test ./internal/classify/ -run TestRenderInquiryUser_QuotedReplyHistory -v` → FAIL, both subtests:

```
message-to-decide-on section: 2181 chars, of which 1838 (84%) sit below the first quote separator
--- FAIL: .../target_body
    quoted reply history is inside the message the model is asked to judge:
    the sentence "I'll send the field list along tomorrow." appears only below
    "-----Original Message-----", yet it is in the decide-on section.
--- FAIL: .../thread_context_line
    a thread-context line carries its own quoted reply history:
    the "-----Original Message-----" separator and the headers under it are inside the
    flattened context line (610 chars).
```

**Seam targeted: `classify.renderInquiryUser` (`internal/classify/inquiry.go:175`).** It is the right
seam because it is the pure function whose return value IS `ai_runs.input->>'user_prompt'` (stored by
`runInput`, `classify.go:665`) — the exact bytes the model was asked to judge. The stored prompt is
confirmed to be its output: the literal header line, the `me:`/`them:` tagging and the 600-char + `" …"`
truncation all match, to the character. Below it, `renderMessage` (`classify.go:652`) renders the body
verbatim under a 4,000-char cap. Asserting the property at this seam needs no model, no database and no
network, and it is where a fix (strip, or fence and subordinate) would have to land to change what the
model sees. The test lives in package `classify` because `renderInquiryUser` is unexported.

The fixture is synthetic, modelled on the real message's shape (203 chars of new text over a two-level
quoted chain with a disclaimer; a prior outbound context message and a prior inbound one that quotes) —
none of the client's words.

### Model-in-the-loop (15 calls, qwen3:8b on the z4, temperature 0, think false)
| variant | prompt chars | runs | result |
|---|---|---|---|
| `stored` (verbatim) | 4,657 | 5 | `needs_reply=false`, `ask_kind=fyi`, `ask=""`, 5/5 — reason **byte-identical** to the stored verdict's |
| `no-quote-target` (target body cut at its first separator; context lines untouched) | 1,680 | 5 | `needs_reply=true`, `ask_kind=request`, `ask="Send the custom fields along"`, 5/5 |
| `no-quote-anywhere` (target body + both context lines cut at their separators) | 1,137 | 5 | `needs_reply=true`, `ask_kind=request`, `ask="Send the custom fields along tomorrow"`, 5/5 |

Reported as observations. The miss is stable and deterministic at temperature 0; the verdict flips in
both edited variants.

## Expected behavior
A reply is judged on its NEW text. Quoted reply history under it (`-----Original Message-----`,
`On … wrote:`, `>` lines) must be absent from — or clearly fenced and subordinate in — what the model is
asked to judge, including the flattened thread-context lines. Message 385428 should have produced
`needs_reply=true` with an ask naming the delivered fields, a `classify_promotions` row, and a task in
the board's incoming section.

## How widespread (read-only, last 30 days, run 2026-09-21)
`psql "$OPS_DATABASE_URL" -f docs/bugs/inquiry-reads-quoted-history_repro.sql`

Inbound **gmail** messages with an inquiry verdict (`worker_type=classify_inquiry`, `status=ok`), split
by whether the body contains a reply-quote separator (`-----Original Message-----`,
`-----Mensaje original-----`, a line starting `On ` containing `wrote:`, or a line starting `>`):

| has quoted history | needs_reply | n | of which new text > 40 chars |
|---|---|---|---|
| no | false | 136 | 136 |
| no | true | 9 | 9 |
| **yes** | **false** | **9** | **9** |
| yes | true | 8 | 8 |

So 9 of the 17 quoted messages classified in the window were answered `needs_reply=false`, and all 9
have non-trivial new text above the separator. The candidates (no bodies):

| message_id | sent | sender domain | project | ask_kind | new text chars | body chars | subject |
|---|---|---|---|---|---|---|---|
| 385428 | 2026-09-21 | ur.rochester.edu | collaboratory | fyi | 218 | 3,191 | RE: [EXT] Re: Questions About Collaboratory Activities Integration |
| 328172 | 2026-09-17 | ur.rochester.edu | collaboratory | fyi | 66 | 2,732 | RE: [EXT] Re: Questions About Collaboratory Activities Integration |
| 327652 | 2026-09-17 | e.atlassian.com | collaboratory | fyi | 559 | 5,418 | Governed agent loops are available in your workflow |
| 294300 | 2026-09-15 | e.atlassian.com | collaboratory | fyi | 765 | 2,255 | Join Atlassian's CEO for State of AI SDLC |
| 273993 | 2026-09-14 | cecollaboratory.com | collaboratory | fyi | 2,870 | 40,643 | Re: [EXT] Re: Courses Endpoint issue |
| 143616 | 2026-09-09 | cecollaboratory.com | collaboratory | fyi | 3,329 | 26,653 | Re: [EXT] Re: Courses Endpoint issue |
| 141858 | 2026-09-06 | cecollaboratory.com | collaboratory | fyi | 2,647 | 19,371 | Re: [EXT] Re: Courses Endpoint issue |
| 141616 | 2026-09-04 | cecollaboratory.com | collaboratory | fyi | 2,267 | 11,715 | Re: [EXT] Re: Courses Endpoint issue |
| 141585 | 2026-09-04 | rochester.edu | collaboratory | fyi | 4,363 | 24,295 | RE: [EXT] Re: Questions About Collaboratory Activities Integration |

Nine rows is the whole candidate set for the window — the list is not truncated by the LIMIT 15. Only
`collaboratory` has `ai_inquiry` armed, so the population is one project. Two of the nine are vendor
marketing, not client threads. Whether each of the other seven contained a real ask is a judgement for
the owner; `385428` is the one he caught.

## Reproduction location
- Deterministic, model-free: `internal/classify/inquiry_quotedhistory_repro_test.go`,
  `TestRenderInquiryUser_QuotedReplyHistoryIsNotJudged` (plain unit test, no build tag, no db).
- Model-in-the-loop: `docs/bugs/inquiry-reads-quoted-history_repro.py` (python3 stdlib only; read-only;
  reads the lane's system prompt and schema out of `internal/classify/inquiry.go` at run time so it
  cannot drift).
- Spread: `docs/bugs/inquiry-reads-quoted-history_repro.sql` (read-only, two result sets).

## Exact commands

```bash
# 1. deterministic failing test (fails today)
cd /home/salvo/projects/personal/switchboard
go test ./internal/classify/ -run TestRenderInquiryUser_QuotedReplyHistory -v

# 2. dump the stored prompt (read-only; client text — keep it out of the repo)
psql "$OPS_DATABASE_URL" -At -o /tmp/prompt-385428.txt \
  -c "select input->>'user_prompt' from ai_runs where id=12721;"

# 3. the stored verdict and run metadata
psql "$OPS_DATABASE_URL" -c "select jsonb_pretty(fields) from ai_extractions where id=12617;"
psql "$OPS_DATABASE_URL" -c "select id, model, provider, status, latency_ms, created_at,
       jsonb_pretty(input - 'user_prompt') from ai_runs where id=12721;"
psql "$OPS_DATABASE_URL" -c "select jsonb_pretty(to_jsonb(c)) from capture_decisions c where c.message_id=385428;"
psql "$OPS_DATABASE_URL" -c "select * from classify_promotions where normalized_message_id=385428;"

# 4. model-in-the-loop, 5 runs x 3 variants (~3 min on the z4; no writes anywhere)
python3 docs/bugs/inquiry-reads-quoted-history_repro.py /tmp/prompt-385428.txt --n 5

# 5. spread
psql "$OPS_DATABASE_URL" -f docs/bugs/inquiry-reads-quoted-history_repro.sql
```

## Environment
- `git rev-parse HEAD`: `d1825e333fdd33f1840bc09cc642135224505e9e`, branch `fix-inquiry-reads-quoted-history`.
- Production `ops` db read-only via `$OPS_DATABASE_URL` (Postgres 17.9). Nothing was written: no
  `classify run`, no promote, no scratch database needed.
- Local model: `OPS_LOCAL_PROVIDER_URL=http://192.168.50.55:11434`, `OPS_LOCAL_MODEL=qwen3:8b` (the z4).
  Request options mirrored from `internal/provider/ollama.go`: `/api/chat`, `stream:false`,
  `think:false`, `keep_alive:"30m"`, `options.temperature:0`, `options.num_predict:512` (cmd/classify's
  512), `format` = `classify.InquiryVerdictSchema` verbatim, system = `classify.InquirySystemPrompt`.
  No `num_ctx` is sent, matching production.
- Data prerequisites: `projects.ai_inquiry` true for `collaboratory` (id 4) — the only armed project.

## Notes
Observations only, no cause claimed.

- The replay of the stored prompt returns the stored `reason` string byte for byte, so the recorded
  verdict is reproducible from the stored input alone; nothing about the run was environmental.
- Both edited variants flip the verdict, and the two edits differ (target-only vs target + context
  lines) yet land on the same answer, with `ask` wording that follows whichever copy of the promise
  survived.
- The `them:` context line's 600-char cap spends roughly 45 chars on that message's own new text and
  the remaining ~555 on its quoted copy of our earlier reply; the cap was reached in both context lines.
- The message-to-decide-on section is 88% quoted history by character count in production, 84% in the
  synthetic fixture.
- The inquiry system prompt describes `me:`/`them:` context and tells the model a question already
  answered in the transcript is `needs_reply=false`; it says nothing about quoted history inside a
  message body.
- `internal/replyfold` exists in this package's imports but is the replied-since / thread-scope fold,
  not quote handling. Flagged only so the diagnoser does not mistake it for one.
- The eval set `docs/evals/inquiry-needs-reply.jsonl` was not modified. Whether it contains
  quoted-reply examples is unexamined here — worth checking before any prompt or renderer change, since
  a re-score is the acceptance gate.
- The `psql -o` dump appends one trailing newline, so the replayed prompt is 4,657 chars against the
  stored 4,656. It reproduced the stored reason byte for byte regardless.
- Widespread-count caveat: the 30-day window covers only `channel='gmail'` inbound messages, per the
  report's framing. Slack messages in the inquiry population are not counted.
