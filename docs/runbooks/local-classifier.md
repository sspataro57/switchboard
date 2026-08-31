# Runbook — the local actionability classifier (SWT-22)

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
curl -s http://127.0.0.1:11434/api/chat -d '{
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
export OPS_LOCAL_PROVIDER_URL=http://127.0.0.1:11434   # NO /v1 — native API
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

## The labelled set

`docs/evals/personal-actionability.jsonl` — the only thing anyone is permitted to
tune against. It carries **no message content**: message id, label, and a hash of
the normalised subject. Bodies are loaded from the database at eval time, so the
file is safe to commit while the mail never leaves the machine.

**Current set and scores** (record here on every regeneration or re-run):

| date       | n   | actionable | recall | precision | median latency | model    |
|------------|-----|-----------:|-------:|----------:|---------------:|----------|
| 2026-08-30 | 280 | 35         | 0.83   | 0.58      | 6.3 s          | qwen3:8b |

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
