package repositories

import (
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func TestStateModuleRefRepository_ReplaceForState(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateModuleRefRepository(db)

	ver := "5.1.0"
	refs := []StateModuleRef{
		{ModuleSource: "terraform-aws-modules/vpc/aws", RegistryHost: "registry.terraform.io"}, // nil version
		{ModuleSource: "acme/net/aws", RegistryHost: "app.terraform.io", ModuleVersion: &ver},
	}

	// Delete-then-insert in one transaction.
	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM state_module_refs").WithArgs("s1", "app.tfstate").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO state_module_refs").
		WithArgs("s1", "app.tfstate", "terraform-aws-modules/vpc/aws", nil, "registry.terraform.io").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO state_module_refs").
		WithArgs("s1", "app.tfstate", "acme/net/aws", "5.1.0", "app.terraform.io").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := r.ReplaceForState(ctx, "s1", "app.tfstate", refs); err != nil {
		t.Fatalf("ReplaceForState: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestStateModuleRefRepository_ReplaceForState_EmptyClears(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateModuleRefRepository(db)

	// An empty set still clears prior provenance (delete, no insert).
	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM state_module_refs").WithArgs("s1", "k").
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	if err := r.ReplaceForState(ctx, "s1", "k", nil); err != nil {
		t.Fatalf("ReplaceForState(empty): %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestStateModuleRefRepository_ListBySource(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateModuleRefRepository(db)

	cols := []string{"source_id", "state_key", "module_source", "module_version", "registry_host", "observed_at"}
	mock.ExpectQuery("FROM state_module_refs WHERE source_id").WithArgs("s1").
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("s1", "app.tfstate", "terraform-aws-modules/vpc/aws", nil, "registry.terraform.io", "2026-06-14"))

	out, err := r.ListBySource(ctx, "s1", "")
	if err != nil {
		t.Fatalf("ListBySource: %v", err)
	}
	if len(out) != 1 || out[0].ModuleSource != "terraform-aws-modules/vpc/aws" || out[0].ModuleVersion != nil {
		t.Fatalf("unexpected rows: %+v", out)
	}
}

func TestStateModuleRefRepository_FindConsumers_HostFilter(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateModuleRefRepository(db)

	cols := []string{"source_id", "source_name", "state_key", "module_version", "observed_at"}
	// Matches the canonical column against the alias set passed as a text[].
	mock.ExpectQuery("WHERE r.registry_host_canon = ANY.+ AND r.module_source").
		WithArgs([]string{"registry.terraform.io", "tf.example.com"}, "terraform-aws-modules/vpc/aws").
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("s1", "prod", "app.tfstate", nil, "2026-06-14"))

	out, err := r.FindConsumers(ctx, []string{"registry.terraform.io", "tf.example.com"}, "terraform-aws-modules/vpc/aws")
	if err != nil {
		t.Fatalf("FindConsumers: %v", err)
	}
	if len(out) != 1 || out[0].SourceName != "prod" {
		t.Fatalf("unexpected consumers: %+v", out)
	}
}
