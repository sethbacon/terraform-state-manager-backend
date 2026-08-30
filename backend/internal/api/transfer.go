// transfer.go implements the Phase 2 transfer plane: copying state to another
// source (backup) and moving it (migrate) with parity verification and an
// optional, explicit source decommission.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/analyzer"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/statesource"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

type transferRequest struct {
	TargetSourceID string `json:"target_source_id" binding:"required"`
	TargetKey      string `json:"target_key" binding:"required"`
	Decommission   bool   `json:"decommission"`
}

// BackupToSource copies the state at ?key= into another source (non-destructive).
// @Summary      Back up state to another source
// @Description  Non-destructively copies the state at ?key= into a target source. Requires state:transfer.
// @Tags         Transfer
// @Accept       json
// @Produce      json
// @Param        id     path   string  true   "Source ID"
// @Param        key    query  string  true   "State file key"
// @Param        force  query  bool    false  "Override the pre-decommission serial/lineage conflict check before an optional decommission (migrate only; no effect on backup)"
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /sources/{id}/state/backup [post]
func (h *SourcesHandlers) BackupToSource() gin.HandlerFunc {
	return func(c *gin.Context) { h.doTransfer(c, "backup") }
}

// MigrateToSource copies the state into another source, verifies parity, and
// optionally decommissions (empties) the original.
// @Summary      Migrate state to another source
// @Description  Copies the state to a target, verifies serial/lineage/resource-count parity, and optionally decommissions (empties) the original after a successful backup. Requires state:transfer.
// @Tags         Transfer
// @Accept       json
// @Produce      json
// @Param        id     path   string  true   "Source ID"
// @Param        key    query  string  true   "State file key"
// @Param        force  query  bool    false  "Override the pre-decommission serial/lineage conflict check before an optional decommission (migrate only; no effect on backup)"
// @Success      200  {object}  map[string]interface{}
// @Failure      502  {object}  map[string]interface{}  "write to target failed"
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /sources/{id}/state/migrate [post]
func (h *SourcesHandlers) MigrateToSource() gin.HandlerFunc {
	return func(c *gin.Context) { h.doTransfer(c, "migrate") }
}

