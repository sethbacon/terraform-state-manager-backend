-- Adds DB-persisted notification/SMTP configuration to system_settings, mirroring
-- terraform-registry's 000043_setup_notifications migration. This lets the SMTP
-- relay (host/port/credentials/from/use_tls) and the notifications-enabled flag
-- be saved/reloaded at runtime via the admin API instead of only via YAML/env.
-- The SMTP password inside notifications_config MUST be encrypted by the caller
-- (via internal/crypto) before it is stored here.
ALTER TABLE system_settings
  ADD COLUMN IF NOT EXISTS notifications_configured BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN IF NOT EXISTS notifications_config     JSONB;
