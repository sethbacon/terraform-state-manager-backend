// health.go implements the Phase 4 version lab: dispatching a terraform
// init/plan against pinned Terraform/provider versions (optionally via the
// registry mirror) through CI, and ingesting the health result via callback.
package api

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/pipelines"
)

// HealthHandlers serves version-lab (health run) endpoints.
type HealthHandlers struct {
	cfg          *config.Config
	pipelineRepo *repositories.PipelineRepository
	healthRepo   *repositories.HealthRepository
}

// NewHealthHandlers constructs the handlers over the app (public) connection.
func NewHealthHandlers(cfg *config.Config, database *sql.DB) *HealthHandlers {
	return &HealthHandlers{
		cfg:          cfg,
		pipelineRepo: repositories.NewPipelineRepository(database),
		healthRepo:   repositories.NewHealthRepository(database),
	}
}

// CreateRun dispatches a version-health run on the chosen pipeline.
// @Summary      Dispatch version-health run
// @Description  Dispatches terraform init/plan against pinned Terraform/provider versions on the chosen CI pipeline. Inputs validated server-side. Requires state:execute.
// @Tags         VersionLab
// @Accept       json
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /health-lab/runs [post]
func (h *HealthHandlers) CreateRun() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			PipelineConnectionID string            `json:"pipeline_connection_id" binding:"required"`
			RepoRef              string            `json:"repo_ref"`
			WorkingDir           string            `json:"working_dir"`
			TerraformVersion     string            `json:"terraform_version"`
			ProviderVersions     map[string]string `json:"provider_versions"`
			ModuleVersions       map[string]string `json:"module_versions"`
			RegistryHost         string            `json:"registry_host"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "pipeline_connection_id is required"})
			return
		}
		if err := validatePipelineInputs(req.WorkingDir, req.RepoRef, req.TerraformVersion, req.RegistryHost, req.ProviderVersions, req.ModuleVersions); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		ctx := c.Request.Context()
		conn, err := h.pipelineRepo.GetByID(ctx, req.PipelineConnectionID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load pipeline connection"})
			return
		}
		if conn == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "pipeline connection not found"})
			return
		}
		token := ""
		if len(conn.EncryptedToken) > 0 {
			pt, derr := crypto.Decrypt(conn.EncryptedToken)
			if derr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decrypt pipeline token"})
				return
			}
			token = string(pt)
		}

		run := &repositories.HealthRun{
			PipelineConnectionID: &conn.ID,
			RepoRef:              req.RepoRef,
			WorkingDir:           req.WorkingDir,
			TerraformVersion:     req.TerraformVersion,
			ProviderVersions:     req.ProviderVersions,
			ModuleVersions:       req.ModuleVersions,
			RegistryHost:         req.RegistryHost,
			Status:               "dispatched",
			CallbackToken:        randomToken(),
			Actor:                userIDOf(c),
		}
		saved, err := h.healthRepo.Create(ctx, run)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create health run"})
			return
		}

		pvJSON, _ := json.Marshal(req.ProviderVersions)
		if req.ProviderVersions == nil {
			pvJSON = []byte("{}")
		}
		mvJSON, _ := json.Marshal(req.ModuleVersions)
		if req.ModuleVersions == nil {
			mvJSON = []byte("{}")
		}
		callbackURL := strings.TrimRight(h.cfg.Server.CallbackBase(), "/") + "/api/v1/health-lab/runs/" + saved.ID + "/results"
		inputs := map[string]string{
			"callback_url":      callbackURL,
			"callback_token":    run.CallbackToken,
			"working_dir":       req.WorkingDir,
			"terraform_version": req.TerraformVersion,
			"provider_versions": string(pvJSON),
			"module_versions":   string(mvJSON),
			"registry_host":     req.RegistryHost,
		}

		var dispatchErr error
		switch conn.Provider {
		case "github_actions":
			dispatchErr = pipelines.DispatchGitHub(ctx, token, pipelines.GitHubConfigFromMap(conn.Config), req.RepoRef, inputs)
		case "azure_devops":
			dispatchErr = pipelines.DispatchAzureDevOps(ctx, token, pipelines.AzureDevOpsConfigFromMap(conn.Config), req.RepoRef, inputs)
		default:
			dispatchErr = errUnsupportedProvider(conn.Provider)
		}
		if dispatchErr != nil {
			_ = h.healthRepo.UpdateStatus(ctx, saved.ID, "failed", dispatchErr.Error())
			saved.Status = "failed"
			saved.Detail = dispatchErr.Error()
			saved.CallbackToken = ""
			c.JSON(http.StatusBadGateway, saved)
			return
		}

		saved.CallbackToken = ""
		c.JSON(http.StatusAccepted, saved)
	}
}

// ListRuns returns recent health runs.
// @Summary      List version-health runs
// @Tags         VersionLab
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /health-lab/runs [get]
func (h *HealthHandlers) ListRuns() gin.HandlerFunc {
	return func(c *gin.Context) {
		runs, err := h.healthRepo.List(c.Request.Context(), 50)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list health runs"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"runs": runs})
	}
}

// GetRun returns a single health run.
func (h *HealthHandlers) GetRun() gin.HandlerFunc {
	return func(c *gin.Context) {
		run, err := h.healthRepo.GetByID(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load health run"})
			return
		}
		if run == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "health run not found"})
			return
		}
		run.CallbackToken = ""
		c.JSON(http.StatusOK, run)
	}
}

// RunResults is the machine callback the CI job posts health results to.
func (h *HealthHandlers) RunResults() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()

		var body struct {
			Token   string          `json:"token"`
			Status  string          `json:"status"`
			InitOK  *bool           `json:"init_ok"`
			PlanOK  *bool           `json:"plan_ok"`
			Success *bool           `json:"success"`
			Detail  string          `json:"detail"`
			Summary json.RawMessage `json:"summary"`
		}
		_ = c.ShouldBindJSON(&body)

		token := c.GetHeader("X-TSM-Callback-Token")
		if token == "" {
			token = body.Token
		}

		run, err := h.healthRepo.GetByID(ctx, c.Param("id"))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load health run"})
			return
		}
		// Uniform 401 whether the run is missing or the token is wrong (no oracle).
		if run == nil || run.CallbackToken == "" ||
			subtle.ConstantTimeCompare([]byte(token), []byte(run.CallbackToken)) != 1 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid callback token"})
			return
		}
		// One-shot: atomically consume the token; a replay finds it already cleared.
		consumed, err := h.healthRepo.ConsumeCallbackToken(ctx, run.ID, run.CallbackToken)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record results"})
			return
		}
		if !consumed {
			c.JSON(http.StatusConflict, gin.H{"error": "callback already processed"})
			return
		}

		status := body.Status
		if status == "" {
			status = "completed"
		}
		initOK := boolVal(body.InitOK)
		planOK := boolVal(body.PlanOK)
		success := initOK && planOK
		if body.Success != nil {
			success = *body.Success
		}
		if err := h.healthRepo.UpdateResult(ctx, run.ID, status, initOK, planOK, success, body.Summary, body.Detail); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record results"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "recorded"})
	}
}

// WorkflowTemplate returns the runner-side health CI definition.
func (h *HealthHandlers) WorkflowTemplate() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.DefaultQuery("provider", "github_actions") {
		case "azure_devops":
			c.Data(http.StatusOK, "text/yaml; charset=utf-8", []byte(azureHealthPipeline))
		default:
			c.Data(http.StatusOK, "text/yaml; charset=utf-8", []byte(githubHealthWorkflow))
		}
	}
}

func boolVal(p *bool) bool { return p != nil && *p }
