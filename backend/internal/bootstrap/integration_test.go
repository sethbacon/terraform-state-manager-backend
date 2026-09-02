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
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
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
// application's own `editor` must grant what THIS build defines, while leaving
// the shared schema untouched for the sibling and the rollback.
//
// THE IDS ARE ALLOWED TO DIFFER HERE NOW. The adopt pass that used to copy
// identity's uuid across was the boot-time read of identity.role_templates,
// and it is retired: on a fresh install beside a sibling-owned identity seed
// the two schemas keep separate uuids for the same name, the write paths
// bridge by NAME, and the drift comparison's `mismatched` kind documents the
// standing divergence until Phase 4 drops identity's role column.
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

	// seedRoles=false: coupled, the sibling owns the shared seed.
	if err := Run(ctx, e.identityDB, e.appDB, false, repositories.NewUserTokenRevocationRepository(e.appDB)); err != nil {
		t.Fatalf("bootstrap.Run: %v", err)
	}

	if got, want := e.appScopes(t, "editor"), buildScopes(t, "editor"); !equal(got, want) {
		t.Fatalf("this application's `editor` grants %v, want this build's %v. The seed did not run, or "+
			"identity's definition was copied over it.", got, want)
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

// A FRESH STANDALONE INSTALL, where the shared seed runs too. The ids must
// agree — and since the adopt pass was retired, the agreement flows the OTHER
// WAY: the app defines its roles first (minting the uuids), and the
// identity-side seed restates those uuids into identity.role_templates. That
// alignment is what keeps the drift comparison's id check quiet on the default
// topology, without this application ever reading identity's copy.
func TestIntegrationRunSeedsIdentityWithTheAppsIDsOnAFreshInstall(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	if err := Run(ctx, e.identityDB, e.appDB, true, repositories.NewUserTokenRevocationRepository(e.appDB)); err != nil {
		t.Fatalf("bootstrap.Run: %v", err)
	}
	for _, rt := range auth.AppRoleTemplates() {
		if got, want := e.appTemplateID(t, rt.Name), e.identityTemplateID(t, rt.Name); got != want {
			t.Errorf("%q: this application holds id %s, identity holds %s — an assignment copied from identity "+
				"would not resolve here", rt.Name, got, want)
		}
	}

	// A membership seeded into identity behind this application's back arrives
	// at the second boot as a membership FACT with no role: the reconcile no
	// longer copies identity's role_template_id, so the role a member holds
	// here is only ever what this application granted.
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
	if err := Run(ctx, e.identityDB, e.appDB, true, repositories.NewUserTokenRevocationRepository(e.appDB)); err != nil {
		t.Fatalf("second bootstrap.Run: %v", err)
	}

	var mirrored sql.NullString
	if err := e.appDB.QueryRow(
		`SELECT role_template_id::text FROM organization_member_roles WHERE organization_id = $1 AND user_id = $2`,
		orgID, user.ID).Scan(&mirrored); err != nil {
		t.Fatalf("read this application's role record: %v", err)
	}
	if mirrored.Valid {
		t.Fatalf("the confirmed membership carries role %s: identity's role opinion was restated over "+
			"this application's tables, which is exactly the read this phase retired", mirrored.String)
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

// TestIntegrationRunRefusesAChannelTableWithNoOrganizationColumn is the sibling
// of the test above, for the column the #393 read flip made load-bearing.
//
// It is a SEPARATE assertion and not another DROP COLUMN inside that test,
// because the two failures are different faults with different remedies: a
// missing encrypted_target is a broken table, while a missing organization_id is
// a deployment that never ran the partition migration. The library reports them
// through different checks and names different fixes, and a test that merged
// them could pass while only one of the two calls existed.
//
// WHAT HAPPENS WITHOUT THE CHECK, verified by mutation rather than assumed:
// Run still fails, but at tenancy.Backfill, with "backfill
// notification_channels.organization_id: column does not exist". So the boot was
// already refused and the value here is WHERE and WHAT IT NAMES — a schema
// assertion at the top of the sequence, naming the partition migration, instead
// of an incidental failure two steps later naming a backfill loop.
//
// The last assertion below is what makes this test about the new check rather
// than about "Run fails somehow": it requires the error to come from the column
// check specifically. Without it this test passes on the backfill's failure and
// would have gone on passing with the new call deleted.
func TestIntegrationRunRefusesAChannelTableWithNoOrganizationColumn(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// Control. The migrated schema must pass, or the negative case below proves
	// only that Run fails for some reason.
	if err := bootstrapRun(ctx, e, true); err != nil {
		t.Fatalf("Run against the migrated schema: %v", err)
	}

	if _, err := e.appDB.ExecContext(ctx,
		`ALTER TABLE notification_channels DROP COLUMN organization_id`); err != nil {
		t.Fatalf("drop the organization column: %v", err)
	}

	err := bootstrapRun(ctx, e, true)
	if err == nil {
		t.Fatal("Run succeeded against a notification_channels table with no organization_id. " +
			"Every scoped channel read binds that column, so this deployment would answer " +
			"`column \"organization_id\" does not exist` on the admin channels page — and the " +
			"startup check that exists to turn that into a refusal to start is not being called.")
	}
	if !strings.Contains(err.Error(), "organization_id") {
		t.Errorf("Run failed, but not in a way that names the column: %v\n"+
			"An operator reading this has to know that the partition migration is the one "+
			"that did not run.", err)
	}
	// And it must be THIS check failing, not the table check next to it tripping
	// over the same DROP. Distinguishing them is the whole reason the two live
	// apart: they name different migrations.
	if !strings.Contains(err.Error(), "organization column") {
		t.Errorf("Run failed on the wrong check (%v). VerifyChannelTable and "+
			"VerifyChannelOrganizationColumn report different faults with different "+
			"remedies, and only the second one is about the partition.", err)
	}
}

// bootstrapRun is Run with this suite's arguments, so the control and the
// negative case cannot drift apart.
func bootstrapRun(ctx context.Context, e *env, seedRoles bool) error {
	// A real repository, because Run refuses to narrow a role without one and a
	// helper that passed nil would quietly exempt every test using it from the
	// one refusal that matters (#557).
	return Run(ctx, e.identityDB, e.appDB, seedRoles, repositories.NewUserTokenRevocationRepository(e.appDB))
}

// GUARD narrowed-role-ends-its-holders-sessions-end-to-end (#557).
//
// internal/approles' own integration suite proves the reconcile DETECTS a
// narrowing and hands the holders to a reducer. What it cannot reach is the
// reducer itself: internal/db/repositories imports approles, so the real
// watermark repository is unreachable from that package's tests. This closes it
// from the side that owns the wiring — the actual bootstrap.Run, with the actual
// retireSessionsOfNarrowedRoles, writing through the actual repository into the
// table middleware.AuthMiddleware reads.
//
// The narrowing is staged the way a real one arrives: the table is left holding
// a WIDER definition than this build's, which is exactly the state a deployment
// is in the moment an operator ships a binary that takes a scope away.
//
// MUTATION: pass NoTemplateAuthorityReduction from bootstrap.Run; or write the
// definitions before invalidating.
func TestIntegrationRunEndsSessionsOfANarrowedRolesHolders(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	revocations := repositories.NewUserTokenRevocationRepository(e.appDB)

	// A first boot writes this build's definitions and reduces nothing.
	if err := Run(ctx, e.identityDB, e.appDB, false, revocations); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// Widen `editor` in the table, so this build's own definition is now a
	// narrowing relative to what the deployment holds.
	if _, err := e.appDB.ExecContext(ctx,
		`UPDATE role_templates SET scopes = scopes || '["admin"]'::jsonb WHERE name = 'editor'`); err != nil {
		t.Fatalf("widening the stored definition: %v", err)
	}

	holder, other := seedHolder(t, e, "editor"), seedHolder(t, e, "viewer")

	if err := Run(ctx, e.identityDB, e.appDB, false, revocations); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	if !hasWatermark(t, e, holder) {
		t.Error("the holder of the narrowed role kept every session they had: their tokens still carry " +
			"the scope the build removed, until they expire")
	}
	if hasWatermark(t, e, other) {
		t.Error("a member holding a different role was logged out by another role's narrowing")
	}
	if got := e.appScopes(t, "editor"); !equal(got, buildScopes(t, "editor")) {
		t.Errorf("editor scopes = %v, want this build's %v — the narrowing did not land", got, buildScopes(t, "editor"))
	}
}

// GUARD run-refuses-without-a-way-to-end-sessions. A deployment that cannot
// invalidate must not narrow a role and report a clean boot.
//
// MUTATION: treat a nil repository as "nothing to do".
func TestIntegrationRunRefusesToNarrowWithNoRevocationRepository(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	revocations := repositories.NewUserTokenRevocationRepository(e.appDB)

	if err := Run(ctx, e.identityDB, e.appDB, false, revocations); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if _, err := e.appDB.ExecContext(ctx,
		`UPDATE role_templates SET scopes = scopes || '["admin"]'::jsonb WHERE name = 'editor'`); err != nil {
		t.Fatalf("widening the stored definition: %v", err)
	}
	seedHolder(t, e, "editor")

	before := e.appScopes(t, "editor")
	// nil: this deployment has no way to end a session. The narrowing must be
	// refused rather than applied silently.
	if err := Run(ctx, e.identityDB, e.appDB, false, nil); err == nil {
		t.Fatal("Run narrowed a role with no way to end its holders' sessions, and reported success")
	}
	if after := e.appScopes(t, "editor"); !equal(after, before) {
		t.Errorf("editor scopes = %v, want the unchanged %v: the narrowing landed although it was refused", after, before)
	}
}

// seedHolder creates a user in a fresh organization holding the named role in
// this application's own mirror, and returns the user id.
func seedHolder(t *testing.T, e *env, roleName string) string {
	t.Helper()
	ctx := context.Background()
	var orgID, userID string
	if err := e.identityDB.QueryRowContext(ctx, `
		INSERT INTO identity.organizations (id, name, display_name, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, $1, now(), now()) RETURNING id`,
		"holder-org-"+roleName).Scan(&orgID); err != nil {
		t.Fatalf("seed organization: %v", err)
	}
	if err := e.identityDB.QueryRowContext(ctx, `
		INSERT INTO identity.users (id, email, name, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, $1, now(), now()) RETURNING id`,
		"holder-"+roleName+"@example.test").Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := e.appDB.ExecContext(ctx, `
		INSERT INTO organization_member_roles (organization_id, user_id, role_template_id, mirrored_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (organization_id, user_id) DO UPDATE SET role_template_id = EXCLUDED.role_template_id`,
		orgID, userID, e.appTemplateID(t, roleName)); err != nil {
		t.Fatalf("assign role: %v", err)
	}
	return userID
}

func hasWatermark(t *testing.T, e *env, userID string) bool {
	t.Helper()
	var n int
	if err := e.appDB.QueryRow(`SELECT count(*) FROM user_token_revocations WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("reading watermark: %v", err)
	}
	return n > 0
}
