package pipelines

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/egress"
)

// The minter is cached so that repeated dispatches reuse one connection pool.
// That cache must follow the egress policy, and NOTHING invalidates it when the
// policy changes: egress.Configure is called at startup, and again by the setup
// wizard at runtime, and neither knows this package exists.
//
// So a minter captured before Configure ran would pin the strict default
// forever, and an operator who allow-listed their internal Entra host would
// watch it keep being refused with no way to make it take. This is the only
// test that covers that path -- without it, comparing the cached guard against
// the current one is dead code that mutates away silently.
func TestSharedMinter_FollowsAnEgressReconfigure(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"ado-token","expires_in":3600}`))
	}))
	defer srv.Close()

	restore := OverrideEntraLoginURLForTest(srv.URL)
	defer restore()
	ResetEntraTokenCacheForTest()

	// Start strict, as a process that has not yet configured an allow-list.
	if err := egress.Configure(nil); err != nil {
		t.Fatalf("egress.Configure: %v", err)
	}
	invalidateMinter()
	t.Cleanup(func() {
		_ = egress.Configure(nil)
		invalidateMinter()
	})

	creds := EntraCreds{TenantID: "t", ClientID: "c", ClientSecret: "s"}
	_, err := MintEntraADOToken(context.Background(), creds)
	if err == nil {
		t.Fatal("a loopback token endpoint was reachable under the strict policy")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("the request failed but not visibly because of egress: %v", err)
	}
	if hits != 0 {
		t.Fatalf("the token endpoint was reached %d times under the strict policy", hits)
	}

	// The operator allow-lists their internal host. NOTE: no invalidateMinter()
	// here -- that is the whole point. Production has no such call, so if the
	// cached minter does not notice the new guard on its own, this mint still
	// fails.
	if err := egress.Configure([]string{"127.0.0.1", "::1"}); err != nil {
		t.Fatalf("egress.Configure: %v", err)
	}

	tok, err := MintEntraADOToken(context.Background(), creds)
	if err != nil {
		t.Fatalf("after allow-listing the host the mint still failed -- the cached minter "+
			"did not follow the reconfigure: %v", err)
	}
	if tok != "ado-token" {
		t.Errorf("token = %q, want ado-token", tok)
	}
	if hits != 1 {
		t.Errorf("token endpoint hit %d times, want 1", hits)
	}
}

// The other direction: tightening the policy must take effect too, or a host
// removed from the allow-list keeps being reachable until the process restarts.
func TestSharedMinter_FollowsAnEgressTightening(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"ado-token","expires_in":3600}`))
	}))
	defer srv.Close()

	restore := OverrideEntraLoginURLForTest(srv.URL)
	defer restore()
	ResetEntraTokenCacheForTest()

	if err := egress.Configure([]string{"127.0.0.1", "::1"}); err != nil {
		t.Fatalf("egress.Configure: %v", err)
	}
	invalidateMinter()
	t.Cleanup(func() {
		_ = egress.Configure(nil)
		invalidateMinter()
	})

	creds := EntraCreds{TenantID: "t2", ClientID: "c2", ClientSecret: "s2"}
	if _, err := MintEntraADOToken(context.Background(), creds); err != nil {
		t.Fatalf("mint under the permissive policy: %v", err)
	}

	// Tighten, and use DIFFERENT credentials so the token cache cannot serve
	// the previous answer and mask the egress decision.
	if err := egress.Configure(nil); err != nil {
		t.Fatalf("egress.Configure: %v", err)
	}
	if _, err := MintEntraADOToken(context.Background(), EntraCreds{
		TenantID: "t3", ClientID: "c3", ClientSecret: "s3",
	}); err == nil {
		t.Fatal("the host was removed from the allow-list but the mint still reached it")
	}
}
