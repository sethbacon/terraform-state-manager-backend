package api

// notifications_smtp_roundtrip_test.go covers the defect the existing SMTP
// tests could not see.
//
// THE DEFECT. PutSMTPConfig stored the AES-GCM ciphertext as
// string(enc) in a struct persisted with json.Marshal. Go's JSON encoder
// replaces every byte sequence that is not valid UTF-8 with U+FFFD, and
// ciphertext is indistinguishable from random -- so the password was DESTROYED
// AS IT WAS SAVED, in every deployment, and the startup reader could only ever
// log a decryption failure. SMTP then authenticated with no password at all.
//
// WHY EVERY EXISTING TEST PASSED. They stub the stored ciphertext as "deadbeef"
// and "existing" (notifications_smtp_test.go:78,133 and
// notifications_apikeyexpiry_test.go:123). Both are ASCII, and ASCII survives
// the round-trip unharmed. The fixtures were chosen for readability, and
// readability is exactly the property that removed the bug from view: a
// printable stand-in for ciphertext cannot exhibit a corruption that only
// affects non-printable bytes.
//
// So these tests use REAL ciphertext from the real crypto package, never a
// literal. That is the whole point, and a future edit that swaps in a readable
// fixture puts the defect back beyond reach of the suite.

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
)

// realCiphertext seals a password with the actual crypto package.
//
// Real, never a literal: a readable stand-in cannot exhibit a corruption that
// only affects non-printable bytes, which is precisely how this defect stayed
// invisible to a suite that otherwise covered these handlers well.
func realCiphertext(t *testing.T, plaintext string) []byte {
	t.Helper()
	t.Setenv("TSM_ENCRYPTION_KEY", testEncryptionKey)
	if !crypto.Available() {
		t.Fatal("crypto is not available with a key set; the rest of this test proves nothing")
	}
	ct, err := crypto.Encrypt([]byte(plaintext))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	return ct
}

// TestSMTPCiphertextSurvivesThePersistenceRoundTrip is the regression.
//
// Written against the PERSISTENCE SHAPE rather than the HTTP handler, because
// that is where the loss happened: the handler was correct about everything
// except the type it put the bytes into.
func TestSMTPCiphertextSurvivesThePersistenceRoundTrip(t *testing.T) {
	// Repeated, because a single ciphertext could be valid UTF-8 by luck and
	// the whole defect is probabilistic per-value. Under the old code this
	// fails on essentially every iteration; one lucky pass must not read as
	// green.
	for i := 0; i < 50; i++ {
		ct := realCiphertext(t, "hunter2-the-smtp-password")

		var out notificationsSMTPConfigDB
		out.SMTP.PasswordSealed = encodeSealedForTest(t, ct)

		blob, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var back notificationsSMTPConfigDB
		if err := json.Unmarshal(blob, &back); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		got, legacy, ok := back.SMTP.decodeStoredPassword()
		if !ok {
			t.Fatalf("iteration %d: the stored password did not decode at all", i)
		}
		if legacy {
			t.Fatalf("iteration %d: a freshly sealed value was reported as legacy", i)
		}
		if string(got) != string(ct) {
			t.Fatalf("iteration %d: ciphertext did not survive the round-trip: %d bytes in, %d bytes out.\n"+
				"This is the #SMTP defect: raw ciphertext put through json.Marshal loses every "+
				"non-UTF-8 byte to U+FFFD, and the password is unrecoverable from the moment it is saved.",
				i, len(ct), len(got))
		}
		// And it must actually decrypt, not merely compare equal.
		pw, err := crypto.Decrypt(got)
		if err != nil {
			t.Fatalf("iteration %d: round-tripped ciphertext will not decrypt: %v", i, err)
		}
		if string(pw) != "hunter2-the-smtp-password" {
			t.Fatalf("iteration %d: decrypted to %q", i, string(pw))
		}
	}
}

