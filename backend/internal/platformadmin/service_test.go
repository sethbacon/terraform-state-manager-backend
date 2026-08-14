package platformadmin

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	idplatformadmin "github.com/sethbacon/terraform-suite-identity/identity/platformadmin"
)

// userCols mirrors identity's user projection, so the resolver's lookup can be
// answered without a database.
var userCols = []string{"id", "email", "name", "oidc_sub", "created_at", "updated_at"}

// grantCols is the carrier projection every read in the module uses.
var grantCols = []string{"user_id", "granted_by", "granted_at", "note"}

const testUserID = "11111111-1111-4111-8111-111111111111"

type rig struct {
	svc      *Service
	app      sqlmock.Sqlmock
	identity sqlmock.Sqlmock
}

func newRig(t *testing.T) rig {
	t.Helper()
	appDB, appMock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New (app): %v", err)
	}
	t.Cleanup(func() { appDB.Close() })
	identityDB, identityMock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New (identity): %v", err)
	}
	t.Cleanup(func() { identityDB.Close() })

	svc, err := New(appDB, identityDB)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return rig{svc: svc, app: appMock, identity: identityMock}
}

func (r rig) expectUserResolves(exists bool) {
	q := r.identity.ExpectQuery("SELECT id, email, name, oidc_sub, created_at, updated_at")
	if !exists {
		q.WillReturnRows(sqlmock.NewRows(userCols))
		return
	}
	now := time.Now()
	q.WillReturnRows(sqlmock.NewRows(userCols).
		AddRow(testUserID, "owner@example.com", "Owner", "sub-1", now, now))
}

func (r rig) expectUserLookupFails(cause error) {
	r.identity.ExpectQuery("SELECT id, email, name, oidc_sub, created_at, updated_at").
		WillReturnError(cause)
}

// --- construction -------------------------------------------------------------

// A half-constructed carrier is the one shape that can silently stop answering,
// so both connections are mandatory and the refusal is a sentinel a caller can
// match rather than a string.
func TestNewRequiresBothConnections(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	for _, tc := range []struct {
		name             string
		appDB, identryDB *sql.DB
	}{
		{"no application connection", nil, db},
		{"no identity connection", db, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, err := New(tc.appDB, tc.identryDB)
			if !errors.Is(err, ErrNotConfigured) {
				t.Errorf("err = %v, want ErrNotConfigured", err)
			}
			if svc != nil {
				t.Errorf("a refused construction must not return a service: %#v", svc)
			}
		})
	}
}

// --- SessionScopes ------------------------------------------------------------

