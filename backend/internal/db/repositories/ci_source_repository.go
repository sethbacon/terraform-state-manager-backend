package repositories

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// CISource is an org-level CI provider connection (ADO org/project or GitHub
// owner) whose credential is shared by the pipeline connections created from it.
//
// AuthMethod selects the credential: "pat" uses EncryptedToken (a personal
// access token); "app" uses a Microsoft Entra app registration (Azure DevOps:
// TenantID, ClientID, EncryptedClientSecret) or a GitHub App (GithubAppID,
// GithubInstallationID, EncryptedAppPrivateKey) whose access token is minted on
// demand.
type CISource struct {
	ID                     string  `json:"id"`
	Name                   string  `json:"name"`
	Provider               string  `json:"provider"`
	Organization           string  `json:"organization"`
	Project                *string `json:"project,omitempty"`
	AuthMethod             string  `json:"auth_method"`
	EncryptedToken         []byte  `json:"-"`
	TenantID               *string `json:"tenant_id,omitempty"`
	ClientID               *string `json:"client_id,omitempty"`
	EncryptedClientSecret  []byte  `json:"-"`
	GithubAppID            *string `json:"github_app_id,omitempty"`
	GithubInstallationID   *string `json:"github_installation_id,omitempty"`
	EncryptedAppPrivateKey []byte  `json:"-"`
	CreatedAt              string  `json:"created_at"`
	UpdatedAt              string  `json:"updated_at"`

	// OrganizationID is the owning tenant (the identity organization, NOT the
	// `organization` remote coordinate above -- see Create's warning). Carried
	// in memory so the write-side linkage invariant can compare a referencing
	// row's organization against this one. Never serialized, matching
	// PipelineConnection: the boundary is enforced server-side and echoing it
	// back invites a client to try setting it (#436).
	OrganizationID string `json:"-"`
}

// CISourceRepository is the DAO for ci_sources.
type CISourceRepository struct {
	db *sql.DB
}

func NewCISourceRepository(db *sql.DB) *CISourceRepository {
	return &CISourceRepository{db: db}
}

// organization_id is selected alongside the remote coordinates because the
// write-side linkage invariant (#393) needs to know which tenant owns a CI
// source before letting a pipeline connection reference it.
const ciSourceColumns = `id, name, provider, organization, project, auth_method, encrypted_token, tenant_id, client_id, encrypted_client_secret, github_app_id, github_installation_id, encrypted_app_private_key, created_at::text, updated_at::text, organization_id::text`

func scanCISource(scanner interface{ Scan(dest ...any) error }) (*CISource, error) {
	var s CISource
	var organizationID sql.NullString
	if err := scanner.Scan(&s.ID, &s.Name, &s.Provider, &s.Organization, &s.Project,
		&s.AuthMethod, &s.EncryptedToken, &s.TenantID, &s.ClientID, &s.EncryptedClientSecret,
		&s.GithubAppID, &s.GithubInstallationID, &s.EncryptedAppPrivateKey,
		&s.CreatedAt, &s.UpdatedAt, &organizationID); err != nil {
		return nil, err
	}
	if organizationID.Valid {
		s.OrganizationID = organizationID.String
	}
	return &s, nil
}

func (r *CISourceRepository) List(ctx context.Context) ([]CISource, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+ciSourceColumns+` FROM ci_sources ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]CISource, 0)
	for rows.Next() {
		s, err := scanCISource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

func (r *CISourceRepository) GetByID(ctx context.Context, id string) (*CISource, error) {
	s, err := scanCISource(r.db.QueryRowContext(ctx,
		`SELECT `+ciSourceColumns+` FROM ci_sources WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return s, err
}

