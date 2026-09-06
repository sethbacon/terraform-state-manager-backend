// Package testsupport holds sqlmock fixtures shared across packages that
// build drift_runs rows in tests -- internal/db/repositories,
// internal/services/driftreconcile and internal/api each used to hand-copy
// the same column list, and a fixture edited in only two of the three places
// is the exact class of bug this package exists to prevent. Nothing in
// cmd/server imports this package; it exists purely for test files to import
// (a normal, non-_test.go package, so it CAN be imported across package
// boundaries -- unlike a _test.go file, which cannot).
package testsupport

import (
	"database/sql/driver"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// DriftRunColumns are the bare column names a drift_runs sqlmock fixture must
// declare, in the exact order internal/db/repositories.scanDrift scans them.
// The ONE definition every drift_runs fixture in this module shares.
//
// Verified against the production driftColumns SQL constant by
// TestDriftRunColumns_MatchesProductionDriftColumns in
// internal/db/repositories (the only package that can see that unexported
// const), so this list and driftColumns cannot silently diverge: a column
// added to one and not the other fails that test with both shapes printed.
var DriftRunColumns = []string{
	"id", "pipeline_connection_id", "source_id", "state_key", "repo_ref", "working_dir",
	"status", "added", "changed", "destroyed", "drifted", "summary", "detail", "callback_token", "actor",
	"created_at", "updated_at", "truncated", "omitted_entries", "omitted_attrs", "unparseable", "unmasked",
	"organization_id", "batch_id", "ci_run_id", "ci_run_url",
	"drift_added", "drift_changed", "drift_destroyed", "drift_summary",
}

// DriftRunRow builds one drift_runs sqlmock row from values given IN
// DriftRunColumns' order. Callers still write a run's fields out
// positionally -- exactly as every fixture already did before this package
// existed -- but compose them onto the ONE shared column list rather than
// re-declaring it locally.
func DriftRunRow(values ...driver.Value) *sqlmock.Rows {
	return sqlmock.NewRows(DriftRunColumns).AddRow(values...)
}

// DriftRecordColumns are the bare column names a drift_records sqlmock
// fixture must declare, in the exact order internal/db/repositories.
// scanDriftRecord scans them. Mirrors DriftRunColumns above for the same
// reason: internal/api and internal/db/repositories each used to hand-copy
// this list (driftRecCols / driftRecordCols) before this package held it,
// which is exactly the divergence class DriftRunColumns was already created
// to prevent for drift_runs.
//
// Verified against the production driftRecordColumns SQL constant by
// TestDriftRecordColumns_MatchesProductionDriftRecordColumns in
// internal/db/repositories (the only package that can see that unexported
// const).
var DriftRecordColumns = []string{
	"id", "source_id", "state_key", "pipeline_connection_id", "last_run_id",
	"origin", "severity", "added", "changed", "destroyed", "summary", "status", "acknowledged_by",
	"acknowledged_at", "ack_note", "resolved_at", "external_ref", "detections", "first_detected_at",
	"last_detected_at", "truncated", "omitted_entries", "omitted_attrs", "unparseable", "unmasked",
	"organization_id",
	"drift_added", "drift_changed", "drift_destroyed", "drift_summary",
}

// DriftRecordRow builds one drift_records sqlmock row from values given IN
// DriftRecordColumns' order, mirroring DriftRunRow.
func DriftRecordRow(values ...driver.Value) *sqlmock.Rows {
	return sqlmock.NewRows(DriftRecordColumns).AddRow(values...)
}
