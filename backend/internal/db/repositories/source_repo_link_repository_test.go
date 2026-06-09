package repositories

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

func newSourceRepoLinkRepo(t *testing.T) (*SourceRepoLinkRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return NewSourceRepoLinkRepository(db), mock
}

func intptr(i int) *int { return &i }

func TestSourceRepoLinkRepo_Upsert_Success(t *testing.T) {
	repo, mock := newSourceRepoLinkRepo(t)

	id := "33333333-3333-3333-3333-333333333333"
	now := time.Now()
	mock.ExpectQuery(`INSERT INTO source_repo_links`).
		WithArgs(
			"org-1", "src-1", "https://dev.azure.com/acme", "infra", "tf-network",
			intptr(7), models.RepoLinkDiscoveryManual,
		).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "updated_at"}).AddRow(id, now, now))

	link := &models.SourceRepoLink{
		OrganizationID:     "org-1",
		SourceID:           "src-1",
		ADOOrganizationURL: "https://dev.azure.com/acme",
		ADOProject:         "infra",
		ADORepo:            "tf-network",
		ADOPipelineID:      intptr(7),
		DiscoveryMethod:    models.RepoLinkDiscoveryManual,
	}
	err := repo.Upsert(context.Background(), link)
	require.NoError(t, err)
	assert.Equal(t, id, link.ID)
	assert.False(t, link.CreatedAt.IsZero())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSourceRepoLinkRepo_Upsert_NilPipeline(t *testing.T) {
	repo, mock := newSourceRepoLinkRepo(t)

	now := time.Now()
	mock.ExpectQuery(`INSERT INTO source_repo_links`).
		WithArgs(
			"org-1", "src-1", "https://dev.azure.com/acme", "infra", "tf-network",
			nil, models.RepoLinkDiscoveryManual,
		).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "updated_at"}).AddRow("id-1", now, now))

	link := &models.SourceRepoLink{
		OrganizationID:     "org-1",
		SourceID:           "src-1",
		ADOOrganizationURL: "https://dev.azure.com/acme",
		ADOProject:         "infra",
		ADORepo:            "tf-network",
		ADOPipelineID:      nil,
		DiscoveryMethod:    models.RepoLinkDiscoveryManual,
	}
	require.NoError(t, repo.Upsert(context.Background(), link))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSourceRepoLinkRepo_Upsert_DBError(t *testing.T) {
	repo, mock := newSourceRepoLinkRepo(t)

	mock.ExpectQuery(`INSERT INTO source_repo_links`).
		WillReturnError(fmt.Errorf("db error"))

	err := repo.Upsert(context.Background(), &models.SourceRepoLink{})
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSourceRepoLinkRepo_GetBySourceID_Found(t *testing.T) {
	repo, mock := newSourceRepoLinkRepo(t)

	now := time.Now()
	cols := []string{
		"id", "organization_id", "source_id", "ado_organization_url", "ado_project",
		"ado_repo", "ado_pipeline_id", "discovery_method", "created_at", "updated_at",
	}
	mock.ExpectQuery(`SELECT id, organization_id, source_id, ado_organization_url, ado_project`).
		WithArgs("src-1").
		WillReturnRows(sqlmock.NewRows(cols).AddRow(
			"link-1", "org-1", "src-1", "https://dev.azure.com/acme", "infra",
			"tf-network", 7, models.RepoLinkDiscoveryManual, now, now,
		))

	got, err := repo.GetBySourceID(context.Background(), "src-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "tf-network", got.ADORepo)
	require.NotNil(t, got.ADOPipelineID)
	assert.Equal(t, 7, *got.ADOPipelineID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSourceRepoLinkRepo_GetBySourceID_NotFound(t *testing.T) {
	repo, mock := newSourceRepoLinkRepo(t)

	mock.ExpectQuery(`SELECT id, organization_id, source_id, ado_organization_url, ado_project`).
		WithArgs("src-1").
		WillReturnError(sql.ErrNoRows)

	got, err := repo.GetBySourceID(context.Background(), "src-1")
	require.NoError(t, err)
	assert.Nil(t, got)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSourceRepoLinkRepo_DeleteBySourceID_Success(t *testing.T) {
	repo, mock := newSourceRepoLinkRepo(t)

	mock.ExpectExec(`DELETE FROM source_repo_links WHERE source_id = \$1`).
		WithArgs("src-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	require.NoError(t, repo.DeleteBySourceID(context.Background(), "src-1"))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSourceRepoLinkRepo_DeleteBySourceID_DBError(t *testing.T) {
	repo, mock := newSourceRepoLinkRepo(t)

	mock.ExpectExec(`DELETE FROM source_repo_links WHERE source_id = \$1`).
		WithArgs("src-1").
		WillReturnError(fmt.Errorf("db error"))

	require.Error(t, repo.DeleteBySourceID(context.Background(), "src-1"))
	require.NoError(t, mock.ExpectationsWereMet())
}
