package maintenance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Re-owning the partition roots of #436.
//
// # Why this is a command and not a migration
//
// Every row written before the acting-organization stamp landed sits at the
// deployment's default organization, and tenancy.Backfill cannot repair it: it
// sweeps `WHERE organization_id IS NULL`, and after the first boot there are no
// NULLs left. Some of those rows belong to a different tenant.
//
// WHICH ones is not derivable. The five configuration roots carry created_at and
// nothing else -- no actor, no created_by, no owner -- so there is no provenance
// in the data to compute an answer from. Only an operator knows.
//
// That is precisely why this is not a migration. A migration runs itself, on
// every deployment, with whatever mapping was compiled into it -- so a mapping
// that is right for one estate would be applied unasked to every other one. The
// mapping has to be an INPUT, supplied per deployment, by someone who knows the
// answer for that database.
//
// # What it does not do
//
// It moves rows from one organization to another, wholesale. It cannot express
// "these sources to A and those to B"; an estate that needs a row-level split
// should use Census to see what is there and then write the UPDATE by hand. A
// predicate language here would be a guess at a requirement nobody has stated,
// and it would double the surface of a command that rewrites ownership.

// configRoots are the five partition roots whose owner CANNOT be derived. They
// carry no provenance column, so the mapping supplies their owner.
//
// They are also, not by coincidence, the five that Phase 4 must re-key to
// UNIQUE (organization_id, name).
var configRoots = []string{
	"state_sources",
	"pipeline_connections",
	"ci_sources",
	"schedules",
	"notification_channels",
}

// derivedRoots are the four whose owner IS recoverable, and the expression that
// recovers it. They are updated AFTER the config roots, never before: each one
// reads its parent's organization_id, so running them first would compute every
// child's owner from a parent that has not moved yet -- and the result would be
// indistinguishable from a correct one, with no NULL left to mark it.
var derivedRoots = []struct {
	table  string
	parent string
	// join is the expression that yields the owner, and whether every row has one.
	join           string
	everyRowHasOne bool
}{
	// source_id is NOT NULL ON DELETE CASCADE, so a transfer always has a living
	// parent and every row is recoverable.
	{"state_transfers", "state_sources", "state_sources.id = state_transfers.source_id", true},
	// The rest reference their parent ON DELETE SET NULL, so a row whose parent
	// was deleted has nothing to derive from and is reported rather than guessed.
	{"drift_records", "state_sources", "state_sources.id = drift_records.source_id", false},
	{"drift_runs", "state_sources", "state_sources.id = drift_runs.source_id", false},
	{"health_runs", "pipeline_connections", "pipeline_connections.id = health_runs.pipeline_connection_id", false},
}

// Ownership is one organization's row count in one table.
type Ownership struct {
	Table          string
	OrganizationID string // "" for NULL
	Rows           int64
}

// CensusResult is what verify reports: who owns what, and nothing else.
type CensusResult struct {
	Rows []Ownership
}

func (c CensusResult) String() string {
	if len(c.Rows) == 0 {
		return "no rows in any partition root"
	}
	var b strings.Builder
	for _, r := range c.Rows {
		owner := r.OrganizationID
		if owner == "" {
			owner = "NULL"
		}
		fmt.Fprintf(&b, "\n  %-22s %-38s %d", r.Table, owner, r.Rows)
	}
	return b.String()
}

// Census reports current ownership across all nine partition roots. Read-only.
//
// It is the first thing to run, and on a deployment that has only ever had one
// organization it is also the last: if every row already sits with the single
// organization that owns them, there is nothing to move.
func Census(ctx context.Context, db *sql.DB) (CensusResult, error) {
	var out CensusResult
	if db == nil {
		return out, fmt.Errorf("reown-roots: nil database")
	}
	tables := append([]string{}, configRoots...)
	for _, d := range derivedRoots {
		tables = append(tables, d.table)
	}
	sort.Strings(tables)

	for _, t := range tables {
		// #nosec G202 -- t comes from configRoots/derivedRoots, package-level
		// literals pinned to migration 000033 by test. Never from input, and
		// PostgreSQL has no placeholder form for an identifier.
		rows, err := db.QueryContext(ctx,
			`SELECT COALESCE(organization_id::text, ''), count(*) FROM `+t+
				` GROUP BY organization_id ORDER BY count(*) DESC`)
		if err != nil {
			return out, fmt.Errorf("census %s: %w", t, err)
		}
		for rows.Next() {
			o := Ownership{Table: t}
			if err := rows.Scan(&o.OrganizationID, &o.Rows); err != nil {
				_ = rows.Close()
				return out, fmt.Errorf("census %s: %w", t, err)
			}
			out.Rows = append(out.Rows, o)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return out, fmt.Errorf("census %s: %w", t, err)
		}
		_ = rows.Close()
	}
	return out, nil
}

// MoveResult counts what a move changed, per table.
type MoveResult struct {
	Moved       map[string]int64
	Underivable map[string]int64 // rows whose parent is gone, left alone
	// MovedAwayFromDefault is true when `from` was the deployment's DEFAULT
	// organization, which the move does not repoint.
	//
	// It matters because the default is where things LAND when nothing else
	// decides: a first login not covered by an OIDC or SAML group mapping is
	// placed there, and every partition root's column DEFAULT is
	// tsm_default_organization_id(). So an estate moved away from the default
	// leaves new users -- and anything still relying on the column default --
	// arriving in an organization that now owns nothing.
	//
	// The move does NOT change the setting. Which organization a deployment
	// considers its default is an operator decision with effects beyond these
	// tables, and repointing it while re-owning rows would be two decisions
	// taken under one command.
	MovedAwayFromDefault bool
	// DefaultOrganizationID is the deployment default at the time of the move,
	// reported so the operator can see what it still points at.
	DefaultOrganizationID string
}

