package repositories

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5/pgconn"
)

// testOrgID is an arbitrary organization for the Create paths, which now refuse
// an empty one (#436).
const testOrgID = "11111111-1111-4111-8111-111111111111"

// errDB is a shared sentinel used to drive error paths (registry pattern).
var errDB = errors.New("db error")

func newMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := newSQLMock()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, mock
}

var ctx = context.Background()

// ---------------------------------------------------------------------------
// SourceRepository
// ---------------------------------------------------------------------------

// organization_id is LAST and is part of the fixture, not an afterthought: the
// projection selects it, so a fixture that omits it makes every scan fail. That
// is the desirable direction of failure -- a fixture cannot silently keep
// passing while the statement stops selecting a column (#436).
var sourceCols = []string{"id", "name", "type", "endpoint", "config", "scope", "encrypted_credentials", "created_at", "updated_at", "organization_id"}

func sourceRow() *sqlmock.Rows {
	return sqlmock.NewRows(sourceCols).
		AddRow("s1", "demo", "local", "", []byte(`{"base_path":"/data"}`), []byte(`{}`), nil, "2026-06-10", "2026-06-10", testOrgID)
}

func TestSourceRepository_List(t *testing.T) {
	db, mock := newMock(t)
	r := NewSourceRepository(db)

	mock.ExpectQuery("SELECT .+ FROM state_sources ORDER BY created_at DESC").WillReturnRows(sourceRow())
	out, err := r.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 1 || out[0].Name != "demo" || out[0].Config["base_path"] != "/data" {
		t.Errorf("unexpected sources: %+v", out)
	}

	mock.ExpectQuery("SELECT .+ FROM state_sources").WillReturnError(errDB)
	if _, err := r.List(ctx); err == nil {
		t.Error("List swallowed the query error")
	}
}

func TestSourceRepository_GetByID(t *testing.T) {
	db, mock := newMock(t)
	r := NewSourceRepository(db)

	mock.ExpectQuery("SELECT .+ FROM state_sources WHERE id").WithArgs("s1").WillReturnRows(sourceRow())
	s, err := r.GetByID(ctx, "s1")
	if err != nil || s == nil || s.ID != "s1" {
		t.Fatalf("GetByID: %v %+v", err, s)
	}

	mock.ExpectQuery("SELECT .+ FROM state_sources WHERE id").WithArgs("missing").
		WillReturnError(sql.ErrNoRows)
	s, err = r.GetByID(ctx, "missing")
	if err != nil || s != nil {
		t.Errorf("missing source should be (nil, nil), got %+v %v", s, err)
	}
}

func TestSourceRepository_CreateAndDelete(t *testing.T) {
	db, mock := newMock(t)
	r := NewSourceRepository(db)

	mock.ExpectQuery("INSERT INTO state_sources").
		WithArgs("demo", "local", nil, `{"base_path":"/data"}`, "{}", nil, testOrgID).
		WillReturnRows(sourceRow())
	created, err := r.Create(ctx, &Source{Name: "demo", Type: "local", Config: map[string]any{"base_path": "/data"}}, testOrgID)
	if err != nil || created.ID != "s1" {
		t.Fatalf("Create: %v %+v", err, created)
	}

	mock.ExpectQuery("INSERT INTO state_sources").WillReturnError(errDB)
	if _, err := r.Create(ctx, &Source{Name: "x", Type: "local"}, testOrgID); err == nil {
		t.Error("Create swallowed the insert error")
	}

	mock.ExpectExec("DELETE FROM state_sources").WithArgs("s1").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.Delete(ctx, "s1"); err != nil {
		t.Errorf("Delete: %v", err)
	}
}

