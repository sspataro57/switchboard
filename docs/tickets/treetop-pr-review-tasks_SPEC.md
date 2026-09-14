> Jira: SWT-54

# treetop-pr-review-tasks — one review task per Treetop PR he did not author

**Status: decided (2026-09-14).** Both owner questions are answered
(`treetop-pr-review-tasks_OPEN_QUESTIONS.md`):
- **OQ-1: (b) Bot PRs get tasks too.** This is NOT the recommended default. Dependabot and every other
  bot PR becomes a review task like a colleague's. The seeded rule carries NO `*[bot]` exclusion;
  its exclude list holds only Salvador's own second login(s), if pre-check 0b finds any. Every
  mention below of seeding `--exclude-pr-author '*[bot]'` is superseded by this answer.
- **OQ-2: (a) A merged or closed notice closes the review task,** with the reason
  "PR #N merged on GitHub" (or closed).

**Migration number:** this ticket takes **0035**. SWT-53 (chat-on-closed-task), specced in
parallel, owns 0034. Wherever this SPEC says 0034, read 0035.

## Source

Ad-hoc, owner request 2026-09-14 (verbatim):

- "also the pull request are not showing up as taks"
- "the pr task important are the ones in the treetop repos. the other I control"
- Asked which PRs should become tasks: "Review requests only"
- Then: "not just collaboratory-www and gonoble all the repos there prs where I'm not the author"

Read together, the last answer widens the first. The set is **every PR in any `treetopllc/*`
repo that Salvador did not author**, not only PRs that formally request his review.
`Foundry-Underwriting/*` and `tower987124/*` are out: he controls those.

## Goal

Capture turns the GitHub notification mail for any `treetopllc/*` pull request he did not author
into exactly ONE human `ready` task, "Review PR #N — <repo>: <title>", keyed by an `external_refs`
row (`system='github'`). Later mail on that PR logs onto the task. His own PRs never create one.

**Usable alone:**
- Once the rule is seeded, the next GitHub mail on a colleague's Treetop PR puts a review task on the
  collaboratory board, with a working PR link.
- Mail on his own PRs keeps doing exactly what it does today.
- No GitHub connector, webhook or token is needed. The mail he already receives is the only input.

## Evidence status (read before trusting any number here)

- **From the owner's investigation (brief, 2026-09-14), not re-measured by this SPEC:**
  - 126 GitHub mails over 17 PRs in 30 days (15 collaboratory-www, 2 gonoble);
  - X-GitHub-Reason values review_requested (2), subscribed, push, mention, comment, with "many" mails
    showing none;
  - 104 `task_log` and 22 `attributed` decisions;
  - no GitHub source account, and 0 github `external_refs`;
  - rules 6/7 are attribution-only at priority 50.
- **Verified by reading code in this worktree (main 62046e5):** everything under "What exists" below.
- **NOT verified: the prod read-only queries.** This SPEC session had no shell, so it could not run
  psql. The header facts the authorship design depends on are GitHub's documented notification
  headers, and Verification step 0 (0a–0f) turns each one into a **blocking** pre-check before any
  code is merged. If 0a or 0b contradicts the design, stop and re-spec. Do not adapt the code to the
  data silently.

## What exists (code-read)

- **Thread keys carry the PR.** `google.NormalizeRFC822` builds
  `gmail:{account}:{threadRoot}`. `threadRoot` is `References[0]`, else `In-Reply-To`, else the
  message's own Message-ID (`internal/connector/google/rfc822.go:154`). GitHub's PR-opened
  notification has Message-ID `<{owner}/{repo}/pull/{N}@github.com>`, and every later notification
  on the PR references it. So **every mail about one PR shares one thread key ending in that root**
  (`internal/capture/rules_test.go:112` pins the sample
  `gmail:sspataro@gmail.com:<treetopllc/collaboratory-www/pull/3179@github.com>`). Issues use
  `/issues/N`.
