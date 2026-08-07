// audit_ingest.go implements the cross-app audit federation receiver. A sibling
// suite app (the registry) ships its audit entries here via its existing webhook
// shipper so they land in this app's shared identity audit trail — the same
// trail the admin Audit Log page reads. Federation is only coherent when both
// apps share one identity store (so the sibling's user/org IDs resolve here);
// the handler refuses otherwise, matching the audit.ingest.v1 manifest
// capability which is advertised only under sharedStore. An id that does not
// resolve even then is nulled before the insert rather than stamped — see
// resolveSiblingIDs, and note that identity v0.25.0 removed the foreign keys
// that used to make that failure detectable after the fact.
package api

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	idmodels "github.com/sethbacon/terraform-suite-identity/identity/models"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
)

// suiteSourceAppHeader optionally identifies which sibling app shipped the entry
// (recorded in metadata for human triage). The registry sets it alongside the
// service token in its webhook shipper config; absent is tolerated.
const suiteSourceAppHeader = "X-Suite-Source-App"

// maxAuditIngestBytes caps the /audit/ingest request body. A federated audit
// entry is small JSON, so 1 MiB is generous while bounding a malformed or hostile
// caller's memory use (#284, CWE-770).
const maxAuditIngestBytes = 1 << 20

// federatedAuditEntry is the wire shape a sibling app POSTs to /audit/ingest. It
// mirrors the registry's audit.LogEntry (its internal/audit/shipper.go) field
// for field so the registry's existing webhook shipper federates with no code
// change on its side.
type federatedAuditEntry struct {
	Timestamp      time.Time              `json:"timestamp"`
	Action         string                 `json:"action"`
	UserID         string                 `json:"user_id,omitempty"`
	OrganizationID string                 `json:"organization_id,omitempty"`
	ResourceType   string                 `json:"resource_type,omitempty"`
	ResourceID     string                 `json:"resource_id,omitempty"`
	IPAddress      string                 `json:"ip_address,omitempty"`
	AuthMethod     string                 `json:"auth_method,omitempty"`
	StatusCode     int                    `json:"status_code,omitempty"`
	Metadata       map[string]interface{} `json:"metadata,omitempty"`
}

// AuditIngestHandlers records federated audit entries from a sibling suite app.
type AuditIngestHandlers struct {
	auditRepo *idstore.AuditRepository
	// userRepo and orgRepo resolve the sibling's actor and organization ids
	// against this database before the entry is written. See resolveSiblingIDs.
	userRepo    *idstore.UserRepository
	orgRepo     *idstore.OrganizationRepository
	sharedStore bool
}

// NewAuditIngestHandlers wires the receiver. A nil identityDB (unit-test rigs)
// leaves the repos unset; the handler then reports the store unavailable.
func NewAuditIngestHandlers(identityDB *sql.DB, cfg *config.Config) *AuditIngestHandlers {
	h := &AuditIngestHandlers{sharedStore: cfg.Suite.IdentitySharedStore}
	if identityDB != nil {
		h.auditRepo = idstore.NewAuditRepository(identityDB)
		h.userRepo = idstore.NewUserRepository(identityDB)
		h.orgRepo = idstore.NewOrganizationRepository(identityDB)
	}
	return h
}

// Ingest records a sibling app's audit entry in the shared identity trail.
// @Summary      Ingest a federated audit entry from a sibling app
// @Tags         Suite
// @Accept       json
// @Produce      json
// @Param        X-Suite-Service-Token  header  string               true   "Shared suite service token (server-to-server)"
// @Param        X-Suite-Source-App     header  string               false  "Sibling app id, e.g. terraform-registry"
// @Param        entry                  body    federatedAuditEntry  true   "Audit entry (registry audit.LogEntry shape)"
// @Success      202  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      403  {object}  map[string]interface{}
// @Router       /audit/ingest [post]
func (h *AuditIngestHandlers) Ingest() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Federation is only coherent under a shared identity store: only then do
		// the sibling's user/org IDs resolve in this app's identity tables, and a
		// merged timeline that cannot resolve its actors mis-attributes them.
		// This mirrors the audit.ingest.v1 capability gate in the manifest.
		if !h.sharedStore {
			c.JSON(http.StatusForbidden, gin.H{"error": "audit federation requires a shared identity store"})
			return
		}
		if h.auditRepo == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "audit store unavailable"})
			return
		}

		var req federatedAuditEntry
		// A federated audit entry is small JSON; cap the body so a malformed or
		// hostile caller cannot force unbounded buffering (#284, CWE-770).
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAuditIngestBytes)
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid audit entry"})
			return
		}
		if req.Action == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "action is required"})
			return
		}

		ctx := c.Request.Context()
		entry := federatedAuditModel(&req, c.GetHeader(suiteSourceAppHeader))
		h.resolveSiblingIDs(ctx, entry, &req)
		if err := h.auditRepo.CreateAuditLog(ctx, entry); err != nil {
			slog.Warn("failed to ingest federated audit entry", "action", req.Action, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record audit entry"})
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"status": "recorded"})
	}
}

