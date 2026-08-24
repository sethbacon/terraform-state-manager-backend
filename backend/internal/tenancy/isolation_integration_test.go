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
func TestIntegration_SourceNameIsUniquePerOrganization_AndDisclosesNothing(t *testing.T) {
	db := newTestDB(t)

	seedSourceInOrg(t, db, orgAlpha, "production")

	// Beta names its own source "production". Different organization, different
	// data, no relationship to Alpha's row whatsoever — and after 000034 that is
	// simply allowed.
	//
	// INVERTED BY PHASE 4. This test used to assert the opposite and was named
	// ...IsGlobal_AndDiscloses: idx_state_sources_name was UNIQUE on (name)
	// alone, so Beta's INSERT failed, and the failure DISCLOSED that some other
	// organization already held that name. A tenant could enumerate names and
	// learn what its neighbours had called things. 000034 re-keys the index to
	// UNIQUE (organization_id, name), which is what removes the disclosure.
	var id string
	if err := db.QueryRow(
		`INSERT INTO state_sources (name, type, organization_id)
		 VALUES ('production', 'local', $1) RETURNING id`, orgBeta).Scan(&id); err != nil {
		t.Fatalf("a second organization could not name its own source 'production': %v\n"+
			"After 000034 the unique key is (organization_id, name), so this must succeed. "+
			"A failure here means the global index survived the re-key.", err)
	}

	// ...and the name is still unique WITHIN an organization: the re-key must not
	// have simply dropped the constraint.
	if _, err := db.Exec(
		`INSERT INTO state_sources (name, type, organization_id) VALUES ('production', 'local', $1)`,
		orgBeta); err == nil {
		t.Fatal("one organization created two sources named 'production': the re-key dropped " +
			"uniqueness instead of narrowing it")
	}

	t.Logf("PROVED: organizations %s and %s each hold a source named 'production', and neither "+
		"can learn of the other's through a constraint error.", orgAlpha, orgBeta)
}

// tsmPartitionRoots is the nine tables migration 000033 gave their OWN
// organization_id, as opposed to the tables that inherit one by joining up to a
// root. Phase 4 makes every one of these columns NOT NULL, which is also the
// moment their uniqueness constraints stop being defensible, so this list is the
// starting point for the re-keying inventory below.
var tsmPartitionRoots = []string{
	"ci_sources", "drift_records", "drift_runs", "health_runs",
	"notification_channels", "pipeline_connections", "schedules",
	"state_sources", "state_transfers",
}

// globallyUniqueNameIndexes returns every UNIQUE index in the public schema
// whose key is exactly one column called `name`, as table -> index name.
//
// Read out of the catalog rather than transcribed from the migrations on
// purpose. A hand-maintained list is a claim about the schema that stops being
// checked the moment someone adds a table, and that is precisely how the gap
// this test exists to close was created.
func globallyUniqueNameIndexes(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`
		SELECT t.relname, i.relname
		FROM pg_index x
		JOIN pg_class     i ON i.oid = x.indexrelid
		JOIN pg_class     t ON t.oid = x.indrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = x.indkey[0]
		WHERE n.nspname = 'public'
		  AND x.indisunique
		  AND x.indnkeyatts = 1
		  AND a.attname = 'name'`)
	if err != nil {
		t.Fatalf("query unique name indexes: %v", err)
	}
	defer rows.Close()

	found := map[string]string{}
	for rows.Next() {
		var table, index string
		if err := rows.Scan(&table, &index); err != nil {
			t.Fatalf("scan: %v", err)
		}
		found[table] = index
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	// An empty result would mean the query stopped matching anything -- a
	// catalog shape change, a schema rename -- and would make every assertion
	// below pass by describing nothing. A signature that cannot see its subject
	// looks exactly like a subject that is clean.
	if len(found) == 0 {
		t.Fatalf("no globally-unique name index found anywhere in the schema; " +
			"the catalog query has stopped matching and this test is now vacuous")
	}
	return found
}

