package api

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/reporting"
)

// reportPreviewCap bounds the rows returned to the live Reports table; the
// reported total still reflects the full match, and exports return everything.
const reportPreviewCap = 500

// reportFilters is the parsed Reports query. Every field is optional and the
// set is AND-combined: a state must satisfy all provided predicates. Numeric
// bounds default to the widest range so an absent bound is a no-op.
type reportFilters struct {
	sourceIDs    []string
	keyQuery     string // lower-cased substring match on state_key
	version      string
	versionOp    string // eq (default) or lt/lte/gt/gte
	provider     string // lower-cased substring match on a provider key
	resourceType string // lower-cased substring match on a resource-type key

	rumMin, rumMax         int
	managedMin, managedMax int
	dataMin, dataMax       int
	totalMin, totalMax     int
	sizeMin, sizeMax       int64
}

// parseReportFilters reads the filter query params, defaulting numeric bounds to
// the widest range. A present-but-unparseable number, or an unknown version
// operator, is a client error.
func parseReportFilters(c *gin.Context) (reportFilters, error) {
	var perr error
	qi := func(name string, def int) int {
		s := c.Query(name)
		if s == "" || perr != nil {
			return def
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			perr = fmt.Errorf("invalid %s", name)
			return def
		}
		return n
	}
	qi64 := func(name string, def int64) int64 {
		s := c.Query(name)
		if s == "" || perr != nil {
			return def
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			perr = fmt.Errorf("invalid %s", name)
			return def
		}
		return n
	}

	f := reportFilters{
		sourceIDs:    c.QueryArray("source_id"),
		keyQuery:     strings.ToLower(strings.TrimSpace(c.Query("q"))),
		version:      strings.TrimSpace(c.Query("version")),
		versionOp:    c.Query("op"),
		provider:     strings.ToLower(strings.TrimSpace(c.Query("provider"))),
		resourceType: strings.ToLower(strings.TrimSpace(c.Query("resource_type"))),
		rumMin:       qi("rum_min", 0),
		rumMax:       qi("rum_max", math.MaxInt),
		managedMin:   qi("managed_min", 0),
		managedMax:   qi("managed_max", math.MaxInt),
		dataMin:      qi("data_min", 0),
		dataMax:      qi("data_max", math.MaxInt),
		totalMin:     qi("total_min", 0),
		totalMax:     qi("total_max", math.MaxInt),
		sizeMin:      qi64("size_min", 0),
		sizeMax:      qi64("size_max", math.MaxInt64),
	}
	if perr != nil {
		return f, perr
	}
	if f.version != "" {
		if f.versionOp == "" {
			f.versionOp = "eq"
		}
		if _, ok := versionOps[f.versionOp]; !ok {
			return f, fmt.Errorf("invalid op")
		}
	}
	return f, nil
}

// sqlFilter projects the SQL-pushable predicates (source membership, key
// substring, exact version, numeric ranges) into a repository filter so the DB
// does the bulk of the work. The no-op numeric bounds parseReportFilters applies
// (0 / MaxInt) are dropped, so an absent bound adds no WHERE term. The residual
// predicates (semver-range version, provider/resource-type substring) are NOT
// projected here — applyResidual handles them.
func (f reportFilters) sqlFilter() repositories.StateQueryFilter {
	q := repositories.StateQueryFilter{
		SourceIDs:   f.sourceIDs,
		KeyContains: f.keyQuery, // already lower-cased in parseReportFilters
	}
	if f.version != "" && f.versionOp == "eq" {
		v := f.version
		if v == "unknown" { // matchesExactVersion treats "unknown" as the empty version
			v = ""
		}
		q.VersionExact = &v
	}
	if f.rumMin > 0 {
		m := f.rumMin
		q.RUMMin = &m
	}
	if f.rumMax < math.MaxInt {
		m := f.rumMax
		q.RUMMax = &m
	}
	if f.managedMin > 0 {
		m := f.managedMin
		q.ManagedMin = &m
	}
	if f.managedMax < math.MaxInt {
		m := f.managedMax
		q.ManagedMax = &m
	}
	if f.dataMin > 0 {
		m := f.dataMin
		q.DataMin = &m
	}
	if f.dataMax < math.MaxInt {
		m := f.dataMax
		q.DataMax = &m
	}
	if f.totalMin > 0 {
		m := f.totalMin
		q.TotalMin = &m
	}
	if f.totalMax < math.MaxInt {
		m := f.totalMax
		q.TotalMax = &m
	}
	if f.sizeMin > 0 {
		m := f.sizeMin
		q.SizeMin = &m
	}
	if f.sizeMax < math.MaxInt64 {
		m := f.sizeMax
		q.SizeMax = &m
	}
	return q
}

