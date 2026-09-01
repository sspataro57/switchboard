# Enabling the Upwork assisted tier (SWT-20)

The upwork_chat channel is drafting-open but sending stays assisted:
`send_delivery` is policy-denied (`channel_assisted`), a human copies the text
into Upwork and marks it sent, and the connector confirms the send when the
outbound message re-enters through ingestion.

## Order of operations — each step gates the next

1. **Migrate first.** `0019_delivery_provenance.sql` must be applied to
   pg-main before any image carrying this code runs there: the matcher's
   shortlist selects `deliveries.target_client_ref`, which does not exist
   until then. Precondition (has always held):
   `SELECT count(*) FROM deliveries WHERE channel='upwork_chat'` → 0 — if it
   is ever non-zero, STOP and re-spec the backfill; do not soften the CHECK
   to NOT VALID.
2. **Image + tag bump** in the kube repo for `connector-upworkcrm` (and the
   drafts worker whenever it deploys). Migrating is not applying; check
   `SELECT max(version) FROM schema_migrations` against `ls migrations/`.
3. **Provenance must exist before a task can be drafted.** A task's upwork
   target comes only from `tasks.source_thread_id`. Capture (in live mode)
   records it for the tasks it creates; every pre-existing task records
   nothing, forever (no backfill is possible), and is refused with an error
   naming `task_set_source_thread`. For a task raised by hand:
   `opsctl call --tool task_set_source_thread --args '{"task_id":N,"thread_id":M}'`
   where M is the `normalized_threads.id` of the conversation that raised it.
   Setting it twice with different values is refused for everyone (D7) — a
   genuine correction is a psql UPDATE plus a note.
4. **Watch.** The reconciler flags a sent-but-unconfirmed delivery after its
   pass threshold; a flagged row stays in its own client's candidate set and
   can still confirm later.

## What each refusal means

- `record no source conversation … task_set_source_thread` — the task predates
  provenance or capture has not recorded it. Step 3.
- `names a thread of client X, but task N's recorded conversation belongs to
  client Y` — the client binding (unconditional, every actor). The target is
  wrong; nobody may draft into a different conversation partner.
- `a different room of the bound client … explicit human decision` — an
  automated caller tried to pick a room. Humans (`dashboard:`/`opsctl:`/
  `manual:`, incl. over MCP) may; the drafts worker always produces the
  recorded key and never needs to.
- `names no ingested thread` — the target parses but was never ingested; an
  unrecognized target would be confirmable by any message from that client.

## Why the shortlist is sound (for the reader about to "optimise" it)

`SameConversation` excludes on a client mismatch before any room logic, so
`target_client_ref = $1` is the only SQL predicate implied by the rule — the
room clause IS the rule and stays in Go. `deliveries_upwork_identity_check`
guarantees every upwork row carries the client value the same parser produced
from the same target_ref, so a wrong column can only cause a MISS (surfaced by
the reconciler), never a wrong stamp. There is deliberately no room column.
