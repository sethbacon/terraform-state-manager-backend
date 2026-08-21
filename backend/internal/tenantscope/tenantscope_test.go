package tenantscope

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	idauth "github.com/sethbacon/terraform-suite-identity/identity/auth"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/approles"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/platformadmin"
)

// MUTATION-VERIFIED, not merely green. A test suite for a fail-closed guard is
// worth nothing until the guard has been broken and the suite has objected, so
// each of these was run against a deliberately broken package before landing:
//
//	Permits returns true unconditionally  -> TestScopePermits (4 cases),
//	                                         TestScopePermitsPtr (2),
//	                                         TestScopeEmpty (2),
//	                                         TestResolvedScopeDoesNotCrossOrganizations
//	PlatformAdmin read off the flat scope -> TestResolve/a_flat_admin_scope_is_not_platform-wide
//	API-key carrier exclusion dropped     -> TestResolve/an_API_key_does_not_inherit...
//	membership error swallowed and widened-> TestResolve/a_membership_lookup_failure_is_an_error...
//	Empty ignores PlatformAdmin           -> TestScopeEmpty (2 cases)
//
// A change here that leaves one of those mutations passing has removed a guard,
// whatever the coverage number says.

// The production types must satisfy the seams, or the seams are describing
// something nothing implements. Asserted at compile time rather than exercised,
// because the whole point of the interfaces is that the real implementations
// need a database and these tests must not.
var (
	_ Memberships    = (*approles.Members)(nil)
	_ PlatformAdmins = (*platformadmin.Service)(nil)
)

const (
	orgA   = "11111111-1111-4111-8111-111111111111"
	orgB   = "22222222-2222-4222-8222-222222222222"
	userID = "user-1"
)

var errLookup = errors.New("membership store is unreachable")

// fakeMemberships records what Resolve asked it, so a test can assert on the
// question and not only on the answer — "did not permit org B" would pass just
// as well against a resolver that never ran.
type fakeMemberships struct {
	scope       idstore.OrgScope
	err         error
	calls       int
	gotUserID   string
	gotRequired string
	gotPairs    idauth.ReadWritePairs
}

func (f *fakeMemberships) OrgScopeForUser(_ context.Context, uid, required string, rwPairs idauth.ReadWritePairs) (idstore.OrgScope, error) {
	f.calls++
	f.gotUserID = uid
	f.gotRequired = required
	f.gotPairs = rwPairs
	return f.scope, f.err
}

// fakeAdmins stands in for the platform_admins carrier.
type fakeAdmins struct {
	admin     bool
	err       error
	calls     int
	gotUserID string
}

func (f *fakeAdmins) IsPlatformAdmin(_ context.Context, uid string) (bool, error) {
	f.calls++
	f.gotUserID = uid
	return f.admin, f.err
}

// newContext builds a gin.Context carrying exactly the keys the auth middleware
// would have published. A nil value means "the middleware did not set this key",
// which is a different state from "set to the empty string" and both are tested.
func newContext(t *testing.T, userIDValue any, authMethod any, scopes any) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/sources", nil)
	if userIDValue != nil {
		c.Set("user_id", userIDValue)
	}
	if authMethod != nil {
		c.Set("auth_method", authMethod)
	}
	if scopes != nil {
		c.Set("scopes", scopes)
	}
	return c
}

// --- Permits / PermitsPtr / Empty ---------------------------------------------

