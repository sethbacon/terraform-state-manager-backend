package repositories

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// #439 — FindConsumersInScope's control flow. The scoped path's tenant predicate
// is a JOIN, and a mock cannot evaluate one: it returns whatever the fixture
// declares regardless of what the join would have excluded. So what is asserted
// here is the branching around it, which is ordinary Go, plus the SHAPE of the
// statement each branch issues.

func TestFindConsumersInScope_EmptyScopeQueriesNothing(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateModuleRefRepository(db)

	// No expectation registered: any query at all fails the test. An empty scope
	// is a caller who may see nothing, which must not fall through to the
	// unscoped reader.
	got, err := r.FindConsumersInScope(context.Background(), scopeNone(),
		[]string{"registry.example.com"}, "acme/vpc/aws")
	if err != nil {
		t.Fatalf("FindConsumersInScope: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d consumers, want none", len(got))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("an empty scope reached the database: %v", err)
	}
}

// A platform admin delegates to the unscoped reader, which is how the LEFT JOIN
// (and therefore parentless rows) is preserved for them.
func TestFindConsumersInScope_PlatformAdminDelegatesUnfiltered(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateModuleRefRepository(db)

	cols := []string{"source_id", "source_name", "state_key", "module_version", "observed_at"}
	// LEFT JOIN, and exactly two bound arguments: no organization predicate.
	mock.ExpectQuery("LEFT JOIN state_sources").
		WithArgs([]string{"registry.example.com"}, "acme/vpc/aws").
		WillReturnRows(sqlmock.NewRows(cols).AddRow("s1", "prod", "app.tfstate", nil, "2026-06-14"))

	got, err := r.FindConsumersInScope(context.Background(), scopeAdmin(),
		[]string{"registry.example.com"}, "acme/vpc/aws")
	if err != nil {
		t.Fatalf("FindConsumersInScope: %v", err)
	}
	if len(got) != 1 || got[0].SourceName != "prod" {
		t.Errorf("got %+v, want the unscoped row", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// The scoped branch must bind the organizations as a THIRD argument and join
// through state_sources -- state_module_refs carries no organization_id.
func TestFindConsumersInScope_ScopedQueryBindsTheOrganizations(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateModuleRefRepository(db)

	cols := []string{"source_id", "source_name", "state_key", "module_version", "observed_at"}
	mock.ExpectQuery("JOIN state_sources s ON s.id = r.source_id AND s.organization_id").
		WithArgs([]string{"registry.example.com"}, "acme/vpc/aws",
			[]string{"aaaaaaaa-0000-4000-8000-000000000001"}).
		WillReturnRows(sqlmock.NewRows(cols).AddRow("s1", "prod", "app.tfstate", "1.2.3", "2026-06-14"))

	got, err := r.FindConsumersInScope(context.Background(), scopeOne(),
		[]string{"registry.example.com"}, "acme/vpc/aws")
	if err != nil {
		t.Fatalf("FindConsumersInScope: %v", err)
	}
	if len(got) != 1 || got[0].StateKey != "app.tfstate" {
		t.Errorf("got %+v, want the scoped row", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A scan failure must surface rather than being swallowed into an empty result,
// which would read as "this organization consumes nothing".
func TestFindConsumersInScope_ScanErrorSurfaces(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateModuleRefRepository(db)

	mock.ExpectQuery("JOIN state_sources s ON s.id = r.source_id AND s.organization_id").
		WillReturnRows(sqlmock.NewRows([]string{"source_id"}).AddRow("only-one-column"))

	if _, err := r.FindConsumersInScope(context.Background(), scopeOne(),
		[]string{"registry.example.com"}, "acme/vpc/aws"); err == nil {
		t.Error("a scan failure must surface, not read as an empty result")
	}
}
