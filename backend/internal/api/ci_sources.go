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
	"net"
	"net/http"
	"net/url"
	"strconv"
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

// ciSourceJSON renders a source without any secret. auth_method selects the
// credential; has_token / has_client_secret report presence only.
func ciSourceJSON(s *repositories.CISource) gin.H {
	authMethod := s.AuthMethod
	if authMethod == "" {
		authMethod = "pat"
	}
	return gin.H{
		"id":                s.ID,
		"name":              s.Name,
		"provider":          s.Provider,
		"organization":      s.Organization,
		"project":           s.Project,
		"auth_method":       authMethod,
		"has_token":         len(s.EncryptedToken) > 0,
		"tenant_id":         s.TenantID,
		"client_id":         s.ClientID,
		"has_client_secret": len(s.EncryptedClientSecret) > 0,
		"created_at":        s.CreatedAt,
		"updated_at":        s.UpdatedAt,
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
	AuthMethod   string `json:"auth_method"` // "pat" (default) | "app"
	Token        string `json:"token"`       // pat
	TenantID     string `json:"tenant_id"`   // app (Entra)
	ClientID     string `json:"client_id"`   // app (Entra)
	ClientSecret string `json:"client_secret"`
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
		req.AuthMethod = strings.TrimSpace(req.AuthMethod)
		if req.AuthMethod == "" {
			req.AuthMethod = "pat"
		}
		if req.Name == "" || req.Organization == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name and organization are required"})
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

		src := &repositories.CISource{
			Name:         req.Name,
			Provider:     req.Provider,
			Organization: req.Organization,
			AuthMethod:   req.AuthMethod,
		}
		if req.Project != "" {
			src.Project = &req.Project
		}

		switch req.AuthMethod {
		case "pat":
			if req.Token == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "token is required for auth_method 'pat'"})
				return
			}
			enc, err := crypto.Encrypt([]byte(req.Token))
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt token"})
				return
			}
			src.EncryptedToken = enc
		case "app":
			// App-registration auth is Azure DevOps-only in this first cut
			// (GitHub App is a separate plan).
			if req.Provider != "azure_devops" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "app auth is currently supported only for azure_devops sources"})
				return
			}
			req.TenantID = strings.TrimSpace(req.TenantID)
			req.ClientID = strings.TrimSpace(req.ClientID)
			if req.TenantID == "" || req.ClientID == "" || req.ClientSecret == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "app auth requires tenant_id, client_id, and client_secret"})
				return
			}
			enc, err := crypto.Encrypt([]byte(req.ClientSecret))
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt client secret"})
				return
			}
			src.EncryptedClientSecret = enc
			src.TenantID = &req.TenantID
			src.ClientID = &req.ClientID
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "auth_method must be pat or app"})
			return
		}

		saved, err := h.repo.Create(c.Request.Context(), src)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create CI source"})
			return
		}
		writeAuditEntry(c, h.audit, "ci_source.create", "ci_source", saved.ID,
			map[string]interface{}{"name": saved.Name, "provider": saved.Provider, "auth_method": saved.AuthMethod})
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