func TestScopePermits(t *testing.T) {
	tests := []struct {
		name  string
		scope Scope
		orgID string
		want  bool
	}{
		{
			name:  "zero value permits nothing",
			scope: Scope{},
			orgID: orgA,
			want:  false,
		},
		{
			name:  "the organization the caller was verified in",
			scope: Scope{OrgIDs: []string{orgA}},
			orgID: orgA,
			want:  true,
		},
		{
			// #393 in one row: holding the scope in A must not reach B.
			name:  "another organization is refused",
			scope: Scope{OrgIDs: []string{orgA}},
			orgID: orgB,
			want:  false,
		},
		{
			name:  "one of several organizations",
			scope: Scope{OrgIDs: []string{orgA, orgB}},
			orgID: orgB,
			want:  true,
		},
		{
			// An unowned row is not a public row. See Permits.
			name:  "unowned row is refused to a member",
			scope: Scope{OrgIDs: []string{orgA}},
			orgID: "",
			want:  false,
		},
		{
			name:  "unowned row is refused to a caller with no organizations",
			scope: Scope{},
			orgID: "",
			want:  false,
		},
		{
			name:  "platform admin reaches an organization it holds no membership in",
			scope: Scope{PlatformAdmin: true},
			orgID: orgB,
			want:  true,
		},
		{
			name:  "platform admin reaches an unowned row",
			scope: Scope{PlatformAdmin: true},
			orgID: "",
			want:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.scope.Permits(tt.orgID); got != tt.want {
				t.Errorf("Scope%+v.Permits(%q) = %v, want %v", tt.scope, tt.orgID, got, tt.want)
			}
		})
	}
}

func TestScopePermitsPtr(t *testing.T) {
	orgAValue := orgA
	empty := ""

	tests := []struct {
		name  string
		scope Scope
		orgID *string
		want  bool
	}{
		{
			name:  "NULL owner is refused to a member",
			scope: Scope{OrgIDs: []string{orgA}},
			orgID: nil,
			want:  false,
		},
		{
			name:  "NULL owner is admitted to a platform admin",
			scope: Scope{PlatformAdmin: true},
			orgID: nil,
			want:  true,
		},
		{
			name:  "a set owner is answered as Permits answers it",
			scope: Scope{OrgIDs: []string{orgA}},
			orgID: &orgAValue,
			want:  true,
		},
		{
			// A pointer to "" is a column that scanned as an empty string rather
			// than as NULL. It names no organization either way, so it gets the
			// NULL answer rather than matching some empty-id member.
			name:  "a pointer to the empty string is not an organization",
			scope: Scope{OrgIDs: []string{orgA}},
			orgID: &empty,
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.scope.PermitsPtr(tt.orgID); got != tt.want {
				t.Errorf("Scope%+v.PermitsPtr(%v) = %v, want %v", tt.scope, tt.orgID, got, tt.want)
			}
		})
	}
}

// TestScopeEmpty pins Empty to "can select nothing", which is the property
// callers will branch on to short-circuit a query.
func TestScopeEmpty(t *testing.T) {
	tests := []struct {
		name  string
		scope Scope
		want  bool
	}{
		{name: "zero value", scope: Scope{}, want: true},
		{name: "explicitly empty organization list", scope: Scope{OrgIDs: []string{}}, want: true},
		{name: "one organization", scope: Scope{OrgIDs: []string{orgA}}, want: false},
		{name: "platform admin", scope: Scope{PlatformAdmin: true}, want: false},
		{
			// Belt and braces: a platform admin with no memberships still selects
			// everything, so Empty must not be reading len(OrgIDs) alone.
			name:  "platform admin with no organizations",
			scope: Scope{PlatformAdmin: true, OrgIDs: nil},
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.scope.Empty(); got != tt.want {
				t.Errorf("Scope%+v.Empty() = %v, want %v", tt.scope, got, tt.want)
			}
			// Empty and Permits must agree: a scope that can select nothing must
			// refuse every row, including the unowned one.
			if tt.want {
				for _, id := range []string{orgA, orgB, ""} {
					if tt.scope.Permits(id) {
						t.Errorf("Scope%+v.Empty() is true but Permits(%q) is true", tt.scope, id)
					}
				}
			}
		})
	}
}

// --- Resolve -------------------------------------------------------------------

