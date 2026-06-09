-- Notification channels: where to send alerts (e.g. when a drift run detects
-- drift or a run fails). The target URL is a capability-bearing secret (a Slack /
-- generic incoming-webhook URL), so it is stored encrypted like pipeline tokens.
CREATE TABLE IF NOT EXISTS notification_channels (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name             TEXT NOT NULL,
    type             TEXT NOT NULL,                       -- webhook | slack
    encrypted_target BYTEA NOT NULL,                      -- encrypted destination URL
    events           TEXT[] NOT NULL DEFAULT '{}',        -- subscribed events; empty = all (drift_detected | run_failed)
    enabled          BOOLEAN NOT NULL DEFAULT true,
    last_status      TEXT,                                -- sent | failed
    last_error       TEXT,
    last_sent_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_notification_channels_name ON notification_channels (name);
