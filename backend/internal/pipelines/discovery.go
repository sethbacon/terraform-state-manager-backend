// discovery.go lists what a CI credential can dispatch to — Azure DevOps
// pipelines, GitHub repositories and their Actions workflows — so pipeline
// connections can be built by selection instead of hand-typed coordinates.
package pipelines

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
)

// Base URLs are package vars so tests can point them at an httptest server.
var (
	azureDevOpsBaseURL = "https://dev.azure.com"
	githubAPIBaseURL   = "https://api.github.com"
)

// PipelineRef is one dispatchable Azure DevOps pipeline definition.
type PipelineRef struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Folder string `json:"folder,omitempty"`
}

// RepoRef is one GitHub repository visible to the credential.
type RepoRef struct {
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch,omitempty"`
}

// WorkflowRef is one GitHub Actions workflow in a repository. File is the
// workflow file name (the form DispatchGitHub accepts as workflow_id).
type WorkflowRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	File string `json:"file"`
}

// maxDiscoveryPages bounds pagination loops so a huge (or misbehaving) provider
// can't hold a request open indefinitely: 20 pages = 10k ADO pipelines / 2k
// GitHub repos or workflows.
const maxDiscoveryPages = 20

func discoveryGET(ctx context.Context, u string, authorize func(*http.Request)) ([]byte, int, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, nil, err
	}
	authorize(req)
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, resp.StatusCode, resp.Header, err
	}
	return body, resp.StatusCode, resp.Header, nil
}

func adoAuth(pat string) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(":"+pat)))
	}
}

func githubAuth(token string) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
}

