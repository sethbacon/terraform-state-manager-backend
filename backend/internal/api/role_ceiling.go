package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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

	tmpl, err := h.roleRepo.GetRoleTemplate(c.Request.Context(), id)
	if err != nil {
		return roleAssignmentCheck{allowed: false, status: http.StatusInternalServerError}
	}
	if tmpl == nil {
		return roleAssignmentCheck{allowed: false, status: http.StatusBadRequest}
	}

	callerScopes := scopesOf(c)
	if !roleScopesPermittedBy(callerScopes, tmpl.Scopes) {
		return roleAssignmentCheck{allowed: false, status: http.StatusForbidden}
	}
	return roleAssignmentCheck{allowed: true}
}