// hasResidual reports whether the filter carries a predicate that cannot be
// expressed in SQL and so must be applied in Go after the WHERE-reduced fetch:
// a semver-range version comparison, or a provider/resource-type key substring.
func (f reportFilters) hasResidual() bool {
	return (f.version != "" && f.versionOp != "eq") || f.provider != "" || f.resourceType != ""
}

// applyResidual filters rows by the predicates SQL could not express. The clean
// predicates are already guaranteed by the WHERE clause, so this only re-checks
// the semver-range version and the provider/resource-type substrings. It is a
// no-op (returns rows unchanged) when no residual predicate is active.
func (f reportFilters) applyResidual(rows []repositories.StateRow) []repositories.StateRow {
	if !f.hasResidual() {
		return rows
	}
	out := make([]repositories.StateRow, 0, len(rows))
	for _, r := range rows {
		if f.version != "" && f.versionOp != "eq" && !matchesVersion(r.TerraformVersion, f.version, f.versionOp) {
			continue
		}
		if f.provider != "" && !mapKeyContains(r.Providers, f.provider) {
			continue
		}
		if f.resourceType != "" && !mapKeyContains(r.ResourceTypes, f.resourceType) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// capRows trims rows to the preview cap, reporting whether truncation occurred.
func capRows(rows []repositories.StateRow) ([]repositories.StateRow, bool) {
	if len(rows) > reportPreviewCap {
		return rows[:reportPreviewCap], true
	}
	return rows, false
}

// describe renders the active filters as a structured object (for the JSON
// export) and a human-readable string (for the Markdown export), built together
// so the two renderings can't drift.
func (f reportFilters) describe() (map[string]any, string) {
	m := map[string]any{}
	var parts []string
	if len(f.sourceIDs) > 0 {
		m["source_ids"] = f.sourceIDs
		parts = append(parts, fmt.Sprintf("%d source(s)", len(f.sourceIDs)))
	}
	if f.keyQuery != "" {
		m["key_contains"] = f.keyQuery
		parts = append(parts, fmt.Sprintf("key contains %q", f.keyQuery))
	}
	if f.version != "" {
		m["version"] = f.version
		m["version_op"] = f.versionOp
		parts = append(parts, fmt.Sprintf("version %s %s", opSymbol(f.versionOp), f.version))
	}
	if f.provider != "" {
		m["provider"] = f.provider
		parts = append(parts, fmt.Sprintf("provider %q", f.provider))
	}
	if f.resourceType != "" {
		m["resource_type"] = f.resourceType
		parts = append(parts, fmt.Sprintf("resource type %q", f.resourceType))
	}
	addRange(m, &parts, "rum", f.rumMin, f.rumMax, math.MaxInt)
	addRange(m, &parts, "managed_resources", f.managedMin, f.managedMax, math.MaxInt)
	addRange(m, &parts, "data_sources", f.dataMin, f.dataMax, math.MaxInt)
	addRange(m, &parts, "total_resources", f.totalMin, f.totalMax, math.MaxInt)
	addRange64(m, &parts, "size", f.sizeMin, f.sizeMax, math.MaxInt64)
	return m, strings.Join(parts, "; ")
}

// reportPreviewRow is the lean per-state row the live table renders: scalar
// columns plus the identity needed to deep-link into the Sources detail view.
// The provider/resource-type maps are intentionally omitted to keep the preview
// payload small — they're only emitted in the exports.
type reportPreviewRow struct {
	SourceID         string `json:"source_id"`
	SourceName       string `json:"source_name"`
	SourceType       string `json:"source_type"`
	StateKey         string `json:"state_key"`
	TerraformVersion string `json:"terraform_version"`
	Serial           int64  `json:"serial"`
	Size             int64  `json:"size"`
	RUM              int    `json:"rum"`
	ManagedResources int    `json:"managed_resources"`
	DataSources      int    `json:"data_sources"`
	TotalResources   int    `json:"total_resources"`
	AnalyzedAt       string `json:"analyzed_at"`
}

// ReportStates lists the analyzed state files matching the filter query, with a
// summary over the full match and a capped row slice for the live table. Use
// POST /reconcile (scoped to source_ids) to refresh the store first.
// Requires state:read.
// @Summary      Query state files for reporting
// @Description  Lists analyzed state files matching optional filters (source_id[], q, version+op, provider, resource_type, and rum/managed/data/total/size min-max). Returns a summary over the full match plus up to 500 rows for the table. Use POST /reconcile (scoped to source_ids) to refresh the store first. Requires state:read.
// @Tags         Reports
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /reports/states [get]
func (h *SourcesHandlers) ReportStates() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		f, err := parseReportFilters(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// Fast path: with no residual predicate, the SQL WHERE fully captures the
		// filter, so the DB returns the exact page plus the full-match COUNT/SUMs
		// (window functions) — no full-store load, no JSONB unmarshal, no Go pass.
		if !f.hasResidual() {
			preview, agg, err := h.analysisRepo.PreviewStatesWithTotals(ctx, f.sqlFilter(), reportPreviewCap)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load states"})
				return
			}
			c.JSON(http.StatusOK, gin.H{
				"total":     agg.Matched,
				"truncated": agg.Matched > reportPreviewCap,
				"summary": reporting.StatesSummary{
					Matched:          agg.Matched,
					RUM:              agg.RUM,
					ManagedResources: agg.ManagedResources,
					DataSources:      agg.DataSources,
					TotalResources:   agg.TotalResources,
				},
				"states": toPreviewRows(preview),
			})
			return
		}

		// Residual path: the WHERE clause still reduces the scan to the clean
		// predicates; the semver-range / provider / resource-type predicates are
		// applied in Go over that reduced set.
		rows, err := h.analysisRepo.FilterStates(ctx, f.sqlFilter())
		if err != nil {
			serverError(c, err, "failed to load states")
			return
		}
		matched := f.applyResidual(rows)
		capped, truncated := capRows(matched)
		c.JSON(http.StatusOK, gin.H{
			"total":     len(matched),
			"truncated": truncated,
			"summary":   reporting.SummarizeStates(toStateRecords(matched)),
			"states":    toPreviewRows(capped),
		})
	}
}

