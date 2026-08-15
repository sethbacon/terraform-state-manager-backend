//go:build integration

// The half of this phase that only a real PostgreSQL can establish.
//
// sqlmock shows that a statement is ISSUED, and the unit tests use that to pin
// the dual write and its ordering. It cannot show that migration 000032's DDL
// produces tables these statements run against, that the routing guard actually
// refuses an app connection whose search_path reaches identity, that the foreign
// key degrades an assignment to NULL rather than erroring, that the keyset scan's
// row comparison is even valid SQL, or that the backfill reproduces identity's
// membership table row for row. Those are properties of Postgres, so they are
// asserted against Postgres.
//
// THE MIGRATION IS EXERCISED IN BOTH DIRECTIONS, because a down migration that
// has never been run is not a rollback plan. TestIntegrationMigrationRoundTrips
// applies, verifies, rolls back, and re-applies.
//
// Run with:
//
//	TEST_DATABASE_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable' \
//	  go test -tags integration ./internal/approles/...
package approles

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/lib/pq"
	identity "github.com/sethbacon/terraform-suite-identity/identity"
	idmodels "github.com/sethbacon/terraform-suite-identity/identity/models"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	appdb "github.com/terraform-state-manager/terraform-state-manager/internal/db"
)

// testDatabaseName is this suite's OWN database. `go test ./...` runs package
// binaries concurrently and this suite drops and re-creates its schema, so
// sharing one database would make its result depend on another suite's timing.
const testDatabaseName = "tsm_approles_test"

type env struct {
	appDB      *sql.DB
	identityDB *sql.DB
	members    *Members
	store      *Store
	users      *idstore.UserRepository
	orgs       *idstore.OrganizationRepository
}

// newEnv builds the production topology against a real server: the app
// connection carrying TSM's migrations, and a separate identity connection whose
// search_path resolves unqualified names to the identity schema — exactly as
// cmd/server does.
func newEnv(t *testing.T) *env {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping PostgreSQL integration test")
	}
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", dsn, err)
	}
	defer func() { _ = admin.Close() }()
	if err := admin.Ping(); err != nil {
		t.Skipf("TEST_DATABASE_URL is not reachable (%v); skipping", err)
	}
	for _, stmt := range []string{
		`DROP DATABASE IF EXISTS ` + pq.QuoteIdentifier(testDatabaseName) + ` WITH (FORCE)`,
		`CREATE DATABASE ` + pq.QuoteIdentifier(testDatabaseName),
	} {
		if _, err := admin.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	appDB := connect(t, dsn, "")
	identityDB := connect(t, dsn, "identity,public")

	// The identity schema first: TSM runs identity.RunMigrations unconditionally
	// at boot, and 000032's routing pre-check is a statement about what identity
	// has already created.
	if err := identity.RunMigrations(identityDB, "up"); err != nil {
		t.Fatalf("identity migrations: %v", err)
	}
	if err := appdb.RunMigrations(appDB, "up"); err != nil {
		t.Fatalf("app migrations: %v", err)
	}

	return &env{
		appDB:      appDB,
		identityDB: identityDB,
		members:    NewMembers(identityDB, appDB, RoleSourceApp),
		store:      NewStore(appDB),
		users:      idstore.NewUserRepository(identityDB),
		orgs:       idstore.NewOrganizationRepository(identityDB),
	}
}

// noSweepNeeded is the integration rig's credential sweep. These tests are about
// ROWS in the two authorization tables; the sweep's own behaviour is the subject
// of internal/api's #330 class test, which drives it end to end through the
// routes. It returns nil rather than being nil, because a nil reducer is refused
// (TestAMissingSweepIsRefused) — which is the point of making it mandatory.
func noSweepNeeded(context.Context, string) error { return nil }

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
	db, err := sql.Open("postgres", parsed.String())
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

func (e *env) newUser(t *testing.T, email string) string {
	t.Helper()
	u := &idmodels.User{Email: email, Name: email}
	if err := e.users.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("CreateUser(%s): %v", email, err)
	}
	return u.ID
}

func (e *env) newOrg(t *testing.T, name string) string {
	t.Helper()
	o := &idmodels.Organization{Name: name, DisplayName: name}
	if err := e.orgs.Create(context.Background(), o); err != nil {
		t.Fatalf("Create org %s: %v", name, err)
	}
	return o.ID
}

// newIdentityRole seeds a role template in the SHARED identity schema, the way
// internal/bootstrap does, and returns its id.
func (e *env) newIdentityRole(t *testing.T, name string, scopes ...string) string {
	t.Helper()
	quoted := make([]string, 0, len(scopes))
	for _, s := range scopes {
		quoted = append(quoted, `"`+s+`"`)
	}
	var id string
	err := e.identityDB.QueryRow(`
		INSERT INTO identity.role_templates (id, name, display_name, description, scopes, is_system, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, $1, NULL, $2::jsonb, true, now(), now())
		ON CONFLICT (name) DO UPDATE SET scopes = EXCLUDED.scopes
		RETURNING id`, name, "["+strings.Join(quoted, ",")+"]").Scan(&id)
	if err != nil {
		t.Fatalf("seed identity role %s: %v", name, err)
	}
	return id
}

// mirroredRole returns the role_template_id TSM's own table holds for a pair,
// and whether the row exists at all.
func (e *env) mirroredRole(t *testing.T, orgID, userID string) (roleID string, present bool) {
	t.Helper()
	var v sql.NullString
	err := e.appDB.QueryRow(
		`SELECT role_template_id FROM organization_member_roles WHERE organization_id = $1 AND user_id = $2`,
		orgID, userID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read mirrored role: %v", err)
	}
	return v.String, true
}

func (e *env) mirroredCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.appDB.QueryRow(`SELECT count(*) FROM organization_member_roles`).Scan(&n); err != nil {
		t.Fatalf("count organization_member_roles: %v", err)
	}
	return n
}

