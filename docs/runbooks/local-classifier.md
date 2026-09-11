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
