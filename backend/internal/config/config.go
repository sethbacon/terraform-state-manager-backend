// Package config loads and validates the state manager configuration using Viper.
//
// Configuration is layered: built-in defaults < YAML config file < environment
// variables. Environment variables use the TSM_ prefix (e.g., TSM_DATABASE_HOST
// overrides database.host in the YAML).
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config holds all application configuration
type Config struct {
	Server        ServerConfig        `mapstructure:"server"`
	Database      DatabaseConfig      `mapstructure:"database"`
	Storage       StorageConfig       `mapstructure:"storage"`
	Auth          AuthConfig          `mapstructure:"auth"`
	ApiDocs       ApiDocsConfig       `mapstructure:"api_docs"`
	MultiTenancy  MultiTenancyConfig  `mapstructure:"multi_tenancy"`
	Security      SecurityConfig      `mapstructure:"security"`
	Logging       LoggingConfig       `mapstructure:"logging"`
	Telemetry     TelemetryConfig     `mapstructure:"telemetry"`
	Audit         AuditConfig         `mapstructure:"audit"`
	Notifications NotificationsConfig `mapstructure:"notifications"`
}

// ServerConfig holds HTTP server configuration
type ServerConfig struct {
	Host         string        `mapstructure:"host"`
	Port         int           `mapstructure:"port"`
	BaseURL      string        `mapstructure:"base_url"`
	PublicURL    string        `mapstructure:"public_url"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
}

// GetPublicURL returns the public-facing URL used for OAuth callbacks.
func (s *ServerConfig) GetPublicURL() string {
	if s.PublicURL != "" {
		return s.PublicURL
	}
	return s.BaseURL
}

// GetAddress returns the server address in host:port format
func (s *ServerConfig) GetAddress() string {
	return fmt.Sprintf("%s:%d", s.Host, s.Port)
}

// DatabaseConfig holds database connection configuration
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

// GetDSN returns the PostgreSQL connection string
func (c *DatabaseConfig) GetDSN() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		c.Host, c.Port, c.User, c.Password, c.Name, c.SSLMode,
	)
}

// StorageConfig holds storage backend configuration
type StorageConfig struct {
	DefaultBackend string             `mapstructure:"default_backend"`
	Azure          AzureStorageConfig `mapstructure:"azure"`
	S3             S3StorageConfig    `mapstructure:"s3"`
	GCS            GCSStorageConfig   `mapstructure:"gcs"`
	Local          LocalStorageConfig `mapstructure:"local"`
}

// AzureStorageConfig holds Azure Blob Storage configuration
type AzureStorageConfig struct {
	AccountName   string `mapstructure:"account_name"`
	AccountKey    string `mapstructure:"account_key"`
	ContainerName string `mapstructure:"container_name"`
}

// S3StorageConfig holds S3-compatible storage configuration
type S3StorageConfig struct {
	Endpoint             string `mapstructure:"endpoint"`
	Region               string `mapstructure:"region"`
	Bucket               string `mapstructure:"bucket"`
	AuthMethod           string `mapstructure:"auth_method"`
	AccessKeyID          string `mapstructure:"access_key_id"`
	SecretAccessKey      string `mapstructure:"secret_access_key"`
	RoleARN              string `mapstructure:"role_arn"`
	RoleSessionName      string `mapstructure:"role_session_name"`
	ExternalID           string `mapstructure:"external_id"`
	WebIdentityTokenFile string `mapstructure:"web_identity_token_file"`
}

// GCSStorageConfig holds Google Cloud Storage configuration
type GCSStorageConfig struct {
	Bucket          string `mapstructure:"bucket"`
	ProjectID       string `mapstructure:"project_id"`
	AuthMethod      string `mapstructure:"auth_method"`
	CredentialsFile string `mapstructure:"credentials_file"`
	CredentialsJSON string `mapstructure:"credentials_json"`
	Endpoint        string `mapstructure:"endpoint"`
}

// LocalStorageConfig holds local filesystem storage configuration
type LocalStorageConfig struct {
	BasePath string `mapstructure:"base_path"`
}

// AuthConfig holds authentication configuration
type AuthConfig struct {
	APIKeys APIKeyConfig  `mapstructure:"api_keys"`
	OIDC    OIDCConfig    `mapstructure:"oidc"`
	AzureAD AzureADConfig `mapstructure:"azure_ad"`
}

// APIKeyConfig holds API key authentication configuration
type APIKeyConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	Prefix  string `mapstructure:"prefix"`
}

// OIDCGroupMapping maps a single IdP group to an organization and role template.
type OIDCGroupMapping struct {
	Group        string `mapstructure:"group"`
	Organization string `mapstructure:"organization"`
	Role         string `mapstructure:"role"`
}

// OIDCConfig holds generic OIDC provider configuration
type OIDCConfig struct {
	Enabled        bool               `mapstructure:"enabled"`
	IssuerURL      string             `mapstructure:"issuer_url"`
	ClientID       string             `mapstructure:"client_id"`
	ClientSecret   string             `mapstructure:"client_secret"`
	RedirectURL    string             `mapstructure:"redirect_url"`
	Scopes         []string           `mapstructure:"scopes"`
	GroupClaimName string             `mapstructure:"group_claim_name"`
	GroupMappings  []OIDCGroupMapping `mapstructure:"group_mappings"`
	DefaultRole    string             `mapstructure:"default_role"`
}

// AzureADConfig holds Azure AD specific configuration
type AzureADConfig struct {
	Enabled      bool   `mapstructure:"enabled"`
	TenantID     string `mapstructure:"tenant_id"`
	ClientID     string `mapstructure:"client_id"`
	ClientSecret string `mapstructure:"client_secret"`
	RedirectURL  string `mapstructure:"redirect_url"`
}

// MultiTenancyConfig holds multi-tenancy configuration
type MultiTenancyConfig struct {
	Enabled             bool   `mapstructure:"enabled"`
	DefaultOrganization string `mapstructure:"default_organization"`
	AllowPublicSignup   bool   `mapstructure:"allow_public_signup"`
}

// SecurityConfig holds security-related configuration
type SecurityConfig struct {
	CORS         CORSConfig         `mapstructure:"cors"`
	RateLimiting RateLimitingConfig `mapstructure:"rate_limiting"`
	TLS          TLSConfig          `mapstructure:"tls"`
}

// CORSConfig holds CORS configuration
type CORSConfig struct {
	AllowedOrigins []string `mapstructure:"allowed_origins"`
	AllowedMethods []string `mapstructure:"allowed_methods"`
}

// RateLimitingConfig holds rate limiting configuration
type RateLimitingConfig struct {
	Enabled           bool `mapstructure:"enabled"`
	RequestsPerMinute int  `mapstructure:"requests_per_minute"`
	Burst             int  `mapstructure:"burst"`
}

// TLSConfig holds TLS/HTTPS configuration
type TLSConfig struct {
	Enabled  bool   `mapstructure:"enabled"`
	CertFile string `mapstructure:"cert_file"`
	KeyFile  string `mapstructure:"key_file"`
}

// LoggingConfig holds logging configuration
type LoggingConfig struct {
	Level  string `mapstructure:"level"`
	Format string `mapstructure:"format"`
	Output string `mapstructure:"output"`
}

// TelemetryConfig holds observability configuration
type TelemetryConfig struct {
	Enabled     bool            `mapstructure:"enabled"`
	ServiceName string          `mapstructure:"service_name"`
	Metrics     MetricsConfig   `mapstructure:"metrics"`
	Tracing     TracingConfig   `mapstructure:"tracing"`
	Profiling   ProfilingConfig `mapstructure:"profiling"`
}

// MetricsConfig holds Prometheus metrics configuration
type MetricsConfig struct {
	Enabled        bool `mapstructure:"enabled"`
	PrometheusPort int  `mapstructure:"prometheus_port"`
}

// TracingConfig holds distributed tracing configuration
type TracingConfig struct {
	Enabled        bool   `mapstructure:"enabled"`
	JaegerEndpoint string `mapstructure:"jaeger_endpoint"`
}

// ProfilingConfig holds profiling configuration
type ProfilingConfig struct {
	Enabled bool `mapstructure:"enabled"`
	Port    int  `mapstructure:"port"`
}

// AuditConfig holds audit logging configuration
type AuditConfig struct {
	Enabled           bool                 `mapstructure:"enabled"`
	LogReadOperations bool                 `mapstructure:"log_read_operations"`
	LogFailedRequests bool                 `mapstructure:"log_failed_requests"`
	Shippers          []AuditShipperConfig `mapstructure:"shippers"`
}

// AuditShipperConfig holds configuration for a single audit shipper
type AuditShipperConfig struct {
	Enabled bool                `mapstructure:"enabled"`
	Type    string              `mapstructure:"type"`
	Syslog  *AuditSyslogConfig  `mapstructure:"syslog"`
	Webhook *AuditWebhookConfig `mapstructure:"webhook"`
	File    *AuditFileConfig    `mapstructure:"file"`
}

// AuditSyslogConfig holds syslog shipper configuration
type AuditSyslogConfig struct {
	Network  string `mapstructure:"network"`
	Address  string `mapstructure:"address"`
	Tag      string `mapstructure:"tag"`
	Facility string `mapstructure:"facility"`
}

// AuditWebhookConfig holds webhook shipper configuration
type AuditWebhookConfig struct {
	URL           string            `mapstructure:"url"`
	Headers       map[string]string `mapstructure:"headers"`
	TimeoutSecs   int               `mapstructure:"timeout_secs"`
	BatchSize     int               `mapstructure:"batch_size"`
	FlushInterval int               `mapstructure:"flush_interval_secs"`
}

// AuditFileConfig holds file shipper configuration
type AuditFileConfig struct {
	Path       string `mapstructure:"path"`
	MaxSizeMB  int    `mapstructure:"max_size_mb"`
	MaxBackups int    `mapstructure:"max_backups"`
}

// ApiDocsConfig holds configurable metadata for OpenAPI/Swagger docs
type ApiDocsConfig struct {
	TermsOfService string `mapstructure:"terms_of_service"`
	ContactName    string `mapstructure:"contact_name"`
	ContactEmail   string `mapstructure:"contact_email"`
	License        string `mapstructure:"license"`
}

// NotificationsConfig holds settings for outbound notifications
type NotificationsConfig struct {
	Enabled                        bool       `mapstructure:"enabled"`
	SMTP                           SMTPConfig `mapstructure:"smtp"`
	APIKeyExpiryWarningDays        int        `mapstructure:"api_key_expiry_warning_days"`
	APIKeyExpiryCheckIntervalHours int        `mapstructure:"api_key_expiry_check_interval_hours"`
}

// SMTPConfig holds outbound mail server configuration
type SMTPConfig struct {
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
	From     string `mapstructure:"from"`
	UseTLS   bool   `mapstructure:"use_tls"`
}

// bindEnvVars explicitly binds environment variables to config keys.
func bindEnvVars(v *viper.Viper) error {
	keys := []string{
		"database.host", "database.port", "database.name", "database.user",
		"database.password", "database.ssl_mode", "database.max_connections",
		"database.min_idle_connections",

		"server.host", "server.port", "server.base_url", "server.public_url",
		"server.read_timeout", "server.write_timeout",

		"storage.default_backend",
		"storage.azure.account_name", "storage.azure.account_key",
		"storage.azure.container_name",
		"storage.s3.endpoint", "storage.s3.region", "storage.s3.bucket",
		"storage.s3.auth_method", "storage.s3.access_key_id",
		"storage.s3.secret_access_key", "storage.s3.role_arn",
		"storage.s3.role_session_name", "storage.s3.external_id",
		"storage.s3.web_identity_token_file",
		"storage.gcs.bucket", "storage.gcs.project_id", "storage.gcs.auth_method",
		"storage.gcs.credentials_file", "storage.gcs.credentials_json",
		"storage.gcs.endpoint",
		"storage.local.base_path",

		"auth.api_keys.enabled", "auth.api_keys.prefix",
		"auth.oidc.enabled", "auth.oidc.issuer_url", "auth.oidc.client_id",
		"auth.oidc.client_secret", "auth.oidc.redirect_url", "auth.oidc.scopes",
		"auth.oidc.group_claim_name", "auth.oidc.group_mappings",
		"auth.oidc.default_role",
		"auth.azure_ad.enabled", "auth.azure_ad.tenant_id",
		"auth.azure_ad.client_id", "auth.azure_ad.client_secret",
		"auth.azure_ad.redirect_url",

		"multi_tenancy.enabled", "multi_tenancy.default_organization",
		"multi_tenancy.allow_public_signup",

		"security.cors.allowed_origins", "security.cors.allowed_methods",
		"security.rate_limiting.enabled", "security.rate_limiting.requests_per_minute",
		"security.rate_limiting.burst",
		"security.tls.enabled", "security.tls.cert_file", "security.tls.key_file",

		"logging.level", "logging.format", "logging.output",

		"telemetry.enabled", "telemetry.service_name",
		"telemetry.metrics.enabled", "telemetry.metrics.prometheus_port",
		"telemetry.tracing.enabled", "telemetry.tracing.jaeger_endpoint",
		"telemetry.profiling.enabled", "telemetry.profiling.port",

		"api_docs.terms_of_service", "api_docs.contact_name",
		"api_docs.contact_email", "api_docs.license",

		"notifications.enabled",
		"notifications.smtp.host", "notifications.smtp.port",
		"notifications.smtp.username", "notifications.smtp.password",
		"notifications.smtp.from", "notifications.smtp.use_tls",
		"notifications.api_key_expiry_warning_days",
		"notifications.api_key_expiry_check_interval_hours",
	}
	for _, key := range keys {
		if err := v.BindEnv(key); err != nil {
			return fmt.Errorf("failed to bind env var %q: %w", key, err)
		}
	}
	return nil
}

// Load loads configuration from file and environment variables
func Load(configPath string) (*Config, error) {
	v := viper.New()

	setDefaults(v)

	if configPath != "" {
		v.SetConfigFile(configPath)
	} else {
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
		v.AddConfigPath("./config")
		v.AddConfigPath("/etc/terraform-state-manager")
	}

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("error reading config file: %w", err)
		}
	}

	v.SetEnvPrefix("TSM")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if err := bindEnvVars(v); err != nil {
		return nil, err
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("error unmarshaling config: %w", err)
	}

	cfg.Database.Password = expandEnv(cfg.Database.Password)
	cfg.Storage.Azure.AccountKey = expandEnv(cfg.Storage.Azure.AccountKey)
	cfg.Storage.S3.AccessKeyID = expandEnv(cfg.Storage.S3.AccessKeyID)
	cfg.Storage.S3.SecretAccessKey = expandEnv(cfg.Storage.S3.SecretAccessKey)
	cfg.Auth.OIDC.ClientSecret = expandEnv(cfg.Auth.OIDC.ClientSecret)
	cfg.Auth.AzureAD.ClientSecret = expandEnv(cfg.Auth.AzureAD.ClientSecret)
	cfg.Notifications.SMTP.Password = expandEnv(cfg.Notifications.SMTP.Password)

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return &cfg, nil
}

// setDefaults sets default configuration values
func setDefaults(v *viper.Viper) {
	v.SetDefault("server.host", "0.0.0.0")
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.base_url", "http://localhost:8080")
	v.SetDefault("server.public_url", "")
	v.SetDefault("server.read_timeout", "30s")
	v.SetDefault("server.write_timeout", "30s")

	v.SetDefault("database.host", "localhost")
	v.SetDefault("database.port", 5432)
	v.SetDefault("database.name", "terraform_state_manager")
	v.SetDefault("database.user", "tsm")
	v.SetDefault("database.ssl_mode", "require")
	v.SetDefault("database.max_connections", 25)
	v.SetDefault("database.min_idle_connections", 5)

	v.SetDefault("storage.default_backend", "local")
	v.SetDefault("storage.local.base_path", "./storage")

	v.SetDefault("auth.api_keys.enabled", true)
	v.SetDefault("auth.api_keys.prefix", "tsm")
	v.SetDefault("auth.oidc.enabled", false)
	v.SetDefault("auth.oidc.scopes", []string{"openid", "email", "profile"})
	v.SetDefault("auth.azure_ad.enabled", false)

	v.SetDefault("multi_tenancy.enabled", false)
	v.SetDefault("multi_tenancy.default_organization", "default")
	v.SetDefault("multi_tenancy.allow_public_signup", false)

	v.SetDefault("security.cors.allowed_origins", []string{"*"})
	v.SetDefault("security.cors.allowed_methods", []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"})
	v.SetDefault("security.rate_limiting.enabled", true)
	v.SetDefault("security.rate_limiting.requests_per_minute", 60)
	v.SetDefault("security.rate_limiting.burst", 10)
	v.SetDefault("security.tls.enabled", false)

	v.SetDefault("logging.level", "info")
	v.SetDefault("logging.format", "json")
	v.SetDefault("logging.output", "stdout")

	v.SetDefault("telemetry.enabled", true)
	v.SetDefault("telemetry.service_name", "terraform-state-manager")
	v.SetDefault("telemetry.metrics.enabled", true)
	v.SetDefault("telemetry.metrics.prometheus_port", 9090)
	v.SetDefault("telemetry.tracing.enabled", false)
	v.SetDefault("telemetry.profiling.enabled", false)
	v.SetDefault("telemetry.profiling.port", 6060)

	v.SetDefault("api_docs.terms_of_service", "")
	v.SetDefault("api_docs.contact_name", "")
	v.SetDefault("api_docs.contact_email", "")
	v.SetDefault("api_docs.license", "")

	v.SetDefault("notifications.enabled", false)
	v.SetDefault("notifications.smtp.port", 587)
	v.SetDefault("notifications.smtp.use_tls", true)
	v.SetDefault("notifications.api_key_expiry_warning_days", 7)
	v.SetDefault("notifications.api_key_expiry_check_interval_hours", 24)
}

func expandEnv(s string) string {
	return os.ExpandEnv(s)
}

// Validate validates the configuration
func (c *Config) Validate() error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("invalid server port: %d", c.Server.Port)
	}
	if c.Server.BaseURL == "" {
		return fmt.Errorf("server.base_url is required")
	}

	if c.Database.Host == "" {
		return fmt.Errorf("database.host is required")
	}
	if c.Database.Name == "" {
		return fmt.Errorf("database.name is required")
	}
	if c.Database.User == "" {
		return fmt.Errorf("database.user is required")
	}

	validBackends := map[string]bool{"azure": true, "s3": true, "gcs": true, "local": true}
	if !validBackends[c.Storage.DefaultBackend] {
		return fmt.Errorf("invalid storage backend: %s (must be azure, s3, gcs, or local)", c.Storage.DefaultBackend)
	}

	if c.Storage.DefaultBackend == "azure" {
		if c.Storage.Azure.AccountName == "" {
			return fmt.Errorf("storage.azure.account_name is required when using Azure backend")
		}
		if c.Storage.Azure.AccountKey == "" {
			return fmt.Errorf("storage.azure.account_key is required when using Azure backend")
		}
		if c.Storage.Azure.ContainerName == "" {
			return fmt.Errorf("storage.azure.container_name is required when using Azure backend")
		}
	}

	if c.Storage.DefaultBackend == "s3" {
		if c.Storage.S3.Bucket == "" {
			return fmt.Errorf("storage.s3.bucket is required when using S3 backend")
		}
		if c.Storage.S3.Region == "" {
			return fmt.Errorf("storage.s3.region is required when using S3 backend")
		}
	}

	if c.Storage.DefaultBackend == "gcs" {
		if c.Storage.GCS.Bucket == "" {
			return fmt.Errorf("storage.gcs.bucket is required when using GCS backend")
		}
	}

	if c.Storage.DefaultBackend == "local" {
		if c.Storage.Local.BasePath == "" {
			return fmt.Errorf("storage.local.base_path is required when using local backend")
		}
	}

	if c.Auth.OIDC.Enabled {
		if c.Auth.OIDC.IssuerURL == "" {
			return fmt.Errorf("auth.oidc.issuer_url is required when OIDC is enabled")
		}
		if c.Auth.OIDC.ClientID == "" {
			return fmt.Errorf("auth.oidc.client_id is required when OIDC is enabled")
		}
		if c.Auth.OIDC.ClientSecret == "" {
			return fmt.Errorf("auth.oidc.client_secret is required when OIDC is enabled")
		}
	}

	if c.Auth.AzureAD.Enabled {
		if c.Auth.AzureAD.TenantID == "" {
			return fmt.Errorf("auth.azure_ad.tenant_id is required when Azure AD is enabled")
		}
		if c.Auth.AzureAD.ClientID == "" {
			return fmt.Errorf("auth.azure_ad.client_id is required when Azure AD is enabled")
		}
		if c.Auth.AzureAD.ClientSecret == "" {
			return fmt.Errorf("auth.azure_ad.client_secret is required when Azure AD is enabled")
		}
	}

	if c.Security.TLS.Enabled {
		if c.Security.TLS.CertFile == "" {
			return fmt.Errorf("security.tls.cert_file is required when TLS is enabled")
		}
		if c.Security.TLS.KeyFile == "" {
			return fmt.Errorf("security.tls.key_file is required when TLS is enabled")
		}
	}

	validLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLevels[c.Logging.Level] {
		return fmt.Errorf("invalid logging level: %s (must be debug, info, warn, or error)", c.Logging.Level)
	}

	return nil
}
