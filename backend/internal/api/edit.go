// edit.go implements the Phase 2 edit plane: replacing a state file with the
// safety pipeline validate → backup → write → audit, plus listing and restoring
// backups. The current state is always backed up before any write, so every edit
// is one-click reversible.
package api

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/analyzer"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/stateops"
	"github.com/terraform-state-manager/terraform-state-manager/internal/statesource"
)

// EditState replaces the state at ?key= with the request body after validating it
// (valid Terraform state JSON; serial non-regressing and lineage match unless
// ?force=true) and backing up the current version.
// @Summary      Replace state (guarded)
// @Description  Lock -> backup -> validate (serial/lineage) -> write -> audit. Pass force=true to override serial/lineage checks. Requires state:write.
// @Tags         Edit
// @Accept       json
// @Produce      json
// @Param        id     path   string  true   "Source ID"
// @Param        key    query  string  true   "State file key"
// @Param        force  query  bool    false  "Override serial/lineage checks"
// @Success      200  {object}  map[string]interface{}
// @Failure      409  {object}  map[string]interface{}  "serial/lineage conflict or locked"
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /sources/{id}/state/raw [put]
func (h *SourcesHandlers) EditState() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.Query("key")
		if key == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "key query parameter is required"})
			return
		}
		newData, err := io.ReadAll(io.LimitReader(c.Request.Body, maxUploadBytes))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
			return
		}
		newAnalysis, err := analyzer.Analyze(newData)
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "rejected: " + err.Error()})
			return
		}

		src, conn, ok := h.sourceAndConnector(c)
		if !ok {
			return
		}
		ctx := c.Request.Context()
		actor := userIDOf(c)
		force := c.Query("force") == "true"

		release, locked := h.acquireLock(c, src.ID, conn, key)
		if !locked {
			return
		}
		defer release()

		var beforeSerial *int64
		var backupID *string

		// Fail closed: only a genuine not-found (first write to this key) may skip
		// the backup and serial/lineage checks. A transient read failure must abort,
		// otherwise the write proceeds unguarded and unbackuped exactly when the
		// backend is flaky.
		cur, readErr := conn.Read(ctx, key)
		if readErr != nil && !statesource.IsNotFound(readErr) {
			c.JSON(http.StatusBadGateway, gin.H{"error": "cannot verify current state before writing: " + readErr.Error()})
			return
		}
		if readErr == nil && cur != nil {
			if curA, e := analyzer.Analyze(cur.Data); e == nil {
				bs := curA.Serial
				beforeSerial = &bs
				if !force && newAnalysis.Serial < curA.Serial {
					c.JSON(http.StatusConflict, gin.H{
						"error": fmt.Sprintf("new serial %d is lower than current %d; pass force=true to override",
							newAnalysis.Serial, curA.Serial),
					})
					return
				}
				if !force && curA.Lineage != "" && newAnalysis.Lineage != "" && curA.Lineage != newAnalysis.Lineage {
					c.JSON(http.StatusConflict, gin.H{"error": "lineage mismatch; pass force=true to override"})
					return
				}
			}
			id, bErr := h.editRepo.CreateBackup(ctx, src.ID, key, cur.Data, beforeSerial, actor)
			if bErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to back up current state"})
				return
			}
			backupID = &id
		}

		after := newAnalysis.Serial
		if err := conn.Write(ctx, key, newData); err != nil {
			h.recordEdit(ctx, src.ID, key, "raw_replace", actor, backupID, beforeSerial, &after, "failed", err.Error())
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		h.recordEdit(ctx, src.ID, key, "raw_replace", actor, backupID, beforeSerial, &after, "success", "")
		h.audit.write(c, "state.edit", "state", src.ID,
			map[string]interface{}{"key": key, "op": "raw_replace"})
		h.refreshAnalysisAsync(src, key)
		c.JSON(http.StatusOK, gin.H{"status": "written", "backup_id": backupID, "serial": after})
	}
}

