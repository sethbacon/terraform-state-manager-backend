// Package repositories - state_module_ref_scope.go is the scoped twin of
// FindConsumers (#439).
//
// state_module_refs carries NO organization_id of its own. Migration 000033
// lists it among the seven tables whose ownership is INHERITED THROUGH
// state_sources (source_id NOT NULL, ON DELETE CASCADE), so the tenant
// predicate has to be expressed as a join, exactly as state_analysis_scope.go
// does for state_analyses.
package repositories

import (
	"context"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// consumerOrgJoin narrows a consumer row to the organizations that own its
// state source.
//
// INNER JOIN, where the unscoped query uses a LEFT JOIN. That difference is
// deliberate and it changes what a parentless row means: unscoped, a ref whose
// source has gone still appears with an empty name; scoped, it cannot appear at
// all, because there is no organization it can be attributed to. "No living
// parent" must mean "no rows" rather than "rows nobody owns" -- the same
// reasoning analysisOrgJoin records.
const consumerOrgJoin = `JOIN state_sources s ON s.id = r.source_id AND s.organization_id = ANY($3::uuid[])`

// FindConsumersInScope is the scoped twin of FindConsumers.
func (r *StateModuleRefRepository) FindConsumersInScope(ctx context.Context, scope tenantscope.Scope, hosts []string, moduleSource string) ([]StateModuleConsumer, error) {
	// An empty scope is a caller who may see nothing, which is not the same as a
	// caller who may see everything. Answer with no rows rather than falling
	// through to the unscoped query.
	if scope.Empty() {
		return []StateModuleConsumer{}, nil
	}
	if scope.PlatformAdmin {
		return r.FindConsumers(ctx, hosts, moduleSource)
	}

	const q = `SELECT r.source_id, COALESCE(s.name, ''), r.state_key, r.module_version, r.observed_at::text
	           FROM state_module_refs r ` + consumerOrgJoin + `
	           WHERE r.registry_host_canon = ANY($1) AND r.module_source = $2
	           ORDER BY r.observed_at DESC`
	rows, err := r.db.QueryContext(ctx, q, hosts, moduleSource, scope.OrgIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []StateModuleConsumer{}
	for rows.Next() {
		var c StateModuleConsumer
		if err := rows.Scan(&c.SourceID, &c.SourceName, &c.StateKey, &c.ModuleVersion, &c.ObservedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
