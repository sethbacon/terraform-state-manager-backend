package repositories

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

func newUIThemeRepo(t *testing.T) (*UIThemeRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return NewUIThemeRepository(sqlx.NewDb(db, "postgres")), mock
}

func strptr(s string) *string { return &s }

func TestUIThemeRepo_Get_Unset_ReturnsNil(t *testing.T) {
	repo, mock := newUIThemeRepo(t)
	mock.ExpectQuery(`SELECT value FROM system_settings`).
		WithArgs(uiThemeSettingKey).
		WillReturnRows(sqlmock.NewRows([]string{"value"})) // no rows -> sql.ErrNoRows

	got, err := repo.Get(context.Background())
	require.NoError(t, err)
	assert.Nil(t, got)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUIThemeRepo_Get_Found(t *testing.T) {
	repo, mock := newUIThemeRepo(t)
	stored := models.UIThemeConfig{ProductName: strptr("Acme"), PrimaryColor: strptr("#5C4EE5")}
	payload, err := json.Marshal(&stored)
	require.NoError(t, err)

	mock.ExpectQuery(`SELECT value FROM system_settings`).
		WithArgs(uiThemeSettingKey).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(string(payload)))

	got, err := repo.Get(context.Background())
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.ProductName)
	assert.Equal(t, "Acme", *got.ProductName)
	require.NotNil(t, got.PrimaryColor)
	assert.Equal(t, "#5C4EE5", *got.PrimaryColor)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUIThemeRepo_Get_DBError(t *testing.T) {
	repo, mock := newUIThemeRepo(t)
	mock.ExpectQuery(`SELECT value FROM system_settings`).
		WithArgs(uiThemeSettingKey).
		WillReturnError(fmt.Errorf("db error"))

	_, err := repo.Get(context.Background())
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUIThemeRepo_Get_BadJSON(t *testing.T) {
	repo, mock := newUIThemeRepo(t)
	mock.ExpectQuery(`SELECT value FROM system_settings`).
		WithArgs(uiThemeSettingKey).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("{not json"))

	_, err := repo.Get(context.Background())
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUIThemeRepo_Upsert_Success(t *testing.T) {
	repo, mock := newUIThemeRepo(t)
	mock.ExpectExec(`INSERT INTO system_settings`).
		WithArgs(uiThemeSettingKey, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	in := &models.UIThemeConfig{ProductName: strptr("Acme")}
	got, err := repo.Upsert(context.Background(), in)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.ProductName)
	assert.Equal(t, "Acme", *got.ProductName)
	assert.False(t, got.UpdatedAt.IsZero(), "UpdatedAt should be set by Upsert")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUIThemeRepo_Upsert_DBError(t *testing.T) {
	repo, mock := newUIThemeRepo(t)
	mock.ExpectExec(`INSERT INTO system_settings`).
		WithArgs(uiThemeSettingKey, sqlmock.AnyArg()).
		WillReturnError(fmt.Errorf("db error"))

	_, err := repo.Upsert(context.Background(), &models.UIThemeConfig{})
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
