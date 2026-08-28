package api

import (
	"net/http"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	idtenantscope "github.com/sethbacon/terraform-suite-identity/identity/tenantscope"

	"github.com/terraform-state-manager/terraform-state-manager/internal/approles"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// Minting a key AS A PLATFORM ADMINISTRATOR, which is the one caller for whom
// every other test in this package is unrepresentative.
//
// # Why the existing rig does not cover this
//
// newAPIKeysEnv stores Scope{OrgIDs: []string{testActingOrg}} -- a caller who
// reaches exactly one organization, for whom ActingOrganization resolves
// implicitly and no header is ever required. Every API-key test therefore
// exercises the one path on which the header cannot matter.
//
// A platform administrator is the opposite case and the shared resolver returns
// a DIFFERENT SHAPE for them: tenantscope.Resolve answers
// Scope{PlatformAdmin: true} and returns BEFORE it reads memberships, so OrgIDs
// is empty rather than populated. Reaching every organization is not the same as
// belonging to one, so ActingOrganization refuses an unnamed write from them
// unconditionally -- not only when they reach several.
//
// That is the whole of sethbacon/terraform-state-manager-backend#437 as it was
// actually reported: rotating a key answered 400 naming a header, and the client
// had no control that could supply one. The transport was correct; nothing
// asserted what a caller of this shape gets at each stage, so the three stages
// below say it out loud.
func newPlatformAdminAPIKeysEnv(t *testing.T) *apiKeysEnv {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	scopes := []string{"admin"}
	h := NewAPIKeysHandlers(db, nil, approles.RoleSourceIdentity)
	h.audit = newAuditor(nil)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", "u1")
		c.Set("scopes", scopes)
		// EXACTLY what middleware.TenantScope publishes for a platform admin:
		// the flag set and OrgIDs EMPTY, because tenantscope.Resolve returns
		// Scope{PlatformAdmin: true} before it ever reads memberships.
		//
		// The flag is what these tests turn on, not the empty slice: the shared
		// resolver checks PlatformAdmin BEFORE it counts OrgIDs, so populating
		// them here would change nothing. Verified, not assumed -- adding
		// OrgIDs to this line leaves all three tests passing. The empty slice is
		// here because it is what production produces, and a rig that disagreed
		// with production about the shape would be a worse lie for being subtle.
		tenantscope.Store(c, tenantscope.Scope{PlatformAdmin: true})
		c.Next()
	})
	v1 := r.Group("/api/v1")
	v1.POST("/apikeys/:id/rotate", h.RotateAPIKey())
	return &apiKeysEnv{sourcesEnv: &sourcesEnv{r: r, mock: mock}, scopes: &scopes}
}

// organizationExists scripts the existence lookup actingOrganization makes for a
// platform administrator, and ONLY for one -- Scope.Permits answers true for any
// id when the flag is set, so nothing else has confirmed the organization is
// real.
func expectOrganizationExists(mock sqlmock.Sqlmock, orgID string) {
	mock.ExpectQuery("FROM organizations").WithArgs(orgID).WillReturnRows(
		sqlmock.NewRows([]string{"id", "name", "display_name", "idp_type", "idp_name", "created_at", "updated_at"}).
			AddRow(orgID, "orgx", "Org X", nil, nil, time.Now(), time.Now()))
}

// STAGE 1: the reported symptom. An administrator who names nothing is refused,
// and the refusal names the header rather than claiming they lack authority.
func TestRotateAsPlatformAdmin_WithoutAHeaderNamesTheHeader(t *testing.T) {
	e := newPlatformAdminAPIKeysEnv(t)
	e.mock.ExpectQuery("FROM api_keys").WithArgs("k1").
		WillReturnRows(apiKeyDBRow("k1", "u1", `["state:read"]`))

	w := e.do(http.MethodPost, "/api/v1/apikeys/k1/rotate", "")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. A platform administrator who named no organization has "+
			"not failed a permission check, they have sent an unfinished request. 403 would tell "+
			"them they lack authority they actually hold. Body: %s", w.Code, w.Body.String())
	}
	if !contains(w.Body.String(), idtenantscope.ActingOrganizationHeader) {
		t.Errorf("the refusal does not name %s, so a client is told to do something without being "+
			"told what: %s", idtenantscope.ActingOrganizationHeader, w.Body.String())
	}
}

// STAGE 2: the header supplied. This is the assertion that says the fix WORKS
// end to end rather than merely stops erroring -- a new key is issued and it is
// stamped with the organization that was named, not with a default.
func TestRotateAsPlatformAdmin_WithAHeaderMintsIntoThatOrganization(t *testing.T) {
	e := newPlatformAdminAPIKeysEnv(t)
	e.mock.ExpectQuery("FROM api_keys").WithArgs("k1").
		WillReturnRows(apiKeyDBRow("k1", "u1", `["state:read"]`))
	expectOrganizationExists(e.mock, "org-x")
	expectOwnerIsMember(e.mock, "org-x", "u1")
	e.mock.ExpectExec("INSERT INTO api_keys").WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectExec("DELETE FROM api_keys").WillReturnResult(sqlmock.NewResult(0, 1))

	w := e.doWithHeader(http.MethodPost, "/api/v1/apikeys/k1/rotate", "",
		idtenantscope.ActingOrganizationHeader, "org-x")

	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 201/200. Naming an organization is the whole remedy offered "+
			"by the 400 above; if it does not then work, the refusal is advice a caller cannot "+
			"act on. Body: %s", w.Code, w.Body.String())
	}
	if !contains(w.Body.String(), `"organization_id":"org-x"`) {
		t.Errorf("the replacement key was not stamped with the organization that was named. "+
			"A key stamped elsewhere is refused by its own authentication. Body: %s", w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the scripted lookups were not all made: %v", err)
	}
}

// STAGE 3: the boundary, so nobody later reads stage 2 as "an administrator may
// mint anywhere". They may not. A key whose owner does not belong to the key's
// organization is refused at authentication, so minting one would create a
// credential that cannot be used -- the two halves have to agree.
//
// A platform administrator who belongs to NO organization therefore cannot hold
// a key of their own at all, and that is deliberate rather than a gap.
func TestRotateAsPlatformAdmin_RefusesWhenTheOwnerIsNotAMember(t *testing.T) {
	e := newPlatformAdminAPIKeysEnv(t)
	e.mock.ExpectQuery("FROM api_keys").WithArgs("k1").
		WillReturnRows(apiKeyDBRow("k1", "u1", `["state:read"]`))
	expectOrganizationExists(e.mock, "org-x")
	e.mock.ExpectQuery("FROM organization_members").WithArgs("org-x", "u1").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id", "created_at"}))

	w := e.doWithHeader(http.MethodPost, "/api/v1/apikeys/k1/rotate", "",
		idtenantscope.ActingOrganizationHeader, "org-x")

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403. Reaching every organization is not membership of one, "+
			"and a key minted where its owner does not belong is refused by its own "+
			"authentication. Body: %s", w.Code, w.Body.String())
	}
}
