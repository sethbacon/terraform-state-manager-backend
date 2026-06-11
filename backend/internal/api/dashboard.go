package api

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/analyzer"
	"github.com/terraform-state-manager/terraform-state-manager/internal/statesource"
)

// Dashboard aggregation guardrails: remote backends (e.g. HCP Terraform) cost
// two HTTP round-trips per state read, so an org with many workspaces read
// serially can exceed proxy timeouts. Reads run dashboardReadConcurrency-wide
// per source (kept modest — HCP rate-limits ~30 req/30s per token) under a hard
// dashboardBudget; whatever doesn't finish is dropped and the source is counted
// in source_errors so the page shows partial data instead of timing out.
//
// On top of that the result is CACHED: a background refresher recomputes every
// dashboardRefreshInterval, the handler serves the cache while it is younger
// than dashboardCacheTTL, and ?refresh=true forces a recompute.
const (
	dashboardBudget          = 12 * time.Second
	dashboardReadConcurrency = 6
	dashboardRefreshInterval = 5 * time.Minute
	dashboardCacheTTL        = 10 * time.Minute
)

// overviewCache holds the most recent aggregation result.
type overviewCache struct {
	mu   sync.Mutex
	data gin.H
	at   time.Time
}

func (c *overviewCache) get() (gin.H, time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data, c.at, c.data != nil
}

func (c *overviewCache) set(d gin.H) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = d
	c.at = time.Now()
}

// StartOverviewRefresher computes the dashboard overview in the background on a
// fixed interval so page loads serve a warm cache. Returns a stop func.
func (h *SourcesHandlers) StartOverviewRefresher() func() {
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(dashboardRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), dashboardBudget)
				if data, err := h.computeOverview(ctx); err == nil {
					h.overview.set(data)
				}
				cancel()
			}
		}
	}()
	return func() { close(stop) }
}

// DashboardOverview aggregates analyzer metrics across every configured source for
// the home page: totals (RUM, managed, data), and provider / resource-type /
// Terraform-version distributions. Per-source and per-state failures are tolerated
// and reads run bounded-concurrent under a hard time budget, so one unreachable
// or slow backend yields partial data (flagged via source_errors) rather than a
// page timeout.
// @Summary      Dashboard overview
// @Description  Aggregates RUM and resource/provider/Terraform-version breakdowns across all sources. Requires state:read.
// @Tags         Dashboard
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /dashboard/overview [get]
func (h *SourcesHandlers) DashboardOverview() gin.HandlerFunc {
	return func(c *gin.Context) {
		force := c.Query("refresh") == "true"
		if !force {
			if data, at, ok := h.overview.get(); ok && time.Since(at) < dashboardCacheTTL {
				c.JSON(http.StatusOK, data)
				return
			}
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), dashboardBudget)
		defer cancel()
		data, err := h.computeOverview(ctx)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list sources"})
			return
		}
		h.overview.set(data)
		c.JSON(http.StatusOK, data)
	}
}

// computeOverview runs the budgeted, bounded-concurrent aggregation.
func (h *SourcesHandlers) computeOverview(ctx context.Context) (gin.H, error) {
	{
		sources, err := h.repo.List(ctx)
		if err != nil {
			return nil, err
		}

		var mu sync.Mutex
		var stateCount, rum, managed, data, total, sourceErrors int
		providers := map[string]int{}
		resTypes := map[string]int{}
		versions := map[string]int{}

		for i := range sources {
			s := &sources[i]
			creds, err := decryptCredentials(s)
			if err != nil {
				sourceErrors++
				continue
			}
			conn, err := statesource.New(s.Type, s.Config, creds)
			if err != nil {
				sourceErrors++
				continue
			}
			refs, err := conn.List(ctx)
			if err != nil {
				sourceErrors++
				continue
			}

			// Read + analyze states concurrently (bounded) under the budget.
			var readErrs atomic.Int32
			sem := make(chan struct{}, dashboardReadConcurrency)
			var wg sync.WaitGroup
			for _, ref := range refs {
				if ctx.Err() != nil {
					readErrs.Add(1)
					continue
				}
				wg.Add(1)
				sem <- struct{}{}
				go func(key string) {
					defer wg.Done()
					defer func() { <-sem }()
					rs, err := conn.Read(ctx, key)
					if err != nil || rs == nil {
						readErrs.Add(1)
						return
					}
					a, err := analyzer.Analyze(rs.Data)
					if err != nil {
						readErrs.Add(1)
						return
					}
					mu.Lock()
					defer mu.Unlock()
					stateCount++
					rum += a.RUM
					managed += a.ManagedResources
					data += a.DataSources
					total += a.TotalResources
					for _, p := range a.Providers {
						providers[p.Key] += p.Count
					}
					for _, rt := range a.ResourceTypes {
						resTypes[rt.Key] += rt.Count
					}
					v := a.TerraformVersion
					if v == "" {
						v = "unknown"
					}
					versions[v]++
				}(ref.Key)
			}
			wg.Wait()
			// Any unread/unanalyzed state (including budget exhaustion) flags the
			// source so the page banner reports partial data.
			if readErrs.Load() > 0 {
				sourceErrors++
			}
		}

		return gin.H{
			"sources":            len(sources),
			"states":             stateCount,
			"rum":                rum,
			"managed_resources":  managed,
			"data_sources":       data,
			"total_resources":    total,
			"providers":          topCounts(providers, 0),
			"resource_types":     topCounts(resTypes, 10),
			"terraform_versions": topCounts(versions, 0),
			"source_errors":      sourceErrors,
			"refreshed_at":       time.Now().UTC().Format(time.RFC3339),
		}, nil
	}
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
