// notfound_class_test.go pins the first-run owner grant across
// terraform-suite-identity's store.ErrNotFound change (module v0.24.0).
//
// ConfigureAdmin is the one place in this app where a single UPDATE decides
// whether anyone can administer the deployment, and its 200 is what burns
// SetAdminConfigured — after which the wizard step is gone. Before v0.24.0 an
// UPDATE that matched no membership row returned nil, so a grant that wrote
// nothing reported success and permanently marked the deployment "owner
// configured" with no owner in it.
package setup

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// errDuplicate stands in for the unique-constraint violation a re-run of the
// wizard provokes; the handler only branches on "the insert failed", not on the
// driver-specific code.
var errDuplicate = errors.New("pq: duplicate key value violates unique constraint")

var setupOrgCols = []string{"id", "name", "display_name", "idp_type", "idp_name", "created_at", "updated_at"}
var setupUserCols = []string{"id", "email", "name", "oidc_sub", "created_at", "updated_at"}

// newConfigureAdminEnv wires ConfigureAdmin in standalone mode (TSM owns
// identity) over one sqlmock database.
func newConfigureAdminEnv(t *testing.T) (*Handlers, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := &config.Config{}
	cfg.Suite.RoleSeedOwner = "self"
	return NewHandlers(repositories.NewSystemSettingsRepository(db), nil, nil, db, nil, cfg, nil), mock
}

// expectOwnerLookups scripts everything up to (but not including) the role
// grant: default org, a user insert that collides, and the reuse-by-email read.
func expectOwnerLookups(mock sqlmock.Sqlmock) {
	now := time.Now()
	mock.ExpectQuery("FROM organizations").WithArgs("default").
		WillReturnRows(sqlmock.NewRows(setupOrgCols).
			AddRow("o1", "default", "Default", nil, nil, now, now))
	// Re-run of the wizard: the owner already exists, so the INSERT collides and
	// the handler falls back to reusing the row.
	mock.ExpectExec("INSERT INTO users").WillReturnError(errDuplicate)
	mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("owner@example.com").
		WillReturnRows(sqlmock.NewRows(setupUserCols).
			AddRow("u1", "owner@example.com", "owner@example.com", nil, now, now))
	// AddMemberWithParams: role lookup succeeds, the INSERT collides because the
	// membership is already there — the documented path into the promote branch.
	mock.ExpectQuery("SELECT id FROM role_templates WHERE name").WithArgs("admin").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("rt-admin"))
	mock.ExpectExec("INSERT INTO organization_members").WillReturnError(errDuplicate)
	// The promote itself resolves the role name again.
	mock.ExpectQuery("SELECT id FROM role_templates WHERE name").WithArgs("admin").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("rt-admin"))
}

// TestConfigureAdmin_PromoteMatchedNoRow_FailsClosed: the promote UPDATE writes
// no row, so nobody was granted admin. The handler must refuse rather than
// answer 200 and burn the setup step.
func TestConfigureAdmin_PromoteMatchedNoRow_FailsClosed(t *testing.T) {
	h, mock := newConfigureAdminEnv(t)
	expectOwnerLookups(mock)
	mock.ExpectExec("UPDATE organization_members").WillReturnResult(sqlmock.NewResult(0, 0))
	// The settings write is SCRIPTED TO SUCCEED on purpose. Without it, a
	// handler that wrongly carried on would still answer 500 — because the
	// unscripted query fails — and this test would pass for the wrong reason.
	// Queued, it is a live tripwire: reaching it means the wizard recorded an
	// owner it never granted, and the assertion below is that it stayed unmet.
	mock.ExpectExec("UPDATE system_settings").WillReturnResult(sqlmock.NewResult(0, 1))

	w := postJSON(h.ConfigureAdmin, `{"email":"owner@example.com"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("promote that matched no row: status = %d, want 500 — a privilege "+
			"grant that wrote nothing must not report success (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no membership to promote") {
		t.Errorf("the 500 must name the actual cause, got %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err == nil {
		t.Error("SetAdminConfigured ran: the deployment is now permanently marked " +
			"owner-configured with nobody holding admin — the exact silent failure " +
			"store.ErrNotFound exists to prevent")
	}
}

// TestConfigureAdmin_PromoteSucceeds_RecordsOwner is the happy re-run: the
// membership existed, the promote matched it, and the wizard step is recorded.
func TestConfigureAdmin_PromoteSucceeds_RecordsOwner(t *testing.T) {
	h, mock := newConfigureAdminEnv(t)
	expectOwnerLookups(mock)
	mock.ExpectExec("UPDATE organization_members").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE system_settings").WillReturnResult(sqlmock.NewResult(0, 1))

	w := postJSON(h.ConfigureAdmin, `{"email":"owner@example.com"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("re-running the wizard must still promote the existing owner: "+
			"status = %d (%s)", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
