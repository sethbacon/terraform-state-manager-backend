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

// ADORunVariable is one entry of the Azure DevOps Runs API "variables" bag: a
// value plus whether the run should register it as a secret (masked in logs,
// never shown in the Parameters view). Used only for the fan-out path's
// per-target callback tokens (drift-fleet-scale.md Phase 1b item 3 / spike
// 1.0(b)): unlike templateParameters, a run variable is resolved at RUN time,
// not compiled into finalYaml, so this is what actually keeps a one-shot
// callback token out of the compiled YAML the spike found every
// `${{ t.callback_token }}` reference exposed. The legacy 3-parameter
// dispatch never populates this, so its wire body carries no "variables" key
// at all -- see TestDispatchAzureDevOps_WireBody_NoTargets_MatchesTodayExactly.
type ADORunVariable struct {
	Value    string `json:"value"`
	IsSecret bool   `json:"isSecret"`
}

// DispatchAzureDevOpsRun triggers an Azure DevOps pipeline run via the
// Pipelines REST API, passing the given template parameters (compiled into
// finalYaml -- never put a secret here) and run variables (resolved at run
// time, never compiled), and decodes the started run's id and web link from
// the response body. Auth is either a PAT (Basic) or an Entra app access
// token (Bearer), per the ADOToken. The run reports its result back through
// the callback, not through this response.
//
// variables is nil for every dispatch that never fans out (or has not yet
// been given any secret run variables to carry) -- the "variables" key is
// then omitted from the request body entirely, rather than sent as `{}`.
//
// A missing or malformed body on a 200/201 is NOT an error and yields
// (nil, nil): the dispatch itself already succeeded by that point (the CI job
// is running), and the run id/link are used only best-effort, to populate
// ci_run_id/ci_run_url -- failing the whole dispatch over a response-shape
// change would desync run status from reality for no benefit.
func DispatchAzureDevOpsRun(ctx context.Context, cred ADOToken, cfg AzureDevOpsConfig, ref string, params map[string]string, variables map[string]ADORunVariable) (*CIRunRef, error) {
	if cfg.Organization == "" || cfg.Project == "" || cfg.PipelineID == "" {
		return nil, fmt.Errorf("azure devops connection requires config.organization, config.project, and config.pipeline_id")
	}
	if cred.empty() {
		return nil, fmt.Errorf("azure devops connection requires a credential (personal access token or Entra app registration)")
	}
	if ref == "" {
		ref = cfg.Ref
	}
	// No ref configured anywhere: omit the override entirely so the run uses the
	// branch configured on the pipeline definition. Guessing one (e.g. main)
	// fails validation on repos whose default branch is named differently.
	payload := map[string]any{"templateParameters": params}
	// Omitted entirely when empty (never sent as {}) so a dispatch that never
	// fans out produces exactly today's wire body -- see
	// TestDispatchAzureDevOps_WireBody_NoTargets_MatchesTodayExactly.
	if len(variables) > 0 {
		payload["variables"] = variables
	}
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
		return nil, err
	}
	// PAT -> Basic base64(":"+pat); Entra app token -> Bearer.
	cred.authorize(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("azure devops pipeline run returned %d: %s%s", resp.StatusCode, string(msg), hint)
	}

	var decoded struct {
		ID    json.Number `json:"id"`
		Links struct {
			Web struct {
				Href string `json:"href"`
			} `json:"web"`
		} `json:"_links"`
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err := json.Unmarshal(respBody, &decoded); err != nil || decoded.ID.String() == "" {
		return nil, nil
	}
	return &CIRunRef{ID: decoded.ID.String(), WebURL: decoded.Links.Web.Href}, nil
}

// DispatchAzureDevOps triggers an Azure DevOps pipeline run, discarding the run
// reference DispatchAzureDevOpsRun decodes. Its signature stays exactly as it
// was -- it is shared with Version Lab (internal/api/health.go) -- so this is a
// thin wrapper rather than the entry point growing a new return value.
func DispatchAzureDevOps(ctx context.Context, cred ADOToken, cfg AzureDevOpsConfig, ref string, params map[string]string) error {
	_, err := DispatchAzureDevOpsRun(ctx, cred, cfg, ref, params, nil)
	return err
}

// DispatchAzureDevOpsDrift dispatches a pipeline with the standard drift
// parameters, plus "targets" when the caller filled DriftInputs.TargetsJSON (a
// fan-out dispatch of 2+ targets) -- omitted entirely otherwise, so a
// no-targets request sends exactly today's three keys. variables carries the
// fan-out path's per-target secret callback tokens (drift-fleet-scale.md
// Phase 1b item 3), keyed by the same name the "fan-out" template's
// `$(cb_token_${{ replace(t.working_dir, '/', '_') }})` macro composes at
// compile time (see FanOutCallbackTokenVariableName in internal/api); nil for
// every dispatch that never fans out.
func DispatchAzureDevOpsDrift(ctx context.Context, cred ADOToken, cfg AzureDevOpsConfig, ref string, inputs DriftInputs, variables map[string]ADORunVariable) (*CIRunRef, error) {
	params := map[string]string{
		"callback_url":   inputs.CallbackURL,
		"callback_token": inputs.CallbackToken,
		"working_dir":    inputs.WorkingDir,
	}
	if inputs.TargetsJSON != "" {
		params["targets"] = inputs.TargetsJSON
	}
	return DispatchAzureDevOpsRun(ctx, cred, cfg, ref, params, variables)
}
