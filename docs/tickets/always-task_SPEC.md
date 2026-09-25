> Jira: SWT-93

# always-task: a capture rule can mark a sender always actionable

## Request

Salvador, 2026-09-25: "there is an email from pines property management those should always pop in incoming as
personal" (swb 703).

## Why it doesn't today

Rule 19 (`sender pinespropertymanagement.com` → personal) is keyless, so capture only attributes the mail. After
that, the personal lane's local classifier decides whether it becomes a task. It judged each Pines notice
"informational" (messages 491849 and 522864 on 09-24; 590311 on 09-25 got no verdict yet), so no task was made. An
owner-declared "always actionable" class must be decided in capture, before any model. That's the SWT-78 rule for
Slack DMs; see the IK entry "A Slack DM is not a channel".

## Design

- **D1. A per-rule flag.** `capture_rules.always_task` (default false). It applies to keyless rules only: a keyed rule
  already makes its key's task. The CHECK is `capture_rules_always_task_keyless`.
- **D2. Reuse the SWT-78 direct path.** At capture's attribution-only exit, `directConversationTask` returns true for
  an always_task rule's message, unless the sender is blank or on the project's notifier list.
  - `decideDirect` / `actDirect` then log onto the thread's open human task with no external ref, or create one.
  - The same path records provenance and makes the activity mark that puts the task in INCOMING.
  - One task per thread: for a non-Slack message the conversation key is the thread key, used whole (the Upwork
    precedent).
  - A closed or dismissed task means the next message makes a new one.
- **D3. Body marker.** A non-DM task's body opens with `alwaysTaskBodyMarker`. The SWT-78 marker names Slack DMs. The
  conversation lookup and the backfill's orphan recovery read the kind's own marker.
- **D4. Arming is data.** Use `capture_rule_add --always-task` / `opsctl capture-rules add --always-task`.
  - Rules cannot be edited, so Pines gets a new rule: sender `@pinespropertymanagement.com`, personal, priority 6,
    always_task.
  - Rule 19 is then disabled with `capture_rule_set_enabled`.
  - Today's message (590311) already carries a live `attributed` decision, which is forever. It is put on a task with
    `opsctl capture-rules direct-backfill --message 590311` (DirectBackfill runs the same decision).
    Disable rule 19 first, so the new rule wins. The message has no classify verdict yet, so it may still get one and
    log a duplicate verdict line onto the task (the SWT-78 backfill trade-off).
- **D5. Not covered.** `DryRunRules` does not simulate the flag, so `opsctl capture-rules try --always-task` is
  refused by name. `capture-rules list` shows the flag.
- **D6. Never a Slack channel.** An always_task rule's Slack channel message stays attributed: Slack keeps the
  SWT-78/79 policy (DMs are tasks, channel messages go through the mention gate and the inquiry lane). A Slack DM on
  such a rule takes SWT-78's path, as before.

## Data model changes

- `migrations/0045_capture_rule_always_task.sql`: `capture_rules.always_task BOOLEAN NOT NULL DEFAULT false` plus
  CHECK `capture_rules_always_task_keyless (NOT always_task OR external_system IS NULL)`.
- **Rollout barrier:** apply it before the image; loadRules selects the column.

## Invariants

- 1: no raw change.
- 2: one task per thread, in the one tasks table.
- 3: every write is a tool call (`create_task`, `task_set_source_thread`, `task_append_log`, `task_mark_activity`).
- 5: inbound only.
- 7: `directConversationTask` stays pure; the flag arrives as a value.

## Tests

- **Pure:** `directConversationTask` table cases for always_task (mail yes; blank sender or notifier no; a Slack DM
  keeps its SWT-78 reason).
- **Integration** (internal/capture):
  - a mail message on an always_task rule creates a human task in the rule's project, marked in INCOMING, with the
    always-task marker;
  - a second message on the same thread attaches;
  - an unflagged keyless rule stays attribution only.
- **Tool:** always_task with an external_system is refused.
