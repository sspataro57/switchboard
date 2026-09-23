#!/usr/bin/env bash
# Reproduction — slack-messages-not-becoming-tasks (Jira SWT-78, swb #521)
#
# Prints the per-message Slack funnel for one EDT day against PRODUCTION (read-only
# SELECTs) and exits 1 when any inbound Slack message from that day failed the
# expectation "every DM becomes a task or an attachment; a channel message at least
# gets a classifier verdict". Exits 0 once the bug is fixed.
#
# Usage: docs/bugs/slack-messages-not-becoming-tasks_repro.sh [YYYY-MM-DD]   (default 2026-09-22)
# Needs: psql with ~/.pgpass for 192.168.50.49 ops/ops.
day="${1:-2026-09-22}"
here="$(cd "$(dirname "$0")" && pwd)"
out="$(psql -h 192.168.50.49 -U ops -d ops -X -q -v day="$day" -f "$here/slack-messages-not-becoming-tasks_repro.sql" 2>&1)"
rc=$?
printf '%s\n' "$out"
if [ $rc -ne 0 ]; then echo "ERROR: psql exited $rc"; exit 2; fi
fails=$(printf '%s\n' "$out" | grep -c '^ FAIL ')
dmfails=$(printf '%s\n' "$out" | grep '^ FAIL ' | grep -c '| dm ')
total=$(printf '%s\n' "$out" | grep -cE '^ (PASS|FAIL) ')
echo
if [ "$fails" -gt 0 ]; then
  echo "FAIL: $fails of $total inbound Slack messages on $day have no task/attachment outcome ($dmfails of them in DMs)"
  exit 1
fi
echo "PASS: all $total inbound Slack messages on $day became a task/attachment (or, in channels, got a classifier verdict)"
