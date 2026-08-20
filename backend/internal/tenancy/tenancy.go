// Package tenancy carries the organization partition for TSM's domain tables.
//
// PHASE 1 OF FOUR of #393. Migration 000033 adds a nullable organization_id to
// nine root tables and a column DEFAULT that stamps every future INSERT. This
// package supplies the two things SQL could not: the value that default reads,
// and the backfill for rows that predate the column.
//
// NOTHING HERE READS THE COLUMN AS A PREDICATE. There is no filtering, no scope
// resolution, no per-request organization. That is Phase 2 (dual-read behind a
// flag, equivalence proven), Phase 3 (flip reads) and Phase 4 (NOT NULL,
// per-organization unique indexes, flag dropped). A helper here that filtered
// would be a partial cutover.
//
// WHAT THE COLUMN MEANS AFTER PHASE 1, precisely, because the wrong reading of
// it is dangerous: every row is stamped with the DEFAULT organization and only
// the default organization. It is not yet the writer's organization, because
// nothing in a TSM request knows which organization the caller is acting as —
// session scopes are a union across memberships and internal/approles/reads.go
// discards the membership each came from. So the column is uniform-valued, and
// its purpose in this phase is that it EXISTS, is populated on every row, and is
// therefore something Phase 2 can compare against. Reading it as a tenant
// boundary today would answer "is the default organization yours", which is the
// same defect internal/api/apikeys.go:95-109 already documents for api_keys.
package tenancy

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
)

// PartitionedTables are the tables migration 000033 gave their own
// organization_id, in the order the migration declares them.
//
// THIS LIST IS LOAD-BEARING AND IS GUARDED. It is what the backfill iterates,
// and it must equal the set of tables the migration actually altered.
// TestPartitionedTablesMatchTheMigration parses 000033 and fails if the two
// disagree, in either direction — a table in the migration and not here is a
// table whose historical rows are never backfilled and whose organization_id
// stays NULL until Phase 4 tries to make it NOT NULL and fails; a table here and
// not in the migration is an UPDATE against a column that does not exist, which
// breaks startup.
//
// The seven tables that INHERIT their organization through a NOT NULL
// ON DELETE CASCADE foreign key to state_sources are deliberately absent:
// state_backups, state_edits, state_locks, state_analyses, source_sync_status,
// state_analysis_history, state_module_refs. They have no column to backfill.
// The migration's closing comment explains why duplicating the value onto them
// would be a worse design rather than a safer one.
var PartitionedTables = []string{
	"state_sources",
	"pipeline_connections",
	"ci_sources",
	"notification_channels",
	"schedules",
	"state_transfers",
	"drift_runs",
	"drift_records",
	"health_runs",
}

// Backfill records the default organization on the app connection and stamps
// every row that predates the column.
//
// CALLED AT STARTUP, NOT FROM THE MIGRATION, and the migration's header explains
// the three independent reasons SQL could not do this: the app migration runs
// before the identity migration and before ensureDefaultOrg (cmd/server/main.go
// lines 220, 256, 275), identity may be a separate database entirely, and
// 000032's routing pre-check guarantees identity's tables are NOT reachable
// unqualified from this connection. orgID therefore has to be carried across the
// connection boundary by a caller that holds both — internal/bootstrap.Run.
//
// IDEMPOTENT, AND RE-RUN ON EVERY BOOT ON PURPOSE. During a rolling upgrade a
// replica still on the previous build writes rows with no organization_id, since
// its INSERTs predate the column default being visible to it. Those rows are
// stamped by the next booting replica rather than lingering as NULLs nobody
// looks for. After the first pass each UPDATE matches nothing and costs an index
// probe.
//
// ORDER: the carrier is written FIRST. The column default
// (tsm_default_organization_id) reads it, so until it is set every concurrent
// INSERT is stamped NULL. Writing it before the sweep means the sweep is
// cleaning up a set that has stopped growing; the other order would race, and
// rows inserted between the sweep and the carrier write would keep a NULL that
// nothing in this boot revisits.
func Backfill(ctx context.Context, appDB *sql.DB, orgID string) error {
	if appDB == nil {
		return fmt.Errorf("tenancy backfill: nil application connection")
	}
	if orgID == "" {
		// Refuse rather than write NULLs. An empty id here means the caller
		// could not resolve the default organization, and stamping the estate
		// with nothing would look exactly like a successful backfill.
		return fmt.Errorf("tenancy backfill: refusing to run with an empty default organization id")
	}

	if _, err := appDB.ExecContext(ctx,
		`UPDATE system_settings SET default_organization_id = $1::uuid, updated_at = now() WHERE id = 1`,
		orgID); err != nil {
		return fmt.Errorf("record the default organization on the app connection: %w", err)
	}

	var total int64
	for _, table := range PartitionedTables {
		// The table name is interpolated, and it is safe BY CONSTRUCTION rather
		// than by escaping: it comes from PartitionedTables, a package-level
		// literal of nine constants, and never from input. The guard test pins
		// that set to the migration, so it cannot grow an attacker-supplied
		// entry. Parameters cannot carry an identifier in Postgres, so there is
		// no placeholder form of this statement.
		//
		// #nosec G202 -- the concatenated identifier comes from PartitionedTables, a
		// package-level literal of nine constants pinned to migration 000033 by
		// TestPartitionedTablesMatchTheMigration. It is never derived from input, and
		// Postgres has no placeholder form for an identifier.
		res, err := appDB.ExecContext(ctx,
			`UPDATE `+table+` SET organization_id = $1::uuid WHERE organization_id IS NULL`, orgID)
		if err != nil {
			return fmt.Errorf("backfill %s.organization_id: %w", table, err)
		}
		if n, aErr := res.RowsAffected(); aErr == nil {
			total += n
		}
	}

	if total > 0 {
		slog.Info("organization partition backfilled",
			"organization_id", orgID, "rows", total, "tables", len(PartitionedTables))
	}
	return nil
}
