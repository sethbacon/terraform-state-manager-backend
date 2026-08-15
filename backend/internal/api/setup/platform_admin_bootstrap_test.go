// platform_admin_bootstrap_test.go pins the ONE path by which a deployment with
// nobody in it acquires its first platform administrator.
//
// Everything else that can write the carrier requires an authenticated caller
// who already holds `admin` — which is exactly what a fresh deployment does not
// have. This step runs behind the setup-token middleware, before any owner
// exists, and is permanently unreachable once setup completes; so it must (a)
// actually write the grant, (b) converge when replayed, and (c) never let the
// deployment be recorded as owner-configured on a grant that did not happen,
// because that recording is what burns the step.
package setup

import (
	"database/sql"
	"net/http"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/platformadmin"
)

var carrierGrantCols = []string{"user_id", "granted_by", "granted_at", "note"}

// newConfigureAdminEnvWithCarrier is newConfigureAdminEnv plus a real carrier
// service over a SECOND handle — which is the production topology, not a test
// convenience: the carrier and its outbox live on the app connection while
// identity lives on its own, and the whole reason the outbox exists is that
// those two cannot share a transaction.
func newConfigureAdminEnvWithCarrier(t *testing.T) (*Handlers, sqlmock.Sqlmock, sqlmock.Sqlmock) {
	t.Helper()
	identityDB, identityMock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New (identity): %v", err)
	}
	t.Cleanup(func() { _ = identityDB.Close() })
	appDB, appMock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New (app): %v", err)
	}
	t.Cleanup(func() { _ = appDB.Close() })

	svc, err := platformadmin.New(appDB, identityDB)
	if err != nil {
		t.Fatalf("platformadmin.New: %v", err)
	}
	cfg := &config.Config{}
	cfg.Suite.RoleSeedOwner = "self"
	h := NewHandlers(repositories.NewSystemSettingsRepository(identityDB), nil, nil, identityDB, nil, svc, cfg, nil)
	return h, identityMock, appMock
}

// expectCarrierTargetResolves scripts the resolver lookup the grant makes before
// touching the carrier. Granting to an id that answers to nobody would mint a row
// that elevates no one and counts for nothing in the floor, so the service
// refuses it — even here, where the id came from a user this handler just wrote.
func expectCarrierTargetResolves(mock sqlmock.Sqlmock) {
	now := time.Now()
	mock.ExpectQuery("SELECT id, email, name, oidc_sub").
		WillReturnRows(sqlmock.NewRows(setupUserCols).
			AddRow("u1", "owner@example.com", "owner@example.com", nil, now, now))
}

// TestConfigureAdmin_WritesTheFirstPlatformAdminGrant: the wizard's owner becomes
// this deployment's platform administrator, with the audit intent in the grant's
// own transaction.
func TestConfigureAdmin_WritesTheFirstPlatformAdminGrant(t *testing.T) {
	h, identityMock, appMock := newConfigureAdminEnvWithCarrier(t)
	expectOwnerLookups(identityMock)
	identityMock.ExpectExec("UPDATE organization_members").WillReturnResult(sqlmock.NewResult(0, 1))
	expectCarrierTargetResolves(identityMock)

	appMock.ExpectBegin()
	appMock.ExpectQuery(`INSERT INTO "platform_admins"`).
		WillReturnRows(sqlmock.NewRows(carrierGrantCols).
			AddRow("u1", nil, time.Now(), "granted by the first-run setup wizard"))
	appMock.ExpectExec(`INSERT INTO "audit_outbox"`).WillReturnResult(sqlmock.NewResult(0, 1))
	appMock.ExpectCommit()

	identityMock.ExpectExec("UPDATE system_settings").WillReturnResult(sqlmock.NewResult(0, 1))

	w := postJSON(h.ConfigureAdmin, `{"email":"owner@example.com"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if err := appMock.ExpectationsWereMet(); err != nil {
		t.Errorf("the first platform-admin grant was not written with its audit intent: %v", err)
	}
	if err := identityMock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet identity expectations: %v", err)
	}
}

// TestConfigureAdmin_CarrierGrantIsIdempotent: the wizard step can be replayed,
// and a second run must converge rather than fail — leaving the original
// granted_by/granted_at/note alone, which is what the module's
// ON CONFLICT (user_id) DO NOTHING does.
func TestConfigureAdmin_CarrierGrantIsIdempotent(t *testing.T) {
	h, identityMock, appMock := newConfigureAdminEnvWithCarrier(t)
	expectOwnerLookups(identityMock)
	identityMock.ExpectExec("UPDATE organization_members").WillReturnResult(sqlmock.NewResult(0, 1))
	expectCarrierTargetResolves(identityMock)

	appMock.ExpectBegin()
	// The row is already there: RETURNING matches nothing.
	appMock.ExpectQuery(`INSERT INTO "platform_admins"`).WillReturnError(sql.ErrNoRows)
	appMock.ExpectRollback()

	identityMock.ExpectExec("UPDATE system_settings").WillReturnResult(sqlmock.NewResult(0, 1))

	w := postJSON(h.ConfigureAdmin, `{"email":"owner@example.com"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("re-running the wizard must converge, got status %d (%s)", w.Code, w.Body.String())
	}
	if err := identityMock.ExpectationsWereMet(); err != nil {
		t.Errorf("the wizard did not complete on a replay: %v", err)
	}
}

// TestConfigureAdmin_DoesNotBurnTheStepWhenTheCarrierGrantFails is the
// fails-closed half, and it uses the same tripwire the promote test does: the
// settings write is SCRIPTED TO SUCCEED, so reaching it means the deployment was
// recorded as owner-configured on a grant that never landed — after which the
// wizard is gone and nobody can grant it.
func TestConfigureAdmin_DoesNotBurnTheStepWhenTheCarrierGrantFails(t *testing.T) {
	h, identityMock, appMock := newConfigureAdminEnvWithCarrier(t)
	expectOwnerLookups(identityMock)
	identityMock.ExpectExec("UPDATE organization_members").WillReturnResult(sqlmock.NewResult(0, 1))
	expectCarrierTargetResolves(identityMock)

	appMock.ExpectBegin()
	appMock.ExpectQuery(`INSERT INTO "platform_admins"`).WillReturnError(sql.ErrConnDone)
	appMock.ExpectRollback()

	identityMock.ExpectExec("UPDATE system_settings").WillReturnResult(sqlmock.NewResult(0, 1))

	w := postJSON(h.ConfigureAdmin, `{"email":"owner@example.com"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when the carrier grant fails (%s)", w.Code, w.Body.String())
	}
	if err := identityMock.ExpectationsWereMet(); err == nil {
		t.Error("SetAdminConfigured ran: the deployment is now permanently marked " +
			"owner-configured with no platform administrator in the carrier, and the " +
			"wizard step that could have created one is burnt")
	}
}