func (h *SourcesHandlers) doTransfer(c *gin.Context, mode string) {
	key := c.Query("key")
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key query parameter is required"})
		return
	}
	var req transferRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "target_source_id and target_key are required"})
		return
	}

	srcA, connA, ok := h.sourceAndConnector(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	actor := userIDOf(c)
	force := c.Query("force") == "true"

	// Scoped, and the ordering matters: transferEndpointsReachable below already
	// refuses a target the caller may not reach, but it runs AFTER this load --
	// and this load decrypts the target's credentials to build its connector. A
	// refusal that happens after the secret is in memory is a late refusal.
	scopeB, resolvedB := tenantscope.FromContext(c)
	if !resolvedB {
		serverError(c, errNoTenantScope, "the tenant scope was not resolved for this route")
		return
	}
	srcB, err := h.repo.GetByIDInScope(ctx, req.TargetSourceID, scopeB)
	if err != nil {
		serverError(c, err, "failed to load target source")
		return
	}
	if srcB == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "target source not found"})
		return
	}
	credsB, err := decryptCredentials(srcB)
	if err != nil {
		serverError(c, err, "failed to decrypt target credentials")
		return
	}
	connB, err := statesource.New(srcB.Type, srcB.Config, credsB)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// A TRANSFER NAMES TWO ENDPOINTS AND THE ORGANIZATION IS NEITHER OF THEM.
	//
	// 000033 keeps a cross-organization transfer as a SUPPORTED way to move a
	// state file across the boundary #436 draws, so this does NOT refuse when the
	// ends disagree. What it refuses is a caller reaching an end they hold no
	// authority over: hold BOTH sides and the move is yours to make; hold one and
	// it is someone else's state file. That is what turns a cross-boundary move
	// into an explicitly authorized act rather than a by-product of an unscoped
	// read -- and the record below names the caller's acting organization, so the
	// act has an owner in the audit trail.
	//
	// Ends that carry NO organization are not judged: the backfill has not
	// necessarily reached them, and refusing an unstamped row would break
	// transfers on exactly the rows #436 is still repairing.
	organizationID := actingOrganization(c, h.orgs)
	if organizationID == "" {
		return // actingOrganization has already written the response
	}
	if !transferEndpointsReachable(c, srcA, srcB) {
		return
	}

	// Lock both the source and target keys for the whole transfer, the same way
	// EditState/StateOperation/RestoreBackup do — previously connA/connB were
	// read and written here with no lock at all, so a concurrent editor could
	// race a transfer and silently lose an update on either side.
	release, locked := h.acquireTransferLocks(c, srcA.ID, connA, key, srcB.ID, connB, req.TargetKey)
	if !locked {
		return
	}
	defer release()

	raw, err := connA.Read(ctx, key)
	if err != nil {
		upstreamError(c, http.StatusBadGateway, err, "failed to read source state")
		return
	}
	srcAnalysis, srcErr := analyzer.Analyze(raw.Data)

	rec := &repositories.Transfer{
		Mode: mode, SourceID: srcA.ID, SourceKey: key,
		TargetSourceID: srcB.ID, TargetKey: req.TargetKey, Actor: actor,
	}

	if err := connB.Write(ctx, req.TargetKey, raw.Data); err != nil {
		_ = c.Error(err) // log the raw connector error; keep it out of the client body/record (#286)
		rec.Status = "failed"
		rec.Detail = "write to target failed"
		saved, _ := h.transferRepo.Create(ctx, rec, organizationID)
		if saved == nil {
			saved = rec
		}
		c.JSON(http.StatusBadGateway, saved)
		return
	}
	rec.Status = "success"
	h.refreshAnalysisAsync(srcB, req.TargetKey)

	if mode == "migrate" {
		verified, detail := verifyTransfer(ctx, connB, req.TargetKey, srcAnalysis, srcErr)
		rec.Verified = &verified
		rec.Detail = detail
		if !verified {
			rec.Status = "verification_failed"
		}
		if req.Decommission && verified && srcErr == nil {
			serial := srcAnalysis.Serial
			// Re-check the source immediately before the irreversible decommission
			// write. The transfer lock is held throughout, so this is defense in
			// depth (mirroring EditState's own serial/lineage guard) rather than the
			// primary protection — it catches drift from a lock bypassed elsewhere
			// (e.g. a force-unlock) rather than relying on locking alone. Fails
			// closed unless the caller passes force=true.
			conflict := ""
			if !force {
				latest, rErr := connA.Read(ctx, key)
				switch {
				case rErr != nil && statesource.IsNotFound(rErr):
					// The source key was read successfully moments ago (this lock has been
					// held for the whole transfer) and is now gone — most likely a
					// concurrent delete after a force-unlock. Nothing is left to
					// decommission, and writing emptyState here would silently *recreate*
					// a key something else just removed, so skip instead of proceeding.
					conflict = "source key no longer exists (nothing to decommission)"
				case rErr != nil:
					_ = c.Error(rErr)
					conflict = "cannot verify source before decommission"
				case rErr == nil:
					if latestA, aErr := analyzer.Analyze(latest.Data); aErr == nil &&
						(latestA.Serial != serial || latestA.Lineage != srcAnalysis.Lineage) {
						conflict = fmt.Sprintf("source changed since transfer read (serial %d→%d); pass force=true to override",
							serial, latestA.Serial)
					}
				}
			}
			// Never empty the source without a recoverable pre-decommission backup:
			// a failed backup here would otherwise mean unrecoverable data loss.
			if conflict != "" {
				rec.Detail = detail + "; decommission skipped: " + conflict
			} else if _, err := h.editRepo.CreateBackup(ctx, srcA.ID, key, raw.Data, &serial, actor); err != nil {
				rec.Detail = detail + "; decommission skipped: source backup failed: " + err.Error()
			} else if err := connA.Write(ctx, key, emptyState(srcAnalysis)); err == nil {
				rec.Decommissioned = true
				after := serial + 1
				h.recordEdit(ctx, srcA.ID, key, "decommission", actor, nil, &serial, &after, "success", "emptied after migrate")
				h.refreshAnalysisAsync(srcA, key)
			} else {
				rec.Detail = detail + "; decommission failed (source preserved): " + err.Error()
			}
		}
	}

	saved, err := h.transferRepo.Create(ctx, rec, organizationID)
	if err != nil {
		serverError(c, err, "transfer completed but failed to record it")
		return
	}
	details := map[string]interface{}{
		"key": key, "target_source_id": srcB.ID, "target_key": req.TargetKey,
		"status": rec.Status, "decommissioned": rec.Decommissioned,
	}
	h.audit.write(c, "state."+mode, "state", srcA.ID, details)

	// THE COUNTERPARTY GETS ITS OWN ENTRY. A transfer spans two organizations
	// but the transfer row records ONE -- the organization the caller declared
	// they were acting as. Everything above is therefore visible to that
	// organization and to nobody else, which means a transfer out of the OTHER
	// organization leaves it with no record that its state was read and copied
	// elsewhere. The row does not need a second owner; the counterparty needs
	// to KNOW.
	//
	// Written per DISTINCT counterparty, and only when there is one: a transfer
	// within a single organization already has its entry, and writing a second
	// identical one would double every ordinary transfer in that tenant's log.
	// An unstamped source contributes nothing rather than an entry attributed
	// to the empty organization, which ListAuditLogs would show to everyone.
	for _, counterparty := range counterpartyOrganizations(organizationID, srcA, srcB) {
		h.audit.writeForOrg(c, counterparty, "state."+mode+".counterparty", "state", srcA.ID, details)
	}
	c.JSON(http.StatusOK, saved)
}

