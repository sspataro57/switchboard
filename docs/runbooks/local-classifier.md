# Runbook — the local actionability classifier (SWT-22)

`classify` runs THREE lanes: `--lane personal` (the SWT-22 lane — mail the capture rules attributed to the `personal` project), `--lane residue` (SWT-23 — the unmatched pile nothing has routed) and `--lane inquiry` (SWT-33 — client conversation on projects armed with `ai_inquiry`: does this message need a reply from Salvador). One binary, two verdict contracts (actionability for the first two, needs-reply for the inquiry lane), three inboxes and three prompts.

`classify` reads personal mail — the messages the capture rules attributed to the
`personal` project — and records a shadow verdict: is this something that has to
be DONE, or is it information. It creates no tasks. It runs only against a model
on this machine, enforced by SWT-21's boundary rather than by configuration.

## The model, and why it is not negotiable without new measurements

**qwen3:8b.** Chosen from a measured spike (`docs/tickets/local-classifier-spike.md`),
not from preference. It reached 0.90 recall on a mixed eval where the best model
on the cluster proxy reached 0.80, and it caught all five HOA violation notices
in a same-sender test where gpt-5.4-mini and gpt-5.6 each missed three of five.

**Recall is the objective, and accuracy is a trap.** A missed payment or fine
notice is a late fee; a false alarm costs a second to dismiss. On the real
distribution, always answering "not actionable" scores 99.8% accuracy and is
worthless.

## Three things that will bite whoever touches this

### 1. `think: false` on every request, and how to SEE the failure it prevents

qwen3 is a thinking model. With thinking left ON it spends its whole token budget
reasoning about a two-line message and returns **empty content**:

```
done_reason: length | content length: 0 | thinking length: 974 | eval_count: 200
```

70 of 70 outputs malformed, a 0.00 score. The response is a 200 OK with a
well-formed envelope — **the failure is invisible unless you read the raw
response**, which is why this runbook shows you how:

```bash
curl -s http://192.168.50.55:11434/api/chat -d '{
  "model":"qwen3:8b","think":true,"stream":false,
  "options":{"num_predict":200},
  "messages":[{"role":"user","content":"Subject: payment due. Actionable?"}]}' \
| python3 -c 'import sys,json; d=json.load(sys.stdin); print("done_reason:",d["done_reason"],
  "| content:",len(d["message"]["content"]), "| thinking:",len(d["message"].get("thinking","")))'
```

Flip `"think"` to `false` and the same request returns `done_reason: stop` with
valid JSON. The adapter sends `think` and `stream` as explicit `false` with **no
`omitempty`** — an `omitempty` bool drops `false` from the wire and hands the
decision back to ollama's default, in a struct that still reads as if it disabled
it.

### 2. Self-reported confidence is a CONSTANT — never make it a threshold

qwen3:8b returns exactly **0.95** for everything it flags: 27 true positives and
17 false positives, identical. There is no dial in there. That is why
`VerdictSchema` has **no `confidence` field at all**, and why a structural test
asserts its absence — otherwise a future session reads the gap as an oversight
and adds it back, producing a gate that looks principled and does nothing.

To trade precision back, use the **second pass**: let qwen3:8b flag broadly, then
re-check only the flagged subset with a *different* model and keep the
disagreements for review. Two disagreeing models beat one model's self-assessment.
Measured as affordable — the swap is a per-pass constant, not per-message, so a
second opinion costs +5–19%. It is Future work and needs the full labelled set
first.

### 3. No per-sender prompts, ever

One prompt for every sender; a structural test fails any branch on a sender
string. Per-sender prompts are rules in a costume — unbounded maintenance,
untestable in aggregate, unattributable when they misfire. What the HOA case
actually shows is a missing *context fact* (this sender emits both announcements
and fines), and a fact belongs in a column, editable and visible in the report.

## Configuration

```bash
export OPS_LOCAL_PROVIDER_URL=http://192.168.50.55:11434   # NO /v1 — native API
# 192.168.50.55 is the z4's dedicated GPU (2026-09-06; measured median 4.1s
# per verdict vs 7.2s on the workstation's shared card). The workstation's own
# ollama systemd service is retired; an IP literal is required — the locality
# boundary does no DNS, so a hostname classifies as remote and skips everything.
export OPS_LOCAL_MODEL=qwen3:8b                        # required; no fallback
OLLAMA_VULKAN=1 ollama serve                           # ROCm crashes on this GPU
```

Since 2026-08-31 `ollama serve` runs as a systemd **user** service on the
workstation (`~/.config/systemd/user/ollama.service`, linger enabled, carries
`OLLAMA_VULKAN=1`) — `systemctl --user status ollama` to check,
`ollama stop qwen3:8b` to evict the model from VRAM without stopping the
daemon. **This deployment is TEMPORARY**: the end-state is ollama in-cluster
behind a MetalLB address in `192.168.50.0/24` (the only in-cluster shape the
locality boundary accepts — see the k8s constraint below). When that lands,
`systemctl --user disable --now ollama` and delete the unit.

`OPS_LOCAL_MODEL` is required once the URL is set. Missing it leaves the lane
absent with one logged refusal — a skipped pass — rather than sending the hosted
model name to ollama and getting a 404 per message.

Ollama **discharges** models: `/api/tags` lists what is on disk, `/api/ps` what is
resident. A cold load is ~3.4s and a warm call ~0.25s, so the adapter sends
`keep_alive: 30m`. Do not write eviction management — `keep_alive: -1` pins
against time but NOT against memory pressure (verified: expiry moves to the year
2318 and the model is still evicted when another needs the VRAM), and ollama's
own eviction is correct.

## Running it

```bash
go run ./cmd/classify run --limit 50     # shadow; creates nothing
go run ./cmd/classify report --since 168h
go run ./cmd/classify eval --labels docs/evals/personal-actionability.jsonl
```

## Reading a report: a skip is not an unremarkable verdict

