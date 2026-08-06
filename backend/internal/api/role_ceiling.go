package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
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

	// A well-formed UUID that names no role template is a BAD REQUEST, not a
	// server fault — the same answer uuid.Parse gives for a malformed one above.
	// Before identity v0.24.0 a miss arrived as (nil, nil) and this branch was
	// unreachable; the sentinel is what makes it reachable.
	tmpl, err := h.roleRepo.GetRoleTemplate(c.Request.Context(), id)
	if errors.Is(err, idstore.ErrNotFound) || (err == nil && tmpl == nil) {
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
