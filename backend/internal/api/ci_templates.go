// ci_templates.go exposes the admin CRUD over operator-managed CI workflow
// templates (workflow_templates). Operators use these endpoints to edit/add/
// replace the drift / version-lab YAML per (provider, kind, profile) so it fits
// their repository structure. The public GET that serves a template to the
// wizard lives on /drift/workflow and /health-lab/workflow (serveWorkflowTemplate).
package api

import (
	"database/sql"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// maxTemplateBytes caps operator-supplied template content, matching the
// SetupSourceWorkflow commit cap (ci_sources.go).
const maxTemplateBytes = 64 * 1024

var (
	validTemplateProviders = map[string]bool{"github_actions": true, "azure_devops": true}
	validTemplateKinds     = map[string]bool{"drift": true, "versionlab": true}
	templateProfileRe      = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// CITemplateHandlers is the admin DAO+audit wrapper for workflow_templates.
type CITemplateHandlers struct {
	repo  *repositories.WorkflowTemplateRepository
	audit auditor
}

func NewCITemplateHandlers(database, identityDB *sql.DB) *CITemplateHandlers {
	return &CITemplateHandlers{
		repo:  repositories.NewWorkflowTemplateRepository(database),
		audit: newAuditor(identityDB),
	}
}

type ciTemplateRequest struct {
	Provider    string `json:"provider"`
	Kind        string `json:"kind"`
	Profile     string `json:"profile"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content"`
}

// validateContent applies the body checks shared by create/update.
func validateContent(name, content string) (string, bool) {
	if strings.TrimSpace(name) == "" {
		return "name is required", false
	}
	if strings.TrimSpace(content) == "" {
		return "content is required", false
	}
	if len(content) > maxTemplateBytes {
		return "content must be under 64KiB", false
	}
	return "", true
}

// ListCITemplates returns all stored templates.
// @Summary  List CI workflow templates
// @Tags     CI
// @Produce  json
// @Success  200 {object} map[string]interface{}
// @Security BearerAuth
// @Security CookieAuth
// @Router   /admin/ci/templates [get]
func (h *CITemplateHandlers) ListCITemplates() gin.HandlerFunc {
	return func(c *gin.Context) {
		templates, err := h.repo.List(c.Request.Context())
		if err != nil {
			serverError(c, err, "failed to list templates")
			return
		}
		c.JSON(http.StatusOK, gin.H{"templates": templates})
	}
}

// GetCITemplate returns a single stored template by id.
// @Summary  Get CI workflow template
// @Tags     CI
// @Produce  json
// @Success  200 {object} repositories.WorkflowTemplate
// @Failure  404 {object} map[string]interface{}
// @Security BearerAuth
// @Security CookieAuth
// @Router   /admin/ci/templates/{id} [get]
func (h *CITemplateHandlers) GetCITemplate() gin.HandlerFunc {
	return func(c *gin.Context) {
		t, err := h.repo.GetByID(c.Request.Context(), c.Param("id"))
		if err != nil {
			serverError(c, err, "failed to load template")
			return
		}
		if t == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "template not found"})
			return
		}
		c.JSON(http.StatusOK, t)
	}
}

// CreateCITemplate adds a new (provider, kind, profile) template.
// @Summary  Create CI workflow template
// @Tags     CI
// @Accept   json
// @Produce  json
// @Success  201 {object} repositories.WorkflowTemplate
// @Failure  400 {object} map[string]interface{}
// @Failure  409 {object} map[string]interface{}
// @Security BearerAuth
// @Security CookieAuth
// @Router   /admin/ci/templates [post]
func (h *CITemplateHandlers) CreateCITemplate() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req ciTemplateRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
		provider := strings.TrimSpace(req.Provider)
		kind := strings.TrimSpace(req.Kind)
		profile := strings.TrimSpace(req.Profile)
		if !validTemplateProviders[provider] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "provider must be github_actions or azure_devops"})
			return
		}
		if !validTemplateKinds[kind] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "kind must be drift or versionlab"})
			return
		}
		if !templateProfileRe.MatchString(profile) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "profile must match [A-Za-z0-9._-]+"})
			return
		}
		if msg, ok := validateContent(req.Name, req.Content); !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": msg})
			return
		}
		// Reject duplicates up front for a clean 409 (the unique index also guards).
		if existing, err := h.repo.GetByKey(c.Request.Context(), provider, kind, profile); err == nil && existing != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "a template for this provider/kind/profile already exists"})
			return
		}
		t, err := h.repo.Create(c.Request.Context(), &repositories.WorkflowTemplate{
			Provider: provider, Kind: kind, Profile: profile,
			Name: strings.TrimSpace(req.Name), Description: strings.TrimSpace(req.Description), Content: req.Content,
		})
		if err != nil {
			serverError(c, err, "failed to create template")
			return
		}
		h.audit.write(c, "ci_template.create", "ci_template", t.ID,
			map[string]interface{}{"provider": provider, "kind": kind, "profile": profile})
		c.JSON(http.StatusCreated, t)
	}
}

// UpdateCITemplate replaces the editable fields (name, description, content) of a
// template; the (provider, kind, profile) key is immutable.
// @Summary  Update CI workflow template
// @Tags     CI
// @Accept   json
// @Produce  json
// @Success  200 {object} repositories.WorkflowTemplate
// @Failure  400 {object} map[string]interface{}
// @Failure  404 {object} map[string]interface{}
// @Security BearerAuth
// @Security CookieAuth
// @Router   /admin/ci/templates/{id} [put]
func (h *CITemplateHandlers) UpdateCITemplate() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req ciTemplateRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
		if msg, ok := validateContent(req.Name, req.Content); !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": msg})
			return
		}
		updated, err := h.repo.Update(c.Request.Context(), &repositories.WorkflowTemplate{
			ID: c.Param("id"), Name: strings.TrimSpace(req.Name),
			Description: strings.TrimSpace(req.Description), Content: req.Content,
		})
		if err != nil {
			serverError(c, err, "failed to update template")
			return
		}
		if updated == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "template not found"})
			return
		}
		h.audit.write(c, "ci_template.update", "ci_template", updated.ID, nil)
		c.JSON(http.StatusOK, updated)
	}
}

// DeleteCITemplate removes a template. Built-ins cannot be deleted (the handler
// still falls back to the embedded const, but the baseline row is preserved).
// @Summary  Delete CI workflow template
// @Tags     CI
// @Produce  json
// @Success  204
// @Failure  403 {object} map[string]interface{}
// @Failure  404 {object} map[string]interface{}
// @Security BearerAuth
// @Security CookieAuth
// @Router   /admin/ci/templates/{id} [delete]
func (h *CITemplateHandlers) DeleteCITemplate() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		t, err := h.repo.GetByID(c.Request.Context(), id)
		if err != nil {
			serverError(c, err, "failed to load template")
			return
		}
		if t == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "template not found"})
			return
		}
		if t.IsBuiltin {
			c.JSON(http.StatusForbidden, gin.H{"error": "built-in templates cannot be deleted"})
			return
		}
		if err := h.repo.Delete(c.Request.Context(), id); err != nil {
			serverError(c, err, "failed to delete template")
			return
		}
		h.audit.write(c, "ci_template.delete", "ci_template", id, nil)
		c.Status(http.StatusNoContent)
	}
}