// TestSessionScopesAddsAdminFromTheCarrier: a session whose token carried no
// admin holds it because the carrier does, right now.
func TestSessionScopesAddsAdminFromTheCarrier(t *testing.T) {
	r := newRig(t)
	r.app.ExpectQuery(`FROM "platform_admins" WHERE user_id`).WithArgs(testUserID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	got, err := r.svc.SessionScopes(context.Background(), testUserID, []string{"state:read"})
	if err != nil {
		t.Fatalf("SessionScopes: %v", err)
	}
	if !contains(got, "admin") || !contains(got, "state:read") {
		t.Errorf("scopes = %v, want state:read and admin", got)
	}
}

// TestSessionScopesKeepsLegacyAdminWhenTheCarrierIsSilent is the PHASE-2 rule,
// and the one an over-eager port of the module's carrier-only reading would
// break.
//
// The module's Carrier.SessionScopes strips `admin` on every path and re-adds it
// only from the carrier. That is the end state. Adopting it today would strip
// the admin that every existing TSM deployment's administrators actually hold —
// through an admin-bearing role template in shared identity — leaving them with
// no administrator, no carrier row to grant one from, and a setup wizard that has
// already been burnt. Until reads move to the carrier alone, the carrier ADDS and
// never removes.
func TestSessionScopesKeepsLegacyAdminWhenTheCarrierIsSilent(t *testing.T) {
	r := newRig(t)
	r.app.ExpectQuery(`FROM "platform_admins" WHERE user_id`).WithArgs(testUserID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	got, err := r.svc.SessionScopes(context.Background(), testUserID, []string{"admin", "state:read"})
	if err != nil {
		t.Fatalf("SessionScopes: %v", err)
	}
	if !contains(got, "admin") {
		t.Errorf("scopes = %v: this phase must not remove the role-template admin an existing "+
			"deployment runs on — doing so locks every current administrator out", got)
	}
	if count(got, "admin") != 1 {
		t.Errorf("scopes = %v: admin appears %d times, want exactly one", got, count(got, "admin"))
	}
}

// A user with neither a carrier row nor a legacy admin claim gains nothing. The
// additive rule above must not become "always admin".
func TestSessionScopesGrantNothingToANonAdmin(t *testing.T) {
	r := newRig(t)
	r.app.ExpectQuery(`FROM "platform_admins" WHERE user_id`).WithArgs(testUserID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	got, err := r.svc.SessionScopes(context.Background(), testUserID, []string{"state:read"})
	if err != nil {
		t.Fatalf("SessionScopes: %v", err)
	}
	if contains(got, "admin") {
		t.Errorf("scopes = %v, want no admin", got)
	}
}

// The caller's slice is typically claims.Scopes, which is published elsewhere on
// the request. Writing through it would elevate a value the caller believes is
// the token's own.
func TestSessionScopesDoesNotWriteThroughTheCallersSlice(t *testing.T) {
	r := newRig(t)
	r.app.ExpectQuery(`FROM "platform_admins" WHERE user_id`).WithArgs(testUserID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	// Spare capacity is what makes an in-place append visible to the caller.
	callers := make([]string, 1, 8)
	callers[0] = "state:read"

	if _, err := r.svc.SessionScopes(context.Background(), testUserID, callers); err != nil {
		t.Fatalf("SessionScopes: %v", err)
	}
	if len(callers) != 1 || callers[0] != "state:read" {
		t.Errorf("caller's slice = %v, want [state:read] untouched", callers)
	}
}

// An unresolved carrier is reported, and the scopes handed back with it carry no
// admin — so a caller that chooses to continue continues unelevated.
func TestSessionScopesReportsAFailureAndReturnsUnelevatedScopes(t *testing.T) {
	r := newRig(t)
	boom := errors.New("connection reset")
	r.app.ExpectQuery(`FROM "platform_admins" WHERE user_id`).WillReturnError(boom)

	got, err := r.svc.SessionScopes(context.Background(), testUserID, []string{"admin", "state:read"})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the driver's own error", err)
	}
	if contains(got, "admin") {
		t.Errorf("scopes = %v: an unresolved carrier must not leave admin standing", got)
	}
}

// --- Grant --------------------------------------------------------------------

// TestGrantRefusesATargetThatResolvesToNobody is the application's own guard.
//
// The module deliberately does not resolve the target — only the app knows where
// its principals live — so without this, granting to a typo would mint a row that
// elevates nobody, counts for nothing in the never-zero floor, and sits in the
// administrator list looking like a person.
func TestGrantRefusesATargetThatResolvesToNobody(t *testing.T) {
	r := newRig(t)
	r.expectUserResolves(false)
	// No transaction and no INSERT are scripted: reaching the carrier at all
	// fails this test through ExpectationsWereMet below.

	_, err := r.svc.Grant(context.Background(), testUserID, Actor{UserID: "actor"}, nil)
	if !errors.Is(err, ErrUnknownUser) {
		t.Fatalf("err = %v, want ErrUnknownUser", err)
	}
	if err := r.app.ExpectationsWereMet(); err != nil {
		t.Errorf("the carrier was written despite an unresolvable target: %v", err)
	}
}

// TestGrantTellsAnOutageApartFromAnUnknownUser. The two answers reach the
// operator as 503 and 400: one says retry, the other says fix the id. Collapsing
// them would have an identity outage read as "that user does not exist", which is
// the reading that gets a real administrator deleted.
func TestGrantTellsAnOutageApartFromAnUnknownUser(t *testing.T) {
	r := newRig(t)
	r.expectUserLookupFails(errors.New("dial tcp: connection refused"))

	_, err := r.svc.Grant(context.Background(), testUserID, Actor{}, nil)
	if !errors.Is(err, idplatformadmin.ErrIdentityUnavailable) {
		t.Fatalf("err = %v, want ErrIdentityUnavailable", err)
	}
	if errors.Is(err, ErrUnknownUser) {
		t.Error("an identity outage must not be reported as an unknown user")
	}
}

// A grant writes its audit intent inside the insert's own transaction. Scripting
// the transaction proves both statements land on the SAME connection between one
// BEGIN and one COMMIT, which is what the constraint trigger checks at commit and
// what makes "granted, but the audit write failed" unreachable.
func TestGrantWritesItsAuditIntentInTheSameTransaction(t *testing.T) {
	r := newRig(t)
	r.expectUserResolves(true)
	r.app.ExpectBegin()
	r.app.ExpectQuery(`INSERT INTO "platform_admins"`).
		WillReturnRows(sqlmock.NewRows(grantCols).AddRow(testUserID, "actor-1", time.Now(), "because"))
	r.app.ExpectExec(`INSERT INTO "audit_outbox"`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	r.app.ExpectCommit()

	note := "because"
	grant, err := r.svc.Grant(context.Background(), testUserID, Actor{UserID: "actor-1", Email: "a@b.c"}, &note)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if grant.UserID != testUserID {
		t.Errorf("granted user = %q, want %q", grant.UserID, testUserID)
	}
	if err := r.app.ExpectationsWereMet(); err != nil {
		t.Errorf("the grant did not write its intent in its own transaction: %v", err)
	}
}

// A refused audit intent takes the grant with it: no COMMIT is issued.
func TestGrantIsRolledBackWhenItsAuditIntentCannotBeWritten(t *testing.T) {
	r := newRig(t)
	r.expectUserResolves(true)
	r.app.ExpectBegin()
	r.app.ExpectQuery(`INSERT INTO "platform_admins"`).
		WillReturnRows(sqlmock.NewRows(grantCols).AddRow(testUserID, nil, time.Now(), nil))
	r.app.ExpectExec(`INSERT INTO "audit_outbox"`).WillReturnError(errors.New("outbox is gone"))
	r.app.ExpectRollback()

	if _, err := r.svc.Grant(context.Background(), testUserID, Actor{}, nil); err == nil {
		t.Fatal("Grant succeeded with no audit record; the mutation must fail with its intent")
	}
	if err := r.app.ExpectationsWereMet(); err != nil {
		t.Errorf("expected BEGIN/INSERT/INSERT/ROLLBACK with no COMMIT: %v", err)
	}
}

// --- EnsureAdmin (the bootstrap path) -----------------------------------------

// TestEnsureAdminIsIdempotent: the first-run wizard step it backs can be
// replayed, so a second run must converge rather than fail — and must leave the
// original granted_by/granted_at/note alone, which the module's
// ON CONFLICT DO NOTHING does by returning ErrAlreadyPlatformAdmin.
func TestEnsureAdminIsIdempotent(t *testing.T) {
	r := newRig(t)

	// First run: the row is created.
	r.expectUserResolves(true)
	r.app.ExpectBegin()
	r.app.ExpectQuery(`INSERT INTO "platform_admins"`).
		WillReturnRows(sqlmock.NewRows(grantCols).AddRow(testUserID, nil, time.Now(), "first-run"))
	r.app.ExpectExec(`INSERT INTO "audit_outbox"`).WillReturnResult(sqlmock.NewResult(0, 1))
	r.app.ExpectCommit()

	created, err := r.svc.EnsureAdmin(context.Background(), testUserID, "first-run")
	if err != nil {
		t.Fatalf("first EnsureAdmin: %v", err)
	}
	if !created {
		t.Error("first EnsureAdmin reported no row created")
	}

	// Second run: ON CONFLICT DO NOTHING returns no row.
	r.expectUserResolves(true)
	r.app.ExpectBegin()
	r.app.ExpectQuery(`INSERT INTO "platform_admins"`).WillReturnError(sql.ErrNoRows)
	r.app.ExpectRollback()

	created, err = r.svc.EnsureAdmin(context.Background(), testUserID, "first-run")
	if err != nil {
		t.Fatalf("second EnsureAdmin must succeed, got %v", err)
	}
	if created {
		t.Error("second EnsureAdmin reported a row created; the grant already existed")
	}
}

// A bootstrap failure that is not "already there" must surface. Reporting it as
// a no-op would let the wizard record the deployment as owner-configured with no
// administrator in the carrier — and the wizard is then permanently unreachable.
func TestEnsureAdminSurfacesRealFailures(t *testing.T) {
	r := newRig(t)
	r.expectUserResolves(false)

	created, err := r.svc.EnsureAdmin(context.Background(), testUserID, "first-run")
	if !errors.Is(err, ErrUnknownUser) {
		t.Errorf("err = %v, want ErrUnknownUser", err)
	}
	if created {
		t.Error("created = true on a failed bootstrap")
	}
}

// --- List ---------------------------------------------------------------------

// TestListLabelsOrphansRatherThanHidingThem. A grant whose user is gone elevates
// nobody and does not count towards the floor, but it is still a row — and this
// listing is the only surface that can remove it. Filtering it would make a live
// row invisible to its only cleanup path.
func TestListLabelsOrphansRatherThanHidingThem(t *testing.T) {
	r := newRig(t)
	const goneUserID = "22222222-2222-4222-8222-222222222222"
	r.app.ExpectQuery(`FROM "platform_admins"`).
		WillReturnRows(sqlmock.NewRows(grantCols).
			AddRow(testUserID, nil, time.Now(), "live").
			AddRow(goneUserID, nil, time.Now(), "deleted user"))
	r.expectUserResolves(true)
	r.expectUserResolves(false)

	entries, err := r.svc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: an orphan must be listed, not filtered out", len(entries))
	}
	if !entries[0].Exists {
		t.Errorf("entry %s is marked orphaned but its user resolves", entries[0].UserID)
	}
	if entries[1].Exists {
		t.Errorf("entry %s is not marked orphaned but its user is gone", entries[1].UserID)
	}
}

// An identity outage must not render every administrator as an orphan: the
// obvious response to that listing is to delete them.
func TestListRefusesToGuessDuringAnIdentityOutage(t *testing.T) {
	r := newRig(t)
	r.app.ExpectQuery(`FROM "platform_admins"`).
		WillReturnRows(sqlmock.NewRows(grantCols).AddRow(testUserID, nil, time.Now(), nil))
	r.expectUserLookupFails(errors.New("dial tcp: connection refused"))

	if _, err := r.svc.List(context.Background()); !errors.Is(err, idplatformadmin.ErrIdentityUnavailable) {
		t.Errorf("err = %v, want ErrIdentityUnavailable rather than a list of false orphans", err)
	}
}

// --- resolver -----------------------------------------------------------------

// The resolver's three outcomes, kept apart. Collapsing the error into false is
// the single defect the Resolver interface exists to make unwriteable: an
// identity store that is down would report every remaining grant as an orphan and
// let the last real administrator revoke themselves.
func TestResolverKeepsAbsenceAndFailureApart(t *testing.T) {
	r := newRig(t)
	resolver := r.svc.resolver

	r.expectUserResolves(false)
	exists, err := resolver.UserExists(context.Background(), testUserID)
	if err != nil || exists {
		t.Errorf("absent user: (%v, %v), want (false, nil)", exists, err)
	}

	boom := errors.New("dial tcp: connection refused")
	r.expectUserLookupFails(boom)
	exists, err = resolver.UserExists(context.Background(), testUserID)
	if err == nil {
		t.Error("a lookup failure must be an error, not a clean 'no such user'")
	}
	if exists {
		t.Error("a failed lookup must not report the user as existing")
	}

	// The empty id is a clean no with no query at all: user_id is UUID, so an
	// empty string would otherwise reach Postgres as an invalid-input error and
	// an authorization path would have to tell that apart from a database fault.
	exists, err = resolver.UserExists(context.Background(), "")
	if err != nil || exists {
		t.Errorf("empty id: (%v, %v), want (false, nil)", exists, err)
	}
	if err := r.identity.ExpectationsWereMet(); err != nil {
		t.Errorf("the empty id reached the database: %v", err)
	}
}

// An unwired resolver refuses rather than reporting nobody: "no resolver, so
// assume they all count" and "no resolver, so nobody counts" are both lockouts.
func TestResolverWithoutARepositoryRefuses(t *testing.T) {
	_, err := identityResolver{}.UserExists(context.Background(), testUserID)
	if !errors.Is(err, idplatformadmin.ErrIdentityUnavailable) {
		t.Errorf("err = %v, want ErrIdentityUnavailable", err)
	}
}

// --- helpers ------------------------------------------------------------------

func contains(scopes []string, want string) bool { return count(scopes, want) > 0 }

func count(scopes []string, want string) int {
	n := 0
	for _, s := range scopes {
		if s == want {
			n++
		}
	}
	return n
}

// --- Revoke -------------------------------------------------------------------

// expectSerializedRevoke scripts the advisory-lock transaction, the revoking
// transaction, and the FOR UPDATE read that feeds the floor predicate.
func (r rig) expectSerializedRevoke(remaining ...string) {
	r.app.ExpectBegin()
	r.app.ExpectExec("pg_advisory_xact_lock").WillReturnResult(sqlmock.NewResult(0, 0))
	r.app.ExpectBegin()
	rows := sqlmock.NewRows(grantCols).AddRow(testUserID, nil, time.Now(), nil)
	for _, id := range remaining {
		rows = rows.AddRow(id, nil, time.Now(), nil)
	}
	r.app.ExpectQuery(`FROM "platform_admins" ORDER BY granted_at ASC, user_id ASC FOR UPDATE`).
		WillReturnRows(rows)
}

// TestRevokeRefusesTheLastAdministrator. A deployment that revokes its last
// administrator has no recovery path short of hand-written SQL against the very
// table this API exists to replace.
func TestRevokeRefusesTheLastAdministrator(t *testing.T) {
	r := newRig(t)
	r.expectSerializedRevoke() // the target is the only grant
	r.app.ExpectRollback()
	r.app.ExpectRollback()

	_, err := r.svc.Revoke(context.Background(), testUserID, Actor{UserID: testUserID})
	if !errors.Is(err, idplatformadmin.ErrLastPlatformAdmin) {
		t.Fatalf("err = %v, want ErrLastPlatformAdmin", err)
	}
}

// The revocation and its audit intent land in ONE transaction, and the DELETE is
// issued only after the floor has accepted the grants that would remain.
func TestRevokeWritesItsIntentInTheDeletesOwnTransaction(t *testing.T) {
	r := newRig(t)
	const survivor = "55555555-5555-4555-8555-555555555555"
	r.expectSerializedRevoke(survivor)
	r.expectUserResolves(true) // the surviving grant is exercisable
	r.app.ExpectExec(`DELETE FROM "platform_admins"`).WillReturnResult(sqlmock.NewResult(0, 1))
	r.app.ExpectExec(`INSERT INTO "audit_outbox"`).WillReturnResult(sqlmock.NewResult(0, 1))
	r.app.ExpectCommit()
	r.app.ExpectRollback() // the lock-only transaction

	grant, err := r.svc.Revoke(context.Background(), testUserID, Actor{UserID: survivor})
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if grant.UserID != testUserID {
		t.Errorf("revoked %q, want %q", grant.UserID, testUserID)
	}
	if err := r.app.ExpectationsWereMet(); err != nil {
		t.Errorf("the revocation did not follow lock/read/floor/delete/intent/commit: %v", err)
	}
}

// A revocation whose floor cannot be evaluated is NOT performed, and reports the
// outage rather than a refusal: "there is nobody else" and "I could not find out
// whether there is anybody else" are different facts.
func TestRevokeReportsAnIdentityOutageWithoutDeletingAnything(t *testing.T) {
	r := newRig(t)
	const survivor = "55555555-5555-4555-8555-555555555555"
	r.expectSerializedRevoke(survivor)
	r.expectUserLookupFails(errors.New("dial tcp: connection refused"))
	r.app.ExpectRollback()
	r.app.ExpectRollback()

	_, err := r.svc.Revoke(context.Background(), testUserID, Actor{})
	if !errors.Is(err, idplatformadmin.ErrIdentityUnavailable) {
		t.Fatalf("err = %v, want ErrIdentityUnavailable", err)
	}
	if errors.Is(err, idplatformadmin.ErrLastPlatformAdmin) {
		t.Error("an outage must not be reported as 'there is nobody else'")
	}
	if err := r.app.ExpectationsWereMet(); err != nil {
		t.Errorf("expected no DELETE at all: %v", err)
	}
}

// --- guards on an unconstructed service ---------------------------------------

// Every entry point fails CLOSED on a nil service rather than answering as
// though the carrier had been consulted and said no.
func TestUnconstructedServiceRefusesEveryOperation(t *testing.T) {
	var s *Service
	ctx := context.Background()

	if _, err := s.SessionScopes(ctx, testUserID, []string{"admin"}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("SessionScopes err = %v, want ErrNotConfigured", err)
	}
	if _, err := s.List(ctx); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("List err = %v, want ErrNotConfigured", err)
	}
	if _, err := s.IsPlatformAdmin(ctx, testUserID); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("IsPlatformAdmin err = %v, want ErrNotConfigured", err)
	}
	if _, err := s.Grant(ctx, testUserID, Actor{}, nil); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Grant err = %v, want ErrNotConfigured", err)
	}
	if _, err := s.Revoke(ctx, testUserID, Actor{}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Revoke err = %v, want ErrNotConfigured", err)
	}
	if _, err := s.EnsureAdmin(ctx, testUserID, "note"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("EnsureAdmin err = %v, want ErrNotConfigured", err)
	}
	if err := s.Verify(ctx); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Verify err = %v, want ErrNotConfigured", err)
	}
	if s.Relay() != nil {
		t.Error("a nil service must not hand out a relay")
	}
}

