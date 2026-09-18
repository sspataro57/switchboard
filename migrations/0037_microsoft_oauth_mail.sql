-- 0037 microsoft-oauth-mail (SWT-66): a third credential kind for the IMAP path.
--
-- outlook.office365.com refuses password logins outright (LOGINDISABLED, with
-- AUTH=XOAUTH2 as its only mechanism), so a Microsoft mailbox authenticates with
-- a bearer token minted from a stored OAuth refresh token.
--
-- No new table and no new column: 0014 already added refresh_token_encrypted,
-- auth_type and the per-account imap/smtp endpoints. Only the CHECK widens.

-- auth_type gains a third value. 'oauth' = the Gmail API transport;
-- 'app_password' = IMAP/SMTP with a stored password; 'xoauth2' = IMAP with an
-- OAuth bearer token. The value names the CREDENTIAL-to-TRANSPORT binding, which
-- is why it is the SASL mechanism's name rather than the vendor's: nothing about
-- this row is Microsoft-specific except the endpoints it happens to carry.
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'source_accounts_auth_type_check'
  ) THEN
    ALTER TABLE source_accounts DROP CONSTRAINT source_accounts_auth_type_check;
  END IF;

  ALTER TABLE source_accounts
    ADD CONSTRAINT source_accounts_auth_type_check
    CHECK (auth_type IN ('oauth','app_password','xoauth2'));
END $$;

-- An xoauth2 row with no refresh token is a mailbox that can never authenticate:
-- every pass would mint nothing and write an error run, forever.
--
-- Spelled to fail CLOSED on NULL, and note what that takes. 0014's shape
-- (`auth_type <> 'xoauth2' OR ...`) evaluates to UNKNOWN for a NULL auth_type
-- and a CHECK ADMITS unknown. Leading with `auth_type IS NULL OR ...` is no
-- better: it evaluates to TRUE and admits the row just as happily, while
-- reading like a guard. Only requiring auth_type to be present actually
-- rejects it. auth_type has been NOT NULL since 0014, so this is unreachable
-- today — it is written this way so it stays closed if that ever changes.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'source_accounts_xoauth2_token_present'
  ) THEN
    ALTER TABLE source_accounts
      ADD CONSTRAINT source_accounts_xoauth2_token_present
      CHECK (auth_type IS NOT NULL
             AND (auth_type <> 'xoauth2'
                  OR refresh_token_encrypted IS NOT NULL));
  END IF;
END $$;
