package api

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
	"github.com/terraform-state-manager/terraform-state-manager/internal/approles"
)

// admin_audit_scope_test.go covers the tenant narrowing on audit-log READS as a
// CLASS rather than per handler (#331, and #298/#182 before it).
//
// The defect this guards is resource x access-axis: audit_logs is one table
// reachable through several /admin routes, and #331 was a single axis (the bulk
// export) missing the narrowing its sibling (the paginated list) already had.
// Testing one handler would have reported green while the export leaked, which
// is exactly what happened. So every axis that reaches AuditRepository's read
// accessors gets a row here, and adding an axis without a row leaves a hole.
//
// Since identity v0.21.0 the narrowing is a SQL predicate built from
// store.OrgScope (store.AuditScope until v0.25.0 renamed it), not a post-filter
// over rows the database already returned.
// That means the guard is only observable in the statement sent to the
// database — a handler that dropped its scope would still answer 200 with
// whatever rows the mock was told to hand back. These tests therefore assert on
// the captured SQL and its bind arguments, not on status codes alone.

// --- axis coverage -----------------------------------------------------------
//
// AuditRepository exposes three read accessors in v0.22.0: ListAuditLogs,
// GetAuditLog (by id) and StreamAuditLogs. TSM calls only ListAuditLogs; there
// is no by-id route and nothing streams the table (verified by grepping the
// non-test tree for all three names). The by-id axis therefore has no row
// below because it has no call site — TestAuditReadAccessorsInUse fails if that
// stops being true, so a newly wired accessor cannot arrive untested.

const (
	auditScopeOrgA   = "org-a" // the caller administers this one
	auditScopeOrgB   = "org-b" // a different tenant's organization
	auditScopeCaller = "caller-1"
	auditScopeTarget = "target-1"
)

// wantScopedPredicate is the SQL the store emits for "these organizations, plus
// rows nobody owns". Asserting the literal fragment pins the specific failure:
// a dropped scope removes it entirely and a scope narrowed to
// OrgScopeOrganizations drops the "OR ... IS NULL" half, and the two are
// different bugs with different fixes.
const wantScopedPredicate = "AND (al.organization_id = ANY($1) OR al.organization_id IS NULL)"

// --- harness -----------------------------------------------------------------

// auditSQLRecorder captures every statement sqlmock is asked to match so a test
// can assert on the tenant predicate the handler actually sent.
type auditSQLRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *auditSQLRecorder) record(sql string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, sql)
}

// auditReads returns the statements that read audit_logs (the count and the
// page ListAuditLogs issues), excluding the INSERT the export writes about
// itself. sqlmock consults the matcher once per candidate expectation, so the
// same statement can be recorded more than once; the assertions below are
// per-statement and unaffected.
func (r *auditSQLRecorder) auditReads() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.seen))
	for _, q := range r.seen {
		if strings.Contains(q, "FROM audit_logs") {
			out = append(out, q)
		}
	}
	return out
}

type auditScopeEnv struct {
	h    *AdminHandlers
	mock sqlmock.Sqlmock
	rec  *auditSQLRecorder
}

func newAuditScopeEnv(t *testing.T) *auditScopeEnv {
	t.Helper()
	rec := &auditSQLRecorder{}
	db, mock, err := newSQLMockMatching(
		sqlmock.QueryMatcherFunc(func(expectedSQL, actualSQL string) error {
			rec.record(actualSQL)
			return sqlmock.QueryMatcherRegexp.Match(expectedSQL, actualSQL)
		}),
	)
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &auditScopeEnv{h: NewAdminHandlers(db, nil, approles.RoleSourceIdentity), mock: mock, rec: rec}
}

// serveAs runs handler with callerID installed the way the real requireAuth
// middleware installs it, so callerAuditScope can resolve the caller.
func (e *auditScopeEnv) serveAs(route, target, callerID string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", callerID)
		c.Next()
	})
	r.GET(route, handler)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

