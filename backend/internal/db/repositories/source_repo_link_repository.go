package repositories

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// SourceRepoLinkRepository handles database operations for source-to-ADO repo
// links. There is at most one link per source, enforced by a UNIQUE (source_id)
// constraint, so Upsert replaces an existing link rather than creating a second.
type SourceRepoLinkRepository struct {
	db *sql.DB
}

// NewSourceRepoLinkRepository creates a new SourceRepoLinkRepository.
func NewSourceRepoLinkRepository(db *sql.DB) *SourceRepoLinkRepository {
	return &SourceRepoLinkRepository{db: db}
}

// Upsert creates the link for a source or replaces the existing one. The
// (source_id) unique constraint drives the ON CONFLICT replacement so set
// operations are idempotent. Generated id and timestamps are populated on link.
func (r *SourceRepoLinkRepository) Upsert(ctx context.Context, link *models.SourceRepoLink) error {
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO source_repo_links (
			organization_id, source_id, ado_organization_url, ado_project,
			ado_repo, ado_pipeline_id, discovery_method
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (source_id)
		 DO UPDATE SET ado_organization_url = EXCLUDED.ado_organization_url,
		               ado_project          = EXCLUDED.ado_project,
		               ado_repo             = EXCLUDED.ado_repo,
		               ado_pipeline_id      = EXCLUDED.ado_pipeline_id,
		               discovery_method     = EXCLUDED.discovery_method,
		               updated_at           = NOW()
		 RETURNING id, created_at, updated_at`,
		link.OrganizationID,
		link.SourceID,
		link.ADOOrganizationURL,
		link.ADOProject,
		link.ADORepo,
		link.ADOPipelineID,
		link.DiscoveryMethod,
	).Scan(&link.ID, &link.CreatedAt, &link.UpdatedAt)
	if err != nil {
		return fmt.Errorf("failed to upsert source repo link: %w", err)
	}
	return nil
}

// GetBySourceID retrieves the repo link for a source, or (nil, nil) if absent.
func (r *SourceRepoLinkRepository) GetBySourceID(ctx context.Context, sourceID string) (*models.SourceRepoLink, error) {
	var l models.SourceRepoLink
	err := r.db.QueryRowContext(ctx,
		`SELECT id, organization_id, source_id, ado_organization_url, ado_project,
		        ado_repo, ado_pipeline_id, discovery_method, created_at, updated_at
		 FROM source_repo_links
		 WHERE source_id = $1`,
		sourceID,
	).Scan(
		&l.ID,
		&l.OrganizationID,
		&l.SourceID,
		&l.ADOOrganizationURL,
		&l.ADOProject,
		&l.ADORepo,
		&l.ADOPipelineID,
		&l.DiscoveryMethod,
		&l.CreatedAt,
		&l.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get source repo link: %w", err)
	}
	return &l, nil
}

// DeleteBySourceID removes the repo link for a source. Deleting a non-existent
// link is a no-op and not an error.
func (r *SourceRepoLinkRepository) DeleteBySourceID(ctx context.Context, sourceID string) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM source_repo_links WHERE source_id = $1", sourceID)
	if err != nil {
		return fmt.Errorf("failed to delete source repo link: %w", err)
	}
	return nil
}
