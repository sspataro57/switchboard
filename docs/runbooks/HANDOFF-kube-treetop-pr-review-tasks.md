# Handoff to the kube session — treetop-pr-review-tasks (SWT-54)

The switchboard session built and tested this ticket, and it builds and pushes the
image after the merge. The kube session owns the manifests in `kube/switchboard`.
The ORDER below is the point: do not reorder it.

**Image:** built from `main` after the merge and pushed to `192.168.50.20:5000`; the
tag is filled in at delivery. Use ONE tag for every workload below.

## 1. Apply migration 0035 FIRST

`migrations/0035_capture_rules_pr_review.sql` adds two columns to `capture_rules`
(`pr_review BOOLEAN NOT NULL DEFAULT false`, `exclude_pr_authors TEXT[] NOT NULL
DEFAULT '{}'`) and two named CHECKs, `capture_rules_pr_review_github` and
`capture_rules_exclude_needs_pr_review`. No data change, no new table, no index.

- Run the one-shot migrate Job against the `ops` db on pg-main. From a
  workstation, `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/tools/migrate --dir migrations`
  does the same.
- Before: 0033 is in `schema_migrations`. 0034 belongs to SWT-53
  (chat-on-closed-task) on another branch and may or may not be applied yet. The
  migrate runner keys on each version, so 0035 applies with or without 0034, and
  a later 0034 still applies after it.
- After:
  `SELECT version FROM schema_migrations WHERE version = '0035'` returns one row, and
  `SELECT conname FROM pg_constraint WHERE conrelid = 'capture_rules'::regclass AND conname LIKE 'capture_rules_%pr_review%'`
  returns both constraint names.

**Why first:** `loadRules` selects `r.pr_review, r.exclude_pr_authors` on EVERY
capture pass. A new image on a db without 0035 fails capture for every connector,
on every tick. `opsctl capture-rules list` and `capture_rule_add` name the columns
too.

Old images on a 0035 db are fine. They never name the columns, and with no
`pr_review` rule seeded the columns are inert.

## 2. Then roll ONE image tag to every capture workload, together

| workload | kind | why |
|---|---|---|
| connector-google | CronJob | runs the capture pass; carries the new counters |
| the google IMAP watcher (`--watch`), if deployed | Deployment | runs the capture pass on every IDLE wake |
| connector-jira, connector-slackweb, connector-upworkcrm | CronJob | each runs the capture pass after its sync |
| pipelined | Deployment | the gate stage prints the capture counter line |
| connector-gcal, classify-*, orchestratord, dashboard | as today | same tag, so no binary lags |

The capture pass is global: any connector's run decides EVERY pending inbound
message, GitHub mail included. So every workload that runs capture must carry the
new binary before step 3, not just connector-google.

**Rollout barrier: step 3 waits until EVERY workload above reports the new tag**
(`kubectl -n ops get cronjob,deploy -o wide`). An old capture binary ignores
`pr_review` and would take the seeded rule as a plain github rule:
- it would key refs in the PATH spelling, `treetopllc/{repo}/pull/{N}`;
- it would create tasks for his OWN PRs;
- the titles would read `{key} — {subject}`;
- a later new binary would then create a SECOND task per PR under the canonical key
  `treetopllc/{repo}#{N}`.

No new env var, no new route, no new port.

**Amendment 2026-09-14: roll 0.7.22 before step 3.** 0.7.21's trust gate rejected
genuine GitHub mail whose Gmail `Authentication-Results` quotes `header.b` (the
signature prefix holds `/` or `+`, e.g. `header.b="FVP32/f4"`). The step-3a dry run
flagged 4 such messages as untrusted. 0.7.22 exempts a quoted base64-only `header.b`
and nothing else (SPEC D1, "Quotes"). No migration and no manifest change beyond
the tag: roll 0.7.22 to the same workloads as above, together, and step 3 waits
for the same barrier on 0.7.22. The follow-up for the tokenizer's whitespace
handling is SWT-55.

## 3. Seed the rule (switchboard session, from `main`, only after step 2)

**3-0. Verify the barrier yourself, first.** It is procedural: nothing in code
enforces it. Run

```bash
kubectl -n ops get cronjob,deploy -o wide
```

and check that EVERY capture workload in step 2's table shows the new image tag in
its IMAGES column. That covers every connector CronJob, the google IMAP watcher if
deployed, and pipelined. If any one shows an older tag, STOP and do not seed. Record
the output in the delivery summary.

`go install ./cmd/opsctl` from `main` first. `capture_rules` cannot be edited, and the
same pattern cannot be re-added (IK F8), so dry-run before the add.

**3a. Dry run on prod. It writes nothing. SAVE its output: step 4 needs it.**

```bash
DATABASE_URL="$OPS_DATABASE_URL" opsctl capture-rules try --project collaboratory \
  --type thread_key_contains --pattern '<treetopllc/' --external-system github \
  --key-regex '<(treetopllc/[A-Za-z0-9._-]+/pull/[0-9]+)@github\.com>$' --priority 91 \
  --pr-review --since 720h --show wins | tee ~/swt54-try-pre-add.txt
```

This must be run BEFORE 3b. Once the rule is stored, `try` with the same (project,
type, pattern) refuses by name: the candidate could never win against the stored rule,
so it would print an empty backfill.

The output file lives OUTSIDE the repo on purpose: it holds client PR titles, and
`.gitignore` does not cover it.

**Origin-check gate, first.** No message in the output may read
`skipped (untrusted GitHub mail, fell through)`:

