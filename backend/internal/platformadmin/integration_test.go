//go:build integration

// The half of this mechanism that only a real PostgreSQL can establish.
//
// sqlmock can show that a statement is ISSUED — the unit tests do, by matching
// on it. It cannot show that the constraint trigger refuses a commit, that
// FOR UPDATE actually makes a second revoker wait, that the migration's DDL
// produces a table these statements can run against, or that the relay's
// delivery is idempotent at the destination's primary key. Those are properties
// of Postgres, so they are asserted against Postgres.
//
// AND THE INTERLEAVING IS FORCED, NOT RACED. Registry recorded this lesson the
// expensive way: its two-goroutine concurrency test passed with AND without the
// row lock, because the window between "read the remaining grants" and "delete
// the row" is a few hundred microseconds and two goroutines started together do
// not reliably land inside it. A test that cannot fail without the fix is not
// evidence of the fix.
//
// So this pins the schedule, using the fact that Revoke calls the caller's own
// Predicate INSIDE the transaction, after the locking read and before the
// DELETE. The predicate is the parking spot, and the test waits until POSTGRES
// ITSELF reports the second revoker as blocked (pg_stat_activity.wait_event_type
// = 'Lock') rather than assuming it.
//
// TestIntegrationAPermissivePredicateReachesZeroAdmins is the falsification,
// kept permanently rather than run once and described in a commit message: the
// same two revocations, the same forced schedule, with the floor predicate
// replaced by one that accepts anything — reaching zero administrators.
//
// Run with:
//
//	TEST_DATABASE_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable' \
//	  go test -tags integration ./internal/platformadmin/...
package platformadmin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	identity "github.com/sethbacon/terraform-suite-identity/identity"
	idauditoutbox "github.com/sethbacon/terraform-suite-identity/identity/auditoutbox"
	idmodels "github.com/sethbacon/terraform-suite-identity/identity/models"
	idplatformadmin "github.com/sethbacon/terraform-suite-identity/identity/platformadmin"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	appdb "github.com/terraform-state-manager/terraform-state-manager/internal/db"
)

// testDatabaseName is this suite's OWN database, derived from TEST_DATABASE_URL.
// `go test ./...` runs package binaries concurrently and this suite drops and
// re-creates its schema, so sharing one database would make its result depend on
// another suite's timing.
const testDatabaseName = "tsm_platformadmin_test"

type env struct {
	appDB      *sql.DB
	identityDB *sql.DB
	svc        *Service
	users      *idstore.UserRepository
}

// newEnv builds the production topology against a real server: the app
// connection carrying TSM's migrations (and therefore the carrier and the
// outbox), and a separate identity connection whose search_path resolves
// unqualified names to the identity schema — exactly as cmd/server does.
func newEnv(t *testing.T) *env {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping PostgreSQL integration test")
	}

	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", dsn, err)
	}
	defer func() { _ = admin.Close() }()
	if err := admin.Ping(); err != nil {
		t.Skipf("TEST_DATABASE_URL is not reachable (%v); skipping", err)
	}
	// A fresh database per run, so a previous failure cannot make the next run
	// pass (or fail) for reasons that are not in this file.
	for _, stmt := range []string{
		`DROP DATABASE IF EXISTS ` + pq.QuoteIdentifier(testDatabaseName) + ` WITH (FORCE)`,
		`CREATE DATABASE ` + pq.QuoteIdentifier(testDatabaseName),
	} {
		if _, err := admin.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	appDB := connect(t, dsn, "")
	identityDB := connect(t, dsn, "identity,public")

	// The identity schema first: TSM runs identity.RunMigrations unconditionally
	// at boot, so the destination audit_logs always exists by the time the app's
	// migrations create the outbox that delivers into it.
	if err := identity.RunMigrations(identityDB, "up"); err != nil {
		t.Fatalf("identity migrations: %v", err)
	}
	if err := appdb.RunMigrations(appDB, "up"); err != nil {
		t.Fatalf("app migrations: %v", err)
	}

	svc, err := New(appDB, identityDB)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &env{appDB: appDB, identityDB: identityDB, svc: svc, users: idstore.NewUserRepository(identityDB)}
}

