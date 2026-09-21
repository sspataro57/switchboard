-- 0038 deliveries.cc (SWT-69, gmail-delivery-cc).
--
-- The explicit carbon-copy recipients of a gmail delivery, as the drafter gave
-- them and the handler normalized them (address only, domain lower-cased,
-- deduped). Empty array = no Cc. Switchboard never adds one by itself: there is
-- no default, no per-project rule — the owner asked for the ability only.
--
-- TEXT[], like every other short list of strings in this schema (scopes,
-- notifier_senders, ticket_delivered_statuses, exclude_pr_authors); jsonb is
-- for provider payloads. NOT NULL DEFAULT '{}' gives "no Cc" exactly one
-- representation, so no reader needs a COALESCE. On Postgres 17 an ADD COLUMN
-- with a constant default rewrites nothing and needs no backfill.
--
-- Two CHECKs, both immutable expressions. Per-address syntax is NOT checked
-- here: net/mail in the handler is the one parser, and a SQL regex would be a
-- second spelling of what an address is.
--
-- Apply before rolling an image that reads the column. Idempotent.
ALTER TABLE deliveries ADD COLUMN IF NOT EXISTS cc TEXT[] NOT NULL DEFAULT '{}';

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'deliveries_cc_gmail_check') THEN
    -- A Cc exists on the gmail channel only; every other channel has no such thing.
    ALTER TABLE deliveries
      ADD CONSTRAINT deliveries_cc_gmail_check CHECK (channel = 'gmail' OR cc = '{}');
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'deliveries_cc_shape_check') THEN
    -- At most ten, and never an empty string standing in for an address.
    ALTER TABLE deliveries
      ADD CONSTRAINT deliveries_cc_shape_check CHECK (cardinality(cc) <= 10 AND NOT ('' = ANY(cc)));
  END IF;
END $$;
