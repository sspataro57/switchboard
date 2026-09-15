> Jira: SWT-58

# sana-email-not-captured

## Report (verbatim, owner, 2026-09-15)

> I see an email from sana not captured. Emails or slacks should be high pritority.

(The second sentence, the board ordering, is ticket `board-incoming-first`. This bug is the capture miss.)

## Evidence gathered by the coordinator (prod, read-only, 2026-09-15)

- **The message:** `normalized_messages` 291568, gmail, inbound, sent 2026-09-15 15:33:15Z from
  a client contact at a university customer (redacted), subject: a question about a field in the
  classes payload, thread 159886.
- **The thread** has three messages:
  - her inbound 158690 on 09-10;
  - Salvador's outbound reply 201924 on 09-12 (salvador@handsonconnect.org);
  - her new inbound 291568 on 09-15.
- **Capture:** one live `capture_decisions` row, 390838 at 15:40:27Z: action `unmatched`, "no enabled
  rule matched", `route_step` NULL, no project.
- **Routing:** `ai_extractions` 11630 at 15:41:08Z (ai_run 11734, worker_type `classify_route`, ollama
  qwen3:8b, status ok) routed it to project `collaboratory` (project_id 4, grounded true, candidates
  2, evidence = the subject).
- **After that, nothing:**
  - no further `ai_extractions` for its raw item (no inquiry classification);
  - no `capture_decisions` row with a `route_step`;
  - no `classify_promotions` row;
  - no task (none by `surfaced_by_message_id` = 291568 or `source_thread_id` = 159886).
- **pipelined** runs the stages gate, route, route_apply, inquiry and inquiry_promote. Since its
  17:48Z restart:
  - the inquiry pass reports `processed=0`;
  - inquiry_promote reports gated `{answered:8 pending:3}`;
  - logs before 17:48Z were lost with the old pod.
- The inquiry-promote "answered" gate counts only a LATER outbound in the thread ("answered in thread
  = a later outbound on a thread-exact key"), so her new message is after the 09-12 reply and should
  not be gated by it.
- Sana's earlier mails were attributed to collaboratory by rule 60 (body_regex) or unmatched, in
  shadow mode, before capture went live.

## Expected

An unmatched inbound client email that routing assigns to collaboratory becomes a Holding task on the
board through the inquiry lane (route → route_apply → inquiry → inquiry_promote).

## Observed

It stops after routing: route_apply records nothing, the inquiry classifier never sees it, and no
task exists.
