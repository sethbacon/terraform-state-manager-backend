//go:build integration

// The other half of isolation_integration_test.go.
//
// That file proved the leak: a row created through the application landed in the
// deployment's default organization whoever created it, because no INSERT named
// organization_id and the column DEFAULT decided. This file proves the fix, to
// the same standard — against a real PostgreSQL, through the repository the
// handlers actually call.
//
// Run with:
//
//	TEST_DATABASE_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable' \
//	  go test -tags integration ./internal/tenancy/...
package tenancy

import (
	"context"
	"errors"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// TestIntegration_CreateStampsTheActingOrganization is the assertion #436 exists
// for: two organizations create a source each, and each row carries the
// organization that created it rather than the deployment default.
func TestIntegration_CreateStampsTheActingOrganization(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := repositories.NewSourceRepository(db)

	// The precondition the bug depended on. If the default is unset the DEFAULT
	// would write NULL and this test would pass for the wrong reason.
	if _, err := db.ExecContext(ctx,
		`UPDATE system_settings SET default_organization_id = $1::uuid WHERE id = 1`, orgAlpha); err != nil {
		t.Fatalf("seed the deployment default: %v", err)
	}

	alpha, err := repo.Create(ctx, &repositories.Source{Name: "alpha-src", Type: "local"}, orgAlpha)
	if err != nil {
		t.Fatalf("create in alpha: %v", err)
	}
	beta, err := repo.Create(ctx, &repositories.Source{Name: "beta-src", Type: "local"}, orgBeta)
	if err != nil {
		t.Fatalf("create in beta: %v", err)
	}

	for _, tc := range []struct{ id, want, name string }{
		{alpha.ID, orgAlpha, "alpha-src"},
		{beta.ID, orgBeta, "beta-src"},
	} {
		var got string
		if err := db.QueryRowContext(ctx,
			`SELECT organization_id::text FROM state_sources WHERE id = $1`, tc.id).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s landed in organization %s, want %s", tc.name, got, tc.want)
		}
	}

	// The point of the whole issue. Before #436 this row would have said alpha,
	// because alpha is the deployment default and nothing named an organization.
	var betaOrg string
	if err := db.QueryRowContext(ctx,
		`SELECT organization_id::text FROM state_sources WHERE name = 'beta-src'`).Scan(&betaOrg); err != nil {
		t.Fatalf("read beta: %v", err)
	}
	if betaOrg == orgAlpha {
		t.Fatalf("beta's source landed in the DEFAULT organization (%s). The column DEFAULT is "+
			"still deciding, which is the whole of #436.", orgAlpha)
	}

	t.Logf("PROVED: two organizations created a source each and the rows carry %s and %s. "+
		"The deployment default is %s, so neither row took it by accident.", orgAlpha, orgBeta, orgAlpha)
}

// TestIntegration_CreateRefusesAnUnownedRow proves the refusal reaches the
// database boundary, not just the unit test's mock — and that no row appears.
func TestIntegration_CreateRefusesAnUnownedRow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := repositories.NewSourceRepository(db)

	if _, err := repo.Create(ctx, &repositories.Source{Name: "unowned", Type: "local"}, ""); !errors.Is(err, repositories.ErrNoOrganization) {
		t.Fatalf("err = %v, want ErrNoOrganization", err)
	}

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM state_sources WHERE name = 'unowned'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("%d unowned row(s) were written. Refusing must happen BEFORE the INSERT, or the "+
			"column DEFAULT quietly supplies an organization nobody chose.", n)
	}
}
