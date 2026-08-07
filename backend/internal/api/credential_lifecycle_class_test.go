package api

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
	idauth "github.com/sethbacon/terraform-suite-identity/identity/auth"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/api/scim"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth/ldap"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth/saml"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/credlifecycle"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/middleware"
)

// ---------------------------------------------------------------------------
// Class test: an authority reduction must invalidate every credential family
// (issue #330)
// ---------------------------------------------------------------------------
//
// DEFECT CLASS
//
//	A handler commits an event that REDUCES a principal's derived authority
//	(organization membership removed, a member's role template reassigned, an
//	organization deleted out from under its members, a user deprovisioned by the
//	IdP through SCIM or a group-mapping sync) but does not invalidate the
//	credentials that carry a *snapshot* of that authority. This app has two such
//	families — JWT sessions (scopes embedded at login) and API keys (scopes and
//	organization_id frozen on the row) — and before this test NEITHER was swept
//	at any of the enumerated sites: SCIM deprovisioning, the control an
//	enterprise relies on for offboarding, revoked no credential at all.
//
// The table below is the class, one row per enumerated reduction site. Each row
// asserts BOTH families at that site: the revoke-all watermark that retires the
// user's live sessions, and the revocation of the API-key rows the reduction
// invalidated. Adding a new authority-reducing site without a row here is the
// regression the enumeration signature exists to catch; removing the sweep from
// the shared helper must fail exactly the rows that helper serves.
//
// EXEMPTIONS — sites the enumeration reaches that are not instances of the
// class, each for a technical reason rather than a scoping one:
//
//   - The IdP login paths (AuthHandlers.CallbackHandler / SAMLACSHandler /
//     LDAPLoginHandler, and the reconcileManagedMemberships they share) sweep the
//     API-key family but deliberately do NOT move the JWT watermark. The
//     reconciliation runs microseconds before the same request mints the user's
//     new session token; the watermark is written at full precision while a JWT's
//     iat is floored to the second, and TokensRevokedSince resolves that
//     same-second ambiguity toward "revoked", so moving it would revoke the token
//     being issued and the user could never log in. The new token is derived from
//     GetUserCombinedScopes AFTER the change committed, so it already carries the
//     reduced authority. The residual — the user's OTHER live sessions from
//     earlier logins — is stated in full on credlifecycle.Sweeper.KeysOnly. Rows
//     for those sites therefore assert wantJWTSweep=false, and assert positively
//     that no watermark write is issued.
//   - setup.Handlers.ConfigureAdmin reaches the same UpdateMemberRole
//     implementation, but the direction is an authority INCREASE (it promotes the
//     first owner to admin) and it runs during first-boot setup, before any
//     credential for that principal can exist. There is nothing to invalidate.

// akListCols mirrors ListAPIKeysByUser's scanAPIKeyWithUserName projection.
var akListCols = apiKeyListCols

