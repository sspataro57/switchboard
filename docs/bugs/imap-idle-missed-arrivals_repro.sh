#!/usr/bin/env bash
# Reproduction — imap-idle-missed-arrivals (SWT-81)
#
# BUG: with connector-google-watch (IMAP IDLE on INBOX + 10-minute reconcile)
# running, some INBOX arrivals get no IDLE wake at all and wait for the next
# reconcile sweep.
#
# WHAT THIS DOES (read-only, SELECT only, against the production ops db):
#   For every INBOX message whose server receipt time (raw_json.internaldate) is
#   inside the window, find the sync_runs 'imap' row that first stored it (the
#   latest run for that account with raw_inserted>0 that started at or before
#   normalized_messages.created_at, the first-stored time), and classify it:
#     idle-wake                    stored by a single-account pass (a wake)
#     idle-wake(late>90s)          same, but the pass started >90 s after receipt
#     reconcile(raced;wake fired)  stored by a sweep, but a wake for that account
#                                  also started within 90 s of receipt (IDLE fired,
#                                  the sweep got there first)
#     reconcile(NO wake)           stored by a sweep and NO wake for that account
#                                  started within 90 s of receipt   <-- THE BUG
#     startup                      received <90 s before a watcher's first sweep
#                                  after a pod start (IDLE was not listening yet);
#                                  not counted. Older arrivals stored by that sweep
#                                  were missed by the PREVIOUS pod and do count.
#     other                        no storing run found
#   A "sweep" is a contiguous group of runs (gap <5 s) covering >=3 accounts
#   (first run per account); every other run is a wake. Cron sweeps start on
#   even hours at :00. sync_runs single-account rows match the pod's
#   "watch: wake <account>" log lines 1:1 (verified 2026-09-23 for the 13:14Z
#   pod); older pods' logs are gone, so this uses sync_runs as the durable log.
#
# FAILS (exit 1) when any message in the window is reconcile(NO wake).
# Passes (exit 0) once every INBOX arrival is caught by an IDLE wake.
#
# RUN:
#   docs/bugs/imap-idle-missed-arrivals_repro.sh                       # all accounts, since the watcher went live
#   ACCOUNT=salvador@handsonconnect.org docs/bugs/imap-idle-missed-arrivals_repro.sh
#   SINCE='2026-09-23 00:00Z' UNTIL='2026-09-23 13:14Z' docs/bugs/imap-idle-missed-arrivals_repro.sh
#
# Needs ~/.pgpass for ops@192.168.50.49 (see INSTITUTIONAL_KNOWLEDGE, Environment facts).
# Caveat: internaldate is Gmail's receipt time, not the time the message got
# its INBOX UID; a message labelled into INBOX later shows an inflated delay.
# The late_uid column flags those (UID higher than a later-received message's).

SINCE="${SINCE:-2026-09-22 20:56Z}"   # connector-google-watch first rolled 2026-09-22T20:55:09Z
UNTIL="${UNTIL:-now}"
ACCOUNT="${ACCOUNT:-%}"
PGURL="${PGURL:-host=192.168.50.49 user=ops dbname=ops}"

read -r -d '' CLASSIFY <<'EOF'
with runs as (
  select s.id, s.source_account_id acct, s.started_at, s.finished_at,
         coalesce((s.stats->>'raw_inserted')::int, 0) ins
    from sync_runs s join source_accounts a on a.id = s.source_account_id
   where a.provider = 'google' and s.stats->>'phase' = 'imap'
     and s.started_at > timestamptz :'since' - interval '15 min'),
l  as (select *, lag(finished_at) over (order by started_at) pfin from runs),
g  as (select *, sum(case when pfin is null or started_at > pfin + interval '5 s' then 1 else 0 end)
                   over (order by started_at) grp from l),
g2 as (select *, row_number() over (partition by grp, acct order by started_at) occ from g),
gnd as (select grp, count(distinct acct) nd, min(started_at) gst from g group by grp),
sw as (select grp, gst,
              (extract(minute from gst at time zone 'UTC') = 0
               and extract(hour from gst at time zone 'UTC')::int % 2 = 0
               and extract(second from gst) < 30) is_cron
         from gnd where nd >= 3),
sw2 as (select s.*, (select max(p.gst) from sw p where not p.is_cron and p.gst < s.gst) prev_watch
          from sw s),
swk as (select grp, case when is_cron then 'cron'
                         when prev_watch is null
                           or extract(epoch from gst - prev_watch) not between 590 and 610 then 'startup'
                         else 'sweep' end kind
          from sw2),