// A mutation that names no principal is refused before the database is touched:
// user_id is UUID, so an empty string would otherwise reach Postgres as an
// invalid-input error that an authorization path cannot tell from a fault.
func TestMutationsWithoutAPrincipalAreRefusedBeforeTheDatabase(t *testing.T) {
	r := newRig(t)
	if _, err := r.svc.Grant(context.Background(), "  ", Actor{}, nil); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Grant err = %v, want ErrNotConfigured", err)
	}
	if _, err := r.svc.Revoke(context.Background(), "", Actor{}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Revoke err = %v, want ErrNotConfigured", err)
	}
	if err := r.app.ExpectationsWereMet(); err != nil {
		t.Errorf("a principal-less mutation reached the carrier: %v", err)
	}
	if err := r.identity.ExpectationsWereMet(); err != nil {
		t.Errorf("a principal-less mutation reached the identity store: %v", err)
	}
}

// IsPlatformAdmin is the raw carrier read the elevation path is built on.
func TestIsPlatformAdminReadsTheCarrier(t *testing.T) {
	r := newRig(t)
	r.app.ExpectQuery(`FROM "platform_admins" WHERE user_id`).WithArgs(testUserID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	isAdmin, err := r.svc.IsPlatformAdmin(context.Background(), testUserID)
	if err != nil || !isAdmin {
		t.Errorf("IsPlatformAdmin = (%v, %v), want (true, nil)", isAdmin, err)
	}
}

// The relay is what drains the outbox, so a constructed service must always have
// one: a nil relay would leave every audit intent written and never delivered,
// with nothing in the deployment reporting it.
func TestConstructedServiceAlwaysHasARelay(t *testing.T) {
	r := newRig(t)
	if r.svc.Relay() == nil {
		t.Fatal("a constructed service handed out no relay")
	}
}
