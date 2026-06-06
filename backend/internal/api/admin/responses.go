package admin

import "time"

// PaginationMeta carries page / per_page / total counts used in paginated list responses.
type PaginationMeta struct {
	Page    int   `json:"page"`
	PerPage int   `json:"per_page"`
	Total   int64 `json:"total,omitempty"`
}

// AuditLogResponse represents a single audit log entry in list or get responses.
type AuditLogResponse struct {
	ID             string                 `json:"id"`
	UserID         *string                `json:"user_id"`
	UserEmail      *string                `json:"user_email"`
	UserName       *string                `json:"user_name"`
	OrganizationID *string                `json:"organization_id"`
	Action         string                 `json:"action"`
	ResourceType   *string                `json:"resource_type"`
	ResourceID     *string                `json:"resource_id"`
	Metadata       map[string]interface{} `json:"metadata"`
	IPAddress      *string                `json:"ip_address"`
	CreatedAt      time.Time              `json:"created_at"`
}

// AuditLogListResponse is returned by GET /api/v1/admin/audit-logs.
type AuditLogListResponse struct {
	Logs       []AuditLogResponse `json:"logs"`
	Pagination PaginationMeta     `json:"pagination"`
}
