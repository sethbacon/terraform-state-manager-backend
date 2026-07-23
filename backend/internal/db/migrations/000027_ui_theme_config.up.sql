-- Whitelabel branding (product name, palette colors, logo/favicon/hero URLs),
-- admin-editable and served publicly via GET /api/v1/ui/theme. Stored as JSONB
-- on the system_settings singleton, mirroring notifications_config.
ALTER TABLE system_settings
    ADD COLUMN IF NOT EXISTS ui_theme_config JSONB;
