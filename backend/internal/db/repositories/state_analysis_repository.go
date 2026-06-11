package repositories

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/lib/pq"
)

// StateAnalysis is one analyzed state file in the persistent analysis store.
// The dashboard aggregates these rows instead of re-reading state backends;
// the statesync service keeps them reconciled via VersionMarker diffing.
type StateAnalysis struct {
	SourceID         string         `json:"source_id"`
	StateKey         string         `json:"state_key"`
	VersionMarker    string         `json:"version_marker"`
	Size             int64          `json:"size"`
	TerraformVersion string         `json:"terraform_version"`
	Serial           int64          `json:"serial"`
	Lineage          string         `json:"lineage"`
	RUM              int            `json:"rum"`
	ManagedResources int            `json:"managed_resources"`
	DataSources      int            `json:"data_sources"`
	TotalResources   int            `json:"total_resources"`
	Providers        map[string]int `json:"providers"`
	ResourceTypes    map[string]int `json:"resource_types"`
	AnalyzedAt       string         `json:"analyzed_at"`
}

// SourceSyncStatus is the outcome of a source's most recent sync cycle.
// StatesStored is derived (rows currently in the store for the source).
type SourceSyncStatus struct {
	SourceID     string `json:"source_id"`
	LastSyncAt   string `json:"last_sync_at"`
	StatesListed int    `json:"states_listed"`
	StatesStored int    `json:"states_stored"`
	ReadErrors   int    `json:"read_errors"`
	LastError    string `json:"last_error"`
}

// AnalysisTotals are the store-wide scalar sums shown on the dashboard cards.
type AnalysisTotals struct {
	States           int `json:"states"`
	RUM              int `json:"rum"`
	ManagedResources int `json:"managed_resources"`
	DataSources      int `json:"data_sources"`
	TotalResources   int `json:"total_resources"`
}

// StateAnalysisRepository is the DAO for state_analyses and source_sync_status.
type StateAnalysisRepository struct {
	db *sql.DB
}

// NewStateAnalysisRepository creates the DAO over the app (public) connection.
func NewStateAnalysisRepository(db *sql.DB) *StateAnalysisRepository {
	return &StateAnalysisRepository{db: db}
}

// Upsert inserts or replaces the analysis row for (source, key).
func (r *StateAnalysisRepository) Upsert(ctx context.Context, a *StateAnalysis) error {
	providersJSON, err := json.Marshal(orEmptyIntMap(a.Providers))
	if err != nil {
		return err
	}
	resTypesJSON, err := json.Marshal(orEmptyIntMap(a.ResourceTypes))
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO state_analyses (
			source_id, state_key, version_marker, size, terraform_version, serial, lineage,
			rum, managed_resources, data_sources, total_resources, providers, resource_types, analyzed_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13, now())
		ON CONFLICT (source_id, state_key) DO UPDATE SET
			version_marker = EXCLUDED.version_marker,
			size = EXCLUDED.size,
			terraform_version = EXCLUDED.terraform_version,
			serial = EXCLUDED.serial,
			lineage = EXCLUDED.lineage,
			rum = EXCLUDED.rum,
			managed_resources = EXCLUDED.managed_resources,
			data_sources = EXCLUDED.data_sources,
			total_resources = EXCLUDED.total_resources,
			providers = EXCLUDED.providers,
			resource_types = EXCLUDED.resource_types,
			analyzed_at = now()`,
		a.SourceID, a.StateKey, a.VersionMarker, a.Size, a.TerraformVersion, a.Serial, a.Lineage,
		a.RUM, a.ManagedResources, a.DataSources, a.TotalResources, providersJSON, resTypesJSON)
	return err
}

// Markers returns state_key -> version_marker for a source, for sync diffing.
func (r *StateAnalysisRepository) Markers(ctx context.Context, sourceID string) (map[string]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT state_key, version_marker FROM state_analyses WHERE source_id = $1`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	markers := map[string]string{}
	for rows.Next() {
		var key, marker string
		if err := rows.Scan(&key, &marker); err != nil {
			return nil, err
		}
		markers[key] = marker
	}
	return markers, rows.Err()
}

