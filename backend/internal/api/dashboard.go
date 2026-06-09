package api

import (
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/analyzer"
	"github.com/terraform-state-manager/terraform-state-manager/internal/statesource"
)

// DashboardOverview aggregates analyzer metrics across every configured source for
// the home page: totals (RUM, managed, data), and provider / resource-type /
// Terraform-version distributions. Per-source and per-state failures are tolerated
// (counted in source_errors) so one unreachable backend doesn't break the page.
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
		ctx := c.Request.Context()
		sources, err := h.repo.List(ctx)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list sources"})
			return
		}

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
			for _, ref := range refs {
				rs, err := conn.Read(ctx, ref.Key)
				if err != nil || rs == nil {
					continue
				}
				a, err := analyzer.Analyze(rs.Data)
				if err != nil {
					continue
				}
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
			}
		}

		c.JSON(http.StatusOK, gin.H{
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
		})
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