func connect(t *testing.T, dsn, searchPath string) *sql.DB {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	parsed.Path = "/" + testDatabaseName
	if searchPath != "" {
		q := parsed.Query()
		q.Set("search_path", searchPath)
		parsed.RawQuery = q.Encode()
	}
	db, err := sql.Open("postgres", parsed.String())
	if err != nil {
		t.Fatalf("open %s: %v", parsed.Redacted(), err)
	}
	// Room for the advisory-lock transaction, the revoking transaction and a
	// blocked second revoker at the same time. A pool that cannot hold all three
	// deadlocks on itself and reads as a lock bug.
	db.SetMaxOpenConns(10)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping %s: %v", parsed.Redacted(), err)
	}
	return db
}

// newUser creates a real identity user, because every property here that
// distinguishes an administrator from a leftover row depends on whether the id
// resolves.
func (e *env) newUser(t *testing.T, email string) string {
	t.Helper()
	u := &idmodels.User{Email: email, Name: email}
	if err := e.users.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("CreateUser(%s): %v", email, err)
	}
	return u.ID
}

func (e *env) deleteUser(t *testing.T, id string) {
	t.Helper()
	if _, err := e.identityDB.Exec(`DELETE FROM identity.users WHERE id = $1`, id); err != nil {
		t.Fatalf("delete user %s: %v", id, err)
	}
}

func (e *env) carrierCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.appDB.QueryRow(`SELECT count(*) FROM platform_admins`).Scan(&n); err != nil {
		t.Fatalf("count platform_admins: %v", err)
	}
	return n
}

func (e *env) grant(t *testing.T, userID, note string) {
	t.Helper()
	if _, err := e.svc.Grant(context.Background(), userID, Actor{}, &note); err != nil {
		t.Fatalf("Grant(%s): %v", userID, err)
	}
}

// --- the migration ------------------------------------------------------------

// TestIntegrationMigrationProducesTheShapeTheModuleRequires. VerifyTable and
// Verify are not smoke tests: they assert the column types, the nullability and
// the unique index the module's statements depend on, and they report the
// schema-qualified name each unqualified name actually resolved to.
func TestIntegrationMigrationProducesTheShapeTheModuleRequires(t *testing.T) {
	e := newEnv(t)
	if err := e.svc.Verify(context.Background()); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	carrier, err := e.svc.carrier.VerifyTable(context.Background())
	if err != nil {
		t.Fatalf("VerifyTable: %v", err)
	}
	if carrier != "public.platform_admins" {
		t.Errorf("carrier resolved to %q, want public.platform_admins on the app connection", carrier)
	}
	outbox, err := e.svc.outbox.Verify(context.Background())
	if err != nil {
		t.Fatalf("outbox Verify: %v", err)
	}
	if outbox != "public.audit_outbox" {
		t.Errorf("outbox resolved to %q, want public.audit_outbox beside the carrier", outbox)
	}
	// The DESTINATION is on the other connection, and that split is the reason
	// the outbox exists at all.
	sink, err := e.svc.sink.Verify(context.Background())
	if err != nil {
		t.Fatalf("sink Verify: %v", err)
	}
	if sink != "identity.audit_logs" {
		t.Errorf("audit destination resolved to %q, want identity.audit_logs", sink)
	}
}