func TestSourceRepository_Update(t *testing.T) {
	db, mock := newMock(t)
	r := NewSourceRepository(db)

	// Blank credentials pass NULL so the CASE keeps the stored secret.
	mock.ExpectQuery("UPDATE state_sources SET").
		WithArgs("s1", "renamed", nil, `{"base_path":"/new"}`, "{}", nil).
		WillReturnRows(sourceRow())
	updated, err := r.Update(ctx, &Source{ID: "s1", Name: "renamed", Config: map[string]any{"base_path": "/new"}})
	if err != nil || updated == nil || updated.ID != "s1" {
		t.Fatalf("Update: %v %+v", err, updated)
	}

	// New credentials replace the blob.
	mock.ExpectQuery("UPDATE state_sources SET").
		WithArgs("s1", "renamed", nil, "{}", "{}", []byte("sealed")).
		WillReturnRows(sourceRow())
	if _, err := r.Update(ctx, &Source{ID: "s1", Name: "renamed", EncryptedCredentials: []byte("sealed")}); err != nil {
		t.Fatalf("Update with creds: %v", err)
	}

	// Unknown id -> (nil, nil); db error surfaces.
	mock.ExpectQuery("UPDATE state_sources SET").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "type", "endpoint", "config", "scope", "encrypted_credentials", "created_at", "updated_at"}))
	if got, err := r.Update(ctx, &Source{ID: "ghost", Name: "x"}); err != nil || got != nil {
		t.Errorf("ghost update = %v, %v; want nil, nil", got, err)
	}
	mock.ExpectQuery("UPDATE state_sources SET").WillReturnError(errDB)
	if _, err := r.Update(ctx, &Source{ID: "s1", Name: "x"}); err == nil {
		t.Error("Update swallowed the error")
	}
}

// ---------------------------------------------------------------------------
// StateLockRepository
// ---------------------------------------------------------------------------

func TestStateLockRepository_AcquireRelease(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateLockRepository(db)

	// Acquire reaps any TTL-expired lock first (a crashed holder must not wedge
	// the key forever), then inserts.
	mock.ExpectExec("DELETE FROM state_locks").WithArgs("s1", "k", staleLockTTL).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("INSERT INTO state_locks").WithArgs("s1", "k", "alice").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("lock-1"))
	id, err := r.Acquire(ctx, "s1", "k", "alice")
	if err != nil || id != "lock-1" {
		t.Fatalf("Acquire: %v %q", err, id)
	}

	// A unique-constraint violation must map to ErrLocked, not a raw pq error,
	// and the conflict names the current holder.
	mock.ExpectExec("DELETE FROM state_locks").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("INSERT INTO state_locks").
		WillReturnError(&pgconn.PgError{Code: "23505"})
	mock.ExpectQuery("SELECT COALESCE").WithArgs("s1", "k").
		WillReturnRows(sqlmock.NewRows([]string{"actor", "acquired_at"}).AddRow("alice", "2026-06-11 10:00:00"))
	_, err = r.Acquire(ctx, "s1", "k", "bob")
	if !errors.Is(err, ErrLocked) {
		t.Errorf("expected ErrLocked on unique violation, got %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "alice") {
		t.Errorf("conflict should name the holder, got %v", err)
	}

	mock.ExpectExec("DELETE FROM state_locks").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("INSERT INTO state_locks").WillReturnError(errDB)
	if _, err := r.Acquire(ctx, "s1", "k", "bob"); errors.Is(err, ErrLocked) || err == nil {
		t.Errorf("non-unique errors must pass through, got %v", err)
	}

	mock.ExpectExec("DELETE FROM state_locks").WithArgs("lock-1", "s1", "k").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.Release(ctx, "s1", "k", "lock-1"); err != nil {
		t.Errorf("Release: %v", err)
	}

	// ForceRelease deletes whatever holds the key and reports whether a lock
	// existed (the admin force-unlock endpoint relays this).
	mock.ExpectExec("DELETE FROM state_locks").WithArgs("s1", "k").
		WillReturnResult(sqlmock.NewResult(0, 1))
	released, err := r.ForceRelease(ctx, "s1", "k")
	if err != nil || !released {
		t.Errorf("ForceRelease: %v released=%v", err, released)
	}
	mock.ExpectExec("DELETE FROM state_locks").WithArgs("s1", "nope").
		WillReturnResult(sqlmock.NewResult(0, 0))
	released, err = r.ForceRelease(ctx, "s1", "nope")
	if err != nil || released {
		t.Errorf("ForceRelease on unlocked key: %v released=%v", err, released)
	}
}

