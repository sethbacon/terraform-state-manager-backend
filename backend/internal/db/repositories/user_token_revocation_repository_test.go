package repositories

import (
	"context"
	"database/sql"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func TestUserTokenRevocationRepository_RevokeAndQuery(t *testing.T) {
	db, mock := newMock(t)
	r := NewUserTokenRevocationRepository(db)

	// Upsert: a user who already has a watermark gets it moved forward rather
	// than a duplicate-key error, so repeated reductions keep working.
	mock.ExpectExec("INSERT INTO user_token_revocations").WithArgs("u1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.RevokeAllUserTokens(ctx, "u1"); err != nil {
		t.Fatalf("RevokeAllUserTokens: %v", err)
	}

	issued := time.Now().Add(-time.Hour)
	mock.ExpectQuery("FROM user_token_revocations").WithArgs("u1", issued).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	revoked, err := r.TokensRevokedSince(ctx, "u1", issued)
	if err != nil || !revoked {
		t.Fatalf("TokensRevokedSince = %v, %v; want true, nil", revoked, err)
	}

	// A user with no watermark, or one that predates the token, is not revoked.
	mock.ExpectQuery("FROM user_token_revocations").WithArgs("u2", issued).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	if revoked, err := r.TokensRevokedSince(ctx, "u2", issued); err != nil || revoked {
		t.Fatalf("TokensRevokedSince (no watermark) = %v, %v; want false, nil", revoked, err)
	}

	// Watermarks older than the longest token lifetime can no longer match any
	// structurally valid token, so they are reaped.
	mock.ExpectExec("DELETE FROM user_token_revocations").WillReturnResult(sqlmock.NewResult(0, 4))
	if err := r.CleanupExpiredWatermarks(ctx, 24*time.Hour); err != nil {
		t.Fatalf("CleanupExpiredWatermarks: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestUserTokenRevocationRepository_Errors(t *testing.T) {
	db, mock := newMock(t)
	r := NewUserTokenRevocationRepository(db)

	mock.ExpectExec("INSERT INTO user_token_revocations").WillReturnError(sql.ErrConnDone)
	if err := r.RevokeAllUserTokens(ctx, "u1"); err == nil {
		t.Error("RevokeAllUserTokens must surface the write error; the caller reports the sweep incomplete")
	}

	mock.ExpectQuery("FROM user_token_revocations").WillReturnError(sql.ErrConnDone)
	if _, err := r.TokensRevokedSince(ctx, "u1", time.Now()); err == nil {
		t.Error("TokensRevokedSince must surface the read error so the middleware can fail closed")
	}

	mock.ExpectExec("DELETE FROM user_token_revocations").WillReturnError(sql.ErrConnDone)
	if err := r.CleanupExpiredWatermarks(ctx, time.Hour); err == nil {
		t.Error("CleanupExpiredWatermarks must surface the delete error")
	}
}

// GUARD bulk-revocation-issues-one-statement. The caller is the boot-time
// role-template reduction, where the holder list can be every member in the
// deployment; a per-user loop there scales the startup path with the membership.
//
// MUTATION: loop over RevokeAllUserTokens instead.
func TestRevokeAllUserTokensFor_IssuesOneStatement(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectExec("INSERT INTO user_token_revocations").
		WillReturnResult(sqlmock.NewResult(0, 3))

	repo := NewUserTokenRevocationRepository(db)
	n, err := repo.RevokeAllUserTokensFor(context.Background(), []string{"u1", "u2", "u3"})
	if err != nil {
		t.Fatalf("RevokeAllUserTokensFor: %v", err)
	}
	if n != 3 {
		t.Errorf("n = %d, want the number of rows the statement reported", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("more than one statement was issued: %v", err)
	}
}

// GUARD bulk-revocation-of-nobody-issues-nothing. A narrowed role nobody holds
// is the ordinary case, and it must not reach the database at all.
//
// MUTATION: drop the length check.
func TestRevokeAllUserTokensFor_EmptyIssuesNothing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	repo := NewUserTokenRevocationRepository(db)
	n, err := repo.RevokeAllUserTokensFor(context.Background(), nil)
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v, want 0 and no error", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a statement was issued for an empty holder list: %v", err)
	}
}
