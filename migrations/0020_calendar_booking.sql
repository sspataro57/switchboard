-- 0020 calendar booking (SWT-28).
--
-- source_accounts.calendar_write_enabled is the per-account go-live gate for
-- the calendar delivery channel, flipped by hand (send_enabled's convention).
-- Under the auto tier (SWT-28 Q1 = b) this column is the ONLY per-account
-- consent an unattended booking has, and the send path re-checks it at SEND
-- time, not only at draft (a draft can predate a revocation).
ALTER TABLE source_accounts ADD COLUMN calendar_write_enabled BOOLEAN NOT NULL DEFAULT false;

-- deliveries.starts_at / ends_at carry the booked interval for calendar rows.
ALTER TABLE deliveries ADD COLUMN starts_at TIMESTAMPTZ, ADD COLUMN ends_at TIMESTAMPTZ;

-- The CHECK is the 0019 pattern and for the same reason: a calendar row
-- missing its interval would not error — it would silently become a delivery
-- nothing can book or confirm. Free at zero rows (the channel has never been
-- live; verify `SELECT count(*) FROM deliveries WHERE channel='calendar'` = 0
-- before applying), impossible to add later without a backfill.
ALTER TABLE deliveries ADD CONSTRAINT deliveries_calendar_identity_check
  CHECK (channel <> 'calendar' OR (starts_at IS NOT NULL AND ends_at IS NOT NULL
         AND ends_at > starts_at AND target_ref IS NOT NULL AND from_account_id IS NOT NULL));
