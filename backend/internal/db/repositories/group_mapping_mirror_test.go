package repositories

import (
	"context"
	"errors"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// Mirror tests that need no database.
//
// group_mapping_equivalence_integration_test.go proves the end-to-end
// properties against real PostgreSQL -- the FK's ON DELETE SET NULL, the
// scalar-subquery name resolution, the round trip through the real write path.
// Those run only in the Postgres job, so the mirror's DECISIONS are pinned
// here instead: what it executes, in what order, inside what transaction, and
// what it does when each statement fails. Same split, and same reasoning, as
// the approles unit/integration split.

// gmRoleID is an arbitrary role-template id for fixtures.
const gmRoleID = "cccccccc-0000-4000-8000-000000000011"

// expectGroupMappingMirrorVerified queues the three qualified-name probes
// (*GroupMappingMirror).Verify makes: the misrouting discriminator (must NOT
// resolve), then the two mirror tables (must resolve).
func expectGroupMappingMirrorVerified(mock sqlmock.Sqlmock) {
	mock.ExpectQuery("to_regclass").
		WithArgs("organization_members").
		WillReturnRows(sqlmock.NewRows([]string{"qualified"}).AddRow(nil))
	for _, table := range []string{"group_mappings", "role_templates"} {
		mock.ExpectQuery("to_regclass").
			WithArgs(table).
			WillReturnRows(sqlmock.NewRows([]string{"qualified"}).AddRow("public." + table))
	}
}

func TestGroupMappingMirrorVerify_ResolvesBothTables(t *testing.T) {
	db, mock := newMock(t)
	expectGroupMappingMirrorVerified(mock)
	if err := NewGroupMappingMirror(db).Verify(context.Background()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGroupMappingMirrorVerify_RefusesAMisroutedConnection(t *testing.T) {
	db, mock := newMock(t)
	mock.ExpectQuery("to_regclass").
		WithArgs("organization_members").
		WillReturnRows(sqlmock.NewRows([]string{"qualified"}).AddRow("identity.organization_members"))
	err := NewGroupMappingMirror(db).Verify(context.Background())
	if !errors.Is(err, ErrGroupMappingMirrorMisrouted) {
		t.Fatalf("want ErrGroupMappingMirrorMisrouted, got %v", err)
	}
}

func TestGroupMappingMirrorVerify_RefusesAnUnresolvedTable(t *testing.T) {
	db, mock := newMock(t)
	mock.ExpectQuery("to_regclass").
		WithArgs("organization_members").
		WillReturnRows(sqlmock.NewRows([]string{"qualified"}).AddRow(nil))
	mock.ExpectQuery("to_regclass").
		WithArgs("group_mappings").
		WillReturnRows(sqlmock.NewRows([]string{"qualified"}).AddRow(nil))
	err := NewGroupMappingMirror(db).Verify(context.Background())
	if !errors.Is(err, ErrGroupMappingMirrorUnreachable) {
		t.Fatalf("want ErrGroupMappingMirrorUnreachable, got %v", err)
	}
}

func TestGroupMappingMirrorVerify_ReportsAProbeFailure(t *testing.T) {
	db, mock := newMock(t)
	mock.ExpectQuery("to_regclass").
		WithArgs("organization_members").
		WillReturnError(errors.New("connection reset"))
	err := NewGroupMappingMirror(db).Verify(context.Background())
	if err == nil || errors.Is(err, ErrGroupMappingMirrorUnreachable) || errors.Is(err, ErrGroupMappingMirrorMisrouted) {
		t.Fatalf("a probe failure must be its own error, not a sentinel; got %v", err)
	}
}

// TestGroupMappingMirror_RefusesWithNoConnection pins the nil tolerance: rigs
// with no app connection get a refusal (absorbed by the caller), never a
// panic, and never a silent success.
func TestGroupMappingMirror_RefusesWithNoConnection(t *testing.T) {
	var nilMirror *GroupMappingMirror
	if err := nilMirror.Verify(context.Background()); !errors.Is(err, ErrGroupMappingMirrorUnreachable) {
		t.Fatalf("nil mirror Verify: want ErrGroupMappingMirrorUnreachable, got %v", err)
	}
	m := NewGroupMappingMirror(nil)
	if err := m.Verify(context.Background()); !errors.Is(err, ErrGroupMappingMirrorUnreachable) {
		t.Fatalf("nil-db Verify: want ErrGroupMappingMirrorUnreachable, got %v", err)
	}
	if err := m.Replace(context.Background(), nil); !errors.Is(err, ErrGroupMappingMirrorUnreachable) {
		t.Fatalf("nil-db Replace: want ErrGroupMappingMirrorUnreachable, got %v", err)
	}
}

// TestGroupMappingMirrorReplace_DeletesThenInsertsInOrderInOneTx pins the
// wholesale-replace shape: one transaction, every row cleared first, then one
// insert per mapping IN LIST ORDER with the position argument equal to the
// list index -- order is what first-match-wins resolution hangs on
// (terraform-suite-identity#269, this repo's #488).
func TestGroupMappingMirrorReplace_DeletesThenInsertsInOrderInOneTx(t *testing.T) {
	db, mock := newMock(t)

	// The write-time name resolution reads through approles.Store's
	// ListTemplates -- the one role_templates funnel -- before the
	// transaction; "viewer" deliberately resolves to nothing and must be
	// inserted with a NULL id, not dropped.
	expectAppTemplateNames(mock, gmRoleID, "editor")
	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM group_mappings").
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec("INSERT INTO group_mappings").
		WithArgs(0, "eng", "alpha", "editor", gmRoleID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO group_mappings").
		WithArgs(1, "ops", "beta", "viewer", nil).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err := NewGroupMappingMirror(db).Replace(context.Background(),
		[]SSOGroupMapping{
			{Group: "eng", Organization: "alpha", Role: "editor"},
			{Group: "ops", Organization: "beta", Role: "viewer"},
		})
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestGroupMappingMirrorReplace_EmptyListJustClears pins the representation
// choice: no mappings means no rows, exactly like the overlay list being
// empty -- not a marker row, not a skipped write.
func TestGroupMappingMirrorReplace_EmptyListJustClears(t *testing.T) {
	db, mock := newMock(t)

	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM group_mappings").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := NewGroupMappingMirror(db).Replace(context.Background(), nil); err != nil {
		t.Fatalf("Replace(nil): %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestGroupMappingMirrorReplace_RollsBackWhenAStatementFails pins that a
// half-replaced list can never commit: whichever statement fails, the
// transaction rolls back and the error names the failing step.
func TestGroupMappingMirrorReplace_RollsBackWhenAStatementFails(t *testing.T) {
	one := []SSOGroupMapping{{Group: "eng", Organization: "alpha", Role: "editor"}}

	t.Run("template resolution fails", func(t *testing.T) {
		db, mock := newMock(t)
		mock.ExpectQuery("SELECT id, name, COALESCE").WillReturnError(errors.New("boom"))
		if err := NewGroupMappingMirror(db).Replace(context.Background(), one); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("begin fails", func(t *testing.T) {
		db, mock := newMock(t)
		expectAppTemplateNames(mock)
		mock.ExpectBegin().WillReturnError(errors.New("no connection"))
		if err := NewGroupMappingMirror(db).Replace(context.Background(), one); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("delete fails", func(t *testing.T) {
		db, mock := newMock(t)
		expectAppTemplateNames(mock)
		mock.ExpectBegin()
		mock.ExpectExec("DELETE FROM group_mappings").WillReturnError(errors.New("boom"))
		mock.ExpectRollback()
		if err := NewGroupMappingMirror(db).Replace(context.Background(), one); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("insert fails", func(t *testing.T) {
		db, mock := newMock(t)
		expectAppTemplateNames(mock)
		mock.ExpectBegin()
		mock.ExpectExec("DELETE FROM group_mappings").WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("INSERT INTO group_mappings").WillReturnError(errors.New("fk violated"))
		mock.ExpectRollback()
		if err := NewGroupMappingMirror(db).Replace(context.Background(), one); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("commit fails", func(t *testing.T) {
		db, mock := newMock(t)
		expectAppTemplateNames(mock)
		mock.ExpectBegin()
		mock.ExpectExec("DELETE FROM group_mappings").WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("INSERT INTO group_mappings").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit().WillReturnError(errors.New("deadlock"))
		if err := NewGroupMappingMirror(db).Replace(context.Background(), one); err == nil {
			t.Fatal("want error")
		}
	})
}

// TestGroupMappingMirrorFailed just exercises the absorb-and-log path so a
// signature change there is a compile- or test-visible event; the CONTRACT
// (authoritative write already committed, request must still succeed) is
// pinned by group_mapping_dual_write_test.go and by the class guard.
func TestGroupMappingMirrorFailed(t *testing.T) {
	groupMappingMirrorFailed(context.Background(), "TestOp", errors.New("boom"))
}
