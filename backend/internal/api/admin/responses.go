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

// MessageResponse is returned by action endpoints that confirm success with a plain message.
type MessageResponse struct {
	Message string `json:"message"`
}

// APIKeyItem represents a single API key in list/get responses (secret omitted).
type APIKeyItem struct {
	ID          string     `json:"id"`
	UserID      *string    `json:"user_id"`
	UserName    *string    `json:"user_name"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	KeyPrefix   string     `json:"key_prefix"`
	Scopes      []string   `json:"scopes"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// ListAPIKeysResponse is returned by GET /api/v1/apikeys.
type ListAPIKeysResponse struct {
	Keys []APIKeyItem `json:"keys"`
}

// APIKeyResponse wraps a single API key for get/update responses.
type APIKeyResponse struct {
	Key APIKeyItem `json:"key"`
}
