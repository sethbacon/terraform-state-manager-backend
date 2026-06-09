// drift.go implements the Phase 3 drift plane: CI pipeline connections, drift-run
// dispatch (GitHub Actions / Azure DevOps), and the signed result callback the CI
// job posts back to. The app never runs terraform or holds cloud credentials.
package api

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/pipelines"
)

// DriftHandlers serves pipeline-connection and drift-run endpoints.
type DriftHandlers struct {
	cfg          *config.Config
	pipelineRepo *repositories.PipelineRepository
	driftRepo    *repositories.DriftRepository
}

// NewDriftHandlers constructs the handlers over the app (public) connection.
func NewDriftHandlers(cfg *config.Config, database *sql.DB) *DriftHandlers {
	return &DriftHandlers{
		cfg:          cfg,
		pipelineRepo: repositories.NewPipelineRepository(database),
		driftRepo:    repositories.NewDriftRepository(database),
	}
}

func randomToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		// A failure of the system CSPRNG is catastrophic and must never yield a
		// predictable (all-zero) callback token; fail loudly instead.
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// --- Pipeline connections ---

// ListPipelines returns configured CI connections (no secrets).
func (h *DriftHandlers) ListPipelines() gin.HandlerFunc {
	return func(c *gin.Context) {
		conns, err := h.pipelineRepo.List(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list pipelines"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"pipelines": conns})
	}
}

// CreatePipeline registers a CI connection, encrypting its token.
func (h *DriftHandlers) CreatePipeline() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Name     string         `json:"name" binding:"required"`
			Provider string         `json:"provider" binding:"required"`
			Config   map[string]any `json:"config"`
			Token    string         `json:"token"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name and provider are required"})
			return
		}
		if req.Provider != "github_actions" && req.Provider != "azure_devops" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "provider must be github_actions or azure_devops"})
			return
		}
		pc := &repositories.PipelineConnection{Name: req.Name, Provider: req.Provider, Config: req.Config}
		if req.Token != "" {
			if !crypto.Available() {
				c.JSON(http.StatusBadRequest, gin.H{"error": "cannot store token: encryption key not configured (set TSM_ENCRYPTION_KEY)"})
				return
			}
			enc, err := crypto.Encrypt([]byte(req.Token))
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt token"})
				return
			}
			pc.EncryptedToken = enc
		}
		saved, err := h.pipelineRepo.Create(c.Request.Context(), pc)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create pipeline connection"})
			return
		}
		c.JSON(http.StatusCreated, saved)
	}
}

// DeletePipeline removes a CI connection.
func (h *DriftHandlers) DeletePipeline() gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := h.pipelineRepo.Delete(c.Request.Context(), c.Param("id")); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete pipeline connection"})
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// --- Drift runs ---

// CreateRun dispatches a drift run on the chosen pipeline and records it.
// @Summary      Dispatch drift run
// @Description  Dispatches a terraform-plan drift run on the chosen CI pipeline and records it. Inputs are validated server-side. Requires state:drift.
// @Tags         Drift
// @Accept       json
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /drift/runs [post]
func (h *DriftHandlers) CreateRun() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			PipelineConnectionID string `json:"pipeline_connection_id" binding:"required"`
			SourceID             string `json:"source_id"`
			StateKey             string `json:"state_key"`
			RepoRef              string `json:"repo_ref"`
			WorkingDir           string `json:"working_dir"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "pipeline_connection_id is required"})
			return
		}
		if err := validatePipelineInputs(req.WorkingDir, req.RepoRef, "", "", nil, nil); err != nil {
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

		run := &repositories.DriftRun{
			PipelineConnectionID: &conn.ID,
			StateKey:             req.StateKey,
			RepoRef:              req.RepoRef,
			WorkingDir:           req.WorkingDir,
			Status:               "dispatched",
			CallbackToken:        randomToken(),
			Actor:                userIDOf(c),
		}
		if req.SourceID != "" {
			run.SourceID = &req.SourceID
		}
		saved, err := h.driftRepo.Create(ctx, run)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create drift run"})
			return
		}

		callbackURL := strings.TrimRight(h.cfg.Server.CallbackBase(), "/") + "/api/v1/drift/runs/" + saved.ID + "/results"
		inputs := pipelines.DriftInputs{CallbackURL: callbackURL, CallbackToken: run.CallbackToken, WorkingDir: req.WorkingDir}

		var dispatchErr error
		switch conn.Provider {
		case "github_actions":
			dispatchErr = pipelines.DispatchGitHubDrift(ctx, token, pipelines.GitHubConfigFromMap(conn.Config), req.RepoRef, inputs)
		case "azure_devops":
			dispatchErr = pipelines.DispatchAzureDevOpsDrift(ctx, token, pipelines.AzureDevOpsConfigFromMap(conn.Config), req.RepoRef, inputs)
		default:
			dispatchErr = errUnsupportedProvider(conn.Provider)
		}
		if dispatchErr != nil {
			_ = h.driftRepo.UpdateStatus(ctx, saved.ID, "failed", dispatchErr.Error())
			saved.Status = "failed"
			saved.Detail = dispatchErr.Error()
			saved.CallbackToken = ""
			c.JSON(http.StatusBadGateway, saved)
			return
		}

		saved.CallbackToken = "" // never expose
		c.JSON(http.StatusAccepted, saved)
	}
}

