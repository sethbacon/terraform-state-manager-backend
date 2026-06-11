package statesource

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// ---------------------------------------------------------------------------
// HCP Terraform / TFE (httptest API double; the struct takes any base URL)
// ---------------------------------------------------------------------------

func newHCPOver(srv *httptest.Server) *hcp {
	return &hcp{client: srv.Client(), baseURL: srv.URL, org: "acme", token: "tfe-token"}
}

func TestHCP_NewValidation(t *testing.T) {
	if _, err := newHCP(map[string]any{}, map[string]any{"token": "t"}); err == nil {
		t.Error("missing organization must error")
	}
	if _, err := newHCP(map[string]any{"organization": "acme"}, map[string]any{}); err == nil {
		t.Error("missing token must error")
	}
	h, err := newHCP(map[string]any{"organization": "acme"}, map[string]any{"token": "t"})
	if err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if h.baseURL != "https://app.terraform.io" {
		t.Errorf("default hostname wrong: %s", h.baseURL)
	}
	h, _ = newHCP(map[string]any{"organization": "acme", "hostname": "tfe.corp.example"}, map[string]any{"token": "t"})
	if h.baseURL != "https://tfe.corp.example" {
		t.Errorf("custom hostname wrong: %s", h.baseURL)
	}
}

func TestHCP_ListPaginatesAndSorts(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tfe-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.Contains(r.URL.RawQuery, "page%5Bnumber%5D=2") || strings.Contains(r.URL.RawQuery, "page[number]=2"):
			// ws-aaa has state; ws-never was created but never applied (null
			// current-state-version) and must be filtered from the listing.
			fmt.Fprint(w, `{"data":[`+
				`{"id":"ws-aaa","attributes":{"name":"alpha","updated-at":"2026-06-01T00:00:00Z"},"relationships":{"current-state-version":{"data":{"id":"sv-1","type":"state-versions"}}}},`+
				`{"id":"ws-never","attributes":{"name":"never-applied","updated-at":"2026-06-03T00:00:00Z"},"relationships":{"current-state-version":{"data":null}}}`+
				`],"links":{"next":""}}`)
		default:
			fmt.Fprintf(w, `{"data":[{"id":"ws-zzz","attributes":{"name":"zulu","updated-at":"2026-06-02T00:00:00Z"}}],"links":{"next":"%s/api/v2/organizations/acme/workspaces?page[number]=2"}}`, srv.URL)
		}
	}))
	defer srv.Close()

	refs, err := newHCPOver(srv).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// ws-never (no current state version) is excluded; ws-zzz carries no
	// relationships at all (older TFE payload) and is kept.
	if len(refs) != 2 {
		t.Fatalf("expected both pages minus the never-applied workspace, got %d refs: %+v", len(refs), refs)
	}
	// Sorted by workspace NAME (friendly), not ID.
	if refs[0].Name != "alpha" || refs[1].Name != "zulu" {
		t.Errorf("not sorted by name: %+v", refs)
	}
	if refs[0].Key != "ws-aaa" || refs[0].LastModified == nil {
		t.Errorf("ref mapping wrong: %+v", refs[0])
	}
}

