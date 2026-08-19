package api

import (
	"database/sql"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/sethbacon/terraform-suite-identity/identity/pgxparam"
)

// newSQLMock builds a mock that models the driver this module actually runs on.
//
// pgx encodes Go values itself, so an org-scope predicate binds its []string
// directly; sqlmock's default conversion accepts only the fixed driver.Value
// set and rejects the slice on both sides — the bind and the expectation. See
// identity/pgxparam.
func newSQLMock() (*sql.DB, sqlmock.Sqlmock, error) {
	return sqlmock.New(sqlmock.ValueConverterOption(pgxparam.Converter{}))
}

// newSQLMockMatching is newSQLMock for the suites that install their own query
// matcher, usually to record the SQL they see.
func newSQLMockMatching(m sqlmock.QueryMatcher) (*sql.DB, sqlmock.Sqlmock, error) {
	return sqlmock.New(
		sqlmock.QueryMatcherOption(m),
		sqlmock.ValueConverterOption(pgxparam.Converter{}),
	)
}
