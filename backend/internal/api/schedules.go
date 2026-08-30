// schedules.go implements CRUD for cron-driven schedules plus the adapter that
// lets the background scheduler dispatch a schedule's target (a drift run). The
// runner lives in internal/services/scheduler; this file provides the HTTP surface
// and the api-side Dispatcher implementation.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/scheduler"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenancy"
)

// driftDispatcher adapts DriftHandlers to scheduler.Dispatcher so a schedule with
// target_type "drift" fires a drift run on its configured pipeline.
type driftDispatcher struct{ drift *DriftHandlers }

// Dispatch takes a tenancy.SystemScope because the scheduler worker has no
// request to resolve one from: its authority is DERIVED from the schedule row
// being fired (#393 option B). A schedule names its target only inside
// target_config JSONB, with no column and no foreign key, so a run fired from
// it cannot join back to discover whose it is — the organization has to travel
// with the schedule, in memory, across this seam (#436) — and it now travels
// as a real scope, so every load under the dispatch is an InScope read.
func (d driftDispatcher) Dispatch(ctx context.Context, targetType string, targetConfig json.RawMessage, actor string, derived tenancy.SystemScope) (runID, status string, err error) {
	if derived.IsZero() {
		// Defense at the seam: a zero scope would read nothing and fail every
		// load anyway, but refusing here names the actual fault -- the caller
		// never derived an authority -- instead of surfacing it as a string of
		// baffling not-found errors.
		return "", "failed", fmt.Errorf("dispatch refused: %s", derived)
	}
	switch targetType {
	case "drift":
		var t DriftTarget
		if uErr := json.Unmarshal(targetConfig, &t); uErr != nil {
			return "", "failed", fmt.Errorf("invalid drift target config: %w", uErr)
		}
		run, dErr := d.drift.dispatchDrift(ctx, t, actor, systemAuthority(derived))
		id := ""
		if run != nil {
			id = run.ID
		}
		if dErr != nil {
			return id, "failed", dErr
		}
		return id, "success", nil
	default:
		return "", "skipped", fmt.Errorf("unknown schedule target type %q", targetType)
	}
}

// ScheduleHandlers serves the schedule CRUD endpoints.
type ScheduleHandlers struct {
	repo       *repositories.ScheduleRepository
	dispatcher scheduler.Dispatcher
	audit      auditor
	// pipelines and sources back the write-side linkage invariant (#393): a
	// schedule's target may only reference rows its own organization owns,
	// verified at write time so a crossing reference never lands.
	pipelines *repositories.PipelineRepository
	sources   *repositories.SourceRepository
	// orgs verifies that an acting organization exists before a row is stamped
	// with it. Wired by the router from its single approles.Members; nil is
	// refused rather than skipped (see actingOrganization).
	orgs organizationExistence
}

// AttachOrganizations wires the existence check used before a row is stamped
// with an acting organization (#436). A setter, and the router supplies its
// EXISTING approles.Members: internal/approles' guard test refuses a second
// construction of the shared organization repository.
func (h *ScheduleHandlers) AttachOrganizations(orgs organizationExistence) { h.orgs = orgs }

// NewScheduleHandlers builds the handlers over the app connection. dispatcher is
// used by the "run now" endpoint and is the same adapter the background runner
// uses; identityDB (may be nil) carries the shared audit log.
func NewScheduleHandlers(database, identityDB *sql.DB, dispatcher scheduler.Dispatcher) *ScheduleHandlers {
	return &ScheduleHandlers{
		repo:       repositories.NewScheduleRepository(database),
		dispatcher: dispatcher,
		audit:      newAuditor(identityDB),
		pipelines:  repositories.NewPipelineRepository(database),
		sources:    repositories.NewSourceRepository(database),
	}
}

type scheduleRequest struct {
	Name         string          `json:"name" binding:"required"`
	CronExpr     string          `json:"cron_expr" binding:"required"`
	TargetType   string          `json:"target_type"`
	TargetConfig json.RawMessage `json:"target_config"`
	Enabled      *bool           `json:"enabled"`
}