// resolveSiblingIDs decides, BEFORE the insert, whether the sibling's actor and
// organization ids name rows this database actually holds — nulling the ones
// that do not and preserving the originals in metadata.
//
// This replaces a catch-the-foreign-key-error-and-retry fallback, and it is not
// a like-for-like port: identity v0.25.0's migration 000007 DROPPED the foreign
// keys on audit_logs.user_id and audit_logs.organization_id (its #142), so the
// insert that used to fail now succeeds and the unresolvable id is stored as
// written. Deleting the fallback and doing nothing else was the wrong answer,
// because the fallback was itself an instance of the class 000007 closes: a row
// whose owning organization does not resolve is a row re-homed into a tenancy
// state that means something else.
//
// The two available decisions are not symmetric HERE, which is what settles it:
//
//   - Keep the stamped id. audit_logs.organization_id then names an
//     organization this deployment has no row for, so it matches no admin's
//     OrgScope and the entry is readable only through OrgScopeAllOrganizations()
//     — which, deliberately, no TSM handler holds. The entry would be written
//     and then visible to nobody: federated audit that silently disappears.
//   - Resolve first and null what does not exist. The entry lands in the
//     platform/unowned bucket, which callerOrgScope ADMITS on purpose and which
//     this app already documents as the home for exactly these rows (see
//     admin_org_scope.go). Every admin is their intended reviewer, which is the
//     same outcome the old fallback produced — now by decision rather than by
//     depending on a constraint that no longer exists.
//
// A resolvable id is kept, so a genuinely shared user or organization keeps
// being attributed to the tenant it belongs to.
//
// ActorEmail is deliberately left nil for an unresolved actor: the federated
// wire shape (the registry's audit.LogEntry) carries no email, so there is
// nothing truthful to denormalise, and the sibling's own id stays in metadata
// under federated_user_id for triage.
func (h *AuditIngestHandlers) resolveSiblingIDs(ctx context.Context, entry *idmodels.AuditLog, req *federatedAuditEntry) {
	// Existence probes on a server-to-server route authenticated by the suite
	// service token: there is no tenant principal to derive a scope from, and
	// the question being asked is "does this id resolve in this database at
	// all". Recorded in admin_audit_scope_test.go's reviewed list.
	probe := idstore.OrgScopeAllOrganizations()
	if entry.UserID != nil && h.userRepo != nil {
		if _, err := h.userRepo.GetUserByID(ctx, *entry.UserID, probe); err != nil {
			entry.Metadata["federated_user_id"] = req.UserID
			entry.UserID = nil
		}
	}
	if entry.OrganizationID != nil && h.orgRepo != nil {
		if _, err := h.orgRepo.GetByID(ctx, *entry.OrganizationID, probe); err != nil {
			entry.Metadata["federated_organization_id"] = req.OrganizationID
			entry.OrganizationID = nil
		}
	}
}

// federatedAuditModel maps a sibling's wire entry to an identity AuditLog. It is
// pure (no DB) so the mapping is unit-testable. CreateAuditLog stamps created_at
// at ingest time, so the sibling's original event time is preserved in metadata
// (source_timestamp); auth_method/status_code have no audit_logs columns and are
// likewise folded into metadata. Every federated row is marked federated=true.
func federatedAuditModel(req *federatedAuditEntry, sourceApp string) *idmodels.AuditLog {
	meta := map[string]interface{}{}
	for k, v := range req.Metadata {
		meta[k] = v
	}
	meta["federated"] = true
	if sourceApp != "" {
		meta["source_app"] = sourceApp
	}
	if !req.Timestamp.IsZero() {
		meta["source_timestamp"] = req.Timestamp.UTC().Format(time.RFC3339)
	}
	if req.AuthMethod != "" {
		meta["auth_method"] = req.AuthMethod
	}
	if req.StatusCode != 0 {
		meta["status_code"] = req.StatusCode
	}

	entry := &idmodels.AuditLog{Action: req.Action, Metadata: meta}
	if req.ResourceType != "" {
		rt := req.ResourceType
		entry.ResourceType = &rt
	}
	if req.ResourceID != "" {
		rid := req.ResourceID
		entry.ResourceID = &rid
	}
	if req.IPAddress != "" {
		ip := req.IPAddress
		entry.IPAddress = &ip
	}
	if req.UserID != "" {
		uid := req.UserID
		entry.UserID = &uid
	}
	if req.OrganizationID != "" {
		oid := req.OrganizationID
		entry.OrganizationID = &oid
	}
	return entry
}
