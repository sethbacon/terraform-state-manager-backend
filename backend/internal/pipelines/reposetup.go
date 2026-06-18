// reposetup.go implements the repo-setup wizard's phase-2 automation: commit
// the TSM workflow file to a new branch and open a pull request, entirely
// through the provider APIs, plus PR-state polling. Both orchestrators are
// idempotent at the file level — if the workflow already exists on the default
// branch nothing is written.
//
// SECURITY: the file path and branch prefix are fixed server-side; callers only
// supply the file CONTENT (size-capped by the HTTP handler). Write scopes
// required: ADO Code (Read & Write); GitHub contents:write + pull_requests:write.
package pipelines

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Canonical workflow file paths by kind (fixed; never client-supplied).
const (
	AzureWorkflowPath  = "/azure-pipelines-tsm-drift.yml"
	GitHubWorkflowPath = ".github/workflows/tsm-drift.yml"
)

// WorkflowPaths maps a workflow kind to its canonical path per provider.
var WorkflowPaths = map[string]struct{ Azure, GitHub string }{
	"drift":      {Azure: "/azure-pipelines-tsm-drift.yml", GitHub: ".github/workflows/tsm-drift.yml"},
	"versionlab": {Azure: "/azure-pipelines-tsm-health.yml", GitHub: ".github/workflows/tsm-health.yml"},
}

// FileSpec is one workflow file to land in the repo.
type FileSpec struct {
	Path    string
	Content string
}

// SetupResult reports what the workflow-setup orchestration did.
type SetupResult struct {
	// Status is "exists" (file already on the default branch — nothing written)
	// or "pr_created".
	Status string `json:"status"`
	Branch string `json:"branch,omitempty"`
	PRID   int    `json:"pr_id,omitempty"`
	PRURL  string `json:"pr_url,omitempty"`
}

func setupBranchName() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "tsm/drift-setup-" + hex.EncodeToString(b)
}

func doJSON(ctx context.Context, method, u string, authorize func(*http.Request), payload any) ([]byte, int, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, 0, err
	}
	authorize(req)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return out, resp.StatusCode, err
}

// --- Azure DevOps ---

func adoRepoBase(org, project, repoID string) string {
	return fmt.Sprintf("%s/%s/%s/_apis/git/repositories/%s",
		azureDevOpsBaseURL, url.PathEscape(org), url.PathEscape(project), url.PathEscape(repoID))
}

// adoDefaultBranch returns the repo's default branch ref (e.g. refs/heads/main).
func adoDefaultBranch(ctx context.Context, cred ADOToken, org, project, repoID string) (string, error) {
	body, status, _, err := discoveryGET(ctx, adoRepoBase(org, project, repoID)+"?api-version=7.1", adoAuth(cred))
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("azure devops repo lookup returned %d", status)
	}
	var out struct {
		DefaultBranch string `json:"defaultBranch"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if out.DefaultBranch == "" {
		return "", fmt.Errorf("repository has no default branch (empty repo?)")
	}
	return out.DefaultBranch, nil
}

// adoFileExists checks for the workflow file on the given branch.
func adoFileExists(ctx context.Context, cred ADOToken, org, project, repoID, path, branchRef string) (bool, error) {
	branch := strings.TrimPrefix(branchRef, "refs/heads/")
	u := fmt.Sprintf("%s/items?path=%s&versionDescriptor.version=%s&api-version=7.1",
		adoRepoBase(org, project, repoID), url.QueryEscape(path), url.QueryEscape(branch))
	_, status, _, err := discoveryGET(ctx, u, adoAuth(cred))
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("azure devops item lookup returned %d", status)
	}
}

// adoBranchTip returns the commit id at the tip of a branch ref.
func adoBranchTip(ctx context.Context, cred ADOToken, org, project, repoID, branchRef string) (string, error) {
	filter := strings.TrimPrefix(branchRef, "refs/")
	u := fmt.Sprintf("%s/refs?filter=%s&api-version=7.1", adoRepoBase(org, project, repoID), url.QueryEscape(filter))
	body, status, _, err := discoveryGET(ctx, u, adoAuth(cred))
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("azure devops ref lookup returned %d", status)
	}
	var out struct {
		Value []struct {
			Name     string `json:"name"`
			ObjectID string `json:"objectId"`
		} `json:"value"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	for _, r := range out.Value {
		if r.Name == branchRef {
			return r.ObjectID, nil
		}
	}
	return "", fmt.Errorf("branch %s not found", branchRef)
}

