package credlifecycle

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/approles"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

var ctx = context.Background()

// apiKeyCols mirrors ListAPIKeysByUser's scanAPIKeyWithUserName projection.
var apiKeyCols = []string{
	"id", "user_id", "organization_id", "name", "description", "key_hash", "key_prefix", "scopes",
	"expires_at", "last_used_at", "expiry_notification_sent_at", "created_at", "user_name",
}

// membershipCols mirrors GetUserMemberships' projection, which
// GetUserCombinedScopes reads to re-derive the authority a principal retains.
var membershipCols = []string{"organization_id", "organization_name", "role_template_id",
	"created_at", "role_template_name", "role_template_display_name", "role_template_scopes"}

func newSweeper(t *testing.T) (*Sweeper, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewSweeper(
		repositories.NewUserTokenRevocationRepository(db),
		idstore.NewAPIKeyRepository(db),
		approles.NewMembers(db, nil, approles.RoleSourceIdentity),
		NoPlatformAdminCarrier{},
	), mock
}

// stubPlatformAdmins answers the carrier question directly, including with an
// error, which is the case the sweep must not read as "not an admin".
type stubPlatformAdmins struct {
	isAdmin bool
	err     error
}

func (f stubPlatformAdmins) IsPlatformAdmin(context.Context, string) (bool, error) {
	return f.isAdmin, f.err
}

// sweeperWithAdmins builds a Sweeper whose carrier answers as given.
func sweeperWithAdmins(t *testing.T, src platformAdminSource) (*Sweeper, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewSweeper(
		repositories.NewUserTokenRevocationRepository(db),
		idstore.NewAPIKeyRepository(db),
		approles.NewMembers(db, nil, approles.RoleSourceIdentity),
		src,
	), mock
}

func keyRow(id, scopes string) *sqlmock.Rows {
	return sqlmock.NewRows(apiKeyCols).
		AddRow(id, "u1", "o1", "CI", nil, "hash", "tsm_abc123", []byte(scopes),
			nil, nil, nil, time.Now(), "Alice")
}

// AuthorityRetained is what separates a REDUCTION from a change, and revoking a
// key is irreversible, so its semantics are pinned directly.
func TestAuthorityRetained(t *testing.T) {
	cases := []struct {
		name     string
		have     []string
		retained []string
		want     bool
	}{
		{"no scopes is vacuously retained", nil, nil, true},
		{"identical scopes are retained", []string{"state:read"}, []string{"state:read"}, true},
		{"read is retained under write", []string{"state:read"}, []string{"state:write"}, true},
		{"write is NOT retained under read", []string{"state:write"}, []string{"state:read"}, false},
		{"everything is retained under admin", []string{"state:write", "sources:manage"}, []string{"admin"}, true},
		{"nothing is retained under an empty set", []string{"state:read"}, nil, false},
		{"one lost scope loses the key", []string{"state:read", "sources:manage"}, []string{"state:read"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AuthorityRetained(tc.have, tc.retained); got != tc.want {
				t.Errorf("AuthorityRetained(%v, %v) = %v, want %v", tc.have, tc.retained, got, tc.want)
			}
		})
	}
}

// A nil *Sweeper is the no-op receiver handler sets rely on when the revocation
// subsystem is not wired; it must not panic or issue queries.
func TestNilSweeperIsNoOp(t *testing.T) {
	var s *Sweeper
	for _, out := range []Outcome{
		s.AuthorityReduced(ctx, "u1", "test"),
		s.KeysOnly(ctx, "u1", "test"),
		s.UserDeprovisioned(ctx, "u1", "test"),
	} {
		if out != (Outcome{}) {
			t.Errorf("nil sweeper returned %+v, want zero Outcome", out)
		}
	}
	if NewSweeper(nil, nil, nil, NoPlatformAdminCarrier{}) != nil {
		t.Error("NewSweeper with no repositories must return nil so the no-op receiver applies")
	}
}

