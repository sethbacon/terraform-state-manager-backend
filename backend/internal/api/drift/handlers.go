// Package drift implements the inbound code-drift ingest endpoint. A CI pipeline
// (e.g. Azure DevOps) posts a Terraform plan JSON authenticated with an OIDC
// workload-identity bearer token; the handler verifies the token, parses the
// plan, and records a code-sourced drift event.
package drift

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth/driftingest"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	ingestsvc "github.com/terraform-state-manager/terraform-state-manager/internal/services/driftingest"
)

// Handlers serves the drift-ingest endpoint.
type Handlers struct {
	validator  *driftingest.Validator
	driftRepo  *repositories.DriftEventRepository
	sourceRepo *repositories.StateSourceRepository
}

// NewHandlers constructs the drift-ingest Handlers. validator may be nil when the
// drift-ingest OIDC issuer is not configured, in which case the endpoint rejects
// all requests with 503.
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