// TestIntegrationMigrationRoundTrips is the evidence the migration is reversible:
// apply, verify, roll back, re-apply. A down migration nobody has run is a
// rollback plan on paper.
func TestIntegrationMigrationRoundTrips(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	templates, assignments, err := e.store.Verify(ctx)
	if err != nil {
		t.Fatalf("Verify after apply: %v", err)
	}
	if templates != "public.role_templates" || assignments != "public.organization_member_roles" {
		t.Fatalf("resolved to %q/%q, want the app connection's public schema", templates, assignments)
	}

	// Down to 000030, which is the state a rollback of this phase leaves.
	if _, err := e.appDB.Exec(`DROP INDEX IF EXISTS organization_member_roles_mirrored_at_idx`); err != nil {
		t.Fatalf("manual pre-drop: %v", err)
	}
	if err := stepDown(e.appDB); err != nil {
		t.Fatalf("rolling back 000032: %v", err)
	}
	if _, _, err := e.store.Verify(ctx); err == nil {
		t.Fatal("Verify succeeded after the tables were rolled back")
	} else if !strings.Contains(err.Error(), "000032") {
		t.Fatalf("the post-rollback error does not name the migration: %v", err)
	}

	if err := appdb.RunMigrations(e.appDB, "up"); err != nil {
		t.Fatalf("re-applying 000032: %v", err)
	}
	if _, _, err := e.store.Verify(ctx); err != nil {
		t.Fatalf("Verify after re-apply: %v", err)
	}
}

// stepDown rolls the app schema back by exactly one migration.
//
// appdb.RunMigrations(db, "down") would unwind the WHOLE schema, which proves
// nothing about 000032 in particular and takes several seconds; this runs the
// down file itself, which is the artefact an operator would apply.
func stepDown(db *sql.DB) error {
	src, err := os.ReadFile("../db/migrations/000032_app_role_authorization.down.sql")
	if err != nil {
		return err
	}
	if _, err := db.Exec(string(src)); err != nil {
		return err
	}
	// golang-migrate's bookkeeping, so the re-apply runs the up file again.
	_, err = db.Exec(`UPDATE schema_migrations SET version = 30, dirty = false`)
	return err
}

// TestIntegrationMigrationRefusesAnIdentityRoutedConnection is the guard that
// matters most and the one no unit test can establish: with the identity schema
// on the search_path, `CREATE TABLE IF NOT EXISTS role_templates` finds
// identity's table, creates nothing, reports success, and every mirror write
// afterwards lands in the SHARED table this phase exists to stop sharing.
func TestIntegrationMigrationRefusesAnIdentityRoutedConnection(t *testing.T) {
	e := newEnv(t)

	src, err := os.ReadFile("../db/migrations/000032_app_role_authorization.up.sql")
	if err != nil {
		t.Fatalf("reading the migration: %v", err)
	}
	// e.identityDB carries search_path=identity,public — the misrouting.
	_, err = e.identityDB.Exec(string(src))
	if err == nil {
		t.Fatal("the migration ran on a connection routed into identity: TSM's per-app authorization " +
			"would have been created in, or shadowed by, the shared identity schema")
	}
	if !strings.Contains(err.Error(), "organization_members") {
		t.Fatalf("the refusal does not name what it detected: %v", err)
	}
}

// TestIntegrationVerifyRefusesAnIdentityRoutedConnection is the same property at
// run time rather than at migration time, for a deployment whose search_path
// changed after the migration had already been applied.
func TestIntegrationVerifyRefusesAnIdentityRoutedConnection(t *testing.T) {
	e := newEnv(t)
	misrouted := NewStore(e.identityDB)
	_, _, err := misrouted.Verify(context.Background())
	if !errors.Is(err, ErrMisrouted) {
		t.Fatalf("expected ErrMisrouted from an identity-routed connection, got %v", err)
	}
}

// TestIntegrationBackfillReproducesIdentity is the backfill itself: memberships
// that exist ONLY in identity — as they do on every deployment upgrading into
// this phase — must appear in TSM's own tables with the same role, keyed by the
// same template id.
func TestIntegrationBackfillReproducesIdentity(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	editorID := e.newIdentityRole(t, "editor", "state:read", "state:write")
	e.newIdentityRole(t, "viewer", "state:read")
	orgA, orgB := e.newOrg(t, "acme"), e.newOrg(t, "globex")
	alice, bob := e.newUser(t, "alice@example.com"), e.newUser(t, "bob@example.com")

	// Written through the RAW repository, not the mirror: this is the
	// pre-upgrade state, where nothing had ever written the app tables.
	raw := idstore.NewOrganizationRepository(e.identityDB)
	if err := raw.AddMemberWithParams(ctx, orgA, alice, "editor", idstore.OrgScopeAllOrganizations()); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	if err := raw.AddMemberWithParams(ctx, orgB, bob, "viewer", idstore.OrgScopeAllOrganizations()); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	// A member with NO role, which identity can represent and the mirror must too.
	if err := raw.AddMemberWithRoleTemplate(ctx, orgB, alice, nil, idstore.OrgScopeAllOrganizations()); err != nil {
		t.Fatalf("seed role-less membership: %v", err)
	}
	if e.mirroredCount(t) != 0 {
		t.Fatal("the app tables are not empty before the backfill; the test is not testing a backfill")
	}

	rep, err := Reconcile(ctx, e.appDB, e.identityDB, ownTemplates)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if rep.AssignmentsRestated != 3 {
		t.Fatalf("AssignmentsRestated = %d, want 3", rep.AssignmentsRestated)
	}

	if got, ok := e.mirroredRole(t, orgA, alice); !ok || got != editorID {
		t.Fatalf("alice@acme mirrored as (%q, present=%v), want identity's editor id %q", got, ok, editorID)
	}
	if got, ok := e.mirroredRole(t, orgB, alice); !ok || got != "" {
		t.Fatalf("alice@globex mirrored as (%q, present=%v), want a present row with a NULL role", got, ok)
	}
	// The ids are identity's, so the two tables can be compared by primary key.
	assertNoDrift(t, e)
}

