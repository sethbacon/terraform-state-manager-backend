// Package repositories implements the data-access layer for the state manager's
// own (public-schema) tables. Identity data uses the shared identity module's
// repositories instead.
package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// Source is a configured connection to a backend where Terraform state lives.
// Credentials (when needed) are stored encrypted separately; Config/Scope hold
// non-secret settings.
type Source struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Type     string         `json:"type"`
	Endpoint string         `json:"endpoint"`
	Config   map[string]any `json:"config"`
	Scope    map[string]any `json:"scope"`
	// EncryptedCredentials holds the AES-GCM-sealed secret blob (never serialized
	// to API responses). Empty when the source needs no credentials.
	EncryptedCredentials []byte `json:"-"`
	CreatedAt            string `json:"created_at"`
	UpdatedAt            string `json:"updated_at"`

	// OrganizationID is the owning tenant, carried in memory so a handler that
	// holds TWO sources -- a transfer names two endpoints -- can check the
	// caller against both. Never serialized (#436).
	OrganizationID string `json:"-"`
}

// SourceRepository is the DAO for the state_sources table.
type SourceRepository struct {
	db *sql.DB
}

// NewSourceRepository creates a SourceRepository over the app (public) connection.
func NewSourceRepository(db *sql.DB) *SourceRepository {
	return &SourceRepository{db: db}
}

const sourceColumns = `id, name, type, COALESCE(endpoint, ''), config, scope, encrypted_credentials, created_at::text, updated_at::text,
	organization_id::text`

func scanSource(scanner interface {
	Scan(dest ...any) error
}) (*Source, error) {
	var s Source
	var configJSON, scopeJSON []byte
	var organizationID sql.NullString
	if err := scanner.Scan(&s.ID, &s.Name, &s.Type, &s.Endpoint, &configJSON, &scopeJSON, &s.EncryptedCredentials, &s.CreatedAt, &s.UpdatedAt,
		&organizationID); err != nil {
		return nil, err
	}
	if organizationID.Valid {
		s.OrganizationID = organizationID.String
	}
	if len(configJSON) > 0 {
		_ = json.Unmarshal(configJSON, &s.Config)
	}
	if len(scopeJSON) > 0 {
		_ = json.Unmarshal(scopeJSON, &s.Scope)
	}
	return &s, nil
}

// List returns all configured sources, newest first. Deliberately unbounded:
// its callers are the statesync reconcile loop and dashboard aggregation, which
// must see EVERY source — capping here would silently stop syncing the fleet
// past the cap. HTTP responses use ListPage instead (#282).
func (r *SourceRepository) List(ctx context.Context) ([]Source, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+sourceColumns+` FROM state_sources ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanSources(rows)
}

// ListPage returns one page of sources, newest first, so an HTTP response can
// never serialize the whole table at once (#282).
func (r *SourceRepository) ListPage(ctx context.Context, limit, offset int) ([]Source, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+sourceColumns+`
		FROM state_sources ORDER BY created_at DESC
		LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanSources(rows)
}

