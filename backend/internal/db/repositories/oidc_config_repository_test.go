package repositories

import (
	"database/sql"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func TestOIDCConfigRepository_Create(t *testing.T) {
	db, mock := newMock(t)
	r := NewOIDCConfigRepository(db)

	// Create must deactivate any existing active config and insert the new one,
	// atomically (a tx), so there is never two active rows.
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE oidc_configs SET is_active = false").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO oidc_configs").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	err := r.Create(ctx, OIDCConfig{
		IssuerURL: "https://idp.example.com", ClientID: "cid",
		ClientSecretEncrypted: []byte{1, 2, 3}, RedirectURL: "https://app/cb", Scopes: []string{"openid"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
}

func TestOIDCConfigRepository_GetActive(t *testing.T) {
	db, mock := newMock(t)
	r := NewOIDCConfigRepository(db)

	cols := []string{"issuer_url", "client_id", "client_secret_encrypted", "redirect_url", "scopes"}
	mock.ExpectQuery("FROM oidc_configs WHERE is_active = true").
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("https://idp.example.com", "cid", []byte{9, 9}, "https://app/cb", []byte(`["openid","email"]`)))

	got, err := r.GetActiveOIDCConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("GetActiveOIDCConfig = %+v, %v", got, err)
	}
	if got.IssuerURL != "https://idp.example.com" || len(got.Scopes) != 2 {
		t.Fatalf("unexpected active config: %+v", got)
	}
}

func TestOIDCConfigRepository_GetActive_None(t *testing.T) {
	db, mock := newMock(t)
	r := NewOIDCConfigRepository(db)
	mock.ExpectQuery("FROM oidc_configs WHERE is_active = true").WillReturnError(sql.ErrNoRows)
	got, err := r.GetActiveOIDCConfig(ctx)
	if err != nil || got != nil {
		t.Fatalf("no active config should be (nil, nil), got %+v, %v", got, err)
	}
}
