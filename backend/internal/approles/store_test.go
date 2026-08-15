package approles

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func newStore(t *testing.T) (*Store, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewStore(db), mock
}

// "Every organization" and "no organizations" must issue DIFFERENT statements.
// Collapsing them is a one-character mistake (nil versus an empty slice) that
// turns a scope permitting nothing into a platform-wide strip, and it produces
// no error at any layer.
func TestDeleteRolesForUser_EverythingIsNotNothing(t *testing.T) {
	t.Run("nil strips every organization", func(t *testing.T) {
		s, mock := newStore(t)
		mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM organization_member_roles WHERE user_id = $1`)).
			WithArgs("user-1").
			WillReturnResult(sqlmock.NewResult(0, 3))
		if err := s.DeleteRolesForUser(context.Background(), "user-1", nil); err != nil {
			t.Fatalf("DeleteRolesForUser: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet: %v", err)
		}
	})

	t.Run("empty allowlist strips nothing", func(t *testing.T) {
		s, mock := newStore(t)
		// No expectation at all: issuing ANY statement fails this test.
		if err := s.DeleteRolesForUser(context.Background(), "user-1", []string{}); err != nil {
			t.Fatalf("DeleteRolesForUser: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("a scope permitting nothing issued a statement: %v", err)
		}
	})

	t.Run("allowlist narrows to its organizations", func(t *testing.T) {
		s, mock := newStore(t)
		mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM organization_member_roles WHERE user_id = $1 AND organization_id = ANY($2)`)).
			WithArgs("user-1", sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		if err := s.DeleteRolesForUser(context.Background(), "user-1", []string{"org-1"}); err != nil {
			t.Fatalf("DeleteRolesForUser: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet: %v", err)
		}
	})
}

// The sentinel is what lets the mirror tell "this deployment has no such role"
// apart from "the mirror's database is down". A bare error would collapse them,
// and the first is a case the mirror recovers from by adopting the template.
func TestTemplateIDByName_SentinelVersusFault(t *testing.T) {
	s, mock := newStore(t)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM role_templates WHERE name = $1`)).
		WithArgs("nope").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	_, err := s.TemplateIDByName(context.Background(), "nope")
	if !errors.Is(err, ErrNoTemplate) {
		t.Fatalf("an absent name must wrap ErrNoTemplate, got %v", err)
	}
	if !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("the error does not name the role: %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM role_templates WHERE name = $1`)).
		WithArgs("editor").
		WillReturnError(errors.New("connection refused"))
	_, err = s.TemplateIDByName(context.Background(), "editor")
	if errors.Is(err, ErrNoTemplate) {
		t.Fatal("a database fault was reported as 'no such role template', which the mirror would try to recover from by adopting a template that exists")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("the underlying fault was lost: %v", err)
	}
}

// TemplateExists must report absence as (false, nil) and a fault as an error:
// the mirror uses the boolean to decide whether to fetch the template from
// identity, and a fault reported as "absent" would send it fetching on every
// write while the database is down.
func TestTemplateExists_AbsenceIsNotAFault(t *testing.T) {
	s, mock := newStore(t)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT 1 FROM role_templates WHERE id = $1`)).
		WithArgs(templateID).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}))
	present, err := s.TemplateExists(context.Background(), templateID)
	if err != nil {
		t.Fatalf("TemplateExists: %v", err)
	}
	if present {
		t.Fatal("an empty result reported the template as present")
	}

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT 1 FROM role_templates WHERE id = $1`)).
		WithArgs(templateID).
		WillReturnError(errors.New("connection refused"))
	if _, err := s.TemplateExists(context.Background(), templateID); err == nil {
		t.Fatal("a database fault was reported as a clean 'not present'")
	}
}

// The sweep's cutoff is the APP DATABASE's clock, taken before the pass. Reading
// it from the process instead would compare two clocks, and on a replica running
// slightly ahead the sweep would delete rows the same reconcile had just written.
func TestGeneration_ComesFromTheDatabase(t *testing.T) {
	s, mock := newStore(t)
	dbClock := time.Date(2031, 4, 5, 6, 7, 8, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT now()`)).
		WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(dbClock))
	got, err := s.Generation(context.Background())
	if err != nil {
		t.Fatalf("Generation: %v", err)
	}
	if !got.Equal(dbClock) {
		t.Fatalf("Generation = %v, want the database's %v", got, dbClock)
	}
}

func TestSweepStaleAssignments_ReportsWhatItRemoved(t *testing.T) {
	s, mock := newStore(t)
	cutoff := time.Now()
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM organization_member_roles WHERE mirrored_at < $1`)).
		WithArgs(cutoff).
		WillReturnResult(sqlmock.NewResult(0, 7))
	n, err := s.SweepStaleAssignments(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("SweepStaleAssignments: %v", err)
	}
	if n != 7 {
		t.Fatalf("SweepStaleAssignments = %d, want 7", n)
	}
}

// An absent scope list must reach the column as [], not as JSON null: a reader
// would otherwise have to handle two spellings of "no scopes", and one of them
// unmarshals into a nil slice that HasScope treats identically to a role that
// was never loaded.
func TestUpsertTemplate_EncodesAbsentScopesAsAnEmptyArray(t *testing.T) {
	s, mock := newStore(t)
	mock.ExpectExec("INSERT INTO role_templates").
		WithArgs(templateID, "viewer", "Viewer", nil, "[]", false).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := s.UpsertTemplate(context.Background(), Template{ID: templateID, Name: "viewer", DisplayName: "Viewer"}); err != nil {
		t.Fatalf("UpsertTemplate: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}
