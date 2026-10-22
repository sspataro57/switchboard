-- 0049 calendar reservations (SWT-109, calendar-mcp-tools).
--
-- Check prod is at 0048 first:
--   SELECT max(version) FROM schema_migrations;
--
-- A reservation is a deliveries row with channel='calendar' (no new table,
-- invariant 2). Booked in switchboard = 'approved'; pushed to Google = 'sent';
-- observed by a read poll = confirmed_at. Every column below is nullable or
-- defaulted, so existing rows are untouched.

-- Which Google calendar a project's reservations are pushed to (Q1, owner
-- 2026-10-21). NULL = the single calendar_default account.
ALTER TABLE projects ADD COLUMN calendar_account_id BIGINT REFERENCES source_accounts(id);

-- The default push calendar. At most one row may carry it.
ALTER TABLE source_accounts ADD COLUMN calendar_default BOOLEAN NOT NULL DEFAULT false;
CREATE UNIQUE INDEX source_accounts_calendar_default_uniq ON source_accounts ((true)) WHERE calendar_default;

ALTER TABLE deliveries
  ADD COLUMN offered_windows      JSONB,
  ADD COLUMN hold_for_delivery_id BIGINT REFERENCES deliveries(id),
  ADD COLUMN hold_expires_at      TIMESTAMPTZ,
  ADD COLUMN hold_kept_at         TIMESTAMPTZ,
  ADD COLUMN reservation_label    TEXT,
  ADD COLUMN booking_verified     BOOLEAN,
  ADD COLUMN push_pending_reason  TEXT,
  ADD COLUMN push_next_attempt_at TIMESTAMPTZ,
  ADD COLUMN push_attempts        INT NOT NULL DEFAULT 0,
  ADD COLUMN release_requested_at TIMESTAMPTZ,
  ADD COLUMN release_reason       TEXT,
  ADD COLUMN release_attempted_at TIMESTAMPTZ,
  ADD COLUMN released_at          TIMESTAMPTZ;

ALTER TABLE deliveries ADD CONSTRAINT deliveries_offered_windows_check
  CHECK (offered_windows IS NULL OR (channel = 'gmail' AND jsonb_typeof(offered_windows) = 'array'));

ALTER TABLE deliveries ADD CONSTRAINT deliveries_reservation_columns_check
  CHECK (channel = 'calendar' OR (hold_for_delivery_id IS NULL AND hold_expires_at IS NULL
         AND hold_kept_at IS NULL AND reservation_label IS NULL AND booking_verified IS NULL
         AND push_pending_reason IS NULL AND push_next_attempt_at IS NULL AND push_attempts = 0
         AND release_requested_at IS NULL AND release_reason IS NULL
         AND release_attempted_at IS NULL AND released_at IS NULL));

ALTER TABLE deliveries ADD CONSTRAINT deliveries_hold_columns_check
  CHECK (hold_for_delivery_id IS NOT NULL OR (hold_expires_at IS NULL AND hold_kept_at IS NULL));

ALTER TABLE deliveries ADD CONSTRAINT deliveries_push_pending_reason_check
  CHECK (push_pending_reason IS NULL OR push_pending_reason IN ('account_not_write_enabled',
         'target_mismatch', 'credit_budget', 'credit_cap', 'calendar_stale', 'transport_error', 'conflict'));

ALTER TABLE deliveries ADD CONSTRAINT deliveries_release_reason_check
  CHECK ((release_requested_at IS NULL) = (release_reason IS NULL)
         AND (release_reason IS NULL OR release_reason IN ('picked_other', 'not_needed', 'expired', 'cancelled')));

ALTER TABLE deliveries ADD CONSTRAINT deliveries_released_requires_request_check
  CHECK (released_at IS NULL OR release_requested_at IS NOT NULL);

-- The push inbox and the hold lookup.
CREATE INDEX deliveries_calendar_push_inbox_idx ON deliveries (push_next_attempt_at)
  WHERE channel = 'calendar' AND released_at IS NULL AND release_requested_at IS NULL;
-- One hold per offered window: the at-send insert and the stage's repair can
-- race, and a second set would put duplicate Holds on the calendar.
-- Released holds are excluded so a re-sent offer can hold the window again.
CREATE UNIQUE INDEX deliveries_hold_window_uniq ON deliveries (hold_for_delivery_id, starts_at)
  WHERE hold_for_delivery_id IS NOT NULL AND release_requested_at IS NULL;

-- Seed (criterion 33). Accounts are matched on provider AND email: the same
-- email exists under more than one provider. Verify after applying:
--   SELECT slug, calendar_account_id FROM projects ORDER BY slug;
-- exactly collaboratory, a-millon, reengine, llamasite-sec are set.
UPDATE source_accounts SET calendar_default = true
 WHERE provider = 'google' AND lower(account_email) = 'sspataro@gmail.com';

UPDATE projects SET calendar_account_id = (
         SELECT id FROM source_accounts
          WHERE provider = 'google' AND lower(account_email) = 'salvador@handsonconnect.org')
 WHERE slug IN ('collaboratory', 'a-millon', 'reengine', 'llamasite-sec');
