package pipelines

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// testRSAKeyPEM generates a throwaway PKCS#1 RSA private key PEM for signing.
func testRSAKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	return string(pem.EncodeToMemory(block))
}

func TestMintGitHubInstallationToken_RequiresAllFields(t *testing.T) {
	ResetGitHubAppTokenCacheForTest()
	pem := testRSAKeyPEM(t)
	for _, c := range []GitHubAppCreds{
		{InstallationID: "1", PrivateKeyPEM: pem},
		{AppID: "1", PrivateKeyPEM: pem},
		{AppID: "1", InstallationID: "1"},
		{},
	} {
		if _, err := MintGitHubInstallationToken(context.Background(), c); err == nil {
			t.Errorf("MintGitHubInstallationToken(%+v) = nil error, want error", c)
		}
	}
}

func TestMintGitHubInstallationToken_RejectsBadKey(t *testing.T) {
	ResetGitHubAppTokenCacheForTest()
	_, err := MintGitHubInstallationToken(context.Background(),
		GitHubAppCreds{AppID: "1", InstallationID: "2", PrivateKeyPEM: "not-a-pem"})
	if err == nil {
		t.Fatal("expected error on malformed private key")
	}
}

func TestMintGitHubInstallationToken_MintsAndCaches(t *testing.T) {
	ResetGitHubAppTokenCacheForTest()
	pemKey := testRSAKeyPEM(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/app/installations/inst-9/access_tokens") {
			t.Errorf("path = %s, want installation access_tokens", r.URL.Path)
		}
		// The app JWT must be presented as a Bearer token.
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
			t.Errorf("authorization = %q, want a Bearer JWT", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_installtoken","expires_at":"2099-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()
	defer OverrideBaseURLsForTest("", srv.URL)()

	creds := GitHubAppCreds{AppID: "12345", InstallationID: "inst-9", PrivateKeyPEM: pemKey}
	tok, err := MintGitHubInstallationToken(context.Background(), creds)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if tok != "ghs_installtoken" {
		t.Fatalf("token = %q, want ghs_installtoken", tok)
	}
	// Cached: second call makes no HTTP request.
	if _, err := MintGitHubInstallationToken(context.Background(), creds); err != nil {
		t.Fatalf("mint (cached): %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("token endpoint hit %d times, want 1", got)
	}
}

func TestMintGitHubInstallationToken_MapsErrorStatus(t *testing.T) {
	ResetGitHubAppTokenCacheForTest()
	pemKey := testRSAKeyPEM(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()
	defer OverrideBaseURLsForTest("", srv.URL)()

	_, err := MintGitHubInstallationToken(context.Background(),
		GitHubAppCreds{AppID: "1", InstallationID: "bad", PrivateKeyPEM: pemKey})
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %v, want it to mention status 404", err)
	}
}

func TestValidRSAPrivateKey(t *testing.T) {
	if !ValidRSAPrivateKey(testRSAKeyPEM(t)) {
		t.Error("valid PKCS#1 RSA key reported invalid")
	}
	if ValidRSAPrivateKey("-----BEGIN RSA PRIVATE KEY-----\nbm90LWtleQ==\n-----END RSA PRIVATE KEY-----") {
		t.Error("garbage key reported valid")
	}
	if ValidRSAPrivateKey("plain text") {
		t.Error("non-PEM reported valid")
	}
}
