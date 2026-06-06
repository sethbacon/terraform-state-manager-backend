// audit_logs.go implements handlers for retrieving audit log entries with
// pagination and filtering. Mirrors the registry's identity surface 1:1.
package admin

import (
	"database/sql"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// AuditLogHandlers handles audit log read endpoints.
type AuditLogHandlers struct {
	db        *sql.DB
	auditRepo *repositories.AuditRepository
}

// NewAuditLogHandlers creates a new AuditLogHandlers instance.
func NewAuditLogHandlers(db *sql.DB) *AuditLogHandlers {
	return &AuditLogHandlers{
		db:        db,
		auditRepo: repositories.NewAuditRepository(db),
	}
}

// ListAuditLogsHandler returns paginated, filtered audit log entries.
// @Summary      List audit logs
// @Description  Get a paginated, filterable list of audit log entries. Requires audit:read scope.
// @Tags         Audit
// @Accept       json
// @Produce      json
// @Param        page           query  int     false  "Page number (default 1)"
// @Param        per_page       query  int     false  "Items per page, max 200 (default 25)"
// @Param        action         query  string  false  "Filter by action string (exact match)"
// @Param        resource_type  query  string  false  "Filter by resource type"
// @Param        user_id        query  string  false  "Filter by actor user ID (exact match)"
// @Param        user_email     query  string  false  "Filter by actor email (partial, case-insensitive)"
// @Param        start_date     query  string  false  "Filter entries at or after this RFC3339 timestamp"
// @Param        end_date       query  string  false  "Filter entries at or before this RFC3339 timestamp"
// @Success      200  {object}  admin.AuditLogListResponse
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      403  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /admin/audit-logs [get]
func (h *AuditLogHandlers) ListAuditLogsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
		perPage, _ := strconv.Atoi(c.DefaultQuery("per_page", "25"))
		if page < 1 {
			page = 1
		}
		if perPage < 1 || perPage > 200 {
			perPage = 25
		}
		offset := (page - 1) * perPage

		filters := repositories.AuditFilters{}
		if v := c.Query("action"); v != "" {
			filters.Action = &v
		}
		if v := c.Query("resource_type"); v != "" {
			filters.ResourceType = &v
		}
		if v := c.Query("user_id"); v != "" {
			filters.UserID = &v
		}
		if v := c.Query("user_email"); v != "" {
			filters.UserEmail = &v
		}
		if v := c.Query("start_date"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "start_date must be an RFC3339 timestamp (e.g. 2006-01-02T15:04:05Z)"})
				return
			}
			filters.StartDate = &t
		}
		if v := c.Query("end_date"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "end_date must be an RFC3339 timestamp (e.g. 2006-01-02T15:04:05Z)"})
				return
			}
			filters.EndDate = &t
		}

		logs, total, err := h.auditRepo.ListAuditLogs(c.Request.Context(), filters, perPage, offset)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve audit logs"})
			return
		}

		items := make([]AuditLogResponse, 0, len(logs))
		for _, l := range logs {
			items = append(items, AuditLogResponse{
				ID:             l.ID,
				UserID:         l.UserID,
				UserEmail:      l.UserEmail,
				UserName:       l.UserName,
				OrganizationID: l.OrganizationID,
				Action:         l.Action,
				ResourceType:   l.ResourceType,
				ResourceID:     l.ResourceID,
				Metadata:       l.Metadata,
				IPAddress:      l.IPAddress,
				CreatedAt:      l.CreatedAt,
			})
		}

		c.JSON(http.StatusOK, AuditLogListResponse{
			Logs: items,
			Pagination: PaginationMeta{
				Page:    page,
				PerPage: perPage,
				Total:   int64(total),
			},
		})
	}
}

// GetAuditLogHandler returns a single audit log entry by ID.
// @Summary      Get audit log entry
// @Description  Retrieve a single audit log entry by ID. Requires audit:read scope.
// @Tags         Audit
// @Produce      json
// @Param        id  path  string  true  "Audit log entry ID"
// @Success      200  {object}  admin.AuditLogResponse
// @Failure      401  {object}  map[string]interface{}
// @Failure      403  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /admin/audit-logs/{id} [get]
func (h *AuditLogHandlers) GetAuditLogHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		logID := c.Param("id")

		log, err := h.auditRepo.GetAuditLog(c.Request.Context(), logID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve audit log entry"})
			return
		}
		if log == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Audit log entry not found"})
			return
		}

		c.JSON(http.StatusOK, AuditLogResponse{
			ID:             log.ID,
			UserID:         log.UserID,
			UserEmail:      log.UserEmail,
			UserName:       log.UserName,
			OrganizationID: log.OrganizationID,
			Action:         log.Action,
			ResourceType:   log.ResourceType,
			ResourceID:     log.ResourceID,
			Metadata:       log.Metadata,
			IPAddress:      log.IPAddress,
			CreatedAt:      log.CreatedAt,
		})
	}
}
