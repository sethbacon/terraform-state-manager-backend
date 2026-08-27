package crypto

// purpose.go binds a ciphertext to what it is FOR (#277).
//
// # The gap
//
// Encrypt and Decrypt pass a nil AAD, so GCM's tag proves a ciphertext has not
// been tampered with but not that it was produced for the column it now
// occupies. One key covers seven secret types, so a blob copied between two
// encrypted fields -- by a faulty migration, or a privileged actor with direct
// database access -- would decrypt "successfully" with no cryptographic signal
// it had moved.
//
// No code path was found that could move one, so this is defence in depth
// rather than a demonstrated exploit. That matters, because it sets how much
// risk the FIX is allowed to carry.
//
// # Why this is forward-only, and why there is no sweep
//
// The obvious completion -- re-seal every existing row under its purpose -- is
// not safe to take, and the reason is specific rather than cautious.
//
// The registry attempted the equivalent (terraform-registry-backend#878) and
// found that a convert-on-read helper's legacy fallback DISCARDS the supplied
// AAD. A sweep that derives the AAD wrongly therefore still opens every unbound
// row, re-seals it under the WRONG AAD, satisfies its own round-trip proof, and
// reports green afterwards -- for ever. The gate cannot catch it, because the
// gate and the sweep derive the AAD the same way.
//
// TSM's write side is worse placed than the registry's for a row-keyed AAD:
// seven of ten Encrypt sites have no row id available, because three
// repositories generate it with gen_random_uuid() and INSERT ... RETURNING, and
// repositories.OIDCConfig has no ID field at all. A row-axis backfill over that
// surface is how a low-severity finding becomes seven columns of unreadable
// credentials.
//
// So: NEW WRITES ARE BOUND, EXISTING ROWS ARE READ AS THEY ARE, FOR EVER. A row
// converts when it is next saved and never otherwise. There is no sweep, no
// backfill, no strict mode and no retirement of the legacy path.
//
// The honest cost is that coverage is a function of how often an administrator
// edits things. On a deployment where nobody touches a source for a year, this
// is closed in the tracker and unmoved in the database.
//
// # Why the ciphertext is self-describing
//
// internal/crypto owns raw bytes with no outer encoding, which lets it do
// something the shared identity TokenCipher cannot: STAMP the purpose into the
// ciphertext. The reader then dispatches on what it finds rather than on what
// it expected, which is what makes deferring the row axis reversible -- a
// later tsm/v2: purpose carrying purpose|rowid arrives the same way, new writes
// only, with v1 and legacy accepted for ever.
//
//	framed:  magic(8) | len(1) | purpose | nonce(12) | ct | tag(16)   AAD = purpose
//	legacy:  nonce(12) | ct | tag(16)                                 AAD = nil
//
// # What is deliberately absent
//
// There is no `bound bool` return and no strict variant. Both exist to support
// convert-on-read, which is the pattern that laundered a wrong AAD in the
// registry. Not offering them makes that pattern unwritable here.

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// Purpose names what a ciphertext is for. It is the AAD.
type Purpose string

// The seven secrets this package seals.
//
// SEMANTIC, never schematic. The registry is permanently stuck binding its SCM
// tokens with the literal "scm_user_tokens:", naming a table that has never
// existed -- and it cannot be corrected, because changing an AAD makes every
// existing ciphertext unopenable. A name that describes the SECRET rather than
// its current storage location cannot rot that way.
//
// Versioned, so a later scheme (say purpose|row-id) is a new constant read
// alongside these rather than a reinterpretation of them.
const (
	PurposeStateSourceCredentials Purpose = "tsm/v1:state-source-credentials"
	PurposeCISourcePAT            Purpose = "tsm/v1:ci-source-pat"
	PurposeCISourceClientSecret   Purpose = "tsm/v1:ci-source-client-secret"
	PurposeCISourceAppPrivateKey  Purpose = "tsm/v1:ci-source-app-private-key"
	PurposePipelineDispatchToken  Purpose = "tsm/v1:pipeline-dispatch-token"
	PurposeOIDCClientSecret       Purpose = "tsm/v1:oidc-client-secret"
	PurposeSMTPRelayPassword      Purpose = "tsm/v1:smtp-relay-password"
)

