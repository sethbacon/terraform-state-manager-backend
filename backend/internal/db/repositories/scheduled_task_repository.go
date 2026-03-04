package repositories

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// ScheduledTaskRepository handles database operations for scheduled tasks.
type ScheduledTaskRepository struct {
	db *sql.DB
}

// NewScheduledTaskRepository creates a new ScheduledTaskRepository.
func NewScheduledTaskRepository(db *sql.DB) *ScheduledTaskRepository {
	return &ScheduledTaskRepository{db: db}
}

// Create inserts a new scheduled task into the database.
func (r *ScheduledTaskRepository) Create(ctx context.Context, task *models.ScheduledTask) error {
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO scheduled_tasks (organization_id, name, task_type, schedule, config, is_active, next_run_at, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING id, created_at, updated_at`,
		task.OrganizationID,
		task.Name,
		task.TaskType,
		task.Schedule,
		task.Config,
		task.IsActive,
		task.NextRunAt,
		task.CreatedBy,
	).Scan(&task.ID, &task.CreatedAt, &task.UpdatedAt)
	if err != nil {
		return fmt.Errorf("failed to create scheduled task: %w", err)
	}
	return nil
}

// GetByID retrieves a scheduled task by its ID.
func (r *ScheduledTaskRepository) GetByID(ctx context.Context, id string) (*models.ScheduledTask, error) {
	var task models.ScheduledTask
	err := r.db.QueryRowContext(ctx,
		`SELECT id, organization_id, name, task_type, schedule, config,
		        is_active, last_run_at, next_run_at, last_run_status,
		        created_by, created_at, updated_at
		 FROM scheduled_tasks
		 WHERE id = $1`,
		id,
	).Scan(
		&task.ID,
		&task.OrganizationID,
		&task.Name,
		&task.TaskType,
		&task.Schedule,
		&task.Config,
		&task.IsActive,
		&task.LastRunAt,
		&task.NextRunAt,
		&task.LastRunStatus,
		&task.CreatedBy,
		&task.CreatedAt,
		&task.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get scheduled task by ID: %w", err)
	}
	return &task, nil
}

// Update modifies an existing scheduled task.
func (r *ScheduledTaskRepository) Update(ctx context.Context, task *models.ScheduledTask) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE scheduled_tasks
		 SET name = $1, task_type = $2, schedule = $3, config = $4,
		     is_active = $5, next_run_at = $6, updated_at = $7
		 WHERE id = $8`,
		task.Name,
		task.TaskType,
		task.Schedule,
		task.Config,
		task.IsActive,
		task.NextRunAt,
		time.Now(),
		task.ID,
	)
	if err != nil {
		return fmt.Errorf("failed to update scheduled task: %w", err)
	}
	return nil
}

// Delete removes a scheduled task by its ID.
func (r *ScheduledTaskRepository) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM scheduled_tasks WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("failed to delete scheduled task: %w", err)
	}
	return nil
}

// ListByOrganization returns paginated scheduled tasks for a given organization, along with the total count.
func (r *ScheduledTaskRepository) ListByOrganization(ctx context.Context, orgID string, limit, offset int) ([]models.ScheduledTask, int, error) {
	var total int
	err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM scheduled_tasks WHERE organization_id = $1",
		orgID,
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to count scheduled tasks: %w", err)
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, organization_id, name, task_type, schedule, config,
		        is_active, last_run_at, next_run_at, last_run_status,
		        created_by, created_at, updated_at
		 FROM scheduled_tasks
		 WHERE organization_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2 OFFSET $3`,
		orgID, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list scheduled tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var tasks []models.ScheduledTask
	for rows.Next() {
		var task models.ScheduledTask
		if err := rows.Scan(
			&task.ID,
			&task.OrganizationID,
			&task.Name,
			&task.TaskType,
			&task.Schedule,
			&task.Config,
			&task.IsActive,
			&task.LastRunAt,
			&task.NextRunAt,
			&task.LastRunStatus,
			&task.CreatedBy,
			&task.CreatedAt,
			&task.UpdatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("failed to scan scheduled task: %w", err)
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("failed to iterate scheduled tasks: %w", err)
	}
	return tasks, total, nil
}

// GetDueTasks returns all active tasks whose next_run_at is at or before the given time.
func (r *ScheduledTaskRepository) GetDueTasks(ctx context.Context, now time.Time) ([]models.ScheduledTask, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, organization_id, name, task_type, schedule, config,
		        is_active, last_run_at, next_run_at, last_run_status,
		        created_by, created_at, updated_at
		 FROM scheduled_tasks
		 WHERE is_active = true AND next_run_at IS NOT NULL AND next_run_at <= $1
		 ORDER BY next_run_at ASC`,
		now,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get due tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var tasks []models.ScheduledTask
	for rows.Next() {
		var task models.ScheduledTask
		if err := rows.Scan(
			&task.ID,
			&task.OrganizationID,
			&task.Name,
			&task.TaskType,
			&task.Schedule,
			&task.Config,
			&task.IsActive,
			&task.LastRunAt,
			&task.NextRunAt,
			&task.LastRunStatus,
			&task.CreatedBy,
			&task.CreatedAt,
			&task.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan due task: %w", err)
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate due tasks: %w", err)
	}
	return tasks, nil
}

// UpdateLastRun updates the last_run_at, last_run_status, and next_run_at fields after execution.
func (r *ScheduledTaskRepository) UpdateLastRun(ctx context.Context, id string, status string, ranAt time.Time, nextRunAt *time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE scheduled_tasks
		 SET last_run_at = $1, last_run_status = $2, next_run_at = $3, updated_at = $4
		 WHERE id = $5`,
		ranAt, status, nextRunAt, time.Now(), id,
	)
	if err != nil {
		return fmt.Errorf("failed to update last run: %w", err)
	}
	return nil
}
