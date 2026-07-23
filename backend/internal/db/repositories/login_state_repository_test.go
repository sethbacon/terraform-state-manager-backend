package repositories

import (
	"encoding/json"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
)

func TestLoginStateRepository_SaveLoadDelete(t *testing.T) {
	db, mock := newMock(t)
	r := NewLoginStateRepository(db)

	ss := &auth.SessionState{State: "st-1", ProviderType: "oidc", Nonce: "n-1", CodeVerifier: "v-1"}
	blob, _ := json.Marshal(ss)

	// Save reaps expired rows first (the unauthenticated /auth/login endpoint
	// otherwise grows the table without bound), then upserts with the TTL.
	mock.ExpectExec("DELETE FROM login_states WHERE expires_at").
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectExec("INSERT INTO login_states").
		WithArgs("st-1", blob, float64(600)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.Save(ctx, "st-1", ss, 10*time.Minute); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Load consumes the row (DELETE ... RETURNING) and decodes the state.
	mock.ExpectQuery("DELETE FROM login_states WHERE key").WithArgs("st-1").
		WillReturnRows(sqlmock.NewRows([]string{"state", "expired"}).AddRow(blob, false))
	got, err := r.Load(ctx, "st-1")
	if err != nil || got == nil || got.Nonce != "n-1" || got.CodeVerifier != "v-1" {
		t.Fatalf("Load: %v %+v", err, got)
	}

	// A consumed/missing key is (nil, nil) — the callback treats it as an
	// invalid state, which is also what makes redemption single-use.
	mock.ExpectQuery("DELETE FROM login_states WHERE key").WithArgs("st-1").
		WillReturnRows(sqlmock.NewRows([]string{"state", "expired"}))
	if got, err := r.Load(ctx, "st-1"); err != nil || got != nil {
		t.Fatalf("consumed key must be (nil, nil), got %+v %v", got, err)
	}

	// An expired row is consumed but not honored.
	mock.ExpectQuery("DELETE FROM login_states WHERE key").WithArgs("st-old").
		WillReturnRows(sqlmock.NewRows([]string{"state", "expired"}).AddRow(blob, true))
	if got, err := r.Load(ctx, "st-old"); err != nil || got != nil {
		t.Fatalf("expired state must be (nil, nil), got %+v %v", got, err)
	}

	mock.ExpectExec("DELETE FROM login_states WHERE key").WithArgs("st-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.Delete(ctx, "st-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestLoginStateRepository_SaveSurvivesReapFailure(t *testing.T) {
	db, mock := newMock(t)
	r := NewLoginStateRepository(db)

	// The opportunistic reap failing must not block a login.
	mock.ExpectExec("DELETE FROM login_states WHERE expires_at").WillReturnError(errDB)
	mock.ExpectExec("INSERT INTO login_states").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.Save(ctx, "st-2", &auth.SessionState{State: "st-2"}, time.Minute); err != nil {
		t.Fatalf("Save must tolerate a failed reap: %v", err)
	}
}
