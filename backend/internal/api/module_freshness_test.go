package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	identityhttpsafe "github.com/sethbacon/terraform-suite-identity/identity/httpsafe"
	"github.com/sethbacon/terraform-suite-identity/identity/suite"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

func freshnessVerPtr(s string) *string { return &s }

// stubRegistry serves the registry versions endpoint for two modules and counts
// hits so we can assert constraint-only modules trigger NO outbound call.
func stubRegistry(t *testing.T, hits *map[string]int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(*hits)[r.URL.Path]++
		switch r.URL.Path {
		case "/v1/modules/acme/vpc/aws/versions":
			// 5.7.1 is the latest NON-deprecated; 9.9.9 is deprecated (must be ignored).
			_, _ = w.Write([]byte(`{"modules":[{"versions":[
				{"version":"5.3.0"},{"version":"v5.7.1"},{"version":"9.9.9","deprecated":true}]}]}`))
		case "/v1/modules/acme/db/aws/versions":
			w.WriteHeader(http.StatusNotFound) // -> unknown
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
}

func TestComputeFreshness_ActiveSibling(t *testing.T) {
	hits := map[string]int{}
	reg := stubRegistry(t, &hits)
	defer reg.Close()
	host := suite.CanonicalHost(reg.URL)

	refs := []repositories.StateModuleRef{
		{ModuleSource: "acme/vpc/aws", RegistryHost: host, ModuleVersion: freshnessVerPtr("5.3.0")},               // behind 5.7.1
		{ModuleSource: "acme/vpc/aws", RegistryHost: host, ModuleVersion: freshnessVerPtr("5.7.1")},               // up_to_date
		{ModuleSource: "acme/cons/aws", RegistryHost: host, ModuleVersion: nil},                                   // constraint_only (no call)
		{ModuleSource: "acme/db/aws", RegistryHost: host, ModuleVersion: freshnessVerPtr("1.0.0")},                // unknown (404)
		{ModuleSource: "x/y/aws", RegistryHost: "registry.terraform.io", ModuleVersion: freshnessVerPtr("1.0.0")}, // no_registry (host)
	}

	got := computeFreshness(context.Background(), &http.Client{}, reg.URL, host, refs)
	if len(got) != len(refs) {
		t.Fatalf("got %d verdicts, want %d", len(got), len(refs))
	}

	want := []struct {
		status string
		latest string // "" => nil
	}{
		{"behind", "v5.7.1"},
		{"up_to_date", "v5.7.1"},
		{"constraint_only", ""},
		{"unknown", ""},
		{"no_registry", ""},
	}
	for i, w := range want {
		if got[i].Status != w.status {
			t.Errorf("ref %d: status = %q, want %q", i, got[i].Status, w.status)
		}
		if w.latest == "" {
			if got[i].Latest != nil {
				t.Errorf("ref %d: latest = %v, want nil", i, *got[i].Latest)
			}
		} else if got[i].Latest == nil || *got[i].Latest != w.latest {
			t.Errorf("ref %d: latest = %v, want %q (deprecated 9.9.9 must be ignored)", i, got[i].Latest, w.latest)
		}
	}

	// constraint_only must make ZERO registry calls; the two vpc refs share one
	// cached lookup (fetched once).
	if hits["/v1/modules/acme/cons/aws/versions"] != 0 {
		t.Errorf("constraint_only module triggered a registry call (want 0)")
	}
	if hits["/v1/modules/acme/vpc/aws/versions"] != 1 {
		t.Errorf("vpc fetched %d times, want 1 (per-request cache)", hits["/v1/modules/acme/vpc/aws/versions"])
	}
}

func TestComputeFreshness_Standalone(t *testing.T) {
	refs := []repositories.StateModuleRef{
		{ModuleSource: "acme/vpc/aws", RegistryHost: "app.terraform.io", ModuleVersion: freshnessVerPtr("5.3.0")},
		{ModuleSource: "acme/db/aws", RegistryHost: "app.terraform.io", ModuleVersion: nil},
	}
	// No active sibling: siblingURL/host empty.
	got := computeFreshness(context.Background(), &http.Client{}, "", "", refs)
	for i, mf := range got {
		if mf.Status != "no_registry" {
			t.Errorf("ref %d: standalone status = %q, want no_registry", i, mf.Status)
		}
		if mf.Latest != nil {
			t.Errorf("ref %d: standalone latest must be nil", i)
		}
	}
	// current is still the locked version (informational), even when no_registry.
	if got[0].Current == nil || *got[0].Current != "5.3.0" {
		t.Errorf("current should carry the locked version even when no_registry")
	}
}

// --- the egress guard on the sibling-asserted publicUrl ------------------------

// moduleRefRows mirrors StateModuleRefRepository.ListBySource' projection.
func moduleRefRows(host, version string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"source_id", "state_key", "module_source", "module_version", "registry_host", "observed_at"}).
		AddRow("s1", "k", "acme/vpc/aws", version, host, "2026-07-10T00:00:00Z")
}

// waitActive polls until the discovery client has fetched the sibling manifest.
func waitActive(t *testing.T, dc *suite.DiscoveryClient) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := dc.Snapshot(); st == suite.StateActive {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("sibling never became active")
}

