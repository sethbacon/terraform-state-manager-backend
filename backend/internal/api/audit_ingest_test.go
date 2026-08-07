package api

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
)

func sharedStoreCfg(shared bool) *config.Config {
	return &config.Config{Suite: config.SuiteConfig{IdentitySharedStore: shared}}
}

func postIngest(h *AuditIngestHandlers, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/audit/ingest", h.Ingest())
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/audit/ingest", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func TestAuditIngest_RejectsWithoutSharedStore(t *testing.T) {
	h := NewAuditIngestHandlers(nil, sharedStoreCfg(false))
	if w := postIngest(h, `{"action":"x"}`); w.Code != http.StatusForbidden {
		t.Fatalf("want 403 when not shared-store, got %d", w.Code)
	}
}

func TestAuditIngest_StoreUnavailableWhenNilDB(t *testing.T) {
	h := NewAuditIngestHandlers(nil, sharedStoreCfg(true))
	if w := postIngest(h, `{"action":"x"}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 when no identity DB, got %d", w.Code)
	}
}

func TestAuditIngest_BadBody(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	h := NewAuditIngestHandlers(db, sharedStoreCfg(true))
	if w := postIngest(h, `{not json`); w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 on malformed body, got %d", w.Code)
	}
}

func TestAuditIngest_MissingAction(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	h := NewAuditIngestHandlers(db, sharedStoreCfg(true))
	if w := postIngest(h, `{"user_id":"u1"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 when action missing, got %d", w.Code)
	}
}

// expectSiblingIDResolves queues the two existence probes resolveSiblingIDs
// issues, answering "this id names a row here" for each non-empty argument and
// "it does not" for an empty one.
func expectSiblingIDResolves(mock sqlmock.Sqlmock, userID, orgID string) {
	if userID != "" {
		mock.ExpectQuery("FROM users").WithArgs(userID).
			WillReturnRows(sqlmock.NewRows([]string{"id", "email", "name", "oidc_sub", "created_at", "updated_at"}).
				AddRow(userID, "a@x.io", "A", nil, time.Now(), time.Now()))
	}
	if orgID != "" {
		mock.ExpectQuery("FROM organizations").WithArgs(orgID).
			WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "idp_type", "idp_name", "created_at", "updated_at"}).
				AddRow(orgID, "org", "Org", nil, nil, time.Now(), time.Now()))
	}
}