// expectWatermarkWrite registers the per-user JWT revoke-all watermark move.
func expectWatermarkWrite(mock sqlmock.Sqlmock, userID string) {
	mock.ExpectExec("INSERT INTO user_token_revocations").WithArgs(userID).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectRetainedScopes registers the re-derivation of the authority the
// principal still holds after the change. Only keys asking for MORE than this
// are revoked, so a row whose reduction must delete a key has to name scopes the
// key over-asks against.
func expectRetainedScopes(mock sqlmock.Sqlmock, userID string, scopes string) {
	mock.ExpectQuery("FROM organization_members om").WithArgs(userID).
		WillReturnRows(sqlmock.NewRows(membershipCols).
			AddRow("o1", "default", nil, time.Now(), "viewer", "Viewer", []byte(scopes)))
}

// expectKeyRevoked registers the key list plus the revocation of the one key it
// returns, whose frozen scopes no reduced authority in this file still grants.
func expectKeyRevoked(mock sqlmock.Sqlmock, userID, keyID string) {
	expectKeyList(mock, userID, keyID, `["admin"]`)
	mock.ExpectExec("DELETE FROM api_keys").WithArgs(keyID).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectAllKeysRevoked is the whole-principal offboarding shape: since identity
// v0.25.0 UserDeprovisioned issues ONE bulk DELETE keyed on the owner instead of
// listing the rows and deleting each. The list-then-delete pair above is still
// what the SELECTIVE sweep (revokeOverAskingKeys) does, because that one has to
// compare each key's frozen scopes against the authority the user retains.
func expectAllKeysRevoked(mock sqlmock.Sqlmock, userID string) {
	mock.ExpectExec("DELETE FROM api_keys").WithArgs(userID).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectKeyList registers ONLY the list, with no revocation following: the
// caller is asserting the key is RETAINED because everything it carries is
// still granted.
func expectKeyList(mock sqlmock.Sqlmock, userID, keyID, scopes string) {
	mock.ExpectQuery("FROM api_keys").WithArgs(userID).
		WillReturnRows(sqlmock.NewRows(akListCols).
			AddRow(keyID, userID, "o1", "CI", nil, "hash", "tsm_abc123", []byte(scopes),
				nil, nil, nil, time.Now(), "Alice"))
}

// newClassAdminHandlers builds AdminHandlers exactly as the router does.
func newClassAdminHandlers(db *sql.DB) *AdminHandlers {
	return NewAdminHandlers(db, WithAdminCredentialSweeper(classSweeper(db)))
}

func classSweeper(db *sql.DB) *credlifecycle.Sweeper {
	return credlifecycle.NewSweeper(
		repositories.NewUserTokenRevocationRepository(db),
		idstore.NewAPIKeyRepository(db),
		idstore.NewOrganizationRepository(db))
}

// newClassAuthHandlers builds AuthHandlers with the sweeper wired, for the IdP
// group-mapping rows.
func newClassAuthHandlers(t *testing.T, db *sql.DB, mutate func(*config.Config)) *AuthHandlers {
	t.Helper()
	cfg := &config.Config{}
	if mutate != nil {
		mutate(cfg)
	}
	h, err := NewAuthHandlers(cfg, db, WithAuthCredentialSweeper(classSweeper(db)))
	if err != nil {
		t.Fatalf("NewAuthHandlers: %v", err)
	}
	return h
}

// newClassSCIMRouter builds the real SCIM handler set with the sweeper wired.
func newClassSCIMRouter(db *sql.DB) *gin.Engine {
	h := scim.NewHandlers(&config.Config{}, db, scim.WithCredentialSweeper(classSweeper(db)))
	r := gin.New()
	r.PUT("/scim/v2/Users/:id", h.PutUser())
	r.PATCH("/scim/v2/Users/:id", h.PatchUser())
	r.DELETE("/scim/v2/Users/:id", h.DeleteUser())
	return r
}

// expectSCIMDeprovision registers the shared tail of every SCIM deactivation:
// strip memberships, move the watermark, revoke every key. Deprovisioning
// retains nothing, so there is no scope re-derivation and no filtering.
func expectSCIMDeprovision(mock sqlmock.Sqlmock, userID string) {
	// The strip RETURNS the organizations it emptied (an OrgScope since
	// v0.25.0), so it is a query with a RETURNING clause rather than an exec.
	mock.ExpectQuery("DELETE FROM organization_members").WithArgs(userID).
		WillReturnRows(sqlmock.NewRows([]string{"organization_id"}).AddRow("o1"))
	expectWatermarkWrite(mock, userID)
	expectAllKeysRevoked(mock, userID)
}

// assertPreExistingSessionRejected is the CONSEQUENCE half of the JWT axis: the
// watermark a row just wrote must actually stop a session minted before it.
// Driven through the real middleware, over its own connection, because the
// watermark lives on the app database while the reduction runs against identity.
func assertPreExistingSessionRejected(t *testing.T, site, userID string) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	// The session's JTI was never denylisted — a membership removal knows no
	// JTIs, which is exactly why the watermark exists.
	mock.ExpectQuery("FROM revoked_tokens").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery("FROM user_token_revocations").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	r := gin.New()
	r.Use(middleware.AuthMiddleware(
		idstore.NewUserRepository(db), idstore.NewTokenRepository(db), nil, nil,
		repositories.NewUserTokenRevocationRepository(db)))
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	token, err := auth.GenerateJWT(userID, "a@b.c", []string{string(auth.ScopeAdmin)}, time.Hour)
	if err != nil {
		t.Fatalf("GenerateJWT: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("%s: a session minted before the reduction still authenticates (status %d); the watermark is inert",
			site, w.Code)
	}
}

// assertRevokedKeyRejected is the CONSEQUENCE half of the API-key axis: once the
// row is gone the presented secret resolves to nothing and authentication fails,
// so revoking the row really does retire the credential.
func assertRevokedKeyRejected(t *testing.T, site string) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	fullKey, _, prefix, err := idauth.GenerateAPIKey(middleware.APIKeyPrefix)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	// The sweep deleted the row: the indexed prefix lookup finds nothing.
	mock.ExpectQuery("FROM api_keys").WithArgs(prefix).
		WillReturnRows(sqlmock.NewRows(akListCols))

	r := gin.New()
	r.Use(middleware.AuthMiddleware(
		idstore.NewUserRepository(db), nil, idstore.NewAPIKeyRepository(db), nil, nil))
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+fullKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("%s: a revoked API key still authenticates (status %d)", site, w.Code)
	}
}

