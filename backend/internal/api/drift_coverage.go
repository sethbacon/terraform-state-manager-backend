// drift_coverage.go implements the Phase 4a dashboard read-path
// (drift-fleet-scale.md #567): GET /drift/coverage joins one source's live
// state listing against its latest drift run, live drift record and schedule
// membership so an operator can see which states have never been checked,
// which are stale, and which last "clean" result was actually unparseable.
// GET /drift/summary is the fleet-wide rollup behind the landing-page cards.
//
// Both resolve a tenant scope and read through the SAME InScope repository
// methods the rest of the drift plane uses (ListRuns/ListDriftRecords):
// coverage's source lookup goes through SourceRepository.GetByIDInScope
// exactly as sources.go's connectorFor does, and every join afterward reads
// through DriftRepository/DriftRecordRepository's InScope methods -- so a
// source this caller cannot see is refused before any run or record is read,
// and a run/record whose own organization_id ever disagreed with its
// source's (it should never, but see LatestPerStateInScope's comment) is
// filtered a second time rather than trusted by association.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/statesource"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// coverageCacheTTL is how long one source's join inputs (state listing +
// latest runs + live records + schedule membership) are reused across
// requests -- design decision #5 (drift-fleet-scale.md): "coverage is
// computed, not stored... cache per source for 60s". The SOURCE lookup itself
// is never cached (see Coverage): every request re-verifies the caller's scope
// against it fresh, so a deleted or re-owned source 404s immediately rather
// than serving another organization's stale cached join.
const coverageCacheTTL = 60 * time.Second

// coverageJoinInputs is the expensive part of one coverage request: the
// connector's live listing (a real backend round trip) plus the three
// repository joins. Cached as a unit per source_id.
type coverageJoinInputs struct {
	refs        []statesource.StateRef
	latestRuns  map[string]repositories.DriftRun
	liveRecords map[string]repositories.DriftRecord
	scheduled   map[string]bool
}

// coverageCache is a plain mutex-guarded map with a per-entry TTL -- the
// "simplest thing that works" this phase asks for, not a caching framework.
// One instance per DriftHandlers (constructed fresh per process, and per test
// rig), so entries never leak across independent handler instances.
//
// Keyed by source id alone. That is safe only because every reader resolves
// the source through GetByIDInScope BEFORE touching the cache, so a caller
// outside the owning organization 404s before any cached row can be seen.
// Keep that ordering; if a second reader is ever added, key by (org, source).
type coverageCache struct {
	mu      sync.Mutex
	entries map[string]coverageCacheEntry
}

type coverageCacheEntry struct {
	at   time.Time
	data coverageJoinInputs
}

func newCoverageCache() *coverageCache {
	return &coverageCache{entries: map[string]coverageCacheEntry{}}
}

// get returns the cached join inputs for sourceID, and whether they are still
// within the TTL. A stale-but-present entry is treated as absent -- the next
// fetch overwrites it, rather than this doing its own eviction sweep.
func (cc *coverageCache) get(sourceID string) (coverageJoinInputs, bool) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	e, ok := cc.entries[sourceID]
	if !ok || time.Since(e.at) > coverageCacheTTL {
		return coverageJoinInputs{}, false
	}
	return e.data, true
}

func (cc *coverageCache) set(sourceID string, data coverageJoinInputs) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.entries[sourceID] = coverageCacheEntry{at: time.Now(), data: data}
}

// defaultStaleAfter is applied when ?stale_after= is absent -- the same 24h
// window the plan's "Done when" acceptance criteria names. A present but
// invalid value is a 400, never silently this default.
const defaultStaleAfter = 24 * time.Hour

// maxStaleAfter bounds ?stale_after=: past it every state reads as fresh and
// the coverage view stops meaning anything.
const maxStaleAfter = 30 * 24 * time.Hour