func TestUserDeprovisioned_RevokesBothFamilies(t *testing.T) {
	s, mock := newSweeper(t)

	mock.ExpectExec("INSERT INTO user_token_revocations").WithArgs("u1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// ONE bulk delete keyed on the owner, since identity v0.25.0. Two things
	// follow from that and both matter: there is no window between a list and a
	// per-row delete in which a freshly minted key escapes the sweep, and the
	// per-key already-gone race disappears because a DELETE matching nothing is
	// a count, not an error to classify.
	mock.ExpectExec("DELETE FROM api_keys").WithArgs("u1").
		WillReturnResult(sqlmock.NewResult(0, 2))

	// Offboarding retains nothing, so no scope comparison happens: every key
	// goes, whatever it carries.
	out := s.UserDeprovisioned(ctx, "u1", "test")
	if !out.TokensRevoked || out.KeysRevoked != 2 || out.Incomplete {
		t.Fatalf("UserDeprovisioned = %+v, want tokens revoked and 2 keys revoked", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAuthorityReduced_RevokesOnlyOverAskingKeys(t *testing.T) {
	s, mock := newSweeper(t)

	mock.ExpectExec("INSERT INTO user_token_revocations").WithArgs("u1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// What the principal still holds after the change: read only.
	mock.ExpectQuery("FROM organization_members om").WithArgs("u1").
		WillReturnRows(sqlmock.NewRows(membershipCols).
			AddRow("o1", "default", nil, time.Now(), "viewer", "Viewer", []byte(`["state:read"]`)))
	mock.ExpectQuery("FROM api_keys").WithArgs("u1").
		WillReturnRows(keyRow("k-retained", `["state:read"]`).
			AddRow("k-overasking", "u1", "o1", "Deploy", nil, "hash", "tsm_def456",
				[]byte(`["state:write"]`), nil, nil, nil, time.Now(), "Alice"))
	// Only the key asking for more than the retained authority is destroyed —
	// an API key secret is shown once and cannot be recovered.
	mock.ExpectExec("DELETE FROM api_keys").WithArgs("k-overasking").
		WillReturnResult(sqlmock.NewResult(0, 1))

	out := s.AuthorityReduced(ctx, "u1", "test")
	if !out.TokensRevoked || out.KeysRevoked != 1 || out.KeysRetained != 1 || out.Incomplete {
		t.Fatalf("AuthorityReduced = %+v, want 1 revoked / 1 retained", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// The IdP login paths sweep keys but deliberately leave the watermark alone —
// moving it microseconds before the same request mints a token would revoke the
// token being issued. Asserted by registering NO watermark write: sqlmock fails
// on an unexpected statement.
func TestKeysOnly_LeavesWatermarkUntouched(t *testing.T) {
	s, mock := newSweeper(t)

	mock.ExpectQuery("FROM organization_members om").WithArgs("u1").
		WillReturnRows(sqlmock.NewRows(membershipCols).
			AddRow("o1", "default", nil, time.Now(), "viewer", "Viewer", []byte(`["state:read"]`)))
	mock.ExpectQuery("FROM api_keys").WithArgs("u1").
		WillReturnRows(keyRow("k1", `["state:write"]`))
	mock.ExpectExec("DELETE FROM api_keys").WithArgs("k1").WillReturnResult(sqlmock.NewResult(0, 1))

	out := s.KeysOnly(ctx, "u1", "test")
	if out.TokensRevoked {
		t.Error("KeysOnly must not move the JWT watermark")
	}
	if out.KeysRevoked != 1 || out.Incomplete {
		t.Fatalf("KeysOnly = %+v, want 1 key revoked", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Every sweep runs after the authority change has committed, so a failure is
// reported rather than rolled back. Incomplete is how a handler learns the
// reduction landed but the sweep did not.
func TestSweepFailuresReportIncomplete(t *testing.T) {
	t.Run("watermark write fails", func(t *testing.T) {
		s, mock := newSweeper(t)
		mock.ExpectExec("INSERT INTO user_token_revocations").WillReturnError(sql.ErrConnDone)
		mock.ExpectQuery("FROM organization_members om").
			WillReturnRows(sqlmock.NewRows(membershipCols))
		mock.ExpectQuery("FROM api_keys").WillReturnRows(sqlmock.NewRows(apiKeyCols))
		if out := s.AuthorityReduced(ctx, "u1", "test"); !out.Incomplete || out.TokensRevoked {
			t.Errorf("AuthorityReduced = %+v, want Incomplete", out)
		}
	})

	t.Run("key listing fails", func(t *testing.T) {
		s, mock := newSweeper(t)
		mock.ExpectExec("INSERT INTO user_token_revocations").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectQuery("FROM api_keys").WillReturnError(sql.ErrConnDone)
		if out := s.UserDeprovisioned(ctx, "u1", "test"); !out.Incomplete {
			t.Errorf("UserDeprovisioned = %+v, want Incomplete", out)
		}
	})

	t.Run("key revocation fails", func(t *testing.T) {
		s, mock := newSweeper(t)
		mock.ExpectExec("INSERT INTO user_token_revocations").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectQuery("FROM api_keys").WillReturnRows(keyRow("k1", `["admin"]`))
		mock.ExpectExec("DELETE FROM api_keys").WillReturnError(sql.ErrConnDone)
		if out := s.UserDeprovisioned(ctx, "u1", "test"); !out.Incomplete || out.KeysRevoked != 0 {
			t.Errorf("UserDeprovisioned = %+v, want Incomplete with nothing revoked", out)
		}
	})

	t.Run("retained authority cannot be re-derived", func(t *testing.T) {
		s, mock := newSweeper(t)
		mock.ExpectQuery("FROM organization_members om").WillReturnError(sql.ErrConnDone)
		// No DELETE is registered: without the retained set, "revoke everything"
		// would destroy unrecoverable credentials over a transient read error.
		if out := s.KeysOnly(ctx, "u1", "test"); !out.Incomplete || out.KeysRevoked != 0 {
			t.Errorf("KeysOnly = %+v, want Incomplete with nothing revoked", out)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet expectations: %v", err)
		}
	})

	t.Run("organization repository not wired", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock.New: %v", err)
		}
		defer db.Close()
		s := NewSweeper(repositories.NewUserTokenRevocationRepository(db), idstore.NewAPIKeyRepository(db), nil, NoPlatformAdminCarrier{})
		mock.ExpectExec("INSERT INTO user_token_revocations").WillReturnResult(sqlmock.NewResult(0, 1))
		if out := s.AuthorityReduced(ctx, "u1", "test"); !out.Incomplete || out.KeysRevoked != 0 {
			t.Errorf("AuthorityReduced = %+v, want Incomplete with nothing revoked", out)
		}
	})
}

// ---------------------------------------------------------------------------
// #492 — the retained set must include authority held through the platform-admin
// carrier, not only through organization memberships.
//
// A platform admin need not be a member of any organization (#485 exists
// because they cannot administer one they do not belong to), so the
// membership-derived set for such a principal is EMPTY. Every key they own then
// looks like it over-asks, and all of them are hard-deleted with no revoked_at.
// ---------------------------------------------------------------------------

// expectNoMemberships stages the membership read returning nothing, which is the
// normal state for a platform admin.
func expectNoMemberships(mock sqlmock.Sqlmock) {
	mock.ExpectQuery("(?s)FROM organization_members").
		WillReturnRows(sqlmock.NewRows([]string{
			"organization_id", "user_id", "role_template_id", "created_at",
			"role_name", "role_display_name", "role_scopes",
		}))
}

func TestRevokeOverAskingKeys_PlatformAdminKeysAreRetained(t *testing.T) {
	s, mock := sweeperWithAdmins(t, stubPlatformAdmins{isAdmin: true})

	mock.ExpectExec("INSERT INTO user_token_revocations").WillReturnResult(sqlmock.NewResult(0, 1))
	expectNoMemberships(mock)
	mock.ExpectQuery("(?s)FROM api_keys").
		WillReturnRows(keyRow("key-1", `["states:write","sources:write"]`))
	// No DELETE is staged. That IS the assertion: sqlmock rejects a statement it
	// was not told to expect, so a revocation here fails the test.

	out := s.AuthorityReduced(context.Background(), "admin-1", "admin: membership removed")

	if out.KeysRevoked != 0 {
		t.Errorf("KeysRevoked = %d, want 0: a platform admin's authority covers these keys", out.KeysRevoked)
	}
	if out.KeysRetained != 1 {
		t.Errorf("KeysRetained = %d, want 1", out.KeysRetained)
	}
}

// The falsification. Without it, retaining EVERYTHING unconditionally would
// satisfy the test above and disable the sweep entirely.
func TestRevokeOverAskingKeys_NonAdminWithNoMembershipsStillLosesKeys(t *testing.T) {
	s, mock := sweeperWithAdmins(t, stubPlatformAdmins{isAdmin: false})

	mock.ExpectExec("INSERT INTO user_token_revocations").WillReturnResult(sqlmock.NewResult(0, 1))
	expectNoMemberships(mock)
	mock.ExpectQuery("(?s)FROM api_keys").
		WillReturnRows(keyRow("key-1", `["states:write"]`))
	mock.ExpectExec("DELETE FROM api_keys WHERE id").
		WillReturnResult(sqlmock.NewResult(0, 1))

	out := s.AuthorityReduced(context.Background(), "user-1", "admin: membership removed")

	if out.KeysRevoked != 1 {
		t.Errorf("KeysRevoked = %d, want 1: a fully deprovisioned principal retains nothing", out.KeysRevoked)
	}
}

// An unreadable carrier means the authority is UNKNOWN, not absent. Deleting on
// an unknown is exactly how a platform admin loses every key they own, and the
// deletion cannot be undone.
func TestRevokeOverAskingKeys_CarrierErrorRevokesNothingAndReportsIncomplete(t *testing.T) {
	s, mock := sweeperWithAdmins(t, stubPlatformAdmins{err: errors.New("platform_admins unreadable")})

	mock.ExpectExec("INSERT INTO user_token_revocations").WillReturnResult(sqlmock.NewResult(0, 1))
	expectNoMemberships(mock)
	// The key list and a DELETE ARE staged, deliberately, even though the sweep
	// must not reach them.
	//
	// Staging nothing here does not work: the sweep would then stop at the
	// unstaged key list instead of at the carrier check, report the same
	// Incomplete, and the test would pass against a version that ignores the
	// carrier error entirely. Staging a viable path means a sweep that carries
	// on WILL delete the key and be caught by KeysRevoked below.
	//
	// ExpectationsWereMet is deliberately NOT asserted: leaving these unconsumed
	// is the correct outcome.
	mock.ExpectQuery("(?s)FROM api_keys").
		WillReturnRows(keyRow("key-1", `["states:write"]`))
	mock.ExpectExec("DELETE FROM api_keys WHERE id").
		WillReturnResult(sqlmock.NewResult(0, 1))

	out := s.AuthorityReduced(context.Background(), "admin-1", "admin: membership removed")

	if out.KeysRevoked != 0 {
		t.Errorf("KeysRevoked = %d, want 0: an authority that could not be determined must never authorise an irreversible delete", out.KeysRevoked)
	}
	if !out.Incomplete {
		t.Error("Incomplete must be set: the sweep did not run, and that has to be visible")
	}
}

// A deployment with no carrier at all is not an unknown: there is no
// carrier-held authority to miss, so memberships are the whole picture and the
// sweep proceeds exactly as before.
func TestRevokeOverAskingKeys_NoCarrierConfiguredStillSweeps(t *testing.T) {
	s, mock := sweeperWithAdmins(t, NoPlatformAdminCarrier{})

	mock.ExpectExec("INSERT INTO user_token_revocations").WillReturnResult(sqlmock.NewResult(0, 1))
	expectNoMemberships(mock)
	mock.ExpectQuery("(?s)FROM api_keys").
		WillReturnRows(keyRow("key-1", `["states:write"]`))
	mock.ExpectExec("DELETE FROM api_keys WHERE id").
		WillReturnResult(sqlmock.NewResult(0, 1))

	out := s.AuthorityReduced(context.Background(), "user-1", "admin: membership removed")

	if out.KeysRevoked != 1 {
		t.Errorf("KeysRevoked = %d, want 1", out.KeysRevoked)
	}
	if out.Incomplete {
		t.Error("a deployment without a carrier is a known state, not an incomplete sweep")
	}
}