type credLifecycleCase struct {
	// site is the axis-qualified identity the enumeration signature reports.
	site string
	// userID is the principal whose authority the row reduces.
	userID string
	// wantJWTSweep is false only for the IdP login paths, whose JWT axis is
	// exempted for the same-second reason documented above; those rows register
	// no watermark write, so sqlmock fails them if one is issued.
	wantJWTSweep bool
	// run wires the real handler over db, registers the SQL the sweep must
	// issue, and drives the lifecycle event.
	run func(t *testing.T, db *sql.DB, mock sqlmock.Sqlmock)
}

func TestCredentialLifecycleClass_AuthorityReductionInvalidatesEveryCredentialFamily(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []credLifecycleCase{
		{
			site:         "api.AdminHandlers.RemoveOrganizationMember / DELETE /admin/organizations/:id/members/:user_id",
			userID:       "u1",
			wantJWTSweep: true,
			run: func(t *testing.T, db *sql.DB, mock sqlmock.Sqlmock) {
				h := newClassAdminHandlers(db)
				r := gin.New()
				r.DELETE("/organizations/:id/members/:user_id", h.RemoveOrganizationMember())

				mock.ExpectExec("DELETE FROM organization_members").WithArgs("o1", "u1", pq.Array([]string{"o1"})).
					WillReturnResult(sqlmock.NewResult(0, 1))
				expectWatermarkWrite(mock, "u1")
				expectRetainedScopes(mock, "u1", `["state:read"]`)
				expectKeyRevoked(mock, "u1", "k-removed-member")

				w := httptest.NewRecorder()
				r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/organizations/o1/members/u1", nil))
				if w.Code != http.StatusNoContent {
					t.Fatalf("status = %d, want 204: %s", w.Code, w.Body.String())
				}
			},
		},
		{
			site:         "api.AdminHandlers.UpdateOrganizationMember / PUT /admin/organizations/:id/members/:user_id",
			userID:       "u1",
			wantJWTSweep: true,
			run: func(t *testing.T, db *sql.DB, mock sqlmock.Sqlmock) {
				h := newClassAdminHandlers(db)
				r := gin.New()
				r.Use(func(c *gin.Context) { c.Set("scopes", []string{string(auth.ScopeAdmin)}) })
				r.PUT("/organizations/:id/members/:user_id", h.UpdateOrganizationMember())

				const roleID = "6e9a2b62-0e58-4b34-8f4b-2a6f9d3c1ab0"
				mock.ExpectQuery("FROM role_templates WHERE").WithArgs(roleID).
					WillReturnRows(sqlmock.NewRows(roleTemplateCols).
						AddRow(roleID, "viewer", "Viewer", "read-only", []byte(`["state:read"]`), false, time.Now(), time.Now()))
				mock.ExpectExec("UPDATE organization_members").WillReturnResult(sqlmock.NewResult(0, 1))
				// The reassignment narrows the member to state:read; the key was
				// minted under a broader role and now over-asks.
				expectWatermarkWrite(mock, "u1")
				expectRetainedScopes(mock, "u1", `["state:read"]`)
				expectKeyRevoked(mock, "u1", "k-rerole")

				w := httptest.NewRecorder()
				r.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/organizations/o1/members/u1",
					strings.NewReader(`{"role_template_id":"`+roleID+`"}`)))
				if w.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
				}
			},
		},
		{
			// Reached through DELETE FROM organizations cascading
			// organization_members: a reduction with no membership statement of
			// its own, and the site issue #330's hand-written table missed.
			site:         "api.AdminHandlers.DeleteOrganization / DELETE /admin/organizations/:id",
			userID:       "u1",
			wantJWTSweep: true,
			run: func(t *testing.T, db *sql.DB, mock sqlmock.Sqlmock) {
				h := newClassAdminHandlers(db)
				r := gin.New()
				r.DELETE("/organizations/:id", h.DeleteOrganization())

				// Members are snapshotted BEFORE the delete; the cascade removes
				// them, so afterwards there is nobody left to sweep.
				mock.ExpectQuery("FROM organization_members").WithArgs("o1", pq.Array([]string{"o1"})).
					WillReturnRows(sqlmock.NewRows(memberRowCols).AddRow("o1", "u1", nil, time.Now()))
				mock.ExpectExec("DELETE FROM organizations").WithArgs("o1", pq.Array([]string{"o1"})).
					WillReturnResult(sqlmock.NewResult(0, 1))
				expectWatermarkWrite(mock, "u1")
				expectRetainedScopes(mock, "u1", `["state:read"]`)
				expectKeyRevoked(mock, "u1", "k-org-deleted")

				w := httptest.NewRecorder()
				r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/organizations/o1", nil))
				if w.Code != http.StatusNoContent {
					t.Fatalf("status = %d, want 204: %s", w.Code, w.Body.String())
				}
			},
		},
		{
			// The sweep runs BEFORE the row is deleted. identity v0.25.0's
			// migration 000007 changed api_keys.user_id to ON DELETE CASCADE, so
			// the rows no longer survive as userless credentials — but that is a
			// backstop that runs after the fact and cannot reach a live JWT, so
			// the order still matters and the sweep still has to succeed first.
			site:         "api.AdminHandlers.DeleteUser / DELETE /admin/users/:id",
			userID:       "u1",
			wantJWTSweep: true,
			run: func(t *testing.T, db *sql.DB, mock sqlmock.Sqlmock) {
				h := newClassAdminHandlers(db)
				r := gin.New()
				r.DELETE("/users/:id", h.DeleteUser())

				expectWatermarkWrite(mock, "u1")
				expectAllKeysRevoked(mock, "u1")
				mock.ExpectExec("DELETE FROM users").WithArgs("u1").
					WillReturnResult(sqlmock.NewResult(0, 1))

				w := httptest.NewRecorder()
				r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/users/u1", nil))
				if w.Code != http.StatusNoContent {
					t.Fatalf("status = %d, want 204: %s", w.Code, w.Body.String())
				}
			},
		},
		{
			// GDPR erasure keeps the user row as a resolvable tombstone, so
			// neither family expires on its own.
			site:         "api.AdminHandlers.EraseUser / POST /admin/users/:id/erase",
			userID:       "u1",
			wantJWTSweep: true,
			run: func(t *testing.T, db *sql.DB, mock sqlmock.Sqlmock) {
				h := newClassAdminHandlers(db)
				r := gin.New()
				r.POST("/users/:id/erase", h.EraseUser())

				mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u1").
					WillReturnRows(idUserRow("u1"))
				mock.ExpectExec("UPDATE users").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectQuery("DELETE FROM organization_members").WithArgs("u1").
					WillReturnRows(sqlmock.NewRows([]string{"organization_id"}).AddRow("o1").AddRow("o2"))
				expectWatermarkWrite(mock, "u1")
				expectAllKeysRevoked(mock, "u1")

				w := httptest.NewRecorder()
				r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/users/u1/erase", nil))
				if w.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
				}
			},
		},
		{
			site:         "scim.Handlers.DeleteUser / DELETE /scim/v2/Users/:id",
			userID:       "u1",
			wantJWTSweep: true,
			run: func(t *testing.T, db *sql.DB, mock sqlmock.Sqlmock) {
				r := newClassSCIMRouter(db)
				mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u1").
					WillReturnRows(idUserRow("u1"))
				expectSCIMDeprovision(mock, "u1")

				w := httptest.NewRecorder()
				r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/scim/v2/Users/u1", nil))
				if w.Code != http.StatusNoContent {
					t.Fatalf("status = %d, want 204: %s", w.Code, w.Body.String())
				}
			},
		},
		{
			site:         "scim.Handlers.PutUser (active=false) / PUT /scim/v2/Users/:id",
			userID:       "u1",
			wantJWTSweep: true,
			run: func(t *testing.T, db *sql.DB, mock sqlmock.Sqlmock) {
				r := newClassSCIMRouter(db)
				mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u1").
					WillReturnRows(idUserRow("u1"))
				expectSCIMDeprovision(mock, "u1")
				mock.ExpectExec("UPDATE users").WillReturnResult(sqlmock.NewResult(0, 1))

				w := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPut, "/scim/v2/Users/u1",
					strings.NewReader(`{"userName":"a@b.c","active":false}`))
				req.Header.Set("Content-Type", "application/json")
				r.ServeHTTP(w, req)
				if w.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
				}
			},
		},
		{
			site:         `scim.Handlers.applyReplaceOp (path="active", false) / PATCH /scim/v2/Users/:id`,
			userID:       "u1",
			wantJWTSweep: true,
			run: func(t *testing.T, db *sql.DB, mock sqlmock.Sqlmock) {
				r := newClassSCIMRouter(db)
				mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u1").
					WillReturnRows(idUserRow("u1"))
				expectSCIMDeprovision(mock, "u1")
				mock.ExpectExec("UPDATE users").WillReturnResult(sqlmock.NewResult(0, 1))

				w := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPatch, "/scim/v2/Users/u1",
					strings.NewReader(`{"schemas":["`+scim.SchemaPatchOp+`"],`+
						`"Operations":[{"op":"replace","path":"active","value":false}]}`))
				req.Header.Set("Content-Type", "application/json")
				r.ServeHTTP(w, req)
				if w.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
				}
			},
		},
		{
			site:         `scim.Handlers.applyReplaceOp (pathless {"active":false}) / PATCH /scim/v2/Users/:id`,
			userID:       "u1",
			wantJWTSweep: true,
			run: func(t *testing.T, db *sql.DB, mock sqlmock.Sqlmock) {
				r := newClassSCIMRouter(db)
				mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u1").
					WillReturnRows(idUserRow("u1"))
				expectSCIMDeprovision(mock, "u1")
				mock.ExpectExec("UPDATE users").WillReturnResult(sqlmock.NewResult(0, 1))

				w := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPatch, "/scim/v2/Users/u1",
					strings.NewReader(`{"schemas":["`+scim.SchemaPatchOp+`"],`+
						`"Operations":[{"op":"replace","value":{"active":false}}]}`))
				req.Header.Set("Content-Type", "application/json")
				r.ServeHTTP(w, req)
				if w.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
				}
			},
		},
		{
			// OIDC login: the group that granted the org is gone, so the
			// membership is removed. CallbackHandler reaches this through
			// applyGroupMappings; the mapping resolution driven here is the same
			// pure function it calls.
			site:         "api.AuthHandlers.reconcileManagedMemberships (removal branch) — OIDC CallbackHandler",
			userID:       "u1",
			wantJWTSweep: false,
			run: func(t *testing.T, db *sql.DB, mock sqlmock.Sqlmock) {
				h := newClassAuthHandlers(t, db, nil)
				mappings := []config.OIDCGroupMapping{{Group: "platform-team", Organization: "acme", Role: "editor"}}
				desired, managed := resolveGroupMappings([]string{"other-team"}, mappings)

				expectOrgByName(mock, "o1", "acme")
				mock.ExpectQuery("FROM organization_members").WithArgs("o1", "u1", pq.Array([]string{"o1"})).
					WillReturnRows(sqlmock.NewRows(memberRowCols).AddRow("o1", "u1", "rt-editor", time.Now()))
				mock.ExpectExec("DELETE FROM organization_members").WithArgs("o1", "u1", pq.Array([]string{"o1"})).
					WillReturnResult(sqlmock.NewResult(0, 1))
				expectRetainedScopes(mock, "u1", `["state:read"]`)
				expectKeyRevoked(mock, "u1", "k-idp-deprovisioned")

				if err := h.reconcileManagedMemberships(context.Background(), "u1", desired, managed, ""); err != nil {
					t.Fatalf("reconcile: %v", err)
				}
			},
		},
		{
			// The OTHER reducing arm of the same conditional, and the one a
			// per-function analysis scores as covered because its sibling sweeps.
			// An IdP group change can map a user to a WEAKER role; that arm commits
			// the reduction through UpdateMemberRole and must sweep in its own
			// right.
			site:         "api.AuthHandlers.reconcileManagedMemberships (role-reassignment branch, demotion)",
			userID:       "u1",
			wantJWTSweep: false,
			run: func(t *testing.T, db *sql.DB, mock sqlmock.Sqlmock) {
				h := newClassAuthHandlers(t, db, nil)
				// LDAP resolves its mappings with its own helper; the reconciler
				// (and therefore the sweep) is shared with OIDC and SAML.
				desired, managed := ldap.ResolveLDAPGroupMappings(
					[]string{"cn=platform,ou=groups"},
					[]config.LDAPGroupMapping{{GroupDN: "cn=platform,ou=groups", Organization: "acme", Role: "viewer"}})

				expectOrgByName(mock, "o1", "acme")
				mock.ExpectQuery("FROM organization_members").WithArgs("o1", "u1", pq.Array([]string{"o1"})).
					WillReturnRows(sqlmock.NewRows(memberRowCols).AddRow("o1", "u1", "rt-owner", time.Now()))
				expectRoleScopesLookup(mock, "viewer", []string{"state:read"})
				mock.ExpectQuery("SELECT id FROM role_templates WHERE name").WithArgs("viewer").
					WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("rt-viewer"))
				mock.ExpectExec("UPDATE organization_members").WillReturnResult(sqlmock.NewResult(0, 1))
				expectRetainedScopes(mock, "u1", `["state:read"]`)
				expectKeyRevoked(mock, "u1", "k-idp-demoted")

				if err := h.reconcileManagedMemberships(context.Background(), "u1", desired, managed, ""); err != nil {
					t.Fatalf("reconcile: %v", err)
				}
			},
		},
		{
			// SAML drives the same reconciler through its own resolver, so the
			// ACS handler inherits the sweep rather than restating it.
			site:         "api.AuthHandlers.reconcileManagedMemberships (removal branch) — SAMLACSHandler",
			userID:       "u1",
			wantJWTSweep: false,
			run: func(t *testing.T, db *sql.DB, mock sqlmock.Sqlmock) {
				h := newClassAuthHandlers(t, db, nil)
				desired, managed := saml.ResolveSAMLGroupMappings(
					[]string{"other-team"},
					[]config.SAMLGroupMapping{{Group: "platform-team", Organization: "acme", Role: "editor"}})

				expectOrgByName(mock, "o1", "acme")
				mock.ExpectQuery("FROM organization_members").WithArgs("o1", "u1", pq.Array([]string{"o1"})).
					WillReturnRows(sqlmock.NewRows(memberRowCols).AddRow("o1", "u1", "rt-editor", time.Now()))
				mock.ExpectExec("DELETE FROM organization_members").WithArgs("o1", "u1", pq.Array([]string{"o1"})).
					WillReturnResult(sqlmock.NewResult(0, 1))
				expectRetainedScopes(mock, "u1", `["state:read"]`)
				expectKeyRevoked(mock, "u1", "k-saml-deprovisioned")

				if err := h.reconcileManagedMemberships(context.Background(), "u1", desired, managed, ""); err != nil {
					t.Fatalf("reconcile: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.site, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer db.Close()

			tc.run(t, db, mock)

			// Every registered statement must have been issued. For the sweep
			// statements this is the assertion that the invalidation happened:
			// sqlmock reports an unfulfilled expectation when it did not. A row
			// with wantJWTSweep=false registers no watermark write, so sqlmock
			// also proves the exempt sites do NOT move it.
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("credential sweep incomplete for %s: %v", tc.site, err)
			}

			// ... and what those statements MEAN, per family.
			if tc.wantJWTSweep {
				assertPreExistingSessionRejected(t, tc.site, tc.userID)
			}
			assertRevokedKeyRejected(t, tc.site)
		})
	}
}