// Create writes a CI source owned by organizationID.
//
// READ THE COLUMN NAMES BEFORE EDITING THIS STATEMENT. ci_sources already has a
// column called `organization` — the Azure DevOps organization or GitHub owner,
// a remote coordinate STRING that has nothing to do with tenancy. The column
// added here is `organization_id`, a uuid naming an identity organization. Two
// different concepts, one letter apart, in one INSERT; migration 000033 warns
// about exactly this. Stamping the wrong one is a silent no-op for tenancy.
//
// An empty organizationID is refused rather than omitted, as everywhere else in
// #436: omitting falls through to the column DEFAULT and looks like success.
func (r *CISourceRepository) Create(ctx context.Context, s *CISource, organizationID string) (*CISource, error) {
	organizationID = strings.TrimSpace(organizationID)
	if organizationID == "" {
		return nil, ErrNoOrganization
	}
	authMethod := s.AuthMethod
	if authMethod == "" {
		authMethod = "pat"
	}
	return scanCISource(r.db.QueryRowContext(ctx,
		`INSERT INTO ci_sources (name, provider, organization, project, auth_method, encrypted_token, tenant_id, client_id, encrypted_client_secret, github_app_id, github_installation_id, encrypted_app_private_key, organization_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::uuid) RETURNING `+ciSourceColumns,
		s.Name, s.Provider, s.Organization, s.Project, authMethod,
		s.EncryptedToken, s.TenantID, s.ClientID, s.EncryptedClientSecret,
		s.GithubAppID, s.GithubInstallationID, s.EncryptedAppPrivateKey, organizationID))
}

func (r *CISourceRepository) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM ci_sources WHERE id = $1`, id)
	return err
}

// ===========================================================================
// THE PHASE 3 READ FLIP FOR ci_sources -- #393.
//
// A ci_sources row is the highest-value read among the nine roots: it holds an
// ORGANIZATION-LEVEL shared credential (a PAT, an Entra client secret, or a
// GitHub App private key) that every pipeline connection created from it
// dispatches with. Before this flip, resolvePipelineToken loaded it BY ID WITH
// NO SCOPE AT ALL -- the confused-deputy hop the #393 background-authority
// decision names: a connection in organization A whose config pointed at a CI
// source in organization B decrypted B's shared token without anything asking
// whether the chain was allowed to cross.
//
// Both paths now read through these: the request path (discovery, verify, the
// repo-setup wizard) under the caller's resolved scope, and the dispatch chain
// under the scope derived from the row that led there.
// ===========================================================================

// ciSourceOrgPredicate is the organization filter, written once for both scoped
// readers. It excludes NULL for the reason every sibling predicate does: an
// unstamped row (possible only on a pre-000034 restore) is invisible to every
// tenant rather than visible to all of them.
const ciSourceOrgPredicate = `organization_id = ANY($1::uuid[])`

// ListInScope returns the CI sources the scope permits, by name.
func (r *CISourceRepository) ListInScope(ctx context.Context, scope tenantscope.Scope) ([]CISource, error) {
	if scope.Empty() {
		return []CISource{}, nil
	}
	if scope.PlatformAdmin {
		return r.List(ctx)
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+ciSourceColumns+`
		FROM ci_sources WHERE `+ciSourceOrgPredicate+`
		ORDER BY name`, scope.OrgIDs)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]CISource, 0)
	for rows.Next() {
		s, err := scanCISource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// GetByIDInScope returns the CI source with the given id when the scope permits
// it, and (nil, nil) otherwise.
//
// A row that exists in another organization is reported EXACTLY as a row that
// does not exist. This is the read that guards a SHARED credential, so the
// enumeration answer matters even more than usual: which organizations run
// which CI providers is itself reconnaissance.
func (r *CISourceRepository) GetByIDInScope(ctx context.Context, id string, scope tenantscope.Scope) (*CISource, error) {
	if scope.Empty() {
		return nil, nil
	}
	if scope.PlatformAdmin {
		return r.GetByID(ctx, id)
	}
	s, err := scanCISource(r.db.QueryRowContext(ctx,
		`SELECT `+ciSourceColumns+` FROM ci_sources WHERE `+ciSourceOrgPredicate+` AND id = $2`,
		scope.OrgIDs, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}