// StateOperation applies a structured edit (rm / mv) or an admin delete to the
// state at ?key=, running it through the same backup → write/delete → audit
// pipeline.
// @Summary      State operation (rm/mv/delete)
// @Description  Apply an rm or mv operation (state:write), or delete the state object outright (admin only), through the backup -> mutate -> audit pipeline. delete creates a final recoverable backup first and refuses while the state is locked; pass purge=true to also drop saved backups.
// @Tags         Edit
// @Accept       json
// @Produce      json
// @Param        id   path   string  true  "Source ID"
// @Param        key  query  string  true  "State file key"
// @Success      200  {object}  map[string]interface{}
// @Failure      403  {object}  map[string]interface{}  "delete requires the admin role"
// @Failure      404  {object}  map[string]interface{}  "state not found"
// @Failure      409  {object}  map[string]interface{}  "state is locked"
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /sources/{id}/state/operations [post]
func (h *SourcesHandlers) StateOperation() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.Query("key")
		if key == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "key query parameter is required"})
			return
		}
		var req struct {
			Op      string `json:"op" binding:"required"` // rm | mv | delete
			Address string `json:"address"`
			To      string `json:"to"`
			Key     string `json:"key"`   // delete: must echo ?key= (defense in depth)
			Purge   bool   `json:"purge"` // delete: also drop saved backups
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "op is required"})
			return
		}

		// delete is gated in-handler: the /state/operations route is granted to
		// state:write (editors), but removing a state object outright is reserved
		// for admins. Check (and require the key echo) before loading the source so
		// a non-admin never triggers a backend lookup.
		if req.Op == "delete" {
			if !isAdmin(c) {
				c.JSON(http.StatusForbidden, gin.H{"error": "deleting a state file requires the admin role"})
				return
			}
			if req.Key != key {
				c.JSON(http.StatusBadRequest, gin.H{"error": "request body must echo the exact state key to delete"})
				return
			}
		} else if req.Address == "" {
			// rm/mv need a resource address; reject before the source lookup so the
			// 400 matches the binding behaviour callers already rely on.
			c.JSON(http.StatusBadRequest, gin.H{"error": "address is required"})
			return
		}

		src, conn, ok := h.sourceAndConnector(c)
		if !ok {
			return
		}
		ctx := c.Request.Context()
		actor := userIDOf(c)

		release, locked := h.acquireLock(c, src.ID, conn, key)
		if !locked {
			return
		}
		defer release()

		if req.Op == "delete" {
			h.deleteState(c, src, conn, key, req.Purge, actor)
			return
		}

		cur, err := conn.Read(ctx, key)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read current state: " + err.Error()})
			return
		}

		var newData []byte
		switch req.Op {
		case "rm":
			newData, err = stateops.RemoveResource(cur.Data, req.Address)
		case "mv":
			if req.To == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "mv requires a 'to' address"})
				return
			}
			newData, err = stateops.MoveResource(cur.Data, req.Address, req.To)
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "op must be 'rm' or 'mv'"})
			return
		}
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
			return
		}
		newAnalysis, err := analyzer.Analyze(newData)
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "operation produced invalid state: " + err.Error()})
			return
		}

		var beforeSerial *int64
		if curA, e := analyzer.Analyze(cur.Data); e == nil {
			bs := curA.Serial
			beforeSerial = &bs
		}
		backupID, bErr := h.editRepo.CreateBackup(ctx, src.ID, key, cur.Data, beforeSerial, actor)
		if bErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to back up current state"})
			return
		}

		after := newAnalysis.Serial
		if err := conn.Write(ctx, key, newData); err != nil {
			h.recordEdit(ctx, src.ID, key, req.Op, actor, &backupID, beforeSerial, &after, "failed", err.Error())
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		detail := req.Address
		if req.Op == "mv" {
			detail = req.Address + " → " + req.To
		}
		h.recordEdit(ctx, src.ID, key, req.Op, actor, &backupID, beforeSerial, &after, "success", detail)
		h.audit.write(c, "state.operation", "state", src.ID,
			map[string]interface{}{"key": key, "op": req.Op, "address": req.Address, "to": req.To})
		h.refreshAnalysisAsync(src, key)
		c.JSON(http.StatusOK, gin.H{"status": "applied", "op": req.Op, "backup_id": backupID, "serial": after})
	}
}

