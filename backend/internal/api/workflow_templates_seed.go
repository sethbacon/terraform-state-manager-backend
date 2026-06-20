package api

import (
	"context"
	"database/sql"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// builtinWorkflowSeeds are the templates inserted into the store at startup so
// operators have an editable baseline. profile="default" matches the handler's
// default lookup, so what is served is unchanged until an operator edits them.
func builtinWorkflowSeeds() []repositories.WorkflowTemplate {
	return []repositories.WorkflowTemplate{
		{Provider: "github_actions", Kind: "drift", Profile: "default", Name: "GitHub Actions — Drift (built-in)", Content: githubDriftWorkflow, IsBuiltin: true},
		{Provider: "azure_devops", Kind: "drift", Profile: "default", Name: "Azure DevOps — Drift (built-in)", Content: azureDriftPipeline, IsBuiltin: true},
		{Provider: "github_actions", Kind: "versionlab", Profile: "default", Name: "GitHub Actions — Version Lab (built-in)", Content: githubHealthWorkflow, IsBuiltin: true},
		{Provider: "azure_devops", Kind: "versionlab", Profile: "default", Name: "Azure DevOps — Version Lab (built-in)", Content: azureHealthPipeline, IsBuiltin: true},
		// Suite variants: use the published Terraform-suite actions / extension tasks.
		{Provider: "github_actions", Kind: "drift", Profile: "suite", Name: "GitHub Actions — Drift (suite actions)", Content: githubDriftWorkflowSuite, IsBuiltin: true},
		{Provider: "azure_devops", Kind: "drift", Profile: "suite", Name: "Azure DevOps — Drift (suite extension)", Content: azureDriftPipelineSuite, IsBuiltin: true},
		{Provider: "github_actions", Kind: "versionlab", Profile: "suite", Name: "GitHub Actions — Version Lab (suite actions)", Content: githubHealthWorkflowSuite, IsBuiltin: true},
		{Provider: "azure_devops", Kind: "versionlab", Profile: "suite", Name: "Azure DevOps — Version Lab (suite extension)", Content: azureHealthPipelineSuite, IsBuiltin: true},
	}
}

// SeedWorkflowTemplates inserts the built-in templates if absent. Idempotent and
// safe to call on every startup; a nil database is a no-op (unit tests).
func SeedWorkflowTemplates(ctx context.Context, database *sql.DB) error {
	if database == nil {
		return nil
	}
	repo := repositories.NewWorkflowTemplateRepository(database)
	for _, t := range builtinWorkflowSeeds() {
		t := t
		if err := repo.EnsureBuiltin(ctx, &t); err != nil {
			return err
		}
	}
	return nil
}
