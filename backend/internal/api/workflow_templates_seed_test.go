package api

import (
	"context"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func TestBuiltinWorkflowSeeds(t *testing.T) {
	seeds := builtinWorkflowSeeds()
	if len(seeds) != 4 {
		t.Fatalf("want 4 built-in seeds, got %d", len(seeds))
	}
	for _, s := range seeds {
		if s.Content == "" || !s.IsBuiltin || s.Profile != "default" {
			t.Errorf("malformed seed: %+v", s)
		}
	}
}

func TestSeedWorkflowTemplates_NilDBIsNoop(t *testing.T) {
	if err := SeedWorkflowTemplates(context.Background(), nil); err != nil {
		t.Fatalf("nil db should be a no-op: %v", err)
	}
}

func TestSeedWorkflowTemplates_InsertsBuiltins(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	for range builtinWorkflowSeeds() {
		mock.ExpectExec("INSERT INTO workflow_templates .+ ON CONFLICT").
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	if err := SeedWorkflowTemplates(context.Background(), db); err != nil {
		t.Fatalf("SeedWorkflowTemplates: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
