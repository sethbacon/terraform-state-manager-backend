package bootstrap

import (
	"context"
	"errors"
	"regexp"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/approles"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// Guards for this application's two halves of the role-template authority
// reduction (#557): the definitions it SUPPLIES, and what it does when a build
// narrows one of them.

// GUARD definer-supplies-and-writes-nothing. The whole reason TemplateDefiner
// returns a slice: the definitions the reconcile compares for a narrowing and
// the definitions it writes are then the same values, rather than two things
// connected by the definer's good behaviour.
//
// The mock is given NO expectations, so any statement at all fails the test.
//
// MUTATION: have appRoleDefinitions write through a store again.
func TestAppRoleDefinitions_SuppliesEveryRoleAndWritesNothing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	defs, err := appRoleDefinitions(context.Background())
	if err != nil {
		t.Fatalf("appRoleDefinitions: %v", err)
	}

	seeds := auth.AppRoleTemplates()
	if len(defs) != len(seeds) {
		t.Fatalf("supplied %d definitions, want %d", len(defs), len(seeds))
	}
	for i, rt := range seeds {
		if defs[i].Name != rt.Name {
			t.Errorf("definition %d is %q, want %q — order carries meaning to the reconcile's per-definition write",
				i, defs[i].Name, rt.Name)
		}
		if len(defs[i].Scopes) != len(rt.Scopes) {
			t.Errorf("%s: supplied %v, want this build's %v", rt.Name, defs[i].Scopes, rt.Scopes)
		}
		if !defs[i].IsSystem {
			t.Errorf("%s: IsSystem = false, want true for a role this build defines", rt.Name)
		}
		if defs[i].ID != "" {
			t.Errorf("%s: carries id %q — the write conflicts on NAME and must keep whatever uuid the "+
				"deployment already holds, which an id here would override", rt.Name, defs[i].ID)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the definer issued a statement: %v", err)
	}
}

// GUARD narrowing-ends-sessions-and-leaves-keys-alone. The application's answer
// to a reduction: move every holder's revoke-all watermark, and issue nothing
// against api_keys.
//
// The api_keys half is asserted by the absence of a statement — the mock is
// given exactly one expectation. That is deliberate rather than an omission:
// middleware.authenticateAPIKey already caps a key's stored scopes by the
// owner's CURRENT combined scopes on every request, so the narrowing reaches
// keys without a sweep, while deleting them here would be irreversible, on the
// startup path, for a change that might be a typo.
//
// MUTATION: sweep API keys here too; or move no watermark at all.
func TestRetireSessions_MovesTheWatermarkAndTouchesNoKeys(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO user_token_revocations`)).
		WillReturnResult(sqlmock.NewResult(0, 2))

	reduce := retireSessionsOfNarrowedRoles(repositories.NewUserTokenRevocationRepository(db))
	err = reduce(context.Background(), []approles.ReducedTemplate{{
		ID: "t-1", Name: "editor",
		Was: []string{"state:read", "state:write"}, Now: []string{"state:read"},
		Holders: []string{"user-a", "user-b"},
	}})
	if err != nil {
		t.Fatalf("reducer: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("statements did not match — an extra one means a credential family was swept that "+
			"the request-time cap already covers: %v", err)
	}
}

// GUARD narrowing-without-a-way-to-end-sessions-is-refused. A deployment that
// cannot invalidate must not narrow a role and report a clean boot.
//
// MUTATION: return nil when the repository is absent.
func TestRetireSessions_RefusesWithNoRepository(t *testing.T) {
	reduce := retireSessionsOfNarrowedRoles(nil)
	err := reduce(context.Background(), []approles.ReducedTemplate{{Name: "editor", Holders: []string{"user-a"}}})
	if err == nil {
		t.Fatal("a narrowing was accepted with no way to end its holders' sessions")
	}
}

// GUARD watermark-failure-is-surfaced. The reducer's error is what stops the
// narrowing, so it must come back rather than be logged and swallowed.
//
// MUTATION: log the repository error and return nil.
func TestRetireSessions_SurfacesTheWatermarkFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	boom := errors.New("watermark write failed")
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO user_token_revocations`)).WillReturnError(boom)

	reduce := retireSessionsOfNarrowedRoles(repositories.NewUserTokenRevocationRepository(db))
	err = reduce(context.Background(), []approles.ReducedTemplate{{Name: "editor", Holders: []string{"user-a"}}})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the repository's own failure", err)
	}
}
