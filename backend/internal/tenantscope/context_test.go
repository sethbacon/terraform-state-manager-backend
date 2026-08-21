package tenantscope

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// MUTATION-VERIFIED. The property under test is not "a value round-trips" — it
// is that FromContext's SECOND return value distinguishes "resolved, and permits
// nothing" from "not resolved at all", because Phase 3 must 500 on the second
// and read nothing on the first. Each case below was run against a FromContext
// that returned `true` unconditionally, and against one that ignored the type
// assertion, and objected to both.

func testContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/", nil)
	return c
}

func TestFromContext(t *testing.T) {
	t.Run("nothing stored is not resolved", func(t *testing.T) {
		scope, ok := FromContext(testContext())
		if ok {
			t.Fatal("FromContext reported a scope on a request that never had one; " +
				"Phase 3 would read this as an answer and serve rows unscoped")
		}
		if !scope.Empty() {
			t.Fatal("the scope returned alongside ok == false must permit nothing")
		}
	})

	t.Run("an empty scope IS resolved", func(t *testing.T) {
		c := testContext()
		Store(c, Scope{})
		scope, ok := FromContext(c)
		if !ok {
			t.Fatal("a resolved-but-empty scope must report ok; it is a real answer " +
				"(this caller may read nothing), not an absent one")
		}
		if !scope.Empty() {
			t.Fatal("Scope{} must permit nothing")
		}
	})

	t.Run("a resolved scope round-trips", func(t *testing.T) {
		c := testContext()
		Store(c, Scope{OrgIDs: []string{orgA}})
		scope, ok := FromContext(c)
		if !ok {
			t.Fatal("a stored scope must be reported as resolved")
		}
		if !scope.Permits(orgA) || scope.Permits(orgB) {
			t.Fatalf("scope did not survive the context: %+v", scope)
		}
	})

	t.Run("a platform-admin scope round-trips", func(t *testing.T) {
		c := testContext()
		Store(c, Scope{PlatformAdmin: true})
		scope, ok := FromContext(c)
		if !ok || !scope.PlatformAdmin {
			t.Fatalf("platform-admin scope did not survive the context: %+v ok=%v", scope, ok)
		}
	})

	t.Run("a foreign value under the key is not resolved", func(t *testing.T) {
		// Something else wrote the key. A value that cannot be interpreted
		// cannot authorize anything, so it is reported as no answer rather than
		// as an empty one — and never as a panic on a request path.
		c := testContext()
		c.Set(ContextKey, []string{orgA})
		if _, ok := FromContext(c); ok {
			t.Fatal("a non-Scope value under the key was reported as a resolved scope")
		}
	})

	t.Run("a nil context is not resolved", func(t *testing.T) {
		if _, ok := FromContext(nil); ok {
			t.Fatal("FromContext(nil) reported a resolved scope")
		}
	})
}

// Store must not panic on the synthetic contexts a non-HTTP caller may hand it,
// for the reason requestContext exists: a resolver that panicked would fail open
// in the worst way, before the code that decides what is permitted ever runs.
func TestStoreNilContext(t *testing.T) {
	Store(nil, Scope{PlatformAdmin: true})
}
