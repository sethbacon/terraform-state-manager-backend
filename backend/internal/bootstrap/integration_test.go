//go:build integration

// The half of the startup sequence only a real PostgreSQL can establish.
//
// internal/approles' own integration suite exercises Reconcile with a LOCAL copy
// of the app-side seed, because internal/bootstrap imports that package and the
// real seed cannot be called from its tests. This file closes that gap from the
// other side: it runs bootstrap.Run — the actual function cmd/server calls, with
// the actual seedRoleTemplates — against two real connections, and asserts on the
// rows.
//
// What it is really testing is an ORDERING. Reconcile adopts identity's role
// templates (bringing identity's uuids across, so an assignment restated from
// identity resolves) and only then writes this build's own definitions by name.
// Reversed, a fresh install mints its own uuid for `editor`, the adopt pass finds
// identity's `editor` under a different uuid, the name is released, and this
// build's scopes are replaced by identity's — silently, on the table that decides
// authorization. Both spellings compile and neither is visible in a unit test
// with a mocked connection.
//
// Run with:
//
//	TEST_DATABASE_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable' \
//	  go test -tags integration ./internal/bootstrap/...
package bootstrap

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	identity "github.com/sethbacon/terraform-suite-identity/identity"
	idmodels "github.com/sethbacon/terraform-suite-identity/identity/models"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	appdb "github.com/terraform-state-manager/terraform-state-manager/internal/db"
)

// testDatabaseName is this suite's OWN database, for the reason
// internal/approles' is: package binaries run concurrently and this suite drops
// and re-creates its schema.
const testDatabaseName = "tsm_bootstrap_test"

type env struct {
	appDB, identityDB *sql.DB
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping PostgreSQL integration test")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", dsn, err)
	}
	defer func() { _ = admin.Close() }()
	if err := admin.Ping(); err != nil {
		t.Skipf("TEST_DATABASE_URL is not reachable (%v); skipping", err)
	}
	for _, stmt := range []string{
		`DROP DATABASE IF EXISTS ` + pgx.Identifier{testDatabaseName}.Sanitize() + ` WITH (FORCE)`,
		`CREATE DATABASE ` + pgx.Identifier{testDatabaseName}.Sanitize(),
	} {
		if _, err := admin.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	e := &env{appDB: connect(t, dsn, ""), identityDB: connect(t, dsn, "identity,public")}
	if err := identity.RunMigrations(e.identityDB, "up"); err != nil {
		t.Fatalf("identity migrations: %v", err)
	}
	if err := appdb.RunMigrations(e.appDB, "up"); err != nil {
		t.Fatalf("app migrations: %v", err)
	}
	return e
}

func connect(t *testing.T, dsn, searchPath string) *sql.DB {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	parsed.Path = "/" + testDatabaseName
	if searchPath != "" {
		q := parsed.Query()
		q.Set("search_path", searchPath)
		parsed.RawQuery = q.Encode()
	}
	db, err := sql.Open("pgx", parsed.String())
	if err != nil {
		t.Fatalf("open %s: %v", parsed.Redacted(), err)
	}
	db.SetMaxOpenConns(10)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping %s: %v", parsed.Redacted(), err)
	}
	return db
}

// appScopes reads what THIS APPLICATION's role_templates grants a role name.
func (e *env) appScopes(t *testing.T, name string) []string {
	t.Helper()
	var raw []byte
	if err := e.appDB.QueryRow(`SELECT scopes FROM role_templates WHERE name = $1`, name).Scan(&raw); err != nil {
		t.Fatalf("read this application's %q scopes: %v", name, err)
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %q scopes: %v", name, err)
	}
	sort.Strings(out)
	return out
}

func (e *env) appTemplateID(t *testing.T, name string) string {
	t.Helper()
	var id string
	if err := e.appDB.QueryRow(`SELECT id::text FROM role_templates WHERE name = $1`, name).Scan(&id); err != nil {
		t.Fatalf("read this application's %q id: %v", name, err)
	}
	return id
}

