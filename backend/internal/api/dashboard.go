package api

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/analyzer"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
	semver "github.com/terraform-state-manager/terraform-state-manager/internal/version"
)

// overviewAggCache memoizes the dashboard's store-wide aggregation queries
// (totals + provider/resource-type/version distributions) behind a short TTL,
// keyed on the newest last_sync_at across sources: a completed sync — including
// one triggered via POST /reconcile — changes the key and recomputes immediately,
// while repeated dashboard loads between syncs reuse the cached aggregates.
// State edits that bypass sync are visible at worst overviewCacheTTL late.
// overviewAggCache memoizes the dashboard aggregates PER TENANT SCOPE.
//
// It used to be a single slot keyed only on the newest last_sync_at, which was
// correct while every read was store-wide: one set of numbers, the same for
// everybody. Scoping the reads (#459) makes that a cross-tenant leak and a worse
// one than the disclosure it fixes — tenant A loads the dashboard, the slot
// fills with A's totals, and tenant B's load inside the TTL is served A's
// numbers under B's name. Today's behaviour at least shows everyone the same
// fleet-wide figures; a scope-blind cache would show B a subset that is not B's.
//
// So the scope is part of the key. Entries are bounded: distinct keys are
// distinct MEMBERSHIP SETS rather than distinct users, which is small in
// practice, but "small in practice" is not a bound and this map is reachable
// from an unauthenticated-shaped request count.
type overviewAggCache struct {
	mu      sync.Mutex
	entries map[string]overviewAggEntry
}

type overviewAggEntry struct {
	expires   time.Time
	totals    *repositories.AnalysisTotals
	providers map[string]int
	resTypes  map[string]int
	versions  map[string]int
}

// overviewCacheMaxEntries bounds the memo. On overflow the whole map is dropped
// rather than one entry evicted: picking a victim needs recency tracking this
// does not carry, and a full clear costs one recomputation per active scope
// against a table the TTL already re-reads every 30 seconds.
const overviewCacheMaxEntries = 64

// scopeCacheKey renders a scope into a stable string.
//
// SORTED, because tenantscope.Scope documents that OrgIDs order is not
// significant — two requests with the same membership in a different order must
// hit the same entry, and would not if the key were built by concatenation
// alone.
//
// The platform-admin key is separate and cannot collide with any organization
// set: an administrator's numbers are the fleet's, and serving them to a tenant
// whose id list happened to render the same way is the leak this key exists to
// prevent.
func scopeCacheKey(scope tenantscope.Scope) string {
	if scope.PlatformAdmin {
		return "platform-admin"
	}
	ids := append([]string(nil), scope.OrgIDs...)
	sort.Strings(ids)
	return "orgs:" + strings.Join(ids, ",")
}

// overviewCacheTTL bounds staleness for store changes that do not go through a
// sync cycle (state edits, deletes). Sync-driven changes invalidate via the key.
const overviewCacheTTL = 30 * time.Second

// overviewAggregates returns the four store-wide aggregates, cached. The lock
// is held across the queries on purpose: concurrent dashboard loads coalesce
// into one recomputation instead of racing the same four scans.
func (h *SourcesHandlers) overviewAggregates(ctx context.Context, syncKey string, scope tenantscope.Scope) (*repositories.AnalysisTotals, map[string]int, map[string]int, map[string]int, error) {
	c := &h.overviewCache
	key := syncKey + "\x00" + scopeCacheKey(scope)

	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok && e.totals != nil && time.Now().Before(e.expires) {
		return e.totals, e.providers, e.resTypes, e.versions, nil
	}
	totals, err := h.analysisRepo.TotalsInScope(ctx, scope)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	providers, err := h.analysisRepo.ProviderCountsInScope(ctx, scope)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	resTypes, err := h.analysisRepo.ResourceTypeCountsInScope(ctx, scope)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	versions, err := h.analysisRepo.VersionCountsInScope(ctx, scope)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if c.entries == nil || len(c.entries) >= overviewCacheMaxEntries {
		c.entries = make(map[string]overviewAggEntry, overviewCacheMaxEntries)
	}
	c.entries[key] = overviewAggEntry{
		expires:   time.Now().Add(overviewCacheTTL),
		totals:    totals,
		providers: providers,
		resTypes:  resTypes,
		versions:  versions,
	}
	return totals, providers, resTypes, versions, nil
}

// The dashboard aggregates the persistent state-analysis store (kept
// reconciled by the statesync service) instead of re-reading every state file
// from its backend per request. That keeps the endpoint O(store rows) no
// matter how many state files the backends hold, and the numbers stable
// between loads. A reconcile is triggered out-of-band via POST /reconcile
// (steady state: one listing call per source plus reads for changed states
// only); this endpoint always serves the current store.

