package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// AuditMiddleware returns a gin.HandlerFunc that creates an audit log entry
// after every mutating request that completes successfully. It runs after the
// main handler (c.Next() is called first), inspects the response status and
// HTTP method, and writes the audit record asynchronously to avoid adding
// latency to the response path.
//
// By default only successful (2xx) write operations (POST, PUT, PATCH, DELETE)
// are logged. OPTIONS requests are always skipped.
func AuditMiddleware(auditRepo *repositories.AuditRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Let the handler run first.
		c.Next()

		// Skip OPTIONS preflight requests.
		if c.Request.Method == http.MethodOptions {
			return
		}

		// Only log successful write operations by default.
		status := c.Writer.Status()
		if status < 200 || status >= 300 {
			return
		}

		method := c.Request.Method
		if method == http.MethodGet || method == http.MethodHead {
			return
		}

		path := c.Request.URL.Path
		action := deriveAction(method, path)
		ipAddress := c.ClientIP()
		resourceType := deriveResourceType(path)

		auditLog := &models.AuditLog{
			Action:    action,
			IPAddress: &ipAddress,
			CreatedAt: time.Now(),
			Metadata:  make(map[string]interface{}),
		}

		// Attach user ID if present.
		if userID, exists := c.Get("user_id"); exists {
			if uid, ok := userID.(string); ok && uid != "" {
				auditLog.UserID = &uid
			}
		}

		// Attach organization ID if present.
		if orgID, exists := c.Get("org_id"); exists {
			if oid, ok := orgID.(string); ok && oid != "" {
				auditLog.OrganizationID = &oid
			}
		}

		if resourceType != "" {
			auditLog.ResourceType = &resourceType
		}

		// Store useful metadata.
		auditLog.Metadata["method"] = method
		auditLog.Metadata["path"] = path
		auditLog.Metadata["status"] = status

		if authMethod, exists := c.Get("auth_method"); exists {
			auditLog.Metadata["auth_method"] = authMethod
		}
		if requestID, exists := c.Get(RequestIDKey); exists {
			auditLog.Metadata["request_id"] = requestID
		}

		// Async write to database.
		go func(entry *models.AuditLog) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			if err := auditRepo.CreateAuditLog(ctx, entry); err != nil {
				slog.Error("TSM audit: failed to create audit log",
					"action", entry.Action,
					"error", err,
				)
			}
		}(auditLog)
	}
}

// deriveAction builds a human-readable action string from the HTTP method
// and request path (e.g. "create_source", "delete_backup").
func deriveAction(method, path string) string {
	var verb string
	switch method {
	case http.MethodPost:
		verb = "create"
	case http.MethodPut, http.MethodPatch:
		verb = "update"
	case http.MethodDelete:
		verb = "delete"
	default:
		verb = strings.ToLower(method)
	}

	resource := deriveResourceType(path)
	if resource == "" {
		resource = "resource"
	}

	return verb + "_" + resource
}

// deriveResourceType inspects the URL path and returns a short resource type
// identifier for audit log categorization.
func deriveResourceType(path string) string {
	switch {
	case strings.Contains(path, "/analysis"):
		return "analysis"
	case strings.Contains(path, "/sources"):
		return "source"
	case strings.Contains(path, "/backups"):
		return "backup"
	case strings.Contains(path, "/migrations"):
		return "migration"
	case strings.Contains(path, "/users"):
		return "user"
	case strings.Contains(path, "/apikeys"):
		return "api_key"
	case strings.Contains(path, "/organizations"):
		return "organization"
	case strings.Contains(path, "/reports"):
		return "report"
	case strings.Contains(path, "/dashboard"):
		return "dashboard"
	default:
		return ""
	}
}
