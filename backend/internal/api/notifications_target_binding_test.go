package api

import (
	"database/sql/driver"
	"errors"
	"net/http"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	identitycrypto "github.com/sethbacon/terraform-suite-identity/identity/crypto"
	identitynotify "github.com/sethbacon/terraform-suite-identity/identity/notify"
)

// suite-identity #153 — the encrypted channel target is now bound to its channel
// row via GCM additional authenticated data, so a sealed target cannot be lifted
// out of one channel row and written into another by anyone with database write
// access.
//
// These assert the property that actually matters: the value REACHING the
// database opens only under its own row's context. Asserting that
// SealWithContext was called would prove nothing about what was stored.

// capturedArg records the value sqlmock was handed for one bind parameter.
// sqlmock.Argument lets a matcher observe the value rather than only compare it.
type capturedArg struct{ got *string }

func (c capturedArg) Match(v driver.Value) bool {
	if s, ok := v.(string); ok {
		*c.got = s
	}
	return true
}

func testCipher(t *testing.T) *identitycrypto.TokenCipher {
	t.Helper()
	tc, err := identitycrypto.NewTokenCipher([]byte(testEncryptionKey))
	if err != nil {
		t.Fatalf("NewTokenCipher: %v", err)
	}
	return tc
}

// assertBoundTo checks a stored ciphertext opens under channelID's context and
// under nothing else -- neither the unbound form nor a neighbouring row's.
func assertBoundTo(t *testing.T, stored, channelID, wantPlaintext string) {
	t.Helper()
	tc := testCipher(t)

	got, err := tc.OpenWithContext(stored, identitynotify.TargetContext(channelID))
	if err != nil {
		t.Fatalf("stored target does not open under its own row context: %v", err)
	}
	if got != wantPlaintext {
		t.Errorf("stored target = %q, want %q", got, wantPlaintext)
	}

	// Unbound read must fail, or the value was never bound.
	if _, err := tc.Open(stored); !errors.Is(err, identitycrypto.ErrDecryptionFailed) {
		t.Errorf("stored target still opens WITHOUT a context; it was not bound (err=%v)", err)
	}

	// And it must not open as some other channel's -- the whole point.
	if _, err := tc.OpenWithContext(stored, identitynotify.TargetContext("someone-else")); err == nil {
		t.Error("stored target opened under another channel's context; the row binding is vacuous")
	}
}

func TestCreateChannel_BindsTheTargetToTheNewRow(t *testing.T) {
	e := newNotificationsEnv(t)
	const target = "https://hooks.example.com/created"

	// Create cannot bind at INSERT time: ChannelRepository.Create uses
	// `INSERT ... RETURNING` and does not take a caller-supplied id, so the row
	// is inserted unbound and immediately re-sealed against its own id.
	e.mock.ExpectQuery("INSERT INTO notification_channels").
		WillReturnRows(notifChannelRow(t, target))

	var stored string
	e.mock.ExpectQuery("UPDATE notification_channels").
		WithArgs(
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), capturedArg{got: &stored},
		).
		WillReturnRows(notifChannelRow(t, target))

	w := e.do(http.MethodPost, "/api/v1/notifications/channels",
		`{"name":"ops","type":"webhook","target":"`+target+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201", w.Code)
	}

	if stored == "" {
		t.Fatal("no follow-up UPDATE captured; the created row was left unbound")
	}
	// notifChannelRow returns id "n1", which is what the handler binds against.
	assertBoundTo(t, stored, "n1", target)
}

func TestUpdateChannel_BindsTheTargetToTheRowBeingUpdated(t *testing.T) {
	e := newNotificationsEnv(t)
	const target = "https://hooks.example.com/updated"

	// Update already has the id in hand, so unlike create this is one write.
	var stored string
	e.mock.ExpectQuery("UPDATE notification_channels").
		WithArgs(
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), capturedArg{got: &stored},
		).
		WillReturnRows(notifChannelRow(t, target))

	w := e.do(http.MethodPut, "/api/v1/notifications/channels/chan-42",
		`{"name":"ops","type":"webhook","target":"`+target+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update: status = %d, want 200", w.Code)
	}

	if stored == "" {
		t.Fatal("no encrypted target captured on update")
	}
	// Bound to the PATH id, not to whatever the fixture row happens to return.
	assertBoundTo(t, stored, "chan-42", target)
}

// A create whose follow-up bind fails must still return 201: the channel exists
// and delivers (the notifier reads unbound ciphertexts too), and failing here
// would report an error for a row that WAS created, inviting a retry that
// creates a duplicate.
func TestCreateChannel_StillSucceedsWhenTheBindWriteFails(t *testing.T) {
	e := newNotificationsEnv(t)
	const target = "https://hooks.example.com/created"

	e.mock.ExpectQuery("INSERT INTO notification_channels").
		WillReturnRows(notifChannelRow(t, target))
	e.mock.ExpectQuery("UPDATE notification_channels").
		WillReturnError(errors.New("bind write failed"))

	if w := e.do(http.MethodPost, "/api/v1/notifications/channels",
		`{"name":"ops","type":"webhook","target":"`+target+`"}`); w.Code != http.StatusCreated {
		t.Fatalf("create with failed bind: status = %d, want 201 (the channel exists)", w.Code)
	}
}