// DashboardOverview aggregates analyzer metrics across every configured source
// for the home page: totals (RUM, managed, data), provider / resource-type /
// Terraform-version distributions, and per-source sync freshness.
// @Summary      Dashboard overview
// @Description  Aggregates RUM and resource/provider/Terraform-version breakdowns from the persistent analysis store, with per-source sync status. Use POST /reconcile to refresh the store first. Requires state:read.
// @Tags         Dashboard
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /dashboard/overview [get]
func (h *SourcesHandlers) DashboardOverview() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()

		// Scoped throughout (#459). The dashboard is the widest aggregate this
		// API serves: totals, provider and resource-type distributions, and
		// per-source sync freshness, all previously computed store-wide. A
		// tenant reading it saw the whole fleet's shape — how many states other
		// organizations hold, which providers they run, which Terraform
		// versions they are on — without ever naming a row it could not fetch.
		scope, resolved := tenantscope.FromContext(c)
		if !resolved {
			serverError(c, errNoTenantScope, "the tenant scope was not resolved for this route")
			return
		}

		sources, err := h.repo.ListInScope(ctx, scope)
		if err != nil {
			serverError(c, err, "failed to list sources")
			return
		}
		// Statuses load first (small table, always fresh — the sync panel must
		// not lag); their newest last_sync_at keys the aggregate cache.
		statuses, err := h.analysisRepo.SyncStatusesInScope(ctx, scope)
		if err != nil {
			serverError(c, err, "failed to load sync status")
			return
		}
		syncKey := ""
		for _, st := range statuses {
			if st.LastSyncAt > syncKey {
				syncKey = st.LastSyncAt
			}
		}
		totals, providers, resTypes, versions, err := h.overviewAggregates(ctx, syncKey, scope)
		if err != nil {
			serverError(c, err, "failed to aggregate analyses")
			return
		}

		// Per-source freshness, in the sources' own (newest-first) order.
		// Sources without a status row have not completed a first sync yet.
		sync := make([]gin.H, 0, len(sources))
		sourceErrors := 0
		statesListed := 0
		refreshedAt := ""
		for i := range sources {
			src := &sources[i]
			entry := gin.H{
				"source_id": src.ID,
				"name":      src.Name,
				"type":      src.Type,
				"synced":    false,
			}
			if st, ok := statuses[src.ID]; ok {
				entry["synced"] = true
				entry["last_sync_at"] = st.LastSyncAt
				entry["states_listed"] = st.StatesListed
				entry["states_stored"] = st.StatesStored
				entry["read_errors"] = st.ReadErrors
				entry["last_error"] = st.LastError
				statesListed += st.StatesListed
				if st.ReadErrors > 0 || st.LastError != "" {
					sourceErrors++
				}
				if st.LastSyncAt > refreshedAt {
					refreshedAt = st.LastSyncAt
				}
			}
			sync = append(sync, entry)
		}

		resp := gin.H{
			"sources":            len(sources),
			"states":             totals.States,
			"states_listed":      statesListed,
			"rum":                totals.RUM,
			"managed_resources":  totals.ManagedResources,
			"data_sources":       totals.DataSources,
			"total_resources":    totals.TotalResources,
			"providers":          topCounts(providers, 0),
			"resource_types":     topCounts(resTypes, 10),
			"terraform_versions": topCounts(versions, 0),
			"source_errors":      sourceErrors,
			"sync":               sync,
		}
		if refreshedAt != "" {
			resp["refreshed_at"] = refreshedAt
		}
		c.JSON(http.StatusOK, resp)

		// After the response, never before it, and only behind the flag —
		// exactly as the /sources observation is placed (#393 Phase 2b). The
		// measurement must not be able to delay or replace a read that has
		// already succeeded, and it costs an extra query.
		//
		// What it measures: these aggregates come from state_analyses, which
		// never touches a partition root, so a Phase 3 flip of the ROOT reads
		// would leave a correctly-scoped source list sitting beside fleet-wide
		// totals (#455).
		if h.tenantDualRead {
			h.observeAnalysisScope(c, totals.States)
		}
	}
}

// versionOps are the comparison operators the states-by-version drill-down
// accepts: eq matches the stored version verbatim; the range operators compare
// by semantic version.
var versionOps = map[string]struct{}{"eq": {}, "lt": {}, "lte": {}, "gt": {}, "gte": {}}

// versionStatesCap bounds the states returned by the version drill-down. total in
// the response still reflects the full match, and truncated flags when the cap hit.
const versionStatesCap = 500

