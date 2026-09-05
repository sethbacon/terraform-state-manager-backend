// drift.go implements the Phase 3 drift plane: CI pipeline connections, drift-run
// dispatch (GitHub Actions / Azure DevOps), and the signed result callback the CI
// job posts back to. The app never runs terraform or holds cloud credentials.
package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/pipelines"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/driftingest"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/notify"
)

// DriftHandlers serves pipeline-connection, drift-run, and drift-record endpoints.
type DriftHandlers struct {
	cfg           *config.Config
	pipelineRepo  *repositories.PipelineRepository
	ciSourceRepo  *repositories.CISourceRepository
	driftRepo     *repositories.DriftRepository
	recordRepo    *repositories.DriftRecordRepository
	sourceRepo    *repositories.SourceRepository
	moduleRefRepo *repositories.StateModuleRefRepository
	audit         auditor
	notifier      *notify.Notifier // may be nil (notifications disabled / no DB)
	// orgs verifies that an acting organization exists before a row is stamped
	// with it. Wired by the router from its single approles.Members; nil is
	// refused rather than skipped (see actingOrganization).
	orgs organizationExistence
}

// AttachOrganizations wires the existence check used before a row is stamped
// with an acting organization (#436). A setter, and the router supplies its
// EXISTING approles.Members: internal/approles' guard test refuses a second
// construction of the shared organization repository.
func (h *DriftHandlers) AttachOrganizations(orgs organizationExistence) { h.orgs = orgs }

// NewDriftHandlers constructs the handlers over the app (public) connection.
// identityDB (may be nil) carries the shared audit log; the notifier (may be
// nil) fires alerts when a result callback reports drift/failure.
func NewDriftHandlers(cfg *config.Config, database, identityDB *sql.DB, notifier *notify.Notifier) *DriftHandlers {
	return &DriftHandlers{
		cfg:           cfg,
		pipelineRepo:  repositories.NewPipelineRepository(database),
		ciSourceRepo:  repositories.NewCISourceRepository(database),
		driftRepo:     repositories.NewDriftRepository(database),
		recordRepo:    repositories.NewDriftRecordRepository(database),
		sourceRepo:    repositories.NewSourceRepository(database),
		moduleRefRepo: repositories.NewStateModuleRefRepository(database),
		audit:         newAuditor(identityDB),
		notifier:      notifier,
	}
}

// driftLog tags drift-record maintenance logs.
var driftLog = slog.With("component", "drift")

// notifyTimeout bounds detached notification sends.
const notifyTimeout = 15 * time.Second

// splitCSV splits a comma-separated query value, trimming blanks.
func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func randomToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		// A failure of the system CSPRNG is catastrophic and must never yield a
		// predictable (all-zero) callback token; fail loudly instead.
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// --- Pipeline connections ---

// ListPipelines returns configured CI connections (no secrets).
func (h *DriftHandlers) ListPipelines() gin.HandlerFunc {
	return func(c *gin.Context) {
		// The Phase 3 read flip for pipeline_connections (#393). Unscoped, this
		// listed every organization's CI connections to any caller holding
		// sources:manage in one of them -- including each connection's config,
		// which names its repository coordinates and its CI source.
		//
		// An UNRESOLVED scope is a 500, never an empty one and certainly never
		// a full one: it means the route was registered without
		// middleware.TenantScope.
		scope, resolved := tenantscope.FromContext(c)
		if !resolved {
			serverError(c, errNoTenantScope, "the tenant scope was not resolved for this route")
			return
		}
		conns, err := h.pipelineRepo.ListInScope(c.Request.Context(), scope)
		if err != nil {
			serverError(c, err, "failed to list pipelines")
			return
		}
		c.JSON(http.StatusOK, gin.H{"pipelines": conns})
	}
}

