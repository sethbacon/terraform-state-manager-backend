// schedules.go implements CRUD for cron-driven schedules plus the adapter that
// lets the background scheduler dispatch a schedule's target (a drift run). The
// runner lives in internal/services/scheduler; this file provides the HTTP surface
// and the api-side Dispatcher implementation.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/scheduler"
)

// driftDispatcher adapts DriftHandlers to scheduler.Dispatcher so a schedule with
// target_type "drift" fires a drift run on its configured pipeline.
type driftDispatcher struct{ drift *DriftHandlers }

func (d driftDispatcher) Dispatch(ctx context.Context, targetType string, targetConfig json.RawMessage, actor string) (runID, status string, err error) {
	switch targetType {
	case "drift":
		var t DriftTarget
		if uErr := json.Unmarshal(targetConfig, &t); uErr != nil {
			return "", "failed", fmt.Errorf("invalid drift target config: %w", uErr)
		}
		run, dErr := d.drift.dispatchDrift(ctx, t, actor)
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
}

// NewScheduleHandlers builds the handlers over the app connection. dispatcher is
// used by the "run now" endpoint and is the same adapter the background runner
// uses; identityDB (may be nil) carries the shared audit log.
func NewScheduleHandlers(database, identityDB *sql.DB, dispatcher scheduler.Dispatcher) *ScheduleHandlers {
	return &ScheduleHandlers{
		repo:       repositories.NewScheduleRepository(database),
		dispatcher: dispatcher,
		audit:      newAuditor(identityDB),
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
		items, err := h.repo.List(c.Request.Context())
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
		saved, err := h.repo.Create(c.Request.Context(), s, h.nextRun(req.CronExpr, enabled))
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
		s, err := h.repo.GetByID(c.Request.Context(), c.Param("id"))
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
		updated, err := h.repo.Update(c.Request.Context(), c.Param("id"), s, h.nextRun(req.CronExpr, enabled))
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
		if err := h.repo.Delete(c.Request.Context(), id); err != nil {
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
		s, err := h.repo.GetByID(ctx, c.Param("id"))
		if err != nil {
			serverError(c, err, "failed to load schedule")
			return
		}
		if s == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "schedule not found"})
			return
		}
		runID, status, dErr := h.dispatcher.Dispatch(ctx, s.TargetType, s.TargetConfig, userIDOf(c))
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
		updated, _ := h.repo.GetByID(ctx, s.ID)
		c.JSON(http.StatusOK, updated)
	}
}

// nextRun returns the next fire time for an enabled schedule, or nil when disabled
// (a disabled schedule has no scheduled next run).
func (h *ScheduleHandlers) nextRun(cronExpr string, enabled bool) *time.Time {
	if !enabled {
		return nil
	}
	return scheduler.ComputeNextRun(cronExpr, time.Now())
}
