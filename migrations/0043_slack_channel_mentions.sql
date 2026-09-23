-- 0043 slack channel mentions (SWT-79, slack-channel-mentions).
--
-- Salvador, 2026-09-23: "channels is only when they mention me" / "we only
-- respond to mentions on those channels". A Slack CHANNEL message reaches the
-- inquiry lane (qwen) only when its text @-mentions him. Capture records that
-- fact on its own decision row, where it already tells a DM from a channel:
-- channel_unmentioned = true on an `attributed` Slack channel decision whose
-- text does not mention him, and both inquiry inboxes skip such rows.
--
-- Polarity and default follow 0034: every writer that does not name the column
-- (the gate and route stages, rows written before this ships, an old binary)
-- records false, "eligible, as today". So this migration changes no behaviour
-- by itself, and an image rollback stays safe. The CHECK pins the fact to the
-- one action it can mean.
--
-- It also creates the #a-millon project (the 0016/0018 precedent: no tool
-- creates projects). Salvador: "a-million is generally not collaboratory". Every
-- column is named so it is reviewed; ai_locality is 'any' EXPLICITLY (the
-- local_only default would make the drafts lane skip it), client stays NULL so
-- no worker console claims it, and inquiry_promote_after stays NULL — it is
-- armed by hand at go-live, before the capture rule is re-pointed (SPEC D7/D8).
-- `bulk` is not touched.
--
-- Rollout ORDER: apply BEFORE any image built from this branch — a new capture
-- binary writes the column and both inquiry inboxes select it. Idempotent.
ALTER TABLE capture_decisions ADD COLUMN IF NOT EXISTS channel_unmentioned BOOLEAN NOT NULL DEFAULT false;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint
                  WHERE conname = 'capture_decisions_channel_unmentioned_is_attributed') THEN
    ALTER TABLE capture_decisions ADD CONSTRAINT capture_decisions_channel_unmentioned_is_attributed
      CHECK (NOT channel_unmentioned OR action = 'attributed');
  END IF;
END $$;

-- Each capture pass re-checks messages re-ingested after their decision (a
-- Slack edit adds or removes the mention; recheckEditedMentions). It starts
-- from raw_source_items.ingested_at, which nothing indexed before.
CREATE INDEX IF NOT EXISTS raw_source_items_ingested_at_idx ON raw_source_items (ingested_at);

INSERT INTO projects (name, slug, client, execution, delivery, ai_locality, ai_classify, ai_inquiry,
                      notifier_senders)
VALUES ('#a-millon (Avviato general)', 'a-millon', NULL, 'manual', 'dashboard', 'any', false, true, '{}')
ON CONFLICT (slug) DO NOTHING;
