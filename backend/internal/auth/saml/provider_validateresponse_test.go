package saml

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/crewjam/saml"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
)

// This file gives ValidateResponse — the SAML authentication decision itself
// (signature / replay / IdP-initiated gating, delegated to crewjam/saml) — real
// end-to-end coverage, which it previously lacked entirely (#268, CWE-347). A
// self-signed test IdP mints genuine signed SAML responses so we can assert that
// a valid response is accepted and that tampered, replayed (wrong InResponseTo),
// unsolicited (IdP-initiated while disabled), and malformed responses are all
// rejected — mirroring the mocked-issuer approach already used for OIDC.

const (
	testSPEntityID = "https://tsm.example.com"
	testACSURL     = "https://tsm.example.com/api/v1/auth/saml/acs"
	testGroupAttr  = "groups"
)

// testIDP is a self-signed SAML IdP capable of minting signed responses.
type testIDP struct{ idp *saml.IdentityProvider }

func mustURL(t *testing.T, s string) url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", s, err)
	}
	return *u
}

func newTestIDP(t *testing.T, now time.Time) *testIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-idp"},
		NotBefore:    now.Add(-1 * time.Hour),
		NotAfter:     now.Add(365 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return &testIDP{idp: &saml.IdentityProvider{
		Key:         key,
		Certificate: cert,
		MetadataURL: mustURL(t, "https://idp.example.com/metadata"),
		SSOURL:      mustURL(t, "https://idp.example.com/sso"),
	}}
}

// metadataXML renders the IdP metadata (with the signing certificate) so the SP
// built by NewProvider trusts responses signed by this IdP.
func (ti *testIDP) metadataXML(t *testing.T) string {
	t.Helper()
	b, err := xml.MarshalIndent(ti.idp.Metadata(), "", "  ")
	if err != nil {
		t.Fatalf("marshal IdP metadata: %v", err)
	}
	return string(b)
}

// mintResponse produces a signed SAML Response (base64, HTTP-POST binding) whose
// assertion binds to requestID via InResponseTo. Pass requestID="" to mint an
// unsolicited (IdP-initiated) response.
func (ti *testIDP) mintResponse(t *testing.T, spMeta *saml.EntityDescriptor, requestID string, now time.Time, session *saml.Session) string {
	t.Helper()
	req := &saml.IdpAuthnRequest{
		IDP:                     ti.idp,
		Now:                     now,
		HTTPRequest:             httptest.NewRequest(http.MethodPost, testACSURL, nil),
		ServiceProviderMetadata: spMeta,
		SPSSODescriptor:         &spMeta.SPSSODescriptors[0],
		ACSEndpoint:             &saml.IndexedEndpoint{Binding: saml.HTTPPostBinding, Location: testACSURL},
		Request:                 saml.AuthnRequest{ID: requestID, IssueInstant: now, AssertionConsumerServiceURL: testACSURL},
	}
	if err := (saml.DefaultAssertionMaker{}).MakeAssertion(req, session); err != nil {
		t.Fatalf("MakeAssertion: %v", err)
	}
	form, err := req.PostBinding()
	if err != nil {
		t.Fatalf("PostBinding: %v", err)
	}
	return form.SAMLResponse
}

