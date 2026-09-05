package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

	// BatchID groups the rows ONE dispatch produced when it fanned out to more
	// than one target (repo-level fan-out dispatch). It stays NULL for a
	// legacy/unfanned run -- schedules.last_run_id must remain a real run id
	// for those, which is why the dispatch handler's DriftBatch.BatchID falls
	// back to the run's own id rather than to this column when there was only
	// one target in the request.
	BatchID *string `json:"batch_id"`
	// CIRunID/CIRunURL identify the CI run the dispatch started. Populated from
	// the dispatch API's OWN response -- never from the callback body, which is
	// input the CI job controls -- and best-effort: the dispatch has already
	// succeeded by the time these are set, so a failure to record them must not
	// fail the HTTP response (see dispatchDriftBatch).
	CIRunID  string `json:"ci_run_id"`
	CIRunURL string `json:"ci_run_url"`

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
	organization_id::text, batch_id::text, COALESCE(ci_run_id,''), COALESCE(ci_run_url,'')`

func scanDrift(scanner interface{ Scan(dest ...any) error }) (*DriftRun, error) {
	var d DriftRun
	var organizationID, batchID sql.NullString
	var connID, srcID sql.NullString
	var added, changed, destroyed sql.NullInt64
	var drifted sql.NullBool
	var summary []byte
	if err := scanner.Scan(&d.ID, &connID, &srcID, &d.StateKey, &d.RepoRef, &d.WorkingDir, &d.Status,
		&added, &changed, &destroyed, &drifted, &summary, &d.Detail, &d.CallbackToken, &d.Actor,
		&d.CreatedAt, &d.UpdatedAt,
		&d.Truncated, &d.OmittedEntries, &d.OmittedAttrs, &d.Unparseable, &d.Unmasked,
		&organizationID, &batchID, &d.CIRunID, &d.CIRunURL); err != nil {
		return nil, err
	}
	if organizationID.Valid {
		d.OrganizationID = organizationID.String
	}
	if batchID.Valid {
		d.BatchID = &batchID.String
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
			(pipeline_connection_id, source_id, state_key, repo_ref, working_dir, status, callback_token, actor, organization_id, batch_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::uuid, $10)
		RETURNING `+driftColumns,
		d.PipelineConnectionID, d.SourceID, nullStr(d.StateKey), nullStr(d.RepoRef), nullStr(d.WorkingDir),
		d.Status, d.CallbackToken, nullStr(d.Actor), organizationID, d.BatchID)
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

// DriftRunFilter narrows List/CountRuns. The zero value matches every run.
//
// BatchID matches EITHER a fanned batch's shared batch_id OR a single
// (unfanned) run's own id -- see the BatchID field's comment: a caller filtering
// "give me this batch" does not know in advance whether the schedule that
// produced it fanned out to more than one target, and a single-item dispatch's
// row never gets a batch_id at all.
type DriftRunFilter struct {
	Status   string
	BatchID  string
	SourceID string
	StateKey string
}

// driftRunFilterClause renders f as a `AND …`-prefixed SQL fragment, appending
// its bound values to args (already seeded, e.g. with the scope's org array so
// the scoped and unscoped readers share one builder and one placeholder
// numbering). Returns the fragment and the extended args slice.
func driftRunFilterClause(args []any, f DriftRunFilter) (string, []any) {
	clause := ""
	if f.Status != "" {
		args = append(args, f.Status)
		clause += fmt.Sprintf(" AND status = $%d", len(args)) // #nosec G202 -- placeholder only; value bound via args
	}
	if f.BatchID != "" {
		args = append(args, f.BatchID)
		n := len(args)
		clause += fmt.Sprintf(" AND (batch_id = $%d OR id = $%d)", n, n) // #nosec G202 -- placeholder only; value bound via args
	}
	if f.SourceID != "" {
		args = append(args, f.SourceID)
		clause += fmt.Sprintf(" AND source_id = $%d", len(args)) // #nosec G202 -- placeholder only; value bound via args
	}
	if f.StateKey != "" {
		args = append(args, f.StateKey)
		clause += fmt.Sprintf(" AND state_key = $%d", len(args)) // #nosec G202 -- placeholder only; value bound via args
	}
	return clause, args
}

