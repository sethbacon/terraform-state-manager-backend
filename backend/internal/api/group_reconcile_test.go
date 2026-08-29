package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"

	"github.com/terraform-state-manager/terraform-state-manager/internal/approles"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
)

// newReconcileEnv returns AuthHandlers over sqlmock for direct method tests.
//
// ONE database serves BOTH connections, so the app-side statements the role
// paths issue since the identity.role_templates reads were retired (template
// resolution, the mirror upsert/delete) land on the same ordered mock as the
// identity leg's, in call order. RoleSource stays identity in these rigs, so
// the role-carrying READS are byte-for-byte what they were.
func newReconcileEnv(t *testing.T, mutate func(*config.Config)) (*AuthHandlers, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := newSQLMock()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := &config.Config{}
	// The rollback source, stated: these rigs stage identity-shaped rows for
	// every role-carrying read, which is exactly what RoleSource=identity
	// serves. The app tables still take every WRITE under either source.
	cfg.Authz.RoleSource = string(approles.RoleSourceIdentity)
	if mutate != nil {
		mutate(cfg)
	}
	h, err := NewAuthHandlers(cfg, db, db)
	if err != nil {
		t.Fatalf("NewAuthHandlers: %v", err)
	}
	return h, mock
}

var orgRowCols = []string{"id", "name", "display_name", "idp_type", "idp_name", "created_at", "updated_at"}
var memberRowCols = []string{"organization_id", "user_id", "role_template_id", "created_at"}

func expectOrgByName(mock sqlmock.Sqlmock, id, name string) {
	now := time.Now()
	mock.ExpectQuery("FROM organizations").WithArgs(name).
		WillReturnRows(sqlmock.NewRows(orgRowCols).AddRow(id, name, name, nil, nil, now, now))
}

var roleTemplateCols = []string{"id", "name", "display_name", "description", "scopes", "is_system", "created_at", "updated_at"}

// expectRoleScopesLookup queues the guardProvisionableRole scopes lookup that
// runs before every "wanted" (add/update) branch of reconcileManagedMemberships.
// Since the identity.role_templates reads were retired it resolves from THIS
// application's own role_templates (approles.Store.TemplateByName), whose
// statement spells its COALESCEs — which is also what keeps it distinguishable
// from the identity leg's own name lookup on this shared mock.
func expectRoleScopesLookup(mock sqlmock.Sqlmock, roleName string, scopes []string) {
	scopesJSON, _ := json.Marshal(scopes)
	now := time.Now()
	mock.ExpectQuery(`SELECT id, name, COALESCE\(display_name`).WithArgs(roleName).
		WillReturnRows(sqlmock.NewRows(roleTemplateCols).
			AddRow(uuid.New(), roleName, roleName, nil, scopesJSON, false, now, now))
}

// expectMirrorRoleResolution queues the app-side name resolution the mirror leg
// performs (approles.Store.TemplateIDByName), which precedes the identity leg.
func expectMirrorRoleResolution(mock sqlmock.Sqlmock, roleName, id string) {
	mock.ExpectQuery("SELECT id FROM role_templates WHERE name").WithArgs(roleName).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(id))
}

