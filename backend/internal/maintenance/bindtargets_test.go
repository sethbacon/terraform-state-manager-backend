package maintenance

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	identitycrypto "github.com/sethbacon/terraform-suite-identity/identity/crypto"
	identitynotify "github.com/sethbacon/terraform-suite-identity/identity/notify"
)

// suite-identity #153 backfill. The properties worth pinning are the ones an
// operator relies on when running this against live data: it is safe to re-run,
// one bad row does not abandon the rest, and verify writes nothing.

const testKey = "0123456789abcdef0123456789abcdef"

func newCipher(t *testing.T) *identitycrypto.TokenCipher {
	t.Helper()
	tc, err := identitycrypto.NewTokenCipher([]byte(testKey))
	if err != nil {
		t.Fatalf("NewTokenCipher: %v", err)
	}
	return tc
}

// capturedArg records the value handed to one bind parameter.
type capturedArg struct{ got *string }

func (c capturedArg) Match(v driver.Value) bool {
	if s, ok := v.(string); ok {
		*c.got = s
	}
	return true
}

func channelRows(pairs ...[2]string) *sqlmock.Rows {
	r := sqlmock.NewRows([]string{"id", "encrypted_target"})
	for _, p := range pairs {
		r.AddRow(p[0], p[1])
	}
	return r
}

func TestBindChannelTargets_ConvertsAnUnboundRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tc := newCipher(t)

	legacy, err := tc.Seal("https://hooks.example.com/a")
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT id, encrypted_target FROM notification_channels").
		WillReturnRows(channelRows([2]string{"chan-1", legacy}))

	var stored string
	mock.ExpectExec("UPDATE notification_channels").
		WithArgs(sqlmock.AnyArg(), capturedArg{got: &stored}).
		WillReturnResult(sqlmock.NewResult(0, 1))

	res, err := BindChannelTargets(context.Background(), db, tc, false)
	if err != nil {
		t.Fatalf("BindChannelTargets: %v", err)
	}
	if res.Converted != 1 || res.AlreadyBound != 0 || res.Failed != 0 {
		t.Fatalf("result = %s; want exactly one conversion", res)
	}

	// The value written must be bound to that row, and must no longer open unbound.
	got, err := tc.OpenWithContext(stored, identitynotify.TargetContext("chan-1"))
	if err != nil || got != "https://hooks.example.com/a" {
		t.Fatalf("converted value does not open under its row context: (%q, %v)", got, err)
	}
	if _, err := tc.Open(stored); err == nil {
		t.Error("converted value still opens WITHOUT a context; it was not bound")
	}
}

// Re-running must be a no-op rather than double-sealing. This is what makes an
// interrupted sweep safe to resume, so it is asserted by the absence of any
// UPDATE at all -- sqlmock fails on an unexpected one.
func TestBindChannelTargets_SkipsAnAlreadyBoundRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tc := newCipher(t)

	bound, err := tc.SealWithContext("https://hooks.example.com/a", identitynotify.TargetContext("chan-1"))
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT id, encrypted_target FROM notification_channels").
		WillReturnRows(channelRows([2]string{"chan-1", bound}))

	res, err := BindChannelTargets(context.Background(), db, tc, false)
	if err != nil {
		t.Fatalf("BindChannelTargets: %v", err)
	}
	if res.AlreadyBound != 1 || res.Converted != 0 {
		t.Fatalf("result = %s; want the row recognised as already bound", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("an already-bound row should trigger no write: %v", err)
	}
}

// A row that cannot be decrypted at all is a pre-existing problem this sweep did
// not cause. It must be reported and stepped over, not allowed to abandon every
// remaining row.
func TestBindChannelTargets_OneUndecryptableRowDoesNotAbortTheSweep(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tc := newCipher(t)

	good, err := tc.Seal("https://hooks.example.com/good")
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT id, encrypted_target FROM notification_channels").
		WillReturnRows(channelRows(
			[2]string{"chan-bad", "not-a-valid-ciphertext"},
			[2]string{"chan-good", good},
		))
	mock.ExpectExec("UPDATE notification_channels").
		WillReturnResult(sqlmock.NewResult(0, 1))

	res, err := BindChannelTargets(context.Background(), db, tc, false)
	if err != nil {
		t.Fatalf("BindChannelTargets: %v", err)
	}
	if res.Failed != 1 {
		t.Errorf("failed = %d, want 1", res.Failed)
	}
	if res.Converted != 1 {
		t.Errorf("converted = %d, want 1 — the good row must still be processed", res.Converted)
	}
}

// Verify must write nothing and must FAIL while work remains, so it can gate the
// removal of the legacy read in a script rather than needing someone to read it.
func TestBindChannelTargets_VerifyWritesNothingAndFailsWhileUnboundRemain(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tc := newCipher(t)

	legacy, err := tc.Seal("https://hooks.example.com/a")
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT id, encrypted_target FROM notification_channels").
		WillReturnRows(channelRows([2]string{"chan-1", legacy}))

	res, err := BindChannelTargets(context.Background(), db, tc, true)
	if !errors.Is(err, ErrUnboundRemain) {
		t.Fatalf("verify error = %v, want ErrUnboundRemain so it can gate a script", err)
	}
	if res.Converted != 1 {
		t.Errorf("verify should report what WOULD convert; got %s", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("verify must not write: %v", err)
	}
}

// The other half: verify succeeds once everything is bound. That zero is the
// signal that OpenWithContextOrLegacy can become OpenWithContext.
func TestBindChannelTargets_VerifySucceedsWhenAllBound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tc := newCipher(t)

	bound, err := tc.SealWithContext("https://hooks.example.com/a", identitynotify.TargetContext("chan-1"))
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT id, encrypted_target FROM notification_channels").
		WillReturnRows(channelRows([2]string{"chan-1", bound}))

	res, err := BindChannelTargets(context.Background(), db, tc, true)
	if err != nil {
		t.Fatalf("verify with everything bound must succeed, got %v", err)
	}
	if res.AlreadyBound != 1 {
		t.Errorf("result = %s", res)
	}
}

func TestBindChannelTargets_RequiresACipher(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := BindChannelTargets(context.Background(), db, nil, false); err == nil {
		t.Fatal("a nil cipher must be refused, not treated as 'nothing to do'")
	}
}
