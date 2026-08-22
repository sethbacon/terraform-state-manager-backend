package api

// Shared fixture column lists for the two projections that carry an owning
// organization.
//
// They are shared rather than written out per call site because of what a
// sqlmock fixture actually controls: the DAO scans whatever columns the FIXTURE
// declares, so a hand-written list that falls behind the projection does not
// report a stale test -- it reports a scan failure in whichever test happens to
// use it, and the fix looks like "add a column here" rather than "the statement
// changed". One list per projection makes the projection's shape a single fact
// the suite states once (#436).
var (
	apiSourceCols = []string{
		"id", "name", "type", "endpoint", "config", "scope", "encrypted_credentials",
		"created_at", "updated_at", "organization_id",
	}
	apiPipelineCols = []string{
		"id", "name", "provider", "config", "encrypted_token", "created_at", "updated_at",
		"organization_id",
	}
)
