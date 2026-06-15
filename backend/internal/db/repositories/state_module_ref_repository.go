package repositories

import (
	"context"
	"database/sql"

	"github.com/lib/pq"
)

// StateModuleRef is one captured module dependency of a state (migration 000015).
// ModuleVersion is nil when only a version constraint is known (no lockfile).
type StateModuleRef struct {
	SourceID      string  `json:"source_id"`
	StateKey      string  `json:"state_key"`
	ModuleSource  string  `json:"module_source"`
	ModuleVersion *string `json:"module_version"`
	RegistryHost  string  `json:"registry_host"`
	ObservedAt    string  `json:"observed_at"`
}

// StateModuleConsumer is one state that consumes a given registry module,
// returned by the cross-app /consumers query.
type StateModuleConsumer struct {
	SourceID      string  `json:"source_id"`
	SourceName    string  `json:"source_name"`
	StateKey      string  `json:"state_key"`
	ModuleVersion *string `json:"module_version"`
	ObservedAt    string  `json:"observed_at"`
}

// StateModuleRefRepository is the DAO for the state_module_refs table.
type StateModuleRefRepository struct {
	db *sql.DB
}

// NewStateModuleRefRepository creates the repository over the app (public) connection.
func NewStateModuleRefRepository(db *sql.DB) *StateModuleRefRepository {
	return &StateModuleRefRepository{db: db}
}

// ReplaceForState replaces all captured module refs for one (source, state) with
// the given set, atomically. An empty slice clears the state's provenance. A
// state may legitimately call the same module twice, so there is no surrogate
// unique key — the whole set is rewritten on each ingest.
func (r *StateModuleRefRepository) ReplaceForState(ctx context.Context, sourceID, stateKey string, refs []StateModuleRef) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM state_module_refs WHERE source_id = $1 AND state_key = $2`, sourceID, stateKey); err != nil {
		return err
	}
	const ins = `INSERT INTO state_module_refs (source_id, state_key, module_source, module_version, registry_host)
	             VALUES ($1, $2, $3, $4, $5)`
	for _, m := range refs {
		if _, err := tx.ExecContext(ctx, ins, sourceID, stateKey, m.ModuleSource, m.ModuleVersion, m.RegistryHost); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListBySource returns captured module refs for a source, optionally narrowed to
// a single state key (pass "" for all states of the source).
func (r *StateModuleRefRepository) ListBySource(ctx context.Context, sourceID, stateKey string) ([]StateModuleRef, error) {
	q := `SELECT source_id, state_key, module_source, module_version, registry_host, observed_at::text
	      FROM state_module_refs WHERE source_id = $1`
	args := []any{sourceID}
	if stateKey != "" {
		q += ` AND state_key = $2`
		args = append(args, stateKey)
	}
	q += ` ORDER BY module_source`

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []StateModuleRef{}
	for rows.Next() {
		var m StateModuleRef
		if err := rows.Scan(&m.SourceID, &m.StateKey, &m.ModuleSource, &m.ModuleVersion, &m.RegistryHost, &m.ObservedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// FindConsumers returns the states that consume a given registry module, matched
// on (registry_host_canon, module_source) — the honesty-guarded join key, so a
// local module named like a public one never produces a false "consumed by"
// result. hosts is the set of acceptable canonical host identities for the
// calling registry (its public host plus any operator-configured aliases); a
// row matches if its canonical host is any of them. Matching the generated
// registry_host_canon column (not raw registry_host) also rescues rows captured
// before host canonicalization without a backfill.
func (r *StateModuleRefRepository) FindConsumers(ctx context.Context, hosts []string, moduleSource string) ([]StateModuleConsumer, error) {
	const q = `SELECT r.source_id, COALESCE(s.name, ''), r.state_key, r.module_version, r.observed_at::text
	           FROM state_module_refs r
	           LEFT JOIN state_sources s ON s.id = r.source_id
	           WHERE r.registry_host_canon = ANY($1) AND r.module_source = $2
	           ORDER BY r.observed_at DESC`
	rows, err := r.db.QueryContext(ctx, q, pq.Array(hosts), moduleSource)
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
