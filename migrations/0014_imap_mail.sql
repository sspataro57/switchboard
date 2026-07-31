-- 0014_imap_mail.sql — SWT-11, the IMAP/SMTP mail path.
-- docs/tickets/imap-mail-connector_SPEC.md (specced as 0011; renumbered because
-- 0011-0013 were taken while this ticket was parked — see the SPEC's amendments).
--
-- Google OAuth is abandoned as the mail path, not deferred. Of the mailboxes in
-- scope one is personal Gmail and several are Workspace orgs Salvador does not
-- administer: an Internal OAuth app covers only his own org, External plus
-- restricted Gmail scopes needs Google verification and a CASA assessment, and a
-- third-party Workspace admin can block the client id regardless. App passwords
-- over IMAP were tested live against all three, including the org he does not
-- control.
--
-- So an account row now describes HOW to authenticate, not just who it is.
-- auth_type is the discriminator the send router reads to choose SMTP or the
-- Gmail API, and the ingest path reads to choose IMAP or the Gmail API. Both
-- kinds coexist deliberately: the column defaults to 'oauth' so every existing
-- row keeps its current behaviour with no backfill and no code change.
--
-- The password is encrypted with pgcrypto under OPS_TOKEN_KEY, exactly as
-- refresh_token_encrypted already is. The key never reaches the database and the
-- password never reaches a log line: a database dump must not be a credential
-- dump. The k8s secret holds the original; google-auth add-app-password pipes it
-- in over stdin.
--
-- No new table, and no new extension — the ops role cannot CREATE EXTENSION, and
-- pgcrypto is already present (see INSTITUTIONAL_KNOWLEDGE.md, Environment facts).

ALTER TABLE source_accounts
  ADD COLUMN IF NOT EXISTS auth_type              TEXT NOT NULL DEFAULT 'oauth',
  ADD COLUMN IF NOT EXISTS app_password_encrypted BYTEA,
  ADD COLUMN IF NOT EXISTS imap_host              TEXT,
  ADD COLUMN IF NOT EXISTS imap_port              INT,
  ADD COLUMN IF NOT EXISTS smtp_host              TEXT,
  ADD COLUMN IF NOT EXISTS smtp_port              INT;

-- Guarded so a re-apply on a database that already has the constraint is a
-- no-op rather than an error; ADD CONSTRAINT has no IF NOT EXISTS.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'source_accounts_auth_type_check'
  ) THEN
    ALTER TABLE source_accounts
      ADD CONSTRAINT source_accounts_auth_type_check
      CHECK (auth_type IN ('oauth','app_password'));
  END IF;
END $$;

-- An app-password account with no password is unusable and, worse, silently
-- unusable: every ingest pass would fail to authenticate and every send would
-- error at dial time. Refuse the state outright rather than discovering it in
-- production logs.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'source_accounts_app_password_present'
  ) THEN
    ALTER TABLE source_accounts
      ADD CONSTRAINT source_accounts_app_password_present
      CHECK (auth_type <> 'app_password' OR app_password_encrypted IS NOT NULL);
  END IF;
END $$;
