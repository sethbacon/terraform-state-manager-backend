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

// UserItem is the shape of a user in list/get/create/update responses.
type UserItem struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ListUsersResponse is returned by GET /api/v1/users and /users/search.
type ListUsersResponse struct {
	Users      []UserItem     `json:"users"`
	Pagination PaginationMeta `json:"pagination"`
}

// UserWithOrgsResponse is returned by GET /api/v1/users/{id}.
type UserWithOrgsResponse struct {
	User          UserItem    `json:"user"`
	Organizations interface{} `json:"organizations"`
}

// UserResponse is returned by POST /api/v1/users and PUT /api/v1/users/{id}.
type UserResponse struct {
	User UserItem `json:"user"`
}

// UserMembershipsResponse is returned by the membership endpoints.
type UserMembershipsResponse struct {
	Memberships interface{} `json:"memberships"`
}

// ListOrganizationsResponse is returned by GET /api/v1/organizations and /organizations/search.
type ListOrganizationsResponse struct {
	Organizations interface{}    `json:"organizations"`
	Pagination    PaginationMeta `json:"pagination"`
}

// OrganizationWithMembersResponse is returned by GET /api/v1/organizations/{id}.
type OrganizationWithMembersResponse struct {
	Organization interface{} `json:"organization"`
	Members      interface{} `json:"members"`
}

// OrganizationMembersResponse is returned by GET /api/v1/organizations/{id}/members.
type OrganizationMembersResponse struct {
	Members interface{} `json:"members"`
}

// OrganizationResponse is returned by POST and PUT /api/v1/organizations.
type OrganizationResponse struct {
	Organization interface{} `json:"organization"`
}

// MemberResponse is returned by the member endpoints.
type MemberResponse struct {
	Member interface{} `json:"member"`
}
