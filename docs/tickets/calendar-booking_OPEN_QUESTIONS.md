> Jira: SWT-28

# calendar-booking — open questions

**ANSWERED by Salvador, 2026-09-07. Both folded into
`docs/tickets/calendar-booking_SPEC.md`, which is now FINAL.** This file stays as
the decision record: it holds the alternatives and the reasoning, which the SPEC
deliberately does not repeat.

---

## Q1 — Does `calendar` ship at the auto tier now, or at approve with auto as the promotion?

CLAUDE.md's matrix says **auto** for calendar own blocks. Every other channel in
this repo shipped at **approve** first — including `jira_comment`, whose matrix
row also says auto for progress comments, and which IK records as "all comments
start at approve — the auto tier is the earned-promotion path".

The two are not the same shape of code, so this is a design fork, not a dial:

**(a) Approve tier.** `send_delivery` stays `humanOnly`. An agent calls
`draft_delivery`; Salvador approves and sends from `opsctl` or the dashboard.
Zero new tools, zero change to `policy.humanOnly`, and the first live bookings
are all watched. Cost: an agent cannot actually book anything, so "switchboard
books your focus time" is not true until a follow-up ticket.

**(b) Auto tier now.** Needs a send verb an agent may call, because
`send_delivery` is human-only and widening that gate is off the table — it is
one shared predicate for gmail, jira and slack, and the IK entry "an actor-prefix
check is a transport label, not a trust boundary" is about exactly this kind of
edit. So (b) means a new tool, e.g. `book_calendar_block(delivery_id)`, which
approves-and-sends a drafted `calendar` row in one audited executor call, not
human-only, kill-switch- and rate-limit-gated, MCP-listed. The conflict /
freshness / horizon refusal (criterion 19) is unchanged and is what keeps it
safe. Cost: an agent with a prompt injection in its context can put real events
on a real calendar, ten per hour, until someone looks.

Answer affects: criteria 14–17, criterion 24 (whether `cmd/ops-mcp` wires the
booker), the MCP allowlist in `internal/mcpserver/adapter_test.go`, and the
"Out of scope" line that deferred `book_slot`.

**Answer: (b) — auto tier now.** Ship `book_calendar_block(delivery_id)`:
approves and sends a drafted calendar row in one audited executor call, NOT
human-only, kill-switch- and rate-limit-gated, MCP-listed. `policy.humanOnly` is
not widened. The prompt-injection exposure above was read and accepted: the
blast radius is a block on Salvador's own calendar, at a time provably free, on
an account a human explicitly write-enabled, at most ten per hour, every call
audited, with `set_sending_frozen` as the stop button. The conflict / freshness /
horizon refusal stays the safety backstop.

Folded in as: criteria 14–17 (policy: the calendar branch, the
`channel_mismatch` deny on every other channel, the not-human-only pin across
eight actor shapes, sendShaped + freezeGated), criterion 18 (the verb and its
approve-then-send handler), criterion 24 (`cmd/ops-mcp` now wires the booker),
criterion 25 (MCP schema + allowlist), and the smoke's step 4 (kill-switch drill
by hand). "Out of scope" now defers only an automatic BOOKER — nothing decides
to book on its own in this ticket.

---

## Q2 — Should a calendar `delivery_sent` drive orchestrator R8?

R8 (`internal/orchestrator/rules.go:114-115`, `:271-291`) fires on ANY
`delivery_sent` regardless of channel: it marks the delivery's task
`done_locally → delivered` and closes the R3 Deliver task. It has no channel
test today, and the event payload already carries `channel`.

If a calendar booking is the task's deliverable ("book the intro call slot"),
R8 firing is right. If the block is incidental to a task that is still being
worked ("reserve two hours to finish this"), R8 is wrong — and the failure is
not merely noisy: `task_mark_delivered` refuses a task that is not
`done_locally`, the engine logs that failure and continues
(`engine.go:110-141`), but the `record_orchestration` that follows is only
suppressed after a failed `create_task`. So the `delivery_lifecycle` dedup key
gets written anyway, and a LATER real delivery on that task would be deduped and
never mark it delivered.

**(a) Leave R8 channel-blind.** No orchestrator change. Accept that booking a
block against an active task burns that task's `delivery_lifecycle` dedup key.

**(b) R8 ignores `channel == "calendar"`.** A three-line test inside the pure
`ruleDeliveryLifecycle` over the payload it already has — invariant 7 intact, no
I/O. A booking then never advances a task's lifecycle, and a task whose
deliverable IS the booking has to be closed by its worker with
`mark_done_local` / `task_close` like any other.

Answer affects: whether `internal/orchestrator/rules.go` and
`rules_r8_test.go` are touched at all, and one line in "Invariants that apply" (7).

**Answer: (b) — R8 ignores `channel == "calendar"`.** The pure-rule change plus
its test. Bookings never advance a task's lifecycle; a task whose deliverable is
the booking is closed by its worker via `mark_done_local` / `task_close`. This
matters more under Q1's answer than it would have under (a): an unattended
booker firing R8 could silently mark work delivered.

Folded in as: criterion 29 (zero actions AND no `record_orchestration` for a
calendar `delivery_sent`; a gmail control; an absent-`channel` control so the
missing key does not become a silent skip), invariant 7's paragraph, the
`internal/orchestrator/*` entries under "Files likely to touch", the sibling
pattern pointing at the existing `rules_r8_test.go` header, and the integration
assertion that a drain leaves the task's status unchanged and writes no
`delivery_lifecycle` row.

---

Answer by editing the entries. Say "questions answered" and I'll fold them into
the SPEC. *(Done — 2026-09-07. Nothing further is pending here.)*
