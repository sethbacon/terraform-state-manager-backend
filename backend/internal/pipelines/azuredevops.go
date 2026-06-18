package pipelines

import (
	"bytes"
	"context"
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
// REST API, passing the given template parameters. Auth is either a PAT (Basic)
// or an Entra app access token (Bearer), per the ADOToken. The run reports back
// through the callback.
func DispatchAzureDevOps(ctx context.Context, cred ADOToken, cfg AzureDevOpsConfig, ref string, params map[string]string) error {
	if cfg.Organization == "" || cfg.Project == "" || cfg.PipelineID == "" {
		return fmt.Errorf("azure devops connection requires config.organization, config.project, and config.pipeline_id")
	}
	if cred.empty() {
		return fmt.Errorf("azure devops connection requires a credential (personal access token or Entra app registration)")
	}
	if ref == "" {
		ref = cfg.Ref
	}
	// No ref configured anywhere: omit the override entirely so the run uses the
	// branch configured on the pipeline definition. Guessing one (e.g. main)
	// fails validation on repos whose default branch is named differently.
	payload := map[string]any{"templateParameters": params}
	if ref != "" {
		if !strings.HasPrefix(ref, "refs/") {
			ref = "refs/heads/" + ref
		}
		payload["resources"] = map[string]any{
			"repositories": map[string]any{
				"self": map[string]any{"refName": ref},
			},
		}
	}
	body, _ := json.Marshal(payload)
	u := fmt.Sprintf("%s/%s/%s/_apis/pipelines/%s/runs?api-version=7.1",
		azureDevOpsBaseURL, cfg.Organization, cfg.Project, cfg.PipelineID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	// PAT -> Basic base64(":"+pat); Entra app token -> Bearer.
	cred.authorize(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		// ADO rejects templateParameters the pipeline's YAML does not declare —
		// the classic symptom of pointing a connection at a regular CI pipeline
		// instead of one created from the TSM workflow template.
		hint := ""
		if strings.Contains(string(msg), "Unexpected parameter") {
			hint = " — the target pipeline does not declare the TSM parameters; create a pipeline from the in-app workflow template (Drift → Workflow template → Azure DevOps) and point the connection at that"
		}
		return fmt.Errorf("azure devops pipeline run returned %d: %s%s", resp.StatusCode, string(msg), hint)
	}
	return nil
}

// DispatchAzureDevOpsDrift dispatches a pipeline with the standard drift parameters.
func DispatchAzureDevOpsDrift(ctx context.Context, cred ADOToken, cfg AzureDevOpsConfig, ref string, inputs DriftInputs) error {
	return DispatchAzureDevOps(ctx, cred, cfg, ref, map[string]string{
		"callback_url":   inputs.CallbackURL,
		"callback_token": inputs.CallbackToken,
		"working_dir":    inputs.WorkingDir,
	})
}
