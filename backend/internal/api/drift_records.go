// drift_records.go implements the durable drift-record plane layered over drift
// runs: persistent, acknowledgeable records of "this state is drifted", with a
// push-style ingest endpoint so pipelines TSM did not dispatch can still report
// plan results. Re-detections collapse onto the live record per state; clean
// results auto-resolve it.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/driftingest"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/notify"
)

// maxIngestPlanBytes caps the terraform show -json payload accepted by ingest.
const maxIngestPlanBytes = 5 << 20 // 5 MiB

// completeness carries the drift contract's five markers — what a check did NOT
// do — from a request body to the stored record. Grouped rather than passed as
// five more positional arguments so a marker cannot be threaded into the wrong
// slot, and so adding the next one touches the struct instead of every caller.
//
// Both receiving DTOs embed it, which is also what keeps the push endpoint and
// the dispatched-run callback accepting the identical vocabulary.
type completeness struct {
	Truncated      bool `json:"truncated"`
	OmittedEntries int  `json:"omitted_entries"`
	OmittedAttrs   int  `json:"omitted_attrs"`
	Unparseable    bool `json:"unparseable"`
	Unmasked       bool `json:"unmasked"`
}

// apply copies the markers onto a Detection bound for storage.
func (m completeness) apply(d *repositories.Detection) {
	d.Truncated, d.OmittedEntries, d.OmittedAttrs = m.Truncated, m.OmittedEntries, m.OmittedAttrs
	d.Unparseable, d.Unmasked = m.Unparseable, m.Unmasked
}

// fromResult replaces the sender's claims with the markers computed from a plan
// this server parsed itself. Server-side parsing is ground truth; a sender that
// also sent markers was describing a document we have now read directly.
func (m *completeness) fromResult(res *driftingest.Result) {
	m.Truncated, m.OmittedEntries, m.OmittedAttrs = res.Truncated(), res.OmittedEntries, res.OmittedAttrs
	m.Unparseable, m.Unmasked = res.Unparseable, res.Unmasked
}

// recordDriftOutcome maintains drift_records from a run result: a drifted
// completion upserts the live record for the state, a clean completion resolves
// it. Runs without a source_id + state_key cannot be mapped to a record (the
// pair is the record identity) and are skipped; failures don't touch records.
func (h *DriftHandlers) recordDriftOutcome(ctx context.Context, run *repositories.DriftRun, status string, added, changed, destroyed int, drifted bool, summary []byte, marks completeness) {
	if h.recordRepo == nil || status != "completed" || run.SourceID == nil || run.StateKey == "" {
		return
	}
	if drifted {
		d := &repositories.Detection{
			SourceID:             *run.SourceID,
			StateKey:             run.StateKey,
			PipelineConnectionID: run.PipelineConnectionID,
			RunID:                &run.ID,
			Origin:               "run",
			Added:                added,
			Changed:              changed,
			Destroyed:            destroyed,
			Summary:              summary,
		}
		marks.apply(d)
		if _, err := h.recordRepo.UpsertDetection(ctx, d); err != nil {
			driftLog.Error("failed to upsert drift record from run", "run", run.ID, "error", err)
		}
		return
	}
	// Zero counts from a document the producer could not read are ignorance, not
	// a clean result. Auto-resolving on one would let an unreadable plan silently
	// close a live drift record — the fail-open the marker exists to name. The
	// run itself is still recorded; the record is simply left as it was, so the
	// finding survives until something actually verifies it.
	if marks.Unparseable {
		driftLog.Warn("drift result was unparseable; leaving the record unresolved",
			"run", run.ID, "state", run.StateKey)
		return
	}
	if _, err := h.recordRepo.ResolveClean(ctx, *run.SourceID, run.StateKey); err != nil {
		driftLog.Error("failed to resolve drift record after clean run", "run", run.ID, "error", err)
	}
}

// driftIngestPayload is the body /drift/ingest accepts. Named (rather than
// declared inline) so the marker set is visible next to the callback's, and so
// tests can reflect over the keys this endpoint actually decodes.
//
// Decoding is deliberately lenient — see driftRunResultPayload for why.
type driftIngestPayload struct {
	SourceID    string          `json:"source_id"`
	StateKey    string          `json:"state_key"`
	ExternalRef string          `json:"external_ref"`
	Plan        json.RawMessage `json:"plan"`
	Added       int             `json:"added"`
	Changed     int             `json:"changed"`
	Destroyed   int             `json:"destroyed"`
	Drifted     *bool           `json:"drifted"`
	Summary     json.RawMessage `json:"summary"`
	Detail      string          `json:"detail"`
	// ModuleLocks is an optional .terraform/modules/modules.json upload; it
	// supplies resolved module versions the plan's configuration lacks.
	ModuleLocks json.RawMessage `json:"module_locks"`
	// Markers describing what the sender's own check did not do. Honoured when
	// the sender supplies counts/summary; overwritten by the server's own
	// reading when a raw plan is supplied instead.
	completeness
}

