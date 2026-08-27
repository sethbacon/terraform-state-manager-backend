package legalhold

// legalhold_test.go covers the hold store, and above all the property the whole
// feature rests on: the table this package writes must be the table the
// retention sweep reads (#373).

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
)

func repoFor(t *testing.T) (*Repository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r, err := New(db)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, mock
}

// TestEveryStatementNamesTheSweepsTable is THE property.
//
// The sweep runs on the identity pool, whose search_path is "identity,public".
// If this package wrote an unqualified legal_holds while the sweep read a
// qualified one -- or the reverse -- the two could resolve to different tables.
// Every hold would look placed, the API would return 201, and the sweep would
// delete the rows anyway and report success. Nothing would be wrong-looking
// anywhere.
//
// So every statement here is asserted to name the SAME qualified relation the
// application passes to store.WithLegalHolds.
func TestEveryStatementNamesTheSweepsTable(t *testing.T) {
	src := readSource(t)
	// The qualified form as it appears in SQL.
	const qualified = `"public"."legal_holds"`

	stmts := regexp.MustCompile(`(?i)(INSERT INTO|UPDATE|FROM)\s+("?[a-z_]+"?\."?[a-z_]+"?|"?legal_holds"?)`).
		FindAllStringSubmatch(src, -1)
	if len(stmts) == 0 {
		t.Fatal("no SQL statements were found in this package; the assertion below checks nothing")
	}
	for _, m := range stmts {
		if m[2] != qualified {
			t.Errorf("a statement names %s rather than %s.\n"+
				"The sweep resolves its exemption on the identity pool (search_path=identity,public), "+
				"so an unqualified name here could address a different table than the sweep reads -- "+
				"holds would look placed and the rows would be deleted anyway.", m[2], qualified)
		}
	}
	t.Logf("checked %d statements, all naming %s", len(stmts), qualified)
}

// TestTableConstantMatchesTheStatements ties the exported name -- the one handed
// to store.WithLegalHolds -- to the SQL this package actually runs.
func TestTableConstantMatchesTheStatements(t *testing.T) {
	if Table != "public.legal_holds" {
		t.Fatalf("Table = %q; the migration creates public.legal_holds", Table)
	}
	// The same relation, written the way SQL writes it.
	parts := strings.SplitN(Table, ".", 2)
	if len(parts) != 2 {
		t.Fatal("Table is not schema-qualified, so the sweep could resolve it elsewhere")
	}
	want := `"` + parts[0] + `"."` + parts[1] + `"`
	if !strings.Contains(readSource(t), want) {
		t.Errorf("no statement in this package names %s, so the exported Table constant and the "+
			"SQL disagree about which relation holds live in", want)
	}
}

// TestTableIsAcceptedByTheSweepsValidator.
//
// store.WithLegalHolds validates the name at statement-build time and refuses a
// malformed one. If Table were unacceptable there, the sweep would error on
// every run -- discovered in production rather than here.
func TestTableIsAcceptedByTheSweepsValidator(t *testing.T) {
	if err := idstore.VerifyLegalHoldTable(context.Background(), nil, Table); err == nil {
		t.Fatal("VerifyLegalHoldTable accepted a nil database; this test cannot distinguish a " +
			"name error from a connection error")
	} else if !strings.Contains(err.Error(), "no database connection") {
		// A nil DB must fail on the CONNECTION, not on the name. If it failed
		// on the name, Table is malformed.
		t.Errorf("Table %q was rejected by the sweep's own validator before it reached the "+
			"connection check: %v", Table, err)
	}
}

func TestPlaceRejectsAnInvertedRange(t *testing.T) {
	r, _ := repoFor(t)
	now := time.Now()
	_, err := r.Place(context.Background(), "n", "r", now, now.Add(-time.Hour), "u")
	if !errors.Is(err, ErrInvalidRange) {
		t.Errorf("err = %v, want ErrInvalidRange. The database refuses it too, but the caller "+
			"deserves a message naming the problem rather than a constraint violation.", err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	r, mock := repoFor(t)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE "public"."legal_holds"`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM "public"."legal_holds"`)).
		WillReturnRows(holdRow("h1", false))

	if _, err := r.Release(context.Background(), "h1", "u1"); err != nil {
		t.Fatalf("releasing an already-released hold errored: %v.\n"+
			"A retried call must not look like a failure.", err)
	}
}

func TestReleaseOfAMissingHoldIsNotFound(t *testing.T) {
	r, mock := repoFor(t)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE "public"."legal_holds"`)).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if _, err := r.Release(context.Background(), "nope", "u1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestReleaseKeepsTheRow. A hold is the RECORD of a decision; deleting it would
// erase the fact that someone made one.
func TestReleaseKeepsTheRow(t *testing.T) {
	src := readSource(t)
	if regexp.MustCompile(`(?i)DELETE\s+FROM\s+"?public"?\."?legal_holds"?`).MatchString(src) {
		t.Error("this package deletes hold rows. Release must set active = FALSE and keep the row: " +
			"the hold is the record of a decision, and removing it erases the fact that one was made.")
	}
	if !regexp.MustCompile(`(?i)SET\s+active\s*=\s*FALSE`).MatchString(src) {
		t.Error("Release does not deactivate the hold")
	}
}

// TestNewRefusesANilConnection. A repository over no database would accept
// holds that go nowhere.
func TestNewRefusesANilConnection(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Error("New accepted a nil connection")
	}
}

func holdRow(id string, active bool) *sqlmock.Rows {
	now := time.Now()
	return sqlmock.NewRows([]string{
		"id", "name", "reason", "start_date", "end_date", "active",
		"placed_by", "placed_at", "released_by", "released_at",
	}).AddRow(id, "n", "r", now, now.Add(time.Hour), active,
		sql.NullString{}, now, sql.NullString{}, sql.NullTime{})
}

func readSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("legalhold.go")
	if err != nil {
		t.Fatalf("read legalhold.go: %v", err)
	}
	return string(b)
}