// refreshForReport reconciles the analysis store before a report read so the
// table reflects current state. It scopes the reconcile to the selected
// source(s) when the filter names any, so a filtered view refreshes only its own
// data instead of the whole fleet (which matters for backend rate limits); an
// unscoped report falls back to a full cycle. Best-effort: a cycle already in
// progress (ErrSyncInProgress) or any error leaves the existing store served.
// ReportStatesExport downloads the filtered state files as a report in the
// requested format (json, md, or csv). The filter set is the same as
// ReportStates. Requires state:read.
// @Summary      Export filtered state files
// @Description  Downloads the state files matching the same filters as /reports/states as a report (format: json, md, or csv). Requires state:read.
// @Tags         Reports
// @Produce      json,text/markdown,text/csv
// @Param        format  query  string  false  "Report format: json, md, or csv"  Enums(json, md, csv)
// @Success      200  {string}  string  "report file"
// @Failure      400  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /reports/states/export [get]
func (h *SourcesHandlers) ReportStatesExport() gin.HandlerFunc {
	return func(c *gin.Context) {
		f, err := parseReportFilters(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		format := reporting.Format(c.DefaultQuery("format", "json"))
		// Exports need every matched row (all columns), so the WHERE clause reduces
		// the scan to the clean predicates and the residual pass finishes the rest.
		rows, err := h.analysisRepo.FilterStates(c.Request.Context(), f.sqlFilter())
		if err != nil {
			serverError(c, err, "failed to load states")
			return
		}
		matched := f.applyResidual(rows)
		filters, filterText := f.describe()
		meta := reporting.StatesReportMeta{
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
			Filters:     filters,
			FilterText:  filterText,
		}
		contentType, filename, body, err := reporting.GenerateStatesReport(toStateRecords(matched), meta, format)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		h.audit.write(c, "report.states.generate", "report", "states",
			map[string]interface{}{"format": string(format), "matched": len(matched)})
		c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
		c.Data(http.StatusOK, contentType, body)
	}
}

// toPreviewRows projects the matched rows onto the lean table shape.
func toPreviewRows(rows []repositories.StateRow) []reportPreviewRow {
	out := make([]reportPreviewRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, reportPreviewRow{
			SourceID: r.SourceID, SourceName: r.SourceName, SourceType: r.SourceType,
			StateKey: r.StateKey, TerraformVersion: r.TerraformVersion,
			Serial: r.Serial, Size: r.Size, RUM: r.RUM,
			ManagedResources: r.ManagedResources, DataSources: r.DataSources,
			TotalResources: r.TotalResources, AnalyzedAt: r.AnalyzedAt,
		})
	}
	return out
}

