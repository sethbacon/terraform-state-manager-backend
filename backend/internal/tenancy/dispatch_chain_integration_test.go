//go:build integration

// THE CROSS-ORGANIZATION CHAIN REFUSAL, against a real PostgreSQL — #393.
//
// The dispatch chain's unit tests are built on sqlmock, and sqlmock CANNOT
// ANSWER THIS QUESTION. It returns the rows the test handed it without ever
// evaluating a WHERE clause, so a scoped reader and an unscoped one look
// identical to it. Proved during review: replacing the derived authority with
// a permissive platform-admin scope -- which makes GetByIDInScope take its
// bypass branch and serve any organization's row -- compiles and leaves every
// dispatch unit test green.
//
// So the claim that a chain crossing organizations "fails closed in SQL" is
// exactly the claim a mock is unable to check. It is checked here instead,
// against the real predicate, with the poisoned row written by direct SQL the
// way a bug or an attacker would produce it -- bypassing the write-side
// invariant, which is the case the read side exists to survive.
package tenancy

import (
	"context"
	"database/sql"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// seedPipelineConnectionInOrg writes one pipeline_connections row owned by
// orgID and returns its id. encrypted_token is populated because it is the
// thing the chain protects: a reader that returned the row without it would
// look like a refusal while being a leak of everything else.
func seedPipelineConnectionInOrg(t *testing.T, db *sql.DB, orgID, name string) string {
	t.Helper()
	var id string
	err := db.QueryRow(`
		INSERT INTO pipeline_connections (name, provider, config, encrypted_token, organization_id)
		VALUES ($1, 'github_actions', '{}'::jsonb, decode('deadbeef','hex'), $2)
		RETURNING id::text`, name, orgID).Scan(&id)
	if err != nil {
		t.Fatalf("seed pipeline_connection in %s: %v", orgID, err)
	}
	return id
}

func TestIntegrationDispatchChainRefusesAConnectionInAnotherOrganization(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// The two organization ids the other suites in this package use; the
	// fixtures they share create the rows.
	orgA, orgB := orgAlpha, orgBeta

	// The poisoned row: B's connection, which A's schedule will name.
	connB := seedPipelineConnectionInOrg(t, db, orgB, "chain-conn-b")

	repo := repositories.NewPipelineRepository(db)

	// A's derived authority is exactly one organization -- what
	// tenancy.SystemActingIn produces for a row owned by A.
	scopeA := tenantscope.Scope{OrgIDs: []string{orgA}}

	got, err := repo.GetByIDInScope(ctx, connB, scopeA)
	if err == nil && got != nil {
		t.Fatalf("the chain served organization B's connection %s under organization A's authority: "+
			"a schedule in A naming a connection in B would decrypt B's CI token. This is the "+
			"confused deputy the derived-scope design exists to close, and the predicate did not close it.",
			connB)
	}

	// The control: the SAME row under its OWN organization's authority must be
	// readable, or the test above would pass on a reader that returns nothing
	// to anybody -- refusal and brokenness look identical from one assertion.
	scopeB := tenantscope.Scope{OrgIDs: []string{orgB}}
	own, err := repo.GetByIDInScope(ctx, connB, scopeB)
	if err != nil || own == nil {
		t.Fatalf("organization B cannot read its OWN connection %s (err=%v): the refusal above is "+
			"not evidence of scoping, only of a reader that returns nothing", connB, err)
	}
}
