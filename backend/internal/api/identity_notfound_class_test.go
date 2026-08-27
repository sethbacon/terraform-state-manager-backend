// identity_notfound_class_test.go pins the HTTP-level contract this app keeps
// across terraform-suite-identity's store.ErrNotFound change (module v0.24.0).
//
// That change is breaking WITHOUT being a compile error: reads that missed used
// to return (nil, nil) and by-id UPDATE/DELETEs that matched zero rows used to
// return nil, so every `if err != nil { 500 }` call site kept building while
// silently changing what it answers. The tests here therefore assert STATUS
// CODES and login outcomes rather than repository behaviour — a regression in
// this class is invisible at every other level.
//
// Four categories, one per failure mode the sentinel introduces:
//
//  1. Existence probes where NOT-FOUND IS THE HAPPY PATH. The email-rebind
//     guard is the sharpest: an unclaimed email is the ordinary answer for
//     every first login, and treating it as an error denies the login.
//  2. By-id reads that must keep answering 404 rather than 500.
//  3. By-id DELETEs that must keep their existing 2xx on a repeat call.
//  4. Reconciliation loops that must skip an already-applied element rather
//     than abort.
package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
)

// ---------------------------------------------------------------------------
// 1. The email-rebind guard: an unclaimed email must still be able to log in.
// ---------------------------------------------------------------------------

// TestOIDCLogin_BrandNewEmail_Succeeds is the single most important test in this
// file. guardEmailRebind asks "is this email already bound to a different
// identity?" — and "no user holds it" is the answer for EVERY first login.
// Written as `if err != nil { return err }`, the guard turns the store's
// ErrNotFound into a hard failure, and the callback redirects to
// ?error=email_bound instead of minting a session: nobody with a new email
// could ever log in again, with the whole suite still compiling and every
// repository test green.
func TestOIDCLogin_BrandNewEmail_Succeeds(t *testing.T) {
	idp := newOIDCTestIdP(t)
	e, cfg := newOIDCCallbackEnv(t, idp)

	state, nonce := beginLogin(t, e)
	idp.nonceToSend = nonce

	// guardEmailRebind: nobody holds user1@example.com. Empty rows is what the
	// store turns into ErrNotFound — the case that must NOT deny the login.
	e.mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("user1@example.com").
		WillReturnRows(sqlmock.NewRows(idUserCols))
	// GetOrCreateUserFromOIDC: no user by sub, none by email, so it inserts.
	e.mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows(idUserCols))
	e.mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("user1@example.com").
		WillReturnRows(sqlmock.NewRows(idUserCols))
	e.mock.ExpectExec("INSERT INTO users").WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectQuery("FROM sso_settings").WillReturnError(sql.ErrNoRows)
	e.mock.ExpectQuery("FROM sso_settings").WillReturnError(sql.ErrNoRows)
	e.mock.ExpectQuery("FROM organization_members om").WillReturnRows(sqlmock.NewRows(membershipCols))
	e.mock.ExpectQuery("INSERT INTO audit_logs").WillReturnRows(auditInsertReturn())

	w := e.do(http.MethodGet, "/api/v1/auth/callback?state="+state+"&code=test-code", "")
	if w.Code != http.StatusFound {
		t.Fatalf("callback: status = %d, want 302 (%s)", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if loc != cfg.Server.PublicURL+"/auth/callback" {
		t.Fatalf("a login with a brand-new email must SUCCEED; redirect = %q\n"+
			"an ?error=email_bound here means guardEmailRebind is treating "+
			"store.ErrNotFound as a failure instead of as the free-email happy path", loc)
	}
	var gotSession bool
	for _, ck := range w.Result().Cookies() {
		if ck.Name == "tsm_auth_token" && ck.Value != "" {
			gotSession = true
		}
	}
	if !gotSession {
		t.Error("no session cookie: the new-email login did not complete")
	}
}

// TestOIDCLogin_EmailBoundToAnotherIdentity_StillRejected is the counterweight:
// the fix above must not be "stop checking". An email already bound to a
// DIFFERENT oidc_sub is still refused, so an IdP that lets a subject change its
// email cannot take over an existing account.
func TestOIDCLogin_EmailBoundToAnotherIdentity_StillRejected(t *testing.T) {
	idp := newOIDCTestIdP(t)
	e, cfg := newOIDCCallbackEnv(t, idp)

	state, nonce := beginLogin(t, e)
	idp.nonceToSend = nonce

	now := time.Now()
	e.mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("user1@example.com").
		WillReturnRows(sqlmock.NewRows(idUserCols).
			AddRow("victim", "user1@example.com", "Victim", "someone-else", now, now))

	w := e.do(http.MethodGet, "/api/v1/auth/callback?state="+state+"&code=test-code", "")
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, cfg.Server.PublicURL+"/auth/callback?error=email_bound") {
		t.Fatalf("redirect = %q, want ?error=email_bound — an email held by another "+
			"identity must still be refused", loc)
	}
}

