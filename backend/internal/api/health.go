// health.go implements the Phase 4 version lab: dispatching a terraform
// init/plan against pinned Terraform/provider versions (optionally via the
// registry mirror) through CI, and ingesting the health result via callback.
package api

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/pipelines"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/notify"
)

// HealthHandlers serves version-lab (health run) endpoints.
type HealthHandlers struct {
	cfg          *config.Config
	pipelineRepo *repositories.PipelineRepository
	ciSourceRepo *repositories.CISourceRepository
	healthRepo   *repositories.HealthRepository
	audit        auditor
	notifier     *notify.Notifier // may be nil (notifications disabled / no DB)
	orgs         organizationExistence
}

// AttachOrganizations supplies the existence check the acting-organization
// resolver uses on the platform-admin branch. See acting_organization.go.
func (h *HealthHandlers) AttachOrganizations(orgs organizationExistence) { h.orgs = orgs }

// NewHealthHandlers constructs the handlers over the app (public) connection.
// identityDB (may be nil) carries the shared audit log; the notifier (may be nil)
// fires the run_failed alert when the background reconciler expires a stuck run.
func NewHealthHandlers(cfg *config.Config, database, identityDB *sql.DB, notifier *notify.Notifier) *HealthHandlers {
	return &HealthHandlers{
		cfg:          cfg,
		pipelineRepo: repositories.NewPipelineRepository(database),
		ciSourceRepo: repositories.NewCISourceRepository(database),
		healthRepo:   repositories.NewHealthRepository(database),
		audit:        newAuditor(identityDB),
		notifier:     notifier,
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
		// The health run is stamped with the ACTING organization, not with the
		// pipeline connection's -- health_runs.pipeline_connection_id is
		// ON DELETE SET NULL, so an inherited answer evaporates into an
		// unpartitioned row the moment the connection is deleted (#436). The
		// connection is a CROSS-CHECK below, not the source of the value.
		//
		// Resolved BEFORE the first statement runs, so a caller with no resolvable
		// acting organization is refused without a database round trip -- and so
		// the unresolved-scope path is reachable in a test without being confused
		// for a failed query, which is how this refusal previously passed its own
		// test for the wrong reason.
		organizationID := actingOrganization(c, h.orgs)
		if organizationID == "" {
			return // actingOrganization has already written the response
		}

		ctx := c.Request.Context()
		// The shared ownership rule, not a second copy of it: this check and
		// dispatchDrift's used to be written separately, and only this one
		// existed. See dispatch_ownership.go.
		conn, err := pipelineConnectionFor(ctx, h.pipelineRepo, req.PipelineConnectionID, organizationID)
		if errors.Is(err, errNotOwnedHere) || (err == nil && conn == nil) {
			// Same shape either way: a caller outside the owning organization
			// learns nothing about whether the id exists.
			c.JSON(http.StatusNotFound, gin.H{"error": "pipeline connection not found"})
			return
		}
		if err != nil {
			serverError(c, err, "failed to load pipeline connection")
			return
		}
		// Connection-level token, or the shared token of its CI source.
		token, bearer, err := resolvePipelineToken(ctx, h.ciSourceRepo, conn)
		if err != nil {
			serverError(c, err, "failed to resolve pipeline token")
			return
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
		saved, err := h.healthRepo.Create(ctx, run, organizationID)
		if err != nil {
			serverError(c, err, "failed to create health run")
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
			dispatchErr = pipelines.DispatchAzureDevOps(ctx, adoCred(token, bearer), pipelines.AzureDevOpsConfigFromMap(conn.Config), req.RepoRef, inputs)
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
		h.audit.write(c, "health_run.dispatch", "health_run", saved.ID, map[string]interface{}{
			"pipeline_connection_id": req.PipelineConnectionID,
			"terraform_version":      req.TerraformVersion,
			"status":                 saved.Status,
		})
		c.JSON(http.StatusAccepted, saved)
	}
}

// ListRuns returns health runs, newest first, with server-side pagination.
// @Summary      List version-health runs
// @Tags         VersionLab
// @Produce      json
// @Param        status  query  string  false  "filter by status (dispatched|running|completed|failed)"
// @Param        limit   query  int     false  "page size (default 50, max 200)"
// @Param        offset  query  int     false  "rows to skip"
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /health-lab/runs [get]
func (h *HealthHandlers) ListRuns() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		status := c.Query("status")
		switch status {
		case "", "dispatched", "running", "completed", "failed":
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status filter"})
			return
		}
		limit, _ := strconv.Atoi(c.Query("limit"))   // 0 -> repo default (50)
		offset, _ := strconv.Atoi(c.Query("offset")) // 0 -> first page
		runs, err := h.healthRepo.List(ctx, limit, offset, status)
		if err != nil {
			serverError(c, err, "failed to list health runs")
			return
		}
		total, err := h.healthRepo.CountRuns(ctx, status)
		if err != nil {
			serverError(c, err, "failed to count health runs")
			return
		}
		c.JSON(http.StatusOK, gin.H{"runs": runs, "total": total})
	}
}

// GetRun returns a single health run.
func (h *HealthHandlers) GetRun() gin.HandlerFunc {
	return func(c *gin.Context) {
		run, err := h.healthRepo.GetByID(c.Request.Context(), c.Param("id"))
		if err != nil {
			serverError(c, err, "failed to load health run")
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
			serverError(c, err, "failed to load health run")
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
			serverError(c, err, "failed to record results")
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
			serverError(c, err, "failed to record results")
			return
		}

		h.notifyHealthResult(run.OrganizationID, run.ID, status, success, body.Detail)
		c.JSON(http.StatusOK, gin.H{"status": "recorded"})
	}
}

// healthResultFailed reports whether a health result warrants a run_failed alert.
// A health run has no "drift detected" outcome — the only thing to alert on is
// failure — but "failure" is not just status=="failed": the version-lab runner
// always posts status="completed" and signals a broken version combination via
// success=false (init/plan failed). So a result is a failure when it did not
// succeed OR the status is an explicit "failed" (which is what the background
// reconciler sends when it expires a stuck run).
func healthResultFailed(status string, success bool) bool {
	return status == "failed" || !success
}

// notifyHealthResult fires an alert event when a health result is a failure (see
// healthResultFailed). It runs detached (the caller must not block on webhook
// latency) with its own timeout; a nil notifier (notifications disabled) is a
// no-op. Shared by the result callback and the background reconciler, so an
// expiry fires the same run_failed alert a real failure callback would.
func (h *HealthHandlers) notifyHealthResult(organizationID, runID, status string, success bool, detail string) {
	if h.notifier == nil || !healthResultFailed(status, success) {
		return
	}
	ev := notify.Event{
		Type:    notify.EventRunFailed,
		Title:   "Health run failed",
		Message: fmt.Sprintf("Health run %s failed: %s", runID, detail),
	}
	// Fanned out to THIS organization's channels only. Without the scope the
	// notifier selects every enabled channel in the deployment, so the health run that failed
	// would be announced to every other tenant's webhooks (#459).
	go func(ev notify.Event, organizationID string) {
		ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
		defer cancel()
		h.notifier.Notify(ctx, ev, notify.ForOrganization(organizationID))
	}(ev, organizationID)
}

// WorkflowTemplate returns the runner-side health CI definition, served from the
// operator-managed store (falling back to the embedded built-in).
func (h *HealthHandlers) WorkflowTemplate(templates *repositories.WorkflowTemplateRepository) gin.HandlerFunc {
	return serveWorkflowTemplate(templates, "versionlab")
}

func boolVal(p *bool) bool { return p != nil && *p }