This is the distinction the whole design turns on, and it must be readable
without opening psql.

- **`classified: N`** with verdicts — the model looked. A message it judged
  informational still writes an `ai_extractions` row, which is what removes it
  from the inbox.
- **`skipped: N`** — nothing looked. No extraction is written, so the message
  stays in the inbox and retries next pass. The reason says which:
  - `no_local_provider` — `OPS_LOCAL_PROVIDER_URL` is unset. Configure it.
  - `local_endpoint_not_private` — it points somewhere that is not local. Fix the
    value; the boundary is refusing a lie, not malfunctioning.
  - `local_unreachable` — the box or the model is not answering. **Normal
    operation.** A shared GPU is busy sometimes; this is a skip, not an error, it
    does not count toward the abort ratio, and the pass still exits zero.
  - `unclassified_error` — the adapter is broken (a 404, a 500, malformed JSON).
    Only THIS one counts toward the ratio that raises.

An all-skipped pass when the local box is down is expected. Falling back to a
hosted provider is never the fix.

## Links

Where links come from: the google NORMALIZER extracts anchors from the raw
message's text/html part at normalize time (`internal/connector/google/links.go`)
into the `normalized_messages.links` column — NOT from `body_text`, which
carries no URL at all in 837 of 1,613 personal messages because body selection
keeps anchor text and drops hrefs. The array position is the identity.

The model returns `link_index` — a 1-based number into that array, or null —
and NEVER a URL. Ask a model for a URL and it invents a plausible one; a
hallucinated portal link on a task about a fine is worse than no link at all.
The index makes that structural: `ResolveLink` is the one conversion, an
out-of-range index is recorded as rejected, and there is no string field to
author a link into.

**`link_index: null` is ORDINARY output, not a failure.** The two HOA First
Notices have no usable link at all — their only URL is SendGrid's `/wf/open`
open-tracking pixel inside an `<img>`. `img src` is never extracted and never
followed: widening the extractor "to catch the Pines link" would put a tracking
beacon on a task. The common case is zero candidates; the median message has
two.

Backfill (existing rows get links by re-normalizing from raw):

```bash
DATABASE_URL=... go run ./cmd/connectors/google --normalize-only --all
```

Idempotent: the upsert keys on `raw_source_item_id`, so message ids — and with
them `capture_decisions`, `ai_extractions` and the eval labels — survive a full
corpus rebuild. `body_text` comes out byte-identical (the golden test pins it;
it feeds `confirmDeliveryByBodyPrefix` and google has no reconciler).

To add a drop-list entry: edit the list in `links.go`, re-run
`--normalize-only --all`, re-run the eval and record the new row above.

## The residue lane (SWT-23)

The second inbox: messages whose LATEST capture decision is `unmatched` — no
project, no rule, triage's inbox in name. Run it with:

```bash
DATABASE_URL=... go run ./cmd/classify run --lane residue --since 720h
DATABASE_URL=... go run ./cmd/classify report --lane residue --since 24h
DATABASE_URL=... go run ./cmd/classify eval --lane residue   # labels default to the residue fixture
```

Verdicts land under worker_type `classify_residue`, never `classify`: both
inbox filters key their NOT EXISTS on worker_type, so a shared value would make
a message classified by one lane permanently invisible to the other once a
capture rule claims it.

**`--since` is REQUIRED on a residue run** — an unbounded pass is refused, not
defaulted. The arithmetic: ~8,800 unmatched messages (post-rules; 14,737 before the SWT-23 bulk rules) x the measured **7.2 s**
median (the table above; NOT the 0.25 s warm benchmark, which was a ten-word
prompt and is wrong by 25-29x) = ~17.6 GPU-hours. A default would be a 17-hour
job started by a typo; `--since 87600h` remains available when a full
historical sweep is chosen deliberately. Evals are exempt — they are bounded by
the label file.

**The strata, and which number to quote.** The residue labels are stratified
(`uniform` / `enriched` / `domain_gate`, recorded per line in the file):

- recall is computed over ALL labels — the enriched stratum is the recall
  denominator; a uniform 200 at a ~2% base rate yields four positives and a
  recall that is noise;
- precision is quoted from the **uniform** stratum ONLY — a precision measured
  over a deliberately enriched sample is not production precision, and it will
  be quoted as if it were unless the harness refuses;
- the **base rate** (uniform stratum) is the number that decides this lane's
  future: if the residue is well under 1% actionable after the rules, the
  honest description of this lane is "a daily filter over new mail", never "a
  classifier over history".

Measured numbers for this lane are recorded in the score table above alongside
the personal rows, with their date.

## The inquiry lane (SWT-33)

The third inbox: inbound messages whose LATEST capture decision attributes them
to a project armed with `projects.ai_inquiry` (migration 0024; only
`collaboratory` today). It asks a different question from the other two lanes —
**does this message contain an inquiry that needs a reply from Salvador** — so
it has its own output contract (`needs_reply`, `ask_kind`, `asker`, `ask`,
`reason`), its own prompt and its own labels. Verdicts land under worker_type
`classify_inquiry`. SHADOW: it creates no task and no outbound row of any kind.

```bash
DATABASE_URL=... go run ./cmd/classify run --lane inquiry --since 168h
DATABASE_URL=... go run ./cmd/classify report --lane inquiry --since 168h
DATABASE_URL=... go run ./cmd/classify eval --lane inquiry   # labels default to docs/evals/inquiry-needs-reply.jsonl
```

Arming and disarming a project is one UPDATE each way, and there is nothing to
undo, because the lane creates nothing:

```sql
UPDATE projects SET ai_inquiry = true  WHERE slug = '<slug>';
UPDATE projects SET ai_inquiry = false WHERE slug = '<slug>';
```

