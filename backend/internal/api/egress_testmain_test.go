package api

import (
	"os"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/statesource"
)

// TestMain allows loopback in the state-source egress guard for this package's
// tests: the migrate/transfer integration tests point an http backend connector
// at a 127.0.0.1 httptest stub server, which the production guard (which blocks
// loopback, see internal/statesource/egress.go) would otherwise refuse. The
// production egress policy is asserted in the statesource package.
func TestMain(m *testing.M) {
	_ = statesource.ConfigureEgress([]string{
		"127.0.0.1", "::1",
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	})
	os.Exit(m.Run())
}
