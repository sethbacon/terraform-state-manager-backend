package api

import (
	"os"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/egress"
	"github.com/terraform-state-manager/terraform-state-manager/internal/statesource"
)

// TestMain allows loopback in the state-source egress guard for this package's
// tests: the migrate/transfer integration tests point an http backend connector
// at a 127.0.0.1 httptest stub server, which the production guard (which blocks
// loopback, see internal/statesource/egress.go) would otherwise refuse. It also
// permits the OS temp directory as a local state-source root, so the handler
// tests that register a local source over t.TempDir() clear the containment
// check in internal/statesource/roots.go. Both production policies (loopback
// blocked, no roots permitted until configured) are asserted in the statesource
// package.
//
// The identity-module guard (internal/egress) needs the same treatment as of
// terraform-suite-identity v0.25.0, which routes OIDC discovery, JWKS and the
// token exchange through it: the OIDC callback tests run a stub IdP on a
// 127.0.0.1 httptest server, and without this every one of them fails at
// provider construction with "loopback address blocked". That failure is not a
// test artefact — it is exactly what a deployment sees when its IdP is on an
// internal address and security.egress.allowlist does not say so, which is the
// deployment-configuration change v0.25.0 requires. The strict default is
// asserted in internal/egress's own package.
func TestMain(m *testing.M) {
	_ = statesource.ConfigureEgress([]string{
		"127.0.0.1", "::1",
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	})
	_ = egress.Configure([]string{"127.0.0.1", "::1"})
	_ = statesource.ConfigureLocalRoots([]string{os.TempDir()})
	os.Exit(m.Run())
}
