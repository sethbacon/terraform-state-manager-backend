package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// DriftRun tracks a dispatched terraform-plan run and the drift it reports.
type DriftRun struct {
	ID                   string          `json:"id"`
	PipelineConnectionID *string         `json:"pipeline_connection_id"`
	SourceID             *string         `json:"source_id"`
	StateKey             string          `json:"state_key"`
	RepoRef              string          `json:"repo_ref"`
	WorkingDir           string          `json:"working_dir"`
	Status               string          `json:"status"`
	Added                *int            `json:"added"`
	Changed              *int            `json:"changed"`
	Destroyed            *int            `json:"destroyed"`
	Drifted              *bool           `json:"drifted"`
	Summary              json.RawMessage `json:"summary,omitempty"`
	Detail               string          `json:"detail"`
	CallbackToken        string          `json:"-"`
	Actor                string          `json:"actor"`
	CreatedAt            string          `json:"created_at"`
	UpdatedAt            string          `json:"updated_at"`

	// Completeness markers describing what THIS run's check did not do. Stored
	// on the run rather than derived from its record because a clean run writes
	// no record, an unparseable run touches none by design, and re-detection
	// overwrites a record in place — so for per-run history the record is either
	// absent or describes a later check. See migration 000031.
	Completeness
	// OrganizationID is the owning tenant, carried in memory so an alert can be
	// fanned out to THAT organization's notification channels and no others.
	// Never serialized: the boundary is enforced server-side (#459).
	OrganizationID string `json:"-"`
}

// DriftRepository is the DAO for drift_runs.
type DriftRepository struct {
	db *sql.DB
}

func NewDriftRepository(db *sql.DB) *DriftRepository {
	return &DriftRepository{db: db}
}

const driftColumns = `id, pipeline_connection_id, source_id, COALESCE(state_key,''), COALESCE(repo_ref,''),
	COALESCE(working_dir,''), status, added, changed, destroyed, drifted, summary, COALESCE(detail,''),
	callback_token, COALESCE(actor,''), created_at::text, updated_at::text,
	truncated, omitted_entries, omitted_attrs, unparseable, unmasked,
	organization_id::text`

func scanDrift(scanner interface{ Scan(dest ...any) error }) (*DriftRun, error) {
	var d DriftRun
	var organizationID sql.NullString
	var connID, srcID sql.NullString
	var added, changed, destroyed sql.NullInt64
	var drifted sql.NullBool
	var summary []byte
	if err := scanner.Scan(&d.ID, &connID, &srcID, &d.StateKey, &d.RepoRef, &d.WorkingDir, &d.Status,
		&added, &changed, &destroyed, &drifted, &summary, &d.Detail, &d.CallbackToken, &d.Actor,
		&d.CreatedAt, &d.UpdatedAt,
		&d.Truncated, &d.OmittedEntries, &d.OmittedAttrs, &d.Unparseable, &d.Unmasked,
		&organizationID); err != nil {
		return nil, err
	}
	if organizationID.Valid {
		d.OrganizationID = organizationID.String
	}
	if connID.Valid {
		d.PipelineConnectionID = &connID.String
	}
	if srcID.Valid {
		d.SourceID = &srcID.String
	}
	if added.Valid {
		v := int(added.Int64)
		d.Added = &v
	}
	if changed.Valid {
		v := int(changed.Int64)
		d.Changed = &v
	}
	if destroyed.Valid {
		v := int(destroyed.Int64)
		d.Destroyed = &v
	}
	if drifted.Valid {
		v := drifted.Bool
		d.Drifted = &v
	}
	if len(summary) > 0 {
		d.Summary = summary
	}
	return &d, nil
}

