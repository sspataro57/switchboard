-- SWT-43 (delivery-deny): the human negative verdict on a delivery.
-- Spelled 'rejected', the vocabulary approvals.status and plan_imports already
-- use ('deny' is the POLICY verb). See docs/tickets/delivery-deny_SPEC.md.

ALTER TABLE deliveries DROP CONSTRAINT deliveries_status_check;
ALTER TABLE deliveries ADD CONSTRAINT deliveries_status_check CHECK (status IN
  ('drafted','approved','sending','sent','failed','rejected'));

ALTER TABLE deliveries
  ADD COLUMN rejection_note       TEXT,
  ADD COLUMN redraft_requested_at TIMESTAMPTZ;

-- A rejected row was never sent by switchboard and never will be (invariant 4).
ALTER TABLE deliveries ADD CONSTRAINT deliveries_rejected_unsent_check
  CHECK (status <> 'rejected' OR (sent_external_id IS NULL AND confirmed_at IS NULL));

-- The rejection fields exist only on rejected rows.
ALTER TABLE deliveries ADD CONSTRAINT deliveries_rejection_fields_check
  CHECK ((redraft_requested_at IS NULL AND rejection_note IS NULL) OR status = 'rejected');
