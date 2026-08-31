-- 0017: link storage for SWT-25 (link-preservation). Forward-only.
--
-- Element shape: {"text": "...", "url": "..."} and the ARRAY POSITION IS THE
-- IDENTITY — no id, no ordinal column, and nothing may reorder the array after
-- it is written. The classifier's link_index is a 1-based position into this
-- value; the whole array is rewritten by the google normalizer's upsert on
-- every (re-)normalize. Precedent: normalized_events.attendees (0001).
--
-- DEFAULT '[]' is what keeps the upwork / jira / slackweb inserts — which name
-- no links column — working. The CHECK is what makes "an array" structural: a
-- position into an object means nothing.

ALTER TABLE normalized_messages
  ADD COLUMN links JSONB NOT NULL DEFAULT '[]'::jsonb;

ALTER TABLE normalized_messages
  ADD CONSTRAINT normalized_messages_links_is_array
  CHECK (jsonb_typeof(links) = 'array');
