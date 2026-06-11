package api

import (
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/analyzer"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/statesync"
)

// The dashboard aggregates the persistent state-analysis store (kept
// reconciled by the statesync service) instead of re-reading every state file
// from its backend per request. That keeps the endpoint O(store rows) no
// matter how many state files the backends hold, and the numbers stable
// between loads. ?refresh=true runs a reconcile cycle first (steady state:
// one listing call per source plus reads for changed states only); if a cycle
// is already running the current store is served as-is.

// DashboardOverview aggregates analyzer metrics across every configured source
// for the home page: totals (RUM, managed, data), provider / resource-type /
// Terraform-version distributions, and per-source sync freshness.
// @Summary      Dashboard overview
// @Description  Aggregates RUM and resource/provider/Terraform-version breakdowns from the persistent analysis store, with per-source sync status. ?refresh=true reconciles first. Requires state:read.
// @Tags         Dashboard
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /dashboard/overview [get]
func (h *SourcesHandlers) DashboardOverview() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		if c.Query("refresh") == "true" && h.syncer != nil {
			if err := h.syncer.SyncAll(ctx); err != nil && err != statesync.ErrSyncInProgress {
				// Refresh is best-effort: serve the store either way.
				_ = err
			}
		}

		sources, err := h.repo.List(ctx)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list sources"})
			return
		}
		totals, err := h.analysisRepo.Totals(ctx)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to aggregate analyses"})
			return
		}
		providers, err := h.analysisRepo.ProviderCounts(ctx)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to aggregate providers"})
			return
		}
		resTypes, err := h.analysisRepo.ResourceTypeCounts(ctx)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to aggregate resource types"})
			return
		}
		versions, err := h.analysisRepo.VersionCounts(ctx)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to aggregate versions"})
			return
		}
		statuses, err := h.analysisRepo.SyncStatuses(ctx)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load sync status"})
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
	}
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
