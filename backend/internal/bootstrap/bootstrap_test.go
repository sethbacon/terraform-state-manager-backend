package bootstrap

import (
	"context"
	"errors"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
)

func TestRun_SeedsAllRoleTemplatesAndDefaultOrg(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	// One upsert per app role template, in declaration order.
	for _, rt := range auth.AppRoleTemplates() {
		mock.ExpectExec("INSERT INTO role_templates").
			WithArgs(rt.Name, rt.DisplayName, rt.Description, sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectExec("INSERT INTO organizations").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := Run(context.Background(), db); err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestRun_RoleTemplateSeedFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec("INSERT INTO role_templates").
		WillReturnError(errors.New("boom"))

	err = Run(context.Background(), db)
	if err == nil {
		t.Fatal("Run() succeeded despite a role-template seed failure")
	}
}

func TestRun_DefaultOrgFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	for range auth.AppRoleTemplates() {
		mock.ExpectExec("INSERT INTO role_templates").
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectExec("INSERT INTO organizations").
		WillReturnError(errors.New("boom"))

	err = Run(context.Background(), db)
	if err == nil {
		t.Fatal("Run() succeeded despite a default-org failure")
	}
}
