package models

import "time"

// UIThemeConfig is the white-label theme configuration consumed by the frontend
// to brand the application (login page, header, favicon). All override fields are
// optional pointers — a nil value means "no override; use the built-in default".
//
// The shape mirrors the terraform-registry-backend UIThemeConfig so the shared
// frontend ThemeContext can consume either backend interchangeably. It is stored
// as a single JSON blob in the system_settings key-value table under the
// "ui_theme" key rather than in a dedicated table.
type UIThemeConfig struct {
	ProductName         *string   `json:"product_name,omitempty"`
	PrimaryColor        *string   `json:"primary_color,omitempty"`
	SecondaryColorLight *string   `json:"secondary_color_light,omitempty"`
	SecondaryColorDark  *string   `json:"secondary_color_dark,omitempty"`
	LogoURL             *string   `json:"logo_url,omitempty"`
	FaviconURL          *string   `json:"favicon_url,omitempty"`
	LoginHeroURL        *string   `json:"login_hero_url,omitempty"`
	UpdatedAt           time.Time `json:"updated_at"`
}