// knownPurposes is every purpose this build recognises.
//
// A frame naming something absent from this set is refused rather than opened.
// That is what stops a downgrade to an older binary from silently reading a
// ciphertext bound by a scheme it does not implement.
var knownPurposes = map[Purpose]bool{
	PurposeStateSourceCredentials: true,
	PurposeCISourcePAT:            true,
	PurposeCISourceClientSecret:   true,
	PurposeCISourceAppPrivateKey:  true,
	PurposePipelineDispatchToken:  true,
	PurposeOIDCClientSecret:       true,
	PurposeSMTPRelayPassword:      true,
}

// frameMagic opens a bound ciphertext.
//
// Leading NUL so it cannot be confused with anything text-shaped, and eight
// bytes so a legacy ciphertext colliding with it is a 2^-64 event. That is not
// zero, which is why a framed value that fails to open under its own purpose
// still gets tried as legacy -- see DecryptFor.
var frameMagic = []byte{0x00, 'T', 'S', 'M', '-', 'A', 'A', 'D'}

// ErrPurposeMismatch means the ciphertext is bound to a different purpose.
//
// NOT wrapped into a generic failure, and never followed by a legacy retry: a
// value bound to another purpose is the exact condition this whole mechanism
// exists to detect. Opening it anyway would make the binding decorative.
var ErrPurposeMismatch = errors.New("crypto: ciphertext is bound to a different purpose")

// ErrUnknownPurpose means the frame names a purpose this build does not know.
var ErrUnknownPurpose = errors.New("crypto: ciphertext is bound to a purpose this build does not recognise")

// EncryptFor seals plaintext bound to p.
//
// The result is verified by opening it again under the same purpose before it
// is returned. That costs one GCM open on a path that runs when an
// administrator saves a credential, and it converts the entire class of
// "sealed something unreadable" from a silent write into a failed request.
func EncryptFor(plaintext []byte, p Purpose) ([]byte, error) {
	if !knownPurposes[p] {
		return nil, fmt.Errorf("%w: %q", ErrUnknownPurpose, p)
	}
	gcm, err := newGCM()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nonce, nonce, plaintext, []byte(p))

	out := make([]byte, 0, len(frameMagic)+1+len(p)+len(sealed))
	out = append(out, frameMagic...)
	out = append(out, byte(len(p)))
	out = append(out, []byte(p)...)
	out = append(out, sealed...)

	if err := verifyRoundTrip(out, plaintext, p); err != nil {
		return nil, err
	}
	return out, nil
}

// verifyRoundTrip opens a freshly sealed value and checks it came back
// unchanged.
//
// A separate function so it can be falsified DIRECTLY. Its removal is
// undetectable against correct input -- the seal opens either way -- so the
// only way to prove it does something is to hand it a value that does not
// round-trip and require it to object.
//
// What it defends against is a write that produces an unreadable secret:
// without it that is a silent success and a credential nobody can use, found
// later by a failing connection. With it, the request fails.
func verifyRoundTrip(sealed, plaintext []byte, p Purpose) error {
	got, err := DecryptFor(sealed, p)
	if err != nil {
		return fmt.Errorf("crypto: sealed value did not survive its own round trip: %w", err)
	}
	if !bytes.Equal(got, plaintext) {
		return errors.New("crypto: sealed value round-tripped to different bytes")
	}
	return nil
}

