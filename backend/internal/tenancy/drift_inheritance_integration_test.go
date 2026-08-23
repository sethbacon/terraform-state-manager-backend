//go:build integration

// drift_records inherits its organization from the SOURCE, and this proves the
// three properties that makes true — against a real PostgreSQL, because all
// three are properties of the SQL rather than of the Go around it.
//
// A sqlmock test cannot see any of them: it matches statements by regex and
// returns whatever rows the test hands back, so an added predicate, a changed
// conflict target, or an organization_id that crept into the DO UPDATE SET would
// all pass. That is not hypothetical — it is exactly how two mutations survived
// the first round on terraform-registry's namespace resolver.
package tenancy

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

func seedSourceForDrift(t *testing.T, db *sql.DB, orgID, name string) string {
	t.Helper()
	var id string
	q := `INSERT INTO state_sources (name, type, organization_id) VALUES ($1,'local',$2) RETURNING id`
	if orgID == "" {
		q = `INSERT INTO state_sources (name, type, organization_id) VALUES ($1,'local',NULL) RETURNING id`
		if err := db.QueryRow(q, name).Scan(&id); err != nil {
			t.Fatalf("seed unstamped source: %v", err)
		}
		return id
	}
	if err := db.QueryRow(q, name, orgID).Scan(&id); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	return id
}

func driftRecordOrg(t *testing.T, db *sql.DB, recordID string) string {
	t.Helper()
	var org sql.NullString
	if err := db.QueryRow(`SELECT organization_id::text FROM drift_records WHERE id = $1`, recordID).Scan(&org); err != nil {
		t.Fatalf("read record organization: %v", err)
	}
	if !org.Valid {
		t.Fatal("the drift record has a NULL organization — invisible to every tenant")
	}
	return org.String
}

// TestIntegration_DriftRecordInheritsItsSourcesOrganization is the base case.
func TestIntegration_DriftRecordInheritsItsSourcesOrganization(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := repositories.NewDriftRecordRepository(db)

	betaSource := seedSourceForDrift(t, db, orgBeta, "beta-drift-src")

	rec, err := repo.UpsertDetection(ctx, &repositories.Detection{
		SourceID: betaSource, StateKey: "prod/vpc", Origin: "callback", Added: 1,
	})
	if err != nil {
		t.Fatalf("UpsertDetection: %v", err)
	}
	if got := driftRecordOrg(t, db, rec.ID); got != orgBeta {
		t.Errorf("record landed in organization %s, want %s (its source's)", got, orgBeta)
	}
}

// TestIntegration_ASecondDetectionCannotRepartentTheRecord is the property the
// ON CONFLICT makes load-bearing.
//
// The two producers — the drift callback and /drift/ingest — collapse onto ONE
// row. If organization_id were in the DO UPDATE SET, whichever producer wrote
// LAST would decide a live record's tenant, and the two could differ. It is
// deliberately absent, so the first write fixes it.
func TestIntegration_ASecondDetectionCannotRepartentTheRecord(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := repositories.NewDriftRecordRepository(db)

	src := seedSourceForDrift(t, db, orgBeta, "beta-repartent-src")
	first, err := repo.UpsertDetection(ctx, &repositories.Detection{
		SourceID: src, StateKey: "prod/vpc", Origin: "callback", Added: 1,
	})
	if err != nil {
		t.Fatalf("first detection: %v", err)
	}

	// Move the SOURCE to another organization behind the record's back, then
	// detect again. The conflicting upsert must update the finding and leave the
	// record's organization alone.
	if _, err := db.ExecContext(ctx,
		`UPDATE state_sources SET organization_id = $1::uuid WHERE id = $2`, orgAlpha, src); err != nil {
		t.Fatalf("move source: %v", err)
	}
	second, err := repo.UpsertDetection(ctx, &repositories.Detection{
		SourceID: src, StateKey: "prod/vpc", Origin: "ingest", Added: 9,
	})
	if err != nil {
		t.Fatalf("second detection: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("the second detection created a NEW record (%s vs %s); the ON CONFLICT no longer "+
			"collapses the two producers onto one row", second.ID, first.ID)
	}
	if got := driftRecordOrg(t, db, first.ID); got != orgBeta {
		t.Errorf("the record was re-parented to %s by a later detection; organization_id must not "+
			"appear in the DO UPDATE SET, or a live record's tenant is decided by whichever "+
			"producer wrote last", got)
	}
	if second.Added != 9 {
		t.Errorf("added = %d, want 9 — the update stopped applying while we were at it", second.Added)
	}
}

