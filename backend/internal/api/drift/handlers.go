// Package drift implements the drift HTTP surface: the inbound code-drift ingest
// endpoint (OIDC workload-identity authenticated) plus the manual,
// JWT/API-key-authenticated triggers for the environment-drift and outbound
// plan-trigger capabilities.
//
// A CI pipeline (e.g. Azure DevOps) posts a Terraform plan JSON to /drift/ingest
// authenticated with an OIDC workload-identity bearer token; the handler verifies
// the token, parses the plan, and records a code-sourced drift event. Operators
// invoke /drift/env-check and /drift/trigger with a TSM JWT or API key carrying
// the drift:write scope. When a capability's credentials are unconfigured its
// trigger returns 503, mirroring the ingest endpoint's behaviour when its OIDC
// issuer is unset.
package drift

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth/driftingest"
	captrigger "github.com/terraform-state-manager/terraform-state-manager/internal/capability/drifttrigger"
	capenvdrift "github.com/terraform-state-manager/terraform-state-manager/internal/capability/envdrift"
	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/hcp"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	ingestsvc "github.com/terraform-state-manager/terraform-state-manager/internal/services/driftingest"
	triggersvc "github.com/terraform-state-manager/terraform-state-manager/internal/services/drifttrigger"
)

// Handlers serves the drift HTTP endpoints: inbound ingest and the manual
// env-drift and outbound-trigger capability triggers.
type Handlers struct {
	validator  *driftingest.Validator
	driftRepo  *repositories.DriftEventRepository
	sourceRepo *repositories.StateSourceRepository

	// envDrift and trigger are the capability handlers behind the manual
	// triggers. Either may be unconfigured (no credential), in which case its
	// endpoint returns 503; both may be nil when the capabilities are not wired.
	envDrift *capenvdrift.Handler
	trigger  *captrigger.Handler
}

// NewHandlers constructs the drift-ingest Handlers. validator may be nil when the
// drift-ingest OIDC issuer is not configured, in which case the ingest endpoint
// rejects all requests with 503. The manual trigger handlers are wired
// separately via WithCapabilities.
func NewHandlers(
	validator *driftingest.Validator,
	driftRepo *repositories.DriftEventRepository,
	sourceRepo *repositories.StateSourceRepository,
) *Handlers {
	return &Handlers{
		validator:  validator,
		driftRepo:  driftRepo,
		sourceRepo: sourceRepo,
	}
}

// WithCapabilities attaches the environment-drift and outbound-trigger capability
// handlers used by the manual trigger endpoints. Either handler may be nil or
// unconfigured; the endpoints return 503 accordingly. It returns the receiver for
// fluent wiring.
func (h *Handlers) WithCapabilities(
	envDrift *capenvdrift.Handler,
	trigger *captrigger.Handler,
) *Handlers {
	h.envDrift = envDrift
	h.trigger = trigger
	return h
}

// ingestRequest is the request body for the IngestPlan endpoint.
type ingestRequest struct {
	SourceID       string          `json:"source_id" binding:"required"`
	Plan           json.RawMessage `json:"plan"`
	ChangesPresent bool            `json:"changes_present"`
	ExternalRef    string          `json:"external_ref" binding:"required"`
}

