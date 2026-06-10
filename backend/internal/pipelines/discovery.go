// discovery.go lists what a CI credential can dispatch to — Azure DevOps
// pipelines, GitHub repositories and their Actions workflows — so pipeline
// connections can be built by selection instead of hand-typed coordinates.
package pipelines

import (
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

func discoveryGET(ctx context.Context, u string, authorize func(*http.Request)) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	authorize(req)
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
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

// ListAzurePipelines returns the pipeline definitions in an ADO project.
func ListAzurePipelines(ctx context.Context, pat, organization, project string) ([]PipelineRef, error) {
	if pat == "" || organization == "" || project == "" {
		return nil, fmt.Errorf("azure devops discovery requires organization, project, and a PAT")
	}
	u := fmt.Sprintf("%s/%s/%s/_apis/pipelines?api-version=7.1&$top=500",
		azureDevOpsBaseURL, url.PathEscape(organization), url.PathEscape(project))
	body, status, err := discoveryGET(ctx, u, adoAuth(pat))
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
	sort.Slice(out.Value, func(i, j int) bool { return out.Value[i].Name < out.Value[j].Name })
	return out.Value, nil
}

// ListGitHubRepos returns the repositories under a GitHub owner. The org
// endpoint is tried first; a 404 falls back to the user endpoint so personal
// accounts work with the same configuration shape.
func ListGitHubRepos(ctx context.Context, token, owner string) ([]RepoRef, error) {
	if token == "" || owner == "" {
		return nil, fmt.Errorf("github discovery requires an owner and a token")
	}
	parse := func(body []byte) ([]RepoRef, error) {
		var repos []struct {
			Name          string `json:"name"`
			DefaultBranch string `json:"default_branch"`
		}
		if err := json.Unmarshal(body, &repos); err != nil {
			return nil, fmt.Errorf("github repo list parse failed: %w", err)
		}
		out := make([]RepoRef, 0, len(repos))
		for _, r := range repos {
			out = append(out, RepoRef{Name: r.Name, DefaultBranch: r.DefaultBranch})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out, nil
	}

	body, status, err := discoveryGET(ctx,
		fmt.Sprintf("%s/orgs/%s/repos?per_page=100&sort=full_name", githubAPIBaseURL, url.PathEscape(owner)),
		githubAuth(token))
	if err != nil {
		return nil, fmt.Errorf("github repo list failed: %w", err)
	}
	if status == http.StatusNotFound {
		body, status, err = discoveryGET(ctx,
			fmt.Sprintf("%s/users/%s/repos?per_page=100&sort=full_name", githubAPIBaseURL, url.PathEscape(owner)),
			githubAuth(token))
		if err != nil {
			return nil, fmt.Errorf("github repo list failed: %w", err)
		}
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("github repo list returned %d", status)
	}
	return parse(body)
}

// ListGitHubWorkflows returns the active Actions workflows in a repository.
func ListGitHubWorkflows(ctx context.Context, token, owner, repo string) ([]WorkflowRef, error) {
	if token == "" || owner == "" || repo == "" {
		return nil, fmt.Errorf("github workflow discovery requires owner, repo, and a token")
	}
	u := fmt.Sprintf("%s/repos/%s/%s/actions/workflows?per_page=100",
		githubAPIBaseURL, url.PathEscape(owner), url.PathEscape(repo))
	body, status, err := discoveryGET(ctx, u, githubAuth(token))
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
	refs := make([]WorkflowRef, 0, len(out.Workflows))
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
