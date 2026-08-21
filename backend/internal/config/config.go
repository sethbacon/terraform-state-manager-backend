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
	Server   ServerConfig   `mapstructure:"server"`
	Database DatabaseConfig `mapstructure:"database"`
	// IdentityDatabase optionally points the identity schema (users, orgs, roles,
	// tokens) at a separate/shared database. Any unset field falls back to
	// Database, so fully unset = the app database (standalone default). Set
	// TSM_IDENTITY_DATABASE_* to share one identity store across the suite.
	IdentityDatabase DatabaseConfig      `mapstructure:"identity_database"`
	Logging          LoggingConfig       `mapstructure:"logging"`
	Telemetry        TelemetryConfig     `mapstructure:"telemetry"`
	Auth             AuthConfig          `mapstructure:"auth"`
	Authz            AuthzConfig         `mapstructure:"authz"`
	Workers          WorkersConfig       `mapstructure:"workers"`
	Suite            SuiteConfig         `mapstructure:"suite"`
	Notifications    NotificationsConfig `mapstructure:"notifications"`
	Drift            DriftConfig         `mapstructure:"drift"`
	Security         SecurityConfig      `mapstructure:"security"`
	// StateSource confines the server-local filesystem paths a state source may
	// name (see StateSourceConfig).
	StateSource StateSourceConfig `mapstructure:"statesource"`
	// BackupRetention bounds the state_backups table (#257).
	BackupRetention BackupRetentionConfig `mapstructure:"backup_retention"`
	// Tenancy carries the organization-partition rollout controls (#393).
	Tenancy TenancyConfig `mapstructure:"tenancy"`
}

// TenancyConfig controls the organization-partition rollout of
// sethbacon/terraform-state-manager-backend#393.
//
// # DualRead is the evidence, and it is the only thing this phase adds
//
// Migration 000033 sequences that issue in four phases and says why Phase 2 is
// "dual-reads behind a flag and proves equivalence" rather than a flip: a change
// that starts filtering before equivalence is demonstrated is a partial cutover,
// "which is how a deployment ends up half-isolated and nobody can say which
// half". Turning this on makes the /sources read routes ALSO run the
// organization-scoped query beside the unscoped one they serve, and report where
// the two disagree — to the log, and to the tsm_tenant_scope_* series
// (internal/telemetry). The scoped answer is computed and discarded. Nothing an
// operator or a client sees changes.
//
// # It reports; it never errors and never withholds
//
// A divergence found here is not necessarily a fault. On a deployment with two
// organizations the scoped read returning FEWER rows is the whole point of #393
// — it is the leak being closed, observed. Failing the request on it would turn
// "the fix works" into a 500, and would make the flag itself the partial cutover
// the migration refuses: some sources requests served, some 5xx, and the
// deployment half-available in a way no client can characterise. So divergence
// is recorded and the request is served exactly as it was before. What the
// evidence is FOR is the Phase 3 go/no-go: on a single-organization deployment
// the counters must stay at zero, and on a partitioned one the withheld rows
// must be the rows that tenant should never have been able to read.
//
// # Off by default, because it costs a second query
//
// Each observed read runs twice. state_sources is operator-provisioned and small
// (internal/api caps a page at 500 and calls that "far above any realistic
// install"), so the cost is a duplicate index scan on a short table — but it is
// not nothing, and a flag whose default changed behaviour would not be a flag.
//
// Env: TSM_TENANCY_DUAL_READ.
type TenancyConfig struct {
	DualRead bool `mapstructure:"dual_read"`
}

