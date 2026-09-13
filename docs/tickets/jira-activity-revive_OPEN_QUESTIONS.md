> Jira: SWT-45

# jira-activity-revive — open questions

One question. It does not block the code, the tests or delivery: the SPEC's code is the same
either way. The answer decides one flag on a rule this ticket does not add.

## Q1 — Does a Slack or GitHub mention of a ticket key count as "Jira activity"?

**What happens today.** Rule 10 catches any message that mentions WEB-n, API-n or OPS-n, whether
it comes from Slack, GitHub mail or other email. It files each one as a log line on one of three
catch-all tasks (56, 57, 60).

**What this ticket changes.** Treetop's own Jira emails ("Katie Evans mentioned you on API-4104",
"(API-4103) …") now go to the real ticket's task. They bring it back if it is closed and create
it if it is missing. The connector copy of a comment is left alone, because the email already
covers it.

**What is still open.** Suppose a colleague writes "can you look at API-4104?" in Treetop Slack,
or a GitHub email names WEB-10442. Should that also bring back a closed task, or create one?

- **(a) No.** Only Jira's own notification emails count. Mentions keep today's behaviour until
  capture-rule-ticket-keys ships. That ticket then picks its Q2 option (i), attribution only, or
  (iii), log onto an existing task and never create.
- **(b) Yes.** Mentions count too. capture-rule-ticket-keys' replacement for rule 10 carries
  `--revive`, and every newly mentioned Treetop ticket gets a task.
  - Volume: 654 distinct keys across 3,235 messages all-time, though only NEW messages act.
  - Those tasks stay on the board even when the ticket is done, until you close them by hand.

(a) keeps the board to what Jira itself says involves you. (b) catches work that is only ever
discussed in Slack, at the cost of the board partly mirroring Slack.

Reengine is unaffected in practice. Under your refined decision 3, a non-addressed LHH mention
still goes through the assignee gate whatever you answer here.

Answer by editing the entries. Say 'questions answered' and I'll fold them into the SPEC.