// deleteState removes the state object at key (the admin "delete" operation).
// The lock is already held by the caller. A final backup is always taken before
// the destructive delete so a connector failure — or a change of heart — stays
// recoverable, exactly as rm/mv/decommission do. With purge=true the saved
// backups (including that final one) are dropped after the object is gone; the
// edit audit trail is kept regardless so the deletion stays accountable.
func (h *SourcesHandlers) deleteState(c *gin.Context, src *repositories.Source, conn statesource.Connector, key string, purge bool, actor string) {
	ctx := c.Request.Context()

	cur, err := conn.Read(ctx, key)
	if err != nil {
		if statesource.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "state not found"})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read current state: " + err.Error()})
		return
	}

	var beforeSerial *int64
	if curA, e := analyzer.Analyze(cur.Data); e == nil {
		bs := curA.Serial
		beforeSerial = &bs
	}

	backupID, bErr := h.editRepo.CreateBackup(ctx, src.ID, key, cur.Data, beforeSerial, actor)
	if bErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to back up current state before delete"})
		return
	}

	if err := conn.Delete(ctx, key); err != nil {
		if statesource.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "state not found"})
			return
		}
		h.recordEdit(ctx, src.ID, key, "delete", actor, &backupID, beforeSerial, nil, "failed", err.Error())
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	purged := false
	detail := "deleted state object (backup retained)"
	recBackupID := &backupID
	if purge {
		if _, pErr := h.editRepo.DeleteBackups(ctx, src.ID, key); pErr != nil {
			// The object is already gone; report success but flag the purge failure
			// so the operator knows backups may still linger.
			h.recordEdit(ctx, src.ID, key, "delete", actor, nil, beforeSerial, nil, "success",
				"deleted state object; purge of backups failed: "+pErr.Error())
			h.audit.write(c, "state.delete", "state", src.ID,
				map[string]interface{}{"key": key, "purged": false, "purge_error": pErr.Error()})
			c.JSON(http.StatusOK, gin.H{"status": "deleted", "key": key, "purged": false,
				"warning": "state deleted but its backups could not be purged"})
			return
		}
		purged = true
		detail = "deleted state object and purged all backups"
		recBackupID = nil // the backup row was just removed; don't dangle a reference
	}

	h.recordEdit(ctx, src.ID, key, "delete", actor, recBackupID, beforeSerial, nil, "success", detail)
	h.audit.write(c, "state.delete", "state", src.ID,
		map[string]interface{}{"key": key, "purged": purged})
	resp := gin.H{"status": "deleted", "key": key, "purged": purged}
	if !purged {
		resp["backup_id"] = backupID
	}
	c.JSON(http.StatusOK, resp)
}

// ListBackups returns the backup history for a state file (?key=).
func (h *SourcesHandlers) ListBackups() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.Query("key")
		if key == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "key query parameter is required"})
			return
		}
		backups, err := h.editRepo.ListBackups(c.Request.Context(), c.Param("id"), key)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list backups"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"backups": backups})
	}
}

// RestoreBackup writes a stored backup back to its source/key (after backing up
// the current version first).
func (h *SourcesHandlers) RestoreBackup() gin.HandlerFunc {
	return func(c *gin.Context) {
		backup, err := h.editRepo.GetBackup(c.Request.Context(), c.Param("backupId"))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load backup"})
			return
		}
		if backup == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "backup not found"})
			return
		}
		src, conn, ok := h.sourceAndConnector(c)
		if !ok {
			return
		}
		if backup.SourceID != src.ID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "backup does not belong to this source"})
			return
		}
		ctx := c.Request.Context()
		actor := userIDOf(c)

		release, locked := h.acquireLock(c, src.ID, conn, backup.StateKey)
		if !locked {
			return
		}
		defer release()

		var beforeSerial *int64
		var preBackupID *string
		// Same fail-closed rule as EditState: a transient read failure aborts the
		// restore (the current state could not be backed up); only a genuine
		// not-found proceeds without a pre-restore backup.
		cur, readErr := conn.Read(ctx, backup.StateKey)
		if readErr != nil && !statesource.IsNotFound(readErr) {
			c.JSON(http.StatusBadGateway, gin.H{"error": "cannot verify current state before restoring: " + readErr.Error()})
			return
		}
		if readErr == nil && cur != nil {
			if curA, e := analyzer.Analyze(cur.Data); e == nil {
				bs := curA.Serial
				beforeSerial = &bs
			}
			id, bErr := h.editRepo.CreateBackup(ctx, src.ID, backup.StateKey, cur.Data, beforeSerial, actor)
			if bErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to back up current state"})
				return
			}
			preBackupID = &id
		}

		if err := conn.Write(ctx, backup.StateKey, backup.Data); err != nil {
			h.recordEdit(ctx, src.ID, backup.StateKey, "restore", actor, preBackupID, beforeSerial, backup.Serial, "failed", err.Error())
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		h.recordEdit(ctx, src.ID, backup.StateKey, "restore", actor, preBackupID, beforeSerial, backup.Serial, "success", "restored backup "+backup.ID)
		h.audit.write(c, "state.restore", "state", src.ID,
			map[string]interface{}{"key": backup.StateKey, "backup_id": backup.ID})
		h.refreshAnalysisAsync(src, backup.StateKey)
		c.JSON(http.StatusOK, gin.H{"status": "restored", "key": backup.StateKey})
	}
}

