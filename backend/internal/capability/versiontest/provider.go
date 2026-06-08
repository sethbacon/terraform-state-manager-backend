// Package versiontest implements the version-no-op-test capability — the first
// worked example of the capability contract (internal/capability).
//
// The capability answers a single question for a repository: does bumping a
// dependency (provider/module/Terraform) to a candidate version produce a no-op
// plan, or does it drift the infrastructure? It obtains a Terraform plan via a
// PlanProvider, reuses the driftingest plan parser to classify the changes, and
// records a drift event when the candidate is NOT a no-op.
//
// The live provider that triggers a plan in an external CI system (e.g. Azure
// DevOps) is deferred (O6). This package ships FixturePlanProvider, which reads a
// recorded `terraform show -json` document from disk, so the capability is
// exercisable and testable end-to-end without an outbound connection.
package versiontest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	ingestsvc "github.com/terraform-state-manager/terraform-state-manager/internal/services/driftingest"
)

// PlanProvider obtains a Terraform plan (parsed `terraform show -json`) for a
// repository at a candidate version. Implementations may read a recorded fixture
// (FixturePlanProvider) or, once O6 lands, trigger a live plan in a CI pipeline.
type PlanProvider interface {
	// PlanFor returns the parsed plan for repoURL at candidateVersion. The
	// candidate identifies the dependency version under test; FixturePlanProvider
	// ignores it and returns the recorded plan.
	PlanFor(ctx context.Context, repoURL, candidateVersion string) (*ingestsvc.Plan, error)
}

// FixturePlanProvider is a PlanProvider that returns a plan parsed from a
// recorded `terraform show -json` file. The fixture path is supplied per task via
// the task Config (plan_fixture); the live ADO-backed provider is deferred (O6).
type FixturePlanProvider struct{}

// NewFixturePlanProvider returns a FixturePlanProvider.
func NewFixturePlanProvider() *FixturePlanProvider {
	return &FixturePlanProvider{}
}

// fixturePathKey is the context key carrying the fixture path for a single
// PlanFor call. It is set by the handler from the task Config so the
// PlanProvider interface itself stays free of fixture-specific parameters.
type fixturePathKey struct{}

// withFixturePath returns ctx carrying the fixture path to load.
func withFixturePath(ctx context.Context, path string) context.Context {
	return context.WithValue(ctx, fixturePathKey{}, path)
}

// PlanFor reads the recorded plan fixture whose path was attached to ctx via the
// handler. repoURL and candidateVersion are accepted to satisfy the interface but
// do not influence which fixture is read.
func (p *FixturePlanProvider) PlanFor(ctx context.Context, _, _ string) (*ingestsvc.Plan, error) {
	path, _ := ctx.Value(fixturePathKey{}).(string)
	if path == "" {
		return nil, fmt.Errorf("fixture plan provider: no plan_fixture path configured")
	}

	// #nosec G304 -- the fixture path is operator-supplied task configuration
	// (scheduler:admin/versiontest:admin scope), not untrusted external input.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("fixture plan provider: read %s: %w", path, err)
	}

	var plan ingestsvc.Plan
	if err := json.Unmarshal(data, &plan); err != nil {
		return nil, fmt.Errorf("fixture plan provider: parse %s: %w", path, err)
	}
	return &plan, nil
}
