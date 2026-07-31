> Jira: SWT-11

# imap-mail-connector — open questions

**BOTH ANSWERED 2026-07-26: option A for each.** Folded into the SPEC; kept here for
the reasoning and the rejected alternatives.

---

## Q1. `human_only` vs the MCP actor prefix — the brief's assumption is wrong

**The fact.** `mcpserver.CallTool` sets `Actor: "mcp:" + s.workerID`
(`internal/mcpserver/adapter.go:73`). `.mcp.json` sets `OPS_WORKER_ID=manual:salvo`,
so an interactive-session call arrives at the executor as **`mcp:manual:salvo`**.
`policy.humanActor()` (`internal/policy/matrix.go:36`) checks
`strings.HasPrefix(actor, "dashboard:"|"opsctl:"|"manual:")` — `mcp:manual:salvo`
matches none of them. So MCP-listing `approve_delivery`/`send_delivery` today yields
`deny / human_only` for Salvador too, not just for workers. The brief's "denied by the
existing `human_only` rule with no policy change" holds for `worker:*` but breaks the
human path.

**A — `humanActor` strips one optional `mcp:` transport prefix, then checks the human
set.** One function, one test. The audit row keeps the full `mcp:manual:salvo`, so
"came through MCP" stays distinguishable from `dashboard:salvo` forever. Cost: the
policy core now knows a transport prefix exists, and any future transport wrapper has
to be added there too.

**B — `mcpserver` does not prefix when `OPS_WORKER_ID` already carries a human prefix**
(`manual:`/`dashboard:`/`opsctl:` pass through as-is; everything else gets `mcp:`).
Policy stays untouched and transport-agnostic. Cost: the audit trail can no longer
distinguish an MCP `manual:salvo` from an `opsctl` `manual:salvo`, and the adapter now
contains a list that duplicates policy's human-prefix set.

(A third option — a distinct `OPS_WORKER_ID` value for interactive sessions, e.g.
`dashboard:salvo` — was rejected as a lie: it is neither the dashboard nor a worker.)

**Answer: A.** `humanActor()` strips one optional leading `mcp:` before checking the
human set. The audit row keeps the full `mcp:manual:salvo`, so "which surface
triggered this send" survives — that is what B would have destroyed permanently, and
it is exactly the question asked when a send looks wrong. Accepted cost: the policy
core knows a transport prefix exists, and future transport wrappers must be added
there.

---

## Q2. Which mailboxes may `mail_search` / `mail_read_thread` read?

`sspataro@gmail.com` is personal (106,930 messages) and will be ingested in the same
90-day window as the client mailboxes. `mail_search` is agent-facing, so whatever is
searchable is readable by any Claude Code worker session, not just the interactive one.

**A — every ingested `provider='google'` mailbox, no flag.** Consistent with the
SPEC's own line that "what agents can see is an ingestion decision, not an MCP one":
if it is in `normalized_messages` it is already visible to triage, drafts, and
`task_context`, so a search tool adds no new trust boundary. Zero schema, zero config.

**B — an opt-in per-account boolean** (`source_accounts.agent_searchable`, defaulting
false, set true per mailbox — same shape and migration as `calendar_in_availability`),
with both tools filtering on it. Personal mail stays ingested (so triage and dedup
still work) but is not reachable by an agent tool. Cost: one more column in 0010, one
more join in both handlers, one more thing to remember when adding a mailbox.

If B: does the *interactive* session (`manual:`) get the unfiltered view, or does the
flag bind everyone equally? Unfiltered-for-humans means the filter is an actor check
rather than a plain WHERE clause.

**Answer: A.** Every ingested `provider='google'` mailbox is searchable; no
`agent_searchable` column, nothing added to migration 0010. Grounds: the mail already
sits in `normalized_messages` where triage, drafts, and `task_context` read it, so a
search tool is a new door to a room agents are already in.

Consequence, to be stated plainly in the tool descriptions rather than discovered:
the personal mailbox (~106k messages inside the 90-day window) is readable by ANY
Claude Code worker session, not only the interactive one. Retrofit if that changes is
option B — one column plus a filter in both handlers.

---

Answer by editing the entries. Say "questions answered" and I'll fold them into the
SPEC.