// List returns drift runs newest-first, windowed by limit/offset for
// server-side pagination. Use CountRuns with the same filter for the total.
func (r *DriftRepository) List(ctx context.Context, limit, offset int, f DriftRunFilter) ([]DriftRun, error) {
	limit, offset = runPage(limit, offset)
	clause, args := driftRunFilterClause(nil, f)
	q := `SELECT ` + driftColumns + ` FROM drift_runs WHERE 1=1` + clause // #nosec G202 -- fixed SQL + placeholder-only clause from driftRunFilterClause; no interpolated values
	args = append(args, limit, offset)
	q += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args)) // #nosec G202 -- placeholder only

	rows, err := r.db.QueryContext(ctx, q, args...) // #nosec G202 -- q is built from fixed SQL + placeholders above
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

// CountRuns returns the run total for the same filter List pages over.
func (r *DriftRepository) CountRuns(ctx context.Context, f DriftRunFilter) (int, error) {
	clause, args := driftRunFilterClause(nil, f)
	var n int
	// #nosec G202 -- clause is fixed SQL with positional placeholders; values bound via args
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM drift_runs WHERE 1=1`+clause, args...).Scan(&n)
	return n, err
}

// CountRunsIn returns the number of runs currently in one of the given
// statuses, with no organization scoping. This backs the scheduler's in-flight
// cap (Phase 2): pacing is a deployment-wide pool limit, not a per-tenant one,
// so it deliberately counts across every organization.
func (r *DriftRepository) CountRunsIn(ctx context.Context, statuses []string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM drift_runs WHERE status = ANY($1)`, statuses).Scan(&n)
	return n, err
}

// UpdateStatus sets the run status and optional detail.
func (r *DriftRepository) UpdateStatus(ctx context.Context, id, status, detail string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE drift_runs SET status=$2, detail=COALESCE(NULLIF($3,''), detail), updated_at=now() WHERE id=$1`,
		id, status, detail)
	return err
}

// SetCIRun stores the CI run id and web link the dispatch API's own response
// named, against every run sharing batchOrRunID -- either every row in a fanned
// batch (batch_id) or the single row of an unfanned run (id), which is why the
// predicate matches both the same way the batch/run filter above does.
//
// Called AFTER the dispatch already succeeded, so its own error is
// best-effort: the caller logs and continues rather than failing the HTTP
// response, because the CI job is already running and un-dispatching it is not
// an option (see dispatchDriftBatch). ci_run_id/ci_run_url therefore stay
// nullable even on an otherwise-successful dispatch.
func (r *DriftRepository) SetCIRun(ctx context.Context, batchOrRunID, ciRunID, ciRunURL string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE drift_runs SET ci_run_id=$2, ci_run_url=$3, updated_at=now() WHERE batch_id=$1 OR id=$1`,
		batchOrRunID, nullStr(ciRunID), nullStr(ciRunURL))
	return err
}

// FailBatch fails every STILL-DISPATCHED run in a fanned batch after the
// dispatch call itself failed (the CI job never started, so none of the
// batch's per-target callbacks can ever land). Scoped to status='dispatched'
// so it cannot overwrite a sibling row a callback has already completed or
// failed by some other path, and clears the callback token like
// ExpireDispatched does, so a stray callback for one of these runs still gets
// the uniform 401 rather than a stale one-shot token that could be replayed.
//
// Only ever called with a REAL batch_id (len(items) > 1): a single-item
// dispatch failure uses UpdateStatus directly, because its run's batch_id is
// NULL and `batch_id=$1` cannot match a NULL by construction.
func (r *DriftRepository) FailBatch(ctx context.Context, batchID, detail string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE drift_runs SET status='failed', detail=$2, callback_token='', updated_at=now()
		WHERE batch_id=$1 AND status='dispatched'`,
		batchID, detail)
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
