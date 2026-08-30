package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// Schedule is a cron-driven recurring task. target_config is opaque to the DAO;
// the runner interprets it per target_type (e.g. drift run parameters).
type Schedule struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	CronExpr     string          `json:"cron_expr"`
	TargetType   string          `json:"target_type"`
	TargetConfig json.RawMessage `json:"target_config"`
	Enabled      bool            `json:"enabled"`
	LastRunAt    *string         `json:"last_run_at"`
	NextRunAt    *string         `json:"next_run_at"`
	LastRunID    *string         `json:"last_run_id"`
	LastStatus   *string         `json:"last_status"`
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
	// OrganizationID is the tenant this schedule belongs to, carried in memory
	// so a run fired from it can be stamped with the same organization. It is
	// NOT serialized: the field exists to cross the dispatcher seam, not to be
	// shown, and putting a tenancy id in an API response is a decision to make
	// deliberately rather than by adding a struct field (#436).
	OrganizationID string `json:"-"`
}

// ScheduleRepository is the DAO for schedules.
type ScheduleRepository struct {
	db *sql.DB
}

func NewScheduleRepository(db *sql.DB) *ScheduleRepository {
	return &ScheduleRepository{db: db}
}

// organization_id is selected because a schedule's organization has to travel
// with it IN MEMORY: a schedule names its target only inside target_config
// JSONB, with no column and no foreign key (000008), so a drift run fired from
// one cannot join back to find out whose it is (#436).
const scheduleColumns = `id, name, cron_expr, target_type, target_config, enabled,
	last_run_at::text, next_run_at::text, last_run_id::text, last_status,
	created_at::text, updated_at::text, organization_id::text`

func scanSchedule(scanner interface{ Scan(dest ...any) error }) (*Schedule, error) {
	var s Schedule
	var targetConfig []byte
	var lastRunAt, nextRunAt, lastRunID, lastStatus, organizationID sql.NullString
	if err := scanner.Scan(&s.ID, &s.Name, &s.CronExpr, &s.TargetType, &targetConfig, &s.Enabled,
		&lastRunAt, &nextRunAt, &lastRunID, &lastStatus, &s.CreatedAt, &s.UpdatedAt,
		&organizationID); err != nil {
		return nil, err
	}
	if organizationID.Valid {
		s.OrganizationID = organizationID.String
	}
	if len(targetConfig) > 0 {
		s.TargetConfig = targetConfig
	}
	if lastRunAt.Valid {
		s.LastRunAt = &lastRunAt.String
	}
	if nextRunAt.Valid {
		s.NextRunAt = &nextRunAt.String
	}
	if lastRunID.Valid {
		s.LastRunID = &lastRunID.String
	}
	if lastStatus.Valid {
		s.LastStatus = &lastStatus.String
	}
	return &s, nil
}

// Create inserts a schedule. nextRun (may be nil) seeds next_run_at so the runner
// fires it at the right time.
// Create writes a schedule owned by organizationID.
//
// schedules CANNOT inherit an organization by join: it references its target
// only inside target_config JSONB, with no column and no foreign key (000008),
// which is why 000033 gave it a column of its own. So this stamp is the only
// thing that will ever say which tenant a schedule belongs to — including for
// the runs it later fires, which carry it forward in memory through the
// dispatcher because there is no edge to join back along.
//
// An empty organizationID is refused rather than omitted (#436).
func (r *ScheduleRepository) Create(ctx context.Context, s *Schedule, nextRun *time.Time, organizationID string) (*Schedule, error) {
	organizationID = strings.TrimSpace(organizationID)
	if organizationID == "" {
		return nil, ErrNoOrganization
	}
	cfg := s.TargetConfig
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO schedules (name, cron_expr, target_type, target_config, enabled, next_run_at, organization_id)
		VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7::uuid)
		RETURNING `+scheduleColumns,
		s.Name, s.CronExpr, s.TargetType, string(cfg), s.Enabled, nullTime(nextRun), organizationID)
	return scanSchedule(row)
}

func (r *ScheduleRepository) List(ctx context.Context) ([]Schedule, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+scheduleColumns+` FROM schedules ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSchedules(rows)
}