// Count returns the total number of configured sources, so a paginated response
// can report how many exist beyond the page it returned.
func (r *SourceRepository) Count(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM state_sources`).Scan(&n)
	return n, err
}

func scanSources(rows *sql.Rows) ([]Source, error) {
	sources := []Source{}
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		sources = append(sources, *s)
	}
	return sources, rows.Err()
}

// GetByID returns the source with the given id, or (nil, nil) if not found.
func (r *SourceRepository) GetByID(ctx context.Context, id string) (*Source, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+sourceColumns+` FROM state_sources WHERE id = $1`, id)
	s, err := scanSource(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Create inserts a source and returns it with its generated id/timestamps.
// Create writes a source owned by organizationID.
//
// THE ORGANIZATION IS A PARAMETER, NOT A FIELD ON Source, and the distinction is
// load-bearing. Source is JSON-serialized straight to API responses, and it is
// also the argument to Update — whose UPDATE deliberately does not touch
// organization_id, because a source does not change hands. A field would make
// the column look settable on that path too, and the mistake it invites is a
// silent re-parent of a row that already has an owner.
//
// AN EMPTY organizationID IS REFUSED rather than omitted from the INSERT, and
// this is the whole point of #436. A Postgres column DEFAULT applies only when
// the column is OMITTED, so leaving it out falls through to
// tsm_default_organization_id() — which is indistinguishable from a successful
// stamp, and is exactly how every row in the deployment came to belong to the
// default organization. Naming the column with an empty value would be worse
// still: it writes NULL, and a NULL organization is invisible to every tenant.
//
// So there is no way to reach this function without saying who owns the row,
// and getting it wrong is a refusal rather than a quiet default.
func (r *SourceRepository) Create(ctx context.Context, s *Source, organizationID string) (*Source, error) {
	organizationID = strings.TrimSpace(organizationID)
	if organizationID == "" {
		return nil, ErrNoOrganization
	}
	configJSON, err := json.Marshal(orEmptyMap(s.Config))
	if err != nil {
		return nil, err
	}
	scopeJSON, err := json.Marshal(orEmptyMap(s.Scope))
	if err != nil {
		return nil, err
	}
	var endpoint any
	if s.Endpoint != "" {
		endpoint = s.Endpoint
	}
	var creds any
	if len(s.EncryptedCredentials) > 0 {
		creds = s.EncryptedCredentials
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO state_sources (name, type, endpoint, config, scope, encrypted_credentials, organization_id)
		VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6, $7::uuid)
		RETURNING `+sourceColumns,
		s.Name, s.Type, endpoint, string(configJSON), string(scopeJSON), creds, organizationID)
	created, err := scanSource(row)
	if err != nil {
		return nil, fmt.Errorf("failed to create source: %w", err)
	}
	return created, nil
}

// Update replaces a source's name, endpoint, config, and scope. Credentials
// are replaced only when EncryptedCredentials is non-empty (blank keeps the
// stored secret). Type is immutable. Returns (nil, nil) if the id is unknown.
func (r *SourceRepository) Update(ctx context.Context, s *Source) (*Source, error) {
	configJSON, err := json.Marshal(orEmptyMap(s.Config))
	if err != nil {
		return nil, err
	}
	scopeJSON, err := json.Marshal(orEmptyMap(s.Scope))
	if err != nil {
		return nil, err
	}
	var endpoint any
	if s.Endpoint != "" {
		endpoint = s.Endpoint
	}
	row := r.db.QueryRowContext(ctx, `
		UPDATE state_sources SET
			name = $2,
			endpoint = $3,
			config = $4::jsonb,
			scope = $5::jsonb,
			encrypted_credentials = CASE WHEN $6::bytea IS NULL THEN encrypted_credentials ELSE $6 END,
			updated_at = now()
		WHERE id = $1
		RETURNING `+sourceColumns,
		s.ID, s.Name, endpoint, string(configJSON), string(scopeJSON), nullableBytes(s.EncryptedCredentials))
	updated, err := scanSource(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to update source: %w", err)
	}
	return updated, nil
}

func nullableBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// Delete removes a source by id.
func (r *SourceRepository) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM state_sources WHERE id = $1`, id)
	return err
}

func orEmptyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// ===========================================================================
// SCOPED READS — PHASE 2b OF FOUR of
// sethbacon/terraform-state-manager-backend#393.
//
// These sit BESIDE List and GetByID rather than replacing them, and that is the
// phase boundary, not a transitional untidiness. Migration 000033 states the
// rule the whole issue is sequenced around: a change that starts filtering
// before equivalence has been demonstrated is a partial cutover, "which is how a
// deployment ends up half-isolated and nobody can say which half". So Phase 2b
// adds a second reader, proves it returns what the first one returns on a
// deployment with one organization, and serves the first one's answer unchanged.
// PHASE 3 is what moves the callers over and deletes the unscoped variants.
//
// internal/tenancy/isolation_integration_test.go calls List(ctx) and
// GetByID(ctx, id) at those exact signatures ON PURPOSE, so that Phase 3 breaks
// the build and forces its assertions to be inverted. Changing either signature
// here would spring that tripwire in the phase that has not earned it — the
// build would break, someone would invert the assertions, and the executable
// record of the leak would be retired while the leak was still open.
// ===========================================================================

// ListInScope returns the sources the scope permits, newest first.
//
// It is the scoped twin of List and shares its unbounded shape for the same
// reason (#282): its Phase 3 callers are the statesync reconcile loop and
// dashboard aggregation, which must see every source they are entitled to.
//
// An empty scope reads NOTHING, and does so without a query. That is the
// fail-closed direction: a caller whose tenancy could not be established, or who
// holds the required scope in no organization, selects no rows rather than every
// row. The early return is not an optimisation — `= ANY('{}')` would return the
// same empty set — it is here so that the "reads nothing" answer does not depend
// on a Postgres subtlety that a later edit could change by accident.
func (r *SourceRepository) ListInScope(ctx context.Context, scope tenantscope.Scope) ([]Source, error) {
	if scope.Empty() {
		return []Source{}, nil
	}
	if scope.PlatformAdmin {
		// The one principal that is genuinely platform-wide (see
		// tenantscope.Scope.PlatformAdmin). It also sees rows whose
		// organization_id is still NULL — a row written by a replica on the
		// previous build, before the startup backfill stamped it — which the
		// organization predicate below cannot match and no tenant should see.
		return r.List(ctx)
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+sourceColumns+`
		FROM state_sources WHERE `+sourceOrgPredicate+`
		ORDER BY created_at DESC`, scope.OrgIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanSources(rows)
}

// GetByIDInScope returns the source with the given id when the scope permits it,
// and (nil, nil) otherwise.
//
// A row that exists but belongs to another organization is reported EXACTLY as a
// row that does not exist. The caller cannot tell the two apart, and must not be
// able to: internal/tenancy/isolation_integration_test.go already proves that
// state_sources' globally-unique name discloses the existence of another
// organization's source through a constraint error, and answering "403, that one
// is not yours" here would rebuild the same disclosure on the read path — a
// caller could enumerate ids and learn which of them name real sources somewhere
// in the deployment. 404 is the whole answer.
func (r *SourceRepository) GetByIDInScope(ctx context.Context, id string, scope tenantscope.Scope) (*Source, error) {
	if scope.Empty() {
		return nil, nil
	}
	if scope.PlatformAdmin {
		return r.GetByID(ctx, id)
	}
	// The organization array is $1 and the id is $2, so that both readers can
	// share one predicate string rather than keeping two copies that agree only
	// as long as somebody remembers they must.
	row := r.db.QueryRowContext(ctx, `SELECT `+sourceColumns+`
		FROM state_sources WHERE `+sourceOrgPredicate+` AND id = $2`, scope.OrgIDs, id)
	s, err := scanSource(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

// sourceOrgPredicate is the organization filter, written once so the two readers
// above cannot come to mean different things — which is the failure mode that
// matters here, because a predicate that has drifted on ONE of them still passes
// every test written against the other.
//
// `= ANY($n::uuid[])` rather than a generated IN list: one parameter, so the
// plan is stable whatever the caller's organization count, and no string
// concatenation reaches the query at all.
//
// IT EXCLUDES NULL, and deliberately. `NULL = ANY(...)` is NULL, never true, so a
// row whose organization_id has not been stamped is invisible to every tenant.
// That is the same rule tenantscope.Scope.Permits applies to the empty owner and
// for the same reason: on these tables NULL means "no tenant has been asserted",
// not "belongs to everyone", and admitting such rows to everybody would leak
// whichever tenant owns the most unstamped rows.
const sourceOrgPredicate = `organization_id = ANY($1::uuid[])`