// validate normalizes the request and rejects bad cron expressions / targets.
func (req *scheduleRequest) validate() error {
	if req.TargetType == "" {
		req.TargetType = "drift"
	}
	if req.TargetType != "drift" {
		return fmt.Errorf("unsupported target_type %q (only \"drift\" is supported)", req.TargetType)
	}
	if scheduler.ComputeNextRun(req.CronExpr, time.Now()) == nil {
		return fmt.Errorf("invalid cron_expr (use a 5-field cron expression or \"daily\"/\"weekly\"/\"every <dur>\")")
	}
	// Validate the drift target.
	var t DriftTarget
	if len(req.TargetConfig) > 0 {
		if err := json.Unmarshal(req.TargetConfig, &t); err != nil {
			return fmt.Errorf("invalid target_config: %w", err)
		}
	}
	if t.PipelineConnectionID == "" {
		return fmt.Errorf("target_config.pipeline_connection_id is required")
	}
	if err := validatePipelineInputs(t.WorkingDir, t.RepoRef, "", "", nil, nil); err != nil {
		return err
	}
	return nil
}

// ListSchedules returns all schedules.
// @Summary      List schedules
// @Tags         Schedules
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /schedules [get]
func (h *ScheduleHandlers) ListSchedules() gin.HandlerFunc {
	return func(c *gin.Context) {
		// The Phase 3 read flip for schedules (#393). Unscoped, this listed
		// every organization's schedules to any caller holding state:read in one
		// of them, target_config included -- which names the pipeline connection
		// a firing dispatches to.
		//
		// An UNRESOLVED scope is a 500, never an empty one and certainly never a
		// full one: it means the route was registered without
		// middleware.TenantScope, and reading unscoped because a line is missing
		// from the router is the least visible way to reintroduce this leak.
		scope, resolved := tenantscope.FromContext(c)
		if !resolved {
			serverError(c, errNoTenantScope, "the tenant scope was not resolved for this route")
			return
		}
		items, err := h.repo.ListInScope(c.Request.Context(), scope)
		if err != nil {
			serverError(c, err, "failed to list schedules")
			return
		}
		c.JSON(http.StatusOK, gin.H{"schedules": items})
	}
}

// CreateSchedule registers a cron-driven schedule.
// @Summary      Create schedule
// @Tags         Schedules
// @Accept       json
// @Produce      json
// @Success      201  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /schedules [post]
func (h *ScheduleHandlers) CreateSchedule() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req scheduleRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name and cron_expr are required"})
			return
		}
		if err := req.validate(); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		enabled := req.Enabled == nil || *req.Enabled
		s := &repositories.Schedule{
			Name: req.Name, CronExpr: req.CronExpr, TargetType: req.TargetType,
			TargetConfig: req.TargetConfig, Enabled: enabled,
		}
		// The organization this row belongs to, named by the request and verified
		// against a scope this server resolved (#436). Writes the response
		// itself on every failure path.
		orgID := actingOrganization(c, h.orgs)
		if orgID == "" {
			return
		}

		// THE WRITE-SIDE LINKAGE INVARIANT (#393): the target must reference
		// rows the schedule's own organization owns. The dispatch chain fails
		// closed on a crossing reference at fire time, but refusing it HERE is
		// what keeps the deployment from accumulating schedules that can never
		// fire -- and what stops a write from aiming the background runner at
		// another organization in the first place.
		if !h.targetReferencesInOrganization(c, req.TargetConfig, orgID) {
			return
		}

		saved, err := h.repo.Create(c.Request.Context(), s, h.nextRun(req.CronExpr, enabled), orgID)
		if err != nil {
			serverError(c, err, "failed to create schedule")
			return
		}
		h.audit.write(c, "schedule.create", "schedule", saved.ID,
			map[string]interface{}{"name": saved.Name, "cron_expr": saved.CronExpr})
		c.JSON(http.StatusCreated, saved)
	}
}