// TestIntegrationMigrationRollsBackAndReapplies. A down migration nobody runs is
// a down migration nobody knows is broken — and this one has an order that
// matters (the trigger reads the outbox, so it must be dropped first).
func TestIntegrationMigrationRollsBackAndReapplies(t *testing.T) {
	e := newEnv(t)

	assertPresent := func(when string, want bool) {
		t.Helper()
		for _, object := range []struct {
			query string
			what  string
		}{
			{`SELECT to_regclass('public.platform_admins') IS NOT NULL`, "platform_admins"},
			{`SELECT to_regclass('public.audit_outbox') IS NOT NULL`, "audit_outbox"},
			{`SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'platform_admins_require_audit_intent')`, "the constraint trigger"},
			{`SELECT EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'audit_outbox_assert_intent')`, "the assertion function"},
		} {
			var present bool
			if err := e.appDB.QueryRow(object.query).Scan(&present); err != nil {
				t.Fatalf("%s (%s): %v", object.what, when, err)
			}
			if present != want {
				t.Errorf("%s %s: present = %v, want %v", object.what, when, present, want)
			}
		}
	}

	assertPresent("after the first up", true)
	if err := appdb.RunMigrations(e.appDB, "down"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	assertPresent("after the rollback", false)
	if err := appdb.RunMigrations(e.appDB, "up"); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	assertPresent("after the re-apply", true)

	// And the re-applied objects still satisfy the module's shape contract, which
	// is the part a hand-edited down/up pair silently loses.
	if err := e.svc.Verify(context.Background()); err != nil {
		t.Errorf("Verify after re-apply: %v", err)
	}
}

// --- the constraint trigger ---------------------------------------------------

// isCheckViolation reports whether err is the trigger's refusal. The DDL raises
// with ERRCODE 23514, so this asserts on the code rather than on message text,
// which is what makes the assertion survive a reworded RAISE.
func isCheckViolation(err error) bool {
	var pgErr *pq.Error
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}

// TestIntegrationTriggerRefusesUnauditedCarrierWrites is the property the whole
// migration exists for: not "the code writes an audit record", but "the database
// will not let a carrier change commit without one". It holds for a future
// handler that forgets, for a migration, and for the hand-written SQL the
// management API was built to replace.
func TestIntegrationTriggerRefusesUnauditedCarrierWrites(t *testing.T) {
	e := newEnv(t)
	victim := e.newUser(t, "victim@example.com")

	t.Run("insert with no intent", func(t *testing.T) {
		tx, err := e.appDB.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		// The INSERT itself SUCCEEDS: the trigger is DEFERRABLE INITIALLY
		// DEFERRED, so the refusal lands on COMMIT. Asserting on the statement
		// would report green against a trigger that was never deferred.
		if _, err := tx.Exec(`INSERT INTO platform_admins (user_id) VALUES ($1)`, victim); err != nil {
			t.Fatalf("the insert should be accepted and refused at commit, got %v", err)
		}
		err = tx.Commit()
		if err == nil {
			t.Fatal("an unaudited platform-admin grant committed; the trigger is inert")
		}
		if !isCheckViolation(err) {
			t.Errorf("commit failed with %v, want a 23514 check violation from the trigger", err)
		}
	})

	t.Run("delete with no intent", func(t *testing.T) {
		// Put a row in legitimately first.
		e.grant(t, victim, "seed")
		tx, err := e.appDB.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`DELETE FROM platform_admins WHERE user_id = $1`, victim); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if err := tx.Commit(); !isCheckViolation(err) {
			t.Errorf("commit failed with %v, want a 23514 check violation: a revocation with no "+
				"record is exactly what this trigger refuses", err)
		}
		if e.carrierCount(t) != 1 {
			t.Errorf("the refused delete still removed the row (count = %d)", e.carrierCount(t))
		}
	})

	t.Run("intent naming the wrong action", func(t *testing.T) {
		// An intent that merely MENTIONS the subject is not enough: without the
		// verbatim action match, a revocation could be committed under a grant's
		// audit record.
		other := e.newUser(t, "other@example.com")
		tx, err := e.appDB.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`INSERT INTO platform_admins (user_id) VALUES ($1)`, other); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if _, err := tx.Exec(
			`INSERT INTO audit_outbox (event_id, action, resource_type, resource_id)
			 VALUES ($1, $2, $3, $4)`,
			uuid.New().String(), idplatformadmin.AuditActionRevoked, idplatformadmin.AuditResourceType, other,
		); err != nil {
			t.Fatalf("intent insert: %v", err)
		}
		if err := tx.Commit(); !isCheckViolation(err) {
			t.Errorf("commit failed with %v, want 23514: a grant recorded as a revocation is not a record", err)
		}
	})

	t.Run("intent naming a different subject", func(t *testing.T) {
		other := e.newUser(t, "third@example.com")
		bystander := e.newUser(t, "bystander@example.com")
		tx, err := e.appDB.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`INSERT INTO platform_admins (user_id) VALUES ($1)`, other); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if _, err := tx.Exec(
			`INSERT INTO audit_outbox (event_id, action, resource_type, resource_id)
			 VALUES ($1, $2, $3, $4)`,
			uuid.New().String(), idplatformadmin.AuditActionGranted, idplatformadmin.AuditResourceType, bystander,
		); err != nil {
			t.Fatalf("intent insert: %v", err)
		}
		if err := tx.Commit(); !isCheckViolation(err) {
			t.Errorf("commit failed with %v, want 23514: an intent about somebody else does not "+
				"account for this grant", err)
		}
	})

	t.Run("intent from an earlier transaction does not satisfy it", func(t *testing.T) {
		// This is why the trigger matches pg_current_xact_id() and not a foreign
		// key: a foreign key would be satisfied by an intent written days ago.
		other := e.newUser(t, "fourth@example.com")
		if _, err := e.appDB.Exec(
			`INSERT INTO audit_outbox (event_id, action, resource_type, resource_id)
			 VALUES ($1, $2, $3, $4)`,
			uuid.New().String(), idplatformadmin.AuditActionGranted, idplatformadmin.AuditResourceType, other,
		); err != nil {
			t.Fatalf("pre-written intent: %v", err)
		}
		tx, err := e.appDB.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`INSERT INTO platform_admins (user_id) VALUES ($1)`, other); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if err := tx.Commit(); !isCheckViolation(err) {
			t.Errorf("commit failed with %v, want 23514: the intent must be written in THIS "+
				"transaction, which is the property no constraint can express", err)
		}
	})
}

