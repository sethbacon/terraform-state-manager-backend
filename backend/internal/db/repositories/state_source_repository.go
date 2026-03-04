package repositories

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// StateSourceRepository handles database operations for state sources.
type StateSourceRepository struct {
	db *sql.DB
}

// NewStateSourceRepository creates a new StateSourceRepository.
func NewStateSourceRepository(db *sql.DB) *StateSourceRepository {
	return &StateSourceRepository{db: db}
}

// Create inserts a new state source into the database.
func (r *StateSourceRepository) Create(ctx context.Context, source *models.StateSource) error {
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO state_sources (organization_id, name, source_type, config, is_active, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, created_at, updated_at`,
		source.OrganizationID,
		source.Name,
		source.SourceType,
		source.Config,
		source.IsActive,
		source.CreatedBy,
	).Scan(&source.ID, &source.CreatedAt, &source.UpdatedAt)
	if err != nil {
		return fmt.Errorf("failed to create state source: %w", err)
	}
	return nil
}

// GetByID retrieves a state source by its ID.
func (r *StateSourceRepository) GetByID(ctx context.Context, id string) (*models.StateSource, error) {
	var s models.StateSource
	err := r.db.QueryRowContext(ctx,
		`SELECT id, organization_id, name, source_type, config, is_active,
		        last_tested_at, last_test_status, created_by, created_at, updated_at
		 FROM state_sources
		 WHERE id = $1`,
		id,
	).Scan(
		&s.ID,
		&s.OrganizationID,
		&s.Name,
		&s.SourceType,
		&s.Config,
		&s.IsActive,
		&s.LastTestedAt,
		&s.LastTestStatus,
		&s.CreatedBy,
		&s.CreatedAt,
		&s.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get state source by ID: %w", err)
	}
	return &s, nil
}

// Update modifies an existing state source.
func (r *StateSourceRepository) Update(ctx context.Context, source *models.StateSource) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE state_sources
		 SET name = $1, source_type = $2, config = $3, is_active = $4, updated_at = $5
		 WHERE id = $6`,
		source.Name,
		source.SourceType,
		source.Config,
		source.IsActive,
		time.Now(),
		source.ID,
	)
	if err != nil {
		return fmt.Errorf("failed to update state source: %w", err)
	}
	return nil
}

// Delete removes a state source by its ID.
func (r *StateSourceRepository) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM state_sources WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("failed to delete state source: %w", err)
	}
	return nil
}

// ListByOrganization returns paginated state sources for a given organization, along with the total count.
func (r *StateSourceRepository) ListByOrganization(ctx context.Context, orgID string, limit, offset int) ([]models.StateSource, int, error) {
	var total int
	err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM state_sources WHERE organization_id = $1",
		orgID,
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to count state sources: %w", err)
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, organization_id, name, source_type, config, is_active,
		        last_tested_at, last_test_status, created_by, created_at, updated_at
		 FROM state_sources
		 WHERE organization_id = $1
		 ORDER BY name
		 LIMIT $2 OFFSET $3`,
		orgID, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list state sources: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var sources []models.StateSource
	for rows.Next() {
		var s models.StateSource
		if err := rows.Scan(
			&s.ID,
			&s.OrganizationID,
			&s.Name,
			&s.SourceType,
			&s.Config,
			&s.IsActive,
			&s.LastTestedAt,
			&s.LastTestStatus,
			&s.CreatedBy,
			&s.CreatedAt,
			&s.UpdatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("failed to scan state source: %w", err)
		}
		sources = append(sources, s)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("failed to iterate state sources: %w", err)
	}
	return sources, total, nil
}

// UpdateTestStatus records the result of a connectivity test for a state source.
func (r *StateSourceRepository) UpdateTestStatus(ctx context.Context, id string, status string, testedAt time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE state_sources
		 SET last_test_status = $1, last_tested_at = $2, updated_at = $3
		 WHERE id = $4`,
		status, testedAt, time.Now(), id,
	)
	if err != nil {
		return fmt.Errorf("failed to update test status: %w", err)
	}
	return nil
}
