> Jira: SWT-78

# slack-messages-not-becoming-tasks

swb task: #521 (priority high)

## Report (verbatim, Salvador, 2026-09-23)

> there are a bunch of slacks from yesterday that didn't come in as taks. This has proven unreliable.

## Context at the time of the report

- "Yesterday" is 2026-09-22.
- The Slack watcher (`connector-slackweb-watch`) was on image 0.7.44 all day; the rest of the ops workloads
  went from 0.7.45 to 0.7.46 at about 00:27Z on 2026-09-23 (SWT-77 / #513 roll). The watcher was held on its
  own tag by another session pending delivery 58.
- The same day saw SWT-75 (per-minute watch sweep of José/Katie) and SWT-76 (send queue) go live, and the
  kube session was holding a watcher bump. The Slack browser on the mini was repeatedly busy ("retry no
  sooner than ~532s").
- Not named in the report: which conversations, which workspace, how many messages.

## Expected behaviour (Salvador, 2026-09-23, verbatim follow-ups)

> did qwen drop them?

> basically all DMs to me are actionable just messages on the general forum need decision

So the expected outcome is: every Slack DM to Salvador from someone else becomes a task (or attaches to one)
with no model gate; only messages in shared channels (e.g. #general) go through a classifier decision.

## Decisions (Salvador, 2026-09-23)

> and backfill the ones from yesterday as tasks

> and fix it so DMs skip qwen

Asked: every DM its own task, or one open task per DM conversation that later messages attach to?

> one task per conversation, go ahead

So: a DM (or group DM) to Salvador from a person gets no classifier call. If the conversation already has
an OPEN task, the message attaches to it; otherwise it creates one. Channel messages keep the classifier.
Backfill 2026-09-22's missed DMs the same way.

> ok keep going. Make qwen say yes when unsure. That will create a task and claude would decide with a smarter model

So, in addition: the inquiry classifier's prompt flips its tie-break from "no reply needed when unsure" to
"needs a reply when unsure" (channel messages, and anything else still on the inquiry lane).