func TestHCP_ReadDownloadsCurrentStateVersion(t *testing.T) {
	state := []byte(`{"version":4,"serial":7}`)
	var gzState bytes.Buffer
	gz := gzip.NewWriter(&gzState)
	_, _ = gz.Write(state)
	_ = gz.Close()

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v2/workspaces/ws-gz/"):
			fmt.Fprintf(w, `{"data":{"attributes":{"hosted-state-download-url":"%s/download/gz","serial":7}}}`, srv.URL)
		case strings.HasPrefix(r.URL.Path, "/api/v2/workspaces/ws-plain/"):
			fmt.Fprintf(w, `{"data":{"attributes":{"hosted-state-download-url":"%s/download/plain","serial":7}}}`, srv.URL)
		case strings.HasPrefix(r.URL.Path, "/api/v2/workspaces/ws-empty/"):
			fmt.Fprint(w, `{"data":{"attributes":{"hosted-state-download-url":"","serial":0}}}`)
		case r.URL.Path == "/download/gz":
			_, _ = w.Write(gzState.Bytes())
		case r.URL.Path == "/download/plain":
			_, _ = w.Write(state)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	h := newHCPOver(srv)

	// Gzip-compressed state is transparently decompressed.
	rs, err := h.Read(context.Background(), "ws-gz")
	if err != nil || !bytes.Equal(rs.Data, state) {
		t.Fatalf("gzip read: %v (%s)", err, rs.Data)
	}
	// Plain state passes through.
	rs, err = h.Read(context.Background(), "ws-plain")
	if err != nil || !bytes.Equal(rs.Data, state) {
		t.Fatalf("plain read: %v", err)
	}
	// A workspace with no state version is a clear error.
	if _, err := h.Read(context.Background(), "ws-empty"); err == nil || !strings.Contains(err.Error(), "no current state version") {
		t.Errorf("empty workspace: %v", err)
	}
	// Missing workspace surfaces the API status.
	if _, err := h.Read(context.Background(), "ws-missing"); err == nil {
		t.Error("missing workspace must error")
	}
}

func TestHCP_WriteUnsupported(t *testing.T) {
	h := &hcp{}
	if err := h.Write(context.Background(), "ws-x", nil); err == nil {
		t.Error("HCP writes must be rejected")
	}
}

