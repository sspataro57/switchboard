# mcp-task-verbs — open questions

## Q1 — Hand-delivered or closed work: should its Deliver task stop getting drafts?

**What happens today.** When a task reaches `done_locally`, the orchestrator creates a
`Deliver #N` child task (R3). The draft worker picks that child up and drafts a delivery.
Normally the send closes the child (R8). But when Salvador says "swb delivered 412" or
"swb close 412", only the parent moves. The child stays open, and the draft worker still
drafts a delivery for work that is already delivered or closed. The draft goes to the
approval queue; nothing is sent.

The cause is in `drafts.DeliverTasks` (`internal/drafts/store.go:59-73`). It checks the
Deliver task's own status and "no delivery row for the parent yet". It never checks the
parent's status. The gap already exists through `opsctl call`; this ticket makes it one
sentence away in every session.

**Pick one:**

- **(a) Descriptions only.** The `task_mark_delivered` and `task_close` descriptions, and
  the Instructions, tell the session to also close the `Deliver #N` child with
  `task_close`. No code outside this ticket's MCP surface changes. The fix depends on the
  model following the text.
- **(b) Also add `AND parent.status = 'done_locally'` to `DeliverTasks`.** No path (MCP,
  opsctl, a future dashboard verb) could then get a draft for work that has already been
  delivered or closed. The Deliver task itself still sits on the board until someone
  closes it. The cost is one predicate in `internal/drafts`, plus an integration test that
  fails when the clause is removed from the SELECT.

(a) keeps this ticket to the MCP surface. (b) closes the gap for every path, at the price
of touching the draft worker, which this ticket otherwise leaves alone.

Answer: (b), decided 2026-09-10. Salvador asked the main thread to decide ("don't understand the question"); (b) is strictly safer: no path can draft for delivered or closed work, at the cost of one READ predicate in drafts.DeliverTasks plus its integration test.

---

Answer by editing the entries. Say 'questions answered' and I'll fold them into the SPEC.
