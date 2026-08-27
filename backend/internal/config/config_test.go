package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("expected default server port 8080, got %d", cfg.Server.Port)
	}
	if cfg.Database.Name != "terraform_state_manager" {
		t.Errorf("unexpected default database name: %q", cfg.Database.Name)
	}
	if got := cfg.Server.GetAddress(); got != "0.0.0.0:8080" {
		t.Errorf("unexpected address: %q", got)
	}
	// Workers default ON: single-replica installs need no extra configuration.
	if !cfg.Workers.Enabled {
		t.Error("workers must default to enabled")
	}
}

func TestWorkersEnvGate(t *testing.T) {
	// API replicas in multi-replica deployments disable the periodic workers.
	t.Setenv("TSM_WORKERS_ENABLED", "false")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Workers.Enabled {
		t.Error("TSM_WORKERS_ENABLED=false must disable workers")
	}
}

// The #393 Phase 2b flag: off unless asked for, because it costs a second query
// per read and a flag whose default changed behaviour would not be a flag.
func TestSMTPDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	// Email channel disabled by default (no relay host), submission port assumed.
	if cfg.Notifications.SMTP.Host != "" {
		t.Errorf("default SMTP host = %q, want empty (email disabled)", cfg.Notifications.SMTP.Host)
	}
	if cfg.Notifications.SMTP.Port != 587 {
		t.Errorf("default SMTP port = %d, want 587", cfg.Notifications.SMTP.Port)
	}
}

