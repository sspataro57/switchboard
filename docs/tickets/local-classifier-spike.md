# Spike — local model for actionability classification (2026-08-28)

Measured, not argued. Everything here ran against the real corpus on the
workstation's **RX 6650 XT (RDNA2, 8 GB) via Vulkan**. Decision at the end.

**Outcome: qwen3:8b** (Salvador, 2026-08-28).

## Why this was worth measuring

SWT-17 shipped and left 15,993 messages unmatched — triage's inbox. That
residue is disproportionately personal, and personal mail is the reason the
classifier must be local (SWT-21). The open question was whether a local model
is a *compromise* accepted for privacy, or actually good enough. It is the
latter, which materially strengthens SWT-21: the restricted lane is not a
degraded lane.

## Results

Two evals. The general one is 30 actionable / 45 not, drawn from the unmatched
pile with a deliberate **near-miss class** (statement-available, balance,
card-was-used from the *same* senders) so nothing can win by matching on sender.
The Pines one is all 31 messages from a single HOA property manager that sends
both routine announcements and violation notices carrying fines — the
same-sender-different-actionability case in its purest form.

| model | where | general recall | general precision | Pines recall | Pines missed | median |
|---|---|---:|---:|---:|---:|---:|
| **qwen3:8b** | local | **0.90** | 0.61 | **1.00** | 0 | 1.23s |
| gemma3:12b | local | 0.87 | 0.76 | 0.40 | 3 | 5.69s¹ |
| gemma3:4b | local | 0.80 | 0.86 | 0.80 | 1 | 0.93s |
| gpt-5.4 | proxy | 0.80 | 0.92 | — | — | 1.31s |
| gpt-5.4-mini | proxy | 0.77 | 1.00 | 0.40 | 3 | 1.20s |
| gpt-5.6-terra | proxy | 0.73 | 1.00 | — | — | 1.50s |
| gpt-5.6-sol | proxy | 0.73 | 1.00 | — | — | 1.94s |
| llama3.1:8b | local | 0.70 | 0.84 | 1.00 | 0 | 2.52s |
| qwen3:4b | local | 0.57 | 1.00 | — | — | 0.65s |
| qwen2.5:3b | local | 0.10 | 0.27 | — | — | 0.71s |

¹ spilled to CPU — the desktop holds ~3 GB of the 8 GB. On a headless box it fits.

