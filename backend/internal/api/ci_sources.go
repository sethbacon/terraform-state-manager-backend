// ci_sources.go implements org-level CI provider connections (an Azure DevOps
// org/project or a GitHub owner) and the discovery endpoints that list their
// dispatchable pipelines / repos / workflows — so pipeline connections can be
// created by selection (mirrors the registry's SCM-provider model). The shared
// credential is encrypted at rest, never returned, and resolved at dispatch
// time for connections that reference a source instead of carrying a token.
package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/pipelines"
)

// CISourceHandlers serves the CI source CRUD + discovery endpoints.
type CISourceHandlers struct {
	repo  *repositories.CISourceRepository
	audit *idstore.AuditRepository
}

// NewCISourceHandlers builds the handlers. identityDB (search_path
// identity,public) carries the shared audit log.
func NewCISourceHandlers(database, identityDB *sql.DB) *CISourceHandlers {
	return &CISourceHandlers{
		repo:  repositories.NewCISourceRepository(database),
		audit: idstore.NewAuditRepository(identityDB),
	}
}

// ciSourceJSON renders a source without its credential ("has_token" only).
func ciSourceJSON(s *repositories.CISource) gin.H {
	return gin.H{
		"id":           s.ID,
		"name":         s.Name,
		"provider":     s.Provider,
		"organization": s.Organization,
		"project":      s.Project,
		"has_token":    len(s.EncryptedToken) > 0,
		"created_at":   s.CreatedAt,
		"updated_at":   s.UpdatedAt,
	}
}

// ListCISources returns the configured CI sources (no secrets).
// @Summary      List CI sources
// @Tags         Pipelines
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /ci-sources [get]
func (h *CISourceHandlers) ListCISources() gin.HandlerFunc {
	return func(c *gin.Context) {
		sources, err := h.repo.List(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list CI sources"})
			return
		}
		out := make([]gin.H, 0, len(sources))
		for i := range sources {
			out = append(out, ciSourceJSON(&sources[i]))
		}
		c.JSON(http.StatusOK, gin.H{"ci_sources": out})
	}
}

type ciSourceRequest struct {
	Name         string `json:"name"`
	Provider     string `json:"provider"`
	Organization string `json:"organization"`
	Project      string `json:"project"`
	Token        string `json:"token"`
}

// CreateCISource registers a CI source, encrypting its credential.
// @Summary      Create CI source
// @Tags         Pipelines
// @Accept       json
// @Produce      json
// @Success      201  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /ci-sources [post]
func (h *CISourceHandlers) CreateCISource() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req ciSourceRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		req.Organization = strings.TrimSpace(req.Organization)
		req.Project = strings.TrimSpace(req.Project)
		if req.Name == "" || req.Organization == "" || req.Token == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name, organization, and token are required"})
			return
		}
		switch req.Provider {
		case "github_actions":
			// project not used
		case "azure_devops":
			if req.Project == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "azure_devops sources require a project"})
				return
			}
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "provider must be github_actions or azure_devops"})
			return
		}
		enc, err := crypto.Encrypt([]byte(req.Token))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt token"})
			return
		}
		src := &repositories.CISource{
			Name:           req.Name,
			Provider:       req.Provider,
			Organization:   req.Organization,
			EncryptedToken: enc,
		}
		if req.Project != "" {
			src.Project = &req.Project
		}
		saved, err := h.repo.Create(c.Request.Context(), src)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create CI source"})
			return
		}
		writeAuditEntry(c, h.audit, "ci_source.create", "ci_source", saved.ID,
			map[string]interface{}{"name": saved.Name, "provider": saved.Provider})
		c.JSON(http.StatusCreated, ciSourceJSON(saved))
	}
}

// DeleteCISource removes a CI source. Pipeline connections that reference it
// keep working only if they carry their own token.
// @Summary      Delete CI source
// @Tags         Pipelines
// @Produce      json
// @Success      204
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /ci-sources/{id} [delete]
func (h *CISourceHandlers) DeleteCISource() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if err := h.repo.Delete(c.Request.Context(), id); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete CI source"})
			return
		}
		writeAuditEntry(c, h.audit, "ci_source.delete", "ci_source", id, nil)
		c.Status(http.StatusNoContent)
	}
}

