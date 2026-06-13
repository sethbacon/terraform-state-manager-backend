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
	Workers   WorkersConfig   `mapstructure:"workers"`
	Suite     SuiteConfig     `mapstructure:"suite"`
}

// WorkersConfig gates the periodic background workers (schedule runner +
// statesync reconcile loop). In multi-replica deployments exactly ONE replica
// should run them — the schedule runner has no cross-replica claim, so two
// replicas would double-dispatch due schedules. Set TSM_WORKERS_ENABLED=false
// on API replicas and true on a single dedicated worker replica. On-demand
// syncs (post-write refresh, source-create backfill) stay available on every
// replica regardless.
type WorkersConfig struct {
	Enabled bool `mapstructure:"enabled"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Host      string `mapstructure:"host"`
	Port      int    `mapstructure:"port"`
	BaseURL   string `mapstructure:"base_url"`
	PublicURL string `mapstructure:"public_url"`
	// CallbackURL is the backend base URL the CI runner posts drift results to.
	// It must be reachable from the CI runner (public internet). Defaults to BaseURL.
	CallbackURL  string        `mapstructure:"callback_url"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
	// TLSCertFile / TLSKeyFile enable HTTPS directly on the server. Required for
	// the direct-TLS mTLS path; leave empty when TLS is terminated by a proxy.
	TLSCertFile string `mapstructure:"tls_cert_file"`
	TLSKeyFile  string `mapstructure:"tls_key_file"`
}

// CallbackBase returns the base URL for CI result callbacks (CallbackURL, else BaseURL).
func (s ServerConfig) CallbackBase() string {
	if s.CallbackURL != "" {
		return s.CallbackURL
	}
	return s.BaseURL
}

// SuiteConfig configures optional runtime coupling to the sibling Suite app.
// SiblingURL empty (default) = standalone.
type SuiteConfig struct {
	SiblingURL   string        `mapstructure:"sibling_url"`   // TSM_SUITE_SIBLING_URL
	PollInterval time.Duration `mapstructure:"poll_interval"` // TSM_SUITE_POLL_INTERVAL, default 60s
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
	MTLS MTLSConfig `mapstructure:"mtls"`
	LDAP LDAPConfig `mapstructure:"ldap"`
	SAML SAMLConfig `mapstructure:"saml"`
	SCIM SCIMConfig `mapstructure:"scim"`
}

// SCIMConfig gates the SCIM 2.0 provisioning endpoints (/scim/v2). Disabled by
// default: when off the routes are not mounted at all, so the surface does not
// exist unless an operator opts in. When on, the endpoints are still guarded by
// bearer-token auth + the scim:provision scope (admin satisfies it).
type SCIMConfig struct {
	Enabled bool `mapstructure:"enabled"`
}

// LDAPConfig holds LDAP / Active Directory search-bind authentication settings.
type LDAPConfig struct {
	Enabled            bool   `mapstructure:"enabled"`
	Host               string `mapstructure:"host"`
	Port               int    `mapstructure:"port"`
	UseTLS             bool   `mapstructure:"use_tls"`              // LDAPS (TLS from connect)
	StartTLS           bool   `mapstructure:"start_tls"`            // upgrade plain LDAP to TLS
	InsecureSkipVerify bool   `mapstructure:"insecure_skip_verify"` // dev only — never in production
	BaseDN             string `mapstructure:"base_dn"`
	BindDN             string `mapstructure:"bind_dn"` // service account for search
	BindPassword       string `mapstructure:"bind_password"`
	UserFilter         string `mapstructure:"user_filter"` // must contain %s for the escaped username
	UserAttrEmail      string `mapstructure:"user_attr_email"`
	UserAttrName       string `mapstructure:"user_attr_name"`
	GroupBaseDN        string `mapstructure:"group_base_dn"`
	GroupFilter        string `mapstructure:"group_filter"` // optional; %s = escaped user DN
	GroupMemberAttr    string `mapstructure:"group_member_attr"`
	// GroupMappings map an LDAP group DN to an organization + role template.
	GroupMappings []LDAPGroupMapping `mapstructure:"group_mappings"`
	// DefaultRole granted in the default org on first login when no group matches.
	DefaultRole string `mapstructure:"default_role"`
}

// LDAPGroupMapping maps an LDAP group DN to an organization and role template.
type LDAPGroupMapping struct {
	GroupDN      string `mapstructure:"group_dn"`
	Organization string `mapstructure:"organization"`
	Role         string `mapstructure:"role"`
}

// MTLSConfig holds mutual-TLS client-certificate authentication settings. The
// TLS server verifies presented certs against ClientCAFile; mappings then grant
// scopes to verified subjects.
type MTLSConfig struct {
	Enabled      bool                 `mapstructure:"enabled"`
	ClientCAFile string               `mapstructure:"client_ca_file"`
	Mappings     []MTLSSubjectMapping `mapstructure:"mappings"`
}