// --- grant, revoke, and the floor ---------------------------------------------

func TestIntegrationGrantWritesItsProvenanceAndItsIntent(t *testing.T) {
	e := newEnv(t)
	actor := e.newUser(t, "actor@example.com")
	target := e.newUser(t, "target@example.com")

	note := "on-call rotation"
	grant, err := e.svc.Grant(context.Background(), target,
		Actor{UserID: actor, Email: "actor@example.com", IPAddress: "192.0.2.7"}, &note)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if grant.GrantedBy == nil || *grant.GrantedBy != actor {
		t.Errorf("granted_by = %v, want %s: provenance is what a table buys over a boolean", grant.GrantedBy, actor)
	}
	if grant.Note == nil || *grant.Note != note {
		t.Errorf("note = %v, want %q", grant.Note, note)
	}

	// The intent landed in the same transaction, so it is queryable now.
	var action, resourceID, ip string
	if err := e.appDB.QueryRow(
		`SELECT action, resource_id, ip_address FROM audit_outbox WHERE resource_id = $1`, target,
	).Scan(&action, &resourceID, &ip); err != nil {
		t.Fatalf("read intent: %v", err)
	}
	if action != idplatformadmin.AuditActionGranted {
		t.Errorf("action = %q, want %q", action, idplatformadmin.AuditActionGranted)
	}
	if ip != "192.0.2.7" {
		t.Errorf("ip_address = %q, want the request's client address", ip)
	}

	// A re-grant leaves the original row ALONE rather than overwriting who
	// conferred the privilege.
	other := "someone else entirely"
	if _, err := e.svc.Grant(context.Background(), target, Actor{UserID: target}, &other); !errors.Is(err, idplatformadmin.ErrAlreadyPlatformAdmin) {
		t.Fatalf("re-grant err = %v, want ErrAlreadyPlatformAdmin", err)
	}
	var storedNote string
	if err := e.appDB.QueryRow(`SELECT note FROM platform_admins WHERE user_id = $1`, target).Scan(&storedNote); err != nil {
		t.Fatalf("read note: %v", err)
	}
	if storedNote != note {
		t.Errorf("note = %q after a re-grant, want the original %q preserved", storedNote, note)
	}
}