`ai_inquiry` is a WORKLOAD flag, like `ai_classify` (which opts mail into the
personal lane's actionability question and is set on `personal` only). Neither
is the boundary; `ai_locality` is, and this lane's filter deliberately does
NOT read it — `collaboratory` is `ai_locality='any'`, and a `local_only` clause
would return zero rows. The lane is contained instead by `classify.Run` pinning
its routed class to restricted and by `cmd/classify` having no hosted client at
all. **Never "fix" skips by adding a hosted client**: the pin exists so the lane
works without one.

**`--since` is REQUIRED on an inquiry run** — an unbounded pass is refused, not
defaulted. An inquiry goes stale in days, and the armed project's history is
unbounded from the code's point of view. The arithmetic: `collaboratory`
receives ~34 inbound messages a day (184 slack + 35 gmail + 17 jira over the
week measured 2026-09-10) x the measured **4.5 s** median per verdict = ~2.5
GPU-minutes per day of history. Measured on the first shadow pass, 2026-09-10:
50 verdicts, median 4,454 ms, p90 5,263 ms, z4 at the 90 W cap, with the full
six-message context in the prompt. (The SPEC's planning figure was ~10 s; size
passes with the measured one, and re-measure if the context window changes.)
Evals are exempt: they are bounded by the label file.

Cadence during the shadow period: twice daily with `--since 24h` (overlap is
free — the extraction `NOT EXISTS` dedups). **The advisory lock `0x5157_0022`
is shared by all three lanes**, and `run` treats losing it as an error, so never
schedule two lanes in the same minute: chain them in one command
(`classify run --lane personal && classify run --lane inquiry --since 24h`) or
stagger them by more than the longest pass. The CronJob is a kube-repo handoff.

### What the model decides, and what the spine decides

The model sees up to six PRIOR messages of the same thread — both directions,
tagged `me:` / `them:`, oldest first — then the message itself. Never a later
message: a verdict stays a stable property of the message and a label stays
valid forever. The model answers "is this an inquiry to me, in this context";
it does not answer "is it still open".

"Still open" is decided at READ time by `classify.Summarize`, from the stored
verdicts plus a join to `normalized_messages`: a flagged verdict whose thread
carries an `outbound` message sent after it is no longer open. Nothing is
written back, so retuning the rule costs no GPU. Every flagged verdict is in
exactly one of three states:

| state | meaning |
|---|---|
| `open` | no outbound message on the thread since |
| `answered in thread` | a later outbound on a thread-exact key — a reply in that thread |
| `spoke in conversation since` | a later outbound anywhere in an unthreaded Slack channel or DM |

### `thread_scope`

Every verdict records `thread_id`, `thread_key` (verbatim), `thread_scope` and
`external_message_id`, so a later drafting ticket can aim a reply at the exact
thread without re-deriving anything. This ticket aims nothing.

- `thread` — the key names one thread: a gmail thread, a jira issue
  (`jira:{host}:{KEY}`), or a Slack message with a thread root
  (`slack:{ws}:{conv}:{root}`).
- `conversation` — an unthreaded Slack message, whose key is the whole channel
  or DM (`slack:{ws}:{conv}`). **A weaker claim**: a later outbound there only
  means Salvador has spoken in the conversation since — nearly a reply in a DM,
  almost nothing in a busy channel. That is why it is its own counter and never
  counted as answered. There is no thread to reply into yet; the recorded
  `external_message_id` is what a later ticket would root one at.
- `none` — the message has no thread at all.

Measured 2026-09-10: 113 thread-exact Slack keys vs 83 conversation-level ones,
and ONE conversation-level key holds 9,704 messages — which is why classifying
whole threads was rejected. Whether a Slack key is rooted is decided by
`slackweb.IsRootedThreadKey` alone; never parse the key in SQL or anywhere else.

### The fold's input, measured

The fold rests on `normalized_messages.direction = 'outbound'`, and Slack
direction fails closed per workspace. Measured per workspace on 2026-09-10 (all
time): `T0360B84U` (Avviato) 24,352 inbound / 19,012 outbound, latest outbound
2026-09-04; `T0HPR78RX` (Collaboratory/LlamaSite) 2,155 inbound / 2,393
outbound, latest outbound 2026-09-09.

Per channel, over the 30 days to 2026-09-10, on every thread that carries a
collaboratory-attributed message — the fold's actual input. (Counting the
attributed messages themselves shows almost no outbound at all, because capture
never decides an outbound message; count the THREADS.)

| channel | outbound on those threads | all messages | the fold |
|---|---:|---:|---|
| slack | 793 | 1,681 | discriminates |
| jira  | 25 | 105 | discriminates |
| gmail | 1 | 447 | **near-INERT for this channel** — replies to collaboratory mail almost never land on the same thread, so a gmail verdict reads `open` whether or not it was answered |

Re-measure before trusting a gmail `open`. Never freeze these numbers in a test
— the corpus is live.

### Reading the report, by channel

`classify report --lane inquiry` — and the inquiry block on `/funnel`, which
prints the same numbers from the same fold — shows the three states and then
every count **by channel**: classified, flagged, open, answered in thread, spoke
in conversation since, skipped. There is deliberately no `--channel` flag: the
lane runs over every channel of an armed project, and the breakdown is what says
which message shape broke when a number looks wrong. The rows sum to the lane
totals; `(unrecorded)` holds skips written before the lane recorded a channel.

The **base rate** — flagged ÷ classified, per channel — is the number that
decides this lane's future. Record it here after the first week of shadow
output.

**First shadow pass, 2026-09-10** (`run --lane inquiry --since 168h --limit 50`):
50 processed, 16 flagged, 0 skipped, 0 errors — all 50 slack and all
`conversation` scope, because the oldest-first limit never reached the week's
gmail. open 9 / answered in thread 0 / spoke in conversation since 7. Nothing
else moved: tasks 70 → 70, deliveries 3 → 3, and the personal and residue
reports were byte-identical before and after. 16 of 50 is NOT a base rate —
the sample is the week's oldest 50, not a uniform draw. Observations for the
labelling, not for tuning (tuning happens against labels, never intuition):
3 of the 16 flags carry `ask_kind: fyi` with `needs_reply: true`, a
self-contradiction; several flags in the `a-millon` channel are team members
addressing each other (`@esteban …`), the "addressed to someone else" case the
prompt names. One verdict's thread is in workspace `T0360B84U` (Avviato),
channel `C1C1TSLJH`, which a capture rule attributes to collaboratory — a
capture-rule question, outside this lane.