// Paired negative control for the reassignment row. Revoking an API key is
// irreversible — the secret is shown once — so sweeping on every reconciliation,
// i.e. on every login for every managed organization, would destroy working
// credentials fleet-wide.
//
// sqlmock cannot carry this alone: an unregistered DELETE returns an error and
// the sweep swallows its own errors by design (the authority change has already
// committed), so a wrongly revoked key would still leave ExpectationsWereMet()
// green. The decisive assertion is the key list WITHOUT a following delete.
func TestCredentialLifecycleClass_PromotionRetainsKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	h := newClassAuthHandlers(t, db, nil)

	expectOrgByName(mock, "o1", "acme")
	mock.ExpectQuery("FROM organization_members").WithArgs("o1", "u1", pq.Array([]string{"o1"})).
		WillReturnRows(sqlmock.NewRows(memberRowCols).AddRow("o1", "u1", "rt-viewer", time.Now()))
	expectRoleScopesLookup(mock, "editor", []string{"state:read", "state:write"})
	mock.ExpectQuery("SELECT id FROM role_templates WHERE name").WithArgs("editor").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("rt-editor"))
	mock.ExpectExec("UPDATE organization_members").WillReturnResult(sqlmock.NewResult(0, 1))
	// The member now holds write; the key only asks for read, so it is listed
	// and left alone. No DELETE is registered.
	expectRetainedScopes(mock, "u1", `["state:write"]`)
	expectKeyList(mock, "u1", "k-promoted", `["state:read"]`)

	if err := h.reconcileManagedMemberships(context.Background(), "u1",
		map[string]string{"acme": "editor"}, map[string]struct{}{"acme": {}}, ""); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a promotion must not revoke keys the new authority still grants: %v", err)
	}
}

// Paired negative control for the SCIM rows. SCIMUser.Active is a pointer so an
// omitted attribute is distinguishable from an explicit false; a PUT that never
// mentions "active" must not deprovision. Asserted by registering ONLY the
// lookup and the update — sqlmock fails the test if the handler issues a
// membership delete, a watermark write, or any key statement.
func TestCredentialLifecycleClass_SCIMPutWithoutActiveDoesNotDeprovision(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	r := newClassSCIMRouter(db)
	mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u1").
		WillReturnRows(idUserRow("u1"))
	mock.ExpectExec("UPDATE users").WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/scim/v2/Users/u1",
		strings.NewReader(`{"userName":"a@b.c","name":{"formatted":"Alice R"}}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a PUT omitting \"active\" destroyed the user's credentials: %v", err)
	}
}