// expectAdminMemberships queues the caller lookup OrgScopeForUser issues,
// returning an admin-scoped membership per organization given.
func expectAdminMemberships(mock sqlmock.Sqlmock, userID string, adminOrgIDs ...string) {
	rows := sqlmock.NewRows(userMembershipCols)
	for _, orgID := range adminOrgIDs {
		rows.AddRow(orgID, "Org "+orgID, "rt-admin", time.Now(), "admin", "Admin", []byte(`["admin"]`))
	}
	mock.ExpectQuery("FROM organization_members om").WithArgs(userID).WillReturnRows(rows)
}

// scopedAuditRow builds one audit row owned by orgID ("" meaning an org-less
// platform event, which is how TSM records logins and state/source actions).
func scopedAuditRow(rows *sqlmock.Rows, id, action, orgID string) *sqlmock.Rows {
	var org interface{}
	if orgID != "" {
		org = orgID
	}
	return rows.AddRow(id, "u1", org, action, "state", nil, nil, "10.0.0.9",
		time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC), "a@x.io", "a@x.io", "Alice")
}

// --- the class test ----------------------------------------------------------

// auditReadAxis is one route through which the /admin surface reads audit_logs.
type auditReadAxis struct {
	name string
	// exercise queues the axis's own round-trips around the ListAuditLogs
	// count+page pair and issues the request. wantOrgs is the organization
	// allowlist the tenant predicate must carry; page is what the (notionally
	// already-filtered) database hands back.
	exercise func(e *auditScopeEnv, callerID string, wantOrgs []string, page *sqlmock.Rows) *httptest.ResponseRecorder
}

var auditReadAxes = []auditReadAxis{
	{
		// GET /admin/audit-logs — admin.go ListAuditLogs.
		name: "list",
		exercise: func(e *auditScopeEnv, callerID string, wantOrgs []string, page *sqlmock.Rows) *httptest.ResponseRecorder {
			expectAdminMemberships(e.mock, callerID, wantOrgs...)
			e.mock.ExpectQuery(`SELECT COUNT\(\*\) FROM audit_logs`).WithArgs(wantOrgs).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
			e.mock.ExpectQuery("SELECT al.id, .+ FROM audit_logs").
				WithArgs(wantOrgs, sqlmock.AnyArg(), sqlmock.AnyArg()).
				WillReturnRows(page)
			return e.serveAs("/x", "/x", callerID, e.h.ListAuditLogs())
		},
	},
	{
		// GET /admin/audit-logs/export — admin_audit_export.go ExportAuditLogs.
		// This is the axis that carried no tenant predicate at all (#331).
		name: "export",
		exercise: func(e *auditScopeEnv, callerID string, wantOrgs []string, page *sqlmock.Rows) *httptest.ResponseRecorder {
			expectAdminMemberships(e.mock, callerID, wantOrgs...)
			e.mock.ExpectQuery(`SELECT COUNT\(\*\) FROM audit_logs`).WithArgs(wantOrgs).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
			e.mock.ExpectQuery("SELECT al.id, .+ FROM audit_logs").
				WithArgs(wantOrgs, sqlmock.AnyArg(), sqlmock.AnyArg()).
				WillReturnRows(page)
			e.mock.ExpectQuery("INSERT INTO audit_logs").WillReturnRows(auditInsertReturn())
			return e.serveAs("/x", "/x?format=json", callerID, e.h.ExportAuditLogs())
		},
	},
	{
		// GET /admin/users/:id/export — admin_write.go ExportUserData (GDPR).
		// Reads the same table filtered to one subject; the caller's own tenancy
		// still bounds it, because the route's gate only proves the caller shares
		// ONE organization with the target.
		name: "user-export",
		exercise: func(e *auditScopeEnv, callerID string, wantOrgs []string, page *sqlmock.Rows) *httptest.ResponseRecorder {
			// The caller's scope is resolved FIRST since v0.25.0: it bounds the
			// subject lookup as well as the audit read, so the export cannot
			// disclose through one axis what it refuses on the other.
			expectAdminMemberships(e.mock, callerID, wantOrgs...)
			e.mock.ExpectQuery("SELECT id, email, name, oidc_sub").
				WithArgs(auditScopeTarget, wantOrgs).
				WillReturnRows(idUserRow(auditScopeTarget))
			e.mock.ExpectQuery("FROM organization_members om").WithArgs(auditScopeTarget).
				WillReturnRows(sqlmock.NewRows(userMembershipCols))
			e.mock.ExpectQuery(`SELECT COUNT\(\*\) FROM audit_logs`).
				WithArgs(wantOrgs, auditScopeTarget).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
			e.mock.ExpectQuery("SELECT al.id, .+ FROM audit_logs").
				WithArgs(wantOrgs, auditScopeTarget, sqlmock.AnyArg(), sqlmock.AnyArg()).
				WillReturnRows(page)
			e.mock.ExpectQuery("INSERT INTO audit_logs").WillReturnRows(auditInsertReturn())
			return e.serveAs("/x/:id", "/x/"+auditScopeTarget, callerID, e.h.ExportUserData())
		},
	},
}