// TestIntegrationReconcileSweepsWhatIdentityNoLongerHas covers the reason
// mirrored_at exists: a membership removed WITHOUT passing through this
// application's code — a CASCADE, or the sibling registry — must stop being
// mirrored.
func TestIntegrationReconcileSweepsWhatIdentityNoLongerHas(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	e.newIdentityRole(t, "viewer", "state:read")
	org := e.newOrg(t, "acme")
	user := e.newUser(t, "carol@example.com")
	if err := e.members.AddMemberWithParams(ctx, org, user, "viewer", idstore.OrgScopeAllOrganizations()); err != nil {
		t.Fatalf("AddMemberWithParams: %v", err)
	}
	if _, ok := e.mirroredRole(t, org, user); !ok {
		t.Fatal("the dual write did not reach TSM's own table")
	}

	// Behind the application's back, as a CASCADE or a sibling app would.
	if _, err := e.identityDB.Exec(
		`DELETE FROM identity.organization_members WHERE organization_id = $1 AND user_id = $2`, org, user); err != nil {
		t.Fatalf("out-of-band delete: %v", err)
	}

	rep, err := Reconcile(ctx, e.appDB, e.identityDB, ownTemplates)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if rep.StaleRemoved != 1 {
		t.Fatalf("StaleRemoved = %d, want 1", rep.StaleRemoved)
	}
	if _, ok := e.mirroredRole(t, org, user); ok {
		t.Fatal("the mirror still records a role identity no longer has")
	}
}

// A reconcile must NOT sweep a row the mirror wrote while it was running. The
// generation is taken from the database clock before the pass, and a concurrent
// write stamps a later mirrored_at.
func TestIntegrationReconcileSparesAConcurrentWrite(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	e.newIdentityRole(t, "viewer", "state:read")
	org := e.newOrg(t, "acme")
	user := e.newUser(t, "dave@example.com")
	if err := e.members.AddMemberWithParams(ctx, org, user, "viewer", idstore.OrgScopeAllOrganizations()); err != nil {
		t.Fatalf("AddMemberWithParams: %v", err)
	}
	if _, err := Reconcile(ctx, e.appDB, e.identityDB, ownTemplates); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := e.mirroredRole(t, org, user); !ok {
		t.Fatal("a live assignment was swept by its own reconcile")
	}
}

// TestIntegrationDualWriteEndToEnd exercises every overridden write against both
// real connections, including the two whose effect is a CASCADE in identity and
// an explicit delete here.
func TestIntegrationDualWriteEndToEnd(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	editorID := e.newIdentityRole(t, "editor", "state:read", "state:write")
	viewerID := e.newIdentityRole(t, "viewer", "state:read")
	if _, err := Reconcile(ctx, e.appDB, e.identityDB, ownTemplates); err != nil {
		t.Fatalf("initial Reconcile: %v", err)
	}
	org := e.newOrg(t, "acme")
	user := e.newUser(t, "erin@example.com")
	all := idstore.OrgScopeAllOrganizations()

	if err := e.members.AddMemberWithParams(ctx, org, user, "editor", all); err != nil {
		t.Fatalf("AddMemberWithParams: %v", err)
	}
	if got, _ := e.mirroredRole(t, org, user); got != editorID {
		t.Fatalf("after add, mirrored role = %q, want %q", got, editorID)
	}

	if err := e.members.UpdateMemberRoleTemplate(ctx, org, user, &viewerID, all, noSweepNeeded); err != nil {
		t.Fatalf("UpdateMemberRoleTemplate: %v", err)
	}
	if got, _ := e.mirroredRole(t, org, user); got != viewerID {
		t.Fatalf("after update, mirrored role = %q, want %q", got, viewerID)
	}
	assertNoDrift(t, e)

	if err := e.members.RemoveMember(ctx, org, user, all, noSweepNeeded); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	if _, ok := e.mirroredRole(t, org, user); ok {
		t.Fatal("after remove, the mirror still records the role")
	}

	// The organization delete, whose identity effect is a CASCADE this table has
	// no foreign key to participate in.
	if err := e.members.AddMemberWithParams(ctx, org, user, "editor", all); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if err := e.members.Delete(ctx, org, all, noSweepNeeded); err != nil {
		t.Fatalf("Delete organization: %v", err)
	}
	if _, ok := e.mirroredRole(t, org, user); ok {
		t.Fatal("deleting the organization left its mirrored roles behind")
	}

	// The user hard-delete, likewise a CASCADE, mirrored by PurgeUserRoles.
	org2 := e.newOrg(t, "globex")
	if err := e.members.AddMemberWithParams(ctx, org2, user, "viewer", all); err != nil {
		t.Fatalf("add to second org: %v", err)
	}
	if _, err := e.identityDB.Exec(`DELETE FROM identity.users WHERE id = $1`, user); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	e.members.PurgeUserRoles(ctx, user, idstore.OrgScopeAllOrganizations())
	if _, ok := e.mirroredRole(t, org2, user); ok {
		t.Fatal("deleting the user left its mirrored roles behind")
	}
	assertNoDrift(t, e)
}

// TestIntegrationDroppingATemplateNullsTheAssignment pins the foreign key's
// ON DELETE SET NULL, which mirrors identity's own
// organization_members.role_template_id. RESTRICT would make a template drop
// fail; CASCADE would delete the membership record entirely. Both are wrong,
// and both compile.
func TestIntegrationDroppingATemplateNullsTheAssignment(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	e.newIdentityRole(t, "viewer", "state:read")
	if _, err := Reconcile(ctx, e.appDB, e.identityDB, ownTemplates); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	org := e.newOrg(t, "acme")
	user := e.newUser(t, "frank@example.com")
	if err := e.members.AddMemberWithParams(ctx, org, user, "viewer", idstore.OrgScopeAllOrganizations()); err != nil {
		t.Fatalf("AddMemberWithParams: %v", err)
	}

	if _, err := e.appDB.Exec(`DELETE FROM role_templates WHERE name = 'viewer'`); err != nil {
		t.Fatalf("dropping the app's role template: %v", err)
	}
	role, present := e.mirroredRole(t, org, user)
	if !present {
		t.Fatal("dropping a role template deleted the membership record: the foreign key cascades where it must set null")
	}
	if role != "" {
		t.Fatalf("the assignment still names a dropped template: %q", role)
	}
}