// coverageState is one row of GET /drift/coverage's states array.
type coverageState struct {
	Key          string  `json:"key"`
	Scheduled    bool    `json:"scheduled"`
	LastRunID    *string `json:"last_run_id"`
	LastRunAt    *string `json:"last_run_at"`
	LastStatus   *string `json:"last_status"`
	Drifted      *bool   `json:"drifted"`
	Unparseable  bool    `json:"unparseable"`
	Truncated    bool    `json:"truncated"`
	CIRunURL     *string `json:"ci_run_url"`
	RecordID     *string `json:"record_id"`
	RecordStatus *string `json:"record_status"`
	Severity     *string `json:"severity"`
	// InfraDrifted reports the latest run's drift contract's second triplet
	// (resource_drift, migration 000039): whether that run's own check found
	// drift OUTSIDE Terraform, independent of Drifted above (which stays
	// exactly the resource_changes-derived "unapplied changes" signal). Nil
	// under the same condition Drifted is nil -- no run has ever been
	// dispatched for this state -- and a real bool once one has, even a run
	// still "dispatched" (drift_added/changed/destroyed default to 0 until the
	// callback lands, so a not-yet-checked run reads identically to a
	// checked-and-clean one here, exactly as Added/Changed/Destroyed already
	// do until their own UpdateResult). Phase 5's whole point is that this
	// field and Drifted must never be conflated into one signal.
	InfraDrifted *bool `json:"infra_drifted"`
}

// coverageSummary is GET /drift/coverage's summary object: chip counts over
// the same states array, so the dashboard's headline numbers and its table
// can never disagree about what "showing" means.
type coverageSummary struct {
	Total       int `json:"total"`
	Scheduled   int `json:"scheduled"`
	Unscheduled int `json:"unscheduled"`
	Stale       int `json:"stale"`
	Incomplete  int `json:"incomplete"`
	Open        int `json:"open"`
	Critical    int `json:"critical"`
	// InfraDrifted counts the states above whose InfraDrifted is true --
	// the coverage view's own chip for resource_drift, kept separate from
	// Open/Critical (which describe live drift_records, not the latest run).
	InfraDrifted int `json:"infra_drifted"`
}

// sourceInScopeByID loads a source by an id NOT taken from the URL path
// (coverage/summary read it from ?source_id=, unlike every :id-keyed source
// route), refusing it exactly as SourcesHandlers.sourceInScope does: a 404
// for both a missing id and one outside the scope, so the two are never
// distinguishable to the caller.
func (h *DriftHandlers) sourceInScopeByID(c *gin.Context, id string, scope tenantscope.Scope) (*repositories.Source, bool) {
	s, err := h.sourceRepo.GetByIDInScope(c.Request.Context(), id, scope)
	if err != nil {
		serverError(c, err, "failed to load source")
		return nil, false
	}
	if s == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
		return nil, false
	}
	return s, true
}

// connectorForSource builds a connector over an already-scoped source, the
// same two steps SourcesHandlers.connectorFor takes (decrypt, then
// statesource.New) but starting from a *repositories.Source already in hand
// rather than re-resolving one from c.Param("id").
func (h *DriftHandlers) connectorForSource(c *gin.Context, s *repositories.Source) (statesource.Connector, bool) {
	creds, err := decryptCredentials(s)
	if err != nil {
		serverError(c, err, "failed to decrypt source credentials")
		return nil, false
	}
	conn, err := statesource.New(s.Type, s.Config, creds)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return nil, false
	}
	return conn, true
}