// loadWithToken fetches a source and its decrypted credential for discovery.
func (h *CISourceHandlers) loadWithToken(c *gin.Context) (*repositories.CISource, string, bool) {
	src, err := h.repo.GetByID(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load CI source"})
		return nil, "", false
	}
	if src == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "CI source not found"})
		return nil, "", false
	}
	pt, err := crypto.Decrypt(src.EncryptedToken)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decrypt CI source token"})
		return nil, "", false
	}
	return src, string(pt), true
}

// ListSourcePipelines lists the dispatchable pipelines of an Azure DevOps source.
// @Summary      List CI source pipelines (Azure DevOps)
// @Tags         Pipelines
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /ci-sources/{id}/pipelines [get]
func (h *CISourceHandlers) ListSourcePipelines() gin.HandlerFunc {
	return func(c *gin.Context) {
		src, token, ok := h.loadWithToken(c)
		if !ok {
			return
		}
		if src.Provider != "azure_devops" || src.Project == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "pipeline discovery is only available for azure_devops sources"})
			return
		}
		refs, err := pipelines.ListAzurePipelines(c.Request.Context(), token, src.Organization, *src.Project)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"pipelines": refs})
	}
}

// ListSourceRepos lists the repositories of a GitHub source.
// @Summary      List CI source repositories (GitHub)
// @Tags         Pipelines
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /ci-sources/{id}/repos [get]
func (h *CISourceHandlers) ListSourceRepos() gin.HandlerFunc {
	return func(c *gin.Context) {
		src, token, ok := h.loadWithToken(c)
		if !ok {
			return
		}
		if src.Provider != "github_actions" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "repo discovery is only available for github_actions sources"})
			return
		}
		repos, err := pipelines.ListGitHubRepos(c.Request.Context(), token, src.Organization)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"repos": repos})
	}
}

// ListSourceWorkflows lists the active Actions workflows of one repository.
// @Summary      List CI source repo workflows (GitHub)
// @Tags         Pipelines
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /ci-sources/{id}/repos/{repo}/workflows [get]
func (h *CISourceHandlers) ListSourceWorkflows() gin.HandlerFunc {
	return func(c *gin.Context) {
		src, token, ok := h.loadWithToken(c)
		if !ok {
			return
		}
		if src.Provider != "github_actions" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "workflow discovery is only available for github_actions sources"})
			return
		}
		workflows, err := pipelines.ListGitHubWorkflows(c.Request.Context(), token, src.Organization, c.Param("repo"))
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"workflows": workflows})
	}
}

// resolvePipelineToken returns the dispatch credential for a connection: its own
// token when it has one, else the token of the CI source referenced by
// config.ci_source_id. Returns "" when neither exists (the dispatcher rejects
// empty tokens with its own message).
func resolvePipelineToken(ctx context.Context, ciRepo *repositories.CISourceRepository, conn *repositories.PipelineConnection) (string, error) {
	if len(conn.EncryptedToken) > 0 {
		pt, err := crypto.Decrypt(conn.EncryptedToken)
		if err != nil {
			return "", fmt.Errorf("decrypt pipeline token: %w", err)
		}
		return string(pt), nil
	}
	id, _ := conn.Config["ci_source_id"].(string)
	if id == "" {
		return "", nil
	}
	src, err := ciRepo.GetByID(ctx, id)
	if err != nil {
		return "", fmt.Errorf("load CI source: %w", err)
	}
	if src == nil {
		return "", fmt.Errorf("CI source referenced by this connection no longer exists")
	}
	pt, err := crypto.Decrypt(src.EncryptedToken)
	if err != nil {
		return "", fmt.Errorf("decrypt CI source token: %w", err)
	}
	return string(pt), nil
}
