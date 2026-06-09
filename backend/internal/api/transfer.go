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
// @Param        id   path   string  true  "Source ID"
// @Param        key  query  string  true  "State file key"
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
// @Param        id   path   string  true  "Source ID"
// @Param        key  query  string  true  "State file key"
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

	srcB, err := h.repo.GetByID(ctx, req.TargetSourceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load target source"})
		return
	}
	if srcB == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "target source not found"})
		return
	}
	credsB, err := decryptCredentials(srcB)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decrypt target credentials"})
		return
	}
	connB, err := statesource.New(srcB.Type, srcB.Config, credsB)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	raw, err := connA.Read(ctx, key)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read source state: " + err.Error()})
		return
	}
	srcAnalysis, srcErr := analyzer.Analyze(raw.Data)

	rec := &repositories.Transfer{
		Mode: mode, SourceID: srcA.ID, SourceKey: key,
		TargetSourceID: srcB.ID, TargetKey: req.TargetKey, Actor: actor,
	}

	if err := connB.Write(ctx, req.TargetKey, raw.Data); err != nil {
		rec.Status = "failed"
		rec.Detail = "write to target failed: " + err.Error()
		saved, _ := h.transferRepo.Create(ctx, rec)
		if saved == nil {
			saved = rec
		}
		c.JSON(http.StatusBadGateway, saved)
		return
	}
	rec.Status = "success"

	if mode == "migrate" {
		verified, detail := verifyTransfer(ctx, connB, req.TargetKey, srcAnalysis, srcErr)
		rec.Verified = &verified
		rec.Detail = detail
		if !verified {
			rec.Status = "verification_failed"
		}
		if req.Decommission && verified && srcErr == nil {
			serial := srcAnalysis.Serial
			// Never empty the source without a recoverable pre-decommission backup:
			// a failed backup here would otherwise mean unrecoverable data loss.
			if _, err := h.editRepo.CreateBackup(ctx, srcA.ID, key, raw.Data, &serial, actor); err != nil {
				rec.Detail = detail + "; decommission skipped: source backup failed: " + err.Error()
			} else if err := connA.Write(ctx, key, emptyState(srcAnalysis)); err == nil {
				rec.Decommissioned = true
				after := serial + 1
				h.recordEdit(ctx, srcA.ID, key, "decommission", actor, nil, &serial, &after, "success", "emptied after migrate")
			} else {
				rec.Detail = detail + "; decommission failed (source preserved): " + err.Error()
			}
		}
	}

	saved, err := h.transferRepo.Create(ctx, rec)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "transfer completed but failed to record it"})
		return
	}
	c.JSON(http.StatusOK, saved)
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
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load transfer"})
			return
		}
		if t == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "transfer not found"})
			return
		}
		c.JSON(http.StatusOK, t)
	}
}
