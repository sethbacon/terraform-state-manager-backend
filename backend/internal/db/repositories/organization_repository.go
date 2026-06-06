package repositories

import (
	"context"
	"database/sql"

	identitymodels "github.com/sethbacon/terraform-suite-identity/identity/models"
	identitystore "github.com/sethbacon/terraform-suite-identity/identity/store"
)

// OrganizationRepository wraps the shared identity organization repository and
// adds TSM-named convenience methods (plus a total-count list) so existing
// handlers keep their call sites. Identity CRUD and membership operations are
// promoted from the embedded module repository.
type OrganizationRepository struct {
	*identitystore.OrganizationRepository
}

// NewOrganizationRepository constructs an OrganizationRepository over the given connection.
func NewOrganizationRepository(db *sql.DB) *OrganizationRepository {
	return &OrganizationRepository{identitystore.NewOrganizationRepository(db)}
}

// GetOrganizationByID returns an organization by ID (alias for GetByID).
func (r *OrganizationRepository) GetOrganizationByID(ctx context.Context, id string) (*identitymodels.Organization, error) {
	return r.GetByID(ctx, id)
}

// GetOrganizationByName returns an organization by name (alias for GetByName).
func (r *OrganizationRepository) GetOrganizationByName(ctx context.Context, name string) (*identitymodels.Organization, error) {
	return r.GetByName(ctx, name)
}

// UpdateOrganization persists display_name and IdP binding (alias for Update).
// Renames go through Rename since the organization name may be denormalized
// in app-owned tables.
func (r *OrganizationRepository) UpdateOrganization(ctx context.Context, org *identitymodels.Organization) error {
	return r.Update(ctx, org)
}

// DeleteOrganization deletes an organization by ID (alias for Delete).
func (r *OrganizationRepository) DeleteOrganization(ctx context.Context, id string) error {
	return r.Delete(ctx, id)
}

// ListOrganizations returns a page of organizations together with the total count.
func (r *OrganizationRepository) ListOrganizations(ctx context.Context, offset, limit int) ([]*identitymodels.Organization, int, error) {
	orgs, err := r.List(ctx, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	total, err := r.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	return orgs, total, nil
}

// SearchOrganizations searches organizations by name or display name.
func (r *OrganizationRepository) SearchOrganizations(ctx context.Context, query string, limit int) ([]*identitymodels.Organization, error) {
	return r.Search(ctx, query, limit, 0)
}