// TestRawCiphertextInAStringIsLossy proves the mechanism, so the reason for the
// base64 is recorded as a fact rather than as a claim in a comment.
//
// If this ever stops failing, Go's JSON encoder has changed and the base64 may
// be reconsidered -- but not before.
func TestRawCiphertextInAStringIsLossy(t *testing.T) {
	corrupted := 0
	const runs = 50
	for i := 0; i < runs; i++ {
		ct := realCiphertext(t, "hunter2")

		var out notificationsSMTPConfigDB
		out.SMTP.PasswordEncrypted = string(ct) // exactly what the old code did

		blob, _ := json.Marshal(out)
		var back notificationsSMTPConfigDB
		_ = json.Unmarshal(blob, &back)

		if back.SMTP.PasswordEncrypted != string(ct) {
			corrupted++
		}
	}
	if corrupted == 0 {
		t.Fatalf("0/%d raw ciphertexts were corrupted by the string+JSON round-trip. Either the "+
			"encoder changed or this test is not exercising real ciphertext -- check that "+
			"realCiphertext is not returning printable bytes.", runs)
	}
	t.Logf("%d/%d raw ciphertexts corrupted by string()+json.Marshal (this is why the sealed "+
		"field is base64)", corrupted, runs)
}

// TestLegacyPasswordIsStillOfferedToTheDecrypter guards the deliberate
// non-removal of the old path.
//
// A ciphertext that happened to be valid UTF-8 survived the old write intact,
// so a small number of deployments have a recoverable password. Dropping the
// legacy read for tidiness would destroy exactly those.
func TestLegacyPasswordIsStillOfferedToTheDecrypter(t *testing.T) {
	var cfg notificationsSMTPConfigDB
	cfg.SMTP.PasswordEncrypted = "a-value-that-is-valid-utf8"

	ct, legacy, ok := cfg.SMTP.decodeStoredPassword()
	if !ok {
		t.Fatal("a legacy-only config reported no stored password, so a recoverable one would be discarded")
	}
	if !legacy {
		t.Error("a legacy value was not reported as legacy, so the operator gets the wrong remediation")
	}
	if string(ct) != "a-value-that-is-valid-utf8" {
		t.Errorf("legacy bytes were altered: %q", string(ct))
	}
	if !cfg.SMTP.storesAPassword() {
		t.Error("storesAPassword said no while a legacy value is present; the UI would report the " +
			"password as unset mid-upgrade")
	}
}

// TestSealedIsPreferredOverLegacy pins precedence. After a re-save both fields
// can be populated in a config an older release wrote; the good one must win.
func TestSealedIsPreferredOverLegacy(t *testing.T) {
	ct := realCiphertext(t, "the-real-one")
	var cfg notificationsSMTPConfigDB
	cfg.SMTP.PasswordSealed = encodeSealedForTest(t, ct)
	cfg.SMTP.PasswordEncrypted = "stale-corrupt-value"

	got, legacy, ok := cfg.SMTP.decodeStoredPassword()
	if !ok || legacy {
		t.Fatalf("sealed value not preferred: ok=%v legacy=%v", ok, legacy)
	}
	if string(got) != string(ct) {
		t.Error("the legacy value won over the sealed one")
	}
}

// TestUnparseableSealedValueDoesNotFallBackToLegacy.
//
// A corrupt base64 blob is a different failure from "no sealed value". Falling
// through to the legacy field there would silently substitute an older
// password for the one the operator believes is stored.
func TestUnparseableSealedValueDoesNotFallBackToLegacy(t *testing.T) {
	var cfg notificationsSMTPConfigDB
	cfg.SMTP.PasswordSealed = "!!!not-base64!!!"
	cfg.SMTP.PasswordEncrypted = "an-older-password"

	if _, _, ok := cfg.SMTP.decodeStoredPassword(); ok {
		t.Error("an unparseable sealed value fell through to the legacy field; that silently " +
			"substitutes an older password for the one the operator thinks is configured")
	}
}

// encodeSealedForTest mirrors what the handler writes into PasswordSealed.
func encodeSealedForTest(t *testing.T, ct []byte) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString(ct)
}
