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
	"strconv"

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
				serverError(c, bErr, "failed to back up current state")
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
			serverError(c, bErr, "failed to back up current state")
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
		serverError(c, bErr, "failed to back up current state before delete")
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

// ListBackups returns the backup history for a state file (?key=), paginated via
// ?page (1-indexed) and ?per_page (default 100, max 500) so a frequently-edited
// key with an unbounded backup history is never returned in full in one response
// (#262).
func (h *SourcesHandlers) ListBackups() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.Query("key")
		if key == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "key query parameter is required"})
			return
		}
		perPage := 100
		if v, err := strconv.Atoi(c.Query("per_page")); err == nil && v > 0 && v <= 500 {
			perPage = v
		}
		page := 1
		// Bound page so (page-1)*perPage cannot overflow int on a crafted value.
		if v, err := strconv.Atoi(c.Query("page")); err == nil && v > 0 && v <= 1_000_000 {
			page = v
		}
		backups, err := h.editRepo.ListBackups(c.Request.Context(), c.Param("id"), key, perPage, (page-1)*perPage)
		if err != nil {
			serverError(c, err, "failed to list backups")
			return
		}
		c.JSON(http.StatusOK, gin.H{"backups": backups})
	}
}

// GetBackupContent streams a stored backup's full state JSON, so operators can
// inspect what a restore would write. Backup.Data is excluded from JSON
// marshalling (json:"-"), hence the raw c.Data response. Unlike RestoreBackup's
// 400, cross-source access reads as 404 — reads must not leak that a backup id
// exists under another source.
// @Summary      Get backup content
// @Description  The full state JSON stored in a backup.
// @Tags         Edit
// @Produce      json
// @Param        id        path  string  true  "Source ID"
// @Param        backupId  path  string  true  "Backup ID"
// @Success      200  {string}  string  "state JSON"
// @Failure      404  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /sources/{id}/state/backups/{backupId} [get]
func (h *SourcesHandlers) GetBackupContent() gin.HandlerFunc {
	return func(c *gin.Context) {
		backup, ok := h.loadOwnedBackup(c)
		if !ok {
			return
		}
		c.Data(http.StatusOK, "application/json; charset=utf-8", backup.Data)
	}
}

// DiffBackup previews a restore: which resources restoring the backup would
// re-create (added), drop (removed), or alter (changed) relative to the current
// state. "Changed" is an instance-level approximation — the analyzer summarises
// instance counts and keys, not attribute values — flagged by
// approximate_changed in the response.
// @Summary      Preview a backup restore
// @Description  Resource-level diff of the backup against the current state, phrased as what restoring would do. The changed bucket is an instance-count/key approximation, not an attribute diff.
// @Tags         Edit
// @Produce      json
// @Param        id        path  string  true  "Source ID"
// @Param        backupId  path  string  true  "Backup ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /sources/{id}/state/backups/{backupId}/diff [get]
func (h *SourcesHandlers) DiffBackup() gin.HandlerFunc {
	return func(c *gin.Context) {
		backup, err := h.editRepo.GetBackup(c.Request.Context(), c.Param("backupId"))
		if err != nil {
			serverError(c, err, "failed to load backup")
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
			// 404, not 400: reads must not leak that the id exists elsewhere.
			c.JSON(http.StatusNotFound, gin.H{"error": "backup not found"})
			return
		}
		ctx := c.Request.Context()

		backupRes, err := analyzer.ListResources(backup.Data)
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "backup is not valid state JSON: " + err.Error()})
			return
		}

		// Same fail-closed rule as RestoreBackup: a transient read failure means
		// the current state is unknown; only a genuine not-found diffs against
		// an empty state (restore would re-create everything).
		var currentRes []analyzer.ResourceSummary
		var currentSerial *int64
		cur, readErr := conn.Read(ctx, backup.StateKey)
		if readErr != nil && !statesource.IsNotFound(readErr) {
			c.JSON(http.StatusBadGateway, gin.H{"error": "cannot read current state: " + readErr.Error()})
			return
		}
		if readErr == nil && cur != nil {
			if currentRes, err = analyzer.ListResources(cur.Data); err != nil {
				c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "current state is not valid state JSON: " + err.Error()})
				return
			}
			if curA, aErr := analyzer.Analyze(cur.Data); aErr == nil {
				s := curA.Serial
				currentSerial = &s
			}
		}

		// Identity is module+mode+type+name (ResourceSummary carries no address).
		key := func(r analyzer.ResourceSummary) string {
			return r.Module + "\x00" + r.Mode + "\x00" + r.Type + "\x00" + r.Name
		}
		currentBy := make(map[string]analyzer.ResourceSummary, len(currentRes))
		for _, r := range currentRes {
			currentBy[key(r)] = r
		}
		added := make([]analyzer.ResourceSummary, 0)
		changed := make([]analyzer.ResourceSummary, 0)
		removed := make([]analyzer.ResourceSummary, 0)
		inBackup := make(map[string]struct{}, len(backupRes))
		for _, b := range backupRes {
			inBackup[key(b)] = struct{}{}
			cr, exists := currentBy[key(b)]
			switch {
			case !exists:
				added = append(added, b)
			case cr.Instances != b.Instances || fmt.Sprint(cr.InstanceKeys) != fmt.Sprint(b.InstanceKeys):
				changed = append(changed, b) // reported as the post-restore shape
			}
		}
		for _, r := range currentRes {
			if _, exists := inBackup[key(r)]; !exists {
				removed = append(removed, r)
			}
		}

		c.JSON(http.StatusOK, gin.H{
			"key":                 backup.StateKey,
			"backup_serial":       backup.Serial,
			"current_serial":      currentSerial,
			"added":               added,
			"removed":             removed,
			"changed":             changed,
			"approximate_changed": true,
		})
	}
}