// IngestPlan handles POST /api/v1/drift/ingest.
//
// Authentication is an OIDC workload-identity bearer token (NOT a TSM API key):
// the request must carry "Authorization: Bearer <oidc-token>", verified against
// the configured drift-ingest issuer's JWKS and audience.
//
// The body carries a source_id, a Terraform plan JSON (terraform show -json),
// a changes_present flag, and an external_ref idempotency key. If external_ref
// has already been ingested the existing event is returned (200, idempotent).
// Otherwise, when the plan contains non-no-op resource changes, a code-sourced
// drift event is created and returned (201). A plan with no changes records no
// event and returns 200.
//
// @Summary      Ingest code drift from a Terraform plan
// @Description  Ingests a Terraform plan JSON from a CI pipeline as a code-drift event. Authenticated with an OIDC workload-identity bearer token verified against the configured drift-ingest issuer (not a TSM API key). Idempotent on external_ref.
// @Tags         Drift
// @Accept       json
// @Produce      json
// @Param        body  body      ingestRequest  true  "Plan ingest request"
// @Success      200  {object}  map[string]interface{}
// @Success      201  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Failure      503  {object}  map[string]interface{}
// @Security     OIDCIngestAuth
// @Router       /drift/ingest [post]
func (h *Handlers) IngestPlan(c *gin.Context) {
	// The validator is nil when the drift-ingest OIDC issuer is not configured.
	if h.validator == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "drift ingest is not configured"})
		return
	}

	// --- OIDC bearer-token authentication ---
	authHeader := c.GetHeader("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "OIDC bearer token is required"})
		return
	}
	rawToken := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	if rawToken == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "OIDC bearer token is required"})
		return
	}

	ctx := c.Request.Context()
	if _, err := h.validator.Verify(ctx, rawToken); err != nil {
		slog.Warn("drift ingest: token verification failed", "error", err)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid OIDC token"})
		return
	}

	// --- Parse and validate the body ---
	var req ingestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid input",
			"details": err.Error(),
		})
		return
	}

	if _, err := uuid.Parse(req.SourceID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source_id"})
		return
	}

	// Resolve the source to obtain its organization and a workspace label.
	source, err := h.sourceRepo.GetByID(ctx, req.SourceID)
	if err != nil {
		slog.Error("drift ingest: failed to get source", "error", err, "source_id", req.SourceID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve source"})
		return
	}
	if source == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
		return
	}

	// --- Idempotency: return the existing event for a known external_ref ---
	existing, err := h.driftRepo.GetByExternalRef(ctx, source.OrganizationID, req.ExternalRef)
	if err != nil {
		slog.Error("drift ingest: failed to check external_ref", "error", err, "external_ref", req.ExternalRef)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check idempotency"})
		return
	}
	if existing != nil {
		c.JSON(http.StatusOK, gin.H{
			"data":       existing,
			"idempotent": true,
		})
		return
	}

	// --- Parse the plan and summarize the changes ---
	var plan ingestsvc.Plan
	if len(req.Plan) > 0 {
		if err := json.Unmarshal(req.Plan, &plan); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   "invalid plan JSON",
				"details": err.Error(),
			})
			return
		}
	}

	changes := ingestsvc.SummarizePlan(&plan)

	// No drift if neither the caller flag nor the plan indicate changes.
	if !req.ChangesPresent && !changes.HasChanges() {
		c.JSON(http.StatusOK, gin.H{
			"data":            nil,
			"changes_present": false,
			"message":         "no drift detected",
		})
		return
	}

	changesBytes, err := json.Marshal(changes)
	if err != nil {
		slog.Error("drift ingest: failed to marshal changes", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode drift changes"})
		return
	}

	severity := models.ClassifyDriftSeverity(
		len(changes.Added),
		len(changes.Removed),
		len(changes.Modified),
	)

	externalRef := req.ExternalRef
	event := &models.DriftEvent{
		OrganizationID: source.OrganizationID,
		WorkspaceName:  source.Name,
		Changes:        changesBytes,
		Severity:       severity,
		DriftSource:    models.DriftSourceCode,
		ExternalRef:    &externalRef,
	}

	if err := h.driftRepo.Create(ctx, event); err != nil {
		slog.Error("drift ingest: failed to create drift event", "error", err, "external_ref", externalRef)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create drift event"})
		return
	}

	slog.Info("code drift ingested",
		"event_id", event.ID,
		"source_id", req.SourceID,
		"org_id", source.OrganizationID,
		"severity", severity,
		"added", len(changes.Added),
		"removed", len(changes.Removed),
		"modified", len(changes.Modified),
		"external_ref", externalRef)

	c.JSON(http.StatusCreated, gin.H{"data": event})
}

// envCheckRequest is the request body for the EnvCheck endpoint. SourceID scopes
// the check to a state source (used for org membership and the drift-event
// workspace label). State is the parsed Terraform state JSON to evaluate against
// live Azure; it is supplied inline because live state fetching from a source is
// deferred.
type envCheckRequest struct {
	SourceID string         `json:"source_id" binding:"required"`
	State    *hcp.StateFile `json:"state" binding:"required"`
}

// EnvCheck handles POST /api/v1/drift/env-check.
//
// It runs an on-demand environment-drift check: the azurerm resources recorded in
// the supplied state are compared against live Azure and, when any are missing or
// changed, a drift_events row (drift_source = "environment") is written. The
// source_id scopes the check to the caller's organization. Authentication is the
// standard TSM JWT/API key with the drift:write scope.
//
// When the environment-drift capability is unconfigured (no Azure credential) the
// endpoint returns 503, mirroring the inbound ingest endpoint when its OIDC issuer
// is unset.
//
// @Summary      Trigger an environment-drift check
// @Description  Compares the azurerm resources in the supplied Terraform state against live Azure and records an environment-sourced drift event when any resource is missing or changed. Returns 503 when the environment-drift capability has no Azure credential configured.
// @Tags         Drift
// @Accept       json
// @Produce      json
// @Param        body  body      envCheckRequest  true  "Environment-drift check request"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      403  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Failure      503  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /drift/env-check [post]
func (h *Handlers) EnvCheck(c *gin.Context) {
	if h.envDrift == nil || !h.envDrift.Configured() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "environment-drift detection is not configured"})
		return
	}

	orgID, ok := orgIDFromContext(c)
	if !ok {
		return
	}

	var req envCheckRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid input", "details": err.Error()})
		return
	}
	if _, err := uuid.Parse(req.SourceID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source_id"})
		return
	}

	ctx := c.Request.Context()
	source, ok := h.resolveOwnedSource(c, ctx, req.SourceID, orgID)
	if !ok {
		return
	}

	result, err := h.envDrift.Detect(ctx, orgID, source.Name, req.State)
	if err != nil {
		slog.Error("drift env-check: detection failed", "error", err, "source_id", req.SourceID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "environment-drift detection failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"source_id":      req.SourceID,
		"workspace":      source.Name,
		"severity":       result.Severity,
		"drift_detected": result.DriftEventID != "",
		"drift_event_id": result.DriftEventID,
		"changes":        result.Changes,
	})
}