// TestIntegrationMirrorAdoptsATemplateItHasNeverSeen covers the shared-identity
// case: the sibling registry creates a role after this deployment's last
// reconcile, an administrator assigns it, and the foreign key would otherwise
// reject the mirror write and lose a grant that did happen.
func TestIntegrationMirrorAdoptsATemplateItHasNeverSeen(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	if _, err := Reconcile(ctx, e.appDB, e.identityDB, ownTemplates); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// Created AFTER the reconcile, so TSM's own table has never seen it.
	lateID := e.newIdentityRole(t, "late_arrival", "state:read")
	org := e.newOrg(t, "acme")
	user := e.newUser(t, "grace@example.com")

	if err := e.members.AddMemberWithParams(ctx, org, user, "late_arrival", idstore.OrgScopeAllOrganizations()); err != nil {
		t.Fatalf("AddMemberWithParams: %v", err)
	}
	if got, ok := e.mirroredRole(t, org, user); !ok || got != lateID {
		t.Fatalf("mirrored role = (%q, present=%v), want the adopted template %q", got, ok, lateID)
	}

	// By id, too: the same hazard reached through the admin route's uuid form.
	user2 := e.newUser(t, "heidi@example.com")
	byIDRole := e.newIdentityRole(t, "late_arrival_two", "state:read")
	if err := e.members.AddMemberWithRoleTemplate(ctx, org, user2, &byIDRole, idstore.OrgScopeAllOrganizations()); err != nil {
		t.Fatalf("AddMemberWithRoleTemplate: %v", err)
	}
	if got, ok := e.mirroredRole(t, org, user2); !ok || got != byIDRole {
		t.Fatalf("mirrored role = (%q, present=%v), want the adopted template %q", got, ok, byIDRole)
	}
}

// TestIntegrationDriftQueryReportsAllThreeKinds runs the query an operator is
// pointed at, against real divergence of each kind. A detection query nobody has
// executed is a detection query that does not parse.
func TestIntegrationDriftQueryReportsAllThreeKinds(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	roleA := e.newIdentityRole(t, "editor", "state:read", "state:write")
	roleB := e.newIdentityRole(t, "viewer", "state:read")
	if _, err := Reconcile(ctx, e.appDB, e.identityDB, ownTemplates); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	org := e.newOrg(t, "acme")
	missing := e.newUser(t, "missing@example.com")
	stale := e.newUser(t, "stale@example.com")
	mismatched := e.newUser(t, "mismatched@example.com")
	all := idstore.OrgScopeAllOrganizations()
	raw := idstore.NewOrganizationRepository(e.identityDB)

	// missing: identity only.
	if err := raw.AddMemberWithParams(ctx, org, missing, "editor", all); err != nil {
		t.Fatalf("seed missing: %v", err)
	}
	// stale: the mirror only.
	if err := e.store.SetRole(ctx, org, stale, &roleA, idstore.OrgScopeAllOrganizations()); err != nil {
		t.Fatalf("seed stale: %v", err)
	}
	// mismatched: both, different roles.
	if err := e.members.AddMemberWithParams(ctx, org, mismatched, "editor", all); err != nil {
		t.Fatalf("seed mismatched: %v", err)
	}
	if err := e.store.SetRole(ctx, org, mismatched, &roleB, idstore.OrgScopeAllOrganizations()); err != nil {
		t.Fatalf("skew mismatched: %v", err)
	}

	kinds := driftKinds(t, e)
	for _, want := range []string{"missing", "stale", "mismatched"} {
		if kinds[want] != 1 {
			t.Errorf("DriftQuery reported %d %q rows, want 1 (all kinds: %v)", kinds[want], want, kinds)
		}
	}

	// And the standing repair: a restart clears every kind.
	if _, err := Reconcile(ctx, e.appDB, e.identityDB, ownTemplates); err != nil {
		t.Fatalf("repairing Reconcile: %v", err)
	}
	assertNoDrift(t, e)
}

// TestIntegrationKeysetScanPagesBeyondOneBatch exercises the pagination the
// backfill uses. The row-comparison predicate `(organization_id, user_id) > ($1, $2)`
// either works or does not parse, and one short page would never find out.
func TestIntegrationKeysetScanPagesBeyondOneBatch(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	roleID := e.newIdentityRole(t, "viewer", "state:read")
	org := e.newOrg(t, "acme")
	const n = membershipPage + 7
	for i := 0; i < n; i++ {
		var userID string
		if err := e.identityDB.QueryRow(
			`INSERT INTO identity.users (id, email, name, created_at, updated_at)
			 VALUES (gen_random_uuid(), $1, $1, now(), now()) RETURNING id`,
			fmt.Sprintf("bulk-%d@example.com", i)).Scan(&userID); err != nil {
			t.Fatalf("bulk user %d: %v", i, err)
		}
		if _, err := e.identityDB.Exec(
			`INSERT INTO identity.organization_members (organization_id, user_id, role_template_id, created_at, updated_at)
			 VALUES ($1, $2, $3, now(), now())`, org, userID, roleID); err != nil {
			t.Fatalf("bulk membership %d: %v", i, err)
		}
	}

	rep, err := Reconcile(ctx, e.appDB, e.identityDB, ownTemplates)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if rep.AssignmentsRestated != n {
		t.Fatalf("AssignmentsRestated = %d, want %d: the keyset scan stopped short of the last page", rep.AssignmentsRestated, n)
	}
	if got := e.mirroredCount(t); got != n {
		t.Fatalf("mirrored rows = %d, want %d", got, n)
	}
	assertNoDrift(t, e)
}

