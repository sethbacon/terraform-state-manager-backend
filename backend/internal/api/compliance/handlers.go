// Package compliance implements the HTTP handlers for compliance policy and result management.
package compliance

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	compliancesvc "github.com/terraform-state-manager/terraform-state-manager/internal/services/compliance"
)

// Handlers provides the HTTP handlers for compliance API endpoints.
type Handlers struct {
	policyRepo *repositories.CompliancePolicyRepository
	resultRepo *repositories.ComplianceResultRepository
	checker    *compliancesvc.Checker
}

// NewHandlers creates a new Handlers instance.
func NewHandlers(
	policyRepo *repositories.CompliancePolicyRepository,
	resultRepo *repositories.ComplianceResultRepository,
	checker *compliancesvc.Checker,
) *Handlers {
	return &Handlers{
		policyRepo: policyRepo,
		resultRepo: resultRepo,
		checker:    checker,
	}
}

// ---------------------------------------------------------------------------
// Policy handlers
// ---------------------------------------------------------------------------

// CreatePolicy handles POST /api/v1/compliance/policies.
// Creates a new compliance policy for the authenticated user's organization.
//
// @Summary      Create compliance policy
// @Tags         Compliance
// @Accept       json
// @Produce      json
// @Param        body  body      models.CompliancePolicyCreateRequest  true  "Compliance policy"
// @Success      201  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /compliance/policies [post]
func (h *Handlers) CreatePolicy(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	var req models.CompliancePolicyCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid input",
			"details": err.Error(),
		})
		return
	}

	severity := "warning"
	if req.Severity != "" {
		severity = req.Severity
	}

	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}

	policy := &models.CompliancePolicy{
		OrganizationID: orgIDStr,
		Name:           req.Name,
		PolicyType:     req.PolicyType,
		Config:         req.Config,
		Severity:       severity,
		IsActive:       isActive,
	}

	ctx := c.Request.Context()
	if err := h.policyRepo.Create(ctx, policy); err != nil {
		slog.Error("Failed to create compliance policy", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create compliance policy"})
		return
	}

	slog.Info("Compliance policy created",
		"policy_id", policy.ID, "name", policy.Name, "type", policy.PolicyType)

	c.JSON(http.StatusCreated, gin.H{"data": policy})
}

// ListPolicies handles GET /api/v1/compliance/policies.
// Returns a paginated list of compliance policies for the authenticated user's organization.
//
// @Summary      List compliance policies
// @Tags         Compliance
// @Accept       json
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /compliance/policies [get]
func (h *Handlers) ListPolicies(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	ctx := c.Request.Context()
	policies, total, err := h.policyRepo.ListByOrganization(ctx, orgIDStr, limit, offset)
	if err != nil {
		slog.Error("Failed to list compliance policies", "error", err, "org_id", orgIDStr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list compliance policies"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data":   policies,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// GetPolicy handles GET /api/v1/compliance/policies/:id.
// Returns a single compliance policy by ID.
//
// @Summary      Get compliance policy
// @Tags         Compliance
// @Accept       json
// @Produce      json
// @Param        id   path      string  true  "Policy ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /compliance/policies/{id} [get]
func (h *Handlers) GetPolicy(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid policy ID"})
		return
	}

	ctx := c.Request.Context()
	policy, err := h.policyRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get compliance policy", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve compliance policy"})
		return
	}
	if policy == nil || policy.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "compliance policy not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": policy})
}