func TestStateLockRepository_RenewHeartbeat(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateLockRepository(db)

	// A live lock renews (rows=1 -> alive).
	mock.ExpectExec("UPDATE state_locks SET renewed_at").WithArgs("lock-1", "s1", "k").
		WillReturnResult(sqlmock.NewResult(0, 1))
	alive, err := r.Renew(ctx, "s1", "k", "lock-1")
	if err != nil || !alive {
		t.Errorf("Renew live: %v alive=%v", err, alive)
	}

	// A reaped/force-released lock reports gone (rows=0) so renewal stops.
	mock.ExpectExec("UPDATE state_locks SET renewed_at").WithArgs("lock-1", "s1", "k").
		WillReturnResult(sqlmock.NewResult(0, 0))
	alive, err = r.Renew(ctx, "s1", "k", "lock-1")
	if err != nil || alive {
		t.Errorf("Renew gone: %v alive=%v", err, alive)
	}
}

func TestStateLockRepository_KeepAlive(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateLockRepository(db)

	// First tick renews; second tick finds the row gone and the loop exits on
	// its own (force-release/reap while the operation ran).
	mock.ExpectExec("UPDATE state_locks SET renewed_at").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE state_locks SET renewed_at").WillReturnResult(sqlmock.NewResult(0, 0))

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		r.KeepAlive("s1", "k", "lock-1", 5*time.Millisecond, stop)
		close(done)
	}()
	select {
	case <-done: // exited after the rows=0 tick
	case <-time.After(2 * time.Second):
		t.Fatal("KeepAlive did not exit after the lock disappeared")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expected two renewals: %v", err)
	}

	// The stop channel ends the loop without further queries.
	stop2 := make(chan struct{})
	done2 := make(chan struct{})
	go func() {
		r.KeepAlive("s1", "k", "lock-1", time.Hour, stop2)
		close(done2)
	}()
	close(stop2)
	select {
	case <-done2:
	case <-time.After(2 * time.Second):
		t.Fatal("KeepAlive did not exit on stop")
	}
}

// ---------------------------------------------------------------------------
// CISourceRepository
// ---------------------------------------------------------------------------

var ciCols = []string{"id", "name", "provider", "organization", "project", "auth_method", "encrypted_token", "tenant_id", "client_id", "encrypted_client_secret", "github_app_id", "github_installation_id", "encrypted_app_private_key", "created_at", "updated_at"}

func ciRow() *sqlmock.Rows {
	proj := "Platform"
	return sqlmock.NewRows(ciCols).
		AddRow("c1", "corp-ado", "azuredevops", "corp", proj, "pat", []byte("sealed"), nil, nil, nil, nil, nil, nil, "2026-06-10", "2026-06-10")
}

func TestCISourceRepository_CRUD(t *testing.T) {
	db, mock := newMock(t)
	r := NewCISourceRepository(db)

	mock.ExpectQuery("SELECT .+ FROM ci_sources ORDER BY name").WillReturnRows(ciRow())
	list, err := r.List(ctx)
	if err != nil || len(list) != 1 || list[0].Provider != "azuredevops" {
		t.Fatalf("List: %v %+v", err, list)
	}

	mock.ExpectQuery("SELECT .+ FROM ci_sources WHERE id").WithArgs("missing").WillReturnError(sql.ErrNoRows)
	if s, err := r.GetByID(ctx, "missing"); err != nil || s != nil {
		t.Errorf("missing CI source should be (nil, nil), got %+v %v", s, err)
	}

	mock.ExpectQuery("INSERT INTO ci_sources").WillReturnRows(ciRow())
	created, err := r.Create(ctx, &CISource{Name: "corp-ado", Provider: "azuredevops", Organization: "corp"}, testOrgID)
	if err != nil || created.ID != "c1" {
		t.Fatalf("Create: %v %+v", err, created)
	}

	mock.ExpectExec("DELETE FROM ci_sources").WithArgs("c1").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.Delete(ctx, "c1"); err != nil {
		t.Errorf("Delete: %v", err)
	}
}

// ---------------------------------------------------------------------------
// PipelineRepository
// ---------------------------------------------------------------------------

var pipelineCols = []string{"id", "name", "provider", "config", "encrypted_token", "created_at", "updated_at", "organization_id"}

func pipelineRow() *sqlmock.Rows {
	return sqlmock.NewRows(pipelineCols).
		AddRow("p1", "drift-ci", "github", []byte(`{"repo":"org/repo"}`), nil, "2026-06-10", "2026-06-10", testOrgID)
}