// TestAuditReadScope_NarrowsEveryAxis asserts that every audit read axis pushes
// the caller's tenancy into the query.
//
// Two caller shapes per axis:
//
//   - a single-org admin, who must get their own organization's rows PLUS
//     org-less platform events and nothing else; and
//   - an admin of every organization — TSM's platform-wide operator, since
//     "admin" is a per-organization grant that surfaces as a flat scope and
//     there is no separate platform-admin role — who must get everything.
//
// The second shape is not redundant: it is what fails if a fix over-corrects
// into refusing rows the caller is entitled to.
func TestAuditReadScope_NarrowsEveryAxis(t *testing.T) {
	callers := []struct {
		name        string
		adminOrgs   []string
		wantVisible map[string]bool // organization id ("" = unowned) -> visible?
	}{
		{
			name:      "single-org admin sees own org and unowned only",
			adminOrgs: []string{auditScopeOrgA},
			wantVisible: map[string]bool{
				auditScopeOrgA: true,
				auditScopeOrgB: false,
				"":             true,
			},
		},
		{
			name:      "admin of every org sees everything",
			adminOrgs: []string{auditScopeOrgA, auditScopeOrgB},
			wantVisible: map[string]bool{
				auditScopeOrgA: true,
				auditScopeOrgB: true,
				"":             true,
			},
		},
	}

	for _, axis := range auditReadAxes {
		for _, caller := range callers {
			t.Run(axis.name+"/"+caller.name, func(t *testing.T) {
				e := newAuditScopeEnv(t)

				// The database is stubbed with exactly the rows the predicate
				// admits, mirroring what a real Postgres would return.
				page := sqlmock.NewRows(auditRowCols)
				for orgID, visible := range caller.wantVisible {
					if visible {
						page = scopedAuditRow(page, "row-"+orgID, "state.edit", orgID)
					}
				}

				w := axis.exercise(e, auditScopeCaller, caller.adminOrgs, page)

				// The guard itself, asserted FIRST and independently of the
				// response: every statement that reads audit_logs must carry the
				// tenant predicate. Order matters here. A handler that drops its
				// scope sends a query whose bind arguments no longer match, which
				// surfaces as a 500 — and "status = 500" is exactly the
				// uninformative error-vs-nil signal that has twice let a removed
				// guard pass in this codebase for an unrelated reason. Checking
				// the SQL before the status makes the failure name the missing
				// predicate.
				reads := e.rec.auditReads()
				if len(reads) == 0 {
					t.Fatal("no audit_logs read reached the database")
				}
				for _, q := range reads {
					if !strings.Contains(q, wantScopedPredicate) {
						t.Errorf("audit read is not tenant-scoped — it can return another tenant's rows.\nwant fragment: %s\ngot statement:  %s",
							wantScopedPredicate, q)
					}
				}

				if w.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
				}

				// ...and the allowlist it binds must be the caller's admin
				// organizations, which is what the WithArgs above pin. Restate
				// the resulting visibility rule so a reader can see what the
				// predicate means without decoding SQL.
				scope := idstore.OrgScopeOrganizationsAndUnowned(caller.adminOrgs...)
				if scope.IsAllOrganizations() {
					t.Error("scope must never be the platform-wide scope on an org-admin path")
				}
				for orgID, want := range caller.wantVisible {
					if got := scope.PermitsOrganization(orgID); got != want {
						t.Errorf("PermitsOrganization(%q) = %v, want %v", orgID, got, want)
					}
				}

				// Nothing may re-narrow after the database: rows the predicate
				// admitted must reach the response. This is what fails if a
				// redundant in-memory filter is reintroduced and gets it wrong.
				body := w.Body.String()
				for orgID, visible := range caller.wantVisible {
					if !visible {
						continue
					}
					if !strings.Contains(body, "row-"+orgID) {
						t.Errorf("row for org %q was admitted by the scope but is missing from the response: %s", orgID, body)
					}
				}

				if err := e.mock.ExpectationsWereMet(); err != nil {
					t.Errorf("queued round-trips did not all run: %v", err)
				}
			})
		}
	}
}