// TestGuardEmailRebind_Arms pins all four arms directly, so a regression in any
// one of them is attributable without reading a redirect URL.
func TestGuardEmailRebind_Arms(t *testing.T) {
	now := time.Now()

	t.Run("unclaimed email is allowed", func(t *testing.T) {
		h, mock := newReconcileEnv(t, nil)
		mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("new@example.com").
			WillReturnRows(sqlmock.NewRows(idUserCols))
		if err := h.guardEmailRebind(context.Background(), "sub-1", "new@example.com"); err != nil {
			t.Fatalf("an unclaimed email must be allowed, got %v", err)
		}
	})

	t.Run("email held by the same identity is allowed", func(t *testing.T) {
		h, mock := newReconcileEnv(t, nil)
		mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("me@example.com").
			WillReturnRows(sqlmock.NewRows(idUserCols).
				AddRow("u1", "me@example.com", "Me", "sub-1", now, now))
		if err := h.guardEmailRebind(context.Background(), "sub-1", "me@example.com"); err != nil {
			t.Fatalf("re-login with your own email must be allowed, got %v", err)
		}
	})

	t.Run("email held by another identity is refused", func(t *testing.T) {
		h, mock := newReconcileEnv(t, nil)
		mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("victim@example.com").
			WillReturnRows(sqlmock.NewRows(idUserCols).
				AddRow("u2", "victim@example.com", "Victim", "other-sub", now, now))
		err := h.guardEmailRebind(context.Background(), "sub-1", "victim@example.com")
		if !errors.Is(err, errEmailBoundToAnotherIdentity) {
			t.Fatalf("want errEmailBoundToAnotherIdentity, got %v", err)
		}
	})

	t.Run("a real lookup failure fails closed", func(t *testing.T) {
		h, mock := newReconcileEnv(t, nil)
		boom := errors.New("connection refused")
		mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("x@example.com").
			WillReturnError(boom)
		err := h.guardEmailRebind(context.Background(), "sub-1", "x@example.com")
		if err == nil || errors.Is(err, idstore.ErrNotFound) {
			t.Fatalf("a database fault must propagate, not be absorbed as not-found: %v", err)
		}
	})
}

