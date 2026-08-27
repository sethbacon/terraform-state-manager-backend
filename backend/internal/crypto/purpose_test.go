package crypto

// purpose_test.go covers the binding, and above all the properties that make it
// safe to ship WITHOUT a sweep (#277).

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

const purposeTestKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func withKey(t *testing.T) {
	t.Helper()
	t.Setenv("TSM_ENCRYPTION_KEY", purposeTestKey)
	if !Available() {
		t.Fatal("crypto unavailable with a key set; the rest of this test proves nothing")
	}
}

func TestEncryptForRoundTrips(t *testing.T) {
	withKey(t)
	for p := range knownPurposes {
		t.Run(string(p), func(t *testing.T) {
			ct, err := EncryptFor([]byte("hunter2"), p)
			if err != nil {
				t.Fatalf("EncryptFor: %v", err)
			}
			got, err := DecryptFor(ct, p)
			if err != nil {
				t.Fatalf("DecryptFor: %v", err)
			}
			if string(got) != "hunter2" {
				t.Errorf("round-tripped to %q", got)
			}
		})
	}
}

// TestBindingRefusesTheWrongPurpose is the defect #277 describes: a blob moved
// between two encrypted columns decrypting "successfully" with no signal.
func TestBindingRefusesTheWrongPurpose(t *testing.T) {
	withKey(t)
	ct, err := EncryptFor([]byte("the-smtp-password"), PurposeSMTPRelayPassword)
	if err != nil {
		t.Fatalf("EncryptFor: %v", err)
	}
	// The move: the same key opens both columns, so before binding this
	// succeeded.
	got, err := DecryptFor(ct, PurposeStateSourceCredentials)
	if !errors.Is(err, ErrPurposeMismatch) {
		t.Fatalf("a ciphertext sealed for SMTP opened as state-source credentials (%q, err %v). "+
			"That is exactly the move #277 is about.", got, err)
	}
}

// TestAMismatchNeverFallsBackToLegacy is the property that keeps the binding
// meaningful.
//
// The registry's equivalent helper retried the LEGACY (nil-AAD) path when the
// bound read failed, which meant a wrong AAD opened the value anyway and could
// then be re-sealed wrongly, self-certifying (terraform-registry-backend#878).
// A bound value must never be openable under the wrong purpose by any route.
func TestAMismatchNeverFallsBackToLegacy(t *testing.T) {
	withKey(t)
	ct, err := EncryptFor([]byte("secret"), PurposeOIDCClientSecret)
	if err != nil {
		t.Fatalf("EncryptFor: %v", err)
	}
	for p := range knownPurposes {
		if p == PurposeOIDCClientSecret {
			continue
		}
		if pt, err := DecryptFor(ct, p); err == nil {
			t.Fatalf("opened under %q as %q -- a bound ciphertext must not be readable under any "+
				"other purpose, by any path", p, pt)
		}
	}
}

// TestLegacyCiphertextStillOpens is the migration property, and the one that
// makes shipping this without a sweep safe.
//
// Every existing row in every deployment is unbound. If this ever fails, the
// release makes seven columns of credentials unreadable.
func TestLegacyCiphertextStillOpens(t *testing.T) {
	withKey(t)
	legacy, err := Encrypt([]byte("written-by-an-older-release"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	for p := range knownPurposes {
		got, err := DecryptFor(legacy, p)
		if err != nil {
			t.Fatalf("an unbound ciphertext failed to open under %q: %v.\nEvery existing row in "+
				"every deployment is unbound; this failing means the release destroys them.", p, err)
		}
		if string(got) != "written-by-an-older-release" {
			t.Fatalf("legacy value opened to %q", got)
		}
	}
}

// TestUnknownPurposeIsRefusedBothWays. A frame naming a purpose this build does
// not implement must not be opened -- that is what stops a downgrade to an
// older binary reading a value bound by a newer scheme.
func TestUnknownPurposeIsRefusedBothWays(t *testing.T) {
	withKey(t)
	if _, err := EncryptFor([]byte("x"), Purpose("tsm/v9:invented")); !errors.Is(err, ErrUnknownPurpose) {
		t.Errorf("EncryptFor accepted an unknown purpose: %v", err)
	}
	if _, err := DecryptFor([]byte("x"), Purpose("tsm/v9:invented")); !errors.Is(err, ErrUnknownPurpose) {
		t.Errorf("DecryptFor accepted an unknown purpose: %v", err)
	}

	// A frame stamped with a purpose this build does not know.
	ct, err := EncryptFor([]byte("x"), PurposeCISourcePAT)
	if err != nil {
		t.Fatalf("EncryptFor: %v", err)
	}
	forged := append([]byte{}, frameMagic...)
	forged = append(forged, byte(len("tsm/v9:from-the-future")))
	forged = append(forged, []byte("tsm/v9:from-the-future")...)
	forged = append(forged, ct[len(frameMagic)+1+len(PurposeCISourcePAT):]...)
	if _, err := DecryptFor(forged, PurposeCISourcePAT); !errors.Is(err, ErrUnknownPurpose) {
		t.Errorf("a frame naming an unknown purpose was not refused: %v", err)
	}
}

// TestStampIsKeylessAndAdvisory.
func TestStampIsKeylessAndAdvisory(t *testing.T) {
	withKey(t)
	ct, err := EncryptFor([]byte("x"), PurposeCISourcePAT)
	if err != nil {
		t.Fatalf("EncryptFor: %v", err)
	}
	if p, ok := Stamp(ct); !ok || p != PurposeCISourcePAT {
		t.Errorf("Stamp = (%q, %v), want the sealed purpose", p, ok)
	}
	legacy, _ := Encrypt([]byte("x"))
	if p, ok := Stamp(legacy); ok {
		t.Errorf("an unbound ciphertext reported a stamp %q", p)
	}
	// Keyless: it must work with no key configured at all, because the census
	// runs against a read replica without one.
	t.Setenv("TSM_ENCRYPTION_KEY", "")
	if _, ok := Stamp(ct); !ok {
		t.Error("Stamp needs a key; the census cannot run keyless")
	}
}

// TestShortAndTruncatedValuesAreTreatedAsLegacy.
//
// A value too short to be a frame must fall through to the legacy path rather
// than being read as a malformed bound one -- otherwise a corrupt row becomes
// permanently unopenable instead of merely failing.
func TestShortAndTruncatedValuesAreTreatedAsLegacy(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"shorter than the magic", frameMagic[:4]},
		{"magic but no length byte", frameMagic},
		{"length byte of zero", append(append([]byte{}, frameMagic...), 0)},
		{"declared length overruns", append(append([]byte{}, frameMagic...), 200)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, framed := parseFrame(tc.in); framed {
				t.Error("treated as a frame; a value that cannot be one must fall through to legacy")
			}
		})
	}
}