// loadOwnedBackup fetches :backupId and enforces that it belongs to :id,
// answering 404 on missing or cross-source access (see GetBackupContent).
func (h *SourcesHandlers) loadOwnedBackup(c *gin.Context) (*repositories.Backup, bool) {
	backup, err := h.editRepo.GetBackup(c.Request.Context(), c.Param("backupId"))
	if err != nil {
		serverError(c, err, "failed to load backup")
		return nil, false
	}
	if backup == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "backup not found"})
		return nil, false
	}
	src, err := h.repo.GetByID(c.Request.Context(), c.Param("id"))
	if err != nil {
		serverError(c, err, "failed to load source")
		return nil, false
	}
	if src == nil || backup.SourceID != src.ID {
		c.JSON(http.StatusNotFound, gin.H{"error": "backup not found"})
		return nil, false
	}
	return backup, true
}

// RestoreBackup writes a stored backup back to its source/key (after backing up
// the current version first).
func (h *SourcesHandlers) RestoreBackup() gin.HandlerFunc {
	return func(c *gin.Context) {
		backup, err := h.editRepo.GetBackup(c.Request.Context(), c.Param("backupId"))
		if err != nil {
			serverError(c, err, "failed to load backup")
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
				serverError(c, bErr, "failed to back up current state")
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
		serverError(c, err, "failed to load source")
		return nil, nil, false
	}
	if s == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
		return nil, nil, false
	}
	creds, err := decryptCredentials(s)
	if err != nil {
		serverError(c, err, "failed to decrypt source credentials")
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
	// Heartbeat while the operation runs: the stale reap keys on renewed_at, so
	// a slow backend (a >15min transfer against a rate-limited store) is never
	// mistaken for a crashed holder, while a real crash stops renewing.
	stop := make(chan struct{})
	go h.lockRepo.KeepAlive(sourceID, key, lockID, repositories.LockRenewInterval, stop)
	return func() {
		close(stop)
		_ = h.lockRepo.Release(context.Background(), sourceID, key, lockID)
	}, true
}

// ListLocks returns the app-level advisory locks currently held for a source,
// so operators can see who holds what (and how stale it is) before reaching
// for the force-unlock escape hatch. Native backend locks (local lock files,
// consul/http, HCP workspace locks) are not visible here — they are owned by
// the backend itself (see ADR 003). An unknown source id yields an empty list,
// matching ListBackups.
// @Summary      List active state locks
// @Description  App-level advisory locks currently held for the source, newest first. Native backend locks are not included.
// @Tags         Edit
// @Produce      json
// @Param        id  path  string  true  "Source ID"
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /sources/{id}/state/locks [get]
func (h *SourcesHandlers) ListLocks() gin.HandlerFunc {
	return func(c *gin.Context) {
		locks, err := h.lockRepo.List(c.Request.Context(), c.Param("id"))
		if err != nil {
			serverError(c, err, "failed to list locks")
			return
		}
		c.JSON(http.StatusOK, gin.H{"locks": locks})
	}
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
			serverError(c, err, "failed to release lock")
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