// acquireTransferLocks locks both the (source, key) and (target, targetKey)
// pairs for the duration of a transfer/migrate, using the exact same acquireLock
// path EditState/StateOperation/RestoreBackup use (native statesource.Locker
// first, else the app-level DB advisory lock). Locks are taken in a
// deterministic order — sorted by (sourceID, key) — so two transfers racing in
// opposite directions between the same two (source, key) pairs can never
// deadlock. If both sides name the exact same (source, key) — a self-transfer
// — only one lock is taken, since acquiring the same native/DB lock twice from
// one caller would otherwise conflict with itself rather than with another
// operation.
func (h *SourcesHandlers) acquireTransferLocks(
	c *gin.Context,
	sourceIDA string, connA statesource.Connector, keyA string,
	sourceIDB string, connB statesource.Connector, keyB string,
) (release func(), ok bool) {
	if sourceIDA == sourceIDB && keyA == keyB {
		return h.acquireLock(c, sourceIDA, connA, keyA)
	}

	type lockTarget struct {
		sourceID string
		conn     statesource.Connector
		key      string
	}
	first := lockTarget{sourceIDA, connA, keyA}
	second := lockTarget{sourceIDB, connB, keyB}
	if second.sourceID < first.sourceID || (second.sourceID == first.sourceID && second.key < first.key) {
		first, second = second, first
	}

	release1, locked := h.acquireLock(c, first.sourceID, first.conn, first.key)
	if !locked {
		return nil, false
	}
	release2, locked := h.acquireLock(c, second.sourceID, second.conn, second.key)
	if !locked {
		release1()
		return nil, false
	}
	return func() { release2(); release1() }, true
}

// verifyTransfer reads the freshly written target and checks serial, lineage, and
// resource-count parity against the source.
func verifyTransfer(ctx context.Context, conn statesource.Connector, key string, src *analyzer.Analysis, srcErr error) (bool, string) {
	if srcErr != nil {
		return false, "source state could not be analyzed"
	}
	rb, err := conn.Read(ctx, key)
	if err != nil {
		return false, "could not read back target: " + err.Error()
	}
	tb, err := analyzer.Analyze(rb.Data)
	if err != nil {
		return false, "target state could not be analyzed"
	}
	if tb.Serial != src.Serial || tb.Lineage != src.Lineage ||
		tb.TotalResources != src.TotalResources || tb.ManagedResources != src.ManagedResources {
		return false, fmt.Sprintf("parity mismatch (serial %d→%d, instances %d→%d)",
			src.Serial, tb.Serial, src.TotalResources, tb.TotalResources)
	}
	return true, "serial, lineage, and resource counts match"
}

