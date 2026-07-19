-- Revert notification_channels columns to the pre-shared-schema types.
-- Safe only while the table remains empty (see the corresponding .up.sql);
-- if real channels have since been created, this will need a real data
-- conversion instead of the formality below.
ALTER TABLE notification_channels
    -- Same DROP-DEFAULT-before-TYPE-change requirement as the up migration
    -- (see its comment) applies in reverse for the jsonb -> text[] default.
    -- The USING expression also can't be `ARRAY(SELECT jsonb_array_elements_text(events))`
    -- (Postgres: "cannot use subquery in transform expression") -- translate()
    -- swaps JSON's [...] brackets for Postgres array {...} syntax, which is a
    -- valid single-expression conversion since both use the same
    -- double-quoted-element format for a JSON/text array of plain strings.
    ALTER COLUMN events DROP DEFAULT,
    ALTER COLUMN events TYPE TEXT[] USING translate(events::text, '[]', '{}')::text[],
    ALTER COLUMN events SET DEFAULT '{}',
    ALTER COLUMN encrypted_target TYPE BYTEA USING decode(encrypted_target, 'base64');