### Eval — and why it prints no ratio yet

| date       | n  | needs_reply | caught | flagged & labelled | median latency | model    | note |
|------------|----|------------:|-------:|-------------------:|---------------:|----------|------|
| 2026-09-10 | 61 | 2           | 2 of 2 | 2 of 7 (uniform)   | 4.3 s          | qwen3:8b | base rate 2 of 45 uniform · owner-blanket: 28 scored, 17 `not`+flagged, 2 of them uniform · INDICATIVE ONLY — this is not a measurement |

The 2026-09-10 row, read with the starter-set notes below: both labelled asks
were caught (`caught 2 of 2`), and 5 of the 7 uniform-stratum flags were
labelled `not`. The eval's own owner-blanket line says 2 of those 5 are
bulk-labelled rows (possibly real asks at the time), so at least 3 are false
alarms on individually judged labels. The precision count is therefore a LOWER
bound, and two positives say nothing about recall. Median 4,303 ms over 61
verdicts on the z4 (the first run: 4,325 ms), consistent with the shadow pass's
4.5 s.

Below `classify.EvalResultThreshold` = 120 scored labels,
`classify eval --lane inquiry` prints COUNTS (`caught 7 of 9 labelled
needs_reply`) and the marker `INDICATIVE ONLY — this is not a measurement`,
and no ratio anywhere. Quote the counts WITH the marker, in this table and in
any Jira comment — never a percentage. The refusal is code, not convention:
this repo once turned 29.5 GPU-hours into "60 minutes" by quoting a number
outside the context that produced it.

**The starter set as actually built (2026-09-10) — read the counts with this.**
61 labels, all Salvador's. What a label MEANS, decided with him: "when this
message arrived, was someone asking me something?" — judged against the
messages before it, exactly as the model sees it; whether he replied LATER is
the fold's job (answered in thread / spoke since), never the label's.

- 42 from the 14-day draw (16 `enriched` = the first shadow pass's flags, 26
  `uniform`), all `not`: 24 judged one by one, 18 by his blanket instruction
  that everything before 2026-09-10 was already dealt with. Some of those were asks at the time (his
  choice, recorded, not re-litigated), so a model that flags them is scored as
  a false positive: precision on this set is a LOWER bound.
- 19 = every message in the inquiry inbox on 2026-09-10 (`uniform`): 9
  labelled one by one, 10 by his statement that the rest were "just info"; 2
  are `needs_reply`.
- **Provenance is in the file.** 33 labels were judged one by one; the 28 set
  by a blanket instruction carry the exact `classify.OwnerBlanketNote` marker in `note` (content-free).
  Both are his judgement — criterion 32 bars an AGENT or a heuristic from
  labelling, not the owner from answering in bulk — but the Codex adversarial
  review (round 4) flagged that "hand-checked" reads as one-by-one, so the
  distinction is kept, and `classify eval` READS it (round 5): it prints
  `owner-blanket labels: N scored, M labelled not and flagged (U in the uniform
  stratum …)` on its own line. On this stratified set the precision line counts
  the UNIFORM stratum only, so U — never M — is the number that may be
  subtracted from its false positives: most enriched rows are the model's own
  earlier flags, and the all-strata M would drive the correction negative. When the set
  is grown by the protocol below, re-judge the blanket rows individually first.
- **Two positives measure almost nothing about recall.** The useful number here
  is the precision count; recall waits for the protocol below.
- Reported while labelling: HOC and LlamaSite work is not his, yet the
  `a-millon` channel is attributed to collaboratory — a capture-rule follow-up
  that would remove a known source of false flags from this lane.

**Dated commitment (2026-09-10):** grow the set to 120 stratified labels —
`uniform` >= 80, `enriched` >= 40 — during the shadow period. When it gets
there, raise the structure test's minimum in the same change: strata become
required, the refusal stops firing, and the first real recall and precision may
be published.

### Labelling protocol

The judgement "does Salvador need to answer this" is **his**. It cannot be
delegated to an agent or inferred from a heuristic: an agent may draw the
candidates and compute the hashes, never choose the label.

1. **The starter 40**: `collaboratory` messages from the last 14 days — recent
   enough that he can confirm each from memory in seconds — mixed across slack,
   gmail and jira in roughly the population's proportions (~78% slack, ~15%
   gmail, ~7% jira in the week measured 2026-09-10), each labelled
   `needs_reply` or `not` by Salvador with the thread context in front of him.
2. **The remaining 80**, during the shadow period: real flagged output from
   `/funnel` (stratum `enriched`) **plus a uniform sample of unflagged messages
   from the same window** (stratum `uniform`). The uniform half is not
   optional — a set built only from flagged output can measure precision and can
   never measure recall.
3. Every line is `{"message_id", "label", "subject_sha256"}` with optional
   `stratum` and `note`; `subject_sha256` is `classify.SubjectHash(subject)`.
   The file never carries message content. The record is CLOSED — `classify
   eval` refuses any other key — and `note` is a closed vocabulary
   (`classify.LabelNoteAllowed`; today only `classify.OwnerBlanketNote`), never
   free text: a free-text note is one paste from a client's message in git.

**The Slack subject hash is weak, and that is accepted.** slackweb sets a
message's `subject` to the CONVERSATION NAME, so every message in a channel
shares one `subject_sha256`, and the drift detector can only catch an id that
moved to a different channel. Do not add a second hash spelling to fix it —
`classify.SubjectHash` is the one spelling.