func TestHCP_APIErrorsSurfaceStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"errors":[{"status":"401"}]}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	if _, err := newHCPOver(srv).List(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("expected status in error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Git (real go-git repo on disk; cloned in-memory by the connector)
// ---------------------------------------------------------------------------

func seedGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	files := map[string]string{
		"prod/network.tfstate": `{"version":4,"serial":3}`,
		"prod/app.tfstate":     `{"version":4,"serial":9}`,
		"README.md":            "not state",
	}
	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	wt, _ := repo.Worktree()
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := wt.Commit("seed", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	return dir
}

func TestGit_NewValidation(t *testing.T) {
	if _, err := newGit(map[string]any{}, nil); err == nil {
		t.Error("missing repo_url must error")
	}
	g, err := newGit(map[string]any{"repo_url": "https://example.com/r.git"}, map[string]any{"token": "tok"})
	if err != nil {
		t.Fatalf("valid: %v", err)
	}
	if g.username != "git" {
		t.Errorf("default username = %q, want git", g.username)
	}
}

func TestGit_ListAndRead(t *testing.T) {
	dir := seedGitRepo(t)

	g := &gitConn{repoURL: dir}
	refs, err := g.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 .tfstate files (README excluded), got %+v", refs)
	}

	// Prefix narrows the listing.
	g = &gitConn{repoURL: dir, prefix: "prod/network"}
	refs, err = g.List(context.Background())
	if err != nil || len(refs) != 1 || refs[0].Key != "prod/network.tfstate" {
		t.Fatalf("prefixed list: %v %+v", err, refs)
	}

	rs, err := (&gitConn{repoURL: dir}).Read(context.Background(), "prod/app.tfstate")
	if err != nil || !strings.Contains(string(rs.Data), `"serial":9`) {
		t.Fatalf("Read: %v (%s)", err, rs.Data)
	}

	if _, err := (&gitConn{repoURL: dir}).Read(context.Background(), "missing.tfstate"); err == nil {
		t.Error("missing key must error")
	}
}

func TestGit_CloneFailureAndWrite(t *testing.T) {
	g := &gitConn{repoURL: filepath.Join(t.TempDir(), "nope")}
	if _, err := g.List(context.Background()); err == nil || !strings.Contains(err.Error(), "clone failed") {
		t.Errorf("clone failure: %v", err)
	}
	if err := g.Write(context.Background(), "k", nil); err == nil {
		t.Error("git writes must be rejected")
	}
}

// ---------------------------------------------------------------------------
// S3 (httptest fake speaking minimal ListObjectsV2/Get/Put; config.endpoint)
// ---------------------------------------------------------------------------

func newS3Fake(t *testing.T) (*s3conn, *map[string][]byte) {
	t.Helper()
	objects := map[string][]byte{
		"envs/prod.tfstate": []byte(`{"version":4,"serial":2}`),
		"envs/notes.txt":    []byte("skip me"),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.RawQuery != "" && strings.Contains(r.URL.RawQuery, "list-type=2"):
			w.Header().Set("Content-Type", "application/xml")
			var b strings.Builder
			b.WriteString(`<?xml version="1.0"?><ListBucketResult><IsTruncated>false</IsTruncated>`)
			for k, v := range objects {
				fmt.Fprintf(&b, "<Contents><Key>%s</Key><Size>%d</Size><LastModified>2026-06-01T00:00:00Z</LastModified></Contents>", k, len(v))
			}
			b.WriteString(`</ListBucketResult>`)
			_, _ = w.Write([]byte(b.String()))
		case r.Method == http.MethodGet:
			key := strings.TrimPrefix(r.URL.Path, "/state-bucket/")
			if data, ok := objects[key]; ok {
				_, _ = w.Write(data)
				return
			}
			http.Error(w, "<Error><Code>NoSuchKey</Code></Error>", http.StatusNotFound)
		case r.Method == http.MethodPut:
			key := strings.TrimPrefix(r.URL.Path, "/state-bucket/")
			body := new(bytes.Buffer)
			_, _ = body.ReadFrom(r.Body)
			objects[key] = body.Bytes()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(srv.Close)

	conn, err := newS3(
		map[string]any{"bucket": "state-bucket", "endpoint": srv.URL, "region": "us-east-1"},
		map[string]any{"access_key_id": "AKIA-test", "secret_access_key": "secret"},
	)
	if err != nil {
		t.Fatalf("newS3: %v", err)
	}
	return conn, &objects
}

func TestS3_NewValidation(t *testing.T) {
	if _, err := newS3(map[string]any{}, nil); err == nil {
		t.Error("missing bucket must error")
	}
}

func TestS3_ListReadWrite(t *testing.T) {
	conn, objects := newS3Fake(t)
	ctx := context.Background()

	refs, err := conn.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 || refs[0].Key != "envs/prod.tfstate" {
		t.Fatalf("only .tfstate keys should list: %+v", refs)
	}
	if refs[0].Size == 0 || refs[0].LastModified == nil {
		t.Errorf("metadata not mapped: %+v", refs[0])
	}

	rs, err := conn.Read(ctx, "envs/prod.tfstate")
	if err != nil || !strings.Contains(string(rs.Data), `"serial":2`) {
		t.Fatalf("Read: %v", err)
	}
	if _, err := conn.Read(ctx, "missing.tfstate"); err == nil {
		t.Error("missing key must error")
	}

	if err := conn.Write(ctx, "envs/new.tfstate", []byte(`{"version":4}`)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if string((*objects)["envs/new.tfstate"]) != `{"version":4}` {
		t.Error("Write did not store the object")
	}
}

// ---------------------------------------------------------------------------
// Azure / GCS / k8s / pg constructors (validation only — clients need clouds)
// ---------------------------------------------------------------------------

func TestCloudConstructorValidation(t *testing.T) {
	if _, err := newAzure(map[string]any{"container": "c"}, nil); err == nil {
		t.Error("azure: missing account must error")
	}
	if _, err := newAzure(map[string]any{"account": "a"}, nil); err == nil {
		t.Error("azure: missing container must error")
	}
	if _, err := newGCS(map[string]any{}, nil); err == nil {
		t.Error("gcs: missing bucket must error")
	}
	if _, err := newPG(map[string]any{}, nil); err == nil {
		t.Error("pg: missing connection config must error")
	}
}