// AuthzConfig controls where authorization decisions read a principal's role
// from, and how often the two candidate sources are compared.
//
// # RoleSource is the rollback lever for Phase 3b
//
// Under sethbacon/terraform-suite-identity#206, identity is SHARED and
// authorization is PER-APP: membership stays a fact in the identity schema, and
// which role a member holds HERE is a row in this application's own
// organization_member_roles. "app" (the default) is that model. "identity" is the
// Phase 3a position — every role read comes from identity.organization_members
// joined to identity.role_templates, exactly as it did before the reads moved.
//
// BOTH TABLES ARE WRITTEN UNDER EITHER VALUE. The dual write is not conditional
// on this setting, so the source that is not being read stays current rather than
// going stale, and switching back is a restart rather than a restore. That is the
// whole reason this is a runtime setting and not a code change: an operator who
// finds a role wrong in production sets TSM_AUTHZ_ROLE_SOURCE=identity, restarts,
// and is running the previous phase's behaviour with no data movement, no
// migration, and no window in which authorization is undefined.
//
// BEFORE UPGRADING ONTO A BUILD THAT DEFAULTS TO "app", run `server authz-drift`
// against the deployment and require a zero exit. A gap between the two sources
// does not surface as an error — it surfaces as a principal silently holding the
// wrong role — so the flip is gated on that command rather than on a release note.
//
// # DriftInterval is the standing detector
//
// How often the running server re-compares the two sources and reports the
// difference (it never corrects; approles.Reconcile does that, at boot). Zero
// disables the loop. The comparison is two ordered scans of the membership tables,
// so it is bounded work on a generous interval rather than a per-request shadow
// read on the API-key hot path.
//
// Env: TSM_AUTHZ_ROLE_SOURCE, TSM_AUTHZ_DRIFT_INTERVAL.
type AuthzConfig struct {
	RoleSource    string        `mapstructure:"role_source"`
	DriftInterval time.Duration `mapstructure:"drift_interval"`
}

// DriftConfig tunes the background reconciler that expires drift runs stuck in
// "dispatched" because their CI job never posted a result callback (build failed
// at init/plan, the agent crashed, the pipeline was cancelled). RunTTL is how long
// a run may sit dispatched before it is failed. The CI job posts its only callback
// at the very end (there is no heartbeat), so RunTTL bounds the run's TOTAL
// wall-clock — checkout + init + provider refresh + plan + callback — not idle
// time; set it above the worst-case plan duration for your largest states so a
// legitimately slow plan is not expired mid-flight. ReconcileInterval is how often
// the sweep runs. Like the schedule runner and statesync, the sweep is a periodic
// worker gated by Workers.Enabled so it runs on exactly one replica.
// Env: TSM_DRIFT_RUN_TTL, TSM_DRIFT_RECONCILE_INTERVAL.
type DriftConfig struct {
	RunTTL            time.Duration `mapstructure:"run_ttl"`
	ReconcileInterval time.Duration `mapstructure:"reconcile_interval"`
}

// SecurityConfig groups defense-in-depth controls. Egress governs the SSRF guard
// applied to every outbound request whose destination is operator- or
// sibling-supplied.
type SecurityConfig struct {
	Egress EgressConfig `mapstructure:"egress"`
}

// EgressConfig controls the SSRF egress allow-list. It governs TWO sets of
// outbound requests, and the entries mean the same thing for both — what differs
// is only what an EMPTY list falls back to.
//
//  1. The state-source connectors (http, consul, k8s, git) that dial an
//     operator-supplied backend host with an attached credential. These block
//     loopback, link-local (including the 169.254.169.254 cloud metadata
//     endpoint) and other non-private reserved ranges, while ALLOWING the
//     RFC1918 / IPv6-ULA private ranges by default so legitimate internal
//     backends keep working.
//  2. The requests the shared identity module makes on this app's behalf, as of
//     terraform-suite-identity v0.25.0: OIDC discovery, the JWKS key-set fetches
//     and the authorization-code token exchange, the sibling-manifest discovery
//     poll, and the module-freshness join that follows a sibling-asserted
//     publicUrl. These default to STRICT DENY — an IdP or sibling app is pinned
//     by URL, so a deployment whose one lives on an internal address states it.
//
// Env: TSM_SECURITY_EGRESS_ALLOWLIST (comma-separated).
type EgressConfig struct {
	// Allowlist is the set of hostnames, IPs, or CIDR ranges that may be dialed
	// in addition to public addresses.
	//
	// Setting it REPLACES the connectors' private-range default with exactly the
	// entries given — it does not add to it. That is what lets an operator
	// tighten egress to only their own internal ranges, and it is also the
	// trap: a value added to admit an internal IdP silently withdraws RFC1918
	// from the state-source connectors unless it re-states those ranges. A
	// deployment that needs both looks like
	//
	//	allowlist: [10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, "fc00::/7", keycloak]
	//
	// Empty = the built-in private-range default for the connectors (RFC1918 +
	// IPv6-ULA allowed; metadata/loopback/link-local blocked) and strict deny
	// for the identity-module requests.
	Allowlist []string `mapstructure:"allowlist"`
}