// GetSchedule returns a single schedule.
func (h *ScheduleHandlers) GetSchedule() gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, resolved := tenantscope.FromContext(c)
		if !resolved {
			serverError(c, errNoTenantScope, "the tenant scope was not resolved for this route")
			return
		}
		// A schedule in another organization is reported as a schedule that does
		// not exist, which is the same answer UpdateSchedule and DeleteSchedule
		// already give. Anything else lets a caller enumerate ids.
		s, err := h.repo.GetByIDInScope(c.Request.Context(), c.Param("id"), scope)
		if err != nil {
			serverError(c, err, "failed to load schedule")
			return
		}
		if s == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "schedule not found"})
			return
		}
		c.JSON(http.StatusOK, s)
	}
}

// UpdateSchedule replaces a schedule and recomputes its next run.
// @Summary      Update schedule
// @Tags         Schedules
// @Accept       json
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /schedules/{id} [put]
func (h *ScheduleHandlers) UpdateSchedule() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req scheduleRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name and cron_expr are required"})
			return
		}
		if err := req.validate(); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		enabled := req.Enabled == nil || *req.Enabled
		s := &repositories.Schedule{
			Name: req.Name, CronExpr: req.CronExpr, TargetType: req.TargetType,
			TargetConfig: req.TargetConfig, Enabled: enabled,
		}
		scope, resolved := tenantscope.FromContext(c)
		if !resolved {
			serverError(c, errNoTenantScope, "the tenant scope was not resolved for this route")
			return
		}
		// THE WRITE-SIDE LINKAGE INVARIANT (#393), against the SCHEDULE'S
		// organization rather than the caller's: a multi-organization caller
		// may reach both sides, but the background runner will later fire this
		// row under its own organization alone. The row is loaded in the
		// caller's scope first, so a schedule elsewhere stays a plain 404.
		existing, err := h.repo.GetByIDInScope(c.Request.Context(), c.Param("id"), scope)
		if err != nil {
			serverError(c, err, "failed to load schedule")
			return
		}
		if existing == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "schedule not found"})
			return
		}
		if !h.targetReferencesInOrganization(c, req.TargetConfig, existing.OrganizationID) {
			return
		}
		// Scoped: a schedule names a dispatch target, so an unscoped update lets
		// one tenant repoint another tenant's schedule — and the background
		// runner then executes it on their cron, under their organization.
		updated, err := h.repo.UpdateInScope(c.Request.Context(), c.Param("id"), s, h.nextRun(req.CronExpr, enabled), scope)
		if errors.Is(err, repositories.ErrNotInScope) {
			c.JSON(http.StatusNotFound, gin.H{"error": "schedule not found"})
			return
		}
		if err != nil {
			serverError(c, err, "failed to update schedule")
			return
		}
		if updated == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "schedule not found"})
			return
		}
		h.audit.write(c, "schedule.update", "schedule", updated.ID,
			map[string]interface{}{"name": updated.Name, "cron_expr": updated.CronExpr})
		c.JSON(http.StatusOK, updated)
	}
}

// DeleteSchedule removes a schedule.
func (h *ScheduleHandlers) DeleteSchedule() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		scope, resolved := tenantscope.FromContext(c)
		if !resolved {
			serverError(c, errNoTenantScope, "the tenant scope was not resolved for this route")
			return
		}
		deleted, err := h.repo.DeleteInScope(c.Request.Context(), id, scope)
		if errors.Is(err, repositories.ErrNotInScope) || (err == nil && !deleted) {
			c.JSON(http.StatusNotFound, gin.H{"error": "schedule not found"})
			return
		}
		if err != nil {
			serverError(c, err, "failed to delete schedule")
			return
		}
		h.audit.write(c, "schedule.delete", "schedule", id, nil)
		c.Status(http.StatusNoContent)
	}
}

