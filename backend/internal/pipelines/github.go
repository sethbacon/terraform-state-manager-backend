// Package pipelines dispatches CI runs (drift / version-health plans) to external
// providers. The app is a control plane: it triggers runs and ingests results
// via callback — no terraform binary or cloud credentials live here. GitHub
// Actions is implemented first; Azure DevOps follows the same shape.
package pipelines

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// GitHubConfig is the non-secret configuration for a GitHub Actions connection.
type GitHubConfig struct {
	Owner      string
	Repo       string
	WorkflowID string // workflow file name (e.g. "tsm-drift.yml") or numeric id
	Ref        string // default git ref to run the workflow from
}

// GitHubConfigFromMap reads a GitHubConfig from a connection's config map.
func GitHubConfigFromMap(m map[string]any) GitHubConfig {
	s := func(k string) string {
		v, _ := m[k].(string)
		return v
	}
	return GitHubConfig{Owner: s("owner"), Repo: s("repo"), WorkflowID: s("workflow_id"), Ref: s("ref")}
}

// DriftInputs are the workflow_dispatch inputs handed to the drift workflow.
type DriftInputs struct {
	CallbackURL   string
	CallbackToken string
	WorkingDir    string
}

var httpClient = &http.Client{Timeout: 20 * time.Second}

// DispatchGitHub triggers a workflow via the workflow_dispatch API, passing the
// given inputs. GitHub returns 204 with no run id; runs report via the callback.
func DispatchGitHub(ctx context.Context, token string, cfg GitHubConfig, ref string, inputs map[string]string) error {
	if cfg.Owner == "" || cfg.Repo == "" || cfg.WorkflowID == "" {
		return fmt.Errorf("github connection requires config.owner, config.repo, and config.workflow_id")
	}
	if token == "" {
		return fmt.Errorf("github connection requires an API token")
	}
	if ref == "" {
		ref = cfg.Ref
	}
	if ref == "" {
		ref = "main"
	}

	body, _ := json.Marshal(map[string]any{"ref": ref, "inputs": inputs})
	u := fmt.Sprintf("%s/repos/%s/%s/actions/workflows/%s/dispatches",
		githubAPIBaseURL, cfg.Owner, cfg.Repo, cfg.WorkflowID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("github workflow dispatch returned %d: %s", resp.StatusCode, string(msg))
	}
	return nil
}

// DispatchGitHubDrift dispatches the drift workflow with its standard inputs.
func DispatchGitHubDrift(ctx context.Context, token string, cfg GitHubConfig, ref string, inputs DriftInputs) error {
	return DispatchGitHub(ctx, token, cfg, ref, map[string]string{
		"callback_url":   inputs.CallbackURL,
		"callback_token": inputs.CallbackToken,
		"working_dir":    inputs.WorkingDir,
	})
}