// CreatePipeline registers a CI connection, encrypting its token.
func (h *DriftHandlers) CreatePipeline() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Name     string         `json:"name" binding:"required"`
			Provider string         `json:"provider" binding:"required"`
			Config   map[string]any `json:"config"`
			Token    string         `json:"token"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name and provider are required"})
			return
		}
		if req.Provider != "github_actions" && req.Provider != "azure_devops" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "provider must be github_actions or azure_devops"})
			return
		}
		if !fanOutConfigIsValid(req.Config) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "config.fan_out must be a boolean"})
			return
		}
		pc := &repositories.PipelineConnection{Name: req.Name, Provider: req.Provider, Config: req.Config}
		if req.Token != "" {
			if !crypto.Available() {
				c.JSON(http.StatusBadRequest, gin.H{"error": "cannot store token: encryption key not configured (set TSM_ENCRYPTION_KEY)"})
				return
			}
			enc, err := crypto.EncryptFor([]byte(req.Token), crypto.PurposePipelineDispatchToken)
			if err != nil {
				serverError(c, err, "failed to encrypt token")
				return
			}
			pc.EncryptedToken = enc
		}
		// The organization this row belongs to, named by the request and verified
		// against a scope this server resolved (#436). Writes the response
		// itself on every failure path.
		orgID := actingOrganization(c, h.orgs)
		if orgID == "" {
			return
		}

		// THE WRITE-SIDE LINKAGE INVARIANT (#393): a connection may only
		// reference a CI source its own organization owns. The dispatch chain
		// fails closed on a crossing reference, but refusing it here -- before
		// the row exists -- is what keeps refused rows from accumulating as
		// schedules and connections that can never fire.
		if !h.ciSourceReferenceInOrganization(c, pc.Config, orgID) {
			return
		}

		saved, err := h.pipelineRepo.Create(c.Request.Context(), pc, orgID)
		if err != nil {
			serverError(c, err, "failed to create pipeline connection")
			return
		}
		h.audit.write(c, "pipeline.create", "pipeline_connection", saved.ID,
			map[string]interface{}{"name": saved.Name, "provider": saved.Provider})
		c.JSON(http.StatusCreated, saved)
	}
}

// UpdatePipeline edits a connection's name and config. The provider is immutable
// and the stored token is rotated only when a new one is supplied.
func (h *DriftHandlers) UpdatePipeline() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		var req struct {
			Name   string         `json:"name" binding:"required"`
			Config map[string]any `json:"config"`
			Token  string         `json:"token"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
			return
		}
		if !fanOutConfigIsValid(req.Config) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "config.fan_out must be a boolean"})
			return
		}
		pc := &repositories.PipelineConnection{ID: id, Name: req.Name, Config: req.Config}
		updateToken := req.Token != ""
		if updateToken {
			if !crypto.Available() {
				c.JSON(http.StatusBadRequest, gin.H{"error": "cannot store token: encryption key not configured (set TSM_ENCRYPTION_KEY)"})
				return
			}
			enc, err := crypto.EncryptFor([]byte(req.Token), crypto.PurposePipelineDispatchToken)
			if err != nil {
				serverError(c, err, "failed to encrypt token")
				return
			}
			pc.EncryptedToken = enc
		}
		scope, resolved := tenantscope.FromContext(c)
		if !resolved {
			serverError(c, errNoTenantScope, "the tenant scope was not resolved for this route")
			return
		}
		// THE WRITE-SIDE LINKAGE INVARIANT (#393): the referenced CI source must
		// belong to the CONNECTION'S organization -- the row's, not the
		// caller's, because a multi-organization caller may reach both while
		// the dispatch chain later runs under the row's organization alone.
		// The row is loaded in the caller's scope first, so a connection in
		// another organization stays a plain 404.
		existing, err := h.pipelineRepo.GetByIDInScope(c.Request.Context(), id, scope)
		if err != nil {
			serverError(c, err, "failed to load pipeline connection")
			return
		}
		if existing == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "pipeline connection not found"})
			return
		}
		if !h.ciSourceReferenceInOrganization(c, pc.Config, existing.OrganizationID) {
			return
		}
		// Scoped: this can REPLACE the stored CI token, so an unscoped update is
		// a credential overwrite on another tenant's connection.
		saved, err := h.pipelineRepo.UpdateInScope(c.Request.Context(), pc, updateToken, scope)
		if errors.Is(err, repositories.ErrNotInScope) {
			c.JSON(http.StatusNotFound, gin.H{"error": "pipeline connection not found"})
			return
		}
		if err != nil {
			serverError(c, err, "failed to update pipeline connection")
			return
		}
		if saved == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "pipeline connection not found"})
			return
		}
		h.audit.write(c, "pipeline.update", "pipeline_connection", saved.ID,
			map[string]interface{}{"name": saved.Name})
		c.JSON(http.StatusOK, saved)
	}
}

// DeletePipeline removes a CI connection.
func (h *DriftHandlers) DeletePipeline() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		scope, resolved := tenantscope.FromContext(c)
		if !resolved {
			serverError(c, errNoTenantScope, "the tenant scope was not resolved for this route")
			return
		}
		deleted, err := h.pipelineRepo.DeleteInScope(c.Request.Context(), id, scope)
		if errors.Is(err, repositories.ErrNotInScope) || (err == nil && !deleted) {
			c.JSON(http.StatusNotFound, gin.H{"error": "pipeline connection not found"})
			return
		}
		if err != nil {
			serverError(c, err, "failed to delete pipeline connection")
			return
		}
		h.audit.write(c, "pipeline.delete", "pipeline_connection", id, nil)
		c.Status(http.StatusNoContent)
	}
}

// --- Drift runs ---