// RunSchedule fires a schedule immediately (a manual trigger) and records the
// outcome, leaving the regular cadence intact.
// @Summary      Run schedule now
// @Tags         Schedules
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /schedules/{id}/run [post]
func (h *ScheduleHandlers) RunSchedule() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		// SCOPED, and this is the read that mattered most on this root. The load
		// below decides which target_config gets dispatched, so an unscoped one
		// let a caller in one organization fire another organization's schedule
		// -- on that organization's pipeline connection, decrypting its token to
		// do it. Whether the caller may run a schedule at all is the route
		// guard's question; WHICH schedules exist for them is this one's.
		scope, resolved := tenantscope.FromContext(c)
		if !resolved {
			serverError(c, errNoTenantScope, "the tenant scope was not resolved for this route")
			return
		}
		s, err := h.repo.GetByIDInScope(ctx, c.Param("id"), scope)
		if err != nil {
			serverError(c, err, "failed to load schedule")
			return
		}
		if s == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "schedule not found"})
			return
		}
		// THE SCHEDULE'S ORGANIZATION, NOT THE CALLER'S. The run is a firing of
		// this schedule, so it belongs where the schedule does — a caller who
		// may trigger it does not thereby own its output. Whether they may
		// trigger it at all is the route guard's question, not this one.
		//
		// The derivation is the SAME one the background runner makes (#393
		// option B): "system, acting in the schedule's organization", traceable
		// to this row. It refuses an unstamped schedule rather than defaulting:
		// a run with no organization is invisible to every tenant, including
		// the one whose schedule produced it (#436).
		sysScope, sErr := tenancy.SystemActingIn(s.OrganizationID, "schedules", s.ID)
		if sErr != nil {
			serverError(c, sErr, "the schedule has no owning organization")
			return
		}
		runID, status, dErr := h.dispatcher.Dispatch(ctx, s.TargetType, s.TargetConfig, userIDOf(c), sysScope)
		h.audit.write(c, "schedule.run", "schedule", s.ID,
			map[string]interface{}{"name": s.Name, "status": status})
		var runPtr *string
		if runID != "" {
			runPtr = &runID
		}
		_ = h.repo.RecordRun(ctx, s.ID, status, runPtr, time.Now(), h.nextRun(s.CronExpr, s.Enabled))
		if dErr != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": dErr.Error(), "status": status})
			return
		}
		updated, _ := h.repo.GetByIDInScope(ctx, s.ID, scope)
		c.JSON(http.StatusOK, updated)
	}
}

// targetReferencesInOrganization enforces the write-side linkage invariant for
// schedules (#393): every row the target references -- the pipeline connection
// it fires, and the state source it aims at when one is named -- must belong to
// organizationID. Writes the refusal itself and reports whether the caller may
// proceed.
//
// The refusals do not say whether an id exists elsewhere: "does not name X in
// this schedule's organization" covers a typo and a crossing reference in the
// same words, deliberately.
func (h *ScheduleHandlers) targetReferencesInOrganization(c *gin.Context, targetConfig json.RawMessage, organizationID string) bool {
	var t DriftTarget
	if len(targetConfig) > 0 {
		// validate() has already parsed this; a failure here is defensive.
		if err := json.Unmarshal(targetConfig, &t); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid target_config"})
			return false
		}
	}
	orgScope := organizationScope(organizationID)
	if t.PipelineConnectionID != "" {
		conn, err := h.pipelines.GetByIDInScope(c.Request.Context(), t.PipelineConnectionID, orgScope)
		if err != nil {
			serverError(c, err, "failed to verify the schedule's pipeline connection")
			return false
		}
		if conn == nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "target_config.pipeline_connection_id does not name a pipeline connection in this schedule's organization",
			})
			return false
		}
	}
	if t.SourceID != "" {
		src, err := h.sources.GetByIDInScope(c.Request.Context(), t.SourceID, orgScope)
		if err != nil {
			serverError(c, err, "failed to verify the schedule's target source")
			return false
		}
		if src == nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "target_config.source_id does not name a state source in this schedule's organization",
			})
			return false
		}
	}
	return true
}

// nextRun returns the next fire time for an enabled schedule, or nil when disabled
// (a disabled schedule has no scheduled next run).
func (h *ScheduleHandlers) nextRun(cronExpr string, enabled bool) *time.Time {
	if !enabled {
		return nil
	}
	return scheduler.ComputeNextRun(cronExpr, time.Now())
}