// TestGuardProvisionableRole_UnknownTemplate_Allows keeps the documented
// contract of the OTHER existence probe: a group mapping naming a role template
// that does not exist defers to the membership write's own error rather than
// failing the whole reconciliation here.
func TestGuardProvisionableRole_UnknownTemplate_Allows(t *testing.T) {
	h, mock := newReconcileEnv(t, nil)
	mock.ExpectQuery("SELECT id, name, display_name, description, scopes").WithArgs("ghost-role").
		WillReturnRows(sqlmock.NewRows(roleTemplateCols))
	if err := h.guardProvisionableRole(context.Background(), "ghost-role"); err != nil {
		t.Fatalf("an unresolved role name must not fail the guard, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 2. By-id reads: 404, never 500.
// ---------------------------------------------------------------------------

func TestAPIKeyByID_Missing_Returns404(t *testing.T) {
	e := newAPIKeysEnv(t)
	e.mock.ExpectQuery("FROM api_keys").WithArgs("ghost").
		WillReturnRows(sqlmock.NewRows(apiKeyRowCols))
	if w := e.do(http.MethodGet, "/api/v1/apikeys/ghost", ""); w.Code != http.StatusNotFound {
		t.Errorf("GET missing api key: status = %d, want 404", w.Code)
	}
}

func TestMeHandler_DeletedUser_Returns404(t *testing.T) {
	e := newAuthEnv(t, "u-gone", nil)
	e.mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u-gone").
		WillReturnRows(sqlmock.NewRows(idUserCols))
	if w := e.do(http.MethodGet, "/api/v1/auth/me", ""); w.Code != http.StatusNotFound {
		t.Errorf("/auth/me for a deleted user: status = %d, want 404", w.Code)
	}
}

func TestNotificationChannel_UpdateMissing_Returns404(t *testing.T) {
	e := newNotificationsEnv(t)
	e.mock.ExpectQuery("UPDATE notification_channels").
		WillReturnRows(sqlmock.NewRows(notifChannelCols))
	w := e.do(http.MethodPut, "/api/v1/notifications/channels/ghost",
		`{"name":"ops","type":"webhook"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("PUT missing channel: status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
}

// TestCheckRoleAssignment_UnknownTemplate_Returns400 covers a branch that was
// UNREACHABLE before the sentinel existed: GetRoleTemplate returned (nil, nil)
// for a well-formed UUID naming no row, the `err != nil` arm never fired, and
// `tmpl == nil` was dead code. It is reachable now, and 400 (not 500) is the
// answer that matches the malformed-UUID case right above it.
func TestCheckRoleAssignment_UnknownTemplate_Returns400(t *testing.T) {
	e := newAdminWriteEnv(t)
	e.mock.ExpectQuery("SELECT id, name, display_name, description, scopes").
		WillReturnRows(sqlmock.NewRows(roleTemplateCols))
	w := e.do(http.MethodPost, "/api/v1/admin/organizations/o1/members",
		`{"user_id":"u1","role_template_id":"11111111-1111-1111-1111-111111111111"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("unknown role_template_id: status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 3. Repeat DELETEs keep the success code they already answered.
// ---------------------------------------------------------------------------

func TestAdminDeleteUser_AlreadyGone_Returns204(t *testing.T) {
	e := newAdminWriteEnv(t)
	e.mock.ExpectExec("INSERT INTO user_token_revocations").WithArgs("u1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectExec("DELETE FROM api_keys").WithArgs("u1").WillReturnResult(sqlmock.NewResult(0, 0))
	// Zero rows: the account was already deleted by a previous call.
	e.mock.ExpectExec("DELETE FROM users").WithArgs("u1").WillReturnResult(sqlmock.NewResult(0, 0))
	if w := e.do(http.MethodDelete, "/api/v1/admin/users/u1", ""); w.Code != http.StatusNoContent {
		t.Errorf("repeat DELETE user: status = %d, want 204 (this route has never "+
			"pre-checked existence, so it must stay idempotent)", w.Code)
	}
}

func TestAdminDeleteOrganization_AlreadyGone_Returns204(t *testing.T) {
	e := newAdminWriteEnv(t)
	e.mock.ExpectQuery("FROM organization_members").WithArgs("o1", []string{"o1"}).
		WillReturnRows(sqlmock.NewRows(memberRowCols))
	e.mock.ExpectExec("DELETE FROM organizations").WithArgs("o1", []string{"o1"}).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if w := e.do(http.MethodDelete, "/api/v1/admin/organizations/o1", ""); w.Code != http.StatusNoContent {
		t.Errorf("repeat DELETE organization: status = %d, want 204", w.Code)
	}
}

func TestAdminRemoveOrganizationMember_NotAMember_Returns204(t *testing.T) {
	e := newAdminWriteEnv(t)
	e.mock.ExpectExec("DELETE FROM organization_members").WithArgs("o1", "u1", []string{"o1"}).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// AuthorityReduced still runs — it re-derives what the user retains rather
	// than assuming a row moved, so it is safe on a no-op removal.
	e.mock.ExpectExec("INSERT INTO user_token_revocations").WithArgs("u1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectQuery("FROM organization_members om").WithArgs("u1").
		WillReturnRows(sqlmock.NewRows(membershipCols))
	e.mock.ExpectQuery("FROM api_keys").WithArgs("u1").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	if w := e.do(http.MethodDelete, "/api/v1/admin/organizations/o1/members/u1", ""); w.Code != http.StatusNoContent {
		t.Errorf("repeat DELETE member: status = %d, want 204", w.Code)
	}
}

// TestAdminUpdateOrganizationMember_NotAMember_Returns200 pins the deliberate
// PRESERVATION decision: this route never pre-checked membership, so a PUT
// naming a non-member answered 200 before the identity bump. Turning it into a
// 404 would be a silent API change riding along with a dependency upgrade;
// whether it should 404 is a separate decision.
func TestAdminUpdateOrganizationMember_NotAMember_Returns200(t *testing.T) {
	e := newAdminWriteEnv(t)
	e.mock.ExpectExec("UPDATE organization_members").
		WillReturnResult(sqlmock.NewResult(0, 0))
	e.mock.ExpectExec("INSERT INTO user_token_revocations").WithArgs("u1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectQuery("FROM organization_members om").WithArgs("u1").
		WillReturnRows(sqlmock.NewRows(membershipCols))
	e.mock.ExpectQuery("FROM api_keys").WithArgs("u1").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	w := e.do(http.MethodPut, "/api/v1/admin/organizations/o1/members/u1", `{"user_id":"u1"}`)
	if w.Code != http.StatusOK {
		t.Errorf("PUT member role for a non-member: status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

func TestAPIKeyDelete_AlreadyGone_Returns204(t *testing.T) {
	e := newAPIKeysEnv(t)
	e.mock.ExpectQuery("FROM api_keys").WithArgs("k1").
		WillReturnRows(apiKeyDBRow("k1", "u1", `["state:read"]`))
	// Raced against a rotation or a credential sweep: the row is already gone.
	e.mock.ExpectExec("DELETE FROM api_keys").WithArgs("k1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if w := e.do(http.MethodDelete, "/api/v1/apikeys/k1", ""); w.Code != http.StatusNoContent {
		t.Errorf("DELETE raced api key: status = %d, want 204", w.Code)
	}
}

func TestNotificationChannel_DeleteMissing_Returns204(t *testing.T) {
	e := newNotificationsEnv(t)
	e.mock.ExpectExec("DELETE FROM notification_channels").WithArgs("ghost").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if w := e.do(http.MethodDelete, "/api/v1/notifications/channels/ghost", ""); w.Code != http.StatusNoContent {
		t.Errorf("repeat DELETE channel: status = %d, want 204", w.Code)
	}
}

// ---------------------------------------------------------------------------
// 4. Reconciliation loops complete when one element is already applied.
// ---------------------------------------------------------------------------

// TestReconcile_AlreadyRemovedMembership_CompletesLoop drives the deprovisioning
// arm over TWO IdP-managed organizations where one membership is already gone.
// Before the skip, the first zero-row RemoveMember aborted the whole
// reconciliation — leaving every organization after it un-deprovisioned, and
// failing the login, over an element that was already in the desired state.
func TestReconcile_AlreadyRemovedMembership_CompletesLoop(t *testing.T) {
	h, mock := newReconcileEnv(t, nil)
	// Map iteration order is unspecified, so the two organizations' identical
	// query shapes are matched without regard to order.
	mock.MatchExpectationsInOrder(false)

	for _, org := range []string{"alpha", "beta"} {
		expectOrgByName(mock, "o-"+org, org)
	}
	// Both look like members...
	mock.ExpectQuery("FROM organization_members").WithArgs("o-alpha", "u1", []string{"o-alpha"}).
		WillReturnRows(sqlmock.NewRows(memberRowCols).AddRow("o-alpha", "u1", nil, time.Now()))
	mock.ExpectQuery("FROM organization_members").WithArgs("o-beta", "u1", []string{"o-beta"}).
		WillReturnRows(sqlmock.NewRows(memberRowCols).AddRow("o-beta", "u1", nil, time.Now()))
	// ...but alpha's row is already gone by the time the DELETE lands.
	mock.ExpectExec("DELETE FROM organization_members").WithArgs("o-alpha", "u1", []string{"o-alpha"}).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM organization_members").WithArgs("o-beta", "u1", []string{"o-beta"}).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := h.reconcileManagedMemberships(context.Background(), "u1",
		map[string]string{}, map[string]struct{}{"alpha": {}, "beta": {}}, nil, "")
	if err != nil {
		t.Fatalf("an already-removed membership must not abort the loop: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("both organizations must be reconciled, not just the first: %v", err)
	}
}

// TestReconcile_MembershipVanishedBeforeRoleUpdate_Continues covers the sibling
// arm: CheckMembership said "member", the row disappeared before the UPDATE, and
// the login must still complete.
func TestReconcile_MembershipVanishedBeforeRoleUpdate_Continues(t *testing.T) {
	h, mock := newReconcileEnv(t, nil)

	expectOrgByName(mock, "o1", "platform")
	mock.ExpectQuery("FROM organization_members").WithArgs("o1", "u1", []string{"o1"}).
		WillReturnRows(sqlmock.NewRows(memberRowCols).AddRow("o1", "u1", nil, time.Now()))
	expectRoleScopesLookup(mock, "editor", []string{"state:read", "state:write"})
	mock.ExpectQuery("SELECT id FROM role_templates WHERE name").WithArgs("editor").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("rt-editor"))
	mock.ExpectExec("UPDATE organization_members").WillReturnResult(sqlmock.NewResult(0, 0))

	err := h.reconcileManagedMemberships(context.Background(), "u1",
		map[string]string{"platform": "editor"}, map[string]struct{}{"platform": {}}, nil, "")
	if err != nil {
		t.Fatalf("a vanished membership must be skipped, not abort the login: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 5. Read-then-mutate races. Each of these handlers reads a row, then writes it
//    by id. Before v0.24.0 a zero-row write returned nil and the handler
//    answered 2xx describing a change it had not made; the answer that matches
//    the read's own 404 is 404.
// ---------------------------------------------------------------------------

func TestAdminUpdateUser_RacedDelete_Returns404(t *testing.T) {
	e := newAdminWriteEnv(t)
	e.mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u1").WillReturnRows(idUserRow("u1"))
	e.mock.ExpectExec("UPDATE users").WillReturnResult(sqlmock.NewResult(0, 0))
	w := e.do(http.MethodPut, "/api/v1/admin/users/u1", `{"name":"Renamed"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("PUT user raced against a delete: status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
}

func TestAdminEraseUser_RacedDelete_Returns404(t *testing.T) {
	e := newAdminWriteEnv(t)
	e.mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u1").WillReturnRows(idUserRow("u1"))
	e.mock.ExpectExec("UPDATE users").WillReturnResult(sqlmock.NewResult(0, 0))
	w := e.do(http.MethodPost, "/api/v1/admin/users/u1/erase", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("GDPR erase raced against a delete: status = %d, want 404 — anonymization "+
			"that wrote no row must not report success (%s)", w.Code, w.Body.String())
	}
}

func TestAdminRenameOrganization_RacedDelete_Returns404(t *testing.T) {
	e := newAdminWriteEnv(t)
	now := time.Now()
	e.mock.ExpectQuery("FROM organizations").WithArgs("o1").
		WillReturnRows(sqlmock.NewRows(orgRowCols).AddRow("o1", "eng", "Engineering", nil, nil, now, now))
	e.mock.ExpectExec("UPDATE organizations SET name").WillReturnResult(sqlmock.NewResult(0, 0))
	w := e.do(http.MethodPut, "/api/v1/admin/organizations/o1", `{"name":"platform"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("rename raced against a delete: status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
}

func TestAdminUpdateOrganization_RacedDelete_Returns404(t *testing.T) {
	e := newAdminWriteEnv(t)
	now := time.Now()
	e.mock.ExpectQuery("FROM organizations").WithArgs("o1").
		WillReturnRows(sqlmock.NewRows(orgRowCols).AddRow("o1", "eng", "Engineering", nil, nil, now, now))
	e.mock.ExpectExec("UPDATE organizations").WillReturnResult(sqlmock.NewResult(0, 0))
	w := e.do(http.MethodPut, "/api/v1/admin/organizations/o1", `{"display_name":"Platform"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("org update raced against a delete: status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
}

func TestAPIKeyUpdate_RacedDelete_Returns404(t *testing.T) {
	e := newAPIKeysEnv(t)
	e.mock.ExpectQuery("FROM api_keys").WithArgs("k1").
		WillReturnRows(apiKeyDBRow("k1", "u1", `["state:read"]`))
	e.mock.ExpectExec("UPDATE api_keys").WillReturnResult(sqlmock.NewResult(0, 0))
	w := e.do(http.MethodPut, "/api/v1/apikeys/k1", `{"name":"renamed","scopes":["state:read"]}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("PUT api key raced against a delete: status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
}

// TestAPIKeyRotate_OldKeyAlreadyGone_Returns201: the replacement is already
// minted and its secret exists ONLY in this response. Failing the request
// because the superseded key was already destroyed would strand the caller
// holding an unsaved secret for a key they were told was not created.
func TestAPIKeyRotate_OldKeyAlreadyGone_Returns201(t *testing.T) {
	e := newAPIKeysEnv(t)
	e.mock.ExpectQuery("FROM api_keys").WithArgs("k1").
		WillReturnRows(apiKeyDBRow("k1", "u1", `["state:read"]`))
	expectOwnerIsMember(e.mock, testActingOrg, "u1")
	e.mock.ExpectExec("INSERT INTO api_keys").WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectExec("DELETE FROM api_keys").WithArgs("k1").WillReturnResult(sqlmock.NewResult(0, 0))

	w := e.do(http.MethodPost, "/api/v1/apikeys/k1/rotate", `{"grace_period_hours":0}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("rotate with an already-deleted old key: status = %d, want 201 (%s)",
			w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"key":"tsm_`) {
		t.Error("rotation must still return the new secret")
	}
}

func TestAPIKeyRotate_GracePeriod_OldKeyAlreadyGone_Returns201(t *testing.T) {
	e := newAPIKeysEnv(t)
	e.mock.ExpectQuery("FROM api_keys").WithArgs("k1").
		WillReturnRows(apiKeyDBRow("k1", "u1", `["state:read"]`))
	expectOwnerIsMember(e.mock, testActingOrg, "u1")
	e.mock.ExpectExec("INSERT INTO api_keys").WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectExec("UPDATE api_keys").WillReturnResult(sqlmock.NewResult(0, 0))

	w := e.do(http.MethodPost, "/api/v1/apikeys/k1/rotate", `{"grace_period_hours":24}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("grace rotation with an already-deleted old key: status = %d, want 201 (%s)",
			w.Code, w.Body.String())
	}
}

// TestAPIKeyCreate_NoDefaultOrganization_Returns500 pins the nil-dereference
// this bump removes: mintKey read the default org and used org.ID immediately,
// so a deployment without one panicked. It is now an explicit 500.
func TestAPIKeyCreate_NoDefaultOrganization_Returns500(t *testing.T) {
	e := newAPIKeysEnv(t)
	e.mock.ExpectQuery("FROM organizations").WithArgs("default").
		WillReturnRows(sqlmock.NewRows(orgRowCols))
	w := e.do(http.MethodPost, "/api/v1/apikeys", `{"name":"ci","scopes":["state:read"]}`)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("mint with no default organization: status = %d, want 500 (%s)", w.Code, w.Body.String())
	}
}