**Recall is the objective, not accuracy.** A missed payment notice is a late fee;
a false alarm costs a second to dismiss (Salvador, 2026-08-28: "safer false
positives than false negatives"). Accuracy is actively misleading here — always
answering "not actionable" scores 99.8% on the real distribution.

## What the numbers say

**Local beats hosted on recall.** Nothing on the proxy exceeded 0.80, and
gpt-5.4-mini and gpt-5.6 both missed **3 of 5** violation notices on Pines.
Local is also faster, having no network hop.

**The newest hosted models were the worst of them** — gpt-5.6-terra and -sol at
0.73, below both 5.4 variants, and slowest. Consistent with what Salvador found
independently on proposalWriter. This is a small crisp discrimination; extra
capability shows up as latency, not accuracy.

**Bigger did not win.** gemma3:12b is *worse* than gemma3:4b on Pines (0.40 vs
0.80). Scaling within a family moved the balance toward caution — the wrong
direction here.

## Three findings that will bite whoever builds this

**1. Thinking models are the wrong tool, and fail invisibly.** The first qwen3:4b
run scored 0.00 across the board with 70/70 malformed outputs: it spent its
entire token budget reasoning about a ten-word subject line and returned EMPTY
content with `done_reason: length`. Set `"think": false`. Reasoning is pure
latency on a binary classification.

**2. Self-reported confidence is a constant and must not be used as a
threshold.** qwen3:8b returns exactly `0.95` on everything it flags — 27 true
positives and 17 false positives, identical. There is no dial. This is the
"predicate whose discriminating column is a constant" landmine wearing a model's
clothes; a confidence gate built on it would look principled and do nothing.
To trade precision for recall, use a **second pass** — let qwen3:8b flag broadly,
then re-check only the flagged subset with a *different* model. Two disagreeing
small models beat one model's self-assessment.

**3. Do not prompt differently per sender.** That is rules in a costume:
unbounded maintenance, untestable in aggregate, and unattributable when it
misfires. What Pines actually shows is a missing *context* fact — the model does
not know this sender emits both announcements and fines — and a fact belongs in
a column, editable through `opsctl` and visible in the report, injected into ONE
prompt. Better still, "First Notice" and "violation" are keywords a rule catches
perfectly and reproducibly; the classifier's job is the sender nobody has
written a rule for yet.

## Operational facts

- ROCm **crashed** during GPU discovery on gfx1032. Vulkan works and is OFF by
  default: `OLLAMA_VULKAN=1`. Expect the same on the RX 570 (Polaris/gfx803),
  where ROCm support was dropped entirely — Vulkan is the only route.
- qwen3:8b Q4 is ~5.2 GB. It did NOT fully fit here (35/37 layers on GPU, output
  layer on CPU) because the desktop holds ~3 GB. On a headless 8 GB card it fits.
- Prompt size is small: the classifier needs subject + sender for most senders,
  ~25 tokens. **Pines is the exception that proves the body is sometimes
  required** — its subjects are a `[#XN######] Message from <Association> - ...`
  template with the topic truncated away, so the signal is only in the body.
- Ceiling on Pines regardless of model: the body says *"Please see attachment for
  additional detail"*, and the fine amount and cure-by date are in a PDF that is
  ingested inside `rfc822_b64` but never extracted. No classifier can read them
  today. Raising "HOA violation notice — open the attachment" is the honest
  best, and the raw message is preserved, so extraction is future work rather
  than lost data.

## Caveats on this spike's own credibility

- 75 general and 31 Pines messages. Roughly ±10 points of noise: 0.80 vs 0.77 is
  not a real difference; the local-vs-hosted recall gap probably is.
- **Labels are mine, generated by regex, and were wrong once already.** The first
  general eval scored every model at 0.10–0.27 recall because it labelled "your
  statement is available" as actionable while the prompt said informational
  notices were not. The models were right and the fixture was wrong; gemma3:4b
  went 0.27 → 0.80 on the fix. The Pines labels are auto-generated the same way
  and are worth eyeballing before anyone tunes against them.
- A properly labelled set — a few hundred messages, hand-checked — is the
  prerequisite for tuning anything. This spike answers "is it good enough to
  build on", not "which threshold".

## Left on the workstation

An `ollama serve` process (started with `OLLAMA_VULKAN=1`) and ~25 GB of models
under `~/.ollama/models`: qwen3:8b, gemma3:12b, gemma3:4b, llama3.1:8b,
qwen3:4b, qwen2.5:3b-instruct. Only qwen3:8b is needed going forward; the rest
are ~20 GB reclaimable with `ollama rm`.

---

## Reproduced and extended, 2026-08-28 (during SWT-22 ticket-start)

Re-run against the same box and model before building an adapter that depends on
these findings. All four are wire-level measurements, not recollections.

**1. The thinking failure reproduces EXACTLY.** With `"think": true` and a
200-token budget, qwen3:8b on `/api/chat` returned:

```
done_reason: length | content length: 0 | thinking length: 974 | eval_count: 200
```

Unparseable, because there is nothing to parse. The whole budget went to
reasoning about a two-line message. Same request with `"think": false`:
`done_reason: stop`, 242 characters of valid schema-conforming JSON, 5.7s.
This is criterion 3's entire justification and it is current, not historical.

**2. The prompt is load-bearing, and far more so than the model.** The first
attempt here used a bare "Classify." instruction with no system prompt, and
qwen3:8b called an HOA violation notice carrying a fine `actionable: false` — a
miss on precisely the class the spike reports 1.00 recall for. With a proper
system prompt (defining actionable as "requires the recipient to DO something
with a consequence", and naming the near-miss class explicitly), the same model
scored 5/5:

```
HOA violation + fine      -> true   (want true)
payment due               -> true   (want true)
statement available       -> false  (want false)
card was used             -> false  (want false)
HOA announcement          -> false  (want false)
```

Both near-misses correct — the cases this spike built specifically to defeat
sender-matching. So the spike's numbers reproduce; the adapter is the easy half.

**3. Dropping the near-miss guidance breaks it, which is what the labelled set is
for.** A SHORTENED system prompt, identical except that it no longer enumerated
"statement-available, balance notices, card was used" as informational, flipped
"your statement is available" to `actionable: true`. One clause in the prompt is
the difference between a false positive and a correct answer on the single most
common message shape in the corpus (883 BofA messages). Tune the prompt against
a labelled set, never against intuition.

**4. `format` enforces a JSON-schema `enum`, so `kind` MUST be one.** Left as a
free string, the model returns the same concept in three casings —
`"payment due"`, `"violation_to_cure"`, `"statement-availabl…"`,
`"transaction_notifi…"` — which is a column nothing can `GROUP BY`. Constrained
to an enum, every response landed inside it. An unconstrained `kind` would be a
report field that silently means nothing, which is this repo's recurring landmine
in yet another costume.

**5. The corpus is BILINGUAL, and the original is kept — no translation pass.**
51 of the 1,609 personal messages are Spanish; Bank of America duplicates its
alerts. Decision (Salvador, 2026-08-28): "if it can parse spanish better to keep
the original."

Measured on matched pairs — the same notice in both languages, which this corpus
supplies for free:

```
we've transferred money to cover…  EN true/payment_due   ES true/payment_due   agree
your available balance is low      EN true/payment_due   ES true/payment_due   agree
your statement is available        EN false/informational ES false/informational agree
a direct deposit was credited      EN false/informational ES TRUE/payment_due   DISAGREE
your account may not have funds    EN true/payment_due   ES true/payment_due   agree
```

4/5, with valid schema output in both languages every time. So Spanish is not a
comprehension problem and a translation pass would be a second inference per
message plus a new failure mode — a bad translation silently changes the answer
and leaves nothing to compare against. Keeping the original also keeps
raw-first's promise that reprocessing is always possible.

The 1/5 disagreement is the finding, not a rounding error: `a direct deposit was
credited` is a BORDERLINE case where the model is unstable, and the language
flipped it. A translation pass would have concealed that instability rather than
fixed it. Consequence for the eval set: the Spanish messages get their own
labelled rows (12 are in the worksheet) so language-driven disagreement is
MEASURED per release rather than assumed away, and the prompt is written to be
language-neutral rather than English-with-Spanish-tolerated.

**6. CORRECTION — the "violation PDF" does not exist, and attachments are not the
ceiling this document claimed.** Measured by decoding `rfc822_b64` for every
Pines / Pembroke Pines message, 2026-08-28.

This document said: "the fine amount and cure-by date are in a PDF that is
ingested inside `rfc822_b64` but never extracted. No classifier can read them
today." Both halves are wrong.

The two actual `First Notice` messages carry **NO attachment at all** — 8,243
bytes, a single `text/html` part. The body says *"Please see attachment for
additional detail"* and there is no attachment; the sender's template promises
one it does not send. What the body DOES carry is extracted and available today:

```
inspection conducted on 08/12/2026 by the Silverlakes Community Association Inc
Covenants Enforcement Committee ... the following violation of the covenants was
noted: Shrub Trim- Pursuant to the SL Mod Gudelines ... (p. 28)
```

Date, specific violation, and the rule cited. `body_text` for that message is
1,254 characters — the normalizer extracts HTML correctly, and only 1 of 1,609
personal messages has a body under 40 characters.

What the Pines PDFs actually are: board meeting agendas and notices, proposed
parking rules, community guidelines, portal login info. Announcements.

The ONE fact genuinely locked in an attachment is the **amount on the Pembroke
Pines utility bills** (4 messages, `application/octet-stream`). Their body gives
the due date, customer id and account number — everything except the figure. And
"utility bill due 09/01/2026" is already an actionable task without it.

So attachment extraction is a small enhancement affecting four messages, not a
hard ceiling on the classifier. Do not budget for it as though it were.

**Design consequence — reference attachments, never copy them onto tasks.** The
complete MIME already lives in `raw_source_items.rfc822_b64` (invariant 1), so a
task copying those bytes creates a second copy of something already stored and a
second thing to keep in sync, and forces a storage decision — blob column?
attachments table? filesystem? — that nothing today needs. A task carries the
message id; rendering or downloading the attachment from the raw item on demand
is a dashboard concern.