// IngestDrift accepts a drift result pushed by a pipeline TSM did not dispatch.
// The caller either supplies the parsed counts/summary or a raw
// `terraform show -json` plan (parsed server-side with the same semantics the
// dispatched workflows compute with jq). external_ref (e.g. the CI run id)
// makes retries idempotent.
// @Summary      Ingest drift result (push)
// @Description  Pipelines TSM did not dispatch POST plan results here. Supply counts/summary or a raw `terraform show -json` plan. external_ref deduplicates retries. Requires state:drift. Accepts the drift contract's completeness markers (truncated, omitted_entries, omitted_attrs, unparseable, unmasked) and stores them on the record; a raw plan supplied here is parsed server-side and its markers win. An unparseable result never auto-resolves a record: a raw plan that cannot be parsed answers 422, and a sender-reported unparseable answers 200 with status "unverified".
// @Tags         Drift
// @Accept       json
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /drift/ingest [post]
func (h *DriftHandlers) IngestDrift() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		raw, err := io.ReadAll(io.LimitReader(c.Request.Body, maxIngestPlanBytes+1))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
			return
		}
		if len(raw) > maxIngestPlanBytes {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "payload exceeds 5 MiB; send counts/summary instead of the full plan"})
			return
		}
		var req driftIngestPayload
		if err := json.Unmarshal(raw, &req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
			return
		}
		if req.SourceID == "" || req.StateKey == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "source_id and state_key are required"})
			return
		}
		src, err := h.sourceRepo.GetByID(ctx, req.SourceID)
		if err != nil {
			serverError(c, err, "failed to load source")
			return
		}
		if src == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
			return
		}

		// Idempotency: a replayed external_ref returns the existing record.
		if req.ExternalRef != "" {
			if existing, err := h.recordRepo.GetByExternalRef(ctx, req.SourceID, req.ExternalRef); err == nil && existing != nil {
				c.JSON(http.StatusOK, gin.H{"record": existing, "replay": true})
				return
			}
		}

		added, changed, destroyed := req.Added, req.Changed, req.Destroyed
		summary := req.Summary
		drifted := added+changed+destroyed > 0 || len(summary) > 2 // "[]" is clean
		if len(req.Plan) > 0 {
			var plan driftingest.Plan
			if err := json.Unmarshal(req.Plan, &plan); err != nil {
				c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "plan is not valid terraform show -json output"})
				return
			}
			res := driftingest.Summarize(&plan)
			// A document that parses as JSON but carries no resource_changes array
			// is not `terraform show -json` output — a truncated show, the wrong
			// file, or a broken pipeline step. It used to be recorded as a clean
			// drift record, indistinguishable from a verified-clean plan, which is
			// a false negative on the signal this endpoint exists to store. The
			// error message below was already the right one; it just never covered
			// this case.
			if res.Unparseable {
				c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "plan is not valid terraform show -json output"})
				return
			}
			added, changed, destroyed = res.Added, res.Changed, res.Destroyed
			summary, _ = json.Marshal(res.Summary)
			drifted = res.Drifted()
			// We read the plan ourselves, so our markers replace whatever the
			// sender claimed about it.
			req.completeness.fromResult(res)
			// Best-effort: capture registry-module provenance from the plan's
			// configuration block. Never fails the ingest — the drift record is the
			// primary product; provenance powers the optional "modules in use" /
			// "consumed by" views.
			h.captureModuleRefs(ctx, req.SourceID, req.StateKey, &plan, req.ModuleLocks)
		}
		if req.Drifted != nil {
			drifted = *req.Drifted
		}

		var extRef *string
		if req.ExternalRef != "" {
			extRef = &req.ExternalRef
		}

		if !drifted {
			// A sender that could not read its own plan has reported ignorance,
			// not cleanliness. Resolving the live record on that would let an
			// unreadable plan close an open finding — indistinguishable, once
			// stored, from a verified-clean check. A raw plan we parse ourselves
			// already answers 422 above; this is the same rule for a sender that
			// did its own parsing and told us it failed. The result is accepted
			// (200, so a forward-compatible sender is not broken) but nothing is
			// resolved and the record is left exactly as it was.
			if req.Unparseable {
				h.audit.write(c, "drift.ingest", "drift_record", req.SourceID,
					map[string]interface{}{"state_key": req.StateKey, "unparseable": true, "resolved": false, "external_ref": req.ExternalRef})
				c.JSON(http.StatusOK, gin.H{"status": "unverified", "resolved": false,
					"reason": "result reported unparseable; the drift record was left unresolved"})
				return
			}
			resolved, err := h.recordRepo.ResolveClean(ctx, req.SourceID, req.StateKey)
			if err != nil {
				serverError(c, err, "failed to record clean result")
				return
			}
			h.audit.write(c, "drift.ingest", "drift_record", req.SourceID,
				map[string]interface{}{"state_key": req.StateKey, "drifted": false, "resolved": resolved, "external_ref": req.ExternalRef})
			c.JSON(http.StatusOK, gin.H{"status": "clean", "resolved": resolved})
			return
		}

		det := &repositories.Detection{
			SourceID:    req.SourceID,
			StateKey:    req.StateKey,
			Origin:      "ingest",
			Added:       added,
			Changed:     changed,
			Destroyed:   destroyed,
			Summary:     summary,
			ExternalRef: extRef,
		}
		req.completeness.apply(det)
		rec, err := h.recordRepo.UpsertDetection(ctx, det)
		if err != nil {
			serverError(c, err, "failed to record drift")
			return
		}
		h.audit.write(c, "drift.ingest", "drift_record", rec.ID,
			map[string]interface{}{"state_key": req.StateKey, "drifted": true, "severity": rec.Severity, "external_ref": req.ExternalRef})
		h.notifyIngestedDrift(src.Name, req.StateKey, added, changed, destroyed)
		c.JSON(http.StatusOK, gin.H{"record": rec})
	}
}

