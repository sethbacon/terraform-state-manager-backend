package pipelines

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// AzureDevOpsConfig is the non-secret configuration for an Azure DevOps Pipelines
// connection.
type AzureDevOpsConfig struct {
	Organization string
	Project      string
	PipelineID   string // numeric pipeline definition id
	Ref          string // default branch ref (e.g. "refs/heads/main" or "main")
}

// AzureDevOpsConfigFromMap reads an AzureDevOpsConfig from a connection's config map.
func AzureDevOpsConfigFromMap(m map[string]any) AzureDevOpsConfig {
	s := func(k string) string {
		v, _ := m[k].(string)
		return v
	}
	return AzureDevOpsConfig{Organization: s("organization"), Project: s("project"), PipelineID: s("pipeline_id"), Ref: s("ref")}
}

// DispatchAzureDevOps triggers an Azure DevOps pipeline run via the Pipelines
// REST API, passing the given template parameters. Auth is a PAT (Basic). The run
// reports back through the callback.
func DispatchAzureDevOps(ctx context.Context, pat string, cfg AzureDevOpsConfig, ref string, params map[string]string) error {
	if cfg.Organization == "" || cfg.Project == "" || cfg.PipelineID == "" {
		return fmt.Errorf("azure devops connection requires config.organization, config.project, and config.pipeline_id")
	}
	if pat == "" {
		return fmt.Errorf("azure devops connection requires a personal access token")
	}
	if ref == "" {
		ref = cfg.Ref
	}
	if ref == "" {
		ref = "refs/heads/main"
	}
	if !strings.HasPrefix(ref, "refs/") {
		ref = "refs/heads/" + ref
	}

	body, _ := json.Marshal(map[string]any{
		"resources": map[string]any{
			"repositories": map[string]any{
				"self": map[string]any{"refName": ref},
			},
		},
		"templateParameters": params,
	})
	u := fmt.Sprintf("https://dev.azure.com/%s/%s/_apis/pipelines/%s/runs?api-version=7.1",
		cfg.Organization, cfg.Project, cfg.PipelineID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	// PAT auth: Basic base64(":" + pat).
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(":"+pat)))
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("azure devops pipeline run returned %d: %s", resp.StatusCode, string(msg))
	}
	return nil
}

// DispatchAzureDevOpsDrift dispatches a pipeline with the standard drift parameters.
func DispatchAzureDevOpsDrift(ctx context.Context, pat string, cfg AzureDevOpsConfig, ref string, inputs DriftInputs) error {
	return DispatchAzureDevOps(ctx, pat, cfg, ref, map[string]string{
		"callback_url":   inputs.CallbackURL,
		"callback_token": inputs.CallbackToken,
		"working_dir":    inputs.WorkingDir,
	})
}