// emptyState produces a managed-resource-free state for decommissioning a source
// after a verified migrate, keeping the lineage and bumping the serial.
func emptyState(a *analyzer.Analysis) []byte {
	b, _ := json.Marshal(map[string]any{
		"version":           4,
		"terraform_version": a.TerraformVersion,
		"serial":            a.Serial + 1,
		"lineage":           a.Lineage,
		"outputs":           map[string]any{},
		"resources":         []any{},
	})
	return b
}

// GetTransfer returns a transfer record by id, scoped to the caller's
// organization (#393 Phase 3, the last of the nine roots).
//
// It read by id alone, so any caller holding state:read anywhere could fetch any
// transfer in the deployment: both source ids, both state keys, the actor, and
// whether the source was decommissioned. That is a map of another tenant's state
// files, and the state key is the argument the read routes take.
//
// The row records ONE organization -- the one the caller was acting as when they
// performed the transfer -- and this predicate is on that column alone. A
// transfer whose ends sit in different organizations is a supported move
// (000033), and the counterparty learns of it through its own audit entry
// (#541), not by being admitted to this read. transfer_scope.go states why
// widening the predicate to either end would be wrong in both directions.
func (h *SourcesHandlers) GetTransfer() gin.HandlerFunc {
	return func(c *gin.Context) {
		// An unresolved scope is a wiring fault, not an empty one: this route
		// carries middleware.TenantScope, and if it stopped doing so, reading it
		// as "no memberships" would turn a missing router line into a silent
		// unscoped read the next time someone simplified the branch.
		scope, resolved := tenantscope.FromContext(c)
		if !resolved {
			serverError(c, errNoTenantScope, "the tenant scope was not resolved for this route")
			return
		}
		t, err := h.transferRepo.GetByIDInScope(c.Request.Context(), c.Param("id"), scope)
		if err != nil {
			serverError(c, err, "failed to load transfer")
			return
		}
		// A transfer in another organization is reported EXACTLY as one that does
		// not exist; the two share this branch on purpose.
		if t == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "transfer not found"})
			return
		}
		c.JSON(http.StatusOK, t)
	}
}

// transferEndpointsReachable reports whether the caller may act on BOTH ends of
// a transfer, writing the refusal itself when they may not.
//
// The refusal is a 404, matching the shape a missing source returns, so a caller
// outside an owning organization cannot use a transfer to probe which source ids
// exist elsewhere in the fleet.
func transferEndpointsReachable(c *gin.Context, ends ...*repositories.Source) bool {
	scope, resolved := tenantscope.FromContext(c)
	if !resolved {
		// Same posture as actingOrganization: an unresolved scope is a wiring
		// fault, not an empty one. Failing open here would make the whole check
		// disappear the moment the middleware came unwired.
		serverError(c, errNoTenantScope, "the tenant scope was not resolved for this route")
		return false
	}
	for _, end := range ends {
		if end == nil || end.OrganizationID == "" {
			continue // unstamped: not judged, see doTransfer
		}
		if !scope.Permits(end.OrganizationID) {
			c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
			return false
		}
	}
	return true
}

// counterpartyOrganizations returns the organizations that were party to a
// transfer OTHER than the one it was recorded under, de-duplicated.
//
// Both ends are considered because either can be the counterparty: a caller
// acting as A may move state INTO A from B, or OUT OF A into B, and in both
// cases B is the tenant with no record of its own involvement. The acting
// organization is excluded because it already has the primary entry, and the
// empty string is excluded because an audit entry attributed to no
// organization is shown to everyone by ListAuditLogs -- which would turn a fix
// for under-disclosure into over-disclosure.
func counterpartyOrganizations(actingOrg string, ends ...*repositories.Source) []string {
	seen := map[string]bool{strings.TrimSpace(actingOrg): true, "": true}
	var out []string
	for _, e := range ends {
		if e == nil {
			continue
		}
		org := strings.TrimSpace(e.OrganizationID)
		if seen[org] {
			continue
		}
		seen[org] = true
		out = append(out, org)
	}
	return out
}