// captureModuleRefs persists the registry-module provenance found in an ingested
// plan, replacing any prior refs for the (source, state). Best-effort: a failure
// is logged, never surfaced — the drift record is the primary product.
func (h *DriftHandlers) captureModuleRefs(ctx context.Context, sourceID, stateKey string, plan *driftingest.Plan, moduleLocks []byte) {
	refs := driftingest.ModuleRefs(plan, driftingest.ParseModuleLocks(moduleLocks))
	rows := make([]repositories.StateModuleRef, len(refs))
	for i, m := range refs {
		rows[i] = repositories.StateModuleRef{
			ModuleSource:  m.ModuleSource,
			RegistryHost:  m.RegistryHost,
			ModuleVersion: m.ModuleVersion,
		}
	}
	if err := h.moduleRefRepo.ReplaceForState(ctx, sourceID, stateKey, rows); err != nil {
		driftLog.Warn("failed to capture module provenance",
			"source_id", sourceID, "state_key", stateKey, "error", err)
	}
}

// notifyIngestedDrift mirrors notifyDriftResult for the push path (detached,
// nil-safe). The dispatched path already notifies from the run callback.
func (h *DriftHandlers) notifyIngestedDrift(sourceName, stateKey string, added, changed, destroyed int) {
	if h.notifier == nil {
		return
	}
	ev := notify.Event{
		Type:    notify.EventDriftDetected,
		Title:   "Drift detected",
		Message: fmt.Sprintf("Ingested drift on %s/%s (+%d ~%d -%d).", sourceName, stateKey, added, changed, destroyed),
	}
	go func(ev notify.Event) {
		ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
		defer cancel()
		h.notifier.Notify(ctx, ev)
	}(ev)
}

