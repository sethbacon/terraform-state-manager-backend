// Package config loads and validates the Terraform State Manager configuration
// using Viper.
//
// Configuration is layered: built-in defaults < YAML config file < environment
// variables. Environment variables use the TSM_ prefix (e.g., TSM_DATABASE_HOST
// overrides database.host in the YAML). This layering lets the same binary run
// with a config.yaml in local development and with pure environment variables in
// containerized deployments — no recompilation needed.
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config holds all application configuration. It is intentionally lean for the
// Phase 0 scaffold; new sections (storage, auth, pipelines) are added as those
// features land, following the sibling registry backend's layout.
type Config struct {
	Server    ServerConfig    `mapstructure:"server"`
	Database  DatabaseConfig  `mapstructure:"database"`
	Logging   LoggingConfig   `mapstructure:"logging"`
	Telemetry TelemetryConfig `mapstructure:"telemetry"`
	Auth      AuthConfig      `mapstructure:"auth"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Host         string        `mapstructure:"host"`
	Port         int           `mapstructure:"port"`
	BaseURL      string        `mapstructure:"base_url"`
	PublicURL    string        `mapstructure:"public_url"`
	// CallbackURL is the backend base URL the CI runner posts drift results to.
	// It must be reachable from the CI runner (public internet). Defaults to BaseURL.
	CallbackURL  string        `mapstructure:"callback_url"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
}

// CallbackBase returns the base URL for CI result callbacks (CallbackURL, else BaseURL).
func (s ServerConfig) CallbackBase() string {
	if s.CallbackURL != "" {
		return s.CallbackURL
	}
	return s.BaseURL
}

// GetAddress returns the host:port the server listens on.
func (s ServerConfig) GetAddress() string {
	return fmt.Sprintf("%s:%d", s.Host, s.Port)
}

// DatabaseConfig holds PostgreSQL connection settings.
type DatabaseConfig struct {
	Host               string `mapstructure:"host"`
	Port               int    `mapstructure:"port"`
	Name               string `mapstructure:"name"`
	User               string `mapstructure:"user"`
	Password           string `mapstructure:"password"`
	SSLMode            string `mapstructure:"ssl_mode"`
	MaxConnections     int    `mapstructure:"max_connections"`
	MinIdleConnections int    `mapstructure:"min_idle_connections"`
}

// GetDSN returns a libpq-style connection string.
func (d DatabaseConfig) GetDSN() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		d.Host, d.Port, d.User, d.Password, d.Name, d.SSLMode,
	)
}

// GetDSNWithSearchPath returns the DSN with a per-connection search_path, used
// for the identity-schema connection so the shared identity repositories (which
// query unqualified table names) resolve to the identity schema.
func (d DatabaseConfig) GetDSNWithSearchPath(searchPath string) string {
	return d.GetDSN() + fmt.Sprintf(" options='-c search_path=%s'", searchPath)
}

// LoggingConfig controls structured logging.
type LoggingConfig struct {
	Level  string `mapstructure:"level"`  // debug, info, warn, error
	Format string `mapstructure:"format"` // json or text
}

// TelemetryConfig controls observability.
type TelemetryConfig struct {
	Metrics MetricsConfig `mapstructure:"metrics"`
}

// MetricsConfig controls the Prometheus side-channel endpoint.
type MetricsConfig struct {
	Enabled        bool `mapstructure:"enabled"`
	PrometheusPort int  `mapstructure:"prometheus_port"`
}

// AuthConfig holds authentication configuration. The scaffold ships the OIDC
// (Keycloak) path used for local development; additional providers (SAML, LDAP,
// Azure AD) can be layered on later, mirroring the registry backend.
type AuthConfig struct {
	OIDC OIDCConfig `mapstructure:"oidc"`
}

// OIDCConfig holds generic OpenID Connect provider configuration.
type OIDCConfig struct {
	Enabled      bool     `mapstructure:"enabled"`
	IssuerURL    string   `mapstructure:"issuer_url"`
	ClientID     string   `mapstructure:"client_id"`
	ClientSecret string   `mapstructure:"client_secret"`
	RedirectURL  string   `mapstructure:"redirect_url"`
	Scopes       []string `mapstructure:"scopes"`

	// GroupClaimName is the ID-token claim carrying the user's IdP groups.
	GroupClaimName string `mapstructure:"group_claim_name"`
	// GroupMappings map a verified IdP group claim value to an organization + role
	// template. Applied on login from the cryptographically-verified ID token;
	// admin-configured via YAML/file config, never user-supplied.
	GroupMappings []OIDCGroupMapping `mapstructure:"group_mappings"`
	// DefaultRole is the role template assigned (in the default organization) to a
	// user on login when no group mapping applies. Empty means no scopes are
	// granted automatically.
	DefaultRole string `mapstructure:"default_role"`
}

// OIDCGroupMapping maps a single verified IdP group to an organization and role
// template. The group value is matched against the user's groups claim from the
// verified ID token.
type OIDCGroupMapping struct {
	Group        string `mapstructure:"group"`
	Organization string `mapstructure:"organization"`
	Role         string `mapstructure:"role"`
}

// Load reads configuration from an optional YAML file (path may be empty) layered
// under TSM_-prefixed environment variables and built-in defaults.
func Load(path string) (*Config, error) {
	v := viper.New()
	setDefaults(v)

	v.SetEnvPrefix("TSM")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if path != "" {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("failed to read config file %q: %w", path, err)
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}
	return &cfg, nil
}

// setDefaults registers every key so AutomaticEnv can bind nested TSM_ overrides
// and so a config file is entirely optional.
func setDefaults(v *viper.Viper) {
	v.SetDefault("server.host", "0.0.0.0")
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.base_url", "http://localhost:8080")
	v.SetDefault("server.public_url", "http://localhost:3000")
	v.SetDefault("server.callback_url", "")
	v.SetDefault("server.read_timeout", 30*time.Second)
	v.SetDefault("server.write_timeout", 30*time.Second)

	v.SetDefault("database.host", "localhost")
	v.SetDefault("database.port", 5432)
	v.SetDefault("database.name", "terraform_state_manager")
	v.SetDefault("database.user", "tsm")
	v.SetDefault("database.password", "")
	v.SetDefault("database.ssl_mode", "prefer")
	v.SetDefault("database.max_connections", 25)
	v.SetDefault("database.min_idle_connections", 5)

	v.SetDefault("logging.level", "info")
	v.SetDefault("logging.format", "json")

	v.SetDefault("telemetry.metrics.enabled", true)
	v.SetDefault("telemetry.metrics.prometheus_port", 9090)

	v.SetDefault("auth.oidc.enabled", false)
	v.SetDefault("auth.oidc.issuer_url", "")
	v.SetDefault("auth.oidc.client_id", "")
	v.SetDefault("auth.oidc.client_secret", "")
	v.SetDefault("auth.oidc.redirect_url", "")
	v.SetDefault("auth.oidc.scopes", []string{"openid", "email", "profile"})
	v.SetDefault("auth.oidc.group_claim_name", "groups")
	v.SetDefault("auth.oidc.default_role", "")
}
