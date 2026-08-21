//go:build integration

// THE LEAK, PROVED RATHER THAN ARGUED — sethbacon/terraform-state-manager-backend#393.
//
// Every previous statement of TSM's cross-tenant exposure has been read off call
// graphs: `SourceRepository.GetByID` takes only an id, no repository names
// organization_id, therefore a caller in one organization can read another's
// data. That reasoning is sound and it has never once been executed. This file
// stands up two organizations against a real PostgreSQL and demonstrates the
// leak end to end, because a security claim nobody has run is a hypothesis.
//
// WHY THESE TESTS PASS TODAY, AND WHY THAT IS NOT A CONTRADICTION.
//
// They assert what the system DOES, not what it should do. A test asserting the
// desired property would fail on every run until Phase 3 lands, and a
// permanently red test is indistinguishable from a broken one — it gets skipped,
// then deleted. So these characterise the gap instead, and they are built to
// STOP COMPILING the moment it is closed:
//
//   - they call SourceRepository.List(ctx) and GetByID(ctx, id) at their CURRENT
//     signatures. Phase 3 gives those readers an organization scope. When it
//     does, this file fails to build, and a build failure cannot be ignored,
//     re-run away, or mistaken for a flake.
//
// That is the tripwire. Whoever lands Phase 3 must come here and invert these
// assertions, which is exactly the moment the desired property becomes testable.
// Until then this file is the executable record of what is broken.
//
// Run with:
//
//	TEST_DATABASE_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable' \
//	  go test -tags integration ./internal/tenancy/...
package tenancy

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// Two organizations that do not exist in identity and do not need to. The
// partition column carries a uuid and has NO foreign key — deliberately, because
// identity may be a separate database entirely — so a plain uuid literal is
// exactly as real to these tables as a bootstrapped organization is.
const (
	orgAlpha = "11111111-1111-4111-8111-111111111111"
	orgBeta  = "22222222-2222-4222-8222-222222222222"
)

// seedSourceInOrg writes one state_sources row owned by orgID and returns its id.
//
// Raw SQL rather than SourceRepository.Create on purpose: the subject under test
// is the READ path, and going through the writer would make the test depend on
// whether the writer stamps the column — a different question, answered by
// TestIntegration_OrganizationPartition_ColumnShape.
func seedSourceInOrg(t *testing.T, db *sql.DB, orgID, name string) string {
	t.Helper()
	var id string
	err := db.QueryRow(
		`INSERT INTO state_sources (name, type, organization_id)
		 VALUES ($1, 'local', $2) RETURNING id`, name, orgID).Scan(&id)
	if err != nil {
		t.Fatalf("seed source %q in org %s: %v", name, orgID, err)
	}
	return id
}

// TestIntegration_CrossTenantRead_IsCurrentlyPossible is the whole argument in
// one function: a row owned by Beta is returned to a reader that has nothing to
// do with Beta, because the reader cannot express an organization at all.
func TestIntegration_CrossTenantRead_IsCurrentlyPossible(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	alphaID := seedSourceInOrg(t, db, orgAlpha, "alpha-production")
	betaID := seedSourceInOrg(t, db, orgBeta, "beta-production")

	// Precondition. If the two rows landed in the same organization the rest of
	// this test proves nothing, so establish the partition is real before
	// concluding anything about crossing it.
	var alphaOrg, betaOrg sql.NullString
	if err := db.QueryRow(`SELECT organization_id FROM state_sources WHERE id = $1`, alphaID).Scan(&alphaOrg); err != nil {
		t.Fatalf("read alpha org: %v", err)
	}
	if err := db.QueryRow(`SELECT organization_id FROM state_sources WHERE id = $1`, betaID).Scan(&betaOrg); err != nil {
		t.Fatalf("read beta org: %v", err)
	}
	if !alphaOrg.Valid || !betaOrg.Valid {
		t.Fatalf("a seeded row has a NULL organization_id (alpha=%v beta=%v); "+
			"the partition column is not carrying the value this test is about",
			alphaOrg, betaOrg)
	}
	if alphaOrg.String == betaOrg.String {
		t.Fatalf("both rows are in organization %s; there is no boundary here to cross", alphaOrg.String)
	}

	repo := repositories.NewSourceRepository(db)

	// THE LEAK, POINT-BLANK.
	//
	// There is no argument to say "on behalf of Alpha". The signature does not
	// admit one, so Beta's row comes back to anybody who knows an id — and ids
	// are not secrets: they are handed out by every list endpoint, which is
	// itself unscoped (see the next test).
	got, err := repo.GetByID(ctx, betaID)
	if err != nil {
		t.Fatalf("GetByID(beta): %v", err)
	}
	if got == nil {
		t.Fatalf("GetByID returned nothing for a row that exists — the premise of this "+
			"test is wrong and #393's exposure analysis needs re-reading (id %s)", betaID)
	}
	if got.Name != "beta-production" {
		t.Fatalf("GetByID(beta) returned %q, want beta-production", got.Name)
	}

	t.Logf("PROVED: state_sources %s belongs to organization %s and was returned "+
		"by SourceRepository.GetByID, which takes (ctx, id) and has no organization "+
		"parameter to refuse it with. /sources/:id/state/* feeds this straight into "+
		"credential decryption.", betaID, betaOrg.String)
}