// TestCallerOrgScope_BuildsFromAdminMembershipsOnly pins how the scope is
// derived: admin memberships only, never plain membership, and never the
// platform-wide scope. (Named callerAuditScope until identity v0.25.0 made the
// same value bound the user list and the user-lifecycle routes too.)
func TestCallerOrgScope_BuildsFromAdminMembershipsOnly(t *testing.T) {
	e := newAuditScopeEnv(t)

	rows := sqlmock.NewRows(userMembershipCols).
		AddRow(auditScopeOrgA, "Org A", "rt-admin", time.Now(), "admin", "Admin", []byte(`["admin"]`)).
		AddRow("org-c", "Org C", "rt-viewer", time.Now(), "viewer", "Viewer", []byte(`["state:read"]`))
	e.mock.ExpectQuery("FROM organization_members om").WithArgs(auditScopeCaller).WillReturnRows(rows)

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/x", nil)
	c.Set("user_id", auditScopeCaller)

	scope, err := e.h.callerOrgScope(c)
	if err != nil {
		t.Fatalf("callerOrgScope: %v", err)
	}
	if scope.IsAllOrganizations() {
		t.Fatal("an org admin must not receive the platform-wide scope")
	}
	checks := map[string]bool{
		auditScopeOrgA: true,  // admin here
		"org-c":        false, // a member, but not an admin
		auditScopeOrgB: false, // another tenant entirely
		"":             true,  // org-less platform events
	}
	for orgID, want := range checks {
		if got := scope.PermitsOrganization(orgID); got != want {
			t.Errorf("PermitsOrganization(%q) = %v, want %v", orgID, got, want)
		}
	}
	if got := scope.OrganizationIDs(); len(got) != 1 || got[0] != auditScopeOrgA {
		t.Errorf("allowlist = %v, want [%s]", got, auditScopeOrgA)
	}
}

// TestCallerOrgScope_FailsClosedWithoutCaller covers the misconfigured-chain
// case: no user_id in the context (requireAuth absent or broken) must not read
// another tenant's rows. It degrades to org-less platform events, which is what
// the in-memory filter did for an empty admin set.
func TestCallerOrgScope_FailsClosedWithoutCaller(t *testing.T) {
	e := newAuditScopeEnv(t)

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/x", nil)

	scope, err := e.h.callerOrgScope(c)
	if err != nil {
		t.Fatalf("callerOrgScope: %v", err)
	}
	if scope.IsAllOrganizations() {
		t.Fatal("an unauthenticated context must never yield the platform-wide scope")
	}
	if scope.PermitsOrganization(auditScopeOrgA) || scope.PermitsOrganization(auditScopeOrgB) {
		t.Error("no caller means no organization-owned rows")
	}
	if !scope.PermitsOrganization("") {
		t.Error("org-less platform events stay visible")
	}
}

