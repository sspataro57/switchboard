-- 0024 projects.ai_inquiry (SWT-33, the inquiry lane).
--
-- A WORKLOAD flag: "inbound messages attributed to this project get an inquiry
-- verdict from the local inquiry lane" — does this message need a reply from
-- Salvador. It is NOT the locality boundary (ai_locality, 0016), which stays out
-- of this lane's filter on purpose, and it is not 0018's personal-lane flag
-- either: that one opts mail into a different question, and reusing it would
-- drag personal mail into a lane that asks a client-conversation question.
--
-- A typed column, never a projects.policies jsonb key — 0016, 0018 and 0023 are
-- the precedents and the recorded argument. DEFAULT false is fail-closed: a
-- stall is one UPDATE, and nothing classifies a project nobody armed. NOT NULL,
-- because a nullable boolean makes `AND p.ai_inquiry` silently drop every row
-- nobody set. No index: projects holds tens of rows, reached by primary key.
--
-- ARMING collaboratory here is deliberate (D4), unlike 0021, which left its
-- column NULL because it EXITS shadow. This lane creates nothing — no task, no
-- outbound row of any kind — so arming it is reversible with one UPDATE:
--     UPDATE projects SET ai_inquiry = false WHERE slug = 'collaboratory';
--
-- Numbered 0024 although 0025 (SWT-34) merged and was applied first. The runner
-- keys on schema_migrations.version and applies every file it has not seen, so
-- this one applies on the next migrate regardless of the order.
ALTER TABLE projects ADD COLUMN ai_inquiry BOOLEAN NOT NULL DEFAULT false;

UPDATE projects SET ai_inquiry = true WHERE slug = 'collaboratory';