// TestEncryptForVerifiesItsOwnOutput. The seal is opened again before it is
// returned, so a configuration that would write an unreadable secret fails the
// request instead of persisting one.
func TestEncryptForVerifiesItsOwnOutput(t *testing.T) {
	withKey(t)
	ct, err := EncryptFor([]byte("payload"), PurposeStateSourceCredentials)
	if err != nil {
		t.Fatalf("EncryptFor: %v", err)
	}
	if !bytes.HasPrefix(ct, frameMagic) {
		t.Fatal("output is not framed")
	}
	if bytes.Contains(ct, []byte("payload")) {
		t.Error("the plaintext appears in the ciphertext")
	}
}

// TestPurposesAreSemanticNotSchematic.
//
// The registry is permanently stuck binding its SCM tokens with the literal
// "scm_user_tokens:", naming a table that has never existed -- and it cannot be
// corrected, because changing an AAD makes every existing ciphertext
// unopenable. A purpose that names the SECRET rather than its storage location
// cannot rot that way.
func TestPurposesAreSemanticNotSchematic(t *testing.T) {
	schemaish := []string{"_table", "s.", "column", "_encrypted", "system_settings", "_id"}
	for p := range knownPurposes {
		if !strings.HasPrefix(string(p), "tsm/v1:") {
			t.Errorf("%q is not versioned; a scheme change must be a new constant, not a "+
				"reinterpretation of this one", p)
		}
		for _, bad := range schemaish {
			if strings.Contains(string(p), bad) {
				t.Errorf("%q looks like a schema reference (%q). Name the SECRET: a purpose that "+
					"names a table cannot be corrected when the table is renamed, because changing "+
					"an AAD makes every existing ciphertext unopenable.", p, bad)
			}
		}
	}
	if len(knownPurposes) < 7 {
		t.Errorf("only %d purposes are declared; the inventory says seven secret types",
			len(knownPurposes))
	}
}

// TestVerifyRoundTripRejectsAValueThatDoesNotOpen falsifies the verification
// directly.
//
// Its removal is undetectable against correct input -- the seal opens either
// way -- so a mutation deleting it looked harmless. The only way to prove it
// does something is to hand it a value that does not round-trip.
func TestVerifyRoundTripRejectsAValueThatDoesNotOpen(t *testing.T) {
	withKey(t)
	good, err := EncryptFor([]byte("payload"), PurposeCISourcePAT)
	if err != nil {
		t.Fatalf("EncryptFor: %v", err)
	}
	if err := verifyRoundTrip(good, []byte("payload"), PurposeCISourcePAT); err != nil {
		t.Fatalf("a correct value was rejected: %v", err)
	}

	// A corrupted tag: the frame still parses, the open fails.
	corrupt := append([]byte{}, good...)
	corrupt[len(corrupt)-1] ^= 0xFF
	if err := verifyRoundTrip(corrupt, []byte("payload"), PurposeCISourcePAT); err == nil {
		t.Error("a ciphertext that does not open was accepted. Without this check a write that " +
			"produced an unreadable secret would succeed silently, and the credential would be " +
			"found broken later by a failing connection.")
	}

	// Right ciphertext, wrong expectation: catches a seal that encrypted
	// something other than what the caller passed.
	if err := verifyRoundTrip(good, []byte("different"), PurposeCISourcePAT); err == nil {
		t.Error("a value that round-tripped to different bytes was accepted")
	}
}