// TestIntegrationFloorRefusesTheLastAdministrator: a deployment that revokes its
// last administrator has no recovery path short of hand-written SQL against the
// very table this API exists to replace.
func TestIntegrationFloorRefusesTheLastAdministrator(t *testing.T) {
	e := newEnv(t)
	only := e.newUser(t, "only@example.com")
	e.grant(t, only, "the only one")

	_, err := e.svc.Revoke(context.Background(), only, Actor{UserID: only})
	if !errors.Is(err, idplatformadmin.ErrLastPlatformAdmin) {
		t.Fatalf("err = %v, want ErrLastPlatformAdmin", err)
	}
	if n := e.carrierCount(t); n != 1 {
		t.Errorf("carrier holds %d rows, want the refused revocation to have changed nothing", n)
	}
}

// TestIntegrationAnOrphanedGrantIsNotAnAdministrator. The row count says two; the
// exercisable count says one. Counting rows — the version that needs no resolver
// and looks so much simpler — would let the last real administrator revoke
// themselves against a count of two.
func TestIntegrationAnOrphanedGrantIsNotAnAdministrator(t *testing.T) {
	e := newEnv(t)
	live := e.newUser(t, "live@example.com")
	doomed := e.newUser(t, "doomed@example.com")
	e.grant(t, live, "live")
	e.grant(t, doomed, "about to be deleted")

	// The carrier holds no foreign key to identity, so the grant survives the user.
	e.deleteUser(t, doomed)
	if n := e.carrierCount(t); n != 2 {
		t.Fatalf("carrier holds %d rows, want 2: the grant must OUTLIVE the deleted user, "+
			"which is what makes the resolver necessary", n)
	}

	if _, err := e.svc.Revoke(context.Background(), live, Actor{}); !errors.Is(err, idplatformadmin.ErrLastPlatformAdmin) {
		t.Fatalf("err = %v, want ErrLastPlatformAdmin: the remaining grant names nobody", err)
	}

	// And the listing SHOWS the orphan rather than hiding it, because this is the
	// only surface that can remove it.
	entries, err := e.svc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var orphans int
	for _, entry := range entries {
		if !entry.Exists {
			orphans++
		}
	}
	if orphans != 1 {
		t.Errorf("List reported %d orphans of %d entries, want exactly 1", orphans, len(entries))
	}
}