func (e *env) identityTemplateID(t *testing.T, name string) string {
	t.Helper()
	var id string
	if err := e.identityDB.QueryRow(
		`SELECT id::text FROM identity.role_templates WHERE name = $1`, name).Scan(&id); err != nil {
		t.Fatalf("read identity's %q id: %v", name, err)
	}
	return id
}

func buildScopes(t *testing.T, name string) []string {
	t.Helper()
	for _, rt := range auth.AppRoleTemplates() {
		if rt.Name == name {
			out := append([]string(nil), rt.Scopes...)
			sort.Strings(out)
			return out
		}
	}
	t.Fatalf("this build defines no role named %q", name)
	return nil
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A COUPLED DEPLOYMENT, WHICH IS THE CASE THAT MATTERS.
//
// suite.role_seed_owner names the sibling, so this application does not seed the
// shared schema and identity's `editor` carries the REGISTRY's scopes. Phase 3a
// mirrored that verbatim and authorized against it. After this phase, this
// application's own `editor` must grant what THIS build defines — while carrying
// identity's uuid, so the assignment restated from identity still resolves, and
// while leaving the shared schema untouched for the sibling and the rollback.
func TestIntegrationRunSeedsThisApplicationsOwnScopes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// The sibling's definition of a name this build also defines.
	if _, err := e.identityDB.Exec(`
		INSERT INTO identity.role_templates (id, name, display_name, description, scopes, is_system, created_at, updated_at)
		VALUES (gen_random_uuid(), 'editor', 'Editor', NULL, '["modules:read","providers:read"]'::jsonb, true, now(), now())`,
	); err != nil {
		t.Fatalf("seed the sibling's editor: %v", err)
	}
	identityEditorID := e.identityTemplateID(t, "editor")

	// seedRoles=false: coupled, the sibling owns the shared seed.
	if err := Run(ctx, e.identityDB, e.appDB, false); err != nil {
		t.Fatalf("bootstrap.Run: %v", err)
	}

	if got, want := e.appScopes(t, "editor"), buildScopes(t, "editor"); !equal(got, want) {
		t.Fatalf("this application's `editor` grants %v, want this build's %v. The seed either did not run, "+
			"or ran BEFORE the adopt pass and had its row replaced by identity's.", got, want)
	}
	if got := e.appTemplateID(t, "editor"); got != identityEditorID {
		t.Fatalf("this application's `editor` carries id %s, want identity's %s. An assignment restated from "+
			"identity would reference a template id this table does not have.", got, identityEditorID)
	}

	// THE SHARED SCHEMA IS UNTOUCHED. seedRoles=false means this application must
	// not write it at all, which is what stops two apps overwriting each other
	// while identity.role_templates still exists (it does, until Phase 4).
	var identityScopes []byte
	if err := e.identityDB.QueryRow(
		`SELECT scopes FROM identity.role_templates WHERE name = 'editor'`).Scan(&identityScopes); err != nil {
		t.Fatalf("read identity's editor: %v", err)
	}
	if !strings.Contains(string(identityScopes), "modules:read") || !strings.Contains(string(identityScopes), "providers:read") {
		t.Fatalf("the shared schema's `editor` was rewritten: %s", identityScopes)
	}

	// Every role this build defines is present, so no membership can resolve to a
	// role this application has no definition for.
	for _, rt := range auth.AppRoleTemplates() {
		if got, want := e.appScopes(t, rt.Name), buildScopes(t, rt.Name); !equal(got, want) {
			t.Errorf("this application's %q grants %v, want %v", rt.Name, got, want)
		}
	}
}

