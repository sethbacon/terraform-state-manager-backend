package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// MUTATION-VERIFIED. Each was run against a deliberately broken comparator:
//
//	diffSourceIDs computes only one direction -> TestDiffSourceIDs (widened cases)
//	widened reported at WARN like withheld    -> TestReportSourceDivergenceLevels
//	an unresolved scope compared as empty     -> TestScopeToCompare...
//	the id cap removed                        -> TestCapIDs
//
// The rows themselves are never logged. A Source carries an encrypted credential
// blob and a connector config; ids are the only field that may appear, and every
// list endpoint hands those out already.

const (
	dualOrgA = "11111111-1111-4111-8111-111111111111"
)

func src(ids ...string) []repositories.Source {
	out := make([]repositories.Source, 0, len(ids))
	for _, id := range ids {
		out = append(out, repositories.Source{ID: id})
	}
	return out
}

func TestDiffSourceIDs(t *testing.T) {
	tests := []struct {
		name             string
		unscoped, scoped []repositories.Source
		withheld         []string
		widened          []string
	}{
		{
			name:     "equivalence: one organization, the two readers agree",
			unscoped: src("a", "b", "c"),
			scoped:   src("a", "b", "c"),
		},
		{
			name:     "divergence: the scoped read withholds another tenant's rows",
			unscoped: src("a", "b", "c"),
			scoped:   src("a"),
			withheld: []string{"b", "c"},
		},
		{
			name:     "the empty scope withholds everything",
			unscoped: src("a", "b"),
			scoped:   nil,
			withheld: []string{"a", "b"},
		},
		{
			// Impossible by construction — the scoped query is the unscoped
			// query plus a conjunct — so this direction exists to catch the two
			// readers having stopped asking the same question.
			name:     "widening: the scoped read returned a row the unscoped read did not",
			unscoped: src("a"),
			scoped:   src("a", "b"),
			widened:  []string{"b"},
		},
		{
			name:     "both directions at once",
			unscoped: src("a", "b"),
			scoped:   src("b", "c"),
			withheld: []string{"a"},
			widened:  []string{"c"},
		},
		{
			name: "nothing to compare",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withheld, widened := diffSourceIDs(tt.unscoped, tt.scoped)
			if strings.Join(withheld, ",") != strings.Join(tt.withheld, ",") {
				t.Errorf("withheld = %v, want %v", withheld, tt.withheld)
			}
			if strings.Join(widened, ",") != strings.Join(tt.widened, ",") {
				t.Errorf("widened = %v, want %v", widened, tt.widened)
			}
		})
	}
}

func TestCapIDs(t *testing.T) {
	many := make([]string, divergenceIDCap+5)
	for i := range many {
		many[i] = strconv.Itoa(i)
	}
	if got := len(capIDs(many)); got != divergenceIDCap {
		t.Errorf("capIDs kept %d ids, want %d — one request must not write a log line the size of the fleet", got, divergenceIDCap)
	}
	if got := len(capIDs([]string{"a", "b"})); got != 2 {
		t.Errorf("capIDs dropped ids below the cap: kept %d, want 2", got)
	}
}

func dualReadContext(t *testing.T) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/api/v1/sources", nil)
	return c
}

// An unresolved scope is NOT compared as an empty one. Doing so would report
// every served row as withheld and bury the real signal under an artefact of the
// membership resolver being down — turning the one measurement Phase 3 is gated
// on into noise at exactly the moment it is least reliable.
func TestScopeToCompareDeclinesAnUnresolvedScope(t *testing.T) {
	if _, ok := scopeToCompare(dualReadContext(t)); ok {
		t.Fatal("scopeToCompare accepted a request whose scope was never resolved")
	}

	c := dualReadContext(t)
	tenantscope.Store(c, tenantscope.Scope{})
	scope, ok := scopeToCompare(c)
	if !ok {
		t.Fatal("scopeToCompare declined a resolved-but-empty scope; " +
			"\"this caller may read nothing\" is a real answer and is exactly what must be counted")
	}
	if !scope.Empty() {
		t.Fatalf("scope = %+v, want the empty scope", scope)
	}
}

// Withheld and widened are not interchangeable, and the level is how an operator
// tells them apart without parsing the line: withheld may be #393's leak being
// measured correctly, widened cannot be anything but a defect.
func TestReportSourceDivergenceLevels(t *testing.T) {
	tests := []struct {
		name      string
		withheld  []string
		widened   []string
		wantLevel string
		wantLine  bool
	}{
		{name: "agreement is not logged", wantLine: false},
		{name: "withheld warns", withheld: []string{"a"}, wantLevel: "WARN", wantLine: true},
		{name: "widened errors", widened: []string{"b"}, wantLevel: "ERROR", wantLine: true},
		{name: "both errors", withheld: []string{"a"}, widened: []string{"b"}, wantLevel: "ERROR", wantLine: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			reportSourceDivergence(dualReadContext(t), "/api/v1/sources",
				tenantscope.Scope{OrgIDs: []string{dualOrgA}}, tt.withheld, tt.widened)

			if !tt.wantLine {
				if buf.Len() != 0 {
					t.Fatalf("agreement produced a log line: %s", buf.String())
				}
				return
			}
			var rec map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
				t.Fatalf("log line is not JSON (%v): %s", err, buf.String())
			}
			if rec["level"] != tt.wantLevel {
				t.Errorf("level = %v, want %v", rec["level"], tt.wantLevel)
			}
			if rec["scope_organizations"] != float64(1) {
				t.Errorf("scope_organizations = %v, want 1", rec["scope_organizations"])
			}
		})
	}
}