cls as (select g2.id, g2.acct, g2.started_at, g2.ins,
               case when swk.kind is not null and g2.occ = 1 then swk.kind else 'wake' end kind
          from g2 left join swk using (grp)),
msgs as (
  select a.account_email acct_email, r.source_account_id acct, (r.raw_json->>'uid')::bigint uid,
         (r.raw_json->>'internaldate')::timestamptz idate, n.created_at stored,
         (r.raw_json->>'size')::bigint size
    from raw_source_items r
    join normalized_messages n on n.raw_source_item_id = r.id
    join source_accounts a on a.id = r.source_account_id
   where a.provider = 'google' and r.raw_json->>'folder' = 'INBOX'
     and a.account_email like :'account'
     and (r.raw_json->>'internaldate')::timestamptz > timestamptz :'since'
     and (r.raw_json->>'internaldate')::timestamptz <= timestamptz :'until'),
m2 as (
  select m.*,
    (select c.id from cls c where c.acct = m.acct and c.ins > 0 and c.started_at <= m.stored
      order by c.started_at desc limit 1) run_id,
    (select min(c.started_at) from cls c where c.acct = m.acct and c.kind = 'wake'
      and c.started_at between m.idate - interval '5 s' and m.idate + interval '90 s') wake_after,
    exists (select 1 from msgs o where o.acct = m.acct and o.idate > m.idate and o.uid < m.uid) late_uid
  from msgs m)
select m2.acct_email, m2.uid, m2.idate, m2.stored,
       round(extract(epoch from m2.stored - m2.idate))::int delay_s,
       c.kind stored_by, c.id run_id, c.started_at run_start,
       round(extract(epoch from m2.wake_after - m2.idate))::int wake_s,
       case when c.kind = 'wake' and m2.wake_after is null then 'idle-wake(late>90s)'
            when c.kind = 'wake' then 'idle-wake'
            when c.kind = 'startup' and m2.idate > c.started_at - interval '90 s' then 'startup'
            when c.kind in ('sweep','cron','startup') and m2.wake_after is not null then 'reconcile(raced;wake fired)'
            when c.kind in ('sweep','cron','startup') then 'reconcile(NO wake)'
            else 'other' end class,
       m2.size, m2.late_uid
  from m2 left join cls c on c.id = m2.run_id
EOF

read -r -d '' SQL <<EOF
\\echo '== per message (UTC) =='
with c0 as ($CLASSIFY)
select acct_email, uid, to_char(idate at time zone 'UTC','MM-DD HH24:MI:SS') received,
       to_char(stored at time zone 'UTC','HH24:MI:SS') stored, delay_s, stored_by, run_id,
       coalesce('+'||wake_s||'s','-') wake_for_acct, class, size, late_uid
  from c0 order by acct_email, idate;

\\echo '== per account =='
with c0 as ($CLASSIFY)
select acct_email, count(*) n,
       count(*) filter (where class like 'idle-wake%') idle,
       count(*) filter (where class = 'reconcile(raced;wake fired)') raced,
       count(*) filter (where class = 'reconcile(NO wake)') missed,
       count(*) filter (where class in ('startup','other')) excluded,
       round(100.0 * count(*) filter (where class like 'idle-wake%')
             / nullif(count(*) filter (where class not in ('startup','other')), 0)) idle_pct,
       percentile_cont(0.5) within group (order by delay_s)::int med_s, max(delay_s) worst_s
  from c0 group by acct_email order by acct_email;

\\t on
with c0 as ($CLASSIFY)
select 'REPRO_MISSED=' || count(*) from c0 where class = 'reconcile(NO wake)';
EOF

out=$(psql "$PGURL" -X -q -v ON_ERROR_STOP=1 \
     -v since="$SINCE" -v until="$UNTIL" -v account="$ACCOUNT" <<<"$SQL") || { echo "psql error" >&2; exit 2; }
printf '%s\n' "$out" | grep -v 'REPRO_MISSED='
missed=$(printf '%s\n' "$out" | sed -n 's/.*REPRO_MISSED=\([0-9]*\).*/\1/p')
[ -n "$missed" ] || { echo "no REPRO_MISSED marker in output" >&2; exit 2; }
if [ "$missed" -gt 0 ]; then
  echo "REPRO FAILS: $missed INBOX arrival(s) got no IDLE wake and waited for a reconcile sweep"
  exit 1
fi
echo "REPRO PASSES: every INBOX arrival in the window was caught by an IDLE wake (or raced one)"
exit 0
