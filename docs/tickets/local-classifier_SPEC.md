> Jira: SWT-22
> Jira description is SPLIT: this SPEC is ~47k characters of wiki markup and
> Jira's description field caps at 32,767. The issue carries everything up to
> "In scope / Out of scope"; the remainder is a comment on the issue. A
> re-sync must split at an h2 boundary again, not truncate. This file is the
> authoritative copy.

# local-classifier — qwen3:8b behind the locality boundary, and what it actually reads

**Status: FINAL** (both questions answered 2026-08-28; see
`docs/tickets/local-classifier_OPEN_QUESTIONS.md`).

**Q1 — which pile: BOTH.** Which, as that file argued, is this ticket plus a
cheap second one rather than a third option. THIS ticket builds the adapter, the
`classify` worker in shadow over the ~1,609 `personal`-attributed messages, the
labelled eval set, and triage's local lane wired and proven — the last of which
is what makes the ~14,500 `unmatched` residue reachable at all. **SWT-23** then
runs over that residue, and owns the question this ticket does not answer: what
prompt the residue gets, since triage's existing prompt asks client-work
questions and asking those of newsletters is ~5 GPU-hours of asking the wrong
question.

**Q2 — where it runs: Option A**, the workstation, by hand. No new hardware, and
one environment variable from the cluster or from camserv later. The two facts
that keep that move cheap are in this SPEC rather than left to be re-derived:
`LocalityOf` accepts only a numeric address (so a cluster path needs a
`192.168.50.x` load-balancer IP, never a service name), and on camserv both
models should be held resident so the narrow slot is paid once at boot.

**One model this ticket: qwen3:8b.** The second-pass precision mechanism stays in
Future work.

## Source

Not a build-order step. Ad-hoc, from the SWT-22 issue (written 2026-08-28):

> A local provider adapter so triage can run again. SWT-21 makes an unmatched
> message restricted, and triage's entire inbox is unmatched, so triage cannot
> run at all until this exists. That ordering was chosen deliberately rather than
> fallen into: the boundary should not depend on a sender list being complete.
>
> The model is chosen and measured, not argued: qwen3:8b, from the spike. 0.90
> recall on a mixed eval against 0.80 for the best cluster-proxy model; it caught
> all five HOA violation notices where gpt-5.4-mini and gpt-5.6 each missed three
> of five. Recall is the objective — a missed payment notice is a late fee, a
> false alarm costs a second to dismiss.

Measurement inputs: `docs/tickets/local-classifier-spike.md` (2026-08-28).
Socket this plugs into: `docs/tickets/provider-locality_SPEC.md` (SWT-21,
shipped) and `docs/runbooks/provider-locality.md`.

## Premises, verified against this repo and the production db before writing