// sourceAndConnector loads the source by :id and its connector (with decrypted
// credentials), writing an error response on failure.
func (h *SourcesHandlers) sourceAndConnector(c *gin.Context) (*repositories.Source, statesource.Connector, bool) {
	s, err := h.repo.GetByID(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load source"})
		return nil, nil, false
	}
	if s == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
		return nil, nil, false
	}
	creds, err := decryptCredentials(s)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decrypt source credentials"})
		return nil, nil, false
	}
	conn, err := statesource.New(s.Type, s.Config, creds)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return nil, nil, false
	}
	return s, conn, true
}

// acquireLock takes an advisory lock on (source, key) before an edit. It prefers
// the connector's native backend lock when available (which also guards against
// the terraform CLI itself); otherwise it falls back to an app-level DB lock so
// connectors without native locking (S3/GCS/Azure/HCP/git) are still mutually
// excluded. On success it returns a release func; when the key is already locked
// it writes a 409 and returns ok=false.
func (h *SourcesHandlers) acquireLock(c *gin.Context, sourceID string, conn statesource.Connector, key string) (release func(), ok bool) {
	if locker, supported := conn.(statesource.Locker); supported {
		id, err := locker.Lock(c.Request.Context(), key)
		if err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return nil, false
		}
		return func() { _ = locker.Unlock(context.Background(), key, id) }, true
	}
	lockID, err := h.lockRepo.Acquire(c.Request.Context(), sourceID, key, userIDOf(c))
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return nil, false
	}
	return func() { _ = h.lockRepo.Release(context.Background(), sourceID, key, lockID) }, true
}

// ForceUnlock releases the app-level advisory lock on ?key= regardless of
// holder — the admin escape hatch for a lock orphaned by a crash that has not
// yet aged past the repository's stale-lock TTL. It does NOT touch native
// backend locks (local lock files, consul/http backend locks, HCP workspace
// locks); those are owned by the backend itself.
// @Summary      Force-unlock a state key (admin)
// @Description  Removes the app-level advisory lock for the key, whoever holds it. Native backend locks are unaffected. Requires admin.
// @Tags         Edit
// @Produce      json
// @Param        id   path   string  true  "Source ID"
// @Param        key  query  string  true  "State file key"
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /sources/{id}/state/lock [delete]
func (h *SourcesHandlers) ForceUnlock() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.Query("key")
		if key == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "key query parameter is required"})
			return
		}
		released, err := h.lockRepo.ForceRelease(c.Request.Context(), c.Param("id"), key)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to release lock"})
			return
		}
		h.audit.write(c, "state.force_unlock", "state", c.Param("id"),
			map[string]interface{}{"key": key, "released": released})
		c.JSON(http.StatusOK, gin.H{"released": released})
	}
}

func userIDOf(c *gin.Context) string {
	if v, ok := c.Get("user_id"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func (h *SourcesHandlers) recordEdit(ctx context.Context, sourceID, key, op, actor string, backupID *string, before, after *int64, result, detail string) {
	_ = h.editRepo.RecordEdit(ctx, &repositories.Edit{
		SourceID:     sourceID,
		StateKey:     key,
		Operation:    op,
		Actor:        actor,
		BackupID:     backupID,
		BeforeSerial: before,
		AfterSerial:  after,
		Result:       result,
		Detail:       detail,
	})
}