- **Headers are not normalized, but they are stored.** `normalized_messages` holds sender (the raw
  From), subject, body_text and external_message_id. No X-GitHub-* header reaches it. The whole
  RFC822 is kept in `raw_source_items.raw_json.rfc822_b64`: always the headers, and the body up to
  `MAIL_MAX_MESSAGE_BYTES`; a truncated row keeps headers plus text parts (IK "Mail attachments over
  MCP"). `google.walkEnvelope` (`attachments.go:149`) already decodes that envelope.
- **Capture's evaluator sees no headers** (`capture.Message`, `rules.go:104`). First match wins by
  `priority DESC, id ASC`. The key comes from `key_regex`'s FIRST capture group, run over the thread
  key for `thread_key_*` kinds (`keyText`, `rules.go:251`).
- **The driver** (`rules_store.go` `decideMessage`) does the rest:
  - It looks up `taskForExternalRef(system, key)`. Found means `task_log`: logs on the task whatever
    its status, and runs SWT-36's reopen if the task is dismissed, or SWT-45's revive if the rule is
    jira activity.
  - Not found means `task`: `create_task` (human, `ready`, priority 0), `link_external_ref`, then
    `task_set_source_thread`.
  - Titles come from `ruleTaskTitle`: `{key} — {subject}` for a key_regex rule.
- **The GitHub connector's PR key is `{owner}/{repo}#{N}`.** It is spelled inline as
  `fmt.Sprintf("%s#%d", ref.Repo, ref.PR)` in `github.PGTaskResolver.Resolve`
  (`internal/connector/github/store.go:114`), and orchestrator R9–R11 drive tasks through it. It is
  not deployed.
- **`system='github'` is already legal in all three enum spellings.** They are the 0015 CHECK,
  `captureExternalSystems` in `internal/tools/capturerules.go:56`, and `validateLinkExternalRef` in
  `prci.go:47`. No enum change is needed.
- **`revive` is jira-only** (`capturerules.go:150`, `decideMessage`'s
  `activity := system == "jira" && …`), and Part D's hold keys on jira. A github rule is never held.
- **`capture_rules` cannot be edited, and the same pattern cannot be re-added** (UNIQUE
  `(project_id, criteria_type, pattern)`; IK F8). The seeded rule must be right the first time.
- **Capture is LIVE in prod (2026-09-09).** A rule added to prod acts on the next connector tick,
  which is why Verification uses a write-nothing dry run instead of seeding "in shadow"
  (see D10).

## Decisions (the eight asked, with rationale)

### D1. Authorship comes from GitHub's own headers, read from the stored RFC822

**Evidence used**, all read from `raw_json.rfc822_b64` headers (the raw-first row) and never from
body text:

- **`X-GitHub-Reason: author`** on ANY stored inbound mail of the PR. GitHub sets this reason exactly
  when the recipient authored the thread, which makes it the strongest signal and one that needs no
  login data.
- **The opening notification**: the inbound message whose `external_message_id` equals
  `<{owner}/{repo}/pull/{N}@github.com>`. An exact equality lookup; no key is parsed in SQL.
  - Its `X-GitHub-Sender` is the PR author's login.
  - Its `X-GitHub-Recipient` is his own login, as GitHub addressed that mail.
- **`X-GitHub-Reason: review_requested`** on any stored mail of the PR. You cannot request your own
  review, so it proves the author is someone else, without naming them.

**Pure verdict**, `decidePRAuthor(facts, excludeList)`, first hit wins:

| # | evidence | verdict | effect |
|---|---|---|---|
| 1 | any mail has reason `author` | `own` | the rule does not apply (D2 fall-through) |
| 2 | opening found, sender equal (case-folded) to that mail's recipient | `own` | fall-through |
| 3 | opening found, sender matches `exclude_pr_authors` (D6: his other logins, bots) | `excluded` | fall-through |
| 4 | opening found, sender not empty | `other` | create the task; the reason names the author login |
| 5 | any mail has reason `review_requested` | `other` | create the task; the reason says "author not named" |
| 6 | none of the above | `undetermined` | **create the task**; the reason says `author undetermined` |

**Undetermined creates the task (fail-open).** The two errors are not symmetric:
- A false task on his own PR costs one Dismiss (`wrong_kind`, which is labelled data).
- A missed review request on a client repo is invisible, and review is the work the owner said
  matters.

It should be rare. GitHub does not mail you your own activity by default, so the first mail he gets
on his own PR is normally someone else's comment or review, and that carries reason `author`
(row 1). Pre-check 0b measures the rate.

**His login is data, not a Go literal.** Row 2 reads it per mail from `X-GitHub-Recipient`. Extra
logins (another account) and bots are listed in `capture_rules.exclude_pr_authors`. An entry either
equals a login case-insensitively, or starts with `*` and is a suffix match: `*[bot]` covers every
GitHub App bot.

**Rejected candidates:**
- **From display name.** It names the actor of THAT notification, not the PR author, and a display
  name is not a login, so it cannot be compared with stored logins.
- **Reply-To.** A per-thread reply token.
- **"@login opened this pull request" in the body.** Not in GitHub's text part in a stable form, and
  a body parse would re-spell what the header states.

**Scope of the read.** Two sets of mail are read:
- every inbound message on the pending message's `thread_id`;
- the opening message by exact `external_message_id`.

Newest first, capped at 100 rows. Each row's `raw_json` is decoded to headers only. A row with no
`rfc822_b64` (a gmail:-shaped API or bridge row) contributes nothing. **Residual:** if one PR's mail
were split across two receiving accounts, the other account's thread is not read. Pre-check 0f
measures this (expected: one account).

**When authorship is checked.** Only on the CREATE branch, when no ref exists yet. Once a task
exists, every later mail logs to it whatever the verdict would say. A task created as undetermined
is not retracted by later `own` evidence. He closes it as Done (`task_close`); a Dismiss does not
stick, because SWT-36 reopens a dismissed task on the PR's next trusted mail that is not a merged, closed or reopened notice (see "Accepted
residuals").

**Amended 2026-09-14 (review fix; Codex CRITICAL): the origin check, and the threat model.**

- **Threat.** Every input the rule reads is sender-controlled. Any inbound mail can carry a
  GitHub-shaped `Message-ID` / `References`, which becomes the thread key the rule matches, and any
  `X-GitHub-*` header it likes. Without an origin check, a forged mail could:
  - CREATE a review task (a forged PR root);
  - SUPPRESS one (a forged `X-GitHub-Reason: author` on a colleague's PR thread);
  - CLOSE one (a forged `Closed #N.` notice).
- **Evidence.** Prod, read-only, 2026-09-14: 121 of 121 Treetop PR-thread mails over 90 days carry a
  Google-added `Authentication-Results` header. Its authserv-id is `mx.google.com`, and it shows
  `dkim=pass` with `header.i=@github.com` / `header.d=github.com`. This holds on both receiving
  accounts (1003 and 1009).
- **Rule.** `trustedGitHubNotification(mail.Header) bool` is pure and lives in `prreview.go`.
  - It reads ONLY the FIRST (topmost) `Authentication-Results` header. The receiving MX prepends its
    own, so every header below it travelled with the message and may be forged.
  - The authserv-id must be exactly `prTrustedAuthServID` (`mx.google.com`, a named constant: data,
    not scattered literals).
  - It needs a `dkim=pass` result whose `header.d` is `github.com` or whose `header.i` ends in
    `@github.com`.
  - Comments are removed before parsing, so a `(dkim=pass …)` comment is not a result.
  - **Single-instance headers (Codex re-review, 2026-09-14).** An attacker can relay a GENUINE
    GitHub-signed notification and PREPEND unsigned copies of action-driving headers. DKIM verifies
    the signed instance, so Gmail still stamps dkim=pass for github.com, but `mail.Header.Get`
    returns the attacker's first instance, which could point a genuine create or close at a
    treetopllc PR root. So the mail is UNTRUSTED when any of `Message-ID`, `References`,
    `In-Reply-To`, `X-GitHub-Reason`, `X-GitHub-Sender` or `X-GitHub-Recipient` appears more than
    once (`duplicatedGitHubHeader`, pure). Genuine GitHub mail never duplicates them. The reason
    names the header: `header References appears more than once; …`.
  - **What we rely on:** Gmail's dkim=pass, single-instance headers, and the PR bound to the SIGNED
    `List-ID` and `Subject` (below). The body close notice is covered by DKIM's body hash (GitHub does
    not sign with `l=`). We do not re-verify signatures ourselves.
- **Where it applies: before ANY pr_review action.**
  - The pending mail itself (`prMailTrusted`, read from its own raw row). An untrusted mail on a
    pr_review rule FALLS THROUGH to the next rule, exactly like the own-PR fall-through, so today's
    behaviour is preserved.
    - Its reason begins `rule R skipped: untrusted GitHub mail for PR {key} (…); `.
    - It creates, logs and closes nothing through the PR rule.
    - It is not counted in `pr_author_skipped`.
  - Authorship evidence. Thread-side reasons and the opening lookup count only trusted mails, so a
    create happens only when the matching mail is trusted, and a merged/closed notice closes only
    when trusted.
- **Consequences.**
  - A row with no RFC822 header (a gmail:-shaped `{}` row) cannot prove where it came from, so it
    falls through. Prod has no such row: all 16,490 google raw items are IMAP envelopes.
  - If the receiving accounts ever move off Gmail, the constant changes, in one place. If
    `MAIL_SOURCE` moves off `imap` (bridge, gmail_api), every PR mail becomes untrusted (residual c).
- **Outside the threat model:** Gmail itself, and a compromised github.com DKIM key.
- **Residuals of the origin check:**
  - An UNMODIFIED relay of an attacker's own GitHub mail is harmless. It is genuinely GitHub's, but
    its thread root names their repo, not `treetopllc/`, so the rule never matches it.
  - **GitHub's DKIM `h=` lists, checked on prod 2026-09-14** (read-only, 90 days, Treetop PR mail):
    - follow-ups (71 mails): `date:from:reply-to:to:cc:in-reply-to:references:subject:list-id:list-archive:list-post:list-unsubscribe:list-unsubscribe-post:from`;
    - openings (52 mails): `date:from:reply-to:to:cc:subject:list-id:list-archive:list-post:list-unsubscribe:list-unsubscribe-post:from`.

    So **`Message-ID` and every `X-GitHub-*` header are NOT signed**, and `In-Reply-To` /
    `References` are signed on follow-ups only. Single-instance enforcement alone does not stop a
    REPLACED unsigned header. The attack it leaves: relay a genuine GitHub OPENING of the attacker's
    own PR and replace its single, unsigned `Message-ID` with `<treetopllc/{repo}/pull/{N}@github.com>`.
    The thread key derives from `Message-ID` when there are no `References`, and DKIM still passes.
  - **Binding the PR to SIGNED headers (2026-09-14).** A mail is trusted for pr_review only if
    `prMailBindingFailure(h, ref)` is empty:
    - **List-ID.** Its angle part equals `{repo}.{owner}.github.com` for the thread key's PR,
      case-insensitively. Prod: 123 of 123 Treetop PR-thread mails carry exactly one List-ID, and
      all 123 are exactly `{owner}/{repo} <{repo}.{owner}.github.com>`, e.g.
      `treetopllc/collaboratory-www <collaboratory-www.treetopllc.github.com>`.
    - **Subject.** RFC 2047-decoded, it ENDS with `(PR #N)` for the thread key's N. Prod: 122 of 123
      raw; the one exception (`collaboratory-www#3218`) is an encoded-word Subject that ends
      `(PR #3218)` once decoded; it is pinned verbatim as a unit fixture. Replies read
      `Re: [owner/repo] Title (PR #N)`, openings `[owner/repo] Title (PR #N)`.

    A mismatch is untrusted, and the reason names the binding (`List-ID binding failed: …` /
    `Subject binding failed: …`). `List-ID` and `Subject` join the single-instance list. Retargeting
    now needs a change to a signed header, which breaks DKIM.
  - **Remaining after the binding (accepted, residual b):** the unsigned `X-GitHub-Reason`,
    `X-GitHub-Sender` and `X-GitHub-Recipient` stay spoofable, whether added or replaced, but only
    by someone relaying GENUINE treetopllc PR mail. They already receive the repo's notifications.
    Impact: at most one review task suppressed (for example a replaced `X-GitHub-Reason: author`).
  - **(a) Outside the threat model: a delivery path where Gmail does NOT prepend its own
    `Authentication-Results`** (for example intra-Workspace mail). A forged header would then be
    topmost. The check assumes Gmail always stamps its own.
  - **(b) Insider relay, accepted.** Someone who already receives genuine treetopllc PR mail could
    relay it with an ADDED X-GitHub-* header the original lacked. GitHub probably does not DKIM-sign
    those headers, and the duplicate guard only catches a second copy. Impact: at most one review
    task suppressed.
  - **(c) `MAIL_SOURCE`.** Switching from `imap` to `bridge` or `gmail_api` stores no RFC822, so
    EVERY PR mail becomes untrusted and the rule stops creating tasks. Moving off Gmail changes the
    one constant; changing `MAIL_SOURCE` needs the header read re-sourced.
  - **(d) A future github-keyed rule WITHOUT `pr_review` on the same threads** would compute the
    same canonical key. On an untrusted fall-through its notice flag is empty, so it could reopen a
    dismissed review task through SWT-36. Nothing does this today; do not add such a rule.
  - **Quotes.** A `"` anywhere in a dkim result makes the mail untrusted (Gmail's real header has
    none): a quoted `header.i` could otherwise smuggle a `header.d=github.com` token.

### D2. Data for the match, code for the author filter, the PR key and the title

**What the data does.** One `capture_rules` row does the "any treetopllc/* repo" match and
one-per-PR dedup, with no new criteria type:

```
--type thread_key_contains --pattern '<treetopllc/'
--external-system github
--key-regex '<(treetopllc/[A-Za-z0-9._-]+/pull/[0-9]+)@github\.com>$'
```

**The pattern.** `<treetopllc/` matches any repo of the org, because the root Message-ID sits inside
every Treetop GitHub thread key (the runbook's "repo rules fire through GitHub notification mail").

**The key_regex.** It runs over the thread key and captures only a `pull` root:
- issue threads (`/issues/N`) and anything else derive NO key, so the rule gives attribution only
  and never creates a task;
- `$` anchors it to the end of the key, where the root sits.

**Code is needed for four things. Data cannot do any of them:**
1. **The authorship filter (D1).** It depends on per-PR facts from other messages' raw headers, and
   no criteria type can express "unless another message says X". It is enabled by a rule flag,
   `capture_rules.pr_review`.
2. **Fall-through.** When the verdict is `own` or `excluded`, the driver re-decides the message
   without the PR-review rule: the next rule in `evaluateAll`'s matched list, else `unmatched`. His
   own PR's mail then gets EXACTLY today's decision:
   - rule 10's log onto the WEB/API/OPS bucket task if its title carries a key;
   - else rule 6/7's attribution;
   - else unmatched, for repos rules 6/7 do not name.

   A `own` decision that did not fall through would take that mail out of every lower rule, including
   the J16 mention successor (see Coordination), which is a regression.
3. **One spelling of a PR key.** `key_regex` returns ONE capture group, so it cannot produce the
   connector's `{owner}/{repo}#{N}`, because `/pull/` sits between repo and number. Two spellings of
   one PR under `system='github'` would let one PR hold two tasks, silently, since the UNIQUE
   `(system, external_key)` cannot see through spellings. The repo has paid for this class of bug
   five times (IK: SWT-13/18/19).

   So the driver canonicalizes every github-derived key through `github.ParsePRRef`, which accepts
   `{owner}/{repo}/pull/{N}` or `{owner}/{repo}#{N}`, and `github.PRKey`, which gives
   `{owner}/{repo}#{N}`. `PGTaskResolver.Resolve` is refactored to call `github.PRKey` (one
   spelling). A github key that does not parse gives attribution only, never a ref.

   The URL is `github.PRURL(ref)`, `https://github.com/{owner}/{repo}/pull/{N}`. **`url_template` is
   refused for `external_system='github'`**, because `{key}` in the canonical form contains `#`.
   Pre-check 0e confirms that no github rule exists today.
4. **The review title (D8's shape).** `ruleTaskTitle`'s `{key} — {subject}` would read
   `treetopllc/collaboratory-www#3179 — Re: [treetopllc/collaboratory-www] Ranking widget (PR #3179)`.
   A pure `prReviewTitle(ref, subject, body)` gives `Review PR #3179 — collaboratory-www: Ranking widget`:
   - it strips leading `Re: ` (repeated, case-insensitive), a leading `[owner/repo] `, and a trailing
     ` (PR #N)` whose N equals the ref's number;
   - it falls back to the body's first line, then to nothing (`Review PR #3179 — collaboratory-www`);
   - it truncates with `textmatch.NormalizedPrefix`, 120 runes.

   It applies only to `pr_review` rules. All other titles are byte-identical.

### D3. A PR mail that names a Jira key goes to the PR-review task

**Priority.** The rule sits at **priority 91**:
- above rule 10 (90, the WEB/API/OPS body_regex);
- below J2 (92, sender `jira@treetopllc.jira.com`, which never matches GitHub mail);
- below rule 59 (95, Foundry mail);
- below rule 63 (99, Slack-only);
- below rules 1 and 2 (100, LHH/Avviato, where a Treetop PR naming an LHH key is reengine work).

Pre-check 0e proves no rule above 91 matches the Treetop PR corpus except by an LHH key.

**Rationale:**
- Rule 10 has no key_regex, so its key is the pattern's first group, the PREFIX. Today a PR titled
  "WEB-1234 …" logs onto the catch-all `WEB` bucket task, not onto the WEB-1234 ticket's task. That
  is where the 104 `task_log` decisions went: the mail effectively vanished.
- Reviewing a colleague's PR is its own work item. The Jira ticket is usually the colleague's
  feature.
- The key stays visible, because it is in the review task's title.
- His own PRs fall through to rule 10 unchanged (D2).

### D4. Later mail logs onto the task, closed or not, and this ticket adds no resurfacing

This is already true with no new code, and it becomes a pinned criterion. `taskForExternalRef` finds
the ref whatever the task's status, so a later mail on the PR:
- is a `task_log` onto the same task, never a second task (`external_refs` UNIQUE);
- lands in the closed task's history if the task is closed (it does not vanish);
- runs SWT-36's guarded reopen if the task was DISMISSED. **Amended 2026-09-14 (owner decision):**
  a PR state notice (merged, closed or reopened) NEVER reopens a review task. `decideMessage`'s
  found branch never sets the dismissal for a notice, so live, shadow and the dry run agree. The
  notice is logged and nothing else changes.

**What this ticket does NOT do: bring a plain-closed (Done) PR task back on new activity.** That is
**SWT-53 (chat-on-closed-task)**, specced in parallel at `/home/salvo/projects/personal/wt/chatclosed`;
its SPEC file did not exist when this was written.

The boundary, so the two designs do not overlap:
- This ticket leaves the `task_log` branch of `decideMessage` byte-identical for github refs,
  except for the D5 close call and the state-notice guard on SWT-36's reopen (above).
- SWT-53 owns any reopen or resurface on that branch, for every system.
- This ticket exports two pure predicates SWT-53 may call:
  - `github.PRStateNotice(body, n)`. SWT-53 should not resurface a task on the very notice that
    closed it, nor on a "Closed #N" notice.
  - `capture`'s `pr_review` flag.
- Neither ticket may add a `revive` for github: revive stays jira-only.
- Whichever merges second rebases onto the other, re-runs both integration suites, and takes the
  next free migration number (both may want 0034).

### D5. A merged or closed PR closes its review task (recommended default; OQ-2)

When a `pr_review` rule's `task_log` message is GitHub's merge or close notice for that PR:
- the notice's body opens with `Merged #N into <branch>.` or `Closed #N.`, where N is the ref's
  number (`github.PRStateNotice`, pure; exact shapes pinned by pre-check 0d);
- and the task is not `closed`.

**Live pass:** the log is appended first, then `task_close` runs through the executor as the pass's
`capture:{connector}` actor, with reason `PR #N merged on GitHub (message M)` or `… closed …`. It
writes no dismissal label, so it counts as Done.

**Refusals:**
- A refusal carrying `activeWorkRefusal` (`close.go:40`) is a non-fatal skip. The reason and the log
  line say so; the ticketstatus precedent.
- Any other error fails the pass, as `linkRuleRef` does.

**A notice that would CREATE** (no ref yet, so the first mail seen for a PR is its merge notice)
creates no task. It is `attributed`, reason `PR already merged/closed; no review task`: a review of
merged work is not work.

**Other modes:** shadow closes nothing and says `would close`.

**Amended 2026-09-14 (owner decision): `Reopened #N.` is a PR state notice too.**
- `github.PRStateNotice` answers a third state, `PRStateReopened`, and `PRStateEndsPR` is false
  for it.
- It never closes a task, and it never reopens a dismissed one: it is logged, nothing more.
- As the first mail seen for a PR, it creates the review task like any mail, because the PR is
  open.
- No state notice reopens a dismissed review task (D4).

**Why close rather than annotate:**
- The board is his to-do list, and a merged PR's review is moot.
- Annotating means opening the task to learn it merged.
- The close is reversible (`task_reopen`), audited, and makes no model call.

### D6. Bot PRs get review tasks (OQ-1 = b; the status block is the authority)

The seeded rule's `exclude_pr_authors` is EMPTY. A PR opened by `dependabot[bot]`, for example
"Bump sanitize-html …", is a colleague's PR (D1 row 4) and gets a review task. The exclusion
mechanism stays as data: a rule that lists `*[bot]` makes bot PRs fall through (D1 row 3), and the
reason reads `excluded author (*[bot])`. Flipping it is a rule change, not code. (The recommended
default this section first proposed, `*[bot]` seeded, was superseded by the owner's answer.)

### D7. Every treetopllc repo attributes to collaboratory

Treetop LLC is Collaboratory's org:
- rules 6/7 already put collaboratory-www and gonoble there;
- rule 10's Treetop keys and J2's `jira@treetopllc.jira.com` do too.

The one rule attributes all of it to `collaboratory`, with the repo name in every title so a stray
repo stands out.

**Consequence, stated:** issue and CI mail from treetopllc repos other than www/gonoble moves from
`unmatched` (triage/route residue) to collaboratory, attribution-only. If a repo turns out to be
HOC or LlamaSite work ("not my projects", SWT-40 O4), a higher-priority `thread_key_contains
<treetopllc/{repo}/` rule pointing at `bulk` re-points it. That is data, with no code.

### D8. New tasks are `ready`, not `holding`

`holding` is the review lane for MODEL-inferred work awaiting confirmation (SWT-30's promoter,
SWT-40 O7). A review task is created from GitHub's deterministic statement about a real PR, the same
footing as every capture-created Jira task, which `createRuleTask` already makes human `ready`,
priority 0.

Under SWT-52 it lights blue as the collaboratory human queue's head, else none/grey. Priority stays
0, and he reorders with `task_set_priority`. No lights code changes.

### D9. Existing PRs get tasks by a one-off hand backfill, not by code

Once the rule is seeded, only NEW messages act: historical messages have spent their live claim, and
`--all` is refused live. So a colleague's PR sitting idle, waiting for exactly his review, would get
no task.

The dry run (D10) lists every PR it would create a task for, with:
- its latest mail date;
- whether a merge or close notice was seen.

**Backfill scope and method.** For each PR that meets all three conditions, the operator runs the
three executor calls `createRuleTask` makes, by hand with `opsctl call`, recorded in the delivery
summary:
- the verdict is `other` or `undetermined`;
- no merge or close notice was seen;
- there has been mail in the last 30 days.

The three calls are `create_task`, `link_external_ref` with the canonical key and `PRURL`, and
`task_set_source_thread`. Each uses the same title `prReviewTitle` would produce. The dry run prints
those three argument payloads ready to paste. A general backfill verb is Future work.

### D10. The "shadow pass" is a write-nothing dry run, because prod capture is live

The rule cannot be seeded "in shadow" on prod: the live CronJobs act on it at the next tick.

A DB shadow pass (`CAPTURE_RULES_MODE` unset) run after seeding is worse:
- it writes newer shadow rows that every latest-decision reader follows;
- and it only happens after live has already acted.

So this ticket adds `opsctl capture-rules try`. It takes `capture-rules add`'s flags plus `--since`
and `--show all|wins`, and it:
- loads the enabled rules plus the candidate, in memory, with id `max(id)+1`, the id it would get;
- runs `decideMessage` (Evaluate, the D1 authorship reads, canonicalization, the D5 notice check)
  over every inbound message in the window, `--all` semantics;
- prints, per message, its CURRENT latest decision beside the candidate's proposed decision, then a
  per-PR rollup (key, verdict + evidence, title, would-create/log/close, last mail, notice seen) and
  the D9 backfill payloads.

**It writes NOTHING:** no `capture_decisions` row, no executor call, no lock. The same shape as
`capture-rules gate --dry-run`. It is shadow mode's safety property ("decides everything, creates
nothing") applied to a candidate rule. Because F8 makes a wrong rule permanent, it is how the
key_regex is proven in Go on the real corpus before insert.

## Acceptance criteria

**Migration and rule tool**

1. Migration 0035 (SWT-53 owns 0034; see the status block) adds to `capture_rules`:
   - `pr_review BOOLEAN NOT NULL DEFAULT false`;
   - `exclude_pr_authors TEXT[] NOT NULL DEFAULT '{}'`;
   - CHECK `NOT pr_review OR (external_system IS NOT NULL AND external_system = 'github' AND key_regex IS NOT
     NULL AND NOT revive)`;
   - CHECK `cardinality(exclude_pr_authors) = 0 OR pr_review`.

   **Amended 2026-09-14 (main session).** The CHECK first read `NOT pr_review OR (external_system = 'github'
   AND …)`. That lets `pr_review=true` with a NULL `external_system` through: `NULL = 'github'` is NULL, the
   AND is NULL, `false OR NULL` is NULL, and a CHECK passes on NULL. The CHECK now requires
   `external_system IS NOT NULL` explicitly, so it fails closed. `TestMigration0035_CaptureRulesPRReviewShape`'s
   regex was updated to match, and the migration integration test gained the NULL case.
2. `capture_rule_add` accepts `pr_review` and `exclude_pr_authors`, and refuses, naming the field:
   - `pr_review` without `external_system=github` or without `key_regex`;
   - `pr_review` with `revive`;
   - `exclude_pr_authors` without `pr_review`;
   - any entry that is not a GitHub login or `*`+suffix (`^\*?[A-Za-z0-9][A-Za-z0-9-]*(\[bot\])?$` or
     `^\*\[bot\]$`);
   - `url_template` with `external_system=github`.

   The tool stays `humanOnly` and off both MCP profiles.
3. `opsctl capture-rules add` gains `--pr-review` and a repeatable `--exclude-pr-author`.
   `capture-rules list` prints both on the rule's key line.

**Matching, keys and dedup**

4. `loadRules` selects both columns. A column-fed integration test goes RED when either is replaced
   by a literal in the SELECT (the "test the column" rule).
5. With the D2 rule, a message whose thread key ends `<treetopllc/{any-repo}/pull/{N}@github.com>`
   derives key `treetopllc/{repo}#{N}` and `external_url https://github.com/treetopllc/{repo}/pull/{N}`.
   `/issues/N` and other roots derive no key (attribution only). A `<foundry-underwriting/…>` or
   `<tower987124/…>` root does not match.
6. Every github-keyed decision stores the canonical `github.PRKey` spelling.
   `PGTaskResolver.Resolve` uses `github.PRKey`. A structure test fails on any other
   `"%s#%d"`-shaped PR key construction under `internal/`.

**Authorship and fall-through**

7. `decidePRAuthor` is pure: no pgx, no time, no env; pinned by a structure test in the gate.go
   precedent. It implements D1's table exactly, including:
   - case-folded logins;
   - `*`-suffix entries;
   - `author` outranking every other row;
   - `undetermined` when no evidence exists.
8. On the create branch of a `pr_review` rule, a verdict of `own` or `excluded` re-decides the
   message without that rule. The decision row:
   - records the fallen-to rule's action and project;
   - keeps `matched_rule_ids` from the full evaluation;
   - carries a reason that begins `rule R skipped: PR {key} authored by him ({evidence}); ` for `own`,
     and `rule R skipped: PR {key} excluded author ({entry}): {evidence}; ` for `excluded`
     (amended 2026-09-14: an excluded bot is not "him").

   Integration cases:
   - (a) a mail titled `WEB-12 …` on his own PR `task_log`s onto the rule-10 fixture's bucket task,
     exactly as without the new rule;
   - (b) a keyless mail on his own collaboratory-www PR is `attributed` by rule 6;
   - (c) his own PR on a third treetopllc repo is `unmatched`.
9. Authorship evidence is read from `raw_source_items.raw_json` headers of real stored rows. The
   integration test seeds IMAP envelopes whose `rfc822_b64` carries X-GitHub-* headers, and a
   mutation proves the read is live: flipping one stored `X-GitHub-Reason: author` to `comment` turns
   "no task" into "task created".
10. `undetermined` creates the task, and its decision reason contains `author undetermined`. `other`
    names the author login when known.

**The review task**

11. The created task is `assignee_type=human`, status `ready`, priority 0, project collaboratory.
    Its title is `prReviewTitle`, pinned for:
    - `Re: [treetopllc/collaboratory-www] Ranking widget (PR #3179)` gives
      `Review PR #3179 — collaboratory-www: Ranking widget`;
    - an empty subject;
    - a subject without the bracket;
    - a `(PR #N)` whose N differs, which is left in place.

    It carries `external_refs` (github, canonical key, PRURL) and `tasks.source_thread_id`, the
    SWT-20 provenance, all via the executor.
12. A Treetop PR mail from another author whose subject carries `WEB-1234` becomes the review task
    (priority 91 beats rule 10). One carrying `LHH-…` goes to reengine (rule 1, 100).

**Later mail, closed tasks, notices**

13. A second and third mail on the same PR each write a `task_log` onto the same task. When the task
    is `closed` (plain Done), the log is appended, the status stays `closed`, and no second task or
    ref appears. When the task is DISMISSED, SWT-36's guarded reopen fires exactly as for any other
    ref, EXCEPT on a PR state notice. **Amended 2026-09-14:** a merged, closed or reopened notice on a
    dismissed review task stays a `task_log`. The task stays `closed`, the dismissal's `reopened_at`
    stays NULL, no `task_reopen` runs, and `Reopened` is 0, in live, shadow and the dry run alike.
14. (D5, subject to OQ-2.) A `Merged #N into main.` or `Closed #N.` notice on an open review task
    appends the log, then closes the task through `task_close` as `capture:{connector}`. The audit
    row and the `status_changed` event name the reason.
    - An `activeWorkRefusal` is a counted skip, not a pass failure.
    - Shadow closes nothing.
    - A notice with a different N, or `#N` mentioned mid-body, does not close.
    - A notice that would create gives `attributed`, and no task.
    - (Amended 2026-09-14.) A `Reopened #N.` notice only logs. It never closes a task, and as the
      first mail seen it creates the task like any mail.
    - (Amended 2026-09-14.) An UNTRUSTED mail (D1 amendment) never creates, logs or closes through
      the PR rule. It falls through, and its reason says `untrusted GitHub mail`.

**Bots, counters, modes and the dry run**

15. (D6, subject to OQ-1.) A PR whose opening notification's `X-GitHub-Sender` is `dependabot[bot]`,
    under a rule listing `*[bot]`, falls through. `RulesStats` gains `PRAuthorSkipped` and `PRClosed`,
    and every `capture_rules:` counter line prints them, zeros included (`printCaptureRules` and
    opsctl). The `capture_gate:` line prints both as 0.
16. Shadow mode creates, links, logs and closes nothing for a `pr_review` rule, and still writes the
    decision rows with the same actions a live pass would.
17. `opsctl capture-rules try` writes nothing. The integration test asserts unchanged row counts of
    `capture_decisions`, `tasks`, `external_refs`, `task_events` and `audit_events` across a run, and
    a structure test bans INSERT, UPDATE, DELETE and `Execute(` in the dry-run file. Its per-PR
    rollup reports the D1 verdict and evidence, the title, and the D9 payloads.

**Unchanged behaviour**

18. `pendingMessages` still filters `direction='inbound'` (invariant 5). No outbound message can
    create or log a review task.
19. No change to `NormalizeRFC822`'s output: `body_text` byte-identical (IK "STANDING RULE"). The
    header reader is a separate function over the raw envelope.

## Data model changes

Migration **0035_capture_rules_pr_review.sql** (forward-only; SWT-53 owns 0034):

```sql
ALTER TABLE capture_rules
  ADD COLUMN pr_review          BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN exclude_pr_authors TEXT[]  NOT NULL DEFAULT '{}',
  ADD CONSTRAINT capture_rules_pr_review_github   -- amended 2026-09-14: fail-closed on NULL (criterion 1)
    CHECK (NOT pr_review OR (external_system IS NOT NULL AND external_system = 'github'
                             AND key_regex IS NOT NULL AND NOT revive)),
  ADD CONSTRAINT capture_rules_exclude_needs_pr_review
    CHECK (cardinality(exclude_pr_authors) = 0 OR pr_review);
```

No new tables; `tasks`, `external_refs` (`system='github'`, already legal) and `capture_decisions`
are reused as is. No `capture_decisions.action` change: fall-through writes the fallen-to rule's
action, and D5's close rides on an ordinary `task_log` row, the SWT-45 revive precedent. The
typed outcome is the task's `status_changed` event and audit row.

**LANDMINE (deploy order): 0035 BEFORE any image built from this branch.** `loadRules` selects the
new columns on every capture pass, so a new image on a pre-0035 db fails capture for every connector.

## API / MCP tool changes

- **`capture_rule_add`**: new optional args `pr_review` (bool) and `exclude_pr_authors` (string
  array). The criterion 2 validations apply. Executor path: validate → policy (`humanOnly`) → audit
  → handler, unchanged. Not MCP-listed.
- **No new tool.** Capture's existing executor calls are reused, as the configured
  `capture:{connector}` actor:
  - `create_task`, `link_external_ref`, `task_set_source_thread`, `task_append_log`;
  - `task_close` (D5). It is allowed for a non-MCP actor: `mcpHumanOnly` gates only
    MCP-prefixed non-humans, and the integration test uses the real-matrix executor
    (`queueMatrixExecutor` pattern) to prove it.
- **`opsctl capture-rules try`**: a CLI over `capture.DryRunRules`. It makes no executor call at
  all, because it writes nothing.

## MQTT topics

None. Capture already publishes `ops/pipeline/captured` when a pass commits ≥1 decision. That is
unchanged.

## Files likely to touch

- `migrations/0035_capture_rules_pr_review.sql`: new.
- `internal/connector/github/prref.go`: new, pure:
  - `ParsePRRef`, `PRKey`, `PRURL`, `PRRootMessageID`, `PRStateNotice`;
  - the header-name consts `X-GitHub-Reason`, `X-GitHub-Sender` and `X-GitHub-Recipient`;
  - `NotificationFacts(mail.Header)`.

  `store.go`: `Resolve` uses `PRKey`.
- `internal/connector/google/attachments.go` (or `rfc822.go`): `RawMailHeaders(raw
  json.RawMessage) (mail.Header, bool, error)`, reusing `walkEnvelope`'s decode. It returns
  `ok=false` for a gmail:-shaped row. No cycle: neither connector package imports `internal/capture`
  (checked).
- `internal/capture/prreview.go`: new, pure. `decidePRAuthor`, the verdict consts,
  `prReviewTitle`, and exclude-list matching.
- `internal/capture/rules_store.go`:
  - `storedRule.prReview` and `.excludePRAuthors`, loaded in `loadRules`;
  - in `decideMessage`: github canonicalization, the create-branch authorship check,
    fall-through, and D5 notice detection;
  - in `EvaluateRules`: the close call;
  - `createRuleTask`'s title and `linkRuleRef`'s URL for github;
  - `RulesStats`.
- `internal/capture/prreview_store.go`: new. `prReviewFacts`, the D1 reads: thread messages and the
  opening message by exact `external_message_id`, 100-row cap, headers only.
- `internal/capture/dryrun.go`: new. `DryRunRules`.
- `internal/tools/capturerules.go`: args, validation, INSERT columns.
- `cmd/opsctl/main.go`: add flags, list printing, the `try` subcommand, counter printing.
- `cmd/connectors/google/main.go`: `printCaptureRules`, the two new counters.
- Tests: `internal/connector/github/prref_test.go`, `internal/connector/google/rawheaders_test.go`,
  `internal/capture/prreview_test.go`, `prreview_structure_test.go`,
  `prreview_integration_test.go`, `dryrun_integration_test.go`, `internal/tools/capturerules_test.go`
  (extend).
- Docs: `docs/runbooks/capture-rules.md` (new "PR review rules" section: the seed command, the dry
  run, the backfill, rollback); `docs/runbooks/HANDOFF-kube-treetop-pr-review-tasks.md` (0035
  migrate Job, image roll to every capture workload); the IK entry at delivery.

## In scope

- The two columns, the tool and flag changes, and the authorship verdict with fall-through.
- Canonical github PR keys (including the `Resolve` refactor), the review title, and close on a
  merge or close notice (per OQ-2).
- Bot exclusion as data (per OQ-1).
- The write-nothing `try` dry run.
- The seed command and the one-off backfill procedure, in the runbook.

## Out of scope

- **SWT-53 chat-on-closed-task.** Resurfacing a plain-closed task on new activity, for github refs
  as for every other system. Also "Reopened #N" handling.
- **capture-rule-ticket-keys**, the rule-10 successor with mention revive (SWT-45 J16). This ticket
  does not replace or modify rule 10. See Coordination.
- **A GitHub connector / webhooks / `cmd/hooksd` deploy** (build-order step 9's GitHub half), and
  orchestrator R9–R11 on review tasks.
- **Posting reviews or comments to GitHub.** That is the `github_review` delivery channel, policy
  "approve".
- Foundry-Underwriting/* and tower987124/* PR tasks, and rule 59.
- A `capture_rule_update` tool; editing rules 6/7.
- Jira-ticket cross-linking from a review task.
- Dashboard or lights changes.

## Coordination

- **SWT-53.** See D4. The two tickets share the `task_log` branch of `decideMessage` and possibly
  the migration number. Whichever merges second owns the rebase and re-verification.
- **capture-rule-ticket-keys (J16 successor).** Mail from a colleague's Treetop PR no longer reaches
  any lower rule, so a PR naming `WEB-1234` will not revive WEB-1234's task through the mention rule.
  His own PRs' mail still falls through to it (D2). That successor's load check should measure GitHub
  mentions AFTER this rule, not before.
- **Rules 6/7 stay enabled.** They still attribute issue and CI mail and own-PR fall-through for
  their two repos. Without them, own-PR mail on collaboratory-www would go unmatched.

## Invariants that apply

1. **Raw-first.**
   - No connector change.
   - Authorship is re-derived from `raw_source_items.raw_json.rfc822_b64`, which is the reason the
     raw row exists: re-running the dry run after a rule change re-reads the same bytes.
   - `NormalizeRFC822` output is untouched (criterion 19).
2. **One funnel.** Review tasks are `tasks` rows keyed by `external_refs (github, …)`, and the
   board is a filter. The migration adds CONFIGURATION columns to `capture_rules`, not a table of
   things to act on.
3. **Everything through the executor.**
   - Every write goes through the executor: create, link, provenance, log and close, as
     `capture:{connector}`.
   - `capture_decisions` stays capture's own append-only log.
   - `capture_rule_add` stays humanOnly and off MCP.
   - The dry run writes nothing (criterion 17).
   - No raw SQL or raw API tool is added.
4. **Nothing external without a delivery row.** Nothing is sent. A review comment on GitHub would be
   a `github_review` delivery, which is out of scope.
5. **Own-message loop closure.**
   - `direction='inbound'` stays the pending filter (criterion 18).
   - The new exposure is the J17 analogue: mail ABOUT his own PR is inbound (from
     `notifications@github.com`), and D1 is what keeps it from becoming a task. His own PRs' mail
     falls through to today's decision and is never re-triaged into a review task.
6. **Stealth attribution.** Titles and log lines are internal. Nothing client-visible is produced.
7. **Orchestrator purity.**
   - The orchestrator is untouched.
   - The authorship verdict, the title and the notice predicate are pure functions of stored
     facts, with no LLM.
   - Every decision writes a `capture_decisions` row whose reason carries the evidence, and every
     close writes an audit row.

## Sibling patterns to copy

- **Store-backed guard feeding a pure verdict.** SWT-45's own-action guard: `ownActionFacts`
  (reads) → `decideOwnAction` (pure) → reason suffix (`rules_store.go:808-946`, `ownaction.go`).
  `prReviewFacts` / `decidePRAuthor` follow that exact split.
- **Follow-on executor call after `task_log`.** `reviveRuleTask` / `reopenRuleTask`
  (`rules_store.go:1228-1293`): log first, then the extra call. A crash between them leaves today's
  behaviour.
- **Non-fatal close refusal.** ticketstatus's match on `activeWorkRefusal` (`close.go:36-40`).
- **Write-nothing dry run.** `opsctl capture-rules gate --dry-run` (`decideGateHolds` shared by
  `RunGate` and `DryRunGate`, IK "The capture-time assignee gate").
- **Raw envelope decode outside the normalizer.** SWT-42 `ListAttachments` → `walkEnvelope`
  (`attachments.go:93-172`).
- **Rule-flag CHECK pairs and tool refusals.** Migration 0030 and `parseCaptureRuleAdd`'s J1 block
  (`capturerules.go:144-166`).

## Tests (for test-author)

- **Unit, no db:**
  - `prref_test.go`: parse and round-trip; issues and actions refused; `PRStateNotice`
    shapes (use 0d's real first lines).
  - `rawheaders_test.go`: IMAP, truncated, gmail:-shaped, undecodable.
  - `prreview_test.go`: D1 table row by row, the title cases, exclude matching.
  - `capturerules_test.go`: criterion 2's refusals.
  - An Evaluate case pinning the exact seeded pattern and key_regex strings against the thread
    keys: www/pull, gonoble/pull, other-repo/pull, www/issues, Foundry, tower.
- **Structure:**
  - `decidePRAuthor` and `prReviewTitle` bodies are pure;
  - one PR-key spelling;
  - the dry-run file has no writes;
  - `rules.go` still imports no connector package.
- **Integration** (compose db, `make integration`, join the cleanup pact; delete `capture:*`
  audit/policy rows by task_id before task cleanup, per IK TRAP 2). Criteria 4, 8–17. Every
  authorship case seeds a real `raw_source_items` IMAP envelope; the headers must never be supplied
  through a Go fixture struct. The mutation of criterion 9 is the proof that the fixture is not
  what is being tested.

## Verification protocol

**0. Blocking read-only pre-checks against prod**
(`psql -h 192.168.50.49 -U ops -d ops`, inside `BEGIN READ ONLY; … ROLLBACK;`). Paste each result
into the delivery summary. **NOT run by the spec session** (no shell).

Base set used by 0a–0d:

```sql
WITH gh AS (
  SELECT m.id, m.sent_at, m.subject, m.body_text, m.external_message_id, nt.thread_key,
         ri.source_account_id,
         convert_from(decode(ri.raw_json->>'rfc822_b64','base64'),'LATIN1') AS rfc
    FROM normalized_messages m
    JOIN normalized_threads nt ON nt.id = m.thread_id
    JOIN raw_source_items ri ON ri.id = m.raw_source_item_id
   WHERE nt.thread_key LIKE 'gmail:%:<treetopllc/%' AND m.direction = 'inbound'
     AND m.sent_at > now() - interval '90 days')
SELECT ... FROM gh ...;
```

- **0a. Reason coverage.** `(regexp_match(rfc, '^X-GitHub-Reason:[ \t]*(\S+)', 'n'))[1]`, counted,
  NULLs included.
  - If most mails show NULL, check `raw_json->>'truncated'` and `raw_json ? 'rfc822_b64'` before
    concluding. The brief's "many have no header" may be a normalized-column reading (IK: "list the
    columns before concluding the data is missing").
  - **Gate:** `author` or `review_requested` present on the PRs he knows he authored or was asked to
    review, or STOP.
- **0b. His login and the opening mails.**
  - Distinct `X-GitHub-Recipient` values: his login(s). Record them. Any second login goes into
    `--exclude-pr-author`.
  - For opening messages (`external_message_id = substring(thread_key from '<[^>]+>$')`): distinct
    `X-GitHub-Sender`, and the count where the sender equals the recipient. Expected 0 unless
    "Include your own updates" is on.
  - The count of PRs (distinct roots) with no opening message and no `author`/`review_requested`
    mail: the `undetermined` rate.
- **0c. Thread shape.** Count roots by kind (`/pull/`, `/issues/`, other). Also count mails whose
  subject contains `(PR #` but whose root is not `/pull/`. Expected 0; otherwise the key_regex misses
  them.
- **0d. Notice shapes.** The first non-empty `body_text` line of pull-thread mails matching
  `^(Merged|Closed|Reopened) #`. Record the exact strings; they become `PRStateNotice`'s test
  fixtures.
- **0e. Rule landscape.**
  - `SELECT id, priority, criteria_type, pattern, external_system FROM capture_rules WHERE enabled
    AND priority > 91 OR external_system = 'github';`
  - Expect no github rule. Rule 59's pattern must not be able to match a treetopllc root; the dry
    run's `matched_rule_ids` confirms it on the corpus.
- **0f. Accounts.** `count(DISTINCT source_account_id)` over the base set. Expected 1; more means
  the D1 thread-read residual is real. Record it.

**Step 0 results.** Run by the main session on 2026-09-14, read-only, 90 days, 264 mails. NO STOP condition was hit.
- **Base set.** All 264 mails have `rfc822_b64`; none is truncated.
- **0a.** Reasons: NULL 122, subscribed 55, push 32, security_alert 22, comment 14, mention 11,
  review_requested 7, state_change 1.
  - All 122 NULLs are non-pull roots whose From is `<name> <noreply@github.com>`, i.e. commit and push
    mail (the senders include "Salvador Spataro"). Every pull-thread mail (120) carries the X-GitHub
    headers.
  - **No `author` reason appears at all** in 90 days. Nothing on any PR he authored reached him,
    because he had no Treetop PR activity in the window. The `author` branch stays, as designed;
    record this as an unexercised path.
- **0b.** His login is **`sspataro57`**, the only X-GitHub-Recipient. No second login, so the
  exclude list starts EMPTY (OQ-1 means no `*[bot]` entry).
  - Opening-message senders: joseg-avviato 42, dependabot[bot] 6, ananthsekar007 4. sender =
    recipient: 0.
  - Undetermined PR roots: 2 of 55, `noble-go-sdk#486` (joseg-avviato, state_change) and
    `collaboratory-www#3186` (dependabot[bot], subscribed). Both are someone else's, so fail-open is
    correct for both.
- **0c.** 55 pull roots with 120 mails, and 125 other roots with 144 mails. PR-titled subjects
  outside a pull root: 0.
- **0d.** Only "Closed #N." lines, 3 of them (#3145, #3186, #3854). No "Merged #N into …" line
  arrived in 90 days, so `PRStateNotice` fixtures use GitHub's documented shape
  "Merged #N into <branch>." plus the observed "Closed #N.". Match on the first body line only:
  merged prose inside PR descriptions, such as "merged in #3202", must NOT match.
- **0e.** No github rule exists. The rules above 91 are 1 and 2 (jira LHH), 63 (an Avviato channel
  prefix) and 59 (Foundry). Rule 59 requires `[Foundry-Underwriting/`, so it cannot match a
  treetopllc root.
- **0f.** **2 accounts**, not 1. The D1 cross-account thread-read residual is real; keep it as a
  recorded residual.

**1. Local.**
- `go test ./...` and `make integration`, both green. Capture the exit status separately from any
  pipe (memory: gate commits on test exit status).
- `/ticket-review` with a codex pass: capture, close path.

**2. Deploy order** (kube session; `HANDOFF-kube-treetop-pr-review-tasks.md`):
1. 0035 via the migrate Job, checked with `SELECT version FROM schema_migrations WHERE version = '0035'`.
2. One image tag to EVERY capture workload: every connector CronJob, the google IMAP watcher, and
   pipelined.
3. **Barrier (procedural; accepted residual b).** The switchboard session verifies with
   `kubectl -n ops get cronjob,deploy -o wide` that EVERY capture workload shows the new image tag.
   Only then is the rule seeded.
4. `go install ./cmd/opsctl` here.

**Seed nothing before step 2 completes.** An old capture binary ignores `pr_review` and would take
the rule as a plain github rule. It would create path-spelled refs and tasks for his own PRs, with
ugly titles, and a later new binary would then create SECOND tasks under the canonical key.

**3. Shadow = dry run on prod (writes nothing):**

```bash
DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/opsctl capture-rules try --project collaboratory \
  --type thread_key_contains --pattern '<treetopllc/' --external-system github \
  --key-regex '<(treetopllc/[A-Za-z0-9._-]+/pull/[0-9]+)@github\.com>$' --priority 91 \
  --pr-review --since 720h --show wins | tee ~/swt54-try-pre-add.txt
```

(No `--exclude-pr-author`: OQ-1 = b, and 0b found no second login.) SAVE the output. Its "D9 backfill
payloads" section is the backfill source (step 5): once the rule is added, `try` with the same
flags refuses (Finding A).

Expect about 17 PRs. For each one, check the verdict against the PR's author on github.com:
- his PRs `own`, falling through to rule 10 or 6/7;
- colleagues' PRs `other` or `undetermined`;
- Dependabot `other` (OQ-1 = b);
- WEB-/API-titled PRs won by the candidate;
- no candidate loss except to an LHH key.

Any wrong verdict means STOP. F8 makes the rule permanent once added.

As a belt-and-braces check, a real shadow-mode pass runs in compose: criterion 16's integration
test.

**4. Seed** (same flags as step 3, `add` instead of `try`, minus `--since`/`--show`, plus
`--note "treetop-pr-review-tasks: one review task per treetopllc PR he did not author"`). Then
`opsctl capture-rules list` must show the flags.

**5. Backfill (D9)** from the payloads SAVED in step 3 (`~/swt54-try-pre-add.txt`), for open PRs with mail
in the last 30 days. Record the task ids. Do not rerun `try` after the add: it refuses.

**6. Watch.**
- The next google ticks' `capture_rules:` lines, for `pr_author_skipped` and `pr_closed`.
- `opsctl capture-rules report --since 24h`.
- `SELECT system, external_key, external_url, task_id FROM external_refs WHERE system='github';`
  All keys `treetopllc/…#N`, none path-shaped.
- The collaboratory board: titles, blue/grey lights.
- On the first merge notice, the task closes with the reason (per OQ-2).

**7. Rollback.** `opsctl call --tool capture_rule_set_enabled --args '{"rule_id":<id>,"enabled":false}'`.
PR mail returns to rules 10/6/7 on the next NEW message. Created tasks stay (real PRs); dismiss by
hand. 0035 stays (forward-only, and inert with no `pr_review` rule enabled). Never roll an image
back past this ticket while a pr_review rule is enabled: disable the rule first.

## Accepted residuals (2026-09-14, review fixes)

- **(a) Undetermined authorship stays fail-open, and this can occasionally create a task for his
  OWN PR.** This was a deliberate choice (D1): a wrong task costs one close, while a missed review
  is invisible.
  - Prod: 2 of 55 PRs in 90 days were undetermined, and both were someone else's. He had no Treetop
    PR activity in the window.
  - A false own-PR task appears when, for example, his first mail on his own PR arrives with reason
    `mention` (pinned by
    `TestCapturePRReview_Integration_AMentionOnlyMailOnHisOwnPRIsUndeterminedAndCreates` and a D1
    table row), or when the `author` mail went to the other receiving account.
  - **A Dismiss does not stick.** SWT-36 reopens a dismissed task on the PR's next trusted mail that is not a merged, closed or reopened notice.
    Closing it as Done (`task_close`) is the fix that sticks.
- **(b) The mixed-version barrier is procedural.** An old capture binary would take the pr_review
  rule as a plain github rule. The switchboard session seeds the rule ONLY after verifying with
  `kubectl -n ops get cronjob,deploy -o wide` that every capture workload runs the new image
  (HANDOFF step 3). A later image rollback past this ticket must disable the rule first.
- **(c) A transient `link_external_ref` failure after `create_task` can yield a second task.** The
  pass fails loudly, but the created task has no ref, so a later notification creates another. This
  is the general capture claim/partial-write weakness, tracked in follow-up **SWT-50**.
- Two receiving accounts (0f): the thread-side read covers one of them.
- `X-GitHub-Reason: author` has never been observed on prod (0a).

## Decisions made unilaterally

D1–D5 and D7–D10 above, each with its rationale. OPEN_QUESTIONS holds only D5's close and D6's bot
default, which change what lands on his board.

## Future work

- `capture_rule_update`, to amend `exclude_pr_authors` without the disable-and-re-add-with-new-pattern
  dance (F8).
- A general `capture-rules backfill` verb (D9 is by hand).
- Cross-account PR thread reads (D1 residual), if 0f is ever > 1.
- Cross-linking a review task to the Jira ticket named in its title.
- If the GitHub connector is ever deployed on treetopllc repos: R9–R11 must skip human review tasks.
  Otherwise `pr_merged → done_locally` would chain R3's delivery task onto a review.
