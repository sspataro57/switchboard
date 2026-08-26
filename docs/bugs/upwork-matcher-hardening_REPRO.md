> Jira: SWT-18

# Reproduction — upwork-matcher-hardening

## Status

**Defect 1 (raw text comparison): Confirmed.**
**Defect 2 (no attempt-time floor over a client-wide scope): Confirmed.**

Both reproduce independently, in separate tests with separate fixtures (separate
client ids, projects, tasks, rooms and messages), so neither masks the other.
Each fails the way the report describes.

## Trigger

Both go through the real code path: seed `deliveries` + a raw outbound
`upwork_crm` communication, then call `upworkcrm.Normalize`, which runs the
post-hoc matcher. No Upwork and no `upwork_crm` source database is involved —
`Normalize` reads `raw_source_items` only.

### Defect 1 — whitespace-only difference across the round trip

1. One `upwork_chat` delivery, `status='sent'`, `sent_external_id IS NULL`,
   `target_ref='upwork_crm:{client}:chat'`, body =
   `"Thanks for the update. I pushed the staging fix and re-ran the migration, so the queue is draining now.  \n\n\nWill confirm once the backlog clears, around 18:00."`
   — an NBSP (U+00A0) at character 81, a trailing double space, a three-newline
   blank-line run.
2. One raw outbound communication for the same client, same room, whose body is
   the same text as the provider handed it back: plain space instead of the
   NBSP, no trailing spaces, one blank line.
3. `upworkcrm.Normalize`.

The fixture's asymmetry is asserted before the run, and holds:

```
left(body,120) says DIFFERENT
textmatch.NormalizedPrefix says SAME
  ("Thanks for the update. I pushed the staging fix and re-ran the migration, so the queue is draining now. Will confirm onc")
```

The NBSP does not change the rune count, so the two 120-rune windows stay
aligned; the only differences between the bodies are whitespace.

### Defect 2 — newer delivery in another room wins

1. `D_old`: `upwork_chat`, `target_ref='upwork_crm:{client}:chat'`, `status='sent'`,
   `sent_at = send_attempted_at = 2026-07-11T10:00:00Z` — the delivery that
   actually produced the message.
2. `D_new`: same client, **different room**
   (`target_ref='upwork_crm:{client}:room-b'`), identical body,
   `sent_at = send_attempted_at = 2026-07-11T13:00:00Z` — three hours *after* the
   message already existed, so it cannot have produced it.
3. One raw outbound communication in room `chat`, `communicated_at =
   2026-07-11T10:00:00Z`, external id `upwork-room-msg-itest-umh-202`, observed
   on a later poll (which is why `D_new` exists by the time normalization runs).
4. `upworkcrm.Normalize`.

## Observed behavior

### Defect 1

```
sent_external_id is NULL: the matcher missed a body that differs only in whitespace
  (NBSP at char 81, trailing spaces, blank-line run). textmatch.NormalizedPrefix on
  both sides matches. want "upwork-room-msg-itest-umh-101"
confirmed_at is NULL
delivery_confirmed task_events = 0, want 1
```

The delivery row is left exactly as it was seeded. Nothing retries an exact
comparison, so it stays unclaimable.

### Defect 2

```
delivery 3 (room-b, sent 2026-07-11T13:00:00Z — 3h AFTER the message at
  2026-07-11T10:00:00Z) was stamped sent_external_id="upwork-room-msg-itest-umh-202"
delivery 2 (room chat, the one that produced the message) has sent_external_id NULL
```

The wrong row gets stamped and the correct one stays unclaimable — the reported
symptom exactly.

The mutation-style proof in the same test (the model the jira entry used) runs
the candidate selection twice against the same reset fixture:

- without the floor → picks `D_new` (the wrong row), matching what `Normalize`
  did;
- with `AND (send_attempted_at IS NULL OR send_attempted_at - interval '2
  minutes' <= {message sent_at})` added → picks `D_old` (the correct row).

That half of the test passes, i.e. the floor is sufficient to select correctly on
this fixture. It is recorded as evidence, not as a proposed fix.

