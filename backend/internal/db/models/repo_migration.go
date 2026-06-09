package models

import "time"

// Repo migration run statuses.
const (
	RepoMigrationStatusPending   = "pending"
	RepoMigrationStatusRunning   = "running"
	RepoMigrationStatusCompleted = "completed"
	RepoMigrationStatusFailed    = "failed"
)

// Repo migration step (checkpoint) statuses. SkippedExists records a resource
// that already existed in the target and was treated as an idempotent success.
const (
	RepoMigrationStepPending       = "pending"
	RepoMigrationStepCreated       = "created"
	RepoMigrationStepSkippedExists = "skipped-exists"
	RepoMigrationStepFailed        = "failed"
)

// Repo migration resource types, one per provisioning phase. GitHistory is the
// post-repository git push (commits/branches/tags) and is checkpointed as its
// own step so a resumed run can re-run just the history transfer.
const (
	RepoMigrationResourceRepository        = "repository"
	RepoMigrationResourceGitHistory        = "git_history"
	RepoMigrationResourcePipeline          = "pipeline"
	RepoMigrationResourceBranchPolicy      = "branch_policy"
	RepoMigrationResourceVariableGroup     = "variable_group"
	RepoMigrationResourceServiceConnection = "service_connection"
)

// RepoMigration is one Azure DevOps repo-migration EXECUTE run. It is distinct
// from MigrationJob (storage state-file migration) and tracks provisioning of a
// target ADO project from a dry-run plan. Per-resource progress lives in
// RepoMigrationStep rows so an interrupted run can resume.
type RepoMigration struct {
	ID               string     `db:"id" json:"id"`
	OrganizationID   string     `db:"organization_id" json:"organization_id"`
	SourceOrgURL     string     `db:"source_org_url" json:"source_org_url"`
	SourceProject    string     `db:"source_project" json:"source_project"`
	TargetOrgURL     string     `db:"target_org_url" json:"target_org_url"`
	TargetProject    string     `db:"target_project" json:"target_project"`
	Status           string     `db:"status" json:"status"`
	TotalResources   int        `db:"total_resources" json:"total_resources"`
	CreatedResources int        `db:"created_resources" json:"created_resources"`
	SkippedResources int        `db:"skipped_resources" json:"skipped_resources"`
	FailedResources  int        `db:"failed_resources" json:"failed_resources"`
	StartedAt        *time.Time `db:"started_at" json:"started_at,omitempty"`
	CompletedAt      *time.Time `db:"completed_at" json:"completed_at,omitempty"`
	CreatedBy        *string    `db:"created_by" json:"created_by,omitempty"`
	CreatedAt        time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt        time.Time  `db:"updated_at" json:"updated_at"`
}

// RepoMigrationStep is the checkpoint for provisioning a single target resource.
// The (MigrationID, ResourceType, ResourceKey) triple is unique; the
// orchestrator reads terminal steps on re-run to skip already-done work.
type RepoMigrationStep struct {
	ID           string    `db:"id" json:"id"`
	MigrationID  string    `db:"migration_id" json:"migration_id"`
	ResourceType string    `db:"resource_type" json:"resource_type"`
	ResourceKey  string    `db:"resource_key" json:"resource_key"`
	Status       string    `db:"status" json:"status"`
	Detail       *string   `db:"detail" json:"detail,omitempty"`
	Error        *string   `db:"error" json:"error,omitempty"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
	UpdatedAt    time.Time `db:"updated_at" json:"updated_at"`
}