// CreateRun dispatches a drift run on the chosen pipeline and records it. A
// request naming 2+ targets (fan-out) plans them all in one CI job and
// records one drift_runs row per target under a shared batch_id; a request
// with no targets (or exactly one) is the legacy single-target shape and gets
// a byte-identical dispatch to before.
// @Summary      Dispatch drift run
// @Description  Dispatches a terraform-plan drift run on the chosen CI pipeline and records it. Inputs are validated server-side. Requires state:drift. A request naming 2+ items in `targets` plans them all in one CI job (requires the pipeline connection's config.fan_out=true) and returns {batch_id, runs}; omitting `targets` (or naming exactly one) is the legacy single-target shape and returns a single run.
// @Tags         Drift
// @Accept       json
// @Produce      json
// @Success      202  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      502  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /drift/runs [post]
func (h *DriftHandlers) CreateRun() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			PipelineConnectionID string            `json:"pipeline_connection_id" binding:"required"`
			SourceID             string            `json:"source_id"`
			StateKey             string            `json:"state_key"`
			RepoRef              string            `json:"repo_ref"`
			WorkingDir           string            `json:"working_dir"`
			Targets              []DriftTargetItem `json:"targets"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "pipeline_connection_id is required"})
			return
		}
		// repo_ref is shared by the whole request (not per-item); validated once,
		// here, rather than once per item inside validateDriftTargets.
		if err := validatePipelineInputs("", req.RepoRef, "", "", nil, nil); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		tgt := DriftTarget{
			PipelineConnectionID: req.PipelineConnectionID,
			SourceID:             req.SourceID,
			StateKey:             req.StateKey,
			RepoRef:              req.RepoRef,
			WorkingDir:           req.WorkingDir,
			Targets:              req.Targets,
		}
		items := tgt.items()
		if err := validateDriftTargets(items); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		// A user request: the organization is the one the caller is acting in.
		orgID := actingOrganization(c, h.orgs)
		if orgID == "" {
			return
		}

		batch, err := h.dispatchDriftBatch(c.Request.Context(), tgt, userIDOf(c), requestAuthority(orgID))
		if batch != nil {
			for _, run := range batch.Runs {
				h.audit.write(c, "drift_run.dispatch", "drift_run", run.ID, map[string]interface{}{
					"pipeline_connection_id": req.PipelineConnectionID,
					"state_key":              run.StateKey,
					"status":                 run.Status,
					"batch_id":               batch.BatchID,
					"targets":                len(items),
				})
			}
		}
		switch {
		case errors.Is(err, errPipelineNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "pipeline connection not found"})
		case errors.Is(err, errFanOutNotCapable):
			c.JSON(http.StatusBadRequest, gin.H{"error": errFanOutNotCapable.Error()})
		case err != nil && batch != nil:
			// The run(s) were recorded but the CI dispatch failed; return the detail.
			c.JSON(http.StatusBadGateway, driftBatchResponse(batch))
		case err != nil:
			serverError(c, err, "failed to dispatch drift run")
		default:
			c.JSON(http.StatusAccepted, driftBatchResponse(batch))
		}
	}
}

// driftBatchResponse renders a dispatch outcome on the wire: a single DriftRun
// (the same keys the legacy single-target response always had, plus
// batch_id/ci_run_id/ci_run_url) for a request that resolved to exactly one
// target, or {"batch_id","runs"} for 2+. This is what keeps a no-targets (or
// one-item) request's response shape unchanged.
func driftBatchResponse(batch *DriftBatch) any {
	if len(batch.Runs) == 1 {
		return batch.Runs[0]
	}
	return gin.H{"batch_id": batch.BatchID, "runs": batch.Runs}
}

// DriftTarget is the input for dispatching a drift run. Shared by the HTTP handler
// and the scheduler (decoded from a schedule's target_config).
//
// Targets carries a repo-level fan-out request: 2+ states planned by one CI
// job, each reporting its own callback. PipelineConnectionID and RepoRef are
// shared by every item (they name the ONE pipeline/ref the whole batch runs
// on); SourceID/StateKey/WorkingDir at the top level are the legacy
// single-target shape, read only when Targets is empty. items() is the single
// place that resolves the two into one list.
type DriftTarget struct {
	PipelineConnectionID string            `json:"pipeline_connection_id"`
	SourceID             string            `json:"source_id"`
	StateKey             string            `json:"state_key"`
	RepoRef              string            `json:"repo_ref"`
	WorkingDir           string            `json:"working_dir"`
	Targets              []DriftTargetItem `json:"targets,omitempty"`
}

// DriftTargetItem is one target (one state) inside a fan-out DriftTarget.
type DriftTargetItem struct {
	SourceID   string `json:"source_id"`
	StateKey   string `json:"state_key"`
	WorkingDir string `json:"working_dir"`
}

// items returns the targets to dispatch: t.Targets when it carries 1+ items,
// else the single legacy triple synthesized from the top-level fields. ONE
// CODE PATH for both shapes -- every caller (validation, dispatch, the
// write-side organization check) ranges over items() rather than branching on
// whether Targets was set, which is what keeps the legacy shape from silently
// drifting out of step with the fan-out one as this file changes.
func (t DriftTarget) items() []DriftTargetItem {
	if len(t.Targets) > 0 {
		return t.Targets
	}
	return []DriftTargetItem{{SourceID: t.SourceID, StateKey: t.StateKey, WorkingDir: t.WorkingDir}}
}

var errPipelineNotFound = errors.New("pipeline connection not found")

// errCISourceNotReachable reports a connection whose config names a CI source
// the dispatching organization cannot reach. Surfaced to HTTP callers only as a
// generic 500 (the connection is theirs; its reference is poisoned), while the
// wrapped chain error carries the provenance an operator needs.
var errCISourceNotReachable = errors.New("the connection's CI source is not reachable in the dispatching organization")

// ciSourceReferenceInOrganization enforces the write-side linkage invariant for
// pipeline connections (#393): config.ci_source_id, when present, must name a
// CI source owned by organizationID. Writes the refusal itself and reports
// whether the caller may proceed.
//
// The refusal does not say whether the id exists elsewhere -- "does not name a
// CI source in your organization" covers a typo and a crossing reference in the
// same words, deliberately.
func (h *DriftHandlers) ciSourceReferenceInOrganization(c *gin.Context, config map[string]any, organizationID string) bool {
	refID, _ := config["ci_source_id"].(string)
	if strings.TrimSpace(refID) == "" {
		return true
	}
	src, err := h.ciSourceRepo.GetByIDInScope(c.Request.Context(), refID, organizationScope(organizationID))
	if err != nil {
		serverError(c, err, "failed to verify the connection's CI source")
		return false
	}
	if src == nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "config.ci_source_id does not name a CI source in this connection's organization",
		})
		return false
	}
	return true
}

// fanOutConfigIsValid enforces fan_out validated-at-write-time (drift-fleet-scale.md
// Phase 1): config.fan_out, when present, must be a JSON boolean. Absent is
// valid (reads as false via pipelines.FanOutFromMap); present but not a bool
// (a string "true", a number, null) is refused here so it can never reach a
// stored row and be misread as an operator's deliberate opt-in later.
func fanOutConfigIsValid(config map[string]any) bool {
	raw, ok := config["fan_out"]
	if !ok {
		return true
	}
	_, isBool := raw.(bool)
	return isBool
}

// DriftBatch is the outcome of dispatching a DriftTarget: one CI job planning
// N targets, each with its own drift_runs row, its own one-shot callback
// token and its own TTL.
//
// BatchID is the row-grouping id shared by every row in Runs when N > 1
// (batch_id on the rows themselves); for N == 1 it is simply that one run's
// own id, and the row's own batch_id column stays NULL. This is what keeps
// schedules.last_run_id (fed BatchID by driftDispatcher.Dispatch) a real run
// id for every schedule that never fans out, which SchedulesPage already
// assumes.
type DriftBatch struct {
	BatchID string                   `json:"batch_id"`
	Runs    []*repositories.DriftRun `json:"runs"`
}

// errFanOutNotCapable reports a 2+-target dispatch aimed at a pipeline
// connection whose config.fan_out is not true (see pipelines.FanOutFromMap).
// A one-item targets array never reaches this check: it is the legacy single
// path by definition (items() collapses it before dispatchDriftBatch is
// called with more than one item to gate).
var errFanOutNotCapable = errors.New("pipeline connection is not fan-out capable")

// driftFanOutTarget is one entry of the "targets" template parameter/workflow
// input sent to a fan-out-capable pipeline: everything its per-target loop
// needs to plan that state and report its own callback.
type driftFanOutTarget struct {
	WorkingDir    string `json:"working_dir"`
	StateKey      string `json:"state_key"`
	CallbackURL   string `json:"callback_url"`
	CallbackToken string `json:"callback_token"`
}

// marshalFanOutTargets is json.Marshal, exposed as a package var so a test
// can force the encode-failure branch below without needing a real
// unmarshalable value -- every driftFanOutTarget field is a plain string
// that has already passed validateDriftTargets, so this realistically never
// fails in production; the seam exists so that fact is proven by a test
// rather than assumed.
var marshalFanOutTargets = json.Marshal

// failBatch marks every run in batch "failed" with detail, persisting it the
// same way dispatchDriftBatch's own dispatch-failure branch always has: a
// fanned batch through FailBatch (keyed on the shared batch_id), a single
// unfanned run through UpdateStatus (keyed on its own id, since its batch_id
// column is NULL and FailBatch's predicate could never match it). Shared by
// both dispatch failure and target-encoding failure so a pre-dispatch failure
// degrades identically to a real one rather than through a second, easier
// to miss code path.
func (h *DriftHandlers) failBatch(ctx context.Context, batch *DriftBatch, batchIDCol *string, detail string) {
	if batchIDCol != nil {
		_ = h.driftRepo.FailBatch(ctx, batch.BatchID, detail)
	} else {
		_ = h.driftRepo.UpdateStatus(ctx, batch.Runs[0].ID, "failed", detail)
	}
	for _, r := range batch.Runs {
		r.Status = "failed"
		r.Detail = detail
		r.CallbackToken = ""
	}
}

// dispatchDriftBatch loads the pipeline, records one drift run per target,
// and triggers ONE CI job to plan them all. tgt.items() is the single code
// path for both shapes: a request without `targets` (or with exactly one)
// produces exactly one item and therefore exactly today's dispatch, byte for
// byte on the wire (TestDispatchAzureDevOps_WireBody_NoTargets_MatchesTodayExactly);
// 2+ items send "targets" as a fourth parameter alongside the legacy three
// (taken from item 0), so a pipeline that has not adopted it yet still gets a
// request it understands.
//
// THE AUTH PARAMETER IS NOT OPTIONAL (#393 option B). auth is the ONE
// authority every InScope load on this chain runs under -- request-resolved
// for CreateRun, system-derived for the scheduler -- and it is what makes a
// connection or source in another organization fail closed rather than
// silently succeed. It guards, in order: the pipeline connection load, EVERY
// item's source load, and the token resolution that follows (which decrypts a
// credential -- see the comment on that call below). Dropping this parameter,
// or looping the per-item source check without it, reopens the exact
// cross-organization dispatch #393 closed.
//
// On a CI-dispatch failure it returns the batch (every run now "failed")
// alongside the error so the HTTP caller can surface the detail; every
// returned run's callback token is always blanked. On a failure BEFORE any
// run row exists (connection not found, fan-out gate, a target's source not
// owned here), it returns (nil, err) -- there is nothing to report yet.
//
// CONCURRENCY IS UNCHANGED: two runs dispatched against the same
// (source_id, state_key) -- whether from two different batches, or (by
// misconfiguration) the same batch -- remain independent rows, and
// UpsertDetection/ResolveClean stay last-write-wins exactly as they were
// before fan-out existed. validateDriftTargets refuses a duplicate pair
// WITHIN one request, but nothing here reaches across requests; the
// onboarding script is what prevents duplicate targets across schedules by
// construction.
func (h *DriftHandlers) dispatchDriftBatch(ctx context.Context, tgt DriftTarget, actor string, auth dispatchAuthority) (*DriftBatch, error) {
	items := tgt.items()

	// THE SCOPED LOAD FIRST, because the next call decrypts a credential.
	// resolvePipelineToken opens the connection's token, or its CI source's
	// shared token, so a load placed after it runs with the other tenant's
	// secret already in memory. Every by-id load on this chain is an InScope
	// read under ONE authority -- request-resolved or system-derived, with
	// provenance either way (#393 option B) -- so a connection in another
	// organization matches no row. Reported as "not found", so a caller cannot
	// use dispatch to probe which connection ids exist elsewhere; the wrapped
	// error carries the provenance for the log line.
	conn, err := pipelineConnectionFor(ctx, h.pipelineRepo, tgt.PipelineConnectionID, auth)
	if err != nil {
		return nil, fmt.Errorf("load pipeline connection: %w", err)
	}
	if conn == nil {
		return nil, errChainCrossesOrganizations("pipeline_connections", tgt.PipelineConnectionID, auth, errPipelineNotFound)
	}
	// The fan-out gate: a 2+ item request against a connection that never
	// opted in. A one-item request is the legacy path by definition and never
	// reaches this branch, regardless of the connection's fan_out setting.
	if len(items) > 1 && !pipelines.FanOutFromMap(conn.Config) {
		return nil, errFanOutNotCapable
	}
	// ...and every item's state source, for the same reason pipeline
	// connection is scoped: a target naming another organization's source aims
	// that target's run at their state.
	for _, item := range items {
		if _, err := sourceFor(ctx, h.sourceRepo, item.SourceID, auth); err != nil {
			if errors.Is(err, errNotOwnedHere) {
				return nil, errChainCrossesOrganizations("state_sources", item.SourceID, auth, errPipelineNotFound)
			}
			return nil, fmt.Errorf("load target source: %w", err)
		}
	}
	// Connection-level token, or the shared token of its CI source -- the
	// latter loaded under the SAME authority, which is the hop that used to be
	// entirely unscoped. Resolved ONCE: every target in the batch dispatches
	// through the same connection.
	token, bearer, err := resolvePipelineToken(ctx, h.ciSourceRepo, conn, auth)
	if err != nil {
		return nil, err
	}

	// batchID is the shared grouping id for 2+ targets, or left empty (each
	// run's own id fills DriftBatch.BatchID below) for exactly one -- so a
	// legacy/unfanned run's batch_id column stays NULL.
	var batchIDCol *string
	if len(items) > 1 {
		id := uuid.NewString()
		batchIDCol = &id
	}
	callbackBase := strings.TrimRight(h.cfg.Server.CallbackBase(), "/") + "/api/v1/drift/runs/"

	runs := make([]*repositories.DriftRun, 0, len(items))
	fanOutTargets := make([]driftFanOutTarget, 0, len(items))
	for _, item := range items {
		run := &repositories.DriftRun{
			PipelineConnectionID: &conn.ID,
			StateKey:             item.StateKey,
			RepoRef:              tgt.RepoRef,
			WorkingDir:           item.WorkingDir,
			Status:               "dispatched",
			CallbackToken:        randomToken(),
			Actor:                actor,
			BatchID:              batchIDCol,
		}
		if item.SourceID != "" {
			run.SourceID = &item.SourceID
		}
		saved, err := h.driftRepo.Create(ctx, run, auth.organizationID)
		if err != nil {
			return nil, fmt.Errorf("create drift run: %w", err)
		}
		runs = append(runs, saved)
		fanOutTargets = append(fanOutTargets, driftFanOutTarget{
			WorkingDir:    item.WorkingDir,
			StateKey:      item.StateKey,
			CallbackURL:   callbackBase + saved.ID + "/results",
			CallbackToken: run.CallbackToken,
		})
	}

	batchID := runs[0].ID
	if batchIDCol != nil {
		batchID = *batchIDCol
	}
	batch := &DriftBatch{BatchID: batchID, Runs: runs}

	// Params: the legacy three from item[0] -- today's exact shape -- plus
	// "targets" only when the request fanned out to more than one, so a
	// no-targets (or one-item) request's wire body is unchanged.
	first := runs[0]
	inputs := pipelines.DriftInputs{
		CallbackURL:   callbackBase + first.ID + "/results",
		CallbackToken: first.CallbackToken,
		WorkingDir:    items[0].WorkingDir,
	}
	if len(items) > 1 {
		targetsJSON, marshalErr := marshalFanOutTargets(fanOutTargets)
		if marshalErr != nil {
			// Every run already exists at this point (the batch's rows are
			// real), so silently sending a body with no "targets" -- only
			// item 0's legacy three params -- would create N runs and
			// dispatch only the first: N-1 stuck "dispatched" until the
			// reconciler expires them at TSM_DRIFT_RUN_TTL. Fail the whole
			// batch instead, exactly like a real dispatch failure below,
			// rather than silently degrading the wire body.
			failErr := fmt.Errorf("encode fan-out targets: %w", marshalErr)
			h.failBatch(ctx, batch, batchIDCol, failErr.Error())
			return batch, failErr
		}
		inputs.TargetsJSON = string(targetsJSON)
	}

	var ciRef *pipelines.CIRunRef
	var dispatchErr error
	switch conn.Provider {
	case "github_actions":
		ciRef, dispatchErr = pipelines.DispatchGitHubDrift(ctx, token, pipelines.GitHubConfigFromMap(conn.Config), tgt.RepoRef, inputs)
	case "azure_devops":
		ciRef, dispatchErr = pipelines.DispatchAzureDevOpsDrift(ctx, adoCred(token, bearer), pipelines.AzureDevOpsConfigFromMap(conn.Config), tgt.RepoRef, inputs)
	default:
		dispatchErr = errUnsupportedProvider(conn.Provider)
	}
	if dispatchErr != nil {
		h.failBatch(ctx, batch, batchIDCol, dispatchErr.Error())
		return batch, dispatchErr
	}

	// Best-effort: the CI job is already running by this point, so a failure
	// to record its id/link must not fail the response -- that would desync
	// run status from reality (the run stays "dispatched" either way).
	if ciRef != nil {
		if err := h.driftRepo.SetCIRun(ctx, batchID, ciRef.ID, ciRef.WebURL); err != nil {
			driftLog.Error("failed to record the CI run id/link for a dispatched run",
				"batch_id", batchID, "error", err)
		} else {
			for _, r := range runs {
				r.CIRunID = ciRef.ID
				r.CIRunURL = ciRef.WebURL
			}
		}
	}

	for _, r := range runs {
		r.CallbackToken = "" // never expose
	}
	return batch, nil
}

// ListRuns returns drift runs, newest first, with server-side pagination.
// @Summary      List drift runs
// @Description  Drift runs newest-first. Each run carries the drift contract's completeness markers (truncated, omitted_entries, omitted_attrs, unparseable, unmasked) describing what that run's own check did not do — zero counts alone cannot distinguish a verified-clean run from one that never finished checking. batch_id matches either a fan-out batch's shared id or a single run's own id, so a schedule's last_run_id works as a filter whether or not it fanned.
// @Tags         Drift
// @Produce      json
// @Param        status    query  string  false  "filter by status (dispatched|running|completed|failed)"
// @Param        batch_id  query  string  false  "filter by batch id (or a single run's own id)"
// @Param        source_id query  string  false  "filter by state source id"
// @Param        state_key query  string  false  "filter by state key"
// @Param        limit     query  int     false  "page size (default 50, max 200)"
// @Param        offset    query  int     false  "rows to skip"
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /drift/runs [get]
func (h *DriftHandlers) ListRuns() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		status := c.Query("status")
		switch status {
		case "", "dispatched", "running", "completed", "failed":
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status filter"})
			return
		}
		filter := repositories.DriftRunFilter{
			Status:   status,
			BatchID:  c.Query("batch_id"),
			SourceID: c.Query("source_id"),
			StateKey: c.Query("state_key"),
		}
		limit, _ := strconv.Atoi(c.Query("limit"))   // 0 -> repo default (50)
		offset, _ := strconv.Atoi(c.Query("offset")) // 0 -> first page
		// The Phase 3 read flip for drift_runs (#393). Unscoped, this listed
		// every organization's runs -- each one naming a state key and carrying
		// the plan summary, i.e. the resource addresses another tenant is about
		// to change or destroy -- to any caller holding state:read anywhere.
		//
		// An UNRESOLVED scope is a 500, never an empty one and certainly never a
		// full one: it means the route was registered without
		// middleware.TenantScope.
		scope, resolved := tenantscope.FromContext(c)
		if !resolved {
			serverError(c, errNoTenantScope, "the tenant scope was not resolved for this route")
			return
		}
		runs, err := h.driftRepo.ListInScope(ctx, limit, offset, filter, scope)
		if err != nil {
			serverError(c, err, "failed to list drift runs")
			return
		}
		// The total is scoped with the page: "showing 3 of 47" beside a
		// three-row list would report how many runs the rest of the deployment
		// has.
		total, err := h.driftRepo.CountRunsInScope(ctx, filter, scope)
		if err != nil {
			serverError(c, err, "failed to count drift runs")
			return
		}
		c.JSON(http.StatusOK, gin.H{"runs": runs, "total": total})
	}
}

// GetRun returns a single drift run (without the callback token).
func (h *DriftHandlers) GetRun() gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, resolved := tenantscope.FromContext(c)
		if !resolved {
			serverError(c, errNoTenantScope, "the tenant scope was not resolved for this route")
			return
		}
		// A run in another organization is reported EXACTLY as one that does not
		// exist, so this endpoint cannot be used to test which run ids are real
		// elsewhere in the deployment.
		run, err := h.driftRepo.GetByIDInScope(c.Request.Context(), c.Param("id"), scope)
		if err != nil {
			serverError(c, err, "failed to load drift run")
			return
		}
		if run == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "drift run not found"})
			return
		}
		run.CallbackToken = ""
		c.JSON(http.StatusOK, run)
	}
}

// driftRunResultPayload is the body the dispatched CI job posts to the run
// callback. TSM GENERATES the producer of this payload (the jq in
// drift_workflows.go), so the two must agree; that agreement is enforced by
// TestDriftCallbackPayload_EveryGeneratedKeyIsDecoded rather than by strict
// decoding.
//
// Decoding stays lenient on purpose. The callback token is one-shot, so a body
// rejected for carrying an unknown key cannot be retried — the run would be
// stranded and its drift result lost permanently, which is strictly worse than
// ignoring a key. Users also commit the generated workflow into their own repo,
// where TSM cannot update it, so a newer template must degrade against an older
// server instead of failing.
type driftRunResultPayload struct {
	Token     string          `json:"token"`
	Status    string          `json:"status"`
	Added     int             `json:"added"`
	Changed   int             `json:"changed"`
	Destroyed int             `json:"destroyed"`
	Drifted   *bool           `json:"drifted"`
	Detail    string          `json:"detail"`
	Summary   json.RawMessage `json:"summary"`
	// Optional module provenance: the plan's configuration (module calls)
	// and the resolved module lockfile. Small (module_calls + modules.json,
	// not the full plan), so no size cap is needed. Absent on older runners.
	Plan        json.RawMessage `json:"plan"`
	ModuleLocks json.RawMessage `json:"module_locks"`
	// Markers describing what the run's own check did not do. This endpoint
	// never re-parses the plan (only its module_calls projection arrives), so
	// the producer's markers are the only account of completeness there is.
	repositories.Completeness
}

// RunResults is the machine callback the CI job posts drift results to. It is
// authenticated by the per-run callback token (no user session).
// @Summary      Drift run callback (machine)
// @Description  CI job posts drift results here, authenticated by the per-run one-shot X-TSM-Callback-Token. Not a user endpoint. Accepts the drift contract's completeness markers (truncated, omitted_entries, omitted_attrs, unparseable, unmasked) and stores them on BOTH the run and the drift record, so per-run history can tell a check that verified clean from one that never finished. An unparseable result never auto-resolves a live record. Unknown fields are ignored so a newer runner can post to an older server.
// @Tags         Drift
// @Accept       json
// @Produce      json
// @Param        id   path  string  true  "Drift run ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}  "invalid callback token"
// @Failure      409  {object}  map[string]interface{}  "callback already processed"
// @Security     CallbackToken
// @Router       /drift/runs/{id}/results [post]
func (h *DriftHandlers) RunResults() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()

		var body driftRunResultPayload
		_ = c.ShouldBindJSON(&body)

		token := callbackTokenFrom(c.GetHeader("X-TSM-Callback-Token"), body.Token)

		// THE PRE-AUTHENTICATION LOOKUP, and the one read on this path that has
		// no scope to run under: the credential being authenticated is what
		// identifies the run, so there is nothing to scope BY until this returns.
		// Recorded in unscoped_twin_class_test.go's justifiedUnscoped for exactly
		// that reason. Everything after it runs under the authority the run
		// confers.
		run, err := h.driftRepo.GetByID(ctx, c.Param("id"))
		if err != nil {
			serverError(c, err, "failed to load drift run")
			return
		}
		// Uniform 401 whether the run is missing, the token is wrong, or the run
		// belongs to no organization -- so the endpoint is not a run-ID existence
		// oracle, and an unstamped run confers no authority rather than a
		// deployment-wide one. See callback_authority.go.
		auth, authenticated := dispatchAuthority{}, false
		if run != nil {
			auth, authenticated = authenticateCallback("drift_runs",
				callbackRun{ID: run.ID, OrganizationID: run.OrganizationID, StoredToken: run.CallbackToken}, token)
		}
		if !authenticated {
			// ONE EXIT for all three refusals, which is what keeps them
			// indistinguishable from outside.
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid callback token"})
			return
		}
		// One-shot: atomically consume the token; a replay finds it already
		// cleared. Keyed on the credential itself (id AND token), which is a
		// narrower predicate than the organization one and needs no scope on top.
		consumed, err := h.driftRepo.ConsumeCallbackToken(ctx, run.ID, run.CallbackToken)
		if err != nil {
			serverError(c, err, "failed to record results")
			return
		}
		if !consumed {
			c.JSON(http.StatusConflict, gin.H{"error": "callback already processed"})
			return
		}

		status := body.Status
		if status == "" {
			status = "completed"
		}
		drifted := body.Added+body.Changed+body.Destroyed > 0
		if body.Drifted != nil {
			drifted = *body.Drifted
		}
		if err := h.driftRepo.UpdateResultInScope(ctx, run.ID, status, body.Added, body.Changed, body.Destroyed, drifted, body.Summary, body.Detail, body.Completeness, auth.scope); err != nil {
			serverError(c, err, "failed to record results")
			return
		}

		// THE RUN'S SOURCE, LOADED UNDER THE RUN'S OWN AUTHORITY, before anything
		// keyed on it is written.
		//
		// drift_runs.source_id is nullable and carries no same-organization
		// constraint. The dispatch chain refuses a crossing target now, but a run
		// dispatched before that landed -- or written by direct SQL -- can still
		// name a source in another organization, and every statement below is
		// keyed on that source: UpsertDetection would write into the other
		// tenant's drift ledger, ResolveClean would close their live finding, and
		// ReplaceForState would rewrite their module provenance. #393's survey
		// settled the rule: the run is the CROSS-CHECK, a mismatch is refused
		// rather than silently resolved.
		//
		// A source that is gone (ON DELETE SET NULL) is the same answer as one in
		// another organization, and the same answer the record maintenance
		// already gives for a run with no source: nothing is written, the run
		// result itself is still recorded, and the finding survives until
		// something that can see it verifies it.
		runSourceID := ""
		if run.SourceID != nil {
			runSourceID = *run.SourceID
		}
		src, err := sourceFor(ctx, h.sourceRepo, runSourceID, auth)
		switch {
		case errors.Is(err, errNotOwnedHere):
			driftLog.Error("drift callback refused: the run's source is not reachable under its own organization",
				"run", run.ID, "error", errChainCrossesOrganizations("state_sources", runSourceID, auth, errNotOwnedHere))
			src = nil
		case err != nil:
			serverError(c, err, "failed to record results")
			return
		}
		if src != nil {
			h.recordDriftOutcome(ctx, run, status, body.Added, body.Changed, body.Destroyed, drifted, body.Summary, body.Completeness, auth)
			// Best-effort module provenance for dispatched runs: if the runner
			// uploaded the plan's module calls (+ optional locks), capture them
			// against this run's source/state (both taken from the token-scoped
			// run record, never the body). Never fails the callback — the drift
			// result is the primary product.
			if len(body.Plan) > 0 {
				var plan driftingest.Plan
				if err := json.Unmarshal(body.Plan, &plan); err == nil {
					h.captureModuleRefs(ctx, src.ID, run.StateKey, &plan, body.ModuleLocks, auth.scope)
				}
			}
		}
		h.notifyDriftResult(run.OrganizationID, run.ID, run.StateKey, status, body.Added, body.Changed, body.Destroyed, drifted, body.Detail, run.CIRunURL)
		c.JSON(http.StatusOK, gin.H{"status": "recorded"})
	}
}

// notifyDriftResult fires an alert event when a drift result reports drift or a
// failure. It runs detached (the CI callback must not block on webhook latency)
// with its own timeout; a nil notifier (notifications disabled) is a no-op.
// N notifications per fan-out batch is intentional (one per state = one per
// record): a batch that plans 5 apps and drifts on 2 fires exactly 2 alerts,
// each naming its own state_key rather than a single alert naming the batch.
func (h *DriftHandlers) notifyDriftResult(organizationID, runID, stateKey, status string, added, changed, destroyed int, drifted bool, detail, ciRunURL string) {
	if h.notifier == nil {
		return
	}
	var ev notify.Event
	switch {
	case status == "failed":
		ev = notify.Event{
			Type:    notify.EventRunFailed,
			Title:   "Drift run failed",
			Message: fmt.Sprintf("Drift run %s failed: %s", runID, detail),
		}
	case drifted:
		ev = notify.Event{
			Type:    notify.EventDriftDetected,
			Title:   "Drift detected",
			Message: fmt.Sprintf("Drift run %s detected changes (+%d ~%d -%d).", runID, added, changed, destroyed),
		}
	default:
		return // no drift, no failure — nothing to alert on
	}
	// Which state this is about, and a link to the CI run that checked it --
	// both absent from the message above because a single-target run gains
	// nothing from repeating what its run id already implies, while a
	// fan-out batch's N per-target alerts need the state_key to be
	// distinguishable from one another at all.
	if stateKey != "" {
		ev.Message += fmt.Sprintf(" state_key=%s", stateKey)
	}
	if ciRunURL != "" {
		ev.Message += fmt.Sprintf(" (%s)", ciRunURL)
	}
	// Fanned out to THIS organization's channels only. Without the scope the
	// notifier selects every enabled channel in the deployment, so the drift run that failed or drifted
	// would be announced to every other tenant's webhooks (#459).
	go func(ev notify.Event, organizationID string) {
		ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
		defer cancel()
		h.notifier.Notify(ctx, ev, notify.ForOrganization(organizationID))
	}(ev, organizationID)
}

// WorkflowTemplate returns the runner-side CI definition to copy into a repo.
// GET /api/v1/drift/workflow?provider=github_actions|azure_devops[&profile=default]
// Served from the operator-managed store (falling back to the embedded built-in).
func (h *DriftHandlers) WorkflowTemplate(templates *repositories.WorkflowTemplateRepository) gin.HandlerFunc {
	return serveWorkflowTemplate(templates, "drift")
}

func errUnsupportedProvider(p string) error {
	return &unsupportedProviderError{provider: p}
}

type unsupportedProviderError struct{ provider string }

func (e *unsupportedProviderError) Error() string {
	return "pipeline provider " + e.provider + " is not supported yet"
}
