// audit.go provides the shared, nil-safe audit hook used by every feature
// handler. Entries land in the identity schema's audit_logs (the same trail the
// admin Audit Log page reads), attributed to the acting user and client IP.
package api

import (
	"database/sql"
	"strings"

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

// writeForOrg attributes the entry to an organization OTHER than the acting
// one, so a tenant can see something that happened TO it.
//
// The case this exists for is a state transfer, which spans two organizations
// while the transfer row records only one -- the organization the caller
// declared they were acting as. Without this, a transfer out of organization B
// performed by a caller acting as A is visible to A and INVISIBLE TO B: B's
// state was read and copied elsewhere and B has no record of it. Recording the
// counterparty on the row itself was the alternative and was rejected as
// heavier than the problem; the counterparty needs to KNOW, not to own.
func (a auditor) writeForOrg(c *gin.Context, orgID, action, resourceType, resourceID string, metadata map[string]interface{}) {
	if a.repo == nil || strings.TrimSpace(orgID) == "" {
		return
	}
	writeAuditEntryOrg(c, a.repo, action, resourceType, resourceID, orgID, metadata)
}