// ListDriftRecords returns drift records, filterable by status/source/severity
// and a last-detected date range, windowed by page/per_page. ?status= is
// comma-separated; default returns every status. counts stays the global
// by-status tally (the status chips); total is the filtered count for paging.
// @Summary      List drift records
// @Tags         Drift
// @Produce      json
// @Param        status      query  string  false  "comma-separated: open, acknowledged, resolved"
// @Param        source_id   query  string  false  "filter by source"
// @Param        severity    query  string  false  "filter by severity"
// @Param        page        query  int     false  "page (default 1)"
// @Param        per_page    query  int     false  "page size (default 100, max 500)"
// @Param        start_date  query  string  false  "RFC3339 last-detected lower bound"
// @Param        end_date    query  string  false  "RFC3339 last-detected upper bound"
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /drift/records [get]
func (h *DriftHandlers) ListDriftRecords() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		var statuses []string
		if s := c.Query("status"); s != "" {
			for _, part := range splitCSV(s) {
				if part != "open" && part != "acknowledged" && part != "resolved" {
					c.JSON(http.StatusBadRequest, gin.H{"error": "status must be open, acknowledged, or resolved"})
					return
				}
				statuses = append(statuses, part)
			}
		}
		perPage := 100
		if v, err := strconv.Atoi(c.Query("per_page")); err == nil && v > 0 && v <= 500 {
			perPage = v
		}
		page := 1
		if v, err := strconv.Atoi(c.Query("page")); err == nil && v > 0 {
			page = v
		}
		// Unparsable dates are ignored rather than erroring, mirroring the
		// audit-log filters.
		var start, end *time.Time
		if v := c.Query("start_date"); v != "" {
			if ts, err := time.Parse(time.RFC3339, v); err == nil {
				start = &ts
			}
		}
		if v := c.Query("end_date"); v != "" {
			if ts, err := time.Parse(time.RFC3339, v); err == nil {
				end = &ts
			}
		}

		sourceID, severity := c.Query("source_id"), c.Query("severity")
		records, err := h.recordRepo.List(ctx, statuses, sourceID, severity, perPage, (page-1)*perPage, start, end)
		if err != nil {
			serverError(c, err, "failed to list drift records")
			return
		}
		total, err := h.recordRepo.CountRecords(ctx, statuses, sourceID, severity, start, end)
		if err != nil {
			serverError(c, err, "failed to list drift records")
			return
		}
		counts, err := h.recordRepo.CountsByStatus(ctx)
		if err != nil {
			serverError(c, err, "failed to list drift records")
			return
		}
		c.JSON(http.StatusOK, gin.H{"records": records, "counts": counts, "total": total})
	}
}

// GetDriftRecord returns one drift record.
func (h *DriftHandlers) GetDriftRecord() gin.HandlerFunc {
	return func(c *gin.Context) {
		rec, err := h.recordRepo.GetByID(c.Request.Context(), c.Param("id"))
		if err != nil {
			serverError(c, err, "failed to load drift record")
			return
		}
		if rec == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "drift record not found"})
			return
		}
		c.JSON(http.StatusOK, rec)
	}
}

// AcknowledgeDriftRecord marks an open record acknowledged with an optional note.
// @Summary      Acknowledge drift record
// @Description  Marks an open drift record acknowledged (who/when + optional note). Re-detections keep the acknowledgement. Requires state:drift.
// @Tags         Drift
// @Accept       json
// @Produce      json
// @Param        id  path  string  true  "Drift record ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      409  {object}  map[string]interface{}  "record is not open"
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /drift/records/{id}/acknowledge [post]
func (h *DriftHandlers) AcknowledgeDriftRecord() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Note string `json:"note"`
		}
		_ = c.ShouldBindJSON(&req) // body optional
		if len(req.Note) > 1000 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "note must be 1000 characters or fewer"})
			return
		}
		rec, err := h.recordRepo.Acknowledge(c.Request.Context(), c.Param("id"), userIDOf(c), req.Note)
		if err != nil {
			serverError(c, err, "failed to acknowledge drift record")
			return
		}
		if rec == nil {
			// Missing vs not-open: look it up to answer precisely.
			existing, gErr := h.recordRepo.GetByID(c.Request.Context(), c.Param("id"))
			if gErr == nil && existing != nil {
				c.JSON(http.StatusConflict, gin.H{"error": "drift record is not open (status: " + existing.Status + ")"})
				return
			}
			c.JSON(http.StatusNotFound, gin.H{"error": "drift record not found"})
			return
		}
		h.audit.write(c, "drift_record.acknowledge", "drift_record", rec.ID,
			map[string]interface{}{"state_key": rec.StateKey, "note": req.Note})
		c.JSON(http.StatusOK, rec)
	}
}

// ResolveDriftRecord manually closes a record (drift remediated out-of-band).
func (h *DriftHandlers) ResolveDriftRecord() gin.HandlerFunc {
	return func(c *gin.Context) {
		rec, err := h.recordRepo.Resolve(c.Request.Context(), c.Param("id"))
		if err != nil {
			serverError(c, err, "failed to resolve drift record")
			return
		}
		if rec == nil {
			existing, gErr := h.recordRepo.GetByID(c.Request.Context(), c.Param("id"))
			if gErr == nil && existing != nil {
				c.JSON(http.StatusConflict, gin.H{"error": "drift record is already resolved"})
				return
			}
			c.JSON(http.StatusNotFound, gin.H{"error": "drift record not found"})
			return
		}
		h.audit.write(c, "drift_record.resolve", "drift_record", rec.ID,
			map[string]interface{}{"state_key": rec.StateKey})
		c.JSON(http.StatusOK, rec)
	}
}
