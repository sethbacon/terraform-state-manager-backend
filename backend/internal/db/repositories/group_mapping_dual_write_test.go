package repositories

import (
	"context"
	"errors"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// The repository half of the group-mapping dual-write
// (terraform-suite-identity#206 phase 2, migration 000036), pinned without a
// database.
//
// Two contracts, and the second is the whole safety argument for the phase:
//
//  1. The one authoritative write that can change the DB-stored mapping list
//     (SSOSettingsRepository.Upsert) is FOLLOWED by the mirror write, carrying
//     the new list -- in all three flavours: the first save (INSERT), an edit
//     (UPDATE), and saving an empty list (DELETE of the mirror rows).
//  2. A mirror failure is ABSORBED: the authoritative write has committed and
//     reads still come from sso_settings, so the caller's request must succeed
//     anyway -- turning the mirror's failure into a 500 on the group-mapping
//     admin path would make a nothing-observable-changes phase change
//     behaviour. And an authoritative failure must reach the mirror NEVER, or
//     the mirror would hold a list the source refused.

// newDualWriteRepo builds the repository over two scripted connections --
// source backs the authoritative sso_settings write, app backs the mirror --
// mirroring NewAuthHandlers' wiring shape (identity pool + app connection).
func newDualWriteRepo(t *testing.T) (*SSOSettingsRepository, sqlmock.Sqlmock, sqlmock.Sqlmock) {
	t.Helper()
	sourceDB, sourceMock := newMock(t)
	appDB, appMock := newMock(t)
	return NewSSOSettingsRepository(sourceDB, appDB), sourceMock, appMock
}

func expectOverlayUpserted(mock sqlmock.Sqlmock) {
	mock.ExpectExec("INSERT INTO sso_settings").WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestSSOSettingsUpsert_MirrorsTheNewListInOrder(t *testing.T) {
	repo, sourceMock, appMock := newDualWriteRepo(t)
	expectOverlayUpserted(sourceMock)
	expectAppTemplateNames(appMock, gmRoleID, "editor")
	appMock.ExpectBegin()
	appMock.ExpectExec("DELETE FROM group_mappings").WillReturnResult(sqlmock.NewResult(0, 0))
	appMock.ExpectExec("INSERT INTO group_mappings").
		WithArgs(0, "eng", "alpha", "editor", gmRoleID).WillReturnResult(sqlmock.NewResult(0, 1))
	appMock.ExpectExec("INSERT INTO group_mappings").
		WithArgs(1, "ops", "beta", "viewer", nil).WillReturnResult(sqlmock.NewResult(0, 1))
	appMock.ExpectCommit()

	err := repo.Upsert(context.Background(), &SSOSettings{
		OIDCGroupClaimName: "groups", OIDCDefaultRole: "viewer",
		OIDCGroupMappings: []SSOGroupMapping{
			{Group: "eng", Organization: "alpha", Role: "editor"},
			{Group: "ops", Organization: "beta", Role: "viewer"},
		},
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := appMock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the mirror leg did not run: %v", err)
	}
	if err := sourceMock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestSSOSettingsUpsert_EmptyListClearsTheMirror is the DELETE flavour: the
// admin removing every mapping must remove the mirror rows too, or the read
// cutover would resurrect deleted policy.
func TestSSOSettingsUpsert_EmptyListClearsTheMirror(t *testing.T) {
	repo, sourceMock, appMock := newDualWriteRepo(t)
	expectOverlayUpserted(sourceMock)
	appMock.ExpectBegin()
	appMock.ExpectExec("DELETE FROM group_mappings").WillReturnResult(sqlmock.NewResult(0, 2))
	appMock.ExpectCommit()

	if err := repo.Upsert(context.Background(), &SSOSettings{OIDCGroupMappings: nil}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := appMock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the mirror clear did not run: %v", err)
	}
}

// TestSSOSettingsUpsert_MirrorFailureDoesNotFailTheRequest is contract 2's
// first half.
func TestSSOSettingsUpsert_MirrorFailureDoesNotFailTheRequest(t *testing.T) {
	repo, sourceMock, appMock := newDualWriteRepo(t)
	expectOverlayUpserted(sourceMock)
	expectAppTemplateNames(appMock)
	appMock.ExpectBegin().WillReturnError(errors.New("mirror down"))

	err := repo.Upsert(context.Background(), &SSOSettings{
		OIDCGroupMappings: []SSOGroupMapping{{Group: "eng", Organization: "alpha", Role: "editor"}},
	})
	if err != nil {
		t.Fatalf("a mirror failure surfaced to the caller: %v", err)
	}
}

// TestSSOSettingsUpsert_AuthoritativeFailureNeverReachesTheMirror is contract
// 2's second half: sqlmock fails on any statement it was not told to expect,
// and the app mock below expects NONE.
func TestSSOSettingsUpsert_AuthoritativeFailureNeverReachesTheMirror(t *testing.T) {
	repo, sourceMock, appMock := newDualWriteRepo(t)
	sourceMock.ExpectExec("INSERT INTO sso_settings").WillReturnError(errors.New("refused"))

	err := repo.Upsert(context.Background(), &SSOSettings{
		OIDCGroupMappings: []SSOGroupMapping{{Group: "eng", Organization: "alpha", Role: "editor"}},
	})
	if err == nil {
		t.Fatal("want the authoritative error")
	}
	if err := appMock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
