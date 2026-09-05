package api

import (
	"context"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func TestBuiltinWorkflowSeeds(t *testing.T) {
	seeds := builtinWorkflowSeeds()
	// 4 default + 4 suite (drift/versionlab × github/azure) + 1 fan-out
	// (drift, azure_devops only).
	if len(seeds) != 9 {
		t.Fatalf("want 9 built-in seeds, got %d", len(seeds))
	}
	for _, s := range seeds {
		if s.Content == "" || !s.IsBuiltin || (s.Profile != "default" && s.Profile != "suite" && s.Profile != "fan-out") {
			t.Errorf("malformed seed: %+v", s)
		}
		if s.Profile == "fan-out" && (s.Provider != "azure_devops" || s.Kind != "drift") {
			t.Errorf("fan-out is only defined for azure_devops/drift, got %+v", s)
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