## The labelled set

`docs/evals/personal-actionability.jsonl` — the only thing anyone is permitted to
tune against. It carries **no message content**: message id, label, and a hash of
the normalised subject. Bodies are loaded from the database at eval time, so the
file is safe to commit while the mail never leaves the machine.

**Current set and scores** (record here on every regeneration or re-run):

| date       | n   | actionable | recall | precision | median latency | model    |
|------------|-----|-----------:|-------:|----------:|---------------:|----------|
| 2026-08-30 | 280 | 35         | 0.83   | 0.58      | 6.3 s          | qwen3:8b |
| 2026-08-31 | 280 | 35         | 0.94   | 0.50      | 7.2 s          | qwen3:8b |
| 2026-09-02 | 874 | 34         | 0.59   | 0.28      | 11.9 s         | qwen3:8b |
| 2026-09-07 | 280 | 35         | 0.57   | 0.67      | 31.3 s         | qwen3:8b (think:true) |

**Below 120 scored labels, `classify eval` prints COUNTS and
`INDICATIVE ONLY — this is not a measurement` on every lane (SWT-33)** — so a
hand-picked `--labels` subset of this file prints counts, never a ratio. Every
row above was measured at n >= 280.

The 2026-09-07 row is the THINKING A/B (`eval --think`: think:true, 2048-token
budget, 8k ctx — the knob exists exactly for this measurement) on the same 280
personal labels as the 2026-08-31 baseline row above it. The verdict is
decisive and closes the question for qwen3:8b: recall COLLAPSED 0.94 → 0.57 at
3× the latency — the model deliberates itself out of flagging Rx refills,
appointment reminders and fraud alerts, and 4 of 280 messages exhausted the
entire 2048 budget reasoning and scored as forced misses (the harness records
those as `ErrIncomplete`, latency 0, rather than aborting). Precision rose
(0.50 → 0.67) because thinking flags less of everything — the wrong trade on a
lane whose objective section above says recall, in bold. `think: false` stays;
re-open only with a NEW model and a new eval, never by intuition.

The 2026-09-02 row is the RESIDUE lane (SWT-23), scored over the stratified
874-label set — read it with the strata semantics, not like the personal rows
above: recall is over all strata (actionable-shaped mail over-represented by
design), precision and the base rate come from the uniform stratum only
(8 of 29 flagged actionable; 22 of 220 uniform labels actionable, ~10%), and
511 of the 874 labels had been claimed by the SWT-23 bulk rules between
labelling and scoring (still scored — the label file is the population). The
median latency is inflated by GPU contention during the run (the desktop held
15% of the model on CPU); the personal-lane rows were measured on a quiet GPU.
What the 14 false negatives actually are matters more than the 0.59: most are
WORK-shaped — GAV approval threads, milestone assignments, an NDA
confirmation, meeting invites — i.e. unrouted client asks that belong to
capture rules and attribution, not to this recall-first safety net. The
residue lane's job (don't lose a personal fine/payment notice hiding in the
unmatched pile) is served; the misses argue for more rules, not more prompt.

