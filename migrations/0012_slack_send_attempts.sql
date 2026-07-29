-- 0012_slack_send_attempts.sql — SWT-12 review findings.
-- docs/tickets/slack-send-promotion_SPEC.md, amended after adversarial review.
--
-- Three defects shared one root cause: status='sending' carried no information
-- about the attempt that put it there.
--
-- 1. 'sending' meant BOTH "the bridge call is executing right now" and
--    "the call returned an ambiguous outcome". mark_delivery_failed could not
--    tell them apart, so a human could mark an in-flight send failed,
--    re-approve, and start a second click while the first was still running —
--    two client-visible posts, the exact thing invariant 4 exists to prevent.
--
-- 2. The hourly rate limit counted only status='sent'. A send whose click
--    landed but whose response was ambiguous stays 'sending' with sent_at NULL,
--    so it consumed no allowance: under degraded bridge responses real Slack
--    traffic could exceed the configured limit indefinitely.
--
-- 3. The export's prefix matcher had no lower time bound, so a replay (--all)
--    could bind a months-old message to a newly stuck row and promote a send
--    that never happened.
--
-- send_attempted_at is the dispatch instant and send_settled_at the instant the
-- bridge call returned, whatever the outcome. In-flight is therefore
-- (send_attempted_at IS NOT NULL AND send_settled_at IS NULL), which is a fact
-- about the attempt rather than an inference from status. Both are advisory
-- rather than authoritative: a crashed process leaves send_settled_at NULL
-- forever, so callers treat an attempt older than a lease as resolvable.

ALTER TABLE deliveries
  ADD COLUMN send_attempted_at TIMESTAMPTZ,
  ADD COLUMN send_settled_at   TIMESTAMPTZ;

-- Rows already sent get their attempt backfilled from sent_at so the rate-limit
-- window and the confirmation floor behave uniformly across the deployment
-- boundary.
UPDATE deliveries
   SET send_attempted_at = sent_at,
       send_settled_at   = sent_at
 WHERE sent_at IS NOT NULL
   AND send_attempted_at IS NULL;

-- 0011 backfilled approval_source only for ('sending','sent'), which left every
-- pre-existing 'approved' and 'failed' row NULL — yet those rows demonstrably
-- passed approve_delivery, because it is the only writer of an approvals row.
-- A NULL there is not "unknown authority", it is a lost fact, and the automated
-- send path now REQUIRES 'switchboard', so leaving it NULL would refuse a
-- legitimately approved delivery. Backfill from the evidence instead of from
-- current status.
UPDATE deliveries d
   SET approval_source = 'switchboard'
 WHERE d.approval_source IS NULL
   AND EXISTS (
     SELECT 1 FROM approvals a
      WHERE a.subject_type = 'delivery'
        AND a.subject_id = d.id
        AND a.status = 'approved'
   );

-- Counting sends per channel per hour now reads ('sent','sending') and needs the
-- attempt instant for rows with no sent_at.
CREATE INDEX deliveries_send_window_idx
  ON deliveries (channel, send_attempted_at)
  WHERE send_attempted_at IS NOT NULL;