// MTLSSubjectMapping maps a verified client-cert subject (CN=…, dns:<san>, or a
// full DN) to scopes.
type MTLSSubjectMapping struct {
	Subject string   `mapstructure:"subject"`
	Scopes  []string `mapstructure:"scopes"`
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

// SAMLConfig holds SAML 2.0 Service Provider settings. The SP validates
// XML-signed assertions from one or more configured IdPs (auth.saml.idps) and
// maps SAML group-attribute values to org/role memberships (same model as
// OIDC/LDAP). The ACS endpoint is POST /api/v1/auth/saml/acs.
type SAMLConfig struct {
	Enabled  bool   `mapstructure:"enabled"`
	EntityID string `mapstructure:"entity_id"` // SP entity ID; defaults to acs_url minus /saml/acs
	ACSURL   string `mapstructure:"acs_url"`   // public URL of POST /api/v1/auth/saml/acs
	CertFile string `mapstructure:"cert_file"` // optional SP signing cert (PEM)
	KeyFile  string `mapstructure:"key_file"`  // optional SP signing key (PEM)

	// AllowIDPInitiated permits unsolicited (IdP-initiated) SSO. Default false:
	// only SP-initiated flows are accepted and the AuthnRequest ID is bound to the
	// response (InResponseTo), defeating stolen-assertion replay and login CSRF.
	AllowIDPInitiated bool `mapstructure:"allow_idp_initiated"`

	// IdPs lists one or more SAML Identity Providers (by metadata URL or XML).
	IdPs []SAMLIdPConfig `mapstructure:"idps"`

	// GroupAttributeName is the assertion attribute carrying the user's groups.
	GroupAttributeName string `mapstructure:"group_attribute_name"`
	// GroupMappings map a SAML group-attribute value to an organization + role
	// template. Orgs referenced are IdP-authoritative: a user's membership is
	// reconciled on every login (and revoked when the group is lost).
	GroupMappings []SAMLGroupMapping `mapstructure:"group_mappings"`
	// DefaultRole granted in the default org on a user's FIRST login only when no
	// group mapping applies.
	DefaultRole string `mapstructure:"default_role"`
}

// SAMLIdPConfig describes a single SAML Identity Provider. Provide either a
// metadata_url (HTTPS) or inline metadata_xml.
type SAMLIdPConfig struct {
	Name        string `mapstructure:"name"`
	MetadataURL string `mapstructure:"metadata_url"`
	MetadataXML string `mapstructure:"metadata_xml"`
}

// SAMLGroupMapping maps a SAML group-attribute value to an organization and role
// template. The group value is matched exactly against the assertion attribute.
type SAMLGroupMapping struct {
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
	v.SetDefault("server.tls_cert_file", "")
	v.SetDefault("server.tls_key_file", "")

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

	v.SetDefault("workers.enabled", true)

	v.SetDefault("auth.oidc.enabled", false)
	v.SetDefault("auth.oidc.issuer_url", "")
	v.SetDefault("auth.oidc.client_id", "")
	v.SetDefault("auth.oidc.client_secret", "")
	v.SetDefault("auth.oidc.redirect_url", "")
	v.SetDefault("auth.oidc.scopes", []string{"openid", "email", "profile"})
	v.SetDefault("auth.oidc.group_claim_name", "groups")
	v.SetDefault("auth.oidc.default_role", "")

	v.SetDefault("auth.mtls.enabled", false)
	v.SetDefault("auth.mtls.client_ca_file", "")

	v.SetDefault("auth.ldap.enabled", false)
	v.SetDefault("auth.ldap.host", "")
	v.SetDefault("auth.ldap.port", 0)
	v.SetDefault("auth.ldap.use_tls", false)
	v.SetDefault("auth.ldap.start_tls", false)
	v.SetDefault("auth.ldap.insecure_skip_verify", false)
	v.SetDefault("auth.ldap.base_dn", "")
	v.SetDefault("auth.ldap.bind_dn", "")
	v.SetDefault("auth.ldap.bind_password", "")
	v.SetDefault("auth.ldap.user_filter", "")
	v.SetDefault("auth.ldap.user_attr_email", "mail")
	v.SetDefault("auth.ldap.user_attr_name", "displayName")
	v.SetDefault("auth.ldap.group_base_dn", "")
	v.SetDefault("auth.ldap.group_filter", "")
	v.SetDefault("auth.ldap.group_member_attr", "member")
	v.SetDefault("auth.ldap.default_role", "")

	// Suite runtime discovery
	v.SetDefault("suite.sibling_url", "")
	v.SetDefault("suite.poll_interval", 60*time.Second)
}
