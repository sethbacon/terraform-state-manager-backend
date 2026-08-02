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
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
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
// store.AuditScope, not a post-filter over rows the database already returned.
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
// AuditScopeOrganizations drops the "OR ... IS NULL" half, and the two are
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
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(
		sqlmock.QueryMatcherFunc(func(expectedSQL, actualSQL string) error {
			rec.record(actualSQL)
			return sqlmock.QueryMatcherRegexp.Match(expectedSQL, actualSQL)
		}),
	))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &auditScopeEnv{h: NewAdminHandlers(sqlx.NewDb(db, "sqlmock")), mock: mock, rec: rec}
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

// expectAdminMemberships queues the caller lookup adminOrgSet issues, returning
// an admin-scoped membership per organization given.
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
		time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC), "a@x.io", "Alice")
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
			e.mock.ExpectQuery(`SELECT COUNT\(\*\) FROM audit_logs`).WithArgs(pq.Array(wantOrgs)).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
			e.mock.ExpectQuery("SELECT al.id, .+ FROM audit_logs").
				WithArgs(pq.Array(wantOrgs), sqlmock.AnyArg(), sqlmock.AnyArg()).
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
			e.mock.ExpectQuery(`SELECT COUNT\(\*\) FROM audit_logs`).WithArgs(pq.Array(wantOrgs)).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
			e.mock.ExpectQuery("SELECT al.id, .+ FROM audit_logs").
				WithArgs(pq.Array(wantOrgs), sqlmock.AnyArg(), sqlmock.AnyArg()).
				WillReturnRows(page)
			e.mock.ExpectExec("INSERT INTO audit_logs").WillReturnResult(sqlmock.NewResult(0, 1))
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
			e.mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs(auditScopeTarget).
				WillReturnRows(idUserRow(auditScopeTarget))
			e.mock.ExpectQuery("FROM organization_members om").WithArgs(auditScopeTarget).
				WillReturnRows(sqlmock.NewRows(userMembershipCols))
			expectAdminMemberships(e.mock, callerID, wantOrgs...)
			e.mock.ExpectQuery(`SELECT COUNT\(\*\) FROM audit_logs`).
				WithArgs(pq.Array(wantOrgs), auditScopeTarget).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
			e.mock.ExpectQuery("SELECT al.id, .+ FROM audit_logs").
				WithArgs(pq.Array(wantOrgs), auditScopeTarget, sqlmock.AnyArg(), sqlmock.AnyArg()).
				WillReturnRows(page)
			e.mock.ExpectExec("INSERT INTO audit_logs").WillReturnResult(sqlmock.NewResult(0, 1))
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
				scope := idstore.AuditScopeOrganizationsAndUnowned(caller.adminOrgs...)
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

// TestCallerAuditScope_BuildsFromAdminMembershipsOnly pins how the scope is
// derived: admin memberships only, never plain membership, and never the
// platform-wide scope.
func TestCallerAuditScope_BuildsFromAdminMembershipsOnly(t *testing.T) {
	e := newAuditScopeEnv(t)

	rows := sqlmock.NewRows(userMembershipCols).
		AddRow(auditScopeOrgA, "Org A", "rt-admin", time.Now(), "admin", "Admin", []byte(`["admin"]`)).
		AddRow("org-c", "Org C", "rt-viewer", time.Now(), "viewer", "Viewer", []byte(`["state:read"]`))
	e.mock.ExpectQuery("FROM organization_members om").WithArgs(auditScopeCaller).WillReturnRows(rows)

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/x", nil)
	c.Set("user_id", auditScopeCaller)

	scope, err := e.h.callerAuditScope(c)
	if err != nil {
		t.Fatalf("callerAuditScope: %v", err)
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

// TestCallerAuditScope_FailsClosedWithoutCaller covers the misconfigured-chain
// case: no user_id in the context (requireAuth absent or broken) must not read
// another tenant's rows. It degrades to org-less platform events, which is what
// the in-memory filter did for an empty admin set.
func TestCallerAuditScope_FailsClosedWithoutCaller(t *testing.T) {
	e := newAuditScopeEnv(t)

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/x", nil)

	scope, err := e.h.callerAuditScope(c)
	if err != nil {
		t.Fatalf("callerAuditScope: %v", err)
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

// TestNoPlatformWideAuditScopeInHandlers keeps the trap shut. Every audit read
// accessor now REQUIRES a scope, so a future compile break is fixed either by
// threading the caller's tenancy through or by silencing it with
// AuditScopeAllOrganizations() — which compiles, passes every behavioural test
// above (the mock returns whatever it is told to), and restores #331.
//
// There is no platform-wide reader in TSM: "admin" is granted per organization
// and surfaces as a flat scope, so every /admin caller is somebody's tenant
// admin. If a genuinely org-less path is ever added (a retention sweep, a
// health check), this test is the place to record that decision.
func TestNoPlatformWideAuditScopeInHandlers(t *testing.T) {
	for _, file := range callSitesOf(t, "AuditScopeAllOrganizations") {
		t.Errorf("%s reads audit logs across every organization. If that is deliberate, "+
			"say why at the call site and amend this test; otherwise scope the read to "+
			"the caller with callerAuditScope.", file)
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

// callSitesOf returns the non-test files under backend/ that reference name as
// a selector (the "Foo" in x.Foo). It parses rather than greps so that prose in
// a doc comment — including the comments explaining these very rules — is not
// mistaken for a call site.
func callSitesOf(t *testing.T, name string) []string {
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
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
				found = append(found, path)
				return false
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
