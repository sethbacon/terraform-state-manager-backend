package api

import (
	"context"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
)

// newReconcileEnv returns AuthHandlers over sqlmock for direct method tests.
func newReconcileEnv(t *testing.T, mutate func(*config.Config)) (*AuthHandlers, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := &config.Config{}
	if mutate != nil {
		mutate(cfg)
	}
	h, err := NewAuthHandlers(cfg, db)
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

func TestReconcile_UpsertExistingMember(t *testing.T) {
	h, mock := newReconcileEnv(t, nil)

	expectOrgByName(mock, "o1", "platform")
	// Already a member → role update (template id lookup + UPDATE).
	mock.ExpectQuery("FROM organization_members").WithArgs("o1", "u1").
		WillReturnRows(sqlmock.NewRows(memberRowCols).AddRow("o1", "u1", nil, time.Now()))
	mock.ExpectQuery("SELECT id FROM role_templates WHERE name").WithArgs("editor").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("rt-editor"))
	mock.ExpectExec("UPDATE organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := h.reconcileManagedMemberships(context.Background(), "u1",
		map[string]string{"platform": "editor"}, map[string]struct{}{"platform": {}}, "")
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
	mock.ExpectQuery("FROM organization_members").WithArgs("o1", "u1").
		WillReturnRows(sqlmock.NewRows(memberRowCols)) // not a member
	mock.ExpectQuery("SELECT id FROM role_templates WHERE name").WithArgs("viewer").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("rt-viewer"))
	mock.ExpectExec("INSERT INTO organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := h.reconcileManagedMemberships(context.Background(), "u1",
		map[string]string{"platform": "viewer"}, map[string]struct{}{"platform": {}}, "")
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
	mock.ExpectQuery("FROM organization_members").WithArgs("o1", "u1").
		WillReturnRows(sqlmock.NewRows(memberRowCols).AddRow("o1", "u1", "rt-editor", time.Now()))
	mock.ExpectExec("DELETE FROM organization_members").WithArgs("o1", "u1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := h.reconcileManagedMemberships(context.Background(), "u1",
		map[string]string{}, map[string]struct{}{"platform": {}}, "")
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
		map[string]string{"ghost-org": "viewer"}, map[string]struct{}{"ghost-org": {}}, "")
	if err != nil {
		t.Fatalf("a mapping to a missing org must be skipped, not fatal: %v", err)
	}
}

func TestReconcile_DefaultRoleFirstLoginOnly(t *testing.T) {
	// First login: no memberships → default role added.
	h, mock := newReconcileEnv(t, nil)
	expectOrgByName(mock, "o-def", "default") // GetDefaultOrganization → GetByName("default")
	mock.ExpectQuery("FROM organization_members").WithArgs("o-def", "u1").
		WillReturnRows(sqlmock.NewRows(memberRowCols))
	mock.ExpectQuery("SELECT id FROM role_templates WHERE name").WithArgs("viewer").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("rt-viewer"))
	mock.ExpectExec("INSERT INTO organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := h.reconcileManagedMemberships(context.Background(), "u1", nil, nil, "viewer"); err != nil {
		t.Fatalf("first login: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("first login must add the default role: %v", err)
	}

	// Existing member: the default role must NOT overwrite their role.
	h2, mock2 := newReconcileEnv(t, nil)
	expectOrgByName(mock2, "o-def", "default")
	mock2.ExpectQuery("FROM organization_members").WithArgs("o-def", "u1").
		WillReturnRows(sqlmock.NewRows(memberRowCols).AddRow("o-def", "u1", "rt-admin", time.Now()))

	if err := h2.reconcileManagedMemberships(context.Background(), "u1", nil, nil, "viewer"); err != nil {
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
	mock.ExpectQuery("FROM organization_members").WithArgs("o-def", "u1").
		WillReturnRows(sqlmock.NewRows(memberRowCols))
	expectOrgByName(mock, "o-def", "default") // GetDefaultOrganization lookup

	err := h.reconcileManagedMemberships(context.Background(), "u1",
		map[string]string{}, map[string]struct{}{"default": {}}, "viewer")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("managed default org must not get the fallback INSERT: %v", err)
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
	mock.ExpectQuery("FROM organization_members").WithArgs("o-def", "u1").
		WillReturnRows(sqlmock.NewRows(memberRowCols))
	mock.ExpectQuery("SELECT id FROM role_templates WHERE name").WithArgs("editor").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("rt-editor"))
	mock.ExpectExec("INSERT INTO organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))

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