// StatesByVersion lists the state files whose Terraform version matches the
// given version and comparison operator, backing the dashboard's click-a-version
// drill-down. op defaults to "eq"; lt/lte/gt/gte select a semver range (e.g.
// everything older than 1.0.0). Requires state:read.
// @Summary      State files by Terraform version
// @Description  Lists state files matching a Terraform version and operator (eq, lt, lte, gt, gte). Range operators compare by semantic version and skip non-semver versions (e.g. "unknown"). Requires state:read.
// @Tags         Dashboard
// @Produce      json
// @Param        version  query     string  true   "Terraform version to match, or 'unknown' for states with no recorded version"
// @Param        op       query     string  false  "Comparison operator: eq (default), lt, lte, gt, gte"
// @Success      200      {object}  map[string]interface{}
// @Failure      400      {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /dashboard/states-by-version [get]
func (h *SourcesHandlers) StatesByVersion() gin.HandlerFunc {
	return func(c *gin.Context) {
		ver := c.Query("version")
		if ver == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "version is required"})
			return
		}
		op := c.Query("op")
		if op == "" {
			op = "eq"
		}
		if _, ok := versionOps[op]; !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid op"})
			return
		}
		ctx := c.Request.Context()

		// Scoped (#459). This route answers "which states run version X", and
		// unscoped it answered it for the whole fleet — a tenant clicking a bar
		// on its own dashboard listed other organizations' state files by name.
		scope, resolved := tenantscope.FromContext(c)
		if !resolved {
			serverError(c, errNoTenantScope, "the tenant scope was not resolved for this route")
			return
		}

		// Exact match (the common click-a-version-bar case) pushes the predicate and
		// a cap into SQL instead of loading the whole store to filter in Go. The
		// dashboard's "unknown" bucket is the empty version the store records.
		if op == "eq" {
			v := ver
			if v == "unknown" {
				v = ""
			}
			states, total, err := h.analysisRepo.StatesByVersionExactInScope(ctx, scope, v, versionStatesCap)
			if err != nil {
				serverError(c, err, "failed to load states by version")
				return
			}
			c.JSON(http.StatusOK, gin.H{
				"version":   ver,
				"op":        op,
				"states":    states,
				"total":     total,
				"truncated": total > len(states),
			})
			return
		}

		// Range operators need semantic-version comparison, so load and filter in Go,
		// then cap the result (with a truncated flag) to bound a very large bucket.
		rows, err := h.analysisRepo.StateVersionsInScope(ctx, scope)
		if err != nil {
			serverError(c, err, "failed to load state versions")
			return
		}
		matched := filterStatesByVersion(rows, ver, op)
		total := len(matched)
		truncated := false
		if len(matched) > versionStatesCap {
			matched = matched[:versionStatesCap]
			truncated = true
		}
		c.JSON(http.StatusOK, gin.H{
			"version":   ver,
			"op":        op,
			"states":    matched,
			"total":     total,
			"truncated": truncated,
		})
	}
}

// filterStatesByVersion selects the states matching (ver, op). For "eq" the
// match is exact on the stored string, with "unknown"/"" treated as the empty
// version the store records for stateless or legacy files. The range operators
// use semantic-version precedence and skip states whose version (or the pivot)
// isn't valid semver, since "unknown" has no place in a "< 1.0.0" range.
func filterStatesByVersion(rows []repositories.StateVersionRow, ver, op string) []repositories.StateVersionRow {
	out := make([]repositories.StateVersionRow, 0)
	for _, r := range rows {
		if matchesVersion(r.TerraformVersion, ver, op) {
			out = append(out, r)
		}
	}
	return out
}

// matchesVersion reports whether a stored Terraform version satisfies (ver, op).
// "eq" is an exact string match (with "unknown"/"" mapping to the empty version
// the store records); the range operators compare by semantic version and never
// match when either side isn't valid semver. Shared by the version drill-down
// and the Reports filter so both treat versions identically.
func matchesVersion(stored, ver, op string) bool {
	if op == "eq" {
		return matchesExactVersion(stored, ver)
	}
	if !semver.IsValid(stored) || !semver.IsValid(ver) {
		return false
	}
	return rangeMatch(semver.Compare(stored, ver), op)
}

// matchesExactVersion reports whether a stored version equals the requested one,
// mapping the dashboard's "unknown" bucket (and the empty pivot) to the empty
// string the store persists for states with no recorded version.
func matchesExactVersion(stored, ver string) bool {
	if ver == "unknown" || ver == "" {
		return stored == ""
	}
	return stored == ver
}

// rangeMatch reports whether a semver comparison result satisfies a range op.
func rangeMatch(cmp int, op string) bool {
	switch op {
	case "lt":
		return cmp < 0
	case "lte":
		return cmp <= 0
	case "gt":
		return cmp > 0
	case "gte":
		return cmp >= 0
	}
	return false
}

// refreshAnalysisAsync re-analyzes one state in the background after a
// TSM-initiated write so the dashboard reflects it immediately.
func (h *SourcesHandlers) refreshAnalysisAsync(src *repositories.Source, key string) {
	if h.syncer == nil {
		return
	}
	srcCopy := *src
	go h.syncer.SyncKey(&srcCopy, key)
}

// topCounts turns a map into a descending []Count (ties broken by key for
// determinism). limit <= 0 returns all entries.
func topCounts(m map[string]int, limit int) []analyzer.Count {
	out := make([]analyzer.Count, 0, len(m))
	for k, v := range m {
		out = append(out, analyzer.Count{Key: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