func (m MoveResult) String() string {
	if len(m.Moved) == 0 {
		return "nothing to move"
	}
	tables := make([]string, 0, len(m.Moved))
	for t := range m.Moved {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	var b strings.Builder
	for _, t := range tables {
		fmt.Fprintf(&b, "\n  %-22s %d moved", t, m.Moved[t])
		if n := m.Underivable[t]; n > 0 {
			fmt.Fprintf(&b, "  (%d left: parent deleted, owner not recoverable)", n)
		}
	}
	if m.MovedAwayFromDefault {
		fmt.Fprintf(&b, "\n\n  WARNING: the rows moved OUT of this deployment's default organization\n"+
			"  (%s), and that setting is unchanged. New users whose first login is not\n"+
			"  covered by a group mapping still land there, and so does anything still\n"+
			"  relying on the column DEFAULT. Repoint it if the destination is now the\n"+
			"  organization this deployment is really for.", m.DefaultOrganizationID)
	}
	return b.String()
}

// Move re-owns every row currently owned by `from` to `to`.
//
// ONE TRANSACTION. A partial move is worse than no move: the config roots would
// have changed hands while their dependent records still pointed at the old
// owner, and the two are only consistent together.
//
// Both organizations must be named explicitly. There is deliberately no "move
// everything that is not X" and no default for `from`: the whole hazard here is
// moving rows nobody meant to move, and an omitted argument that means "all of
// them" is how that happens.
// DestinationExists reports whether an organization id names a real
// organization. It is supplied by the caller rather than queried here, because
// organizations live in the IDENTITY schema and 000033 deliberately gives the
// partition no foreign key into it -- identity "may be a different database".
// A `SELECT ... FROM organizations` on the application connection is therefore
// not merely bad style; on a split deployment it is a query against a table that
// does not exist.
type DestinationExists func(ctx context.Context, organizationID string) (bool, error)

func Move(ctx context.Context, db *sql.DB, from, to string, exists DestinationExists) (MoveResult, error) {
	res := MoveResult{Moved: map[string]int64{}, Underivable: map[string]int64{}}
	if db == nil {
		return res, fmt.Errorf("reown-roots: nil database")
	}
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	if from == "" || to == "" {
		return res, fmt.Errorf("reown-roots: both --from and --to are required; " +
			"this command will not infer either")
	}
	if from == to {
		return res, fmt.Errorf("reown-roots: --from and --to are the same organization (%s)", from)
	}

	// The destination must exist, and the check is REQUIRED rather than optional.
	// Stamping rows into an organization that names nothing produces well-formed
	// rows that are invisible to every tenant and visible only to a platform
	// admin -- the exact failure #436 exists to close, reached through the tool
	// meant to repair it. A nil checker is refused rather than skipped: "could
	// not check" and "checked and it was fine" must not have the same outcome.
	if exists == nil {
		return res, fmt.Errorf("reown-roots: refusing to move without a way to confirm " +
			"the destination organization exists")
	}
	ok, err := exists(ctx, to)
	if err != nil {
		return res, fmt.Errorf("confirm destination organization %s: %w", to, err)
	}
	if !ok {
		return res, fmt.Errorf("reown-roots: destination organization %s does not exist", to)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback() }()

	// Note whether this move empties the deployment's default organization. Read
	// inside the transaction so the report describes the same instant the move
	// did.
	var currentDefault sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT default_organization_id::text FROM system_settings WHERE id = 1`).Scan(&currentDefault); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return res, fmt.Errorf("read the deployment default organization: %w", err)
	}
	res.DefaultOrganizationID = currentDefault.String
	res.MovedAwayFromDefault = currentDefault.Valid && currentDefault.String == from

	// 1. The config roots, from the operator's mapping.
	for _, t := range configRoots {
		// #nosec G202 -- see Census.
		r, err := tx.ExecContext(ctx,
			`UPDATE `+t+` SET organization_id = $2::uuid WHERE organization_id = $1::uuid`, from, to)
		if err != nil {
			return res, fmt.Errorf("move %s: %w", t, err)
		}
		n, _ := r.RowsAffected()
		res.Moved[t] = n
	}

	// 2. The derived roots, from their parents -- which have just moved.
	for _, d := range derivedRoots {
		// #nosec G202 -- see Census.
		r, err := tx.ExecContext(ctx,
			`UPDATE `+d.table+` SET organization_id = `+d.parent+`.organization_id
			 FROM `+d.parent+`
			 WHERE `+d.join+`
			   AND `+d.table+`.organization_id IS DISTINCT FROM `+d.parent+`.organization_id`)
		if err != nil {
			return res, fmt.Errorf("derive %s: %w", d.table, err)
		}
		n, _ := r.RowsAffected()
		res.Moved[d.table] = n

		if !d.everyRowHasOne {
			// Rows whose parent was deleted still point at `from` and cannot be
			// derived. Counted and left alone rather than swept along: a row with
			// no parent has no owner to recover, and moving it to `to` would be a
			// guess wearing the same clothes as a computed answer.
			// #nosec G202 -- see Census.
			var orphans int64
			if err := tx.QueryRowContext(ctx,
				`SELECT count(*) FROM `+d.table+
					` WHERE organization_id = $1::uuid AND NOT EXISTS (
					     SELECT 1 FROM `+d.parent+` WHERE `+d.join+`)`, from).Scan(&orphans); err != nil {
				return res, fmt.Errorf("count underivable %s: %w", d.table, err)
			}
			res.Underivable[d.table] = orphans
		}
	}

	if err := tx.Commit(); err != nil {
		return res, err
	}
	return res, nil
}