func TestIntegrationRevokeSucceedsWhenAnotherAdministratorRemains(t *testing.T) {
	e := newEnv(t)
	a := e.newUser(t, "a@example.com")
	b := e.newUser(t, "b@example.com")
	e.grant(t, a, "a")
	e.grant(t, b, "b")

	if _, err := e.svc.Revoke(context.Background(), a, Actor{UserID: b, Email: "b@example.com"}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if n := e.carrierCount(t); n != 1 {
		t.Errorf("carrier holds %d rows, want 1", n)
	}
	var action string
	if err := e.appDB.QueryRow(
		`SELECT action FROM audit_outbox WHERE resource_id = $1 AND action = $2`,
		a, idplatformadmin.AuditActionRevoked,
	).Scan(&action); err != nil {
		t.Fatalf("the revocation wrote no intent: %v", err)
	}
}

// --- forced interleaving ------------------------------------------------------

// parkingFloor wraps the real predicate so a test can hold a revoking
// transaction open at the exact point between the locking read and the DELETE.
func parkingFloor(parked chan<- struct{}, release <-chan struct{}) func(idplatformadmin.Resolver) idplatformadmin.Predicate {
	var once sync.Once
	return func(r idplatformadmin.Resolver) idplatformadmin.Predicate {
		real := idplatformadmin.RequireAnotherExercisableAdmin(r)
		return func(ctx context.Context, remaining []idplatformadmin.Grant) error {
			once.Do(func() {
				close(parked)
				<-release
			})
			return real(ctx, remaining)
		}
	}
}

// waitForBlockedBackend waits until Postgres reports at least one backend on this
// database waiting on a lock. Observing the block rather than sleeping is what
// keeps the schedule pinned instead of merely likely.
func waitForBlockedBackend(t *testing.T, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var blocked int
		if err := db.QueryRow(
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database() AND wait_event_type = 'Lock'`,
		).Scan(&blocked); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if blocked > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no backend ever blocked: the second revoker did not wait, so this test is not " +
		"exercising the serialisation it claims to")
}

// TestIntegrationConcurrentRevocationsCannotReachZero.
//
// Two administrators, two revocations, one forced schedule:
//
//  1. revoker A enters Revoke, takes the carrier under FOR UPDATE, and parks
//     inside its predicate
//  2. revoker B enters Revoke and blocks
//  3. the test waits until Postgres reports B as waiting on a lock
//  4. A is released, sees two administrators, removes one, commits
//  5. B wakes, reads ONE administrator, and refuses
//
// Without the serialisation both would see the other still standing, both would
// pass the floor, and the deployment would end with zero administrators — two
// well-formed requests and no error anywhere.
func TestIntegrationConcurrentRevocationsCannotReachZero(t *testing.T) {
	e := newEnv(t)
	a := e.newUser(t, "concurrent-a@example.com")
	b := e.newUser(t, "concurrent-b@example.com")
	e.grant(t, a, "a")
	e.grant(t, b, "b")

	parked := make(chan struct{})
	release := make(chan struct{})

	// Two services over the SAME tables: one parks in its predicate, the other
	// runs the production floor. Same table name, so the same advisory-lock key.
	parking, err := New(e.appDB, e.identityDB)
	if err != nil {
		t.Fatalf("New (parking): %v", err)
	}
	parking.floor = parkingFloor(parked, release)

	var errA, errB error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, errA = parking.Revoke(context.Background(), a, Actor{UserID: b})
	}()

	<-parked

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, errB = e.svc.Revoke(context.Background(), b, Actor{UserID: a})
	}()

	waitForBlockedBackend(t, e.appDB)
	close(release)
	wg.Wait()

	if errA != nil {
		t.Errorf("the first revocation failed: %v", errA)
	}
	if !errors.Is(errB, idplatformadmin.ErrLastPlatformAdmin) {
		t.Errorf("the second revocation err = %v, want ErrLastPlatformAdmin", errB)
	}
	if n := e.carrierCount(t); n != 1 {
		t.Fatalf("carrier holds %d rows, want exactly 1 administrator left standing", n)
	}
}

// TestIntegrationAPermissivePredicateReachesZeroAdmins is the FALSIFICATION, and
// it is kept rather than run once and described in a commit message.
//
// Same two administrators, same forced schedule, the only difference being a
// predicate that accepts anything — which is what "count the rows" or "no floor
// configured" amounts to. It reaches zero administrators. That is what the test
// above would look like if the floor were not doing the work, and it is why
// RequireAnotherExercisableAdmin is passed explicitly rather than defaulted.
func TestIntegrationAPermissivePredicateReachesZeroAdmins(t *testing.T) {
	e := newEnv(t)
	a := e.newUser(t, "permissive-a@example.com")
	b := e.newUser(t, "permissive-b@example.com")
	e.grant(t, a, "a")
	e.grant(t, b, "b")

	permissive := func(idplatformadmin.Resolver) idplatformadmin.Predicate {
		return func(context.Context, []idplatformadmin.Grant) error { return nil }
	}
	first, err := New(e.appDB, e.identityDB)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	first.floor = permissive
	second, err := New(e.appDB, e.identityDB)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	second.floor = permissive

	if _, err := first.Revoke(context.Background(), a, Actor{}); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	if _, err := second.Revoke(context.Background(), b, Actor{}); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if n := e.carrierCount(t); n != 0 {
		t.Fatalf("carrier holds %d rows; this test is meant to DEMONSTRATE the lockout a "+
			"permissive predicate allows, and it did not happen — so the assertion in "+
			"TestIntegrationConcurrentRevocationsCannotReachZero is proving less than it claims", n)
	}
}

// --- the relay ----------------------------------------------------------------

// TestIntegrationRelayDeliversIntentsToTheAuditLog closes the loop: the intent
// written on the app connection reaches identity.audit_logs on the other one,
// keyed by the event id chosen before the mutation committed.
func TestIntegrationRelayDeliversIntentsToTheAuditLog(t *testing.T) {
	e := newEnv(t)
	actor := e.newUser(t, "relay-actor@example.com")
	target := e.newUser(t, "relay-target@example.com")
	note := "delivered"
	if _, err := e.svc.Grant(context.Background(), target,
		Actor{UserID: actor, Email: "relay-actor@example.com", IPAddress: "198.51.100.4"}, &note); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	var eventID string
	if err := e.appDB.QueryRow(`SELECT event_id FROM audit_outbox WHERE resource_id = $1`, target).Scan(&eventID); err != nil {
		t.Fatalf("read intent: %v", err)
	}

	relay := e.svc.Relay()
	claimed, delivered, err := relay.DeliverBatch(context.Background())
	if err != nil {
		t.Fatalf("DeliverBatch: %v", err)
	}
	if claimed != 1 || delivered != 1 {
		t.Fatalf("claimed %d / delivered %d, want 1/1", claimed, delivered)
	}

	var action, resourceType, resourceID, ip, actorEmail string
	var userID string
	if err := e.identityDB.QueryRow(
		`SELECT action, resource_type, resource_id, ip_address, user_id, actor_email
		   FROM identity.audit_logs WHERE id = $1`, eventID,
	).Scan(&action, &resourceType, &resourceID, &ip, &userID, &actorEmail); err != nil {
		t.Fatalf("the intent never reached identity.audit_logs: %v", err)
	}
	if action != idplatformadmin.AuditActionGranted || resourceType != idplatformadmin.AuditResourceType || resourceID != target {
		t.Errorf("delivered row = (%s, %s, %s), want the grant it describes", action, resourceType, resourceID)
	}
	if userID != actor || actorEmail != "relay-actor@example.com" {
		t.Errorf("delivered actor = (%s, %s), want the acting principal as the request knew it", userID, actorEmail)
	}
	if ip != "198.51.100.4" {
		t.Errorf("ip_address = %q, want the request's client address", ip)
	}

	// Delivery is at-least-once in transport. Re-delivering the SAME event id
	// must be a no-op at the destination's primary key, or a crash between the
	// destination write and the commit would duplicate the audit record.
	if err := e.svc.sink.Deliver(context.Background(), idauditoutbox.Intent{
		EventID:    eventID,
		Action:     idplatformadmin.AuditActionGranted,
		OccurredAt: time.Now(),
	}); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	var rows int
	if err := e.identityDB.QueryRow(`SELECT count(*) FROM identity.audit_logs WHERE id = $1`, eventID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Errorf("redelivery produced %d rows, want exactly 1: at-least-once transport has to be "+
			"exactly-once in effect", rows)
	}

	// Nothing is left in the backlog, and a delivered intent is marked rather
	// than deleted on the delivery path.
	backlog, err := e.svc.outbox.Backlog(context.Background())
	if err != nil {
		t.Fatalf("Backlog: %v", err)
	}
	if backlog.Pending != 0 {
		t.Errorf("backlog pending = %d, want 0 after a successful cycle", backlog.Pending)
	}
}

// A destination that refuses the write must RETAIN the intent. Dropping it would
// destroy the record the whole design exists to keep, and the backlog — reported
// and alarmed on — is how the failure becomes visible instead.
//
// The failure injected here is the realistic one: the destination is not where
// the sink is looking. identity.audit_logs carries NO foreign keys on user_id or
// organization_id (identity migration 000007 removed them so an entry outlives
// the principal it describes), and its column types are the outbox's own, so no
// VALUE can be made undeliverable — which is a property worth having and a
// reason this test injects a routing failure rather than a bad row.
func TestIntegrationUndeliverableIntentStaysInTheBacklog(t *testing.T) {
	e := newEnv(t)
	target := e.newUser(t, "backlog@example.com")
	e.grant(t, target, "will not deliver yet")

	if _, err := e.identityDB.Exec(`ALTER TABLE identity.audit_logs RENAME TO audit_logs_moved`); err != nil {
		t.Fatalf("move the destination: %v", err)
	}

	claimed, delivered, err := e.svc.Relay().DeliverBatch(context.Background())
	if err != nil {
		t.Fatalf("DeliverBatch: %v", err)
	}
	if claimed != 1 || delivered != 0 {
		t.Fatalf("claimed %d / delivered %d, want 1/0", claimed, delivered)
	}

	backlog, err := e.svc.outbox.Backlog(context.Background())
	if err != nil {
		t.Fatalf("Backlog: %v", err)
	}
	if backlog.Pending != 1 || backlog.Failed != 1 {
		t.Errorf("backlog = %+v, want 1 pending and 1 failed: an audit intent is never dropped "+
			"for failing to deliver", backlog)
	}
	var lastError sql.NullString
	if err := e.appDB.QueryRow(`SELECT last_error FROM audit_outbox WHERE resource_id = $1`, target).Scan(&lastError); err != nil {
		t.Fatalf("read last_error: %v", err)
	}
	if !lastError.Valid || !strings.Contains(lastError.String, "resolves to nothing") {
		t.Errorf("last_error = %v, want the destination's own refusal recorded for the operator", lastError)
	}

	// And the retained intent DELIVERS once the destination is reachable again:
	// a backlog that cannot drain is not a queue, it is a loss with extra steps.
	if _, err := e.identityDB.Exec(`ALTER TABLE identity.audit_logs_moved RENAME TO audit_logs`); err != nil {
		t.Fatalf("restore the destination: %v", err)
	}
	if _, delivered, err = e.svc.Relay().DeliverBatch(context.Background()); err != nil {
		t.Fatalf("DeliverBatch after recovery: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("delivered %d after the destination came back, want 1", delivered)
	}
	backlog, err = e.svc.outbox.Backlog(context.Background())
	if err != nil {
		t.Fatalf("Backlog: %v", err)
	}
	if backlog.Pending != 0 {
		t.Errorf("backlog pending = %d after recovery, want 0", backlog.Pending)
	}
}

// A small guard against the one wiring mistake that would look like everything
// working until the first grant: an outbox on the identity connection instead of
// the app's. The mutation and its intent would then be in different transactions
// and the constraint trigger would refuse every commit.
func TestIntegrationOutboxSharesTheCarriersConnection(t *testing.T) {
	e := newEnv(t)
	if e.svc.outbox.DB() != e.appDB {
		t.Fatal("the outbox is not on the connection the carrier mutates through")
	}
	if fmt.Sprint(e.svc.sink.Table()) != AuditLogsTable {
		t.Errorf("sink table = %q, want %q", e.svc.sink.Table(), AuditLogsTable)
	}
}