// PHASE 3B'S ONE DELIBERATE AUTHORIZATION CHANGE, asserted on the rows.
//
// Phase 3a mirrored identity's scopes VERBATIM, because the mirror had to equal
// what authorization already read; its report named the roles whose meaning came
// from the sibling app so an operator could see it "before the phase that makes it
// permanent". This is that phase. A coupled deployment's `editor` becomes THIS
// build's editor at the first boot on this build, and the previous test in this
// position asserted the opposite — it is inverted here rather than deleted,
// because the inversion is the change.
func TestIntegrationThisBuildsScopesReplaceTheSiblings(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	e.newIdentityRole(t, "editor", "modules:read", "providers:read") // the sibling's, not this build's
	if _, err := Reconcile(ctx, e.appDB, e.identityDB, ownTemplates); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var scopes string
	if err := e.appDB.QueryRow(`SELECT scopes::text FROM role_templates WHERE name = 'editor'`).Scan(&scopes); err != nil {
		t.Fatalf("read this application's scopes: %v", err)
	}
	if strings.Contains(scopes, "modules:read") {
		t.Fatalf("the sibling's scopes are still what this application's `editor` grants: %s", scopes)
	}
	if !strings.Contains(scopes, "state:write") {
		t.Fatalf("this build's `editor` scopes were not written: %s", scopes)
	}

	// THE SHARED SCHEMA IS UNTOUCHED. The rollback path and the sibling both read
	// it, and this phase drops nothing.
	var identityScopes string
	if err := e.identityDB.QueryRow(`SELECT scopes::text FROM identity.role_templates WHERE name = 'editor'`).Scan(&identityScopes); err != nil {
		t.Fatalf("read identity's scopes: %v", err)
	}
	if !strings.Contains(identityScopes, "modules:read") {
		t.Fatalf("the reconcile rewrote the SHARED identity schema, which the sibling and the rollback both read: %s", identityScopes)
	}
}

// A role only the sibling defines is adopted AS IT IS — an assignment pointing at
// it must keep resolving — and reported, because this application does not own
// what it means.
func TestIntegrationAForeignRoleIsAdoptedAndNamed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	foreignID := e.newIdentityRole(t, "registry_publisher", "modules:write")
	rep, err := Reconcile(ctx, e.appDB, e.identityDB, ownTemplates)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	found := false
	for _, name := range rep.ForeignTemplates {
		if name == "registry_publisher" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ForeignTemplates = %v, want it to name registry_publisher", rep.ForeignTemplates)
	}
	var id, scopes string
	if err := e.appDB.QueryRow(
		`SELECT id::text, scopes::text FROM role_templates WHERE name = 'registry_publisher'`).Scan(&id, &scopes); err != nil {
		t.Fatalf("read the adopted template: %v", err)
	}
	if id != foreignID {
		t.Fatalf("adopted id = %s, want identity's %s: an assignment restated from identity would not resolve", id, foreignID)
	}
	if !strings.Contains(scopes, "modules:write") {
		t.Fatalf("the adopted scopes were rewritten: %s", scopes)
	}
	// A role this build DOES define is not foreign, even though it was adopted
	// from the same table.
	for _, name := range rep.ForeignTemplates {
		if name == "editor" {
			t.Error("ForeignTemplates names `editor`, a role this build defines")
		}
	}
}