// siblingAdvertising starts a manifest server that advertises publicURL as its
// own address, and returns a discovery client polling it.
func siblingAdvertising(t *testing.T, publicURL string) *suite.DiscoveryClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(suite.Manifest{
			SchemaVersion: suite.SchemaVersionV1,
			App:           "terraform-registry",
			PublicURL:     suite.UntrustedURL(publicURL),
		})
	}))
	t.Cleanup(srv.Close)
	dc := suite.NewInsecureDiscoveryClient(srv.URL, suite.Manifest{
		SchemaVersion: suite.SchemaVersionV1, App: "terraform-state-manager",
	}, time.Minute, identityhttpsafe.MustGuard("127.0.0.1", "::1"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go dc.Start(ctx)
	waitActive(t, dc)
	return dc
}

// serveFreshness drives ListStateModuleFreshness with one captured module ref
// pointing at wantHost.
func serveFreshness(t *testing.T, dc *suite.DiscoveryClient, db *sql.DB, wantHost string) *httptest.ResponseRecorder {
	t.Helper()
	h := NewSourcesHandlers(db, nil)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/sources/:id/modules/freshness",
		h.ListStateModuleFreshness(func() *suite.DiscoveryClient { return dc }))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/sources/s1/modules/freshness", nil))
	return w
}

// TestListStateModuleFreshness_RefusesSiblingAdvertisingDeniedAddress is the
// regression guard for identity #144.
//
// publicUrl is asserted BY THE SIBLING, not pinned by the operator, so before
// terraform-suite-identity v0.25.0 this handler read it into a bare
// &http.Client{} and issued a GET to whatever address the manifest named — from
// inside the deployment network, with Go's default cross-host redirect
// following. A compromised, tricked or merely misconfigured sibling therefore
// chose the destination.
//
// SiblingPublicURL validates that value against the deployment's egress policy
// and GuardedClient supplies the matching dialer. This test pins the outcome
// that proves both are in place: a sibling advertising the cloud metadata
// address is not dialed at all, and the endpoint still answers 200 with
// no_registry rather than erroring.
func TestListStateModuleFreshness_RefusesSiblingAdvertisingDeniedAddress(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	// 169.254.169.254 is link-local: denied by every guard this package builds,
	// including the loopback-widened one TestMain installs.
	const denied = "http://169.254.169.254"
	mock.ExpectQuery("FROM state_module_refs").
		WillReturnRows(moduleRefRows(suite.CanonicalHost(denied), "1.0.0"))

	w := serveFreshness(t, siblingAdvertising(t, denied), db, suite.CanonicalHost(denied))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"no_registry"`) {
		t.Errorf("a sibling advertising a denied address must not be dialed; "+
			"the module degrades to no_registry. got %s", w.Body.String())
	}
	for _, forbidden := range []string{`"behind"`, `"up_to_date"`, `"unknown"`} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Errorf("status %s means the handler resolved the denied publicUrl and dialed it: %s",
				forbidden, w.Body.String())
		}
	}
}

// TestListStateModuleFreshness_UsesPermittedSiblingPublicURL is the
// counterweight: the guard must not have turned every sibling into no_registry.
// A sibling on an address the policy permits is still followed, so the panel
// keeps working.
func TestListStateModuleFreshness_UsesPermittedSiblingPublicURL(t *testing.T) {
	hits := map[string]int{}
	reg := stubRegistry(t, &hits)
	defer reg.Close()

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery("FROM state_module_refs").
		WillReturnRows(moduleRefRows(suite.CanonicalHost(reg.URL), "5.3.0"))

	w := serveFreshness(t, siblingAdvertising(t, reg.URL), db, suite.CanonicalHost(reg.URL))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"behind"`) {
		t.Errorf("a permitted sibling must still be reached: %s", w.Body.String())
	}
	if hits["/v1/modules/acme/vpc/aws/versions"] == 0 {
		t.Error("the guarded client never reached the permitted registry")
	}
}

// TestListStateModuleFreshness_BuildsNoUnguardedClient closes the half of the
// guard a behavioural test cannot reach.
//
// SiblingPublicURL and GuardedClient are two halves of one control, and only the
// first is observable from outside: a handler that validates the sibling's
// publicUrl and then dials it with a bare &http.Client{} passes every test
// above, because the pre-flight already refused the denied host. What it loses
// is the dial-time re-check — the thing that stops a name resolving to a
// permitted address at validation and a denied one at connect, which is the
// whole reason httpsafe resolves and pins rather than just parsing. That is not
// reproducible without a controllable resolver, so it is asserted structurally
// instead: this file constructs no HTTP client of its own.
func TestListStateModuleFreshness_BuildsNoUnguardedClient(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "module_freshness.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse module_freshness.go: %v", err)
	}
	var found bool
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "http" && sel.Sel.Name == "Client" {
			found = true
			return false
		}
		return true
	})
	if found {
		t.Error("module_freshness.go constructs its own http.Client. The sibling-following " +
			"request must use DiscoveryClient.GuardedClient (or httpsafe.NewClient with the " +
			"deployment guard) so the destination is re-checked at DIAL time, not only " +
			"pre-flighted — see identity #144.")
	}
}