// UpdatePolicy handles PUT /api/v1/compliance/policies/:id.
// Applies partial updates to a compliance policy.
//
// @Summary      Update compliance policy
// @Tags         Compliance
// @Accept       json
// @Produce      json
// @Param        id    path      string                                true  "Policy ID"
// @Param        body  body      models.CompliancePolicyUpdateRequest  true  "Compliance policy update"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /compliance/policies/{id} [put]
func (h *Handlers) UpdatePolicy(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid policy ID"})
		return
	}

	var req models.CompliancePolicyUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid input",
			"details": err.Error(),
		})
		return
	}

	ctx := c.Request.Context()
	policy, err := h.policyRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get compliance policy for update", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve compliance policy"})
		return
	}
	if policy == nil || policy.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "compliance policy not found"})
		return
	}

	// Apply partial updates.
	if req.Name != nil {
		policy.Name = *req.Name
	}
	if req.PolicyType != nil {
		policy.PolicyType = *req.PolicyType
	}
	if req.Config != nil {
		policy.Config = *req.Config
	}
	if req.Severity != nil {
		policy.Severity = *req.Severity
	}
	if req.IsActive != nil {
		policy.IsActive = *req.IsActive
	}

	if err := h.policyRepo.Update(ctx, policy); err != nil {
		slog.Error("Failed to update compliance policy", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update compliance policy"})
		return
	}

	slog.Info("Compliance policy updated", "policy_id", id)
	c.JSON(http.StatusOK, gin.H{"data": policy})
}

// DeletePolicy handles DELETE /api/v1/compliance/policies/:id.
//
// @Summary      Delete compliance policy
// @Tags         Compliance
// @Accept       json
// @Produce      json
// @Param        id   path      string  true  "Policy ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /compliance/policies/{id} [delete]
func (h *Handlers) DeletePolicy(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid policy ID"})
		return
	}

	ctx := c.Request.Context()
	policy, err := h.policyRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get compliance policy for deletion", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve compliance policy"})
		return
	}
	if policy == nil || policy.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "compliance policy not found"})
		return
	}

	if err := h.policyRepo.Delete(ctx, id); err != nil {
		slog.Error("Failed to delete compliance policy", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete compliance policy"})
		return
	}

	slog.Info("Compliance policy deleted", "policy_id", id, "name", policy.Name)
	c.JSON(http.StatusOK, gin.H{"message": "compliance policy deleted successfully"})
}

// ---------------------------------------------------------------------------
// Result handlers
// ---------------------------------------------------------------------------

// ListResults handles GET /api/v1/compliance/results.
// Returns a paginated list of compliance results, filterable by run_id.
//
// @Summary      Get compliance results
// @Tags         Compliance
// @Accept       json
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /compliance/results [get]
func (h *Handlers) ListResults(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	runID := c.Query("run_id")
	if runID != "" {
		if _, err := uuid.Parse(runID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run_id"})
			return
		}
	}

	ctx := c.Request.Context()

	if runID != "" {
		results, total, err := h.resultRepo.ListByRun(ctx, runID, limit, offset)
		if err != nil {
			slog.Error("Failed to list compliance results by run", "error", err, "run_id", runID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list compliance results"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"data":   results,
			"total":  total,
			"limit":  limit,
			"offset": offset,
		})
		return
	}

	// Without a run_id filter, return results for all policies in the organization.
	// First get all policies for the org, then aggregate results.
	policies, _, err := h.policyRepo.ListByOrganization(ctx, orgIDStr, 1000, 0)
	if err != nil {
		slog.Error("Failed to list policies for results", "error", err, "org_id", orgIDStr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list compliance results"})
		return
	}

	var allResults []models.ComplianceResult
	totalCount := 0

	for _, policy := range policies {
		results, total, err := h.resultRepo.ListByPolicy(ctx, policy.ID, limit, offset)
		if err != nil {
			slog.Error("Failed to list compliance results by policy",
				"error", err, "policy_id", policy.ID)
			continue
		}
		allResults = append(allResults, results...)
		totalCount += total
	}

	c.JSON(http.StatusOK, gin.H{
		"data":   allResults,
		"total":  totalCount,
		"limit":  limit,
		"offset": offset,
	})
}

// GetComplianceScore handles GET /api/v1/compliance/score.
// Returns the aggregate compliance score for the authenticated user's organization.
//
// @Summary      Get compliance score
// @Tags         Compliance
// @Accept       json
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /compliance/score [get]
func (h *Handlers) GetComplianceScore(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	ctx := c.Request.Context()
	score, err := h.resultRepo.GetComplianceScore(ctx, orgIDStr)
	if err != nil {
		slog.Error("Failed to get compliance score", "error", err, "org_id", orgIDStr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get compliance score"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": score})
}
