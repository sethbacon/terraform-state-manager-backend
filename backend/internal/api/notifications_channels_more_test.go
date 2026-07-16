package api

import (
	"errors"
	"net/http"
	"testing"
)

// TestChannelHandlers_ErrorBranches covers the previously-uncovered error and
// re-encrypt branches of UpdateChannel and CreateChannel via the wired router.
func TestChannelHandlers_ErrorBranches(t *testing.T) {
	e := newNotificationsEnv(t)

	// UpdateChannel: malformed body and an invalid type both 400 before any DB.
	if w := e.do(http.MethodPut, "/api/v1/notifications/channels/n1", `{bad`); w.Code != http.StatusBadRequest {
		t.Errorf("update bad json: status = %d, want 400", w.Code)
	}
	if w := e.do(http.MethodPut, "/api/v1/notifications/channels/n1", `{"name":"x","type":"pager"}`); w.Code != http.StatusBadRequest {
		t.Errorf("update bad type: status = %d, want 400", w.Code)
	}

	// UpdateChannel with a new target re-encrypts and persists it.
	e.mock.ExpectQuery("UPDATE notification_channels").
		WillReturnRows(notifChannelRow(t, "https://hooks.example.com/x"))
	if w := e.do(http.MethodPut, "/api/v1/notifications/channels/n1",
		`{"name":"ops","type":"webhook","target":"https://hooks.example.com/new"}`); w.Code != http.StatusOK {
		t.Errorf("update with new target: status = %d, want 200", w.Code)
	}

	// UpdateChannel repo failure → 500.
	e.mock.ExpectQuery("UPDATE notification_channels").WillReturnError(errors.New("boom"))
	if w := e.do(http.MethodPut, "/api/v1/notifications/channels/n1",
		`{"name":"ops","type":"webhook"}`); w.Code != http.StatusInternalServerError {
		t.Errorf("update repo error: status = %d, want 500", w.Code)
	}

	// CreateChannel repo failure → 500.
	e.mock.ExpectQuery("INSERT INTO notification_channels").WillReturnError(errors.New("boom"))
	if w := e.do(http.MethodPost, "/api/v1/notifications/channels",
		`{"name":"ops","type":"webhook","target":"https://hooks.example.com/x"}`); w.Code != http.StatusInternalServerError {
		t.Errorf("create repo error: status = %d, want 500", w.Code)
	}

	// DeleteChannel repo failure → 500.
	e.mock.ExpectExec("DELETE FROM notification_channels").WithArgs("n1").WillReturnError(errors.New("boom"))
	if w := e.do(http.MethodDelete, "/api/v1/notifications/channels/n1", ""); w.Code != http.StatusInternalServerError {
		t.Errorf("delete repo error: status = %d, want 500", w.Code)
	}
}