```bash
grep -c 'untrusted GitHub mail' ~/swt54-try-pre-add.txt   # must print 0
```

Zero proves the Go origin check agrees with the prod findings on the real stored mail.
The check covers three things: Gmail's dkim=pass for github.com, single-instance
headers, and the PR bound to the signed `List-ID` and `Subject`. On 2026-09-14,
123 of 123 Treetop PR-thread mails bound, with one List-ID each of the form
`owner/repo <repo.owner.github.com>` and a decoded Subject ending `(PR #N)`. Any hit
means STOP. The reason names what failed (`List-ID binding failed`,
`Subject binding failed`, `header … appears more than once`, or the
Authentication-Results). Bring the output back to the switchboard session and do not
add the rule.

Then check every PR in the rollup against its author on github.com:
- his PRs read `verdict own` and fall through to rule 10 or rules 6/7;
- colleagues' PRs, Dependabot included, read `other` or `undetermined`;
- WEB-/API-titled PRs are won by the candidate;
- the candidate loses only to an LHH key (rules 1/2).

Any wrong verdict means STOP: bring it back to the switchboard session. Do not add
the rule.

**3b. Add it:**

```bash
DATABASE_URL="$OPS_DATABASE_URL" opsctl capture-rules add --project collaboratory \
  --type thread_key_contains --pattern '<treetopllc/' --external-system github \
  --key-regex '<(treetopllc/[A-Za-z0-9._-]+/pull/[0-9]+)@github\.com>$' --priority 91 \
  --pr-review \
  --note "treetop-pr-review-tasks: one review task per treetopllc PR he did not author"
```

- **No `--exclude-pr-author`.** The exclude list starts EMPTY. OQ-1 = (b): bot PRs,
  Dependabot included, get review tasks. Pre-check 0b found one login, `sspataro57`,
  and no second one.
- **No `--url-template`.** It is refused for github; capture builds
  `https://github.com/{owner}/{repo}/pull/{N}` itself.
- **The notice close needs no flag.** It is part of `pr_review` (OQ-2 = a): a later
  "Merged #N into …" or "Closed #N." notice closes that PR's review task through
  `task_close`.

Then `opsctl capture-rules list` must show the rule at `prio 91` with
`-> github key <…> pr_review exclude_pr_authors=[]` on its key line.

## 4. The one-off hand backfill for PRs already open (switchboard session)

Only NEW messages act once the rule is seeded, so a colleague's PR already waiting
for review would get no task. The payloads come from the output SAVED in step 3a
(`~/swt54-try-pre-add.txt`). Do NOT rerun `try` after the add: it refuses. Its
"D9 backfill payloads" section prints three one-line calls per PR that meets all of:
- verdict `other` or `undetermined`;
- no merge or close notice seen;
- mail in the last 30 days;
- no task yet.

For each such PR, run the three calls in order (SPEC D9):

1. `opsctl call --tool create_task --args '…'`, then note the `task_id` it returns;
2. `opsctl call --tool link_external_ref --args '…'`, with that id where `TASK_ID`
   stands;
3. `opsctl call --tool task_set_source_thread --args '…'`, with the same id.

`TASK_ID` is printed bare on purpose: a payload pasted without substituting it is
invalid JSON, and `opsctl call` refuses it. Before creating each PR's task, check
two things:
- the PR is still open on github.com;
- it still has no task. Mail that arrived between 3a and 3b may have created one:
  `SELECT task_id FROM external_refs WHERE system='github' AND external_key='<key>'`.

Record the created task ids in the delivery summary.

## 5. Watch

- The next ticks' `capture_rules:` lines carry `"pr_author_skipped"` and
  `"pr_closed"` (zeros included). The `capture_gate:` line prints both as 0 always.
- `SELECT system, external_key, external_url, task_id FROM external_refs WHERE system='github';`:
  every key is `treetopllc/{repo}#{N}` and none is path-shaped (`/pull/`).
- `opsctl capture-rules report --since 24h`.
- The collaboratory board: "Review PR #N — {repo}: {title}" rows, lit blue/grey.
- On the first merge or close notice, the task closes with the reason
  "PR #N merged on GitHub (message M)" or "… closed …".
- Any decision reason containing `untrusted GitHub mail` on a treetopllc PR thread.
  None is expected: every prod PR mail carries Gmail's dkim=pass for github.com. One
  on real GitHub mail means the origin check's premise changed; bring it back.

## Rollback

**Never roll an image back past this ticket while a pr_review rule is enabled.** An
old binary takes the rule as a plain github rule: path-spelled refs, tasks for his
own PRs, and a second task per PR afterwards. Disable the rule FIRST, then roll back:

`opsctl call --tool capture_rule_set_enabled --args '{"rule_id":<id>,"enabled":false}'`.

PR mail returns to rules 10/6/7 on the next NEW message. Created review tasks stay:
they are real PRs, so close them by hand. 0035 stays, since migrations are
forward-only, and it is inert with no `pr_review` rule enabled.

## Accepted residuals (SPEC "Accepted residuals")

- **(a)** Undetermined authorship is fail-open. A task can occasionally appear for
  his OWN PR, for example when his first mail on it says reason `mention`. Close it
  as Done: a Dismiss is reopened by SWT-36 on the PR's next trusted mail that is not a merged, closed or reopened notice.
- **(b)** The mixed-version barrier is procedural: step 3-0, with the exact
  `kubectl -n ops get cronjob,deploy -o wide` check.
- **(c)** A transient `link_external_ref` failure after `create_task` can give one PR
  a second task on a later notification. This is the general capture partial-write
  weakness, tracked in SWT-50.