// StateSourceConfig confines the state-source connectors whose configuration
// names a path on THIS server's filesystem — the local connector's base_path and
// the kubernetes connector's kubeconfig. Those paths arrive with the source
// record (created by an API caller holding sources:manage), so the operator, not
// the caller, decides which directories the server will serve state from.
//
// Both lists FAIL CLOSED: empty (the default) means no source may name a
// server-local path at all, rather than any path being acceptable. Set them to
// exactly the directories the deployment mounts for this purpose — e.g.
// TSM_STATESOURCE_LOCAL_ROOTS=/data/states. A base_path is accepted when it is a
// permitted root or lies beneath one, compared on path-segment boundaries and
// after symlink resolution on both sides.
// Env: TSM_STATESOURCE_LOCAL_ROOTS, TSM_STATESOURCE_KUBECONFIG_ROOTS
// (comma-separated absolute paths).
type StateSourceConfig struct {
	// LocalRoots are the directories a "local" source's base_path may live in.
	LocalRoots []string `mapstructure:"local_roots"`
	// KubeconfigRoots are the directories (or exact files) a "kubernetes"
	// source's config.kubeconfig may name. Empty leaves server + token the only
	// way to configure that connector.
	KubeconfigRoots []string `mapstructure:"kubeconfig_roots"`
}

// NotificationsConfig configures outbound alert delivery. The webhook/Slack/Teams
// channel types carry their own destination URL per channel and need nothing
// here; the email channel type sends through one shared SMTP relay configured
// below. Enabled/Events/APIKeyExpiry* gate ONLY the per-user API-key-expiry
// warning email (a personal notice, never routed through admin-configured
// channels) — mirroring terraform-registry's identical mechanism for
// cross-app parity. drift_detected/run_failed alerts are routed exclusively
// through admin-configured channels (gated by which events each channel
// subscribes to, not by a global toggle here), matching this app's existing
// behavior; there is no direct-recipients-list email path for them.
type NotificationsConfig struct {
	Enabled bool                     `mapstructure:"enabled"`
	SMTP    SMTPConfig               `mapstructure:"smtp"`
	Events  NotificationEventsConfig `mapstructure:"events"`
	// APIKeyExpiryWarningDays sets how many days before expiry the owning
	// user is warned (default 7 when <= 0). APIKeyExpiryCheckIntervalHours
	// sets how often the background job checks for expiring keys (default
	// 24 when <= 0).
	APIKeyExpiryWarningDays        int `mapstructure:"api_key_expiry_warning_days"`
	APIKeyExpiryCheckIntervalHours int `mapstructure:"api_key_expiry_check_interval_hours"`
}

// NotificationEventsConfig toggles the per-user API-key-expiry warning email.
// (terraform-registry's equivalent struct also gates several admin-facing
// broadcast events sent to a direct recipients list; this app has no such
// direct-recipients mechanism, so APIKeyExpiring is the only field needed here.)
type NotificationEventsConfig struct {
	APIKeyExpiring bool `mapstructure:"api_key_expiring"`
}

