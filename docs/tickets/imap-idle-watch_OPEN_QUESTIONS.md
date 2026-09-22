> Jira: SWT-73

# imap-idle-watch — open questions

One question. The SPEC body is written for **A** and names the exact change for **B** (D3, and
rollout step 5.3).

## OQ-1 — When the watcher Deployment goes live, does `cronjob/connector-google` get suspended, or does it keep running beside it?

**A. Suspend it.** One process touches the mailboxes. The in-process reconcile sweep
(`MAIL_RECONCILE_INTERVAL`, 10m) does byte-identical work to a `*/10` tick — `watchPass` is
`runIMAPIngest` -> `Normalize` -> `ObserveOutbound` -> `EvaluateRules` -> `AnnounceCaptured`, the same
five calls in the same order as the one-shot pass — so the CronJob adds no coverage the sweep lacks. It
does remove the last rotation hazard on the MSN mailbox: side-by-side mints are serialized by the
per-account advisory lock, but only because `idleOnce` makes exactly one connection per lock
acquisition, which is a comment at `watch.go:257-263` and (after this ticket) a test, not a schema
constraint. Cost: if the Deployment crash-loops on a bad image or a bad env, mail ingestion stops
entirely until someone notices `/healthz`, `/funnel` staleness or the absence of mail. Rollback is one
`kubectl patch` (`suspend: false`), no image, under a minute.

**B. Keep it running at `*/10`.** Belt-and-braces: a crash-looping or wedged watcher costs latency, not
ingestion, because every ten minutes a fresh short-lived process does the whole pass anyway — the
failure mode cron is genuinely good at. Cost: two processes hold the same per-account locks, so roughly
half the ticks do nothing but count `accounts_busy`; the MSN refresh token is redeemed by two processes
(safe today, by the invariant above — criteria 8 and 22 become load-bearing rather than regression
guards); both announce `captured` on the broker; and any env drift between the CronJob and the
Deployment (`CAPTURE_RULES_MODE`, `CAPTURE_RULES_SINCE`, `MAIL_MAX_MESSAGE_BYTES`) produces two
different behaviours on the same mailbox depending on which one won the lock — the kind of split-brain
configuration that is invisible in logs.

A middle option exists and is worth saying out loud so it is a deliberate choice, not a default: keep
the CronJob but move it to `0 */2 * * *` (or `*/30`), so it is a safety net rather than a co-worker —
most of the mail is instant, and a wedged watcher costs at most one net interval. It carries B's
split-brain risk at lower volume.

**Answer:** _A / B / B with a reduced schedule (say which)_

---

Answer by editing the entries. Say "questions answered" and I'll fold them into the SPEC.
