package pipelines

import (
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/egress"
)

// allowLoopbackEgress lets a test reach an httptest server.
//
// Since suite-identity#301 the token exchanges run under this process's egress
// policy, which denies loopback by default -- the point of the change, since
// these requests carry a client secret or an app JWT and previously went out on
// a bare http.Client with no policy at all. Tests therefore have to say so, the
// same way internal/auth's OIDC tests do.
//
// A test that forgets this fails with a visible egress error rather than passing
// for the wrong reason, which is the shape where "the endpoint returned 401" is
// really "the request never left the process".
func allowLoopbackEgress(t *testing.T) {
	t.Helper()
	if err := egress.Configure([]string{"127.0.0.1", "::1"}); err != nil {
		t.Fatalf("egress.Configure: %v", err)
	}
	invalidateMinter()
	t.Cleanup(func() {
		_ = egress.Configure(nil)
		invalidateMinter()
	})
}
