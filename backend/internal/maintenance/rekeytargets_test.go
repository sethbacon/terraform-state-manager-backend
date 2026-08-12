package maintenance

// Tests for the rekey sweep (#364).
//
// The properties pinned here are the ones an operator's decision to DELETE
// TSM_ENCRYPTION_KEY_PREVIOUS rests on. Getting any of them wrong destroys a
// credential, so each is asserted on an exact value or a named sentinel rather
// than on "an error happened" — sqlmock's own "unexpected query" error satisfies
// `err != nil`, and a check removed from the code under test would otherwise
// still look green.

import (
	"context"
	"errors"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	identitycrypto "github.com/sethbacon/terraform-suite-identity/identity/crypto"
	identitynotify "github.com/sethbacon/terraform-suite-identity/identity/notify"
)

// testPreviousKey stands in for the key an operator is trying to retire.
// testKey (bindtargets_test.go) is the current one.
const testPreviousKey = "fedcba9876543210fedcba9876543210"

// newDualCipher is the cipher the SERVER runs with mid-rotation: seals with the
// current key, opens with either.
func newDualCipher(t *testing.T) *identitycrypto.TokenCipher {
	t.Helper()
	tc, err := identitycrypto.NewTokenCipherWithPrevious([]byte(testKey), []byte(testPreviousKey))
	if err != nil {
		t.Fatalf("NewTokenCipherWithPrevious: %v", err)
	}
	return tc
}

// newPreviousCipher writes the rows as they existed BEFORE the rotation. It is
// also the oracle for "this value is still readable with the old key", which is
// exactly what must stop being true.
func newPreviousCipher(t *testing.T) *identitycrypto.TokenCipher {
	t.Helper()
	tc, err := identitycrypto.NewTokenCipher([]byte(testPreviousKey))
	if err != nil {
		t.Fatalf("NewTokenCipher(previous): %v", err)
	}
	return tc
}

// The defect in #364: a row that is already BOUND but still sealed under the
// previous key opens through the dual cipher, so bind-targets calls it done.
// Rekey must re-encrypt it, keep the binding, and preserve the plaintext.
func TestRekeyChannelTargets_ReEncryptsARowStillUnderThePreviousKey(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const target = "https://hooks.example.com/a"
	aad := identitynotify.TargetContext("chan-1")
	old := newPreviousCipher(t)
	sealedOld, err := old.SealWithContext(target, aad)
	if err != nil {
		t.Fatal(err)
	}

	mock.ExpectQuery("SELECT id, encrypted_target FROM notification_channels").
		WillReturnRows(channelRows([2]string{"chan-1", sealedOld}))
	var stored string
	mock.ExpectExec("UPDATE notification_channels").
		WithArgs(sqlmock.AnyArg(), capturedArg{got: &stored}).
		WillReturnResult(sqlmock.NewResult(0, 1))

	res, err := RekeyChannelTargets(context.Background(), db, newDualCipher(t), []byte(testKey), false)
	if err != nil {
		t.Fatalf("RekeyChannelTargets: %v", err)
	}
	if res.Total != 1 || res.Converted != 1 || res.AlreadyBound != 0 || res.Failed != 0 {
		t.Fatalf("result = %s; want exactly one re-encryption", res)
	}

	// The whole point: readable with the current key ALONE.
	current := newCipher(t)
	got, err := current.OpenWithContext(stored, aad)
	if err != nil {
		t.Fatalf("re-encrypted value does not open under the current key alone: %v", err)
	}
	if got != target {
		t.Fatalf("plaintext = %q, want %q", got, target)
	}
	// Still bound, and no longer readable with the key being retired.
	if _, err := current.Open(stored); err == nil {
		t.Error("re-encrypted value opens WITHOUT a context; the binding was dropped")
	}
	if _, err := old.OpenWithContext(stored, aad); err == nil {
		t.Error("re-encrypted value still opens under the PREVIOUS key; nothing was rotated")
	}
}