// VerifyCISource resolves the source credential (decrypting a PAT or minting an
// Entra app token) and makes a cheap provider API call to confirm it works.
// @Summary      Verify CI source credential
// @Tags         Pipelines
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      502  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /ci-sources/{id}/verify [post]
func (h *CISourceHandlers) VerifyCISource() gin.HandlerFunc {
	return func(c *gin.Context) {
		src, token, ok := h.loadWithToken(c)
		if !ok {
			return
		}
		var err error
		switch src.Provider {
		case "azure_devops":
			err = pipelines.VerifyAzureDevOps(c.Request.Context(), adoCred(token, src.AuthMethod == "app"), src.Organization)
		case "github_actions":
			err = pipelines.VerifyGitHub(c.Request.Context(), token)
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported provider"})
			return
		}
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"ok": false, "error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// sourceToken returns a usable Azure DevOps / GitHub credential value for a CI
// source: the decrypted personal access token for auth_method "pat", or a freshly
// minted (cached) Microsoft Entra app access token for "app".
func sourceToken(ctx context.Context, src *repositories.CISource) (string, error) {
	if src.AuthMethod == "app" {
		secret, err := crypto.Decrypt(src.EncryptedClientSecret)
		if err != nil {
			return "", fmt.Errorf("decrypt client secret: %w", err)
		}
		return pipelines.MintEntraADOToken(ctx, pipelines.EntraCreds{
			TenantID:     ptrStr(src.TenantID),
			ClientID:     ptrStr(src.ClientID),
			ClientSecret: string(secret),
		})
	}
	pt, err := crypto.Decrypt(src.EncryptedToken)
	if err != nil {
		return "", fmt.Errorf("decrypt CI source token: %w", err)
	}
	return string(pt), nil
}

// adoCred wraps a resolved token in the right Azure DevOps auth scheme: Bearer
// for Entra app access tokens, Basic for PATs.
func adoCred(token string, bearer bool) pipelines.ADOToken {
	if bearer {
		return pipelines.ADOBearer(token)
	}
	return pipelines.ADOPAT(token)
}

func ptrStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// loadWithToken fetches a source and a usable credential for discovery.
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
	token, err := sourceToken(c.Request.Context(), src)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve CI source credential"})
		return nil, "", false
	}
	return src, token, true
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
		refs, err := pipelines.ListAzurePipelines(c.Request.Context(), adoCred(token, src.AuthMethod == "app"), src.Organization, *src.Project)
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
		switch src.Provider {
		case "github_actions":
			repos, err := pipelines.ListGitHubRepos(c.Request.Context(), token, src.Organization)
			if err != nil {
				c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"repos": repos})
		case "azure_devops":
			if src.Project == nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "azure_devops source has no project"})
				return
			}
			repos, err := pipelines.ListAzureRepos(c.Request.Context(), adoCred(token, src.AuthMethod == "app"), src.Organization, *src.Project)
			if err != nil {
				c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"repos": repos})
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "repo discovery is not available for this provider"})
		}
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

// ListSourceServiceConnections lists an ADO project's service connections so
// the repo-setup wizard can name one in the generated pipeline's credential
// guidance. Requires the PAT to carry Service Connections (read).
// @Summary      List CI source service connections (Azure DevOps)
// @Tags         Pipelines
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /ci-sources/{id}/service-connections [get]
func (h *CISourceHandlers) ListSourceServiceConnections() gin.HandlerFunc {
	return func(c *gin.Context) {
		src, token, ok := h.loadWithToken(c)
		if !ok {
			return
		}
		if src.Provider != "azure_devops" || src.Project == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "service connections are only available for azure_devops sources"})
			return
		}
		scs, err := pipelines.ListAzureServiceConnections(c.Request.Context(), adoCred(token, src.AuthMethod == "app"), src.Organization, *src.Project)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"service_connections": scs})
	}
}

type createSourcePipelineRequest struct {
	Name     string `json:"name"`
	YAMLPath string `json:"yaml_path"`
}

