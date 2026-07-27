// transfer.go implements the Phase 2 transfer plane: copying state to another
// source (backup) and moving it (migrate) with parity verification and an
// optional, explicit source decommission.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/analyzer"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/statesource"
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

	srcB, err := h.repo.GetByID(ctx, req.TargetSourceID)
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
		saved, _ := h.transferRepo.Create(ctx, rec)
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

	saved, err := h.transferRepo.Create(ctx, rec)
	if err != nil {
		serverError(c, err, "transfer completed but failed to record it")
		return
	}
	h.audit.write(c, "state."+mode, "state", srcA.ID, map[string]interface{}{
		"key": key, "target_source_id": srcB.ID, "target_key": req.TargetKey,
		"status": rec.Status, "decommissioned": rec.Decommissioned,
	})
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

// GetTransfer returns a transfer record by id.
func (h *SourcesHandlers) GetTransfer() gin.HandlerFunc {
	return func(c *gin.Context) {
		t, err := h.transferRepo.GetByID(c.Request.Context(), c.Param("id"))
		if err != nil {
			serverError(c, err, "failed to load transfer")
			return
		}
		if t == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "transfer not found"})
			return
		}
		c.JSON(http.StatusOK, t)
	}
}