// driftKinds runs DriftQuery and counts each kind.
func driftKinds(t *testing.T, e *env) map[string]int {
	t.Helper()
	rows, err := e.appDB.Query(DriftQuery)
	if err != nil {
		t.Fatalf("DriftQuery: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var kind, org, user string
		var identityRole, appRole sql.NullString
		if err := rows.Scan(&kind, &org, &user, &identityRole, &appRole); err != nil {
			t.Fatalf("scanning drift row: %v", err)
		}
		out[kind]++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("DriftQuery rows: %v", err)
	}
	return out
}

func assertNoDrift(t *testing.T, e *env) {
	t.Helper()
	kinds := driftKinds(t, e)
	if len(kinds) == 0 {
		return
	}
	keys := make([]string, 0, len(kinds))
	for k := range kinds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Fatalf("the mirror does not agree with identity: %v (kinds %v)", kinds, keys)
}

// TestIntegrationMirrorIsTenantScoped is the same property as the unit test,
// asserted on ROWS rather than on mock expectations.
//
// It exists because the mock version of this test is delicate: sqlmock reports
// unmet expectations, not unexpected statements, so the obvious spelling passes
// with the fix reverted. A row that is still there after an out-of-tenancy call
// cannot be misread.
func TestIntegrationMirrorIsTenantScoped(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	e.newIdentityRole(t, "viewer", "state:read")
	if _, err := Reconcile(ctx, e.appDB, e.identityDB, ownTemplates); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	victim := e.newOrg(t, "victim")
	other := e.newOrg(t, "other")
	user := e.newUser(t, "ivan@example.com")
	if err := e.members.AddMemberWithParams(ctx, victim, user, "viewer", idstore.OrgScopeAllOrganizations()); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	if _, ok := e.mirroredRole(t, victim, user); !ok {
		t.Fatal("the dual write did not reach TSM's own table")
	}

	// A caller whose tenancy is `other` acting on `victim`.
	outside := idstore.OrgScopeOrganizations(other)
	_ = e.members.RemoveMember(ctx, victim, user, outside, noSweepNeeded)
	if _, ok := e.mirroredRole(t, victim, user); !ok {
		t.Fatal("an out-of-tenancy RemoveMember deleted another organization's mirrored role")
	}
	_ = e.members.Delete(ctx, victim, outside, noSweepNeeded)
	if _, ok := e.mirroredRole(t, victim, user); !ok {
		t.Fatal("an out-of-tenancy organization delete removed another organization's mirrored roles")
	}
	// The identity row is likewise untouched, so the two sides still agree.
	assertNoDrift(t, e)

	// In tenancy, the same calls do apply.
	if err := e.members.RemoveMember(ctx, victim, user, idstore.OrgScopeOrganizations(victim), noSweepNeeded); err != nil {
		t.Fatalf("in-tenancy RemoveMember: %v", err)
	}
	if _, ok := e.mirroredRole(t, victim, user); ok {
		t.Fatal("an in-tenancy RemoveMember left the mirrored role behind")
	}
}

// ownTemplates is the app-side seed the server runs.
//
// It is spelled here rather than imported because internal/bootstrap imports THIS
// package, so the real one cannot be called from its tests. The real one is run
// end to end against Postgres by internal/bootstrap's own integration test, which
// is what establishes that these two agree.
func ownTemplates(ctx context.Context, s *Store) error {
	for _, rt := range auth.AppRoleTemplates() {
		description := rt.Description
		if err := s.DefineTemplate(ctx, Template{
			Name:        rt.Name,
			DisplayName: rt.DisplayName,
			Description: &description,
			Scopes:      rt.Scopes,
			IsSystem:    true,
		}); err != nil {
			return err
		}
	}
	return nil
}

// seedIdentityWithThisBuildsRoles puts this build's own role -> scope mapping in
// the SHARED schema, which is the standalone deployment (suite.role_seed_owner =
// "self") — the topology the flip is required to be invisible on.
func (e *env) seedIdentityWithThisBuildsRoles(t *testing.T) map[string]string {
	t.Helper()
	ids := make(map[string]string)
	for _, rt := range auth.AppRoleTemplates() {
		ids[rt.Name] = e.newIdentityRole(t, rt.Name, rt.Scopes...)
	}
	return ids
}

// principal is one subject of the equivalence proof.
type principal struct {
	name   string
	userID string
	orgs   map[string]string // organization id -> role name ("" = a member with no role)
}

// resolveBothWays computes every authorization answer TSM derives for a
// principal, once from this application's tables and once from the shared
// identity schema, and returns them side by side.
//
// EVERY ACCESSOR AUTHORIZATION ACTUALLY USES, not just the scope union: the
// per-organization set (requireOrgScope), the tenancy resolver (every scoped
// admin route) and the membership list the other two are derived from. A proof
// that covered only GetUserCombinedScopes would certify the one accessor whose
// override is most obvious and say nothing about the three derived ones, which
// are exactly where a promoted method hides.
type resolution struct {
	combined    []string
	perOrg      map[string][]string
	orgScope    []string
	roleNames   map[string]string
	memberships int
}

func resolveWith(t *testing.T, m *Members, p principal, orgIDs []string) resolution {
	t.Helper()
	ctx := context.Background()
	out := resolution{perOrg: map[string][]string{}, roleNames: map[string]string{}}

	combined, err := m.GetUserCombinedScopes(ctx, p.userID)
	if err != nil {
		t.Fatalf("%s: GetUserCombinedScopes: %v", p.name, err)
	}
	out.combined = sorted(combined)

	for _, orgID := range orgIDs {
		scopes, serr := m.GetUserScopesForOrg(ctx, p.userID, orgID)
		if serr != nil {
			t.Fatalf("%s: GetUserScopesForOrg(%s): %v", p.name, orgID, serr)
		}
		out.perOrg[orgID] = sorted(scopes)
	}

	scope, err := m.OrgScopeForUser(ctx, p.userID, "state:write", nil)
	if err != nil {
		t.Fatalf("%s: OrgScopeForUser: %v", p.name, err)
	}
	out.orgScope = sorted(scope.OrganizationIDs())

	memberships, err := m.GetUserMemberships(ctx, p.userID)
	if err != nil {
		t.Fatalf("%s: GetUserMemberships: %v", p.name, err)
	}
	out.memberships = len(memberships)
	for _, mem := range memberships {
		name := "<none>"
		if mem.RoleTemplateName != nil {
			name = *mem.RoleTemplateName
		}
		out.roleNames[mem.OrganizationID] = name
	}
	return out
}

// diffs names every DIMENSION on which two resolutions disagree, keyed by the
// accessor that produced it.
//
// PER-ACCESSOR, NOT A SINGLE BOOLEAN, and that distinction is not stylistic — a
// mutation found it. The first version of this proof returned "same or not". Its
// negative control then passed with the GetUserMemberships overlay DELETED,
// because GetMemberWithRole's overlay was still in place and one differing
// accessor was enough to satisfy "not same". A partially reverted flip — the
// session scope union reading identity while the per-organization check read this
// application's tables — would have been certified as equivalent.
//
// So the negative control below requires EACH named dimension to move, which is
// the only form of the assertion that establishes every accessor is reading the
// source it is supposed to.
func diffs(a, b resolution) map[string]string {
	out := map[string]string{}
	if !equalStrings(a.combined, b.combined) {
		out["GetUserCombinedScopes"] = fmt.Sprintf("identity=%v app=%v", a.combined, b.combined)
	}
	if !equalStrings(a.orgScope, b.orgScope) {
		out["OrgScopeForUser"] = fmt.Sprintf("identity=%v app=%v", a.orgScope, b.orgScope)
	}
	if a.memberships != b.memberships {
		out["membership count"] = fmt.Sprintf("identity=%d app=%d", a.memberships, b.memberships)
	}
	for org, want := range a.perOrg {
		if !equalStrings(want, b.perOrg[org]) {
			out["GetUserScopesForOrg"] = fmt.Sprintf("in %s: identity=%v app=%v", org, want, b.perOrg[org])
		}
	}
	for org, want := range a.roleNames {
		if b.roleNames[org] != want {
			out["GetUserMemberships"] = fmt.Sprintf("role name in %s: identity=%q app=%q", org, want, b.roleNames[org])
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
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

// TestIntegrationEffectiveScopesAreEquivalentBothWays IS THE PROOF THIS PHASE IS
// GATED ON.
//
// It builds a representative estate — several organizations, principals holding
// different roles in different ones, an administrator, a member with NO role, a
// principal who belongs to nothing — and resolves EVERY authorization answer TSM
// derives, twice: once through a repository reading the shared identity schema
// (Phase 3a) and once through one reading this application's own tables
// (Phase 3b). The two must agree, value for value.
//
// # The negative control is half the test
//
// A comparison of two things that happen to agree passes whether or not either
// side is being read. So the second half MUTATES one mirror row — the exact
// failure this phase risks, a principal silently holding the wrong role — and
// requires the SAME comparison to fail, and CheckDrift to name that pair. Without
// it, this test would pass with the entire overlay deleted.
func TestIntegrationEffectiveScopesAreEquivalentBothWays(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	roleIDs := e.seedIdentityWithThisBuildsRoles(t)
	acme, globex, initech := e.newOrg(t, "acme"), e.newOrg(t, "globex"), e.newOrg(t, "initech")
	orgIDs := []string{acme, globex, initech}

	principals := []principal{
		{name: "alice-admin", orgs: map[string]string{acme: "admin"}},
		{name: "bob-split", orgs: map[string]string{acme: "editor", globex: "viewer"}},
		{name: "carol-owner", orgs: map[string]string{globex: "org_owner", initech: "operator"}},
		{name: "dave-roleless", orgs: map[string]string{initech: ""}},
		{name: "erin-unaffiliated", orgs: map[string]string{}},
	}

	// Seeded through the RAW repository: this is the pre-upgrade state, where
	// only identity had ever been written.
	raw := idstore.NewOrganizationRepository(e.identityDB)
	for i := range principals {
		principals[i].userID = e.newUser(t, principals[i].name+"@example.com")
		for orgID, role := range principals[i].orgs {
			var err error
			if role == "" {
				err = raw.AddMemberWithRoleTemplate(ctx, orgID, principals[i].userID, nil, idstore.OrgScopeAllOrganizations())
			} else {
				err = raw.AddMemberWithParams(ctx, orgID, principals[i].userID, role, idstore.OrgScopeAllOrganizations())
			}
			if err != nil {
				t.Fatalf("seed %s in %s: %v", principals[i].name, orgID, err)
			}
		}
	}

	if _, err := Reconcile(ctx, e.appDB, e.identityDB, ownTemplates); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// THE GATE. Zero before the reads flip; asserted here so the proof below is
	// known to be comparing a reconciled pair rather than two arbitrary tables.
	drift, err := CheckDrift(ctx, e.appDB, e.identityDB)
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if !drift.Clean() {
		t.Fatalf("drift is not zero before the comparison, so nothing below is evidence:\n%s", drift.String())
	}
	if drift.Compared == 0 {
		t.Fatal("the drift check compared nothing: a clean result from an empty comparison is not agreement")
	}

	identityReader := NewMembers(e.identityDB, e.appDB, RoleSourceIdentity)
	appReader := NewMembers(e.identityDB, e.appDB, RoleSourceApp)
	if identityReader.Source() != RoleSourceIdentity || appReader.Source() != RoleSourceApp {
		t.Fatalf("the two readers are not reading different sources: %q and %q",
			identityReader.Source(), appReader.Source())
	}

	for _, p := range principals {
		fromIdentity := resolveWith(t, identityReader, p, orgIDs)
		fromApp := resolveWith(t, appReader, p, orgIDs)
		for accessor, diff := range diffs(fromIdentity, fromApp) {
			t.Errorf("%s resolves differently depending on which tables are read — %s: %s", p.name, accessor, diff)
		}
	}
	// The proof is worthless if the negative control below runs against an
	// already-failing comparison.
	if t.Failed() {
		t.FailNow()
	}

	// ---- NEGATIVE CONTROL -------------------------------------------------
	//
	// One mirror row is made wrong, in the direction that is silent in
	// production: a principal KEEPS a role they should not. The comparison above
	// must now fail for that principal and no other, and the drift check must
	// name the pair.
	// bob is `editor` in acme. The mirror is narrowed to `viewer` — a NARROWING,
	// so that every dimension moves: the scope union loses state:write, the
	// per-organization set loses it, the tenancy resolver stops returning acme for
	// state:write, and the role name changes. A widening to `admin` would have
	// left OrgScopeForUser unchanged (the admin wildcard still grants state:write)
	// and the control would have been blind to that accessor.
	bob := principals[1]
	if _, err := e.appDB.Exec(
		`UPDATE organization_member_roles SET role_template_id = $1 WHERE organization_id = $2 AND user_id = $3`,
		roleIDs["viewer"], acme, bob.userID); err != nil {
		t.Fatalf("mutating a mirror row: %v", err)
	}

	got := diffs(resolveWith(t, identityReader, bob, orgIDs), resolveWith(t, appReader, bob, orgIDs))
	for _, accessor := range []string{
		"GetUserCombinedScopes",
		"GetUserScopesForOrg",
		"OrgScopeForUser",
		"GetUserMemberships",
	} {
		if _, moved := got[accessor]; !moved {
			t.Errorf("%s gave the SAME answer from both sources after a mirror row was made wrong, so it is "+
				"not reading this application's tables. A flip that moved the other accessors and left this one "+
				"on identity would have been certified equivalent by a comparison that only asked whether "+
				"ANYTHING differed.", accessor)
		}
	}
	if len(got) == 0 {
		t.Fatal("nothing changed at all with a mirror row pointing at a DIFFERENT role: the proof is not " +
			"reading this application's tables and would have certified a broken flip")
	}

	// Everyone else must still agree: a proof that fails for every principal
	// whenever one row is wrong is not localising anything.
	for _, p := range principals {
		if p.name == bob.name {
			continue
		}
		if other := diffs(resolveWith(t, identityReader, p, orgIDs), resolveWith(t, appReader, p, orgIDs)); len(other) > 0 {
			t.Errorf("%s changed answer because a DIFFERENT principal's row was mutated: %v", p.name, other)
		}
	}

	drift, err = CheckDrift(ctx, e.appDB, e.identityDB)
	if err != nil {
		t.Fatalf("CheckDrift after the mutation: %v", err)
	}
	if drift.Mismatched != 1 {
		t.Fatalf("CheckDrift.Mismatched = %d, want 1: the gate cannot see the state that breaks the flip\n%s",
			drift.Mismatched, drift.String())
	}
	if drift.Clean() {
		t.Fatal("CheckDrift reports clean with a mirror row pointing at the wrong role")
	}
	found := false
	for _, s := range drift.Sample {
		if s.Kind == DriftMismatched && s.OrganizationID == acme && s.UserID == bob.userID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the drift sample does not name the mutated pair (org=%s user=%s):\n%s", acme, bob.userID, drift.String())
	}
}

// A ROLE DEFINITION THAT DIVERGES UNDER AN IDENTICAL ID is the case Phase 3a's
// DriftQuery cannot see, and the one that after the flip means the right role
// name granting the wrong permissions. Asserted against real Postgres because the
// claim is about two tables in two schemas, not about a comparison in Go.
func TestIntegrationDriftSeesScopeDivergenceThatDriftQueryCannot(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	e.seedIdentityWithThisBuildsRoles(t)
	org := e.newOrg(t, "acme")
	user := e.newUser(t, "frank@example.com")
	raw := idstore.NewOrganizationRepository(e.identityDB)
	if err := raw.AddMemberWithParams(ctx, org, user, "editor", idstore.OrgScopeAllOrganizations()); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	if _, err := Reconcile(ctx, e.appDB, e.identityDB, ownTemplates); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertNoDrift(t, e)

	// The shared schema's `editor` is widened behind this application's back, as
	// the sibling registry's own seed would do on ITS restart.
	if _, err := e.identityDB.Exec(
		`UPDATE identity.role_templates SET scopes = '["admin"]'::jsonb WHERE name = 'editor'`); err != nil {
		t.Fatalf("widening identity's editor: %v", err)
	}

	// DriftQuery still reports nothing: the ids agree.
	if kinds := driftKinds(t, e); len(kinds) != 0 {
		t.Fatalf("DriftQuery unexpectedly reported %v; this test's premise is that it cannot see this", kinds)
	}

	res, err := CheckDrift(ctx, e.appDB, e.identityDB)
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if res.AssignmentDrift() != 0 {
		t.Fatalf("AssignmentDrift() = %d, want 0: no assignment changed", res.AssignmentDrift())
	}
	if res.ScopeDivergent != 1 {
		t.Fatalf("ScopeDivergent = %d, want 1: one principal's effective permissions now differ by source\n%s",
			res.ScopeDivergent, res.String())
	}
	if res.Clean() {
		t.Fatal("Clean() = true: the gate would pass a deployment on which the flip changes what a role grants")
	}

	// And it is REAL: the two readers now disagree about what this principal may do.
	identityScopes, err := NewMembers(e.identityDB, e.appDB, RoleSourceIdentity).GetUserCombinedScopes(ctx, user)
	if err != nil {
		t.Fatalf("identity scopes: %v", err)
	}
	appScopes, err := NewMembers(e.identityDB, e.appDB, RoleSourceApp).GetUserCombinedScopes(ctx, user)
	if err != nil {
		t.Fatalf("app scopes: %v", err)
	}
	if equalStrings(sorted(identityScopes), sorted(appScopes)) {
		t.Fatalf("the two readers agree (%v) even though the two schemas define `editor` differently", sorted(appScopes))
	}
}

// THE ROLLBACK IS REAL, asserted as a behaviour rather than as a paragraph.
//
// An operator who finds the flip wrong sets TSM_AUTHZ_ROLE_SOURCE=identity and
// restarts. That works only if the shared schema is still CURRENT — which is the
// property Phase 3a's dual write provides and this phase does not switch off. So:
// perform a role change through the application AFTER the flip, then read it back
// through a rollback-position repository and require the new role.
func TestIntegrationRollbackToIdentityStillSeesPostFlipWrites(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	e.seedIdentityWithThisBuildsRoles(t)
	org := e.newOrg(t, "acme")
	user := e.newUser(t, "grace@example.com")
	if _, err := Reconcile(ctx, e.appDB, e.identityDB, ownTemplates); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// A grant made by a deployment running the FLIPPED build.
	flipped := NewMembers(e.identityDB, e.appDB, RoleSourceApp)
	if err := flipped.AddMemberWithParams(ctx, org, user, "editor", idstore.OrgScopeAllOrganizations()); err != nil {
		t.Fatalf("AddMemberWithParams: %v", err)
	}
	// ...then narrowed, which is the direction that matters: a rollback must not
	// restore authority the flipped build had already withdrawn.
	if err := flipped.UpdateMemberRole(ctx, org, user, "viewer", idstore.OrgScopeAllOrganizations(), noSweepNeeded); err != nil {
		t.Fatalf("UpdateMemberRole: %v", err)
	}

	rolledBack := NewMembers(e.identityDB, e.appDB, RoleSourceIdentity)
	scopes, err := rolledBack.GetUserCombinedScopes(ctx, user)
	if err != nil {
		t.Fatalf("rolled-back read: %v", err)
	}
	got := sorted(scopes)
	if len(got) != 1 || got[0] != "state:read" {
		t.Fatalf("after rollback the principal resolves to %v, want this build's viewer scopes [state:read]. "+
			"The shared schema went stale, so TSM_AUTHZ_ROLE_SOURCE=identity is not a rollback.", got)
	}
	assertNoDrift(t, e)
}
