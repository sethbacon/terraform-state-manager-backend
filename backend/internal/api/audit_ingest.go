// audit_ingest.go implements the cross-app audit federation receiver. A sibling
// suite app (the registry) ships its audit entries here via its existing webhook
// shipper so they land in this app's shared identity audit trail — the same
// trail the admin Audit Log page reads. Federation is only coherent when both
// apps share one identity store (so the sibling's user/org IDs resolve here and
// satisfy the audit_logs FK); the handler refuses otherwise, matching the
// audit.ingest.v1 manifest capability which is advertised only under sharedStore.
package api

import (
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
	auditRepo   *idstore.AuditRepository
	sharedStore bool
}

// NewAuditIngestHandlers wires the receiver. A nil identityDB (unit-test rigs)
// leaves the repo unset; the handler then reports the store unavailable.
func NewAuditIngestHandlers(identityDB *sql.DB, cfg *config.Config) *AuditIngestHandlers {
	h := &AuditIngestHandlers{sharedStore: cfg.Suite.IdentitySharedStore}
	if identityDB != nil {
		h.auditRepo = idstore.NewAuditRepository(identityDB)
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
		// the sibling's user/org IDs resolve in this app's identity tables (else a
		// merged timeline mis-attributes actors, and the user_id FK rejects the
		// row). This mirrors the audit.ingest.v1 capability gate in the manifest.
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

		entry := federatedAuditModel(&req, c.GetHeader(suiteSourceAppHeader))
		ctx := c.Request.Context()
		if err := h.auditRepo.CreateAuditLog(ctx, entry); err != nil {
			// Most likely the sibling's user/org id doesn't exist here (sharedStore
			// mis-declared, or the actor was provisioned only in the sibling).
			// Degrade to an attributed-in-metadata record rather than 500-storm the
			// shipper: null the actor FKs, preserve the originals in metadata, retry.
			entry.Metadata["federated_user_id"] = req.UserID
			entry.Metadata["federated_organization_id"] = req.OrganizationID
			entry.UserID = nil
			entry.OrganizationID = nil
			if err2 := h.auditRepo.CreateAuditLog(ctx, entry); err2 != nil {
				slog.Warn("failed to ingest federated audit entry", "action", req.Action, "error", err2)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record audit entry"})
				return
			}
		}
		c.JSON(http.StatusAccepted, gin.H{"status": "recorded"})
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