func (r *ScheduleRepository) GetByID(ctx context.Context, id string) (*Schedule, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM schedules WHERE id = $1`, id)
	s, err := scanSchedule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

func scanSchedules(rows *sql.Rows) ([]Schedule, error) {
	out := []Schedule{}
	for rows.Next() {
		s, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// ===========================================================================
// THE PHASE 3 READ FLIP FOR schedules -- #393.
//
// The write side of this root was scoped first (tenant_write_scope.go) because a
// scoped write has no re-ownership window. The read side is the other half, and
// until now a caller holding state:read in ANY organization listed every
// organization's schedules -- including target_config, which names the pipeline
// connection a firing dispatches to.
//
// GetByIDInScope matters more than the list. RunSchedule loads a schedule BY ID
// and hands its target_config to the dispatcher, stamped with the SCHEDULE's
// organization: an unscoped load therefore let a caller in one organization fire
// another organization's schedule, on another organization's CI connection,
// decrypting that connection's token to do it. That is an execution path, not a
// disclosure, which is why it is flipped in the same change as the list rather
// than after it.
//
// WHAT IS DELIBERATELY NOT SCOPED HERE: GetDue, ClaimDue, RecordOutcome. The
// background runner (internal/services/scheduler) has no request and therefore
// no principal, and the #393 background-authority decision (option B,
// 2026-08-29) settled the shape precisely: ENUMERATION IS CROSS-ORGANIZATION BY
// DESIGN -- finding due work across every tenant is the system's job -- and
// every per-item load that follows carries a scope DERIVED from the enumerated
// row (tenancy.SystemActingIn), through the same InScope readers the request
// path uses. So GetDue stays unscoped, and nothing loaded FROM a due schedule
// does. It carries each schedule's organization forward in memory -- see
// Create's comment on why there is no edge to join back along.
// ===========================================================================

// scheduleOrgPredicate is the organization filter, written once so the two
// scoped readers below cannot come to mean different things. That is the failure
// mode that matters here: a predicate that has drifted on ONE of them still
// passes every test written against the other.
//
// `= ANY($n::uuid[])` rather than a generated IN list: one parameter, so the
// plan is stable whatever the caller's organization count, and no string
// concatenation reaches the query at all.
//
// IT EXCLUDES NULL, and deliberately. `NULL = ANY(...)` is NULL, never true, so
// a row whose organization_id was never stamped is invisible to every tenant
// instead of visible to all of them. Migration 000034 made the column NOT NULL,
// so this schema can no longer produce such a row -- but a database restored
// from a backup taken before it still holds them, and this is the layer that has
// to keep working when the constraint above it is absent.
const scheduleOrgPredicate = `organization_id = ANY($1::uuid[])`

// ListInScope returns the schedules the scope permits, newest first.
//
// An empty scope reads NOTHING, and does so WITHOUT A QUERY. That is the
// fail-closed direction: a caller whose tenancy could not be established, or who
// holds the required scope in no organization, selects no rows rather than every
// row. The early return is not an optimisation -- `= ANY('{}')` returns the same
// empty set -- it is here so the "reads nothing" answer does not depend on a
// Postgres subtlety a later edit could change by accident.
func (r *ScheduleRepository) ListInScope(ctx context.Context, scope tenantscope.Scope) ([]Schedule, error) {
	if scope.Empty() {
		return []Schedule{}, nil
	}
	if scope.PlatformAdmin {
		// The one principal that is genuinely deployment-wide. It also reaches
		// rows whose organization_id is NULL, which the predicate below cannot
		// match and no tenant should see.
		return r.List(ctx)
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+scheduleColumns+`
		FROM schedules WHERE `+scheduleOrgPredicate+`
		ORDER BY created_at DESC`, scope.OrgIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSchedules(rows)
}

// GetByIDInScope returns the schedule with the given id when the scope permits
// it, and (nil, nil) otherwise.
//
// A row that exists but belongs to another organization is reported EXACTLY as a
// row that does not exist, and the caller must not be able to tell the two
// apart: answering "that one is not yours" would let them enumerate ids and
// learn which of them name real schedules somewhere in the deployment. 404 is
// the whole answer, which is also what the already-scoped UpdateInScope and
// DeleteInScope on this table return.
func (r *ScheduleRepository) GetByIDInScope(ctx context.Context, id string, scope tenantscope.Scope) (*Schedule, error) {
	if scope.Empty() {
		return nil, nil
	}
	if scope.PlatformAdmin {
		return r.GetByID(ctx, id)
	}
	// The organization array is $1 and the id is $2, so both readers share one
	// predicate string rather than keeping two copies that agree only as long as
	// somebody remembers they must.
	row := r.db.QueryRowContext(ctx, `SELECT `+scheduleColumns+`
		FROM schedules WHERE `+scheduleOrgPredicate+` AND id = $2`, scope.OrgIDs, id)
	s, err := scanSchedule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Update replaces the mutable fields and resets next_run_at (recomputed by the
// caller from the new cron/enabled state).
func (r *ScheduleRepository) Update(ctx context.Context, id string, s *Schedule, nextRun *time.Time) (*Schedule, error) {
	cfg := s.TargetConfig
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}
	row := r.db.QueryRowContext(ctx, `
		UPDATE schedules
		SET name=$2, cron_expr=$3, target_type=$4, target_config=$5::jsonb, enabled=$6,
		    next_run_at=$7, updated_at=now()
		WHERE id=$1
		RETURNING `+scheduleColumns,
		id, s.Name, s.CronExpr, s.TargetType, string(cfg), s.Enabled, nullTime(nextRun))
	sc, err := scanSchedule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sc, nil
}

func (r *ScheduleRepository) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM schedules WHERE id = $1`, id)
	return err
}