// DecryptFor opens a ciphertext that is either bound to p or unbound (legacy).
//
// The dispatch is on what the ciphertext SAYS, never on what the caller
// expected:
//
//   - unbound: opened exactly as Decrypt does today, previous-key retry and
//     all. This is the overwhelming majority of rows and stays so.
//   - bound to p: opened with p as the AAD.
//   - bound to something else: refused. No legacy retry, because that is the
//     path by which a wrong AAD launders itself into looking correct.
func DecryptFor(ciphertext []byte, p Purpose) ([]byte, error) {
	if !knownPurposes[p] {
		return nil, fmt.Errorf("%w: %q", ErrUnknownPurpose, p)
	}
	stamp, body, framed := parseFrame(ciphertext)
	if !framed {
		return Decrypt(ciphertext)
	}
	if !knownPurposes[stamp] {
		return nil, fmt.Errorf("%w: %q", ErrUnknownPurpose, stamp)
	}
	if stamp != p {
		return nil, fmt.Errorf("%w: sealed as %q, opened as %q", ErrPurposeMismatch, stamp, p)
	}
	pt, err := openWithAAD(body, []byte(stamp))
	if err == nil {
		return pt, nil
	}
	// A legacy ciphertext whose first bytes happened to equal the magic is a
	// 2^-64 event, not an impossible one -- and treating it as a bound value
	// that will not open would make it permanently unreadable. Retried as
	// legacy, which cannot launder anything because nothing is re-sealed here.
	if pt, legacyErr := Decrypt(ciphertext); legacyErr == nil {
		return pt, nil
	}
	return nil, err
}

// Stamp reports the purpose a ciphertext is bound to, if any. Keyless.
//
// For diagnosis and for the census only. It must never be used to choose the
// AAD to open with: that would make the ciphertext's own claim authoritative,
// which is precisely the property binding it is meant to remove.
func Stamp(ciphertext []byte) (Purpose, bool) {
	stamp, _, framed := parseFrame(ciphertext)
	if !framed || !knownPurposes[stamp] {
		return "", false
	}
	return stamp, true
}

// parseFrame splits a framed ciphertext. A value is only a frame if the magic
// matches, the declared length fits, and the remainder is long enough to be a
// nonce plus a tag -- so a short or truncated value falls through to legacy
// rather than being mistaken for a bound one.
func parseFrame(ciphertext []byte) (Purpose, []byte, bool) {
	const minSealed = 12 + 16 // nonce + tag
	if len(ciphertext) < len(frameMagic)+1 {
		return "", nil, false
	}
	if !bytes.Equal(ciphertext[:len(frameMagic)], frameMagic) {
		return "", nil, false
	}
	n := int(ciphertext[len(frameMagic)])
	start := len(frameMagic) + 1
	if n == 0 || len(ciphertext) < start+n+minSealed {
		return "", nil, false
	}
	return Purpose(ciphertext[start : start+n]), ciphertext[start+n:], true
}

// openWithAAD opens sealed under the current key, then the previous one.
//
// Mirrors Decrypt's two-key behaviour deliberately: a rotation must not become
// harder for a bound row than for an unbound one, or binding would create a
// reason to avoid binding.
func openWithAAD(sealed, aad []byte) ([]byte, error) {
	gcm, err := newGCM()
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(sealed) < ns {
		return nil, errors.New("ciphertext too short")
	}
	nonce, ct := sealed[:ns], sealed[ns:]
	if pt, err := gcm.Open(nil, nonce, ct, aad); err == nil {
		return pt, nil
	}
	prev, perr := loadPreviousKey()
	if perr != nil {
		return nil, perr
	}
	if prev == nil {
		return nil, errors.New("crypto: decryption failed and no previous key is configured (set TSM_ENCRYPTION_KEY_PREVIOUS during a rotation)")
	}
	prevGCM, gerr := gcmFor(prev)
	if gerr != nil {
		return nil, gerr
	}
	pt, oerr := prevGCM.Open(nil, nonce, ct, aad)
	if oerr != nil {
		return nil, errors.New("crypto: decryption failed under both the current and previous keys")
	}
	markPreviousKeyUsed()
	return pt, nil
}