Re-measure the counts at verification time; the corpus is live and a frozen
literal cries wolf every day a message arrives (SWT-19's lesson).

1. **The wiring already exists; the server and the adapter do not.**
   `cmd/triage/main.go:104-128` already reads `OPS_LOCAL_PROVIDER_URL`,
   `OPS_LOCAL_MODEL` and `OPS_LOCAL_API_KEY`, builds a two-lane
   `provider.NewRouter`, and warns when the URL is not local.
   `provider.NewOpenAI` already implements `Describe()` and `Probe()`
   (`internal/provider/openai.go:32-58`). What is missing is an adapter that
   speaks ollama's request shape, a reachable server, and a decision about what
   the classifier processes.
2. **There are two distinct populations, and they are not the same work.**
   Measured 2026-08-28 after the 18 personal capture rules (ids 11-28) were
   seeded and a full shadow pass ran (considered 49,506 / matched 35,033 /
   unmatched 14,473 / tasks_created 0):
   - **~14,500 `unmatched`** messages — triage's inbox
     (`internal/triage/store.go:52-69`, latest decision `action='unmatched'`).
     Restricted only *because* unmatched. Mostly newsletters, job alerts,
     GitHub/LinkedIn notifications.
   - **~1,609 `attributed` to `personal`** — the bank / HOA / health mail the
     whole boundary was built for. **No worker consumes these at all**, because
     triage's pending filter takes `unmatched` and an attributed message is not
     unmatched. `docs/runbooks/provider-locality.md` already records this
     ("nothing consumes it at all").
3. **The spike measured population 2, not population 1.** Its general eval was
   drawn from the unmatched pile *before* the personal rules existed, and its
   Pines eval is a single HOA property manager — `pinespropertymanagement.com` is
   capture rule material today, so every Pines message is now `attributed` to
   `personal`. Applying the spike's numbers to what is left in `unmatched` would
   be quoting a measurement of a corpus that no longer exists.
4. **Triage's prompt is a client-work prompt.** `internal/triage/prompt.go:19-60`
   opens "The input is ONE inbound message from an EXISTING client thread … and a
   list of currently-open tasks for that client's project", and its schema
   carries `attach_to_task_id` over a candidate set. Neither population is that.
   A classifier prompt is a new prompt either way.
5. **Migration state is current.** `migrations/0016_provider_locality.sql` is the
   highest and is applied to production; `projects.ai_locality` exists, every
   pre-existing project is `any`, `personal` is `local_only` with a NULL client.
6. **`capture_decisions` guarantees the shape this ticket filters on.**
   `action='attributed'` implies a non-NULL `project_id`
   (`capture_decisions_unmatched_has_no_project`, migration 0015), and capture
   only ever evaluates `direction='inbound'` (`internal/capture/rules_store.go` —
   that line is invariant 5).
7. **ollama is running on the workstation** with `OLLAMA_VULKAN=1` and six
   models (~25 GB), qwen3:8b among them. ROCm crashes during GPU discovery on
   gfx1032; Vulkan is the only route and is off by default. The RX 570 / camserv
   box from the spike's plan is **not installed**.
8. **None of the workers are deployed.** Institutional knowledge, "Deploy":
   *still not deployed: orchestrator, triage, drafts, fleetd, hooksd*. Triage has
   only ever been run by hand from the workstation.

## Runtime behaviour of ollama, measured 2026-08-28 (not assumed)

Measured on this workstation because a session assumed the wrong thing about the
first item and was corrected by Salvador.

**Models are DISCHARGED, not held.** `/api/tags` lists what is on disk;
`/api/ps` lists what is resident. After `keep_alive` expires (5 min default)
VRAM is released and the next request pays a full reload. "Six models installed"
is six models on disk and zero bytes of VRAM.

**Cold vs warm, qwen3:8b, 5.9 GB:**

| | measured |
|---|---|
| cold (disk → VRAM) | 3.40s load + 0.47s work = **4.04s** |
| warm (resident) | **0.25–0.38s** per message |
| residency here | 5.1 GB of 5.9 GB in VRAM (**85%**) — the desktop holds 2.7 GB of the 8.2 GB card |

gemma3:4b for comparison: 3.81s cold, **0.45s** warm, **100%** on GPU (4.3 GB).
Note the small model is SLOWER per message despite being fully resident — model
size is not the cost driver at this scale, so "use a small model for the cheap
second pass" is true only because the subset is small, not because the model is.

**ONE MODEL THIS TICKET.** qwen3:8b, nothing else (Salvador, 2026-08-28: "let's
just use qwen now and we figure out later about the other model"). The two-model
arithmetic below is recorded because it was measured and because it answers
whether the future second pass is affordable — it is NOT a description of what
this ticket builds. Anything here that loads a second model is out of scope; see
Future work.

**Because triage and this worker are BATCH, the cold load is amortised**: it is
paid once per pass, not once per message. Over the 1,609-message personal pile
that is 3.4s against ~6.8 min of work — **0.8% overhead**. This is the fact that
makes a per-pass model swap affordable, and the reason the two-pass precision
mechanism (below) is worth keeping as a real option rather than a theoretical one.

**Two-pass cost, measured rather than estimated** (qwen3:8b over all 1,609, then
gemma3:4b over only what pass 1 flagged):

```
flag rate  2%:  pass1 6.8 min + pass2 0.3 min = 7.1 min   (+5%)
flag rate  5%:  pass1 6.8 min + pass2 0.7 min = 7.4 min  (+10%)
flag rate 10%:  pass1 6.8 min + pass2 1.3 min = 8.0 min  (+19%)
```

The swap is a per-PASS constant, not a per-message one, which is what makes a
second independent opinion cheap. **These figures are x16-PCIe figures and do not
transfer to a narrow slot — see Q2.**

**`keep_alive: -1` pins against TIME, not against MEMORY PRESSURE.** Verified:
pinning qwen3:8b set its expiry to the year 2318, and then requesting gemma3:4b
evicted it anyway — one model resident afterwards. You cannot pin your way past
5.1 + 4.3 GB not fitting in 5.5 GB of free VRAM. Consequence for this ticket:
**do not write swap management.** Ollama already evicts and loads correctly; code
that tries to help will be code that fights it.

**CPU offload is not a small penalty on a narrow bus.** Spilled layers stream
over PCIe *per token*, not once at load. The 15% spill measured here costs little
on x16; on a bandwidth-limited slot it dominates. So "does the model fit entirely
in VRAM" is a hard requirement wherever the slot is narrow, and it is checkable —
`size_vram / size` from `/api/ps` must be exactly `1.00`. A partial fit produces
silent slowness, never an error.

## Goal

Ship a local `provider.Client` for ollama that satisfies the SWT-21 boundary, and
put it to work on the population that has no consumer: give `personal`-attributed
mail a shadow-mode actionability classifier, with a labelled eval set as the only
thing anyone is allowed to tune against.

**Usable alone.** After this ticket, with no cluster change, no new hardware and
no migration:

1. The restricted lane is provably not a dead lane: a real model answers through
   `Router.Route`, and `ai_runs` records `provider='ollama'`, `status='ok'`.
2. The ~1,609 personal messages that nothing reads get read — every pass flags
   the payment notices and violation notices and records why, creating nothing.
3. `triage run` processes messages again instead of skipping 100% of them
   (criterion 9's smoke), which is SWT-21's stated unblocking condition.
4. Recall is measurable rather than asserted: `classify eval` scores the
   classifier against a hand-checked labelled set and prints the misses.

## The scope decision this SPEC makes, and why

**This ticket classifies population 2 (the ~1,609 `personal`-attributed
messages). It does not classify population 1 (the ~14,500 `unmatched`
residue).** Pending Q1; if the answer flips, see "What changes under the other
answer".

Reasons, in order of weight:

- **Population 2 is where a miss costs money.** A missed payment notice is a late
  fee. A missed newsletter is a newsletter.
- **Population 2 has no consumer at all.** Attribution removed it from triage's
  inbox and gave it nothing else. Population 1 has a consumer (triage) that this
  ticket unblocks as a by-product.
- **Population 2 is what the spike measured** (premise 3). Building for
  population 1 would mean shipping against numbers taken on different mail.
- **Population 1 is 9× the volume and mostly noise.** At the spike's 1.23 s
  median that is ~5 GPU-hours per full pass on a card shared with a desktop, to
  produce shadow verdicts on brand marketing.

**Triage's unblocking is delivered but deliberately bounded**: this ticket gives
`cmd/triage` a working local lane and proves it with a `--limit 5` smoke. Whether
to point triage at the whole 14,500-message residue is an operator decision
recorded in the runbook, not something this ticket does.

## Acceptance criteria

Numbered and testable. 1-8 are unit tests with `httptest` — never a live model.

### The adapter

1. `internal/provider/ollama.go` defines `Ollama`, implementing both
   `provider.Client` (`Complete` + `Describe`) and `provider.Prober`.
   `provider.NewOllama` is called from `cmd/` only — the existing
   `internal/provider/callsites_test.go` scan (`provider\.New[A-Za-z]*\(`)
   covers it with no change, and `TestProviderConstructors_LiveInCmdOnly` is its
   positive control.
2. It POSTs to `{base}/api/chat` — ollama's **native** API, not the
   OpenAI-compatible `/v1` route. The reason is evidence, not preference, and
   belongs in the file's comment: the spike's failure signature was
   `done_reason: length` with EMPTY content, and `done_reason` is a native-API
   response field (`/v1` returns `finish_reason`) — so the measurement that
   chose this model was taken against `/api/chat`, and `think` is a native
   request field. An `httptest` test asserts the path.
3. **Every request body carries `"think": false` and `"stream": false`.** A unit
   test asserts both on the wire, and its failure message carries the spike's
   finding: with thinking on, qwen3 scored 0.00 with 70/70 malformed outputs,
   spending its whole token budget reasoning about a ten-word subject and
   returning empty content. It is invisible unless you read the raw response.
4. `Request.Schema` is sent as ollama's `format` (schema object), `MaxTokens` as
   `options.num_predict`, `options.temperature` pinned to 0, and `keep_alive`
   defaulting to `"30m"` (a 5.2 GB model reloaded per message is the difference
   between 1.2 s and ~10 s). Asserted on the wire.
5. `Describe()` returns `Descriptor{Name: "ollama", Endpoint: base}`.
   `NewOllama` trims a trailing `/` **and a trailing `/v1`**, logging when it
   trims. Reason: `docs/runbooks/provider-locality.md` currently tells the
   operator to set `OPS_LOCAL_PROVIDER_URL=http://127.0.0.1:11434/v1` for the
   OpenAI adapter, so a stale value is the likely case — and an untrimmed
   `/v1/api/chat` is a 404, which is an HTTP error, not `ErrUnavailable`, so it
   would trip criterion 16's unclassified-error raise instead of skipping. Unit
   test covers both spellings and asserts `LocalityOf(Describe())` is
   `LocalityLocal` for `127.0.0.1` and `192.168.50.x`.
6. `Probe(ctx)` GETs `{base}/api/tags` and requires **both** a 2xx **and the
   configured model present** in `models[].name`. Transport failure, non-2xx, or
   an absent model all return an error wrapping `provider.ErrUnavailable`.
   Rationale, in the doc comment: the spike left six models on the box and the
   plan is to `ollama rm` five of them, so "the server is up but qwen3:8b is
   gone" is a live failure mode — and it must be a SKIP (retried next pass), never
   an error and never a hosted call. Three `httptest` cases.
7. Failure typing, exactly per SWT-21 criterion 8: transport failure and deadline
   exceeded wrap `ErrUnavailable`; HTTP status, JSON parse failure, empty
   `message.content`, and `done_reason != "stop"` do **not**. The empty-content
   case returns an error naming `done_reason` verbatim — that is the
   thinking-model regression, and it must be loud rather than look like a busy
   box.
8. `Response.PromptTokens` / `CompletionTokens` / `LatencyMS` are filled from
   `prompt_eval_count` / `eval_count` / `total_duration` (ns → ms), so `ai_runs`
   reads the same way for both lanes.
9. `cmd/triage` builds the local lane with `provider.NewOllama` instead of
   `NewOpenAI`. **`OPS_LOCAL_MODEL` becomes required when
   `OPS_LOCAL_PROVIDER_URL` is set**: today it falls back to `cfg.Model`
   (`gpt-5-mini`), which on ollama is a 404 per message. Missing → the local lane
   is ABSENT with one logged refusal (fail closed; a skipped pass, not 14,000
   unclassified errors). Not a startup error.

### The classifier

10. New package `internal/classify` and binary `cmd/classify` with
    `run` / `report` / `eval`. `ai_runs.worker_type = 'classify'`. Single instance
    via `pg_try_advisory_lock`, key `0x5157_0022`; grep-verify no collision before
    use — `0x51570005` (orchestrator), `0x51570006` (triage), `0x51570007`
    (google `bridgeAccountLockNS`, and drafts per SPEC 08), `0x51570015`
    (capture) are taken.
11. **The inbox** is inbound `normalized_messages` whose LATEST
    `capture_decisions` row has `action='attributed'` **and** whose project has
    `ai_locality='local_only'`, with no `ai_extractions` row reachable through an
    `ai_runs` row of `worker_type='classify'`. Ordered oldest-first, `--limit` and
    `--since` as triage's.
    **Keyed on `ai_locality`, never on `slug='personal'`** — a slug literal in a
    predicate is the SWT-13 magic-literal landmine, and the property this filter
    means is "may only be processed locally", which is what the column says.
12. **The three-state discipline is restated for this worker**, and its
    integration test is the load-bearing one:
    - no decision row (unseen) → **not ours**, the engine has not looked;
    - latest `unmatched` → **triage's inbox, never ours**;
    - latest `attributed` to an `ai_locality='any'` project → **never ours**
      (client work, already routed, may already have a task);
    - latest `attributed` to an `ai_locality='local_only'` project → **ours**.

    An integration test seeds all four shapes **from Postgres** and asserts only
    the fourth is returned. Mutating the SELECT to drop the `ai_locality` join
    must turn it red.
13. **Honesty label, required in the code comment** (this repo's 7th-instance
    landmine, applied before it bites): because criterion 11 selects only
    `local_only` projects, **every message this worker sees is `ClassRestricted`
    by construction**. The class fold therefore cannot change an outcome here, and
    a unit test that supplies the class proves nothing about production. The two
    things that actually protect this worker are (a) the inbox filter, pinned by
    criterion 12's integration test, and (b) the Router's refusal, pinned by
    criterion 14's zero-hosted-calls test. The comment must say this rather than
    let the fold read as a guard.
14. The class is computed with the **same spelling** as triage and drafts —
    `provider.ClassOf` over the message, folded with `provider.MostRestrictive`
    over its **inbound** thread neighbours — and `classify.Run` takes
    `*provider.Router`. Add `{"internal/classify/classify.go", "func Run("}` to
    `TestWorkerEntryPoints_TakeARouter` and `internal/classify` to
    `workerPackages` in `internal/provider/callsites_test.go`. A unit test builds
    a Router with a nil local slot, runs a pass, and asserts the general fake
    recorded **zero** `Complete` calls.
15. **Shadow is structural**, exactly as triage's: `classify.Store` has no
    task-writing method, enforced by a reflection test in the shape of
    `internal/triage/worker_test.go`'s `TestShadow_StoreHasNoTaskWriteMethod`.
    Zero rows added to `tasks`, `task_events`, `deliveries`, `external_refs`.
16. Skip semantics are triage's, reused rather than re-invented: a route refusal
    writes ONE aggregate `ai_runs` row per pass (`status='skipped'`, payload
    `{avail_reason, avail_reasons, class_reasons, skipped_count, message_ids ≤100,
    sampled}`); a per-message `ErrUnavailable` writes a per-message skipped row
    with `avail_reason='local_unreachable'` and does not count toward the ratio;
    any other error writes `avail_reason='unclassified_error'` and does; the pass
    raises when unclassified errors exceed half the restricted-lane attempts with
    a floor of 20. No skip of any kind writes an `ai_extractions` row.
17. **The operational difference is structural and is tested end to end**
    (required by the brief):
    | | ai_runs | ai_extractions | next pass |
    |---|---|---|---|
    | the classifier looked and found nothing | `status='ok'` | one row, `actionable=false` | message is **gone** from the inbox |
    | no permitted provider looked | `status='skipped'` | **none** | message is **still** in the inbox |

    An integration test asserts both rows AND the re-appearance: run with a fake
    that skips, assert the message is still pending; run with a fake that answers
    `actionable=false`, assert it is not.
18. Prompt and schema live in `internal/classify/prompt.go`, with a
    `PromptVersion` constant recorded in every `ai_runs.input` (triage's
    convention). Output contract, deliberately small:
    `{actionable: bool, kind: enum(payment_due|deadline|appointment|action_required|informational), title: string, reason: string}`.
    **No confidence field anywhere**, and a test asserts the schema contains no
    `confidence` key, with the reason in its failure message: qwen3:8b returns
    exactly 0.95 on everything it flags — 27 true positives and 17 false
    positives, identical — so a confidence gate would look principled and do
    nothing. That is the constant-discriminator landmine wearing a model's
    clothes.
19. The prompt states the objective it was measured on — recall first, a missed
    payment notice is a late fee and a false alarm costs a second to dismiss — and
    handles the defer-to-attachment case explicitly: when the body defers to an
    attachment, say so in `title` ("HOA violation notice — open the attachment")
    rather than guessing an amount or a date. Note the measured wrinkle this
    guards against (spike finding 6): the Pines template says "please see
    attachment for additional detail" on messages that carry NO attachment, so
    the prompt must not promise the reader a document that is not there. The
    body itself carries the date, the violation and the rule. **One prompt for all senders.** A
    structural test fails any file in `internal/classify` that branches the prompt
    on a sender string: per-sender prompts are rules in a costume — unbounded
    maintenance, untestable in aggregate, unattributable when they misfire.
19b. **The verdict carries the message's actionable LINK, and the model never
    authors it** (Salvador, 2026-08-28: "what we need to do is preserve the links
    on the summaries"). This is added AFTER the test pass was written, so it needs
    its own tests.

    The rule that makes it safe: a model asked for a URL will invent a plausible
    one, and a hallucinated portal link on a task about a fine is worse than no
    link at all. So links are extracted DETERMINISTICALLY by the application and
    offered to the model as a numbered closed set of anchor TEXTS; the model
    returns `link_index` (an integer, or null), and the application resolves the
    index back to the URL. An out-of-range index is rejected. **The model cannot
    emit a URL because it is never given the chance to** — spine/leaves, exactly
    as CLAUDE.md states it.

    Extraction, measured over 400 personal messages on 2026-08-28:
    * ANCHORS ONLY (`<a href>`). Never `img src`: the only "link" in a Pines
      First Notice is an `<img>` pointing at `/wf/open`, a SendGrid open-tracking
      pixel. Following img src would put a tracking beacon on a task.
    * Drop by URL: unsubscribe, opt-out, privacy, terms, preferences, `/wf/open`,
      and asset/CDN hosts.
    * Drop by ANCHOR TEXT too, which the URL filter alone misses — "Unsubscribe",
      "Privacy", "Terms of Use", "View in Browser", "here". Also drop EMPTY text:
      those are image wrappers, not destinations.
    * Result: median 2 candidates per message (from a median of 4 and a mean of
      12 unfiltered), 288 of 400 messages at 1-3, 3 messages at zero. What
      survives is the real call to action — `VIEW DETAILS`, `VIEW ACCOUNT`,
      `See My Loan Option`, the `portal.pinespropertymanagement.com` link.

    Cap the set at 8 and pass the texts, not the hrefs, so a long marketing mail
    cannot flood the prompt.

    **Fetching the link is out of scope and stays out.** The Pines portal
    requires a login (Salvador, 2026-08-28: "you can't it requires login"), so
    the destination is unreachable to this system by design. The link exists so a
    HUMAN can open it. Nothing in this ticket dereferences a URL.

20. `classify report` prints: counts (`classified`, `flagged`, by `kind`), the
    flagged set newest-first with sender / subject / kind / title, and the
    skipped lane in `internal/triage/report.go`'s `reportSkipped` shape and
    vocabulary. It must print the same closing note as triage's — that an
    all-skipped pass is expected when the local box is down and that falling back
    to a hosted provider is never the fix — and a structural test asserts that
    sentence appears in **both** report files. One rule, two renderings, one test.

### The eval set

21. `docs/evals/personal-actionability.jsonl`, one JSON object per line:
    `{"message_id": N, "label": "actionable"|"not", "subject_sha256": "<16 hex>",
    "note": "<optional, no message content>"}`.
    **The file carries no message content** — not a subject, not a body, not a
    sender. A structural test asserts the file exists, every line parses, every
    label is one of the two values, and no line contains a `subject`, `body` or
    `sender` key.
22. `classify eval --labels <file>` loads the bodies from the db by id, runs the
    classifier prompt through the Router, and prints recall, precision, n, median
    latency, and **the list of false negatives with their message ids** — recall
    is the objective, so the misses are the output that matters. It **refuses to
    run** (non-zero exit, nothing sent) when `Route` does not return the local
    lane: an eval on the hosted lane would be both a leak and a meaningless
    number.
23. The harness reports **label drift**: for each id it recomputes
    `textmatch.NormalizedPrefix(subject, 120)`, hashes it, compares to
    `subject_sha256`, and prints and EXCLUDES any id that is missing or whose hash
    disagrees. Reason: the labels are the fixture, this fixture has already been
    wrong once, and a silently re-pointed id would move the score with no visible
    cause. (`internal/textmatch` is the one spelling of prefix normalisation in
    this repo; do not re-spell it.)
24. The initial set is **hand-checked, not regex-generated**, and the runbook
    records its size, its scores and the date. The spike's labels do **not** carry
    over: its first general eval scored every model 0.10-0.27 recall because it
    labelled "your statement is available" actionable while the prompt said
    informational notices were not — the models were right and the fixture was
    wrong. Minimum to merge: 100 hand-checked messages drawn from the `personal`
    population, including the Pines announcements-vs-violations pairs. Growing it
    to a few hundred is the prerequisite for any tuning, and tuning is not this
    ticket.

### Runbooks

25. New `docs/runbooks/local-classifier.md` covering: the model and why
    (0.90 vs 0.80 recall; hosted missed 3 of 5 violation notices); `"think":
    false` and how to *see* the failure it prevents; that self-reported confidence
    is a constant and must never be a threshold; that per-sender prompts are out;
    the env contract; where the model runs and what to do when it is off; what a
    skip looks like versus an unremarkable verdict; the eval workflow; and the
    second pass as the named future dial. A structural test in the
    `internal/capture/rules_structure_test.go:125`
    (`TestRunbook_DocumentsCaptureBeforeTriage`) shape asserts the file exists and
    contains the think-disabled rule and the no-confidence-threshold rule — both
    are prose-shaped, and both are exactly what a future session would "clean up".
26. `docs/runbooks/provider-locality.md` updated for what changed underneath it:
    `OPS_LOCAL_PROVIDER_URL` no longer carries `/v1`; the local adapter is
    ollama-native; `OPS_LOCAL_MODEL` is required when the URL is set; and the
    "an all-skipped triage report is the SUCCESS state … until SWT-22 lands"
    passage is corrected now that SWT-22 exists.
27. **The k8s constraint is written down before it bites.** `LocalityOf`
    (`internal/provider/locality.go:172-177`) returns `LocalityRemote` for any
    host that is not an IP literal or `localhost` — deliberately, since resolving
    a name would be both I/O and a TOCTOU. So a cluster-side consumer **cannot**
    reach ollama at `http://ollama.ops.svc:11434`: that classifies REMOTE and the
    pass skips 100% with `local_endpoint_not_private`, which reads like an outage
    and is not one. The two shapes that work are (a) the LAN address of the box
    running ollama — `http://192.168.50.x:11434`, the `pg-main-rw-lb`
    precedent — or (b) a MetalLB LoadBalancer address in `192.168.50.0/24` if
    ollama ever runs in-cluster. A ClusterIP literal also classifies local
    (10/8) but is not stable across a Service delete/recreate; do not recommend
    it. Runbook prose, no code change.

## Data model changes

**None. No migration in this ticket.** Highest applied stays
`0016_provider_locality.sql`, and `go run ./cmd/tools/migrate` must be a no-op.

- Verdicts are rows in the existing `ai_runs` (`worker_type='classify'`) +
  `ai_extractions` — the same "a worker considered this" log triage and
  plan_import use. `worker_type` is what keeps the three workers' rows from
  seeing each other: triage's `NOT EXISTS` keys on `worker_type='triage'`
  (`internal/triage/store.go:59-61`), so classify's rows are invisible to it and
  vice versa, exactly as plan_import's are.
- No new tables. A `classified_messages` or `personal_alerts` table would be
  invariant 2's violation; there is none here.
- No `capture_rules` / `capture_decisions` change. The classifier reads
  decisions, never writes them.
- No `projects` change. `ai_locality` is read, not extended.

## API / MCP tool changes

**None.** This ticket adds no executor tool and no MCP tool, and exposes no new
capability to an agent. The classifier creates nothing, so it calls nothing
through the executor.

Recorded for the ticket that takes it live (invariant 3): when a flagged message
becomes a task, it must go through the existing `create_task` executor tool with
a `classify:` actor, never a direct INSERT — the same shape triage's live slice
is specified to take. Nothing in this ticket may pre-empt that with a direct
write.

Internal Go surface added (not a tool):

```go
// internal/provider
func NewOllama(baseURL, model string, opts ...OllamaOption) *Ollama
func (o *Ollama) Complete(ctx context.Context, req Request) (Response, error)
func (o *Ollama) Describe() Descriptor          // {Name: "ollama", Endpoint: base}
func (o *Ollama) Probe(ctx context.Context) error

// internal/classify
func Run(ctx context.Context, store Store, router *provider.Router, cfg Config) (Stats, error)
func Report(ctx context.Context, pool *pgxpool.Pool, w io.Writer, since time.Duration) error
func Eval(ctx context.Context, store Store, router *provider.Router, labels []Label, w io.Writer) error
```

## MQTT topics

None. No heartbeat, command topic or LWT is touched. `classify` is a one-shot
CLI pass like `triage`, not a fleet worker.

## Files likely to touch

New:
- `internal/provider/ollama.go`, `internal/provider/ollama_test.go` (`httptest`)
- `internal/classify/classify.go` (Run, skip accounting, ratio raise)
- `internal/classify/store.go` (inbox filter, attribution + `ai_locality` read,
  `RecordRun` / `RecordExtraction`, `TryLock`)
- `internal/classify/prompt.go` (SystemPrompt, SchemaName, schema, renderUser)
- `internal/classify/report.go`
- `internal/classify/eval.go`
- `internal/classify/worker_test.go` (fakes, zero-hosted-calls, skip paths)
- `internal/classify/structure_test.go` (no-confidence-field, no per-sender
  branch, runbook assertions, labels-file assertions)
- `internal/classify/store_integration_test.go` (criterion 12's four shapes and
  criterion 17's two lanes, both with Postgres producing the values)
- `cmd/classify/main.go`
- `docs/runbooks/local-classifier.md`
- `docs/evals/personal-actionability.jsonl`

Modified:
- `cmd/triage/main.go` (local lane → `NewOllama`; `OPS_LOCAL_MODEL` required
  when the URL is set)
- `internal/provider/callsites_test.go` (`workerPackages` += `internal/classify`;
  `TestWorkerEntryPoints_TakeARouter` += classify's `Run`)
- `docs/runbooks/provider-locality.md` (criterion 26)
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` (a "Local classifier contract" section, in
  the shape of the triage and capture contract sections)

Deliberately NOT touched: `internal/triage/*` (the worker, its prompt and its
filter are unchanged — only the adapter behind its local lane changes),
`internal/drafts/*`, `internal/planimport/*`, `internal/capture/*`,
`internal/orchestrator/*`, `migrations/`.

## In scope / Out of scope

**In scope**
- The ollama adapter, its typed failures, its model-presence probe.
- `cmd/triage` pointing at it, with a bounded smoke that proves the lane.
- The `classify` worker in SHADOW over the `local_only`-attributed population.
- The labelled eval set format, the initial hand-checked set, and the scorer.
- Two runbooks and the institutional-knowledge entry.

**Out of scope — including what it is tempting to bundle**
- **Population 1, the ~14,500 `unmatched` residue.** Triage can process it once
  the lane exists; whether it should is an operator decision, and a prompt for
  newsletters and job alerts is a different piece of work.
- **Taking anything live.** `classify` creates no tasks, `CAPTURE_RULES_MODE`
  stays shadow, triage stays shadow. Three separate later decisions, still
  ordered (capture live before triage live).
- **The second-pass precision mechanism.** Named as the future dial (below);
  nothing in this ticket implements a second model.
- **A sender-context column.** The spike's finding is that Pines needs a *fact*
  ("this sender emits both announcements and fines") in a column injected into
  ONE prompt, editable through `opsctl` and visible in the report. That is right
  and it is not this ticket: it needs the labelled set first, or there is no way
  to tell whether it helped.
- **PDF / attachment extraction.** Still out of scope, but it is a MUCH smaller
  thing than this SPEC first claimed, and the claim was measured and corrected on
  2026-08-28 (see the spike doc, finding 6). The violation notices carry **no
  attachment at all** — the body says "please see attachment" and none is sent —
  and everything that matters (inspection date, the specific violation, the rule
  cited) is in `body_text`, which the normalizer extracts from HTML correctly.
  The only fact genuinely locked in an attachment is the AMOUNT on four Pembroke
  Pines utility bills, whose bodies already carry the due date and account. So
  this is a four-message enhancement, not a ceiling on the classifier.
- **Copying attachments onto tasks.** Explicitly not done, now or later. The full
  MIME already lives in `raw_source_items.rfc822_b64` (invariant 1), so copying
  the bytes onto a task duplicates what is already stored, creates a second thing
  to keep in sync, and forces a storage decision — blob column, attachments
  table, filesystem path — that nothing today requires. A task carries the
  message id; rendering or downloading an attachment from the raw item on demand
  is a dashboard concern.
- **Deploying anything to the cluster.** No CronJob, no image, no manifest — the
  kube repo is a different session's territory (institutional knowledge). This
  ticket runs on the workstation.
- **New hardware.** The RX 570 / camserv box is not installed and this ticket
  does not depend on it.
- **Model comparison or threshold tuning.** The model is chosen. Tuning needs the
  full labelled set and is explicitly a later step.

## Invariants that apply

1. **Raw-first** — untouched, and load-bearing for the honest ceiling. No
   connector changes; `classify` reads `normalized_messages` rows that were
   raw-first when ingested. The Pines PDF stays in `raw_source_items.raw_json`
   (`rfc822_b64`) unextracted, which is *why* extraction remains possible later.
   Nothing in this ticket may parse an attachment out of raw and skip normalize.
2. **One funnel** — no new task-like table. Verdicts are `ai_runs` +
   `ai_extractions` rows discriminated by `worker_type='classify'`; the queue is
   a filter (criterion 11), not a table. The shadow guarantee is structural
   (criterion 15).
3. **Everything through the executor** — nothing here is an executor tool, and no
   handler is invoked outside `Executor.Execute` because no handler is invoked at
   all. Stated for the live ticket: the first task this classifier ever causes
   must be created by `create_task` through the executor.
4. **Nothing external without a delivery row** — nothing sends. SWT-21's sharper
   reading carries over: a POST to a model is not a `deliveries` channel and never
   will be, so its gate is the Router, and this ticket's job is to make that gate
   *permit* something for the first time without weakening it. There is no code
   path in this diff from restricted content to the general client (criterion 14).
5. **Own-message loop closure** — the inbox is inbound-only. Note honestly that
   the `direction='inbound'` clause is *structurally redundant*: capture only
   decides inbound messages, so an outbound message can never carry an
   `attributed` decision and could never enter this filter. It is written anyway
   because a reader must not have to know that to trust the query — but it must
   not be described as the thing that keeps our own sends out. (This is the
   absent-because-impossible case from the 7th-instance landmine; the neighbour
   fold in criterion 14 excludes outbound for the same reason, copying
   `internal/triage/store.go:232-242`.)
6. **Stealth attribution** — nothing here writes client-visible words. The
   classifier's `title` and `reason` are internal.
7. **Orchestrator purity** — the orchestrator is not touched and does not learn
   about `classify`. The new adapter lives in `internal/provider` only; a vendor
   or HTTP import outside it is a review flag. Nothing in `internal/classify`
   constructs a client (criterion 14's scan).

Landmine classes this ticket walks past, and what it does about each:

- **A predicate whose discriminating column is a constant in production**
  (7 instances, two of them inside SWT-21). This ticket contains one by
  construction — the class of every message `classify` sees — and criterion 13
  labels it rather than dressing it as protection. The predicate that *does*
  discriminate is criterion 11's `ai_locality` filter (two values in production:
  `any` on ~20 projects, `local_only` on `personal`), and criterion 12 puts its
  regression test in the **integration** suite so Postgres produces the value,
  not a fixture.
- **The same shape in the model itself.** Self-reported confidence is a constant
  0.95; criterion 18 removes the field entirely rather than storing a number that
  looks like a dial.
- **A comment that states the opposite of its code.** SWT-21 shipped two.
  Criteria 13, 19 and criterion 5's invariant note are all comments a reviewer
  must verify against the code, not accept.
- **An alarm that cannot tell "did not run" from "found nothing."** Criterion 17
  is that separation, structurally, and criterion 20 makes it visible.

## Sibling patterns to copy

- **Worker shape end to end:** `internal/triage/triage.go` `Run` — routing, the
  skip branches, `flushSkips`, the ratio raise, `classReasonOf`. `classify.Run`
  should read as its smaller sibling; do not invent a second idiom for skips.
- **Store shape:** `internal/triage/store.go:52-97` (the pending filter and its
  three-state comment — read it before writing criterion 11) and
  `store.go:153-208` (attribution + `ai_locality` in ONE query, so class and
  project cannot disagree). `internal/drafts/store.go` folds neighbours in the
  *same* query as the bodies, which SWT-21's Deviation 9 names as the better
  pattern — copy drafts here, not triage.
- **Adapter tests:** `internal/provider/openai_test.go` and
  `openai_unavailable_test.go` (`httptest`, closed listener, sleeping handler,
  1 ms deadline) — criteria 2-8 extend that file's shape exactly.
- **Structural scans:** `internal/provider/callsites_test.go` (with its positive
  control — copy that too; a scan for a token that cannot appear is worse than no
  scan) and `internal/capture/rules_structure_test.go` (`TestRulesGo_IsPure`,
  `TestRunbook_DocumentsCaptureBeforeTriage`).
- **Shadow enforcement:** `internal/triage/worker_test.go`
  `TestShadow_StoreHasNoTaskWriteMethod` (reflection over the Store interface).
- **Integration-suite hygiene:** `make integration` runs `-p 1` because the
  triage and connector suites cross-pollute on one compose db. A new suite with
  global-count assertions joins that mutual-cleanup pact and cleans up its own
  rows first, in FK order, scoped by a test-owned slug.
- **Prefix normalisation:** `internal/textmatch.NormalizedPrefix` is the one
  spelling (criterion 23). Do not re-spell it in SQL or in the eval harness.

## Verification protocol

Before commit. Every command is meant to be run, not reasoned about.

```bash
eval "$(grep '^export OPS_DATABASE_URL=' ~/.bashrc)"
export OPS_LOCAL_PROVIDER_URL=http://127.0.0.1:11434
export OPS_LOCAL_MODEL=qwen3:8b
```

1. `go test ./...` — adapter wire assertions, the zero-hosted-calls router test,
   the skip paths, the reflection shadow test, and the four structural scans. No
   db, no broker, no network, no model.
2. `make db-up && make migrate && make integration` — criterion 12's four
   population shapes and criterion 17's two lanes, both with Postgres producing
   the values. **Mutate to confirm they bite:** drop `ai_locality` from
   criterion 12's SELECT and watch it go red; if it stays green you tested your
   fixture.
3. **Migration state is unchanged.**
   `psql "$OPS_DATABASE_URL" -tAc "SELECT max(version) FROM schema_migrations"`
   reads `0016` before and after; `ls migrations/` shows no 0017.
4. **The server is up and the model is present** (criterion 6's real precondition):
   ```bash
   pgrep -af 'ollama serve'
   curl -s http://127.0.0.1:11434/api/tags | jq -r '.models[].name'
   ```
   `qwen3:8b` must appear. If ollama is not running, start it with
   `OLLAMA_VULKAN=1` — ROCm crashes during GPU discovery on gfx1032.
5. **Observe the thinking failure once, then observe it fixed** (criterion 3).
   This is the finding most likely to be "cleaned up" by a later session, so the
   evidence goes in the runbook:
   ```bash
   # think ON — expect empty content and done_reason "length"
   curl -s http://127.0.0.1:11434/api/chat -d '{"model":"qwen3:8b","stream":false,
     "think":true,"options":{"num_predict":256},
     "messages":[{"role":"user","content":"Subject: Your statement is ready. Actionable? Answer JSON."}]}' \
     | jq '{content: .message.content, done_reason}'

   # think OFF — expect non-empty content and done_reason "stop"
   curl -s http://127.0.0.1:11434/api/chat -d '{"model":"qwen3:8b","stream":false,
     "think":false,"options":{"num_predict":256},
     "messages":[{"role":"user","content":"Subject: Your statement is ready. Actionable? Answer JSON."}]}' \
     | jq '{content: .message.content, done_reason}'
   ```
6. **The headline smoke — the restricted lane serves for the first time.**
   ```bash
   go run ./cmd/classify run --limit 5
   psql "$OPS_DATABASE_URL" -tAF' | ' -c \
     "SELECT status, provider, model, count(*) FROM ai_runs
       WHERE worker_type='classify' GROUP BY 1,2,3;"
   psql "$OPS_DATABASE_URL" -tAF' | ' -c \
     "SELECT count(*) FROM ai_extractions e JOIN ai_runs r ON r.id=e.ai_run_id
       WHERE r.worker_type='classify';"
   ```
   Expect `ok | ollama | qwen3:8b | 5` and 5 extractions. Then confirm nothing
   was created: `SELECT count(*) FROM tasks;`, `FROM external_refs;`,
   `FROM deliveries;` unchanged before/after.
7. **The negative smoke — a lie about locality is still refused.**
   `OPS_LOCAL_PROVIDER_URL=https://api.openai.com go run ./cmd/classify run --limit 5`
   with `OPENAI_API_KEY` exported. Expect: startup refusal,
   `avail_reason='local_endpoint_not_private'`, one `status='skipped'` row, zero
   extractions, zero hosted calls. This is SWT-21's thesis surviving a ticket
   that adds a working local lane.
8. **The unreachable smoke — and the difference criterion 17 draws.**
   Stop ollama (or point at a dead private port, `http://127.0.0.1:1`), run
   `go run ./cmd/classify run --limit 5`, and confirm: exit code **0**,
   `status='skipped'` with `avail_reason='local_unreachable'`, no extraction, and
   **the same five message ids still pending**. Restart ollama, run again,
   confirm they are processed and now leave the inbox. Two states, two records,
   visibly different:
   ```bash
   psql "$OPS_DATABASE_URL" -tAF' | ' -c \
     "SELECT r.status, r.input->>'avail_reason', count(e.id)
        FROM ai_runs r LEFT JOIN ai_extractions e ON e.ai_run_id=r.id
       WHERE r.worker_type='classify' GROUP BY 1,2;"
   ```
9. **Triage is unblocked** (the ticket's title, bounded per the scope decision):
   `go run ./cmd/triage run --limit 5` → `processed: 5`, `skipped: 0`, and
   `ai_runs.provider='ollama'`. Read one extraction and expect *poor* verdicts —
   this is a client-work prompt over newsletter mail; the check is that the LANE
   works, not that the verdict is good. Say so in the commit summary rather than
   implying triage is now useful on the residue.
10. **`classify report` reads as a person would need it to.**
    `go run ./cmd/classify report --since 24h` — flagged items first, counts, and
    the skipped lane with the no-fallback note. A reader must be able to tell
    "the box was off" from "nothing was actionable" without opening psql.
11. **The eval scores, and the misses are named.**
    `go run ./cmd/classify eval --labels docs/evals/personal-actionability.jsonl`
    — prints n, recall, precision, median latency, drift exclusions, and every
    false negative by message id. Record the numbers and the date in
    `docs/runbooks/local-classifier.md`. Then confirm the refusal:
    `OPS_LOCAL_PROVIDER_URL= go run ./cmd/classify eval --labels …` must exit
    non-zero having sent nothing.
12. **Re-measure the populations rather than trusting this SPEC's literals**
    (they were true on 2026-08-28 and the corpus is live):
    ```bash
    psql "$OPS_DATABASE_URL" -tAF' | ' -c "
    WITH latest AS (
      SELECT DISTINCT ON (message_id) message_id, action, project_id
        FROM capture_decisions ORDER BY message_id, id DESC)
    SELECT l.action, COALESCE(p.slug,'(none)'), COALESCE(p.ai_locality,'-'), count(*)
      FROM latest l LEFT JOIN projects p ON p.id = l.project_id
     GROUP BY 1,2,3 ORDER BY 4 DESC;"
    ```
    `DISTINCT ON … ORDER BY id DESC` is not decoration: shadow mode writes a
    decision per pass, so a plain count triple-counts (the runbook records a
    49,493-vs-16,066 instance of exactly that).

## Decisions made unilaterally (argue if wrong)

- **Native `/api/chat`, not the OpenAI-compatible `/v1` route.** The spike's own
  failure signature (`done_reason: length`) is a native-API field, so the
  measurement that chose this model was taken there; `think` is a native request
  field; and `format` takes a JSON schema directly. Cost: a second adapter
  instead of reusing `provider.OpenAI` with a different base URL. Verify the
  `think` handling at implementation time (criterion 5's smoke) — if the `/v1`
  route turns out to honour it, that is a fact worth recording, not a reason to
  switch.
- **No `OPS_LOCAL_PROVIDER_KIND` env.** One local stack is measured; a kind
  selector would add a branch nothing in production exercises. `OPS_LOCAL_PROVIDER_URL`
  set ⇒ ollama. Add the selector when a second local stack actually exists.
- **`OPS_LOCAL_MODEL` becomes required with the URL** rather than falling back to
  `TRIAGE_MODEL`. The fallback produces a per-message 404 that reads as a broken
  adapter; an absent lane reads as an absent lane.
- **The classifier is a new worker, not a mode of triage.** Different prompt,
  different schema, different confidence semantics, different report. Bolting a
  second prompt into `triage.Run` would make every future reader ask which mode a
  line is in, and triage's three-state filter is precisely tuned to a different
  inbox.
- **No confidence field in the output at all** (criterion 18), rather than
  storing 0.95 and documenting that it means nothing. CLAUDE.md's "per-field
  confidence → human-review lane" is a *triage* contract; here it would be a
  human-review lane keyed on a constant.
- **Shadow, not live.** Every AI worker in this repo shipped shadow first
  (triage, capture, plan import proposals). A classifier whose recall has been
  measured on 106 auto-labelled messages does not get to create tasks.
- **The eval set is ids + labels + a subject hash, in the repo.** No message
  content is committed. The hash exists so a re-pointed id is visible; it is not
  a security measure.
- **Deployment: the workstation, this ticket.** No worker is deployed today,
  triage has only ever run by hand, and the only usable GPU is in the
  workstation. The cluster path is documented (criterion 27), not built. Pending
  Q2.

## What changes under the other answer to Q1

If the ticket is to classify population 1 (the ~14,500 `unmatched`) instead:

- Criterion 11's filter becomes triage's — which means the work is not a new
  worker at all, it is a *prompt* for triage and a decision to run it over the
  residue.
- Criteria 12, 13 and 17 lose their integration surface (there is no second
  population to exclude) and the "usable alone" claim reduces to "triage runs
  again", with shadow verdicts on brand marketing.
- The spike's numbers stop applying (premise 3) and criterion 24's labelled set
  must be drawn from the residue instead.
- Runtime per full pass goes from ~30 minutes to ~5 GPU-hours on a shared card.

Answering "both" is not a third option here — it is this ticket plus a second
one, and the second one is cheap once the adapter exists.

**This is what was chosen** (2026-08-28). The residue work is tracked as SWT-23
rather than folded in here: its expensive half — the adapter, the boundary proof,
the local lane — is delivered by this ticket, and what remains genuinely belongs
in its own ticket because it is a prompt-and-labels problem, not a wiring one.

## Future work (not this ticket)

- **Second-pass precision.** qwen3:8b flags broadly (0.61 precision on the
  general eval). To trade precision back without touching recall, re-check only
  the *flagged* subset with a different small model and keep the disagreements
  for review. Two disagreeing small models beat one model's self-assessment —
  and this is the mechanism that replaces the confidence threshold criterion 18
  refuses to fake. It needs the full labelled set first.
- **A sender-context column**, editable through `opsctl` and rendered into the
  one prompt: "this sender emits both routine announcements and fine notices".
  A fact in a column, never a per-sender prompt.
- **Attachment extraction** from `rfc822_b64` — the fine amount and cure-by date
  live in a PDF. The hard ceiling on Pines regardless of model.
- **Taking `classify` live**: flagged → `create_task` through the executor, into a
  `personal` project that currently has no tasks by design. That ticket also owns
  `task_get_next`'s locality predicate, which SWT-21 deliberately dropped because
  `personal.client` is NULL and the existing `p.client = $1` already excludes it.
- **A dashboard lane** for classifier verdicts and skipped counts.
- **Running `classify` on a schedule**, in the cluster or on the box, once the
  deployment question (Q2) has an answer that survives the workstation being off.
