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

// matches reports whether a state satisfies every provided predicate.
func (f reportFilters) matches(r repositories.StateRow) bool {
	if len(f.sourceIDs) > 0 && !containsString(f.sourceIDs, r.SourceID) {
		return false
	}
	if f.keyQuery != "" && !strings.Contains(strings.ToLower(r.StateKey), f.keyQuery) {
		return false
	}
	if f.version != "" && !matchesVersion(r.TerraformVersion, f.version, f.versionOp) {
		return false
	}
	if f.provider != "" && !mapKeyContains(r.Providers, f.provider) {
		return false
	}
	if f.resourceType != "" && !mapKeyContains(r.ResourceTypes, f.resourceType) {
		return false
	}
	if r.RUM < f.rumMin || r.RUM > f.rumMax {
		return false
	}
	if r.ManagedResources < f.managedMin || r.ManagedResources > f.managedMax {
		return false
	}
	if r.DataSources < f.dataMin || r.DataSources > f.dataMax {
		return false
	}
	if r.TotalResources < f.totalMin || r.TotalResources > f.totalMax {
		return false
	}
	if r.Size < f.sizeMin || r.Size > f.sizeMax {
		return false
	}
	return true
}

// applyReportFilters returns the subset of rows matching the filters, preserving
// order. Shared by the preview and export endpoints so they can never diverge.
func applyReportFilters(rows []repositories.StateRow, f reportFilters) []repositories.StateRow {
	out := make([]repositories.StateRow, 0)
	for _, r := range rows {
		if f.matches(r) {
			out = append(out, r)
		}
	}
	return out
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
// summary over the full match and a capped row slice for the live table.
// Requires state:read.
// @Summary      Query state files for reporting
// @Description  Lists analyzed state files matching optional filters (source_id[], q, version+op, provider, resource_type, and rum/managed/data/total/size min-max). Returns a summary over the full match plus up to 500 rows for the table. Requires state:read.
// @Tags         Reports
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /reports/states [get]
func (h *SourcesHandlers) ReportStates() gin.HandlerFunc {
	return func(c *gin.Context) {
		f, err := parseReportFilters(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		rows, err := h.analysisRepo.AllStates(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load states"})
			return
		}
		matched := applyReportFilters(rows, f)
		summary := reporting.SummarizeStates(toStateRecords(matched))

		capped := matched
		truncated := false
		if len(capped) > reportPreviewCap {
			capped = capped[:reportPreviewCap]
			truncated = true
		}
		c.JSON(http.StatusOK, gin.H{
			"total":     len(matched),
			"truncated": truncated,
			"summary":   summary,
			"states":    toPreviewRows(capped),
		})
	}
}

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
		rows, err := h.analysisRepo.AllStates(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load states"})
			return
		}
		matched := applyReportFilters(rows, f)
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

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
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