// TestUnknownPurposeIsRefusedBeforeSealing.
//
// EncryptFor rejects an unknown purpose UP FRONT. Removing that check was
// initially undetectable, because the round-trip verification calls DecryptFor
// which rejects it anyway -- defence in depth masking the specific guard. This
// asserts the error comes from the up-front check by requiring it NOT to be the
// round-trip wrapper: an unknown purpose should never reach a Seal at all.
func TestUnknownPurposeIsRefusedBeforeSealing(t *testing.T) {
	withKey(t)
	_, err := EncryptFor([]byte("x"), Purpose("tsm/v9:invented"))
	if !errors.Is(err, ErrUnknownPurpose) {
		t.Fatalf("err = %v, want ErrUnknownPurpose", err)
	}
	if strings.Contains(err.Error(), "round trip") {
		t.Errorf("the unknown purpose was caught by the round-trip verification rather than up "+
			"front: %v.\nIt should be refused before anything is sealed -- relying on the "+
			"verification means the check exists only as a side effect of another guard.", err)
	}
}

// TestEncryptForCallsVerifyRoundTrip pins a call that cannot be observed.
//
// Deleting the CALL is behaviourally invisible: a correct seal opens either
// way, so every functional test still passes. That is the same shape as a
// vacuity floor -- the guard protects against a future defect, so present-day
// behaviour cannot distinguish its presence from its absence.
//
// Checked at the source level, which is the estate's usual answer for wiring
// that has no observable effect until the day it matters.
func TestEncryptForCallsVerifyRoundTrip(t *testing.T) {
	src, err := os.ReadFile("purpose.go")
	if err != nil {
		t.Fatalf("read purpose.go: %v", err)
	}
	body, ok := funcBodyOf(string(src), "EncryptFor")
	if !ok {
		t.Fatal("EncryptFor not found. If it was renamed, point this guard at the new name rather " +
			"than deleting it: it is the only thing keeping the seal from going unverified.")
	}
	if !strings.Contains(body, "verifyRoundTrip(") {
		t.Error("EncryptFor does not verify its own output.\n" +
			"Without it, a configuration that seals an unreadable secret succeeds silently and the " +
			"credential is found broken later by a failing connection. The check costs one GCM " +
			"open on a path that runs when an administrator saves a credential.")
	}
}

// funcBodyOf returns the body of a top-level func, matched by brace depth so a
// nested brace cannot end it early.
func funcBodyOf(src, name string) (string, bool) {
	i := strings.Index(src, "\nfunc "+name+"(")
	if i < 0 {
		return "", false
	}
	open := strings.Index(src[i:], "{")
	if open < 0 {
		return "", false
	}
	start := i + open
	depth := 0
	for j := start; j < len(src); j++ {
		switch src[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : j+1], true
			}
		}
	}
	return "", false
}

// TestEveryPurposeFitsTheLengthByte makes the int->byte conversion in the frame
// header provably safe rather than suppressed.
//
// The header carries the purpose length in ONE byte. A longer purpose would
// wrap, parseFrame would split at the wrong offset, and the result would be a
// value that neither opens nor says why. EncryptFor refuses it; this proves no
// declared purpose can reach that refusal, so the conversion is safe by
// construction and not merely by convention.
func TestEveryPurposeFitsTheLengthByte(t *testing.T) {
	if len(knownPurposes) == 0 {
		t.Fatal("no purposes declared; this check is vacuous")
	}
	for p := range knownPurposes {
		if len(p) == 0 {
			t.Errorf("purpose %q is empty; parseFrame treats a zero length as not-a-frame", p)
		}
		if len(p) > maxPurposeLen {
			t.Errorf("purpose %q is %d bytes, over the %d-byte frame limit", p, len(p), maxPurposeLen)
		}
	}
}

// TestEncryptForRefusesAnOverlongPurpose falsifies the guard directly: no
// declared purpose is long enough to trip it, so the only way to prove it does
// anything is to construct one.
func TestEncryptForRefusesAnOverlongPurpose(t *testing.T) {
	withKey(t)
	long := Purpose("tsm/v1:" + strings.Repeat("x", maxPurposeLen))
	// Registered only for this test, so the length check is what refuses it
	// rather than the unknown-purpose check.
	knownPurposes[long] = true
	t.Cleanup(func() { delete(knownPurposes, long) })

	if _, err := EncryptFor([]byte("x"), long); err == nil {
		t.Error("a purpose too long for the one-byte length header was accepted. It would wrap, " +
			"and parseFrame would split the frame at the wrong offset.")
	}
}
