package repositories

import (
	"database/sql"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

var wtCols = []string{"id", "provider", "kind", "profile", "name", "description", "content", "is_builtin", "created_at", "updated_at"}

func wtRow() *sqlmock.Rows {
	return sqlmock.NewRows(wtCols).
		AddRow("t1", "azure_devops", "drift", "default", "Azure Drift", "", "trigger: none", true, "2026-06-18", "2026-06-18")
}

func TestWorkflowTemplateRepository_GetByKey(t *testing.T) {
	db, mock := newMock(t)
	r := NewWorkflowTemplateRepository(db)

	mock.ExpectQuery("SELECT .+ FROM workflow_templates WHERE provider = .+ AND kind = .+ AND profile = .+").
		WithArgs("azure_devops", "drift", "default").WillReturnRows(wtRow())
	got, err := r.GetByKey(ctx, "azure_devops", "drift", "default")
	if err != nil || got == nil || got.Provider != "azure_devops" || got.Content != "trigger: none" {
		t.Fatalf("GetByKey: %v %+v", err, got)
	}

	// Not found -> (nil, nil) so the handler can fall back to the const.
	mock.ExpectQuery("SELECT .+ FROM workflow_templates WHERE provider").
		WithArgs("github_actions", "drift", "missing").WillReturnError(sql.ErrNoRows)
	got, err = r.GetByKey(ctx, "github_actions", "drift", "missing")
	if err != nil || got != nil {
		t.Fatalf("GetByKey missing should be (nil,nil): %v %+v", err, got)
	}

	mock.ExpectQuery("SELECT .+ FROM workflow_templates WHERE provider").WillReturnError(errDB)
	if _, err := r.GetByKey(ctx, "azure_devops", "drift", "default"); err == nil {
		t.Error("GetByKey swallowed the query error")
	}
}

func TestWorkflowTemplateRepository_List(t *testing.T) {
	db, mock := newMock(t)
	r := NewWorkflowTemplateRepository(db)

	mock.ExpectQuery("SELECT .+ FROM workflow_templates ORDER BY provider, kind, profile").WillReturnRows(wtRow())
	out, err := r.List(ctx)
	if err != nil || len(out) != 1 || out[0].ID != "t1" {
		t.Fatalf("List: %v %+v", err, out)
	}
}

func TestWorkflowTemplateRepository_Create(t *testing.T) {
	db, mock := newMock(t)
	r := NewWorkflowTemplateRepository(db)

	mock.ExpectQuery("INSERT INTO workflow_templates").
		WithArgs("azure_devops", "drift", "brunswick-azure", "Brunswick Azure", "desc", "yaml", false).
		WillReturnRows(wtRow())
	got, err := r.Create(ctx, &WorkflowTemplate{
		Provider: "azure_devops", Kind: "drift", Profile: "brunswick-azure",
		Name: "Brunswick Azure", Description: "desc", Content: "yaml",
	})
	if err != nil || got == nil {
		t.Fatalf("Create: %v %+v", err, got)
	}
}

func TestWorkflowTemplateRepository_Update(t *testing.T) {
	db, mock := newMock(t)
	r := NewWorkflowTemplateRepository(db)

	mock.ExpectQuery("UPDATE workflow_templates SET").
		WithArgs("t1", "New Name", "new desc", "new yaml").WillReturnRows(wtRow())
	got, err := r.Update(ctx, &WorkflowTemplate{ID: "t1", Name: "New Name", Description: "new desc", Content: "new yaml"})
	if err != nil || got == nil {
		t.Fatalf("Update: %v %+v", err, got)
	}

	mock.ExpectQuery("UPDATE workflow_templates SET").WithArgs("missing", "", "", "").WillReturnError(sql.ErrNoRows)
	got, err = r.Update(ctx, &WorkflowTemplate{ID: "missing"})
	if err != nil || got != nil {
		t.Fatalf("Update missing should be (nil,nil): %v %+v", err, got)
	}
}

func TestWorkflowTemplateRepository_Delete(t *testing.T) {
	db, mock := newMock(t)
	r := NewWorkflowTemplateRepository(db)

	mock.ExpectExec("DELETE FROM workflow_templates WHERE id").WithArgs("t1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.Delete(ctx, "t1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestWorkflowTemplateRepository_EnsureBuiltin(t *testing.T) {
	db, mock := newMock(t)
	r := NewWorkflowTemplateRepository(db)

	mock.ExpectExec("INSERT INTO workflow_templates .+ ON CONFLICT").
		WithArgs("github_actions", "drift", "default", "GitHub Drift", "", "wf").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.EnsureBuiltin(ctx, &WorkflowTemplate{
		Provider: "github_actions", Kind: "drift", Profile: "default", Name: "GitHub Drift", Content: "wf",
	}); err != nil {
		t.Fatalf("EnsureBuiltin: %v", err)
	}
}
