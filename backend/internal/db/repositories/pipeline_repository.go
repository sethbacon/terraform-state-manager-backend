package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// PipelineConnection is a CI integration used to dispatch drift/version runs.
type PipelineConnection struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Provider       string         `json:"provider"`
	Config         map[string]any `json:"config"`
	EncryptedToken []byte         `json:"-"`
	CreatedAt      string         `json:"created_at"`
	UpdatedAt      string         `json:"updated_at"`

	// OrganizationID is the owning tenant, carried in memory so a handler can
	// cross-check a connection against the caller's acting organization before
	// dispatching work at it. Never serialized: the tenant boundary is enforced
	// server-side and echoing it back invites a client to try setting it (#436).
	OrganizationID string `json:"-"`
}

// PipelineRepository is the DAO for pipeline_connections.
type PipelineRepository struct {
	db *sql.DB
}

func NewPipelineRepository(db *sql.DB) *PipelineRepository {
	return &PipelineRepository{db: db}
}

const pipelineColumns = `id, name, provider, config, encrypted_token, created_at::text, updated_at::text,
	organization_id::text`

func scanPipeline(scanner interface{ Scan(dest ...any) error }) (*PipelineConnection, error) {
	var p PipelineConnection
	var configJSON []byte
	var organizationID sql.NullString
	if err := scanner.Scan(&p.ID, &p.Name, &p.Provider, &configJSON, &p.EncryptedToken, &p.CreatedAt, &p.UpdatedAt,
		&organizationID); err != nil {
		return nil, err
	}
	if organizationID.Valid {
		p.OrganizationID = organizationID.String
	}
	if len(configJSON) > 0 {
		_ = json.Unmarshal(configJSON, &p.Config)
	}
	return &p, nil
}

func (r *PipelineRepository) List(ctx context.Context) ([]PipelineConnection, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+pipelineColumns+` FROM pipeline_connections ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PipelineConnection{}
	for rows.Next() {
		p, err := scanPipeline(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (r *PipelineRepository) GetByID(ctx context.Context, id string) (*PipelineConnection, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+pipelineColumns+` FROM pipeline_connections WHERE id = $1`, id)
	p, err := scanPipeline(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// Create writes a pipeline connection owned by organizationID.
//
// An empty organization is REFUSED, not omitted: omitting the column falls
// through to migration 000033's DEFAULT, which is indistinguishable from a
// successful stamp and is how every row in the deployment came to belong to the
// default organization (#436). Naming it with an empty value writes NULL, which
// is invisible to every tenant.
//
// A parameter rather than a field on PipelineConnection, matching Source: the
// struct is serialized to API responses and is the argument to Update, whose
// UPDATE deliberately does not touch organization_id.
func (r *PipelineRepository) Create(ctx context.Context, p *PipelineConnection, organizationID string) (*PipelineConnection, error) {
	organizationID = strings.TrimSpace(organizationID)
	if organizationID == "" {
		return nil, ErrNoOrganization
	}
	configJSON, err := json.Marshal(orEmptyMap(p.Config))
	if err != nil {
		return nil, err
	}
	var token any
	if len(p.EncryptedToken) > 0 {
		token = p.EncryptedToken
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO pipeline_connections (name, provider, config, encrypted_token, organization_id)
		VALUES ($1, $2, $3::jsonb, $4, $5::uuid)
		RETURNING `+pipelineColumns,
		p.Name, p.Provider, string(configJSON), token, organizationID)
	return scanPipeline(row)
}

// Update edits a connection's name and config. The provider is immutable. The
// stored token is replaced only when updateToken is true (callers pass false to
// preserve the existing credential). Returns (nil, nil) when no row matches.
func (r *PipelineRepository) Update(ctx context.Context, p *PipelineConnection, updateToken bool) (*PipelineConnection, error) {
	configJSON, err := json.Marshal(orEmptyMap(p.Config))
	if err != nil {
		return nil, err
	}
	var token any
	if len(p.EncryptedToken) > 0 {
		token = p.EncryptedToken
	}
	row := r.db.QueryRowContext(ctx, `
		UPDATE pipeline_connections
		SET name = $2,
		    config = $3::jsonb,
		    encrypted_token = CASE WHEN $4 THEN $5 ELSE encrypted_token END,
		    updated_at = now()
		WHERE id = $1
		RETURNING `+pipelineColumns,
		p.ID, p.Name, string(configJSON), updateToken, token)
	updated, err := scanPipeline(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (r *PipelineRepository) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM pipeline_connections WHERE id = $1`, id)
	return err
}

// ===========================================================================
// THE PHASE 3 READ FLIP FOR pipeline_connections -- #393.
//
// The write side was scoped first (tenant_write_scope.go); these are the reads.
// A pipeline connection row holds an encrypted CI token and names, in config,
// the CI source whose SHARED token stands in when it has none of its own --
// so an unscoped read here is not a listing leak, it is the first hop of the
// execution chain the #393 background-authority decision (option B) exists to
// close: load the connection, resolve its credential, fire a pipeline.
//
// Both the request path (ListPipelines, the dispatch handlers) and the
// background path (the scheduler's dispatch) read through these. The
// background path passes a scope DERIVED from the row that led here -- see
// internal/tenancy.SystemActingIn -- so a schedule in one organization
// reaching a connection in another matches no row and fails closed.
// ===========================================================================

// pipelineOrgPredicate is the organization filter, written once so the two
// scoped readers cannot come to mean different things.
//
// IT EXCLUDES NULL: `NULL = ANY(...)` is NULL, never true, so a row whose
// organization_id was never stamped is invisible to every tenant instead of
// visible to all of them. 000034 made the column NOT NULL, but a database
// restored from an older backup still holds such rows, and this is the layer
// that keeps working when the constraint above it is absent.
const pipelineOrgPredicate = `organization_id = ANY($1::uuid[])`

// ListInScope returns the pipeline connections the scope permits, newest first.
//
// An empty scope reads NOTHING, without a query -- the early return is not an
// optimisation, it is the fail-closed answer stated where a later edit cannot
// change it by accident (see ScheduleRepository.ListInScope).
func (r *PipelineRepository) ListInScope(ctx context.Context, scope tenantscope.Scope) ([]PipelineConnection, error) {
	if scope.Empty() {
		return []PipelineConnection{}, nil
	}
	if scope.PlatformAdmin {
		// The one principal that is genuinely deployment-wide; also the only
		// reader of rows whose organization_id is NULL.
		return r.List(ctx)
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+pipelineColumns+`
		FROM pipeline_connections WHERE `+pipelineOrgPredicate+`
		ORDER BY created_at DESC`, scope.OrgIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PipelineConnection{}
	for rows.Next() {
		p, err := scanPipeline(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// GetByIDInScope returns the connection with the given id when the scope
// permits it, and (nil, nil) otherwise.
//
// A row that exists in another organization is reported EXACTLY as a row that
// does not exist: this is the read that precedes a credential decryption, and
// "that one is not yours" would let a caller enumerate which ids name real
// connections elsewhere in the deployment.
func (r *PipelineRepository) GetByIDInScope(ctx context.Context, id string, scope tenantscope.Scope) (*PipelineConnection, error) {
	if scope.Empty() {
		return nil, nil
	}
	if scope.PlatformAdmin {
		return r.GetByID(ctx, id)
	}
	row := r.db.QueryRowContext(ctx, `SELECT `+pipelineColumns+`
		FROM pipeline_connections WHERE `+pipelineOrgPredicate+` AND id = $2`, scope.OrgIDs, id)
	p, err := scanPipeline(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}
