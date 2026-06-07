package ado

import (
	"context"
	"fmt"
	"time"
)

// MigrationPlan is the structured, read-only result of enumerating an Azure
// DevOps project for migration. It aggregates the five resource listings along
// with per-type counts, any non-fatal Warnings encountered during enumeration,
// and the time the plan was generated. It describes what would be migrated; it
// performs no writes.
type MigrationPlan struct {
	Repositories       []Repository        `json:"repositories"`
	Pipelines          []Pipeline          `json:"pipelines"`
	BranchPolicies     []BranchPolicy      `json:"branch_policies"`
	VariableGroups     []VariableGroup     `json:"variable_groups"`
	ServiceConnections []ServiceConnection `json:"service_connections"`

	RepositoryCount        int `json:"repository_count"`
	PipelineCount          int `json:"pipeline_count"`
	BranchPolicyCount      int `json:"branch_policy_count"`
	VariableGroupCount     int `json:"variable_group_count"`
	ServiceConnectionCount int `json:"service_connection_count"`

	Warnings    []string  `json:"warnings"`
	GeneratedAt time.Time `json:"generated_at"`
}

// EnumerateMigrationPlan builds a MigrationPlan by enumerating all five ADO
// resource types for the client's configured project. Enumeration is resilient:
// if any single resource type fails (e.g. a 403 or 500), the error is recorded
// in Warnings and enumeration continues with the remaining types, mirroring the
// per-file error log used by the storage migration service. It always returns
// a non-nil plan containing whatever could be enumerated; the error return is
// reserved for a future fatal-failure mode and is currently always nil.
func EnumerateMigrationPlan(ctx context.Context, c *Client) (*MigrationPlan, error) {
	plan := &MigrationPlan{
		Warnings:    []string{},
		GeneratedAt: time.Now().UTC(),
	}

	if repos, err := c.ListRepositories(ctx); err != nil {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("repositories: %v", err))
	} else {
		plan.Repositories = repos
	}

	if pipelines, err := c.ListPipelines(ctx); err != nil {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("pipelines: %v", err))
	} else {
		plan.Pipelines = pipelines
	}

	if policies, err := c.ListBranchPolicies(ctx); err != nil {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("branch_policies: %v", err))
	} else {
		plan.BranchPolicies = policies
	}

	if groups, err := c.ListVariableGroups(ctx); err != nil {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("variable_groups: %v", err))
	} else {
		plan.VariableGroups = groups
	}

	if conns, err := c.ListServiceConnections(ctx); err != nil {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("service_connections: %v", err))
	} else {
		plan.ServiceConnections = conns
	}

	plan.RepositoryCount = len(plan.Repositories)
	plan.PipelineCount = len(plan.Pipelines)
	plan.BranchPolicyCount = len(plan.BranchPolicies)
	plan.VariableGroupCount = len(plan.VariableGroups)
	plan.ServiceConnectionCount = len(plan.ServiceConnections)

	return plan, nil
}
