> Jira: SWT-22

# local-classifier — open questions

Two. Both change the SPEC's shape, so it is marked PROVISIONAL until they are
answered.

---

## Q1. Which pile of mail does this thing actually read?

Seeding the 18 personal capture rules split the problem in two, and the ticket
was written before that happened.

**Pile A — the 14,500 "unmatched" messages.** This is triage's inbox. It is what
is left after the capture rules claim everything they recognise: newsletters, job
alerts, GitHub and LinkedIn notifications, brand marketing. It is restricted only
because nothing has positively placed it, not because it is sensitive.

**Pile B — the 1,609 messages attributed to `personal`.** The bank, the mortgage
servicer, the HOA, the health billers. **Nothing reads these today.** Attributing
them took them out of triage's inbox and gave them no other consumer, so they sit
in the database and nobody looks at them. This is also the mail the spike actually
measured against — the Pines violation notices are in this pile now, not in
pile A.

Pile A is nine times bigger and mostly noise. Pile B is where a miss is a late
fee.

**Recommendation: Pile B.** Build a small classifier over the personal mail, in
shadow, and give triage the working local lane as a by-product so it *can* run
against pile A whenever that seems worth the GPU hours. Building for pile A means
running a client-work prompt over brand marketing for about five hours a pass and
quoting recall numbers taken on a corpus that no longer exists.

**Answer:**

---

## Q2. Where does the model live, and who runs the pass?

Right now ollama is on the workstation, on the desktop's own graphics card. The
model is 5.2 GB and the desktop holds about 3 GB of the card's 8, so it does not
quite fit and spills a layer or two to the CPU. The RX 570 in a separate box was
the plan; it is not installed.

**Option A — keep it on the workstation, run the pass by hand.** Nothing new to
install. The model competes with the desktop for the card. Nothing runs when the
workstation is off. None of the workers are deployed today anyway — triage has
only ever been run by hand from here — so this changes nothing about how the
system is operated.

**Option B — open ollama to the LAN and run the pass in the cluster**, as a
scheduled job like the connectors. Needs ollama listening on `0.0.0.0` and the
firewall opened to the cluster, and the workstation must be awake when the job
fires, or every run is a skip. The upside is that it runs on a schedule without
anyone thinking about it.

One thing worth knowing either way, because it will otherwise be discovered at
runtime: the locality check only accepts a numeric address. `192.168.50.x` is
fine. A cluster service *name* like `ollama.ops.svc` is refused as "not local"
and the pass would skip 100% of the time and look like an outage. So if ollama
ever runs inside the cluster it needs a `192.168.50.x` load-balancer address, the
same as Postgres has.

**Option C — the RX 570 in camserv (192.168.50.3), as originally planned.**
Raised by Salvador 2026-08-28. Headless, so the whole 8 GB is available instead
of the ~5.5 GB left over here — the model would fit 100% in VRAM rather than
spilling 15% to the CPU.

The catch he identified: **camserv's slot is not full bandwidth, so loading is
slow.** That inverts two of the numbers measured on this workstation, which is a
x16 Gen4 machine whose 1.77 GB/s load rate is disk-bound, not bus-bound:

| slot | time to move 5.9 GB |
|---|---|
| x1 Gen2 | ~12s |
| x1 Gen3 | ~6s |
| x4 Gen3 | ~1.5s |
| x16 (this workstation) | ~0.4s — so disk dominates |

A cold load there is plausibly 15-20s rather than 3.4s. **Two consequences, and
the second one matters more than the first:**

1. A per-pass model swap stops being cheap. The "+5% overhead" two-pass figure in
   the SPEC is an x16 figure; on a narrow slot every alternation pays the slow
   transfer again.
2. **But no swap is needed there.** Headless 8 GB means qwen3:8b (5.2 GB) and a
   small second model — qwen2.5:3b is 1.9 GB, qwen3:4b is 2.5 GB — total
   7.1-7.7 GB and fit SIMULTANEOUSLY. With `OLLAMA_KEEP_ALIVE=-1` and
   `OLLAMA_MAX_LOADED_MODELS=2` both load once at boot and never transfer again.
   The slow slot is then paid once in the machine's lifetime instead of once per
   pass, and the two-model design Salvador wanted works better there than here.

Do NOT pair qwen3:8b with gemma3:4b on that box: 5.2 + 4.3 = 9.5 GB does not fit,
and the failure mode is a silent CPU spill that streams over the narrow bus per
token — the one penalty that is genuinely severe on a x1 link.

Unverified, and worth settling before buying into the card: the RX 570 is Polaris
(gfx803), where ROCm support was dropped entirely, so it is Vulkan-only. The
spike's "Vulkan works, ROCm crashes" finding was measured on RDNA2, NOT on
Polaris. Ollama's Vulkan backend on that generation has not been tested here.

The slot width is one command away and has not been run — SSH to camserv failed
host-key verification from this session and the key was not auto-accepted:

```
ssh camserv.home.arpa 'for d in /sys/bus/pci/devices/*/; do [ -f "$d/max_link_width" ] && [ "$(cat $d/class)" = 0x060400 ] && echo "$(basename $d) max x$(cat $d/max_link_width) $(cat $d/max_link_speed)"; done'
```

**Recommendation: Option A for this ticket, Option C as the destination.**
A is one environment variable away from either of the others, and B without the
RX 570 still means "runs only when the workstation happens to be on", which is
not really a schedule. C is the right home but adds an uninstalled card, an
untested Vulkan path on an older generation, and an unmeasured slot — none of
which should block the adapter and the eval set being built.

**Answer:**

---

Answer by editing the entries. Say "questions answered" and I'll fold them into
the SPEC.