func TestPipelineRepository_CRUD(t *testing.T) {
	db, mock := newMock(t)
	r := NewPipelineRepository(db)

	mock.ExpectQuery("SELECT .+ FROM pipeline_connections ORDER BY created_at DESC").WillReturnRows(pipelineRow())
	list, err := r.List(ctx)
	if err != nil || len(list) != 1 || list[0].Config["repo"] != "org/repo" {
		t.Fatalf("List: %v %+v", err, list)
	}

	mock.ExpectQuery("SELECT .+ FROM pipeline_connections WHERE id").WithArgs("p1").WillReturnRows(pipelineRow())
	p, err := r.GetByID(ctx, "p1")
	if err != nil || p == nil || p.Name != "drift-ci" {
		t.Fatalf("GetByID: %v %+v", err, p)
	}

	mock.ExpectQuery("SELECT .+ FROM pipeline_connections WHERE id").WithArgs("nope").WillReturnError(sql.ErrNoRows)
	if p, err := r.GetByID(ctx, "nope"); err != nil || p != nil {
		t.Errorf("missing pipeline should be (nil, nil), got %+v %v", p, err)
	}

	mock.ExpectQuery("INSERT INTO pipeline_connections").
		WithArgs("drift-ci", "github", `{"repo":"org/repo"}`, []byte("tok"), testOrgID).
		WillReturnRows(pipelineRow())
	created, err := r.Create(ctx, &PipelineConnection{
		Name: "drift-ci", Provider: "github",
		Config:         map[string]any{"repo": "org/repo"},
		EncryptedToken: []byte("tok"),
	}, testOrgID)
	if err != nil || created.ID != "p1" {
		t.Fatalf("Create: %v %+v", err, created)
	}

	mock.ExpectQuery("UPDATE pipeline_connections").
		WithArgs("p1", "renamed", `{"repo":"org/repo"}`, false, nil).
		WillReturnRows(pipelineRow())
	updated, err := r.Update(ctx, &PipelineConnection{
		ID: "p1", Name: "renamed", Config: map[string]any{"repo": "org/repo"},
	}, false)
	if err != nil || updated == nil || updated.ID != "p1" {
		t.Fatalf("Update: %v %+v", err, updated)
	}

	mock.ExpectQuery("UPDATE pipeline_connections").WillReturnError(sql.ErrNoRows)
	if u, err := r.Update(ctx, &PipelineConnection{ID: "nope", Name: "x"}, false); err != nil || u != nil {
		t.Errorf("Update of missing row should be (nil, nil), got %+v %v", u, err)
	}

	mock.ExpectExec("DELETE FROM pipeline_connections").WithArgs("p1").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.Delete(ctx, "p1"); err != nil {
		t.Errorf("Delete: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TransferRepository
// ---------------------------------------------------------------------------

var transferCols = []string{"id", "mode", "source_id", "source_key", "target_source_id", "target_key", "status", "verified", "decommissioned", "detail", "actor", "created_at"}

func TestTransferRepository(t *testing.T) {
	db, mock := newMock(t)
	r := NewTransferRepository(db)

	row := sqlmock.NewRows(transferCols).
		AddRow("t1", "migrate", "s1", "k", "s2", "k2", "success", true, true, "", "alice", "2026-06-10")
	mock.ExpectQuery("INSERT INTO state_transfers").WillReturnRows(row)
	created, err := r.Create(ctx, &Transfer{Mode: "migrate", SourceID: "s1", SourceKey: "k", TargetSourceID: "s2", TargetKey: "k2", Status: "success"}, testOrgID)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Verified == nil || !*created.Verified || !created.Decommissioned {
		t.Errorf("verified/decommissioned not mapped: %+v", created)
	}

	// NULL verified must stay nil.
	row2 := sqlmock.NewRows(transferCols).
		AddRow("t2", "backup", "s1", "k", "s2", "k2", "success", nil, false, "", "", "2026-06-10")
	mock.ExpectQuery("SELECT .+ FROM state_transfers WHERE id").WithArgs("t2").WillReturnRows(row2)
	got, err := r.GetByID(ctx, "t2")
	if err != nil || got == nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Verified != nil {
		t.Errorf("NULL verified should map to nil, got %+v", got.Verified)
	}

	mock.ExpectQuery("SELECT .+ FROM state_transfers WHERE id").WithArgs("nope").WillReturnError(sql.ErrNoRows)
	if tr, err := r.GetByID(ctx, "nope"); err != nil || tr != nil {
		t.Errorf("missing transfer should be (nil, nil), got %+v %v", tr, err)
	}
}

// ---------------------------------------------------------------------------
// SSOSettingsRepository
// ---------------------------------------------------------------------------

func TestSSOSettingsRepository(t *testing.T) {
	db, mock := newMock(t)
	appDB, appMock := newMock(t)
	r := NewSSOSettingsRepository(db, appDB)

	mock.ExpectQuery("SELECT oidc_group_claim_name").
		WillReturnRows(sqlmock.NewRows([]string{"oidc_group_claim_name", "oidc_default_role", "oidc_group_mappings", "updated_at"}).
			AddRow("groups", "viewer", []byte(`[{"group":"platform","organization":"default","role":"editor"}]`), "2026-06-10"))
	s, err := r.Get(ctx)
	if err != nil || s == nil {
		t.Fatalf("Get: %v", err)
	}
	if s.OIDCGroupClaimName != "groups" || len(s.OIDCGroupMappings) != 1 || s.OIDCGroupMappings[0].Role != "editor" {
		t.Errorf("overlay not mapped: %+v", s)
	}

	// No overlay saved yet → (nil, nil).
	mock.ExpectQuery("SELECT oidc_group_claim_name").WillReturnError(sql.ErrNoRows)
	if s, err := r.Get(ctx); err != nil || s != nil {
		t.Errorf("empty overlay should be (nil, nil), got %+v %v", s, err)
	}

	mock.ExpectExec("INSERT INTO sso_settings").
		WithArgs("groups", "viewer", []byte(`[]`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// The dual-write's mirror leg runs on the APP connection after the
	// authoritative upsert (terraform-suite-identity#206 phase 2): an empty
	// list clears the mirror. Pinned in depth in group_mapping_dual_write_test.go;
	// expected here so this test keeps modelling the repository's real traffic.
	appMock.ExpectBegin()
	appMock.ExpectExec("DELETE FROM group_mappings").WillReturnResult(sqlmock.NewResult(0, 0))
	appMock.ExpectCommit()
	if err := r.Upsert(ctx, &SSOSettings{OIDCGroupClaimName: "groups", OIDCDefaultRole: "viewer", OIDCGroupMappings: []SSOGroupMapping{}}); err != nil {
		t.Errorf("Upsert: %v", err)
	}
	if err := appMock.ExpectationsWereMet(); err != nil {
		t.Errorf("mirror leg missing: %v", err)
	}
}

// ---------------------------------------------------------------------------
// SourceRepository — bounded listing (#282)
// ---------------------------------------------------------------------------

func TestSourceListPage(t *testing.T) {
	db, mock := newMock(t)
	r := NewSourceRepository(db)

	mock.ExpectQuery(`SELECT .+ FROM state_sources ORDER BY created_at DESC\s+LIMIT`).
		WithArgs(500, 0).
		WillReturnRows(sqlmock.NewRows(sourceCols).
			AddRow("s1", "demo", "local", "", []byte(`{}`), []byte(`{}`), nil, "2026-06-10", "2026-06-10", testOrgID))

	got, err := r.ListPage(ctx, 500, 0)
	if err != nil {
		t.Fatalf("ListPage: %v", err)
	}
	if len(got) != 1 || got[0].Name != "demo" {
		t.Errorf("unexpected page: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestSourceCount(t *testing.T) {
	db, mock := newMock(t)
	r := NewSourceRepository(db)

	mock.ExpectQuery(`SELECT count\(\*\) FROM state_sources`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(42))

	n, err := r.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 42 {
		t.Errorf("count = %d, want 42", n)
	}
}

// TestSourceCreateRefusesAnUnownedRow is the whole of #436 in one assertion.
//
// A Postgres column DEFAULT applies only when the column is OMITTED, so leaving
// organization_id out of the INSERT falls through to
// tsm_default_organization_id() — indistinguishable from a successful stamp, and
// exactly how every row in the deployment came to belong to the default
// organization. Naming it with an empty value would be worse: that writes NULL,
// and a NULL organization is invisible to every tenant.
//
// So the only safe behaviour is to refuse, and to refuse BEFORE touching the
// database — asserted here by giving sqlmock no expectation at all, so any
// statement reaching it fails the test.
func TestSourceCreateRefusesAnUnownedRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	r := NewSourceRepository(db)

	for _, orgID := range []string{"", "   ", "\t"} {
		created, err := r.Create(context.Background(), &Source{Name: "x", Type: "local"}, orgID)
		if !errors.Is(err, ErrNoOrganization) {
			t.Errorf("organizationID=%q: err = %v, want ErrNoOrganization", orgID, err)
		}
		if created != nil {
			t.Errorf("organizationID=%q: returned a source despite refusing", orgID)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestCISourceCreateStampsTheTenancyColumnNotTheRemoteOwner is the trap
// migration 000033 names explicitly, asserted.
//
// ci_sources already has a column called `organization` — the Azure DevOps
// organization or GitHub owner, a remote coordinate STRING. The tenancy column
// added by 000033 is `organization_id`, a uuid naming an identity organization.
// Two different concepts, one letter apart, in one INSERT.
//
// Stamping the wrong one is a silent no-op for tenancy: the row still gets an
// `organization` (it always did), `organization_id` receives a string that is
// not a uuid, and nothing at the call site looks wrong. This pins both arguments
// so the two cannot be transposed.
func TestCISourceCreateStampsTheTenancyColumnNotTheRemoteOwner(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	r := NewCISourceRepository(db)

	mock.ExpectQuery("INSERT INTO ci_sources").
		// $3 is `organization`, the ADO/GitHub owner. $13 is `organization_id`,
		// the tenancy uuid. Both named, so transposing them fails here.
		// Only the two columns this test is about are pinned. The credential
		// blobs in between are AnyArg: asserting their exact nil-vs-empty shape
		// would make this test fail for reasons that have nothing to do with the
		// transposition it exists to catch.
		WithArgs("corp-ado", "azuredevops", "corp-ado-org",
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), testOrgID).
		WillReturnRows(ciRow())

	if _, err := r.Create(context.Background(), &CISource{
		Name:         "corp-ado",
		Provider:     "azuredevops",
		Organization: "corp-ado-org", // deliberately NOT the tenancy uuid
	}, testOrgID); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the INSERT did not carry both columns as expected — the remote owner and the "+
			"tenancy uuid may have been transposed: %v", err)
	}
}

// TestCreateRefusesAnUnownedRow covers the other two roots the same way
// TestSourceCreateRefusesAnUnownedRow covers state_sources: refusing must happen
// BEFORE the database, or the column DEFAULT quietly supplies an organization
// nobody chose (#436).
func TestCreateRefusesAnUnownedRow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		create func(*sql.DB) error
	}{
		{"pipeline_connections", func(db *sql.DB) error {
			_, err := NewPipelineRepository(db).Create(context.Background(),
				&PipelineConnection{Name: "x", Provider: "github"}, "  ")
			return err
		}},
		{"ci_sources", func(db *sql.DB) error {
			_, err := NewCISourceRepository(db).Create(context.Background(),
				&CISource{Name: "x", Provider: "github"}, "")
			return err
		}},
		{"drift_runs", func(db *sql.DB) error {
			conn := "p1"
			_, err := NewDriftRepository(db).Create(context.Background(),
				&DriftRun{PipelineConnectionID: &conn, Status: "pending", CallbackToken: "t"}, "")
			return err
		}},
		{"schedules", func(db *sql.DB) error {
			_, err := NewScheduleRepository(db).Create(context.Background(),
				&Schedule{Name: "x", CronExpr: "@daily", TargetType: "drift_run"}, nil, "\t")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer db.Close()

			// No expectation is registered, so ANY statement reaching the
			// database fails this test. That is the assertion: the refusal is
			// before the INSERT, not after it.
			if err := tc.create(db); !errors.Is(err, ErrNoOrganization) {
				t.Errorf("err = %v, want ErrNoOrganization", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("a statement reached the database despite the refusal: %v", err)
			}
		})
	}
}
