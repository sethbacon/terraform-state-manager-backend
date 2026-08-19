package approles

import (
	"database/sql"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/sethbacon/terraform-suite-identity/identity/pgxparam"
)

// newSQLMock builds a mock that models the driver this module actually runs on.
// See identity/pgxparam: pgx binds a []string directly, and sqlmock's default
// conversion rejects it on both the bind and the expectation.
func newSQLMock() (*sql.DB, sqlmock.Sqlmock, error) {
	return sqlmock.New(sqlmock.ValueConverterOption(pgxparam.Converter{}))
}

// newSQLMockRegexp is newSQLMock for the suites that match queries by regexp.
func newSQLMockRegexp() (*sql.DB, sqlmock.Sqlmock, error) {
	return sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
		sqlmock.ValueConverterOption(pgxparam.Converter{}),
	)
}