// CreateSourcePipeline creates an ADO YAML pipeline definition pointing at the
// committed TSM workflow file (the wizard's "create the pipeline for me" step).
// :repo is the ADO repository id from the repos listing.
// @Summary      Create CI pipeline definition (Azure DevOps)
// @Tags         Pipelines
// @Accept       json
// @Produce      json
// @Success      201  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /ci-sources/{id}/repos/{repo}/pipelines [post]
func (h *CISourceHandlers) CreateSourcePipeline() gin.HandlerFunc {
	return func(c *gin.Context) {
		src, token, ok := h.loadWithToken(c)
		if !ok {
			return
		}
		if src.Provider != "azure_devops" || src.Project == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "pipeline creation is only available for azure_devops sources"})
			return
		}
		var req createSourcePipelineRequest
		if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Name) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
			return
		}
		yamlPath := strings.TrimSpace(req.YAMLPath)
		if yamlPath == "" {
			yamlPath = "/azure-pipelines-tsm-drift.yml"
		}
		if !strings.HasPrefix(yamlPath, "/") {
			yamlPath = "/" + yamlPath
		}
		ref, err := pipelines.CreateAzurePipeline(c.Request.Context(), adoCred(token, src.AuthMethod == "app"),
			src.Organization, *src.Project, strings.TrimSpace(req.Name), yamlPath, c.Param("repo"))
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		writeAuditEntry(c, h.audit, "ci_source.pipeline.create", "ci_source", src.ID,
			map[string]interface{}{"pipeline": ref.Name, "pipeline_id": ref.ID, "repo": c.Param("repo")})
		c.JSON(http.StatusCreated, gin.H{"pipeline": ref})
	}
}

// resolvePipelineToken returns the dispatch credential for a connection together
// with whether it is a Bearer token (an Entra app access token) rather than a PAT
// (Basic): its own token when it has one (always a PAT), else the resolved token
// of the CI source referenced by config.ci_source_id (Bearer when that source is
// app-auth). token is "" when neither exists (the dispatcher rejects empty
// credentials with its own message).
func resolvePipelineToken(ctx context.Context, ciRepo *repositories.CISourceRepository, conn *repositories.PipelineConnection) (token string, bearer bool, err error) {
	if len(conn.EncryptedToken) > 0 {
		pt, decErr := crypto.Decrypt(conn.EncryptedToken)
		if decErr != nil {
			return "", false, fmt.Errorf("decrypt pipeline token: %w", decErr)
		}
		return string(pt), false, nil
	}
	id, _ := conn.Config["ci_source_id"].(string)
	if id == "" {
		return "", false, nil
	}
	src, loadErr := ciRepo.GetByID(ctx, id)
	if loadErr != nil {
		return "", false, fmt.Errorf("load CI source: %w", loadErr)
	}
	if src == nil {
		return "", false, fmt.Errorf("CI source referenced by this connection no longer exists")
	}
	tok, tokErr := sourceToken(ctx, src)
	if tokErr != nil {
		return "", false, tokErr
	}
	return tok, src.AuthMethod == "app", nil
}

// callbackLooksUnreachable reports whether a callback base URL is unlikely to be
// reachable from hosted CI agents (localhost/private/link-local/container-only
// addresses, or no URL at all). Heuristic — a private address can be fine with
// self-hosted agents, so callers should warn, not block.
func callbackLooksUnreachable(base string) bool {
	if strings.TrimSpace(base) == "" {
		return true
	}
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" {
		return true
	}
	host := strings.ToLower(u.Hostname())
	switch host {
	case "localhost", "host.docker.internal", "backend", "frontend":
		return true
	}
	if strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
	}
	return false
}

// CallbackPreflight reports the callback base URL CI jobs will POST results to,
// flagging addresses hosted agents cannot reach (the repo-setup wizard surfaces
// this before any pipeline is created).
// @Summary      Callback reachability preflight
// @Tags         Pipelines
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /pipelines/callback-preflight [get]
func (h *DriftHandlers) CallbackPreflight() gin.HandlerFunc {
	return func(c *gin.Context) {
		base := h.cfg.Server.CallbackBase()
		c.JSON(http.StatusOK, gin.H{
			"callback_base":      base,
			"likely_unreachable": callbackLooksUnreachable(base),
		})
	}
}

type workflowSetupRequest struct {
	// Content alone is the legacy single-drift-file form.
	Content string `json:"content"`
	// Files lands multiple workflows in one branch + PR. Kind selects the
	// canonical, server-fixed path: "drift" or "versionlab".
	Files []struct {
		Kind    string `json:"kind"`
		Content string `json:"content"`
	} `json:"files"`
}