// Idempotence. A row already on the current key must not be rewritten, so a
// second run issues no UPDATE at all -- asserted through the counters, because
// sqlmock turns an unexpected Exec into a row-level failure rather than a panic.
func TestRekeyChannelTargets_SkipsARowAlreadyUnderTheCurrentKey(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	dual := newDualCipher(t)
	bound, err := dual.SealWithContext("https://hooks.example.com/a", identitynotify.TargetContext("chan-1"))
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT id, encrypted_target FROM notification_channels").
		WillReturnRows(channelRows([2]string{"chan-1", bound}))

	res, err := RekeyChannelTargets(context.Background(), db, dual, []byte(testKey), false)
	if err != nil {
		t.Fatalf("RekeyChannelTargets: %v", err)
	}
	if res.AlreadyBound != 1 || res.Converted != 0 || res.Failed != 0 {
		t.Fatalf("result = %s; want the row left alone", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a row already on the current key must trigger no write: %v", err)
	}
}

// One pass, not two commands in an order. A row that is BOTH unbound and on the
// previous key converges to bound-and-current, so bind-targets is not a
// prerequisite for finishing a rotation.
func TestRekeyChannelTargets_ConvergesAnUnboundRowUnderThePreviousKey(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const target = "https://hooks.example.com/legacy"
	old := newPreviousCipher(t)
	legacy, err := old.Seal(target) // unbound AND on the old key
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT id, encrypted_target FROM notification_channels").
		WillReturnRows(channelRows([2]string{"chan-1", legacy}))
	var stored string
	mock.ExpectExec("UPDATE notification_channels").
		WithArgs(sqlmock.AnyArg(), capturedArg{got: &stored}).
		WillReturnResult(sqlmock.NewResult(0, 1))

	res, err := RekeyChannelTargets(context.Background(), db, newDualCipher(t), []byte(testKey), false)
	if err != nil {
		t.Fatalf("RekeyChannelTargets: %v", err)
	}
	if res.Converted != 1 || res.Failed != 0 {
		t.Fatalf("result = %s; want the row converted in one pass", res)
	}

	current := newCipher(t)
	got, err := current.OpenWithContext(stored, identitynotify.TargetContext("chan-1"))
	if err != nil {
		t.Fatalf("converged value does not open bound under the current key: %v", err)
	}
	if got != target {
		t.Fatalf("plaintext = %q, want %q", got, target)
	}
	if _, err := current.Open(stored); err == nil {
		t.Error("converged value still opens unbound; it was re-encrypted but not bound")
	}
}

// Verify is the exit criterion: it must write nothing and exit non-zero while a
// row still needs the key the operator is about to delete.
func TestRekeyChannelTargets_VerifyWritesNothingAndHoldsTheGateShut(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	old := newPreviousCipher(t)
	sealedOld, err := old.SealWithContext("https://hooks.example.com/a", identitynotify.TargetContext("chan-1"))
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT id, encrypted_target FROM notification_channels").
		WillReturnRows(channelRows([2]string{"chan-1", sealedOld}))

	res, err := RekeyChannelTargets(context.Background(), db, newDualCipher(t), []byte(testKey), true)
	if !errors.Is(err, ErrPreviousKeyStillRequired) {
		t.Fatalf("verify error = %v, want ErrPreviousKeyStillRequired so a runbook step can gate on it", err)
	}
	if res.Converted != 1 {
		t.Errorf("verify should report what WOULD be re-encrypted; got %s", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("verify must not write: %v", err)
	}
}

// The other half. A clean zero is what permits deleting the previous key, so
// "no error at all" is the assertion, not "no sentinel".
func TestRekeyChannelTargets_VerifyPassesOnceEverythingIsCurrent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	dual := newDualCipher(t)
	bound, err := dual.SealWithContext("https://hooks.example.com/a", identitynotify.TargetContext("chan-1"))
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT id, encrypted_target FROM notification_channels").
		WillReturnRows(channelRows([2]string{"chan-1", bound}))

	res, err := RekeyChannelTargets(context.Background(), db, dual, []byte(testKey), true)
	if err != nil {
		t.Fatalf("verify must return nil once every row is current, got %v", err)
	}
	if res.AlreadyBound != 1 || res.Converted != 0 || res.Failed != 0 {
		t.Fatalf("result = %s; want one already-current row", res)
	}
}

// A target bound to ANOTHER channel is the attack the AAD exists to detect.
// Re-binding it into the row it was found in would mint the binding the attacker
// could not forge, with a routine rotation as the audit trail. It must be
// reported, left untouched, and it must hold the gate shut.
func TestRekeyChannelTargets_ReportsAValueBoundToAnotherRowAndLeavesItAlone(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	dual := newDualCipher(t)
	// Sealed under the CURRENT key, but with a different row's context: the only
	// thing wrong with it is where it is stored.
	moved, err := dual.SealWithContext("https://hooks.example.com/victim", identitynotify.TargetContext("chan-2"))
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT id, encrypted_target FROM notification_channels").
		WillReturnRows(channelRows([2]string{"chan-1", moved}))

	res, err := RekeyChannelTargets(context.Background(), db, dual, []byte(testKey), false)
	if err != nil {
		t.Fatalf("RekeyChannelTargets: %v", err)
	}
	if res.Failed != 1 || res.Converted != 0 || res.AlreadyBound != 0 {
		t.Fatalf("result = %s; want the moved value reported, not re-bound", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a value bound to another row must never be written back: %v", err)
	}
}

// ... and the same row must make verify fail, because "one row could not be
// read" is not evidence that the previous key can go.
func TestRekeyChannelTargets_AnUnreadableRowBlocksVerify(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery("SELECT id, encrypted_target FROM notification_channels").
		WillReturnRows(channelRows([2]string{"chan-1", "not-a-valid-ciphertext"}))

	res, err := RekeyChannelTargets(context.Background(), db, newDualCipher(t), []byte(testKey), true)
	if !errors.Is(err, ErrPreviousKeyStillRequired) {
		t.Fatalf("verify error = %v, want ErrPreviousKeyStillRequired: an unreadable row must fail closed", err)
	}
	if res.Failed != 1 {
		t.Errorf("result = %s; want the bad row counted as failed", res)
	}
}

// One bad row must not abandon the rest -- a half-swept table during a rotation
// is the worst outcome available.
func TestRekeyChannelTargets_OneBadRowDoesNotAbandonTheSweep(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const target = "https://hooks.example.com/good"
	old := newPreviousCipher(t)
	good, err := old.SealWithContext(target, identitynotify.TargetContext("chan-good"))
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT id, encrypted_target FROM notification_channels").
		WillReturnRows(channelRows(
			[2]string{"chan-bad", "not-a-valid-ciphertext"},
			[2]string{"chan-good", good},
		))
	var stored string
	mock.ExpectExec("UPDATE notification_channels").
		WithArgs(sqlmock.AnyArg(), capturedArg{got: &stored}).
		WillReturnResult(sqlmock.NewResult(0, 1))

	res, err := RekeyChannelTargets(context.Background(), db, newDualCipher(t), []byte(testKey), false)
	if err != nil {
		t.Fatalf("RekeyChannelTargets: %v", err)
	}
	if res.Failed != 1 || res.Converted != 1 {
		t.Fatalf("result = %s; want the bad row reported and the good row still converted", res)
	}
	got, err := newCipher(t).OpenWithContext(stored, identitynotify.TargetContext("chan-good"))
	if err != nil || got != target {
		t.Fatalf("the good row was not correctly re-encrypted: (%q, %v)", got, err)
	}
}

// A failure to LIST is not a row-level failure: it means the sweep never saw the
// table, so it must abort and surface the cause rather than report "0 examined,
// nothing to do" -- which reads exactly like a finished rotation.
func TestRekeyChannelTargets_AListFailureAbortsRatherThanReportingSuccess(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	boom := errors.New("connection reset by peer")
	mock.ExpectQuery("SELECT id, encrypted_target FROM notification_channels").WillReturnError(boom)

	res, err := RekeyChannelTargets(context.Background(), db, newDualCipher(t), []byte(testKey), true)
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the underlying list failure", err)
	}
	if res.Total != 0 {
		t.Errorf("result = %s; nothing was examined", res)
	}
}

// The sealing-key probe. A cipher that does not seal with the key it was handed
// would produce values the running service cannot read, so the run is refused
// BEFORE a single row is read.
//
// The mock is primed with a clean, empty sweep on purpose: without the probe
// this call succeeds and returns nil, so the assertion below cannot be satisfied
// by sqlmock's own "unexpected query" error. That is what makes the guard
// mutation-visible rather than inertly green.
func TestRekeyChannelTargets_RefusesAKeyTheCipherDoesNotSealWith(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery("SELECT id, encrypted_target FROM notification_channels").
		WillReturnRows(channelRows())

	// The cipher seals with testKey; the operator passed the key being retired.
	_, err = RekeyChannelTargets(context.Background(), db, newDualCipher(t), []byte(testPreviousKey), false)
	if !errors.Is(err, ErrEncryptionKeyMismatch) {
		t.Fatalf("error = %v, want ErrEncryptionKeyMismatch", err)
	}
	if merr := mock.ExpectationsWereMet(); merr == nil {
		t.Error("the sweep read rows before checking the key; the probe must come first")
	}
}

// A key of the wrong length is a configuration mistake, not data damage. It must
// be named as such rather than surfacing as every row failing.
func TestRekeyChannelTargets_RejectsAKeyThatIsNotAES256(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery("SELECT id, encrypted_target FROM notification_channels").
		WillReturnRows(channelRows())

	_, err = RekeyChannelTargets(context.Background(), db, newDualCipher(t), []byte("too-short"), false)
	if !errors.Is(err, identitycrypto.ErrKeyLengthInvalid) {
		t.Fatalf("error = %v, want identitycrypto.ErrKeyLengthInvalid", err)
	}
	if merr := mock.ExpectationsWereMet(); merr == nil {
		t.Error("the sweep read rows despite an unusable key")
	}
}

func TestRekeyChannelTargets_RequiresACipher(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery("SELECT id, encrypted_target FROM notification_channels").
		WillReturnRows(channelRows())

	if _, err := RekeyChannelTargets(context.Background(), db, nil, []byte(testKey), false); !errors.Is(err, ErrNoTokenCipher) {
		t.Fatalf("error = %v, want ErrNoTokenCipher; a nil cipher is not 'nothing to do'", err)
	}
}
