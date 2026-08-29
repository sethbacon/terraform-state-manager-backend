//go:build integration

package repositories_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"

	appdb "github.com/terraform-state-manager/terraform-state-manager/internal/db"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// THE EQUIVALENCE PROOF for the group-mapping dual-write
// (sethbacon/terraform-suite-identity#206 phase 2, migration 000036), against
// real PostgreSQL and the real migrations.
//
// Everything else in this change argues that TSM's own group_mappings table
// stays equal to the oidc_group_mappings list in the sso_settings overlay.
// This file DEMONSTRATES it: it writes through the REAL write path --
// repositories.SSOSettingsRepository.Upsert, the same object the
// PUT /admin/oidc/group-mapping handler holds -- and then compares the two
// copies row-for-row, both against the stored bytes read back out of
// sso_settings and through CheckGroupMappingDrift, the check `authz-drift`
// runs against a live database.
//
// The falsification tests are the other half of the proof, and they are what
// make the rest of the file mean anything: each corrupts the mirror ONE way
// and asserts the comparison FAILS with the right kind -- a test that only
// ever sees agreement cannot distinguish "the two copies agree" from "the
// comparison is not looking at anything", and this estate has shipped guards
// that could not tell those apart. Each corruption is then repaired by
// ReconcileGroupMappings, the boot backfill, proving the repair path against
// every divergence class it claims to converge.
//
// These tests skip without TEST_DATABASE_URL and run in CI's postgres-tests
// job, whose grader (scripts/assert_integration_tests_ran.sh) fails if they
// skip.

// gmTestDatabaseName is this suite's OWN database, for the same reason
// tsm_analysis_scope_test is: package binaries run concurrently and this
// suite drops and re-creates its schema.
const gmTestDatabaseName = "tsm_group_mappings_test"

func newGroupMappingDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping PostgreSQL integration test")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = admin.Close() }()
	if err := admin.Ping(); err != nil {
		t.Skipf("TEST_DATABASE_URL unreachable (%v); skipping", err)
	}
	for _, stmt := range []string{
		`DROP DATABASE IF EXISTS ` + pgx.Identifier{gmTestDatabaseName}.Sanitize() + ` WITH (FORCE)`,
		`CREATE DATABASE ` + pgx.Identifier{gmTestDatabaseName}.Sanitize(),
	} {
		if _, err := admin.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	parsed.Path = "/" + gmTestDatabaseName
	db, err := sql.Open("pgx", parsed.String())
	if err != nil {
		t.Fatalf("open %s: %v", parsed.Redacted(), err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := appdb.RunMigrations(db, "up"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	return db
}

func gmExec(t *testing.T, db *sql.DB, query string, args ...interface{}) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// gmSeedTemplate creates one role template in TSM's OWN role_templates table
// -- the FK target -- and returns its id.
func gmSeedTemplate(t *testing.T, db *sql.DB, id, name string) string {
	t.Helper()
	gmExec(t, db, `INSERT INTO role_templates (id, name, display_name, scopes, is_system)
	               VALUES ($1, $2, $2, '["states:read"]'::jsonb, false)`, id, name)
	return id
}

// gmMirrorRow is one group_mappings row as read back for comparison.
type gmMirrorRow struct {
	Position       int
	Group          string
	Organization   string
	RoleName       string
	RoleTemplateID sql.NullString
}

func gmReadMirror(t *testing.T, db *sql.DB) []gmMirrorRow {
	t.Helper()
	rows, err := db.Query(`SELECT position, group_name, organization_name, role_template_name, role_template_id
	                         FROM group_mappings ORDER BY position`)
	if err != nil {
		t.Fatalf("read mirror: %v", err)
	}
	defer rows.Close()
	var out []gmMirrorRow
	for rows.Next() {
		var r gmMirrorRow
		if err := rows.Scan(&r.Position, &r.Group, &r.Organization, &r.RoleName, &r.RoleTemplateID); err != nil {
			t.Fatalf("scan mirror: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// gmRequireEqual compares the mirror rows against the SOURCE OF TRUTH read
// back out of sso_settings.oidc_group_mappings -- not against the list the
// test happens to hold in a variable. Row for row: position, group,
// organization, role name, and the role-template resolution.
func gmRequireEqual(t *testing.T, db *sql.DB, wantRoleIDs map[string]string) {
	t.Helper()
	var raw []byte
	if err := db.QueryRow(`SELECT oidc_group_mappings FROM sso_settings WHERE id = 1`).Scan(&raw); err != nil {
		t.Fatalf("read stored overlay: %v", err)
	}
	var source []repositories.SSOGroupMapping
	if err := json.Unmarshal(raw, &source); err != nil {
		t.Fatalf("decode stored overlay: %v", err)
	}
	mirror := gmReadMirror(t, db)
	if len(mirror) != len(source) {
		t.Fatalf("mirror has %d row(s), the stored overlay has %d mapping(s)", len(mirror), len(source))
	}
	for i, m := range source {
		got := mirror[i]
		if got.Position != i || got.Group != m.Group || got.Organization != m.Organization || got.RoleName != m.Role {
			t.Errorf("row %d: mirror (pos=%d group=%q org=%q role=%q) != source (group=%q org=%q role=%q)",
				i, got.Position, got.Group, got.Organization, got.RoleName, m.Group, m.Organization, m.Role)
		}
		wantID, resolvable := wantRoleIDs[m.Role]
		switch {
		case resolvable && (!got.RoleTemplateID.Valid || got.RoleTemplateID.String != wantID):
			t.Errorf("row %d: role %q should resolve to %s, mirror holds %v", i, m.Role, wantID, got.RoleTemplateID)
		case !resolvable && got.RoleTemplateID.Valid:
			t.Errorf("row %d: role %q resolves to nothing, mirror holds %s", i, m.Role, got.RoleTemplateID.String)
		}
	}
}

func gmRequireClean(t *testing.T, db *sql.DB) repositories.GroupMappingDriftReport {
	t.Helper()
	report, err := repositories.CheckGroupMappingDrift(context.Background(), db, db)
	if err != nil {
		t.Fatalf("CheckGroupMappingDrift: %v", err)
	}
	if !report.Clean() {
		for _, row := range report.Rows {
			t.Errorf("unexpected drift: %s", row.String())
		}
		t.Fatalf("expected the two copies to agree; %d drift row(s)", len(report.Rows))
	}
	return report
}

const (
	gmEditorID = "cccccccc-0000-4000-8000-000000000021"
	gmViewerID = "cccccccc-0000-4000-8000-000000000022"
)

// TestIntegrationGroupMappingEquivalence_RealWritePathKeepsBothCopiesEqual
// drives the three flavours the one write site carries -- the first save
// (INSERT side), an edit that reorders the survivors (UPDATE side, because
// first-match-wins makes order load-bearing), and saving an empty list
// (DELETE side) -- and proves the mirror equal after each.
func TestIntegrationGroupMappingEquivalence_RealWritePathKeepsBothCopiesEqual(t *testing.T) {
	db := newGroupMappingDB(t)
	ctx := context.Background()

	gmSeedTemplate(t, db, gmEditorID, "gm-editor")
	gmSeedTemplate(t, db, gmViewerID, "gm-viewer")
	roleIDs := map[string]string{"gm-editor": gmEditorID, "gm-viewer": gmViewerID}

	// The production wiring shape: one repository over the connection the
	// overlay resolves through and the app connection the mirror writes on --
	// the same handle here, as in every single-database deployment.
	repo := repositories.NewSSOSettingsRepository(db, db)

	// FIRST SAVE, including one mapping whose role names no template: a legal
	// row that must be mirrored faithfully with a NULL resolution, not dropped.
	err := repo.Upsert(ctx, &repositories.SSOSettings{
		OIDCGroupClaimName: "groups", OIDCDefaultRole: "gm-viewer",
		OIDCGroupMappings: []repositories.SSOGroupMapping{
			{Group: "eng", Organization: "alpha", Role: "gm-editor"},
			{Group: "eng", Organization: "beta", Role: "gm-viewer"},
			{Group: "ops", Organization: "alpha", Role: "gm-missing"},
		},
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	gmRequireEqual(t, db, roleIDs)
	report := gmRequireClean(t, db)
	if report.SourceMappings != 3 || report.MirroredMappings != 3 {
		t.Fatalf("gate compared %d source / %d mirrored mapping(s), want 3/3 -- a clean result that "+
			"looked at nothing proves nothing", report.SourceMappings, report.MirroredMappings)
	}

	// EDIT: the new list REORDERS the survivors, so a mirror that preserved
	// membership but not order fails here.
	err = repo.Upsert(ctx, &repositories.SSOSettings{
		OIDCGroupClaimName: "groups", OIDCDefaultRole: "gm-viewer",
		OIDCGroupMappings: []repositories.SSOGroupMapping{
			{Group: "eng", Organization: "beta", Role: "gm-viewer"},
			{Group: "eng", Organization: "alpha", Role: "gm-editor"},
		},
	})
	if err != nil {
		t.Fatalf("Upsert (reorder): %v", err)
	}
	gmRequireEqual(t, db, roleIDs)
	gmRequireClean(t, db)
	if got := gmReadMirror(t, db); len(got) != 2 || got[0].RoleName != "gm-viewer" {
		t.Fatalf("after the reorder the mirror should hold 2 rows led by gm-viewer, got %+v", got)
	}

	// EMPTY LIST: the admin removing every mapping removes them in both copies.
	err = repo.Upsert(ctx, &repositories.SSOSettings{
		OIDCGroupClaimName: "groups", OIDCDefaultRole: "",
		OIDCGroupMappings: []repositories.SSOGroupMapping{},
	})
	if err != nil {
		t.Fatalf("Upsert (empty): %v", err)
	}
	if got := gmReadMirror(t, db); len(got) != 0 {
		t.Fatalf("mirror rows survived the empty save: %+v", got)
	}
	gmRequireClean(t, db)
}

// TestIntegrationGroupMappingEquivalence_ComparisonFailsOnEveryDivergenceClass
// corrupts the mirror one way per divergence kind, asserts the gate reports
// EXACTLY that kind, and asserts the boot reconcile repairs it back to clean.
func TestIntegrationGroupMappingEquivalence_ComparisonFailsOnEveryDivergenceClass(t *testing.T) {
	db := newGroupMappingDB(t)
	ctx := context.Background()

	gmSeedTemplate(t, db, gmEditorID, "gm-editor")
	repo := repositories.NewSSOSettingsRepository(db, db)
	err := repo.Upsert(ctx, &repositories.SSOSettings{
		OIDCGroupClaimName: "groups",
		OIDCGroupMappings: []repositories.SSOGroupMapping{
			{Group: "eng", Organization: "alpha", Role: "gm-editor"},
			{Group: "ops", Organization: "beta", Role: "gm-editor"},
		},
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	gmRequireClean(t, db)

	cases := []struct {
		name    string
		corrupt func()
		kind    string
	}{
		{"a mirrored field is wrong", func() {
			gmExec(t, db, `UPDATE group_mappings SET organization_name='wrong' WHERE position=0`)
		}, repositories.GroupMappingDriftFieldsDiffer},
		{"a stored mapping is not mirrored", func() {
			gmExec(t, db, `DELETE FROM group_mappings WHERE position=1`)
		}, repositories.GroupMappingDriftNotMirrored},
		{"the mirror holds an extra row", func() {
			gmExec(t, db, `INSERT INTO group_mappings (position, group_name, organization_name, role_template_name)
			               VALUES (2, 'ghost', 'alpha', 'gm-editor')`)
		}, repositories.GroupMappingDriftMirrorOrphaned},
		{"the role resolution went stale", func() {
			gmExec(t, db, `UPDATE group_mappings SET role_template_id=NULL WHERE position=0`)
		}, repositories.GroupMappingDriftRoleRefStale},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.corrupt()
			report, err := repositories.CheckGroupMappingDrift(ctx, db, db)
			if err != nil {
				t.Fatalf("CheckGroupMappingDrift: %v", err)
			}
			if report.Clean() {
				t.Fatalf("the gate reported CLEAN over a corrupted mirror -- the comparison is not " +
					"looking at anything, which is the failure this file exists to rule out")
			}
			var sawKind bool
			for _, row := range report.Rows {
				if row.Kind == tc.kind {
					sawKind = true
				}
			}
			if !sawKind {
				t.Fatalf("expected a %s row, got: %+v", tc.kind, report.Rows)
			}
			// The boot backfill must repair exactly this class.
			if _, err := repositories.ReconcileGroupMappings(ctx, db, db); err != nil {
				t.Fatalf("ReconcileGroupMappings: %v", err)
			}
			gmRequireClean(t, db)
		})
	}
}

// TestIntegrationGroupMappingEquivalence_BackfillsAPreexistingOverlay is the
// upgrade story: an sso_settings row written BEFORE this change exists
// (inserted here by direct SQL, bypassing the repository exactly as the old
// binary's rows do), the mirror is empty, and one boot reconcile converges it.
// Also pins the gate's other duty -- reporting the unpopulated mirror BEFORE
// the backfill -- and the steady state after it.
func TestIntegrationGroupMappingEquivalence_BackfillsAPreexistingOverlay(t *testing.T) {
	db := newGroupMappingDB(t)
	ctx := context.Background()

	gmSeedTemplate(t, db, gmEditorID, "gm-editor")
	gmExec(t, db, `INSERT INTO sso_settings (id, oidc_group_claim_name, oidc_default_role, oidc_group_mappings)
	               VALUES (1, 'groups', 'gm-editor',
	                       '[{"group":"eng","organization":"alpha","role":"gm-editor"},{"group":"ops","organization":"beta","role":"gm-missing"}]'::jsonb)`)

	// Before the backfill the gate must refuse to call this clean.
	pre, err := repositories.CheckGroupMappingDrift(ctx, db, db)
	if err != nil {
		t.Fatalf("CheckGroupMappingDrift: %v", err)
	}
	if pre.Clean() {
		t.Fatal("the gate reported CLEAN over an unpopulated mirror with live stored mappings")
	}

	report, err := repositories.ReconcileGroupMappings(ctx, db, db)
	if err != nil {
		t.Fatalf("ReconcileGroupMappings: %v", err)
	}
	if !report.OverlayPresent || report.SourceMappings != 2 || !report.Rewritten ||
		report.UnresolvedRoleRefs != 1 || report.OverlayUnparseable {
		t.Fatalf("backfill report does not match the seeded estate: %+v", report)
	}
	gmRequireEqual(t, db, map[string]string{"gm-editor": gmEditorID})
	post := gmRequireClean(t, db)
	if post.SourceMappings != 2 || post.MirroredMappings != 2 {
		t.Fatalf("gate compared %d/%d mapping(s) after the backfill, want 2/2", post.SourceMappings, post.MirroredMappings)
	}

	// A second reconcile over a converged estate writes nothing -- the
	// steady-state property that makes running this on every boot cheap.
	again, err := repositories.ReconcileGroupMappings(ctx, db, db)
	if err != nil {
		t.Fatalf("second ReconcileGroupMappings: %v", err)
	}
	if again.Rewritten {
		t.Fatal("a no-change reconcile rewrote the mirror; steady state must write nothing")
	}
}

// TestIntegrationGroupMappingEquivalence_TemplateLifecycleDegradesAndReconverges
// exercises the one real FK. Deleting the mapped template must degrade the
// mirror row to NULL (ON DELETE SET NULL) -- which is CLEAN, because the name
// now resolves to nothing on both sides, exactly the "mapping confers nothing
// at login" semantics -- and re-creating the template under a NEW id must
// surface as stale resolution until the reconcile re-resolves it.
func TestIntegrationGroupMappingEquivalence_TemplateLifecycleDegradesAndReconverges(t *testing.T) {
	db := newGroupMappingDB(t)
	ctx := context.Background()

	gmSeedTemplate(t, db, gmEditorID, "gm-editor")
	repo := repositories.NewSSOSettingsRepository(db, db)
	err := repo.Upsert(ctx, &repositories.SSOSettings{
		OIDCGroupMappings: []repositories.SSOGroupMapping{{Group: "eng", Organization: "alpha", Role: "gm-editor"}},
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	gmRequireClean(t, db)

	gmExec(t, db, `DELETE FROM role_templates WHERE id = $1`, gmEditorID)
	rows := gmReadMirror(t, db)
	if len(rows) != 1 || rows[0].RoleTemplateID.Valid {
		t.Fatalf("the FK should have degraded the resolution to NULL, got %+v", rows)
	}
	gmRequireClean(t, db)

	gmSeedTemplate(t, db, gmViewerID, "gm-editor") // same name, NEW id
	report, err := repositories.CheckGroupMappingDrift(ctx, db, db)
	if err != nil {
		t.Fatalf("CheckGroupMappingDrift: %v", err)
	}
	if report.Clean() || report.Rows[0].Kind != repositories.GroupMappingDriftRoleRefStale {
		t.Fatalf("a re-created template must read as stale resolution, got %+v", report.Rows)
	}
	if _, err := repositories.ReconcileGroupMappings(ctx, db, db); err != nil {
		t.Fatalf("ReconcileGroupMappings: %v", err)
	}
	gmRequireEqual(t, db, map[string]string{"gm-editor": gmViewerID})
	gmRequireClean(t, db)
}

// TestIntegrationGroupMappingEquivalence_RefusesToRunWithoutTheTable pins the
// structural failure mode: on a database that predates migration 000036 both
// the reconcile and the gate must ERROR -- never report clean, never quietly
// write nowhere. "Could not check" and "checked and found nothing" must stay
// distinguishable, or an upgrade-ordering mistake gates the cutover open.
func TestIntegrationGroupMappingEquivalence_RefusesToRunWithoutTheTable(t *testing.T) {
	db := newGroupMappingDB(t)
	ctx := context.Background()

	// The down migration's own statements, so this also exercises the rollback
	// path 000036 documents as safe while nothing reads the table.
	gmExec(t, db, `DROP INDEX IF EXISTS idx_group_mappings_role_template`)
	gmExec(t, db, `DROP TABLE IF EXISTS group_mappings`)

	if _, err := repositories.ReconcileGroupMappings(ctx, db, db); err == nil {
		t.Fatal("ReconcileGroupMappings succeeded against a database without group_mappings")
	}
	if _, err := repositories.CheckGroupMappingDrift(ctx, db, db); err == nil {
		t.Fatal("CheckGroupMappingDrift succeeded against a database without group_mappings")
	}
}

// TestIntegrationGroupMappingMirror_RefusesAMisroutedConnection proves the
// misrouting refusal against a real search_path, not a mock: a connection that
// resolves identity's organization_members unqualified -- the exact
// misconfiguration migration 000032's pre-check refuses -- must be refused by
// the mirror too, or its writes would land in the shared schema.
func TestIntegrationGroupMappingMirror_RefusesAMisroutedConnection(t *testing.T) {
	db := newGroupMappingDB(t)
	ctx := context.Background()

	gmExec(t, db, `CREATE SCHEMA IF NOT EXISTS identity`)
	gmExec(t, db, `CREATE TABLE IF NOT EXISTS identity.organization_members (id UUID PRIMARY KEY)`)

	// A SECOND pool whose every connection carries the identity search_path --
	// the same spelling config.GetDSNWithSearchPath gives the real identity
	// pool -- so the misrouting is a property of the connection, not of one
	// pooled session a SET happened to reach.
	dsn := os.Getenv("TEST_DATABASE_URL")
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	parsed.Path = "/" + gmTestDatabaseName
	q := parsed.Query()
	q.Set("options", "-c search_path=identity,public")
	parsed.RawQuery = q.Encode()
	misrouted, err := sql.Open("pgx", parsed.String())
	if err != nil {
		t.Fatalf("open misrouted pool: %v", err)
	}
	t.Cleanup(func() { _ = misrouted.Close() })

	if err := repositories.NewGroupMappingMirror(misrouted).Verify(ctx); err == nil {
		t.Fatal("Verify accepted a connection that resolves identity's tables unqualified")
	}
	if _, err := repositories.ReconcileGroupMappings(ctx, db, misrouted); err == nil {
		t.Fatal("ReconcileGroupMappings ran over a misrouted app connection")
	}
}
