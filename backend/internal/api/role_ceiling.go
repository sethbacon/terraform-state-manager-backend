package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/terraform-state-manager/terraform-state-manager/internal/approles"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
)

type roleAssignmentCheck struct {
	allowed bool
	status  int
}

func roleScopesPermittedBy(callerScopes, roleScopes []string) bool {
	if len(roleScopes) == 0 {
		return true
	}
	if auth.HasScope(callerScopes, auth.ScopeAdmin) {
		return true
	}
	for _, s := range roleScopes {
		if s == string(auth.ScopeAdmin) {
			return false
		}
		if !auth.HasScope(callerScopes, auth.Scope(s)) {
			return false
		}
	}
	return true
}

func (h *AdminHandlers) checkRoleAssignment(c *gin.Context, roleTemplateID *string) roleAssignmentCheck {
	if roleTemplateID == nil || *roleTemplateID == "" {
		return roleAssignmentCheck{allowed: true}
	}

	id, err := uuid.Parse(*roleTemplateID)
	if err != nil {
		return roleAssignmentCheck{allowed: false, status: http.StatusBadRequest}
	}

	// Resolved against THIS APPLICATION's own role_templates, which is the whole
	// point of the ceiling: the scopes being checked are the scopes assigning
	// this role would grant HERE, and since the residual identity.role_templates
	// reads were retired the app table is also the only place the id could name
	// anything. A well-formed UUID that names no role template here is a BAD
	// REQUEST, not a server fault — the same answer uuid.Parse gives for a
	// malformed one above. A rig with no application connection fails CLOSED as
	// a server fault: a ceiling that cannot read the definitions must not wave
	// the assignment through.
	tmpl, err := h.orgRepo.TemplateByID(c.Request.Context(), id.String())
	if errors.Is(err, approles.ErrNoTemplate) {
		return roleAssignmentCheck{allowed: false, status: http.StatusBadRequest}
	}
	if err != nil {
		return roleAssignmentCheck{allowed: false, status: http.StatusInternalServerError}
	}

	callerScopes := scopesOf(c)
	if !roleScopesPermittedBy(callerScopes, tmpl.Scopes) {
		return roleAssignmentCheck{allowed: false, status: http.StatusForbidden}
	}
	return roleAssignmentCheck{allowed: true}
}
