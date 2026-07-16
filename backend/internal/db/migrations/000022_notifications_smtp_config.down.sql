ALTER TABLE system_settings
  DROP COLUMN IF EXISTS notifications_configured,
  DROP COLUMN IF EXISTS notifications_config;
