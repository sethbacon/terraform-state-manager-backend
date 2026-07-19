-- Align notification_channels columns with the shared schema used by
-- terraform-suite-identity's identity/notify package (also adopted by
-- terraform-registry): encrypted_target as TEXT (base64 output of the shared
-- TokenCipher, replacing the raw-bytes internal/crypto encryption previously
-- used only for this table) and events as JSONB (matching the registry's
-- convention for list-valued config, replacing TEXT[]). This table has zero
-- rows in every deployed environment as of this migration (notification
-- channels are a newly-adopted, not-yet-used feature here), so no existing
-- ciphertext needs re-encryption and the USING clauses below are a formality
-- for schema correctness rather than a real data conversion.
ALTER TABLE notification_channels
    ALTER COLUMN encrypted_target TYPE TEXT USING encode(encrypted_target, 'base64'),
    -- The existing '{}'::text[] default must be dropped before the type change:
    -- Postgres validates the old default is auto-castable to the NEW column
    -- type as part of ALTER COLUMN TYPE, and there is no automatic cast from
    -- text[] to jsonb, so leaving the old default in place makes this whole
    -- statement fail with "default for column \"events\" cannot be cast
    -- automatically to type jsonb" (confirmed live against a real deployment).
    ALTER COLUMN events DROP DEFAULT,
    ALTER COLUMN events TYPE JSONB USING to_jsonb(events),
    ALTER COLUMN events SET DEFAULT '[]'::jsonb;
