package repositories

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// ReportRepository handles database operations for reports.
type ReportRepository struct {
	db *sql.DB
}

// NewReportRepository creates a new ReportRepository.
func NewReportRepository(db *sql.DB) *ReportRepository {
	return &ReportRepository{db: db}
}

// Create inserts a new report into the database.
func (r *ReportRepository) Create(ctx context.Context, report *models.Report) error {
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO reports (organization_id, run_id, name, format, storage_path, file_size_bytes, generated_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id, created_at`,
		report.OrganizationID,
		report.RunID,
		report.Name,
		report.Format,
		report.StoragePath,
		report.FileSizeBytes,
		report.GeneratedBy,
	).Scan(&report.ID, &report.CreatedAt)
	if err != nil {
		return fmt.Errorf("failed to create report: %w", err)
	}
	return nil
}

// GetByID retrieves a report by its ID.
func (r *ReportRepository) GetByID(ctx context.Context, id string) (*models.Report, error) {
	var report models.Report
	err := r.db.QueryRowContext(ctx,
		`SELECT id, organization_id, run_id, name, format, storage_path, file_size_bytes, generated_by, created_at
		 FROM reports
		 WHERE id = $1`,
		id,
	).Scan(
		&report.ID,
		&report.OrganizationID,
		&report.RunID,
		&report.Name,
		&report.Format,
		&report.StoragePath,
		&report.FileSizeBytes,
		&report.GeneratedBy,
		&report.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get report by ID: %w", err)
	}
	return &report, nil
}

// ListByOrganization returns paginated reports for a given organization, along with the total count.
func (r *ReportRepository) ListByOrganization(ctx context.Context, orgID string, limit, offset int) ([]models.Report, int, error) {
	var total int
	err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM reports WHERE organization_id = $1",
		orgID,
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to count reports: %w", err)
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, organization_id, run_id, name, format, storage_path, file_size_bytes, generated_by, created_at
		 FROM reports
		 WHERE organization_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2 OFFSET $3`,
		orgID, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list reports: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var reports []models.Report
	for rows.Next() {
		var report models.Report
		if err := rows.Scan(
			&report.ID,
			&report.OrganizationID,
			&report.RunID,
			&report.Name,
			&report.Format,
			&report.StoragePath,
			&report.FileSizeBytes,
			&report.GeneratedBy,
			&report.CreatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("failed to scan report: %w", err)
		}
		reports = append(reports, report)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("failed to iterate reports: %w", err)
	}
	return reports, total, nil
}

// Delete removes a report from the database.
func (r *ReportRepository) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx,
		"DELETE FROM reports WHERE id = $1",
		id,
	)
	if err != nil {
		return fmt.Errorf("failed to delete report: %w", err)
	}
	return nil
}
