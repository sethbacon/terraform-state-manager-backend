package repositories

import (
	"context"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// Scoped readers over the INHERITED analysis tables.
//
// # Why these tables need a join and not a column
//
// state_analyses and its siblings carry no organization_id, and migration 000033
// argues at length that they should not: each has source_id NOT NULL ON DELETE
// CASCADE, so the parent's organization IS the row's organization, derivable by
// a join that cannot return NULL. A duplicated column would be a second answer to
// "whose is this row", and the copy is the one that goes stale — in a predicate
// that decides who may read a Terraform state file. state_analysis_history is
// also the largest table in the schema.
//
// That reasoning is right, and it left a gap: the argument for the join was made
// and the join was never written. Nine readers on this repository take no source
// id and no scope, so the four routes that call them never touch a partition
// root and a Phase 3 flip of the ROOT reads does nothing for them (#455).
//
// # These are additive, and turning them on is a separate act
//
// Each is the scoped twin of an existing reader, alongside it rather than
// replacing it, exactly as ListInScope sits beside List. Landing them is safe at
// any time. SERVING them is not: while every row still sits at the default
// organization, a scoped read returns nothing to a member of any other
// organization, so the flip is gated on the estate having been re-owned
// (reown-roots).

// analysisOrgJoin is the ownership edge, written once so every reader below
// derives the owner the same way.
//
// INNER JOIN, not LEFT: a row whose source is gone cannot happen — the foreign
// key is ON DELETE CASCADE — and if it somehow did, an outer join would produce a
// NULL organization that no tenant predicate matches and every platform admin
// sees. The inner join makes "no living parent" mean "no rows" rather than "rows
// nobody owns".
const analysisOrgJoin = `JOIN state_sources s ON s.id = a.source_id AND s.organization_id = ANY($1::uuid[])`

// TotalsInScope is the scoped twin of Totals.
func (r *StateAnalysisRepository) TotalsInScope(ctx context.Context, scope tenantscope.Scope) (*AnalysisTotals, error) {
	if scope.Empty() {
		return &AnalysisTotals{}, nil
	}
	if scope.PlatformAdmin {
		return r.Totals(ctx)
	}
	var t AnalysisTotals
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(a.rum), 0),
		       COALESCE(SUM(a.managed_resources), 0),
		       COALESCE(SUM(a.data_sources), 0),
		       COALESCE(SUM(a.total_resources), 0)
		FROM state_analyses a `+analysisOrgJoin, scope.OrgIDs).
		Scan(&t.States, &t.RUM, &t.ManagedResources, &t.DataSources, &t.TotalResources)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ProviderCountsInScope is the scoped twin of ProviderCounts.
func (r *StateAnalysisRepository) ProviderCountsInScope(ctx context.Context, scope tenantscope.Scope) (map[string]int, error) {
	if scope.Empty() {
		return map[string]int{}, nil
	}
	if scope.PlatformAdmin {
		return r.ProviderCounts(ctx)
	}
	return r.jsonbCounts(ctx,
		`SELECT key, SUM(value::int) FROM state_analyses a `+analysisOrgJoin+
			`, jsonb_each_text(a.providers) GROUP BY key`, scope.OrgIDs)
}

// ResourceTypeCountsInScope is the scoped twin of ResourceTypeCounts.
func (r *StateAnalysisRepository) ResourceTypeCountsInScope(ctx context.Context, scope tenantscope.Scope) (map[string]int, error) {
	if scope.Empty() {
		return map[string]int{}, nil
	}
	if scope.PlatformAdmin {
		return r.ResourceTypeCounts(ctx)
	}
	return r.jsonbCounts(ctx,
		`SELECT key, SUM(value::int) FROM state_analyses a `+analysisOrgJoin+
			`, jsonb_each_text(a.resource_types) GROUP BY key`, scope.OrgIDs)
}

// VersionCountsInScope is the scoped twin of VersionCounts.
func (r *StateAnalysisRepository) VersionCountsInScope(ctx context.Context, scope tenantscope.Scope) (map[string]int, error) {
	if scope.Empty() {
		return map[string]int{}, nil
	}
	if scope.PlatformAdmin {
		return r.VersionCounts(ctx)
	}
	return r.jsonbCounts(ctx, `
		SELECT CASE WHEN a.terraform_version = '' THEN 'unknown' ELSE a.terraform_version END, COUNT(*)
		FROM state_analyses a `+analysisOrgJoin+` GROUP BY 1`, scope.OrgIDs)
}

// StateVersionsInScope is the scoped twin of StateVersions.
//
// The unscoped reader LEFT JOINs state_sources so a state whose source row is
// missing still appears with an empty name. The scoped one cannot: the join is
// what carries the owner, so an outer join would admit exactly the rows with no
// derivable organization. Scoping turns that outer join into an inner one, which
// is a real behaviour difference and the reason this is a separate method rather
// than a predicate spliced into the original.
func (r *StateAnalysisRepository) StateVersionsInScope(ctx context.Context, scope tenantscope.Scope) ([]StateVersionRow, error) {
	if scope.Empty() {
		return []StateVersionRow{}, nil
	}
	if scope.PlatformAdmin {
		return r.StateVersions(ctx)
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT a.source_id, COALESCE(s.name, ''), a.state_key, a.terraform_version, a.rum
		FROM state_analyses a `+analysisOrgJoin+`
		ORDER BY a.terraform_version, s.name, a.state_key`, scope.OrgIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStateVersionRows(rows)
}