// parsePGTimestamp best-effort parses a DriftRun's CreatedAt/UpdatedAt string
// -- CAST TO ::text by driftColumns, so its exact rendering depends on the
// database session's TimeZone (this app never overrides it) rather than on
// anything Go controls. The layouts below cover PostgreSQL's own
// `timestamptz::text` output (space-separated, optional fractional seconds,
// a bare 2-digit UTC offset with no colon -- verified against a live server:
// "2026-09-05 01:56:52.563088+00") plus RFC3339 and a bare date, for
// safety and for test fixtures.
//
// An unparseable value is reported as such rather than guessed at, and the
// ONLY caller (coverage's staleness classification) treats that as "stale":
// the safe direction for a dashboard is a false "needs attention", never a
// false "up to date" that hides a state nobody has actually checked.
func parsePGTimestamp(s string) (time.Time, bool) {
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999-07",
		"2006-01-02 15:04:05-07",
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// scheduledStateKeys returns the set of (source_id-matching) state keys any
// "drift"-type schedule already targets, parsing each schedule's opaque
// target_config as a DriftTarget and walking items() -- the SAME accessor
// dispatchDriftBatch uses, so a legacy single-target schedule and a fanned
// one are read through one code path here too.
func scheduledStateKeys(schedules []repositories.Schedule, sourceID string) map[string]bool {
	out := map[string]bool{}
	for _, s := range schedules {
		// A disabled schedule never fires (GetDue filters on enabled), so its
		// targets are NOT covered -- counting them would hide exactly the gap
		// this view exists to show.
		if s.TargetType != "drift" || !s.Enabled {
			continue
		}
		var t DriftTarget
		if err := json.Unmarshal(s.TargetConfig, &t); err != nil {
			continue
		}
		for _, item := range t.items() {
			if item.SourceID == sourceID && item.StateKey != "" {
				out[item.StateKey] = true
			}
		}
	}
	return out
}

// Coverage joins one source's live state listing against its latest drift
// run, live drift record, and schedule membership.
// @Summary      Drift coverage for one source
// @Description  For each state the connector currently lists: whether a schedule already targets it, its latest run (status, drifted, infra_drifted, completeness, CI link), and its live (non-resolved) drift record, if any. infra_drifted (migration 000039) reports the latest run's resource_drift finding and is never conflated with drifted, which stays the resource_changes signal. summary gives the same chip counts the states array would produce if counted by hand.
// @Tags         Drift
// @Produce      json
// @Param        source_id   query  string  true   "state source id"
// @Param        stale_after query  string  false  "duration a state may go unchecked before it counts as stale (default 24h; a positive Go duration no greater than 720h, else 400)"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /drift/coverage [get]
func (h *DriftHandlers) Coverage() gin.HandlerFunc {
	return func(c *gin.Context) {
		sourceID := c.Query("source_id")
		if sourceID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "source_id is required"})
			return
		}
		// stale_after shapes the "stale" verdict on every row, so a bad value
		// is refused rather than silently swapped for the default -- and it is
		// checked before any database read.
		staleAfter := defaultStaleAfter
		if v := c.Query("stale_after"); v != "" {
			d, dErr := time.ParseDuration(v)
			if dErr != nil || d <= 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "stale_after must be a positive duration such as 24h or 90m"})
				return
			}
			if d > maxStaleAfter {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("stale_after may not exceed %s", maxStaleAfter)})
				return
			}
			staleAfter = d
		}
		// The Phase 4a read for drift_runs/drift_records/schedules, on the same
		// terms ListRuns/ListDriftRecords already resolve it on (#393 Phase 3):
		// an UNRESOLVED scope is a 500, never an empty one and certainly never a
		// full one, because that means the route was registered without
		// middleware.TenantScope.
		scope, resolved := tenantscope.FromContext(c)
		if !resolved {
			serverError(c, errNoTenantScope, "the tenant scope was not resolved for this route")
			return
		}
		src, ok := h.sourceInScopeByID(c, sourceID, scope)
		if !ok {
			return
		}
		ctx := c.Request.Context()

		inputs, ok := h.coverage.get(sourceID)
		if !ok {
			conn, connOK := h.connectorForSource(c, src)
			if !connOK {
				return
			}
			refs, err := conn.List(ctx)
			if err != nil {
				upstreamError(c, http.StatusBadGateway, err, "failed to list states from the backend")
				return
			}
			latestRuns, err := h.driftRepo.LatestPerStateInScope(ctx, sourceID, scope)
			if err != nil {
				serverError(c, err, "failed to load latest drift runs")
				return
			}
			liveRecords, err := h.recordRepo.LiveByStateInScope(ctx, sourceID, scope)
			if err != nil {
				serverError(c, err, "failed to load live drift records")
				return
			}
			schedules, err := h.scheduleRepo.ListInScope(ctx, scope)
			if err != nil {
				serverError(c, err, "failed to load schedules")
				return
			}
			inputs = coverageJoinInputs{
				refs:        refs,
				latestRuns:  latestRuns,
				liveRecords: liveRecords,
				scheduled:   scheduledStateKeys(schedules, sourceID),
			}
			h.coverage.set(sourceID, inputs)
		}

		now := time.Now()
		states := make([]coverageState, 0, len(inputs.refs))
		var summary coverageSummary
		for _, ref := range inputs.refs {
			summary.Total++
			cs := coverageState{Key: ref.Key}
			if inputs.scheduled[ref.Key] {
				cs.Scheduled = true
				summary.Scheduled++
			} else {
				summary.Unscheduled++
			}

			stale := true // no run at all is the definitionally-stale case
			if run, ok := inputs.latestRuns[ref.Key]; ok {
				id := run.ID
				cs.LastRunID = &id
				at := run.CreatedAt
				cs.LastRunAt = &at
				status := run.Status
				cs.LastStatus = &status
				cs.Drifted = run.Drifted
				cs.Unparseable = run.Unparseable
				cs.Truncated = run.Truncated
				if run.CIRunURL != "" {
					url := run.CIRunURL
					cs.CIRunURL = &url
				}
				// Independent of Drifted above -- never derived from it, and
				// never folded into the same field. See InfraDrifted's own
				// comment on coverageState.
				infraDrifted := run.DriftAdded > 0 || run.DriftChanged > 0 || run.DriftDestroyed > 0
				cs.InfraDrifted = &infraDrifted
				if t, ok := parsePGTimestamp(run.CreatedAt); ok && now.Sub(t) < staleAfter {
					stale = false
				}
			}
			if stale {
				summary.Stale++
			}
			if cs.Unparseable || cs.Truncated {
				summary.Incomplete++
			}
			if cs.InfraDrifted != nil && *cs.InfraDrifted {
				summary.InfraDrifted++
			}

			if rec, ok := inputs.liveRecords[ref.Key]; ok {
				id := rec.ID
				cs.RecordID = &id
				status := rec.Status
				cs.RecordStatus = &status
				severity := rec.Severity
				cs.Severity = &severity
				if rec.Status == "open" {
					summary.Open++
				}
				if rec.Severity == "critical" {
					summary.Critical++
				}
			}
			states = append(states, cs)
		}
		c.JSON(http.StatusOK, gin.H{"states": states, "summary": summary})
	}
}