func TestResolve(t *testing.T) {
	tests := []struct {
		name        string
		userIDValue any
		authMethod  any
		scopes      any
		memberships *fakeMemberships
		admins      *fakeAdmins
		required    auth.Scope

		wantScope       Scope
		wantErr         error
		wantAdminCalls  int
		wantMemberCalls int
	}{
		{
			// FAIL CLOSED. Nothing authenticated this request, so there is nobody
			// whose tenancy could be resolved and nothing may be selected.
			name:        "no principal yields an empty scope",
			memberships: &fakeMemberships{scope: idstore.OrgScopeOrganizations(orgA, orgB)},
			admins:      &fakeAdmins{admin: true},
			required:    auth.ScopeStateRead,

			wantScope:       Scope{},
			wantAdminCalls:  0,
			wantMemberCalls: 0,
		},
		{
			name:        "an empty user id is not a principal",
			userIDValue: "   ",
			memberships: &fakeMemberships{scope: idstore.OrgScopeOrganizations(orgA)},
			admins:      &fakeAdmins{admin: true},
			required:    auth.ScopeStateRead,

			wantScope:       Scope{},
			wantAdminCalls:  0,
			wantMemberCalls: 0,
		},
		{
			// A principal we cannot interpret is a principal we cannot authorize.
			name:        "a non-string user id is not a principal",
			userIDValue: 42,
			memberships: &fakeMemberships{scope: idstore.OrgScopeOrganizations(orgA)},
			admins:      &fakeAdmins{admin: true},
			required:    auth.ScopeStateRead,

			wantScope:       Scope{},
			wantAdminCalls:  0,
			wantMemberCalls: 0,
		},
		{
			name:        "a member gets the organizations the role template qualified",
			userIDValue: userID,
			memberships: &fakeMemberships{scope: idstore.OrgScopeOrganizations(orgA)},
			admins:      &fakeAdmins{},
			required:    auth.ScopeStateRead,

			wantScope:       Scope{OrgIDs: []string{orgA}},
			wantAdminCalls:  1,
			wantMemberCalls: 1,
		},
		{
			// FAIL CLOSED, LOUDLY. The distinction that matters: this is an error,
			// not an empty scope and emphatically not a wide one.
			name:        "a membership lookup failure is an error, not a scope",
			userIDValue: userID,
			memberships: &fakeMemberships{scope: idstore.OrgScopeAllOrganizations(), err: errLookup},
			admins:      &fakeAdmins{},
			required:    auth.ScopeStateRead,

			wantScope:       Scope{},
			wantErr:         errLookup,
			wantAdminCalls:  1,
			wantMemberCalls: 1,
		},
		{
			// The carrier is the ONLY source of platform-wide authority.
			name:        "a carrier row is platform-wide",
			userIDValue: userID,
			memberships: &fakeMemberships{scope: idstore.OrgScopeOrganizations(orgA)},
			admins:      &fakeAdmins{admin: true},
			required:    auth.ScopeStateRead,

			wantScope:       Scope{PlatformAdmin: true},
			wantAdminCalls:  1,
			wantMemberCalls: 0,
		},
		{
			// GUARD tenant-scope-platform-admin. TSM grants `admin` per
			// organization and it merely surfaces flat, so a flat `admin` must buy
			// no tenancy at all beyond what the memberships say. If this row ever
			// reports PlatformAdmin, #393's leak has been rebuilt inside the type
			// that closes it.
			name:        "a flat admin scope is not platform-wide",
			userIDValue: userID,
			scopes:      []string{string(auth.ScopeAdmin)},
			memberships: &fakeMemberships{scope: idstore.OrgScopeOrganizations(orgA)},
			admins:      &fakeAdmins{admin: false},
			required:    auth.ScopeStateRead,

			wantScope:       Scope{OrgIDs: []string{orgA}},
			wantAdminCalls:  1,
			wantMemberCalls: 1,
		},
		{
			// GUARD tenant-scope-key-no-elevation. The owner holds a carrier row;
			// the key must not. middleware.authenticateAPIKey cannot consult the
			// carrier, and neither may this.
			name:        "an API key does not inherit its owner's platform-admin",
			userIDValue: userID,
			authMethod:  "apikey",
			memberships: &fakeMemberships{scope: idstore.OrgScopeOrganizations(orgA)},
			admins:      &fakeAdmins{admin: true},
			required:    auth.ScopeStateRead,

			wantScope:       Scope{OrgIDs: []string{orgA}},
			wantAdminCalls:  0,
			wantMemberCalls: 1,
		},
		{
			name:        "a session is not mistaken for a key",
			userIDValue: userID,
			authMethod:  "jwt_cookie",
			memberships: &fakeMemberships{scope: idstore.OrgScopeOrganizations(orgA)},
			admins:      &fakeAdmins{admin: true},
			required:    auth.ScopeStateRead,

			wantScope:       Scope{PlatformAdmin: true},
			wantAdminCalls:  1,
			wantMemberCalls: 0,
		},
		{
			// An unwired carrier withholds authority; it does not fail the request.
			name:        "an unconfigured carrier falls through to memberships",
			userIDValue: userID,
			memberships: &fakeMemberships{scope: idstore.OrgScopeOrganizations(orgA)},
			admins:      &fakeAdmins{err: platformadmin.ErrNotConfigured},
			required:    auth.ScopeStateRead,

			wantScope:       Scope{OrgIDs: []string{orgA}},
			wantAdminCalls:  1,
			wantMemberCalls: 1,
		},
		{
			// A carrier that is wired but unreachable is an unresolved authority
			// question, which is not a completed "no".
			name:        "a carrier lookup failure is an error",
			userIDValue: userID,
			memberships: &fakeMemberships{scope: idstore.OrgScopeAllOrganizations()},
			admins:      &fakeAdmins{err: errLookup},
			required:    auth.ScopeStateRead,

			wantScope:       Scope{},
			wantErr:         errLookup,
			wantAdminCalls:  1,
			wantMemberCalls: 0,
		},
		{
			name:        "a principal with no qualifying membership selects nothing",
			userIDValue: userID,
			memberships: &fakeMemberships{scope: idstore.OrgScopeOrganizations()},
			admins:      &fakeAdmins{},
			required:    auth.ScopeStateRead,

			wantScope:       Scope{OrgIDs: []string{}},
			wantAdminCalls:  1,
			wantMemberCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newContext(t, tt.userIDValue, tt.authMethod, tt.scopes)

			got, err := Resolve(c, tt.memberships, tt.admins, tt.required)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Resolve error = %v, want %v", err, tt.wantErr)
				}
				// An error must never come back with a usable scope: a caller that
				// logs the error and carries on must still select nothing.
				if !got.Empty() {
					t.Errorf("Resolve returned %+v alongside an error; a failed lookup must "+
						"not widen the scope", got)
				}
			} else if err != nil {
				t.Fatalf("Resolve: unexpected error %v", err)
			}

			if got.PlatformAdmin != tt.wantScope.PlatformAdmin {
				t.Errorf("PlatformAdmin = %v, want %v", got.PlatformAdmin, tt.wantScope.PlatformAdmin)
			}
			if !sameIDs(got.OrgIDs, tt.wantScope.OrgIDs) {
				t.Errorf("OrgIDs = %v, want %v", got.OrgIDs, tt.wantScope.OrgIDs)
			}
			if tt.admins.calls != tt.wantAdminCalls {
				t.Errorf("carrier consulted %d times, want %d", tt.admins.calls, tt.wantAdminCalls)
			}
			if tt.memberships.calls != tt.wantMemberCalls {
				t.Errorf("membership resolver consulted %d times, want %d",
					tt.memberships.calls, tt.wantMemberCalls)
			}
		})
	}
}

