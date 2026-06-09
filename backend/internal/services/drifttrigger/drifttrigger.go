// Package drifttrigger implements the outbound half of code-drift detection: it
// triggers a Terraform "plan" pipeline in Azure DevOps. The plan it kicks off
// later reports its results back to TSM via the inbound drift-ingest endpoint
// (POST /api/v1/drift/ingest), closing the loop.
//
// Authentication uses Workload Identity Federation: the service acquires an
// Azure DevOps-scoped bearer token from a federation.TokenProvider (a projected
// service-account OIDC token exchanged at Entra), then queues the pipeline run
// via the Azure DevOps REST client.
//
// Scope note: this slice is the engine only. There is no scheduler integration
// and no HTTP endpoint yet — Trigger is the clean entry point those will call.
// The source↔pipeline link lookup is a separate slice, so the target pipeline
// id is supplied per request rather than resolved from a source.
package drifttrigger

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth/federation"
	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/ado"
)

// PipelineQueuer is the subset of the Azure DevOps client this service needs. It
// is an interface so the service can be unit-tested with a fake, and so the
// concrete client (built per-call with a fresh federated bearer) can be swapped.
type PipelineQueuer interface {
	QueuePipelineRun(ctx context.Context, req ado.QueuePipelineRunRequest) (*ado.Run, error)
}

// QueuerFactory builds a PipelineQueuer bound to the given bearer token. The
// production factory constructs an ado.Client; tests supply a fake. The token is
// the access token obtained from the federation TokenProvider.
type QueuerFactory func(token string) (PipelineQueuer, error)

// NewADOQueuerFactory returns a QueuerFactory that builds a real ado.Client for
// the configured organization and project, authenticating with the supplied
// (federated) bearer token. Each trigger constructs a fresh client so a rotated
// token is always used.
func NewADOQueuerFactory(organizationURL, project string) QueuerFactory {
	return func(token string) (PipelineQueuer, error) {
		return ado.NewClient(ado.Config{
			OrganizationURL: organizationURL,
			Project:         project,
			Token:           token,
		})
	}
}

// Service triggers Azure DevOps plan pipelines using a federated bearer token.
type Service struct {
	tokenProvider federation.TokenProvider
	queuerFactory QueuerFactory
}

// NewService constructs a trigger Service. tokenProvider supplies the Azure
// DevOps bearer (typically a federation.FederatedTokenProvider; a
// StaticTokenProvider works for tests and early wiring). queuerFactory builds
// the ADO client bound to that token.
func NewService(tokenProvider federation.TokenProvider, queuerFactory QueuerFactory) *Service {
	return &Service{
		tokenProvider: tokenProvider,
		queuerFactory: queuerFactory,
	}
}

// TriggerRequest describes a single outbound plan-trigger. PipelineID is the
// Azure DevOps pipeline to run (supplied directly until the source↔pipeline link
// slice lands). Branch and Parameters are optional and forwarded to the run.
// SourceID is carried through for logging/correlation only.
type TriggerRequest struct {
	SourceID   string
	PipelineID int
	Branch     string
	Parameters map[string]string
}

// TriggerResult is the outcome of a successful trigger: the queued run's id,
// state, and URL as reported by Azure DevOps.
type TriggerResult struct {
	RunID    int
	RunState string
	RunURL   string
}

// Trigger acquires a federated Azure DevOps token and queues a run of the target
// plan pipeline. It is the entry point a scheduler or HTTP handler will call in a
// later slice. Errors from token acquisition or the queue call are wrapped and
// returned; on success the queued run details are returned.
func (s *Service) Trigger(ctx context.Context, req TriggerRequest) (*TriggerResult, error) {
	if req.PipelineID <= 0 {
		return nil, fmt.Errorf("drifttrigger: PipelineID is required")
	}

	logger := slog.With(
		"component", "drift_trigger",
		"source_id", req.SourceID,
		"pipeline_id", req.PipelineID,
	)

	token, err := s.tokenProvider.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("drifttrigger: acquiring federated token: %w", err)
	}

	queuer, err := s.queuerFactory(token)
	if err != nil {
		return nil, fmt.Errorf("drifttrigger: building ADO client: %w", err)
	}

	run, err := queuer.QueuePipelineRun(ctx, ado.QueuePipelineRunRequest{
		PipelineID: req.PipelineID,
		Branch:     req.Branch,
		Parameters: req.Parameters,
	})
	if err != nil {
		return nil, fmt.Errorf("drifttrigger: queuing plan run: %w", err)
	}

	logger.Info("queued plan pipeline run", "run_id", run.ID, "run_state", run.State)

	return &TriggerResult{
		RunID:    run.ID,
		RunState: run.State,
		RunURL:   run.URL,
	}, nil
}
