package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
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
}

// ScheduleRepository is the DAO for schedules.
type ScheduleRepository struct {
	db *sql.DB
}

func NewScheduleRepository(db *sql.DB) *ScheduleRepository {
	return &ScheduleRepository{db: db}
}

const scheduleColumns = `id, name, cron_expr, target_type, target_config, enabled,
	last_run_at::text, next_run_at::text, last_run_id::text, last_status,
	created_at::text, updated_at::text`

func scanSchedule(scanner interface{ Scan(dest ...any) error }) (*Schedule, error) {
	var s Schedule
	var targetConfig []byte
	var lastRunAt, nextRunAt, lastRunID, lastStatus sql.NullString
	if err := scanner.Scan(&s.ID, &s.Name, &s.CronExpr, &s.TargetType, &targetConfig, &s.Enabled,
		&lastRunAt, &nextRunAt, &lastRunID, &lastStatus, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, err
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
func (r *ScheduleRepository) Create(ctx context.Context, s *Schedule, nextRun *time.Time) (*Schedule, error) {
	cfg := s.TargetConfig
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO schedules (name, cron_expr, target_type, target_config, enabled, next_run_at)
		VALUES ($1, $2, $3, $4::jsonb, $5, $6)
		RETURNING `+scheduleColumns,
		s.Name, s.CronExpr, s.TargetType, string(cfg), s.Enabled, nullTime(nextRun))
	return scanSchedule(row)
}

func (r *ScheduleRepository) List(ctx context.Context) ([]Schedule, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+scheduleColumns+` FROM schedules ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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

func (r *ScheduleRepository) GetByID(ctx context.Context, id string) (*Schedule, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM schedules WHERE id = $1`, id)
	s, err := scanSchedule(row)
	if err == sql.ErrNoRows {
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
	if err == sql.ErrNoRows {
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
