// notfound_class_test.go pins the credential sweep's behaviour when a key it
// was about to revoke is already gone, across terraform-suite-identity's
// store.ErrNotFound change (module v0.24.0).
//
// The SELECTIVE sweep (revokeOverAskingKeys, behind the membership and role
// routes) lists a user's keys and then deletes them one by one, because it has
// to compare each key's frozen scopes against the authority the user retains.
// That leaves a window: a rotation, a parallel sweep, or an admin can destroy a
// row between the list and the delete. Since v0.24.0 that zero-row delete
// reports ErrNotFound instead of nil.
//
// The WHOLE-PRINCIPAL sweep (UserDeprovisioned) has no such window since
// identity v0.25.0: it is one bulk DELETE keyed on the owner, and a bulk delete
// that matches nothing returns a count of zero rather than an error, so there
// is no sentinel to classify. Its rows below assert that directly.
//
// Outcome.Incomplete is not a log field — AdminHandlers.DeleteUser and
// EraseUser turn it into a 500 and refuse to remove the account. Counting a
// raced key as a failure would therefore make OFFBOARDING FAIL precisely
// because one of the user's credentials was already destroyed, which is the
// outcome the sweep exists to produce.
package credlifecycle

import (
	"errors"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// TestUserDeprovisioned_AlreadyRevokedKey_DoesNotBlockOffboarding is the case
// that matters: keys that were already gone must leave Incomplete false, so the
// caller still deletes the account. Since v0.25.0 the bulk sweep expresses that
// as a zero affected-row count, which is the shape that CANNOT be mistaken for
// a failure — the sentinel-vs-error classification the per-key loop needed is
// gone from this path entirely.
func TestUserDeprovisioned_AlreadyRevokedKey_DoesNotBlockOffboarding(t *testing.T) {
	s, mock := newSweeper(t)
	mock.ExpectExec("INSERT INTO user_token_revocations").WithArgs("u1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Zero rows: the keys were destroyed by something else first.
	mock.ExpectExec("DELETE FROM api_keys").WithArgs("u1").
		WillReturnResult(sqlmock.NewResult(0, 0))

	out := s.UserDeprovisioned(ctx, "u1", "admin: user deleted")
	if out.Incomplete {
		t.Fatal("a key that was already gone must NOT mark the sweep incomplete — " +
			"AdminHandlers.DeleteUser turns Incomplete into a 500 and refuses to " +
			"delete the account, so a raced key would block offboarding entirely")
	}
	if out.KeysRevoked != 0 {
		t.Errorf("KeysRevoked = %d, want 0: this sweep did not revoke anything", out.KeysRevoked)
	}
	if !out.TokensRevoked {
		t.Error("the session watermark must still have moved")
	}
}

// TestUserDeprovisioned_RealDeleteFailure_IsIncomplete is the counterweight: the
// skip above must key on the sentinel, not on "any delete error". A genuine
// failure still blocks the offboarding, because the key really is still live.
func TestUserDeprovisioned_RealDeleteFailure_IsIncomplete(t *testing.T) {
	s, mock := newSweeper(t)
	mock.ExpectExec("INSERT INTO user_token_revocations").WithArgs("u1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM api_keys").WithArgs("u1").
		WillReturnError(errors.New("connection refused"))

	out := s.UserDeprovisioned(ctx, "u1", "admin: user deleted")
	if !out.Incomplete {
		t.Fatal("a real delete failure must mark the sweep incomplete: the keys are still live")
	}
}

// TestAuthorityReduced_RacedOverAskingKey_IsNotIncomplete covers the other
// revocation loop, the scope-filtered one behind the membership/role routes.
func TestAuthorityReduced_RacedOverAskingKey_IsNotIncomplete(t *testing.T) {
	s, mock := newSweeper(t)
	mock.ExpectExec("INSERT INTO user_token_revocations").WithArgs("u1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Retained authority after the reduction: read only.
	mock.ExpectQuery("FROM organization_members om").WithArgs("u1").
		WillReturnRows(sqlmock.NewRows(membershipCols).
			AddRow("o1", "default", nil, time.Now(), "viewer", "Viewer", []byte(`["state:read"]`)))
	// The key asks for write, so it over-asks and is selected for revocation...
	mock.ExpectQuery("FROM api_keys").WithArgs("u1").
		WillReturnRows(keyRow("k1", `["state:write"]`))
	// ...but it is already gone.
	mock.ExpectExec("DELETE FROM api_keys").WithArgs("k1").
		WillReturnResult(sqlmock.NewResult(0, 0))

	out := s.AuthorityReduced(ctx, "u1", "admin: organization membership removed")
	if out.Incomplete {
		t.Fatal("an already-revoked over-asking key must not mark the sweep incomplete")
	}
}
