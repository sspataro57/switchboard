-- 0010: make a Calendar sync-token reset able to REPLACE prior state.
--
-- Google's 410 recovery contract says a reset means "your state is stale, take
-- this full snapshot instead". Before this column the connector only upserted
-- the events present in the replacement export and then saved the new token, so
-- an event deleted while the token was expired stayed active in
-- normalized_events forever: it is absent from the snapshot, and the fresh
-- token guarantees no future delta will ever mention it again. Availability
-- would keep treating that slot as busy with no way to repair it.
--
-- Raw-first (invariant 1) forbids deleting the observation, so absence is
-- recorded instead: the raw row is stamped superseded_at and normalization
-- skips it thereafter, including under --all (which would otherwise resurrect
-- the stale event on the next full replay).

ALTER TABLE raw_source_items ADD COLUMN superseded_at TIMESTAMPTZ;

-- No index: UNIQUE (source_account_id, external_id) from 0001 already serves
-- every lookup here, and raw_source_items is the highest-volume table in the
-- system — a second index would tax every connector's writes for nothing.
--
-- Scope: only BRIDGE-mode Calendar sets this column. Direct database-token mode
-- still applies a 410 re-window as an overlay (calendar SPEC criterion 6 keeps
-- it unchanged), so the stranding bug above persists there until direct mode
-- gets the same treatment. Other connectors never set it, and only the google
-- normalize path filters on it.