// SMTPConfig is the shared outbound mail relay backing email notification
// channels. Each email channel stores only its recipient address(es); they all
// send through this relay. Host empty (default) disables the email channel type.
// Auth is optional — an internal relay may accept unauthenticated mail — but when
// Username is set the relay must offer TLS so the password is never sent in
// the clear. Env: TSM_NOTIFICATIONS_SMTP_*.
type SMTPConfig struct {
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	From     string `mapstructure:"from"`     // envelope + header From address
	Username string `mapstructure:"username"` // optional SMTP AUTH user
	Password string `mapstructure:"password"` // optional SMTP AUTH password
	// UseTLS enables STARTTLS (port 587) or implicit TLS (port 465) when true.
	// When false the connection is deliberately kept plaintext and never
	// opportunistically upgraded, even if the relay advertises STARTTLS.
	// Mirrors terraform-registry's notifications.smtp.use_tls for parity.
	UseTLS bool `mapstructure:"use_tls"`
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

// BackupRetentionConfig bounds the state_backups table, which otherwise grows
// without limit for the lifetime of a source (#257) — every state edit snapshots
// the full pre-edit state, and those snapshots commonly embed credentials.
//
// The policy is an age cap with a keep floor: a backup is deleted only if it is
// older than MaxAge AND is not among the newest Keep backups for its
// (source_id, state_key). The floor is what makes the age cap safe — a plain age
// DELETE would wipe every restore point for a state that has simply not been
// edited lately. Enabled by default so an install that never touches config
// still gets a bounded table; the prune runs once per statesync cycle and is
// therefore gated by Workers.Enabled like the other periodic sweeps.
// Env: TSM_BACKUP_RETENTION_ENABLED, TSM_BACKUP_RETENTION_KEEP,
// TSM_BACKUP_RETENTION_MAX_AGE.
type BackupRetentionConfig struct {
	Enabled bool          `mapstructure:"enabled"`
	Keep    int           `mapstructure:"keep"`
	MaxAge  time.Duration `mapstructure:"max_age"`
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
	// TrustedProxies lists CIDRs/IPs of reverse proxies allowed to set
	// X-Forwarded-For. Empty (default) = trust no proxy.
	TrustedProxies []string `mapstructure:"trusted_proxies"`
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
	// RoleSeedOwner controls which app seeds the shared identity schema's system
	// role templates. "self" (default) = this app seeds its own store, matching
	// standalone behavior. When two apps share one identity database, exactly one
	// must own the seed ("registry" or "tsm"); otherwise they overwrite each
	// other's role scopes on every restart.
	RoleSeedOwner string `mapstructure:"role_seed_owner"` // TSM_SUITE_ROLE_SEED_OWNER: self|registry|tsm
	// IdentitySharedStore is an operator assertion that THIS app uses the shared
	// identity store + single IdP. It is advertised in the manifest; the SPA drops
	// the "you may need to sign in" hint only when both this app AND the sibling
	// assert it. Default false. Env: TSM_SUITE_IDENTITY_SHARED_STORE.
	IdentitySharedStore bool `mapstructure:"identity_shared_store"`
	// ServiceToken is a shared secret the sibling app presents (X-Suite-Service-Token)
	// for server-to-server cross-app reads (GET /consumers). Empty (default) leaves
	// that endpoint disabled; set it to the SAME value as the sibling registry's
	// TFR_SUITE_SIBLING_TOKEN to enable the "Consumed by" join.
	ServiceToken string `mapstructure:"service_token"` // TSM_SUITE_SERVICE_TOKEN
}

// ShouldSeedRoles reports whether this app (identified by app, e.g. "tsm") should
// seed system role templates given the configured RoleSeedOwner. "self" (the
// default) means every app seeds its own store; otherwise only the named owner
// seeds, so a shared identity database is written by exactly one app.
func (s SuiteConfig) ShouldSeedRoles(app string) bool {
	return s.RoleSeedOwner == "self" || s.RoleSeedOwner == app
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

// resolveIdentityDatabase fills any unset IdentityDatabase field from the primary
// Database, so an operator can point identity at a different host or database name
// while inheriting the rest (port, user, password, ssl_mode, pool sizing). Fully
// unset → identical to Database (the standalone default).
func (c *Config) resolveIdentityDatabase() {
	id := &c.IdentityDatabase
	if id.Host == "" {
		id.Host = c.Database.Host
	}
	if id.Port == 0 {
		id.Port = c.Database.Port
	}
	if id.Name == "" {
		id.Name = c.Database.Name
	}
	if id.User == "" {
		id.User = c.Database.User
	}
	if id.Password == "" {
		id.Password = c.Database.Password
	}
	if id.SSLMode == "" {
		id.SSLMode = c.Database.SSLMode
	}
	if id.MaxConnections == 0 {
		id.MaxConnections = c.Database.MaxConnections
	}
	if id.MinIdleConnections == 0 {
		id.MinIdleConnections = c.Database.MinIdleConnections
	}
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
	Enabled              bool     `mapstructure:"enabled"`
	IssuerURL            string   `mapstructure:"issuer_url"`
	ClientID             string   `mapstructure:"client_id"`
	ClientSecret         string   `mapstructure:"client_secret"`
	RedirectURL          string   `mapstructure:"redirect_url"`
	Scopes               []string `mapstructure:"scopes"`
	RequireVerifiedEmail bool     `mapstructure:"require_verified_email"`

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
	cfg.resolveIdentityDatabase()
	cfg.Authz.RoleSource = normaliseRoleSource(cfg.Authz.RoleSource)
	return &cfg, nil
}

// normaliseRoleSource lowercases and trims authz.role_source AT LOAD.
//
// NORMALISED HERE, ONCE, because the handler constructors convert this string to
// an approles.RoleSource with a plain cast — a cast cannot reject anything, and
// approles.ParseRoleSource (which does) is called only by the serve path, for the
// startup line. With `TSM_AUTHZ_ROLE_SOURCE=App` the boot therefore SUCCEEDED and
// logged `source=app`, while every repository held the un-normalised "App" and
// denied every role read as an undecided source: an authorization outage that
// announced itself as healthy, on the one setting an operator reaches for when
// something is already wrong.
//
// It normalises but does not validate. A value that is neither `app` nor
// `identity` still has to fail the boot rather than fall back, and that refusal
// belongs where the error can name the bad value and stop the process —
// cmd/server's ParseRoleSource — not in a loader that has no way to refuse.
func normaliseRoleSource(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

// validSSLModes are the libpq sslmode values the Postgres driver accepts.
var validSSLModes = map[string]struct{}{
	"disable": {}, "allow": {}, "prefer": {}, "require": {}, "verify-ca": {}, "verify-full": {},
}

// validLogLevels mirrors LoggingConfig.Level's documented set.
var validLogLevels = map[string]struct{}{
	"debug": {}, "info": {}, "warn": {}, "error": {},
}

// Validate checks cross-field configuration invariants and returns a combined
// error listing every problem, so an operator sees a misconfiguration at boot
// (fail-fast) instead of a first-use crash or silent misbehaviour. It performs
// no I/O — call it right after Load, before connecting to anything.
func (c *Config) Validate() error {
	var problems []string

	// An enabled auth provider must carry the fields it cannot function without,
	// otherwise the provider silently no-ops or panics on first login.
	if c.Auth.OIDC.Enabled {
		if c.Auth.OIDC.IssuerURL == "" {
			problems = append(problems, "auth.oidc.enabled is true but auth.oidc.issuer_url is empty")
		}
		if c.Auth.OIDC.ClientID == "" {
			problems = append(problems, "auth.oidc.enabled is true but auth.oidc.client_id is empty")
		}
	}
	if c.Auth.LDAP.Enabled {
		if c.Auth.LDAP.Host == "" {
			problems = append(problems, "auth.ldap.enabled is true but auth.ldap.host is empty")
		}
		if c.Auth.LDAP.BaseDN == "" {
			problems = append(problems, "auth.ldap.enabled is true but auth.ldap.base_dn is empty")
		}
	}

	// A configured SMTP username means a password will be sent; refuse to send it
	// over a plaintext connection (the SMTPConfig contract).
	if c.Notifications.SMTP.Username != "" && !c.Notifications.SMTP.UseTLS {
		problems = append(problems,
			"notifications.smtp.username is set but notifications.smtp.use_tls is false (the SMTP password would be sent in cleartext)")
	}

	// A zero keep floor or non-positive max age would turn the retention sweep
	// into a purge that can delete a state's last restore point.
	if c.BackupRetention.Enabled {
		if c.BackupRetention.Keep < 1 {
			problems = append(problems, fmt.Sprintf(
				"backup_retention.keep must be >= 1 when backup_retention.enabled is true, got %d", c.BackupRetention.Keep))
		}
		if c.BackupRetention.MaxAge <= 0 {
			problems = append(problems, fmt.Sprintf(
				"backup_retention.max_age must be > 0 when backup_retention.enabled is true, got %s", c.BackupRetention.MaxAge))
		}
	}

	if _, ok := validSSLModes[c.Database.SSLMode]; !ok {
		problems = append(problems, fmt.Sprintf(
			"database.ssl_mode %q is invalid (want one of disable, allow, prefer, require, verify-ca, verify-full)", c.Database.SSLMode))
	}
	// identity_database inherits database.ssl_mode when unset (resolveIdentityDatabase),
	// so only validate it when explicitly configured.
	if c.IdentityDatabase.SSLMode != "" {
		if _, ok := validSSLModes[c.IdentityDatabase.SSLMode]; !ok {
			problems = append(problems, fmt.Sprintf(
				"identity_database.ssl_mode %q is invalid (want one of disable, allow, prefer, require, verify-ca, verify-full)", c.IdentityDatabase.SSLMode))
		}
	}

	// An empty level is fine (defaults apply); a non-empty typo must not silently
	// coerce to info.
	if c.Logging.Level != "" {
		if _, ok := validLogLevels[strings.ToLower(c.Logging.Level)]; !ok {
			problems = append(problems, fmt.Sprintf("logging.level %q is invalid (want debug, info, warn, or error)", c.Logging.Level))
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
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
	v.SetDefault("server.trusted_proxies", []string{})

	v.SetDefault("database.host", "localhost")
	v.SetDefault("database.port", 5432)
	v.SetDefault("database.name", "terraform_state_manager")
	v.SetDefault("database.user", "tsm")
	v.SetDefault("database.password", "")
	v.SetDefault("database.ssl_mode", "prefer")
	v.SetDefault("database.max_connections", 25)
	v.SetDefault("database.min_idle_connections", 5)

	// Identity database — empty defaults so each field falls back to the app
	// database (above) unless TSM_IDENTITY_DATABASE_* overrides it. Registering
	// the keys lets AutomaticEnv bind the nested overrides.
	v.SetDefault("identity_database.host", "")
	v.SetDefault("identity_database.port", 0)
	v.SetDefault("identity_database.name", "")
	v.SetDefault("identity_database.user", "")
	v.SetDefault("identity_database.password", "")
	v.SetDefault("identity_database.ssl_mode", "")
	v.SetDefault("identity_database.max_connections", 0)
	v.SetDefault("identity_database.min_idle_connections", 0)

	v.SetDefault("logging.level", "info")
	v.SetDefault("logging.format", "json")

	v.SetDefault("telemetry.metrics.enabled", true)
	v.SetDefault("telemetry.metrics.prometheus_port", 9090)

	v.SetDefault("workers.enabled", true)

	// Organization-partition rollout (#393). Off: Phase 2b observes nothing
	// unless an operator asks it to, and observing costs a second query per
	// read. See TenancyConfig.
	v.SetDefault("tenancy.dual_read", false)

	// Drift-run reconciler: fail a dispatched run whose CI job never called back
	// after run_ttl, sweeping every reconcile_interval.
	v.SetDefault("drift.run_ttl", 2*time.Hour)
	v.SetDefault("drift.reconcile_interval", 5*time.Minute)

	// Backup retention: keep the newest 20 backups per state regardless of age,
	// and drop anything older than 90 days beyond that floor.
	v.SetDefault("backup_retention.enabled", true)
	v.SetDefault("backup_retention.keep", 20)
	v.SetDefault("backup_retention.max_age", 90*24*time.Hour)

	v.SetDefault("auth.oidc.enabled", false)
	v.SetDefault("auth.oidc.issuer_url", "")
	v.SetDefault("auth.oidc.client_id", "")
	v.SetDefault("auth.oidc.client_secret", "")
	v.SetDefault("auth.oidc.redirect_url", "")
	v.SetDefault("auth.oidc.scopes", []string{"openid", "email", "profile"})
	v.SetDefault("auth.oidc.require_verified_email", true)
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

	// Notifications — shared outbound SMTP relay backing the "email" channel type.
	// Empty host (default) leaves the email channel type disabled.
	v.SetDefault("notifications.enabled", false)
	v.SetDefault("notifications.smtp.host", "")
	v.SetDefault("notifications.smtp.port", 587)
	v.SetDefault("notifications.smtp.from", "")
	v.SetDefault("notifications.smtp.username", "")
	v.SetDefault("notifications.smtp.password", "")
	v.SetDefault("notifications.smtp.use_tls", true)
	v.SetDefault("notifications.api_key_expiry_warning_days", 7)
	v.SetDefault("notifications.api_key_expiry_check_interval_hours", 24)
	v.SetDefault("notifications.events.api_key_expiring", true)

	// Per-app authorization. "app" is Phase 3b (this application's own tables);
	// "identity" is the Phase 3a rollback position. See AuthzConfig.
	v.SetDefault("authz.role_source", "app")
	v.SetDefault("authz.drift_interval", 15*time.Minute)

	// Suite runtime discovery
	v.SetDefault("suite.sibling_url", "")
	v.SetDefault("suite.poll_interval", 60*time.Second)
	v.SetDefault("suite.role_seed_owner", "self")
	v.SetDefault("suite.identity_shared_store", false)
	v.SetDefault("suite.service_token", "")

	// SSRF egress allow-list. Empty = the built-in private-range default for the
	// state-source connectors (see statesource/egress.go) and strict deny for
	// the identity module's own outbound requests (see internal/egress). Setting
	// it REPLACES the connector default rather than adding to it.
	v.SetDefault("security.egress.allowlist", []string{})

	// Roots the state sources that name a server-local path are confined to.
	// Empty = fail closed: no such source can be created (see
	// statesource/roots.go).
	v.SetDefault("statesource.local_roots", []string{})
	v.SetDefault("statesource.kubeconfig_roots", []string{})
}