// TestIntegration_Phase4NameRekeyInventory_IsCompleteAndDerived is the
// generalisation of the test above it, and it exists because that test was one
// instance of a five-instance class.
//
// TestIntegration_SourceNameUniquenessIsGlobal_AndDiscloses proves that two
// organizations cannot both own a source named "production", and that the
// rejection tells the loser so. Nothing about that argument is specific to
// state_sources. FIVE partition roots carry a UNIQUE index on `name` alone:
//
//	state_sources         000001:17   idx_state_sources_name
//	pipeline_connections  000004:14   idx_pipeline_connections_name
//	schedules             000008:18   idx_schedules_name
//	notification_channels 000009:16   idx_notification_channels_name
//	ci_sources            000011:15   idx_ci_sources_name
//
// 000033 walks the roots one by one and calls out the globally-unique name on
// FOUR of them, each with a note that Phase 4 re-keys it. ci_sources is the one
// it does not. That paragraph is spent instead on a genuinely important warning
// -- ci_sources already has a column called `organization`, which is the Azure
// DevOps organization or GitHub owner, a remote coordinate one letter away from
// the tenancy column -- and the uniqueness fact drops out.
//
// The consequence is narrow and entirely mechanical: whoever writes Phase 4 from
// 000033's inventory re-keys four tables, leaves idx_ci_sources_name alone, and
// ships a deployment where two organizations still cannot both name a CI source
// "github-prod" and the error still discloses that the other one has. The leak
// this whole issue is about, surviving in a sibling of the table it was fixed
// in, because the fix was written from a list rather than from the schema.
//
// So this asserts the inventory ITSELF, derived from the catalog:
//
//   - every partition root that has a global unique name is named here. When
//     Phase 4 re-keys one to UNIQUE (organization_id, name) the index stops
//     having a single `name` key, this set shrinks, and the test fails until
//     someone records that it was deliberate.
//   - a table OUTSIDE the roots that acquires one fails too. role_templates is
//     the only one today and 000033 argues it: deployment-wide on purpose, its
//     uniqueness already per-app. A new tenant-owned table arriving with a
//     global unique name is exactly the case a transcribed list would miss.
func TestIntegration_Phase4NameRekeyInventory_IsCompleteAndDerived(t *testing.T) {
	db := newTestDB(t)
	found := globallyUniqueNameIndexes(t, db)

	roots := map[string]bool{}
	for _, r := range tsmPartitionRoots {
		roots[r] = true
	}

	// EMPTY, AND THAT IS PHASE 4 DONE. This held the five roots that still
	// carried a global UNIQUE(name) and had to be re-keyed; migration 000034
	// re-keyed all five to UNIQUE (organization_id, name), so none of them
	// should appear in a scan for single-column `name` uniqueness any more.
	//
	// The map stays rather than being deleted, and so does the check below it: a
	// root that reappears here means a global name index came BACK -- restored by
	// a rollback, or added by a new migration that copied an older one -- and
	// that is a tenancy regression with a constraint error that discloses another
	// organization's row.
	wantRoots := map[string]bool{}
	// Tables outside the partition that may hold a global unique name, each
	// because 000033 argues they are not tenant data.
	allowedNonRoots := map[string]string{
		"role_templates": "000033: TSM's own role -> scope definitions, " +
			"deployment-wide on purpose; 000032:92 notes its uniqueness is already per-app",
	}

	gotRoots := map[string]bool{}
	for table := range found {
		switch {
		case roots[table]:
			gotRoots[table] = true
		case allowedNonRoots[table] != "":
			// Justified in 000033. Left alone.
		default:
			t.Errorf("%s has a globally-unique `name` index (%s) and is neither a "+
				"partition root nor one of the tables 000033 argues out of the "+
				"partition. Either it is tenant data, in which case it needs an "+
				"organization_id and a per-organization key, or it is not, in which "+
				"case say so where 000033 says so about the others.",
				table, found[table])
		}
	}

	for table := range gotRoots {
		if !wantRoots[table] {
			t.Errorf("%s carries a globally-unique `name` index again. Phase 4 (000034) re-keyed "+
				"every partition root to UNIQUE (organization_id, name); a single-column one here "+
				"means the global namespace is back, and with it a constraint error that tells one "+
				"organization what another has named its rows.", table)
		}
	}
	for table := range wantRoots {
		if !gotRoots[table] {
			t.Errorf("%s no longer has a globally-unique `name` index. If Phase 4 "+
				"re-keyed it to UNIQUE (organization_id, name), that is the intended "+
				"outcome and this inventory should record it as done -- remove it from "+
				"wantRoots and invert the disclosure test for it. If nobody re-keyed "+
				"it, an index that guarded uniqueness has been dropped.", table)
		}
	}
	for table := range gotRoots {
		if !wantRoots[table] {
			t.Errorf("partition root %s has a globally-unique `name` index (%s) that "+
				"this inventory does not name, so Phase 4 would not re-key it and two "+
				"organizations could not both use a name in it.", table, found[table])
		}
	}

	t.Logf("Phase 4 name re-keying inventory, derived from the catalog: %d partition "+
		"roots carry UNIQUE(name) and must become UNIQUE(organization_id, name); "+
		"%d non-root tables carry one and are argued out of the partition by 000033.",
		len(gotRoots), len(found)-len(gotRoots))
}