// ListAzurePipelines returns the pipeline definitions in an ADO project,
// following the continuation-token pagination so large projects list fully.
func ListAzurePipelines(ctx context.Context, pat, organization, project string) ([]PipelineRef, error) {
	if pat == "" || organization == "" || project == "" {
		return nil, fmt.Errorf("azure devops discovery requires organization, project, and a PAT")
	}
	base := fmt.Sprintf("%s/%s/%s/_apis/pipelines?api-version=7.1&$top=500",
		azureDevOpsBaseURL, url.PathEscape(organization), url.PathEscape(project))
	var refs []PipelineRef
	continuation := ""
	for page := 0; page < maxDiscoveryPages; page++ {
		u := base
		if continuation != "" {
			u += "&continuationToken=" + url.QueryEscape(continuation)
		}
		body, status, header, err := discoveryGET(ctx, u, adoAuth(pat))
		if err != nil {
			return nil, fmt.Errorf("azure devops pipeline list failed: %w", err)
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("azure devops pipeline list returned %d", status)
		}
		var out struct {
			Value []PipelineRef `json:"value"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, fmt.Errorf("azure devops pipeline list parse failed: %w", err)
		}
		refs = append(refs, out.Value...)
		continuation = header.Get("X-MS-ContinuationToken")
		if continuation == "" {
			break
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs, nil
}

// listGitHubReposFrom pages through a repos endpoint (org or user form) until a
// short page or the page cap. A 404 on the FIRST page is reported as notFound so
// the caller can fall back to the other endpoint form.
func listGitHubReposFrom(ctx context.Context, token, base string) (refs []RepoRef, notFound bool, err error) {
	const perPage = 100
	for page := 1; page <= maxDiscoveryPages; page++ {
		u := fmt.Sprintf("%s&per_page=%d&page=%d", base, perPage, page)
		body, status, _, gerr := discoveryGET(ctx, u, githubAuth(token))
		if gerr != nil {
			return nil, false, fmt.Errorf("github repo list failed: %w", gerr)
		}
		if status == http.StatusNotFound && page == 1 {
			return nil, true, nil
		}
		if status != http.StatusOK {
			return nil, false, fmt.Errorf("github repo list returned %d", status)
		}
		var repos []struct {
			Name          string `json:"name"`
			DefaultBranch string `json:"default_branch"`
		}
		if uerr := json.Unmarshal(body, &repos); uerr != nil {
			return nil, false, fmt.Errorf("github repo list parse failed: %w", uerr)
		}
		for _, r := range repos {
			refs = append(refs, RepoRef{Name: r.Name, DefaultBranch: r.DefaultBranch})
		}
		if len(repos) < perPage {
			break
		}
	}
	return refs, false, nil
}

// ListGitHubRepos returns the repositories under a GitHub owner (all pages).
// The org endpoint is tried first; a 404 falls back to the user endpoint so
// personal accounts work with the same configuration shape.
func ListGitHubRepos(ctx context.Context, token, owner string) ([]RepoRef, error) {
	if token == "" || owner == "" {
		return nil, fmt.Errorf("github discovery requires an owner and a token")
	}
	refs, notFound, err := listGitHubReposFrom(ctx, token,
		fmt.Sprintf("%s/orgs/%s/repos?sort=full_name", githubAPIBaseURL, url.PathEscape(owner)))
	if err != nil {
		return nil, err
	}
	if notFound {
		refs, _, err = listGitHubReposFrom(ctx, token,
			fmt.Sprintf("%s/users/%s/repos?sort=full_name", githubAPIBaseURL, url.PathEscape(owner)))
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs, nil
}

// ListGitHubWorkflows returns the active Actions workflows in a repository
// (all pages).
func ListGitHubWorkflows(ctx context.Context, token, owner, repo string) ([]WorkflowRef, error) {
	if token == "" || owner == "" || repo == "" {
		return nil, fmt.Errorf("github workflow discovery requires owner, repo, and a token")
	}
	const perPage = 100
	var refs []WorkflowRef
	for page := 1; page <= maxDiscoveryPages; page++ {
		u := fmt.Sprintf("%s/repos/%s/%s/actions/workflows?per_page=%d&page=%d",
			githubAPIBaseURL, url.PathEscape(owner), url.PathEscape(repo), perPage, page)
		body, status, _, err := discoveryGET(ctx, u, githubAuth(token))
		if err != nil {
			return nil, fmt.Errorf("github workflow list failed: %w", err)
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("github workflow list returned %d", status)
		}
		var out struct {
			Workflows []struct {
				ID    int64  `json:"id"`
				Name  string `json:"name"`
				Path  string `json:"path"`
				State string `json:"state"`
			} `json:"workflows"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, fmt.Errorf("github workflow list parse failed: %w", err)
		}
		for _, w := range out.Workflows {
			if w.State != "active" {
				continue
			}
			// The dispatch API accepts the workflow file name; strip the
			// ".github/workflows/" prefix from path.
			file := w.Path
			if idx := lastSlash(file); idx >= 0 {
				file = file[idx+1:]
			}
			refs = append(refs, WorkflowRef{ID: w.ID, Name: w.Name, File: file})
		}
		if len(out.Workflows) < perPage {
			break
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs, nil
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

// AzureRepoRef is one Git repository in an ADO project. ID is required by the
// pipeline-creation API.
type AzureRepoRef struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch,omitempty"`
}

// ServiceConnectionRef is one ADO service connection (cloud credential) the
// generated pipeline can reference for terraform auth.
type ServiceConnectionRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
}

// ListAzureRepos returns the Git repositories in an ADO project.
func ListAzureRepos(ctx context.Context, pat, organization, project string) ([]AzureRepoRef, error) {
	if pat == "" || organization == "" || project == "" {
		return nil, fmt.Errorf("azure devops discovery requires organization, project, and a PAT")
	}
	u := fmt.Sprintf("%s/%s/%s/_apis/git/repositories?api-version=7.1",
		azureDevOpsBaseURL, url.PathEscape(organization), url.PathEscape(project))
	body, status, _, err := discoveryGET(ctx, u, adoAuth(pat))
	if err != nil {
		return nil, fmt.Errorf("azure devops repo list failed: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("azure devops repo list returned %d", status)
	}
	var out struct {
		Value []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			DefaultBranch string `json:"defaultBranch"`
		} `json:"value"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("azure devops repo list parse failed: %w", err)
	}
	refs := make([]AzureRepoRef, 0, len(out.Value))
	for _, r := range out.Value {
		refs = append(refs, AzureRepoRef{ID: r.ID, Name: r.Name, DefaultBranch: r.DefaultBranch})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs, nil
}

// ListAzureServiceConnections returns the project's service connections so the
// wizard can name one in the generated pipeline's credential guidance. Requires
// the PAT to carry Service Connections (read); callers degrade gracefully on 403.
func ListAzureServiceConnections(ctx context.Context, pat, organization, project string) ([]ServiceConnectionRef, error) {
	if pat == "" || organization == "" || project == "" {
		return nil, fmt.Errorf("azure devops discovery requires organization, project, and a PAT")
	}
	u := fmt.Sprintf("%s/%s/%s/_apis/serviceendpoint/endpoints?api-version=7.1-preview.4",
		azureDevOpsBaseURL, url.PathEscape(organization), url.PathEscape(project))
	body, status, _, err := discoveryGET(ctx, u, adoAuth(pat))
	if err != nil {
		return nil, fmt.Errorf("azure devops service connection list failed: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("azure devops service connection list returned %d", status)
	}
	var out struct {
		Value []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"value"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("azure devops service connection list parse failed: %w", err)
	}
	refs := make([]ServiceConnectionRef, 0, len(out.Value))
	for _, sc := range out.Value {
		refs = append(refs, ServiceConnectionRef{ID: sc.ID, Name: sc.Name, Type: sc.Type})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs, nil
}

// CreateAzurePipeline creates a YAML pipeline definition pointing at yamlPath in
// the given repository (the wizard's "create the pipeline for me" step). The PAT
// must carry Build (read & execute) with pipeline-creation rights.
func CreateAzurePipeline(ctx context.Context, pat, organization, project, name, yamlPath, repoID string) (*PipelineRef, error) {
	if pat == "" || organization == "" || project == "" {
		return nil, fmt.Errorf("azure devops pipeline creation requires organization, project, and a PAT")
	}
	if name == "" || yamlPath == "" || repoID == "" {
		return nil, fmt.Errorf("azure devops pipeline creation requires name, yaml_path, and repository id")
	}
	payload, _ := json.Marshal(map[string]any{
		"name": name,
		"configuration": map[string]any{
			"type": "yaml",
			"path": yamlPath,
			"repository": map[string]any{
				"id":   repoID,
				"type": "azureReposGit",
			},
		},
	})
	u := fmt.Sprintf("%s/%s/%s/_apis/pipelines?api-version=7.1",
		azureDevOpsBaseURL, url.PathEscape(organization), url.PathEscape(project))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	adoAuth(pat)(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("azure devops pipeline creation failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("azure devops pipeline creation returned %d: %s", resp.StatusCode, string(body))
	}
	var ref PipelineRef
	if err := json.Unmarshal(body, &ref); err != nil {
		return nil, fmt.Errorf("azure devops pipeline creation parse failed: %w", err)
	}
	return &ref, nil
}
