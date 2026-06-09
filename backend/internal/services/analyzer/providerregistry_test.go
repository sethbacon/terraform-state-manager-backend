package analyzer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseProviderSource(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		wantNS   string
		wantType string
		wantOK   bool
	}{
		{"fully qualified", "registry.terraform.io/hashicorp/aws", "hashicorp", "aws", true},
		{"short form", "hashicorp/aws", "hashicorp", "aws", true},
		{"whitespace tolerated", "  hashicorp/aws  ", "hashicorp", "aws", true},
		{"single segment", "aws", "", "", false},
		{"empty", "", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns, typ, ok := parseProviderSource(tt.source)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantNS, ns)
			assert.Equal(t, tt.wantType, typ)
		})
	}
}

func TestLatestStableVersion(t *testing.T) {
	tests := []struct {
		name     string
		versions []string
		want     string
	}{
		{"picks highest", []string{"5.30.0", "5.40.0", "5.31.0"}, "5.40.0"},
		{"skips prerelease", []string{"5.40.0", "6.0.0-beta1", "6.0.0-rc1"}, "5.40.0"},
		{"numeric ordering", []string{"1.9.0", "1.10.0"}, "1.10.0"},
		{"all prerelease yields empty", []string{"1.0.0-alpha", "1.0.0-beta"}, ""},
		{"empty list", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, latestStableVersion(tt.versions))
		})
	}
}

// TestStaticProviderVersionSource verifies the fixture/stub source used by CI
// so tests never reach the live registry.
func TestStaticProviderVersionSource(t *testing.T) {
	src := &StaticProviderVersionSource{
		Versions: map[string][]string{
			"hashicorp/aws": {"5.30.0", "5.40.0", "6.0.0-beta1"},
		},
		Unknown: map[string]bool{
			"hashicorp/null": true,
		},
	}

	latest, err := src.LatestVersion(context.Background(), "hashicorp", "aws")
	require.NoError(t, err)
	assert.Equal(t, "5.40.0", latest) // prerelease skipped

	_, err = src.LatestVersion(context.Background(), "hashicorp", "null")
	assert.Error(t, err) // explicitly unknown

	_, err = src.LatestVersion(context.Background(), "hashicorp", "absent")
	assert.Error(t, err) // not in the map
}

// TestRegistryProviderVersionSource_RecordedFixture serves the recorded registry
// payload from a local httptest server, exercising the real HTTP source without
// any outbound network call.
func TestRegistryProviderVersionSource_RecordedFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "registry_aws_versions.json"))
	require.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/providers/hashicorp/aws/versions", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	src := NewRegistryProviderVersionSource(server.URL)
	latest, err := src.LatestVersion(context.Background(), "hashicorp", "aws")
	require.NoError(t, err)
	assert.Equal(t, "5.40.0", latest) // 6.0.0-beta1 skipped as prerelease
}

// TestRegistryProviderVersionSource_NotFound confirms a non-2xx registry
// response surfaces as an error (callers downgrade it to "unknown").
func TestRegistryProviderVersionSource_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	src := NewRegistryProviderVersionSource(server.URL)
	_, err := src.LatestVersion(context.Background(), "acme", "missing")
	assert.Error(t, err)
}

func TestRegistryProviderVersionSource_RequiresNamespaceAndType(t *testing.T) {
	src := NewRegistryProviderVersionSource("")
	_, err := src.LatestVersion(context.Background(), "", "aws")
	assert.Error(t, err)
}
