package api

import (
	"context"
	"errors"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// Ownership checks for the things a dispatch REACHES, written once because the
// last version of this rule was written twice and only one copy was right.
//
// health.CreateRun compared conn.OrganizationID to the acting organization and
// refused with a 404; dispatchDrift, which is the same operation for drift, did
// not — it loaded the connection by id and handed it straight to
// resolvePipelineToken, which DECRYPTS the connection's token or its CI source's
// shared token. So the credential was in memory before anything asked whether
// the caller was entitled to it.
//
// A rule that has to be restated at each call site is a rule that will be missed
// at one of them. This is the single statement of it.

// errNotOwnedHere reports a target the caller's organization does not own. It is
// deliberately the same error the callers already map to "not found": a caller
// outside the owning organization must not be able to tell a target that exists
// elsewhere from one that does not exist at all.
var errNotOwnedHere = errors.New("api: the target belongs to another organization")

// pipelineConnectionFor loads a pipeline connection and refuses one the acting
// organization does not own.
//
// An UNSTAMPED connection (organization_id still NULL, from before the backfill)
// is allowed through: refusing it would break dispatch on exactly the rows #436
// is still repairing, and it carries no claim about ownership either way.
func pipelineConnectionFor(
	ctx context.Context,
	repo *repositories.PipelineRepository,
	id, organizationID string,
) (*repositories.PipelineConnection, error) {
	conn, err := repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if conn == nil {
		return nil, nil
	}
	if conn.OrganizationID != "" && conn.OrganizationID != organizationID {
		return nil, errNotOwnedHere
	}
	return conn, nil
}

// sourceFor is the same rule for a dispatch target's state source. A drift
// target names one, and it decides which source's state a CI job is pointed at.
func sourceFor(
	ctx context.Context,
	repo *repositories.SourceRepository,
	id, organizationID string,
) (*repositories.Source, error) {
	if id == "" {
		return nil, nil
	}
	src, err := repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if src == nil {
		return nil, nil
	}
	if src.OrganizationID != "" && src.OrganizationID != organizationID {
		return nil, errNotOwnedHere
	}
	return src, nil
}