// TestIntegration_AnUnownedSourceRefusesTheDetection covers the case that would
// otherwise produce a finding nobody can see.
func TestIntegration_AnUnownedSourceRefusesTheDetection(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := repositories.NewDriftRecordRepository(db)

	// PHASE 4 REMOVED ONE OF THESE CASES BY MAKING IT IMPOSSIBLE. Through phases
	// 1-3 an unstamped source could exist -- written by a replica on the previous
	// build, before the backfill -- and UpsertDetection had to refuse it. After
	// 000034's NOT NULL it cannot be created at all, which is asserted below
	// instead: "this bad state is handled" becomes "this bad state cannot arise",
	// and the second is what the phase actually bought.
	for _, tc := range []struct{ name, sourceID string }{
		{"missing source", "99999999-9999-4999-8999-999999999999"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := repo.UpsertDetection(ctx, &repositories.Detection{
				SourceID: tc.sourceID, StateKey: "prod/vpc", Origin: "callback",
			})
			if !errors.Is(err, repositories.ErrSourceNotOwned) {
				t.Fatalf("err = %v, want ErrSourceNotOwned", err)
			}
			var n int
			if err := db.QueryRow(`SELECT count(*) FROM drift_records WHERE source_id::text = $1`, tc.sourceID).Scan(&n); err != nil {
				t.Fatalf("count: %v", err)
			}
			if n != 0 {
				t.Errorf("%d record(s) written for an unowned source; a drift record with a NULL "+
					"organization is a finding invisible to every tenant", n)
			}
		})
	}
}

// TestIntegration_ScheduleCarriesItsOrganizationOutOfTheDatabase guards the one
// link in the worker chain that sqlmock cannot see.
//
// The scheduler has no request, so a scheduled drift run is stamped with the
// organization the SCHEDULE carries in memory. That value has to survive the
// projection — and a mutation removing organization_id from scheduleColumns
// passed every unit test, because sqlmock returns whatever columns the FIXTURE
// declares regardless of what the SQL asked for.
//
// Against a real database the column either comes back or it does not.
func TestIntegration_ScheduleCarriesItsOrganizationOutOfTheDatabase(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := repositories.NewScheduleRepository(db)

	created, err := repo.Create(ctx, &repositories.Schedule{
		Name: "nightly", CronExpr: "0 2 * * *", TargetType: "drift",
	}, nil, orgBeta)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.OrganizationID != orgBeta {
		t.Errorf("Create returned organization %q, want %q", created.OrganizationID, orgBeta)
	}

	// The read path matters more than the write: this is the value the worker
	// carries into Dispatch, and it comes from a SELECT.
	loaded, err := repo.GetByID(ctx, created.ID)
	if err != nil || loaded == nil {
		t.Fatalf("GetByID: %v", err)
	}
	if loaded.OrganizationID != orgBeta {
		t.Errorf("a schedule loaded from the database carries organization %q, want %q. "+
			"Without it the worker dispatches an empty organization and the run is unowned — "+
			"and there is no edge from a run back to its schedule to recover it.",
			loaded.OrganizationID, orgBeta)
	}
}

// TestIntegration_AnUnstampedSourceCannotBeCreated is the other half of the case
// above, and it is the one Phase 4 added.
//
// UpsertDetection's refusal still exists and is still correct -- a defence that
// only holds while the schema does is not a defence -- but the schema now makes
// the state unreachable, and that is worth pinning separately. If 000034 were
// reverted this fails, and the refusal above becomes load-bearing again.
func TestIntegration_AnUnstampedSourceCannotBeCreated(t *testing.T) {
	db := newTestDB(t)
	_, err := db.Exec(`INSERT INTO state_sources (name, type, organization_id) VALUES ('unstamped', 'local', NULL)`)
	if err == nil {
		t.Fatal("a source was created with no organization: after 000034 that column is NOT NULL, " +
			"and an unstamped row is invisible to every tenant while visible to a platform admin")
	}
}