func TestAuditIngest_RecordsEntry(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Both ids resolve locally (the shared-store case this endpoint is gated on),
	// so both are stamped on the row and the entry belongs to o1's admins.
	expectSiblingIDResolves(mock, "u1", "o1")
	// args: id, user_id, organization_id, action, resource_type, resource_id, metadata, ip_address, created_at, actor_email
	mock.ExpectQuery("INSERT INTO audit_logs").
		WithArgs(sqlmock.AnyArg(), "u1", "o1", "module.upload", "module", nil, sqlmock.AnyArg(), nil, sqlmock.AnyArg(), nil).
		WillReturnRows(auditInsertReturn())

	h := NewAuditIngestHandlers(db, sharedStoreCfg(true))
	body := `{"action":"module.upload","user_id":"u1","organization_id":"o1","resource_type":"module","timestamp":"2026-06-16T10:00:00Z","auth_method":"api_key","status_code":201}`
	if w := postIngest(h, body); w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d body=%s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestAuditIngest_NullsUnresolvableActorBeforeInsert is the re-pointed successor
// to TestAuditIngest_NullsActorOnInsertFailureThenRetries.
//
// The OUTCOME asserted is unchanged and still has to hold — an entry naming an
// actor or organization this database does not have lands org-less, with the
// sibling's originals preserved in metadata, and the shipper gets its 202. What
// changed is the mechanism, and it had to: identity v0.25.0's migration 000007
// dropped the audit_logs foreign keys, so the insert the old test made fail
// CANNOT fail that way any more. A test that keeps stubbing an FK violation
// would keep passing while asserting a branch production can no longer take.
//
// The single insert is itself the assertion that the retry is gone.
func TestAuditIngest_NullsUnresolvableActorBeforeInsert(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	// Neither id resolves here: the actor was provisioned only in the sibling,
	// and the organization is one this deployment has no row for.
	mock.ExpectQuery("FROM users").WithArgs("ghost").WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("FROM organizations").WithArgs("o9").WillReturnError(sql.ErrNoRows)
	// Exactly ONE insert, with both actor columns nulled.
	mock.ExpectQuery("INSERT INTO audit_logs").
		WithArgs(sqlmock.AnyArg(), nil, nil, "x", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), nil).
		WillReturnRows(auditInsertReturn())

	h := NewAuditIngestHandlers(db, sharedStoreCfg(true))
	if w := postIngest(h, `{"action":"x","user_id":"ghost","organization_id":"o9"}`); w.Code != http.StatusAccepted {
		t.Fatalf("want 202 for an unresolvable federated actor, got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestAuditIngest_KeepsResolvableOrganization is the other half of the decision:
// narrowing must not turn every federated entry into an org-less one. An id that
// DOES resolve stays stamped, so the entry reaches that tenant's admins rather
// than the platform bucket every admin can read.
func TestAuditIngest_KeepsResolvableOrganization(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery("FROM users").WithArgs("ghost").WillReturnError(sql.ErrNoRows)
	expectSiblingIDResolves(mock, "", "o1")
	mock.ExpectQuery("INSERT INTO audit_logs").
		WithArgs(sqlmock.AnyArg(), nil, "o1", "x", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), nil).
		WillReturnRows(auditInsertReturn())

	h := NewAuditIngestHandlers(db, sharedStoreCfg(true))
	if w := postIngest(h, `{"action":"x","user_id":"ghost","organization_id":"o1"}`); w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestFederatedAuditModel_FoldsExtrasIntoMetadata(t *testing.T) {
	ts := time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC)
	req := &federatedAuditEntry{
		Action: "module.upload", UserID: "u1", OrganizationID: "o1",
		ResourceType: "module", ResourceID: "m1", IPAddress: "1.2.3.4",
		AuthMethod: "api_key", StatusCode: 201, Timestamp: ts,
		Metadata: map[string]interface{}{"existing": "kept"},
	}
	m := federatedAuditModel(req, "terraform-registry")

	if m.Action != "module.upload" {
		t.Fatalf("action: %q", m.Action)
	}
	if m.UserID == nil || *m.UserID != "u1" {
		t.Fatalf("user_id not carried")
	}
	if m.ResourceType == nil || *m.ResourceType != "module" {
		t.Fatalf("resource_type not carried")
	}
	if m.Metadata["federated"] != true {
		t.Fatalf("federated flag missing")
	}
	if m.Metadata["source_app"] != "terraform-registry" {
		t.Fatalf("source_app missing")
	}
	if m.Metadata["source_timestamp"] != "2026-06-16T10:00:00Z" {
		t.Fatalf("source_timestamp wrong: %v", m.Metadata["source_timestamp"])
	}
	if m.Metadata["auth_method"] != "api_key" {
		t.Fatalf("auth_method not folded")
	}
	if m.Metadata["status_code"] != 201 {
		t.Fatalf("status_code not folded: %v", m.Metadata["status_code"])
	}
	if m.Metadata["existing"] != "kept" {
		t.Fatalf("caller metadata dropped")
	}
}

func TestFederatedAuditModel_OmitsEmptyOptionalFields(t *testing.T) {
	m := federatedAuditModel(&federatedAuditEntry{Action: "x"}, "")
	if m.UserID != nil || m.OrganizationID != nil || m.ResourceType != nil || m.ResourceID != nil || m.IPAddress != nil {
		t.Fatalf("expected nil optional pointers for empty inputs")
	}
	if _, ok := m.Metadata["source_app"]; ok {
		t.Fatalf("source_app should be absent when header empty")
	}
	if _, ok := m.Metadata["source_timestamp"]; ok {
		t.Fatalf("source_timestamp should be absent for zero time")
	}
	if m.Metadata["federated"] != true {
		t.Fatalf("federated should always be true")
	}
}
