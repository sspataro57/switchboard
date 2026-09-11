> Jira: SWT-41

# orchestrator-deploy — open questions

Four decisions. None of them changes the code. Q1–Q3 decide what happens on switch-on day, and Q4
decides one sentence of the written plan for later.

---

## Q1 — The July test tasks: close them before switching on?

Tasks #4, #5 and #6 are finished test tasks from the July trial, and #8 ("Deliver #6") is the
follow-up task that trial created. None of them is real work. They do nothing by themselves, but
they will sit in the board and in the daily counts forever.

Closing them **before** the switch-on means the orchestrator never even sees those closes.

**Recommended:** yes, close all four before switching on.

Answer:

---

## Q2 — The July follow-up plan (#9 to #20): close all, or keep some?

These twelve tasks came from the July plan import ("switchboard follow-ups"). Nine of them are
waiting on others (#10–13, 15, 16, 18–20). This is the one place the orchestrator can surprise you:
if you close one of the "parent" tasks (#9, #14, #17) **after** switching on, the tasks waiting on
it wake up and become workable. A switchboard console, if one were running, could then pick them up.

Closing them **before** switching on avoids that entirely.

**Recommended:** close all twelve before switching on. Most of that list was overtaken by later
tickets. If any are still real, name them and they stay.

Answer:

---

## Q3 — Turn on the daily "Morning brief" task now?

When on, the orchestrator creates one task a day, titled "Morning brief" plus the date. It holds a
count of ready, waiting, parked and finished tasks per project, in a project you pick. Nothing
closes old briefs, so they pile up unless you close them. The dashboard's `/briefs` page lists them.

**Recommended:** leave it off for this deploy. Turn it on later with one setting once you know you
want it. If yes now: which project, and what hour (Eastern)?

Answer:

---

## Q4 — Later, when classify and promote stop being cron jobs: who wakes each step?

This does not change this ticket's code. It only fixes one sentence in the written plan for the
follow-up.

- **(a) Each step wakes the next.** When capture finishes, it announces "capture done" on MQTT, and
  classify (listening) runs. When classify finishes, it announces, and promote runs. The
  orchestrator keeps doing what it does today: managing tasks once they exist.
- **(b) The orchestrator wakes every step.** Every step reports to the orchestrator, and the
  orchestrator decides which step runs next. That makes the orchestrator responsible for mail and
  messages too, not just tasks.

In both cases the database stays the real to-do list, and a timer re-checks every 15 minutes in
case a message is missed.

**Recommended:** (a). It is fewer moving parts. A step that is down does not stall the
orchestrator's task work, and the orchestrator stays small enough to test without a database.

Answer:

---

Answer by editing the entries. Say 'questions answered' and I'll fold them into the SPEC.
