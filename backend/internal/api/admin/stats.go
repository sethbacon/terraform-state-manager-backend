package admin

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

// StatsHandler provides HTTP handlers for dashboard statistics.
type StatsHandler struct {
	db *sqlx.DB
}

// NewStatsHandler creates a new StatsHandler instance.
func NewStatsHandler(db *sqlx.DB) *StatsHandler {
	return &StatsHandler{db: db}
}

// DashboardStats holds aggregate counts for the TSM admin dashboard.
type DashboardStats struct {
	Users         int64 `json:"users"`
	Organizations int64 `json:"organizations"`
	APIKeys       int64 `json:"api_keys"`
	AuditEvents   int64 `json:"audit_events"`
}

// GetDashboardStats handles the dashboard statistics endpoint. It counts rows
// across users, organizations, api_keys, and audit_logs tables in a single
// query. Missing tables gracefully fall back to zero.
// @Summary      Get admin dashboard statistics
// @Description  Returns aggregate counts for users, organizations, API keys, and audit events.
// @Tags         Admin
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /admin/stats/dashboard [get]
func (h *StatsHandler) GetDashboardStats(c *gin.Context) {
	var stats DashboardStats

	err := h.db.QueryRowxContext(c.Request.Context(), `
		SELECT
			COALESCE((SELECT COUNT(*) FROM users), 0) AS users,
			COALESCE((SELECT COUNT(*) FROM organizations), 0) AS organizations,
			COALESCE((SELECT COUNT(*) FROM api_keys), 0) AS api_keys,
			COALESCE((SELECT COUNT(*) FROM audit_logs), 0) AS audit_events
	`).StructScan(&stats)
	if err != nil {
		// If the query fails (e.g. a table does not exist yet), try
		// individual counts with graceful fallbacks.
		slog.Warn("dashboard stats aggregate query failed, falling back to individual counts", "error", err)
		stats = h.fallbackStats(c)
	}

	c.JSON(http.StatusOK, gin.H{"stats": stats})
}

// fallbackStats queries each table independently, returning zero for any
// table that does not exist or produces an error.
func (h *StatsHandler) fallbackStats(c *gin.Context) DashboardStats {
	var stats DashboardStats

	if err := h.db.QueryRowxContext(c.Request.Context(), "SELECT COUNT(*) FROM users").Scan(&stats.Users); err != nil {
		slog.Warn("failed to count users", "error", err)
		stats.Users = 0
	}

	if err := h.db.QueryRowxContext(c.Request.Context(), "SELECT COUNT(*) FROM organizations").Scan(&stats.Organizations); err != nil {
		slog.Warn("failed to count organizations", "error", err)
		stats.Organizations = 0
	}

	if err := h.db.QueryRowxContext(c.Request.Context(), "SELECT COUNT(*) FROM api_keys").Scan(&stats.APIKeys); err != nil {
		slog.Warn("failed to count api_keys", "error", err)
		stats.APIKeys = 0
	}

	if err := h.db.QueryRowxContext(c.Request.Context(), "SELECT COUNT(*) FROM audit_logs").Scan(&stats.AuditEvents); err != nil {
		slog.Warn("failed to count audit_logs", "error", err)
		stats.AuditEvents = 0
	}

	return stats
}