// TestResolveAsksTheRightQuestion pins the arguments Resolve hands the
// membership resolver. The scope it returns is only as good as the question it
// asked: `required` threaded through wrongly (or the read/write pairs dropped)
// would produce a plausible-looking scope for the wrong permission.
func TestResolveAsksTheRightQuestion(t *testing.T) {
	memberships := &fakeMemberships{scope: idstore.OrgScopeOrganizations(orgA)}
	c := newContext(t, userID, nil, nil)

	if _, err := Resolve(c, memberships, &fakeAdmins{}, auth.ScopeSourcesManage); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if memberships.gotUserID != userID {
		t.Errorf("resolved for user %q, want %q", memberships.gotUserID, userID)
	}
	if memberships.gotRequired != string(auth.ScopeSourcesManage) {
		t.Errorf("resolved for scope %q, want %q", memberships.gotRequired, auth.ScopeSourcesManage)
	}
	// Without the pairs, an editor holding state:write would not resolve for a
	// route requiring state:read — the write-implies-read table is part of the
	// question, not a convenience.
	want := auth.ReadWritePairs()
	if len(memberships.gotPairs) != len(want) {
		t.Fatalf("read/write pairs = %v, want %v", memberships.gotPairs, want)
	}
	for read, write := range want {
		if memberships.gotPairs[read] != write {
			t.Errorf("read/write pairs = %v, want %v", memberships.gotPairs, want)
		}
	}
}

