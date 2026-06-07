package driftingest

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	testIssuer   = "https://login.example.test/tenant"
	testAudience = "api://tsm-drift-ingest"
	testKeyID    = "test-key-1"
)

// jwksServer signs tokens with an RSA key and serves the matching public JWKS.
type jwksServer struct {
	priv   *rsa.PrivateKey
	server *httptest.Server
}

// newJWKSServer starts an httptest server exposing /.well-known/jwks.json with
// the public half of a freshly generated RSA key.
func newJWKSServer(t *testing.T) *jwksServer {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	jwks := jose.JSONWebKeySet{
		Keys: []jose.JSONWebKey{{
			Key:       priv.Public(),
			KeyID:     testKeyID,
			Algorithm: string(jose.RS256),
			Use:       "sig",
		}},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &jwksServer{priv: priv, server: srv}
}

// signToken signs a JWT with the server's private key and the given claims.
func (j *jwksServer) signToken(t *testing.T, claims map[string]interface{}) string {
	t.Helper()
	signingKey := jose.SigningKey{
		Algorithm: jose.RS256,
		Key:       j.priv,
	}
	signer, err := jose.NewSigner(signingKey, (&jose.SignerOptions{}).
		WithType("JWT").
		WithHeader("kid", testKeyID))
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return raw
}

// validator builds a Validator whose verifier reads the mock JWKS, skipping live
// OIDC discovery.
func (j *jwksServer) validator() *Validator {
	keySet := oidc.NewRemoteKeySet(context.Background(), j.server.URL+"/.well-known/jwks.json")
	verifier := oidc.NewVerifier(testIssuer, keySet, &oidc.Config{ClientID: testAudience})
	return NewValidatorWithVerifierForTest(verifier, testIssuer, testAudience)
}

func baseClaims() map[string]interface{} {
	now := time.Now()
	return map[string]interface{}{
		"iss": testIssuer,
		"aud": testAudience,
		"sub": "pipeline-sp-123",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
}

func TestValidator_ValidToken(t *testing.T) {
	js := newJWKSServer(t)
	v := js.validator()

	claims := baseClaims()
	claims["scope"] = "drift:write other:read"
	token := js.signToken(t, claims)

	vt, err := v.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("expected valid token, got error: %v", err)
	}
	if vt.Subject != "pipeline-sp-123" {
		t.Errorf("subject = %q, want pipeline-sp-123", vt.Subject)
	}
	if len(vt.Scopes) != 2 || vt.Scopes[0] != "drift:write" {
		t.Errorf("scopes = %v, want [drift:write other:read]", vt.Scopes)
	}
}

func TestValidator_WrongAudience(t *testing.T) {
	js := newJWKSServer(t)
	v := js.validator()

	claims := baseClaims()
	claims["aud"] = "api://some-other-app"
	token := js.signToken(t, claims)

	if _, err := v.Verify(context.Background(), token); err == nil {
		t.Fatal("expected verification to fail for wrong audience")
	}
}

func TestValidator_Expired(t *testing.T) {
	js := newJWKSServer(t)
	v := js.validator()

	claims := baseClaims()
	claims["exp"] = time.Now().Add(-time.Hour).Unix()
	claims["iat"] = time.Now().Add(-2 * time.Hour).Unix()
	token := js.signToken(t, claims)

	if _, err := v.Verify(context.Background(), token); err == nil {
		t.Fatal("expected verification to fail for expired token")
	}
}

func TestValidator_WrongSignature(t *testing.T) {
	js := newJWKSServer(t)
	v := js.validator()

	// Sign with a different key than the one in the served JWKS.
	other := newJWKSServer(t)
	token := other.signToken(t, baseClaims())

	if _, err := v.Verify(context.Background(), token); err == nil {
		t.Fatal("expected verification to fail for token signed by unknown key")
	}
}

func TestValidator_EmptyToken(t *testing.T) {
	js := newJWKSServer(t)
	v := js.validator()
	if _, err := v.Verify(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestNewValidator_RequiresIssuerAndAudience(t *testing.T) {
	if _, err := NewValidator(context.Background(), "", testAudience); err == nil {
		t.Error("expected error for empty issuer")
	}
	if _, err := NewValidator(context.Background(), testIssuer, ""); err == nil {
		t.Error("expected error for empty audience")
	}
}

func TestValidator_ScpFallback(t *testing.T) {
	js := newJWKSServer(t)
	v := js.validator()

	claims := baseClaims()
	claims["scp"] = "drift:write"
	token := js.signToken(t, claims)

	vt, err := v.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vt.Scopes) != 1 || vt.Scopes[0] != "drift:write" {
		t.Errorf("scopes = %v, want [drift:write] from scp claim", vt.Scopes)
	}
}
