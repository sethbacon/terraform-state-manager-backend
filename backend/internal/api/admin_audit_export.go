package api

import (
	"bytes"
	"encoding/csv"
	"net/http"

	"github.com/gin-gonic/gin"
	idmodels "github.com/sethbacon/terraform-suite-identity/identity/models"
)

// Export paging/backstop knobs. Vars (not consts) so tests can shrink them;
// the cap bounds memory on a compliance extract without paginating the client.
var (
	exportAuditPageSize = 200 // the repository-honoured maximum page size
	exportAuditMaxRows  = 10000
)

// ExportAuditLogs streams the full filtered audit trail as a CSV or JSON
// attachment. The paginated ListAuditLogs endpoint caps pages at 200 rows;
// this walks every page server-side so compliance extracts are complete
// rather than silently truncated at one page. Collection stops at
// exportAuditMaxRows, flagged via the X-Truncated response header.
// @Summary      Export audit log
// @Description  Full filtered audit trail as a file attachment. Capped at 10000 rows (X-Truncated: true when hit). Requires admin.
// @Tags         Admin
// @Produce      text/csv,json
// @Param        format         query  string  false  "Export format: csv or json (default csv)"  Enums(csv, json)
// @Param        action         query  string  false  "Filter by action"
// @Param        resource_type  query  string  false  "Filter by resource type"
// @Param        user_email     query  string  false  "Filter by user email (partial)"
// @Param        start_date     query  string  false  "RFC3339 lower bound"
// @Param        end_date       query  string  false  "RFC3339 upper bound"
// @Success      200  {string}  string  "audit-log file"
// @Failure      400  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/audit-logs/export [get]
func (h *AdminHandlers) ExportAuditLogs() gin.HandlerFunc {
	return func(c *gin.Context) {
		format := c.DefaultQuery("format", "csv")
		if format != "csv" && format != "json" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "format must be csv or json"})
			return
		}
		filters := auditFiltersFromQuery(c)

		var collected []*idmodels.AuditLog
		truncated := false
		for offset := 0; ; offset += exportAuditPageSize {
			logs, total, err := h.auditRepo.ListAuditLogs(c.Request.Context(), filters, exportAuditPageSize, offset)
			if err != nil {
				serverError(c, err, "failed to export audit logs")
				return
			}
			collected = append(collected, logs...)
			if len(collected) >= exportAuditMaxRows {
				truncated = len(collected) < total
				collected = collected[:exportAuditMaxRows]
				break
			}
			if len(collected) >= total || len(logs) < exportAuditPageSize {
				break
			}
		}

		h.writeAudit(c, "audit.export", "audit", "",
			map[string]interface{}{"format": format, "rows": len(collected), "truncated": truncated})
		if truncated {
			c.Header("X-Truncated", "true")
		}
		if format == "json" {
			c.Header("Content-Disposition", `attachment; filename="audit-logs.json"`)
			c.JSON(http.StatusOK, gin.H{"logs": auditLogsJSON(collected), "truncated": truncated})
			return
		}
		c.Header("Content-Disposition", `attachment; filename="audit-logs.csv"`)
		c.Data(http.StatusOK, "text/csv; charset=utf-8", auditLogsCSV(collected))
	}
}

// auditLogsCSV renders the same columns the admin UI's table shows, RFC 4180
// quoted via encoding/csv.
func auditLogsCSV(logs []*idmodels.AuditLog) []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"created_at", "action", "resource_type", "resource_id", "user_email", "user_name", "ip_address"})
	deref := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	for _, l := range logs {
		_ = w.Write([]string{
			l.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			l.Action,
			deref(l.ResourceType),
			deref(l.ResourceID),
			deref(l.UserEmail),
			deref(l.UserName),
			deref(l.IPAddress),
		})
	}
	w.Flush()
	return buf.Bytes()
}