// toStateRecords maps persisted rows onto the reporting package's neutral input.
func toStateRecords(rows []repositories.StateRow) []reporting.StateRecord {
	out := make([]reporting.StateRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, reporting.StateRecord{
			SourceName: r.SourceName, SourceType: r.SourceType, StateKey: r.StateKey,
			TerraformVersion: r.TerraformVersion, Serial: r.Serial, Lineage: r.Lineage,
			Size: r.Size, RUM: r.RUM, ManagedResources: r.ManagedResources,
			DataSources: r.DataSources, TotalResources: r.TotalResources,
			Providers: r.Providers, ResourceTypes: r.ResourceTypes, AnalyzedAt: r.AnalyzedAt,
		})
	}
	return out
}

// mapKeyContains reports whether any key of m contains sub (both lower-cased).
func mapKeyContains(m map[string]int, sub string) bool {
	for k := range m {
		if strings.Contains(strings.ToLower(k), sub) {
			return true
		}
	}
	return false
}

// opSymbol renders a version operator for the human-readable filter summary.
func opSymbol(op string) string {
	switch op {
	case "lt":
		return "<"
	case "lte":
		return "≤"
	case "gt":
		return ">"
	case "gte":
		return "≥"
	default:
		return "="
	}
}

// addRange records a numeric bound in the filter description when it isn't the
// widest default (min > 0 or max below the sentinel).
func addRange(m map[string]any, parts *[]string, name string, min, max, maxSentinel int) {
	lo, hi := min > 0, max < maxSentinel
	if lo {
		m[name+"_min"] = min
	}
	if hi {
		m[name+"_max"] = max
	}
	switch {
	case lo && hi:
		*parts = append(*parts, fmt.Sprintf("%s %d–%d", name, min, max))
	case lo:
		*parts = append(*parts, fmt.Sprintf("%s ≥ %d", name, min))
	case hi:
		*parts = append(*parts, fmt.Sprintf("%s ≤ %d", name, max))
	}
}

func addRange64(m map[string]any, parts *[]string, name string, min, max, maxSentinel int64) {
	lo, hi := min > 0, max < maxSentinel
	if lo {
		m[name+"_min"] = min
	}
	if hi {
		m[name+"_max"] = max
	}
	switch {
	case lo && hi:
		*parts = append(*parts, fmt.Sprintf("%s %d–%d", name, min, max))
	case lo:
		*parts = append(*parts, fmt.Sprintf("%s ≥ %d", name, min))
	case hi:
		*parts = append(*parts, fmt.Sprintf("%s ≤ %d", name, max))
	}
}