// Create records a drift run owned by organizationID.
//
// THIS TABLE CANNOT INHERIT, and 000033 says why: drift_runs' two parents are
// both nullable ON DELETE SET NULL, so an inherited organization would become
// NULL the moment either was deleted — and a NULL organization is unpartitioned,
// i.e. readable by everyone. Its own column is the only answer that survives.
//
// So the organization is supplied by the caller, and there are three of them
// with three different authority stories (#436): a user request resolves the
// acting organization; a schedule fired through a handler uses the SCHEDULE's;
// and the scheduler worker has no request at all and carries the schedule's
// organization in memory through the dispatcher, because a schedule names its
// target only inside target_config JSONB and there is no edge to join back on.
//
// An empty organizationID is refused rather than omitted, as everywhere else in
// #436: omitting falls through to the column DEFAULT and looks like success.
func (r *DriftRepository) Create(ctx context.Context, d *DriftRun, organizationID string) (*DriftRun, error) {
	organizationID = strings.TrimSpace(organizationID)
	if organizationID == "" {
		return nil, ErrNoOrganization
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO drift_runs
			(pipeline_connection_id, source_id, state_key, repo_ref, working_dir, status, callback_token, actor, organization_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::uuid)
		RETURNING `+driftColumns,
		d.PipelineConnectionID, d.SourceID, nullStr(d.StateKey), nullStr(d.RepoRef), nullStr(d.WorkingDir),
		d.Status, d.CallbackToken, nullStr(d.Actor), organizationID)
	return scanDrift(row)
}

func (r *DriftRepository) GetByID(ctx context.Context, id string) (*DriftRun, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+driftColumns+` FROM drift_runs WHERE id = $1`, id)
	d, err := scanDrift(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

// List returns drift runs newest-first, optionally filtered by status, windowed
// by limit/offset for server-side pagination. Use CountRuns with the same status
// for the total.
func (r *DriftRepository) List(ctx context.Context, limit, offset int, status string) ([]DriftRun, error) {
	limit, offset = runPage(limit, offset)
	var (
		rows *sql.Rows
		err  error
	)
	if status != "" {
		rows, err = r.db.QueryContext(ctx, `SELECT `+driftColumns+`
			FROM drift_runs WHERE status = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3`, status, limit, offset)
	} else {
		rows, err = r.db.QueryContext(ctx, `SELECT `+driftColumns+`
			FROM drift_runs ORDER BY created_at DESC LIMIT $1 OFFSET $2`, limit, offset)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DriftRun{}
	for rows.Next() {
		d, err := scanDrift(rows)
		if err != nil {
			return nil, err
		}
		d.CallbackToken = "" // never expose in lists
		out = append(out, *d)
	}
	return out, rows.Err()
}

// CountRuns returns the run total for the same optional status filter as List.
func (r *DriftRepository) CountRuns(ctx context.Context, status string) (int, error) {
	var n int
	var err error
	if status != "" {
		err = r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM drift_runs WHERE status = $1`, status).Scan(&n)
	} else {
		err = r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM drift_runs`).Scan(&n)
	}
	return n, err
}

// UpdateStatus sets the run status and optional detail.
func (r *DriftRepository) UpdateStatus(ctx context.Context, id, status, detail string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE drift_runs SET status=$2, detail=COALESCE(NULLIF($3,''), detail), updated_at=now() WHERE id=$1`,
		id, status, detail)
	return err
}

// ConsumeCallbackToken atomically clears the run's callback token if (and only
// if) it still equals the supplied value, returning true when it did. This makes
// the machine callback one-shot: a replayed callback (same token, second time)
// finds the token already cleared and is rejected.
func (r *DriftRepository) ConsumeCallbackToken(ctx context.Context, id, token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE drift_runs SET callback_token='' WHERE id=$1 AND callback_token=$2`, id, token)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// UpdateResult records the drift outcome reported by the CI job.
//
// The counts say how much drift; marks say whether the run finished looking for
// it. Both are stored, because zero counts alone cannot distinguish a run that
// verified clean from one that never completed its check — and for a clean or
// unparseable run there is no record to derive that from (see migration 000031).
// MarkTruncation runs here for the same reason it runs on the record path, so
// one callback cannot leave the two rows disagreeing about the same check.
func (r *DriftRepository) UpdateResult(ctx context.Context, id, status string, added, changed, destroyed int, drifted bool, summary []byte, detail string, marks Completeness) error {
	var summaryArg any
	if len(summary) > 0 {
		summaryArg = string(summary)
	}
	marks.MarkTruncation()
	_, err := r.db.ExecContext(ctx, `
		UPDATE drift_runs
		SET status=$2, added=$3, changed=$4, destroyed=$5, drifted=$6, summary=$7::jsonb,
		    detail=COALESCE(NULLIF($8,''), detail), updated_at=now(),
		    truncated=$9, omitted_entries=$10, omitted_attrs=$11, unparseable=$12, unmasked=$13
		WHERE id=$1`,
		id, status, added, changed, destroyed, drifted, summaryArg, detail,
		marks.Truncated, marks.OmittedEntries, marks.OmittedAttrs, marks.Unparseable, marks.Unmasked)
	return err
}

// ListExpiredDispatched returns dispatched runs created before cutoff — runs
// whose CI job never posted a result callback (build failed at init/plan, the
// agent crashed, the pipeline was cancelled). created_at is the dispatch time and
// never moves while a run is stuck in "dispatched", so it is the TTL anchor. The
// callback_token is retained (not blanked like List does) because the reconciler
// needs it to expire each run race-safely. Bounded by limit so one sweep cannot
// load an unbounded backlog; a larger backlog drains over subsequent sweeps.
func (r *DriftRepository) ListExpiredDispatched(ctx context.Context, cutoff time.Time, limit int) ([]DriftRun, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+driftColumns+`
		FROM drift_runs WHERE status='dispatched' AND created_at < $1
		ORDER BY created_at ASC LIMIT $2`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DriftRun{}
	for rows.Next() {
		d, err := scanDrift(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// ExpireDispatched atomically fails a dispatched run that never received a
// callback: in one statement it flips status dispatched->failed, records detail,
// and clears the callback token — but only if the run is STILL dispatched and
// still holds the supplied token. This mirrors ConsumeCallbackToken's
// compare-and-clear so expiry is race-safe against a real callback landing
// concurrently: that callback consumes the token first, so this matches zero rows
// and reports false (the caller then skips notifying). Clearing the token also
// makes any later/replayed callback fail the run's uniform 401 token check.
// Returns true only when this call performed the expiry.
func (r *DriftRepository) ExpireDispatched(ctx context.Context, id, token, detail string) (bool, error) {
	if token == "" {
		return false, nil
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE drift_runs SET status='failed', detail=$3, callback_token='', updated_at=now()
		WHERE id=$1 AND status='dispatched' AND callback_token=$2`,
		id, token, detail)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
