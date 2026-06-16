package api

import (
	"errors"
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

func TestAuditIngest_RecordsEntry(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// args: id, user_id, organization_id, action, resource_type, resource_id, metadata, ip_address, created_at
	mock.ExpectExec("INSERT INTO audit_logs").
		WithArgs(sqlmock.AnyArg(), "u1", "o1", "module.upload", "module", nil, sqlmock.AnyArg(), nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	h := NewAuditIngestHandlers(db, sharedStoreCfg(true))
	body := `{"action":"module.upload","user_id":"u1","organization_id":"o1","resource_type":"module","timestamp":"2026-06-16T10:00:00Z","auth_method":"api_key","status_code":201}`
	if w := postIngest(h, body); w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d body=%s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestAuditIngest_NullsActorOnInsertFailureThenRetries(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	// First insert (with the sibling's actor) fails like an FK violation...
	mock.ExpectExec("INSERT INTO audit_logs").
		WithArgs(sqlmock.AnyArg(), "ghost", "o9", "x", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnError(errors.New("insert or update on table \"audit_logs\" violates foreign key constraint"))
	// ...so it retries with the actor nulled (originals preserved in metadata).
	mock.ExpectExec("INSERT INTO audit_logs").
		WithArgs(sqlmock.AnyArg(), nil, nil, "x", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	h := NewAuditIngestHandlers(db, sharedStoreCfg(true))
	if w := postIngest(h, `{"action":"x","user_id":"ghost","organization_id":"o9"}`); w.Code != http.StatusAccepted {
		t.Fatalf("want 202 after FK-failure retry, got %d", w.Code)
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
