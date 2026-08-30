package repositories

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// ---------------------------------------------------------------------------
// The final two partition roots — notification_channels and state_transfers
// (#393 Phase 3).
//
// THESE COVER THE SHAPE OF THE STATEMENT AND NOTHING MORE, and that limit is
// worth restating here because this increment is the one where it would be most
// tempting to stop. sqlmock matches SQL with a regex and returns the rows the
// test handed it; it NEVER evaluates a WHERE clause. So it can say "the
// organization array was bound, as this parameter, on this statement" and it
// CANNOT say "another organization's channel was not served". Those are
// different claims and the second is answered against a real PostgreSQL in
// internal/tenancy/final_roots_integration_test.go — including, for
// notification_channels, that the SECRET TARGET does not cross.
//
// Every expectation below therefore BINDS AND ASSERTS the organization
// argument. An expectation that waved it through with AnyArg would keep passing
// against a reader scoped to the wrong organization, or to none — which is the
// exact defect this estate has now shipped three times, most recently as an
// UPDATE expectation that bound no arguments at all and so could not see a
// platform-admin scope swap.
//
// MUTATION-VERIFIED: each case was run against a deliberately broken repository
// and observed to fail by name. The table is in the commit message.
// ---------------------------------------------------------------------------

var notifCols = []string{"id", "name", "type", "encrypted_target", "events", "enabled",
	"last_status", "last_error", "last_sent_at", "created_at", "updated_at"}

// notifRow is a fixture with EVERY projected column populated. A fixture that
// left encrypted_target empty could not tell a reader that dropped the secret
// from one that served it, which is the whole subject on this root.
func notifRow(id, target string) *sqlmock.Rows {
	now := time.Now()
	return sqlmock.NewRows(notifCols).
		AddRow(id, "ops", "webhook", target, []byte(`["drift_detected"]`), true,
			"sent", nil, now, now, now)
}

// ---------------------------------------------------------------------------
// notification_channels
// ---------------------------------------------------------------------------

