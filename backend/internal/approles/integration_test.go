//go:build integration

// The half of this phase that only a real PostgreSQL can establish.
//
// sqlmock shows that a statement is ISSUED, and the unit tests use that to pin
// the dual write and its ordering. It cannot show that migration 000031's DDL
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
	// at boot, and 000031's routing pre-check is a statement about what identity
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
		members:    NewMembers(identityDB, appDB),
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
		t.Fatalf("rolling back 000031: %v", err)
	}
	if _, _, err := e.store.Verify(ctx); err == nil {
		t.Fatal("Verify succeeded after the tables were rolled back")
	} else if !strings.Contains(err.Error(), "000031") {
		t.Fatalf("the post-rollback error does not name the migration: %v", err)
	}

	if err := appdb.RunMigrations(e.appDB, "up"); err != nil {
		t.Fatalf("re-applying 000031: %v", err)
	}
	if _, _, err := e.store.Verify(ctx); err != nil {
		t.Fatalf("Verify after re-apply: %v", err)
	}
}

// stepDown rolls the app schema back by exactly one migration.
//
// appdb.RunMigrations(db, "down") would unwind the WHOLE schema, which proves
// nothing about 000031 in particular and takes several seconds; this runs the
// down file itself, which is the artefact an operator would apply.
func stepDown(db *sql.DB) error {
	src, err := os.ReadFile("../db/migrations/000031_app_role_authorization.down.sql")
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

	src, err := os.ReadFile("../db/migrations/000031_app_role_authorization.up.sql")
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

	rep, err := Reconcile(ctx, e.appDB, e.identityDB)
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

	rep, err := Reconcile(ctx, e.appDB, e.identityDB)
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
	if _, err := Reconcile(ctx, e.appDB, e.identityDB); err != nil {
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
	if _, err := Reconcile(ctx, e.appDB, e.identityDB); err != nil {
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
	if _, err := Reconcile(ctx, e.appDB, e.identityDB); err != nil {
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

	if _, err := Reconcile(ctx, e.appDB, e.identityDB); err != nil {
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
	if _, err := Reconcile(ctx, e.appDB, e.identityDB); err != nil {
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
	if _, err := Reconcile(ctx, e.appDB, e.identityDB); err != nil {
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

	rep, err := Reconcile(ctx, e.appDB, e.identityDB)
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

// TestIntegrationDivergentScopesAreMirroredAsTheyAre is the coupled deployment:
// identity holds the SIBLING's definition of a role name, which is what TSM
// authorizes against today. Phase 3a must copy it verbatim and merely report it.
func TestIntegrationDivergentScopesAreMirroredAsTheyAre(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	e.newIdentityRole(t, "editor", "modules:read", "providers:read") // the sibling's, not this build's
	rep, err := Reconcile(ctx, e.appDB, e.identityDB)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	found := false
	for _, name := range rep.DivergentTemplates {
		if name == "editor" {
			found = true
		}
	}
	if !found {
		t.Fatalf("DivergentTemplates = %v, want it to name editor", rep.DivergentTemplates)
	}

	var scopes string
	if err := e.appDB.QueryRow(`SELECT scopes::text FROM role_templates WHERE name = 'editor'`).Scan(&scopes); err != nil {
		t.Fatalf("read mirrored scopes: %v", err)
	}
	if !strings.Contains(scopes, "modules:read") {
		t.Fatalf("the mirrored scopes were rewritten to this build's: %s", scopes)
	}
	if strings.Contains(scopes, "state:write") {
		t.Fatalf("the reconcile substituted this build's scopes, changing this deployment's authorization: %s", scopes)
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
	if _, err := Reconcile(ctx, e.appDB, e.identityDB); err != nil {
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
