# sources-slow (swb 709)

Reported by the kube session after the 0.7.57 roll (2026-09-25): "/sources took 22 s to render (a 15 s curl timed
out once)".

## Reproduction (prod, read-only, 2026-09-25)

Each subquery of `listSources` (internal/dashboard/sources.go), timed separately:

| counter | time |
|---|---|
| raw items / normalized / pending | ~0.1 s |
| messages by direction | ~0.4 s |
| **raw items with `raw_json->>'truncated' = 'true'`** | **9.8 s** |
| **raw items with MIME `parts`** | **10.4 s** |
| channels | ~0.1 s |

`raw_source_items` holds 98,415 rows (2.2 GB). Both slow counters evaluate a predicate on `raw_json`, which
de-TOASTs every row on every page load.

## Fix

Migration `0046_raw_items_flag_indexes.sql` adds two partial indexes on `raw_source_items (source_account_id)`, each
with the counter's exact predicate. The planner then counts the matching rows from the index (verified with
EXPLAIN on a scratch DB). `TestSources_CountersMatchTheirIndexes` pins that the predicates keep matching.
