// audit.go provides the shared, nil-safe audit hook used by every feature
// handler. Entries land in the identity schema's audit_logs (the same trail the
// admin Audit Log page reads), attributed to the acting user and client IP.
package api

import (
	"database/sql"

	"github.com/gin-gonic/gin"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
)

// auditor wraps the shared audit log with a nil-safe write so handlers
// constructed without an identity DB (unit tests) skip auditing.
type auditor struct {
	repo *idstore.AuditRepository
}

func newAuditor(identityDB *sql.DB) auditor {
	if identityDB == nil {
		return auditor{}
	}
	return auditor{repo: idstore.NewAuditRepository(identityDB)}
}

func (a auditor) write(c *gin.Context, action, resourceType, resourceID string, metadata map[string]interface{}) {
	if a.repo == nil {
		return
	}
	writeAuditEntry(c, a.repo, action, resourceType, resourceID, metadata)
}