// TestResolveWithoutResolvers denies rather than widening. A handler wired
// without a membership resolver has no way to verify anything, and the empty
// scope is the only safe answer.
func TestResolveWithoutResolvers(t *testing.T) {
	tests := []struct {
		name        string
		noContext   bool
		memberships Memberships
		admins      PlatformAdmins
	}{
		// A nil context must deny, not panic: a panicking resolver fails OPEN in
		// the worst way, because the request never reaches the code that decides
		// whether it is permitted.
		{name: "no context at all", noContext: true, memberships: &fakeMemberships{}, admins: &fakeAdmins{}},
		{name: "no membership resolver", memberships: nil, admins: &fakeAdmins{}},
		{name: "no resolver of any kind", memberships: nil, admins: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c *gin.Context
			if !tt.noContext {
				c = newContext(t, userID, nil, nil)
			}
			got, err := Resolve(c, tt.memberships, tt.admins, auth.ScopeStateRead)
			if err != nil {
				t.Fatalf("Resolve: unexpected error %v", err)
			}
			if !got.Empty() {
				t.Errorf("Resolve = %+v, want a scope that selects nothing", got)
			}
		})
	}
}

// TestResolveWithoutARequest covers the gin.Context that carries no *http.Request
// — the shape a non-HTTP caller or a hand-built test rig produces. Resolve must
// still answer, because a panic here fails OPEN in the worst way: the request
// never reaches the code that would have decided whether it is permitted.
func TestResolveWithoutARequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("user_id", userID)
	memberships := &fakeMemberships{scope: idstore.OrgScopeOrganizations(orgA)}

	got, err := Resolve(c, memberships, &fakeAdmins{}, auth.ScopeStateRead)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.Permits(orgA) || got.Permits(orgB) {
		t.Errorf("Resolve = %+v, want the caller's own organization only", got)
	}
	if memberships.calls != 1 {
		t.Errorf("membership resolver consulted %d times, want 1", memberships.calls)
	}
}

// TestResolvedScopeDoesNotCrossOrganizations is the end-to-end statement of what
// #393 asked for: the scope a caller resolves in one organization refuses rows
// in another.
func TestResolvedScopeDoesNotCrossOrganizations(t *testing.T) {
	c := newContext(t, userID, nil, []string{string(auth.ScopeStateRead)})
	memberships := &fakeMemberships{scope: idstore.OrgScopeOrganizations(orgA)}

	scope, err := Resolve(c, memberships, &fakeAdmins{}, auth.ScopeStateRead)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if !scope.Permits(orgA) {
		t.Errorf("a caller holding state:read in %s cannot read it", orgA)
	}
	if scope.Permits(orgB) {
		t.Errorf("a caller holding state:read only in %s permitted %s — this is exactly the "+
			"leak #393 exists to close", orgA, orgB)
	}
	if scope.Permits("") {
		t.Errorf("a caller holding state:read in %s permitted an unowned row; NULL means "+
			"'no tenant asserted', not 'public'", orgA)
	}
	if scope.Empty() {
		t.Errorf("scope %+v reports Empty, but it permits %s", scope, orgA)
	}
}

func sameIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