// THE FAIL-CLOSED CASE, asserted as "no statement reached the database" rather
// than as "no rows came back". A reader that issued the query and happened to
// match nothing satisfies the second while remaining one predicate edit away
// from returning the whole table.
func TestNotificationChannels_ScopedAccessOnAnEmptyScopeTouchesNothing(t *testing.T) {
	db, mock := newMock(t)
	r := NewNotificationChannelRepository(db)

	if out, err := r.ListInScope(ctx, tenantscope.Scope{}); err != nil || len(out) != 0 {
		t.Errorf("ListInScope on an empty scope = %v, %v; want no channels", out, err)
	}
	if got, err := r.GetByIDInScope(ctx, "n1", tenantscope.Scope{}); err != nil || got != nil {
		t.Errorf("GetByIDInScope on an empty scope = %+v, %v; want nothing", got, err)
	}
	if _, err := r.UpdateInScope(ctx, "n1", "ops", "webhook", nil, true, "", tenantscope.Scope{}); !errors.Is(err, ErrNotInScope) {
		t.Errorf("UpdateInScope on an empty scope = %v; want ErrNotInScope", err)
	}
	if err := r.DeleteInScope(ctx, "n1", tenantscope.Scope{}); !errors.Is(err, ErrNotInScope) {
		t.Errorf("DeleteInScope on an empty scope = %v; want ErrNotInScope", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestNotificationChannels_ScopedAccess_BindsTheOrganization(t *testing.T) {
	scope := tenantscope.Scope{OrgIDs: []string{scopeOrgA}}
	db, mock := newMock(t)
	r := NewNotificationChannelRepository(db)

	mock.ExpectQuery("FROM notification_channels WHERE organization_id = ANY.+ORDER BY").
		WithArgs([]string{scopeOrgA}).
		WillReturnRows(notifRow("n1", "sealed-target"))
	out, err := r.ListInScope(ctx, scope)
	if err != nil || len(out) != 1 {
		t.Fatalf("ListInScope: %v %+v", err, out)
	}
	if out[0].EncryptedTarget != "" {
		t.Error("ListInScope returned an encrypted target; the scoped list must strip it exactly as List does")
	}

	// The by-id read is the one that returns the secret, so the parameter order
	// is asserted too: id is $1 and the organization array is $2, and a by-id
	// read filtered on the wrong parameter is somebody else's webhook.
	mock.ExpectQuery("FROM notification_channels WHERE id .+ organization_id = ANY").
		WithArgs("n1", []string{scopeOrgA}).
		WillReturnRows(notifRow("n1", "sealed-target"))
	got, err := r.GetByIDInScope(ctx, "n1", scope)
	if err != nil || got == nil {
		t.Fatalf("GetByIDInScope: %v %+v", err, got)
	}
	if got.EncryptedTarget != "sealed-target" {
		t.Errorf("GetByIDInScope dropped the encrypted target (%q); the notifier and the "+
			"test-send need it, and a fixture that could not see it could not see a leak either",
			got.EncryptedTarget)
	}

	// A channel in another organization reads EXACTLY as one that does not exist,
	// and the shared package's ErrNotFound is translated to this repository's
	// (nil, nil) by-id convention on the way out.
	mock.ExpectQuery("FROM notification_channels WHERE id .+ organization_id = ANY").
		WithArgs("elsewhere", []string{scopeOrgA}).
		WillReturnError(sql.ErrNoRows)
	if got, err := r.GetByIDInScope(ctx, "elsewhere", scope); err != nil || got != nil {
		t.Errorf("a channel outside the scope must be (nil, nil), got %+v %v", got, err)
	}

	// The write side binds the organization LAST, after the six value arguments.
	mock.ExpectQuery(`UPDATE notification_channels[\s\S]*organization_id = ANY`).
		WithArgs("n1", "ops", "webhook", sqlmock.AnyArg(), true, "new-sealed", []string{scopeOrgA}).
		WillReturnRows(notifRow("n1", "new-sealed"))
	if _, err := r.UpdateInScope(ctx, "n1", "ops", "webhook", []string{"drift_detected"}, true, "new-sealed", scope); err != nil {
		t.Fatalf("UpdateInScope: %v", err)
	}

	mock.ExpectExec(`DELETE FROM notification_channels[\s\S]*organization_id = ANY`).
		WithArgs("n1", []string{scopeOrgA}).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.DeleteInScope(ctx, "n1", scope); err != nil {
		t.Fatalf("DeleteInScope: %v", err)
	}

	// A delete outside the scope matches nothing and reports ErrNotFound, which is
	// both the non-disclosing answer and the true one.
	mock.ExpectExec(`DELETE FROM notification_channels[\s\S]*organization_id = ANY`).
		WithArgs("elsewhere", []string{scopeOrgA}).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := r.DeleteInScope(ctx, "elsewhere", scope); !errors.Is(err, idstore.ErrNotFound) {
		t.Errorf("DeleteInScope outside the scope = %v; want store.ErrNotFound", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A PLATFORM ADMIN TAKES THE UNFILTERED BRANCH, and the expectations say so by
// binding NO organization array. This is the branch that must still reach a row
// whose organization_id is NULL — written by a replica on the previous build,
// before the backfill — which the tenant predicate cannot match.
func TestNotificationChannels_PlatformAdminReadsUnfiltered(t *testing.T) {
	db, mock := newMock(t)
	r := NewNotificationChannelRepository(db)
	admin := tenantscope.Scope{PlatformAdmin: true}

	mock.ExpectQuery("FROM notification_channels ORDER BY").WithArgs().
		WillReturnRows(notifRow("n1", "sealed-target"))
	if out, err := r.ListInScope(ctx, admin); err != nil || len(out) != 1 {
		t.Fatalf("ListInScope(platform admin): %v %+v", err, out)
	}

	mock.ExpectQuery("FROM notification_channels WHERE id = ").WithArgs("n1").
		WillReturnRows(notifRow("n1", "sealed-target"))
	if got, err := r.GetByIDInScope(ctx, "n1", admin); err != nil || got == nil {
		t.Fatalf("GetByIDInScope(platform admin): %v %+v", err, got)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// ChannelOrgScope is the ONE conversion, so it is asserted directly rather than
// only through the readers above: a ChannelQueryOption is an opaque closure, and
// a test that could only check it was non-nil is the shape of a test that passes
// while the scope inside it reaches everything.
func TestChannelOrgScope(t *testing.T) {
	if !ChannelOrgScope().MatchesNothing() {
		t.Error("ChannelOrgScope() with no ids must match nothing; it is the fail-closed end " +
			"of every caller, including an alert raised by a run whose source was deleted")
	}
	if !ChannelOrgScope("", "   ").MatchesNothing() {
		t.Error("ChannelOrgScope of blank ids must match nothing. Passing a blank through would " +
			"bind an empty string into `organization_id = ANY($1)` against a uuid column")
	}
	s := ChannelOrgScope(scopeOrgA, "  ", scopeOrgB)
	if s.MatchesNothing() {
		t.Fatal("two real organizations produced a scope that matches nothing")
	}
	if got := s.OrganizationIDs(); len(got) != 2 || got[0] != scopeOrgA || got[1] != scopeOrgB {
		t.Errorf("ChannelOrgScope organizations = %v, want exactly [%s %s]", got, scopeOrgA, scopeOrgB)
	}
	if s.IsAllOrganizations() || s.IncludesUnowned() {
		t.Errorf("ChannelOrgScope produced a widened scope (%s). Neither widening is this "+
			"package's to grant: an unowned channel is reachable by a platform admin only.", s)
	}
}

// ---------------------------------------------------------------------------
// state_transfers
// ---------------------------------------------------------------------------

func TestTransferRepository_ScopedReadOnAnEmptyScopeTouchesNothing(t *testing.T) {
	db, mock := newMock(t)
	r := NewTransferRepository(db)

	if got, err := r.GetByIDInScope(ctx, "t1", tenantscope.Scope{}); err != nil || got != nil {
		t.Errorf("GetByIDInScope on an empty scope = %+v, %v; want nothing", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestTransferRepository_ScopedRead_BindsTheOrganization(t *testing.T) {
	scope := tenantscope.Scope{OrgIDs: []string{scopeOrgA}}
	db, mock := newMock(t)
	r := NewTransferRepository(db)

	row := sqlmock.NewRows(transferCols).
		AddRow("t1", "migrate", "s1", "k", "s2", "k2", "success", true, true, "ok", "alice", "2026-08-30")
	// The organization array is $1 and the id is $2, matching every other by-id
	// reader on a partition root in this package.
	mock.ExpectQuery("FROM state_transfers WHERE organization_id = ANY.+AND id").
		WithArgs([]string{scopeOrgA}, "t1").WillReturnRows(row)
	got, err := r.GetByIDInScope(ctx, "t1", scope)
	if err != nil || got == nil || got.ID != "t1" {
		t.Fatalf("GetByIDInScope: %v %+v", err, got)
	}
	if got.SourceID != "s1" || got.TargetSourceID != "s2" || got.SourceKey != "k" {
		t.Errorf("the scoped read dropped a projected column: %+v", got)
	}

	mock.ExpectQuery("FROM state_transfers WHERE organization_id = ANY.+AND id").
		WithArgs([]string{scopeOrgA}, "elsewhere").WillReturnError(sql.ErrNoRows)
	if got, err := r.GetByIDInScope(ctx, "elsewhere", scope); err != nil || got != nil {
		t.Errorf("a transfer outside the scope must be (nil, nil), got %+v %v", got, err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestTransferRepository_PlatformAdminReadsUnfiltered(t *testing.T) {
	db, mock := newMock(t)
	r := NewTransferRepository(db)

	row := sqlmock.NewRows(transferCols).
		AddRow("t1", "backup", "s1", "k", "s2", "k2", "success", nil, false, "", "", "2026-08-30")
	// No organization bound: the unscoped statement, so a transfer whose
	// organization_id is still NULL stays reachable by the one caller who can
	// repair it.
	mock.ExpectQuery("FROM state_transfers WHERE id = ").WithArgs("t1").WillReturnRows(row)
	if got, err := r.GetByIDInScope(ctx, "t1", tenantscope.Scope{PlatformAdmin: true}); err != nil || got == nil {
		t.Fatalf("GetByIDInScope(platform admin): %v %+v", err, got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