// --- the source-scanning guards ------------------------------------------------
//
// These two tests are the ones that keep the trap shut, and they share a
// failure mode worth naming: a source scan for a NAME goes quiet, silently and
// permanently, the moment the name stops existing. That is not hypothetical.
// identity v0.25.0 renamed store.AuditScope to store.OrgScope, and the
// predecessor of TestNoPlatformWideOrgScopeInAuditHandlers looked for the
// literal "AuditScopeAllOrganizations" — after the rename it kept compiling and
// kept passing while checking nothing at all. The module's own UPGRADING.md
// flagged this test by name.
//
// Three things stop that recurring:
//
//  1. scannedSymbols below REFERENCES each scanned name as a Go symbol, so a
//     future rename in the identity module is a compile error here rather than
//     a guard that quietly searches for a name nothing spells any more.
//  2. TestCallSiteScannerFindsCallSites proves the scanner still works — right
//     root, right parse, right selector match — by asserting it finds a name
//     that IS present. Without it, "found nothing" and "cannot see anything"
//     are the same green.
//  3. The platform-wide guard is an ALLOW-LIST keyed by call site, not a blanket
//     ban. OrgScope is not AuditScope: since v0.25.0 the same constructor is
//     needed on paths that genuinely have no tenant (authentication, IdP-driven
//     provisioning), so a blanket ban would have to be deleted the first time
//     one appeared — and deleting it takes the audit guard with it.

// scannedSymbols pins every string literal the scans below search for to the
// symbol it names. It exists to be compiled, not called.
var scannedSymbols = []interface{}{
	idstore.OrgScopeAllOrganizations,                  // "OrgScopeAllOrganizations"
	(*idstore.AuditRepository).GetAuditLog,            // "GetAuditLog"
	(*idstore.AuditRepository).StreamAuditLogs,        // "StreamAuditLogs"
	(*idstore.AuditRepository).ListAuditLogs,          // "ListAuditLogs"
	(*idstore.OrganizationRepository).OrgScopeForUser, // "OrgScopeForUser"
}

// platformWideScope is the constructor that reaches every organization.
const platformWideScope = "OrgScopeAllOrganizations"

// reviewedPlatformWideSites maps "<path>:<enclosing func>" to the reason that
// call site is allowed to reach every organization. A site not listed here
// fails the build.
//
// The bar for adding an entry: the path must have NO tenant to scope by — not
// "the scope was inconvenient to thread". Authentication qualifies, because the
// scope that would narrow it is the one it exists to compute. An admin READ
// never qualifies: "admin" in TSM is granted per organization and merely
// surfaces as a flat scope, so every /admin caller is somebody's tenant admin.
//
// Nothing in this map reads audit_logs, and that is the invariant
// TestNoPlatformWideOrgScopeInAuditHandlers enforces separately.
var reviewedPlatformWideSites = map[string]string{
	"internal/middleware/auth.go:AuthMiddleware":             "authority derivation: resolves the token's subject, which is the prerequisite of every tenant check",
	"internal/middleware/auth.go:OptionalAuthMiddleware":     "authority derivation, as AuthMiddleware",
	"internal/middleware/auth.go:authenticateAPIKey":         "authority derivation: confirms a bcrypt-verified key's owner still exists",
	"internal/api/auth.go:loginScope":                        "login and IdP group-mapping reconciliation; see the loginScope doc",
	"internal/api/apikeys.go:keyScope":                       "a TSM key's organization_id is the default org, not an authority binding; the owner's membership is the boundary and keysVisibleToScope applies it",
	"internal/api/scim/handlers.go:deprovisionUser":          "IdP-driven whole-principal deactivation must be complete; see the deprovisionUser doc",
	"internal/api/scim/handlers.go:directoryScope":           "SCIM syncs the whole directory for an IdP that is authoritative over it",
	"internal/api/admin_write.go:EraseUser":                  "GDPR Article 17 erasure is an obligation about the whole data subject, and the anonymize and credential-revoke steps either side of it are already whole-principal",
	"internal/api/admin_write.go:DeleteUser":                 "mirrors identity's ON DELETE CASCADE, which removed this principal's memberships in EVERY organization; reached only after the SCOPED delete above is established to have applied, so the tenancy was enforced on the delete and this follows it",
	"internal/api/audit_ingest.go:resolveSiblingIDs":         "existence probe on a service-token route with no tenant principal; decides whether a federated id resolves at all",
	"internal/approles/reconcile.go:reconcileScope":          "startup reconciliation from no principal: it makes this application's tables agree with identity's ENTIRE membership table, so there is no caller to narrow it to, and narrowing it would leave other tenants' assignments un-restated and then swept as stale",
	"internal/approles/reads.go:roleReadScope":               "authority derivation for the two accessors the shared library itself marks UNSCOPED BY DESIGN (GetUserMemberships, GetUserScopesForOrg): they compute WHERE a principal may act, so gating them on a scope derived from that answer is circular. Every other read override in that file forwards the caller's scope, and the rows this one decorates were already filtered by the identity leg under it — so the overlay cannot disclose a membership the caller could not already see",
	"internal/credlifecycle/sweeper.go:UserDeprovisioned":    "see the package doc: a TSM key is bound to its principal, not to an organization",
	"internal/credlifecycle/sweeper.go:revokeOverAskingKeys": "as UserDeprovisioned",
	"internal/platformadmin/resolver.go:lookup":              "authority derivation for a PLATFORM-wide grant, and the single identity read in that package (UserExists is this function with the person discarded): the question is whether a platform-admin carrier row still names a live principal, and there is no tenant to scope by — narrowing it to some organization would make a live administrator read as an orphan merely because their membership lay outside the scope, which is exactly the reading the never-zero floor must not take. The name and address it now also returns are shown ONLY on the platform-admin listing, which is behind the admin scope and is a platform-wide surface by definition",
}

