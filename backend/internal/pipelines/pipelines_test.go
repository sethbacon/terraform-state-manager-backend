package pipelines

import (
	"context"
	"testing"
)

func TestGitHubConfigFromMap(t *testing.T) {
	cfg := GitHubConfigFromMap(map[string]any{"owner": "o", "repo": "r", "workflow_id": "w.yml", "ref": "main"})
	if cfg.Owner != "o" || cfg.Repo != "r" || cfg.WorkflowID != "w.yml" || cfg.Ref != "main" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestDispatchGitHubValidates(t *testing.T) {
	ctx := context.Background()
	if err := DispatchGitHubDrift(ctx, "tok", GitHubConfig{Repo: "r", WorkflowID: "w"}, "", DriftInputs{}); err == nil {
		t.Error("expected error for missing owner")
	}
	if err := DispatchGitHubDrift(ctx, "", GitHubConfig{Owner: "o", Repo: "r", WorkflowID: "w"}, "", DriftInputs{}); err == nil {
		t.Error("expected error for missing token")
	}
}

func TestAzureDevOpsConfigAndValidation(t *testing.T) {
	cfg := AzureDevOpsConfigFromMap(map[string]any{"organization": "org", "project": "proj", "pipeline_id": "42"})
	if cfg.Organization != "org" || cfg.Project != "proj" || cfg.PipelineID != "42" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	ctx := context.Background()
	if err := DispatchAzureDevOpsDrift(ctx, ADOPAT("pat"), AzureDevOpsConfig{Project: "p", PipelineID: "1"}, "", DriftInputs{}); err == nil {
		t.Error("expected error for missing organization")
	}
	if err := DispatchAzureDevOpsDrift(ctx, ADOPAT(""), AzureDevOpsConfig{Organization: "o", Project: "p", PipelineID: "1"}, "", DriftInputs{}); err == nil {
		t.Error("expected error for missing pat")
	}
}