func TestSMTPEnvBinding(t *testing.T) {
	t.Setenv("TSM_NOTIFICATIONS_SMTP_HOST", "smtp.internal")
	t.Setenv("TSM_NOTIFICATIONS_SMTP_FROM", "tsm@example.com")
	t.Setenv("TSM_NOTIFICATIONS_SMTP_USERNAME", "relay-user")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Notifications.SMTP.Host != "smtp.internal" {
		t.Errorf("SMTP host = %q, want smtp.internal", cfg.Notifications.SMTP.Host)
	}
	if cfg.Notifications.SMTP.From != "tsm@example.com" {
		t.Errorf("SMTP from = %q, want tsm@example.com", cfg.Notifications.SMTP.From)
	}
	if cfg.Notifications.SMTP.Username != "relay-user" {
		t.Errorf("SMTP username = %q, want relay-user", cfg.Notifications.SMTP.Username)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("TSM_SERVER_PORT", "9999")
	t.Setenv("TSM_DATABASE_HOST", "db.internal")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Server.Port != 9999 {
		t.Errorf("expected env-overridden port 9999, got %d", cfg.Server.Port)
	}
	if cfg.Database.Host != "db.internal" {
		t.Errorf("expected env-overridden host, got %q", cfg.Database.Host)
	}
}

func TestGetDSN(t *testing.T) {
	d := DatabaseConfig{Host: "h", Port: 5432, User: "u", Password: "p", Name: "n", SSLMode: "disable"}
	want := "host=h port=5432 user=u password=p dbname=n sslmode=disable"
	if got := d.GetDSN(); got != want {
		t.Errorf("GetDSN() = %q, want %q", got, want)
	}
}

func TestGetDSNWithSearchPath(t *testing.T) {
	d := DatabaseConfig{Host: "h", Port: 5432, User: "u", Password: "p", Name: "n", SSLMode: "disable"}
	got := d.GetDSNWithSearchPath("identity,public")
	want := "host=h port=5432 user=u password=p dbname=n sslmode=disable options='-c search_path=identity,public'"
	if got != want {
		t.Errorf("GetDSNWithSearchPath() = %q, want %q", got, want)
	}
}

func TestOIDCEnvBinding(t *testing.T) {
	t.Setenv("TSM_AUTH_OIDC_ENABLED", "true")
	t.Setenv("TSM_AUTH_OIDC_ISSUER_URL", "http://keycloak:8180/realms/terraform-state-manager")
	t.Setenv("TSM_AUTH_OIDC_CLIENT_ID", "terraform-state-manager")
	t.Setenv("TSM_AUTH_OIDC_DEFAULT_ROLE", "admin")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !cfg.Auth.OIDC.Enabled {
		t.Error("expected OIDC enabled via env")
	}
	if cfg.Auth.OIDC.IssuerURL != "http://keycloak:8180/realms/terraform-state-manager" {
		t.Errorf("unexpected issuer: %q", cfg.Auth.OIDC.IssuerURL)
	}
	if cfg.Auth.OIDC.ClientID != "terraform-state-manager" {
		t.Errorf("unexpected client_id: %q", cfg.Auth.OIDC.ClientID)
	}
	if cfg.Auth.OIDC.DefaultRole != "admin" {
		t.Errorf("unexpected default_role: %q", cfg.Auth.OIDC.DefaultRole)
	}
	if len(cfg.Auth.OIDC.Scopes) == 0 {
		t.Error("expected default OIDC scopes to be preserved")
	}
}

func TestSuiteConfig_ShouldSeedRoles(t *testing.T) {
	cases := []struct {
		owner string
		app   string
		want  bool
	}{
		{"self", "tsm", true},      // standalone default: every app seeds its own
		{"tsm", "tsm", true},       // this app is the designated owner
		{"registry", "tsm", false}, // sibling owns the shared seed → skip
		{"self", "registry", true}, // "self" is app-agnostic
		{"", "tsm", false},         // unset (shouldn't happen post-default) → not owner
	}
	for _, c := range cases {
		if got := (SuiteConfig{RoleSeedOwner: c.owner}).ShouldSeedRoles(c.app); got != c.want {
			t.Errorf("ShouldSeedRoles(owner=%q, app=%q) = %v, want %v", c.owner, c.app, got, c.want)
		}
	}
}

func TestRoleSeedOwnerDefault(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Suite.RoleSeedOwner != "self" {
		t.Errorf("default Suite.RoleSeedOwner = %q, want self", cfg.Suite.RoleSeedOwner)
	}
}

func TestRoleSeedOwnerEnvOverride(t *testing.T) {
	t.Setenv("TSM_SUITE_ROLE_SEED_OWNER", "tsm")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Suite.RoleSeedOwner != "tsm" {
		t.Errorf("Suite.RoleSeedOwner = %q, want tsm (TSM_SUITE_ROLE_SEED_OWNER override)", cfg.Suite.RoleSeedOwner)
	}
}

func TestIdentityDatabaseDefaultsToAppDB(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	// Unset → the identity database is byte-for-byte the app database.
	if cfg.IdentityDatabase != cfg.Database {
		t.Errorf("IdentityDatabase = %+v, want == Database %+v", cfg.IdentityDatabase, cfg.Database)
	}
}

func TestIdentityDatabasePartialOverride(t *testing.T) {
	// Override only host + database name; everything else inherits the app DB —
	// the common "shared identity DB on the same server" case.
	t.Setenv("TSM_IDENTITY_DATABASE_HOST", "identity.db.internal")
	t.Setenv("TSM_IDENTITY_DATABASE_NAME", "terraform_suite_identity")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.IdentityDatabase.Host != "identity.db.internal" {
		t.Errorf("IdentityDatabase.Host = %q, want identity.db.internal", cfg.IdentityDatabase.Host)
	}
	if cfg.IdentityDatabase.Name != "terraform_suite_identity" {
		t.Errorf("IdentityDatabase.Name = %q, want terraform_suite_identity", cfg.IdentityDatabase.Name)
	}
	// Inherited from the app database (unset identity fields fall back).
	if cfg.IdentityDatabase.User != cfg.Database.User {
		t.Errorf("IdentityDatabase.User = %q, want inherited %q", cfg.IdentityDatabase.User, cfg.Database.User)
	}
	if cfg.IdentityDatabase.Port != cfg.Database.Port {
		t.Errorf("IdentityDatabase.Port = %d, want inherited %d", cfg.IdentityDatabase.Port, cfg.Database.Port)
	}
	if cfg.IdentityDatabase.SSLMode != cfg.Database.SSLMode {
		t.Errorf("IdentityDatabase.SSLMode = %q, want inherited %q", cfg.IdentityDatabase.SSLMode, cfg.Database.SSLMode)
	}
	if cfg.IdentityDatabase.MaxConnections != cfg.Database.MaxConnections {
		t.Errorf("IdentityDatabase.MaxConnections = %d, want inherited %d", cfg.IdentityDatabase.MaxConnections, cfg.Database.MaxConnections)
	}
}

func TestValidate(t *testing.T) {
	// The default config must pass — otherwise every boot fails.
	base, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("default config failed Validate: %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantErr bool
	}{
		{"oidc enabled without issuer", func(c *Config) { c.Auth.OIDC.Enabled = true; c.Auth.OIDC.ClientID = "x" }, true},
		{"oidc enabled without client id", func(c *Config) { c.Auth.OIDC.Enabled = true; c.Auth.OIDC.IssuerURL = "https://idp" }, true},
		{"oidc enabled complete", func(c *Config) {
			c.Auth.OIDC.Enabled, c.Auth.OIDC.IssuerURL, c.Auth.OIDC.ClientID = true, "https://idp", "x"
		}, false},
		{"ldap enabled without host", func(c *Config) { c.Auth.LDAP.Enabled = true; c.Auth.LDAP.BaseDN = "dc=x" }, true},
		{"ldap enabled without base_dn", func(c *Config) { c.Auth.LDAP.Enabled = true; c.Auth.LDAP.Host = "ldap" }, true},
		{"ldap disabled ignores missing fields", func(c *Config) { c.Auth.LDAP.Enabled = false }, false},
		{"smtp user without tls", func(c *Config) { c.Notifications.SMTP.Username = "u"; c.Notifications.SMTP.UseTLS = false }, true},
		{"smtp user with tls", func(c *Config) { c.Notifications.SMTP.Username = "u"; c.Notifications.SMTP.UseTLS = true }, false},
		{"bad ssl_mode", func(c *Config) { c.Database.SSLMode = "sorta" }, true},
		{"good ssl_mode", func(c *Config) { c.Database.SSLMode = "verify-full" }, false},
		{"bad identity ssl_mode", func(c *Config) { c.IdentityDatabase.SSLMode = "nope" }, true},
		{"bad log level", func(c *Config) { c.Logging.Level = "verbose" }, true},
		{"empty log level ok", func(c *Config) { c.Logging.Level = "" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := Load("")
			tc.mutate(c)
			err := c.Validate()
			if tc.wantErr && err == nil {
				t.Error("expected a validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestBackupRetentionDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	// Prune defaults ON: an install that never touches config still gets a
	// bounded state_backups table (#257).
	if !cfg.BackupRetention.Enabled {
		t.Error("backup retention must default to enabled")
	}
	if cfg.BackupRetention.Keep != 20 {
		t.Errorf("default keep = %d, want 20", cfg.BackupRetention.Keep)
	}
	if cfg.BackupRetention.MaxAge != 90*24*time.Hour {
		t.Errorf("default max_age = %s, want 2160h", cfg.BackupRetention.MaxAge)
	}
}

func TestBackupRetentionEnvOverride(t *testing.T) {
	t.Setenv("TSM_BACKUP_RETENTION_ENABLED", "false")
	t.Setenv("TSM_BACKUP_RETENTION_KEEP", "5")
	t.Setenv("TSM_BACKUP_RETENTION_MAX_AGE", "24h")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.BackupRetention.Enabled {
		t.Error("TSM_BACKUP_RETENTION_ENABLED=false must disable the prune")
	}
	if cfg.BackupRetention.Keep != 5 {
		t.Errorf("keep = %d, want 5", cfg.BackupRetention.Keep)
	}
	if cfg.BackupRetention.MaxAge != 24*time.Hour {
		t.Errorf("max_age = %s, want 24h", cfg.BackupRetention.MaxAge)
	}
}

// A keep floor of 0 would let the age cap delete a state's last restore point,
// so Validate rejects it.
func TestBackupRetentionValidation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"keep zero", func(c *Config) { c.BackupRetention.Keep = 0 }, true},
		{"keep negative", func(c *Config) { c.BackupRetention.Keep = -1 }, true},
		{"max_age zero", func(c *Config) { c.BackupRetention.MaxAge = 0 }, true},
		{"keep one ok", func(c *Config) { c.BackupRetention.Keep = 1 }, false},
		{"disabled skips checks", func(c *Config) {
			c.BackupRetention.Enabled = false
			c.BackupRetention.Keep = 0
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := Load("")
			tc.mutate(c)
			err := c.Validate()
			if tc.wantErr && err == nil {
				t.Error("expected a validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestStateSourceRootsEnvBinding(t *testing.T) {
	// Default: no roots at all — the connectors that name a server-local path
	// (local base_path, kubernetes kubeconfig) are unavailable until an operator
	// says which directories they may use.
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(cfg.StateSource.LocalRoots) != 0 || len(cfg.StateSource.KubeconfigRoots) != 0 {
		t.Errorf("state-source roots must default to empty, got %v / %v",
			cfg.StateSource.LocalRoots, cfg.StateSource.KubeconfigRoots)
	}

	t.Setenv("TSM_STATESOURCE_LOCAL_ROOTS", "/data/states,/srv/tfstate")
	t.Setenv("TSM_STATESOURCE_KUBECONFIG_ROOTS", "/etc/tsm/kubeconfigs")
	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	want := []string{"/data/states", "/srv/tfstate"}
	if len(cfg.StateSource.LocalRoots) != len(want) {
		t.Fatalf("local roots = %v, want %v", cfg.StateSource.LocalRoots, want)
	}
	for i, w := range want {
		if cfg.StateSource.LocalRoots[i] != w {
			t.Errorf("local root %d = %q, want %q", i, cfg.StateSource.LocalRoots[i], w)
		}
	}
	if len(cfg.StateSource.KubeconfigRoots) != 1 || cfg.StateSource.KubeconfigRoots[0] != "/etc/tsm/kubeconfigs" {
		t.Errorf("kubeconfig roots = %v, want [/etc/tsm/kubeconfigs]", cfg.StateSource.KubeconfigRoots)
	}
}

// TestRoleSourceIsNormalisedAtLoad guards the shape that made the rollback lever
// fail silently.
//
// The handler constructors turn authz.role_source into an approles.RoleSource
// with a plain CAST. A cast cannot reject anything, and the one function that can
// (approles.ParseRoleSource) is called only by the serve path, for the startup
// line — which it derives from its OWN normalised copy. So `TSM_AUTHZ_ROLE_SOURCE=App`
// booted, logged `source=app`, and left every repository holding "App": an
// undecided source, which denies every role read. An authorization outage that
// announced itself as healthy, on the setting an operator reaches for when
// something is already wrong.
//
// Asserted on the loaded VALUE rather than on "it did not error", because the
// broken version did not error either.
func TestRoleSourceIsNormalisedAtLoad(t *testing.T) {
	for _, c := range []struct{ set, want string }{
		{"app", "app"},
		{"App", "app"},
		{"IDENTITY", "identity"},
		{"  identity  ", "identity"},
	} {
		t.Setenv("TSM_AUTHZ_ROLE_SOURCE", c.set)
		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load with role_source=%q: %v", c.set, err)
		}
		if cfg.Authz.RoleSource != c.want {
			t.Errorf("role_source %q loaded as %q, want %q — the cast at every construction site "+
				"would produce a repository that denies every role read", c.set, cfg.Authz.RoleSource, c.want)
		}
	}

	// A value that is neither is left alone rather than coerced: refusing it is
	// cmd/server's job, where the error can name it and stop the process. Silently
	// mapping it to a default is how a typo becomes an authorization change.
	t.Setenv("TSM_AUTHZ_ROLE_SOURCE", "shared")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Authz.RoleSource != "shared" {
		t.Errorf("an unknown role_source was coerced to %q instead of being left for the boot to refuse", cfg.Authz.RoleSource)
	}
}

// The default is the Phase 3b position, and it is asserted rather than assumed:
// a default of "identity" would ship a build that reads the shared schema while
// every document in the repository says it does not.
func TestRoleSourceDefaultsToThisApplicationsTables(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Authz.RoleSource != "app" {
		t.Errorf("authz.role_source defaults to %q, want \"app\"", cfg.Authz.RoleSource)
	}
	if cfg.Authz.DriftInterval <= 0 {
		t.Errorf("authz.drift_interval defaults to %v, so the standing detector never runs", cfg.Authz.DriftInterval)
	}
}

// TestAuditRetentionShipsDisabled pins the decision, not the value.
//
// backup_retention ships ENABLED and audit_retention ships DISABLED, and the
// asymmetry is deliberate: backup pruning is safe on by default because of its
// KEEP FLOOR -- a state untouched for max_age still keeps its newest `keep`
// restore points, so the age cap can never take the last copy of anything. An
// age-based audit sweep has no equivalent floor; a quiet month is simply gone.
//
// So an enabled default would delete audit history on the first boot after an
// upgrade, before any operator had chosen a retention period. Deletion is
// irreversible; unbounded growth, which is the CURRENT behaviour, is not.
func TestAuditRetentionShipsDisabled(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.AuditRetention.Enabled {
		t.Error("audit_retention ships ENABLED.\n" +
			"That deletes audit history on the first boot after an upgrade, before any operator " +
			"has chosen a retention period -- and docs/capacity-planning.md has told operators " +
			"these logs are never pruned, so some will have built external policies against that.")
	}
	if days := cfg.AuditRetention.RetentionDays; days != 0 {
		t.Errorf("audit_retention.retention_days defaults to %d; it must be 0 so that enabling "+
			"without stating a period is a config error rather than a surprise purge", days)
	}
	// The contrast, asserted so the two cannot silently converge.
	if !cfg.BackupRetention.Enabled {
		t.Error("backup_retention is no longer enabled by default; if that changed deliberately, " +
			"the asymmetry argued above needs revisiting")
	}
}

// TestEnablingAuditRetentionWithoutAPeriodIsRefused.
//
// Half-enabling must fail at load. A zero or negative retention puts the cutoff
// at or after now, which deletes every entry no legal hold covers.
func TestEnablingAuditRetentionWithoutAPeriodIsRefused(t *testing.T) {
	for _, days := range []int{0, -1} {
		cfg := &Config{}
		cfg.AuditRetention = AuditRetentionConfig{
			Enabled: true, RetentionDays: days,
			Interval: time.Hour, BatchSize: 100, MaxBatches: 10,
		}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "audit_retention.retention_days") {
			t.Errorf("retention_days=%d with enabled=true was not refused: %v", days, err)
		}
	}
}