## Expected behavior

### Defect 1

The whitespace-only difference must not defeat loop closure. The delivery gets
`sent_external_id = 'upwork-room-msg-itest-umh-101'`, `confirmed_at` set, and one
`delivery_confirmed` task event — which is what the other three matchers do by
comparing `textmatch.NormalizedPrefix` on both sides
(`google/sink.go:543`, `jira/sink.go:285`, `slackweb/sink.go:304`).

### Defect 2

`D_old` — the delivery in the room the message is actually in, attempted before
the message existed — gets the external id. `D_new` is untouched and remains
available for its own message.

## Reproduction location

**SUPERSEDED 2026-08-26.** The repro file was replaced by the permanent
regression suite, `internal/connector/upworkcrm/matcherhardening_regression_integration_test.go`
— test-author converted it rather than keeping both, because the repro's defect-2
test asserted an attempt-time floor that the settled fix explicitly rejects
(see the diagnosis, "Superseded: the era assumption"), and two contradictory
contracts in one package is worse than one. The regression suite still fails on
pristine `main`, which is what this section existed to establish; the command
below is unchanged and now runs it.

~~`internal/connector/upworkcrm/matcherhardening_repro_integration_test.go`~~

```
DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
  go test -tags integration -p 1 -count=1 -run 'SWT18' -v ./internal/connector/upworkcrm/
```

Both tests fail. Nothing else in the package changes state: the whole suite under
`-tags integration` is 21 PASS + these 2 FAIL, and a second consecutive run
produces byte-identical failures (rerunnable against the persistent compose db).

## Environment

- `git rev-parse HEAD` = `526233b259a71b9e059e2747908ac9b59ec8c9e7` (main, clean)
- Local compose Postgres, `switchboard-postgres-1` healthy,
  `DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable`,
  `schema_migrations` at **0014** (matches `ls migrations/`). The production
  cluster db was not touched.
- No MQTT, no network, no LLM, no Upwork credentials needed.
- Data prerequisites: only what the tests seed. The shared `upwork_crm`
  `source_accounts` row is created via `EnsureAccount` and never deleted.
- Cross-suite cleanup pact respected (IK "integration suites cross-pollute"):
  every fixture is keyed `itest-umh-*` / `dddddddd-*`, cleaned before and after in
  FK order, including this suite's own `person_identities` (scoped by value, never
  provider-wide) and the orphan `people` they leave behind, per
  `integration_test.go:83-86`.

## Notes

Observations only.

- The matcher is reached through `upworkcrm.Normalize(ctx, sink, Config{})`, the
  same entry point `loopclosure_integration_test.go` uses; `confirmUpworkDelivery`
  needed no new exported surface to exercise.
- Defect 1's fixture-validity assertions are hard failures (`t.Fatalf`) on
  purpose: if `left(body,120)` ever agreed, or `NormalizedPrefix` ever disagreed,
  the fixture would no longer isolate the defect and the run below it would prove
  nothing.
- The `NormalizedPrefix` comparison is done in Go, never re-spelled in SQL — IK
  records that Postgres POSIX `\s` does not cover the unicode spaces Go's
  `strings.Fields` does, so a SQL re-spelling would not be the same test.
- Defect 1 wrote **no** `outbound_observed` task event (the count was 0, and the
  test asserts 0). The report's second consequence — capture logging switchboard's
  own message as sent by hand — is therefore not exercised by this artifact;
  `Normalize` alone is what these tests run. Whether capture is a separate pass was
  not investigated.
- Defect 2's two deliveries hang off the same task, which is not required by the
  matcher (it filters on `channel`, `status`, the two NULL columns, `target_ref
  LIKE`, and the body prefix) — it just keeps the fixture small.
- Both stamped/unstamped outcomes are consistent with the unique index
  `deliveries_sent_external_idx (channel, sent_external_id) WHERE sent_external_id
  IS NOT NULL`: only one row can carry a given external id, so a wrong bind is not
  merely untidy, it locks the correct row out.