// SetupSourceWorkflow commits the TSM workflow file to a new branch of the repo
// and opens a pull request through the provider API (phase 2 of the repo-setup
// wizard). Returns {"status":"exists"} without writing when the file is already
// on the default branch. The file path and branch name are fixed server-side;
// the request supplies only the (size-capped) file content. Requires a PAT with
// repo write scopes (ADO Code R&W / GitHub contents+PRs).
// @Summary      Commit workflow + open PR (repo-setup wizard)
// @Tags         Pipelines
// @Accept       json
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /ci-sources/{id}/repos/{repo}/workflow-setup [post]
func (h *CISourceHandlers) SetupSourceWorkflow() gin.HandlerFunc {
	return func(c *gin.Context) {
		src, token, ok := h.loadWithToken(c)
		if !ok {
			return
		}
		var req workflowSetupRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
		// Normalize: legacy single-content form means one drift workflow.
		type kindContent struct{ kind, content string }
		var wanted []kindContent
		if strings.TrimSpace(req.Content) != "" {
			wanted = append(wanted, kindContent{"drift", req.Content})
		}
		for _, f := range req.Files {
			wanted = append(wanted, kindContent{f.Kind, f.Content})
		}
		if len(wanted) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "content is required"})
			return
		}
		isADO := src.Provider == "azure_devops"
		files := make([]pipelines.FileSpec, 0, len(wanted))
		for _, w := range wanted {
			paths, ok := pipelines.WorkflowPaths[w.kind]
			if !ok {
				c.JSON(http.StatusBadRequest, gin.H{"error": "kind must be drift or versionlab"})
				return
			}
			if strings.TrimSpace(w.content) == "" || len(w.content) > 64*1024 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "each file needs content under 64KiB"})
				return
			}
			path := paths.GitHub
			if isADO {
				path = paths.Azure
			}
			files = append(files, pipelines.FileSpec{Path: path, Content: w.content})
		}
		repo := c.Param("repo")
		var (
			result *pipelines.SetupResult
			err    error
		)
		switch src.Provider {
		case "azure_devops":
			if src.Project == nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "azure_devops source has no project"})
				return
			}
			result, err = pipelines.SetupAzureWorkflow(c.Request.Context(), adoCred(token, src.AuthMethod == "app"), src.Organization, *src.Project, repo, files)
		case "github_actions":
			result, err = pipelines.SetupGitHubWorkflow(c.Request.Context(), token, src.Organization, repo, files)
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "workflow setup is not available for this provider"})
			return
		}
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		writeAuditEntry(c, h.audit, "ci_source.workflow.setup", "ci_source", src.ID,
			map[string]interface{}{"repo": repo, "status": result.Status, "pr_url": result.PRURL})
		c.JSON(http.StatusOK, result)
	}
}

// GetSourcePRState reports the normalized state (open | merged | closed) of a
// pull request opened by SetupSourceWorkflow, for the wizard's poller.
// @Summary      Pull request state (repo-setup wizard)
// @Tags         Pipelines
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /ci-sources/{id}/repos/{repo}/prs/{pr} [get]
func (h *CISourceHandlers) GetSourcePRState() gin.HandlerFunc {
	return func(c *gin.Context) {
		src, token, ok := h.loadWithToken(c)
		if !ok {
			return
		}
		prID, err := strconv.Atoi(c.Param("pr"))
		if err != nil || prID <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pull request id"})
			return
		}
		repo := c.Param("repo")
		var state string
		switch src.Provider {
		case "azure_devops":
			if src.Project == nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "azure_devops source has no project"})
				return
			}
			state, err = pipelines.AzurePRState(c.Request.Context(), adoCred(token, src.AuthMethod == "app"), src.Organization, *src.Project, repo, prID)
		case "github_actions":
			state, err = pipelines.GitHubPRState(c.Request.Context(), token, src.Organization, repo, prID)
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "pull request state is not available for this provider"})
			return
		}
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"state": state})
	}
}
