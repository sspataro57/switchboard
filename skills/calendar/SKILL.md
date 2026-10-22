---
name: calendar
description: Read and book Salvador's calendar from any session — what's on today or this week, when he is free, whether a time conflicts, free slots to offer someone, and booking or cancelling time. Use whenever a question or a draft depends on his schedule or availability, or when he asks directly as `/calendar [days]`. Live data through switchboard's MCP tools; never the old .ics files.
---

# calendar — Salvador's live calendar, through switchboard

Every answer comes from switchboard's `ops` MCP tools (the user-scope
`ops-mcp-user` install, present in every repo's session). They read the
calendars switchboard syncs from Google (sspataro@gmail.com and
salvador@handsonconnect.org), plus switchboard's own reservations, in his time
zone (America/New_York).

**There is no fallback.** The old `~/.claude/calendar-agenda.py` and the
`.ics` files under `~/.local/share/calendar/` are a snapshot from 2026-08-03
and are retired for every answer: a session offered a meeting window over a
live standup from that snapshot on 2026-10-21. If the tools refuse, say so;
never answer from another source.

## Reading

| Question | Call |
|---|---|
| what's on today / this week / `/calendar [days]` | `calendar_list_events` (default: today for 7 days; `window_start`/`window_end` as `YYYY-MM-DD` or RFC3339, at most 31 days) |
| am I free Thursday 2–3? | `calendar_list_events` over that span, and look for busy events |
| times to offer someone | `propose_slots {duration_minutes, window_start?, window_end?, count?}` |

`calendar_list_events` **refuses** when any calendar's last sync is older than
its `max_sync_age` (24h). That is deliberate: say "the calendar copy is stale
(last synced …)", never guess. Each response lists `accounts` with their last
sync time; quote it when an answer drives a commitment.

Events with a `reservation` object are switchboard's own: `state` is `pending`
(saved in switchboard, not on Google yet — `pending_reason` says why),
`pushed`, or `confirmed`. All of them count as busy.

## Offering times to someone

1. `propose_slots` for the duration and range.
2. Draft the email with `draft_delivery` (gmail) and pass the windows you offer
   as `offered_windows` (1–3, each at most 4h). Switchboard re-checks them
   before sending and, once Salvador sends the email, holds each window on his
   calendar so nothing else lands there.
3. When the person picks one: `release_offer_holds {delivery_id: <the offer
   email's delivery>, keep_start: <picked window start>}`. If they decline:
   `release_offer_holds` without `keep_start`. Unpicked holds expire after 72h.

## Booking time

`book_time {task_id, start, end, label}` saves a booking in switchboard at
once; from that moment every free/busy answer treats it as busy. It is pushed
to Google in the background, to the calendar the task's project dictates (you
cannot choose; Avviato projects go to salvador@handsonconnect.org, everything
else to sspataro@gmail.com). Google only ever sees "Busy" — the label stays in
switchboard.

Report a booking as **"saved, not yet on Google (reason)"** until
`calendar_list_events` shows it `pushed` or `confirmed`. A booking for a
calendar that cannot be written yet waits there, busy in switchboard; it never
moves to another calendar.

`cancel_reservation {delivery_id}` removes a booking or a single hold.

Book, cancel and release only when Salvador asks in this conversation.