// expectMirrorPriorRole queues the mirror's pre-write read of the role it
// currently records, which is what the no-op session-sweep detection compares
// against (#491). Absent row: uncertainty, which costs a sweep, never a missed
// reduction.
func expectMirrorPriorRoleAbsent(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT r\.role_template_id`).
		WillReturnRows(sqlmock.NewRows([]string{"role_template_id", "name", "display_name", "scopes"}))
}

// expectMirrorUpsert queues the mirror leg's write into this application's own
// organization_member_roles.
func expectMirrorUpsert(mock sqlmock.Sqlmock) {
	mock.ExpectExec("INSERT INTO organization_member_roles").
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestReconcile_UpsertExistingMember(t *testing.T) {
	h, mock := newReconcileEnv(t, nil)

	expectOrgByName(mock, "o1", "platform")
	// Already a member → role update: guard scopes lookup, the mirror's own
	// name resolution and prior-role read, then the identity leg's lookup and
	// UPDATE, then the mirror upsert.
	mock.ExpectQuery("FROM organization_members").WithArgs("o1", "u1", []string{"o1"}).
		WillReturnRows(sqlmock.NewRows(memberRowCols).AddRow("o1", "u1", nil, time.Now()))
	expectRoleScopesLookup(mock, "editor", []string{"state:read", "state:write"})
	expectMirrorRoleResolution(mock, "editor", "rt-editor")
	expectMirrorPriorRoleAbsent(mock)
	mock.ExpectQuery("SELECT id FROM role_templates WHERE name").WithArgs("editor").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("rt-editor"))
	mock.ExpectExec("UPDATE organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectMirrorUpsert(mock)

	err := h.reconcileManagedMemberships(context.Background(), "u1",
		map[string]string{"platform": "editor"}, map[string]struct{}{"platform": {}}, nil, "")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("existing member must get a role UPDATE, not an INSERT: %v", err)
	}
}

func TestReconcile_AddsNewMember(t *testing.T) {
	h, mock := newReconcileEnv(t, nil)

	expectOrgByName(mock, "o1", "platform")
	mock.ExpectQuery("FROM organization_members").WithArgs("o1", "u1", []string{"o1"}).
		WillReturnRows(sqlmock.NewRows(memberRowCols)) // not a member
	expectRoleScopesLookup(mock, "viewer", []string{"state:read"})
	expectMirrorRoleResolution(mock, "viewer", "rt-viewer")
	mock.ExpectQuery("SELECT id FROM role_templates WHERE name").WithArgs("viewer").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("rt-viewer"))
	mock.ExpectExec("INSERT INTO organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectMirrorUpsert(mock)

	err := h.reconcileManagedMemberships(context.Background(), "u1",
		map[string]string{"platform": "viewer"}, map[string]struct{}{"platform": {}}, nil, "")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("new member must be inserted: %v", err)
	}
}

func TestReconcile_DeprovisionsOnGroupLoss(t *testing.T) {
	h, mock := newReconcileEnv(t, nil)

	// Managed org, no desired entry, currently a member → membership revoked.
	expectOrgByName(mock, "o1", "platform")
	mock.ExpectQuery("FROM organization_members").WithArgs("o1", "u1", []string{"o1"}).
		WillReturnRows(sqlmock.NewRows(memberRowCols).AddRow("o1", "u1", "rt-editor", time.Now()))
	// REVOCATION: the mirror's delete goes FIRST (see approles.Members).
	mock.ExpectExec("DELETE FROM organization_member_roles").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM organization_members").WithArgs("o1", "u1", []string{"o1"}).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := h.reconcileManagedMemberships(context.Background(), "u1",
		map[string]string{}, map[string]struct{}{"platform": {}}, nil, "")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("losing the IdP group must revoke the managed membership: %v", err)
	}
}

func TestReconcile_UnknownOrgIsSkipped(t *testing.T) {
	h, mock := newReconcileEnv(t, nil)

	mock.ExpectQuery("FROM organizations").WithArgs("ghost-org").
		WillReturnRows(sqlmock.NewRows(orgRowCols))

	err := h.reconcileManagedMemberships(context.Background(), "u1",
		map[string]string{"ghost-org": "viewer"}, map[string]struct{}{"ghost-org": {}}, nil, "")
	if err != nil {
		t.Fatalf("a mapping to a missing org must be skipped, not fatal: %v", err)
	}
}

func TestReconcile_DefaultRoleFirstLoginOnly(t *testing.T) {
	// First login: no memberships → default role added. default_role is a
	// static, admin-configured fallback (not an IdP-driven group mapping), so
	// guardProvisionableRole does not run here — no scopes lookup is expected,
	// unlike the "wanted" branch tests above.
	h, mock := newReconcileEnv(t, nil)
	expectOrgByName(mock, "o-def", "default") // GetDefaultOrganization → GetByName("default")
	mock.ExpectQuery("FROM organization_members").WithArgs("o-def", "u1", []string{"o-def"}).
		WillReturnRows(sqlmock.NewRows(memberRowCols))
	expectMirrorRoleResolution(mock, "viewer", "rt-viewer")
	mock.ExpectQuery("SELECT id FROM role_templates WHERE name").WithArgs("viewer").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("rt-viewer"))
	mock.ExpectExec("INSERT INTO organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectMirrorUpsert(mock)

	if err := h.reconcileManagedMemberships(context.Background(), "u1", nil, nil, nil, "viewer"); err != nil {
		t.Fatalf("first login: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("first login must add the default role: %v", err)
	}

	// Existing member: the default role must NOT overwrite their role.
	h2, mock2 := newReconcileEnv(t, nil)
	expectOrgByName(mock2, "o-def", "default")
	mock2.ExpectQuery("FROM organization_members").WithArgs("o-def", "u1", []string{"o-def"}).
		WillReturnRows(sqlmock.NewRows(memberRowCols).AddRow("o-def", "u1", "rt-admin", time.Now()))

	if err := h2.reconcileManagedMemberships(context.Background(), "u1", nil, nil, nil, "viewer"); err != nil {
		t.Fatalf("existing member: %v", err)
	}
	if err := mock2.ExpectationsWereMet(); err != nil {
		t.Errorf("default role must be first-login-only: %v", err)
	}
}

func TestReconcile_SkipsManagedDefaultOrgFallback(t *testing.T) {
	// When the default org is itself IdP-managed, the fallback must not run —
	// reconciliation above is authoritative for it.
	h, mock := newReconcileEnv(t, nil)
	expectOrgByName(mock, "o-def", "default") // managed loop reconciles it (not a member, nothing desired)
	mock.ExpectQuery("FROM organization_members").WithArgs("o-def", "u1", []string{"o-def"}).
		WillReturnRows(sqlmock.NewRows(memberRowCols))
	expectOrgByName(mock, "o-def", "default") // GetDefaultOrganization lookup

	err := h.reconcileManagedMemberships(context.Background(), "u1",
		map[string]string{}, map[string]struct{}{"default": {}}, nil, "viewer")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("managed default org must not get the fallback INSERT: %v", err)
	}
}

// ---------------------------------------------------------------------------
// reconcileManagedMemberships — ScopeAdmin guard (issue #173, defense-in-depth
// adoption of terraform-suite-identity's ValidateProvisionableScopes). An
// IdP-driven group mapping that resolves to a role_template carrying
// auth.ScopeAdmin must be refused automatically, not silently granted.
// ---------------------------------------------------------------------------

// New member add is REJECTED when the resolved role_template's scopes carry
// ScopeAdmin: no role-template-id lookup or INSERT is issued, and no error is
// returned (the login itself still succeeds — only the automatic grant is
// refused and logged).
func TestReconcile_ResolvedRoleCarriesScopeAdmin_AddPath_Rejected(t *testing.T) {
	h, mock := newReconcileEnv(t, nil)

	expectOrgByName(mock, "o1", "platform")
	mock.ExpectQuery("FROM organization_members").WithArgs("o1", "u1", []string{"o1"}).
		WillReturnRows(sqlmock.NewRows(memberRowCols)) // not a member
	// guardProvisionableRole's scopes lookup returns the grant-all wildcard —
	// AddMemberWithParams's own role-template-id lookup and INSERT must NEVER
	// be reached.
	expectRoleScopesLookup(mock, "admin", []string{"admin"})

	err := h.reconcileManagedMemberships(context.Background(), "u1",
		map[string]string{"platform": "admin"}, map[string]struct{}{"platform": {}}, nil, "")
	if err != nil {
		t.Fatalf("reconcile: unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations (no role-template-id lookup/INSERT should have been issued): %v", err)
	}
}

// Existing member's role UPDATE is REJECTED the same way: the membership is
// left untouched (not upgraded, and — importantly — not revoked either).
func TestReconcile_ResolvedRoleCarriesScopeAdmin_UpdatePath_Rejected(t *testing.T) {
	h, mock := newReconcileEnv(t, nil)

	expectOrgByName(mock, "o1", "platform")
	mock.ExpectQuery("FROM organization_members").WithArgs("o1", "u1", []string{"o1"}).
		WillReturnRows(sqlmock.NewRows(memberRowCols).AddRow("o1", "u1", "rt-editor", time.Now()))
	expectRoleScopesLookup(mock, "admin", []string{"admin"})
	// No UPDATE (or the revoke DELETE) must follow.

	err := h.reconcileManagedMemberships(context.Background(), "u1",
		map[string]string{"platform": "admin"}, map[string]struct{}{"platform": {}}, nil, "")
	if err != nil {
		t.Fatalf("reconcile: unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations (no UPDATE/DELETE should have been issued): %v", err)
	}
}

// A role_template whose scopes are a normal, non-admin set proceeds exactly as
// before (already covered by the success-path tests above); this test only
// pins down the "role name doesn't exist" edge case: guardProvisionableRole
// must defer to AddMemberWithParams's own lookup error rather than swallowing
// or duplicating it.
func TestReconcile_GuardProvisionableRole_UnknownRoleTemplate_DefersToRealLookup(t *testing.T) {
	h, mock := newReconcileEnv(t, nil)

	expectOrgByName(mock, "o1", "platform")
	mock.ExpectQuery("FROM organization_members").WithArgs("o1", "u1", []string{"o1"}).
		WillReturnRows(sqlmock.NewRows(memberRowCols)) // not a member
	// guardProvisionableRole's own scopes lookup finds no such role template.
	mock.ExpectQuery(`SELECT id, name, COALESCE\(display_name`).WithArgs("ghost-role").
		WillReturnRows(sqlmock.NewRows(roleTemplateCols))
	// AddMemberWithParams's own resolution — against THIS application's
	// role_templates, before any leg writes — is reached and fails with its own
	// clear error. Identity is never asked.
	mock.ExpectQuery("SELECT id FROM role_templates WHERE name").WithArgs("ghost-role").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	err := h.reconcileManagedMemberships(context.Background(), "u1",
		map[string]string{"platform": "ghost-role"}, map[string]struct{}{"platform": {}}, nil, "")
	if err == nil {
		t.Fatal("expected error from AddMemberWithParams' role-template resolution, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestApplyGroupMappings_EndToEnd(t *testing.T) {
	h, mock := newReconcileEnv(t, func(cfg *config.Config) {
		cfg.Auth.OIDC.GroupMappings = []config.OIDCGroupMapping{
			{Group: "platform", Organization: "default", Role: "editor"},
		}
	})

	// effectiveOIDCGroupConfig: no DB overlay → file config governs.
	mock.ExpectQuery("SELECT oidc_group_claim_name").
		WillReturnRows(sqlmock.NewRows([]string{"oidc_group_claim_name", "oidc_default_role", "oidc_group_mappings", "updated_at"}))
	expectOrgByName(mock, "o-def", "default")
	mock.ExpectQuery("FROM organization_members").WithArgs("o-def", "u1", []string{"o-def"}).
		WillReturnRows(sqlmock.NewRows(memberRowCols))
	expectRoleScopesLookup(mock, "editor", []string{"state:read", "state:write"})
	expectMirrorRoleResolution(mock, "editor", "rt-editor")
	mock.ExpectQuery("SELECT id FROM role_templates WHERE name").WithArgs("editor").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("rt-editor"))
	mock.ExpectExec("INSERT INTO organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectMirrorUpsert(mock)

	if err := h.applyGroupMappings(context.Background(), "u1", []string{"platform"}); err != nil {
		t.Fatalf("applyGroupMappings: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("verified group must grant the mapped role: %v", err)
	}

	// No mappings and no default role → a no-op without any DB traffic beyond
	// the overlay read.
	h2, mock2 := newReconcileEnv(t, nil)
	mock2.ExpectQuery("SELECT oidc_group_claim_name").
		WillReturnRows(sqlmock.NewRows([]string{"oidc_group_claim_name", "oidc_default_role", "oidc_group_mappings", "updated_at"}))
	if err := h2.applyGroupMappings(context.Background(), "u1", []string{"any"}); err != nil {
		t.Fatalf("no-op path: %v", err)
	}
}

// #488 — THE SILENT DEMOTION. This is the regression the precedence flip would
// have caused, and the reason the guard reads every matching role rather than
// the winner.
//
// Two mappings match this user for one organization: `editor` and `admin`.
// Under first-wins the WINNER is `editor` — a role that passes
// guardProvisionableRole perfectly well. Guarding only the winner would
// therefore apply it, and UpdateMemberRole would demote a real administrator
// with no error, no warning, and nothing visible until they lost authority at
// their next login.
//
// Staging no UPDATE and no DELETE is the assertion: the organization must be
// left entirely alone because a REFUSED role also matched, whichever one won.
func TestReconcile_AdminMappingThatLosesStillPreservesTheMembership(t *testing.T) {
	h, mock := newReconcileEnv(t, nil)

	expectOrgByName(mock, "o1", "platform")
	mock.ExpectQuery("FROM organization_members").WithArgs("o1", "u1", []string{"o1"}).
		WillReturnRows(sqlmock.NewRows(memberRowCols).AddRow("o1", "u1", "rt-admin", time.Now()))
	// The guard walks the matching roles in order and stops at the refused one.
	expectRoleScopesLookup(mock, "editor", []string{"states:read", "states:write"})
	expectRoleScopesLookup(mock, "admin", []string{"admin"})

	err := h.reconcileManagedMemberships(context.Background(), "u1",
		// desired is the WINNER under first-wins: the non-admin role.
		map[string]string{"platform": "editor"},
		map[string]struct{}{"platform": {}},
		// ...but both roles matched, and that is what the guard must see.
		map[string][]string{"platform": {"editor", "admin"}},
		"")
	if err != nil {
		t.Fatalf("reconcile: unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations — no UPDATE or DELETE may be issued when a refused role also matched: %v", err)
	}
}
