// ui_theme.go implements runtime whitelabel branding: an admin-editable
// UIThemeConfig persisted in system_settings and served publicly to the SPA,
// which wires it into the shared SuiteThemeProvider (product name, palette,
// logo/favicon/login-hero). The read is unauthenticated on purpose — branding
// must apply on the login page, before any session exists.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// UIThemeConfig mirrors the frontend's UIThemeConfig (services/api.ts). Every
// field is optional; absent fields fall back to built-in defaults client-side.
type UIThemeConfig struct {
	ProductName         string `json:"product_name,omitempty"`
	PrimaryColor        string `json:"primary_color,omitempty"`
	SecondaryColorLight string `json:"secondary_color_light,omitempty"`
	SecondaryColorDark  string `json:"secondary_color_dark,omitempty"`
	LogoURL             string `json:"logo_url,omitempty"`
	FaviconURL          string `json:"favicon_url,omitempty"`
	LoginHeroURL        string `json:"login_hero_url,omitempty"`
}

// themeColor accepts the color notations MUI's decomposeColor parses — hex
// (#rgb through #rrggbbaa) and the rgb/rgba/hsl/hsla functional forms. The
// suite-ui theme factory guards against unparsable values client-side; this
// server-side gate keeps a bad value from ever being persisted.
var themeColor = regexp.MustCompile(
	`^(#(?:[0-9a-fA-F]{3,4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})|(?:rgb|rgba|hsl|hsla)\([0-9a-zA-Z.,%\s/]+\))$`)

// maxProductNameLen bounds the branding name shown in the app bar and titles.
const maxProductNameLen = 100

// validateUITheme rejects values that could break the SPA render or smuggle an
// unsafe URL. URLs must be https, or root-relative for assets co-hosted with
// the SPA (http is allowed only for localhost development).
func validateUITheme(t *UIThemeConfig) error {
	if len(t.ProductName) > maxProductNameLen {
		return fmt.Errorf("product_name exceeds %d characters", maxProductNameLen)
	}
	for name, v := range map[string]string{
		"primary_color":         t.PrimaryColor,
		"secondary_color_light": t.SecondaryColorLight,
		"secondary_color_dark":  t.SecondaryColorDark,
	} {
		if v != "" && !themeColor.MatchString(v) {
			return fmt.Errorf("%s is not a valid color (hex or rgb()/hsl() notation)", name)
		}
	}
	for name, v := range map[string]string{
		"logo_url":       t.LogoURL,
		"favicon_url":    t.FaviconURL,
		"login_hero_url": t.LoginHeroURL,
	} {
		if v == "" {
			continue
		}
		lower := strings.ToLower(v)
		ok := strings.HasPrefix(lower, "https://") ||
			(strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//")) ||
			strings.HasPrefix(lower, "http://localhost") ||
			strings.HasPrefix(lower, "http://127.0.0.1")
		if !ok {
			return fmt.Errorf("%s must be an https:// URL or a root-relative path", name)
		}
	}
	return nil
}

// GetUITheme serves the stored branding; {} when unset (the SPA falls back to
// built-in defaults). Public: the login page renders before any session exists.
// @Summary      Whitelabel theme
// @Description  Runtime branding for the SPA (product name, palette colors, logo/favicon/login-hero URLs). Empty object when no branding is configured.
// @Tags         UI
// @Produce      json
// @Success      200  {object}  UIThemeConfig
// @Router       /ui/theme [get]
func GetUITheme(settings *repositories.SystemSettingsRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		if settings == nil { // nil-DB rigs
			c.JSON(http.StatusOK, gin.H{})
			return
		}
		raw, err := settings.GetUIThemeConfig(c.Request.Context())
		if err != nil {
			serverError(c, err, "failed to load ui theme")
			return
		}
		if len(raw) == 0 {
			c.JSON(http.StatusOK, gin.H{})
			return
		}
		c.Data(http.StatusOK, "application/json", raw)
	}
}

// UpdateUITheme validates and persists the branding (admin). An empty body
// ({} or all-blank fields) clears every override back to built-in defaults.
// @Summary      Update whitelabel theme
// @Description  Validates and persists runtime branding. Colors must be hex or rgb()/hsl() notation; URLs must be https or root-relative. Empty fields clear their overrides. Requires admin.
// @Tags         UI
// @Accept       json
// @Produce      json
// @Success      200  {object}  UIThemeConfig
// @Failure      400  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/ui/theme [put]
func UpdateUITheme(settings *repositories.SystemSettingsRepository, audit auditor) gin.HandlerFunc {
	return func(c *gin.Context) {
		var t UIThemeConfig
		if err := c.ShouldBindJSON(&t); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid theme payload"})
			return
		}
		if err := validateUITheme(&t); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		blob, err := json.Marshal(&t)
		if err != nil {
			serverError(c, err, "failed to encode ui theme")
			return
		}
		if err := settings.SetUIThemeConfig(c.Request.Context(), blob); err != nil {
			serverError(c, err, "failed to save ui theme")
			return
		}
		audit.write(c, "admin.ui_theme.update", "system_settings", "ui_theme",
			map[string]interface{}{"product_name": t.ProductName})
		c.JSON(http.StatusOK, t)
	}
}
