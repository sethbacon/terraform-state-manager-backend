//go:build integration

package repositories_test

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"

	appdb "github.com/terraform-state-manager/terraform-state-manager/internal/db"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// AGAINST A REAL POSTGRESQL, because the thing under test is a rule of the
// engine rather than a statement anybody issues.
//
// RevokeAllUserTokensFor writes one watermark per principal in a single
// INSERT ... SELECT ... ON CONFLICT. Postgres refuses to let ON CONFLICT DO
// UPDATE touch the same key twice inside one statement — "command cannot affect
// row a second time" — so a duplicated id does not merely write twice, it fails
// the whole write. The caller that produces those duplicates is the boot-time
// role-template reduction (#557): a principal holding a narrowed role in two
// organizations arrives once per assignment. sqlmock cannot have an opinion
// about any of that; it returns whatever the fixture declares.

const revocationBulkTestDB = "tsm_revocation_bulk_test"

func newRevocationDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping PostgreSQL integration test")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = admin.Close() }()
	if err := admin.Ping(); err != nil {
		t.Skipf("TEST_DATABASE_URL is not reachable (%v); skipping", err)
	}
	for _, stmt := range []string{
		`DROP DATABASE IF EXISTS ` + pgx.Identifier{revocationBulkTestDB}.Sanitize() + ` WITH (FORCE)`,
		`CREATE DATABASE ` + pgx.Identifier{revocationBulkTestDB}.Sanitize(),
	} {
		if _, err := admin.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	parsed.Path = "/" + revocationBulkTestDB
	db, err := sql.Open("pgx", parsed.String())
	if err != nil {
		t.Fatalf("open %s: %v", parsed.Redacted(), err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := appdb.RunMigrations(db, "up"); err != nil {
		t.Fatalf("app migrations: %v", err)
	}
	return db
}

const (
	revUserA = "11111111-0000-4000-8000-00000000000a"
	revUserB = "22222222-0000-4000-8000-00000000000b"
)

func watermarkCount(t *testing.T, db *sql.DB, userID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM user_token_revocations WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("counting watermarks: %v", err)
	}
	return n
}

// GUARD bulk-revocation-tolerates-a-duplicate-principal. The case the caller
// really produces, and the one that fails the entire statement without DISTINCT.
//
// MUTATION: drop DISTINCT from the SELECT in RevokeAllUserTokensFor.
func TestIntegration_BulkRevocation_ToleratesADuplicatePrincipal(t *testing.T) {
	db := newRevocationDB(t)
	repo := repositories.NewUserTokenRevocationRepository(db)
	ctx := context.Background()

	n, err := repo.RevokeAllUserTokensFor(ctx, []string{revUserA, revUserA, revUserB})
	if err != nil {
		t.Fatalf("RevokeAllUserTokensFor with a repeated id: %v", err)
	}
	if n != 2 {
		t.Errorf("wrote %d watermarks, want 2 — the count is the number of principals whose sessions ended, "+
			"not the number of ids handed in", n)
	}
	for _, u := range []string{revUserA, revUserB} {
		if got := watermarkCount(t, db, u); got != 1 {
			t.Errorf("user %s has %d watermark rows, want exactly 1", u, got)
		}
	}
}

// GUARD bulk-revocation-moves-an-existing-watermark. A principal who already had
// one gets it moved forward, not left where it was: a session minted after the
// previous reduction and before this one must still be ended.
//
// MUTATION: change ON CONFLICT DO UPDATE to DO NOTHING.
func TestIntegration_BulkRevocation_MovesAnExistingWatermarkForward(t *testing.T) {
	db := newRevocationDB(t)
	repo := repositories.NewUserTokenRevocationRepository(db)
	ctx := context.Background()

	if err := repo.RevokeAllUserTokens(ctx, revUserA); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}
	var first string
	if err := db.QueryRow(`SELECT revoked_before::text FROM user_token_revocations WHERE user_id = $1`, revUserA).Scan(&first); err != nil {
		t.Fatalf("read seeded watermark: %v", err)
	}

	if _, err := repo.RevokeAllUserTokensFor(ctx, []string{revUserA}); err != nil {
		t.Fatalf("RevokeAllUserTokensFor: %v", err)
	}
	var second string
	if err := db.QueryRow(`SELECT revoked_before::text FROM user_token_revocations WHERE user_id = $1`, revUserA).Scan(&second); err != nil {
		t.Fatalf("read moved watermark: %v", err)
	}
	if second == first {
		t.Errorf("watermark stayed at %s: a session minted since the previous reduction would survive this one", first)
	}
	if got := watermarkCount(t, db, revUserA); got != 1 {
		t.Errorf("user has %d watermark rows, want 1 — the write must move the row, not add one", got)
	}
}

// GUARD bulk-revocation-writes-nothing-for-nobody. An empty holder list is the
// ordinary case for a narrowed role nobody holds, and it must issue no statement
// at all rather than one with an empty array.
//
// MUTATION: remove the length check and let the statement run with an empty
// array.
func TestIntegration_BulkRevocation_EmptyListWritesNothing(t *testing.T) {
	db := newRevocationDB(t)
	repo := repositories.NewUserTokenRevocationRepository(db)

	n, err := repo.RevokeAllUserTokensFor(context.Background(), nil)
	if err != nil || n != 0 {
		t.Fatalf("empty call: n=%d err=%v, want 0 and no error", n, err)
	}
	var total int
	if err := db.QueryRow(`SELECT count(*) FROM user_token_revocations`).Scan(&total); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if total != 0 {
		t.Errorf("%d watermark rows exist after a call with no principals", total)
	}
}