// ListRuns returns recent drift runs.
// @Summary      List drift runs
// @Tags         Drift
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /drift/runs [get]
func (h *DriftHandlers) ListRuns() gin.HandlerFunc {
	return func(c *gin.Context) {
		runs, err := h.driftRepo.List(c.Request.Context(), 50)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list drift runs"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"runs": runs})
	}
}

// GetRun returns a single drift run (without the callback token).
func (h *DriftHandlers) GetRun() gin.HandlerFunc {
	return func(c *gin.Context) {
		run, err := h.driftRepo.GetByID(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load drift run"})
			return
		}
		if run == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "drift run not found"})
			return
		}
		run.CallbackToken = ""
		c.JSON(http.StatusOK, run)
	}
}

// RunResults is the machine callback the CI job posts drift results to. It is
// authenticated by the per-run callback token (no user session).
// @Summary      Drift run callback (machine)
// @Description  CI job posts drift results here, authenticated by the per-run one-shot X-TSM-Callback-Token. Not a user endpoint.
// @Tags         Drift
// @Accept       json
// @Produce      json
// @Param        id   path  string  true  "Drift run ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}  "invalid callback token"
// @Failure      409  {object}  map[string]interface{}  "callback already processed"
// @Security     CallbackToken
// @Router       /drift/runs/{id}/results [post]
func (h *DriftHandlers) RunResults() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()

		var body struct {
			Token     string          `json:"token"`
			Status    string          `json:"status"`
			Added     int             `json:"added"`
			Changed   int             `json:"changed"`
			Destroyed int             `json:"destroyed"`
			Drifted   *bool           `json:"drifted"`
			Detail    string          `json:"detail"`
			Summary   json.RawMessage `json:"summary"`
		}
		_ = c.ShouldBindJSON(&body)

		token := c.GetHeader("X-TSM-Callback-Token")
		if token == "" {
			token = body.Token
		}

		run, err := h.driftRepo.GetByID(ctx, c.Param("id"))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load drift run"})
			return
		}
		// Uniform 401 whether the run is missing or the token is wrong, so the
		// endpoint is not a run-ID existence oracle.
		if run == nil || run.CallbackToken == "" ||
			subtle.ConstantTimeCompare([]byte(token), []byte(run.CallbackToken)) != 1 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid callback token"})
			return
		}
		// One-shot: atomically consume the token; a replay finds it already cleared.
		consumed, err := h.driftRepo.ConsumeCallbackToken(ctx, run.ID, run.CallbackToken)
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
		drifted := body.Added+body.Changed+body.Destroyed > 0
		if body.Drifted != nil {
			drifted = *body.Drifted
		}
		if err := h.driftRepo.UpdateResult(ctx, run.ID, status, body.Added, body.Changed, body.Destroyed, drifted, body.Summary, body.Detail); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record results"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "recorded"})
	}
}

// WorkflowTemplate returns the runner-side CI definition to copy into a repo.
// GET /api/v1/drift/workflow?provider=github_actions|azure_devops
func (h *DriftHandlers) WorkflowTemplate() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.DefaultQuery("provider", "github_actions") {
		case "azure_devops":
			c.Data(http.StatusOK, "text/yaml; charset=utf-8", []byte(azureDriftPipeline))
		default:
			c.Data(http.StatusOK, "text/yaml; charset=utf-8", []byte(githubDriftWorkflow))
		}
	}
}

func errUnsupportedProvider(p string) error {
	return &unsupportedProviderError{provider: p}
}

type unsupportedProviderError struct{ provider string }

func (e *unsupportedProviderError) Error() string {
	return "pipeline provider " + e.provider + " is not supported yet"
}