// Sizes returns state_key -> stored byte size for a source, used to enrich
// connector listings whose backend exposes no size (e.g. HCP workspaces).
func (r *StateAnalysisRepository) Sizes(ctx context.Context, sourceID string) (map[string]int64, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT state_key, size FROM state_analyses WHERE source_id = $1`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sizes := map[string]int64{}
	for rows.Next() {
		var key string
		var size int64
		if err := rows.Scan(&key, &size); err != nil {
			return nil, err
		}
		sizes[key] = size
	}
	return sizes, rows.Err()
}

// DeleteMissing removes rows for states no longer present in the source's
// listing. keep is the full set of currently-listed keys.
func (r *StateAnalysisRepository) DeleteMissing(ctx context.Context, sourceID string, keep []string) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM state_analyses WHERE source_id = $1 AND NOT (state_key = ANY($2))`,
		sourceID, pq.Array(keep))
	return err
}

// Delete removes a single analysis row (state deleted via TSM ops).
func (r *StateAnalysisRepository) Delete(ctx context.Context, sourceID, stateKey string) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM state_analyses WHERE source_id = $1 AND state_key = $2`, sourceID, stateKey)
	return err
}

// Totals returns the store-wide scalar sums.
func (r *StateAnalysisRepository) Totals(ctx context.Context) (*AnalysisTotals, error) {
	var t AnalysisTotals
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(rum), 0),
		       COALESCE(SUM(managed_resources), 0),
		       COALESCE(SUM(data_sources), 0),
		       COALESCE(SUM(total_resources), 0)
		FROM state_analyses`).
		Scan(&t.States, &t.RUM, &t.ManagedResources, &t.DataSources, &t.TotalResources)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ProviderCounts aggregates the per-state provider maps across the store.
func (r *StateAnalysisRepository) ProviderCounts(ctx context.Context) (map[string]int, error) {
	return r.jsonbCounts(ctx,
		`SELECT key, SUM(value::int) FROM state_analyses, jsonb_each_text(providers) GROUP BY key`)
}

// ResourceTypeCounts aggregates the per-state resource-type maps across the store.
func (r *StateAnalysisRepository) ResourceTypeCounts(ctx context.Context) (map[string]int, error) {
	return r.jsonbCounts(ctx,
		`SELECT key, SUM(value::int) FROM state_analyses, jsonb_each_text(resource_types) GROUP BY key`)
}

// VersionCounts returns state counts per Terraform version (” -> "unknown").
func (r *StateAnalysisRepository) VersionCounts(ctx context.Context) (map[string]int, error) {
	return r.jsonbCounts(ctx, `
		SELECT CASE WHEN terraform_version = '' THEN 'unknown' ELSE terraform_version END, COUNT(*)
		FROM state_analyses GROUP BY 1`)
}

func (r *StateAnalysisRepository) jsonbCounts(ctx context.Context, query string) (map[string]int, error) {
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var key string
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			return nil, err
		}
		counts[key] = n
	}
	return counts, rows.Err()
}

// UpsertSyncStatus records the outcome of a source's sync cycle.
func (r *StateAnalysisRepository) UpsertSyncStatus(ctx context.Context, s *SourceSyncStatus) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO source_sync_status (source_id, last_sync_at, states_listed, read_errors, last_error)
		VALUES ($1, now(), $2, $3, $4)
		ON CONFLICT (source_id) DO UPDATE SET
			last_sync_at = now(),
			states_listed = EXCLUDED.states_listed,
			read_errors = EXCLUDED.read_errors,
			last_error = EXCLUDED.last_error`,
		s.SourceID, s.StatesListed, s.ReadErrors, s.LastError)
	return err
}

// SyncStatuses returns the latest sync outcome per source, including the
// number of rows currently stored for it.
func (r *StateAnalysisRepository) SyncStatuses(ctx context.Context) (map[string]SourceSyncStatus, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT s.source_id,
		       to_char(s.last_sync_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		       s.states_listed, s.read_errors, s.last_error,
		       (SELECT COUNT(*) FROM state_analyses a WHERE a.source_id = s.source_id)
		FROM source_sync_status s`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	statuses := map[string]SourceSyncStatus{}
	for rows.Next() {
		var st SourceSyncStatus
		if err := rows.Scan(&st.SourceID, &st.LastSyncAt, &st.StatesListed, &st.ReadErrors, &st.LastError, &st.StatesStored); err != nil {
			return nil, err
		}
		statuses[st.SourceID] = st
	}
	return statuses, rows.Err()
}

func orEmptyIntMap(m map[string]int) map[string]int {
	if m == nil {
		return map[string]int{}
	}
	return m
}