// Summary is the fleet-wide rollup behind the landing page's drift cards.
// @Summary      Drift summary
// @Description  Per-source open/acknowledged/critical/infra_drift drift-record counts, the last 24h of drift runs by terminal status, how many live records are incomplete (unparseable or truncated), how many live records carry infra drift (resource_drift, migration 000039, never conflated with the unapplied-change counts beside it), and how many runs are currently in flight -- all scoped to the caller's organization(s).
// @Tags         Drift
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /drift/summary [get]
func (h *DriftHandlers) Summary() gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, resolved := tenantscope.FromContext(c)
		if !resolved {
			serverError(c, errNoTenantScope, "the tenant scope was not resolved for this route")
			return
		}
		ctx := c.Request.Context()

		bySource, err := h.recordRepo.CountsBySourceInScope(ctx, scope)
		if err != nil {
			serverError(c, err, "failed to load drift summary")
			return
		}
		incomplete, err := h.recordRepo.CountIncompleteInScope(ctx, scope)
		if err != nil {
			serverError(c, err, "failed to load drift summary")
			return
		}
		// infra_drifted mirrors incomplete_records' shape exactly (a single
		// scoped count over LIVE records) but classifies by the drift
		// contract's second triplet instead: how many open findings carry
		// resource_drift, independent of whatever their own
		// added/changed/destroyed (resource_changes) says.
		infraDrifted, err := h.recordRepo.CountInfraDriftedInScope(ctx, scope)
		if err != nil {
			serverError(c, err, "failed to load drift summary")
			return
		}

		// Each count below reuses DriftRepository.CountRunsInScope (already
		// scoped, already tested) rather than a bespoke GROUP BY query against
		// drift_runs -- four small counts for the 24h window plus two for the
		// current in-flight total. This is a dashboard rollup, not a hot path,
		// so the extra round trips buy "no new scoped SQL surface" cheaply.
		since := time.Now().Add(-24 * time.Hour)
		count := func(status string, win *time.Time) (int, error) {
			return h.driftRepo.CountRunsInScope(ctx, repositories.DriftRunFilter{Status: status, Since: win}, scope)
		}
		completed, err := count("completed", &since)
		if err != nil {
			serverError(c, err, "failed to load drift summary")
			return
		}
		failed, err := count("failed", &since)
		if err != nil {
			serverError(c, err, "failed to load drift summary")
			return
		}
		dispatched24h, err := count("dispatched", &since)
		if err != nil {
			serverError(c, err, "failed to load drift summary")
			return
		}
		running24h, err := count("running", &since)
		if err != nil {
			serverError(c, err, "failed to load drift summary")
			return
		}
		inFlightDispatched, err := count("dispatched", nil)
		if err != nil {
			serverError(c, err, "failed to load drift summary")
			return
		}
		inFlightRunning, err := count("running", nil)
		if err != nil {
			serverError(c, err, "failed to load drift summary")
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"records_by_source": bySource,
			"runs_24h": gin.H{
				"completed":  completed,
				"failed":     failed,
				"dispatched": dispatched24h + running24h,
			},
			"incomplete_records": incomplete,
			"infra_drifted":      infraDrifted,
			"in_flight":          inFlightDispatched + inFlightRunning,
		})
	}
}