// StatesByVersionExactInScope is the scoped twin of StatesByVersionExact.
//
// The window aggregate counts the SCOPED set, so `total` is the caller's match
// count and not the fleet's. Reporting the fleet total beside a scoped page
// would disclose how many states exist elsewhere — a smaller leak than the rows
// themselves, and the same kind.
func (r *StateAnalysisRepository) StatesByVersionExactInScope(ctx context.Context, scope tenantscope.Scope, version string, limit int) ([]StateVersionRow, int, error) {
	if scope.Empty() {
		return []StateVersionRow{}, 0, nil
	}
	if scope.PlatformAdmin {
		return r.StatesByVersionExact(ctx, version, limit)
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT a.source_id, COALESCE(s.name, ''), a.state_key, a.terraform_version, a.rum,
		       COUNT(*) OVER() AS full_count
		FROM state_analyses a `+analysisOrgJoin+`
		WHERE a.terraform_version = $2
		ORDER BY s.name, a.state_key
		LIMIT $3`, scope.OrgIDs, version, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []StateVersionRow{}
	total := 0
	for rows.Next() {
		var v StateVersionRow
		if err := rows.Scan(&v.SourceID, &v.SourceName, &v.StateKey, &v.TerraformVersion, &v.RUM, &total); err != nil {
			return nil, 0, err
		}
		out = append(out, v)
	}
	return out, total, rows.Err()
}

// scanStateVersionRows is shared by the scoped and unscoped version readers so
// the two cannot drift in what they project.
func scanStateVersionRows(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]StateVersionRow, error) {
	var out []StateVersionRow
	for rows.Next() {
		var v StateVersionRow
		if err := rows.Scan(&v.SourceID, &v.SourceName, &v.StateKey, &v.TerraformVersion, &v.RUM); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// FilterStatesInScope is the scoped twin of FilterStates.
func (r *StateAnalysisRepository) FilterStatesInScope(ctx context.Context, scope tenantscope.Scope, f StateQueryFilter) ([]StateRow, error) {
	if scope.Empty() {
		return []StateRow{}, nil
	}
	if scope.PlatformAdmin {
		return r.FilterStates(ctx, f)
	}
	// The organization array is bound FIRST, so buildStateWhere numbers the
	// filter's own placeholders from $2.
	where, args := buildStateWhere(f, []any{scope.OrgIDs})
	q := `
		SELECT a.source_id, COALESCE(s.name, ''), COALESCE(s.type, ''), a.state_key,
		       a.terraform_version, a.serial, a.lineage, a.size,
		       a.rum, a.managed_resources, a.data_sources, a.total_resources,
		       a.providers, a.resource_types, a.analyzed_at::text
		FROM state_analyses a ` + analysisOrgJoin
	q += " " + where + " ORDER BY s.name, a.state_key" // #nosec G202 -- as FilterStates: fixed columns + $N placeholders, values bound
	rows, err := r.db.QueryContext(ctx, q, args...)    // #nosec G202 -- as above
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStateRows(rows)
}

// PreviewStatesWithTotalsInScope is the scoped twin of PreviewStatesWithTotals.
//
// As with StatesByVersionExactInScope, the window aggregate summarises the
// SCOPED set: the totals beside a scoped page must describe that page's
// population, not the fleet's.
func (r *StateAnalysisRepository) PreviewStatesWithTotalsInScope(ctx context.Context, scope tenantscope.Scope, f StateQueryFilter, limit int) ([]StateRow, StatesAggregate, error) {
	if scope.Empty() {
		return []StateRow{}, StatesAggregate{}, nil
	}
	if scope.PlatformAdmin {
		return r.PreviewStatesWithTotals(ctx, f, limit)
	}
	return r.previewStatesWithTotals(ctx, f, limit, []any{scope.OrgIDs}, analysisOrgJoin)
}

// SyncStatusesInScope is the scoped twin of SyncStatuses.
//
// source_sync_status inherits through source_id like the rest, but its shape is
// the other way round: it is the LEFT side of the join, and its state_analyses
// join is an outer one that must stay outer — a source that has synced and found
// no states still has a status row, and turning that into an inner join would
// hide exactly the sources an operator most needs to see.
func (r *StateAnalysisRepository) SyncStatusesInScope(ctx context.Context, scope tenantscope.Scope) (map[string]SourceSyncStatus, error) {
	if scope.Empty() {
		return map[string]SourceSyncStatus{}, nil
	}
	if scope.PlatformAdmin {
		return r.SyncStatuses(ctx)
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT s.source_id,
		       to_char(s.last_sync_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		       s.states_listed, s.read_errors, s.last_error,
		       COUNT(a.source_id)
		FROM source_sync_status s
		JOIN state_sources src ON src.id = s.source_id AND src.organization_id = ANY($1::uuid[])
		LEFT JOIN state_analyses a ON a.source_id = s.source_id
		GROUP BY s.source_id, s.last_sync_at, s.states_listed, s.read_errors, s.last_error`, scope.OrgIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSyncStatuses(rows)
}
