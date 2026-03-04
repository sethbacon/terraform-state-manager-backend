// Package scheduler implements the HTTP handlers for managing scheduled tasks:
// creating, listing, updating, deleting, and manually triggering task execution.
package scheduler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	schedulerSvc "github.com/terraform-state-manager/terraform-state-manager/internal/services/scheduler"
)

// Handlers provides the HTTP handlers for scheduled task API endpoints.
type Handlers struct {
	taskRepo  *repositories.ScheduledTaskRepository
	scheduler *schedulerSvc.Scheduler
}

// NewHandlers creates a new Handlers instance.
func NewHandlers(
	taskRepo *repositories.ScheduledTaskRepository,
	scheduler *schedulerSvc.Scheduler,
) *Handlers {
	return &Handlers{
		taskRepo:  taskRepo,
		scheduler: scheduler,
	}
}

// ---------------------------------------------------------------------------
// Handler methods
// ---------------------------------------------------------------------------

// CreateTask handles POST /api/v1/scheduler/tasks.
// Creates a new scheduled task for the authenticated user's organization.
func (h *Handlers) CreateTask(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	userID, _ := c.Get("user_id")
	userIDStr, _ := userID.(string)

	var req models.ScheduledTaskCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid input",
			"details": err.Error(),
		})
		return
	}

	// Validate task type.
	if !models.ValidTaskTypes()[req.TaskType] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task_type"})
		return
	}

	taskConfig, _ := json.Marshal(map[string]interface{}{})
	if req.Config != nil {
		taskConfig = *req.Config
	}

	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}

	// Compute the initial next_run_at based on the schedule.
	now := time.Now()
	nextRunAt := computeNextRunFromSchedule(req.Schedule, now)

	task := &models.ScheduledTask{
		OrganizationID: orgIDStr,
		Name:           req.Name,
		TaskType:       req.TaskType,
		Schedule:       req.Schedule,
		Config:         taskConfig,
		IsActive:       isActive,
		NextRunAt:      nextRunAt,
		CreatedBy:      nilIfEmpty(userIDStr),
	}

	ctx := c.Request.Context()
	if err := h.taskRepo.Create(ctx, task); err != nil {
		slog.Error("Failed to create scheduled task", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create scheduled task"})
		return
	}

	slog.Info("Scheduled task created",
		"task_id", task.ID, "name", task.Name, "type", task.TaskType, "schedule", task.Schedule)

	c.JSON(http.StatusCreated, gin.H{"data": task})
}

// ListTasks handles GET /api/v1/scheduler/tasks.
// Returns a paginated list of scheduled tasks for the organization.
func (h *Handlers) ListTasks(c *gin.Context) {
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
	tasks, total, err := h.taskRepo.ListByOrganization(ctx, orgIDStr, limit, offset)
	if err != nil {
		slog.Error("Failed to list scheduled tasks", "error", err, "org_id", orgIDStr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list scheduled tasks"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data":   tasks,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// GetTask handles GET /api/v1/scheduler/tasks/:id.
// Returns a single scheduled task by ID.
func (h *Handlers) GetTask(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task ID"})
		return
	}

	ctx := c.Request.Context()
	task, err := h.taskRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get scheduled task", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve scheduled task"})
		return
	}
	if task == nil || task.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "scheduled task not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": task})
}

// UpdateTask handles PUT /api/v1/scheduler/tasks/:id.
// Applies partial updates to a scheduled task.
func (h *Handlers) UpdateTask(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task ID"})
		return
	}

	var req models.ScheduledTaskUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid input",
			"details": err.Error(),
		})
		return
	}

	ctx := c.Request.Context()
	task, err := h.taskRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get scheduled task for update", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve scheduled task"})
		return
	}
	if task == nil || task.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "scheduled task not found"})
		return
	}

	// Apply partial updates.
	if req.Name != nil {
		task.Name = *req.Name
	}
	if req.TaskType != nil {
		if !models.ValidTaskTypes()[*req.TaskType] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task_type"})
			return
		}
		task.TaskType = *req.TaskType
	}
	if req.Schedule != nil {
		task.Schedule = *req.Schedule
		// Recompute next run when schedule changes.
		task.NextRunAt = computeNextRunFromSchedule(*req.Schedule, time.Now())
	}
	if req.Config != nil {
		task.Config = *req.Config
	}
	if req.IsActive != nil {
		task.IsActive = *req.IsActive
	}

	if err := h.taskRepo.Update(ctx, task); err != nil {
		slog.Error("Failed to update scheduled task", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update scheduled task"})
		return
	}

	slog.Info("Scheduled task updated", "task_id", id)
	c.JSON(http.StatusOK, gin.H{"data": task})
}

// DeleteTask handles DELETE /api/v1/scheduler/tasks/:id.
func (h *Handlers) DeleteTask(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task ID"})
		return
	}

	ctx := c.Request.Context()
	task, err := h.taskRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get scheduled task for deletion", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve scheduled task"})
		return
	}
	if task == nil || task.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "scheduled task not found"})
		return
	}

	if err := h.taskRepo.Delete(ctx, id); err != nil {
		slog.Error("Failed to delete scheduled task", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete scheduled task"})
		return
	}

	slog.Info("Scheduled task deleted", "task_id", id, "name", task.Name)
	c.JSON(http.StatusOK, gin.H{
		"message": "scheduled task deleted successfully",
	})
}

// TriggerTask handles POST /api/v1/scheduler/tasks/:id/trigger.
// Manually triggers immediate execution of a scheduled task by setting its
// next_run_at to now so the scheduler picks it up on its next tick.
func (h *Handlers) TriggerTask(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task ID"})
		return
	}

	ctx := c.Request.Context()
	task, err := h.taskRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get scheduled task for trigger", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve scheduled task"})
		return
	}
	if task == nil || task.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "scheduled task not found"})
		return
	}

	if !task.IsActive {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot trigger an inactive task"})
		return
	}

	// Set next_run_at to now so the scheduler executes it on the next tick.
	now := time.Now()
	task.NextRunAt = &now

	if err := h.taskRepo.Update(ctx, task); err != nil {
		slog.Error("Failed to trigger scheduled task", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to trigger scheduled task"})
		return
	}

	slog.Info("Scheduled task manually triggered", "task_id", id, "name", task.Name)
	c.JSON(http.StatusOK, gin.H{
		"data":    task,
		"message": "task triggered; it will execute on the next scheduler tick",
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// computeNextRunFromSchedule wraps the scheduler service's schedule parsing.
// It duplicates the logic here to avoid a circular dependency.
func computeNextRunFromSchedule(schedule string, from time.Time) *time.Time {
	var next time.Time

	switch {
	case schedule == "daily":
		next = from.Add(24 * time.Hour)
	case schedule == "weekly":
		next = from.Add(7 * 24 * time.Hour)
	case len(schedule) > 6 && schedule[:6] == "every ":
		durationStr := schedule[6:]
		d, err := time.ParseDuration(durationStr)
		if err != nil {
			return nil
		}
		if d < time.Minute {
			d = time.Minute
		}
		next = from.Add(d)
	default:
		return nil
	}

	return &next
}

// nilIfEmpty returns a pointer to s if non-empty, otherwise nil.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