// TestNoPlatformWideOrgScopeInAuditHandlers keeps the audit trail tenant-bound.
//
// Every audit read accessor REQUIRES a scope, so a future compile break is
// fixed either by threading the caller's tenancy through or by silencing it
// with OrgScopeAllOrganizations() — which compiles, passes every behavioural
// test above (the mock returns whatever it is told to), and restores #331.
//
// If a genuinely org-less audit path is ever added (a retention sweep, a health
// check), this test is the place to record that decision.
func TestNoPlatformWideOrgScopeInAuditHandlers(t *testing.T) {
	// Per FUNCTION, not per file: admin_write.go legitimately holds both
	// ExportUserData (an audit read axis) and EraseUser (a reviewed platform-wide
	// site), and a file-level test would have to exempt the file — taking the
	// audit guard with it, which is the failure mode this whole file exists to
	// prevent.
	auditReaders := map[string]bool{}
	for _, accessor := range []string{"ListAuditLogs", "GetAuditLog", "StreamAuditLogs"} {
		for _, site := range qualifiedCallSitesOf(t, accessor) {
			auditReaders[site] = true
		}
	}
	if len(auditReaders) == 0 {
		t.Fatal("no function reaches an audit read accessor — the scan is not seeing the tree it is meant to guard")
	}
	for _, site := range qualifiedCallSitesOf(t, platformWideScope) {
		if auditReaders[site] {
			t.Errorf("%s reads audit logs AND constructs the platform-wide scope. Scope the "+
				"read to the caller with callerOrgScope; a platform-wide audit read is #331.", site)
		}
	}
}

