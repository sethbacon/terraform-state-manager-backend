package tenancy

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// These are the assertions that CANNOT be made against a real database, because
// a real database accidentally satisfies them.
//
// The integration suite originally proved the empty-orgID refusal by calling
// Backfill(ctx, db, "") and checking for a non-nil error. Deleting the refusal
// entirely left that test PASSING: without the guard the sweep runs, Postgres
// rejects ''::uuid, and an error comes back anyway — a different error, from a
// later statement, after the carrier UPDATE has already been attempted. The
// guard was inert and looked exactly like a working one.
//
// sqlmock closes that hole in the only way that is not circular: a mock with NO
// expectations fails on the FIRST query anybody sends it. So "no queries were
// sent" is directly observable, which is the actual claim — the refusal
// short-circuits before touching the database — rather than "something,
// somewhere, eventually errored".

func TestBackfill_RefusesAnEmptyOrganizationIDBeforeTouchingTheDatabase(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	// No expectations are registered on purpose: sqlmock fails any query it was
	// not told to expect, so reaching the database at all is a test failure.
	err = Backfill(context.Background(), db, "")
	if err == nil {
		t.Fatal("Backfill accepted an empty organization id")
	}
	if !strings.Contains(err.Error(), "empty default organization id") {
		t.Errorf("error = %v; want the refusal. An error raised LATER — by Postgres rejecting "+
			"''::uuid, say — means the guard is gone and the sweep ran anyway.", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestBackfill_RefusesANilConnection(t *testing.T) {
	if err := Backfill(context.Background(), nil, "org-1"); err == nil {
		t.Fatal("Backfill accepted a nil application connection")
	}
}

// TestBackfill_WritesTheCarrierBeforeSweeping pins the ORDER, which is a
// correctness property and not a stylistic one.
//
// The column defaults read the carrier. Sweeping first and writing the carrier
// afterwards would leave a window in which concurrent INSERTs are still stamped
// NULL and the sweep that would have caught them has already passed — rows that
// no part of this boot revisits. The two statements are independent enough that
// nothing else would notice them being swapped.
//
// MatchExpectationsInOrder is sqlmock's default and is what makes this an
// ordering assertion rather than a set assertion; it is restated explicitly so a
// future edit cannot relax it by accident.
func TestBackfill_WritesTheCarrierBeforeSweeping(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(true)

	const orgID = "11111111-2222-3333-4444-555555555555"
	mock.ExpectExec(regexp.QuoteMeta("UPDATE system_settings SET default_organization_id")).
		WithArgs(orgID).WillReturnResult(sqlmock.NewResult(0, 1))
	for _, table := range PartitionedTables {
		mock.ExpectExec(regexp.QuoteMeta("UPDATE " + table + " SET organization_id")).
			WithArgs(orgID).WillReturnResult(sqlmock.NewResult(0, 0))
	}

	if err := Backfill(context.Background(), db, orgID); err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v.\nThe carrier UPDATE must come FIRST and every table in "+
			"PartitionedTables must be swept exactly once.", err)
	}
}

// TestBackfill_StopsAtTheFirstFailedTable keeps a partial sweep from being
// reported as a completed one. There is no transaction here — nine UPDATEs over
// potentially large tables in one transaction is a lock-duration problem, and
// the operation is idempotent so a partial run is safely resumed by the next
// boot. What must not happen is a partial run returning nil, because then
// nothing ever resumes it.
func TestBackfill_StopsAtTheFirstFailedTable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	const orgID = "11111111-2222-3333-4444-555555555555"
	mock.ExpectExec(regexp.QuoteMeta("UPDATE system_settings")).
		WithArgs(orgID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE " + PartitionedTables[0])).
		WithArgs(orgID).WillReturnError(errors.New("boom"))

	err = Backfill(context.Background(), db, orgID)
	if err == nil {
		t.Fatal("Backfill reported success despite a failed table sweep")
	}
	// Named, so an operator reading the log knows which table to look at.
	if !strings.Contains(err.Error(), PartitionedTables[0]) {
		t.Errorf("error = %v; want it to name %s", err, PartitionedTables[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations (the sweep continued past the failure?): %v", err)
	}
}
