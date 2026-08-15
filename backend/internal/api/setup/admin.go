package setup

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/sethbacon/terraform-suite-identity/identity/models"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/approles"
)

// ConfigureAdmin creates the first OWNER: an email-only user in the identity
// store, granted the admin role in the default organization AND recorded in this
// deployment's platform-admin carrier. No password and no pending-email column
// are needed — the IdP mints the credential on first login, where the
// suite-identity store links the email to the OIDC subject.
//
// THIS IS THE CARRIER'S BOOTSTRAP PATH, and it is the only one that can be:
// every other way of granting platform-admin requires an authenticated caller
// who already holds `admin`, which is precisely what a deployment with nobody in
// it does not have. This step runs behind the setup-token middleware, before any
// owner exists, and is permanently unreachable once setup completes — so it
// cannot become a standing privilege-escalation route.
//
// IDEMPOTENT END TO END. The user is created or reused, the membership is
// inserted or promoted, and the carrier grant is a no-op when the row is already
// there (EnsureAdmin swallows ErrAlreadyPlatformAdmin), leaving the original
// granted_by/granted_at/note provenance intact. Re-running the wizard step
// therefore converges rather than either failing or rewriting history.
//
// Refused with 409 in coupled mode (the sibling registry owns identity); the
// wizard hides this step there, and this guard stops a hand-crafted request from
// clobbering the shared identity store. A coupled deployment gets its first TSM
// platform admin from POST /api/v1/admin/platform-admins instead, called by
// somebody the sibling's identity already made an admin here — which works
// because this phase's elevation is additive and the role-template route to
// `admin` still stands.
func (h *Handlers) ConfigureAdmin(c *gin.Context) {
	if !h.cfg.Suite.ShouldSeedRoles("tsm") {
		c.JSON(http.StatusConflict, gin.H{"error": "identity is managed by the suite registry; create the owner there"})
		return
	}
	var req struct {
		Email string `json:"email" binding:"required,email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "a valid email address is required"})
		return
	}
	ctx := c.Request.Context()
	email := strings.ToLower(strings.TrimSpace(req.Email))

	orgRepo := approles.NewMembers(h.identityDB, h.appDB, approles.RoleSource(h.cfg.Authz.RoleSource))
	userRepo := idstore.NewUserRepository(h.identityDB)

	defaultOrg, err := orgRepo.GetDefaultOrganization(ctx)
	if err != nil || defaultOrg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to find the default organization"})
		return
	}
	// The first-run wizard has no principal to derive a tenancy from — it runs
	// behind the setup token, BEFORE any owner exists, and it writes into the one
	// organization bootstrap seeded. The scope names that organization rather
	// than reaching everywhere, so this grant cannot touch any organization
	// created later.
	bootstrapScope := idstore.OrgScopeOrganizations(defaultOrg.ID)
	user := &models.User{Email: email, Name: email}
	if err := userRepo.CreateUser(ctx, user); err != nil {
		// Already exists (e.g. re-run): reuse the existing record.
		existing, ferr := userRepo.GetUserByEmail(ctx, email)
		if ferr != nil || existing == nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create the owner user"})
			return
		}
		user = existing
	}
	if err := orgRepo.AddMemberWithParams(ctx, defaultOrg.ID, user.ID, "admin", bootstrapScope); err != nil {
		// Already a member (the insert is a plain INSERT, so a re-run of the
		// wizard hits the unique constraint): promote to admin.
		//
		// FAILS CLOSED on ErrNotFound. Before identity v0.24.0 an UPDATE that
		// matched no membership row returned nil, so this handler answered 200
		// and went on to record SetAdminConfigured — marking the deployment
		// "owner configured" when nobody had been granted anything and the
		// wizard could never be re-entered. The sentinel makes that case
		// distinguishable, and the only safe answer for a privilege grant that
		// wrote no row is to refuse.
		// The reducer is a no-op HERE, and that is the one place in this
		// application where it legitimately is: this is an authority INCREASE
		// (the first owner is being promoted to admin) running during first-boot
		// setup, before any credential for that principal can exist. There is
		// nothing to invalidate. Recorded the same way in
		// credential_lifecycle_class_test.go's exemption list.
		noSweep := func(context.Context, string) error { return nil }
		uerr := orgRepo.UpdateMemberRole(ctx, defaultOrg.ID, user.ID, "admin", bootstrapScope, noSweep)
		if errors.Is(uerr, idstore.ErrNotFound) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to grant the owner the admin role: no membership to promote"})
			return
		}
		if uerr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to grant the owner the admin role"})
			return
		}
	}
	// The carrier row, written BEFORE SetAdminConfigured for the same reason the
	// membership promotion fails closed above: marking the deployment
	// "owner configured" burns the only re-entry into this step, so anything that
	// did not actually happen must not be recorded as having happened.
	//
	// granted_by is NULL — at first boot there is no principal to attribute the
	// grant to — and the note says where the row came from, so the provenance is
	// not silently invented.
	if h.platformAdmins != nil {
		if _, err := h.platformAdmins.EnsureAdmin(ctx, user.ID,
			"granted by the first-run setup wizard"); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record the owner as a platform administrator"})
			return
		}
	}
	if err := h.settings.SetAdminConfigured(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record owner status"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "email": email, "organization": defaultOrg.DisplayName})
}

// CompleteSetup finalizes first-run setup: it verifies the prerequisites are met,
// then burns the setup token (SetSetupCompleted), permanently disabling the
// wizard. In coupled mode the identity prerequisites are the sibling registry's
// responsibility, so only a state source is required.
func (h *Handlers) CompleteSetup(c *gin.Context) {
	ctx := c.Request.Context()
	st, err := h.settings.GetStatus(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load setup status"})
		return
	}
	if h.cfg.Suite.ShouldSeedRoles("tsm") { // standalone: TSM owns identity
		if !st.AdminConfigured {
			c.JSON(http.StatusBadRequest, gin.H{"error": "configure an owner before completing setup"})
			return
		}
		if !st.OIDCConfigured && !st.LDAPConfigured {
			c.JSON(http.StatusBadRequest, gin.H{"error": "configure an authentication method before completing setup"})
			return
		}
	}
	if !st.SourcesConfigured {
		c.JSON(http.StatusBadRequest, gin.H{"error": "add at least one state source before completing setup"})
		return
	}
	if err := h.settings.SetSetupCompleted(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to complete setup"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