The 2026-08-31 row is the SWT-25 re-run: same 280 labels, same file, same
command, after link candidates entered the prompt and the verdict gained
`link_index`. Label drift exclusions: zero, as expected — the backfill upserts,
so ids and subjects are stable. False negatives went 6 → 2. What moved:
25541 and 27641 (appointment confirmations), 26018 (statement with a minimum
payment) and 26919 (the empty view-your-message shell) are all caught now.
What did NOT move, said plainly: 27871 (the doctor's-office portal message) and
84710 (the unfilled-template portal notice) are still missed — each now carries
its portal link and the model still reads them as informational, so the
candidate list alone does not fix content-behind-a-login; that is prompt or
second-pass territory, not extraction. Precision paid for the recall
(0.58 → 0.50, 33 of 66 flagged) and the median rose ~1 s with the larger
prompt — both acceptable trades while recall is the objective.

The 280 ids are hand-checked, drawn from the `personal` population (1,624
messages at draw time, 2026-08-29; the population grows daily): every Pines
Property Management message including the announcements-vs-violations pairs,
capped per-sender draws favouring distinct subject templates, a targeted sweep
for actionable-shaped subjects, and a uniform 10% sample of the last three
months.

Four "your statement is available" messages are labelled `actionable` even
though the prompt names that exact subject as informational. That is not the
spike's fixture error repeating: those four are labelled on the BODY, which
states a minimum payment with an amount and a due date, and the model itself
agrees on three of the four. Do not "correct" them back by subject — that
would move the score with no visible cause.

The six false negatives of the first run: 25541 and 27641 (appointment
confirmations read as done-deal informational), 26018 (the one
statement-with-minimum-payment miss), 26919 (a due-date warning whose body is
an empty view-your-message shell), 27871 (a doctor's-office portal message,
content behind login), 84710 (a portal notice built from an unfilled
template). Tuning against them is future work, not this ticket.

`classify eval` refuses to run on anything but the local lane, and reports **label
drift** — any id whose subject hash no longer matches is printed and EXCLUDED
before it is classified. The labels are the fixture and this fixture has been
wrong once already: the spike's first eval scored every model 0.10–0.27 recall
because the labels called "your statement is available" actionable while the
prompt said informational notices were not. The models were right; the fixture
was wrong.

Two things measured on the corpus that the labels must respect:

- **The prompt is more load-bearing than the model.** A shortened prompt,
  identical except that it stopped naming statement-available / balance /
  card-was-used as informational, flipped "your statement is available" to
  actionable — on the most common message shape in the corpus (883 BofA
  messages).
- **The corpus is bilingual.** 51 messages are Spanish; the originals are kept
  rather than translated, and matched-pair testing agreed 4/5. The one
  disagreement was a borderline case where the model is unstable, not a
  comprehension failure — which is why the Spanish messages get their own
  labelled rows.

## Known ceiling

Some notices defer to an attachment or a portal the classifier cannot read, and
the portal requires a login. "HOA violation notice — open the attachment" is the
honest best; the prompt is told never to guess an amount or a date it was not
given. Note the wrinkle: the HOA template says "please see attachment" on
messages that carry **no attachment at all** — the detail is in the body.

## Promotion (SWT-30)

The one deliberate exit from shadow: `classify promote` turns stored
PERSONAL-lane verdicts into tasks on the board. Whitelisted kinds —
`payment_due` and `deadline`, a Go constant in `internal/promote`, not
configuration — become live `ready` tasks; every other flagged kind parks as a
`holding` task (the review lane is `/tasks?project=personal&status=holding`).
A follow-up on a thread that already carries an OPEN task attaches as a log
event instead of creating a duplicate; a thread whose task is already
closed/delivered gets a NEW task (a re-raised obligation stays visible).

**Except a DISMISSED one (SWT-36).** If the thread has no open task but has a
`closed` task with an OPEN dismissal (`task_dismissals.reopened_at IS NULL`),
the verdict attaches to THAT task, whatever its kind — never a duplicate of
what you just dismissed. The pass appends the log line, then calls the guarded
`task_reopen {task_id, dismissal_id, message_id}` as `promote:classify`; the
task comes back at the status it was dismissed from (a review-lane `holding`
task stays `holding`) only if the message was INGESTED after the dismissal —
a message already in switchboard when you dismissed only logs. The promotion
row stays `action='attached'` with the reason `thread's task N was dismissed
(code); attached, reopen requested against dismissal D`; the typed outcome is
`task_dismissals.reopened_by_message_id`. `--dry-run` prints the request
(`reopen_dismissal=D`) and writes nothing; the stats line gains `"reopened"`.
A task that was plain-closed after coming back falls through to a new task as
before.

**Arming it.** Promotion is OFF until a human sets the cutover — there is no
flag, no default, and no deploy side effect:

```sql
UPDATE projects SET classify_promote_after = now() WHERE slug = 'personal';
```

**Forward-only, on the verdict clock.** Only verdicts *recorded*
(`ai_runs.created_at`) after the cutover promote. Lowering the timestamp does
NOT backfill: an already-classified old message never re-enters the classify
inbox (its `NOT EXISTS` excludes it), so its verdict's timestamp never moves.
The residue backlog and every pre-cutover personal flag stay unpromoted,
forever, by design. **Accepted residual** (Q2, Salvador 2026-09-09): a
personal-lane run that classifies a *never-before-classified* old message
records a fresh verdict, so an old bill can land on the board as a live task —
the verdict clock, not the message clock, is the fence.

**The residue lane cannot promote, twice over.** `worker_type='classify'`
excludes `classify_residue` by name, and the inner join to the message's
attributed project excludes it structurally — an unmatched message has
`project_id NULL` by 0015's CHECK. Both bars are load-bearing and both are
pinned by fixtures.

**Sequence for going live.** Dry-run first, always:

```
classify promote --dry-run     # prints the plan, writes nothing at all
classify promote               # claims + creates; idempotent per message
classify promote               # a second run must report zero decisions
```

Idempotency is structural (`UNIQUE (normalized_message_id)` on
`classify_promotions`); a promotion row with `task_id IS NULL` is the crash
artifact — decided, not carried out — and later passes leave it alone on
purpose. Counters per lane render on `/funnel` under "Classify promotion".

## Inquiry promotion (SWT-40 Part C)

The inquiry lane's exit from shadow. `classify promote --lane inquiry` turns
stored inquiry verdicts into **holding** tasks on the project's board, as
`promote:inquiry`; the pipelined `inquiry_promote` stage calls the same
function (`docs/runbooks/pipeline.md`). Nothing is sent, and no console can
claim these tasks (`assignee_type=human`). `--lane personal`, the default, is
byte-identical to before.

**What promotes.** An ok `classify_inquiry` verdict with `needs_reply`, on an
inbound message whose LATEST capture decision (any mode) is `attributed` to a
project with `ai_inquiry` and `inquiry_promote_after` set, recorded at or after
that cutover, sent within 72h, and not already promoted. Then a deterministic
gate, which reports the first failing reason:

| reason | means |
|---|---|
| `rethreaded` | the message's thread is no longer the one it was classified on |
| `kind` | `ask_kind` outside {question, request, decision, scheduling}; `fyi` asks nothing |
| `stale` | sent more than 72h ago |
| `pending` | sent less than 1h ago: the grace, so a reply can land first |
| `answered` | Salvador posted on the thread (or in the DM or conversation) after the ask |
| `not_addressed` | not gmail, not a 1:1 Slack DM, and not a thread he posted on before the ask |
| `claude_task` | the thread's open or dismissed task is not `assignee_type=human`: it is never attached to (no log on a worker's task), never reopened, and never shadowed by a second task (C-D13) |

A gated verdict writes nothing and is counted in the stats line's `gated`
block. A pending one promotes on the first pass after its hour. A passing
verdict attaches to the thread's open task, reopens a dismissed one (SWT-36),
or creates a new `holding` task titled `{asker}: {ask}`.

**Arming it.** Off until a human sets the lane's own cutover, after task #110
has its provenance (C-D11):

```sql
UPDATE projects SET inquiry_promote_after = now() WHERE slug = 'collaboratory';
```

**Dry run, and the backfill read.** `classify promote --lane inquiry
--dry-run` prints the plan (every would-create line shows `status=holding`)
and writes nothing. `--max-age 720h` widens the 72h fence for that read only.
It is refused unless `--dry-run`, because the fence is what keeps historical
asks off the board.

**Holding first (O7), and the flip.** New inquiry tasks land in Holding
(`classify_promotions.action='review'`) because `inquiryCreateStatus` is
`"holding"`, a Go constant. After about two weeks, if the readout below
satisfies Salvador, the flip is one line, `inquiryCreateStatus = "ready"`
(action `task`), plus the pinned test it must edit in the same diff. Tasks
already created stay where they are.