// SetupAzureWorkflow commits the given workflow files to a new branch off the
// default branch and opens a PR. Files already on the default branch are
// skipped; Status is "exists" when nothing needed committing.
func SetupAzureWorkflow(ctx context.Context, cred ADOToken, org, project, repoID string, files []FileSpec) (*SetupResult, error) {
	defaultRef, err := adoDefaultBranch(ctx, cred, org, project, repoID)
	if err != nil {
		return nil, fmt.Errorf("default branch: %w", err)
	}
	var missing []FileSpec
	for _, f := range files {
		exists, err := adoFileExists(ctx, cred, org, project, repoID, f.Path, defaultRef)
		if err != nil {
			return nil, fmt.Errorf("file check: %w", err)
		}
		if !exists {
			missing = append(missing, f)
		}
	}
	if len(missing) == 0 {
		return &SetupResult{Status: "exists"}, nil
	}
	tip, err := adoBranchTip(ctx, cred, org, project, repoID, defaultRef)
	if err != nil {
		return nil, fmt.Errorf("branch tip: %w", err)
	}

	branch := setupBranchName()
	changes := make([]map[string]any, 0, len(missing))
	for _, f := range missing {
		changes = append(changes, map[string]any{
			"changeType": "add",
			"item":       map[string]any{"path": f.Path},
			"newContent": map[string]any{"content": f.Content, "contentType": "rawtext"},
		})
	}
	// A push whose refUpdate names a new branch with oldObjectId = the base
	// branch tip creates the branch from that commit with these changes applied.
	push := map[string]any{
		"refUpdates": []map[string]any{{"name": "refs/heads/" + branch, "oldObjectId": tip}},
		"commits": []map[string]any{{
			"comment": "Add TSM CI workflows",
			"changes": changes,
		}},
	}
	body, status, err := doJSON(ctx, http.MethodPost, adoRepoBase(org, project, repoID)+"/pushes?api-version=7.1", adoAuth(cred), push)
	if err != nil {
		return nil, fmt.Errorf("push: %w", err)
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return nil, fmt.Errorf("azure devops push returned %d: %s", status, truncate(body, 300))
	}

	pr := map[string]any{
		"sourceRefName": "refs/heads/" + branch,
		"targetRefName": defaultRef,
		"title":         "Add TSM CI workflows",
		"description":   "Adds TSM pipeline workflow definitions (generated by the Terraform State Manager repo-setup wizard).",
	}
	body, status, err = doJSON(ctx, http.MethodPost, adoRepoBase(org, project, repoID)+"/pullrequests?api-version=7.1", adoAuth(cred), pr)
	if err != nil {
		return nil, fmt.Errorf("pull request: %w", err)
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return nil, fmt.Errorf("azure devops pull request returned %d: %s", status, truncate(body, 300))
	}
	var created struct {
		PullRequestID int `json:"pullRequestId"`
		Repository    struct {
			WebURL string `json:"webUrl"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		return nil, fmt.Errorf("pull request parse: %w", err)
	}
	prURL := ""
	if created.Repository.WebURL != "" {
		prURL = fmt.Sprintf("%s/pullrequest/%d", created.Repository.WebURL, created.PullRequestID)
	}
	return &SetupResult{Status: "pr_created", Branch: branch, PRID: created.PullRequestID, PRURL: prURL}, nil
}

// AzurePRState returns the normalized state of a PR: open | merged | closed.
func AzurePRState(ctx context.Context, cred ADOToken, org, project, repoID string, prID int) (string, error) {
	u := fmt.Sprintf("%s/pullrequests/%d?api-version=7.1", adoRepoBase(org, project, repoID), prID)
	body, status, _, err := discoveryGET(ctx, u, adoAuth(cred))
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("azure devops pull request lookup returned %d", status)
	}
	var out struct {
		Status string `json:"status"` // active | completed | abandoned
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	switch out.Status {
	case "completed":
		return "merged", nil
	case "abandoned":
		return "closed", nil
	default:
		return "open", nil
	}
}

// --- GitHub ---

func ghRepoBase(owner, repo string) string {
	return fmt.Sprintf("%s/repos/%s/%s", githubAPIBaseURL, url.PathEscape(owner), url.PathEscape(repo))
}

func ghDefaultBranch(ctx context.Context, token, owner, repo string) (string, error) {
	body, status, _, err := discoveryGET(ctx, ghRepoBase(owner, repo), githubAuth(token))
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("github repo lookup returned %d", status)
	}
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if out.DefaultBranch == "" {
		return "", fmt.Errorf("repository has no default branch")
	}
	return out.DefaultBranch, nil
}

func ghFileExists(ctx context.Context, token, owner, repo, path, ref string) (bool, error) {
	u := fmt.Sprintf("%s/contents/%s?ref=%s", ghRepoBase(owner, repo), path, url.QueryEscape(ref))
	_, status, _, err := discoveryGET(ctx, u, githubAuth(token))
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("github content lookup returned %d", status)
	}
}

// SetupGitHubWorkflow commits the given workflow files to a new branch off the
// default branch and opens a PR. Files already on the default branch are
// skipped; Status is "exists" when nothing needed committing.
func SetupGitHubWorkflow(ctx context.Context, token, owner, repo string, files []FileSpec) (*SetupResult, error) {
	base, err := ghDefaultBranch(ctx, token, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("default branch: %w", err)
	}
	var missing []FileSpec
	for _, f := range files {
		exists, err := ghFileExists(ctx, token, owner, repo, f.Path, base)
		if err != nil {
			return nil, fmt.Errorf("file check: %w", err)
		}
		if !exists {
			missing = append(missing, f)
		}
	}
	if len(missing) == 0 {
		return &SetupResult{Status: "exists"}, nil
	}

	// Tip of the base branch → create the setup branch from it.
	body, status, _, err := discoveryGET(ctx,
		fmt.Sprintf("%s/git/ref/%s", ghRepoBase(owner, repo), url.PathEscape("heads/"+base)), githubAuth(token))
	if err != nil {
		return nil, fmt.Errorf("base ref: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("github base ref lookup returned %d", status)
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(body, &ref); err != nil {
		return nil, err
	}

	branch := setupBranchName()
	body, status, err = doJSON(ctx, http.MethodPost, ghRepoBase(owner, repo)+"/git/refs", githubAuth(token),
		map[string]any{"ref": "refs/heads/" + branch, "sha": ref.Object.SHA})
	if err != nil {
		return nil, fmt.Errorf("create branch: %w", err)
	}
	if status != http.StatusCreated {
		return nil, fmt.Errorf("github branch creation returned %d: %s", status, truncate(body, 300))
	}

	for _, f := range missing {
		body, status, err = doJSON(ctx, http.MethodPut, ghRepoBase(owner, repo)+"/contents/"+f.Path, githubAuth(token),
			map[string]any{
				"message": "Add TSM CI workflow " + f.Path,
				"content": base64.StdEncoding.EncodeToString([]byte(f.Content)),
				"branch":  branch,
			})
		if err != nil {
			return nil, fmt.Errorf("commit file: %w", err)
		}
		if status != http.StatusCreated && status != http.StatusOK {
			return nil, fmt.Errorf("github file commit returned %d: %s", status, truncate(body, 300))
		}
	}

	body, status, err = doJSON(ctx, http.MethodPost, ghRepoBase(owner, repo)+"/pulls", githubAuth(token),
		map[string]any{
			"title": "Add TSM CI workflows",
			"head":  branch,
			"base":  base,
			"body":  "Adds TSM CI workflows (generated by the Terraform State Manager repo-setup wizard).",
		})
	if err != nil {
		return nil, fmt.Errorf("pull request: %w", err)
	}
	if status != http.StatusCreated {
		return nil, fmt.Errorf("github pull request returned %d: %s", status, truncate(body, 300))
	}
	var pr struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, err
	}
	return &SetupResult{Status: "pr_created", Branch: branch, PRID: pr.Number, PRURL: pr.HTMLURL}, nil
}

// GitHubPRState returns the normalized state of a PR: open | merged | closed.
func GitHubPRState(ctx context.Context, token, owner, repo string, number int) (string, error) {
	body, status, _, err := discoveryGET(ctx,
		fmt.Sprintf("%s/pulls/%d", ghRepoBase(owner, repo), number), githubAuth(token))
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("github pull request lookup returned %d", status)
	}
	var out struct {
		State  string `json:"state"` // open | closed
		Merged bool   `json:"merged"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	switch {
	case out.Merged:
		return "merged", nil
	case out.State == "closed":
		return "closed", nil
	default:
		return "open", nil
	}
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}