// A FRESH STANDALONE INSTALL, where the shared seed runs too. The two seeds write
// the same scopes, so nothing here should differ — but the ids must still agree,
// which is the thing the ordering buys and which a scope comparison alone would
// not notice.
func TestIntegrationRunKeepsIdentitysIDsOnAFreshInstall(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	if err := Run(ctx, e.identityDB, e.appDB, true); err != nil {
		t.Fatalf("bootstrap.Run: %v", err)
	}
	for _, rt := range auth.AppRoleTemplates() {
		if got, want := e.appTemplateID(t, rt.Name), e.identityTemplateID(t, rt.Name); got != want {
			t.Errorf("%q: this application holds id %s, identity holds %s — an assignment copied from identity "+
				"would not resolve here", rt.Name, got, want)
		}
	}

	// And a membership seeded into identity resolves through this application's
	// tables after a second boot, which is what "the ids agree" is FOR: the
	// assignment pass copies identity's role_template_id straight across, so a
	// mismatch would be a foreign-key violation or a NULL role rather than a
	// number nobody looks at.
	var orgID string
	if err := e.identityDB.QueryRow(
		`SELECT id::text FROM identity.organizations WHERE name = 'default'`).Scan(&orgID); err != nil {
		t.Fatalf("read the default organization bootstrap.Run ensures: %v", err)
	}
	user := &idmodels.User{Email: "seeded@example.com", Name: "seeded"}
	if err := idstore.NewUserRepository(e.identityDB).CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := idstore.NewOrganizationRepository(e.identityDB).
		AddMemberWithParams(ctx, orgID, user.ID, "editor", idstore.OrgScopeAllOrganizations()); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	if err := Run(ctx, e.identityDB, e.appDB, true); err != nil {
		t.Fatalf("second bootstrap.Run: %v", err)
	}

	var mirrored string
	if err := e.appDB.QueryRow(
		`SELECT role_template_id::text FROM organization_member_roles WHERE organization_id = $1 AND user_id = $2`,
		orgID, user.ID).Scan(&mirrored); err != nil {
		t.Fatalf("read this application's role record: %v", err)
	}
	if mirrored != e.appTemplateID(t, "editor") {
		t.Fatalf("the restated assignment points at %s, which is not this application's editor (%s)",
			mirrored, e.appTemplateID(t, "editor"))
	}
}

// TestIntegrationRunRefusesADriftedChannelTable proves the assertion added for
// #440 actually fires.
//
// notification_channels is THIS repository's table (000009); identity/notify
// supplies the canonical DDL and the check, and nothing else stands between a
// drifted local schema and a failure a customer finds. Before this call existed,
// a missing column surfaced as a runtime SQL error on the first notification —
// or, once a scoped read lands, as a silently empty channel list, which reads as
// "nobody configured any" rather than "this deployment is broken".
//
// The damage is done AFTER the migrations, so this asserts the check and not the
// migration: the schema is correct, then one column the DAO's statements require
// is removed, and Run must refuse to start.
func TestIntegrationRunRefusesADriftedChannelTable(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// Control. The same call on the schema the migrations produced must SUCCEED,
	// or the negative case below proves only that Run fails for some reason.
	if err := bootstrapRun(ctx, e, true); err != nil {
		t.Fatalf("Run against the migrated schema: %v", err)
	}

	if _, err := e.appDB.ExecContext(ctx,
		`ALTER TABLE notification_channels DROP COLUMN encrypted_target`); err != nil {
		t.Fatalf("drift the channel table: %v", err)
	}

	err := bootstrapRun(ctx, e, true)
	if err == nil {
		t.Fatal("Run succeeded against a notification_channels table missing a column its own " +
			"statements select. The startup assertion is not being called, and the failure has " +
			"been deferred to whenever someone next tries to notify.")
	}
	if !strings.Contains(err.Error(), "notification_channels") {
		t.Errorf("Run failed, but not in a way that names the table: %v\n"+
			"An operator reading this has to know which migration to look at.", err)
	}
}

// bootstrapRun is Run with this suite's arguments, so the control and the
// negative case cannot drift apart.
func bootstrapRun(ctx context.Context, e *env, seedRoles bool) error {
	return Run(ctx, e.identityDB, e.appDB, seedRoles)
}