**The readout, O7's flip signal:** `classify promote --lane inquiry --outcomes
[--since 336h]`. It folds every promoted inquiry task on its FIRST dismissal:

| outcome | condition |
|---|---|
| `false_positive` | first dismissal `not_actionable` or `wrong_kind`: not a real ask |
| `true_positive` | closed or delivered with no dismissal, or first dismissal `handled_elsewhere` |
| `mis_click` | a human reopened the first dismissal: a mis-click, not a label |
| `excluded` | `duplicate`, still open, or an `attached` promotion |

An activity reopen (a new message on the thread) does not undo a label. The
counts always print; a precision ratio (true positives over decided) prints
only at 120 decided or more, with the indicative marker below that. It
measures precision, never recall: an ask the model missed never becomes a
task, so no dismissal can count it. It is a read-only readout, not an eval:
nothing enters the labelled set, and `classify eval` is untouched.

**Stuck claims.** The promoter writes its claim (the `classify_promotions`
row) before it calls the executor, then records the task on that row
(claim-before-act, criterion 12). A crash or a transient error between the two
leaves a claim with `task_id` NULL, and the inbox skips that message from then
on: at most once, never a duplicate task. The personal lane and capture share
that contract. `--outcomes` prints these after the counts, under `stuck claims
(task_id NULL, all time, every lane)`: the total, then one line per lane with
the count and the oldest claim. `--since` does not narrow it. A claim that a
running pass is still working on shows there for a few seconds; one older than
a pass is stuck. To resolve one by hand:

1. Find it: `SELECT cp.id, cp.normalized_message_id, cp.action, cp.created_at,
   r.worker_type FROM classify_promotions cp JOIN ai_extractions e ON e.id =
   cp.ai_extraction_id JOIN ai_runs r ON r.id = e.ai_run_id WHERE cp.task_id IS
   NULL;`
2. Check whether the executor acted before it failed. Look for an `ok`
   `create_task` or `task_append_log` row: `SELECT id, tool, status, task_id,
   started_at FROM audit_events WHERE actor LIKE 'promote:%' AND started_at
   BETWEEN <created_at> AND <created_at> + interval '5 minutes' ORDER BY id;`.
   A promoted task's body names its message on a line of its own, so `SELECT
   id, project_id FROM tasks WHERE position(E'normalized_message_id: <message
   id>\n' in body) > 0` finds one even when the crash came before
   `task_set_source_thread`. Anchor on the newline: a bare `LIKE` on the id
   also matches longer ids (12 matches 123). Both lanes write this line, so
   check the project matches the claim's.
3. If a task exists (or, for `action='attached'`, the log landed on the
   thread's task), link it: `UPDATE classify_promotions SET task_id = <task>
   WHERE id = <claim> AND task_id IS NULL;`
4. If there is no task, delete the claim: `DELETE FROM classify_promotions
   WHERE id = <claim> AND task_id IS NULL;`. The next pass or sweep promotes
   the message again if its verdict is still eligible (on the inquiry lane,
   that includes the 72h fence).

Automatic recovery of stranded claims is the follow-up ticket SWT-50.

## Routing lane (SWT-40 Part B)

The fourth lane, `route` (`worker_type=classify_route`, prompt `route-v2`, contract
`route_verdict`). It answers one question for a message the capture rules left
**unmatched**: which of its receiving account's candidate projects does it belong
to? Spec: `docs/tickets/inquiry-promote_SPEC.md`, Part B. The pipelined stages
are in `docs/runbooks/pipeline.md` ("The route stages").

**Closed candidate sets, per receiving account.** Only accounts with rows in
`source_account_projects` are routed at all. A row is the authorisation to move
that mailbox's mail into the project, so it is written only through the humanOnly
executor tools, from opsctl:

```
opsctl route-candidates add --account salvador@handsonconnect.org --project collaboratory --default \
  --description "university partner integrations: activities, sync, request/response validation"
opsctl route-candidates add --account salvador@handsonconnect.org --project reengine \
  --description "the ReEngine platform and its LHH tickets"
opsctl route-candidates list      # per account: numbered as the prompt numbers them; SHADOW or armed since …
opsctl route-candidates remove --account … --project …
```

At most one default per account. `add` refuses an unknown or ambiguous account
(one address under two providers), an unknown project, an empty description, a
project the account already lists, and a second default. The description is
what the model reads on the candidate's line: write what the project covers,
never a sender's name.

**What the model decides, and what the spine decides.** The model answers
`{project_index, evidence, reason}`: an index into the numbered candidate list
(or `null`) and a VERBATIM quote. It is never asked how sure it is (the constant
of section 2 above). Two deterministic rules replace that:
- `ResolveCandidate` turns the index into one of the account's own rows (`null`,
  0 or out of range → none), the way `ResolveLink` resolves a link.
- **Grounding**: a choice counts only if the evidence, whitespace-collapsed and
  case-folded, is a substring of the subject or the body, and is at least two
  words and 8 characters long (`GroundMinWords`, `GroundMinChars`). A
  paraphrase is ungrounded. So is a quote of the sender, because a shared sender
  is not enough, and so is a trivial span like "the" (SPEC B-D4 amendment,
  2026-09-13; prompt `route-v2`).

Both are decided at classify time and recorded on the verdict
(`fields.project_id`, `fields.grounded`), so `route_apply` never re-reads a body.

**The four steps** (`capture.DecideRoute`, pure), applied by the `route_apply`
stage to an inbound message on an ARMED account whose live decision is
`unmatched` and whose latest decision is still `unmatched`:

| step | when |
|---|---|
| `thread` | the thread's other messages are attributed to exactly one project by the rules or the gate, and it is a candidate (a neighbour's route row never counts: a route never begets a route) |
| `single` | the account has exactly one candidate |
| `model` | the newest verdict is grounded and names a candidate |
| `default` | otherwise, the account's default (O3); no default → the message stays unmatched (`no_default`) |

No verdict yet → `pending_verdict`: a missing verdict never falls to the
default. A verdict recorded before the account's `route_after` →
`verdict_before_arming`: not applied and not defaulted (steps 1-2 still apply).
A candidate removed (`route-candidates remove`) while a pass is running →
`candidate_revoked`: the insert re-checks the candidate row, writes nothing, and
the message retries on the next pass against the current candidates.

**What a route is.** A `capture_decisions` row with `mode='route'`,
`action='attributed'`, a `route_step`, and `ai_extraction_id` iff the step is
`model`. No rule, no task, no tool call, nothing sent. One per message, forever
(`capture_decisions_route_uniq`). It becomes the message's latest decision, so
the inquiry lane and its promotion follow it, and the residue and triage inboxes
drop it. A later shadow `--all` capture pass writes nothing for it. Accepted
residuals: a rule added after routing does not re-point a routed message, and
removing a candidate does not unroute what was routed.

**Shadow, then arming.** With `source_accounts.route_after` NULL the account is
in shadow: the `route` stage records verdicts, `route_apply` writes nothing.
Read the shadow with:

```
classify report --lane route [--since 168h]
```

It breaks the lane down by receiving account: verdicts (`grounded`, `ungrounded`
— a candidate chosen but not quoted, so the default applies — and `no choice`),
`pending_verdict` (live-unmatched on a candidate account with no current verdict),
and route_apply's rows by step (zero while in shadow).

**The go-live gate is an eval against the rules tier's own answers** (B-D7).
Take 120 or more messages on the mailbox that RULES attributed to one of its
candidates, hide the answer and score agreement per project:

```sql
-- read-only on prod; export to a scratch file OUTSIDE the repo
SELECT nm.id AS message_id, p.slug AS label, nm.subject
  FROM normalized_messages nm
  JOIN raw_source_items ri ON ri.id = nm.raw_source_item_id
  JOIN source_accounts sa ON sa.id = ri.source_account_id
  JOIN LATERAL (SELECT cd.matched_rule_id, cd.action, cd.project_id FROM capture_decisions cd
                 WHERE cd.message_id = nm.id ORDER BY cd.id DESC LIMIT 1) latest ON true
  JOIN projects p ON p.id = latest.project_id
 WHERE sa.account_email = 'salvador@handsonconnect.org'
   AND nm.direction = 'inbound'
   AND latest.matched_rule_id IS NOT NULL
   AND latest.action IN ('attributed', 'task', 'task_log')
   AND latest.project_id IN (SELECT project_id FROM source_account_projects WHERE source_account_id = sa.id)
 ORDER BY random() LIMIT 150;
```

Write each row as `{"message_id":…,"label":"<slug>","subject_sha256":"<classify.SubjectHash(subject)>","stratum":"rules"}`
into `docs/evals/route-from-rules.jsonl`. Hash in Go with `classify.SubjectHash`,
as the other label files are (Postgres reads `\b` and friends differently, and
the file carries no content). Then:

```
classify eval --lane route      # --labels defaults to docs/evals/route-from-rules.jsonl
```

The loader requires `stratum: rules` on every line and a slug-shaped label, and
the eval refuses a label that is not a `projects.slug`. It prints a label × routed
count table, agreement per project, and every disagreement by id. A ratio prints
only at 120 scored labels or more (`EvalResultThreshold`). The label set is biased
easy, so **read every disagreement by hand before arming.**

**Overall agreement is inflated by default fallbacks.** A message the model leaves
unrouted (no choice, or ungrounded) lands on the account's default, which agrees
with every default-project label for free. The number that measures the model is
the non-default candidate's row: on handsonconnect, **read the `reengine` row**,
not the overall figure. The eval takes `--checkpoint` like every lane (per-message
resume; the file is removed on success). A verdict that does not parse, or never
arrives, is scored as a miss and counted on the `misses:` line; it never aborts
the batch. Then skim the real
routes for the unmatched mail (the dry-run list: `classify report --lane route`
plus the `pending_verdict` counts).

**Hard precondition: arm an account only after EVERY capture binary runs the
Part B image.** That means all connector CronJobs, `pipelined`, and any hand-run
`opsctl`, rebuilt from main. A pre-Part-B capture pass excludes only gate rows,
so an old binary's shadow `--all` pass would write a newer `unmatched` row above
a route row and bury it for every latest-decision reader. There is no DB guard:
no route row exists before arming, so the ordering is the guard (SPEC B-D7
amendment 2026-09-13).

**Arming is a human decision, per account, by hand:**

```sql
UPDATE source_accounts SET route_after = now() WHERE account_email = 'salvador@handsonconnect.org';
```

Nothing in code sets or clears it (a structure test scans for that). Step 3 is
forward-only on the verdict clock: a verdict recorded before `route_after` is never
applied. So once armed, the route classify inbox treats a verdict as current only
if it was recorded at or after `route_after` (SPEC amendment 2026-09-13). Every
message whose verdicts all predate arming is back in the inbox exactly once, and
the post-arming backfill re-classifies it:

```
classify run --lane route --since 720h     # one fresh verdict per shadow-verdicted message, once
```

`route_apply` then routes it on its next sweep or `route_classified` wake. To
disarm, set the column back to NULL by hand; rows already written stay.

**Locality.** Every message this lane reads is unmatched, so it carries
Attribution = AttrUnmatched and `ClassOf` restricts it (the residue lane's
mechanism). The prompt carries only the message and the candidate rows: no thread
context, no links. `cmd/classify` and `pipelined` build the router with no hosted
client. An all-skipped pass means the local model is down; a hosted fallback is
never the fix.

**`--since` is required** on `classify run --lane route` (the refusal says so).