// TestIntegration_CrossTenantList_ReturnsEveryOrganization covers the axis that
// makes the previous test reachable in practice. GetByID needs an id; List hands
// them out.
func TestIntegration_CrossTenantList_ReturnsEveryOrganization(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	seedSourceInOrg(t, db, orgAlpha, "alpha-listed")
	seedSourceInOrg(t, db, orgBeta, "beta-listed")

	repo := repositories.NewSourceRepository(db)

	all, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	names := make(map[string]bool, len(all))
	for _, s := range all {
		names[s.Name] = true
	}
	if !names["alpha-listed"] || !names["beta-listed"] {
		t.Fatalf("List returned %d rows and did not contain both organizations' sources "+
			"(alpha=%v beta=%v); this test's seeding is wrong, not the system",
			len(all), names["alpha-listed"], names["beta-listed"])
	}

	t.Logf("PROVED: SourceRepository.List(ctx) returned sources from BOTH organizations " +
		"in one call. Its SQL is `SELECT ... FROM state_sources ORDER BY created_at DESC` " +
		"with no WHERE clause at all, so there is no row it would decline to return.")
}

// TestIntegration_SourceNameUniquenessIsGlobal_AndDiscloses is Phase 4's problem,
// provable now.
//
// idx_state_sources_name (000001) is UNIQUE on name alone. Under isolation that
// is wrong twice: one organization's naming choices collide with another's, and
// the error DISCLOSES that a source of that name exists somewhere in the
// deployment — to a caller with no access to it and no way to see it.
//
// Phase 4 replaces this with UNIQUE (organization_id, name). The Phase 1 index
// idx_state_sources_org is deliberately a prefix of that future index.
func TestIntegration_SourceNameUniquenessIsGlobal_AndDiscloses(t *testing.T) {
	db := newTestDB(t)

	seedSourceInOrg(t, db, orgAlpha, "production")

	// Beta now tries to name its own source "production". Different organization,
	// different data, no relationship to Alpha's row whatsoever.
	var id string
	err := db.QueryRow(
		`INSERT INTO state_sources (name, type, organization_id)
		 VALUES ('production', 'local', $1) RETURNING id`, orgBeta).Scan(&id)

	if err == nil {
		t.Fatalf("a second organization created a source named 'production' (id %s) — "+
			"idx_state_sources_name is no longer globally unique, so Phase 4's re-keying "+
			"may already have landed and this test needs inverting", id)
	}
	if !strings.Contains(err.Error(), "idx_state_sources_name") &&
		!strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Fatalf("INSERT failed for an unexpected reason: %v", err)
	}

	t.Logf("PROVED: organization %s cannot create a source named 'production' because "+
		"organization %s already has one, and the rejection tells it so. Error: %v",
		orgBeta, orgAlpha, err)
}