// triggerRequest is the request body for the Trigger endpoint. SourceID scopes
// the trigger to a state source (org membership + correlation). PipelineID is the
// Azure DevOps plan pipeline to queue; Branch and Parameters are optional.
type triggerRequest struct {
	SourceID   string            `json:"source_id" binding:"required"`
	PipelineID int               `json:"pipeline_id" binding:"required"`
	Branch     string            `json:"branch,omitempty"`
	Parameters map[string]string `json:"parameters,omitempty"`
}

// Trigger handles POST /api/v1/drift/trigger.
//
// It queues an outbound Terraform "plan" pipeline run in Azure DevOps for the
// given source. The plan reports back through the inbound ingest endpoint. The
// source_id scopes the trigger to the caller's organization. Authentication is
// the standard TSM JWT/API key with the drift:write scope.
//
// When the outbound-trigger capability is unconfigured (ADO/WIF unset) the
// endpoint returns 503, mirroring the inbound ingest endpoint.
//
// @Summary      Trigger an outbound plan-pipeline run
// @Description  Queues an Azure DevOps Terraform plan pipeline run for the source; the plan reports drift back via the ingest endpoint. Returns 503 when the outbound-trigger capability is unconfigured (ADO/WIF unset).
// @Tags         Drift
// @Accept       json
// @Produce      json
// @Param        body  body      triggerRequest  true  "Outbound plan-trigger request"
// @Success      202  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      403  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Failure      503  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /drift/trigger [post]
func (h *Handlers) Trigger(c *gin.Context) {
	if h.trigger == nil || !h.trigger.Configured() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "outbound drift trigger is not configured"})
		return
	}

	orgID, ok := orgIDFromContext(c)
	if !ok {
		return
	}

	var req triggerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid input", "details": err.Error()})
		return
	}
	if _, err := uuid.Parse(req.SourceID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source_id"})
		return
	}
	if req.PipelineID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "pipeline_id must be positive"})
		return
	}

	ctx := c.Request.Context()
	if _, ok := h.resolveOwnedSource(c, ctx, req.SourceID, orgID); !ok {
		return
	}

	result, err := h.trigger.Fire(ctx, triggersvc.TriggerRequest{
		SourceID:   req.SourceID,
		PipelineID: req.PipelineID,
		Branch:     req.Branch,
		Parameters: req.Parameters,
	})
	if err != nil {
		slog.Error("drift trigger: queuing plan run failed", "error", err, "source_id", req.SourceID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to queue plan pipeline run"})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"source_id": req.SourceID,
		"run_id":    result.RunID,
		"run_state": result.RunState,
		"run_url":   result.RunURL,
	})
}

// orgIDFromContext extracts the authenticated organization id, writing a 400 and
// returning false when it is absent.
func orgIDFromContext(c *gin.Context) (string, bool) {
	v, _ := c.Get("organization_id")
	orgID, ok := v.(string)
	if !ok || orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return "", false
	}
	return orgID, true
}

// resolveOwnedSource loads the source by id and verifies it belongs to orgID,
// writing the appropriate error response and returning false on any failure
// (500 on lookup error, 404 when missing, 403 on cross-org access).
func (h *Handlers) resolveOwnedSource(
	c *gin.Context,
	ctx context.Context,
	sourceID, orgID string,
) (*models.StateSource, bool) {
	source, err := h.sourceRepo.GetByID(ctx, sourceID)
	if err != nil {
		slog.Error("drift: failed to get source", "error", err, "source_id", sourceID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve source"})
		return nil, false
	}
	if source == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
		return nil, false
	}
	if source.OrganizationID != orgID {
		c.JSON(http.StatusForbidden, gin.H{"error": "source does not belong to your organization"})
		return nil, false
	}
	return source, true
}
