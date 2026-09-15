# Reproduction — sana-email-not-captured (SWT-58)

## Status
Confirmed. The failure matches the report: every stage after routing is silent, and no task is created.

## Trigger
This is the production situation around `normalized_messages` 291568, copied from a read-only prod snapshot taken 2026-09-15:

1. **Account.** A google receiving account (prod 1009, `salvador@handsonconnect.org`), with `route_after` NULL as on prod.
   - It has two `source_account_projects` candidates: collaboratory (`is_default` true) and reengine.
2. **Project.** Collaboratory has `ai_inquiry` true, `ai_locality` `any`, `ai_classify` false, and `inquiry_promote_after` in the past.
3. **Thread** (prod 159886), in this order:
   - her inbound, about 5 days ago;
   - Salvador's outbound reply, about 3 days ago;
   - her new inbound, 2 h ago (past the 1 h grace, inside the 72 h window).
4. **Capture.** A live `capture_decisions` row with `action='unmatched'` ("no enabled rule matched") on both inbound messages. The outbound has no decision.
5. **Route verdict.** A `classify_route` `ai_runs` row (status ok, prompt `route-v2`) and its `ai_extractions` row. The fields copy prod 11630: `project_index` 1, `project_id` = collaboratory, `grounded` true, `candidates` 2, the subject as evidence, and `normalized_message_id` / `source_account_id` set.
6. **Passes.** Then the REGISTERED pipelined passes (`buildPass`) run in order: `route_apply` → `inquiry` → `inquiry_promote`.
   - The inquiry stage's local model is a fake httptest ollama with a canned verdict. No live LLM, no broker.

## Observed behavior
```
=== RUN   TestSanaRepro_Integration_RoutedUnmatchedEmailBecomesHoldingTask
INFO route_apply pass written=0 thread=0 single=0 model=0 default=0 pending_verdict=0 no_default=0 verdict_before_arming=0 candidate_revoked=0
INFO inquiry pass processed=0 flagged=0 skipped=0 errors=0
INFO inquiry_promote pass review=0 created=0 attached=0 lost_claims=0 reopened=0 gated="map[answered:0 claude_task:0 kind:0 not_addressed:0 pending:0 rethreaded:0 stale:0]"
    sana_repro_integration_test.go:208: processed: route_apply=0 inquiry=0 inquiry_promote=0; fake-model chat calls=0
    sana_repro_integration_test.go:216: latest capture decision for the new inbound: mode=live action=unmatched project_id=0 (collab=3)
    sana_repro_integration_test.go:226: STAGE route_apply: 0 mode='route' decision(s) for the routed message (project_id=0 step=""); want 1 row attributing it to collaboratory (project_id=3)
    sana_repro_integration_test.go:236: STAGE inquiry: 0 classify_inquiry verdict(s) for the message (fake model called 0 time(s)); want 1
    sana_repro_integration_test.go:247: STAGE inquiry_promote: classify_promotions action="" task_id=<nil>, holding tasks in collaboratory=0; want a promotion with a task and exactly 1 'holding' task
--- FAIL: TestSanaRepro_Integration_RoutedUnmatchedEmailBecomesHoldingTask (0.05s)
FAIL	github.com/sspataro57/switchboard/cmd/pipelined	0.055s
```
The message's latest decision stays `live / unmatched / no project`, as on prod. None of the three stages writes anything, and the inquiry model is never called.

## Expected behavior
- `route_apply` writes one `mode='route'` `capture_decisions` row that attributes the message to collaboratory.
- The inquiry pass writes one `classify_inquiry` verdict.
- `inquiry_promote` records a `classify_promotions` row with a task, and there is one `holding` task in collaboratory.

## Reproduction location
`cmd/pipelined/sana_repro_integration_test.go`. Integration build tag. Later split, with the fix, into the three `TestRegression_SanaEmailNotCaptured_*` cases (unarmed, armed before the verdict, armed after it); the original single test name below is historical. It runs on the isolated DB `ops_sanabug` on the compose Postgres at :5433, migrated to 0036.

```
DATABASE_URL='postgres://ops:ops@localhost:5433/ops_sanabug?sslmode=disable' \
  go test -tags integration -p 1 -count=1 -v -run 'TestRegression_SanaEmailNotCaptured' ./cmd/pipelined/ ./internal/capture/
```
- The test refuses a `DATABASE_URL` containing 192.168.50.49.
- It cleans up its own rows before and after the run, so it can be re-run.
- It reuses `fakeOllama`, `registeredPass` and `piqFake` from `inquiry_integration_test.go` in the same package.

## Environment
- `git rev-parse HEAD`: 8028d3cc947516e7dff668072ad2362e5e62a03a (branch fix-sana-email-not-captured)
- Prod `schema_migrations` max = 0036, and the scratch DB matches.
- No env vars are required beyond `DATABASE_URL`. The test sets `OPS_LOCAL_PROVIDER_URL` / `OPS_LOCAL_MODEL` to the fake server, and leaves `MQTT_BROKER` unused (pub nil).

## Notes
Observations only, not diagnosis.

- **Prod snapshot** (read-only, `BEGIN READ ONLY … ROLLBACK`, 2026-09-15):
  - `source_accounts` 1009 has `route_after = NULL`.
  - Prod has zero `capture_decisions` rows with `mode='route'`, in any action or step.
  - The fixture copies the NULL `route_after`. It was not varied to see whether the result changes; that is left to the diagnoser.
- **route_apply's counters.** The pass logs `written=0` with EVERY unrouted counter at 0, including `pending_verdict`, `no_default`, `verdict_before_arming` and `candidate_revoked`.
- **inquiry.** The inquiry pass logs `processed=0 skipped=0`. That matches prod's `processed=0` since the 17:48Z restart.
- **inquiry_promote.** In this run, `gated` is all zeros. Prod's gated `{answered:8 pending:3}` comes from other messages; this message never appears in either inbox.
- **Prod thread facts:**
  - The thread key is `gmail:salvador@handsonconnect.org:<…outlook.com>`.
  - The earlier inbound 158690 carries live + shadow `unmatched` rows.
  - The outbound 201924 has no decision.
- **Cleanup.** The scratch DB `ops_sanabug` was left in place for the diagnoser. Drop it by hand when finished.