// acsRequest builds the ACS POST carrying a base64 SAMLResponse form value.
// crewjam's ParseResponse reads req.PostForm directly, so the form must already
// be parsed (as it is in production behind the router's body handling).
func acsRequest(samlResponse string) *http.Request {
	form := url.Values{"SAMLResponse": {samlResponse}}
	r := httptest.NewRequest(http.MethodPost, testACSURL, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = r.ParseForm()
	return r
}

func newTestSAMLProvider(t *testing.T, ti *testIDP, allowIDPInitiated bool) *Provider {
	t.Helper()
	cfg := config.SAMLConfig{
		Enabled:           true,
		ACSURL:            testACSURL,
		EntityID:          testSPEntityID,
		AllowIDPInitiated: allowIDPInitiated,
	}
	p, err := NewProvider(cfg, config.SAMLIdPConfig{Name: "test-idp", MetadataXML: ti.metadataXML(t)})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

// overrideSAMLClock pins crewjam's clock so the minted assertion's validity
// window (NotBefore/NotOnOrAfter) is evaluated deterministically.
func overrideSAMLClock(now time.Time) func() {
	prev := saml.TimeNow
	saml.TimeNow = func() time.Time { return now }
	return func() { saml.TimeNow = prev }
}

func testSession(now time.Time) *saml.Session {
	return &saml.Session{
		ID:             "sess-1",
		CreateTime:     now,
		ExpireTime:     now.Add(time.Hour),
		Index:          "idx-1",
		NameID:         "alice@example.com",
		UserEmail:      "alice@example.com",
		UserCommonName: "Alice Example",
		Groups:         []string{"tsm-admins", "tsm-devs"},
	}
}

func TestValidateResponse_ValidSignatureAccepted(t *testing.T) {
	now := time.Now().UTC()
	defer overrideSAMLClock(now)()

	ti := newTestIDP(t, now)
	p := newTestSAMLProvider(t, ti, false)
	const reqID = "id-request-abc123"
	resp := ti.mintResponse(t, p.sp.Metadata(), reqID, now, testSession(now))

	info, err := p.ValidateResponse(acsRequest(resp), []string{reqID}, testGroupAttr)
	if err != nil {
		var ivr *saml.InvalidResponseError
		if errors.As(err, &ivr) && ivr.PrivateErr != nil {
			t.Fatalf("valid signed response was rejected: %v (cause: %v)", err, ivr.PrivateErr)
		}
		t.Fatalf("valid signed response was rejected: %v", err)
	}
	if info.NameID != "alice@example.com" {
		t.Errorf("NameID = %q, want alice@example.com", info.NameID)
	}
	if info.Email != "alice@example.com" {
		t.Errorf("Email = %q, want alice@example.com", info.Email)
	}
}

func TestValidateResponse_TamperedSignatureRejected(t *testing.T) {
	now := time.Now().UTC()
	defer overrideSAMLClock(now)()

	ti := newTestIDP(t, now)
	p := newTestSAMLProvider(t, ti, false)
	const reqID = "id-request-abc123"
	resp := ti.mintResponse(t, p.sp.Metadata(), reqID, now, testSession(now))

	raw, err := base64.StdEncoding.DecodeString(resp)
	if err != nil {
		t.Fatalf("decode minted response: %v", err)
	}
	// Flip the authenticated subject after signing: the enveloped signature no
	// longer matches, so a correct SP must reject it.
	tampered := strings.Replace(string(raw), "alice@example.com", "attacker@evil.example", 1)
	if tampered == string(raw) {
		t.Fatal("test bug: subject not present to tamper")
	}
	bad := base64.StdEncoding.EncodeToString([]byte(tampered))

	if _, err := p.ValidateResponse(acsRequest(bad), []string{reqID}, testGroupAttr); err == nil {
		t.Fatal("tampered SAML response was accepted; signature is not enforced")
	}
}

func TestValidateResponse_WrongInResponseToRejected(t *testing.T) {
	now := time.Now().UTC()
	defer overrideSAMLClock(now)()

	ti := newTestIDP(t, now)
	p := newTestSAMLProvider(t, ti, false)
	resp := ti.mintResponse(t, p.sp.Metadata(), "id-request-abc123", now, testSession(now))

	// A validly-signed response whose InResponseTo does not match any pending
	// AuthnRequest is a replay / unsolicited response and must be rejected.
	if _, err := p.ValidateResponse(acsRequest(resp), []string{"id-some-other-request"}, testGroupAttr); err == nil {
		t.Fatal("response with unmatched InResponseTo was accepted (replay protection missing)")
	}
}

func TestValidateResponse_IDPInitiatedRejectedWhenDisabled(t *testing.T) {
	now := time.Now().UTC()
	defer overrideSAMLClock(now)()

	ti := newTestIDP(t, now)
	p := newTestSAMLProvider(t, ti, false) // AllowIDPInitiated=false (the secure default)
	// Unsolicited response: empty InResponseTo.
	resp := ti.mintResponse(t, p.sp.Metadata(), "", now, testSession(now))

	if _, err := p.ValidateResponse(acsRequest(resp), nil, testGroupAttr); err == nil {
		t.Fatal("IdP-initiated response was accepted while AllowIDPInitiated is false")
	}
}

func TestValidateResponse_MalformedRejected(t *testing.T) {
	now := time.Now().UTC()
	defer overrideSAMLClock(now)()

	ti := newTestIDP(t, now)
	p := newTestSAMLProvider(t, ti, false)

	for name, payload := range map[string]string{
		"not base64":       "!!! not base64 !!!",
		"base64 not xml":   base64.StdEncoding.EncodeToString([]byte("hello, not a saml response")),
		"empty SAMLResponse": "",
	} {
		if _, err := p.ValidateResponse(acsRequest(payload), []string{"id-x"}, testGroupAttr); err == nil {
			t.Errorf("%s: malformed SAMLResponse was accepted", name)
		}
	}
}
