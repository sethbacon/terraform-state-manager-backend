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

	// The default organization first: the identity-side seed moved AFTER the
	// reconcile when it started restating the app table's ids into identity.
	mock.ExpectExec("INSERT INTO organizations").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// One upsert per app role template, in declaration order. With no app
	// connection there is no app id space to restate, so the id argument is
	// nil and identity mints one (COALESCE with gen_random_uuid).
	for _, rt := range auth.AppRoleTemplates() {
		mock.ExpectExec("INSERT INTO role_templates").
			WithArgs(nil, rt.Name, rt.DisplayName, rt.Description, sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}

	if err := Run(context.Background(), db, nil, true, nil); err != nil {
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

	mock.ExpectExec("INSERT INTO organizations").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO role_templates").
		WillReturnError(errors.New("boom"))

	err = Run(context.Background(), db, nil, true, nil)
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

	mock.ExpectExec("INSERT INTO organizations").
		WillReturnError(errors.New("boom"))
	// The identity-side seed never runs: ensuring the default organization
	// failed first. sqlmock fails the test on any unexpected Exec.

	err = Run(context.Background(), db, nil, true, nil)
	if err == nil {
		t.Fatal("Run() succeeded despite a default-org failure")
	}
}

func TestRun_SkipsRoleSeedWhenNotOwner(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	// seedRoles=false: NO role_templates upserts expected (sqlmock fails on any
	// unexpected Exec); only the default organization is ensured.
	mock.ExpectExec("INSERT INTO organizations").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := Run(context.Background(), db, nil, false, nil); err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