// TestPlatformWideOrgScopeSitesAreReviewed fails when OrgScopeAllOrganizations
// appears at a call site nobody has signed off on.
//
// This is the guard that replaces the old blanket ban. The ban was correct
// while the type was AuditScope and applied to one table; since v0.25.0 the
// same constructor is required on paths that have no tenant at all, so the
// question is no longer "does it appear" but "is THIS appearance reviewed".
func TestPlatformWideOrgScopeSitesAreReviewed(t *testing.T) {
	sites := qualifiedCallSitesOf(t, platformWideScope)
	if len(sites) == 0 {
		t.Fatalf("found no %s call site at all. Either every tenancy decision was removed, "+
			"or the identity module renamed the constructor and this guard is now scanning "+
			"for a name that does not exist — the exact failure the AuditScope rename caused.",
			platformWideScope)
	}
	seen := map[string]bool{}
	for _, site := range sites {
		seen[site] = true
		if _, ok := reviewedPlatformWideSites[site]; !ok {
			t.Errorf("%s reaches EVERY organization. Scope it to the caller, or add it to "+
				"reviewedPlatformWideSites with the reason it has no tenant to scope by.", site)
		}
	}
	for site := range reviewedPlatformWideSites {
		if !seen[site] {
			t.Errorf("reviewedPlatformWideSites lists %s, which no longer constructs %s. "+
				"Remove the entry so the list keeps meaning what it says.", site, platformWideScope)
		}
	}
}

// TestAuditReadAccessorsInUse fails when TSM starts calling an audit read
// accessor that has no row in auditReadAxes, so a new access axis cannot be
// added without deciding its tenancy. GetAuditLog and StreamAuditLogs are
// currently unused; ListAuditLogs is reached from the three axes above.
func TestAuditReadAccessorsInUse(t *testing.T) {
	for _, accessor := range []string{"GetAuditLog", "StreamAuditLogs"} {
		for _, file := range callSitesOf(t, accessor) {
			t.Errorf("%s calls AuditRepository.%s, an audit read axis with no row in "+
				"auditReadAxes. Add one asserting its tenant scope before wiring it up.",
				file, accessor)
		}
	}
}

// TestCallSiteScannerFindsCallSites is the non-vacuity check for the scanner
// itself. Every guard above reports "clean" by finding nothing, so a scanner
// that walks the wrong root, fails to parse, or stops matching selectors would
// make all of them pass while checking nothing.
//
// OrgScopeForUser is the probe because it is the resolver every /admin tenancy
// decision goes through: if it has no call site, the narrowing is gone and the
// guards have nothing left to guard anyway.
func TestCallSiteScannerFindsCallSites(t *testing.T) {
	if got := callSitesOf(t, "OrgScopeForUser"); len(got) == 0 {
		t.Fatal("the call-site scanner found no use of OrgScopeForUser. Either the tenant " +
			"resolver is gone, or the scanner is not reading the source tree — in which " +
			"case every guard in this file is silently passing.")
	}
	if got := qualifiedCallSitesOf(t, "OrgScopeForUser"); len(got) == 0 {
		t.Fatal("the qualified call-site scanner found no use of OrgScopeForUser")
	}
}

// callSitesOf returns the non-test files under backend/ that reference name as
// a selector (the "Foo" in x.Foo). It parses rather than greps so that prose in
// a doc comment — including the comments explaining these very rules — is not
// mistaken for a call site. Paths are relative to backend/ so they are stable
// enough to name in an allow-list.
func callSitesOf(t *testing.T, name string) []string {
	t.Helper()
	seen := map[string]bool{}
	var found []string
	for _, site := range qualifiedCallSitesOf(t, name) {
		file := site[:strings.LastIndex(site, ":")]
		if !seen[file] {
			seen[file] = true
			found = append(found, file)
		}
	}
	return found
}

// qualifiedCallSitesOf is callSitesOf resolved to "<path>:<enclosing func>", so
// one file can hold a reviewed platform-wide call site without exempting its
// neighbours. A reference outside any function reports "<path>:<file scope>".
func qualifiedCallSitesOf(t *testing.T, name string) []string {
	t.Helper()
	var found []string
	fset := token.NewFileSet()
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		rel := filepath.ToSlash(strings.TrimPrefix(filepath.Clean(path), "../../"))
		enclosing := "file scope"
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				enclosing = node.Name.Name
			case *ast.SelectorExpr:
				if node.Sel.Name == name {
					found = append(found, rel+":"+enclosing)
					return false
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scan backend tree: %v", err)
	}
	return found
}