// GetDue returns enabled schedules whose next_run_at is at or before now.
func (r *ScheduleRepository) GetDue(ctx context.Context, now time.Time) ([]Schedule, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+scheduleColumns+`
		FROM schedules WHERE enabled AND next_run_at IS NOT NULL AND next_run_at <= $1
		ORDER BY next_run_at`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSchedules(rows)
}

// RecordRun stamps the outcome of a fired schedule and schedules the next fire.
// Used by the manual run-now path; the background scheduler claims first via
// ClaimDue and stamps the outcome with RecordOutcome.
func (r *ScheduleRepository) RecordRun(ctx context.Context, id, status string, runID *string, ranAt time.Time, nextRun *time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE schedules
		SET last_run_at=$2, last_status=$3, last_run_id=$4, next_run_at=$5, updated_at=now()
		WHERE id=$1`,
		id, ranAt, status, nullStr(deref(runID)), nullTime(nextRun))
	return err
}

// ClaimDue atomically advances a due schedule from its observed next_run_at to
// nextRun, stamping last_run_at. False (no error) means no row matched — a
// concurrent poll or another replica already claimed this firing, or the
// schedule was edited/disabled in the gap — and the caller must not dispatch.
// Advancing next_run_at BEFORE dispatch makes a firing at-most-once: a dispatch
// or outcome-write failure can no longer leave the schedule due and re-fire it.
func (r *ScheduleRepository) ClaimDue(ctx context.Context, id, observedNextRun string, ranAt time.Time, nextRun *time.Time) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE schedules
		SET last_run_at=$3, next_run_at=$4, updated_at=now()
		WHERE id=$1 AND enabled AND next_run_at::text=$2`,
		id, observedNextRun, ranAt, nullTime(nextRun))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RecordOutcome stamps the result of a dispatched firing (status + run id).
// next_run_at is deliberately untouched: it advanced at claim time (ClaimDue).
func (r *ScheduleRepository) RecordOutcome(ctx context.Context, id, status string, runID *string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE schedules SET last_status=$2, last_run_id=$3, updated_at=now() WHERE id=$1`,
		id, status, nullStr(deref(runID)))
	return err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}
