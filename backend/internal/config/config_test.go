package config

import "testing"

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
