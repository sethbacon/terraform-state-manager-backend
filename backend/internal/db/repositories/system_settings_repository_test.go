package repositories

import (
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func TestSystemSettingsRepository_TokenLifecycle(t *testing.T) {
	db, mock := newMock(t)
	r := NewSystemSettingsRepository(db)

	mock.ExpectQuery("SELECT setup_token_hash FROM system_settings").
		WillReturnRows(sqlmock.NewRows([]string{"setup_token_hash"}).AddRow("bcrypt-hash"))
	got, err := r.GetSetupTokenHash(ctx)
	if err != nil || got != "bcrypt-hash" {
		t.Fatalf("GetSetupTokenHash = %q, %v", got, err)
	}

	// SetSetupCompleted must mark complete AND null the token (the self-disable).
	mock.ExpectExec("UPDATE system_settings SET setup_completed = true, setup_token_hash = NULL").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.SetSetupCompleted(ctx); err != nil {
		t.Fatalf("SetSetupCompleted: %v", err)
	}
}

func TestSystemSettingsRepository_GetStatus(t *testing.T) {
	db, mock := newMock(t)
	r := NewSystemSettingsRepository(db)

	cols := []string{"setup_completed", "admin_configured", "oidc_configured", "ldap_configured", "sources_configured", "auth_method"}
	mock.ExpectQuery("FROM system_settings WHERE id = 1").
		WillReturnRows(sqlmock.NewRows(cols).AddRow(false, true, false, false, true, "oidc"))

	st, err := r.GetStatus(ctx)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if st.SetupCompleted || !st.AdminConfigured || !st.SourcesConfigured || st.AuthMethod != "oidc" {
		t.Fatalf("unexpected status: %+v", st)
	}
}

// TestSystemSettingsRepository_TokenHashNullEmpty: a NULL hash reads as "".
func TestSystemSettingsRepository_TokenHashNullEmpty(t *testing.T) {
	db, mock := newMock(t)
	r := NewSystemSettingsRepository(db)
	mock.ExpectQuery("SELECT setup_token_hash FROM system_settings").
		WillReturnRows(sqlmock.NewRows([]string{"setup_token_hash"}).AddRow(nil))
	got, err := r.GetSetupTokenHash(ctx)
	if err != nil || got != "" {
		t.Fatalf("null hash should read empty: %q, %v", got, err)
	}
}
