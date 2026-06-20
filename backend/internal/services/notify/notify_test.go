package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

var channelCols = []string{"id", "name", "type", "encrypted_target", "events", "enabled",
	"last_status", "last_error", "last_sent_at", "created_at", "updated_at"}

func newNotifier(t *testing.T) (*Notifier, sqlmock.Sqlmock) {
	t.Helper()
	t.Setenv("TSM_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return New(repositories.NewNotificationChannelRepository(db)), mock
}

// sealedURL encrypts a target URL the way the API layer stores it.
func sealedURL(t *testing.T, url string) []byte {
	t.Helper()
	ct, err := crypto.Encrypt([]byte(url))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	return ct
}

func channelRow(t *testing.T, typ, url string) *sqlmock.Rows {
	return sqlmock.NewRows(channelCols).
		AddRow("n1", "ops", typ, sealedURL(t, url), []byte(`{}`), true, nil, nil, nil, "2026-06-10", "2026-06-10")
}

func TestNotify_WebhookPayloadAndRecord(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, mock := newNotifier(t)
	mock.ExpectQuery("FROM notification_channels WHERE enabled").WithArgs(EventDriftDetected).
		WillReturnRows(channelRow(t, "webhook", srv.URL))
	mock.ExpectExec("UPDATE notification_channels").
		WithArgs("n1", "sent", "", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	n.Notify(context.Background(), Event{Type: EventDriftDetected, Title: "Drift", Message: "3 resources drifted"})

	if got["title"] != "Drift" || got["message"] != "3 resources drifted" || got["source"] != "terraform-state-manager" {
		t.Errorf("webhook payload wrong: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("delivery was not recorded as sent: %v", err)
	}
}

func TestNotify_SlackPayload(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, mock := newNotifier(t)
	mock.ExpectQuery("FROM notification_channels WHERE enabled").WithArgs(EventRunFailed).
		WillReturnRows(channelRow(t, "slack", srv.URL))
	mock.ExpectExec("UPDATE notification_channels").
		WithArgs("n1", "sent", "", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	n.Notify(context.Background(), Event{Type: EventRunFailed, Title: "Run failed", Message: "boom"})

	if !strings.Contains(got["text"], "Run failed") || !strings.Contains(got["text"], "boom") {
		t.Errorf("slack payload wrong: %+v", got)
	}
}

func TestNotify_TeamsPayload(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, mock := newNotifier(t)
	mock.ExpectQuery("FROM notification_channels WHERE enabled").WithArgs(EventDriftDetected).
		WillReturnRows(channelRow(t, "teams", srv.URL))
	mock.ExpectExec("UPDATE notification_channels").
		WithArgs("n1", "sent", "", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	n.Notify(context.Background(), Event{Type: EventDriftDetected, Title: "Drift", Message: "3 resources drifted"})

	// Adaptive Card envelope: type=message with one adaptive-card attachment whose
	// body carries the title and message text blocks.
	if got["type"] != "message" {
		t.Fatalf("teams payload not a message envelope: %+v", got)
	}
	body, _ := json.Marshal(got)
	if !strings.Contains(string(body), "AdaptiveCard") || !strings.Contains(string(body), "Drift") ||
		!strings.Contains(string(body), "3 resources drifted") {
		t.Errorf("teams payload missing card/title/message: %s", body)
	}
}

func TestNotify_DestinationFailureRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	n, mock := newNotifier(t)
	mock.ExpectQuery("FROM notification_channels WHERE enabled").WithArgs(EventDriftDetected).
		WillReturnRows(channelRow(t, "webhook", srv.URL))
	mock.ExpectExec("UPDATE notification_channels").
		WithArgs("n1", "failed", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	n.Notify(context.Background(), Event{Type: EventDriftDetected, Title: "t", Message: "m"})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("failed delivery must be recorded: %v", err)
	}
}

func TestNotify_MissingTargetRecordedAsFailure(t *testing.T) {
	n, mock := newNotifier(t)
	row := sqlmock.NewRows(channelCols).
		AddRow("n1", "ops", "webhook", nil, []byte(`{}`), true, nil, nil, nil, "2026-06-10", "2026-06-10")
	mock.ExpectQuery("FROM notification_channels WHERE enabled").WithArgs(EventDriftDetected).
		WillReturnRows(row)
	mock.ExpectExec("UPDATE notification_channels").
		WithArgs("n1", "failed", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	n.Notify(context.Background(), Event{Type: EventDriftDetected, Title: "t", Message: "m"})
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("missing target must be recorded as failure: %v", err)
	}
}

func TestNotify_ChannelListErrorIsBestEffort(t *testing.T) {
	var hit atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit.Store(true)
	}))
	defer srv.Close()

	n, _ := newNotifier(t) // no expectations → repo query errors
	n.Notify(context.Background(), Event{Type: EventDriftDetected, Title: "t", Message: "m"})
	if hit.Load() {
		t.Error("no delivery should happen when the channel list fails")
	}
}

func TestSendTest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, mock := newNotifier(t)
	mock.ExpectQuery("FROM notification_channels WHERE id").WithArgs("n1").
		WillReturnRows(channelRow(t, "webhook", srv.URL))
	mock.ExpectExec("UPDATE notification_channels").
		WithArgs("n1", "sent", "", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := n.SendTest(context.Background(), "n1"); err != nil {
		t.Fatalf("SendTest: %v", err)
	}
}

func TestSendTest_MissingChannel(t *testing.T) {
	n, mock := newNotifier(t)
	mock.ExpectQuery("FROM notification_channels WHERE id").WithArgs("ghost").
		WillReturnRows(sqlmock.NewRows(channelCols))
	if err := n.SendTest(context.Background(), "ghost"); err == nil {
		t.Error("SendTest should fail for a missing channel")
	}
}

func TestNilNotifierIsSafe(t *testing.T) {
	var n *Notifier
	n.Notify(context.Background(), Event{Type: EventDriftDetected})
	if err := n.SendTest(context.Background(), "n1"); err == nil {
		t.Error("nil notifier SendTest should report unavailability")
	}
}
