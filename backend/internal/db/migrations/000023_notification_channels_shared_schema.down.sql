-- Revert notification_channels columns to the pre-shared-schema types.
-- Safe only while the table remains empty (see the corresponding .up.sql);
-- if real channels have since been created, this will need a real data
-- conversion instead of the formality below.
ALTER TABLE notification_channels
    ALTER COLUMN events TYPE TEXT[] USING ARRAY(SELECT jsonb_array_elements_text(events)),
    ALTER COLUMN events SET DEFAULT '{}',
    ALTER COLUMN encrypted_target TYPE BYTEA USING decode(encrypted_target, 'base64');
