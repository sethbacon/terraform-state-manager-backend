// Package auth defines permission scope constants for all state manager resources
// and provides HasScope, HasAnyScope, and HasAllScopes helper functions for scope checking.
package auth

import (
	"errors"
	"fmt"
)

// Scope represents a permission/scope type
type Scope string

const (
	// Analysis scopes
	ScopeAnalysisRead  Scope = "analysis:read"
	ScopeAnalysisWrite Scope = "analysis:write"

	// State source scopes
	ScopeSourcesRead  Scope = "sources:read"
	ScopeSourcesWrite Scope = "sources:write"

	// Backup scopes
	ScopeBackupsRead  Scope = "backups:read"
	ScopeBackupsWrite Scope = "backups:write"

	// Migration scopes
	ScopeMigrationsRead  Scope = "migrations:read"
	ScopeMigrationsWrite Scope = "migrations:write"

	// Report scopes
	ScopeReportsRead  Scope = "reports:read"
	ScopeReportsWrite Scope = "reports:write"

	// Dashboard scopes
	ScopeDashboardRead  Scope = "dashboard:read"
	ScopeDashboardWrite Scope = "dashboard:write"

	// Compliance scopes
	ScopeComplianceRead  Scope = "compliance:read"
	ScopeComplianceWrite Scope = "compliance:write"

	// Scheduler scopes
	ScopeSchedulerAdmin Scope = "scheduler:admin"

	// Alert scopes
	ScopeAlertsAdmin Scope = "alerts:admin"

	// User management scopes
	ScopeUsersRead  Scope = "users:read"
	ScopeUsersWrite Scope = "users:write"

	// Organization management scopes
	ScopeOrganizationsRead  Scope = "organizations:read"
	ScopeOrganizationsWrite Scope = "organizations:write"

	// API key management scopes
	ScopeAPIKeysManage Scope = "api_keys:manage"

	// Audit log scopes
	ScopeAuditRead Scope = "audit:read"

	// Admin scope (wildcard - all permissions)
	ScopeAdmin Scope = "admin"
)

// readWritePairs maps read scopes to their corresponding write/manage scopes.
// If a user has a write scope, they implicitly have the corresponding read scope.
var readWritePairs = map[Scope]Scope{
	ScopeAnalysisRead:       ScopeAnalysisWrite,
	ScopeSourcesRead:        ScopeSourcesWrite,
	ScopeBackupsRead:        ScopeBackupsWrite,
	ScopeMigrationsRead:     ScopeMigrationsWrite,
	ScopeReportsRead:        ScopeReportsWrite,
	ScopeDashboardRead:      ScopeDashboardWrite,
	ScopeComplianceRead:     ScopeComplianceWrite,
	ScopeUsersRead:          ScopeUsersWrite,
	ScopeOrganizationsRead:  ScopeOrganizationsWrite,
}

// AllScopes returns all valid scopes
func AllScopes() []Scope {
	return []Scope{
		ScopeAnalysisRead,
		ScopeAnalysisWrite,
		ScopeSourcesRead,
		ScopeSourcesWrite,
		ScopeBackupsRead,
		ScopeBackupsWrite,
		ScopeMigrationsRead,
		ScopeMigrationsWrite,
		ScopeReportsRead,
		ScopeReportsWrite,
		ScopeDashboardRead,
		ScopeDashboardWrite,
		ScopeComplianceRead,
		ScopeComplianceWrite,
		ScopeSchedulerAdmin,
		ScopeAlertsAdmin,
		ScopeUsersRead,
		ScopeUsersWrite,
		ScopeOrganizationsRead,
		ScopeOrganizationsWrite,
		ScopeAPIKeysManage,
		ScopeAuditRead,
		ScopeAdmin,
	}
}

// ValidScopes returns a map of valid scope strings
func ValidScopes() map[string]bool {
	validScopes := make(map[string]bool)
	for _, scope := range AllScopes() {
		validScopes[string(scope)] = true
	}
	return validScopes
}

// ValidateScopes checks if all provided scopes are valid
func ValidateScopes(scopes []string) error {
	validScopes := ValidScopes()

	for _, scope := range scopes {
		if !validScopes[scope] {
			return fmt.Errorf("invalid scope: %s", scope)
		}
	}

	return nil
}

// HasScope checks if a user has a required scope.
// Supports wildcard admin scope and write-implies-read logic.
func HasScope(userScopes []string, required Scope) bool {
	requiredStr := string(required)

	for _, scope := range userScopes {
		if scope == requiredStr {
			return true
		}

		if scope == string(ScopeAdmin) {
			return true
		}

		// Check write-implies-read
		if writeScope, ok := readWritePairs[required]; ok {
			if scope == string(writeScope) {
				return true
			}
		}
	}

	return false
}

// HasAnyScope checks if a user has at least one of the required scopes
func HasAnyScope(userScopes []string, requiredScopes []Scope) bool {
	for _, required := range requiredScopes {
		if HasScope(userScopes, required) {
			return true
		}
	}
	return false
}

// HasAllScopes checks if a user has all of the required scopes
func HasAllScopes(userScopes []string, requiredScopes []Scope) bool {
	for _, required := range requiredScopes {
		if !HasScope(userScopes, required) {
			return false
		}
	}
	return true
}

// GetDefaultScopes returns default scopes for a new API key
func GetDefaultScopes() []string {
	return []string{
		string(ScopeAnalysisRead),
		string(ScopeSourcesRead),
		string(ScopeDashboardRead),
	}
}

// GetAdminScopes returns all scopes including admin
func GetAdminScopes() []string {
	scopes := make([]string, 0)
	for _, scope := range AllScopes() {
		scopes = append(scopes, string(scope))
	}
	return scopes
}

// ValidateScopeString validates a single scope string
func ValidateScopeString(scope string) error {
	validScopes := ValidScopes()
	if !validScopes[scope] {
		return errors.New("invalid scope")
	}
	return nil
}
