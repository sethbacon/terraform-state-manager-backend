package repositories

import (
	"context"
	"errors"
	"testing"
)

// An empty organization must be REFUSED rather than passed through to the
// statement.
//
// The two outcomes look the same at the call site and are very different in the
// table: `organization_id` bound to an empty string fails the uuid cast at best,
// and where the column is nullable it writes NULL -- a row no tenant predicate
// can see, because `NULL = ANY($1::uuid[])` is NULL and never true. A row nobody
// can be shown is not a safe default; it is a leak in the other direction, since
// a platform admin still sees it and no owner ever will.
//
// Refusing needs no database, which is why these tests register no expectations:
// reaching one at all is the failure.

func TestHealthRepository_CreateRefusesAnEmptyOrganization(t *testing.T) {
	db, mock := newMock(t)
	r := NewHealthRepository(db)
	conn := "p1"

	for _, blank := range []string{"", "   "} {
		_, err := r.Create(context.Background(), &HealthRun{
			PipelineConnectionID: &conn, Status: "dispatched", CallbackToken: "tok",
		}, blank)
		if !errors.Is(err, ErrNoOrganization) {
			t.Fatalf("Create(organizationID=%q) error = %v, want ErrNoOrganization", blank, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a statement ran: the refusal happened after the INSERT, not before it: %v", err)
	}
}

func TestTransferRepository_CreateRefusesAnEmptyOrganization(t *testing.T) {
	db, mock := newMock(t)
	r := NewTransferRepository(db)

	for _, blank := range []string{"", "   "} {
		_, err := r.Create(context.Background(), &Transfer{
			Mode: "backup", SourceID: "s1", SourceKey: "k", TargetSourceID: "s2",
			TargetKey: "k2", Status: "success",
		}, blank)
		if !errors.Is(err, ErrNoOrganization) {
			t.Fatalf("Create(organizationID=%q) error = %v, want ErrNoOrganization", blank, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a statement ran: the refusal happened after the INSERT, not before it: %v", err)
	}
}
